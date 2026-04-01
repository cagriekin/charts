#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-persistence}"
RELEASE="${RELEASE:-redis-persist}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-persistence-test.yaml")

begin_suite "AOF Persistence Restart"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-persistence-test.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 1 300

POD="${FULLNAME}-0"

result=$(redis_exec "${NAMESPACE}" "${POD}" "PING")
assert_eq "redis is responding" "PONG" "${result}"

echo "Writing test data..."
redis_exec "${NAMESPACE}" "${POD}" "SET persistence-test survived-restart"
redis_exec "${NAMESPACE}" "${POD}" "SET persistence-count 42"

read_val=$(redis_exec "${NAMESPACE}" "${POD}" "GET persistence-test")
read_val="${read_val%\"}"
read_val="${read_val#\"}"
assert_eq "data written successfully" "survived-restart" "${read_val}"

echo "Deleting pod to trigger restart..."
kubectl delete pod "${POD}" -n "${NAMESPACE}" --grace-period=300

echo "Waiting for pod to come back..."
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 1 300

result=$(redis_exec "${NAMESPACE}" "${POD}" "PING")
assert_eq "redis responding after restart" "PONG" "${result}"

echo "Verifying data survived restart..."
read_val=$(redis_exec "${NAMESPACE}" "${POD}" "GET persistence-test")
read_val="${read_val%\"}"
read_val="${read_val#\"}"
assert_eq "data survives pod restart" "survived-restart" "${read_val}"

read_count=$(redis_exec "${NAMESPACE}" "${POD}" "GET persistence-count")
read_count="${read_count%\"}"
read_count="${read_count#\"}"
assert_eq "numeric data survives pod restart" "42" "${read_count}"

end_suite
print_summary
