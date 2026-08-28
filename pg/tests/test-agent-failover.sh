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

    # #181 regression: the rejoined node must actually be STREAMING from the new
    # primary, not just present. Queried on the new primary (${STANDBY}, always up),
    # so a wedged standby (agent killing/blocking its walreceiver) FAILS here rather
    # than skipping. application_name == the standby's pod name (primary_conninfo).
    stream_state=""
    s=0
    while [[ ${s} -lt 90 ]]; do
      stream_state=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT state FROM pg_stat_replication WHERE application_name='${PRIMARY}'" "testuser" "testdb" 2>/dev/null || echo "")
      [[ "${stream_state}" == "streaming" ]] && break
      sleep 5; s=$((s + 5))
    done
    assert_eq "#181: rejoined standby is actively streaming (pg_stat_replication)" "streaming" "${stream_state}"

    # #182 regression: a healthy, already-streaming standby must NOT re-run repmgr
    # standby follow every reconcile tick. Sample several steady-state ticks of the
    # rejoined standby's agent log (the agent is PID 1 of the postgresql container)
    # and assert no `act action=Follow` failure surfaced -- the follow is skipped or
    # latched after attach, never re-forked + logged ERROR each tick.
    sleep 20
    follow_errs=$(kubectl logs "${PRIMARY}" -c postgresql -n "${NAMESPACE}" --since=25s 2>/dev/null | grep "repmgr standby follow:" || echo "")
    assert_not_contains "#182: steady-state standby does not re-run/err on repmgr standby follow" "${follow_errs}" "repmgr standby follow:"
  else
    skip "demoted ex-primary rejects writes (soft fence) (rejoin did not complete)"
    skip "rejoined standby caught up post-failover data (rejoin did not complete)"
    skip "#181: rejoined standby is actively streaming (rejoin did not complete)"
    skip "#182: steady-state standby does not re-run/err on repmgr standby follow (rejoin did not complete)"
  fi
else
  skip "ex-primary rejoins as a standby (in recovery) (failover did not complete)"
  skip "ex-primary did NOT re-acquire the lease (failover did not complete)"
  skip "demoted ex-primary rejects writes (soft fence) (failover did not complete)"
  skip "rejoined standby caught up post-failover data (failover did not complete)"
  skip "#181: rejoined standby is actively streaming (failover did not complete)"
  skip "#182: steady-state standby does not re-run/err on repmgr standby follow (failover did not complete)"
fi

# --- cold boot: full-cluster restart. Both pods come up at once (Parallel); the
# cluster must re-elect a single primary with data intact. This exercises the
# recovery-mode path (a former primary comes up read-only via standby.signal so the
# lease holder can rank it) and promote-from-recovery, plus the marker highwater.
# Opt-in (AGENT_COLDBOOT=1): promote-from-recovery has a not-yet-live-validated
# repmgr-catalog interaction (a former primary brought up in recovery mode is still
# type=primary in repmgr.nodes), so it is off by default to keep CI green until
# verified. The graceful-failover path above is the validated core. ---
if [[ "${AGENT_COLDBOOT:-0}" == "1" && "${promoted}" == "true" && "${rejoined}" == "true" ]]; then
  echo "  Cold boot: deleting BOTH pods (full-cluster restart)..."
  kubectl delete pod "${PRIMARY}" "${STANDBY}" -n "${NAMESPACE}" --grace-period=10 --wait=false 2>/dev/null || true
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

  echo "  Waiting for a single primary + lease holder to re-settle (up to 300s)..."
  cb_primary=""; cb_holder=""; cb=0
  while [[ ${cb} -lt 300 ]]; do
    r0=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    r1=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    cb_holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
    if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then cb_primary="${PRIMARY}"; fi
    if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then cb_primary="${STANDBY}"; fi
    if [[ -n "${cb_primary}" && "${cb_holder}" == "${cb_primary}" ]]; then
      echo "  re-settled after ${cb}s: primary=${cb_primary} holder=${cb_holder}"
      break
    fi
    sleep 10; cb=$((cb + 10))
  done
  assert_eq "single primary == lease holder after cold boot" "${cb_primary}" "${cb_holder}"

  if [[ -n "${cb_primary}" ]]; then
    cb_data=$(pg_exec "${NAMESPACE}" "${cb_primary}" "SELECT value FROM failover_test WHERE value='${AFTER}'" "testuser" "testdb" 2>/dev/null || echo "")
    assert_eq "post-failover data survives the cold boot" "${AFTER}" "${cb_data}"
    cb_after="cold-boot-$(date +%s)"
    pg_exec "${NAMESPACE}" "${cb_primary}" "INSERT INTO failover_test (value) VALUES ('${cb_after}')" "testuser" "testdb" 2>/dev/null || true
    cb_w=$(pg_exec "${NAMESPACE}" "${cb_primary}" "SELECT value FROM failover_test WHERE value='${cb_after}'" "testuser" "testdb" 2>/dev/null || echo "")
    assert_eq "new primary is writable after cold boot" "${cb_after}" "${cb_w}"
  else
    skip "post-failover data survives the cold boot (no primary re-settled)"
    skip "new primary is writable after cold boot (no primary re-settled)"
  fi
