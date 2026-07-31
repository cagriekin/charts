#!/bin/bash
# Live pgBackRest PITR restore-validation test (#38) + live restore Job test (#226).
# Installs pg with pgbackrest + validation against an in-cluster MinIO (TLS), takes a full
# backup, writes more data and archives it as WAL, then:
#   #38  runs the validation CronJob -- restores the repo into a THROWAWAY PostgreSQL,
#        replays the archived WAL and validates, leaving the live cluster untouched.
#   #226 performs a REAL disaster recovery with the chart's restore Job: scale to 0, wipe
#        the data directory, `kubectl create job --from` the suspended restore CronJob,
#        scale back up, and assert the data came back from S3.
# Proves the backups are restorable end to end AND that the documented restore runbook
# works. OPT-IN / standalone: `make -C pg test-pgbackrest-restore`.
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

begin_suite "pgBackRest PITR restore-validation (#38) + live restore Job (#226)"

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

echo "Installing pg chart with pgbackrest + PITR validation + restore Job enabled..."
# restore.enabled is on from the start: the resource it renders is INERT (a suspended
# CronJob), which is the point -- an operator can leave it enabled and clone it when
# disaster strikes, without a helm upgrade during the incident.
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-pgbackrest-minio.yaml" \
  --set pgbackrest.validation.enabled=true \
  --set pgbackrest.validation.recoveryTimeout=240 \
  --set pgbackrest.validation.backoffLimit=0 \
  --set pgbackrest.restore.enabled=true \
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

# =============================================================================
# #226: first-class restore Job -- a REAL restore over the live data PVC.
# The #38 phase above proves the repository is restorable into a throwaway. This phase
# exercises the path an operator actually walks in a disaster, exactly as the README
# runbook documents it: scale to 0 -> `kubectl create job --from=cronjob/...` -> scale up.
# No hand-built `kubectl run --overrides` restore pod anywhere.
# =============================================================================

