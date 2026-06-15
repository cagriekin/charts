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

# Special-character credential safety (#108): no sed placeholder
# substitution, byte-safe splice in both init containers, DSN built with
# percent-encoded credentials in a file instead of a raw env URI
assert_not_contains "full: no sed placeholder substitution remains" "${full}" 'sed -i "s/__POSTGRES'
assert_contains "full: init containers use byte-safe splice" "${full}" "function splice"
assert_contains "full: exporter DSN credentials percent-encoded" "${full}" "od -An -v -tx1"
assert_contains "full: exporter reads DSN from init-built file" "${full}" 'DATA_SOURCE_NAME="$(cat /etc/postgres_exporter/dsn)"'
assert_not_contains "full: no raw unencoded DSN env" "${full}" 'postgresql://$(POSTGRES_USER)'
assert_contains "full: exporter yml placeholders single-quoted" "${full}" "username: '__POSTGRES_USER__'"

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

# The config checksum annotation is unconditional: pgpool.conf and
# pool_passwd live in an emptyDir written by the init container, so
# configmap changes only reach pods through a template-hash roll
assert_contains "pgpool no-annotations: checksum annotation still present" "${no_annotations}" "checksum/pgpool-config"
assert_not_contains "pgpool no-annotations: no prometheus annotations" "${no_annotations}" "prometheus.io/scrape"

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

# Test: postgresql egress allows API server ports
assert_contains "netpol: egress allows 443" "${netpol}" "port: 443"
assert_contains "netpol: egress allows 6443" "${netpol}" "port: 6443"

# Test: postgresql egress allows the pgpool backend port (#129). service-updater
# health-checks pgpool from the pg pods; without an egress rule for 9999 the
# check times out and perpetually rollout-restarts pgpool. The pgpool policy's
# ingress also allows 9999, so isolate the postgresql policy's egress block to
# prove the rule is on the egress side specifically.
pg_egress=$(printf '%s\n' "${netpol}" | awk '
  /^---/ { inpg=0; eg=0 }
  /name: test-pg-postgresql$/ { inpg=1 }
  inpg && /^  egress:/ { eg=1 }
  eg && /^---/ { eg=0 }
  eg { print }
')
assert_contains "netpol: postgresql egress allows pgpool port 9999 (#129)" "${pg_egress}" "port: 9999"
assert_contains "netpol: postgresql egress targets pgpool component (#129)" "${pg_egress}" "app.kubernetes.io/component: pgpool"
# pgpool disabled -> no pgpool egress rule (the rule is gated on pgpool.enabled)
assert_not_contains "netpol minimal: no pgpool egress when pgpool disabled (#129)" "${netpol_minimal}" "app.kubernetes.io/component: pgpool"

# Test: egress derives S3 port from pgbackrest endpoint with explicit port
netpol_pgbackrest=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set networkPolicy.enabled=true \
  --set pgbackrest.s3.endpoint=http://minio.minio.svc.cluster.local:9000 \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol pgbackrest: egress allows S3 port 9000" "${netpol_pgbackrest}" "port: 9000"

# Test: https endpoint without explicit port adds no duplicate 443
netpol_pgbackrest_https=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
https_443_count=$(printf '%s' "${netpol_pgbackrest_https}" | grep -c "port: 443" || true)
assert_eq "netpol pgbackrest: https endpoint adds no duplicate 443" "1" "${https_443_count}"

# Test: http endpoint without explicit port maps to 80
netpol_pgbackrest_http=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set networkPolicy.enabled=true \
  --set pgbackrest.s3.endpoint=http://minio.svc \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol pgbackrest: http endpoint maps to port 80" "${netpol_pgbackrest_http}" "port: 80"

# Test: extraEgress rules render for postgresql and pgpool
netpol_extra=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.postgresql.extraEgress[0].ports[0].port=8080' \
  --set 'networkPolicy.pgpool.extraEgress[0].ports[0].port=8081' \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol extraEgress: postgresql rule rendered" "${netpol_extra}" "port: 8080"
assert_contains "netpol extraEgress: pgpool rule rendered" "${netpol_extra}" "port: 8081"

# #135: the pgpool NetworkPolicy must open the metrics scrape port (9719) when
# pgpool.metrics.enabled, mirroring the scrape annotations the chart emits; else
# the CNI silently drops all pgpool2_exporter scrapes. 9719 only appears in the
# netpol render via this rule (the Deployment is a separate template).
netpol_metrics=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true --set pgpool.metrics.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "#135: pgpool netpol opens metrics port 9719 when metrics enabled" "${netpol_metrics}" "port: 9719"
netpol_nometrics=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true --set pgpool.metrics.enabled=false \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_not_contains "#135: pgpool netpol omits 9719 when metrics disabled" "${netpol_nometrics}" "port: 9719"

# #136: extraIngress must render as a full ingress rule (its own from+ports) at
# the rules level so it can open a non-5432 port -- not be spliced into the 5432
# rule's from: list (which rejected rule-shaped input and limited peers to 5432).
# Parse the postgresql policy: a correct splice yields 2 ingress rules, one with
# the extra port; the old from:-nesting yielded a single rule.
netpol_xi=$(helm template test-pg "${CHART_DIR}" --set networkPolicy.enabled=true \
  --set 'networkPolicy.postgresql.extraIngress[0].from[0].namespaceSelector.matchLabels.name=monitoring' \
  --set 'networkPolicy.postgresql.extraIngress[0].ports[0].port=9116' \
  --show-only templates/networkpolicy.yaml 2>&1)
xi_shape=$(printf '%s' "${netpol_xi}" | python3 -c "
import sys,yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get('kind')=='NetworkPolicy' and d['metadata']['name'].endswith('-postgresql'):
        ing=d['spec']['ingress']
        ports=[p.get('port') for r in ing for p in r.get('ports',[])]
        print('rules=%d has9116=%s' % (len(ing), 9116 in ports)); break
")
assert_contains "#136: postgresql extraIngress renders as its own ingress rule" "${xi_shape}" "rules=2"
assert_contains "#136: postgresql extraIngress can open a non-5432 port" "${xi_shape}" "has9116=True"

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

# Config standalone: no 10.0.0.0/8 trust line is injected (security: issue #14)
assert_not_contains "config standalone: no 10.0.0.0/8 trust injection" "${config_standalone}" "10.0.0.0/8 trust"
assert_not_contains "minimal: no 10.0.0.0/8 trust injection" "${minimal}" "10.0.0.0/8 trust"

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

# #144: in the repmgr branch pg_hba is first-match-wins behind the image's broad
# 10.0.0.0/8 and 0.0.0.0/0 catch-alls, so user entries appended at EOF could
# never match. They must be inserted above the first non-loopback host rule.
hba_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set 'postgresql.pgHba[0]=host replication repmgr 10.0.0.0/8 trust' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#144: repmgr pgHba inserted above network rules (sentinel present)" "${hba_repmgr}" "user pgHba entries (above network auth rules)"
assert_not_contains "#144: repmgr pgHba no longer appended at EOF" "${hba_repmgr}" "sed -i '\$ a"
# behavioral: extract the insertion awk from the rendered postStart and run it
# over a sample pg_hba mirroring the repmgr image's file (post md5-fallback).
hba_awk_body=$(printf '%s\n' "${hba_repmgr}" | sed -n "/awk -v ins=0/,/HBA_FILE_USER\" > \"/p" | sed '1d;$d')
hba_bodyfile=$(mktemp); printf '%s\n' "${hba_awk_body}" > "${hba_bodyfile}"
hba_sample=$(mktemp)
printf '%s\n' \
  "local   all all all trust" \
  "host    all all 127.0.0.1/32 trust" \
  "host    all all ::1/128 trust" \
  "host    replication all 10.0.0.0/8 scram-sha-256" \
  "host    all all 0.0.0.0/0 md5" > "${hba_sample}"
hba_result=$(awk -v ins=0 -f "${hba_bodyfile}" "${hba_sample}")
ut_ln=$(printf '%s\n' "${hba_result}" | grep -n "repmgr 10.0.0.0/8 trust" | head -1 | cut -d: -f1)
lo_ln=$(printf '%s\n' "${hba_result}" | grep -n "::1/128" | head -1 | cut -d: -f1)
sc_ln=$(printf '%s\n' "${hba_result}" | grep -n "all 10.0.0.0/8 scram-sha-256" | head -1 | cut -d: -f1)
if [ -n "${ut_ln}" ] && [ -n "${lo_ln}" ] && [ -n "${sc_ln}" ] && [ "${ut_ln}" -gt "${lo_ln}" ] && [ "${ut_ln}" -lt "${sc_ln}" ]; then hba144=ok; else hba144="ut=${ut_ln:-x} lo=${lo_ln:-x} sc=${sc_ln:-x}"; fi
assert_eq "#144: user pgHba lands below loopback and above network auth rules" "ok" "${hba144}"
rm -f "${hba_bodyfile}" "${hba_sample}"

# Test: configuration disabled still renders setup-config, which strips a
# stale include_dir left in PGDATA by a previous enable (#107)
no_config=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)
assert_not_contains "no config: no postgresql-config configmap" "${no_config}" "postgresql-config"
assert_contains "no config: setup-config init container still present" "${no_config}" "name: setup-config"
assert_contains "no config: setup-config strips stale include_dir" "${no_config}" 'grep -v "include_dir'
assert_not_contains "no config: setup-config does not append include_dir" "${no_config}" 'echo "include_dir'
assert_not_contains "no config: no checksum annotation" "${no_config}" "checksum/postgresql-config"
assert_not_contains "no config: no conf.d volume mount" "${no_config}" "mountPath: /etc/postgresql/conf.d"

# Test: repmgr defaults (no configuration set) also render the cleanup
no_config_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1)
assert_not_contains "no config repmgr: no postgresql-config configmap" "${no_config_repmgr}" "postgresql-config"
assert_contains "no config repmgr: setup-config strips stale include_dir" "${no_config_repmgr}" 'grep -v "include_dir'

