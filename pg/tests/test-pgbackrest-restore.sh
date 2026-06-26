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
  --set pgbackrest.validation.recoveryTimeout=240 \
  --set pgbackrest.validation.backoffLimit=0 \
  --wait --timeout 8m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600
POD="${FULLNAME}-0"

# --- first full backup: runs stanza-create, after which WAL archiving succeeds ---
echo "Triggering initial full backup (creates the stanza)..."
# --ignore-not-found so the standalone target can be rerun in the same namespace.
kubectl delete job pgbr-full -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [ "${full_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${full_rc}"

# archive_command runs `pgbackrest archive-push` from pod startup, but those pushes FAIL
# until the stanza exists (created by the full backup above), bumping pg_stat_archiver
# .failed_count. Reset the archiver stats now so the failed_count assertion below measures
# only post-stanza pushes (mirrors test-backup-concurrent.sh).
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_stat_reset_shared('archiver')" "testuser" "testdb" >/dev/null 2>&1 || true

# --- write data AFTER the full backup and archive it as WAL, so a correct restore must
#     replay WAL (not just unpack the base backup) to recover it: this is the PITR proof.
#     pitr_proof did not exist at backup time, so it can ONLY appear in the throwaway
#     restore if the post-backup WAL was archived and replayed. ---
echo "Writing post-backup data and forcing WAL archiving..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE pitr_proof (id bigserial PRIMARY KEY, v text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO pitr_proof (v) SELECT repeat('x',256) FROM generate_series(1,5000)" "testuser" "testdb"
# Capture the CURRENT segment (the one holding pitr_proof) BEFORE switching, then switch:
# pg_switch_wal() closes that segment so it gets archived. NOTE pg_walfile_name(pg_switch_wal())
# would name the NEXT segment (the new current one), which is not archived until more WAL is
# written -- so we name the pre-switch segment and wait for IT (or later) to land in the repo.
seg=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_walfile_name(pg_current_wal_lsn())" "testuser" "testdb" | tr -d '[:space:]')
pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_switch_wal()" "testuser" "testdb" >/dev/null
echo "Closed WAL segment ${seg} (holds pitr_proof); waiting for it to be archived..."
archived_ok=""
for _ in $(seq 1 90); do
  last=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT COALESCE(last_archived_wal,'')" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  # last_archived_wal >= seg (lexical compare is valid for WAL filenames on one timeline)
  if [ -n "${last}" ] && [ ! "${last}" \< "${seg}" ]; then archived_ok="yes"; break; fi
  sleep 2
done
failed=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT failed_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "WAL archiving healthy before restore (no failed pushes)" "0" "${failed}"
# Best-effort wait only: the archiver can lag (it backs off after the pre-stanza push
# failures), so don't hard-fail if the segment isn't confirmed within the window. The
# load-bearing proof is the "1 table-like relation(s)" assertion below -- if pitr_proof's
# WAL was not archived+replayed, the restore won't contain it and that assertion fails.
if [ "${archived_ok}" != "yes" ]; then
  echo "  WARN: post-backup WAL not confirmed archived within the wait window; proceeding (the restored-relation assertion is the real proof)"
fi

# --- run the validation CronJob: restore repo + replay WAL into a throwaway instance ---
echo "Triggering pgbackrest PITR validation job (throwaway restore + WAL replay)..."
kubectl delete job pgbr-validate -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-validate --from=cronjob/"${FULLNAME}-pgbackrest-validation"
# Wait for the job to either complete OR fail. `kubectl wait --for=condition=complete`
# alone blocks the full timeout on a FAILED job (it only ever satisfies on success), which
# is what made a failing run burn 5 min/attempt. backoffLimit=0 means one pod attempt, so
# .status.failed flips fast on a startup error.
val_rc=2
for _ in $(seq 1 100); do
  succeeded=$(kubectl get job pgbr-validate -n "${NAMESPACE}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "")
  failed=$(kubectl get job pgbr-validate -n "${NAMESPACE}" -o jsonpath='{.status.failed}' 2>/dev/null || echo "")
  [ "${succeeded:-0}" -ge 1 ] 2>/dev/null && { val_rc=0; break; }
  [ "${failed:-0}" -ge 1 ] 2>/dev/null && { val_rc=1; break; }
  sleep 3
done
val_logs=$(kubectl logs -n "${NAMESPACE}" job/pgbr-validate --tail=200 2>/dev/null || true)
if [ "${val_rc}" -ne 0 ]; then
  echo "  validation job did not complete; logs:"; printf '%s\n' "${val_logs}"
fi
assert_eq "#38: pgbackrest PITR validation job completes" "0" "${val_rc}"
assert_contains "#38: validation restored + promoted the throwaway instance" "${val_logs}" "recovery completed and promoted"
assert_contains "#38: validation reports success" "${val_logs}" "PITR validation succeeded"
# The throwaway restored testdb is reported by the validation job. pitr_proof is the only
# user table in testdb and was created AFTER the base backup, so the job logging exactly
# "1 table-like relation(s)" proves WAL was replayed -- not just the base backup unpacked.
# (The job validates POSTGRES_DB == testdb from the secret.) An exact match avoids the
# substring collision a "0 table-like relation" check would have with 10/20/... .
assert_contains "#38: WAL replay restored the post-backup table (not just the base backup)" "${val_logs}" "1 table-like relation(s)"

# --- the live cluster is untouched: validation restored into a throwaway, never here ---
live_rows=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#38: live database is intact after validation (5000 rows)" "5000" "${live_rows}"

end_suite
print_summary
