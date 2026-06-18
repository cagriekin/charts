#!/bin/bash
# Live etcd-mode test (Part G6): the agent runs the etcd DCS backend with the
# bundled etcd subchart. Proves leadership lives in etcd (no apiserver Lease) and
# that failover still works end to end. OPT-IN / standalone (not in test-cluster):
# it deploys 3 extra etcd pods and is not yet validated in CI, so run it explicitly
# in a clean cluster: `make -C pg test-agent-etcd`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-etcd}"
RELEASE="${RELEASE:-pgetcd}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
ETCD_STS="${RELEASE}-etcd"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"

begin_suite "Agent etcd DCS (bundled etcd, leadership off the apiserver) — Part G6"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete statefulset "${FULLNAME}" "${ETCD_STS}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  -f "${SCRIPT_DIR}/values-agent-etcd.yaml" \
  --wait --timeout 10m

POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"

# --- the bundled etcd cluster forms (3 members) ---
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=etcd" 3 300
etcd_ready=$(kubectl get statefulset "${ETCD_STS}" -n "${NAMESPACE}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
assert_eq "bundled etcd: 3 members ready" "3" "${etcd_ready}"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# --- leadership lives in etcd, NOT the apiserver: the agent creates no K8s Lease ---
lease_found=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_eq "etcd mode: no apiserver leader Lease (leadership is in etcd)" "" "${lease_found}"

# --- exactly one primary settles (elected via etcd); discover it ---
echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "exactly one primary elected via etcd" "${PRIMARY:-none}" "${FULLNAME}"

# Everything past election needs a settled primary+standby. Gate it (mirroring the
# failover suite) so a slow/failed election produces clean FAILs + a summary rather
# than a `set -e` abort on an unguarded pg_exec with an empty pod name.
if [[ -n "${PRIMARY}" && -n "${STANDBY}" ]]; then
  svc_endpoint=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
  assert_eq "write service points at the primary" "${PRIMARY}" "${svc_endpoint}"

  # --- replication ---
  FV="etcd-$(date +%s)"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS etcd_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO etcd_test (value) VALUES ('${FV}')" "testuser" "testdb"
  sleep 3
  repl=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM etcd_test WHERE value='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "data replicated to standby" "${FV}" "${repl}"

  # --- failover: delete the primary; the standby promotes (leadership moves in etcd) ---
  echo "  Deleting primary ${PRIMARY} (etcd-mode failover)..."
  kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
  promoted=false; e=0
  while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "t" ]]; then promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; fi
    sleep 3; e=$((e + 3))
  done
  assert_eq "standby promoted on etcd-mode failover" "true" "${promoted}"

  if [[ "${promoted}" == "true" ]]; then
    survived=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM etcd_test WHERE value='${FV}'" "testuser" "testdb")
    assert_eq "data survives failover" "${FV}" "${survived}"
    AFTER="etcd-after-$(date +%s)"
    pg_exec "${NAMESPACE}" "${STANDBY}" "INSERT INTO etcd_test (value) VALUES ('${AFTER}')" "testuser" "testdb"
    wv=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM etcd_test WHERE value='${AFTER}'" "testuser" "testdb")
    assert_eq "new primary is writable" "${AFTER}" "${wv}"
    # The agent patches the write-Service selector asynchronously after promotion,
    # and endpoints update only once kube reconciles the new selector -- so poll
    # rather than checking once immediately after promotion is detected.
    ep=""; r=0
    while [[ ${r} -lt 30 ]]; do
      ep=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
      [[ "${ep}" == "${STANDBY}" ]] && break
      sleep 3; r=$((r + 3))
    done
    assert_eq "write service repoints to the new primary" "${STANDBY}" "${ep}"
  else
    skip "data survives failover (failover did not complete)"
    skip "new primary is writable (failover did not complete)"
    skip "write service repoints to the new primary (failover did not complete)"
  fi
else
  skip "write service points at the primary (no primary settled)"
  skip "data replicated to standby (no primary settled)"
  skip "standby promoted on etcd-mode failover (no primary settled)"
  skip "data survives failover (no primary settled)"
  skip "new primary is writable (no primary settled)"
  skip "write service repoints to the new primary (no primary settled)"
fi

# --- decoupling proof (apiserver outage tolerance) is NOT automated here ---
# kindnet does not enforce NetworkPolicy and a single-node kind control-plane pause
# would freeze the etcd pods too, so the "apiserver down -> primary keeps serving"
# demonstration needs a NetworkPolicy-enforcing CNI or a multi-node cluster. Manual:
# block the agent pods' egress to the apiserver (443/6443) and confirm the primary
# keeps serving writes (vs the kubernetes backend, which self-demotes on renew loss).
echo "  NOTE: the apiserver-outage decoupling proof is a manual step (see comment); not automated under kindnet."

end_suite
print_summary