# Test: enabled render appends and never strips
assert_not_contains "config standalone: no include_dir strip when enabled" "${config_standalone}" 'grep -v "include_dir'

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

# #127: the discovery psql and the user commands connect over TCP to a remote
# pod; the image's pg_hba requires md5/scram for every non-loopback connection,
# so PGPASSWORD must be exported or both fail auth and silently no-op. PGHOST
# must be exported as its own statement -- a bare `PGHOST=... <cmd>` prefix is
# split onto its own line by nindent, so the child psql never sees it and runs
# against the local socket of whatever pod fired the hook.
assert_contains "repmgr additionalCommands: exports PGPASSWORD for TCP auth (#127)" "${repmgr_addcmd}" 'export PGPASSWORD="\$POSTGRES_PASSWORD"'
assert_contains "repmgr additionalCommands: exports PGHOST as its own statement (#127)" "${repmgr_addcmd}" 'export PGHOST="\$PRIMARY_HOST"'
# the PGHOST export must precede the user command (else it runs locally)
pghost_ln=$(printf '%s\n' "${repmgr_addcmd}" | grep -n 'export PGHOST="\$PRIMARY_HOST"' | awk -F: 'NR==1{print $1}') || pghost_ln=""
usercmd_ln=$(printf '%s\n' "${repmgr_addcmd}" | grep -n 'echo test-command' | awk -F: 'NR==1{print $1}') || usercmd_ln=""
if [ -n "${pghost_ln}" ] && [ -n "${usercmd_ln}" ] && [ "${pghost_ln}" -lt "${usercmd_ln}" ]; then ord127=ok; else ord127="pghost=${pghost_ln:-none} usercmd=${usercmd_ln:-none}"; fi
assert_eq "repmgr additionalCommands: PGHOST export precedes user command (#127)" "ok" "${ord127}"

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
assert_contains "repmgr: preStop runs pg_ctl stop" "${repmgr_no_addcmd}" "pg_ctl stop"
assert_contains "repmgr: terminationGracePeriodSeconds present" "${repmgr_no_addcmd}" "terminationGracePeriodSeconds: 120"

# Test: preStop must not promote out-of-band; a raw pg_promote() bypasses
# repmgr.nodes metadata and strands every repmgrd on stale state (#102)
assert_not_contains "repmgr: preStop does not call pg_promote" "${repmgr_no_addcmd}" "pg_promote"
assert_not_contains "repmgr: preStop does not target a standby" "${repmgr_no_addcmd}" "STANDBY_HOST"

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

# Test: split-brain handling present in service-updater configmap
assert_contains "repmgr: split-brain handling in service-updater" "${repmgr}" "handle_split_brain"

# Test: SPLIT_BRAIN_ACTION env var in statefulset
assert_contains "repmgr: SPLIT_BRAIN_ACTION env var in statefulset" "${repmgr_no_addcmd}" "SPLIT_BRAIN_ACTION"

# Test: split-brain handling not present when repmgr disabled
assert_not_contains "minimal: no split-brain handling" "${minimal}" "handle_split_brain"

# --- Stale-primary selector safety (#124) and LSN fence ordering (#131) ---
su_cm=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/service-updater-configmap.yaml 2>&1)

# #124: master is determined by actual role (pg_is_in_recovery), not by each
# node's self-reported repmgr.nodes metadata (which a stale primary forges)
assert_contains "su #124: classifies primaries by pg_is_in_recovery" "${su_cm}" "SELECT pg_is_in_recovery();"
assert_not_contains "su #124: no longer trusts repmgr.nodes metadata for master" "${su_cm}" "WHERE type = 'primary' AND active = true"
# #124: the selector only moves when exactly one primary exists; a split-brain
# is handled (not used to repoint the selector to the lowest-ordinal node)
assert_contains "su #124: selector update gated on single primary" "${su_cm}" 'PRIMARY_COUNT" -eq 1'
assert_contains "su #124: two-or-more primaries routed to split-brain handler" "${su_cm}" 'PRIMARY_COUNT" -ge 2'

# #131: fence picks the survivor by timeline then numeric LSN, not a
# lexicographic string compare that mis-orders unpadded hex
assert_not_contains "su #131: no lexicographic LSN string compare" "${su_cm}" '"$lsn" > "$best_lsn"'
assert_contains "su #131: numeric hex LSN comparison" "${su_cm}" "16#"
assert_contains "su #131: survivor selection is timeline-first" "${su_cm}" "pg_walfile_name(pg_current_wal_lsn())"

# #131: behavioral unit test of the LSN comparator extracted from the rendered
# script -- the boundary cases the lexicographic compare got wrong
printf '%s' "${su_cm}" | python3 -c "import sys,yaml; sys.stdout.write(yaml.safe_load(sys.stdin)['data']['service-updater.sh'])" > "${SCRIPT_DIR}/.lsn_gt_render.sh"
sed -n '/^lsn_gt() {/,/^}/p' "${SCRIPT_DIR}/.lsn_gt_render.sh" > "${SCRIPT_DIR}/.lsn_gt_fn.sh"
lsn_cmp_rc=0
bash -c '
  source "'"${SCRIPT_DIR}"'/.lsn_gt_fn.sh"
  # left genuinely ahead -> true; the cases lexicographic compare inverted
  lsn_gt "10/00000001" "9/2B3C4D50" || exit 1
  lsn_gt "100/0" "F2/FFFFFFFF" || exit 1
  lsn_gt "2/3000000" "2/2FFFFFF" || exit 1
  # behind or equal -> false
  lsn_gt "9/2B3C4D50" "10/00000001" && exit 1
  lsn_gt "5/100" "5/100" && exit 1
  exit 0
' || lsn_cmp_rc=$?
assert_eq "su #131: lsn_gt orders unpadded hex LSNs numerically" "0" "${lsn_cmp_rc}"
rm -f "${SCRIPT_DIR}/.lsn_gt_render.sh" "${SCRIPT_DIR}/.lsn_gt_fn.sh"

# #168: the WAL-filename timeline is HEXADECIMAL; it must be decoded with 16#,
# not a SQL ::int cast (which errors at TL 0x0A and is wrong from 0x10). The
# timeline read must NOT use ::int, and tl_to_int must decode hex correctly.
assert_not_contains "su #168: timeline not parsed with ::int (hex-as-decimal bug)" "${su_cm}" "from 1 for 8)::int"
assert_contains "su #168: timeline decoded via tl_to_int helper" "${su_cm}" "tl_to_int"
# behavioral unit test of the decoder extracted from the rendered script
printf '%s' "${su_cm}" | python3 -c "import sys,yaml; sys.stdout.write(yaml.safe_load(sys.stdin)['data']['service-updater.sh'])" > "${SCRIPT_DIR}/.tl_render.sh"
sed -n '/^tl_to_int() {/,/^}/p' "${SCRIPT_DIR}/.tl_render.sh" > "${SCRIPT_DIR}/.tl_fn.sh"
tl_rc=0
bash -c '
  source "'"${SCRIPT_DIR}"'/.tl_fn.sh"
  [ "$(tl_to_int 00000001)" = "1" ]  || exit 1   # TL 1
  [ "$(tl_to_int 00000009)" = "9" ]  || exit 1   # TL 9 (last where hex==dec)
  [ "$(tl_to_int 0000000A)" = "10" ] || exit 1   # TL 10: ::int would ERROR
  [ "$(tl_to_int 00000010)" = "16" ] || exit 1   # TL 16: ::int would yield 10
  [ "$(tl_to_int 000000FF)" = "255" ] || exit 1
  [ -z "$(tl_to_int "")" ]       || exit 1        # empty -> empty
  [ -z "$(tl_to_int "garbage")" ] || exit 1       # non-hex -> empty
  exit 0
' || tl_rc=$?
assert_eq "su #168: tl_to_int decodes hex timelines (10->10, 16->16, not ::int)" "0" "${tl_rc}"
rm -f "${SCRIPT_DIR}/.tl_render.sh" "${SCRIPT_DIR}/.tl_fn.sh"

