#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

begin_suite "Helm Template Rendering"

# Test: helm lint with default values
lint_output=$(helm lint "${CHART_DIR}" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with default values passes" "0" "${lint_rc}"

# Test: helm lint with minimal values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with minimal values passes" "0" "${lint_rc}"

# Test: helm lint with repmgr values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with repmgr values passes" "0" "${lint_rc}"

# Test: helm lint with full values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with full values passes" "0" "${lint_rc}"

# Render minimal template
minimal=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)

# Minimal: should have statefulset with 1 replica (replicaCount 0 + 1)
assert_contains "minimal: statefulset has replicas: 1" "${minimal}" "replicas: 1"

# Minimal: should NOT have repmgr containers
assert_not_contains "minimal: no repmgrd sidecar" "${minimal}" "name: repmgrd"
assert_not_contains "minimal: no service-updater sidecar" "${minimal}" "name: service-updater"

# Minimal: should NOT have pod annotations on statefulset
assert_not_contains "minimal: no pod annotations on statefulset" "${minimal}" "karpenter.sh/do-not-disrupt"

# Minimal: should NOT have pgpool
assert_not_contains "minimal: no pgpool deployment" "${minimal}" "pgpool"

# Minimal: should NOT have prometheus exporter
assert_not_contains "minimal: no prometheus exporter" "${minimal}" "postgres-exporter"

# Minimal: should create a secret
assert_contains "minimal: secret is created" "${minimal}" "kind: Secret"

# Minimal: should have service
assert_contains "minimal: service exists" "${minimal}" "kind: Service"

# Render repmgr template
repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1)

# Repmgr: should have statefulset with 2 replicas (replicaCount 1 + 1)
assert_contains "repmgr: statefulset has replicas: 2" "${repmgr}" "replicas: 2"

# Repmgr: should have repmgr sidecars
assert_contains "repmgr: repmgrd sidecar present" "${repmgr}" "name: repmgrd"
assert_contains "repmgr: service-updater sidecar present" "${repmgr}" "name: service-updater"

# Repmgr: should have RBAC resources
assert_contains "repmgr: serviceaccount created" "${repmgr}" "kind: ServiceAccount"
assert_contains "repmgr: role created" "${repmgr}" "kind: Role"
assert_contains "repmgr: rolebinding created" "${repmgr}" "kind: RoleBinding"

# Repmgr: should have headless service
assert_contains "repmgr: headless service present" "${repmgr}" "clusterIP: None"

# Repmgr: service-updater configmap
assert_contains "repmgr: service-updater configmap present" "${repmgr}" "service-updater"

# Render full template
full=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)

# Full: should have statefulset with 3 replicas (replicaCount 2 + 1)
assert_contains "full: statefulset has replicas: 3" "${full}" "replicas: 3"

# Full: should have pgpool deployment
assert_contains "full: pgpool deployment present" "${full}" "test-pg-pgpool"

# Full: pgpool should have rolling update strategy with maxUnavailable 0
assert_contains "full: pgpool has maxUnavailable: 0" "${full}" "maxUnavailable: 0"
assert_contains "full: pgpool has maxSurge: 1" "${full}" "maxSurge: 1"

# Full: pgpool should have pod anti-affinity by default
assert_contains "full: pgpool has podAntiAffinity" "${full}" "podAntiAffinity"
assert_contains "full: pgpool anti-affinity uses hostname topology" "${full}" "topologyKey: kubernetes.io/hostname"

# Full: should have pgpool service
pgpool_svc=$(echo "${full}" | grep -c "port: 9999" || echo "0")
assert_gt "full: pgpool service port 9999 present" "${pgpool_svc}" "0"

# Full: should have pgpool configmap
assert_contains "full: pgpool configmap present" "${full}" "pgpool.conf"

# Full: should have prometheus exporter deployment
assert_contains "full: prometheus exporter present" "${full}" "postgres-exporter"

