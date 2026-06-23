#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-replication}"
RELEASE="${RELEASE:-redis-repl}"
VALUES="${SCRIPT_DIR}/values-replication.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
MASTER_NAME="mymaster"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-120}"

# A Sentinel in a known-live pod (anything but the excluded one).
live_sentinel_pod() {
  local exclude="$1" p
  for p in $(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=redis" \
              --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'); do
    if [[ "$p" != "$exclude" ]]; then echo "$p"; return 0; fi
  done
}

sentinel_master() {
  kubectl exec -n "${NAMESPACE}" "$1" -c sentinel -- \
    redis-cli -p 26379 sentinel get-master-addr-by-name "${MASTER_NAME}" 2>/dev/null \
    | head -1 | cut -d. -f1
}

redis_role() {
  kubectl exec -n "${NAMESPACE}" "$1" -c redis -- redis-cli INFO replication 2>/dev/null \
    | tr -d '\r' | awk -F: '/^role:/{print $2}'
}

begin_suite "Sentinel Failover"

# Assumes test-replication.sh already installed the release; install if run standalone.
if ! kubectl get statefulset "${FULLNAME}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
  helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 8m
fi
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 3 480

# Write a durable value before failover.
SURVIVE="survive-$(date +%s)"
OLD_MASTER=$(sentinel_master "${FULLNAME}-1")
[[ -z "${OLD_MASTER}" ]] && OLD_MASTER="${FULLNAME}-0"
echo "  Current master: ${OLD_MASTER}"
kubectl exec -n "${NAMESPACE}" "${OLD_MASTER}" -c redis -- redis-cli SET failover-key "${SURVIVE}" >/dev/null
sleep 3

# Kill the master pod. The StatefulSet recreates it; Sentinel promotes a replica
# once down-after-milliseconds (5s) elapses.
echo "  Deleting master ${OLD_MASTER} to force failover..."
kubectl delete pod "${OLD_MASTER}" -n "${NAMESPACE}" --grace-period=0 --force >/dev/null 2>&1 || true

# Poll a surviving Sentinel until it reports a new master.
WATCH=$(live_sentinel_pod "${OLD_MASTER}")
NEW_MASTER=""
elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  cur=$(sentinel_master "${WATCH}" || true)
  if [[ -n "${cur}" && "${cur}" != "${OLD_MASTER}" ]]; then NEW_MASTER="${cur}"; break; fi
  sleep 5; elapsed=$((elapsed + 5))
done

if [[ -n "${NEW_MASTER}" ]]; then
  pass "sentinel promoted a new master (${NEW_MASTER}) in ~${elapsed}s"
else
  fail "sentinel promoted a new master" "no new master within ${FAILOVER_BUDGET}s"
  print_summary; exit 1
fi

# New master is writable and has the pre-failover data.
assert_eq "new master role is master" "master" "$(redis_role "${NEW_MASTER}")"
read_val=$(kubectl exec -n "${NAMESPACE}" "${NEW_MASTER}" -c redis -- redis-cli GET failover-key 2>/dev/null | tr -d '\r')
assert_eq "data survived failover" "${SURVIVE}" "${read_val}"

# min-replicas-to-write blocks writes until Sentinel re-points a replica at the new
# master, so wait for at least one connected replica before asserting writability.
elapsed=0
while [[ ${elapsed} -lt 60 ]]; do
  cs=$(kubectl exec -n "${NAMESPACE}" "${NEW_MASTER}" -c redis -- redis-cli INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^connected_slaves:/{print $2}')
  [[ "${cs:-0}" -ge 1 ]] && break
  sleep 5; elapsed=$((elapsed + 5))
done
kubectl exec -n "${NAMESPACE}" "${NEW_MASTER}" -c redis -- redis-cli SET post-failover ok >/dev/null
post=$(kubectl exec -n "${NAMESPACE}" "${NEW_MASTER}" -c redis -- redis-cli GET post-failover 2>/dev/null | tr -d '\r')
assert_eq "new master accepts writes" "ok" "${post}"

# The ex-master returns and rejoins as a replica (no split-brain).
echo "  Waiting for ${OLD_MASTER} to rejoin as a replica..."
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 3 300
rejoined=""
elapsed=0
while [[ ${elapsed} -lt 90 ]]; do
  role=$(redis_role "${OLD_MASTER}" || true)
  if [[ "${role}" == "slave" ]]; then rejoined="yes"; break; fi
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "ex-master rejoined as replica (no split-brain)" "yes" "${rejoined}"

# Exactly one master across the cluster.
masters=0
for i in 0 1 2; do
  [[ "$(redis_role "${FULLNAME}-${i}")" == "master" ]] && masters=$((masters + 1))
done
assert_eq "exactly one master after failover" "1" "${masters}"

end_suite
print_summary
