#!/bin/bash
# Live concurrent backup + WAL-archiving test (#34). pgbackrest archives WAL via
# archive_command from the postgresql container while a `pgbackrest backup` runs in the
# pgbackrest sidecar; this proves the two do not conflict -- the backup completes AND WAL
# archiving stays healthy (no failed pushes) across the backup window. Uses an in-cluster
# MinIO (TLS) as the S3 repo. OPT-IN / standalone: `make -C pg test-backup-concurrent`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-backup-concurrent}"
RELEASE="${RELEASE:-pgbrc}"
STANZA="db"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-pgbackrest-minio.yaml")

begin_suite "Concurrent backup + WAL archiving (pgbackrest, #34)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# MinIO + the S3 credential Secret + the bucket (shared with the pgbackrest restore suites).
deploy_minio "${NAMESPACE}"

echo "Installing pg chart with pgbackrest -> MinIO..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-pgbackrest-minio.yaml" \
  --wait --timeout 8m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600
POD="${FULLNAME}-0"

# --- first full backup: this runs stanza-create, after which WAL archiving succeeds ---
echo "Triggering initial full backup (creates the stanza)..."
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [ "${full_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${full_rc}"

# --- #343: the Job recorded the repository state where a controller can read it ---
INFO_CM="${FULLNAME}-pgbackrest-info"
info_json() { kubectl get configmap -n "${NAMESPACE}" "${INFO_CM}" -o jsonpath='{.data.info\.json}' 2>/dev/null || true; }
info_ann() { kubectl get configmap -n "${NAMESPACE}" "${INFO_CM}" -o go-template="{{index .metadata.annotations \"pg-ha/$1\"}}" 2>/dev/null || true; }
full_info=$(info_json)
# pgBackRest's document is an array of stanzas; `st` is ours.
st='.[] | select(.name == "'"${STANZA}"'")'
assert_eq "#343: info.json is pgBackRest's stanza array" "true" "$(jq -e 'type == "array"' <<< "${full_info}" 2>/dev/null || echo false)"
assert_eq "#343: status.code is 0 after the full backup" "0" "$(jq -r "${st} | .status.code" <<< "${full_info}" 2>/dev/null)"
assert_eq "#343: one backup set recorded" "1" "$(jq -r "${st} | .backup | length" <<< "${full_info}" 2>/dev/null)"
assert_eq "#343: the set is the full backup" "full" "$(jq -r "${st} | .backup[0].type" <<< "${full_info}" 2>/dev/null)"
assert_gt "#343: repository size is recorded" "$(jq -r "${st} | .backup[0].info.repository.size // 0" <<< "${full_info}" 2>/dev/null)" "0"
assert_eq "#343: backup-type annotation" "full" "$(info_ann backup-type)"
assert_eq "#343: status-code annotation" "0" "$(info_ann status-code)"
assert_eq "#343: primary annotation names the pod the backup ran on" "${POD}" "$(info_ann primary)"
assert_eq "#343: stanza annotation" "${STANZA}" "$(info_ann stanza)"
full_recorded_at=$(info_ann recorded-at)
assert_contains "#343: recorded-at is RFC3339 UTC" "${full_recorded_at}" '^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z$'
assert_contains "#343: the Job log carries the one-line summary" "$(kubectl logs -n "${NAMESPACE}" job/pgbr-full 2>/dev/null)" "Recorded repository state in configmap/${INFO_CM}: status=0 (ok) backups=1"
# The grant is the smallest that can write the record: get/patch on this one ConfigMap, from
# a ServiceAccount that can exec into the database pods (#134) and must get nothing wider.
PGBR_SA="system:serviceaccount:${NAMESPACE}:${FULLNAME}-pgbackrest"
sa_can() { kubectl auth can-i "$1" "$2" -n "${NAMESPACE}" --as="${PGBR_SA}" 2>/dev/null | tail -1; }
assert_eq "#343 RBAC: pgbackrest SA can patch the info ConfigMap" "yes" "$(sa_can patch "configmaps/${INFO_CM}")"
assert_eq "#343 RBAC: ... but not the pgbackrest config ConfigMap" "no" "$(sa_can patch "configmaps/${FULLNAME}-pgbackrest")"
assert_eq "#343 RBAC: ... nor the primary marker" "no" "$(sa_can patch "configmaps/${FULLNAME}-primary")"
assert_eq "#343 RBAC: ... and cannot create ConfigMaps" "no" "$(sa_can create configmaps)"
assert_eq "#343 RBAC: ... nor update the record (patch is enough)" "no" "$(sa_can update "configmaps/${INFO_CM}")"

# clean archiver stats so the concurrent-window failed_count baseline is the post-stanza
# state (archive_command fails before the stanza exists; that is not what we measure here)
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_stat_reset_shared('archiver')" "testuser" "testdb" >/dev/null 2>&1 || true
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE IF NOT EXISTS wal_load (id bigserial PRIMARY KEY, v text)" "testuser" "testdb"

# --- sustained WAL load (inserts + forced segment switches) running CONCURRENTLY ---
echo "Starting sustained WAL load + concurrent diff backup..."
(
  for _ in $(seq 1 40); do
    pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO wal_load (v) SELECT repeat('x',512) FROM generate_series(1,2000)" "testuser" "testdb" >/dev/null 2>&1 || true
    pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_switch_wal()" "testuser" "testdb" >/dev/null 2>&1 || true
    sleep 1
  done
) &
LOAD_PID=$!

# trigger a diff backup WHILE the load is archiving WAL
kubectl create job -n "${NAMESPACE}" pgbr-diff --from=cronjob/"${FULLNAME}-pgbackrest-diff"

# concurrently (WAL load + pgbackrest physical backup + WAL archiving all active): run a
# logical pg_dump and assert it succeeds. #34 covers BOTH backup paths running together --
# the physical (pgbackrest) and logical (pg_dump) backups must not interfere with each
# other or with WAL archiving (Qodo: the pg_dump + WAL-archiving concurrency scenario).
echo "  Running a concurrent pg_dump during the backup + WAL-archiving window..."
dump_rc=0
dump_lines=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
  pg_dump -U testuser -d testdb 2>/dev/null | wc -l) || dump_rc=$?

