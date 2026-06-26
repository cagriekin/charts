#!/bin/bash
# Live pgBackRest PITR restore-validation test (#38). Installs pg with pgbackrest +
# validation against an in-cluster MinIO (TLS), takes a full backup, writes more data and
# archives it as WAL, then runs the validation CronJob -- which restores the repo into a
# THROWAWAY PostgreSQL, replays the archived WAL, and validates. Proves the backups are
# actually restorable end to end. OPT-IN / standalone: `make -C pg test-pgbackrest-restore`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-pgbackrest-restore}"
RELEASE="${RELEASE:-pgbrv}"
STANZA="db"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-pgbackrest-minio.yaml")
CERTDIR="$(mktemp -d)"
trap 'rm -rf "${CERTDIR}"' EXIT

begin_suite "pgBackRest PITR restore-validation (#38)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# --- self-signed cert for MinIO TLS (pgbackrest verify-tls is off; cert just needs to exist) ---
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=minio" \
  -addext "subjectAltName=DNS:minio" \
  -keyout "${CERTDIR}/private.key" -out "${CERTDIR}/public.crt" >/dev/null 2>&1
kubectl create secret generic minio-tls -n "${NAMESPACE}" \
  --from-file=public.crt="${CERTDIR}/public.crt" --from-file=private.key="${CERTDIR}/private.key" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic s3-backup-creds -n "${NAMESPACE}" \
  --from-literal=access-key-id=minioadmin --from-literal=secret-access-key=minioadmin \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Deploying MinIO (TLS on :9000, Service exposes :443 -> 9000)..."
kubectl apply -n "${NAMESPACE}" -f - <<'MINIO'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: minio/minio:RELEASE.2025-02-18T16-25-55Z
          args: ["server", "/data", "--certs-dir", "/certs"]
          env:
            - { name: MINIO_ROOT_USER, value: minioadmin }
            - { name: MINIO_ROOT_PASSWORD, value: minioadmin }
          ports: [{ containerPort: 9000 }]
          volumeMounts:
            - { name: certs, mountPath: /certs, readOnly: true }
          readinessProbe:
            httpGet: { path: /minio/health/ready, port: 9000, scheme: HTTPS }
            initialDelaySeconds: 5
            periodSeconds: 5
      volumes:
        - name: certs
          secret: { secretName: minio-tls, defaultMode: 0444 }
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 443, targetPort: 9000 }]
MINIO
wait_for_deployment_ready "${NAMESPACE}" "minio" 180

echo "Creating bucket pgbackrest-test..."
kubectl run mc-setup -n "${NAMESPACE}" --restart=Never --image=minio/mc:RELEASE.2024-11-21T17-21-54Z \
  --command -- sh -c "mc --insecure alias set s3 https://minio:443 minioadmin minioadmin && mc --insecure mb s3/pgbackrest-test || true"
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/mc-setup -n "${NAMESPACE}" --timeout=120s
kubectl delete pod mc-setup -n "${NAMESPACE}" --wait=false

echo "Installing pg chart with pgbackrest + PITR validation enabled..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-pgbackrest-minio.yaml" \
  --set pgbackrest.validation.enabled=true \
  --wait --timeout 8m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600
POD="${FULLNAME}-0"

# --- first full backup: runs stanza-create, after which WAL archiving succeeds ---
echo "Triggering initial full backup (creates the stanza)..."
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [ "${full_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${full_rc}"

# --- write data AFTER the full backup and archive it as WAL, so a correct restore must
#     replay WAL (not just unpack the base backup) to recover it: this is the PITR proof ---
echo "Writing post-backup data and forcing WAL archiving..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE pitr_proof (id bigserial PRIMARY KEY, v text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO pitr_proof (v) SELECT repeat('x',256) FROM generate_series(1,5000)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_switch_wal()" "testuser" "testdb" >/dev/null
# give the archiver a moment to push the switched segment
sleep 10
failed=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT failed_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "WAL archiving healthy before restore (no failed pushes)" "0" "${failed}"

# --- run the validation CronJob: restore repo + replay WAL into a throwaway instance ---
echo "Triggering pgbackrest PITR validation job (throwaway restore + WAL replay)..."
kubectl create job -n "${NAMESPACE}" pgbr-validate --from=cronjob/"${FULLNAME}-pgbackrest-validation"
val_rc=0
kubectl wait --for=condition=complete job/pgbr-validate -n "${NAMESPACE}" --timeout=420s || val_rc=$?
val_logs=$(kubectl logs -n "${NAMESPACE}" job/pgbr-validate --tail=120 2>/dev/null || true)
if [ "${val_rc}" -ne 0 ]; then
  echo "  validation job did not complete; logs:"; printf '%s\n' "${val_logs}"
fi
assert_eq "#38: pgbackrest PITR validation job completes" "0" "${val_rc}"
assert_contains "#38: validation restored + promoted the throwaway instance" "${val_logs}" "recovery completed and promoted"
assert_contains "#38: validation reports success" "${val_logs}" "PITR validation succeeded"

# --- the live cluster is untouched: validation restored into a throwaway, never here ---
live_rows=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#38: live database is intact after validation (5000 rows)" "5000" "${live_rows}"

end_suite
print_summary
