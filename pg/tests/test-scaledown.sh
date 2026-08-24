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

# #288: the native equivalent of "is node N in the topology". There is no repmgr.nodes to
# consult -- a native cluster has neither the extension nor the table -- so the question becomes
# "is that pod streaming from the primary", which is the primary's own live connection list.
# Ordinal 0 is the primary itself: it never appears in its own pg_stat_replication, so it counts
# as present whenever it is the node answering.
node_streaming() { # node_streaming <primary-pod> <ordinal>
  local primary="$1" ord="$2"
  if [[ "${primary}" == "${FULLNAME}-${ord}" ]]; then echo "1"; return 0; fi
  pg_exec "${NAMESPACE}" "${primary}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming' AND (application_name='${FULLNAME}-${ord}' OR pid IN (SELECT active_pid FROM pg_replication_slots WHERE slot_name='pg_ha_slot_${ord}'))" \
    repmgr repmgr 2>/dev/null | xargs || echo ""
}

# in_topology dispatches on the mechanism so the three checks below read the same either way.
in_topology() { # in_topology <primary-pod> <ordinal>
  if [ "$(chart_mechanism)" = "native" ]; then node_streaming "$1" "$2"; else node_count "$1" "$((1000 + $2))"; fi
}

# Precondition: all three nodes registered (so the scaled node 1002 exists first --
# otherwise the post-scale assertion would pass vacuously).
echo "  Waiting for all 3 nodes (1000,1001,1002) to register (up to 180s)..."
pre=0; elapsed=0
while [[ ${elapsed} -lt 180 ]]; do
  P=$(find_primary 2)
  if [[ -n "${P}" && "$(in_topology "${P}" 0)" == "1" && "$(in_topology "${P}" 1)" == "1" && "$(in_topology "${P}" 2)" == "1" ]]; then
    pre=1; break
  fi
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "all 3 nodes in the topology before scale-down (incl. ordinal 2)" "1" "${pre}"

# Capture the pre-scale primary and whether pg_ha_slot_2 actually exists on it (#288 review).
# Under native the initial primary is decided by the LEASE RACE, not by ordinal, so pod-2 can be
# the primary -- and the StatefulSet trims the top ordinal, i.e. the primary itself. A survivor
# then promotes, and a newly promoted primary carries no physical slots from its standby life
# (physical slots are not replicated), so pg_ha_slot_2 never exists on the node being polled and
# every "the slot was reclaimed" assertion below passes VACUOUSLY. Record the evidence now so the
# post-scale assertions can tell "reclaimed" apart from "never there".
PRE_PRIMARY="${P:-$(find_primary 2)}"
PRE_SLOT2=""
if [ "$(chart_mechanism)" = "native" ]; then
  PRE_SLOT2=$(pg_exec "${NAMESPACE}" "${PRE_PRIMARY}" \
    "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'pg_ha_slot_2'" repmgr repmgr 2>/dev/null | xargs || echo "")
  echo "  pre-scale: primary=${PRE_PRIMARY} pg_ha_slot_2_present=${PRE_SLOT2:-?}"
fi

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
  if [[ -n "${P}" && "$(in_topology "${P}" 2)" == "0" ]]; then gone=1; break; fi
  sleep 10; elapsed=$((elapsed + 10))
done
assert_eq "the departed ordinal 2 left the topology after scale-down (#139)" "1" "${gone}"

