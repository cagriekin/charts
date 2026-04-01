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

# --- preStop and terminationGracePeriodSeconds Tests ---
assert_contains "minimal: preStop SHUTDOWN SAVE" "${minimal}" "SHUTDOWN"
assert_contains "minimal: terminationGracePeriodSeconds" "${minimal}" "terminationGracePeriodSeconds: 300"

# --- Persistence Tuning Tests ---
assert_contains "minimal: appendfsync everysec" "${minimal}" "appendfsync everysec"
assert_contains "minimal: no-appendfsync-on-rewrite yes" "${minimal}" "no-appendfsync-on-rewrite yes"
assert_contains "minimal: save disabled by default" "${minimal}" 'save ""'

# Test: RDB snapshots render when configured
rdb_output=$(helm template test-redis "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --set 'redis.config.rdbSnapshots[0].seconds=3600' \
  --set 'redis.config.rdbSnapshots[0].changes=1' \
  --show-only templates/configmap.yaml 2>&1)
assert_contains "rdb: save renders with snapshots" "${rdb_output}" "save 3600 1"
assert_not_contains "rdb: save empty not present with snapshots" "${rdb_output}" 'save ""'

# --- AUTH Tests ---

# Test: auth disabled by default
assert_not_contains "default: no REDIS_PASSWORD env" "${minimal}" "REDIS_PASSWORD"
assert_not_contains "default: no requirepass" "${minimal}" "requirepass"

# Test: auth enabled renders correctly
auth_output=$(helm template test-redis "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --set redis.auth.enabled=true \
  --set redis.auth.existingSecret.name=my-secret \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "auth: REDIS_PASSWORD env present" "${auth_output}" "REDIS_PASSWORD"
assert_contains "auth: secretKeyRef present" "${auth_output}" "secretKeyRef"
assert_contains "auth: secret name present" "${auth_output}" "my-secret"
assert_contains "auth: requirepass in command" "${auth_output}" "requirepass"
assert_contains "auth: probe uses password" "${auth_output}" 'REDIS_PASSWORD.*ping'

# Test: exporter with auth has REDIS_PASSWORD
auth_exporter=$(helm template test-redis "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set redis.auth.enabled=true \
  --set redis.auth.existingSecret.name=my-secret \
  2>&1)
assert_contains "auth exporter: REDIS_PASSWORD in exporter" "${auth_exporter}" "REDIS_PASSWORD"

# --- PDB Tests ---
assert_not_contains "default: no PDB" "${minimal}" "PodDisruptionBudget"

pdb_output=$(helm template test-redis "${CHART_DIR}" \
  --set redis.podDisruptionBudget.enabled=true \
  --show-only templates/pdb.yaml 2>&1)
assert_contains "pdb: renders when enabled" "${pdb_output}" "kind: PodDisruptionBudget"
assert_contains "pdb: minAvailable 1" "${pdb_output}" "minAvailable: 1"

# --- NetworkPolicy Tests ---
assert_not_contains "default: no NetworkPolicy" "${minimal}" "kind: NetworkPolicy"

netpol=$(helm template test-redis "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol: renders when enabled" "${netpol}" "kind: NetworkPolicy"
assert_contains "netpol: redis port 6379" "${netpol}" "port: 6379"
assert_contains "netpol: exporter port 9121" "${netpol}" "port: 9121"

netpol_no_exporter=$(helm template test-redis "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_not_contains "netpol minimal: no exporter policy" "${netpol_no_exporter}" "redis-exporter"

end_suite
print_summary
