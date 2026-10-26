#!/bin/bash
# Live PGPool failover + health-check test (#32). Proves that, with a client connected
# THROUGH PGPool, a primary failover is transparent: PGPool's health check recycles the
# moved RW backend, the failed primary is excluded, writes route to the promoted primary,
# and pre-failover data survives. Agent mode (the default), where PGPool fronts the RW
# (<fullname>) and RO (<fullname>-readonly) Services and the agent owns failover.
# OPT-IN / standalone (run in a clean cluster): `make -C pg test-pgpool-failover`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-pgpool-failover}"
RELEASE="${RELEASE:-pgpoolfo}"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent-pgpool.yaml")
LEASE="${FULLNAME}-leader"
PGPOOL="${FULLNAME}-pgpool.${NAMESPACE}.svc.cluster.local"

begin_suite "PGPool failover + health check (#32)"

# --- install agent mode + pgpool (replicaCount 1 -> 2 pg pods + pgpool) ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent-pgpool.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-pgpool" 300

POD0="${FULLNAME}-0"; POD1="${FULLNAME}-1"
PGPW=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.password}' | base64 -d)

# via_pgpool <exec-pod> <sql> -> stdout+stderr, psql rc preserved. Connects to the PGPool
# Service (frontend :9999) as the app user; PGPool decides the backend. The exec pod is
# only a shell host -- it is NOT the routing target.
via_pgpool() {
  kubectl exec -n "${NAMESPACE}" "$1" -c postgresql -- \
    env PGPASSWORD="${PGPW}" psql -h "${PGPOOL}" -p 9999 -U testuser -d testdb -tAc "$2" 2>&1
}

# SHOW POOL_NODES rows are node_id|hostname|port|status|pg_status|...; pgpool's own attach state
# is `status`. `pg_status` turns "up" as soon as the backend answers, while the node may still be
# detached, so a bare "|up|" match would report a re-attach that has not happened. An attached
# node no client has used yet is "waiting".
# up_backends <rows> -> how many backends pgpool has attached.
up_backends() { printf '%s\n' "$1" | awk -F'|' '$4 == "up" || $4 == "waiting"' | wc -l | tr -d ' '; }
# ro_attached <rows> -> "true" when pgpool has the -readonly backend attached, "false" when it is
# detached, "" when the row is missing.
ro_attached() {
  printf '%s\n' "$1" | awk -F'|' '$2 ~ /-readonly\./ { print (($4 == "up" || $4 == "waiting") ? "true" : ($4 == "down" ? "false" : "")) }'
}

# --- settle the primary/standby pair ---
echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a primary" "${PRIMARY:-none}" "${FULLNAME}"

# --- health check: PGPool sees its backends; the primary (RW Service) backend is up ---
nodes=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
up_count=$(up_backends "${nodes}")
assert_gt "#32 health check: PGPool reports a backend up (SHOW POOL_NODES)" "${up_count}" "0"

# --- write through PGPool before failover -> routes to the RW (primary) backend ---
FV="before-$(date +%s)"
mk1=$(via_pgpool "${STANDBY}" "CREATE TABLE IF NOT EXISTS pp_fo (id serial PRIMARY KEY, v text); INSERT INTO pp_fo (v) VALUES ('${FV}'); SELECT 'ok'" || true)
assert_contains "#32: write through PGPool reaches the primary (pre-failover)" "${mk1}" "ok"
# the write actually landed on the elected primary
on_primary=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT v FROM pp_fo WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#32: pre-failover write is on the elected primary" "${FV}" "${on_primary}"

# --- fail the primary over: delete it; the agent promotes the standby + repoints the RW Service ---
echo "  Deleting primary ${PRIMARY} (failover; PGPool must follow the RW Service)..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
promoted=false; e=0
while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "t" && "${holder}" == "${STANDBY}" ]]; then promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; fi
  sleep 3; e=$((e + 3))
done
assert_eq "#32: standby promoted on failover" "true" "${promoted}"

