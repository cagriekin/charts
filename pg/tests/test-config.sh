#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-config}"
RELEASE="${RELEASE:-pg-config}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-config.yaml")

begin_suite "PostgreSQL Configuration (standalone with custom config)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-config.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

POD="${FULLNAME}-0"

# Test: pod is running
pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.phase}')
assert_eq "pod ${POD} is Running" "Running" "${pod_phase}"

# Test: configuration parameters are applied
work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW work_mem" "testuser" "testdb")
assert_eq "work_mem is 64MB" "64MB" "${work_mem}"

maint_work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW maintenance_work_mem" "testuser" "testdb")
assert_eq "maintenance_work_mem is 128MB" "128MB" "${maint_work_mem}"

log_stmt=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW log_statement" "testuser" "testdb")
assert_eq "log_statement is all" "all" "${log_stmt}"

# Test: ConfigMap is mounted
cm_mount=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- ls /etc/postgresql/conf.d/custom.conf 2>/dev/null) && mount_rc=0 || mount_rc=$?
assert_eq "custom.conf mounted at /etc/postgresql/conf.d" "0" "${mount_rc}"

# Test: pg_hba entry was injected
hba_content=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
  bash -c 'cat $(psql -U testuser -d testdb -t -A -c "SHOW hba_file")' 2>/dev/null)
assert_contains "pg_hba contains custom entry" "${hba_content}" "host all all 10.244.0.0/16 md5"

# Test: upgrade with changed configuration (reload-only param)
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-config.yaml" \
  --set 'postgresql.configuration.work_mem=128MB' \
  --set 'postgresql.configuration.maintenance_work_mem=256MB' \
  --set 'postgresql.configuration.log_statement=all' \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

# After upgrade the pod is recycled due to checksum change, so new config should be active
work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW work_mem" "testuser" "testdb")
assert_eq "work_mem updated to 128MB after upgrade" "128MB" "${work_mem}"

maint_work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW maintenance_work_mem" "testuser" "testdb")
assert_eq "maintenance_work_mem updated to 256MB after upgrade" "256MB" "${maint_work_mem}"

end_suite
print_summary
