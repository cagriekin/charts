#!/bin/bash
set -euo pipefail

# #186: a rolling restart of a 2-node AGENT-mode cluster must always converge to a
# single writable primary with the standby re-streaming -- no manual intervention.
# Before P1/P2 this could deadlock: the StatefulSet RollingUpdate rolled the primary
# (clone-source) while a standby was mid-clone (standby reported Ready on bare
# pg_isready), interrupting the clone and leaving an empty node holding the lease.
# P1 (replication-aware standby readiness) serializes the roll safely; P2 (release the
# lease when empty + marker names a different primary) self-heals any residual stall.
# This asserts the end-state invariant rather than reproducing the timing-dependent
# deadlock.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-rolling}"
RELEASE="${RELEASE:-pg-rolling}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"

begin_suite "Agent Rolling Restart (no-deadlock invariant, #186)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
# podManagementPolicy is immutable; clear a leftover STS from a prior repmgrd run.
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# Settle a single primary that holds the lease (agent mode is lease-decided, not
# ordinal-pinned, so discover it).
settle_primary() {
  local budget="${1:-240}" elapsed=0 r0 r1 holder
  PRIMARY=""; STANDBY=""; HOLDER=""
  while [[ ${elapsed} -lt ${budget} ]]; do
    r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
    if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; fi
    if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; fi
    HOLDER="${holder}"
    if [[ -n "${PRIMARY}" && "${HOLDER}" == "${PRIMARY}" ]]; then return 0; fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

settle_primary 240 && pre_ok=true || pre_ok=false
assert_eq "single primary == lease holder before the rolling restart" "true" "${pre_ok}"

if [[ "${pre_ok}" != "true" ]]; then
  skip "rolling restart preserves a single writable primary (#186) (did not settle pre-restart)"
  skip "data survives the rolling restart (#186) (did not settle pre-restart)"
  skip "new primary is writable after the rolling restart (#186) (did not settle pre-restart)"
  skip "standby re-streams after the rolling restart (#186) (did not settle pre-restart)"
  end_suite
  print_summary
  # The pre-restart settle assert above already recorded the failure; honor it rather
  # than forcing a green exit (a non-settling install must fail the suite).
  [ "${FAIL_COUNT:-0}" -eq 0 ] && exit 0 || exit 1
fi

# Write a row to verify survival across the restart.
RV="before-roll-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "DROP TABLE IF EXISTS roll_test" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE roll_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO roll_test (value) VALUES ('${RV}')" "testuser" "testdb"
sleep 3
repl=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM roll_test WHERE value='${RV}'" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "data replicated to standby before the rolling restart" "${RV}" "${repl}"

# Trigger a RollingUpdate of all pods (same path a real image bump takes), then wait
# for the controller to finish rolling. With P1 the controller will not roll the next
# pod until the current one is streaming-Ready, so this must not deadlock.
echo "  Rolling-restarting the StatefulSet (${FULLNAME})..."
kubectl rollout restart statefulset "${FULLNAME}" -n "${NAMESPACE}"
kubectl rollout status statefulset "${FULLNAME}" -n "${NAMESPACE}" --timeout=10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# Invariant: a single primary == lease holder converges again, no manual intervention.
settle_primary 240 && post_ok=true || post_ok=false
assert_eq "rolling restart preserves a single writable primary (#186)" "true" "${post_ok}"

if [[ "${post_ok}" == "true" ]]; then
  survived=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT value FROM roll_test WHERE value='${RV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "data survives the rolling restart (#186)" "${RV}" "${survived}"

  NV="after-roll-$(date +%s)"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO roll_test (value) VALUES ('${NV}')" "testuser" "testdb" 2>/dev/null && wrote=true || wrote=false
  assert_eq "new primary is writable after the rolling restart (#186)" "true" "${wrote}"

  # The standby must re-establish streaming (the P1 readiness gate also asserts this
  # via the rolling update, but verify directly on the primary).
  stream=""; s=0
  while [[ ${s} -lt 120 ]]; do
    stream=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT state FROM pg_stat_replication WHERE application_name='${STANDBY}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${stream}" == "streaming" ]] && break
    sleep 5; s=$((s + 5))
  done
  assert_eq "standby re-streams after the rolling restart (#186)" "streaming" "${stream}"
else
  skip "data survives the rolling restart (#186) (did not settle post-restart)"
  skip "new primary is writable after the rolling restart (#186) (did not settle post-restart)"
  skip "standby re-streams after the rolling restart (#186) (did not settle post-restart)"
fi

# Cleanup.
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete namespace "${NAMESPACE}" --wait=false 2>/dev/null || true

end_suite
print_summary
