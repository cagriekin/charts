#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-minimal}"
RELEASE="${RELEASE:-redis-minimal}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-minimal.yaml")

begin_suite "Minimal Install (standalone Redis, no exporter)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 1 300

POD="${FULLNAME}-0"

pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.phase}')
assert_eq "pod ${POD} is Running" "Running" "${pod_phase}"

ready=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="redis")].ready}')
assert_eq "redis container is ready" "true" "${ready}"

result=$(redis_exec "${NAMESPACE}" "${POD}" "PING")
assert_eq "PING returns PONG" "PONG" "${result}"

TEST_VALUE="hello-$(date +%s)"
redis_exec "${NAMESPACE}" "${POD}" "SET testkey \"${TEST_VALUE}\""
read_val=$(redis_exec "${NAMESPACE}" "${POD}" "GET testkey")
read_val="${read_val%\"}"
read_val="${read_val#\"}"
assert_eq "can SET and GET data" "${TEST_VALUE}" "${read_val}"

svc_endpoint=$(kubectl get endpoints -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.subsets[0].addresses[0].targetRef.name}')
assert_eq "service points to ${POD}" "${POD}" "${svc_endpoint}"

svc_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.spec.ports[0].port}')
assert_eq "service port is 6379" "6379" "${svc_port}"

container_names=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.spec.containers[*].name}')
assert_not_contains "no exporter container" "${container_names}" "redis-exporter"

end_suite
print_summary
