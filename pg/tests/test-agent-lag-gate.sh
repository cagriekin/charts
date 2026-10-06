#!/bin/bash
# RPO gate on automatic failover (#273): ha.agent.maximumLagOnFailover. A lease-holding
# standby further than the limit behind the primary's last marker-recorded position must
# REFUSE to promote and release the lease; a caught-up standby must still fail over normally;
# the pg-ha/accept-failover-lag annotation must override the refusal; and the former primary
# must rejoin afterwards. Lag is induced by planting a far-future position on the marker once
# the primary is gone (recording continues while paused, so the plant has to follow the
# scale-down), so the scenario is deterministic and needs no real replication stall.
# Standalone, opt-in:
# `make -C pg test-agent-lag-gate`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-lag}"
RELEASE="${RELEASE:-pg-lag}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
MARKER="${FULLNAME}-primary"
POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"
LIMIT=1048576
FAILOVER_BUDGET="${FAILOVER_BUDGET:-120}"

begin_suite "Agent RPO gate on automatic failover (#273)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Installing a 2-node agent cluster with maximumLagOnFailover=${LIMIT}..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set ha.agent.maximumLagOnFailover="${LIMIT}" \
  --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

env_val=$(kubectl get pod -n "${NAMESPACE}" "${POD0}" -o jsonpath='{.spec.containers[?(@.name=="postgresql")].env[?(@.name=="MAX_LAG_ON_FAILOVER_BYTES")].value}')
assert_eq "#273: the agent env carries the limit" "${LIMIT}" "${env_val}"

# --- discover roles (Parallel pod management: the lease decides, not the ordinal) ---
settle_roles() { # sets PRIMARY STANDBY HOLDER; waits up to $1 seconds for one primary that holds the lease
  local budget="$1" elapsed=0 r0 r1
  PRIMARY=""; STANDBY=""; HOLDER=""
  while [[ ${elapsed} -lt ${budget} ]]; do
    r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
    PRIMARY=""; STANDBY=""
    if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; fi
    if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; fi
    if [[ -n "${PRIMARY}" && "${HOLDER}" == "${PRIMARY}" ]]; then return 0; fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}
in_recovery() { pg_exec "${NAMESPACE}" "$1" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo ""; }
marker_lsn() { kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.data.lsn}' 2>/dev/null || echo ""; }
# The agent's metrics listener (port 9200) answers plain HTTP; the HA image has no curl, so
# bash's /dev/tcp does the request.
agent_metric() {  # agent_metric <pod> <metric>: the agent's /metrics has no curl in the image; bash /dev/tcp does.
  kubectl exec -n "${NAMESPACE}" "$1" -c postgresql -- bash -c \
    'exec 3<>/dev/tcp/127.0.0.1/9200; printf "GET /metrics HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3; timeout 5 cat <&3' 2>/dev/null \
    | awk -v m="$2" '$1 == m {print $2}' | head -1
}
refused_count() { agent_metric "$1" pg_ha_agent_promotions_refused_lag_total; }
# wait_replayed <standby> <value>: bounded poll until the row is visible on the standby. A fixed
# sleep proves nothing about replication; the assertions after a failover read this row.
wait_replayed() {
  local w=0
  while [[ ${w} -lt 60 ]]; do
    [[ "$(pg_exec "${NAMESPACE}" "$1" "SELECT v FROM lag_gate WHERE v='$2'" "testuser" "testdb" 2>/dev/null || true)" == "$2" ]] && return 0
    sleep 2; w=$((w + 2))
  done
  return 1
}

echo "Waiting for the roles to settle..."
if settle_roles 240; then
  pass "install: one primary holds the lease (${PRIMARY})"
else
  fail "install: one primary holds the lease" "primary=${PRIMARY:-none} holder=${HOLDER:-none}"
  end_suite; print_summary; exit 1
fi

# --- the primary records its position on the marker while the gate is enabled ---
lsn=""; waited=0
while [[ ${waited} -lt 60 ]]; do
  lsn=$(marker_lsn); [[ -n "${lsn}" ]] && break; sleep 5; waited=$((waited + 5))
