#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-backup}"
RELEASE="${RELEASE:-pg-backup}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-backup-test.yaml")
MINIO_RELEASE="minio-test"

begin_suite "Backup and Restore Integration"

# #221 regression guard: the S3 secret key deliberately contains '/' and '+'.
# A prior fix (#167) percent-encoded credentials into an MC_HOST URL, but mc then
# signed SigV4 with the *encoded* secret, so any key with these chars failed every
# upload with a signature mismatch in production. Running the whole backup ->
# validation -> restore path against such a key makes a regression fail the backup
# job here instead of silently in prod. mc alias set (raw argv) handles it fine, so
# the test harness's own setup calls use the same key.
S3_SECRET='9Ea4amnO1POkgnUvz8TC/O9hLv58Ka+n91UW5/ek'

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Deploying MinIO for S3-compatible storage..."
kubectl apply -n "${NAMESPACE}" -f - <<MINIO_MANIFEST
apiVersion: v1
kind: Secret
metadata:
  name: minio-creds
stringData:
  access-key-id: minioadmin
  secret-access-key: "${S3_SECRET}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: minio/minio:RELEASE.2025-02-18T16-25-55Z
          args: ["server", "/data"]
          env:
            - name: MINIO_ROOT_USER
              value: minioadmin
            - name: MINIO_ROOT_PASSWORD
              value: "${S3_SECRET}"
          ports:
            - containerPort: 9000
          readinessProbe:
            httpGet:
              path: /minio/health/ready
              port: 9000
            initialDelaySeconds: 5
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: minio
spec:
  selector:
    app: minio
  ports:
    - port: 9000
      targetPort: 9000
MINIO_MANIFEST

wait_for_deployment_ready "${NAMESPACE}" "minio" 120

echo "Creating S3 bucket..."
kubectl run mc-setup -n "${NAMESPACE}" --restart=Never --image=minio/mc:RELEASE.2024-11-21T17-21-54Z \
  --command -- sh -c "
    mc alias set s3 http://minio:9000 minioadmin '${S3_SECRET}' &&
    mc mb s3/pg-backups || true
  "
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/mc-setup -n "${NAMESPACE}" --timeout=120s
kubectl delete pod mc-setup -n "${NAMESPACE}" --wait=false

echo "Installing pg chart with backup + restore-validation enabled..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-backup-test.yaml" \
  --set backup.validation.enabled=true \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

POD="${FULLNAME}-0"

echo "Inserting test data..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE backup_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO backup_test (value) VALUES ('before-backup-1'), ('before-backup-2'), ('before-backup-3')" "testuser" "testdb"
row_count_before=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM backup_test" "testuser" "testdb")
assert_eq "inserted 3 rows before backup" "3" "${row_count_before}"

# NOTE (#143/#159 coverage): the retention DELETE (mc find --name 'backup_*.dump'
# --older-than ${RETENTION_DAYS}d) and the stale-stage sweep (--older-than 1d) are
# day-granular and mc cannot back-date an S3 object, so they cannot be exercised in a
# minute-scale CI run. Their scoping (per-release subpath + name filter) is asserted at
# the template level in test-template.sh; only the publish/restore path is live here.
echo "Triggering backup job..."
kubectl create job -n "${NAMESPACE}" backup-test --from=cronjob/"${FULLNAME}-backup"

echo "Waiting for backup job to complete..."
kubectl wait --for=condition=complete job/backup-test -n "${NAMESPACE}" --timeout=300s
job_status=$?
assert_eq "backup job completed successfully" "0" "${job_status}"

# #31: drive the restore-validation job end-to-end against the real dump just
# written -- it downloads the latest backup, restores it into a throwaway
# PostgreSQL in the Job pod (uid 999 on the official image + fsGroup), and must
# complete. This is the live coverage the template tests cannot give (it would
# catch e.g. a wrong runAsUser making initdb fail).
echo "Triggering backup-validation job (throwaway restore of the latest dump)..."
kubectl create job -n "${NAMESPACE}" backup-validation-test --from=cronjob/"${FULLNAME}-backup-validation"
val_status=0
kubectl wait --for=condition=complete job/backup-validation-test -n "${NAMESPACE}" --timeout=300s || val_status=$?
if [ "${val_status}" -ne 0 ]; then
  echo "  validation job did not complete; recent logs:"
  kubectl logs -n "${NAMESPACE}" job/backup-validation-test --tail=60 2>/dev/null || true
