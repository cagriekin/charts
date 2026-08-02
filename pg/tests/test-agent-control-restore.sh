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
#     the progress signal that survives scaling to 0;
#   * the ValidatingAdmissionPolicy that bounds the unscopeable `create jobs` grant (#279)
#     admits the agent's own Job and denies every tampered one -- content-based admission
#     exists only in the apiserver, so there is no cheaper layer that could test it.
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
  -f "${VALUES}" --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 900

# --- the granted RBAC is exactly what was documented, no wider ---
#
# The README and rbac.yaml both claim that `create jobs` is the ONE grant that cannot be
# resourceName-scoped, and that nothing else widens. Assert it against the live apiserver
# rather than against rendered YAML: `kubectl auth can-i --as` takes the same
# authorization path the agent's token does, so this is the claim itself under test.
SA="system:serviceaccount:${NAMESPACE}:${FULLNAME}-repmgr"
can() { kubectl auth can-i "$1" "$2" -n "${NAMESPACE}" --as="${SA}" 2>/dev/null | tail -1; }
assert_eq "RBAC: can read the CronJob it clones" "yes" "$(can get "cronjobs/${FULLNAME}-pgbackrest-restore")"
assert_eq "RBAC: can create the restore Job" "yes" "$(can create jobs)"
assert_eq "RBAC: can read its own Job by name" "yes" "$(can get "jobs/${API_JOB}")"
assert_eq "RBAC: can delete its own Job by name" "yes" "$(can delete "jobs/${API_JOB}")"
# The negatives are the point: get/delete really are name-scoped, and none of the sharper
# grants leaked in with them.
assert_eq "RBAC: cannot read any OTHER Job" "no" "$(can get jobs/some-other-job)"
assert_eq "RBAC: cannot delete any OTHER Job" "no" "$(can delete jobs/some-other-job)"
assert_eq "RBAC: cannot read another CronJob" "no" "$(can get "cronjobs/${FULLNAME}-pgbackrest-full")"
assert_eq "RBAC: cannot list Jobs" "no" "$(can list jobs)"
assert_eq "RBAC: no pods/log without restore.readPodLogs" "no" "$(can get pods/log)"
assert_eq "RBAC: still cannot read Secrets" "no" "$(can get secrets)"

# --- and the bound on the one grant RBAC could not scope (#279) ---
#
# `create jobs` above is unscopeable, so what limits it is a ValidatingAdmissionPolicy:
# RBAC restricts by verb and resource, admission restricts by CONTENT. Only a live
# apiserver can test that, and it has to be tested in BOTH directions -- a policy that
# denied the agent's own restore would break the feature, and one that policed every
# creator would break every other controller in the namespace.
#
# Each case submits the Job `kubectl create job --from=cronjob/...` builds -- the same
# clone the agent builds -- as the pods' ServiceAccount, with one field tampered.
# --dry-run=server runs the full admission chain and creates nothing.
GUARD="${NAMESPACE}-${FULLNAME}-restore-guard"
assert_eq "VAP: the policy is installed" "${GUARD}" \
  "$(kubectl get validatingadmissionpolicy "${GUARD}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
assert_eq "VAP: the binding is installed" "${GUARD}" \
  "$(kubectl get validatingadmissionpolicybinding "${GUARD}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
assert_eq "VAP: fails closed (Ignore would silently re-open the hole)" "Fail" \
  "$(kubectl get validatingadmissionpolicy "${GUARD}" -o jsonpath='{.spec.failurePolicy}' 2>/dev/null || true)"
# The apiserver type-checks every expression against batch/v1 Job and reports what does not
# resolve. A warning here means a validation that can never do its job -- and under
# failurePolicy: Fail, one that may deny every restore instead.
vap_warnings=$(kubectl get validatingadmissionpolicy "${GUARD}" \
  -o jsonpath='{.status.typeChecking.expressionWarnings}' 2>/dev/null || true)
[ -n "${vap_warnings}" ] && echo "  note: apiserver type-check warnings: ${vap_warnings}"
assert_eq "VAP: the apiserver type-checks every expression cleanly" "" "${vap_warnings}"

# clone_job <jq filter> -- the Job the agent would create, with the filter applied.
clone_job() {
  kubectl create job "${API_JOB}" --from=cronjob/"${FULLNAME}-pgbackrest-restore" \
    -n "${NAMESPACE}" --dry-run=client -o json 2>/dev/null | jq "$1"
}
# admit <jq filter> [<impersonated user>|human] -> "allowed" | the apiserver's denial message
# The literal "human" means "do not impersonate", i.e. submit as whoever is running the
# suite. A sentinel rather than an empty second argument, because ${2:-${SA}} would silently
# substitute the ServiceAccount for one and turn the control case into a duplicate.
admit() {
  local filter="$1" who="${2:-${SA}}" out rc=0 args=(-f - -n "${NAMESPACE}" --dry-run=server -o name)
  [[ "${who}" != "human" ]] && args+=(--as="${who}")
  out=$(clone_job "${filter}" | kubectl create "${args[@]}" 2>&1) || rc=$?
  if [ "${rc}" -eq 0 ]; then echo "allowed"; else echo "${out}"; fi
}

