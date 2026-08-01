#!/bin/bash
# API-driven pgBackRest restore through the agent control API (#276).
#
# The unit tests cover the request logic with fakes; only a live cluster can prove the
# parts that depend on the cluster itself:
#   * the RBAC actually granted is sufficient -- the pods' ServiceAccount can read the
#     restore CronJob and create the Job (a missing verb is invisible to a fake client);
#   * the CLONED Job really runs, i.e. what the agent builds from the CronJob's
#     jobTemplate is a working restore, not merely a well-formed object;
#   * restore.sh's outcome record lands on the data volume and the agent reads it back
#     as lastRestore AFTER the scale-down/up cycle -- the whole point of that file, since
#     by then the Job may be gone;
#   * WAL-replay progress is visible on GET /v1/status once the cluster is back, which is
#     the progress signal that survives scaling to 0.
#
# It also documents the honest shape of the flow: the API removes the `kubectl create job
# --from` step and nothing else. Scaling to 0 deletes every agent, so the API cannot do it
# and cannot report progress while the copy runs.
#
# OPT-IN / standalone: `make -C pg test-agent-control-restore`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-control-restore}"
RELEASE="${RELEASE:-pgctlres}"
VALUES="${SCRIPT_DIR}/values-agent-control-restore.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
MARKER="${FULLNAME}-primary"
HEADLESS="${FULLNAME}-headless"
TLS_SECRET="pg-control-restore-tls"
API_JOB="${FULLNAME}-pgbackrest-restore-api"
LOCAL_PORT="${LOCAL_PORT:-19202}"
POD0="${FULLNAME}-0"

begin_suite "Agent control API: pgBackRest restore (#276)"

if ! command -v jq >/dev/null 2>&1; then
  fail "jq is required to parse the control API responses" "install jq"
  end_suite; print_summary; exit 1
fi

CERTDIR=$(mktemp -d)
cleanup() {
  [[ -n "${PF_PID:-}" ]] && kill "${PF_PID}" 2>/dev/null || true
  rm -rf "${CERTDIR}"
}
trap cleanup EXIT

gen_ca() {
  openssl genpkey -algorithm RSA -out "${CERTDIR}/ca.key" >/dev/null 2>&1
  openssl req -x509 -new -key "${CERTDIR}/ca.key" -days 3650 -subj "/CN=pg-control-ca" \
    -out "${CERTDIR}/ca.crt" >/dev/null 2>&1
}
gen_cert() { # gen_cert <name> <CN> <san-or-empty> <eku>
  local name="$1" cn="$2" san="$3" eku="$4"
  openssl genpkey -algorithm RSA -out "${CERTDIR}/${name}.key" >/dev/null 2>&1
  openssl req -new -key "${CERTDIR}/${name}.key" -subj "/CN=${cn}" -out "${CERTDIR}/${name}.csr" >/dev/null 2>&1
  { [[ -n "${san}" ]] && echo "subjectAltName=${san}"; echo "extendedKeyUsage=${eku}"; } > "${CERTDIR}/${name}.ext"
  openssl x509 -req -in "${CERTDIR}/${name}.csr" -CA "${CERTDIR}/ca.crt" -CAkey "${CERTDIR}/ca.key" \
    -CAcreateserial -days 3650 -extfile "${CERTDIR}/${name}.ext" -out "${CERTDIR}/${name}.crt" >/dev/null 2>&1
}
gen_ca
gen_cert server "${FULLNAME}" "DNS:*.${HEADLESS}.${NAMESPACE}.svc.cluster.local,DNS:localhost,IP:127.0.0.1" "serverAuth"
gen_cert ops "ops-admin" "" "clientAuth"          # control verbs, but NOT restore
gen_cert dba "dba-break-glass" "" "clientAuth"    # the restore identity
certs_ok=$([[ -s "${CERTDIR}/server.crt" && -s "${CERTDIR}/ops.crt" && -s "${CERTDIR}/dba.crt" ]] && echo ok || echo fail)
assert_eq "test PKI generated" "ok" "${certs_ok}"

# --- install ---

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
deploy_minio "${NAMESPACE}"

helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
kubectl delete job "${API_JOB}" -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
# The timeline-highwater marker is agent-created (not helm-managed), so it survives
# uninstall; a stale marker from a prior run's switchover (timeline > the fresh PVCs')
# trips the #125 guard and the new primary refuses to start read-write. Drop it so a
# same-namespace re-run starts clean (CI runs in a fresh namespace and never hits this).
kubectl delete configmap "${FULLNAME}-primary" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
kubectl delete lease "${FULLNAME}-leader" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