# --- #169: split-brain re-asserts the write selector to the highest-TL primary ---
# Under the default action=log the handler must keep re-asserting the write
# selector (so an ArgoCD sync re-applying the chart's hardcoded pod-0 selector
# during a split-brain window cannot strand writes), and it must re-assert
# toward the HIGHEST-timeline live primary -- the legitimate post-failover node,
# the same survivor the fence path keeps -- not a stale lower-timeline one.
assert_contains "su #169: log-mode split-brain re-asserts highest-timeline selector" "${su_cm}" "re-asserting write selector to highest-timeline primary"
# behavioral unit test of handle_split_brain extracted from the rendered script.
# timeout is stubbed to run its command; psql returns a per-host timeline|LSN so
# the real survivor selection (tl_to_int + lsn_gt, also sourced) is exercised.
printf '%s' "${su_cm}" | python3 -c "import sys,yaml; sys.stdout.write(yaml.safe_load(sys.stdin)['data']['service-updater.sh'])" > "${SCRIPT_DIR}/.sb_render.sh"
sed -n '/^handle_split_brain() {/,/^}/p' "${SCRIPT_DIR}/.sb_render.sh" >  "${SCRIPT_DIR}/.sb_fn.sh"
sed -n '/^tl_to_int() {/,/^}/p'          "${SCRIPT_DIR}/.sb_render.sh" >> "${SCRIPT_DIR}/.sb_fn.sh"
sed -n '/^lsn_gt() {/,/^}/p'             "${SCRIPT_DIR}/.sb_render.sh" >> "${SCRIPT_DIR}/.sb_fn.sh"
sb_rc=0
bash -c '
  source "'"${SCRIPT_DIR}"'/.sb_fn.sh"
  PRIMARY_COUNT=2
  REPMGR_PASSWORD=x REPMGR_USER=r REPMGR_DB=d NAMESPACE=ns
  timeout() { shift; "$@"; }            # drop the duration, run the command
  update_pod_role_labels() { :; }
  kubectl() { :; }                      # no-op the fence deletion path
  # pod-1 is on the higher timeline (the legitimate post-failover primary)
  hi_tl() { local h=""; while [ $# -gt 0 ]; do [ "$1" = "-h" ] && h="$2"; shift; done
            case "$h" in *-0.*) echo "00000003|3/10";; *-1.*) echo "00000004|4/20";; *) echo "";; esac; }

  # log mode re-asserts toward the highest-TL primary (pod-1), even though the
  # stale pod-0 is also live and is where LAST_MASTER/the selector points
  psql() { hi_tl "$@"; }
  SELECTED=""; update_service_selector() { SELECTED="$1"; }
  SPLIT_BRAIN_ACTION=log LAST_MASTER=test-pg-0 PRIMARY_NODES="test-pg-0.h.ns test-pg-1.h.ns" handle_split_brain >/dev/null 2>&1
  [ "$SELECTED" = "test-pg-1" ] || exit 1

  # order independence: same winner when pod-1 is listed first
  SELECTED=""; update_service_selector() { SELECTED="$1"; }
  SPLIT_BRAIN_ACTION=log LAST_MASTER="" PRIMARY_NODES="test-pg-1.h.ns test-pg-0.h.ns" handle_split_brain >/dev/null 2>&1
  [ "$SELECTED" = "test-pg-1" ] || exit 1

  # no readable timeline anywhere -> passive (selector untouched)
  psql() { echo ""; }
  SELECTED=""; update_service_selector() { SELECTED="$1"; }
  SPLIT_BRAIN_ACTION=log LAST_MASTER=test-pg-0 PRIMARY_NODES="test-pg-0.h.ns test-pg-1.h.ns" handle_split_brain >/dev/null 2>&1
  [ -z "$SELECTED" ] || exit 1

  # fence mode selects the same highest-TL survivor
  psql() { hi_tl "$@"; }
  SELECTED=""; update_service_selector() { SELECTED="$1"; }
  SPLIT_BRAIN_ACTION=fence LAST_MASTER=test-pg-0 PRIMARY_NODES="test-pg-0.h.ns test-pg-1.h.ns" handle_split_brain >/dev/null 2>&1
  [ "$SELECTED" = "test-pg-1" ] || exit 1
  exit 0
' || sb_rc=$?
assert_eq "su #169: split-brain selects highest-TL primary (log re-asserts, fence too)" "0" "${sb_rc}"
rm -f "${SCRIPT_DIR}/.sb_render.sh" "${SCRIPT_DIR}/.sb_fn.sh"

# --- Durable primary marker (#125) ---
# The service-updater records the highest-timeline primary in a ConfigMap so a
# node booting first under OrderedReady can tell it is stale even when the real
# primary is not up yet.
assert_contains "su #125: reads the durable marker" "${su_cm}" "read_marker()"
assert_contains "su #125: writes the durable marker" "${su_cm}" "write_marker()"
assert_contains "su #125: refuses a primary below the recorded highwater timeline" "${su_cm}" "below the recorded primary"
# statefulset passes the marker name to the service-updater container
sts_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "su #125: PRIMARY_MARKER env propagated" "${sts_repmgr}" "name: PRIMARY_MARKER"
assert_contains "su #125: marker name is <fullname>-primary" "${sts_repmgr}" "test-pg-primary"
# #170: the entrypoint stale-primary guard (postgresql container) reads the
# marker to gate its empty-data settle, so the marker env must reach the
# postgresql container itself -- not just the service-updater sidecar.
pg_cont=$(printf '%s\n' "${sts_repmgr}" | awk '/^        - name: postgresql$/{f=1; next} f && /^        - name: /{exit} f{print}')
assert_contains "#170: postgresql container gets PRIMARY_MARKER (guard reads marker)" "${pg_cont}" "name: PRIMARY_MARKER"
assert_contains "#170: postgresql container gets NAMESPACE" "${pg_cont}" "name: NAMESPACE"

# --- #172: startupProbe absorbs the stale-primary guard's startup latency ---
# The guard runs before postgres opens (up to ~147s with the REPMGR_NODE_COUNT
# fallback) plus crash-recovery WAL replay, which can exceed the ~130s liveness
# budget and crash-loop the pod. A startupProbe suspends liveness/readiness until
# the first success, so a slow start is not killed mid-guard.
assert_contains "#172: postgresql container has a startupProbe" "${pg_cont}" "startupProbe:"
startup_block=$(printf '%s\n' "${pg_cont}" | awk '/startupProbe:/{f=1; next} f && /Probe:/{exit} f{print}')
assert_contains "#172: startupProbe probes pg_isready" "${startup_block}" "pg_isready"
# the startup budget (periodSeconds x failureThreshold) must comfortably cover
# the worst-case guard latency plus crash recovery
startup_period=$(printf '%s\n' "${startup_block}" | awk '/periodSeconds:/{print $2; exit}')
startup_threshold=$(printf '%s\n' "${startup_block}" | awk '/failureThreshold:/{print $2; exit}')
startup_budget=$(( ${startup_period:-0} * ${startup_threshold:-0} ))
[ "$startup_budget" -ge 300 ] && budget172=ok || budget172="budget=${startup_budget}s (period=${startup_period:-?} threshold=${startup_threshold:-?})"
assert_eq "#172: startup budget covers guard + crash-recovery worst case (>=300s)" "ok" "${budget172}"
# gated on enabled: disabling removes the startupProbe but keeps liveness
startup_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set postgresql.startupProbe.enabled=false --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#172: startupProbe omitted when disabled" "${startup_off}" "startupProbe:"
assert_contains "#172: livenessProbe still present when startupProbe disabled" "${startup_off}" "livenessProbe:"

# #176: the #125 full-restart guard depends on OrderedReady (lowest-ordinal pod
# boots first and ALONE). Pin/assert it so switching to Parallel -- which would
# silently stop exercising the guard -- fails the suite instead.
assert_contains "#176: statefulset pins podManagementPolicy OrderedReady (#125 depends on it)" "${sts_repmgr}" "podManagementPolicy: OrderedReady"

# rbac grants configmap access for the marker
rbac_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "su #125: rbac grants configmaps verbs" "${rbac_repmgr}" "configmaps"
# the marker is runtime-owned (service-updater create/apply), not a helm
# template, so helm upgrade / ArgoCD sync cannot reset the highwater
assert_contains "su #125: marker written at runtime via kubectl" "${su_cm}" "kubectl create configmap"

# --- #171/#173/#174: lone-primary marker guard hardening (service-updater) ---
# read_marker must distinguish a corrupt marker from an absent one (#174), and
# evaluate_lone_primary must fail closed on an unreadable timeline even with no
# marker (#173) and on an equal-timeline different-node split-brain (#171),
# while still allowing a legitimate higher-timeline failover through.
printf '%s' "${su_cm}" | python3 -c "import sys,yaml; sys.stdout.write(yaml.safe_load(sys.stdin)['data']['service-updater.sh'])" > "${SCRIPT_DIR}/.su_render.sh"
sed -n '/^read_marker() {/,/^}/p'           "${SCRIPT_DIR}/.su_render.sh" >  "${SCRIPT_DIR}/.su_fn.sh"
sed -n '/^evaluate_lone_primary() {/,/^}/p' "${SCRIPT_DIR}/.su_render.sh" >> "${SCRIPT_DIR}/.su_fn.sh"

# read_marker: a present-but-corrupt timeline is flagged, not aliased to absent
rm_rc=0
bash -c '
  source "'"${SCRIPT_DIR}"'/.su_fn.sh"
  PRIMARY_MARKER=m NAMESPACE=ns
  kubectl() { echo "test-pg-1|5"; }                 # valid marker
  read_marker
  [ "$MARKER_PRIMARY" = "test-pg-1" ] && [ "$MARKER_TL" = "5" ] && [ "$MARKER_MALFORMED" = false ] || exit 1
  kubectl() { echo "test-pg-1|garbage"; }           # corrupt timeline
  read_marker
  [ "$MARKER_TL" = "0" ] && [ "$MARKER_MALFORMED" = true ] || exit 2
  kubectl() { return 1; }                           # absent (kubectl get fails)
  read_marker
  [ "$MARKER_TL" = "0" ] && [ "$MARKER_MALFORMED" = false ] || exit 3
  exit 0
' || rm_rc=$?
assert_eq "su #174: read_marker flags a corrupt marker distinct from absent" "0" "${rm_rc}"

