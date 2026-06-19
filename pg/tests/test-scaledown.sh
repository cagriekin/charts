#!/bin/bash
set -euo pipefail

# #139: scaling postgresql.replicaCount down must not leave permanent ghost rows in
# repmgr.nodes. The primary now reconciles repmgr.nodes against the live ordinal range
# and unregisters records for pods the StatefulSet no longer runs. This exercises the
# repmgrd-mode path (service-updater on the master).
#
# Determinism comes from WHICH node becomes the ghost, not from a fixed primary: the
# StatefulSet trims the highest ordinal (pod-2) first, while it is still a standby, so
# node 1002's row is type='standby' -- the case `repmgr standby unregister` handles.
# The scale-down `helm upgrade` itself changes REPMGR_NODE_COUNT, which rolls the
# surviving pods (this is also what makes the re-rendered ConfigMap's lower bound take
# effect); under OrderedReady pod-0 rolls last and its clean preStop fails over to
# pod-1, so the post-scale primary may be pod-1. The assertions therefore locate the
# live primary with find_primary rather than assuming pod-0. The agent-mode path shares
# the same repmgr primitive and is covered by the Go unit tests.

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

# The master's service-updater unregisters the ghost on its next tick; poll until gone.
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

# Cleanup.
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete namespace "${NAMESPACE}" --wait=false 2>/dev/null || true

end_suite
print_summary