kubectl create secret generic "${TLS_SECRET}" -n "${NAMESPACE}" \
  --from-file=tls.crt="${CERTDIR}/server.crt" \
  --from-file=tls.key="${CERTDIR}/server.key" \
  --from-file=ca.crt="${CERTDIR}/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Installing pg chart (agent mode, control API + restore triggering)..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${VALUES}" --set pgbackrest.restore.enabled=true --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 900

# --- data + a backup to restore from ---

pg_exec "${NAMESPACE}" "${POD0}" "CREATE TABLE api_restore_proof AS SELECT g AS id FROM generate_series(1,5000) g;" >/dev/null
rows_before=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT count(*) FROM api_restore_proof" | tr -d '[:space:]')
assert_eq "baseline data written" "5000" "${rows_before}"

# The chart's full-backup CronJob, not a bare `pgbackrest backup` exec: the first run is
# what creates the stanza (without which archive-push and every later backup fail), and it
# brings the repo config, credentials and writable log/lock paths with it. Same pattern as
# the other pgbackrest suites.
echo "Triggering the initial full backup (creates the stanza)..."
kubectl delete job pgbr-full -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
backup_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || backup_rc=$?
if [ "${backup_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"
  kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${backup_rc}"

# Write MORE data after the backup and force a WAL switch, so a correct restore has to
# replay archived WAL rather than just unpacking the base backup -- and so the recovery
# progress GET /v1/status reports after scale-up has something to report.
pg_exec "${NAMESPACE}" "${POD0}" "INSERT INTO api_restore_proof SELECT g FROM generate_series(5001,6000) g;" >/dev/null
pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_switch_wal()" >/dev/null 2>&1 || true
rows_archived=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT count(*) FROM api_restore_proof" | tr -d '[:space:]')
assert_eq "post-backup data written (must come back via WAL replay)" "6000" "${rows_archived}"

# --- port-forward + API helpers ---

# Call this directly, never as $(start_pf): a command substitution would run it in a
# subshell, losing PF_PID and tearing the forward down with the subshell.
start_pf() {
  [[ -n "${PF_PID:-}" ]] && kill "${PF_PID}" 2>/dev/null || true
  for _ in 1 2 3; do
    kubectl port-forward -n "${NAMESPACE}" "pod/${POD0}" "${LOCAL_PORT}:9201" >/dev/null 2>&1 &
    PF_PID=$!
    for _ in $(seq 1 20); do
      if (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null; then exec 3<&- 3>&-; return 0; fi
      sleep 0.5
    done
    kill "${PF_PID}" 2>/dev/null || true
  done
  return 1
}
if start_pf; then pf_ok=ok; else pf_ok=fail; fi
assert_eq "port-forward to the control port established" "ok" "${pf_ok}"

BASE="https://127.0.0.1:${LOCAL_PORT}"
# api <identity> <method> <path> [body] -> "<status>\n<body>"
api() {
  local who="$1" method="$2" path="$3" body="${4:-}"
  local args=(-sS -o "/tmp/ctlres-body.$$" -w '%{http_code}'
    --cacert "${CERTDIR}/ca.crt" --cert "${CERTDIR}/${who}.crt" --key "${CERTDIR}/${who}.key"
    -X "${method}" "${BASE}${path}")
  [[ -n "${body}" ]] && args+=(-H 'Content-Type: application/json' -d "${body}")
  local code=""
  # curl already prints %{http_code} (000 when it could not connect), so its exit status
  # is ignored rather than echoing a second value into the same variable.
  code=$(curl "${args[@]}" 2>/dev/null) || true
  echo "${code:-000}"
  cat "/tmp/ctlres-body.$$" 2>/dev/null || true
  rm -f "/tmp/ctlres-body.$$"
}
api_code() { api "$@" | head -1; }
api_body() { api "$@" | tail -n +2; }

# --- reads that need no extra privilege ---

backups_code=$(api_code ops GET /v1/backups)
assert_eq "GET /v1/backups returns pgbackrest info" "200" "${backups_code}"
backups=$(api_body ops GET /v1/backups)
assert_eq "the backup we just took is listed" "db" "$(jq -r '.info[0].name' <<< "${backups}")"
set_count=$(jq -r '.info[0].backup | length' <<< "${backups}")
assert_gt "at least one backup set is present" "${set_count:-0}" "0"
BACKUP_SET=$(jq -r '.info[0].backup[-1].label' <<< "${backups}")
assert_contains "a backup set label was read" "${BACKUP_SET}" "F"

restore_status=$(api_body ops GET /v1/restore)
assert_eq "no restore has run yet" "none" "$(jq -r '.phase' <<< "${restore_status}")"
steps_ok=$(jq -r 'if (.nextSteps | length) > 0 then "ok" else "none" end' <<< "${restore_status}")
assert_eq "the runbook steps the API does not perform are offered" "ok" "${steps_ok}"

# --- authorization: restore is a SEPARATE verb ---

REQ="{\"node\":\"${POD0}\",\"confirm\":\"${FULLNAME}\",\"force\":true,\"backupSet\":\"${BACKUP_SET}\"}"
ops_restore_code=$(api_code ops POST /v1/restore "${REQ}")
assert_eq "an identity cleared for control but not restore is refused" "403" "${ops_restore_code}"
job_after_403=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o name 2>/dev/null || true)
assert_eq "no Job was created by the refused call" "" "${job_after_403}"

# --- preconditions ---

# The cluster is not paused yet: an active reconcile loop would restart the postmaster
# this restore needs stopped, so the API must refuse rather than race it.
unpaused_code=$(api_code dba POST /v1/restore "${REQ}")
assert_eq "restore while the cluster is not paused is refused" "409" "${unpaused_code}"

wrong_confirm=$(api_code dba POST /v1/restore "{\"node\":\"${POD0}\",\"confirm\":\"not-this-cluster\",\"force\":true}")
assert_eq "a wrong confirm value is refused" "400" "${wrong_confirm}"
no_force=$(api_code dba POST /v1/restore "{\"node\":\"${POD0}\",\"confirm\":\"${FULLNAME}\"}")
assert_eq "force:true is required" "400" "${no_force}"
wrong_ordinal=$(api_code dba POST /v1/restore "{\"node\":\"${POD0}\",\"confirm\":\"${FULLNAME}\",\"force\":true,\"podOrdinal\":3}")
assert_eq "a podOrdinal that is not the rendered target is refused" "409" "${wrong_ordinal}"

assert_eq "pausing the cluster succeeds" "200" "$(api_code ops POST /v1/pause)"

# --- the restore itself ---

restore_resp_code=$(api_code dba POST /v1/restore "${REQ}")
assert_eq "POST /v1/restore is accepted (202)" "202" "${restore_resp_code}"

# The Job exists -- which means the granted RBAC (get on the CronJob, create on jobs)
# is actually sufficient. A missing verb would have surfaced as a 502 above.
created=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o name 2>/dev/null || true)
assert_contains "the restore Job was created by the agent" "${created}" "${API_JOB}"

