#!/bin/bash
# Live bootstrap-from-backup test (#266). Proves the failure this feature exists for:
# replica 0 loses its PVC entirely, and instead of the entrypoint initdb-ing a BRAND NEW EMPTY
# cluster (backups intact in S3, live database silently empty), the bootstrap init container
# restores from the repository and PostgreSQL replays the archived WAL on startup.
#
# The load-bearing assertion is the PostgreSQL *system identifier*: a fresh initdb mints a new
# one, so an unchanged identifier proves the original cluster came back rather than an empty
# look-alike. The PVC uid is checked too, so the test cannot pass because the volume survived.
#
# Also asserts the idempotency AC: a subsequent pod restart with the PVC intact must NOT
# re-restore -- it must see the completion marker and leave the data directory alone.
#
# OPT-IN / standalone: `make -C pg test-pgbackrest-bootstrap`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-pgbackrest-bootstrap}"
RELEASE="${RELEASE:-pgbrbs}"
VALUES="${SCRIPT_DIR}/values-pgbackrest-minio.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
POD="${FULLNAME}-0"
PVC="data-${FULLNAME}-0"

begin_suite "pgBackRest bootstrap-from-backup (#266)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
deploy_minio "${NAMESPACE}"

echo "Installing pg chart with pgbackrest + bootstrap-from-backup enabled..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${VALUES}" \
  --set pgbackrest.bootstrap.enabled=true \
  --wait --timeout 8m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600

# On a FIRST install the data directory is empty but the repository has no stanza yet, so the
# bootstrap must be a clean no-op that does not block startup -- if it hard-failed here, the
# pod would never come up at all (which the readiness wait above already proves).
# (`restore` with no backup exits non-zero, and Kubernetes retries a failed init container
# forever -- so getting this wrong would mean no pod EVER starts on a fresh install.)
first_boot_logs=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c pgbackrest-bootstrap 2>/dev/null || true)
assert_contains "#266: first install detects the empty repository" "${first_boot_logs}" "no backup for stanza"
assert_contains "#266: first install falls through to a normal initialization" "${first_boot_logs}" "initializes normally"

