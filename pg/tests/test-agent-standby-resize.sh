#!/bin/bash
# Standby sizing and in-place resize on a role change (#386). On a 3-node cluster with
# pgbackrest on and postgresql/pgbackrest standbyResources set:
#   - the primary's postgresql and pgbackrest containers run the primary size, actuated by the
#     kubelet (status.containerStatuses[].resources), and the standbys the standby size, with
#     no container restart anywhere;
#   - a backup runs in the raised primary's pgbackrest sidecar;
#   - after a switchover the new primary is raised and the old one lowered once it rejoins;
#   - after the primary pod is deleted the promoted standby is raised, and the recreated pod
#     comes back at the standby size (the template's) and stays there;
#   - a primary size no node can fit leaves the primary serving at the standby size, with
#     pg_ha_agent_pod_resize_infeasible=1 and an Error in its log.
# Standalone, opt-in: `make -C pg test-agent-standby-resize`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-resize}"
RELEASE="${RELEASE:-pg-resize}"
VALUES="${SCRIPT_DIR}/values-pgbackrest-minio-ha.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
LEASE="${FULLNAME}-leader"
MARKER="${FULLNAME}-primary"
PODS=("${FULLNAME}-0" "${FULLNAME}-1" "${FULLNAME}-2")
FAILOVER_BUDGET="${FAILOVER_BUDGET:-120}"
RESIZE_BUDGET="${RESIZE_BUDGET:-120}"

# Primary sizes come from the fixture (postgresql 1Gi / pgbackrest 1Gi memory limits); the
# standby sizes are set here and told apart by their memory limits.
PG_PRIMARY_MEM=1Gi
PG_STANDBY_MEM=512Mi
BR_PRIMARY_MEM=1Gi
BR_STANDBY_MEM=64Mi
SIZE_ARGS=(
  --set postgresql.replicaCount=2
  --set postgresql.standbyResources.requests.cpu=50m --set postgresql.standbyResources.requests.memory=256Mi
  --set postgresql.standbyResources.limits.cpu=500m --set "postgresql.standbyResources.limits.memory=${PG_STANDBY_MEM}"
  --set pgbackrest.standbyResources.requests.cpu=10m --set pgbackrest.standbyResources.requests.memory=32Mi
  --set pgbackrest.standbyResources.limits.cpu=100m --set "pgbackrest.standbyResources.limits.memory=${BR_STANDBY_MEM}"
)

begin_suite "Agent standby sizing and in-place resize (#386)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
deploy_minio "${NAMESPACE}"

echo "Installing a 3-node agent cluster with standbyResources on postgresql and pgbackrest..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${VALUES}" "${SIZE_ARGS[@]}" \
  --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/switchover-target- >/dev/null 2>&1 || true

in_recovery() { pg_exec "${NAMESPACE}" "$1" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo ""; }
holder() { kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo ""; }
# actuated <pod> <container>: the memory limit the kubelet reports in effect.
actuated() {
  kubectl get pod "$1" -n "${NAMESPACE}" \
    -o jsonpath="{.status.containerStatuses[?(@.name==\"$2\")].resources.limits.memory}" 2>/dev/null || echo ""
}
restarts() { # summed restartCount of the pod's containers
  kubectl get pod "$1" -n "${NAMESPACE}" \
    -o jsonpath='{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s += $1} END {print s + 0}'
}
uid_of() { kubectl get pod "$1" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo ""; }
metric() { # metric <pod> <name>: the agent's gauge, through the apiserver pod proxy
  kubectl get --raw "/api/v1/namespaces/${NAMESPACE}/pods/$1:9200/proxy/metrics" 2>/dev/null \
    | awk -v n="$2" '$1 == n {print $2}' || echo ""
}

# wait_size <pod> <pg-mem> <br-mem> <budget>: both containers actuated at the given sizes.
wait_size() {
  local e=0
  while [[ ${e} -lt $4 ]]; do
    [[ "$(actuated "$1" postgresql)" == "$2" && "$(actuated "$1" pgbackrest)" == "$3" ]] && return 0
    sleep 3; e=$((e + 3))
  done
  return 1
}
sizes_of() { echo "postgresql=$(actuated "$1" postgresql) pgbackrest=$(actuated "$1" pgbackrest)"; }

