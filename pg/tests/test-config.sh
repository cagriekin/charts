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

# #350: Ready must mean the REAL postmaster. The stock image's init-time server is socket-only
# and used to satisfy the probes, so `helm --wait` could return while the database was still
# being created -- the pg_hba assertion below then read the init-time file. With the probes on
# loopback, Ready implies loopback answers. Checked at the first moment Ready is seen.
assert_loopback_answers "${NAMESPACE}" "${POD}"

probe_lab_350 "${NAMESPACE}" "${POD}"

# #370: the standalone hook reports its outcome through PID 1's stderr, i.e. the container log,
# so a hook that ran late, skipped the rules or failed is visible without the pod events.
hook_log=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql 2>/dev/null | grep "postStart:" || true)
assert_contains "#370: the hook's outcome is in the container log" "${hook_log}" "real server up on 127.0.0.1 after"
assert_contains "#370: it names the pg_hba entries it wrote" "${hook_log}" "1 pg_hba entries written"
assert_eq "#370: no FailedPostStartHook event for the pod" "" "$(kubectl get events -n "${NAMESPACE}" --field-selector reason=FailedPostStartHook,involvedObject.name="${POD}" -o name 2>/dev/null)"

# Test: configuration parameters are applied
work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW work_mem" "testuser" "testdb")
assert_eq "work_mem is 64MB" "64MB" "${work_mem}"

# conf.d must be loaded by the FIRST server start of a fresh install, not by a later reload:
# postmaster-level settings (max_connections here; reserved_connections, which pgpool's check
# role depends on) only apply at start. A conf.d added after the start shows the new value as
# pending_restart with the old one still live.
max_conn=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT setting || ':' || pending_restart FROM pg_settings WHERE name = 'max_connections'" "testuser" "testdb")
assert_eq "postmaster-level conf.d setting is live at first start (max_connections=120, no pending restart)" "120:false" "${max_conn}"

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
# Diagnostics for the historical flake on this assertion (#350): the entries are written by
# the standalone postStart hook, so show what the file holds and whether the hook ran late.
if ! grep -q "host all all 10.244.0.0/16 md5" <<< "${hba_content}"; then
  echo "  --- pg_hba.conf as read ---"; printf '%s\n' "${hba_content}" | grep -vE '^\s*(#|$)' | head -20
  echo "  --- container timing ---"
  kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{range .status.containerStatuses[?(@.name=="postgresql")]}started={.state.running.startedAt} ready={.ready} restarts={.restartCount}{"\n"}{end}'
  kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}readyAt={.lastTransitionTime}{"\n"}{end}'
  kubectl get events -n "${NAMESPACE}" --field-selector involvedObject.name="${POD}" -o custom-columns='T:.lastTimestamp,R:.reason,M:.message' 2>/dev/null | tail -12
  kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql --timestamps 2>/dev/null | grep -iE "postStart|hba|ready to accept|shutting down|init" | head -12
fi

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
# #370: a restart on the same PVC finds the rules already in place (the awk re-emit is idempotent).
hook_log2=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql 2>/dev/null | grep "postStart:" || true)
assert_contains "#370: the restarted container's hook found the pg_hba entries already in place" "${hook_log2}" "1 pg_hba entries already in place"

maint_work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW maintenance_work_mem" "testuser" "testdb")
assert_eq "maintenance_work_mem updated to 256MB after upgrade" "256MB" "${maint_work_mem}"

# Test: disabling configuration after enable must not brick restarts (#107).
# include_dir persists in PGDATA on the PVC; without the cleanup the pod
# crash-loops on the missing conf.d directory once the mount is removed.
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-config.yaml" \
  --set 'postgresql.configuration=null' \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.phase}')
assert_eq "pod Running after configuration disabled" "Running" "${pod_phase}"

# The conf.d ConfigMap always renders now (it carries reserved-connections.conf), so the
# mount never goes away and include_dir is kept rather than stripped: it can no longer point
# at a missing directory, which is what #107 guarded against. Exact match: stock initdb
# postgresql.conf ships a commented "#include_dir = '...'" documentation line that must not
# count.
include_rc=0
kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
  grep -q "^include_dir = '/etc/postgresql/conf.d'" /var/lib/postgresql/data/pgdata/postgresql.conf 2>/dev/null || include_rc=$?
assert_eq "include_dir kept: conf.d stays mounted with configuration disabled" "0" "${include_rc}"

# By source file, not value: standalone's 3 is also PostgreSQL's default, so the value alone
# would pass with the chart's file ignored.
su_reserved_src=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT sourcefile FROM pg_settings WHERE name = 'superuser_reserved_connections'" "testuser" "testdb")
assert_eq "superuser_reserved_connections comes from the chart's reserved-connections.conf" "/etc/postgresql/conf.d/reserved-connections.conf" "${su_reserved_src}"

work_mem=$(pg_exec "${NAMESPACE}" "${POD}" "SHOW work_mem" "testuser" "testdb")
assert_eq "work_mem reverted to default after disable" "4MB" "${work_mem}"

# --- #369: with log_statement=all the standalone hook's managed-role passwords never log ---
# The hook sends the plaintext to the server (password_encryption hashes it); it must appear in
# no logged statement: the hook lowers log_statement first and the password rides only in an
# ALTER ROLE, never in a SET. The fixture's log_statement=all is the strongest setting there is.
app_pw=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.data.app-password}' | base64 -d)
chk_pw=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.data.pgpool-check-password}' | base64 -d)
pod_log=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql 2>/dev/null || true)
assert_eq "#369: no plaintext managed-role password in the standalone log under log_statement=all" "0" "$(( $(grep -cF -- "${app_pw}" <<< "${pod_log}" || true) + $(grep -cF -- "${chk_pw}" <<< "${pod_log}" || true) ))"
assert_eq "#369: no secret-bearing SET in the standalone log" "0" "$(grep -c "SET myvars\.[a-z_]*\(secret\|pw\)" <<< "${pod_log}" || true)"
assert_contains "#369: the hook provisioned the managed roles on this start" "${pod_log}" "postStart: real server up on 127.0.0.1 after [0-9]*s; applied: .*managed roles app_user, pgpool_check"

end_suite
print_summary