# --- first full backup: runs stanza-create, after which WAL archiving succeeds ---
echo "Triggering initial full backup (creates the stanza)..."
kubectl delete job pgbr-full -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [ "${full_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${full_rc}"
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_stat_reset_shared('archiver')" "testuser" "testdb" >/dev/null 2>&1 || true

# --- data written AFTER the backup, archived as WAL: recovering it requires WAL replay, not
#     just unpacking the base backup ---
echo "Writing post-backup data and archiving it as WAL..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE pitr_proof (id bigserial PRIMARY KEY, v text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO pitr_proof (v) SELECT repeat('x',256) FROM generate_series(1,5000)" "testuser" "testdb"
seg=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_walfile_name(pg_current_wal_lsn())" "testuser" "testdb" | tr -d '[:space:]')
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_switch_wal()" "testuser" "testdb" >/dev/null
for _ in $(seq 1 90); do
  last=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT COALESCE(last_archived_wal,'')" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  if [ -n "${last}" ] && [ ! "${last}" \< "${seg}" ]; then break; fi
  sleep 2
done
failed=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT failed_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "WAL archiving healthy before the simulated PVC loss" "0" "${failed}"

# Identity of the cluster we must get back. A fresh initdb would produce a different one.
sysid_before=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT system_identifier FROM pg_control_system()" "testuser" "testdb" | tr -d '[:space:]')
pvc_uid_before=$(kubectl get pvc "${PVC}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}')
echo "Cluster system identifier before loss: ${sysid_before} (PVC uid ${pvc_uid_before})"

# =============================================================================
# Simulate TOTAL loss of replica 0's volume -- the node died and took the disk with it.
# =============================================================================
echo "Scaling to 0 and deleting the data PVC outright..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=0
kubectl wait --for=delete pod/"${POD}" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true
# Delete only after the pod is gone: the pvc-protection finalizer would otherwise hold the PVC
# in Terminating and the recreated pod could re-bind it, hiding the very loss we are simulating.
kubectl delete pvc "${PVC}" -n "${NAMESPACE}" --wait=true --timeout=300s >/dev/null 2>&1 || true
kubectl wait --for=delete pvc/"${PVC}" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true
gone=$(kubectl get pvc "${PVC}" -n "${NAMESPACE}" --ignore-not-found -o name 2>/dev/null || true)
assert_eq "data PVC is really gone (loss simulated)" "" "${gone}"

echo "Scaling back up: the bootstrap init container should restore from S3..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=1
boot_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600 || boot_rc=$?
if [ "${boot_rc}" -ne 0 ]; then
  kubectl get pod "${POD}" -n "${NAMESPACE}" -o wide 2>/dev/null || true
  kubectl logs -n "${NAMESPACE}" "${POD}" -c pgbackrest-bootstrap --tail=60 2>/dev/null || true
  kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql --tail=60 2>/dev/null || true
fi
assert_eq "pod becomes ready after PVC loss" "0" "${boot_rc}"

# The volume really is new...
pvc_uid_after=$(kubectl get pvc "${PVC}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
if [ -n "${pvc_uid_after}" ] && [ "${pvc_uid_after}" != "${pvc_uid_before}" ]; then
  pass "a fresh PVC was provisioned (uid changed)"
else
  fail "a fresh PVC was provisioned (uid changed)" "uid before=${pvc_uid_before} after=${pvc_uid_after}: the old volume survived, so this run proves nothing"
fi
# ...and the bootstrap, not initdb, populated it.
boot_logs=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c pgbackrest-bootstrap 2>/dev/null || true)
assert_contains "#266: bootstrap restored into the empty data directory" "${boot_logs}" "into the empty data directory"
assert_contains "#266: bootstrap reports success" "${boot_logs}" "Bootstrap succeeded"

# THE assertion: same cluster, not a new empty one.
sysid_after=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT system_identifier FROM pg_control_system()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: the ORIGINAL cluster came back (system identifier unchanged)" "${sysid_before}" "${sysid_after}"
rows_after=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: post-backup data recovered via WAL replay (5000 rows)" "5000" "${rows_after}"
in_recovery=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: bootstrapped node promoted to read-write" "f" "${in_recovery}"
# New writes work: it is a functioning primary, not a read-only leftover.
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE post_bootstrap_write (id int)" "testuser" "testdb" >/dev/null
# INSERT then SELECT rather than INSERT ... RETURNING: psql prints the command tag
# ("INSERT 0 1") alongside the returned row, which a whitespace strip would glue onto the value.
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO post_bootstrap_write VALUES (7)" "testuser" "testdb" >/dev/null
wrote=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT id FROM post_bootstrap_write" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: bootstrapped primary accepts writes" "7" "${wrote}"

# =============================================================================
# Idempotency AC: an ordinary restart must NOT re-restore. The marker lives in PGDATA, which
# survives a pod restart, so the bootstrap must skip and leave the data directory untouched.
# =============================================================================
echo "Restarting the pod with the PVC intact: the bootstrap must skip..."
kubectl delete pod "${POD}" -n "${NAMESPACE}" --wait=true >/dev/null
restart_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600 || restart_rc=$?
assert_eq "pod becomes ready after a plain restart" "0" "${restart_rc}"
boot_logs2=$(kubectl logs -n "${NAMESPACE}" "${POD}" -c pgbackrest-bootstrap 2>/dev/null || true)
assert_contains "#266: restart is a no-op (marker seen, already completed)" "${boot_logs2}" "Bootstrap already completed"
assert_not_contains "#266: restart did NOT re-restore" "${boot_logs2}" "into the empty data directory"
# Nothing was lost or rewound by the restart -- including the write made after bootstrapping.
sysid_restart=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT system_identifier FROM pg_control_system()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: system identifier stable across the restart" "${sysid_before}" "${sysid_restart}"
kept=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT id FROM post_bootstrap_write" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#266: the post-bootstrap write survived the restart (no re-restore)" "7" "${kept}"

end_suite
print_summary
