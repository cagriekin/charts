#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-config-repmgr}"
RELEASE="${RELEASE:-pg-config-repmgr}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-config-repmgr.yaml")

begin_suite "PostgreSQL Configuration (repmgr with custom config)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-config-repmgr.yaml" \
  --wait --timeout 10m

POD_PRIMARY="${FULLNAME}-0"
POD_REPLICA="${FULLNAME}-1"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# Test: both pods are running
for pod in "${POD_PRIMARY}" "${POD_REPLICA}"; do
  pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "pod ${pod} is Running" "Running" "${pod_phase}"
done

# Test: configuration applied on primary
work_mem=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SHOW work_mem" "testuser" "testdb")
assert_eq "primary: work_mem is 64MB" "64MB" "${work_mem}"

maint_work_mem=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SHOW maintenance_work_mem" "testuser" "testdb")
assert_eq "primary: maintenance_work_mem is 128MB" "128MB" "${maint_work_mem}"

log_stmt=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SHOW log_statement" "testuser" "testdb")
assert_eq "primary: log_statement is all" "all" "${log_stmt}"

# Test: configuration applied on replica
work_mem_replica=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SHOW work_mem" "testuser" "testdb")
assert_eq "replica: work_mem is 64MB" "64MB" "${work_mem_replica}"

maint_work_mem_replica=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SHOW maintenance_work_mem" "testuser" "testdb")
assert_eq "replica: maintenance_work_mem is 128MB" "128MB" "${maint_work_mem_replica}"

# Test: ConfigMap is mounted on both pods
for pod in "${POD_PRIMARY}" "${POD_REPLICA}"; do
  cm_mount=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- ls /etc/postgresql/conf.d/custom.conf 2>/dev/null) && mount_rc=0 || mount_rc=$?
  assert_eq "${pod}: custom.conf mounted" "0" "${mount_rc}"
done

# Test: pg_hba entry injected on primary
hba_content=$(kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c postgresql -- \
  bash -c 'cat $(psql -U testuser -d testdb -t -A -c "SHOW hba_file")' 2>/dev/null)
assert_contains "primary: pg_hba contains custom entry" "${hba_content}" "host all all 10.244.0.0/16 md5"

# Test: upgrade with changed configuration
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-config-repmgr.yaml" \
  --set 'postgresql.configuration.work_mem=128MB' \
  --set 'postgresql.configuration.maintenance_work_mem=256MB' \
  --set 'postgresql.configuration.log_statement=all' \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# After upgrade pods are recycled, verify new config on both
work_mem=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SHOW work_mem" "testuser" "testdb")
assert_eq "primary: work_mem updated to 128MB" "128MB" "${work_mem}"

work_mem_replica=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SHOW work_mem" "testuser" "testdb")
assert_eq "replica: work_mem updated to 128MB" "128MB" "${work_mem_replica}"

end_suite
print_summary
