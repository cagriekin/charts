#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent}"
RELEASE="${RELEASE:-pg-agent}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"

# Roles discovered by test-agent.sh (run first); fall back to pod-0/pod-1.
PRIMARY=$(cat "${SCRIPT_DIR}/.agent_primary" 2>/dev/null || echo "${FULLNAME}-0")
STANDBY=$(cat "${SCRIPT_DIR}/.agent_standby" 2>/dev/null || echo "${FULLNAME}-1")

begin_suite "Agent Failover (lease moves, standby promotes, ex-primary rejoins)"

# leaseDuration 15s + retryPeriod 2s + promote + routing; generous margin.
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"

is_primary=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "${PRIMARY} starts as primary" "t" "${is_primary}"

# Write data before failover
FV="before-failover-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS failover_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO failover_test (value) VALUES ('${FV}')" "testuser" "testdb"
sleep 3

# --- Graceful failover: delete the primary pod. SIGTERM -> the agent releases the
# Lease and stops postgres; the standby's agent acquires the freed Lease and
# promotes. This exercises the standby-promote path end to end. ---
echo "  Deleting primary ${PRIMARY} (graceful SIGTERM handoff)..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true

echo "  Waiting for the standby to become primary + lease holder (up to ${FAILOVER_BUDGET}s)..."
promoted=false
elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "t" && "${holder}" == "${STANDBY}" ]]; then
    promoted=true
    echo "  failover complete after ${elapsed}s (new primary + holder = ${STANDBY})"
    break
  fi
  sleep 3
  elapsed=$((elapsed + 3))
done
assert_eq "standby promoted to primary on failover" "true" "${promoted}"

if [[ "${promoted}" == "true" ]]; then
  survived=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM failover_test WHERE value='${FV}'" "testuser" "testdb")
  assert_eq "data survives failover" "${FV}" "${survived}"

  AFTER="after-failover-$(date +%s)"
  pg_exec "${NAMESPACE}" "${STANDBY}" "INSERT INTO failover_test (value) VALUES ('${AFTER}')" "testuser" "testdb"
  after_val=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM failover_test WHERE value='${AFTER}'" "testuser" "testdb")
  assert_eq "can write to the new primary" "${AFTER}" "${after_val}"

  # --- write Service repoints to the new primary (the agent patches the selector) ---
  echo "  Waiting for the write Service to repoint to ${STANDBY} (up to 60s)..."
  svc_ok=false
  s=0
  while [[ ${s} -lt 60 ]]; do
    ep=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
    if [[ "${ep}" == "${STANDBY}" ]]; then svc_ok=true; break; fi
    sleep 3; s=$((s + 3))
  done
  assert_eq "write service repoints to the new primary" "true" "${svc_ok}"
else
  skip "data survives failover (failover did not complete)"
  skip "can write to the new primary (failover did not complete)"
  skip "write service repoints to the new primary (failover did not complete)"
fi

# --- the ex-primary returns and rejoins as a STANDBY, never re-acquiring the Lease ---
if [[ "${promoted}" == "true" ]]; then
  echo "  Waiting for ${PRIMARY} to rejoin as a standby (up to 300s)..."
  rejoined=false
  r=0
  while [[ ${r} -lt 300 ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "t" ]]; then rejoined=true; echo "  ${PRIMARY} rejoined as standby after ~${r}s"; break; fi
    sleep 10; r=$((r + 10))
  done
  assert_eq "ex-primary rejoins as a standby (in recovery)" "true" "${rejoined}"

  holder_now=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  assert_eq "ex-primary did NOT re-acquire the lease (holder stays ${STANDBY})" "${STANDBY}" "${holder_now}"

  if [[ "${rejoined}" == "true" ]]; then
    # soft fence: the demoted ex-primary serves read-only, never a second writer
    pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO failover_test (value) VALUES ('nope')" "testuser" "testdb" 2>/dev/null && w=true || w=false
    assert_eq "demoted ex-primary rejects writes (soft fence)" "false" "${w}"

    # the rejoined node caught up to the post-failover data
    sleep 5
    caught=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT value FROM failover_test WHERE value='${AFTER}'" "testuser" "testdb" 2>/dev/null || echo "")
    assert_eq "rejoined standby caught up post-failover data" "${AFTER}" "${caught}"
  else
    skip "demoted ex-primary rejects writes (soft fence) (rejoin did not complete)"
    skip "rejoined standby caught up post-failover data (rejoin did not complete)"
  fi
else
  skip "ex-primary rejoins as a standby (in recovery) (failover did not complete)"
  skip "ex-primary did NOT re-acquire the lease (failover did not complete)"
  skip "demoted ex-primary rejects writes (soft fence) (failover did not complete)"
  skip "rejoined standby caught up post-failover data (failover did not complete)"
fi

rm -f "${SCRIPT_DIR}/.agent_primary" "${SCRIPT_DIR}/.agent_standby"

end_suite
print_summary
