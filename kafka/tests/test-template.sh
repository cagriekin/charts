#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

begin_suite "Helm Template Rendering"

# Lint tests
lint_output=$(helm lint "${CHART_DIR}" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with default values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with minimal values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with full values passes" "0" "${lint_rc}"

# Render minimal template
minimal=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)

assert_contains "minimal: controller statefulset present" "${minimal}" "kafka-controller"
assert_contains "minimal: broker statefulset present" "${minimal}" "kafka-broker"
assert_contains "minimal: controller service present" "${minimal}" "kind: Service"
assert_contains "minimal: secret created" "${minimal}" "kind: Secret"
assert_contains "minimal: configmap created" "${minimal}" "kind: ConfigMap"
assert_not_contains "minimal: no exporter deployment" "${minimal}" "kafka-exporter"
assert_not_contains "minimal: no topics in configmap" "${minimal}" "test-topic"

# Render full template
full=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)

assert_contains "full: controller statefulset present" "${full}" "kafka-controller"
assert_contains "full: broker statefulset present" "${full}" "kafka-broker"
assert_contains "full: exporter deployment present" "${full}" "kafka-exporter"
assert_contains "full: topic init job present" "${full}" "kafka-topic-init"
assert_contains "full: topics configmap present" "${full}" "kafka-topics"
assert_contains "full: exporter service present" "${full}" "port: 9308"
assert_contains "full: controller port 9093" "${full}" "port: 9093"
assert_contains "full: broker port 9092" "${full}" "port: 9092"
assert_contains "full: SASL configured" "${full}" "SASL_PLAINTEXT"
assert_contains "full: serviceaccount created" "${full}" "kind: ServiceAccount"
assert_contains "full: broker replicas 2" "${full}" "replicas: 2"

end_suite
print_summary