if [[ "${promoted}" == "true" ]]; then
  # --- recovery THROUGH PGPool: writes resume against the new primary. Retry while
  #     PGPool's health check recycles the RW backend connection to the moved endpoint. ---
  AFTER="after-$(date +%s)"
  wrote=false; w=0
  while [[ ${w} -lt ${FAILOVER_BUDGET} ]]; do
    out=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${AFTER}'); SELECT 'ok'" || true)
    if printf '%s' "${out}" | grep -q "ok"; then wrote=true; echo "  PGPool write recovered after ${w}s"; break; fi
    sleep 3; w=$((w + 3))
  done
  assert_eq "#32: client connections recover -- write through PGPool routes to the new primary" "true" "${wrote}"

  # both markers visible through PGPool: pre-failover data survived + reads route correctly
  both=$(via_pgpool "${STANDBY}" "SELECT count(*) FROM pp_fo WHERE v IN ('${FV}','${AFTER}')" || echo "")
  assert_eq "#32: pre- and post-failover rows both readable through PGPool" "2" "$(printf '%s' "${both}" | tr -d '[:space:]')"

  # the failed backend is excluded / the RW backend reconnected to the new primary: a
  # backend is up again and the post-failover write is confirmed on the new primary
  on_new=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM pp_fo WHERE v='${AFTER}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#32: post-failover write landed on the new primary" "${AFTER}" "${on_new}"
  nodes2=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  up2=$(up_backends "${nodes2}")
  assert_gt "#32: PGPool has a healthy backend after failover (excluded the dead primary)" "${up2}" "0"
else
  skip "#32: client connections recover (failover did not complete)"
  skip "#32: pre- and post-failover rows both readable through PGPool (failover did not complete)"
  skip "#32: post-failover write landed on the new primary (failover did not complete)"
  skip "#32: PGPool has a healthy backend after failover (failover did not complete)"
fi

# --- #361: losing the only standby must not take PGPool (and writes) down ---
# Every new PGPool session connects to every attached backend first, so an RO Service with no
# ready endpoint used to stall every session; on a CNI that drops instead of rejecting, the
# stall outlived the probes and the kubelet restarted PGPool in a loop with a healthy primary.
# The RO backend is now detachable: PGPool must mark it down, keep serving writes without a
# restart, and the re-attach loop must bring it back once the Service answers again. KinD's
# kube-proxy REJECTS an endpointless Service, so this exercises the detach/re-attach path, not
# the drop-induced stall (which the health-check and probe budgets cover at render time).
echo "  #361: waiting for the former primary to rejoin so the -readonly Service has an endpoint..."
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
ro_ready=false; s=0
while [[ ${s} -lt 240 ]]; do
  ro_eps=$(kubectl get endpointslices -n "${NAMESPACE}" -l "kubernetes.io/service-name=${FULLNAME}-readonly" -o jsonpath='{range .items[*].endpoints[*]}{.conditions.ready}{" "}{end}' 2>/dev/null || true)
  nodes_pre=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if [[ "${ro_eps}" == *true* ]] && [[ "$(ro_attached "${nodes_pre}")" == "true" ]]; then ro_ready=true; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: precondition -- the RO backend is attached and the -readonly Service has a ready endpoint" "true" "${ro_ready}"
PGPOOL_POD=$(kubectl get pod -n "${NAMESPACE}" -l app.kubernetes.io/component=pgpool -o jsonpath='{.items[0].metadata.name}')
restarts_before=$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')
echo "  #361: emptying the -readonly Service (what one unready standby produces)..."
kubectl patch svc "${FULLNAME}-readonly" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":"no-such-role"}}}' >/dev/null
detached=false; s=0
while [[ ${s} -lt 120 ]]; do
  nodes_mid=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if [[ "$(ro_attached "${nodes_mid}")" == "false" ]]; then detached=true; echo "  RO backend detached after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: PGPool detaches the unreachable RO backend" "true" "${detached}"
W361="standby-away-$(date +%s)"
w361=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${W361}'); SELECT v FROM pp_fo WHERE v='${W361}'" || true)
assert_contains "#361: writes and reads flow through PGPool with the RO backend down" "${w361}" "${W361}"
assert_eq "#361: PGPool stays Ready" "true" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].ready}')"
assert_eq "#361: ... and is not restarted" "${restarts_before}" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')"
echo "  #361: restoring the -readonly Service; the re-attach loop must bring the backend back..."
kubectl patch svc "${FULLNAME}-readonly" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":"standby"}}}' >/dev/null
reattached=false; s=0
while [[ ${s} -lt 120 ]]; do
  nodes_post=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if [[ "$(ro_attached "${nodes_post}")" == "true" ]]; then reattached=true; echo "  RO backend re-attached after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#361: the re-attach loop re-attaches the RO backend once the Service answers" "true" "${reattached}"
assert_contains "#361: ... and says so in the pgpool log" "$(kubectl logs -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool --since=10m 2>/dev/null | grep -m1 '\[reattach\]' || true)" "re-attaching"
assert_eq "#361: no restart across the whole episode" "${restarts_before}" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}')"

# --- #32 revisited: the failover above must not have flipped pgpool's readiness either ---
assert_eq "#374: pgpool is Ready after the failover (the 60s readiness window outlasts a normal failover)" "true" "$(kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].ready}')"

