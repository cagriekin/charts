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

# Repmgr: RBAC role restricts to specific resource names
repmgr_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" --show-only templates/rbac.yaml 2>&1)
assert_contains "repmgr: role has resourceNames for services" "${repmgr_role}" "resourceNames:"
assert_contains "repmgr: role restricts service to release name" "${repmgr_role}" "test-pg"

# Full: RBAC role restricts deployment to pgpool resource name
full_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" --show-only templates/rbac.yaml 2>&1)
assert_contains "full: role has deployment resourceNames" "${full_role}" "test-pg-pgpool"

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

# Full: pgpool should disable clear-text frontend auth by default
assert_contains "full: pgpool disables clear-text auth" "${full}" "allow_clear_text_frontend_auth = off"

# Test: pgpool clear-text auth can be explicitly enabled
pgpool_cleartext=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set pgpool.allowClearTextFrontendAuth=true \
  --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "pgpool clear-text auth: enabled when set to true" "${pgpool_cleartext}" "allow_clear_text_frontend_auth = on"

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

# --- SecurityContext Tests ---

# Test: statefulset pod securityContext
assert_contains "repmgr: pod has runAsNonRoot" "${repmgr}" "runAsNonRoot: true"
assert_contains "repmgr: pod has fsGroup" "${repmgr}" "fsGroup: 103"
assert_contains "repmgr: pod has seccompProfile" "${repmgr}" "type: RuntimeDefault"

# Test: postgresql container securityContext
assert_contains "repmgr: postgresql has allowPrivilegeEscalation false" "${repmgr}" "allowPrivilegeEscalation: false"
assert_contains "repmgr: postgresql drops ALL capabilities" "${repmgr}" "drop:"

# Test: fix-permissions init container keeps runAsUser 0 but has allowPrivilegeEscalation false (requires persistence)
default_sts=$(helm template test-pg "${CHART_DIR}" --show-only templates/statefulset.yaml 2>&1)
fix_perms=$(echo "${default_sts}" | sed -n '/name: fix-permissions/,/name: repmgr-init\|name: copy-base-ext\|name: copy-ext\|name: setup-config\|containers:/p')
assert_contains "fix-permissions: runs as root" "${fix_perms}" "runAsUser: 0"
assert_contains "fix-permissions: has allowPrivilegeEscalation false" "${fix_perms}" "allowPrivilegeEscalation: false"

# Test: pgpool deployment has pod securityContext
assert_contains "full: pgpool has runAsNonRoot" "${full}" "runAsNonRoot: true"

# Test: pgpool container has securityContext
assert_contains "full: pgpool has allowPrivilegeEscalation false" "${full}" "allowPrivilegeEscalation: false"

# --- NetworkPolicy Tests ---

# Test: NetworkPolicy not rendered by default
assert_not_contains "default: no NetworkPolicy" "${repmgr}" "kind: NetworkPolicy"

# Test: NetworkPolicy renders when enabled
netpol=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol: postgresql policy rendered" "${netpol}" "test-pg-postgresql"
assert_contains "netpol: has NetworkPolicy kind" "${netpol}" "kind: NetworkPolicy"
assert_contains "netpol: postgresql allows port 5432" "${netpol}" "port: 5432"
assert_contains "netpol: pgpool policy rendered" "${netpol}" "test-pg-pgpool"
assert_contains "netpol: pgpool allows port 9999" "${netpol}" "port: 9999"
assert_contains "netpol: exporter policy rendered" "${netpol}" "test-pg-prometheus-exporter"
assert_contains "netpol: exporter allows port 9116" "${netpol}" "port: 9116"