# evaluate_lone_primary: fail-closed vs proceed decisions
elp_rc=0
bash -c '
  source "'"${SCRIPT_DIR}"'/.su_fn.sh"
  PRIMARY_MARKER=m NAMESPACE=ns
  check() { evaluate_lone_primary >/dev/null 2>&1; [ "$skip_select" = "$1" ] || { echo "case $2: skip=$skip_select want $1"; exit 1; }; }
  # #173: unreadable timeline with NO marker must still skip (bug: it selected)
  MARKER_MALFORMED=false MARKER_TL=0 MARKER_PRIMARY="" CURRENT_MASTER=test-pg-0 current_tl=""; check true u173
  # #174: corrupt marker -> skip even with a valid current_tl
  MARKER_MALFORMED=true MARKER_TL=0 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=7; check true u174
  # #171: same TL, different node -> skip (equal-timeline split-brain)
  MARKER_MALFORMED=false MARKER_TL=5 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=5; check true u171
  # below marker -> skip (existing #125 behavior preserved)
  MARKER_MALFORMED=false MARKER_TL=5 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=4; check true below
  # legitimate higher-TL failover -> proceed
  MARKER_MALFORMED=false MARKER_TL=5 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=6; check false advance
  # same node re-asserting its own TL -> proceed
  MARKER_MALFORMED=false MARKER_TL=5 MARKER_PRIMARY=test-pg-0 CURRENT_MASTER=test-pg-0 current_tl=5; check false reassert
  # valid timeline, no marker yet (bootstrap) -> proceed
  MARKER_MALFORMED=false MARKER_TL=0 MARKER_PRIMARY="" CURRENT_MASTER=test-pg-0 current_tl=3; check false bootstrap
  # #176/#168: the comparison must stay numeric past TL 10 (0x0A), where the old
  # ::int-on-hex bug broke. tl_to_int already feeds decimals here; assert the
  # -lt/-gt/-eq arithmetic is right above the boundary too.
  MARKER_MALFORMED=false MARKER_TL=10 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=16; check false hi_advance
  MARKER_MALFORMED=false MARKER_TL=16 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=10; check true  hi_below
  MARKER_MALFORMED=false MARKER_TL=10 MARKER_PRIMARY=test-pg-1 CURRENT_MASTER=test-pg-0 current_tl=10; check true  hi_sametl_diffnode
  exit 0
' || elp_rc=$?
assert_eq "su #171/#173: lone-primary guard fails closed on unverified/split-brain, proceeds on valid failover" "0" "${elp_rc}"
rm -f "${SCRIPT_DIR}/.su_render.sh" "${SCRIPT_DIR}/.su_fn.sh"

# #138: LAST_MASTER must seed from the live write-Service selector, not "". The
# sidecar's process state doesn't survive a restart, so an empty seed made the
# first tick treat the existing primary as a "master change" and spuriously
# rollout-restart pgpool (severing pooled connections) on every install/upgrade/
# rolling restart. Seeding from the selector restarts pgpool only on a real
# transition.
assert_contains "#138: LAST_MASTER seeded from service selector" "${su_cm}" 'LAST_MASTER=$(kubectl get service'
assert_not_contains "#138: LAST_MASTER not initialized empty" "${su_cm}" 'LAST_MASTER=""'
# behavioral: source the extracted seeding line with kubectl stubbed
seed_line=$(printf '%s\n' "${su_cm}" | grep -m1 'LAST_MASTER=$(kubectl get service' | sed 's/^[[:space:]]*//')
seed_file=$(mktemp); printf '%s\n' "${seed_line}" > "${seed_file}"
seed_present=$(MASTER_SERVICE=svc NAMESPACE=ns bash -c 'kubectl() { echo pg-repmgr-1; }; source '"${seed_file}"'; printf "%s" "$LAST_MASTER"')
assert_eq "#138: seeds to the current selector pod" "pg-repmgr-1" "${seed_present}"
seed_absent=$(MASTER_SERVICE=svc NAMESPACE=ns bash -c 'kubectl() { return 1; }; source '"${seed_file}"'; printf "%s" "$LAST_MASTER"')
assert_eq "#138: seeds empty when no selector exists yet" "" "${seed_absent}"
rm -f "${seed_file}"

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

# pgBackRest: idle sidecar present in statefulset (exec target for CronJobs)
pgbackrest_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1)
assert_contains "pgbackrest: sidecar present" "${pgbackrest_sts}" "name: pgbackrest"
assert_contains "pgbackrest: PGBACKREST_STANZA env var" "${pgbackrest_sts}" "PGBACKREST_STANZA"
assert_contains "pgbackrest: S3 key env var" "${pgbackrest_sts}" "PGBACKREST_REPO1_S3_KEY"
assert_contains "pgbackrest: config volume mount" "${pgbackrest_sts}" "pgbackrest-config"
assert_contains "pgbackrest: sidecar runs sleep infinity" "${pgbackrest_sts}" "exec sleep infinity"

# pgBackRest: sidecar must NOT carry per-fire schedule envs (scheduling lives
# in CronJobs now, not in the sidecar).
assert_not_contains "pgbackrest: sidecar has no FULL_SCHEDULE env" "${pgbackrest_sts}" "FULL_SCHEDULE"
assert_not_contains "pgbackrest: sidecar has no DIFF_SCHEDULE env" "${pgbackrest_sts}" "DIFF_SCHEDULE"
assert_not_contains "pgbackrest: scripts ConfigMap is gone" "${pgbackrest}" "pgbackrest-scheduler.sh"
assert_not_contains "pgbackrest: scripts volume mount removed" "${pgbackrest_sts}" "pgbackrest-scripts"

# pgBackRest: configmap retention values
assert_contains "pgbackrest: retention full in configmap" "${pgbackrest}" "repo1-retention-full=4"
assert_contains "pgbackrest: retention diff in configmap" "${pgbackrest}" "repo1-retention-diff=14"

# pgBackRest: configmap s3 region and prefix
assert_contains "pgbackrest: s3 region in configmap" "${pgbackrest}" "repo1-s3-region=eu-central-1"
assert_contains "pgbackrest: s3 path prefix in configmap" "${pgbackrest}" "/pgbackrest/"

# pgBackRest: S3 secret references on sidecar
assert_contains "pgbackrest: sidecar S3 key secret ref" "${pgbackrest_sts}" "s3-backup-creds"
assert_contains "pgbackrest: sidecar S3 key ref key" "${pgbackrest_sts}" "access-key-id"
assert_contains "pgbackrest: sidecar S3 secret key ref key" "${pgbackrest_sts}" "secret-access-key"

# pgBackRest: resource limits on sidecar
pgbackrest_sidecar=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1 | sed -n '/^        - name: pgbackrest$/,/^        - name:/p')
assert_contains "pgbackrest: sidecar cpu limit" "${pgbackrest_sidecar}" "cpu: 1000m"
assert_contains "pgbackrest: sidecar memory limit" "${pgbackrest_sidecar}" "memory: 1Gi"
assert_contains "pgbackrest: sidecar cpu request" "${pgbackrest_sidecar}" "cpu: 100m"
assert_contains "pgbackrest: sidecar memory request" "${pgbackrest_sidecar}" "memory: 256Mi"

# pgBackRest: shared unix socket volume (pg-run emptyDir)
assert_contains "pgbackrest: pg-run emptyDir volume" "${pgbackrest_sts}" "name: pg-run"
pgbackrest_pg_container=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1 | sed -n '/name: postgresql$/,/^        - name:/p')
assert_contains "pgbackrest: postgresql mounts pg-run" "${pgbackrest_pg_container}" "pg-run"
assert_contains "pgbackrest: sidecar mounts pg-run" "${pgbackrest_sidecar}" "pg-run"

# #142: pgbackrest requires repmgr -- the pgbackrest binary and archive_command
# run in the postgresql container, which is the plain postgres image (no
# pgbackrest) in standalone mode, so WAL archiving would fail every segment and
# backups time out. Previously the chart rendered a standalone pgbackrest
# sidecar that could never archive; it now fails fast at render time.
pgbackrest_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.test \
  --set pgbackrest.s3.bucket=test \
  --set pgbackrest.existingSecret.name=test-secret \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 \
  --show-only templates/statefulset.yaml 2>&1) && pgbr_sa_rc=0 || pgbr_sa_rc=$?
assert_eq "#142: pgbackrest without repmgr fails fast" "1" "${pgbr_sa_rc}"
assert_contains "#142: error names the repmgr requirement" "${pgbackrest_standalone}" "pgbackrest.enabled requires repmgr.enabled=true"

# pgBackRest: coexists with repmgr
assert_contains "pgbackrest+repmgr: sidecar present" "${pgbackrest_sts}" "name: pgbackrest"
assert_contains "pgbackrest+repmgr: repmgrd present" "${pgbackrest_sts}" "name: repmgrd"
assert_contains "pgbackrest+repmgr: service-updater present" "${pgbackrest_sts}" "name: service-updater"

# pgBackRest: CronJobs (one per backup type) drive scheduling.
pgbackrest_cron=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-cronjob.yaml 2>&1)
assert_contains "pgbackrest: full CronJob renders" "${pgbackrest_cron}" "name: test-pg-pgbackrest-full"
assert_contains "pgbackrest: diff CronJob renders" "${pgbackrest_cron}" "name: test-pg-pgbackrest-diff"
assert_contains "pgbackrest: CronJob uses kubectl image" "${pgbackrest_cron}" "alpine/k8s"
assert_contains "pgbackrest: CronJob runs as service account" "${pgbackrest_cron}" "serviceAccountName: test-pg-pgbackrest"
assert_contains "pgbackrest: CronJob resolves primary via endpoints" "${pgbackrest_cron}" "endpoints"
assert_contains "pgbackrest: CronJob execs into sidecar" "${pgbackrest_cron}" "exec \"\$PRIMARY\" -c pgbackrest"
assert_contains "pgbackrest: CronJob ensures stanza" "${pgbackrest_cron}" "stanza-create"
assert_contains "pgbackrest: CronJob runs backup" "${pgbackrest_cron}" "type=\"\$BACKUP_TYPE\" backup"
assert_contains "pgbackrest: CronJob concurrency Forbid" "${pgbackrest_cron}" "concurrencyPolicy: Forbid"
# assert_contains uses `grep` (regex), so escape the cron-spec stars.
assert_contains "pgbackrest: full CronJob carries full schedule" "${pgbackrest_cron}" 'schedule: "0 1 \* \* 0"'
assert_contains "pgbackrest: diff CronJob carries diff schedule" "${pgbackrest_cron}" 'schedule: "0 1 \* \* 1-6"'