# The live nodes must NOT be unregistered (the discriminator is the ordinal, not
# reachability -- a momentarily-down live node must never be treated as a ghost).
P=$(find_primary 1)
assert_eq "live ordinal 0 still in the topology" "1" "$(in_topology "${P}" 0)"
assert_eq "live ordinal 1 still in the topology" "1" "$(in_topology "${P}" 1)"

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
if [ "$(chart_mechanism)" = "native" ]; then
  # Wait on the RECLAIM, not on the stream stopping (#288 review). in_topology going to 0 under
  # native only means pod-2's row left pg_stat_replication, which happens the instant its stream
  # drops -- seconds before the primary's next slotsTick observes the shrunken live pod set and
  # drops pg_ha_slot_2. Reading the slot counts straight after that loop raced the tick. Under
  # repmgr the equivalent wait was on repmgr.nodes, i.e. on the primary tick itself having run.
  # Only assert the reclaim when there was something to reclaim. An honest SKIP beats a green
  # assertion that never exercised the code path (#288 review).
  if [ "${PRE_SLOT2}" != "1" ]; then
    skip "#289/#288: slot reclaim not exercised (pg_ha_slot_2 was absent pre-scale; primary was ${PRE_PRIMARY}, so the trimmed ordinal owned no slot on it)"
  fi
  echo "  Waiting for the primary to reclaim pg_ha_slot_2 (up to 120s)..."
  reclaimed=0; elapsed=0
  while [[ ${elapsed} -lt 120 ]]; do
    left=$(pg_exec "${NAMESPACE}" "${P}" \
      "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'pg_ha_slot_2'" repmgr repmgr 2>/dev/null | xargs || echo "")
    if [[ "${left}" == "0" ]]; then reclaimed=1; break; fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  if [ "${PRE_SLOT2}" = "1" ]; then
    assert_eq "#289/#288: the primary reclaimed the departed ordinal's slot" "1" "${reclaimed}"
  fi
  agent_slots=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'pg_ha_slot_%'" repmgr repmgr 2>/dev/null | xargs || echo "")
  # #288: the assertion inverts. Native owns its slots, so after scaling 3 -> 2 there must be
  # exactly one agent-minted slot -- ordinal 1, the surviving peer. The primary does not stream
  # from itself, and ordinal 2 is gone.
  assert_eq "#289/#288: native leaves exactly one agent slot for the surviving standby" "1" "${agent_slots}"
  ghost_agent=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'pg_ha_slot_2'" repmgr repmgr 2>/dev/null | xargs || echo "")
  assert_eq "#289/#288: no agent slot left pinning WAL for the scaled-away ordinal 2" "0" "${ghost_agent}"
  # #288's own acceptance line: a native cluster carries no repmgr metadata at all, so there is
  # no stale cache for anything to fall back to by accident.
  ext=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_extension WHERE extname='repmgr'" repmgr repmgr 2>/dev/null | xargs || echo "")
  assert_eq "#288: a native cluster has no repmgr extension" "0" "${ext}"
  nodes_rel=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='repmgr' AND c.relname='nodes'" repmgr repmgr 2>/dev/null | xargs || echo "")
  assert_eq "#288: a native cluster has no repmgr.nodes relation" "0" "${nodes_rel}"
  # And the topology the agent publishes must agree with the surviving pod set.
  streaming=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" repmgr repmgr 2>/dev/null | xargs || echo "")
  assert_eq "#288: the primary sees exactly one streaming standby after the scale-down" "1" "${streaming}"
else
  assert_eq "#289: the agent creates no slots under the repmgr mechanism (repmgr owns them)" "0" "${agent_slots}"
fi

# No physical slot may be left pinning WAL for a pod that no longer exists. Under repmgr mode
# repmgr names them repmgr_slot_<node_id>, so the scaled-away node 1002's slot is the one to
# look for; the native equivalent is asserted above (#288 made it possible to run at all).
if [ "$(chart_mechanism)" != "native" ]; then
  ghost_slot=$(pg_exec "${NAMESPACE}" "${P}" \
    "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'repmgr_slot_1002'" repmgr repmgr 2>/dev/null | xargs || echo "")
  assert_eq "#289: no replication slot left pinning WAL for the scaled-away node 1002" "0" "${ghost_slot}"
fi

# Cleanup.
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete namespace "${NAMESPACE}" --wait=false 2>/dev/null || true

end_suite
print_summary
