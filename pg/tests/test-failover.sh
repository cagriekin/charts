#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-repmgr}"
RELEASE="${RELEASE:-pg-repmgr}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")

begin_suite "Failover (freeze primary postgres, verify promotion)"

POD_PRIMARY="${FULLNAME}-0"
POD_REPLICA="${FULLNAME}-1"

# Verify starting state
is_primary=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "pod-0 starts as primary" "t" "${is_primary}"

# Write data before failover
FAILOVER_VALUE="before-failover-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "CREATE TABLE IF NOT EXISTS failover_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "INSERT INTO failover_test (value) VALUES ('${FAILOVER_VALUE}')" "testuser" "testdb"
sleep 3

# Get the PID of the main postgres process inside the pod
# shareProcessNamespace=true allows cross-container process visibility
PG_PID=$(kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c postgresql -- \
  head -1 /var/lib/postgresql/data/pgdata/postmaster.pid)

# SIGSTOP freezes postgres without killing it, so the container stays alive
# but postgres becomes unresponsive. This gives repmgr time to detect the
# failure (reconnect_attempts=3 * reconnect_interval=10 = 30s) and promote.
echo "  Freezing postgres (PID ${PG_PID}) on ${POD_PRIMARY} with SIGSTOP..."
kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c repmgrd -- kill -STOP "${PG_PID}"

echo "  Waiting for failover (up to 180s)..."
failover_timeout=180
failover_elapsed=0
failover_done=false

while [[ ${failover_elapsed} -lt ${failover_timeout} ]]; do
  replica_is_primary=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${replica_is_primary}" == "t" ]]; then
    failover_done=true
    echo "  Failover detected after ${failover_elapsed}s"
    break
  fi
  sleep 5
  failover_elapsed=$((failover_elapsed + 5))
done

assert_eq "replica promoted to primary" "true" "${failover_done}"

if [[ "${failover_done}" == "true" ]]; then
  survived_val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM failover_test WHERE value='${FAILOVER_VALUE}'" "testuser" "testdb")
  assert_eq "data survives failover" "${FAILOVER_VALUE}" "${survived_val}"

  AFTER_VALUE="after-failover-$(date +%s)"
  pg_exec "${NAMESPACE}" "${POD_REPLICA}" "INSERT INTO failover_test (value) VALUES ('${AFTER_VALUE}')" "testuser" "testdb"
  after_val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM failover_test WHERE value='${AFTER_VALUE}'" "testuser" "testdb")
  assert_eq "can write to new primary" "${AFTER_VALUE}" "${after_val}"

  # Keep old primary frozen so service-updater queries pod-1 (new primary) for
  # the authoritative repmgr.nodes state. If unfrozen, pod-0 still believes
  # it is primary (stale metadata) and the service-updater would not switch.
  echo "  Waiting for service-updater to patch service (up to 120s)..."
  svc_timeout=120
  svc_elapsed=0
  svc_updated=false

  while [[ ${svc_elapsed} -lt ${svc_timeout} ]]; do
    svc_pod=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
    if [[ "${svc_pod}" == "${POD_REPLICA}" ]]; then
      svc_updated=true
      echo "  Service updated after ${svc_elapsed}s"
      break
    fi
    sleep 5
    svc_elapsed=$((svc_elapsed + 5))
  done

  assert_eq "service updated to point to new primary" "true" "${svc_updated}"

  if [[ "${svc_updated}" == "true" ]]; then
    # Regression trap for #109: re-rendering the hardcoded pod-0 selector on
    # upgrade either repointed writes at a standby (helm v3) or failed the
    # upgrade with a field-manager conflict on .spec.selector (helm v4
    # server-side apply, since kubectl-patch owns the field after failover).
    # No --wait: the old primary is still frozen and would never become Ready.
    echo "  Upgrading release after failover (selector must be preserved)..."
    upgrade_rc=0
    helm upgrade "${RELEASE}" "${CHART_DIR}" \
      -n "${NAMESPACE}" \
      -f "${SCRIPT_DIR}/values-repmgr.yaml" > /dev/null 2>&1 || upgrade_rc=$?
    assert_eq "helm upgrade succeeds after failover" "0" "${upgrade_rc}"

    selector_pod=$(kubectl get service -n "${NAMESPACE}" "${FULLNAME}" \
      -o jsonpath='{.spec.selector.statefulset\.kubernetes\.io/pod-name}')
    assert_eq "upgrade preserves selector on new primary" "${POD_REPLICA}" "${selector_pod}"
  else
    skip "helm upgrade succeeds after failover (service never updated)"
    skip "upgrade preserves selector on new primary (service never updated)"
  fi
