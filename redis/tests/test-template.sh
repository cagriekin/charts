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

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-persistence-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with persistence test values passes" "0" "${lint_rc}"

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
assert_contains "pdb: maxUnavailable 1 (HA default)" "${pdb_output}" "maxUnavailable: 1"
assert_contains "pdb: unhealthyPodEvictionPolicy" "${pdb_output}" "unhealthyPodEvictionPolicy: AlwaysAllow"

pdb_minavail=$(helm template test-redis "${CHART_DIR}" \
  --set redis.podDisruptionBudget.enabled=true \
  --set redis.podDisruptionBudget.minAvailable=1 \
  --set redis.podDisruptionBudget.maxUnavailable=null \
  --show-only templates/pdb.yaml 2>&1)
assert_contains "pdb: explicit minAvailable wins" "${pdb_minavail}" "minAvailable: 1"

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

# --- PrometheusRule Tests ---
assert_not_contains "default: no PrometheusRule" "${full}" "kind: PrometheusRule"

promrule=$(helm template test-redis "${CHART_DIR}" \
  --set exporter.enabled=true \
  --set exporter.prometheusRule.enabled=true \
  --show-only templates/prometheusrule.yaml 2>&1)
assert_contains "promrule: renders when enabled" "${promrule}" "kind: PrometheusRule"
assert_contains "promrule: RedisDown alert" "${promrule}" "RedisDown"
assert_contains "promrule: RedisMemoryHigh alert" "${promrule}" "RedisMemoryHigh"
assert_contains "promrule: RedisAOFWriteFailure alert" "${promrule}" "RedisAOFWriteFailure"

# --- Replication (Sentinel HA) ---
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-replication-full.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with replication-full values passes" "0" "${lint_rc}"

repl=$(helm template r "${CHART_DIR}" -f "${SCRIPT_DIR}/values-replication-full.yaml" 2>&1)
assert_contains "repl: 3 pods" "${repl}" "replicas: 3"
assert_contains "repl: sentinel sidecar container" "${repl}" "name: sentinel"
assert_contains "repl: bootstrap init container" "${repl}" "name: redis-bootstrap"
assert_contains "repl: headless service" "${repl}" "r-redis-headless"
assert_contains "repl: sentinel discovery service" "${repl}" "r-redis-sentinel"
assert_contains "repl: tmpfs config volume" "${repl}" "medium: Memory"
assert_contains "repl: generated secret" "${repl}" "kind: Secret"
assert_contains "repl: sentinel monitor in bootstrap" "${repl}" "sentinel monitor"
assert_contains "repl: split-brain alert" "${repl}" "RedisMultipleMasters"
assert_contains "repl: writes-blocked alert" "${repl}" "RedisWritesBlocked"

repl_netpol=$(helm template r "${CHART_DIR}" -f "${SCRIPT_DIR}/values-replication-full.yaml" \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "repl: netpol opens sentinel port 26379" "${repl_netpol}" "port: 26379"
assert_contains "repl: netpol opens metrics port 9121" "${repl_netpol}" "port: 9121"

# Standalone is not affected by the replication apparatus
std=$(helm template r "${CHART_DIR}" --set architecture=standalone --set redis.auth.enabled=false 2>&1)
assert_not_contains "std: no sentinel container" "${std}" "name: sentinel"
assert_not_contains "std: no headless service" "${std}" "redis-headless"
assert_contains "std: single pod" "${std}" "replicas: 1"

# --- TLS ---
tls=$(helm template r "${CHART_DIR}" -f "${SCRIPT_DIR}/values-tls.yaml" 2>&1)
assert_contains "tls: redis tls-port" "${tls}" "tls-port 6379"
assert_contains "tls: replication over TLS" "${tls}" "tls-replication yes"
assert_contains "tls: cert volume mount" "${tls}" "/etc/redis/tls"
tls_fail=$(helm template r "${CHART_DIR}" --set tls.enabled=true 2>&1) && tls_rc=0 || tls_rc=$?
assert_eq "tls: enabled without secret fails" "1" "${tls_rc}"

# --- ACL ---
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-acl.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with acl (standalone) values passes" "0" "${lint_rc}"
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-acl-replication.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with acl (replication) values passes" "0" "${lint_rc}"

# Replication, locked-down default + custom operator: the operator identity is wired into the
# replication link, Sentinel, and the in-pod redis-cli probes, and default becomes a user line.
acl=$(helm template r "${CHART_DIR}" -f "${SCRIPT_DIR}/values-acl-replication.yaml" 2>&1)
assert_contains "acl: operator user line rendered" "${acl}" "user ops on"
assert_contains "acl: default user redefined (requirepass dropped)" "${acl}" "user default on"
assert_contains "acl: replication uses masteruser" "${acl}" "masteruser"
assert_contains "acl: sentinel auth-user wired" "${acl}" "sentinel auth-user"
assert_contains "acl: probes carry --user" "${acl}" "redis-cli --user ops"

# Default operator (additive user) keeps the standalone requirepass path; no operator wiring.
acl_add=$(helm template r "${CHART_DIR}" -f "${SCRIPT_DIR}/values-acl.yaml" 2>&1)
assert_contains "acl(additive): app user line rendered" "${acl_add}" "user app on"
assert_not_contains "acl(additive): no masteruser in standalone" "${acl_add}" "masteruser"

# Guard: locking down default without a separate operator is rejected.
acl_fail=$(helm template r "${CHART_DIR}" --set redis.auth.acl.enabled=true \
  --set 'redis.auth.acl.users[0].name=default' --set 'redis.auth.acl.users[0].rules=~* +@read' 2>&1) && acl_rc=0 || acl_rc=$?
assert_eq "acl: locked default without operator fails" "1" "${acl_rc}"

end_suite
print_summary