# Inert until asked for: enabling restore.enabled must not restore anything by itself.
suspended=$(kubectl get cronjob "${FULLNAME}-pgbackrest-restore" -n "${NAMESPACE}" -o jsonpath='{.spec.suspend}' 2>/dev/null || echo "")
assert_eq "#226: restore CronJob is suspended (never fires on its own)" "true" "${suspended}"
spawned=$(kubectl get jobs -n "${NAMESPACE}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c "^${FULLNAME}-pgbackrest-restore" || true)
assert_eq "#226: suspended restore CronJob spawned no Jobs on its own" "0" "${spawned}"

# The restore must run under the repmgr SA (so s3.keyType=auto would work) with no API
# token, and mount the LIVE PVC -- assert on the live object, not just the rendered YAML.
restore_sa=$(kubectl get cronjob "${FULLNAME}-pgbackrest-restore" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.serviceAccountName}')
assert_eq "#226: restore runs as the repmgr ServiceAccount" "${FULLNAME}-repmgr" "${restore_sa}"
restore_pvc=$(kubectl get cronjob "${FULLNAME}-pgbackrest-restore" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}')
assert_eq "#226: restore mounts the live data PVC of replica 0" "data-${FULLNAME}-0" "${restore_pvc}"

echo "Scaling the StatefulSet to 0 (the destructive step stays explicit and manual)..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=0
kubectl wait --for=delete pod/"${POD}" -n "${NAMESPACE}" --timeout=180s 2>/dev/null || true

# Simulate the disaster the restore is FOR: destroy the data directory outright. Without
# this, a latest-restore over an intact PGDATA would be a near no-op (--delta finds nothing
# to copy) and would prove nothing. Runs as root because the files are owned by uid 101.
# (This throwaway pod is exactly the kind of hand-built spec #226 removes from the restore
# path -- here it is the disaster, not the recovery.)
echo "Destroying the data directory to simulate total data loss..."
kubectl delete pod pg-wipe -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl run pg-wipe -n "${NAMESPACE}" --restart=Never --image=busybox:1.37 \
  --overrides="{\"spec\":{\"containers\":[{\"name\":\"pg-wipe\",\"image\":\"busybox:1.37\",\"command\":[\"sh\",\"-c\",\"rm -rf /data/pgdata && echo wiped\"],\"securityContext\":{\"runAsUser\":0},\"volumeMounts\":[{\"name\":\"data\",\"mountPath\":\"/data\"}]}],\"volumes\":[{\"name\":\"data\",\"persistentVolumeClaim\":{\"claimName\":\"data-${FULLNAME}-0\"}}]}}" >/dev/null
wipe_rc=0
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/pg-wipe -n "${NAMESPACE}" --timeout=180s >/dev/null 2>&1 || wipe_rc=$?
assert_eq "#226: data directory wiped (disaster simulated)" "0" "${wipe_rc}"
kubectl delete pod pg-wipe -n "${NAMESPACE}" --wait=false >/dev/null 2>&1 || true

# --- the runbook: one command, no --overrides ---
echo "Running the chart's restore Job (kubectl create job --from=cronjob/...)..."
kubectl delete job pgbr-restore -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-restore --from=cronjob/"${FULLNAME}-pgbackrest-restore"
# Same complete-or-fail poll as the validation job above: `kubectl wait --for=condition=
# complete` alone burns the whole timeout on a failed Job. backoffLimit=0 => one attempt.
res_rc=2
for _ in $(seq 1 120); do
  succeeded=$(kubectl get job pgbr-restore -n "${NAMESPACE}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "")
  failed=$(kubectl get job pgbr-restore -n "${NAMESPACE}" -o jsonpath='{.status.failed}' 2>/dev/null || echo "")
  [ "${succeeded:-0}" -ge 1 ] 2>/dev/null && { res_rc=0; break; }
  [ "${failed:-0}" -ge 1 ] 2>/dev/null && { res_rc=1; break; }
  sleep 3
done
res_logs=$(kubectl logs -n "${NAMESPACE}" job/pgbr-restore --tail=200 2>/dev/null || true)
if [ "${res_rc}" -ne 0 ]; then
  echo "  restore job did not complete; logs:"; printf '%s\n' "${res_logs}"
fi
assert_eq "#226: restore job completes" "0" "${res_rc}"
assert_contains "#226: restore targeted the LIVE data directory" "${res_logs}" "/var/lib/postgresql/data/pgdata"
assert_contains "#226: restore reports success" "${res_logs}" "pgBackRest restore succeeded"

# --- scale back up: the StatefulSet's own startup performs recovery (WAL replay+promote) ---
echo "Scaling back up: WAL replay + promotion happen under the normal chart entrypoint..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=1
up_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600 || up_rc=$?
if [ "${up_rc}" -ne 0 ]; then
  kubectl logs -n "${NAMESPACE}" "${POD}" -c postgresql --tail=80 2>/dev/null || true
fi
assert_eq "#226: restored cluster becomes ready after scale-up" "0" "${up_rc}"

# The load-bearing assertion: pitr_proof existed ONLY in the wiped data directory and in
# the S3 repo (base backup + archived WAL). Its 5000 rows being back means the Job really
# restored from S3 and the scale-up really replayed WAL and promoted.
restored_rows=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#226: data recovered from S3 after total data-directory loss (5000 rows)" "5000" "${restored_rows}"
in_recovery=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "#226: restored cluster promoted to read-write (not left paused in recovery)" "f" "${in_recovery}"
# A promoted PITR restore lands on a NEW timeline -- proof recovery actually ran here
# rather than the old data directory somehow surviving the wipe.
timeline=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT timeline_id FROM pg_control_checkpoint()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_gt "#226: recovery advanced the timeline (restore + replay really ran)" "${timeline:-0}" "1"

end_suite
print_summary