# Test: NetworkPolicy without pgpool/exporter only renders postgresql policy
netpol_minimal=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol minimal: postgresql policy rendered" "${netpol_minimal}" "test-pg-postgresql"
assert_not_contains "netpol minimal: no pgpool policy" "${netpol_minimal}" "test-pg-pgpool"
assert_not_contains "netpol minimal: no exporter policy" "${netpol_minimal}" "test-pg-prometheus-exporter"

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
repmgr_no_addcmd_poststart=$(echo "${repmgr_no_addcmd}" | sed -n '/postStart:/,/preStop:\|resources:/p')
assert_not_contains "repmgr no additionalCommands: no primary discovery in postStart" "${repmgr_no_addcmd_poststart:-empty}" "pg_is_in_recovery"
assert_not_contains "repmgr no additionalCommands: no PGHOST in postStart" "${repmgr_no_addcmd_poststart:-empty}" "PGHOST"

# --- Graceful Shutdown (preStop) Tests ---

# Test: repmgr enabled renders preStop hook
assert_contains "repmgr: preStop hook present" "${repmgr_no_addcmd}" "preStop:"
assert_contains "repmgr: preStop queries repmgr role" "${repmgr_no_addcmd}" "repmgr.nodes"
assert_contains "repmgr: preStop promotes standby" "${repmgr_no_addcmd}" "pg_promote"
assert_contains "repmgr: preStop runs pg_ctl stop" "${repmgr_no_addcmd}" "pg_ctl stop"
assert_contains "repmgr: preStop waits for recovery mode" "${repmgr_no_addcmd}" "pg_is_in_recovery"
assert_contains "repmgr: terminationGracePeriodSeconds present" "${repmgr_no_addcmd}" "terminationGracePeriodSeconds: 120"

# Test: repmgr with configuration renders both preStop and postStart
repmgr_config=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-config-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr+config: preStop hook present" "${repmgr_config}" "preStop:"
assert_contains "repmgr+config: postStart hook present" "${repmgr_config}" "postStart:"

# Test: custom terminationGracePeriodSeconds
repmgr_custom_tgp=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set repmgr.terminationGracePeriodSeconds=300 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr: custom terminationGracePeriodSeconds" "${repmgr_custom_tgp}" "terminationGracePeriodSeconds: 300"

# Test: repmgrd sidecar has preStop hook and securityContext
repmgrd_section=$(echo "${repmgr_no_addcmd}" | sed -n '/name: repmgrd/,/name: service-updater/p')
assert_contains "repmgr: repmgrd has preStop hook" "${repmgrd_section}" "preStop:"
assert_contains "repmgr: repmgrd preStop runs daemon stop" "${repmgrd_section}" "repmgr daemon stop"
assert_contains "repmgr: repmgrd has allowPrivilegeEscalation false" "${repmgrd_section}" "allowPrivilegeEscalation: false"

# Test: service-updater sidecar has preStop hook, liveness probe, and securityContext
service_updater_section=$(echo "${repmgr_no_addcmd}" | sed -n '/name: service-updater/,/^      volumes:/p')
assert_contains "repmgr: service-updater has preStop hook" "${service_updater_section}" "preStop:"
assert_contains "repmgr: service-updater preStop sleeps" "${service_updater_section}" "sleep 5"
assert_contains "repmgr: service-updater has livenessProbe" "${service_updater_section}" "livenessProbe:"
assert_contains "repmgr: service-updater liveness checks heartbeat file" "${service_updater_section}" "service-updater-alive"
assert_contains "repmgr: service-updater has allowPrivilegeEscalation false" "${service_updater_section}" "allowPrivilegeEscalation: false"

# Test: service-updater configmap writes heartbeat file
assert_contains "repmgr: service-updater script writes heartbeat" "${repmgr_no_addcmd}" "service-updater-alive"

# Test: split-brain detection present in service-updater configmap
assert_contains "repmgr: split-brain detection in service-updater" "${repmgr}" "detect_split_brain"

# Test: SPLIT_BRAIN_ACTION env var in statefulset
assert_contains "repmgr: SPLIT_BRAIN_ACTION env var in statefulset" "${repmgr_no_addcmd}" "SPLIT_BRAIN_ACTION"

