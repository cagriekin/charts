#!/bin/bash
set -euo pipefail

# Live coverage for the agent's authenticated control REST API (#276).
#
# The properties worth proving in a real cluster, none of which a render test can:
#   * mTLS is actually enforced -- no certificate and a foreign/unlisted identity are
#     both refused by the running listener, not just by unit tests;
#   * port 9200 gained no control routes, so the NetworkPolicy separation is real;
#   * pause/switchover through the API produce the SAME markers kubectl writes, and a
#     switchover actually moves leadership;
#   * the node interlock refuses a request addressed to another pod;
#   * features that are off report "not configured" rather than failing obscurely.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-control}"
RELEASE="${RELEASE:-pg-agent-control}"
VALUES="${SCRIPT_DIR}/values-agent-control.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
LEASE="${FULLNAME}-leader"
MARKER="${FULLNAME}-primary"
HEADLESS="${FULLNAME}-headless"
TLS_SECRET="pg-agent-control-tls"
LOCAL_PORT="${LOCAL_PORT:-19201}"
METRICS_PORT="${METRICS_PORT:-19200}"

begin_suite "Agent Control API (mTLS: status, pause, switchover)"

# jq parses every API response below. Name it explicitly rather than letting the
# suite fail on an empty variable ten assertions later.
if ! command -v jq >/dev/null 2>&1; then
  fail "jq is required to parse the control API responses" "install jq"
  end_suite
  print_summary
  exit 1
fi

# --- a throwaway PKI: CA, server cert for the pods, two client identities ---

CERTDIR=$(mktemp -d)
cleanup() {
  # Kill any port-forward this suite started, then drop the certs.
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

# The server cert must cover both ways the API is reached: the headless pod FQDNs
# (in-cluster clients) and 127.0.0.1/localhost (port-forward, the documented path for
# humans and the one this suite uses).
SRV_SAN="DNS:*.${HEADLESS}.${NAMESPACE}.svc.cluster.local,DNS:${HEADLESS}"
SRV_SAN="${SRV_SAN},DNS:localhost,IP:127.0.0.1"
gen_ca
gen_cert server   "${FULLNAME}" "${SRV_SAN}" "serverAuth"
gen_cert ops      "ops-admin"   ""           "clientAuth"
gen_cert intruder "intruder"    ""           "clientAuth"
certs_ok=$([[ -s "${CERTDIR}/server.crt" && -s "${CERTDIR}/ops.crt" && -s "${CERTDIR}/intruder.crt" ]] && echo ok || echo fail)
assert_eq "test PKI generated (CA + server + 2 client identities)" "ok" "${certs_ok}"

# --- install ---

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
# podManagementPolicy is immutable; a leftover StatefulSet from a repmgrd-mode run
# (OrderedReady) blocks an agent-mode (Parallel) install.
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
# The timeline-highwater marker is agent-created (not helm-managed), so it survives
# uninstall; a stale marker from a prior run's switchover (timeline > the fresh PVCs')
# trips the #125 guard and the new primary refuses to start read-write. Drop it so a
# same-namespace re-run starts clean (CI runs in a fresh namespace and never hits this).
kubectl delete configmap "${FULLNAME}-primary" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
kubectl delete lease "${FULLNAME}-leader" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

# All three keys: the agent reads the CA to verify client certificates and refuses to
# boot without it, so `kubectl create secret tls` alone would not do.
kubectl create secret generic "${TLS_SECRET}" -n "${NAMESPACE}" \
  --from-file=tls.crt="${CERTDIR}/server.crt" \
  --from-file=tls.key="${CERTDIR}/server.key" \
  --from-file=ca.crt="${CERTDIR}/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# --- port-forward to the lease holder ---
#
# The API is per-pod and there is deliberately no control Service, so the suite has to
# pick a pod. Cluster-scope verbs go to the LEADER: preconditions are evaluated from
# the answering pod's view, and only the primary can judge a candidate standby.

echo "  Waiting for a lease holder (up to 240s)..."
LEADER=""
for _ in $(seq 1 48); do
  LEADER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "${LEADER}" ]] && break
  sleep 5
done
assert_contains "a lease holder was elected" "${LEADER}" "${FULLNAME}"
# The other pod is the switchover candidate.
if [[ "${LEADER}" == "${FULLNAME}-0" ]]; then CANDIDATE="${FULLNAME}-1"; else CANDIDATE="${FULLNAME}-0"; fi
echo "  leader=${LEADER} candidate=${CANDIDATE}"