diff_rc=0
kubectl wait --for=condition=complete job/pgbr-diff -n "${NAMESPACE}" --timeout=300s || diff_rc=$?
if [ "${diff_rc}" -ne 0 ]; then
  echo "  diff-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-diff --tail=80 2>/dev/null || true
fi
kill "${LOAD_PID}" 2>/dev/null || true; wait "${LOAD_PID}" 2>/dev/null || true

assert_eq "#34: concurrent diff backup completes during active WAL archiving" "0" "${diff_rc}"
assert_eq "#34: concurrent pg_dump completes during the backup + WAL-archiving window" "0" "${dump_rc}"
assert_gt "#34: pg_dump produced a non-trivial logical backup" "${dump_lines:-0}" "20"

# --- both backups are in the repo ---
info=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c pgbackrest -- pgbackrest --stanza="${STANZA}" info 2>&1 || true)
n_full=$(printf '%s\n' "${info}" | grep -c "full backup:" || true)
n_diff=$(printf '%s\n' "${info}" | grep -c "diff backup:" || true)
assert_gt "#34: repository has a full backup" "${n_full}" "0"
assert_gt "#34: repository has the concurrent diff backup" "${n_diff}" "0"

# --- #343: the diff run refreshed the record ---
diff_info=$(info_json)
assert_eq "#343: two backup sets recorded after the diff" "2" "$(jq -r "${st} | .backup | length" <<< "${diff_info}" 2>/dev/null)"
assert_eq "#343: the newest set is the diff" "diff" "$(jq -r "${st} | .backup[-1].type" <<< "${diff_info}" 2>/dev/null)"
assert_eq "#343: the diff names its prior full" "$(jq -r "${st} | .backup[0].label" <<< "${diff_info}" 2>/dev/null)" "$(jq -r "${st} | .backup[-1].prior" <<< "${diff_info}" 2>/dev/null)"
assert_eq "#343: backup-type annotation follows the run" "diff" "$(info_ann backup-type)"
assert_eq "#343: status.code still 0" "0" "$(info_ann status-code)"
assert_not_eq "#343: recorded-at advanced" "${full_recorded_at}" "$(info_ann recorded-at)"

# --- WAL archiving stayed healthy across the backup: no failed pushes, archiver advanced ---
failed=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT failed_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
archived=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT archived_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
last_failed=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT COALESCE(last_failed_wal,'')" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#34: no WAL archive failures during the concurrent backup" "0" "${failed}"
assert_gt "#34: WAL segments were archived during the window" "${archived:-0}" "0"
assert_eq "#34: no last_failed_wal recorded" "" "${last_failed}"

end_suite
print_summary