# Full: should have prometheus exporter service
exporter_svc=$(echo "${full}" | grep -c "port: 9116" || echo "0")
assert_gt "full: exporter service port 9116 present" "${exporter_svc}" "0"

# Full: should have pgpool metrics exporter
assert_contains "full: pgpool metrics sidecar present" "${full}" "pgpool2_exporter"

# Full: postgresql podAnnotations should be rendered
assert_contains "full: postgresql pod has karpenter do-not-disrupt annotation" "${full}" "karpenter.sh/do-not-disrupt"
assert_contains "full: postgresql pod has custom annotation" "${full}" "custom-annotation: pg-value"

# Full: pgpool podAnnotations should be rendered alongside metrics annotations
assert_contains "full: pgpool pod has prometheus scrape annotation" "${full}" "prometheus.io/scrape"
assert_contains "full: pgpool pod has prometheus port annotation" "${full}" "prometheus.io/port"

# Render template with podAnnotations but no metrics on pgpool
no_metrics=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set pgpool.metrics.enabled=false \
  --set pgpool.podAnnotations."karpenter\.sh/do-not-disrupt"=true \
  --show-only templates/pgpool-deployment.yaml 2>&1)

assert_contains "pgpool no-metrics: pod has karpenter annotation" "${no_metrics}" "karpenter.sh/do-not-disrupt"
assert_not_contains "pgpool no-metrics: no prometheus scrape annotation" "${no_metrics}" "prometheus.io/scrape"

# Render template with metrics but no podAnnotations on pgpool
metrics_only=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set pgpool.metrics.enabled=true \
  --show-only templates/pgpool-deployment.yaml 2>&1)

assert_contains "pgpool metrics-only: has prometheus scrape" "${metrics_only}" "prometheus.io/scrape"
assert_not_contains "pgpool metrics-only: no karpenter annotation" "${metrics_only}" "karpenter.sh/do-not-disrupt"

# Render template with neither metrics nor podAnnotations on pgpool
no_annotations=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set pgpool.metrics.enabled=false \
  --show-only templates/pgpool-deployment.yaml 2>&1)

assert_not_contains "pgpool no-annotations: no annotations block" "${no_annotations}" "annotations:"

# --- PostgreSQL Configuration Tests ---

# Test: helm lint with config values (standalone)
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-config.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with config values passes" "0" "${lint_rc}"

# Test: helm lint with config+repmgr values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-config-repmgr.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with config+repmgr values passes" "0" "${lint_rc}"

# Render standalone config template
config_standalone=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-config.yaml" 2>&1)

# Config standalone: ConfigMap is created with custom.conf
assert_contains "config standalone: postgresql-config configmap created" "${config_standalone}" "test-pg-postgresql-config"
assert_contains "config standalone: custom.conf present in configmap" "${config_standalone}" "custom.conf"
assert_contains "config standalone: work_mem in configmap" "${config_standalone}" "work_mem = '64MB'"
assert_contains "config standalone: maintenance_work_mem in configmap" "${config_standalone}" "maintenance_work_mem = '128MB'"
assert_contains "config standalone: log_statement in configmap" "${config_standalone}" "log_statement = 'all'"

# Config standalone: setup-config init container present
assert_contains "config standalone: setup-config init container present" "${config_standalone}" "name: setup-config"
assert_contains "config standalone: include_dir in setup-config" "${config_standalone}" "include_dir = '/etc/postgresql/conf.d'"

# Config standalone: volume mount present
assert_contains "config standalone: conf.d volume mount present" "${config_standalone}" "/etc/postgresql/conf.d"

# Config standalone: checksum annotation present
assert_contains "config standalone: checksum annotation present" "${config_standalone}" "checksum/postgresql-config"

# Config standalone: pgHba entries injected in postStart
assert_contains "config standalone: pgHba entry in postStart" "${config_standalone}" "host all all 10.244.0.0/16 md5"

# Render repmgr config template
config_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-config-repmgr.yaml" 2>&1)

