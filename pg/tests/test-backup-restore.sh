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

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Deploying MinIO for S3-compatible storage..."
kubectl apply -n "${NAMESPACE}" -f - <<'MINIO_MANIFEST'
apiVersion: v1
kind: Secret
metadata:
  name: minio-creds
stringData:
  access-key-id: minioadmin
  secret-access-key: minioadmin
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
              value: minioadmin
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
kubectl run mc-setup -n "${NAMESPACE}" --rm --restart=Never --wait --image=minio/mc:RELEASE.2025-02-18T15-24-42Z \
  --command -- sh -c "
    mc alias set s3 http://minio:9000 minioadmin minioadmin &&
    mc mb s3/pg-backups || true
  "

echo "Installing pg chart with backup enabled..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-backup-test.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300

POD="${FULLNAME}-0"

echo "Inserting test data..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE backup_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO backup_test (value) VALUES ('before-backup-1'), ('before-backup-2'), ('before-backup-3')" "testuser" "testdb"
row_count_before=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM backup_test" "testuser" "testdb")
assert_eq "inserted 3 rows before backup" "3" "${row_count_before}"

echo "Triggering backup job..."
kubectl create job -n "${NAMESPACE}" backup-test --from=cronjob/"${FULLNAME}-backup"

echo "Waiting for backup job to complete..."
kubectl wait --for=condition=complete job/backup-test -n "${NAMESPACE}" --timeout=300s
job_status=$?
assert_eq "backup job completed successfully" "0" "${job_status}"

echo "Verifying backup exists in S3..."
backup_file=$(kubectl run mc-check -n "${NAMESPACE}" --rm --restart=Never --wait --image=minio/mc:RELEASE.2025-02-18T15-24-42Z \
  --command -- sh -c "
    mc alias set s3 http://minio:9000 minioadmin minioadmin &&
    mc ls s3/pg-backups/backups/ --json | head -1
  " 2>/dev/null | tail -1)

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
kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- bash -c "
  apt-get update -qq && apt-get install -y -qq curl > /dev/null
  curl -fsSL https://dl.min.io/client/mc/release/linux-amd64/mc -o /usr/local/bin/mc
  chmod +x /usr/local/bin/mc
  mc alias set s3 http://minio.${NAMESPACE}.svc.cluster.local:9000 minioadmin minioadmin
  DUMP_FILE=\$(mc ls s3/pg-backups/backups/ --json | grep -o '\"key\":\"[^\"]*\"' | head -1 | cut -d'\"' -f4)
  mc cat \"s3/pg-backups/backups/\${DUMP_FILE}\" > /tmp/restore.dump
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
