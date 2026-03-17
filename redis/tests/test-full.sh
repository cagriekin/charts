#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-full}"
RELEASE="${RELEASE:-redis-full}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-full-test.yaml")

begin_suite "Full Install (Redis + Prometheus exporter)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 1 300

POD="${FULLNAME}-0"

pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.phase}')
assert_eq "pod ${POD} is Running" "Running" "${pod_phase}"

result=$(redis_exec "${NAMESPACE}" "${POD}" "PING")
assert_eq "PING returns PONG" "PONG" "${result}"

wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-exporter" 300

exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=redis-exporter" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "exporter pod is Running" "Running" "${exporter_phase}"

exporter_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-exporter" -o jsonpath='{.spec.ports[0].port}')
assert_eq "exporter service port is 9121" "9121" "${exporter_port}"

metrics_output=$(kubectl run curl-metrics --rm -i --restart=Never --image=curlimages/curl -n "${NAMESPACE}" -- \
  curl -sf "http://${FULLNAME}-exporter.${NAMESPACE}.svc.cluster.local:9121/metrics" 2>/dev/null | grep -m1 "redis_" || echo "")
assert_contains "exporter returns redis metrics" "${metrics_output}" "redis_"

end_suite
print_summary