else
  skip "data survives failover (failover did not complete)"
  skip "can write to new primary (failover did not complete)"
  skip "service updated to point to new primary (failover did not complete)"
  skip "helm upgrade succeeds after failover (failover did not complete)"
  skip "upgrade preserves selector on new primary (failover did not complete)"
fi

# Unfreeze the old primary so it resumes as a stale read-write primary on the
# old timeline -- the exact #123 split-brain state, with its container never
# having restarted.
kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c repmgrd -- kill -CONT "${PG_PID}" 2>/dev/null || true

if [[ "${failover_done}" == "true" ]]; then
  # --- #124: selector must NOT follow the resurrected stale primary ---
  # Both pods are now live primaries (stale pod-0 on the old timeline, real
  # pod-1 on the new one). The service-updater must classify this as a
  # split-brain and leave the selector on pod-1, not repoint it to the
  # lowest-ordinal node by its stale self-reported metadata. Wait past one
  # monitoring interval (default 30s) so at least one tick observes both.
  echo "  #124: holding stale primary up for ~40s to let service-updater tick..."
  sleep 40
  selector_during_split=$(kubectl get service -n "${NAMESPACE}" "${FULLNAME}" \
    -o jsonpath='{.spec.selector.statefulset\.kubernetes\.io/pod-name}' 2>/dev/null || echo "")
  assert_eq "service selector stays on real primary during split-brain (#124)" "${POD_REPLICA}" "${selector_during_split}"

  # #125: the durable marker must record the real primary + its timeline (the
  # layer-3 write path and configmap RBAC working in-cluster).
  marker_primary=$(kubectl get configmap "${FULLNAME}-primary" -n "${NAMESPACE}" -o jsonpath='{.data.primary}' 2>/dev/null || echo "")
  assert_eq "durable primary marker records the real primary (#125)" "${POD_REPLICA}" "${marker_primary}"

  # --- #125: a full-cluster restart must not roll the DB back ---
  # Delete BOTH pods. Under OrderedReady pod-0 (stale, old timeline) recreates
  # first and ALONE; it must not be selected, and pod-1's newer-timeline data
  # must survive (pod-1's init must defer to the guard and never clone backward
  # onto the stale node).
  echo "  #125: full-cluster restart (deleting both pods)..."
  kubectl delete pod "${POD_PRIMARY}" "${POD_REPLICA}" -n "${NAMESPACE}" --grace-period=10 2>/dev/null || true
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

  echo "  #125: waiting for ${POD_REPLICA} to return as primary with its data (up to 240s)..."
  survived=false
  s_elapsed=0
  while [[ ${s_elapsed} -lt 240 ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM failover_test WHERE value='${AFTER_VALUE}'" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "f" && "${val}" == "${AFTER_VALUE}" ]]; then
      survived=true
      echo "  ${POD_REPLICA} is primary with post-failover data after ~${s_elapsed}s"
      break
    fi
    sleep 10
    s_elapsed=$((s_elapsed + 10))
  done
  assert_eq "post-failover data survives a full-cluster restart (#125)" "true" "${survived}"

  # The write selector must converge on the real primary, never the stale node.
  echo "  #125: letting service-updater tick post-restart (~40s)..."
  sleep 40
  selector_after=$(kubectl get service -n "${NAMESPACE}" "${FULLNAME}" \
    -o jsonpath='{.spec.selector.statefulset\.kubernetes\.io/pod-name}' 2>/dev/null || echo "")
  assert_eq "write selector on real primary after full restart (#125)" "${POD_REPLICA}" "${selector_after}"

  # --- #123 guard: the lingering stale primary rewind-rejoins on recreation ---
  # Under the default log action pod-0 lingers as an unselected stale primary;
  # recreating it must trigger the entrypoint guard to rewind it forward to
  # pod-1 (newer timeline) and bring it back as a streaming standby.
  if [[ "${survived}" == "true" ]]; then
    echo "  #123 guard: recreating stale ${POD_PRIMARY}; expect rewind-rejoin as standby (up to 300s)..."
    kubectl delete pod "${POD_PRIMARY}" -n "${NAMESPACE}" --grace-period=10 2>/dev/null || true
    rejoined=false
    r_elapsed=0
    while [[ ${r_elapsed} -lt 300 ]]; do
      rec=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
      if [[ "${rec}" == "t" ]]; then
        rejoined=true
        echo "  ${POD_PRIMARY} rewind-rejoined as standby after ~${r_elapsed}s"
        break
      fi
      sleep 10
      r_elapsed=$((r_elapsed + 10))
    done
    assert_eq "stale primary rewinds and rejoins as standby (#123 guard)" "true" "${rejoined}"

    # #178/#176/#123: pg_is_in_recovery=t alone cannot distinguish how the stale
    # node rejoined. Read the guard log to classify the path. The efficient
    # pg_rewind path MUST engage (#178): the guard supplies the repmgr password via
    # PGPASSWORD so repmgr can open the replication connection the rewind needs. The
    # re-clone fallback is still data-safe (reclone_preserving_old moves the stale
    # node's divergent data aside, #175), but taking it here means the rewind path
    # regressed -- so assert rewind, not merely "some data-safe path". Soft only on
    # log availability: an inconclusive log skips (never a false failure).
    if [[ "${rejoined}" == "true" ]]; then
      guard_log=$(kubectl logs "${POD_PRIMARY}" -c postgresql -n "${NAMESPACE}" 2>/dev/null || echo "")
      # The guard always emits exactly one of these two markers before postgres
      # takes over, so we can only fail to classify when the log itself is
      # unavailable (kubectl logs failed / rotated). Skip ONLY in that case; a
      # NON-empty log that matches neither marker is itself anomalous and must fail
      # -- otherwise the inconclusive path silently bypasses the #178 rewind assert
      # and reports green, the exact failure shape #178 is about.
      if [[ -z "${guard_log}" ]]; then
        skip "stale primary rewind-rejoins via pg_rewind, not the re-clone fallback (#178) (guard log unavailable)"
      else
        if echo "${guard_log}" | grep -q "rejoin complete; starting as standby"; then
          rejoin_method="rewind"
        elif echo "${guard_log}" | grep -q "falling back to full re-clone"; then
          rejoin_method="reclone-fallback"
          echo "  #178 regression: #123 guard fell back to a full re-clone instead of pg_rewind (data is still safe via reclone_preserving_old, but the efficient rewind path did not engage)"
        else
          rejoin_method="unclassified"
          echo "  #178: guard log present but matched neither the rewind nor the re-clone marker (the guard always emits one); treating as a failure"
        fi
        assert_eq "stale primary rewind-rejoins via pg_rewind, not the re-clone fallback (#178)" "rewind" "${rejoin_method}"
      fi
    fi
  else
    skip "stale primary rewinds and rejoins as standby (#123 guard) (data did not survive)"
  fi
else
  skip "service selector stays on real primary during split-brain (#124) (failover did not complete)"
  skip "durable primary marker records the real primary (#125) (failover did not complete)"
  skip "post-failover data survives a full-cluster restart (#125) (failover did not complete)"
  skip "write selector on real primary after full restart (#125) (failover did not complete)"
  skip "stale primary rewinds and rejoins as standby (#123 guard) (failover did not complete)"
fi

end_suite
print_summary