# start_pf MUST be called directly, never in a command substitution: $(start_pf) runs in
# a subshell, so PF_PID would be lost to the parent and the backgrounded kubectl would be
# torn down with the subshell -- leaving a local port that accepts connections and a
# forward that is already gone.
start_pf() { # start_pf <pod> <local-port> <remote-port>
  local pod="$1" lport="$2" rport="$3"
  [[ -n "${PF_PID:-}" ]] && kill "${PF_PID}" 2>/dev/null || true
  # Retry: port-forward occasionally loses the race with a just-Ready pod in CI.
  for _ in 1 2 3; do
    kubectl port-forward -n "${NAMESPACE}" "pod/${pod}" "${lport}:${rport}" >/dev/null 2>&1 &
    PF_PID=$!
    for _ in $(seq 1 20); do
      if (exec 3<>"/dev/tcp/127.0.0.1/${lport}") 2>/dev/null; then exec 3<&- 3>&-; return 0; fi
      sleep 0.5
    done
    kill "${PF_PID}" 2>/dev/null || true
  done
  return 1
}

# kubectl binds the LOCAL port immediately, so start_pf returning 0 proves only that much:
# the pod side can still refuse connections for a few seconds after the pod goes Ready
# (the agent's listener and the readiness probe are independent). Every control-port
# forward is therefore followed by an end-to-end probe -- without it the first few API
# calls intermittently see 000 and the suite fails for reasons that have nothing to do
# with the API.
wait_api_ready() {
  for _ in $(seq 1 30); do
    [[ "$(api_code GET /v1/status)" != "000" ]] && return 0
    sleep 2
  done
  return 1
}
# wait_candidate_ready polls the LEADER's view until the named peer satisfies exactly the
# conditions POST /v1/switchover enforces: reachable, a standby, with a readable timeline
# matching the leader's. Needed after a reinitialize -- the leader's snapshot refreshes once
# per reconcile tick, and while a peer is being wiped and re-cloned it is legitimately
# unreachable, so a handoff requested too early is (correctly) refused with 409.
wait_candidate_ready() { # wait_candidate_ready <peer>
  local cand="$1" c ok ctl ltl
  for _ in $(seq 1 60); do
    c=$(api_body GET /v1/cluster)
    ok=$(jq -r --arg n "${cand}" '[.members[] | select(.name==$n) | .reachable, .timelineKnown, (.role=="standby")] | all' <<< "${c}" 2>/dev/null || echo false)
    ctl=$(jq -r --arg n "${cand}" '.members[] | select(.name==$n) | .timeline' <<< "${c}" 2>/dev/null || echo x)
    ltl=$(jq -r '.members[] | select(.self==true) | .timeline' <<< "${c}" 2>/dev/null || echo y)
    if [ "${ok}" = "true" ] && [ -n "${ctl}" ] && [ "${ctl}" = "${ltl}" ]; then
      return 0
    fi
    sleep 5
  done
  return 1
}

wait_http_ready() { # wait_http_ready <url>
  for _ in $(seq 1 30); do
    local code
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$1" 2>/dev/null) || true
    [[ -n "${code}" && "${code}" != "000" ]] && return 0
    sleep 2
  done
  return 1
}

if start_pf "${LEADER}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
assert_eq "port-forward to the control port established" "ok" "${pf_ok}"

BASE="https://127.0.0.1:${LOCAL_PORT}"

# api <method> <path> [body] -- authenticated as ops-admin; prints "<status>\n<body>".
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -o /tmp/ctl-body.$$ -w '%{http_code}'
    --cacert "${CERTDIR}/ca.crt" --cert "${CERTDIR}/ops.crt" --key "${CERTDIR}/ops.key"
    -X "${method}" "${BASE}${path}")
  [[ -n "${body}" ]] && args+=(-H 'Content-Type: application/json' -d "${body}")
  local code=""
  # curl already prints %{http_code} (000 when it could not connect), so its exit status
  # is ignored rather than echoing a second value into the same variable.
  code=$(curl "${args[@]}" 2>/dev/null) || true
  echo "${code:-000}"
  cat "/tmp/ctl-body.$$" 2>/dev/null || true
  rm -f "/tmp/ctl-body.$$"
}
api_code() { api "$@" | head -1; }
api_body() { api "$@" | tail -n +2; }