done
assert_contains "#273: the primary records its write position on the marker (data.lsn)" "${lsn}" '^[0-9A-F][0-9A-F]*/[0-9A-F][0-9A-F]*$'
# The position is written by merge patch, which needs its own verb on the scoped marker rule.
sa="system:serviceaccount:${NAMESPACE}:$(kubectl get sts "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
can_patch=$(kubectl auth can-i patch "configmaps/${MARKER}" -n "${NAMESPACE}" --as="${sa}" 2>/dev/null || true)
assert_eq "#273: the agent SA may patch the marker" "yes" "${can_patch}"
can_patch_other=$(kubectl auth can-i patch configmaps/other -n "${NAMESPACE}" --as="${sa}" 2>/dev/null || true)
assert_eq "#273: ... but no other ConfigMap" "no" "${can_patch_other}"
lsn_later=""; waited=0
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS lag_gate (id serial PRIMARY KEY, v text)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO lag_gate (v) SELECT repeat('x', 1000) FROM generate_series(1, 2000)" "testuser" "testdb" >/dev/null
while [[ ${waited} -lt 60 ]]; do
  lsn_later=$(marker_lsn); [[ -n "${lsn_later}" && "${lsn_later}" != "${lsn}" ]] && break; sleep 5; waited=$((waited + 5))
done
assert_not_eq "#273: the recorded position advances with writes" "${lsn}" "${lsn_later}"

# --- a caught-up standby still fails over: the gate does not block a healthy cluster ---
FV="before-failover-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO lag_gate (v) VALUES ('${FV}')" "testuser" "testdb" >/dev/null
wait_replayed "${STANDBY}" "${FV}" && pass "#273: the standby replayed the pre-failover row" || fail "#273: the standby replayed the pre-failover row" "not visible on ${STANDBY} within 60s"
echo "Deleting primary ${PRIMARY} (graceful) -- the caught-up standby ${STANDBY} must promote..."
OLD_PRIMARY="${PRIMARY}"
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false >/dev/null 2>&1 || true
promoted=false; elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(in_recovery "${STANDBY}")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "f" && "${holder}" == "${STANDBY}" ]]; then promoted=true; echo "  failover complete after ${elapsed}s"; break; fi
  sleep 3; elapsed=$((elapsed + 3))
done
assert_eq "#273: a caught-up standby promotes under the gate" "true" "${promoted}"
assert_eq "#273: ... with the data" "${FV}" "$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM lag_gate WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || true)"
assert_eq "#273: ... without a refusal counted" "0" "$(refused_count "${STANDBY}" || echo 0)"
echo "Waiting for ${OLD_PRIMARY} to come back as a standby..."
NEW_PRIMARY="${STANDBY}"
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
settle_roles 240 || true   # re-derives PRIMARY/STANDBY from the live roles
assert_eq "#273: the promoted standby is the primary after the failover" "${NEW_PRIMARY}" "${PRIMARY}"
assert_eq "#273: the former primary rejoined as a standby" "t" "$(in_recovery "${OLD_PRIMARY}")"