# pgBackRest: RBAC for CronJob exec access.
pgbackrest_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-rbac.yaml 2>&1)
assert_contains "pgbackrest rbac: ServiceAccount renders" "${pgbackrest_rbac}" "kind: ServiceAccount"
assert_contains "pgbackrest rbac: Role renders" "${pgbackrest_rbac}" "kind: Role"
assert_contains "pgbackrest rbac: RoleBinding renders" "${pgbackrest_rbac}" "kind: RoleBinding"
assert_contains "pgbackrest rbac: pods/exec verb create" "${pgbackrest_rbac}" "pods/exec"
assert_contains "pgbackrest rbac: endpoints get verb" "${pgbackrest_rbac}" "endpoints"
# #134: pods get and pods/exec create must be scoped by resourceNames to the
# StatefulSet's deterministic pod names, not left namespace-wide (an unscoped
# Role lets a leaked SA token exec into every pod in the namespace). All three
# rules (endpoints, pods, pods/exec) carry resourceNames after the fix.
pgbackrest_rn_count=$(printf '%s\n' "${pgbackrest_rbac}" | grep -c 'resourceNames:')
assert_eq "pgbackrest rbac: endpoints+pods+pods/exec all resourceName-scoped (#134)" "3" "${pgbackrest_rn_count}"
assert_contains "pgbackrest rbac: scoped to pod test-pg-0 (#134)" "${pgbackrest_rbac}" "\"test-pg-0\""
assert_contains "pgbackrest rbac: scoped to pod test-pg-1 (#134)" "${pgbackrest_rbac}" "\"test-pg-1\""
pgbackrest_rbac_n=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set postgresql.replicaCount=2 --show-only templates/pgbackrest-rbac.yaml 2>&1)
assert_contains "pgbackrest rbac: replicaCount widens scope to test-pg-2 (#134)" "${pgbackrest_rbac_n}" "\"test-pg-2\""

# pgBackRest: enabling pgbackrest must auto-inject WAL archive settings into
# postgresql.conf. Without these, postgres rejects backups with
# "archive_mode must be enabled".
pgbackrest_pgconf=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "pgbackrest: postgresql configmap renders" "${pgbackrest_pgconf}" "test-pg-postgresql-config"
assert_contains "pgbackrest: archive_mode = on" "${pgbackrest_pgconf}" "archive_mode = on"
assert_contains "pgbackrest: archive_command uses stanza" "${pgbackrest_pgconf}" "archive_command = 'pgbackrest --stanza=db archive-push %p'"
assert_contains "pgbackrest: wal_level replica" "${pgbackrest_pgconf}" "wal_level = replica"

# Statefulset must mount the postgresql-config volume even when
# postgresql.configuration is empty, so the archive snippet is delivered.
assert_contains "pgbackrest: postgresql-config volume mounted" "${pgbackrest_sts}" "mountPath: /etc/postgresql/conf.d"
assert_contains "pgbackrest: postgresql-config volume present" "${pgbackrest_sts}" "name: postgresql-config"
assert_contains "pgbackrest: include_dir wired in postStart" "${pgbackrest_sts}" "include_dir = '/etc/postgresql/conf.d'"

# #132: disabling pgbackrest after it was enabled at install must neutralize the
# archive settings the repmgr image entrypoint persisted into PGDATA's
# postgresql.conf. Otherwise archive_mode stays on with a now-failing
# archive_command (pgbackrest config + S3 creds gone), pg_wal is never recycled,
# and the data PVC fills until the postmaster PANICs. The strip lives in
# setup-config and must be independent of the include_dir branch.
pgbr_disable_setup=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set pgbackrest.enabled=false --show-only templates/statefulset.yaml 2>&1 | sed -n '/name: setup-config/,/volumeMounts:/p')
assert_contains "pgbackrest disable: setup-config strips entrypoint archive config (#132)" "${pgbr_disable_setup}" "archive-push %p"
assert_contains "pgbackrest disable: setup-config neutralizes archive_mode (#132)" "${pgbr_disable_setup}" "grep -v -e '^archive_mode = on"
# enabled: setup-config must NOT strip the live archive config
pgbr_enabled_setup=$(printf '%s\n' "${pgbackrest_sts}" | sed -n '/name: setup-config/,/volumeMounts:/p')
assert_not_contains "pgbackrest enabled: setup-config keeps archive config (#132)" "${pgbr_enabled_setup}" "archive-push %p"
# disable while postgresql.configuration is still set: include_dir is kept (conf.d
# still active) AND the archive strip still runs -- the two must be independent
pgbr_disable_cfg_setup=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set pgbackrest.enabled=false --set postgresql.configuration.max_connections=200 --show-only templates/statefulset.yaml 2>&1 | sed -n '/name: setup-config/,/volumeMounts:/p')
assert_contains "pgbackrest disable + config: include_dir kept (#132)" "${pgbr_disable_cfg_setup}" 'echo "include_dir'
assert_contains "pgbackrest disable + config: archive strip still runs (#132)" "${pgbr_disable_cfg_setup}" "archive-push %p"

# pgBackRest: not rendered when disabled (default). Matches the rendered
# resource name prefix (test-pg-pgbackrest) rather than the bare "pgbackrest"
# substring so it targets actual resources, not explanatory comments (#132).
pgbackrest_disabled=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" 2>&1)
assert_not_contains "pgbackrest disabled: no configmap" "${pgbackrest_disabled}" "test-pg-pgbackrest"
assert_not_contains "pgbackrest disabled: no sidecar" "${pgbackrest_disabled}" "name: pgbackrest"
assert_not_contains "pgbackrest disabled: no CronJob" "${pgbackrest_disabled}" "kind: CronJob"

# --- Standalone replica validation Tests ---

# Test: rendering fails fast when repmgr is disabled with replicaCount > 0
standalone_replicas=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=1 2>&1) && standalone_replicas_rc=0 || standalone_replicas_rc=$?
assert_eq "standalone replicas: render fails when repmgr disabled with replicaCount > 0" "1" "${standalone_replicas_rc}"
assert_contains "standalone replicas: error names the constraint" "${standalone_replicas}" "requires repmgr.enabled=true"

# Test: rendering fails with default replicaCount (1) when only repmgr is disabled
standalone_default=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false 2>&1) && standalone_default_rc=0 || standalone_default_rc=$?
assert_eq "standalone replicas: render fails with default replicaCount when repmgr disabled" "1" "${standalone_default_rc}"

# Test: standalone with replicaCount=0 still renders
standalone_ok=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 2>&1) && standalone_ok_rc=0 || standalone_ok_rc=$?
assert_eq "standalone replicas: render passes with replicaCount=0" "0" "${standalone_ok_rc}"

# --- #133: repmgr-mode PostgreSQL major pinning Tests ---

# In repmgr mode the server runs from the repmgr image, which bundles exactly
# one PG major; postgresql.majorVersion/image.tag overrides cannot change it.
# The chart must fail fast on a mismatch instead of crash-looping the extension
# init container (extensions on) or silently running the wrong major (off).
major_mismatch=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.majorVersion=17 \
  --set postgresql.image.tag=pg17 2>&1) && major_mismatch_rc=0 || major_mismatch_rc=$?
assert_eq "major pin: render fails when postgresql.majorVersion != repmgr image major (#133)" "1" "${major_mismatch_rc}"
assert_contains "major pin: error names both values (#133)" "${major_mismatch}" "does not match repmgr.image.majorVersion"

# default (18 == 18) renders
major_default=$(helm template test-pg "${CHART_DIR}" --show-only templates/statefulset.yaml 2>&1) && major_default_rc=0 || major_default_rc=$?
assert_eq "major pin: default 18==18 renders (#133)" "0" "${major_default_rc}"

# a matched rebump to a hypothetical PG17 repmgr image renders (forward-compatible)
major_rebump=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.majorVersion=17 \
  --set repmgr.image.majorVersion=17 \
  --show-only templates/statefulset.yaml 2>&1) && major_rebump_rc=0 || major_rebump_rc=$?
assert_eq "major pin: matched repmgr.image.majorVersion rebump renders (#133)" "0" "${major_rebump_rc}"

# the pin only applies in repmgr mode: standalone may run any major
major_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 \
  --set postgresql.majorVersion=17 \
  --set postgresql.image.tag=pg17 \
  --show-only templates/statefulset.yaml 2>&1) && major_standalone_rc=0 || major_standalone_rc=$?
assert_eq "major pin: standalone is unconstrained by repmgr image major (#133)" "0" "${major_standalone_rc}"

# --- Primary Service selector Tests ---

# Test: selector bootstraps to pod-0 when no live Service exists (lookup is
# empty under helm template; in-cluster preservation is covered by the
# upgrade-after-failover assertions in test-failover.sh)
svc_selector=$(helm template test-pg "${CHART_DIR}" --show-only templates/service.yaml 2>&1)
assert_contains "service selector: bootstraps to pod-0 with repmgr" "${svc_selector}" "statefulset.kubernetes.io/pod-name: test-pg-0"

svc_selector_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 \
  --show-only templates/service.yaml 2>&1)
assert_contains "service selector: pod-0 in standalone mode" "${svc_selector_standalone}" "statefulset.kubernetes.io/pod-name: test-pg-0"

# --- Primary discovery and repmgrd pre-register Tests ---

# The StatefulSet runs replicaCount + 1 pods (ordinals 0..replicaCount), so
# the postStart discovery loop and the repmgrd peer scan must both reach
# ordinal replicaCount; replicaCount=2 disambiguates from the old bounds
discovery_sts=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.replicaCount=2 \
  --set postgresql.lifecycle.postStart.additionalCommands="echo noop" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "discovery loops: scan ordinals 0..replicaCount" "${discovery_sts}" "seq 0 2"
