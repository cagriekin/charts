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

# redis-cli inside a container auto-authenticates via REDISCLI_AUTH (set on the pods).
sentinel_master() {
  # Returns the master pod name as reported by a Sentinel, or empty.
  local from_pod="$1"
  kubectl exec -n "${NAMESPACE}" "${from_pod}" -c sentinel -- \
    redis-cli -p 26379 sentinel get-master-addr-by-name "${MASTER_NAME}" 2>/dev/null \
    | head -1 | cut -d. -f1
}

redis_role() {
  kubectl exec -n "${NAMESPACE}" "$1" -c redis -- redis-cli INFO replication 2>/dev/null \
    | tr -d '\r' | awk -F: '/^role:/{print $2}'
}

begin_suite "Replication (Sentinel HA)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${VALUES}" \
  --wait --timeout 8m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 3 480

# Exactly one master and two replicas across the 3 pods (don't pin which pod, so this is
# idempotent if a prior failover moved the master).
masters=0; replicas=0; MASTER_POD=""; REPLICA_POD=""
for i in 0 1 2; do
  role=$(redis_role "${FULLNAME}-${i}")
  if [ "${role}" = "master" ]; then masters=$((masters + 1)); MASTER_POD="${FULLNAME}-${i}"; fi
  if [ "${role}" = "slave" ]; then replicas=$((replicas + 1)); REPLICA_POD="${FULLNAME}-${i}"; fi
done
assert_eq "exactly one master" "1" "${masters}"
assert_eq "two replicas" "2" "${replicas}"

# Master has its replicas connected.
slaves=$(kubectl exec -n "${NAMESPACE}" "${MASTER_POD}" -c redis -- redis-cli INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^connected_slaves:/{print $2}')
assert_eq "master reports 2 connected replicas" "2" "${slaves}"

# Sentinel discovery agrees with the actual master.
assert_eq "sentinel agrees on the master" "${MASTER_POD}" "$(sentinel_master "${REPLICA_POD}")"

# Write on the master replicates to a replica.
TEST_VALUE="repl-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${MASTER_POD}" -c redis -- redis-cli SET replkey "${TEST_VALUE}" >/dev/null
sleep 3
read_val=$(kubectl exec -n "${NAMESPACE}" "${REPLICA_POD}" -c redis -- redis-cli GET replkey 2>/dev/null | tr -d '\r')
assert_eq "write on master replicates to replica" "${TEST_VALUE}" "${read_val}"

# Replicas reject writes (replica-read-only).
if kubectl exec -n "${NAMESPACE}" "${REPLICA_POD}" -c redis -- redis-cli SET replkey nope 2>&1 | grep -q "READONLY"; then
  pass "replica rejects writes (READONLY)"
else
  fail "replica rejects writes (READONLY)" "expected a READONLY error"
fi

end_suite
print_summary