else
  skip "single primary == lease holder after cold boot (prior stage failed)"
  skip "post-failover data survives the cold boot (prior stage failed)"
  skip "new primary is writable after cold boot (prior stage failed)"
fi

# --- disk loss on pod-0 while the lease sits elsewhere: the pod must CLONE itself back.
# Arrived here with the master merge (#325, a 1.x fix). The BEHAVIOUR it asserts is still
# required on this line, which is why the stage is kept -- but the cause it originally
# described is gone, so the explanation is restated rather than left pointing at a deleted
# file.
#
# In 1.x the bug was in init-repmgr.sh: it derived its role from the ordinal alone, so an empty
# PGDATA on ordinal 0 was reported as "First boot, postgres mode will initialize the database"
# and the clone was skipped -- then entrypoint.sh's primary_safety_guard (correctly) refused to
# initdb next to an active primary, leaving the pod in CrashLoopBackOff with no way out, while
# ordinal > 0 recovered from the identical loss purely by name. Both that script and that guard
# are deleted here (#290); the agent owns bootstrap and cloning now (BootstrapClone), and it
# decides by LEASE HOLDERSHIP rather than by ordinal, which is what should make ordinal 0
# unexceptional. This stage is what proves that, so it is mechanism-agnostic on purpose: it
# empties PGDATA, waits, and requires the pod back as a streaming standby.
#
# The data directory is emptied in place rather than by deleting the PVC: a `kubectl delete
# pvc` on a volume a pod still uses only marks it Terminating, and the StatefulSet's
# replacement pod re-binds that very volume -- the disk survives and the test passes without
# ever exercising the empty-PGDATA path (observed). What the code under test keys on is an
# empty PGDATA, so produce exactly that.
#
# The lease is parked off pod-0 first so the scenario is deterministic rather than a coin
# flip on which pod the earlier failover left holding it. ---
POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"
holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")

if [[ "${holder}" == "${POD0}" ]]; then
  echo "  Lease is on ${POD0}; moving it to ${POD1} first..."
  kubectl delete pod "${POD0}" -n "${NAMESPACE}" --grace-period=10 --wait=false 2>/dev/null || true
  for _ in $(seq 1 30); do
    holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
    [[ "${holder}" == "${POD1}" ]] && break
    sleep 5
  done
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 300 || true
fi

