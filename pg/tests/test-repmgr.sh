#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-repmgr}"
RELEASE="${RELEASE:-pg-repmgr}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")

begin_suite "Repmgr Install (primary + 1 replica with repmgr)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
sleep 3

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --wait --timeout 10m

POD_PRIMARY="${FULLNAME}-0"
POD_REPLICA="${FULLNAME}-1"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# Test: both pods are running
for pod in "${POD_PRIMARY}" "${POD_REPLICA}"; do
  pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "pod ${pod} is Running" "Running" "${pod_phase}"
done

# Test: all containers are present in each pod
for pod in "${POD_PRIMARY}" "${POD_REPLICA}"; do
  containers=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.spec.containers[*].name}')
  assert_contains "${pod} has postgresql container" "${containers}" "postgresql"
  assert_contains "${pod} has repmgrd container" "${containers}" "repmgrd"
  assert_contains "${pod} has service-updater container" "${containers}" "service-updater"
done

# Test: can connect to primary
result=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT 1" "testuser" "testdb")
assert_eq "can connect to primary" "1" "${result}"

# Test: can connect to replica
result=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT 1" "testuser" "testdb")
assert_eq "can connect to replica" "1" "${result}"

# Test: primary is in recovery = false (it's the primary)
is_primary=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "pod-0 is primary (not in recovery)" "t" "${is_primary}"

# Test: replica is in recovery = true
is_replica=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "pod-1 is replica (in recovery)" "t" "${is_replica}"

# Test: repmgr cluster shows both nodes (retry -- standby registration may lag pod readiness)
cluster_output=""
for i in $(seq 1 12); do
  cluster_output=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT node_name, type, active FROM repmgr.nodes ORDER BY node_id" "repmgr" "repmgr")
  if echo "${cluster_output}" | grep -q "standby"; then
    break
  fi
  sleep 5
done
assert_contains "repmgr sees primary node" "${cluster_output}" "primary"
assert_contains "repmgr sees standby node" "${cluster_output}" "standby"

# Test: replication works - write on primary, read on replica
REPL_VALUE="replicated-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "DROP TABLE IF EXISTS repl_test" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "CREATE TABLE repl_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "INSERT INTO repl_test (value) VALUES ('${REPL_VALUE}')" "testuser" "testdb"

sleep 3

replicated_val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM repl_test WHERE value='${REPL_VALUE}'" "testuser" "testdb")
assert_eq "data replicated to standby" "${REPL_VALUE}" "${replicated_val}"

# Test: replica is read-only
pg_exec "${NAMESPACE}" "${POD_REPLICA}" "INSERT INTO repl_test (value) VALUES ('should-fail')" "testuser" "testdb" 2>/dev/null && write_succeeded=true || write_succeeded=false
assert_eq "replica rejects writes" "false" "${write_succeeded}"

# Test: master service points to primary pod
svc_endpoint=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}')
assert_eq "master service points to primary" "${POD_PRIMARY}" "${svc_endpoint}"

# Test: headless service exists
headless_ip=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-headless" -o jsonpath='{.spec.clusterIP}')
assert_eq "headless service has clusterIP None" "None" "${headless_ip}"

# Test: RBAC resources exist
sa=$(kubectl get sa -n "${NAMESPACE}" "${FULLNAME}-repmgr" -o name 2>/dev/null || echo "")
assert_contains "serviceaccount exists" "${sa}" "serviceaccount"

end_suite
print_summary