# It must be indistinguishable from the kubectl runbook's Job, and carry provenance.
instantiate=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.cronjob\.kubernetes\.io/instantiate}' 2>/dev/null || true)
assert_eq "the Job is marked manually instantiated, like kubectl create job --from" "manual" "${instantiate}"
req_by=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/requested-by}' 2>/dev/null || true)
assert_eq "the Job records who requested it" "dba-break-glass" "${req_by}"
# Security-relevant fields must have come from the release, not from the request.
sa=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.serviceAccountName}')
assert_eq "the Job runs as the chart's ServiceAccount" "${FULLNAME}-repmgr" "${sa}"
automount=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.automountServiceAccountToken}')
assert_eq "the Job still has NO Kubernetes API access" "false" "${automount}"
claim=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}')
assert_eq "the target volume came from the rendered spec" "data-${POD0}" "${claim}"
# The requested backup set reached the container; --force did NOT.
set_env=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BACKUP_SET")].value}')
assert_eq "the requested backup set reached the Job" "${BACKUP_SET}" "${set_env}"
force_env=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FORCE")].value}')
assert_eq "the API did not set pgbackrest --force (the postmaster.pid interlock stays armed)" "false" "${force_env}"

# Postgres was stopped BEFORE the Job was created, so the Job cannot race a live
# postmaster. The pod stays up (only the child postmaster is down) because the cluster
# is paused, so the reconcile loop takes no action.
pg_down_rc=0
kubectl exec -n "${NAMESPACE}" "${POD0}" -c postgresql -- \
  psql -U testuser -d testdb -c 'SELECT 1' >/dev/null 2>&1 || pg_down_rc=$?
assert_gt "postgres was stopped on the target pod before the Job was created" "${pg_down_rc}" "0"

# The Job is Pending because the volume is still mounted by this pod -- and the API says
# exactly that, derived from the fact that it is the one answering the request.
pending=$(api_body ops GET /v1/restore)
assert_eq "the restore is pending" "pending" "$(jq -r '.phase' <<< "${pending}")"
assert_contains "the hint names the scale-down the API cannot perform" "$(jq -r '.hint' <<< "${pending}")" "Scale the StatefulSet to 0"
assert_eq "the status reports who requested the restore" "dba-break-glass" "$(jq -r '.requestedBy' <<< "${pending}")"

