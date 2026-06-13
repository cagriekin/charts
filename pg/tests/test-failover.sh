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

# --- Stale-primary guard (#123) ---
# Force a CONTAINER-only restart of the resurrected stale primary (kill the
# postmaster; the pod object and its PVC stay). The image entrypoint guard
# must see pod-1's newer timeline and rejoin this node as a standby via
# pg_rewind IN PLACE -- no pod recreation -- instead of resuming as a second
# read-write primary.
if [[ "${failover_done}" == "true" ]]; then
  OLD_UID=$(kubectl get pod -n "${NAMESPACE}" "${POD_PRIMARY}" -o jsonpath='{.metadata.uid}')
  sleep 5  # let it actually accept a read-write connection as the stale primary
  echo "  Killing postmaster on ${POD_PRIMARY} to force a container-only restart..."
  kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c repmgrd -- kill -9 "${PG_PID}" 2>/dev/null || true

  echo "  Waiting for ${POD_PRIMARY} to rejoin as a standby of the new primary (up to 300s)..."
  rejoined=false
  rejoin_elapsed=0
  while [[ ${rejoin_elapsed} -lt 300 ]]; do
    in_recovery=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${in_recovery}" == "t" ]]; then
      rejoined=true
      echo "  Rejoined as standby after ~${rejoin_elapsed}s"
      break
    fi
    sleep 10
    rejoin_elapsed=$((rejoin_elapsed + 10))
  done
  assert_eq "stale primary rejoins as standby instead of serving read-write" "true" "${rejoined}"

  # The rejoin must happen in place: same pod object (UID), repaired via
  # pg_rewind, not a pod deletion/recreation.
  NEW_UID=$(kubectl get pod -n "${NAMESPACE}" "${POD_PRIMARY}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  assert_eq "rejoin happens in place (pod not recreated)" "${OLD_UID}" "${NEW_UID}"

  if [[ "${rejoined}" == "true" ]]; then
    # The rejoined node must carry data written on the new primary AFTER the
    # failover, proving its stale timeline-1 tail was rewound and replaced.
    has_data=false
    data_elapsed=0
    while [[ ${data_elapsed} -lt 60 ]]; do
      recloned_val=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT value FROM failover_test WHERE value='${AFTER_VALUE}'" "testuser" "testdb" 2>/dev/null || echo "")
      if [[ "${recloned_val}" == "${AFTER_VALUE}" ]]; then
        has_data=true
        break
      fi
      sleep 5
      data_elapsed=$((data_elapsed + 5))
    done
    assert_eq "rejoined standby has post-failover data" "true" "${has_data}"

    streaming=false
    stream_elapsed=0
    while [[ ${stream_elapsed} -lt 60 ]]; do
      new_primary_count=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT count(*) FROM pg_stat_replication" "testuser" "testdb" 2>/dev/null || echo "")
      if [[ "${new_primary_count}" == "1" ]]; then
        streaming=true
        break
      fi
      sleep 5
      stream_elapsed=$((stream_elapsed + 5))
    done
    assert_eq "new primary streams to the rejoined standby" "true" "${streaming}"
  else
    skip "rejoined standby has post-failover data (node never rejoined)"
    skip "new primary streams to the rejoined standby (node never rejoined)"
  fi
else
  skip "stale primary rejoins as standby instead of serving read-write (failover did not complete)"
  skip "rejoin happens in place (pod not recreated) (failover did not complete)"
  skip "rejoined standby has post-failover data (failover did not complete)"
  skip "new primary streams to the rejoined standby (failover did not complete)"
fi

end_suite
print_summary