if wait_api_ready; then api_ready=ok; else api_ready=fail; fi
assert_eq "the control API answers through the forward" "ok" "${api_ready}"

# --- authentication: the listener itself must enforce mTLS ---

# No client certificate: the handshake must fail, so curl exits non-zero. There is no
# anonymous read tier on this port, not even for /v1/status.
nocert_rc=0
curl -sS --cacert "${CERTDIR}/ca.crt" "${BASE}/v1/status" >/dev/null 2>&1 || nocert_rc=$?
assert_gt "a client with NO certificate is refused" "${nocert_rc}" "0"

# Plaintext against the TLS port must not be served. Go's TLS server answers and closes
# abruptly, and kubectl port-forward sometimes tears the whole forward down with it -- so
# re-establish it afterwards rather than letting every later call fail with a connection
# error that looks like an authorization result.
plain_body=$(curl -sS "http://127.0.0.1:${LOCAL_PORT}/v1/status" 2>&1 || true)
assert_not_contains "plaintext HTTP is not served on the control port" "${plain_body}" '"node"'
if start_pf "${LEADER}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "the forward survives (or is restored after) the plaintext probe" "ok" "${pf_ok}"

# A certificate from the SAME CA but an unlisted CN is authenticated yet unauthorized.
intruder_code=$(curl -sS -o /dev/null -w '%{http_code}' \
  --cacert "${CERTDIR}/ca.crt" --cert "${CERTDIR}/intruder.crt" --key "${CERTDIR}/intruder.key" \
  "${BASE}/v1/status" 2>/dev/null) || true
assert_eq "an unlisted client CN is refused with 403" "403" "${intruder_code}"

# --- reads ---

status_code=$(api_code GET /v1/status)
assert_eq "GET /v1/status with a listed identity returns 200" "200" "${status_code}"
status=$(api_body GET /v1/status)
assert_eq "status reports the pod it was served by" "${LEADER}" "$(jq -r '.node' <<< "${status}")"
assert_eq "status reports the cluster name" "${FULLNAME}" "$(jq -r '.cluster' <<< "${status}")"
assert_eq "status reports this pod holds the lease" "true" "$(jq -r '.holdsLease' <<< "${status}")"
assert_eq "status reports the local role" "primary" "$(jq -r '.local.role' <<< "${status}")"
# Never hardcode the major: this suite runs on every major in the CI matrix, and
# set-pg-major.sh retargets the image/values fixtures but cannot rewrite an assertion.
# Compare against what the chart actually rendered into the pod, which is the real contract
# (the API reports the major its image bundles).
pod_major=$(kubectl get pod -n "${NAMESPACE}" "${LEADER}" \
  -o jsonpath='{.spec.containers[?(@.name=="postgresql")].env[?(@.name=="PG_MAJOR")].value}' 2>/dev/null || true)
assert_contains "the chart rendered a PG_MAJOR for the agent" "${pod_major}" "1"
assert_eq "status reports the PostgreSQL major the chart rendered" "${pod_major}" "$(jq -r '.pgMajor' <<< "${status}")"
# The observation age lets a client judge the freshness of a cached snapshot.
age_ok=$(jq -r 'if .observationAgeSeconds != null then "ok" else "missing" end' <<< "${status}")
assert_eq "status reports the observation age" "ok" "${age_ok}"

cluster=$(api_body GET /v1/cluster)
assert_eq "cluster lists both members" "2" "$(jq -r '.members | length' <<< "${cluster}")"
assert_eq "cluster flags the answering pod as self" "true" "$(jq -r '.members[0].self' <<< "${cluster}")"
# The reconcile loop's decision is the thing this endpoint exists for: it answers
# "why is nothing happening" without reading logs.
decision=$(jq -r '.lastDecision.action' <<< "${cluster}")
assert_contains "cluster exposes the last reconcile decision" "${decision}" "Primary"
reason_ok=$(jq -r 'if (.lastDecision.reason | length) > 0 then "ok" else "empty" end' <<< "${cluster}")
assert_eq "the decision carries its reason" "ok" "${reason_ok}"
# Every member carries a position, which nothing else in the chart exposes.
tl_ok=$(jq -r 'if .members[0].timelineKnown then "ok" else "unknown" end' <<< "${cluster}")
assert_eq "members carry a timeline" "ok" "${tl_ok}"