# Test: split-brain detection not present when repmgr disabled
assert_not_contains "minimal: no split-brain detection" "${minimal}" "detect_split_brain"

# Test: repmgr disabled does not render preStop or terminationGracePeriodSeconds
assert_not_contains "minimal: no preStop hook" "${minimal}" "preStop:"
assert_not_contains "minimal: no terminationGracePeriodSeconds" "${minimal}" "terminationGracePeriodSeconds"

# Test: helm lint with backup test values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-backup-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with backup test values passes" "0" "${lint_rc}"

# --- Backup CronJob Tests ---

# Test: backup cronjob renders with activeDeadlineSeconds and backoffLimit
backup_cronjob=$(helm template test-pg "${CHART_DIR}" \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.test \
  --set backup.s3.bucket=test \
  --set backup.existingSecret.name=test-secret \
  --show-only templates/backup-cronjob.yaml 2>&1)
assert_contains "backup: activeDeadlineSeconds present" "${backup_cronjob}" "activeDeadlineSeconds: 3600"
assert_contains "backup: backoffLimit present" "${backup_cronjob}" "backoffLimit: 1"
# Test: backup configmap has pg_restore verification
backup_configmap=$(helm template test-pg "${CHART_DIR}" \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.test \
  --set backup.s3.bucket=test \
  --set backup.existingSecret.name=test-secret \
  --show-only templates/backup-configmap.yaml 2>&1)
assert_contains "backup: pg_restore verification present" "${backup_configmap}" "pg_restore --list"

assert_contains "backup: pod has runAsNonRoot" "${backup_cronjob}" "runAsNonRoot: true"
assert_contains "backup: container has allowPrivilegeEscalation false" "${backup_cronjob}" "allowPrivilegeEscalation: false"

# Test: backup cronjob respects custom activeDeadlineSeconds
backup_custom=$(helm template test-pg "${CHART_DIR}" \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.test \
  --set backup.s3.bucket=test \
  --set backup.existingSecret.name=test-secret \
  --set backup.activeDeadlineSeconds=7200 \
  --set backup.backoffLimit=3 \
  --show-only templates/backup-cronjob.yaml 2>&1)
assert_contains "backup: custom activeDeadlineSeconds" "${backup_custom}" "activeDeadlineSeconds: 7200"
assert_contains "backup: custom backoffLimit" "${backup_custom}" "backoffLimit: 3"

# --- pgBackRest Tests ---

# Test: helm lint with pgbackrest values
lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with pgbackrest values passes" "0" "${lint_rc}"

# Render pgbackrest template
pgbackrest=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" 2>&1)

# pgBackRest: configmap renders
assert_contains "pgbackrest: configmap renders" "${pgbackrest}" "test-pg-pgbackrest"
assert_contains "pgbackrest: configmap has stanza" "${pgbackrest}" "[db]"
assert_contains "pgbackrest: configmap has s3 endpoint" "${pgbackrest}" "repo1-s3-endpoint=https://s3.amazonaws.com"
assert_contains "pgbackrest: configmap has s3 bucket" "${pgbackrest}" "repo1-s3-bucket=test-backups"

# pgBackRest: scripts configmap renders
assert_contains "pgbackrest: scripts configmap renders" "${pgbackrest}" "pgbackrest-scheduler.sh"
assert_contains "pgbackrest: run script renders" "${pgbackrest}" "pgbackrest-run.sh"

# pgBackRest: scheduler sidecar present in statefulset
pgbackrest_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1)
assert_contains "pgbackrest: scheduler sidecar present" "${pgbackrest_sts}" "name: pgbackrest-scheduler"
assert_contains "pgbackrest: PGBACKREST_ENABLED env var" "${pgbackrest_sts}" "PGBACKREST_ENABLED"
assert_contains "pgbackrest: PGBACKREST_STANZA env var" "${pgbackrest_sts}" "PGBACKREST_STANZA"
assert_contains "pgbackrest: S3 key env var" "${pgbackrest_sts}" "PGBACKREST_REPO1_S3_KEY"
assert_contains "pgbackrest: config volume mount" "${pgbackrest_sts}" "pgbackrest-config"
assert_contains "pgbackrest: scripts volume mount" "${pgbackrest_sts}" "pgbackrest-scripts"

