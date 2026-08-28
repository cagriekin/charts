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
# Agent-created state is not chart-owned and survives helm uninstall: a stale primary
# marker makes the next holder refuse initdb over fresh PVCs (#170's guard -- correct in
# production, where empty data plus a marker means PVC loss, but a deadlock on a suite
# rerun into a dirty namespace), and a stale lease parks leadership on a not-yet-recreated
# identity for a lease term. Clear both alongside the PVCs (#298 review, observed live).
kubectl delete configmap "${FULLNAME}-primary" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
kubectl delete lease "${FULLNAME}-leader" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
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

# --- #293: shared_preload_libraries = 'repmgr' must leave PGDATA under native ---
#
# That line is appended to PGDATA/postgresql.conf by the image entrypoint at initdb time and
# cloned verbatim to every standby, so it outlives any chart change and any helm rollback. Once
# the repmgr-free image ships (#290) a data directory still requesting repmgr.so is a postmaster
# that refuses to start -- on every pod at once. The agent strips it on boot, before any start.
#
# Deliberately mechanism-split, which is the whole design (#293): only a native cluster can run
# the repmgr-free image (#294 deletes mechanism.Repmgr), so only native nodes are cleaned, and a
# node still on `mechanism: repmgr` must keep its preload -- removing it there would gamble with
# the repmgr extension's own functions for no gain.
PGDATA_CONF="/var/lib/postgresql/data/pgdata/postgresql.conf"
# The last ACTIVE assignment is what the postmaster uses. initdb also ships a commented-out
# `#shared_preload_libraries = ''`, which must never be counted (or stripped).
active_preload() { # pod
  kubectl exec -n "${NAMESPACE}" "$1" -c postgresql -- \
    sh -c "grep -E '^[[:space:]]*shared_preload_libraries[[:space:]]*=' '${PGDATA_CONF}' | tail -1" 2>/dev/null || echo ""
}
# The `|| echo ""` above means a failed exec, a wrong path or a moved PGDATA_CONF all look
# exactly like "the line is gone" -- so an assertion that the preload is ABSENT would pass
# while testing nothing. Prove the read actually worked by requiring a line we know is in
# that same file and that #293 must never touch (it is a non-goal in the issue).
assert_conf_readable() { # name, pod
  local sentinel
  sentinel=$(kubectl exec -n "${NAMESPACE}" "$2" -c postgresql -- \
    sh -c "grep -cE '^[[:space:]]*wal_log_hints' '${PGDATA_CONF}'" 2>/dev/null | tr -d '[:space:]' || echo "0")
  assert_eq "$1" "1" "${sentinel}"
}

if [[ "${post_ok}" != "true" ]]; then
  skip "#293: preload handling (did not settle post-restart)"
elif [[ "$(chart_mechanism)" == "native" ]]; then
  # A native install never wrote the line, so plant one to stand in for a data directory
  # inherited from a 1.x cluster. `repmgr` alone, matching what the entrypoint actually wrote:
  # the whole assignment must then be dropped, so the restarted postmaster is asking for
  # nothing and cannot fail for an unrelated missing library.
  kubectl exec -n "${NAMESPACE}" "${STANDBY}" -c postgresql -- \
    sh -c "printf \"shared_preload_libraries = 'repmgr'\\n\" >> '${PGDATA_CONF}'"
  planted=$(active_preload "${STANDBY}")
  assert_contains "#293 native: preload planted for the test" "${planted}" "repmgr"

  # shared_preload_libraries is a postmaster parameter, so only a restart applies the removal.
  kubectl delete pod -n "${NAMESPACE}" "${STANDBY}" --wait=true --timeout=180s >/dev/null 2>&1 || true
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

  # Prove the file is still readable at this path BEFORE asserting an absence in it.
  assert_conf_readable "#293 native: postgresql.conf is readable after the restart" "${STANDBY}"
  stripped=$(active_preload "${STANDBY}")
  assert_eq "#293 native: the repmgr assignment is gone from PGDATA" "" "$(echo "${stripped}" | tr -d '[:space:]')"
  # And the RUNNING server agrees -- the file is the mechanism, this is the effect.
  # Same trap on the SQL side: `|| echo ""` would make an unreachable server read as "does
  # not preload repmgr". Assert the query actually ran first.
  alive=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT 1" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "#293 native: the standby answers SQL after the strip" "1" "${alive}"
  shown=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SHOW shared_preload_libraries" "testuser" "testdb" 2>/dev/null || echo "")
  assert_not_contains "#293 native: the running standby does not preload repmgr" "${shown}" "repmgr"
  # The strip must not have cost the node its replication: this is a PGDATA edit on a live
  # standby, so a botched rewrite shows up here as a postmaster that will not start.
  restream=""; s=0
  while [[ ${s} -lt 120 ]]; do
    restream=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT state FROM pg_stat_replication WHERE application_name='${STANDBY}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${restream}" == "streaming" ]] && break
    sleep 5; s=$((s + 5))
  done
  assert_eq "#293 native: the standby still streams after the strip" "streaming" "${restream}"
else
  # The entrypoint wrote it at initdb, and the rolling restart above already ran every node
  # through boot(). It must still be there.
  assert_conf_readable "#293 repmgr mechanism: postgresql.conf is readable" "${STANDBY}"
  kept=$(active_preload "${STANDBY}")
  assert_contains "#293 repmgr mechanism: the preload is left untouched" "${kept}" "repmgr"
fi

# Cleanup.
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete namespace "${NAMESPACE}" --wait=false 2>/dev/null || true

end_suite
print_summary