pgpool_ready() { kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].ready}' 2>/dev/null || echo ""; }
pgpool_restarts() { kubectl get pod -n "${NAMESPACE}" "${PGPOOL_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="pgpool")].restartCount}' 2>/dev/null || echo ""; }
pgpool_log_count() { kubectl logs -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool 2>/dev/null | grep -cF -- "$1" || true; }

# --- #375: a PCP authentication fault must not restart pgpool, and must be logged once ---
# Since 2.5.0 the probes authenticate over PCP; a fault there (a ':' in the admin username,
# any pcp.conf/.pcppass drift) failed liveness every 10s and restarted every replica in a loop.
# Reproduce the drift in place: overwrite .pcppass (the emptyDir is writable) with a wrong
# password, then outwait the liveness window (6 x 10s) with margin. pgpool must keep serving,
# stay Ready, not restart, and say so exactly once in its own log; restoring the password must
# be acknowledged once too.
echo "  #375: breaking PCP authentication in place (wrong password in .pcppass)..."
restarts_375=$(pgpool_restarts)
# Deltas, not totals: a rerun into a surviving pod (helm upgrade --install changes nothing)
# keeps the previous episode's lines in the same container log.
fault_lines_before=$(pgpool_log_count '[probe] PCP authentication failed')
recovery_lines_before=$(pgpool_log_count '[probe] PCP authentication works again')
kubectl exec -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool -- sh -c 'cp /usr/local/etc/.pcppass /usr/local/etc/.pcppass.good && printf "127.0.0.1:9898:%s:wrong\n" "$(cat /usr/local/etc/pcp_user)" > /usr/local/etc/.pcppass'
sleep 90
assert_eq "#375: pgpool is not restarted over a PCP authentication fault" "${restarts_375}" "$(pgpool_restarts)"
assert_eq "#375: ... and stays Ready" "true" "$(pgpool_ready)"
W375="pcp-auth-broken-$(date +%s)"
w375=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${W375}'); SELECT v FROM pp_fo WHERE v='${W375}'" || true)
assert_contains "#375: ... and keeps serving writes" "${w375}" "${W375}"
assert_eq "#375: the fault is logged once in the pgpool container log" "$((fault_lines_before + 1))" "$(pgpool_log_count '[probe] PCP authentication failed')"
echo "  #375: restoring .pcppass..."
kubectl exec -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool -- sh -c 'mv /usr/local/etc/.pcppass.good /usr/local/etc/.pcppass'
sleep 25
assert_eq "#375: the recovery is logged once" "$((recovery_lines_before + 1))" "$(pgpool_log_count '[probe] PCP authentication works again')"
assert_eq "#375: still no restart" "${restarts_375}" "$(pgpool_restarts)"

# --- #374: an unreachable primary must make pgpool unready (readiness, not liveness) ---
# pgpool's own view of backend 0 can never say down (ALWAYS_PRIMARY|DISALLOW_TO_FAILOVER), so
# readiness asks the backend itself with pg_isready. Make the RW Service unroutable without
# touching the pod: the agent re-asserts only the statefulset.kubernetes.io/pod-name key of the
# selector each tick, so an EXTRA key it never writes survives it and empties the endpoints.
echo "  #374: emptying the RW Service (extra selector key the agent does not own)..."
kubectl patch svc "${FULLNAME}" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":"no-such-role"}}}' >/dev/null
unready=false; s=0
while [[ ${s} -lt 150 ]]; do
  if [[ "$(pgpool_ready)" == "false" ]]; then unready=true; echo "  pgpool unready after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#374: pgpool leaves Ready once the primary stops answering" "true" "${unready}"
assert_eq "#374: ... without a restart (readiness, not liveness)" "${restarts_375}" "$(pgpool_restarts)"
pgpool_eps=$(kubectl get endpointslices -n "${NAMESPACE}" -l "kubernetes.io/service-name=${FULLNAME}-pgpool" -o jsonpath='{range .items[*].endpoints[*]}{.conditions.ready}{" "}{end}' 2>/dev/null || true)
assert_not_contains "#374: ... and the pgpool Service has no ready endpoint" "${pgpool_eps}" "true"
echo "  #374: restoring the RW Service selector..."
kubectl patch svc "${FULLNAME}" -n "${NAMESPACE}" --type merge -p '{"spec":{"selector":{"pg-role":null}}}' >/dev/null
ready_again=false; s=0
while [[ ${s} -lt 120 ]]; do
  if [[ "$(pgpool_ready)" == "true" ]]; then ready_again=true; echo "  pgpool Ready again after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#374: pgpool is Ready again once the primary answers" "true" "${ready_again}"
assert_eq "#374: no restart across the whole episode" "${restarts_375}" "$(pgpool_restarts)"

# --- #366: a password rotated in the release Secret reaches pgpool without a restart ---
# pool_passwd used to be an env snapshot taken at pod start, and nothing rolled the pod on a
# Secret change: a rotated password left pgpool on the old one until a restart nobody announced,
# Ready all the while. Now the Secret is mounted as files and the password-sync loop rewrites
# pool_passwd in place, gated on the new value logging in to backend 0. Secret first (the
# application role): pgpool must keep the previous password, say so once, and switch within an
# interval of the primary taking the new one; then the new password logs in through pgpool and
# the old does not. Database first (the check role): pgpool must follow the Secret once the
# kubelet refreshes the mount, and its health checks must end up working again on the new
# password with every backend attached. No restart anywhere; pgpool stays Ready throughout.
current_primary() {
  for p in "${POD0}" "${POD1}"; do
    [[ "$(pg_exec "${NAMESPACE}" "${p}" "SELECT pg_is_in_recovery()" 2>/dev/null || echo "")" == "f" ]] && { echo "${p}"; return; }
  done
}
# via_pgpool_as <exec-pod> <user> <password> <sql> -> stdout+stderr, psql rc preserved.
via_pgpool_as() {
  kubectl exec -n "${NAMESPACE}" "$1" -c postgresql -- \
    env PGPASSWORD="$3" psql -h "${PGPOOL}" -p 9999 -U "$2" -d testdb --no-psqlrc -tAc "$4" 2>&1
}
secret_set() {  # <key> <value>
  kubectl patch secret "${FULLNAME}" -n "${NAMESPACE}" -p "{\"data\":{\"$1\":\"$(printf '%s' "$2" | base64 -w0)\"}}" >/dev/null
}
wait_log_delta() {  # <needle> <count-before> <budget-s> -> seconds waited, or "" on timeout
  local s=0
  while [[ ${s} -lt $3 ]]; do
    [[ "$(pgpool_log_count "$1")" -gt "$2" ]] && { echo "${s}"; return; }
    sleep 5; s=$((s + 5))
  done
  echo ""
}
PRIMARY_366=$(current_primary)
[[ -n "${PRIMARY_366}" ]] || { echo "#366: no primary answers; cannot run the rotation checks" >&2; end_suite; print_summary; exit 1; }
restarts_366=$(pgpool_restarts)
APP_PW_OLD=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.app-password}' | base64 -d)
APP_PW_NEW="rot-app-$(date +%s)"
WAIT_LINE='[pool_passwd] app_user: the Secret carries a new password that backend 0 does not accept yet'
SWITCH_LINE='[pool_passwd] app_user: the password rotated in the Secret authenticates against backend 0'
RELOAD_LINE='[pool_passwd] pool_passwd rewritten in place and pgpool reloaded'
assert_eq "#366: the application role logs in through pgpool before the rotation" "app_user" "$(via_pgpool_as "${STANDBY}" app_user "${APP_PW_OLD}" 'SELECT current_user' || true)"
echo "  #366: rotating app-password in the Secret (the database keeps the old one for now)..."
wait_before=$(pgpool_log_count "${WAIT_LINE}")
switch_before=$(pgpool_log_count "${SWITCH_LINE}")
reload_before=$(pgpool_log_count "${RELOAD_LINE}")
secret_set app-password "${APP_PW_NEW}"
# The kubelet refreshes a Secret volume on its sync period (about a minute), then the loop sees
# it within one interval (10s).
waited=$(wait_log_delta "${WAIT_LINE}" "${wait_before}" 240)
[[ -n "${waited}" ]] && echo "  pgpool saw the rotated Secret after ${waited}s and kept the previous password"
assert_gt "#366: pgpool sees the rotated Secret and keeps the previous password while the primary refuses the new one" "${waited:--1}" -1
assert_eq "#366: ... the old password still logs in through pgpool" "app_user" "$(via_pgpool_as "${STANDBY}" app_user "${APP_PW_OLD}" 'SELECT current_user' || true)"
sleep 25
assert_eq "#366: ... logged once per episode, not per pass" "$((wait_before + 1))" "$(pgpool_log_count "${WAIT_LINE}")"
assert_eq "#366: ... and not switched yet" "${switch_before}" "$(pgpool_log_count "${SWITCH_LINE}")"
echo "  #366: applying the new app-password on the primary ${PRIMARY_366}..."
pg_exec "${NAMESPACE}" "${PRIMARY_366}" "ALTER ROLE app_user PASSWORD '${APP_PW_NEW}'" >/dev/null
switched=$(wait_log_delta "${SWITCH_LINE}" "${switch_before}" 60)
[[ -n "${switched}" ]] && echo "  pgpool switched after ${switched}s"
assert_gt "#366: pgpool switches within an interval of the primary taking the new password" "${switched:--1}" -1
# A child that authenticated the role before serves its buffered pool_passwd until the reload
# makes it reopen the file, so the login is only deterministic after the reload line.
reloaded=$(wait_log_delta "${RELOAD_LINE}" "${reload_before}" 30)
assert_gt "#366: ... the rewrite is followed by a PCP reload" "${reloaded:--1}" -1
sleep 3
# Several sessions, not one: each lands on a different pgpool child, and the stale-buffer
# failure is per child (a child that authenticated the role before the rotation).
new_ok=0; old_ok=0
for i in 1 2 3 4 5 6 7 8; do
  [[ "$(via_pgpool_as "${STANDBY}" app_user "${APP_PW_NEW}" 'SELECT current_user' || true)" == "app_user" ]] && new_ok=$((new_ok + 1))
  via_pgpool_as "${STANDBY}" app_user "${APP_PW_OLD}" 'SELECT 1' >/dev/null 2>&1 && old_ok=$((old_ok + 1))