# settle_roles <budget>: PRIMARY and STANDBYS once exactly one node is read-write, holds the
# lease, and the other two are in recovery.
settle_roles() {
  local budget="$1" elapsed=0 p r rw h
  while [[ ${elapsed} -lt ${budget} ]]; do
    PRIMARY=""; STANDBYS=(); rw=0
    for p in "${PODS[@]}"; do
      r=$(in_recovery "${p}")
      if [[ "${r}" == "f" ]]; then PRIMARY="${p}"; rw=$((rw + 1)); elif [[ "${r}" == "t" ]]; then STANDBYS+=("${p}"); fi
    done
    h=$(holder)
    if [[ ${rw} -eq 1 && ${#STANDBYS[@]} -eq 2 && "${h}" == "${PRIMARY}" ]]; then return 0; fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

assert_sizes() { # assert_sizes <label> <pod> <pg-mem> <br-mem>
  wait_size "$2" "$3" "$4" "${RESIZE_BUDGET}" \
    && pass "$1" \
    || fail "$1" "$(sizes_of "$2") (want postgresql=$3 pgbackrest=$4)"
}

echo "Waiting for the roles to settle..."
if settle_roles 300; then
  pass "install: one primary holds the lease (${PRIMARY}), two standbys (${STANDBYS[*]})"
else
  fail "install: one primary holds the lease and two standbys stream" "primary=${PRIMARY:-none} standbys=${STANDBYS[*]:-none} holder=$(holder)"
  end_suite; print_summary; exit 1
fi

# --- install: the primary is raised, the standbys stay at the template's size ---
assert_sizes "#386: the primary runs the primary size (postgresql + pgbackrest, actuated)" "${PRIMARY}" "${PG_PRIMARY_MEM}" "${BR_PRIMARY_MEM}"
for s in "${STANDBYS[@]}"; do
  assert_sizes "#386: standby ${s} runs the standby size" "${s}" "${PG_STANDBY_MEM}" "${BR_STANDBY_MEM}"
done
assert_eq "#386: pg_ha_agent_pod_sized_as_primary=1 on the primary" "1" "$(metric "${PRIMARY}" pg_ha_agent_pod_sized_as_primary)"
assert_eq "#386: pg_ha_agent_pod_resize_pending=0 on the primary" "0" "$(metric "${PRIMARY}" pg_ha_agent_pod_resize_pending)"
# Baseline restart counts once sized: a resize with resizePolicy NotRequired must not add one.
declare -A RESTARTS
for p in "${PODS[@]}"; do RESTARTS[${p}]=$(restarts "${p}"); done

# --- a backup runs in the raised primary's sidecar ---
echo "Running a full backup (stanza-create) through the primary's pgbackrest sidecar..."
kubectl delete job pgbr-full -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full" >/dev/null
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [[ ${full_rc} -ne 0 ]]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "#386: a full backup succeeds on the raised primary" "0" "${full_rc}"
assert_contains "#386: ... and ran in ${PRIMARY}" "$(kubectl logs -n "${NAMESPACE}" job/pgbr-full 2>/dev/null || true)" "Primary pod resolved to: ${PRIMARY}"

# --- switchover: the new primary is raised, the old one lowered after it rejoins ---
# Sizing is judged on whichever standby takes over. On three nodes the lease can go to the
# standby that was NOT named: the primary clears the request and steps down, and every standby
# then contends. That is the switchover's own behaviour, not this feature's, so it is left to
# the switchover suites.
OLD_PRIMARY="${PRIMARY}"
TARGET="${STANDBYS[0]}"
echo "Switching over ${OLD_PRIMARY} -> ${TARGET}..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/switchover-target="${TARGET}" --overwrite >/dev/null
NEW=""; elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  h=$(holder)
  if [[ -n "${h}" && "${h}" != "${OLD_PRIMARY}" && "$(in_recovery "${h}")" == "f" ]]; then NEW="${h}"; break; fi
  sleep 3; elapsed=$((elapsed + 3))
done
assert_eq "#386: the switchover moves the primary off ${OLD_PRIMARY}" "yes" "$([[ -n "${NEW}" ]] && echo yes || echo no)"
[[ "${NEW}" != "${TARGET}" && -n "${NEW}" ]] && echo "  NOTE: ${NEW} took the lease, not the requested ${TARGET}"
settle_roles 300 || true
if [[ -n "${NEW}" ]]; then
  assert_sizes "#386: switchover: the new primary ${NEW} is raised" "${NEW}" "${PG_PRIMARY_MEM}" "${BR_PRIMARY_MEM}"
  TARGET="${NEW}"
fi
assert_eq "#386: switchover: the old primary rejoined as a standby" "t" "$(in_recovery "${OLD_PRIMARY}")"
assert_sizes "#386: switchover: the old primary ${OLD_PRIMARY} is lowered" "${OLD_PRIMARY}" "${PG_STANDBY_MEM}" "${BR_STANDBY_MEM}"
for p in "${TARGET}" "${OLD_PRIMARY}"; do
  assert_eq "#386: switchover: no container of ${p} restarted across both resizes" "${RESTARTS[${p}]}" "$(restarts "${p}")"
done

# --- failover: the promoted standby is raised, the recreated pod comes back small ---
PRIMARY="${TARGET}"
MARK="before-failover-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS standby_resize (v text)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO standby_resize VALUES ('${MARK}')" "testuser" "testdb" >/dev/null
settle_roles 60 || true
for s in "${STANDBYS[@]}"; do
  e=0
  while [[ ${e} -lt 60 ]]; do
    [[ "$(pg_exec "${NAMESPACE}" "${s}" "SELECT v FROM standby_resize WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || true)" == "${MARK}" ]] && break
    sleep 2; e=$((e + 2))
  done
done
DELETED="${PRIMARY}"
old_uid=$(uid_of "${DELETED}")
echo "Deleting primary ${DELETED}..."
kubectl delete pod "${DELETED}" -n "${NAMESPACE}" --grace-period=30 --wait=false >/dev/null 2>&1 || true
promoted=""; elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  h=$(holder)
  if [[ -n "${h}" && "${h}" != "${DELETED}" && "$(in_recovery "${h}")" == "f" ]]; then promoted="${h}"; echo "  ${h} promoted after ${elapsed}s"; break; fi
  sleep 2; elapsed=$((elapsed + 2))
done
assert_eq "#386: failover: a standby promotes" "yes" "$([[ -n "${promoted}" ]] && echo yes || echo no)"
if [[ -n "${promoted}" ]]; then
  assert_sizes "#386: failover: the promoted ${promoted} is raised" "${promoted}" "${PG_PRIMARY_MEM}" "${BR_PRIMARY_MEM}"
  assert_eq "#386: failover: no data lost" "${MARK}" \
    "$(pg_exec "${NAMESPACE}" "${promoted}" "SELECT v FROM standby_resize WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || true)"
fi
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
settle_roles 300 || true
assert_eq "#386: failover: ${DELETED} was recreated" "yes" "$([[ "$(uid_of "${DELETED}")" != "${old_uid}" ]] && echo yes || echo no)"
assert_eq "#386: failover: the recreated ${DELETED} is a standby" "t" "$(in_recovery "${DELETED}")"
assert_sizes "#386: failover: the recreated ${DELETED} runs the standby size" "${DELETED}" "${PG_STANDBY_MEM}" "${BR_STANDBY_MEM}"
# ... and stays there: nothing raises a standby.
sleep 20
assert_eq "#386: failover: ... and stays at it" "${PG_STANDBY_MEM}" "$(actuated "${DELETED}" postgresql)"

# --- Infeasible: a primary size no node can fit ---
echo "Upgrading to a primary size no node can fit (cpu requests 512)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${VALUES}" "${SIZE_ARGS[@]}" \
  --set postgresql.resources.requests.cpu=512 --set postgresql.resources.limits.cpu=512 \
  --wait --timeout 15m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 900
if settle_roles 300; then
  pass "#386: infeasible: the cluster settles after the upgrade (primary ${PRIMARY})"
else
  fail "#386: infeasible: the cluster settles after the upgrade" "primary=${PRIMARY:-none} holder=$(holder)"
fi
if [[ -n "${PRIMARY}" ]]; then
  e=0; inf=""
  while [[ ${e} -lt ${RESIZE_BUDGET} ]]; do
    inf=$(metric "${PRIMARY}" pg_ha_agent_pod_resize_infeasible)
    [[ "${inf}" == "1" ]] && break
    sleep 3; e=$((e + 3))
  done
  assert_eq "#386: infeasible: pg_ha_agent_pod_resize_infeasible=1 on the primary" "1" "${inf}"
  assert_eq "#386: infeasible: pg_ha_agent_pod_resize_pending=1" "1" "$(metric "${PRIMARY}" pg_ha_agent_pod_resize_pending)"
  assert_eq "#386: infeasible: the primary keeps the standby size" "${PG_STANDBY_MEM}" "$(actuated "${PRIMARY}" postgresql)"
  assert_contains "#386: infeasible: the agent logs an Error naming it" \
    "$(kubectl logs -n "${NAMESPACE}" "${PRIMARY}" -c postgresql --since=10m 2>/dev/null | grep -m1 'cannot be raised to the primary size' || true)" \
    "Infeasible"
  INF="infeasible-$(date +%s)"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO standby_resize VALUES ('${INF}')" "testuser" "testdb" >/dev/null 2>&1 || true
  assert_eq "#386: infeasible: the primary still takes writes" "${INF}" \
    "$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT v FROM standby_resize WHERE v='${INF}'" "testuser" "testdb" 2>/dev/null || true)"
fi

end_suite
print_summary