# --- put the primary on the highest ordinal so a scale-down removes exactly it ---
# One controlled switchover when needed (the install's primary is the lease's pick, not the
# ordinal). Not forced when the primary already sits there: a second back-to-back handoff was
# not accepted within the budget in two suite runs while the same sequence completes in
# seconds in isolation, and the scenario below does not depend on it -- the pause-right-after-
# promotion case it was meant to exercise is covered by the first-position wait below either way.
switchover_to() {  # switchover_to <pod>: request the handoff, wait for the roles to settle on it AND for the
  # demoted node to stream again -- a second switchover is only accepted once its target is a caught-up standby.
  echo "Primary is ${PRIMARY}; requesting a controlled switchover to $1..."
  kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/switchover-target="$1" --overwrite >/dev/null
  local e=0 streaming=""
  while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
    if settle_roles 5 && [[ "${PRIMARY}" == "$1" ]]; then break; fi
    e=$((e + 5))
  done
  [[ "${PRIMARY}" == "$1" ]] || return 1
  e=0
  while [[ ${e} -lt 180 ]]; do
    streaming=$(pg_exec "${NAMESPACE}" "$1" "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" "testuser" "testdb" 2>/dev/null | xargs || echo "")
    [[ "${streaming}" == "1" ]] && return 0
    sleep 5; e=$((e + 5))
  done
  echo "  (the demoted node did not resume streaming within 180s)"
  return 1
}
if [[ "${PRIMARY}" != "${POD1}" ]]; then switchover_to "${POD1}" || true; fi
assert_eq "#273: the primary is ${POD1} ahead of the blocked-failover scenario" "${POD1}" "${PRIMARY}"
FV2="before-block-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD1}" "INSERT INTO lag_gate (v) VALUES ('${FV2}')" "testuser" "testdb" >/dev/null
wait_replayed "${POD0}" "${FV2}" && pass "#273: the surviving standby replayed the pre-block row" || fail "#273: the surviving standby replayed the pre-block row" "not visible on ${POD0} within 60s"

# --- blocked failover: the recorded position is far ahead of the surviving standby ---
# Pause so the surviving standby takes no action yet, remove the primary for good (scale to 1:
# a deleted pod would just come back), plant a far-future position on the marker, then resume.
# The standby acquires the lease, measures a lag far above the limit, and must refuse.
echo "Pausing, scaling the primary away, planting a far-future recorded position..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/pause=true --overwrite >/dev/null
# Recording is observation, not action: it must continue while paused, or a long maintenance
# window would leave a stale reference for a failover right after resume. A promotion's
# timeline advance retires the previous position, so first wait for the primary's position to
# be present (recorded under pause when the switchover above just happened), then prove it
# keeps moving with writes.
lsn_paused_before=""; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  lsn_paused_before=$(marker_lsn); [[ -n "${lsn_paused_before}" ]] && break; sleep 2; elapsed=$((elapsed + 2))
done
assert_contains "#273: the new primary records its first position while paused" "${lsn_paused_before}" '^[0-9A-F][0-9A-F]*/[0-9A-F][0-9A-F]*$'
pg_exec "${NAMESPACE}" "${POD1}" "INSERT INTO lag_gate (v) SELECT repeat('y', 1000) FROM generate_series(1, 2000)" "testuser" "testdb" >/dev/null
lsn_paused_after=""; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  lsn_paused_after=$(marker_lsn); [[ -n "${lsn_paused_after}" && "${lsn_paused_after}" != "${lsn_paused_before}" ]] && break; sleep 5; elapsed=$((elapsed + 5))
done
assert_not_eq "#273: the recorded position keeps advancing while the cluster is paused" "${lsn_paused_before}" "${lsn_paused_after}"
# ... and against the right identity: the position write is fenced to the marker's primary and
# timeline, so a marker left at the previous primary by the switchover would record nothing.
assert_eq "#273: the marker names the serving primary while paused" "${POD1}" "$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.data.primary}' 2>/dev/null)"
pod1_tl=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT ('x' || substring(pg_walfile_name(pg_current_wal_lsn()) from 1 for 8))::bit(32)::int" "testuser" "testdb" 2>/dev/null || true)
assert_eq "#273: ... on its current timeline" "${pod1_tl}" "$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.data.timeline}' 2>/dev/null)"
# The standby must have OBSERVED the pause before the primary goes away, or it could acquire
# and promote in the gap before the planted position lands; the gauge is the agent's own word.
paused_seen=""; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  paused_seen=$(agent_metric "${POD0}" pg_ha_agent_is_paused || true); [[ "${paused_seen}" == "1" ]] && break; sleep 2; elapsed=$((elapsed + 2))
done
assert_eq "#273: the surviving standby observed the pause (pg_ha_agent_is_paused)" "1" "${paused_seen}"
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=1 >/dev/null
kubectl wait --for=delete "pod/${POD1}" -n "${NAMESPACE}" --timeout=180s >/dev/null 2>&1 || true
kubectl patch configmap "${MARKER}" -n "${NAMESPACE}" --type merge -p '{"data":{"lsn":"FFFFFFFF/FFFFFFF0"}}' >/dev/null
assert_eq "#273: the planted position is on the marker" "FFFFFFFF/FFFFFFF0" "$(marker_lsn)"
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/pause- >/dev/null 2>&1 || true

