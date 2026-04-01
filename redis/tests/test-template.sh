#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

begin_suite "Helm Template Rendering"

lint_output=$(helm lint "${CHART_DIR}" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with default values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with minimal values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with full values passes" "0" "${lint_rc}"

minimal=$(helm template test-redis "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)

assert_contains "minimal: statefulset has replicas: 1" "${minimal}" "replicas: 1"
assert_not_contains "minimal: no exporter deployment" "${minimal}" "redis-exporter"
assert_contains "minimal: configmap is created" "${minimal}" "kind: ConfigMap"
assert_contains "minimal: service exists" "${minimal}" "kind: Service"
assert_contains "minimal: redis.conf in configmap" "${minimal}" "redis.conf"

full=$(helm template test-redis "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)

assert_contains "full: statefulset has replicas: 1" "${full}" "replicas: 1"
assert_contains "full: exporter deployment present" "${full}" "redis-exporter"
assert_contains "full: exporter service present" "${full}" "-exporter"
exporter_port=$(echo "${full}" | grep -c "port: 9121" || echo "0")
if [[ "${exporter_port}" -gt 0 ]]; then
  pass "full: exporter service port 9121 present"
else
  fail "full: exporter service port 9121 present" "no match for port 9121"
fi

assert_contains "full: global annotations on statefulset" "${full}" "test-annotation: test-value"

labels_block=$(echo "${full}" | awk '/^  labels:/{capture=1; next} /^  [a-z]/{capture=0} capture' )
if echo "${labels_block}" | grep -q "test-annotation"; then
  fail "full: global annotations not in labels" "test-annotation found inside a labels block"
else
  pass "full: global annotations not in labels"
fi

no_annotations=$(helm template test-redis "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)
assert_not_contains "minimal: no annotations block without global annotations" "${no_annotations}" "test-annotation"

# --- SecurityContext Tests ---
assert_contains "minimal: pod has runAsNonRoot" "${minimal}" "runAsNonRoot: true"
assert_contains "minimal: pod has fsGroup 999" "${minimal}" "fsGroup: 999"
assert_contains "minimal: container has runAsUser 999" "${minimal}" "runAsUser: 999"
assert_contains "minimal: container has allowPrivilegeEscalation false" "${minimal}" "allowPrivilegeEscalation: false"
assert_contains "full: exporter has allowPrivilegeEscalation false" "${full}" "allowPrivilegeEscalation: false"

end_suite
print_summary
