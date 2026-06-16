#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent}"
RELEASE="${RELEASE:-pg-agent}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"

begin_suite "Agent Install (lease-based failover: primary + 1 standby)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
# podManagementPolicy is immutable; a leftover StatefulSet from a prior repmgrd
# run (OrderedReady) blocks an agent-mode (Parallel) install, so clear it.
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --wait --timeout 10m

POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# --- agent mode shape: no repmgrd / service-updater sidecars ---
for pod in "${POD0}" "${POD1}"; do
  containers=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.spec.containers[*].name}')
  assert_contains "${pod} has postgresql container" "${containers}" "postgresql"
  assert_not_contains "${pod} has NO repmgrd sidecar (agent mode)" "${containers}" "repmgrd"
  assert_not_contains "${pod} has NO service-updater sidecar (agent mode)" "${containers}" "service-updater"
done

# --- exactly one primary, and it is the lease holder ---
# Agent mode uses podManagementPolicy: Parallel, so the primary is lease-decided,
# not pinned to pod-0; discover it.
echo "  Waiting for a single primary + lease holder to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; HOLDER=""
settle_elapsed=0
while [[ ${settle_elapsed} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; fi
  if [[ -n "${PRIMARY}" && "${HOLDER}" == "${PRIMARY}" ]]; then
    echo "  settled after ${settle_elapsed}s: primary=${PRIMARY} holder=${HOLDER}"
    break
  fi
  sleep 5
  settle_elapsed=$((settle_elapsed + 5))
done

assert_contains "exactly one primary elected" "${PRIMARY:-none}" "${FULLNAME}"
assert_eq "the lease holder is the primary" "${PRIMARY}" "${HOLDER}"

# Persist the discovered roles for the failover suite (run next in the same ns).
echo "${PRIMARY}" > "${SCRIPT_DIR}/.agent_primary"
echo "${STANDBY}" > "${SCRIPT_DIR}/.agent_standby"

# --- connectivity + replication ---
result=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT 1" "testuser" "testdb")
assert_eq "can connect to primary" "1" "${result}"

REPL_VALUE="replicated-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "DROP TABLE IF EXISTS repl_test" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE repl_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO repl_test (value) VALUES ('${REPL_VALUE}')" "testuser" "testdb"
sleep 3
replicated_val=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM repl_test WHERE value='${REPL_VALUE}'" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "data replicated to standby" "${REPL_VALUE}" "${replicated_val}"

# --- standby is read-only ---
pg_exec "${NAMESPACE}" "${STANDBY}" "INSERT INTO repl_test (value) VALUES ('nope')" "testuser" "testdb" 2>/dev/null && w=true || w=false
assert_eq "standby rejects writes" "false" "${w}"

# --- write Service points at the primary ---
svc_endpoint=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
assert_eq "write service points at the primary" "${PRIMARY}" "${svc_endpoint}"

# --- agent RBAC: lease + serviceaccount exist ---
sa=$(kubectl get sa -n "${NAMESPACE}" "${FULLNAME}-repmgr" -o name 2>/dev/null || echo "")
assert_contains "serviceaccount exists" "${sa}" "serviceaccount"
lease_exists=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_contains "leader Lease exists" "${lease_exists}" "lease"

end_suite
print_summary