echo "Observing ${POD0} for 60s: it must stay a standby and refuse to promote..."
still_standby=true; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  rec=$(in_recovery "${POD0}")
  [[ "${rec}" == "t" ]] || { still_standby=false; echo "  ${POD0} left recovery at ${elapsed}s (rec=${rec})"; break; }
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "#273: the lagging standby refuses automatic promotion (still in recovery after 60s)" "true" "${still_standby}"
# The counter is the deterministic proof that it acquired the lease, decided, and released
# (a refusing holder keeps the lease for one tick at most, so polling the Lease is a coin flip).
refused=$(refused_count "${POD0}" || echo 0)
assert_gt "#273: pg_ha_agent_promotions_refused_lag_total counts the refusals (acquire, refuse, release)" "${refused:-0}" "0"
assert_contains "#273: the agent logs the refusal with the limit named" \
  "$(kubectl logs -n "${NAMESPACE}" "${POD0}" -c postgresql --since=3m 2>/dev/null | grep -m1 'refusing automatic promotion' || true)" \
  "maximumLagOnFailover"
assert_contains "#273: the decision carries the lag gate reason" \
  "$(kubectl logs -n "${NAMESPACE}" "${POD0}" -c postgresql --since=3m 2>/dev/null | grep -m1 'lag gate (#273)' || true)" \
  "bytes behind the primary's last recorded position"

# --- operator acceptance: name the standby explicitly ---
echo "Accepting the loss with pg-ha/accept-failover-lag=${POD0}..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/accept-failover-lag="${POD0}" --overwrite >/dev/null
promoted=false; elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(in_recovery "${POD0}")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "f" && "${holder}" == "${POD0}" ]]; then promoted=true; echo "  override promoted ${POD0} after ${elapsed}s"; break; fi
  sleep 3; elapsed=$((elapsed + 3))
done
assert_eq "#273: the accept-failover-lag annotation promotes the lagging standby" "true" "${promoted}"
cleared=""; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  cleared=$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/accept-failover-lag}' 2>/dev/null || echo "")
  [[ -z "${cleared}" ]] && break; sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "#273: the acceptance is one-shot (cleared after the promote)" "" "${cleared}"
assert_eq "#273: data written before the block is on the new primary" "${FV2}" "$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT v FROM lag_gate WHERE v='${FV2}'" "testuser" "testdb" 2>/dev/null || true)"
# The new primary records its own position again, replacing the planted one.
lsn_new=""; elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  lsn_new=$(marker_lsn); [[ -n "${lsn_new}" && "${lsn_new}" != "FFFFFFFF/FFFFFFF0" ]] && break; sleep 5; elapsed=$((elapsed + 5))
done
assert_not_eq "#273: the new primary overwrites the planted position" "FFFFFFFF/FFFFFFF0" "${lsn_new}"

# --- the removed primary comes back and rejoins the new timeline ---
echo "Scaling back to 2: ${POD1} must rejoin ${POD0} as a standby..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=2 >/dev/null
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
assert_eq "#273: ${POD1} is a standby after rejoining" "t" "$(in_recovery "${POD1}")"
streaming=""; elapsed=0
while [[ ${elapsed} -lt 120 ]]; do
  streaming=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" "testuser" "testdb" 2>/dev/null | xargs || echo "")
  [[ "${streaming}" == "1" ]] && break; sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "#273: ${POD1} streams from the new primary" "1" "${streaming}"

end_suite
print_summary