assert_not_contains "postStart discovery: off-by-one bound is gone" "${discovery_sts}" "seq 0 1)"
assert_not_contains "repmgrd peer scan: hardcoded seq 0 9 is gone" "${discovery_sts}" "seq 0 9"
assert_not_contains "repmgrd role check: hardcoded postgres user is gone" "${discovery_sts}" "psql -h 127.0.0.1 -U postgres"
assert_contains "repmgrd role check: uses repmgr credentials" "${discovery_sts}" 'psql -h 127.0.0.1 -U "${REPMGR_USER}"'
assert_contains "repmgrd backfill: node_id read from repmgr.conf" "${discovery_sts}" 'awk -F='
assert_not_contains "repmgrd backfill: baked-in node-id convention is gone" "${discovery_sts}" 'ORDINAL + 1000'

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

# --- imagePullSecrets Tests ---

# Test: no imagePullSecrets rendered by default
assert_not_contains "default: no imagePullSecrets in minimal render" "${minimal}" "imagePullSecrets"
assert_not_contains "default: no imagePullSecrets in full render" "${full}" "imagePullSecrets"

# Test: imagePullSecrets propagates to statefulset, pgpool, exporter and
# backup cronjob pod templates (one occurrence each)
ips_full=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set 'imagePullSecrets[0].name=registry-cred' \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=test-backups \
  --set backup.existingSecret.name=s3-backup-creds \
  2>&1)
ips_count=$(printf '%s' "${ips_full}" | grep -c "name: registry-cred" || true)
assert_eq "imagePullSecrets: statefulset, pgpool, exporter, backup all carry the secret" "4" "${ips_count}"

# Test: imagePullSecrets propagates to both pgbackrest cronjob pod templates
ips_pgbackrest=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set 'imagePullSecrets[0].name=registry-cred' \
  --show-only templates/pgbackrest-cronjob.yaml 2>&1)
ips_pgb_count=$(printf '%s' "${ips_pgbackrest}" | grep -c "name: registry-cred" || true)
assert_eq "imagePullSecrets: both pgbackrest cronjobs carry the secret" "2" "${ips_pgb_count}"

# --- priorityClassName Tests ---

# Test: no priorityClassName rendered by default
assert_not_contains "default: no priorityClassName in minimal render" "${minimal}" "priorityClassName"
assert_not_contains "default: no priorityClassName in full render" "${full}" "priorityClassName"

# Test: per-component priorityClassName renders on each pod template
prio=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set postgresql.priorityClassName=db-critical \
  --set pgpool.priorityClassName=pool-prio \
  --set prometheusExporter.priorityClassName=exp-prio \
  --set backup.enabled=true \
  --set backup.priorityClassName=backup-prio \
  --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=test-backups \
  --set backup.existingSecret.name=s3-backup-creds \
  2>&1)
assert_contains "priorityClassName: statefulset carries db-critical" "${prio}" "priorityClassName: db-critical"
assert_contains "priorityClassName: pgpool carries pool-prio" "${prio}" "priorityClassName: pool-prio"
assert_contains "priorityClassName: exporter carries exp-prio" "${prio}" "priorityClassName: exp-prio"
assert_contains "priorityClassName: backup cronjob carries backup-prio" "${prio}" "priorityClassName: backup-prio"

# Test: pgbackrest cronjob priorityClassName renders on both cronjobs
prio_pgbackrest=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set pgbackrest.cronjob.priorityClassName=pgb-prio \
  --show-only templates/pgbackrest-cronjob.yaml 2>&1)
prio_pgb_count=$(printf '%s' "${prio_pgbackrest}" | grep -c "priorityClassName: pgb-prio" || true)
assert_eq "priorityClassName: both pgbackrest cronjobs carry pgb-prio" "2" "${prio_pgb_count}"

# --- PostgreSQL Liveness Probe failureThreshold Tests ---

# Test: with default values the postgresql container liveness probe uses
# failureThreshold 10 while readiness stays at 6
probes_default=$(helm template test-pg "${CHART_DIR}" \
  --show-only templates/statefulset.yaml 2>&1)
# the manifest contains a second livenessProbe (service-updater sidecar);
# the postgresql container's block renders first, so keep only that one
pg_liveness=$(printf '%s' "${probes_default}" \
  | sed -n '/livenessProbe:/,/failureThreshold:/p' \
  | sed -n '1,/failureThreshold:/p')
assert_contains "liveness probe: default failureThreshold is 10" "${pg_liveness}" "failureThreshold: 10"
pg_readiness=$(printf '%s' "${probes_default}" \
  | sed -n '/readinessProbe:/,/failureThreshold:/p')
assert_contains "readiness probe: default failureThreshold stays 6" "${pg_readiness}" "failureThreshold: 6"

# --- Backup Retention Guard Tests (issue #21) ---

# Test: backup script verifies a recent backup exists before retention cleanup
guard_configmap=$(helm template test-pg "${CHART_DIR}" \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.test \
  --set backup.s3.bucket=test \
  --set backup.existingSecret.name=test-secret \
  --show-only templates/backup-configmap.yaml 2>&1)
assert_contains "backup guard: --newer-than check present" "${guard_configmap}" 'mc find "s3/${S3_BUCKET}/${S3_PREFIX}/" --newer-than "${RETENTION_DAYS}d"'
assert_contains "backup guard: aborts when no recent backup found" "${guard_configmap}" "aborting retention cleanup"
assert_contains "backup guard: error message goes to stderr" "${guard_configmap}" 'ERROR: No backup newer than ${RETENTION_DAYS} days'

# Test: the guard runs before the retention deletion in the script
guard_line=$(printf '%s\n' "${guard_configmap}" | grep -n -- '--newer-than' | head -1 | cut -d: -f1)
delete_line=$(printf '%s\n' "${guard_configmap}" | grep -n -- '--older-than' | head -1 | cut -d: -f1)
guard_before_delete="false"
if [ -n "${guard_line}" ] && [ -n "${delete_line}" ] && [ "${guard_line}" -lt "${delete_line}" ]; then
  guard_before_delete="true"
fi
assert_eq "backup guard: recent-backup check precedes retention deletion" "true" "${guard_before_delete}"

# --- Prometheus exporter replication lag metrics Tests ---

# Test: exporter configmap carries the pg_wal_replication custom query group.
# The group must NOT be named pg_replication: the exporter's built-in
# replication collector already registers pg_replication_lag_seconds, and a
# custom metric under the same name 500s every scrape (help-text mismatch).
exporter_cm=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --show-only templates/prometheus-exporter-configmap.yaml 2>&1)
assert_contains "exporter cm: queries.yaml key present" "${exporter_cm}" "queries.yaml: |"
assert_contains "exporter cm: pg_wal_replication query group present" "${exporter_cm}" "pg_wal_replication:"
assert_not_contains "exporter cm: no collision with built-in pg_replication group" "${exporter_cm}" "^    pg_replication:"
assert_contains "exporter cm: in_recovery gauge present" "${exporter_cm}" "pg_is_in_recovery()::int AS in_recovery"
assert_contains "exporter cm: receive/replay byte lag NULL-safe on primaries" "${exporter_cm}" "COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()), 0)"
assert_contains "exporter cm: byte lag metric declared" "${exporter_cm}" "receive_replay_lag_bytes:"
gauge_count=$(printf '%s' "${exporter_cm}" | grep -c 'usage: "GAUGE"' || true)
assert_eq "exporter cm: two GAUGE metrics in pg_wal_replication" "2" "${gauge_count}"

# Test: exporter container loads the custom queries file from the configmap mount
assert_contains "exporter: extend.query-path flag wired" "${full}" "extend.query-path=/config/queries.yaml"
exporter_deploy=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
config_mounts=$(printf '%s' "${exporter_deploy}" | grep -c "mountPath: /config" || true)
assert_eq "exporter: configmap mounted in init and exporter containers" "2" "${config_mounts}"

# Test: minimal values still render no exporter queries
assert_not_contains "minimal: no replication query group" "${minimal}" "pg_wal_replication:"

# --- Zone-Aware Pod Anti-Affinity Tests ---