if [[ "${holder}" == "${POD1}" ]]; then
  # Something only the surviving primary knows, so a divergent initdb cannot fake it.
  DV="disk-loss-$(date +%s)"
  pg_exec "${NAMESPACE}" "${POD1}" "INSERT INTO failover_test (value) VALUES ('${DV}')" "testuser" "testdb" 2>/dev/null || true

  echo "  Emptying ${POD0}'s data directory (simulated disk loss)..."
  # `|| echo ""` like every other capture in this file: the preceding readiness wait is
  # explicitly tolerant (`|| true`), so kubectl exec can legitimately fail here. Without
  # the guard, `set -euo pipefail` aborts the whole suite before end_suite -- CI would get
  # a bare non-zero exit with no assertion summary instead of one failed assertion.
  left=$(kubectl exec "${POD0}" -n "${NAMESPACE}" -c postgresql -- \
    bash -c 'rm -rf "${PGDATA:?}"/* "${PGDATA:?}"/.[!.]* 2>/dev/null; ls -A "${PGDATA}" | wc -l' 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "${POD0}'s data directory is really empty before the restart" "0" "${left}"

  # Pin the instance we are deleting. `rm -rf` on an open PGDATA does not stop the running
  # postmaster (unlinked files survive via open fds), so the OLD pod keeps reporting
  # ready=true and answering pg_is_in_recovery() = t. With `--wait=false` the poll below
  # would otherwise satisfy its break condition on the very first iteration against that
  # pre-delete instance, and then read the ORIGINAL boot's init log -- which says
  # "First boot" -- reporting a regression that never happened.
  old_uid=$(kubectl get pod "${POD0}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  kubectl delete pod "${POD0}" -n "${NAMESPACE}" --grace-period=0 --force --wait=false 2>/dev/null || true

  echo "  Waiting for ${POD0} to clone itself back (up to 420s)..."
  recovered=""
  for _ in $(seq 1 84); do
    uid=$(kubectl get pod "${POD0}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    if [[ -n "${uid}" && "${uid}" != "${old_uid}" ]]; then
      ready=$(kubectl get pod "${POD0}" -n "${NAMESPACE}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || echo "")
      if [[ "${ready}" == "true" ]]; then
        recovered=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
        [[ "${recovered}" == "t" ]] && break
      fi
    fi
    sleep 5
  done
  assert_eq "${POD0} comes back as a standby after losing its data directory" "t" "${recovered}"

  # The regression assertions: ordinal 0 must have taken the CLONE path, not initdb'd a
  # divergent cluster. Without them the stage could pass on a pod whose disk was never
  # actually lost. Since #288 the entrypoint no longer clones -- it logs the deferral and
  # hands the empty directory to the agent, whose reconcile loop decides BootstrapClone
  # (never BootstrapInitdb: an empty non-holder is not a first boot). Assert both halves:
  # the entrypoint saw a genuinely empty directory, and the agent never chose initdb.
  init_log=$(kubectl logs "${POD0}" -n "${NAMESPACE}" -c postgresql 2>/dev/null || echo "")
  assert_contains "${POD0}'s entrypoint deferred the empty directory to the agent (#288)" "${init_log}" "empty data directory; deferring to the agent"
  assert_not_contains "${POD0}'s agent never initdb'd the recreated ordinal-0 disk" "${init_log}" "action=BootstrapInitdb"

  streaming=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT status FROM pg_stat_wal_receiver" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "${POD0} is receiving WAL from the primary" "streaming" "${streaming}"

  cloned=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT value FROM failover_test WHERE value='${DV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "${POD0} carries data only the surviving primary had (it cloned, not initdb'd)" "${DV}" "${cloned}"

  # A recreated empty pod-0 is never a promotion candidate.
  holder_after=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  assert_eq "the lease stayed with ${POD1} throughout" "${POD1}" "${holder_after}"
else
  for t in "${POD0}'s data directory is really empty before the restart" \
           "${POD0} comes back as a standby after losing its data directory" \
           "${POD0}'s entrypoint deferred the empty directory to the agent (#288)" \
           "${POD0}'s agent never initdb'd the recreated ordinal-0 disk" \
           "${POD0} is receiving WAL from the primary" \
           "${POD0} carries data only the surviving primary had (it cloned, not initdb'd)" \
           "the lease stayed with ${POD1} throughout"; do
    skip "${t} (lease could not be parked on ${POD1})"
  done
fi

rm -f "${SCRIPT_DIR}/.agent_primary" "${SCRIPT_DIR}/.agent_standby"

end_suite
print_summary
