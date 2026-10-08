#!/bin/bash
# Live PGPool failover + health-check test (#32). Proves that, with a client connected
# THROUGH PGPool, a primary failover is transparent: PGPool's health check recycles the
# moved RW backend, the failed primary is excluded, writes route to the promoted primary,
# and pre-failover data survives. Agent mode (the default), where PGPool fronts the RW
# (<fullname>) and RO (<fullname>-readonly) Services and the agent owns failover.
# OPT-IN / standalone (run in a clean cluster): `make -C pg test-pgpool-failover`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-pgpool-failover}"
RELEASE="${RELEASE:-pgpoolfo}"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent-pgpool.yaml")
LEASE="${FULLNAME}-leader"
PGPOOL="${FULLNAME}-pgpool.${NAMESPACE}.svc.cluster.local"

begin_suite "PGPool failover + health check (#32)"

# --- install agent mode + pgpool (replicaCount 1 -> 2 pg pods + pgpool) ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent-pgpool.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-pgpool" 300

POD0="${FULLNAME}-0"; POD1="${FULLNAME}-1"
PGPW=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.password}' | base64 -d)

# via_pgpool <exec-pod> <sql> -> stdout+stderr, psql rc preserved. Connects to the PGPool
# Service (frontend :9999) as the app user; PGPool decides the backend. The exec pod is
# only a shell host -- it is NOT the routing target.
via_pgpool() {
  kubectl exec -n "${NAMESPACE}" "$1" -c postgresql -- \
    env PGPASSWORD="${PGPW}" psql -h "${PGPOOL}" -p 9999 -U testuser -d testdb -tAc "$2" 2>&1
}

# --- settle the primary/standby pair ---
echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a primary" "${PRIMARY:-none}" "${FULLNAME}"

# --- health check: PGPool sees its backends; the primary (RW Service) backend is up ---
nodes=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
up_count=$(printf '%s\n' "${nodes}" | grep -c "|up|" || true)
assert_gt "#32 health check: PGPool reports a backend up (SHOW POOL_NODES)" "${up_count}" "0"

# --- write through PGPool before failover -> routes to the RW (primary) backend ---
FV="before-$(date +%s)"
mk1=$(via_pgpool "${STANDBY}" "CREATE TABLE IF NOT EXISTS pp_fo (id serial PRIMARY KEY, v text); INSERT INTO pp_fo (v) VALUES ('${FV}'); SELECT 'ok'" || true)
assert_contains "#32: write through PGPool reaches the primary (pre-failover)" "${mk1}" "ok"
# the write actually landed on the elected primary
on_primary=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT v FROM pp_fo WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#32: pre-failover write is on the elected primary" "${FV}" "${on_primary}"

# --- fail the primary over: delete it; the agent promotes the standby + repoints the RW Service ---
echo "  Deleting primary ${PRIMARY} (failover; PGPool must follow the RW Service)..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
promoted=false; e=0
while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "t" && "${holder}" == "${STANDBY}" ]]; then promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; fi
  sleep 3; e=$((e + 3))
done
assert_eq "#32: standby promoted on failover" "true" "${promoted}"

if [[ "${promoted}" == "true" ]]; then
  # --- recovery THROUGH PGPool: writes resume against the new primary. Retry while
  #     PGPool's health check recycles the RW backend connection to the moved endpoint. ---
  AFTER="after-$(date +%s)"
  wrote=false; w=0
  while [[ ${w} -lt ${FAILOVER_BUDGET} ]]; do
    out=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${AFTER}'); SELECT 'ok'" || true)
    if printf '%s' "${out}" | grep -q "ok"; then wrote=true; echo "  PGPool write recovered after ${w}s"; break; fi
    sleep 3; w=$((w + 3))
  done
  assert_eq "#32: client connections recover -- write through PGPool routes to the new primary" "true" "${wrote}"

  # both markers visible through PGPool: pre-failover data survived + reads route correctly
  both=$(via_pgpool "${STANDBY}" "SELECT count(*) FROM pp_fo WHERE v IN ('${FV}','${AFTER}')" || echo "")
  assert_eq "#32: pre- and post-failover rows both readable through PGPool" "2" "$(printf '%s' "${both}" | tr -d '[:space:]')"

  # the failed backend is excluded / the RW backend reconnected to the new primary: a
  # backend is up again and the post-failover write is confirmed on the new primary
  on_new=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM pp_fo WHERE v='${AFTER}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#32: post-failover write landed on the new primary" "${AFTER}" "${on_new}"
  nodes2=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  up2=$(printf '%s\n' "${nodes2}" | grep -c "|up|" || true)
  assert_gt "#32: PGPool has a healthy backend after failover (excluded the dead primary)" "${up2}" "0"
