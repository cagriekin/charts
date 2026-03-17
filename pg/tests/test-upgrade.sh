#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-upgrade}"
RELEASE="${RELEASE:-pg-upgrade}"
FULLNAME_MINIMAL=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-minimal.yaml")
FULLNAME_REPMGR=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")
FULLNAME_FULL=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-full-test.yaml")

begin_suite "Upgrade (minimal -> repmgr -> full)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# Step 1: Install with minimal values
echo "  Step 1: Installing minimal..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

POD_0="${FULLNAME_MINIMAL}-0"
result=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT 1" "testuser" "testdb")
assert_eq "minimal install works" "1" "${result}"

# Write some data
pg_exec "${NAMESPACE}" "${POD_0}" "CREATE TABLE IF NOT EXISTS upgrade_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_0}" "INSERT INTO upgrade_test (value) VALUES ('pre-upgrade')" "testuser" "testdb"

# Step 2: Upgrade to repmgr
echo ""
echo "  Step 2: Upgrading to repmgr..."
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

POD_0="${FULLNAME_REPMGR}-0"

# Test: both pods running
for pod in "${POD_0}" "${FULLNAME_REPMGR}-1"; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "after repmgr upgrade: ${pod} is Running" "Running" "${phase}"
done

# Test: data survives upgrade
survived=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT value FROM upgrade_test WHERE value='pre-upgrade'" "testuser" "testdb")
assert_eq "data survives upgrade to repmgr" "pre-upgrade" "${survived}"

# Step 3: Upgrade to full (add pgpool + exporter)
echo ""
echo "  Step 3: Upgrading to full..."
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --wait --timeout 10m

POD_0="${FULLNAME_FULL}-0"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_FULL}-pgpool" 300
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_FULL}-prometheus-exporter" 300

# Test: all 3 pg pods running
for i in 0 1 2; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${FULLNAME_FULL}-${i}" -o jsonpath='{.status.phase}')
  assert_eq "after full upgrade: pod-${i} is Running" "Running" "${phase}"
done

# Test: pgpool running
pgpool_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=pgpool" -o jsonpath='{.items[0].metadata.name}')
pgpool_phase=$(kubectl get pod -n "${NAMESPACE}" "${pgpool_pod}" -o jsonpath='{.status.phase}')
assert_eq "pgpool pod is Running after upgrade" "Running" "${pgpool_phase}"

# Test: exporter running
exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=prometheus-exporter" -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "exporter pod is Running after upgrade" "Running" "${exporter_phase}"

# Test: data still survives
survived2=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT value FROM upgrade_test WHERE value='pre-upgrade'" "testuser" "testdb")
assert_eq "data survives full upgrade" "pre-upgrade" "${survived2}"

end_suite
print_summary