# pgBackRest: configmap retention values
assert_contains "pgbackrest: retention full in configmap" "${pgbackrest}" "repo1-retention-full=4"
assert_contains "pgbackrest: retention diff in configmap" "${pgbackrest}" "repo1-retention-diff=14"

# pgBackRest: configmap s3 region and prefix
assert_contains "pgbackrest: s3 region in configmap" "${pgbackrest}" "repo1-s3-region=eu-central-1"
assert_contains "pgbackrest: s3 path prefix in configmap" "${pgbackrest}" "/pgbackrest/"

# pgBackRest: schedule env vars on sidecar
assert_contains "pgbackrest: FULL_SCHEDULE env var" "${pgbackrest_sts}" "FULL_SCHEDULE"
assert_contains "pgbackrest: DIFF_SCHEDULE env var" "${pgbackrest_sts}" "DIFF_SCHEDULE"

# pgBackRest: S3 secret references on sidecar
assert_contains "pgbackrest: sidecar S3 key secret ref" "${pgbackrest_sts}" "s3-backup-creds"
assert_contains "pgbackrest: sidecar S3 key ref key" "${pgbackrest_sts}" "access-key-id"
assert_contains "pgbackrest: sidecar S3 secret key ref key" "${pgbackrest_sts}" "secret-access-key"

# pgBackRest: resource limits on scheduler sidecar
pgbackrest_scheduler=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1 | sed -n '/name: pgbackrest-scheduler/,/^        - name:/p')
assert_contains "pgbackrest: scheduler cpu limit" "${pgbackrest_scheduler}" "cpu: 1000m"
assert_contains "pgbackrest: scheduler memory limit" "${pgbackrest_scheduler}" "memory: 1Gi"
assert_contains "pgbackrest: scheduler cpu request" "${pgbackrest_scheduler}" "cpu: 100m"
assert_contains "pgbackrest: scheduler memory request" "${pgbackrest_scheduler}" "memory: 256Mi"

# pgBackRest: shared unix socket volume (pg-run emptyDir)
assert_contains "pgbackrest: pg-run emptyDir volume" "${pgbackrest_sts}" "name: pg-run"
pgbackrest_pg_container=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1 | sed -n '/name: postgresql$/,/^        - name:/p')
assert_contains "pgbackrest: postgresql mounts pg-run" "${pgbackrest_pg_container}" "pg-run"
assert_contains "pgbackrest: scheduler mounts pg-run" "${pgbackrest_scheduler}" "pg-run"

# pgBackRest: works without repmgr (standalone)
pgbackrest_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.test \
  --set pgbackrest.s3.bucket=test \
  --set pgbackrest.existingSecret.name=test-secret \
  --set repmgr.enabled=false \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "pgbackrest standalone: scheduler sidecar present" "${pgbackrest_standalone}" "name: pgbackrest-scheduler"
assert_not_contains "pgbackrest standalone: no repmgrd container" "${pgbackrest_standalone}" "repmgrd"

# pgBackRest: coexists with repmgr
assert_contains "pgbackrest+repmgr: scheduler present" "${pgbackrest_sts}" "name: pgbackrest-scheduler"
assert_contains "pgbackrest+repmgr: repmgrd present" "${pgbackrest_sts}" "name: repmgrd"
assert_contains "pgbackrest+repmgr: service-updater present" "${pgbackrest_sts}" "name: service-updater"

# pgBackRest: verify step after backup
assert_contains "pgbackrest: verify step in run script" "${pgbackrest}" "pgbackrest.*verify"