# Test: default statefulset affinity keeps the required hostname term and
# adds a preferred zone term
sts_zone=$(helm template test-pg "${CHART_DIR}" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "zone affinity: required hostname term present" "${sts_zone}" "topologyKey: kubernetes.io/hostname"
assert_contains "zone affinity: hostname term is required" "${sts_zone}" "requiredDuringSchedulingIgnoredDuringExecution"
assert_contains "zone affinity: preferred zone term present" "${sts_zone}" "topologyKey: topology.kubernetes.io/zone"
assert_contains "zone affinity: zone term is preferred" "${sts_zone}" "preferredDuringSchedulingIgnoredDuringExecution"
assert_contains "zone affinity: zone term weight is 100" "${sts_zone}" "weight: 100"

# Test: user-supplied postgresql.affinity replaces the default block
# wholesale (no default hostname or zone terms remain)
sts_custom_aff=$(helm template test-pg "${CHART_DIR}" \
  --set 'postgresql.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=custom-affinity-key' \
  --set 'postgresql.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "zone affinity: custom affinity renders" "${sts_custom_aff}" "custom-affinity-key"
assert_not_contains "zone affinity: custom affinity drops default zone term" "${sts_custom_aff}" "topology.kubernetes.io/zone"
assert_not_contains "zone affinity: custom affinity drops default hostname term" "${sts_custom_aff}" "topologyKey: kubernetes.io/hostname"

# Test: pgpool default affinity is unchanged (no zone term)
pgpool_aff=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "zone affinity: pgpool keeps hostname anti-affinity" "${pgpool_aff}" "topologyKey: kubernetes.io/hostname"
assert_not_contains "zone affinity: pgpool has no zone term" "${pgpool_aff}" "topology.kubernetes.io/zone"

# --- repmgr Monitoring History Retention Tests ---

# Test: repmgrd sidecar prunes repmgr.monitoring_history daily on the primary
mhd_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr: repmgrd runs cluster cleanup for monitoring history" "${mhd_sts}" 'cluster cleanup --keep-history="${MONITORING_HISTORY_DAYS}"'
assert_contains "repmgr: cleanup loop gates on pg_is_in_recovery" "${mhd_sts}" 'SELECT pg_is_in_recovery'

# Test: retention defaults to 7 days
mhd_line=$(printf '%s' "${mhd_sts}" | grep -A1 "name: MONITORING_HISTORY_DAYS" | tail -1)
assert_contains "repmgr: monitoring history retention defaults to 7 days" "${mhd_line}" 'value: "7"'

# Test: repmgr.monitoringHistoryDays override propagates to the repmgrd env
mhd_sts_30=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set repmgr.monitoringHistoryDays=30 \
  --show-only templates/statefulset.yaml 2>&1)
mhd_line_30=$(printf '%s' "${mhd_sts_30}" | grep -A1 "name: MONITORING_HISTORY_DAYS" | tail -1)
assert_contains "repmgr: monitoring history retention override renders" "${mhd_line_30}" 'value: "30"'

# Test: minimal (no repmgr) render carries no cleanup loop or retention env
assert_not_contains "minimal: no monitoring history cleanup" "${minimal}" "cluster cleanup"
assert_not_contains "minimal: no MONITORING_HISTORY_DAYS env" "${minimal}" "MONITORING_HISTORY_DAYS"

# --- postgresql.majorVersion Extension Path Tests ---

# Test: default majorVersion (18) renders /18/ extension paths in both init
# containers and the postgresql container volumeMounts
extpaths_default=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set postgresql.extensions.enabled=true \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "majorVersion default: ext-lib mountPath uses /usr/lib/postgresql/18/lib" "${extpaths_default}" "mountPath: /usr/lib/postgresql/18/lib"
assert_contains "majorVersion default: ext-share mountPath uses /usr/share/postgresql/18/extension" "${extpaths_default}" "mountPath: /usr/share/postgresql/18/extension"
ext_lib_count=$(printf '%s' "${extpaths_default}" | grep -c "/usr/lib/postgresql/18/lib" || true)
ext_share_count=$(printf '%s' "${extpaths_default}" | grep -c "/usr/share/postgresql/18/extension" || true)
# copy-base-ext cp + copy-ext cp + postgresql volumeMount = 3 each
assert_eq "majorVersion default: three /usr/lib/postgresql/18/lib occurrences" "3" "${ext_lib_count}"
assert_eq "majorVersion default: three /usr/share/postgresql/18/extension occurrences" "3" "${ext_share_count}"

# Test: overriding majorVersion swaps every extension path, leaving no /18/.
# In repmgr mode the major pin (#133) requires repmgr.image.majorVersion to move
# in lockstep, so set both to model a repmgr image rebuilt for the new major.
extpaths_19=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set postgresql.extensions.enabled=true \
  --set postgresql.majorVersion=19 \
  --set repmgr.image.majorVersion=19 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "majorVersion=19: ext-lib mountPath uses /usr/lib/postgresql/19/lib" "${extpaths_19}" "mountPath: /usr/lib/postgresql/19/lib"
assert_contains "majorVersion=19: ext-share mountPath uses /usr/share/postgresql/19/extension" "${extpaths_19}" "mountPath: /usr/share/postgresql/19/extension"
ext19_lib_count=$(printf '%s' "${extpaths_19}" | grep -c "/usr/lib/postgresql/19/lib" || true)
ext19_share_count=$(printf '%s' "${extpaths_19}" | grep -c "/usr/share/postgresql/19/extension" || true)
assert_eq "majorVersion=19: three /usr/lib/postgresql/19/lib occurrences" "3" "${ext19_lib_count}"
assert_eq "majorVersion=19: three /usr/share/postgresql/19/extension occurrences" "3" "${ext19_share_count}"
assert_not_contains "majorVersion=19: no /18/ paths remain in statefulset" "${extpaths_19}" "postgresql/18/"

# Test: empty majorVersion fails fast when extensions are enabled. Run standalone
# so this exercises the extensions `required` guard; the repmgr-major pin (#133)
# would otherwise fire first in repmgr mode (covered by the major-pin tests).
extpaths_empty=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 \
  --set postgresql.extensions.enabled=true \
  --set postgresql.majorVersion= \
  --show-only templates/statefulset.yaml 2>&1) && extpaths_empty_rc=0 || extpaths_empty_rc=$?
assert_eq "majorVersion empty: render fails when extensions enabled" "1" "${extpaths_empty_rc}"
assert_contains "majorVersion empty: error names postgresql.majorVersion" "${extpaths_empty}" "postgresql.majorVersion is required"

# Test: empty majorVersion is ignored while extensions stay disabled (standalone;
# in repmgr mode the major pin requires it to match the repmgr image major)
helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false \
  --set postgresql.replicaCount=0 \
  --set postgresql.majorVersion= \
  --show-only templates/statefulset.yaml >/dev/null 2>&1 && extpaths_off_rc=0 || extpaths_off_rc=$?
assert_eq "majorVersion empty: render succeeds with extensions disabled" "0" "${extpaths_off_rc}"

# --- PGPool admin credentials Tests ---

# Test: chart-managed admin Secret renders the values from pgpool.admin
pcp_secret=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --show-only templates/pgpool-secret.yaml 2>&1)
assert_contains "pgpool admin: chart-managed secret rendered" "${pcp_secret}" "name: test-pg-pgpool-admin"
# "admin" base64-encoded
assert_contains "pgpool admin: secret carries username" "${pcp_secret}" "username: \"YWRtaW4=\""
assert_contains "pgpool admin: secret carries password" "${pcp_secret}" "password: \"YWRtaW4=\""

# Test: admin Secret is gated on pgpool.enabled
assert_not_contains "pgpool admin: no admin secret when pgpool disabled" "${minimal}" "pgpool-admin"

# Test: plaintext admin password never lands in a ConfigMap
pcp_full=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set pgpool.admin.password=pcp-sup3r-secret 2>&1)
pcp_configmaps=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set pgpool.admin.password=pcp-sup3r-secret \
  --show-only templates/pgpool-configmap.yaml 2>&1)
assert_not_contains "pgpool admin: plaintext password absent from pgpool configmap" "${pcp_configmaps}" "pcp-sup3r-secret"
assert_not_contains "pgpool admin: pcp.conf no longer shipped via configmap" "${pcp_configmaps}" "pcp.conf"
assert_not_contains "pgpool admin: plaintext password absent from full render" "${pcp_full}" "pcp-sup3r-secret"
# "pcp-sup3r-secret" base64-encoded
assert_contains "pgpool admin: full render carries password only base64-encoded" "${pcp_full}" "cGNwLXN1cDNyLXNlY3JldA=="

# Test: deployment wires admin credentials from the chart-managed Secret
pcp_deploy=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "pgpool admin: deployment has PCP_ADMIN_USER env" "${pcp_deploy}" "name: PCP_ADMIN_USER"
assert_contains "pgpool admin: deployment has PCP_ADMIN_PASSWORD env" "${pcp_deploy}" "name: PCP_ADMIN_PASSWORD"
pcp_ref_count=$(printf '%s' "${pcp_deploy}" | grep -c "name: test-pg-pgpool-admin" || true)
assert_eq "pgpool admin: deployment references chart-managed secret twice" "2" "${pcp_ref_count}"
# #130: pcp.conf must be md5(password) -- pgpool PCP auth rejects sha256, which
# made every pcp_* admin command fail. md5sum of the raw password equals pg_md5.
assert_contains "pgpool admin: pcp.conf hashed with md5 (#130)" "${pcp_deploy}" "md5sum"
assert_not_contains "pgpool admin: pcp.conf not hashed with sha256 (#130)" "${pcp_deploy}" "sha256sum"

# Test: existingSecret mode omits the chart-managed Secret and uses the named one
pcp_ext=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set pgpool.admin.existingSecret.enabled=true \
  --set pgpool.admin.existingSecret.name=my-pcp-secret \
  --set pgpool.admin.existingSecret.usernameKey=pcp-user \
  --set pgpool.admin.existingSecret.passwordKey=pcp-pass 2>&1)
assert_not_contains "pgpool admin existingSecret: chart-managed secret omitted" "${pcp_ext}" "test-pg-pgpool-admin"
assert_contains "pgpool admin existingSecret: named secret referenced" "${pcp_ext}" "name: my-pcp-secret"
assert_contains "pgpool admin existingSecret: username key referenced" "${pcp_ext}" "key: pcp-user"
assert_contains "pgpool admin existingSecret: password key referenced" "${pcp_ext}" "key: pcp-pass"

# Test: existingSecret mode without a name fails fast
pcp_noname=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set pgpool.admin.existingSecret.enabled=true 2>&1) && pcp_noname_rc=0 || pcp_noname_rc=$?
assert_eq "pgpool admin existingSecret: missing name fails" "1" "${pcp_noname_rc}"
assert_contains "pgpool admin existingSecret: missing name error names the value" "${pcp_noname}" "pgpool.admin.existingSecret.name is required"

# #137: postgresql.existingSecret.enabled=true with no name must fail fast, not
# render an empty secretKeyRef.name across the StatefulSet/pgpool/exporter/backup
pg_secret_noname=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.existingSecret.enabled=true 2>&1) && pg_secret_noname_rc=0 || pg_secret_noname_rc=$?
assert_eq "#137: postgresql.existingSecret missing name fails" "1" "${pg_secret_noname_rc}"
assert_contains "#137: postgresql.existingSecret missing name error names the value" "${pg_secret_noname}" "postgresql.existingSecret.name is required"
# with a name it renders and references the named secret
pg_secret_named=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=my-pg-secret \
  --show-only templates/statefulset.yaml 2>&1) && pg_secret_named_rc=0 || pg_secret_named_rc=$?