fi
assert_eq "backup-validation job completed successfully (#31)" "0" "${val_status}"

echo "Verifying backup exists in S3..."
# Dumps are namespaced per release under <prefix>/<fullname>/ (#143), so list that subpath.
kubectl run mc-check -n "${NAMESPACE}" --restart=Never --image=minio/mc:RELEASE.2024-11-21T17-21-54Z \
  --command -- sh -c "
    mc alias set s3 http://minio:9000 minioadmin '${S3_SECRET}' &&
    mc ls s3/pg-backups/backups/${FULLNAME}/ --json | head -1
  "
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/mc-check -n "${NAMESPACE}" --timeout=120s
backup_file=$(kubectl logs -n "${NAMESPACE}" mc-check | tail -1)
kubectl delete pod mc-check -n "${NAMESPACE}" --wait=false

if echo "${backup_file}" | grep -q '"size"'; then
  pass "backup file exists in S3"
else
  fail "backup file exists in S3" "no file found"
fi

echo "Dropping test data..."
pg_exec "${NAMESPACE}" "${POD}" "DROP TABLE backup_test" "testuser" "testdb"
table_exists=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM information_schema.tables WHERE table_name='backup_test'" "testuser" "testdb")
assert_eq "table dropped successfully" "0" "${table_exists}"

echo "Restoring from backup..."
kubectl run mc-fetch -n "${NAMESPACE}" --restart=Never --image=minio/mc:RELEASE.2024-11-21T17-21-54Z \
  --command -- sh -c "sleep 300"
kubectl wait --for=condition=Ready pod/mc-fetch -n "${NAMESPACE}" --timeout=120s
kubectl exec -n "${NAMESPACE}" mc-fetch -- mc alias set s3 http://minio:9000 minioadmin "${S3_SECRET}"
# #159 adversarial decoy: plant a staging object with a FAR-FUTURE timestamp so it is
# lexically newer than the real dump. A correct selection must still reject it (it is a
# .tmp stage), so this proves the .tmp-rejection rather than restating the filter.
kubectl exec -n "${NAMESPACE}" mc-fetch -- sh -c "echo decoy | mc pipe 's3/pg-backups/backups/${FULLNAME}/backup_99999999_999999.dump.tmp'"
# Select the NEWEST published dump, and only a published one: filter to
# backup_<ts>.dump (rejecting any backup_<ts>.dump.tmp staging object, #159) and sort
# descending so the lexically-greatest timestamp (the latest) wins -- the exact
# "restore the latest" path #159 protects.
DUMP_FILE=$(kubectl exec -n "${NAMESPACE}" mc-fetch -- mc ls "s3/pg-backups/backups/${FULLNAME}/" --json \
  | grep -o '"key":"[^"]*"' | cut -d'"' -f4 | grep -E '^backup_.*\.dump$' | sort -r | head -1)
assert_not_contains "#159: a .tmp stage is never selected for restore (even if lexically newest)" "${DUMP_FILE}" ".tmp"
# the decoy carries the far-future timestamp 99999999; assert it was NOT selected, so
# this independently proves the published dump (not just any 'backup_' string) was chosen.
assert_not_contains "#159: the far-future .tmp decoy is not selected" "${DUMP_FILE}" "99999999"
assert_contains "#159: restore target is a published backup_<ts>.dump" "${DUMP_FILE}" "backup_"
kubectl exec -n "${NAMESPACE}" mc-fetch -- mc cat "s3/pg-backups/backups/${FULLNAME}/${DUMP_FILE}" \
  | kubectl exec -i -n "${NAMESPACE}" "${POD}" -c postgresql -- bash -c "cat > /tmp/restore.dump"
kubectl delete pod mc-fetch -n "${NAMESPACE}" --wait=false
kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- bash -c "
  pg_restore -U testuser -d testdb --clean --if-exists /tmp/restore.dump 2>/dev/null || true
  rm -f /tmp/restore.dump
"

echo "Verifying restored data..."
row_count_after=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM backup_test" "testuser" "testdb")
assert_eq "restored 3 rows after backup" "3" "${row_count_after}"

value_check=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT value FROM backup_test ORDER BY id LIMIT 1" "testuser" "testdb")
assert_eq "restored data matches" "before-backup-1" "${value_check}"

end_suite
print_summary