# pgBackRest: not rendered when disabled (default)
pgbackrest_disabled=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1)
assert_not_contains "pgbackrest disabled: no configmap" "${pgbackrest_disabled}" "pgbackrest"
assert_not_contains "pgbackrest disabled: no scheduler sidecar" "${pgbackrest_disabled}" "pgbackrest-scheduler"

# --- nodeSelector and tolerations Tests ---

# Test: nodeSelector/tolerations not rendered by default on statefulset
assert_not_contains "default: no nodeSelector on statefulset" "${minimal}" "nodeSelector:"
assert_not_contains "default: no tolerations on statefulset" "${minimal}" "tolerations:"

# Test: nodeSelector/tolerations not rendered by default on pgpool
assert_not_contains "default pgpool: no nodeSelector" "${full}" "nodeSelector:"
assert_not_contains "default pgpool: no tolerations on pgpool" "${full}" "tolerations:"

# Test: postgresql nodeSelector renders on statefulset
sts_nodeselector=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.nodeSelector.node\.kubernetes\.io/workload-type=stateful' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "postgresql nodeSelector: renders on statefulset" "${sts_nodeselector}" "node.kubernetes.io/workload-type: stateful"

# Test: postgresql tolerations render on statefulset
sts_tolerations=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.tolerations[0].key=workload-type' \
  --set 'postgresql.tolerations[0].operator=Equal' \
  --set 'postgresql.tolerations[0].value=stateful' \
  --set 'postgresql.tolerations[0].effect=NoSchedule' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "postgresql tolerations: key renders" "${sts_tolerations}" "key: workload-type"
assert_contains "postgresql tolerations: value renders" "${sts_tolerations}" "value: stateful"
assert_contains "postgresql tolerations: effect renders" "${sts_tolerations}" "effect: NoSchedule"

# Test: pgpool nodeSelector renders on deployment
pgpool_nodeselector=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set 'pgpool.nodeSelector.node\.kubernetes\.io/workload-type=stateful' \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "pgpool nodeSelector: renders on deployment" "${pgpool_nodeselector}" "node.kubernetes.io/workload-type: stateful"

# Test: pgpool tolerations render on deployment
pgpool_tolerations=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set 'pgpool.tolerations[0].key=workload-type' \
  --set 'pgpool.tolerations[0].operator=Equal' \
  --set 'pgpool.tolerations[0].value=stateful' \
  --set 'pgpool.tolerations[0].effect=NoSchedule' \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "pgpool tolerations: key renders" "${pgpool_tolerations}" "key: workload-type"
assert_contains "pgpool tolerations: value renders" "${pgpool_tolerations}" "value: stateful"
assert_contains "pgpool tolerations: effect renders" "${pgpool_tolerations}" "effect: NoSchedule"

# Test: nodeSelector coexists with affinity on statefulset
sts_both=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.nodeSelector.node\.kubernetes\.io/workload-type=stateful' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "nodeSelector+affinity: nodeSelector present" "${sts_both}" "nodeSelector:"
assert_contains "nodeSelector+affinity: default anti-affinity still present" "${sts_both}" "podAntiAffinity"

# Test: helm lint passes with nodeSelector and tolerations
lint_output=$(helm lint "${CHART_DIR}" \
  --set 'postgresql.nodeSelector.node\.kubernetes\.io/workload-type=stateful' \
  --set 'postgresql.tolerations[0].key=workload-type' \
  --set 'postgresql.tolerations[0].operator=Equal' \
  --set 'postgresql.tolerations[0].value=stateful' \
  --set 'postgresql.tolerations[0].effect=NoSchedule' \
  --set pgpool.enabled=true \
  --set 'pgpool.nodeSelector.node\.kubernetes\.io/workload-type=stateful' \
  --set 'pgpool.tolerations[0].key=workload-type' \
  --set 'pgpool.tolerations[0].operator=Equal' \
  --set 'pgpool.tolerations[0].value=stateful' \
  --set 'pgpool.tolerations[0].effect=NoSchedule' \
  2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with nodeSelector and tolerations passes" "0" "${lint_rc}"

end_suite
print_summary
