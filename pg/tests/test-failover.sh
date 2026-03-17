#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-repmgr}"
RELEASE="${RELEASE:-pg-repmgr}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")

begin_suite "Failover (kill primary, verify promotion)"

POD_PRIMARY="${FULLNAME}-0"
POD_REPLICA="${FULLNAME}-1"

# Verify starting state
is_primary=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "pod-0 starts as primary" "t" "${is_primary}"

# Write data before failover
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "CREATE TABLE IF NOT EXISTS failover_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "INSERT INTO failover_test (value) VALUES ('before-failover')" "testuser" "testdb"
sleep 2

# Kill the primary postgres process to trigger failover
echo "  Killing primary postgresql process on ${POD_PRIMARY}..."
kubectl exec -n "${NAMESPACE}" "${POD_PRIMARY}" -c postgresql -- \
  bash -c "kill -9 \$(head -1 /var/lib/postgresql/data/pgdata/postmaster.pid)" 2>/dev/null || true

# Wait for repmgr to detect failure and promote replica
echo "  Waiting for failover (up to 120s)..."
failover_timeout=120
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

# Test: data survives failover
if [[ "${failover_done}" == "true" ]]; then
  survived_val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM failover_test WHERE value='before-failover'" "testuser" "testdb")
  assert_eq "data survives failover" "before-failover" "${survived_val}"

  # Test: can write to new primary
  pg_exec "${NAMESPACE}" "${POD_REPLICA}" "INSERT INTO failover_test (value) VALUES ('after-failover')" "testuser" "testdb"
  after_val=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT value FROM failover_test WHERE value='after-failover'" "testuser" "testdb")
  assert_eq "can write to new primary" "after-failover" "${after_val}"

  # Test: service-updater patches service to point to new primary
  echo "  Waiting for service-updater to patch service (up to 60s)..."
  svc_timeout=60
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

end_suite
print_summary