# The direction that is easy to forget: the legitimate restore must still go through, and a
# human or any other controller must be unaffected by the policy existing.
assert_eq "VAP: the agent's own restore Job is admitted" "allowed" "$(admit '.')"
unrelated='.metadata.name="vap-unrelated-job" | .metadata.labels={} | .spec.template.spec.serviceAccountName="default" | .spec.template.spec.automountServiceAccountToken=true'
assert_eq "VAP: a Job created by a human is untouched" "allowed" "$(admit "${unrelated}" human)"

# The escalation primitive itself: a Job's pod may name any ServiceAccount, with no separate
# permission check. This is the denial the whole feature's safety rests on.
assert_contains "VAP: naming another ServiceAccount is denied" \
  "$(admit '.spec.template.spec.serviceAccountName="privileged-sa"')" \
  "may only create Jobs running as ServiceAccount"
# ...and why it would not help even if it were not: no token is mounted.
assert_contains "VAP: mounting the ServiceAccount token is denied" \
  "$(admit '.spec.template.spec.automountServiceAccountToken=true')" \
  "automountServiceAccountToken: false"
assert_contains "VAP: omitting automountServiceAccountToken is denied (absent is not a pass)" \
  "$(admit 'del(.spec.template.spec.automountServiceAccountToken)')" \
  "automountServiceAccountToken: false"
# The bound RBAC could not express: create collapses to ONE permitted object name.
assert_contains "VAP: any other Job name is denied" \
  "$(admit '.metadata.name="evil"')" "may create only the Job"
# generateName cannot slip past it: name generation happens before VALIDATING admission, so
# the policy sees the final name. Worth proving rather than reasoning about.
assert_contains "VAP: generateName cannot dodge the name pin" \
  "$(admit 'del(.metadata.name) | .metadata.generateName="evil-"')" "may create only the Job"
assert_contains "VAP: a Job without the pg-ha/restore label is denied" \
  "$(admit '.metadata.labels={}')" "must carry pg-ha/restore"
assert_contains "VAP: hostNetwork is denied" \
  "$(admit '.spec.template.spec.hostNetwork=true')" "hostNetwork, hostPID and hostIPC are not permitted"
assert_contains "VAP: hostPID is denied" \
  "$(admit '.spec.template.spec.hostPID=true')" "hostNetwork, hostPID and hostIPC are not permitted"
assert_contains "VAP: another image is denied" \
  "$(admit '.spec.template.spec.containers[0].image="attacker/img:1"')" "may only run this release's repmgr image"
assert_contains "VAP: a second container is denied" \
  "$(admit '.spec.template.spec.containers += [{"name":"x","image":"busybox"}]')" "exactly one container"
assert_contains "VAP: an init container is denied" \
  "$(admit '.spec.template.spec.initContainers=[{"name":"i","image":"busybox"}]')" "exactly one container"
# With the token gone, mounting somebody else's Secret is the remaining route out of this
# release -- so the volume sources are pinned to the three the restore template renders.
assert_contains "VAP: mounting an arbitrary Secret is denied" \
  "$(admit '.spec.template.spec.volumes += [{"name":"steal","secret":{"secretName":"pg-control-restore-tls"}}]')" \
  "may mount only this release's restore volumes"
assert_contains "VAP: a hostPath mount is denied" \
  "$(admit '.spec.template.spec.volumes += [{"name":"node","hostPath":{"path":"/"}}]')" \
  "may mount only this release's restore volumes"
assert_contains "VAP: a projected ServiceAccount token is denied" \
  "$(admit '.spec.template.spec.volumes += [{"name":"tok","projected":{"sources":[{"serviceAccountToken":{"path":"t"}}]}}]')" \
  "may mount only this release's restore volumes"
assert_contains "VAP: an unrelated PVC is denied" \
  "$(admit '.spec.template.spec.volumes += [{"name":"other","persistentVolumeClaim":{"claimName":"data-other-0"}}]')" \
  "may mount only this release's restore volumes"
