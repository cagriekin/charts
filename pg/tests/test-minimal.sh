#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-minimal}"
RELEASE="${RELEASE:-pg-minimal}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-minimal.yaml")

begin_suite "Minimal Install (standalone PostgreSQL, no repmgr)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

POD="${FULLNAME}-0"

# Test: pod is running
pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.phase}')
assert_eq "pod ${POD} is Running" "Running" "${pod_phase}"

# Test: postgresql container is ready
ready=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="postgresql")].ready}')
assert_eq "postgresql container is ready" "true" "${ready}"

# Test: can connect and run a query
result=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT 1" "testuser" "testdb")
assert_eq "can execute SELECT 1" "1" "${result}"

# Test: database exists
db_exists=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT 1 FROM pg_database WHERE datname='testdb'" "testuser" "testdb")
assert_eq "database testdb exists" "1" "${db_exists}"

# Test: user exists
user_exists=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT 1 FROM pg_roles WHERE rolname='testuser'" "testuser" "testdb")
assert_eq "user testuser exists" "1" "${user_exists}"

# Test: write and read data
TEST_VALUE="hello-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE IF NOT EXISTS test_data (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO test_data (value) VALUES ('${TEST_VALUE}')" "testuser" "testdb"
read_val=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT value FROM test_data WHERE value='${TEST_VALUE}'" "testuser" "testdb")
assert_eq "can write and read data" "${TEST_VALUE}" "${read_val}"

# Test: service resolves to the pod
svc_endpoint=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}')
assert_eq "service points to ${POD}" "${POD}" "${svc_endpoint}"

# Test: service port
svc_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.spec.ports[0].port}')
assert_eq "service port is 5432" "5432" "${svc_port}"

# Test: no PDB created (disabled in minimal)
pdb_count=$(kubectl get pdb -n "${NAMESPACE}" -o name 2>/dev/null | wc -l)
assert_eq "no PDB created" "0" "${pdb_count}"

# Test: no repmgr containers
container_names=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.spec.containers[*].name}')
assert_not_contains "no repmgrd container" "${container_names}" "repmgrd"
assert_not_contains "no service-updater container" "${container_names}" "service-updater"

end_suite
print_summary