done
assert_eq "#366: ... the new password logs in through pgpool (8 of 8 sessions)" "8" "${new_ok}"
assert_eq "#366: ... and the old one no longer does (0 of 8 sessions)" "0" "${old_ok}"
pool_passwd_366=$(kubectl exec -n "${NAMESPACE}" "${PGPOOL_POD}" -c pgpool -- cat /usr/local/etc/pool_passwd)
assert_contains "#366: pool_passwd carries the new password, in place" "${pool_passwd_366}" "app_user:TEXT${APP_PW_NEW}"
assert_not_contains "#366: ... and not the old one" "${pool_passwd_366}" "${APP_PW_OLD}"
assert_eq "#366: no restart" "${restarts_366}" "$(pgpool_restarts)"
assert_eq "#366: pgpool stays Ready" "true" "$(pgpool_ready)"

CHK_PW_NEW="rot-chk-$(date +%s)"
CHK_SWITCH_LINE='[pool_passwd] pgpool_check: the password rotated in the Secret authenticates against backend 0'
echo "  #366: rotating the check role on the primary first, then in the Secret..."
chk_switch_before=$(pgpool_log_count "${CHK_SWITCH_LINE}")
chk_reload_before=$(pgpool_log_count "${RELOAD_LINE}")
pg_exec "${NAMESPACE}" "${PRIMARY_366}" "ALTER ROLE pgpool_check PASSWORD '${CHK_PW_NEW}'" >/dev/null
secret_set pgpool-check-password "${CHK_PW_NEW}"
chk_switched=$(wait_log_delta "${CHK_SWITCH_LINE}" "${chk_switch_before}" 240)
[[ -n "${chk_switched}" ]] && echo "  pgpool took the new check password after ${chk_switched}s"
assert_gt "#366: the check role's new password is switched in once the Secret carries it" "${chk_switched:--1}" -1
chk_reloaded=$(wait_log_delta "${RELOAD_LINE}" "${chk_reload_before}" 30)
assert_gt "#366: ... and reloaded" "${chk_reloaded:--1}" -1
# The health checks ran on the old password while the Secret propagated and may have detached
# the RO backend; on the new one they pass again and the re-attach loop brings it back.
attached=false; s=0
while [[ ${s} -lt 90 ]]; do
  rows=$(via_pgpool "${STANDBY}" "SHOW POOL_NODES" || true)
  if [[ "$(up_backends "${rows}")" == "2" ]]; then attached=true; echo "  both backends attached after ${s}s"; break; fi
  sleep 5; s=$((s + 5))
done
assert_eq "#366: pgpool's health checks work on the rotated check password (both backends attached)" "true" "${attached}"
W366="pw-rotated-$(date +%s)"
w366=$(via_pgpool "${STANDBY}" "INSERT INTO pp_fo (v) VALUES ('${W366}'); SELECT v FROM pp_fo WHERE v='${W366}'" || true)
assert_contains "#366: ... and writes through pgpool still land" "${w366}" "${W366}"
assert_eq "#366: no restart across both rotations" "${restarts_366}" "$(pgpool_restarts)"
assert_eq "#366: pgpool is Ready at the end" "true" "$(pgpool_ready)"

end_suite
print_summary