# The same bound for env: envFrom pulls a whole Secret, secretKeyRef/configMapKeyRef a key
# from any of them. Only the downward API and this release's own Secrets get through.
assert_contains "VAP: reading another Secret through secretKeyRef is denied" \
  "$(admit '.spec.template.spec.containers[0].env += [{"name":"X","valueFrom":{"secretKeyRef":{"name":"pg-control-restore-tls","key":"tls.key"}}}]')" \
  "may not use envFrom"
assert_contains "VAP: envFrom is denied" \
  "$(admit '.spec.template.spec.containers[0].envFrom=[{"secretRef":{"name":"pg-control-restore-tls"}}]')" \
  "may not use envFrom"
assert_contains "VAP: reading a ConfigMap through configMapKeyRef is denied" \
  "$(admit '.spec.template.spec.containers[0].env += [{"name":"X","valueFrom":{"configMapKeyRef":{"name":"kube-root-ca.crt","key":"ca.crt"}}}]')" \
  "may not use envFrom"

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

# kubectl binds the local port immediately, so a successful start_pf proves only that: the
# pod side can still refuse connections for a few seconds after the pod goes Ready. Probe
# end to end before asserting anything, or the first calls intermittently see 000.
wait_api_ready() {
  for _ in $(seq 1 30); do
    [[ "$(api_code ops GET /v1/status)" != "000" ]] && return 0
    sleep 2
  done
  return 1
}
if wait_api_ready; then api_ready=ok; else api_ready=fail; fi
assert_eq "the control API answers through the forward" "ok" "${api_ready}"

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

# Either outcome is correct here, and which one you get is a scheduling detail worth
# knowing: ReadWriteOnce binds a volume to a NODE, not a pod, so a restore Job the
# scheduler places on this pod's node attaches the volume and starts IMMEDIATELY, with the
# StatefulSet still scaled up. A Job placed elsewhere sits Pending until the volume is
# freed. What must hold either way is that the restore is not racing a live postmaster --
# asserted above, and guaranteed by the required pause plus the stop this flow performed.
pending=$(api_body ops GET /v1/restore)
phase=$(jq -r '.phase' <<< "${pending}")
case "${phase}" in
  pending|running|succeeded) phase_ok=ok ;;
  *) phase_ok="${phase}" ;;
esac
assert_eq "the restore Job is pending, running or already complete" "ok" "${phase_ok}"
if [ "${phase}" = "pending" ]; then
  # Only a Job that really is waiting for the volume gets the scale-down hint. The agent
  # suppresses it for waiting reasons that explain themselves (ImagePullBackOff and
  # friends), so echo the observed reason: if this ever fails, the reason is the answer.
  observed_reason=$(jq -r '.waitingReason // ""' <<< "${pending}")
  echo "  note: pending Job waitingReason=${observed_reason:-<none>}"
  assert_contains "a pending Job's hint names the volume it cannot attach (waitingReason=${observed_reason:-<none>})" \
    "$(jq -r '.hint' <<< "${pending}")" "data-${POD0}"
else
  echo "  note: the restore Job co-scheduled with ${POD0} and started without a scale-down"
  assert_eq "a Job that started carries no scale-down hint" "" "$(jq -r '.hint // ""' <<< "${pending}")"
fi
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

# The restored node must NOT come up while the cluster is still paused: maintenance mode
# makes the reconcile loop a no-op, so nothing starts PostgreSQL. This is the step an
# operator forgets, so assert the trap exists before stepping over it -- a runbook that
# omitted the resume would leave the cluster down.
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/${POD0}" -n "${NAMESPACE}" --timeout=300s >/dev/null 2>&1 || true
still_down=notready
for _ in $(seq 1 6); do
  ready=$(kubectl get pod "${POD0}" -n "${NAMESPACE}" -o jsonpath='{.status.containerStatuses[?(@.name=="postgresql")].ready}' 2>/dev/null || true)
  [ "${ready}" = "true" ] && { still_down=ready; break; }
  sleep 5
done
assert_eq "while paused, the restored node deliberately does NOT start postgres" "notready" "${still_down}"

# Resume, and only then expect readiness.
if start_pf; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "port-forward re-established after scale-up" "ok" "${pf_ok}"
assert_eq "resuming the cluster succeeds" "200" "$(api_code ops POST /v1/resume)"

up_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 900 || up_rc=$?
assert_eq "the restored node becomes ready once resumed" "0" "${up_rc}"

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

# --- maintenance mode really is off ---

marker_pause=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/pause}' 2>/dev/null || true)
assert_eq "maintenance mode is off again" "" "${marker_pause}"
resumed=$(api_body ops GET /v1/status)
assert_eq "status no longer reports a pause" "false" "$(jq -r '.intents.paused' <<< "${resumed}")"

end_suite
print_summary