assert_eq "#137: postgresql.existingSecret with a name renders" "0" "${pg_secret_named_rc}"
assert_contains "#137: postgresql.existingSecret named secret referenced" "${pg_secret_named}" "name: my-pg-secret"

# Test: removed pgpool.adminUsername/adminPassword values fail fast
pcp_legacy=$(helm template test-pg "${CHART_DIR}" \
  --set pgpool.enabled=true \
  --set pgpool.adminPassword=admin 2>&1) && pcp_legacy_rc=0 || pcp_legacy_rc=$?
assert_eq "pgpool admin: legacy adminPassword fails" "1" "${pcp_legacy_rc}"
assert_contains "pgpool admin: legacy value error points at pgpool.admin" "${pcp_legacy}" "pgpool.adminUsername and pgpool.adminPassword were removed"

# --- Read-Only Replica Service Tests ---

# Test: readonly service renders with repmgr values and selects the standby
# role label maintained by the service-updater
ro_svc=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/service-readonly.yaml 2>&1)
assert_contains "readonly: service named with -readonly suffix" "${ro_svc}" "name: test-pg-readonly"
assert_contains "readonly: selector targets standby role" "${ro_svc}" "pg-role: standby"
assert_contains "readonly: selector keeps postgresql component" "${ro_svc}" "app.kubernetes.io/component: postgresql"
assert_contains "readonly: service port wired like primary service" "${ro_svc}" "port: 5432"
assert_contains "readonly: targetPort is postgresql" "${ro_svc}" "targetPort: postgresql"
assert_not_contains "readonly: selector never pins a pod-name" "${ro_svc}" "statefulset.kubernetes.io/pod-name"

# Test: readonly service is not rendered in standalone (minimal) mode
assert_not_contains "minimal: no readonly service" "${minimal}" "test-pg-readonly"

# Test: rbac grants pod get/list/patch for role labeling (delete kept for
# split-brain fencing)
ro_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "readonly: rbac grants pods get/list/patch/delete" "${ro_role}" 'verbs: \["get", "list", "patch", "delete"\]'

# Test: service-updater script carries the convergent labeling logic
ro_updater=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/service-updater-configmap.yaml 2>&1)
assert_contains "readonly: updater defines update_pod_role_labels" "${ro_updater}" "update_pod_role_labels() {"
assert_contains "readonly: updater calls labeling each tick" "${ro_updater}" 'update_pod_role_labels "$CURRENT_MASTER"'
assert_contains "readonly: updater labels with --overwrite for idempotency" "${ro_updater}" 'kubectl label pod "${pod}" -n "${NAMESPACE}" "pg-role=${desired_role}" --overwrite'
assert_contains "readonly: updater lists pods by chart selector labels" "${ro_updater}" "app.kubernetes.io/instance=test-pg,app.kubernetes.io/component=postgresql"

# --- service-updater Failover Audit Event Tests ---

# Test: service-updater emits a core/v1 PrimaryChanged Event on the Service
su_audit_cm=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/service-updater-configmap.yaml 2>&1)
assert_contains "audit event: PrimaryChanged reason present" "${su_audit_cm}" "reason: PrimaryChanged"
assert_contains "audit event: manifest is core/v1 Event" "${su_audit_cm}" "kind: Event"
assert_contains "audit event: event regards the primary Service" "${su_audit_cm}" "kind: Service"
assert_contains "audit event: message carries old and new pod names" "${su_audit_cm}" "message: Primary changed from"
assert_contains "audit event: event type is Normal" "${su_audit_cm}" "type: Normal"
assert_contains "audit event: creation failure logged as warning" "${su_audit_cm}" "WARNING: failed to create PrimaryChanged event"

# Test: event emission ordered strictly after the selector patch (patch is
# correctness, event is observability)
su_patch_line=$(printf '%s\n' "${su_audit_cm}" | grep -n "kubectl patch service" | head -1 | cut -d: -f1 || true)
su_event_line=$(printf '%s\n' "${su_audit_cm}" | grep -n 'emit_primary_changed_event "${CURRENT_SELECTOR}"' | head -1 | cut -d: -f1 || true)
assert_gt "audit event: emission ordered after selector patch" "${su_event_line:-0}" "${su_patch_line:-99999}"

# Test: RBAC role grants create on core events for the audit Event
su_audit_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "audit event: rbac role has events resource" "${su_audit_rbac}" 'resources: \["events"\]'
su_events_rule=$(printf '%s\n' "${su_audit_rbac}" | grep -A 1 'resources: \["events"\]' || true)
assert_contains "audit event: events rule grants create verb" "${su_events_rule}" 'verbs: \["create"\]'

# --- Stale-Primary Guard Wiring Tests (issue #123) ---
# The guard itself lives in the repmgr image entrypoint (so it runs on every
# container start, including container-only restarts the init container
# misses). The chart's job is only to invoke the entrypoint cleanly and pass
# the cluster size the peer scan needs.

guard_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
# repmgr mode runs the image entrypoint directly (no chart-side wrapper)
assert_contains "stale guard: postgresql container runs the image entrypoint" "${guard_sts}" '"/usr/local/bin/entrypoint.sh"'
assert_not_contains "stale guard: no chart-side wrapper script" "${guard_sts}" "refusing to start read-write"
# the entrypoint guard scans peers; the chart passes the node count so the
# scan bound matches the cluster (replicaCount + 1)
nodecount=$(printf '%s' "${guard_sts}" | grep -c "name: REPMGR_NODE_COUNT" || true)
assert_gt "stale guard: REPMGR_NODE_COUNT propagated to repmgr containers" "${nodecount}" "0"
# replicaCount 1 -> node count 2; assert the value rendered on the env var
nodecount_val=$(printf '%s' "${guard_sts}" | grep -A1 "name: REPMGR_NODE_COUNT" | grep "value:" | head -1)
assert_contains "stale guard: node count is replicaCount + 1" "${nodecount_val}" '"2"'

# Test: standalone (non-repmgr) mode uses the postgres image, not the guard
assert_not_contains "stale guard: standalone has no REPMGR_NODE_COUNT" "${minimal}" "REPMGR_NODE_COUNT"

# --- pgvector parity (#126) ---
# pgvector/templates/ are symlinks to pg/templates/ but pgvector/values.yaml is
# an independent copy, so a value the shared template dereferences can be
# missing in pgvector and silently break rendering. The shared backup-cronjob
# reads backup.mc.image.* and the backup securityContexts; render the pgvector
# chart with backups enabled (the case that previously nil-pointered) and
# confirm both the mc image and a non-null securityContext appear.
PGVECTOR_DIR="$(cd "${CHART_DIR}/../pgvector" && pwd)"
pgv_backup_rc=0
pgv_backup=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=b \
  --set backup.existingSecret.name=creds \
  --show-only templates/backup-cronjob.yaml 2>&1) || pgv_backup_rc=$?
assert_eq "pgvector #126: renders with backup enabled" "0" "${pgv_backup_rc}"
assert_contains "pgvector #126: backup mc image present" "${pgv_backup}" "minio/mc:"
assert_contains "pgvector #126: backup container securityContext populated" "${pgv_backup}" "runAsUser: 999"
assert_not_contains "pgvector #126: no null securityContext" "${pgv_backup}" "securityContext: null"

# --- #128: global.annotations render as metadata.annotations, not labels ---
# global.annotations used to be spliced into pg.labels and rendered under
# metadata.labels on every resource: non-label-safe values (spaces, URLs, >63
# chars) broke apply, and annotation consumers never saw them. They must render
# under metadata.annotations on every resource (including common-lib resources).
gann=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set prometheusExporter.serviceMonitor.enabled=true \
  --set backup.enabled=true --set-string backup.existingSecret.name=bk \
  --set-string backup.s3.endpoint=s3.example.com --set-string backup.s3.bucket=b \
  --set pgbackrest.enabled=true --set-string pgbackrest.existingSecret.name=pb \
  --set-string pgbackrest.s3.endpoint=s3.example.com --set-string pgbackrest.s3.bucket=b \
  --set networkPolicy.enabled=true \
  --set-string 'global.annotations.example\.com/team=data platform' 2>&1)
# the value is a valid annotation but an INVALID label (contains a space)
gann_kinds=$(printf '%s\n' "${gann}" | grep -c '^kind:')
gann_hits=$(printf '%s\n' "${gann}" | grep -c 'example.com/team')
assert_eq "global.annotations: present on every rendered resource (#128)" "${gann_kinds}" "${gann_hits}"
# never under a labels: block (would fail server-side label validation)
gann_leak=$(printf '%s\n' "${gann}" | awk '/^  labels:/{l=1;next} /^  [a-z]/{l=0} l && /example.com\/team/{c++} END{print c+0}')
assert_eq "global.annotations: never rendered under labels (#128)" "0" "${gann_leak}"
# common-lib-rendered resources (PDB, exporter Service/Deployment/ServiceMonitor) carry it too
assert_contains "global.annotations: reaches common-lib resources (#128)" "${gann}" "example.com/team: data platform"
# unset -> no stray resource-level annotations block on a resource that has none
gann_unset=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --show-only templates/service.yaml 2>&1)
assert_not_contains "global.annotations: unset adds no annotations block (#128)" "${gann_unset}" "annotations:"
# merge: global + per-component annotations coexist on the StatefulSet
gann_merge=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set-string 'global.annotations.example\.com/team=data platform' \
  --set-string 'postgresql.annotations.sts\.local/own=yes' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "global.annotations: merges, global key kept (#128)" "${gann_merge}" "example.com/team: data platform"
assert_contains "global.annotations: merges, component key kept (#128)" "${gann_merge}" "sts.local/own"

end_suite
print_summary