# --- the metrics port gained no control routes ---

if start_pf "${LEADER}" "${METRICS_PORT}" 9200; then metrics_pf=ok; else metrics_pf=fail; fi
assert_eq "port-forward to the metrics port established" "ok" "${metrics_pf}"
wait_http_ready "http://127.0.0.1:${METRICS_PORT}/metrics" || true
metrics_code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null) || true
assert_eq "the metrics port still serves /metrics over plain HTTP" "200" "${metrics_code}"
for path in /v1/status /v1/pause /v1/switchover /v1/restore; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${METRICS_PORT}${path}" 2>/dev/null) || true
  assert_eq "the metrics port serves no ${path}" "404" "${code}"
done
# Control counters are exported on the read-only port, so a denied call is alertable
# without opening the control port to Prometheus.
metrics_body=$(curl -sS "http://127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null || true)
assert_contains "control request counter is exported" "${metrics_body}" "pg_ha_agent_control_requests_total"
assert_contains "control rejection counter is exported" "${metrics_body}" "pg_ha_agent_control_rejected_total"
rejected=$(grep -E '^pg_ha_agent_control_rejected_total ' <<< "${metrics_body}" | awk '{print $2}')
assert_gt "the refused calls above were counted" "${rejected:-0}" "0"

# --- pause: the API writes the SAME marker kubectl writes ---

if start_pf "${LEADER}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "port-forward re-established on the control port" "ok" "${pf_ok}"

pause_code=$(api_code POST /v1/pause)
assert_eq "POST /v1/pause returns 200" "200" "${pause_code}"
marker_pause=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/pause}' 2>/dev/null || true)
assert_eq "the pause marker annotation kubectl uses is set" "true" "${marker_pause}"
# Provenance survives a pod restart and shows in kubectl describe, which the agent's
# stdout audit log does not.
paused_by=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/paused-by}' 2>/dev/null || true)
assert_eq "the requester is recorded on the marker" "ops-admin" "${paused_by}"
# The marker's Data (the highwater timeline) must be untouched by an annotation write.
marker_tl=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.data.timeline}' 2>/dev/null || true)
assert_gt "the marker timeline survived the annotation write" "${marker_tl:-0}" "0"

paused_status=$(api_body GET /v1/status)
assert_eq "status reflects the pause" "true" "$(jq -r '.intents.paused' <<< "${paused_status}")"
assert_eq "status reports who paused" "ops-admin" "$(jq -r '.intents.pausedBy' <<< "${paused_status}")"

# Pausing again is idempotent, not an error.
assert_eq "a second pause is idempotent" "200" "$(api_code POST /v1/pause)"

# A paused loop takes no action, so a switchover recorded now would sit unnoticed --
# the API refuses instead of accepting a no-op. This is the preflight that plain
# `kubectl annotate` cannot do.
sw_paused_code=$(api_code POST /v1/switchover "{\"candidate\":\"${CANDIDATE}\"}")
assert_eq "switchover while paused is refused" "409" "${sw_paused_code}"

resume_code=$(api_code POST /v1/resume)
assert_eq "POST /v1/resume returns 200" "200" "${resume_code}"
marker_pause_after=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/pause}' 2>/dev/null || true)
assert_eq "the pause annotation is cleared on resume" "" "${marker_pause_after}"
paused_by_after=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/paused-by}' 2>/dev/null || true)
assert_eq "the requester is cleared with the pause" "" "${paused_by_after}"

# --- preflight refusals that kubectl annotate would accept silently ---

unknown_code=$(api_code POST /v1/switchover '{"candidate":"'"${FULLNAME}"'-9"}')
assert_eq "switchover to a non-member is refused" "400" "${unknown_code}"

self_code=$(api_code POST /v1/switchover "{\"candidate\":\"${LEADER}\"}")
assert_eq "switchover to the current leader is refused" "409" "${self_code}"

stale_leader_code=$(api_code POST /v1/switchover "{\"leader\":\"${CANDIDATE}\",\"candidate\":\"${LEADER}\"}")
assert_eq "a stale leader in the request is refused" "409" "${stale_leader_code}"

# Nothing above may have left a request behind on the marker.
sw_marker=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-target}' 2>/dev/null || true)
assert_eq "no switchover request was recorded by the refused calls" "" "${sw_marker}"