else
  skip "#32: client connections recover (failover did not complete)"
  skip "#32: pre- and post-failover rows both readable through PGPool (failover did not complete)"
  skip "#32: post-failover write landed on the new primary (failover did not complete)"
  skip "#32: PGPool has a healthy backend after failover (failover did not complete)"
fi

# --- #361: losing the only standby must not take PGPool (and writes) down ---
# Every new PGPool session connects to every attached backend first, so an RO Service with no
# ready endpoint used to stall every session; on a CNI that drops instead of rejecting, the
# stall outlived the probes and the kubelet restarted PGPool in a loop with a healthy primary.
# The RO backend is now detachable: PGPool must mark it down, keep serving writes without a
# restart, and the re-attach loop must bring it back once the Service answers again. KinD's
# kube-proxy REJECTS an endpointless Service, so this exercises the detach/re-attach path, not
# the drop-induced stall (which the health-check and probe budgets cover at render time).
echo "  #361: waiting for the former primary to rejoin so the -readonly Service has an endpoint..."
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
ro_ready=false; s=0
while [[ ${s} -lt 240 ]]; do
  ro_eps=$(kubectl get endpointslices -n "${NAMESPACE}" -l "kubernetes.io/service-name=${FULLNAME}-readonly" -o jsonpath='{range .items[*].endpoints[*]}{.conditions.ready}{" "}{end}' 2>/dev/null || true)
  nodes_pre=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if [[ "${ro_eps}" == *true* ]] && printf '%s\n' "${nodes_pre}" | grep -q -- "-readonly.*|up|"; then ro_ready=true; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: precondition -- the RO backend is attached and the -readonly Service has a ready endpoint" "true" "${ro_ready}"
PGPOOL_POD=$(kubectl get pod -n "${NAMESPACE}" -l app.kubernetes.io/component=pgpool -o jsonpath='{.items[0].metadata.name}')
restarts_before=$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')
echo "  #361: emptying the -readonly Service (what one unready standby produces)..."
kubectl patch svc "${FULLNAME}-readonly" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":"no-such-role"}}}' >/dev/null
detached=false; s=0
while [[ ${s} -lt 120 ]]; do
  nodes_mid=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if printf '%s\n' "${nodes_mid}" | grep -q -- "-readonly.*|down|"; then detached=true; echo "  RO backend detached after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: PGPool detaches the unreachable RO backend" "true" "${detached}"
W361="standby-away-$(date +%s)"
w361=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${W361}'); SELECT v FROM pp_fo WHERE v='${W361}'" || true)
assert_contains "#361: writes and reads flow through PGPool with the RO backend down" "${w361}" "${W361}"
assert_eq "#361: PGPool stays Ready" "true" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].ready}')"
assert_eq "#361: ... and is not restarted" "${restarts_before}" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')"
echo "  #361: restoring the -readonly Service; the re-attach loop must bring the backend back..."
kubectl patch svc "${FULLNAME}-readonly" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":"standby"}}}' >/dev/null
reattached=false; s=0
while [[ ${s} -lt 120 ]]; do
  nodes_post=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if printf '%s\n' "${nodes_post}" | grep -q -- "-readonly.*|up|"; then reattached=true; echo "  RO backend re-attached after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: the re-attach loop re-attaches the RO backend once the Service answers" "true" "${reattached}"
assert_contains "#361: ... and says so in the pgpool log" "$(kubectl logs -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool --since=10m 2>/dev/null | grep -m1 '\[reattach\]' || true)" "re-attaching"
assert_eq "#361: no restart across the whole episode" "${restarts_before}" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')"

end_suite
print_summary
