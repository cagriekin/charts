#!/bin/bash
set -euo pipefail

# #139: scaling postgresql.replicaCount down must not leave permanent ghost rows in
# repmgr.nodes. The primary reconciles repmgr.nodes against the live ordinal range and
# unregisters records for pods the StatefulSet no longer runs.
#
# Ported to agent mode in #286: this used to pin failoverMode: repmgrd for a deterministic
# standby ghost, and the service-updater on the master did the unregistering. repmgrd is
# gone, so the agent's own cleanupGhostNodes now runs it on the lease holder each tick,
# bounded by REPMGR_NODE_COUNT (which the scale-down `helm upgrade` re-renders). Keeping
# this suite is the point of the port -- it is the only end-to-end #139 regression, and
# the Go unit tests cover ghostNodeIDs' arithmetic but not the live unregister.
#
# Determinism comes from WHICH node becomes the ghost, not from a fixed primary: the
# StatefulSet trims the highest ordinal (pod-2) first, while it is still a standby, so
# node 1002's row is type='standby' -- the case `repmgr standby unregister` handles. The
# post-scale primary is whichever pod holds the lease, so the assertions locate it with
# find_primary rather than assuming pod-0.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-scaledown}"
RELEASE="${RELEASE:-pg-scaledown}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")

begin_suite "Scale-down ghost-node cleanup (#139)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
sleep 3

# Install 3 instances: ordinals 0,1,2 -> repmgr node_ids 1000,1001,1002.
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set postgresql.replicaCount=2 \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600

# Find the primary (not in recovery) among the live ordinals and count a node_id in
# repmgr.nodes there (the primary owns the table; the count replicates to standbys).
find_primary() {
  local max="$1" i rec
  for i in $(seq 0 "${max}"); do
    rec=$(pg_exec "${NAMESPACE}" "${FULLNAME}-${i}" "SELECT pg_is_in_recovery()" repmgr repmgr 2>/dev/null || echo "")
    if [[ "${rec}" == "f" ]]; then echo "${FULLNAME}-${i}"; return 0; fi
  done
  echo ""
}
node_count() { # node_count <primary-pod> <node_id>
  pg_exec "${NAMESPACE}" "$1" "SELECT count(*) FROM repmgr.nodes WHERE node_id=$2" repmgr repmgr 2>/dev/null | xargs || echo ""
}

# Precondition: all three nodes registered (so the scaled node 1002 exists first --
# otherwise the post-scale assertion would pass vacuously).
echo "  Waiting for all 3 nodes (1000,1001,1002) to register (up to 180s)..."
pre=0; elapsed=0
while [[ ${elapsed} -lt 180 ]]; do
  P=$(find_primary 2)
  if [[ -n "${P}" && "$(node_count "${P}" 1000)" == "1" && "$(node_count "${P}" 1001)" == "1" && "$(node_count "${P}" 1002)" == "1" ]]; then
    pre=1; break
  fi
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "all 3 nodes registered before scale-down (incl. node 1002)" "1" "${pre}"

# Scale down to 2 instances (replicaCount=1 -> ordinals 0,1). The StatefulSet trims the
# top ordinal, so pod-2 (a standby) is removed; its repmgr.nodes row (node_id 1002) is
# now a ghost the primary must unregister (#139).
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set postgresql.replicaCount=1 \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 300

# The lease holder's agent unregisters the ghost on its next reconcile tick; poll until
# gone. Agent mode is Parallel, so the surviving pods roll concurrently -- give the lease
# time to settle before concluding the row survived.
echo "  Waiting for the ghost node 1002 to be unregistered (up to 180s)..."
gone=0; elapsed=0
while [[ ${elapsed} -lt 180 ]]; do
  P=$(find_primary 1)
  if [[ -n "${P}" && "$(node_count "${P}" 1002)" == "0" ]]; then gone=1; break; fi
  sleep 10; elapsed=$((elapsed + 10))
done
assert_eq "ghost node 1002 unregistered after scale-down (#139)" "1" "${gone}"

# The live nodes must NOT be unregistered (the discriminator is the ordinal, not
# reachability -- a momentarily-down live node must never be treated as a ghost).
P=$(find_primary 1)
assert_eq "live node 1000 still registered" "1" "$(node_count "${P}" 1000)"
assert_eq "live node 1001 still registered" "1" "$(node_count "${P}" 1001)"

# The surviving cluster is still healthy: a single primary serves.
serves=$(pg_exec "${NAMESPACE}" "${P}" "SELECT NOT pg_is_in_recovery()" repmgr repmgr 2>/dev/null || echo "")
assert_eq "a primary still serves after scale-down + cleanup" "t" "${serves}"

# #289: this suite runs the repmgr mechanism, where repmgr -- not the agent -- owns
# replication slots. The agent's slot reconcile is gated on MECHANISM=native and must be
# completely inert here, so no agent-minted slot may exist. This is the regression guard
# for that gate: if reconcileSlots ever ran under repmgr mode it would create
# pg_ha_slot_<ordinal> alongside repmgr's own repmgr_slot_<node_id>, giving two owners for
# the same resource -- and, worse, its orphan rule would start dropping repmgr's slots.
agent_slots=$(pg_exec "${NAMESPACE}" "${P}" \
  "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'pg_ha_slot_%'" repmgr repmgr 2>/dev/null | xargs || echo "")
assert_eq "#289: the agent creates no slots under the repmgr mechanism (repmgr owns them)" "0" "${agent_slots}"

# No physical slot may be left pinning WAL for a pod that no longer exists. Under repmgr
# mode repmgr names them repmgr_slot_<node_id>, so the scaled-away node 1002's slot is the
# one to look for. NOTE: the equivalent native-mode assertion (zero pg_ha_slot_* above the
# live ordinal range) cannot be written until #288 lands -- native mode cannot run with any
# replicas at all today, since the shared repmgr-init container leaves every standby in
# Init:CrashLoopBackOff. Tracked there; the native path is covered at the unit level and was
# verified by hand against a real two-node PostgreSQL 18 pair.
ghost_slot=$(pg_exec "${NAMESPACE}" "${P}" \
  "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'repmgr_slot_1002'" repmgr repmgr 2>/dev/null | xargs || echo "")
assert_eq "#289: no replication slot left pinning WAL for the scaled-away node 1002" "0" "${ghost_slot}"

# Cleanup.
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete namespace "${NAMESPACE}" --wait=false 2>/dev/null || true

end_suite
print_summary