# Config repmgr: ConfigMap is created
assert_contains "config repmgr: postgresql-config configmap created" "${config_repmgr}" "test-pg-postgresql-config"
assert_contains "config repmgr: work_mem in configmap" "${config_repmgr}" "work_mem = '64MB'"

# Config repmgr: setup-config init container present
assert_contains "config repmgr: setup-config init container present" "${config_repmgr}" "name: setup-config"
assert_contains "config repmgr: include_dir in setup-config" "${config_repmgr}" "include_dir = '/etc/postgresql/conf.d'"

# Config repmgr: volume mount present
assert_contains "config repmgr: conf.d volume mount present" "${config_repmgr}" "/etc/postgresql/conf.d"

# Config repmgr: checksum annotation present
assert_contains "config repmgr: checksum annotation present" "${config_repmgr}" "checksum/postgresql-config"

# Config repmgr: pgHba entries in postStart
assert_contains "config repmgr: pgHba entry in postStart" "${config_repmgr}" "host all all 10.244.0.0/16 md5"

# Test: no config resources when configuration is empty (defaults)
no_config=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)
assert_not_contains "no config: no postgresql-config configmap" "${no_config}" "postgresql-config"
assert_not_contains "no config: no setup-config init container" "${no_config}" "setup-config"
assert_not_contains "no config: no checksum annotation" "${no_config}" "checksum/postgresql-config"
assert_not_contains "no config: no conf.d volume mount" "${no_config}" "/etc/postgresql/conf.d"

# Test: no config resources when repmgr defaults (no configuration set)
no_config_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1)
assert_not_contains "no config repmgr: no postgresql-config configmap" "${no_config_repmgr}" "postgresql-config"
assert_not_contains "no config repmgr: no setup-config init container" "${no_config_repmgr}" "setup-config"

# Test: checksum changes when configuration changes
config_v1=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.configuration.work_mem=32MB' \
  --show-only templates/statefulset.yaml 2>&1)
config_v2=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.configuration.work_mem=64MB' \
  --show-only templates/statefulset.yaml 2>&1)
checksum_v1=$(echo "${config_v1}" | grep "checksum/postgresql-config" | awk '{print $2}')
checksum_v2=$(echo "${config_v2}" | grep "checksum/postgresql-config" | awk '{print $2}')
if [[ -n "${checksum_v1}" && -n "${checksum_v2}" && "${checksum_v1}" != "${checksum_v2}" ]]; then
  pass "config checksum changes when configuration changes"
else
  fail "config checksum changes when configuration changes" "v1='${checksum_v1}' v2='${checksum_v2}'"
fi

# --- postStart additionalCommands Tests ---

# Test: repmgr + additionalCommands renders primary discovery logic
repmgr_addcmd=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set 'postgresql.lifecycle.postStart.additionalCommands=echo test-command' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr additionalCommands: discovers primary via pg_is_in_recovery" "${repmgr_addcmd}" "pg_is_in_recovery"
assert_contains "repmgr additionalCommands: sets PGHOST to primary" "${repmgr_addcmd}" "PGHOST"
assert_contains "repmgr additionalCommands: scans headless service pods" "${repmgr_addcmd}" "headless"
assert_contains "repmgr additionalCommands: renders the command" "${repmgr_addcmd}" "test-command"

# Test: standalone + additionalCommands runs directly without primary discovery
standalone_addcmd=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --set 'postgresql.lifecycle.postStart.additionalCommands=echo test-command' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "standalone additionalCommands: renders the command" "${standalone_addcmd}" "test-command"
assert_not_contains "standalone additionalCommands: no primary discovery" "${standalone_addcmd}" "pg_is_in_recovery"

# Test: repmgr without additionalCommands does not render primary discovery
repmgr_no_addcmd=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "repmgr no additionalCommands: no primary discovery" "${repmgr_no_addcmd}" "pg_is_in_recovery"
assert_not_contains "repmgr no additionalCommands: no PGHOST" "${repmgr_no_addcmd}" "PGHOST"

end_suite
print_summary