# --- the operator's part: scale down, let it run, scale up ---

echo "Scaling to 0 so the restore Job can attach the data volume..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=0
kubectl wait --for=delete "pod/${POD0}" -n "${NAMESPACE}" --timeout=300s >/dev/null 2>&1 || true

res_rc=2
for _ in $(seq 1 120); do
  succeeded=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "")
  failed_j=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o jsonpath='{.status.failed}' 2>/dev/null || echo "")
  [ "${succeeded:-0}" -ge 1 ] 2>/dev/null && { res_rc=0; break; }
  [ "${failed_j:-0}" -ge 1 ] 2>/dev/null && { res_rc=1; break; }
  sleep 3
done
if [ "${res_rc}" -ne 0 ]; then
  echo "  the cloned restore Job did not complete; logs:"
  kubectl logs -n "${NAMESPACE}" "job/${API_JOB}" --tail=200 2>/dev/null || true
fi
assert_eq "the Job the agent cloned actually completes a restore" "0" "${res_rc}"

echo "Scaling back up..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=1
up_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 900 || up_rc=$?
assert_eq "the restored node becomes ready" "0" "${up_rc}"

# 6000, not 5000: the base backup held 5000 rows and the rest can only appear by replaying
# archived WAL -- which is what makes this a real PITR rather than an unpack.
restored_rows=""
for _ in $(seq 1 30); do
  restored_rows=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT count(*) FROM api_restore_proof" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${restored_rows}" = "6000" ] && break
  sleep 5
done
assert_eq "the data came back from the repository, WAL replay included" "6000" "${restored_rows}"

# --- the record that outlives the Job ---
#
# This is the answer to "what happened to my restore?" after the cycle: the Job may be
# gone to history limits, but restore.sh wrote the outcome onto the volume and the agent
# reads it back.

if start_pf; then pf_ok=ok; else pf_ok=fail; fi
assert_eq "port-forward re-established after scale-up" "ok" "${pf_ok}"

status=""
for _ in $(seq 1 24); do
  status=$(api_body ops GET /v1/status)
  [[ "$(jq -r '.lastRestore.succeeded // false' <<< "${status}")" == "true" ]] && break
  sleep 5
done
assert_eq "GET /v1/status reports the restore succeeded" "true" "$(jq -r '.lastRestore.succeeded' <<< "${status}")"
assert_eq "the record carries the backup set the data came from" "${BACKUP_SET}" "$(jq -r '.lastRestore.backupSet' <<< "${status}")"
assert_eq "the record carries the requester (via the downward API)" "dba-break-glass" "$(jq -r '.lastRestore.requestedBy' <<< "${status}")"
finished_ok=$(jq -r 'if (.lastRestore.finishedAt | length) > 0 then "ok" else "empty" end' <<< "${status}")
assert_eq "the record carries a finish timestamp" "ok" "${finished_ok}"
# The control-file reading restore.sh captured is the first thing to check when a
# restored cluster misbehaves, so it must survive in the record too.
state_ok=$(jq -r 'if (.lastRestore.clusterState | length) > 0 then "ok" else "empty" end' <<< "${status}")
assert_eq "the record carries the post-restore cluster state" "ok" "${state_ok}"

# The Job's own status is still reported while it exists.
after=$(api_body ops GET /v1/restore)
assert_eq "GET /v1/restore reports the completed Job" "succeeded" "$(jq -r '.phase' <<< "${after}")"

# DELETE clears the Job but must not touch the recorded outcome.
assert_eq "DELETE /v1/restore removes the Job" "200" "$(api_code dba DELETE /v1/restore)"
gone=""
for _ in $(seq 1 24); do
  gone=$(kubectl get job "${API_JOB}" -n "${NAMESPACE}" -o name 2>/dev/null || true)
  [[ -z "${gone}" ]] && break
  sleep 5
done
assert_eq "the Job is gone" "" "${gone}"
after_delete=$(api_body ops GET /v1/status)
assert_eq "the recorded outcome survives the Job's deletion" "true" "$(jq -r '.lastRestore.succeeded' <<< "${after_delete}")"

# --- resume ---

assert_eq "resuming the cluster succeeds" "200" "$(api_code ops POST /v1/resume)"
marker_pause=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/pause}' 2>/dev/null || true)
assert_eq "maintenance mode is off again" "" "${marker_pause}"

end_suite
print_summary
