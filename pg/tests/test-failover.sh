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
else
  skip "data survives failover (failover did not complete)"
  skip "can write to new primary (failover did not complete)"
  skip "service updated to point to new primary (failover did not complete)"
fi

# Unfreeze the old primary after all assertions
kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c repmgrd -- kill -CONT "${PG_PID}" 2>/dev/null || true

end_suite
print_summary
