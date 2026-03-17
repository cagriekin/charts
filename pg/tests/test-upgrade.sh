#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-upgrade}"
RELEASE="${RELEASE:-pg-upgrade}"
FULLNAME_FROM=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-upgrade-from.yaml")
FULLNAME_TO=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-upgrade-to.yaml")

begin_suite "Upgrade (repmgr 2-node -> 3-node with pgpool + exporter)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# Step 1: Install with repmgr (2 replicas, persistence enabled)
echo "  Step 1: Installing repmgr (2 nodes)..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-upgrade-from.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

POD_0="${FULLNAME_FROM}-0"
result=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT 1" "testuser" "testdb")
assert_eq "repmgr install works" "1" "${result}"

# Write data before upgrade
UPGRADE_VALUE="pre-upgrade-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD_0}" "CREATE TABLE IF NOT EXISTS upgrade_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_0}" "INSERT INTO upgrade_test (value) VALUES ('${UPGRADE_VALUE}')" "testuser" "testdb"

# Step 2: Upgrade to full (3 replicas + pgpool + exporter, same persistence)
echo ""
echo "  Step 2: Upgrading to full (3 nodes + pgpool + exporter)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-upgrade-to.yaml" \
  --wait --timeout 10m

POD_0="${FULLNAME_TO}-0"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_TO}-pgpool" 300
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_TO}-postgres-exporter" 300

# Test: all 3 pg pods running
for i in 0 1 2; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${FULLNAME_TO}-${i}" -o jsonpath='{.status.phase}')
  assert_eq "after upgrade: pod-${i} is Running" "Running" "${phase}"
done

# Test: pgpool running
pgpool_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=pgpool" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
pgpool_phase=$(kubectl get pod -n "${NAMESPACE}" "${pgpool_pod}" -o jsonpath='{.status.phase}')
assert_eq "pgpool pod is Running after upgrade" "Running" "${pgpool_phase}"

# Test: exporter running
exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=postgres-exporter" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "exporter pod is Running after upgrade" "Running" "${exporter_phase}"

# Test: data written before upgrade survives (persistence enabled)
survived=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT value FROM upgrade_test WHERE value='${UPGRADE_VALUE}'" "testuser" "testdb")
assert_eq "data survives upgrade" "${UPGRADE_VALUE}" "${survived}"

end_suite
print_summary