# --- node interlock ---

wrong_node_code=$(api_code POST /v1/restart "{\"node\":\"${CANDIDATE}\"}")
assert_eq "a restart addressed to another pod is refused" "409" "${wrong_node_code}"
no_node_code=$(api_code POST /v1/restart '{}')
assert_eq "a restart with no node named is refused" "400" "${no_node_code}"
# The serving primary needs an explicit force, so a fat-fingered call cannot interrupt
# writes by default.
primary_restart_code=$(api_code POST /v1/restart "{\"node\":\"${LEADER}\"}")
assert_eq "restarting the serving primary needs force:true" "409" "${primary_restart_code}"

# --- features that are off must say so, not fail obscurely ---

backups_code=$(api_code GET /v1/backups)
assert_eq "GET /v1/backups reports pgbackrest is not enabled" "501" "${backups_code}"
restore_code=$(api_code POST /v1/restore "{\"node\":\"${LEADER}\",\"confirm\":\"${FULLNAME}\",\"force\":true}")
assert_eq "POST /v1/restore reports restore triggering is not enabled" "501" "${restore_code}"

# A typo in a destructive request must fail rather than default the field it meant.
typo_code=$(api_code POST /v1/restart "{\"node\":\"${LEADER}\",\"forced\":true}")
assert_eq "an unknown body field is rejected" "400" "${typo_code}"

unknown_route_code=$(api_code GET /v1/nope)
assert_eq "an unknown route returns 404" "404" "${unknown_route_code}"
wrong_method_code=$(api_code GET /v1/pause)
assert_eq "a GET on a mutating route returns 405" "405" "${wrong_method_code}"

# --- reinitialize: rebuild a standby for real ---
#
# The endpoint is replica-only and destructive, so this proves both halves: the primary is
# refused, and the standby is genuinely wiped and re-cloned by the reconcile loop.

reinit_primary_code=$(api_code POST /v1/reinitialize "{\"node\":\"${LEADER}\",\"force\":true}")
assert_eq "reinitialize on the lease holder is refused" "409" "${reinit_primary_code}"
reinit_noforce_code=$(api_code POST /v1/reinitialize "{\"node\":\"${LEADER}\"}")
assert_eq "reinitialize without force is refused" "400" "${reinit_noforce_code}"

# Mark the standby's data so the re-clone is provable: the row is written on the PRIMARY and
# streamed, so after a wipe + clone it must be present again from a fresh copy.
pg_exec "${NAMESPACE}" "${LEADER}" "CREATE TABLE IF NOT EXISTS reinit_proof (id int); INSERT INTO reinit_proof VALUES (42);" >/dev/null 2>&1 || true
standby_rows=""
for _ in $(seq 1 20); do
  standby_rows=$(pg_exec "${NAMESPACE}" "${CANDIDATE}" "SELECT count(*) FROM reinit_proof" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${standby_rows}" = "1" ] && break
  sleep 3
done
assert_eq "standby streamed the marker row before the rebuild" "1" "${standby_rows}"

# Address the STANDBY's own API: node-local verbs act on the pod that answers.
if start_pf "${CANDIDATE}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "port-forward to the standby's control port" "ok" "${pf_ok}"
reinit_code=$(api_code POST /v1/reinitialize "{\"node\":\"${CANDIDATE}\",\"force\":true}")
assert_eq "reinitialize on the standby is accepted (202)" "202" "${reinit_code}"

# The loop re-clones from the lease holder. Watch it land: data comes back, the role returns
# to standby, and the marker row is present again from the fresh copy.
echo "  Waiting for the standby to be re-cloned (up to 300s)..."
recloned=""
for _ in $(seq 1 60); do
  recloned=$(pg_exec "${NAMESPACE}" "${CANDIDATE}" "SELECT count(*) FROM reinit_proof" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${recloned}" = "1" ] && break
  sleep 5
done
assert_eq "the standby was re-cloned and has the data again" "1" "${recloned}"
in_recovery=$(pg_exec "${NAMESPACE}" "${CANDIDATE}" "SELECT pg_is_in_recovery()" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "the rebuilt node is a standby again" "t" "${in_recovery}"
# The API serves a snapshot refreshed once per reconcile tick, so a rebuild that SQL has
# already confirmed is not necessarily published yet -- poll for convergence rather than
# asserting on the instant the clone landed.
reinit_status=""
for _ in $(seq 1 24); do
  reinit_status=$(api_body GET /v1/status)
  [ "$(jq -r '.local.role' <<< "${reinit_status}")" = "standby" ] && \
    [ "$(jq -r '.local.hasData' <<< "${reinit_status}")" = "true" ] && break
  sleep 5
done
assert_eq "its API reports the standby role" "standby" "$(jq -r '.local.role' <<< "${reinit_status}")"
assert_eq "its API reports it has data" "true" "$(jq -r '.local.hasData' <<< "${reinit_status}")"

# Back to the leader for the switchover below.
if start_pf "${LEADER}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "port-forward back to the leader" "ok" "${pf_ok}"

# A rebuilt replica must become a fully usable one, not merely a running one: wait for the
# leader to see it as a caught-up same-timeline standby, which is exactly what makes it a
# legal switchover target. (Asking before this holds gets a correct 409 from the preflight.)
echo "  Waiting for the rebuilt standby to re-qualify as a switchover candidate (up to 300s)..."
if wait_candidate_ready "${CANDIDATE}"; then cand_ready=ok; else cand_ready=fail; fi
assert_eq "the rebuilt standby re-qualifies as a switchover candidate" "ok" "${cand_ready}"

# --- the switchover actually moves leadership ---

sw_code=$(api_code POST /v1/switchover "{\"leader\":\"${LEADER}\",\"candidate\":\"${CANDIDATE}\"}")
assert_eq "POST /v1/switchover is accepted (202)" "202" "${sw_code}"
recorded=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-target}' 2>/dev/null || true)
assert_eq "the switchover request is on the marker kubectl uses" "${CANDIDATE}" "${recorded}"
requested_by=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-requested-by}' 2>/dev/null || true)
assert_eq "the switchover requester is recorded" "ops-admin" "${requested_by}"

echo "  Waiting for leadership to move to ${CANDIDATE} (up to 300s)..."
NEW_LEADER=""
for _ in $(seq 1 60); do
  NEW_LEADER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ "${NEW_LEADER}" == "${CANDIDATE}" ]] && break
  sleep 5
done
assert_eq "leadership moved to the requested candidate" "${CANDIDATE}" "${NEW_LEADER}"

# One-shot: the loop clears the request (with its requester) so a later, unrelated
# failover cannot re-trigger a handoff to the same pod.
cleared=""
for _ in $(seq 1 24); do
  cleared=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-target}' 2>/dev/null || true)
  [[ -z "${cleared}" ]] && break
  sleep 5
done
assert_eq "the switchover request was cleared (one-shot)" "" "${cleared}"
cleared_by=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-requested-by}' 2>/dev/null || true)
assert_eq "the requester was cleared with the request" "" "${cleared_by}"

# The new primary serves writes, i.e. the handoff was a real switchover. The lease moves
# BEFORE the promotion completes (the candidate acquires it, then promotes), so poll
# rather than asserting on the instant the holder changed.
write_ok=fail
for _ in $(seq 1 30); do
  if pg_exec "${NAMESPACE}" "${CANDIDATE}" "CREATE TABLE IF NOT EXISTS ctl_switchover (id int); INSERT INTO ctl_switchover VALUES (1);" >/dev/null 2>&1; then
    write_ok=ok; break
  fi
  sleep 5
done
assert_eq "the promoted candidate accepts writes" "ok" "${write_ok}"

# And the API on the NEW leader reports it as primary. Its snapshot refreshes once per
# reconcile tick, so give it a few ticks after the promotion.
if start_pf "${CANDIDATE}" "${LOCAL_PORT}" 9201; then pf_ok=ok; else pf_ok=fail; fi
wait_api_ready || true
assert_eq "port-forward to the new leader established" "ok" "${pf_ok}"
new_status=""
for _ in $(seq 1 24); do
  new_status=$(api_body GET /v1/status)
  [[ "$(jq -r '.local.role // empty' <<< "${new_status}")" == "primary" ]] && break
  sleep 5
done
assert_eq "the new leader reports it holds the lease" "true" "$(jq -r '.holdsLease' <<< "${new_status}")"
assert_eq "the new leader reports the primary role" "primary" "$(jq -r '.local.role' <<< "${new_status}")"

end_suite
print_summary
