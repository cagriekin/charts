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

# #286: repmgrd and its service-updater are gone; HA runs the agent as PID 1 in the
# postgresql container, so neither sidecar may appear in any render.
assert_not_contains "#286: no repmgrd sidecar" "${repmgr}" "name: repmgrd"
assert_not_contains "#286: no service-updater sidecar" "${repmgr}" "name: service-updater"
assert_contains "#286: HA runs the agent entrypoint" "${repmgr}" '"/usr/local/bin/entrypoint.sh", "agent"'

# Repmgr: should have RBAC resources
assert_contains "repmgr: serviceaccount created" "${repmgr}" "kind: ServiceAccount"
assert_contains "repmgr: role created" "${repmgr}" "kind: Role"
assert_contains "repmgr: rolebinding created" "${repmgr}" "kind: RoleBinding"

# Repmgr: RBAC role restricts to specific resource names
repmgr_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" --show-only templates/rbac.yaml 2>&1)
assert_contains "repmgr: role has resourceNames for services" "${repmgr_role}" "resourceNames:"
assert_contains "repmgr: role restricts service to release name" "${repmgr_role}" "test-pg"

# #154: the repmgr Role's pods access must be least-privilege -- list unscoped
# (cannot be name-scoped), get/patch scoped to the StatefulSet pod names, and
# delete granted only in fence mode (the sole path that deletes pods).
pods_rules_fmt='
import sys,yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind")=="Role" and d["metadata"]["name"].endswith("-repmgr"):
        for r in d["rules"]:
            if r.get("resources")==["pods"]:
                print("pods verbs=%s scoped=%s names=%s" % (",".join(sorted(r["verbs"])), "yes" if "resourceNames" in r else "no", ";".join(r.get("resourceNames") or [])))
        break
'
pods_rules_log=$(printf '%s' "${repmgr_role}" | python3 -c "${pods_rules_fmt}")
assert_contains "#154: pods list rule is unscoped" "${pods_rules_log}" "pods verbs=list scoped=no"
assert_contains "#154: pods get/patch scoped to pod names" "${pods_rules_log}" "pods verbs=get,patch scoped=yes"
assert_contains "#154: scoped rule names the StatefulSet pods" "${pods_rules_log}" "test-pg-0"
assert_not_contains "#154: no pods delete in default (log) mode" "${pods_rules_log}" "delete"
# #286: the pods `delete` verb is gone entirely. It was only ever granted to the
# repmgrd service-updater for split-brain fencing; the agent soft-fences locally via
# pg_ctl, so fence mode must NOT re-introduce a pod-delete grant.
repmgr_role_fence=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set repmgr.splitBrainDetection.action=fence --show-only templates/rbac.yaml 2>&1)
pods_rules_fence=$(printf '%s' "${repmgr_role_fence}" | python3 -c "${pods_rules_fmt}")
assert_contains "#286: fence mode still scopes get/patch to pod names" "${pods_rules_fence}" "pods verbs=get,patch scoped=yes"
assert_not_contains "#286: fence mode grants no pod delete (agent soft-fences)" "${pods_rules_fence}" "delete"

# #286: the pgpool Deployment get/patch grant existed so the service-updater could
# restart pgpool after a repmgrd failover. The agent re-points the Services instead.
full_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" --show-only templates/rbac.yaml 2>&1)
assert_not_contains "#286: no pgpool Deployment grant in the Role" "${full_role}" "test-pg-pgpool"

# Repmgr: should have headless service
assert_contains "repmgr: headless service present" "${repmgr}" "clusterIP: None"

# #286: the service-updater ConfigMap (its rendered script) is gone with the sidecar.
assert_not_contains "#286: no service-updater ConfigMap" "${repmgr}" "service-updater"

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

# #118: the PCP admin port (9898) must not be exposed on the Service by default; it is
# opt-in via pgpool.service.exposePcp.
pgpool_svc_default=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --show-only templates/pgpool-service.yaml 2>&1)
assert_not_contains "#118: pgpool Service does not expose PCP 9898 by default" "${pgpool_svc_default}" "9898"
pgpool_svc_pcp=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set pgpool.service.exposePcp=true --show-only templates/pgpool-service.yaml 2>&1)
assert_contains "#118: pgpool Service exposes PCP 9898 when opted in" "${pgpool_svc_pcp}" "9898"

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

# #157: single quotes in conf values must be doubled or the postmaster/pgpool fails to
# start (PostgreSQL/pgpool conf lexer requires '' to embed a literal single quote).
pgconf_quote=$(helm template test-pg "${CHART_DIR}" --set-string "postgresql.configuration.log_line_prefix=it's" --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#157: postgresql.conf doubles embedded single quotes" "${pgconf_quote}" "log_line_prefix = 'it''s'"
pgpool_quote=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set-string "pgpool.resetQueryList=SET x='y'" --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "#157: pgpool reset_query_list doubles embedded single quotes" "${pgpool_quote}" "reset_query_list = 'SET x=''y'''"
stanza_quote=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://s --set pgbackrest.s3.bucket=b --set pgbackrest.existingSecret.name=sec --set-string "pgbackrest.stanza=x'y" --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#157: archive_command doubles single quotes in the stanza" "${stanza_quote}" "stanza=x''y archive-push"

# #207: agent-mode pgpool must only configure the RO (-readonly) backend when standbys
# exist. With replicaCount=0 the -readonly Service has zero endpoints, so a backend1
# there fails every health check and churns pgpool into "restarting myself" cycles,
# killing live connections to the healthy primary.
pgpool_agent_ro=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set repmgr.enabled=true --set postgresql.replicaCount=2 --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "#207: agent mode with standbys configures RO backend1" "${pgpool_agent_ro}" "backend_hostname1 = 'test-pg-readonly"
assert_contains "#207: agent mode with standbys weights primary 0" "${pgpool_agent_ro}" "backend_weight0 = 0"
pgpool_agent_primary=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set repmgr.enabled=true --set postgresql.replicaCount=0 --show-only templates/pgpool-configmap.yaml 2>&1)
assert_not_contains "#207: agent mode primary-only omits RO backend1" "${pgpool_agent_primary}" "backend_hostname1"
assert_contains "#207: agent mode primary-only weights the sole primary 1" "${pgpool_agent_primary}" "backend_weight0 = 1"

# #156: env values must be quoted (k8s EnvVar.value is a string; a numeric/bool-looking
# value renders as a YAML scalar the API server rejects with an unmarshal error).
numericenv=$(helm template test-pg "${CHART_DIR}" --set repmgr.database=12345 --show-only templates/statefulset.yaml 2>&1)
assert_contains "#156: numeric REPMGR_DB renders as a quoted string" "${numericenv}" 'value: "12345"'
assert_not_contains "#156: numeric REPMGR_DB not a bare YAML scalar" "${numericenv}" 'value: 12345'
# boolean coercion (the other half of #156): YAML types unquoted true/false as a bool,
# which the API server also rejects for the string EnvVar.value.
boolenv=$(helm template test-pg "${CHART_DIR}" --set repmgr.database=true --show-only templates/statefulset.yaml 2>&1)
assert_contains "#156: boolean REPMGR_DB renders as a quoted string" "${boolenv}" 'value: "true"'
assert_not_contains "#156: boolean REPMGR_DB not a bare YAML bool" "${boolenv}" 'value: true'
# a second quoted field (the env-var path the agent/sidecars also consume)
stanzaenv=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://s --set pgbackrest.s3.bucket=b --set pgbackrest.existingSecret.name=sec --set pgbackrest.stanza=123 --show-only templates/statefulset.yaml 2>&1)
assert_contains "#156: numeric PGBACKREST_STANZA renders as a quoted string" "${stanzaenv}" 'value: "123"'

# Full: should have prometheus exporter deployment
assert_contains "full: prometheus exporter present" "${full}" "postgres-exporter"

# #146: exporter probes hit /metrics (not the always-200 landing page /) so a broken
# scrape pipeline (queries.yaml/collector regression -> 500) is detected.
exporter_deploy=$(helm template test-pg "${CHART_DIR}" --set prometheusExporter.enabled=true --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
# anchor to the probe path lines ("<indent>path: /metrics$") so the prometheus.io/path
# annotation is not miscounted; || true keeps a 0-match grep from aborting under set -e.
exporter_metrics_probes=$(printf '%s\n' "${exporter_deploy}" | grep -cE '^ +path: /metrics$' || true)
assert_eq "#146: both exporter probes target /metrics" "2" "${exporter_metrics_probes}"
exporter_root_probes=$(printf '%s\n' "${exporter_deploy}" | grep -cE '^ +path: /$' || true)
assert_eq "#146: no exporter probe targets the bare landing page /" "0" "${exporter_root_probes}"

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
# #162: drop the default capability set, keeping only what chown/chmod need.
assert_contains "#162: fix-permissions drops capabilities" "${fix_perms}" "capabilities:"
assert_contains "#162: fix-permissions keeps CHOWN" "${fix_perms}" "CHOWN"
assert_contains "#162: fix-permissions keeps DAC_OVERRIDE" "${fix_perms}" "DAC_OVERRIDE"
assert_not_contains "#162: fix-permissions does not keep SETUID" "${fix_perms}" "SETUID"

# #165: emptyDir volumes are size-capped so a runaway volume evicts its own pod
# instead of filling the node. A full render (persistence on -> PVC, no data emptyDir)
# must have no uncapped emptyDir.
sized_full=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)
assert_not_contains "#165: no uncapped emptyDir in a full render" "${sized_full}" "emptyDir: {}"
data_capped=$(helm template test-pg "${CHART_DIR}" --set postgresql.persistence.enabled=false --set postgresql.persistence.emptyDir.sizeLimit=8Gi --show-only templates/statefulset.yaml 2>&1)
assert_contains "#165: non-persistent data emptyDir honors the sizeLimit" "${data_capped}" "sizeLimit: 8Gi"
# #214: sizeLimit unset -> fall back to persistence.size (never the old unbounded
# emptyDir: {}), so the ephemeral PGDATA volume can never fill the node.
data_default=$(helm template test-pg "${CHART_DIR}" --set postgresql.persistence.enabled=false --set postgresql.persistence.size=10Gi --show-only templates/statefulset.yaml 2>&1)
assert_contains "#214: non-persistent data emptyDir falls back to persistence.size" "${data_default}" "sizeLimit: 10Gi"
assert_not_contains "#214: non-persistent data emptyDir is never unbounded" "${data_default}" "emptyDir: {}"
# cover the feature-gated caps the full render omits (ext trees 1Gi; pgbackrest pg-run 16Mi)
sized_feature=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set postgresql.extensions.enabled=true --set postgresql.majorVersion=18 2>&1)
assert_contains "#165: extension-tree emptyDir capped at 1Gi" "${sized_feature}" "sizeLimit: 1Gi"

# #153: init containers must declare resources (else pods are Forbidden under a
# ResourceQuota). Lightweight inits use the small shared block (cpu: 10m); repmgr-init
# (the clone) uses its own heavier, overridable resources.
init_res=$(helm template test-pg "${CHART_DIR}" --set postgresql.extensions.enabled=true --set postgresql.majorVersion=18 --show-only templates/statefulset.yaml 2>&1)
assert_contains "#153: lightweight init containers declare resources" "${init_res}" "cpu: 10m"
# scope to the repmgr-init block and assert its distinctive clone CPU limit (cpu: "1"),
# which no other container uses -- a plain "memory: 1Gi" needle would also match the
# main postgresql container and so would not prove repmgr-init has resources.
repmgr_init_res=$(printf '%s\n' "${init_res}" | sed -n '/name: repmgr-init/,/name: setup-config/p')
assert_contains "#153: repmgr-init declares its (heavier) clone resources" "${repmgr_init_res}" 'cpu: "1"'

# #116: the busybox helper init image is a single shared value (default 1.37 for all
# four init containers, no more 1.35/1.37 split) and is overridable for air-gapped
# registries. Default render must carry no hardcoded busybox tag.
busybox_full=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set prometheusExporter.enabled=true --set postgresql.persistence.enabled=true 2>&1)
assert_not_contains "#116: no hardcoded busybox:1.35 left" "${busybox_full}" 'image: busybox:1.35'
assert_eq "#116: all four busybox inits use the shared default 1.37" "4" "$(printf '%s\n' "${busybox_full}" | grep -c 'image: "busybox:1.37"')"
busybox_override=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set prometheusExporter.enabled=true --set busyboxImage.repository=mirror.local/busybox --set busyboxImage.tag=1.36 --set busyboxImage.pullPolicy=Always 2>&1)
assert_contains "#116: busyboxImage override applies" "${busybox_override}" 'image: "mirror.local/busybox:1.36"'
assert_contains "#116: busyboxImage pullPolicy override applies" "${busybox_override}" "imagePullPolicy: Always"

# #26: every image is digest-pinnable via the shared pg.image helper. Empty digest
# (default) renders repository:tag; a set digest appends @<digest>.
img_pin=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set pgpool.metrics.enabled=true --set prometheusExporter.enabled=true \
  --set busyboxImage.digest=sha256:bbb --set postgresql.image.digest=sha256:ppp \
  --set pgpool.image.digest=sha256:ggg --set pgpool.metrics.image.digest=sha256:mmm \
  --set prometheusExporter.image.digest=sha256:eee 2>&1)
assert_contains "#26: busybox image digest-pinnable" "${img_pin}" 'image: "busybox:1.37@sha256:bbb"'
assert_contains "#26: postgresql image digest-pinnable" "${img_pin}" '@sha256:ppp"'
assert_contains "#26: pgpool image digest-pinnable" "${img_pin}" '@sha256:ggg"'
assert_contains "#26: pgpool-exporter image digest-pinnable" "${img_pin}" '@sha256:mmm"'
assert_contains "#26: prometheus-exporter image digest-pinnable" "${img_pin}" '@sha256:eee"'
img_pin_bk=$(helm template test-pg "${CHART_DIR}" --set backup.enabled=true --set backup.s3.endpoint=https://e --set backup.s3.bucket=b --set backup.existingSecret.name=s --set backup.mc.image.digest=sha256:ccc --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://e --set pgbackrest.s3.bucket=b --set pgbackrest.existingSecret.name=s3 --set pgbackrest.cronjob.image.digest=sha256:kkk 2>&1)
assert_contains "#26: backup mc image digest-pinnable" "${img_pin_bk}" '@sha256:ccc"'
assert_contains "#26: pgbackrest cronjob image digest-pinnable" "${img_pin_bk}" '@sha256:kkk"'
# default: no @sha256 in any image ref
assert_not_contains "#26: no digest in image refs by default" "$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set prometheusExporter.enabled=true 2>&1 | grep '          image:')" "@sha256"

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
assert_contains "netpol: exporter policy rendered" "${netpol}" "test-pg-postgres-exporter"
assert_contains "netpol: exporter allows port 9116" "${netpol}" "port: 9116"

# #148: under allowExternal=false the documented extraIngress recipe re-allows scoped
# direct-5432 clients (the read-only Service path), proving the documented workaround.
netpol_extra=$(helm template test-pg "${CHART_DIR}" \
  --set networkPolicy.enabled=true \
  --set networkPolicy.postgresql.allowExternal=false \
  --set "networkPolicy.postgresql.extraIngress[0].ports[0].port=5432" \
  --set "networkPolicy.postgresql.extraIngress[0].from[0].podSelector.matchLabels.app=my-read-client" \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "#148: extraIngress recipe re-allows scoped direct-5432 clients" "${netpol_extra}" "app: my-read-client"

# #147: the exporter NetworkPolicy now has an extraIngress escape hatch so a Prometheus
# in another namespace can scrape 9116 (the default 9116 ingress is same-namespace only).
netpol_exporter_extra=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set networkPolicy.enabled=true \
  --set "networkPolicy.prometheusExporter.extraIngress[0].ports[0].port=9116" \
  --set "networkPolicy.prometheusExporter.extraIngress[0].from[0].namespaceSelector.matchLabels.team=monitoring" \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "#147: exporter NetworkPolicy honors extraIngress (cross-namespace scrape)" "${netpol_exporter_extra}" "team: monitoring"

# Test: NetworkPolicy without pgpool/exporter only renders postgresql policy
netpol_minimal=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "netpol minimal: postgresql policy rendered" "${netpol_minimal}" "test-pg-postgresql"
assert_not_contains "netpol minimal: no pgpool policy" "${netpol_minimal}" "test-pg-pgpool"
assert_not_contains "netpol minimal: no exporter policy" "${netpol_minimal}" "test-pg-postgres-exporter"

# Test: postgresql egress allows API server ports
assert_contains "netpol: egress allows 443" "${netpol}" "port: 443"
assert_contains "netpol: egress allows 6443" "${netpol}" "port: 6443"

# #129/#286: the postgresql policy used to egress to pgpool's backend port (9999) so the
# service-updater could health-check it. That sidecar is gone and nothing in the postgresql
# pods connects to pgpool, so the rule was dropped -- traffic only flows pgpool ->
# postgresql. The pgpool policy's own ingress still allows 9999, so isolate the postgresql
# policy's egress block to prove the removal is on the egress side specifically.
pg_egress=$(printf '%s\n' "${netpol}" | awk '
  /^---/ { inpg=0; eg=0 }
  /name: test-pg-postgresql$/ { inpg=1 }
  inpg && /^  egress:/ { eg=1 }
  eg && /^---/ { eg=0 }
  eg { print }
')
assert_not_contains "#286: postgresql egress no longer opens pgpool port 9999" "${pg_egress}" "port: 9999"
assert_not_contains "#286: postgresql egress does not target the pgpool component" "${pg_egress}" "app.kubernetes.io/component: pgpool"
assert_not_contains "netpol minimal: no pgpool egress when pgpool disabled" "${netpol_minimal}" "app.kubernetes.io/component: pgpool"

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
# Standalone pins the base ingress-rule count (HA mode adds an agent-metrics rule);
# this test isolates the extraIngress splice shape.
netpol_xi=$(helm template test-pg "${CHART_DIR}" --set networkPolicy.enabled=true \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
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

# #144: the repmgr-branch postStart pg_hba insert (user entries spliced above the
# image's broad 10.0.0.0/8 and 0.0.0.0/0 catch-alls) went with repmgrd (#286). The agent
# is the single author of pg_hba now and receives user entries via POSTGRESQL_PGHBA --
# asserted in the "#199 + #144" block further down. Standalone still uses its own
# postStart sed insert, covered by the "config standalone: pgHba entry in postStart" case.

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
# Extract exactly the postStart block: from `postStart:` until the next key at the same
# or shallower indentation. The old `/postStart:/,/preStop:|resources:/` range broke when
# #286 removed the preStop hook -- it then ran on into the readinessProbe, which legitimately
# calls pg_is_in_recovery, and the "no primary discovery" assertion failed on the probe.
repmgr_no_addcmd_poststart=$(echo "${repmgr_no_addcmd}" | awk '
  /postStart:/ && !f { f=1; ind=match($0, /[^ ]/); print; next }
  f { if (match($0, /[^ ]/) <= ind && $0 ~ /[^ ]/) exit; print }
')
assert_not_contains "repmgr no additionalCommands: no primary discovery in postStart" "${repmgr_no_addcmd_poststart:-empty}" "pg_is_in_recovery"
assert_not_contains "repmgr no additionalCommands: no PGHOST in postStart" "${repmgr_no_addcmd_poststart:-empty}" "PGHOST"

# --- Graceful Shutdown Tests ---

# #286: the HA arm renders NO preStop hook. The agent is PID 1 and owns SIGTERM
# (release the Lease, then stop its postgres child); a preStop `pg_ctl stop` would race
# the supervisor and stop postgres before the Lease was released. The repmgrd-tuned
# preStop that used to render here went with repmgrd.
assert_not_contains "#286: HA renders no preStop hook" "${repmgr_no_addcmd}" "preStop:"
assert_contains "repmgr: terminationGracePeriodSeconds present" "${repmgr_no_addcmd}" "terminationGracePeriodSeconds: 120"

# A hook must never promote out-of-band: a raw pg_promote() bypasses the agent's
# lease/timeline election and strands the cluster on stale state (#102).
assert_not_contains "repmgr: no out-of-band pg_promote" "${repmgr_no_addcmd}" "pg_promote"
assert_not_contains "repmgr: nothing targets a standby on shutdown" "${repmgr_no_addcmd}" "STANDBY_HOST"

# Test: repmgr with configuration still renders postStart
repmgr_config=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-config-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr+config: postStart hook present" "${repmgr_config}" "postStart:"

# Test: custom terminationGracePeriodSeconds
repmgr_custom_tgp=$(helm template test-pg "${CHART_DIR}" \
  -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set repmgr.terminationGracePeriodSeconds=300 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "repmgr: custom terminationGracePeriodSeconds" "${repmgr_custom_tgp}" "terminationGracePeriodSeconds: 300"

# #286: the repmgrd and service-updater containers, and the service-updater ConfigMap
# that carried their script, are all gone. The behaviour that script implemented lives in
# the agent and is asserted where the code now is:
#   - split-brain handling / fencing  -> internal/reconcile (Go tests)
#   - ghost repmgr.nodes cleanup (#139) -> cmd/agent ghostNodeIDs + the scaledown suite
#   - stale-primary selector (#124), LSN/timeline ordering (#131, #168), split-brain
#     re-assert (#169), durable marker (#125) -> internal/pg + internal/k8s (Go tests)
assert_not_contains "#286: no repmgrd container" "${repmgr_no_addcmd}" "name: repmgrd"
assert_not_contains "#286: no service-updater container" "${repmgr_no_addcmd}" "name: service-updater"
assert_not_contains "#286: no service-updater heartbeat file" "${repmgr_no_addcmd}" "service-updater-alive"
assert_not_contains "#286: no split-brain shell handler" "${repmgr}" "handle_split_brain"
assert_not_contains "#286: no shell ghost-node cleanup" "${repmgr}" "cleanup_ghost_nodes"

# Test: SPLIT_BRAIN_ACTION env var in statefulset
assert_contains "repmgr: SPLIT_BRAIN_ACTION env var in statefulset" "${repmgr_no_addcmd}" "SPLIT_BRAIN_ACTION"

# Test: split-brain handling not present when repmgr disabled
assert_not_contains "minimal: no split-brain handling" "${minimal}" "handle_split_brain"

# --- Durable primary marker (#125) ---
# The highest-timeline primary is recorded in a ConfigMap so a node booting first can
# tell it is stale even when the real primary is not up yet. The read/write of that
# marker moved into the agent with #286 (internal/k8s/marker.go, covered by Go tests);
# what the chart still owns is propagating the marker's NAME to the containers.
sts_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#125: PRIMARY_MARKER env propagated" "${sts_repmgr}" "name: PRIMARY_MARKER"
assert_contains "#125: marker name is <fullname>-primary" "${sts_repmgr}" "test-pg-primary"
# #170: the entrypoint stale-primary guard (postgresql container) reads the
# marker to gate its empty-data settle, so the marker env must reach the
# postgresql container itself.
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

# #176/#286: the HA render pins Parallel -- all pods are present at a cold boot so the
# lease winner compares timelines across the complete candidate set. OrderedReady now
# renders only for standalone. Pinned so a silent flip fails the suite.
assert_contains "#286: HA statefulset pins podManagementPolicy Parallel" "${sts_repmgr}" "podManagementPolicy: Parallel"
sts_standalone_pmp=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#286: standalone statefulset keeps OrderedReady" "${sts_standalone_pmp}" "podManagementPolicy: OrderedReady"

# --- agent HA arm ---
# The lease-based Go agent is the only failover path since #286.
agent_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
# #176: HA uses podManagementPolicy Parallel (complete-candidate survivor selection at
# cold boot); only standalone renders OrderedReady (asserted above).
assert_contains "agent #176: podManagementPolicy Parallel" "${agent_sts}" "podManagementPolicy: Parallel"
assert_not_contains "agent #176: HA is never OrderedReady" "${agent_sts}" "podManagementPolicy: OrderedReady"
# postgresql container runs the entrypoint 'agent' arm
assert_contains "agent: postgresql runs the agent arm" "${agent_sts}" '"/usr/local/bin/entrypoint.sh", "agent"'
# repmgrd + service-updater sidecars are gone
assert_not_contains "agent: no repmgrd sidecar" "${agent_sts}" "name: repmgrd"
assert_not_contains "agent: no service-updater sidecar" "${agent_sts}" "name: service-updater"
# shareProcessNamespace dropped (the agent is PID 1 in the postgresql container)
assert_not_contains "agent: no shareProcessNamespace" "${agent_sts}" "shareProcessNamespace"
# agent env + metrics port + liveness wiring (scoped to the postgresql container)
agent_pg_cont=$(printf '%s\n' "${agent_sts}" | awk '/^        - name: postgresql$/{f=1; next} f && /^        - name: /{exit} f{print}')
assert_contains "agent: LEASE_NAME env present" "${agent_pg_cont}" "name: LEASE_NAME"
assert_contains "agent: lease name is <fullname>-leader" "${agent_pg_cont}" "test-pg-leader"
assert_contains "agent: POD_NAME env present" "${agent_pg_cont}" "name: POD_NAME"
assert_contains "agent: DCS_BACKEND env present" "${agent_pg_cont}" "name: DCS_BACKEND"
# agent owns pg_hba: POD_CIDR feeds the hardened SCRAM-only pg_hba (no 0.0.0.0/0 md5)
assert_contains "agent: POD_CIDR env present (hardened pg_hba)" "${agent_pg_cont}" "name: POD_CIDR"
agent_hba=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'postgresql.pgHba[0]=host all admin 10.1.0.0/16 scram-sha-256' --show-only templates/statefulset.yaml 2>&1)
assert_contains "agent: postgresql.pgHba flows to POSTGRESQL_PGHBA" "${agent_hba}" "name: POSTGRESQL_PGHBA"
# standalone has no agent, so it gets none of this (the agent owns pg_hba)
standalone_sts_hba=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "standalone: no POD_CIDR env (agent-only pg_hba ownership)" "${standalone_sts_hba}" "name: POD_CIDR"
# config completeness: the agent's Load() fail-fasts on any missing var, so a
# dropped env here is a boot crash-loop the other asserts would not catch.
agent_env_missing=""
for v in POD_NAME NAMESPACE LEASE_NAME LEASE_DURATION RENEW_DEADLINE RETRY_PERIOD \
  RECONCILE_INTERVAL HEADLESS_SERVICE REPMGR_NODE_COUNT MASTER_SERVICE PRIMARY_MARKER \
  POD_SELECTOR REPMGR_USER REPMGR_DB REPMGR_PASSWORD PGDATA DCS_BACKEND SPLIT_BRAIN_ACTION; do
  grep -q "name: ${v}$" <<< "${agent_pg_cont}" || agent_env_missing="${agent_env_missing} ${v}"
done
assert_eq "agent: all required agent env vars present (Load fail-fast)" "" "${agent_env_missing}"
assert_contains "agent: metrics port 9200 exposed" "${agent_pg_cont}" "containerPort: 9200"
assert_contains "agent: liveness probes the agent /healthz" "${agent_pg_cont}" "path: /healthz"
# #186: agent-mode readiness is replication-aware -- a standby is Ready only when its
# walreceiver is streaming, so RollingUpdate won't roll the primary/clone-source while
# a standby is mid-clone. Primary readiness stays plain pg_isready.
assert_contains "agent #186: readiness checks recovery role" "${agent_pg_cont}" "SELECT pg_is_in_recovery()"
assert_contains "agent #186: standby readiness gated on streaming" "${agent_pg_cont}" "SELECT status FROM pg_stat_wal_receiver"
# standalone keeps the bare pg_isready readiness probe -- there is no replication.
assert_not_contains "standalone #186: readiness stays bare pg_isready (no wal_receiver check)" "${standalone_sts_hba}" "SELECT status FROM pg_stat_wal_receiver"
# startupProbe (#172) kept in agent mode
assert_contains "agent #172: startupProbe kept" "${agent_pg_cont}" "startupProbe:"
# the agent owns SIGTERM shutdown, so the repmgrd-tuned preStop pg_ctl stop is
# gated off in agent mode (a competing stop would race the supervisor)
assert_not_contains "agent: no preStop pg_ctl stop (agent owns SIGTERM)" "${agent_pg_cont}" "pg_ctl stop"
assert_contains "agent: postStart config hook kept" "${agent_pg_cont}" "postStart:"

# service-updater configmap is not rendered in agent mode
agent_all=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" 2>&1)
assert_not_contains "agent: no service-updater configmap" "${agent_all}" "name: test-pg-service-updater"

# agent RBAC: leases + scoped marker, no pods delete in the default log mode
agent_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "agent rbac: coordination.k8s.io leases granted" "${agent_rbac}" "coordination.k8s.io"
assert_contains "agent rbac: lease scoped to <fullname>-leader" "${agent_rbac}" "test-pg-leader"
assert_contains "agent rbac: configmaps scoped to <fullname>-primary marker" "${agent_rbac}" "test-pg-primary"
# marker.go does Get -> Create-if-absent -> Update, so the scoped rule must grant
# update (not patch) or every marker advance would be Forbidden once wired
assert_contains "agent rbac: marker configmaps grant get+update (marker.go uses Update)" "${agent_rbac}" '"get", "update"'
assert_not_contains "agent rbac: no pods delete in log mode" "${agent_rbac}" '"delete"'
# agent mode records decisions in a structured audit log, not core/v1 Events, so
# the events:create grant (service-updater only) must be dropped (least privilege)
assert_not_contains "agent rbac: no events grant (agent emits no Events)" "${agent_rbac}" 'resources: ["events"]'
# Agent mode NEVER grants pods delete, even in fence mode: the agent soft-fences
# locally via pg_ctl and never calls pods.Delete. Since #286 removed the repmgrd
# service-updater -- the only thing that ever used it -- the verb is gone entirely.
agent_rbac_fence=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.splitBrainDetection.action=fence --show-only templates/rbac.yaml 2>&1)
assert_not_contains "agent rbac: fence mode still grants NO pods delete (soft fence is local)" "${agent_rbac_fence}" '"delete"'
# #286: there is no longer ANY mode that grants pods delete -- the repmgrd
# service-updater was the only consumer of that verb.

# agent pgpool backends front the RW/RO Services, failover off, health checks on
agent_pgpool=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent-pgpool.yaml" \
  --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "agent pgpool: backend0 RW Service ALWAYS_PRIMARY" "${agent_pgpool}" "backend_flag0 = 'ALWAYS_PRIMARY|DISALLOW_TO_FAILOVER'"
assert_contains "agent pgpool: backend1 is the RO Service" "${agent_pgpool}" "test-pg-readonly"
assert_contains "agent pgpool: failover disabled" "${agent_pgpool}" "failover_command = ''"
assert_contains "agent pgpool: fail_over_on_backend_error off" "${agent_pgpool}" "fail_over_on_backend_error = off"
assert_contains "agent pgpool: sr_check stays on" "${agent_pgpool}" "sr_check_period = 10"
agent_hc=$(printf '%s\n' "${agent_pgpool}" | awk '/health_check_period =/{print $3; exit}')
[ "${agent_hc:-0}" -gt 0 ] && agent_hc_ok=ok || agent_hc_ok="period=${agent_hc:-0}"
assert_eq "agent pgpool: health_check_period non-zero" "ok" "${agent_hc_ok}"

# agent NetworkPolicy opens the 9200 metrics ingress; apiserver egress present
agent_np=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "agent netpol: 9200 metrics ingress present" "${agent_np}" "port: 9200"
assert_contains "agent netpol: apiserver egress present" "${agent_np}" "port: 6443"

# agent monitoring (Part H6): headless Service exposes the metrics port; the
# ServiceMonitor + PrometheusRule render only in agent mode AND when enabled.
agent_headless=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/service-headless.yaml 2>&1)
assert_contains "agent headless: exposes agent-metrics port" "${agent_headless}" "name: agent-metrics"
# standalone has no agent, so the headless Service exposes no metrics port
standalone_headless=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --show-only templates/service-headless.yaml 2>&1)
assert_not_contains "standalone headless: no agent-metrics port" "${standalone_headless}" "agent-metrics"
agent_sm=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.monitoring.serviceMonitor.enabled=true --show-only templates/agent-servicemonitor.yaml 2>&1)
assert_contains "agent monitoring: ServiceMonitor scrapes agent-metrics" "${agent_sm}" "port: agent-metrics"
assert_contains "agent monitoring: ServiceMonitor is the operator CRD" "${agent_sm}" "kind: ServiceMonitor"
agent_pr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true --show-only templates/agent-prometheusrule.yaml 2>&1)
assert_contains "agent monitoring: PrometheusRule has the no-leader alert" "${agent_pr}" "PGHAAgentNoLeader"
assert_contains "agent monitoring: PrometheusRule has the split-brain alert" "${agent_pr}" "PGHAAgentMultipleLeaders"
assert_contains "agent monitoring: rules scoped to this release's headless Service" "${agent_pr}" "test-pg-headless"
# disabled by default: neither monitoring template renders
agent_sm_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/agent-servicemonitor.yaml 2>&1 || true)
assert_not_contains "agent monitoring: ServiceMonitor off by default" "${agent_sm_off}" "kind: ServiceMonitor"
# agent-gated: not rendered in standalone mode even if enabled (there is no agent)
agent_pr_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true --show-only templates/agent-prometheusrule.yaml 2>&1 || true)
assert_not_contains "agent monitoring: PrometheusRule not in standalone mode" "${agent_pr_standalone}" "kind: PrometheusRule"

# #289: the replication-slot alerts. An orphaned slot pins WAL and fills the volume with
# no error until the disk is full, so these two rules are the only thing that turns that
# silent failure into a page -- assert both the rules and the threshold plumbing.
assert_contains "#289: PrometheusRule has the retained-WAL alert" "${agent_pr}" "PGHAReplicationSlotRetainingWAL"
assert_contains "#289: PrometheusRule has the inactive-slot alert" "${agent_pr}" "PGHAReplicationSlotInactive"
assert_contains "#289: retained-WAL alert reads the max-retained gauge" "${agent_pr}" "pg_ha_agent_replication_slot_max_retained_wal_bytes"
assert_contains "#289: inactive-slot alert reads the inactive gauge" "${agent_pr}" "pg_ha_agent_replication_slots_inactive"
# The default threshold must render as a bare integer, not 1.7179869184e+10: Prometheus
# rejects scientific notation in a comparison, so a float-rendered default would ship an
# alert that never evaluates.
assert_contains "#289: default retained-WAL threshold renders as an integer (3Gi)" "${agent_pr}" "> 3221225472"
# The default MUST stay below the image's max_slot_wal_keep_size = 4GB (4294967296). Past
# that cap PostgreSQL invalidates the slot rather than letting it fill the volume, and
# invalidation nulls restart_lsn so this gauge collapses to zero -- a threshold at or above
# the cap renders an alert no slot can ever trip. The earlier 16Gi default did exactly that.
threshold=$(printf '%s\n' "${agent_pr}" | sed -n 's/.*replication_slot_max_retained_wal_bytes.*> \([0-9][0-9]*\).*/\1/p' | head -1 || true)
if [ -z "${threshold}" ] || [ "${threshold}" -ge 4294967296 ]; then
  fail "#289: default threshold is reachable below max_slot_wal_keep_size" \
    "rendered threshold '${threshold}' is not below the image's 4GB max_slot_wal_keep_size, so the alert can never fire"
else
  pass "#289: default threshold is reachable below max_slot_wal_keep_size"
fi
# The invalidated-slot alert is the one that catches the outcome the retained-WAL gauge
# cannot see, so its absence is a silent hole rather than a missing nicety.
assert_contains "#289: PrometheusRule has the invalidated-slot alert" "${agent_pr}" "PGHAReplicationSlotInvalidated"
assert_contains "#289: invalidated-slot alert reads the invalidated gauge" "${agent_pr}" "pg_ha_agent_replication_slots_invalidated"
assert_not_contains "#289: threshold is not rendered in scientific notation" "${agent_pr}" "e+"
# The threshold is operator-tunable, and the override must reach the expression.
agent_pr_thresh=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true \
  --set repmgr.agent.monitoring.prometheusRule.slotRetainedWALBytes=1073741824 \
  --show-only templates/agent-prometheusrule.yaml 2>&1)
assert_contains "#289: slotRetainedWALBytes override reaches the alert expression" "${agent_pr_thresh}" "> 1073741824"
assert_not_contains "#289: overridden threshold replaces the default" "${agent_pr_thresh}" "> 3221225472"
# A non-integer or zero threshold would render an alert that never fires -- rejected by
# the schema at render time rather than shipping a silently-dead rule.
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true \
  --set repmgr.agent.monitoring.prometheusRule.slotRetainedWALBytes=lots \
  >/dev/null 2>&1 && slot_thresh_str_rc=0 || slot_thresh_str_rc=$?
assert_eq "#289: a non-integer slotRetainedWALBytes fails the render" "1" "${slot_thresh_str_rc}"
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true \
  --set repmgr.agent.monitoring.prometheusRule.slotRetainedWALBytes=0 \
  >/dev/null 2>&1 && slot_thresh_zero_rc=0 || slot_thresh_zero_rc=$?
assert_eq "#289: a zero slotRetainedWALBytes fails the render" "1" "${slot_thresh_zero_rc}"

# agent etcd DCS (BYO/shared): backend selectable; etcd env + TLS mount, the leases
# RBAC dropped, NetworkPolicy egress to 2379; the default kubernetes backend is
# unaffected; missing endpoints fails fast.
agent_etcd=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' \
  --set 'repmgr.agent.dcs.etcd.endpoints={https://e1:2379,https://e2:2379}' \
  --set 'repmgr.agent.dcs.etcd.tls.secretName=etcd-tls' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "agent etcd: DCS_BACKEND=etcd" "${agent_etcd}" 'value: "etcd"'
assert_contains "agent etcd: endpoints joined into ETCD_ENDPOINTS" "${agent_etcd}" "https://e1:2379,https://e2:2379"
assert_contains "agent etcd: ETCD_PREFIX defaulted to /pg-ha/<release>/" "${agent_etcd}" "/pg-ha/test-pg/"
assert_contains "agent etcd: TLS cert path env" "${agent_etcd}" "/etc/etcd-tls/tls.crt"
assert_contains "agent etcd: TLS secret mounted" "${agent_etcd}" 'secretName: "etcd-tls"'
agent_k8s_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "agent k8s: DCS_BACKEND=kubernetes (default)" "${agent_k8s_sts}" 'value: "kubernetes"'
assert_not_contains "agent k8s: no etcd env in kubernetes mode" "${agent_k8s_sts}" "ETCD_ENDPOINTS"
agent_etcd_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'repmgr.agent.dcs.etcd.endpoints={https://e1:2379}' \
  --show-only templates/rbac.yaml 2>&1)
assert_not_contains "agent etcd: leases RBAC dropped (leadership not in apiserver)" "${agent_etcd_rbac}" "coordination.k8s.io"
assert_contains "agent k8s: leases RBAC present (default backend)" "${agent_rbac}" "coordination.k8s.io"
agent_etcd_np=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'repmgr.agent.dcs.etcd.endpoints={https://e1:2379}' \
  --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "agent etcd: NetworkPolicy egress to etcd 2379" "${agent_etcd_np}" "port: 2379"
etcd_noeps_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --show-only templates/statefulset.yaml >/dev/null 2>&1 || etcd_noeps_rc=$?
assert_eq "agent etcd: missing endpoints fails fast" "1" "$([ "${etcd_noeps_rc}" -ne 0 ] && echo 1 || echo 0)"

# bundled etcd subchart (conditional dependency): enabling it deploys the 3-node
# cluster and the agent auto-targets the bundled client Service; disabled by default.
agent_bundled=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'etcd.enabled=true' 2>&1)
assert_contains "bundled etcd: subchart StatefulSet renders" "${agent_bundled}" "name: test-pg-etcd"
assert_contains "bundled etcd: 3-member static initial-cluster" "${agent_bundled}" "test-pg-etcd-2=http://test-pg-etcd-2"
assert_contains "bundled etcd: Parallel pod management for static bootstrap" "${agent_bundled}" "podManagementPolicy: Parallel"
assert_contains "bundled etcd: agent auto-targets the bundled client Service" "${agent_bundled}" 'value: "http://test-pg-etcd:2379"'
assert_contains "bundled etcd: image is digest-pinned" "${agent_bundled}" "quay.io/coreos/etcd:v3.5.16@sha256:"
agent_byo_nobundle=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'repmgr.agent.dcs.etcd.endpoints={https://e:2379}' 2>&1)
assert_not_contains "bundled etcd: not rendered when etcd.enabled=false (BYO)" "${agent_byo_nobundle}" "test-pg-etcd-headless"
# bundled etcd ships an ingress NetworkPolicy (plaintext etcd must not be open
# namespace-wide): agent (postgresql) on the client port + the peer mesh, default on.
agent_etcd_np2=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'etcd.enabled=true' \
  --show-only charts/etcd/templates/networkpolicy.yaml 2>&1)
assert_contains "bundled etcd: ships an ingress NetworkPolicy" "${agent_etcd_np2}" "kind: NetworkPolicy"
assert_contains "bundled etcd: NP admits the agent (postgresql) on the client port" "${agent_etcd_np2}" "app.kubernetes.io/component: postgresql"
assert_not_contains "bundled etcd: no namespaceSelector without allowedClients" "${agent_etcd_np2}" "namespaceSelector"
# allowedClients: cross-namespace client allow-list for a shared/standalone etcd
# (#183); the bundled subchart and the standalone chart share the template. Render
# two entries via the bundled path (proves the re-vendored subchart carries it):
# [0] default podSelector, [1] an explicit custom podSelector.
etcd_allow=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'etcd.enabled=true' \
  --set 'etcd.networkPolicy.allowedClients[0].namespace=backend' \
  --set 'etcd.networkPolicy.allowedClients[1].namespace=sentry' \
  --set 'etcd.networkPolicy.allowedClients[1].podSelector.app\.kubernetes\.io/instance=sentry-pg' \
  --show-only charts/etcd/templates/networkpolicy.yaml 2>&1)
# both entries render (the range loop) -- exactly two namespaceSelectors
assert_contains "bundled etcd: allowedClients first namespace renders" "${etcd_allow}" "kubernetes.io/metadata.name: backend"
assert_contains "bundled etcd: allowedClients second namespace renders" "${etcd_allow}" "kubernetes.io/metadata.name: sentry"
ac_ns_count=$(printf '%s\n' "${etcd_allow}" | grep -c "kubernetes.io/metadata.name:")
assert_eq "bundled etcd: exactly two allowedClients namespaceSelectors" "2" "${ac_ns_count}"
# default podSelector branch -- scoped to the FIRST entry (not a doc-wide grep that
# would also match the built-in same-release rule and the peer mesh)
ac_default=$(printf '%s\n' "${etcd_allow}" | grep -A5 "kubernetes.io/metadata.name: backend")
assert_contains "bundled etcd: entry without podSelector defaults to the postgresql component" "${ac_default}" "app.kubernetes.io/component: postgresql"
# custom podSelector branch -- scoped to the SECOND entry; the default must NOT fire there
ac_custom=$(printf '%s\n' "${etcd_allow}" | grep -A5 "kubernetes.io/metadata.name: sentry")
assert_contains "bundled etcd: custom podSelector is used verbatim" "${ac_custom}" "app.kubernetes.io/instance: sentry-pg"
assert_not_contains "bundled etcd: custom entry does not also get the default component label" "${ac_custom}" "component: postgresql"
# standalone etcd chart (now published on its own): allowedClients renders, and a
# missing namespace fails fast with the required-guard message (not just any error).
etcd_standalone=$(helm template platform-etcd "${SCRIPT_DIR}/../../etcd" \
  --set 'networkPolicy.allowedClients[0].namespace=backend' \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "standalone etcd: allowedClients namespaceSelector renders" "${etcd_standalone}" "kubernetes.io/metadata.name: backend"
etcd_allow_err=$(helm template platform-etcd "${SCRIPT_DIR}/../../etcd" \
  --set 'networkPolicy.allowedClients[0].podSelector.foo=bar' 2>&1 || true)
assert_contains "standalone etcd: missing namespace fails with the required-guard message" "${etcd_allow_err}" "allowedClients\[\].namespace is required"

# etcd transport TLS (#184): client + peer TLS from a BYO Secret, mutual auth, and
# exec health probes. Default (off) must stay byte-stable; on must flip every URL to
# https, mount the cert Secret, and require existingSecret.
etcd_tls=$(helm template e "${SCRIPT_DIR}/../../etcd" \
  --set tls.enabled=true --set tls.existingSecret=etcd-server-tls \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "etcd TLS: client URLs are https" "${etcd_tls}" "value: https://0.0.0.0:2379"
assert_contains "etcd TLS: peer mesh URLs are https in initial-cluster" "${etcd_tls}" "https://e-etcd-0.e-etcd-headless"
assert_contains "etcd TLS: client cert-file env" "${etcd_tls}" "value: /etc/etcd/tls/tls.crt"
# assert the VALUE (true), not just the env name -- the name renders regardless of the setting
cca_on=$(printf '%s\n' "${etcd_tls}" | grep -A1 "name: ETCD_CLIENT_CERT_AUTH" | grep "value:")
assert_contains "etcd TLS: client-cert-auth (mutual TLS) on by default" "${cca_on}" 'value: "true"'
assert_contains "etcd TLS: peer cert-file env" "${etcd_tls}" "name: ETCD_PEER_CERT_FILE"
# clientCertAuth=false (encrypt-only): the value flips to false
etcd_tls_nocca=$(helm template e "${SCRIPT_DIR}/../../etcd" \
  --set tls.enabled=true --set tls.existingSecret=s --set tls.clientCertAuth=false \
  --show-only templates/statefulset.yaml 2>&1)
cca_off=$(printf '%s\n' "${etcd_tls_nocca}" | grep -A1 "name: ETCD_CLIENT_CERT_AUTH" | grep "value:")
assert_contains "etcd TLS: client-cert-auth can be disabled (encrypt-only)" "${cca_off}" 'value: "false"'
# peer.enabled=false: client URLs stay https but the peer mesh stays http (mixed scheme)
etcd_tls_nopeer=$(helm template e "${SCRIPT_DIR}/../../etcd" \
  --set tls.enabled=true --set tls.existingSecret=s --set tls.peer.enabled=false \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "etcd TLS: client URLs https with peer TLS off" "${etcd_tls_nopeer}" "value: https://0.0.0.0:2379"
assert_contains "etcd TLS: peer mesh stays http with peer TLS off" "${etcd_tls_nopeer}" "e-etcd-0=http://e-etcd-0.e-etcd-headless"
assert_not_contains "etcd TLS: no peer cert env with peer TLS off" "${etcd_tls_nopeer}" "ETCD_PEER_CERT_FILE"
assert_contains "etcd TLS: health probe uses etcdctl with the certs" "${etcd_tls}" "/usr/local/bin/etcdctl"
assert_contains "etcd TLS: cert Secret mounted" "${etcd_tls}" 'secretName: "etcd-server-tls"'
# peer secret defaults to the server secret -- one mount, no separate peer volume
assert_not_contains "etcd TLS: no separate peer volume when peer reuses the server secret" "${etcd_tls}" "etcd-peer-tls"
# a distinct peer secret mounts separately
etcd_tls_peer=$(helm template e "${SCRIPT_DIR}/../../etcd" \
  --set tls.enabled=true --set tls.existingSecret=etcd-server-tls \
  --set tls.peer.existingSecret=etcd-peer-tls --show-only templates/statefulset.yaml 2>&1)
assert_contains "etcd TLS: distinct peer Secret mounts separately" "${etcd_tls_peer}" 'secretName: "etcd-peer-tls"'
# default render (TLS off) is byte-stable: no https, no cert env, no etcdctl probe
etcd_plain=$(helm template e "${SCRIPT_DIR}/../../etcd" --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "etcd TLS off: no https URLs" "${etcd_plain}" "https://"
assert_not_contains "etcd TLS off: no cert env" "${etcd_plain}" "ETCD_CERT_FILE"
# fail-fast: tls.enabled without a Secret
etcd_tls_nosecret_rc=0
helm template e "${SCRIPT_DIR}/../../etcd" --set tls.enabled=true >/dev/null 2>&1 || etcd_tls_nosecret_rc=$?
assert_eq "etcd TLS: tls.enabled without existingSecret fails fast" "1" "$([ "${etcd_tls_nosecret_rc}" -ne 0 ] && echo 1 || echo 0)"
# parent wiring: bundled etcd with TLS -> agent connects over https; missing the
# agent client cert under client-cert-auth fails fast.
agent_etcd_tls=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.dcs.backend=etcd --set etcd.enabled=true \
  --set etcd.tls.enabled=true --set etcd.tls.existingSecret=etcd-server-tls \
  --set repmgr.agent.dcs.etcd.tls.secretName=etcd-client-tls \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "bundled etcd TLS: agent endpoint is https" "${agent_etcd_tls}" 'value: "https://test-pg-etcd:2379"'
agent_etcd_tls_nocert_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.dcs.backend=etcd --set etcd.enabled=true \
  --set etcd.tls.enabled=true --set etcd.tls.existingSecret=etcd-server-tls \
  --show-only templates/statefulset.yaml >/dev/null 2>&1 || agent_etcd_tls_nocert_rc=$?
assert_eq "bundled etcd TLS: missing agent client cert fails fast" "1" "$([ "${agent_etcd_tls_nocert_rc}" -ne 0 ] && echo 1 || echo 0)"

# etcd RBAC (#184): per-tenant key-prefix isolation via a CN-keyed bootstrap Job.
etcd_rbac=$(helm template platform-etcd "${SCRIPT_DIR}/../../etcd" \
  --set tls.enabled=true --set tls.existingSecret=etcd-server-tls \
  --set rbac.enabled=true --set rbac.adminSecret=etcd-admin-tls \
  --set 'rbac.tenants[0].commonName=backend-pg' --set 'rbac.tenants[0].prefix=/pg-ha/backend/' \
  --set 'rbac.tenants[1].commonName=sentry-pg' --set 'rbac.tenants[1].prefix=/pg-ha/sentry/' \
  --show-only templates/rbac-bootstrap-job.yaml 2>&1)
assert_contains "etcd RBAC: bootstrap Job renders" "${etcd_rbac}" "kind: Job"
assert_contains "etcd RBAC: runs as a post-install/upgrade hook" "${etcd_rbac}" "post-install,post-upgrade"
# the etcd image is distroless, so the Job runs the agent subcommand, not a shell/etcdctl
assert_contains "etcd RBAC: runs pg-ha-agent rbac-bootstrap" "${etcd_rbac}" "rbac-bootstrap"
assert_contains "etcd RBAC: uses the agent (repmgr) image, not the etcd image" "${etcd_rbac}" "cagriekin/repmgr:"
# tenants travel as a JSON env (commonName/prefix); the value is YAML-quoted, so
# match on the unescaped tokens rather than the escaped-quote punctuation.
assert_contains "etcd RBAC: tenants env is JSON" "${etcd_rbac}" "ETCD_RBAC_TENANTS"
assert_contains "etcd RBAC: JSON carries commonName/prefix keys" "${etcd_rbac}" "commonName"
assert_contains "etcd RBAC: first tenant CN + prefix present" "${etcd_rbac}" "backend-pg"
assert_contains "etcd RBAC: first tenant prefix present" "${etcd_rbac}" "/pg-ha/backend/"
assert_contains "etcd RBAC: second tenant present" "${etcd_rbac}" "sentry-pg"
assert_contains "etcd RBAC: root CN env" "${etcd_rbac}" "ETCD_RBAC_ROOT_CN"
assert_contains "etcd RBAC: connects to etcd over https" "${etcd_rbac}" 'value: "https://platform-etcd-etcd:2379"'
assert_contains "etcd RBAC: admin cert mounted" "${etcd_rbac}" 'secretName: "etcd-admin-tls"'
# the etcd image (distroless) is NOT used for the Job (no shell there)
assert_not_contains "etcd RBAC: Job does not use the distroless etcd image" "${etcd_rbac}" "quay.io/coreos/etcd"
# default off: the bootstrap Job is absent from the full render (--show-only would
# error on an empty template and leak the filename, so render the whole chart).
etcd_norbac=$(helm template platform-etcd "${SCRIPT_DIR}/../../etcd" 2>&1)
assert_not_contains "etcd RBAC: no Job when disabled" "${etcd_norbac}" "rbac-bootstrap"
# fail-fast: rbac without tls, and rbac+tls without adminSecret
etcd_rbac_notls_rc=0
helm template e "${SCRIPT_DIR}/../../etcd" --set rbac.enabled=true --set rbac.adminSecret=a >/dev/null 2>&1 || etcd_rbac_notls_rc=$?
assert_eq "etcd RBAC: rbac.enabled without tls fails fast" "1" "$([ "${etcd_rbac_notls_rc}" -ne 0 ] && echo 1 || echo 0)"
etcd_rbac_noadmin_rc=0
helm template e "${SCRIPT_DIR}/../../etcd" --set tls.enabled=true --set tls.existingSecret=s --set rbac.enabled=true >/dev/null 2>&1 || etcd_rbac_noadmin_rc=$?
assert_eq "etcd RBAC: rbac.enabled without adminSecret fails fast" "1" "$([ "${etcd_rbac_noadmin_rc}" -ne 0 ] && echo 1 || echo 0)"
# empty tenants must fail fast: enabling auth with only root would lock every
# CN-authenticated agent out of the keyspace (review #1).
etcd_rbac_notenants_rc=0
helm template e "${SCRIPT_DIR}/../../etcd" --set tls.enabled=true --set tls.existingSecret=s \
  --set rbac.enabled=true --set rbac.adminSecret=a >/dev/null 2>&1 || etcd_rbac_notenants_rc=$?
assert_eq "etcd RBAC: rbac.enabled with no tenants fails fast (avoids agent lockout)" "1" "$([ "${etcd_rbac_notenants_rc}" -ne 0 ] && echo 1 || echo 0)"
# fail-fast on contradictory etcd config (bundled deployed but unused)
etcd_orphan_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" --set 'etcd.enabled=true' >/dev/null 2>&1 || etcd_orphan_rc=$?
assert_eq "bundled etcd: enabled without the etcd backend fails fast" "1" "$([ "${etcd_orphan_rc}" -ne 0 ] && echo 1 || echo 0)"
etcd_both_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set 'repmgr.agent.dcs.backend=etcd' --set 'etcd.enabled=true' --set 'repmgr.agent.dcs.etcd.endpoints={https://e:2379}' >/dev/null 2>&1 || etcd_both_rc=$?
assert_eq "bundled etcd: enabled + external endpoints fails fast" "1" "$([ "${etcd_both_rc}" -ne 0 ] && echo 1 || echo 0)"

# postgresql PDB: maxUnavailable + unhealthyPodEvictionPolicy (the 1.0.0 default
# change), NOT minAvailable. Applies in both modes (base values).
pdb=$(helm template test-pg "${CHART_DIR}" --show-only templates/pdb-postgresql.yaml 2>&1)
assert_contains "pdb: postgresql uses maxUnavailable: 1" "${pdb}" "maxUnavailable: 1"
assert_contains "pdb: postgresql sets unhealthyPodEvictionPolicy: AlwaysAllow" "${pdb}" "unhealthyPodEvictionPolicy: AlwaysAllow"
assert_not_contains "pdb: postgresql no longer uses minAvailable" "${pdb}" "minAvailable"

# #161: pgpool PDB uses maxUnavailable (not minAvailable: 1) so a single-replica
# pgpool can still be evicted on a node drain instead of wedging it forever.
pdb_pgpool=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --show-only templates/pdb-pgpool.yaml 2>&1)
assert_contains "#161: pgpool PDB uses maxUnavailable: 1" "${pdb_pgpool}" "maxUnavailable: 1"
assert_contains "#161: pgpool PDB sets unhealthyPodEvictionPolicy: AlwaysAllow" "${pdb_pgpool}" "unhealthyPodEvictionPolicy: AlwaysAllow"
assert_not_contains "#161: pgpool PDB no longer uses minAvailable" "${pdb_pgpool}" "minAvailable"
# #161: an explicit minAvailable override must render ONE field, never both (the API
# rejects a PDB with both min+maxUnavailable). minAvailable wins over the default maxUnavailable.
pdb_override=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --set pgpool.podDisruptionBudget.minAvailable=1 --show-only templates/pdb-pgpool.yaml 2>&1)
assert_contains "#161: explicit minAvailable override is honored" "${pdb_override}" "minAvailable: 1"
assert_not_contains "#161: PDB never renders both minAvailable and maxUnavailable" "${pdb_override}" "maxUnavailable"

# values-cloud.yaml preset: 3-node, hard zone spread, managed-cloud lease timings.
# Off by default (base renders no DoNotSchedule spread).
cloud=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/../values-cloud.yaml" --show-only templates/statefulset.yaml 2>&1)
assert_contains "cloud preset: 3-node cluster (replicas: 3)" "${cloud}" "replicas: 3"
assert_contains "cloud preset: hard zone topologySpread" "${cloud}" "whenUnsatisfiable: DoNotSchedule"
assert_contains "cloud preset: managed-cloud lease timing (30s)" "${cloud}" 'value: "30s"'
base_sts_spread=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "cloud preset: hard zone spread off by default" "${base_sts_spread}" "whenUnsatisfiable: DoNotSchedule"

# #286: repmgrd is gone -- the HA render must carry neither sidecar, in any mode.
default_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#286: no repmgrd container in any render" "${default_sts}" "name: repmgrd"
assert_not_contains "#286: no service-updater container in any render" "${default_sts}" "name: service-updater"
assert_contains "#286: the HA arm runs the agent entrypoint" "${default_sts}" '"/usr/local/bin/entrypoint.sh", "agent"'

# #286: the removed repmgrd keys fail the render rather than being silently ignored --
# a values file still pinning failoverMode: repmgrd must not quietly deploy an agent.
for removed in repmgr.failoverMode=repmgrd repmgr.failoverMode=agent \
               repmgr.serviceUpdater.resources.requests.cpu=50m \
               repmgr.monitoringHistoryDays=7 pgpool.autoFailback=true; do
  removed_rc=0
  removed_out=$(helm template test-pg "${CHART_DIR}" --set "${removed}" 2>&1) || removed_rc=$?
  assert_eq "#286: ${removed%%=*} is rejected at render time" "1" "$([ "${removed_rc}" -ne 0 ] && echo 1 || echo 0)"
  assert_contains "#286: ${removed%%=*} rejection names the 2.0.0 removal" "${removed_out}" "removed in chart 2.0.0"
done
# The repmgrd rejection must point at the immutable-field consequence, since deleting the
# key silently flips podManagementPolicy on an existing StatefulSet.
repmgrd_msg=$(helm template test-pg "${CHART_DIR}" --set repmgr.failoverMode=repmgrd 2>&1 || true)
assert_contains "#286: repmgrd rejection warns about podManagementPolicy" "${repmgrd_msg}" "IMMUTABLE"

# values.schema.json: enum guards reject typos at template/install time.
schema_bad_dcs_rc=0
helm template test-pg "${CHART_DIR}" --set repmgr.agent.dcs.backend=zookeeper >/dev/null 2>&1 || schema_bad_dcs_rc=$?
assert_eq "schema: invalid dcs.backend rejected" "1" "$([ "${schema_bad_dcs_rc}" -ne 0 ] && echo 1 || echo 0)"

# rbac grants configmap access for the marker
rbac_repmgr=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "su #125: rbac grants configmaps verbs" "${rbac_repmgr}" "configmaps"
# The marker stays runtime-owned (the agent creates/updates it through the API), not a
# helm template, so a helm upgrade / ArgoCD sync cannot reset the highwater. The write
# path is Go now -- internal/k8s/marker.go, covered by its own tests.
assert_not_contains "#286: no marker ConfigMap rendered by helm" "${repmgr}" "test-pg-primary\n"

# --- #171/#173/#174/#138/#140: behaviour that moved into the agent with #286 ---
# These were behavioural unit tests of shell functions extracted from the rendered
# service-updater ConfigMap. That ConfigMap and its script are gone; the same logic now
# lives in Go and is tested there:
#   - read_marker / evaluate_lone_primary corrupt-vs-absent marker, fail-closed on an
#     unreadable timeline, equal-timeline split-brain (#174/#173/#171)
#       -> internal/k8s/marker.go, internal/reconcile
#   - LAST_MASTER seeding from the live write-Service selector (#138)
#       -> the agent reconciles Service selectors from the Lease every tick, so there is
#          no cross-restart process state to seed
#   - pg-role labeling of non-master pods: standby only when actually in recovery,
#     orphan when not, untouched when unreachable (#140)
#       -> internal/k8s (role labeling), covered by its own tests
# What the chart still owns -- that the marker is runtime-owned rather than templated,
# and that the RBAC verbs exist for it -- is asserted above.

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
# #119: the verify streams the dump to pg_restore (no unbounded /tmp buffer that could
# fill the node on a large DB).
assert_contains "#119: backup verify is streamed to pg_restore" "${backup_configmap}" "| pg_restore --list"
assert_not_contains "#119: backup verify does not buffer the dump to /tmp" "${backup_configmap}" "verify_backup.dump"
# #230: the integrity check must inspect BOTH ends of the pipe via PIPESTATUS, not rely
# on pipefail. pg_restore --list closes the stream after the TOC, so on any dump > the
# ~64 KB pipe buffer mc cat is SIGPIPE-killed (141) and bare pipefail would abort the Job
# before publishing. The fix disables errexit+pipefail around the pipe so it can require
# pg_restore success while tolerating ONLY mc cat's SIGPIPE -- a real mc cat read error
# stays fatal -- and so the diagnostics run instead of being skipped by set -e at the pipe.
# Needles are regex-safe (assert_contains greps as BRE): escaped brackets, no ${ }.
assert_contains "#230: integrity check snapshots both pipe statuses via PIPESTATUS" "${backup_configmap}" 'PIPESTATUS\[@\]'
assert_contains "#230: errexit+pipefail disabled around the integrity pipe" "${backup_configmap}" "set +o pipefail +e"
# ...and both are restored afterward, so the rest of backup.sh keeps its fail-fast
# behavior (the disable is scoped to the integrity pipe only).
assert_contains "#230: errexit+pipefail restored after the integrity pipe" "${backup_configmap}" "set -e -o pipefail"
# The consumer (pg_restore) must succeed...
assert_contains "#230: pg_restore (consumer) failure is fatal" "${backup_configmap}" "rc_restore"
# ...and a non-SIGPIPE mc cat (producer) failure stays fatal -- only 141 is tolerated.
assert_contains "#230: only mc cat SIGPIPE (141) is tolerated, other producer failures fatal" "${backup_configmap}" "ne 141"
# #143: dumps are namespaced per release and retention is scoped to this release's
# own dump objects, so a shared bucket/prefix can never delete another release's backups.
# needles are regex-safe (assert_contains greps as a regex): avoid the *. in backup_*.dump
assert_contains "backup #143: dump path namespaced per release (fullname)" "${backup_configmap}" 'S3_DIR="${S3_BUCKET}/${S3_PREFIX%/}/test-pg"'
assert_contains "backup #143: find calls scoped to the release subpath + name filter" "${backup_configmap}" 'mc find "s3/${S3_DIR}/" --name '"'"'backup_'
assert_not_contains "backup #143: retention not run over the bare shared prefix" "${backup_configmap}" 'mc find "s3/${S3_BUCKET}/${S3_PREFIX}/" --older-than'
# #167 + #221: S3 credentials must not appear in mc argv (/proc/<pid>/cmdline),
# AND must not be percent-encoded into an MC_HOST URL -- mc signs SigV4 with the
# encoded secret, so a key containing '/' or '+' (common in real AWS keys) fails
# with a signature mismatch (#221). Credentials are imported from a 0600 JSON doc
# instead, which feeds the RAW secret to the signer.
assert_not_contains "backup #167: no mc alias set with the endpoint in argv" "${backup_configmap}" 'mc alias set s3 "$S3_ENDPOINT"'
assert_not_contains "backup #221: credentials NOT percent-encoded into an MC_HOST URL" "${backup_configmap}" "export MC_HOST_s3="
assert_contains "backup #221: credentials imported from a JSON doc" "${backup_configmap}" 'mc alias import s3 "$ALIAS_FILE"'
assert_contains "backup #221: alias doc carries url/accessKey/secretKey" "${backup_configmap}" '"url":"%s","accessKey":"%s","secretKey":"%s"'
assert_contains "backup #221: alias doc written with a restrictive umask (0600 contract)" "${backup_configmap}" "umask 077"
# The block above renders validation disabled, so it only exercises backup.sh.
# validate.sh shares the credential path and must not silently regress to the
# MC_HOST URL -- render it and assert the same contract over just that script.
backup_val_configmap=$(helm template test-pg "${CHART_DIR}" \
  --set backup.enabled=true \
  --set backup.validation.enabled=true \
  --set backup.s3.endpoint=https://s3.test \
  --set backup.s3.bucket=test \
  --set backup.existingSecret.name=test-secret \
  --show-only templates/backup-configmap.yaml 2>&1)
validate_sh=$(printf '%s\n' "${backup_val_configmap}" | sed -n '/validate.sh: |/,$p')
assert_not_contains "validate #221: credentials NOT percent-encoded into an MC_HOST URL" "${validate_sh}" "export MC_HOST_s3="
assert_contains "validate #221: credentials imported from a JSON doc" "${validate_sh}" 'mc alias import s3 "$ALIAS_FILE"'
assert_contains "validate #221: alias doc written with a restrictive umask (0600 contract)" "${validate_sh}" "umask 077"
# #221: json_escape is the load-bearing credential path -- it must leave the chars
# urlencode mangled ('/', '+', ':', '@', '=') UNTOUCHED (so SigV4 sees the raw
# secret) and escape only JSON metacharacters. Extract the function from the
# rendered script and run it against adversarial keys.
json_escape_fn=$(printf '%s\n' "${backup_configmap}" | awk '/json_escape\(\) \{/{f=1} f{print} f&&/^    \}$/{exit}' | sed 's/^    //')
if [ -n "${json_escape_fn}" ]; then
  json_escape_out=$(bash -c "${json_escape_fn}
json_escape 'a/b+c:d@e=f'")
  assert_eq "backup #221: json_escape leaves /,+,:,@,= untouched (was percent-encoded, broke SigV4)" 'a/b+c:d@e=f' "${json_escape_out}"
  json_escape_bs=$(bash -c "${json_escape_fn}"'
json_escape "$(printf '"'"'a\\b'"'"')"')
  assert_eq "backup #221: json_escape escapes a backslash for valid JSON" 'a\\b' "${json_escape_bs}"
else
  fail "backup #221: json_escape function extractable from the rendered script" "could not extract json_escape()"
fi
# #159: stage the dump to a .tmp object and publish (mc mv) to the canonical name only
# after integrity verification, so a truncated dump never sits at backup_<ts>.dump.
assert_contains "backup #159: pg_dump streams to the staging object" "${backup_configmap}" 'mc pipe "s3/${S3_TMP}"'
assert_contains "backup #159: verified dump published with mc mv" "${backup_configmap}" 'mc mv "s3/${S3_TMP}" "s3/${S3_PATH}"'
assert_contains "backup #159: staging removed on failure (EXIT trap)" "${backup_configmap}" 'mc rm "s3/${S3_TMP}"'
assert_not_contains "backup #159: pg_dump no longer streams directly to the canonical name" "${backup_configmap}" 'pg_dump -Fc -h "$POSTGRES_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" | mc pipe "s3/${S3_PATH}"'

assert_contains "backup: pod has runAsNonRoot" "${backup_cronjob}" "runAsNonRoot: true"
assert_contains "backup: container has allowPrivilegeEscalation false" "${backup_cronjob}" "allowPrivilegeEscalation: false"

# #166: pods that make no Kubernetes API calls must not mount an SA token; the repmgr
# StatefulSet (agent/service-updater DO call the API) must keep its token.
assert_contains "#166: backup pod disables SA token automount" "${backup_cronjob}" "automountServiceAccountToken: false"
pgpool_deploy166=$(helm template test-pg "${CHART_DIR}" --set pgpool.enabled=true --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "#166: pgpool pod disables SA token automount" "${pgpool_deploy166}" "automountServiceAccountToken: false"
exporter_deploy166=$(helm template test-pg "${CHART_DIR}" --set prometheusExporter.enabled=true --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
assert_contains "#166: exporter pod disables SA token automount" "${exporter_deploy166}" "automountServiceAccountToken: false"
sts_agent166=$(helm template test-pg "${CHART_DIR}" --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#166: repmgr StatefulSet keeps its SA token (agent needs the API)" "${sts_agent166}" "automountServiceAccountToken"
sts_standalone166=$(helm template test-pg "${CHART_DIR}" --set repmgr.enabled=false --set postgresql.replicaCount=0 --show-only templates/statefulset.yaml 2>&1)
assert_contains "#166: standalone StatefulSet disables SA token automount" "${sts_standalone166}" "automountServiceAccountToken: false"

# #199: the agent is the SINGLE author of pg_hba.conf -- the md5-first compat layer and
# the md5->scram re-hash live in the agent. The postStart md5 blocks that used to do this
# in repmgrd mode (and raced the agent, leaving rejoined standbys SCRAM-only) went with
# repmgrd itself (#286), so they must not appear in any render.
assert_not_contains "#199: no postStart md5-fallback awk" "${sts_agent166}" "md5 fallback applied"
assert_not_contains "#199: no postStart md5->scram re-hash" "${sts_agent166}" "fix_user_auth"
assert_contains "#199: MIGRATE_LEGACY_MD5_USERS reaches the agent" "${sts_agent166}" "MIGRATE_LEGACY_MD5_USERS"
# #199 + #144: user pgHba in agent mode flows through the agent (POSTGRESQL_PGHBA env),
# not the postStart insert (which now runs only in repmgrd mode).
sts_agent_hba=$(helm template test-pg "${CHART_DIR}" --set 'postgresql.pgHba[0]=host all custom 10.1.2.3/32 reject' --show-only templates/statefulset.yaml 2>&1)
assert_contains "#199: agent receives user pgHba via POSTGRESQL_PGHBA env" "${sts_agent_hba}" "host all custom 10.1.2.3/32 reject"
assert_not_contains "#199: agent mode drops the postStart user-pgHba insert" "${sts_agent_hba}" "user pgHba entries"

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

# #31: opt-in backup-validation CronJob (restore the latest dump into a throwaway
# PostgreSQL in the Job pod and fail on a bad restore).
backup_val_args="--set backup.enabled=true --set backup.s3.endpoint=https://s3.test --set backup.s3.bucket=test --set backup.existingSecret.name=test-secret"
# default: validation off -> no CronJob, no validate.sh
backup_noval_cm=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --show-only templates/backup-configmap.yaml 2>&1)
assert_not_contains "#31: validate.sh absent when validation disabled" "${backup_noval_cm}" "validate.sh:"
backup_noval_cj=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} 2>&1)
assert_not_contains "#31: no backup-validation CronJob when disabled" "${backup_noval_cj}" "backup-validation"
# enabled: CronJob + script render
backup_val_cj=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --set backup.validation.enabled=true --show-only templates/backup-validation-cronjob.yaml 2>&1)
assert_contains "#31: backup-validation CronJob present when enabled" "${backup_val_cj}" "name: test-pg-backup-validation"
assert_contains "#31: validation runs validate.sh" "${backup_val_cj}" "/scripts/validate.sh"
assert_contains "#31: validation makes no API calls (no SA token)" "${backup_val_cj}" "automountServiceAccountToken: false"
# runs initdb/postgres on the OFFICIAL postgres image (uid 999), so it must use the
# backup securityContext (999), not the repmgr-image uid 101 (which has no passwd
# entry in the official image -> initdb aborts). #shell-1 from the multi-pillar review.
assert_contains "#31: validation container runs as the official-postgres uid 999" "${backup_val_cj}" "runAsUser: 999"
assert_contains "#31: validation pod sets fsGroup 999 so initdb can write the emptyDir PGDATA" "${backup_val_cj}" "fsGroup: 999"
assert_contains "#31: validation schedule wired" "${backup_val_cj}" 'schedule: "0 3 \* \* 0"'
backup_val_cm=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --set backup.validation.enabled=true --show-only templates/backup-configmap.yaml 2>&1)
assert_contains "#31: validate.sh present when enabled" "${backup_val_cm}" "validate.sh:"
# Scope to the validate.sh stanza so these assert what validate.sh carries, not
# what backup.sh (in the same ConfigMap, with the identical S3_DIR line) carries.
validate_sh=$(printf '%s\n' "${backup_val_cm}" | sed -n '/validate.sh: |/,$p')
assert_contains "#31: validate.sh fails the Job on a bad restore (--exit-on-error)" "${validate_sh}" "pg_restore -h"
assert_contains "#31: validate.sh uses --exit-on-error" "${validate_sh}" "exit-on-error"
assert_contains "#31: validate.sh reads this release's scoped backup path" "${validate_sh}" 'S3_DIR="${S3_BUCKET}/${S3_PREFIX%/}/test-pg"'
# workdir emptyDir cap is configurable; default unbounded (emptyDir: {})
assert_contains "#31: workdir cap configurable" "$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --set backup.validation.enabled=true --set backup.validation.workdirSizeLimit=10Gi --show-only templates/backup-validation-cronjob.yaml 2>&1)" "sizeLimit: 10Gi"

# #38: opt-in pgBackRest PITR restore-validation CronJob -- restores the repo into a
# throwaway PostgreSQL, replays WAL, validates, exits. Distinct from the #31 pg_dump
# validation (this exercises the pgbackrest repo + WAL archive, the real DR mechanism).
pgbr_args="--set pgbackrest.enabled=true --set repmgr.enabled=true --set pgbackrest.s3.endpoint=https://s3.test --set pgbackrest.s3.bucket=b --set pgbackrest.existingSecret.name=s3sec"
pgbr_noval=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} 2>&1)
assert_not_contains "#38: no pgbackrest-validation CronJob when disabled" "${pgbr_noval}" "component: pgbackrest-validation"
pgbr_val=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)
assert_contains "#38: pgbackrest-validation CronJob present when enabled" "${pgbr_val}" "name: test-pg-pgbackrest-validation"
# Must restore into a THROWAWAY path, never the live data dir -- the whole safety premise.
assert_contains "#38: restores into a throwaway PGDATA (pg1-path override)" "${pgbr_val}" "name: PGBACKREST_PG1_PATH"
assert_contains "#38: throwaway path is /work, not the live /var/lib/postgresql/data" "${pgbr_val}" "value: /work/pgdata"
assert_not_contains "#38: never restores onto the live data directory" "${pgbr_val}" "/var/lib/postgresql/data/pgdata"
# The restore/validate program lives in the pgbackrest ConfigMap (mirrors #31), mounted
# and invoked by the CronJob -- assert the CronJob wires it...
assert_contains "#38: CronJob invokes the mounted validate.sh" "${pgbr_val}" "/scripts/validate.sh"
assert_contains "#38: CronJob mounts validate.sh from the configmap" "${pgbr_val}" "key: validate.sh"
# ...and assert the script content lives in the ConfigMap (gated on validation.enabled).
pgbr_script=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --show-only templates/pgbackrest-configmap.yaml 2>&1)
pgbr_noscript=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_not_contains "#38: validate.sh absent from the configmap when validation disabled" "${pgbr_noscript}" "validate.sh:"
assert_contains "#38: validate.sh present in the configmap when enabled" "${pgbr_script}" "validate.sh:"
assert_contains "#38: script runs pgbackrest restore" "${pgbr_script}" "pgbackrest --stanza="
assert_contains "#38: script starts a socket-only throwaway postgres (no network listener)" "${pgbr_script}" "listen_addresses=''"
# Safety: the promoted throwaway must NOT push its WAL into the production repo.
assert_contains "#38: script disables archive_mode so the throwaway never pollutes the prod repo" "${pgbr_script}" "archive_mode=off"
assert_contains "#38: script polls until recovery promotes to read-write" "${pgbr_script}" "pg_is_in_recovery()"
assert_contains "#38: script strips the unmountable include_dir to avoid a startup FATAL" "${pgbr_script}" "include_dir = '/etc/postgresql/conf.d'"
assert_contains "#38: script puts the PG bin dir on PATH (pg_ctl/psql are not on the default PATH)" "${pgbr_script}" "/usr/lib/postgresql"
# Runs from the repmgr image (has pgbackrest + matching PG major), as the repmgr SA
# (workload identity for keyType=auto), with no API token.
assert_contains "#38: uses the repmgr image" "${pgbr_val}" "cagriekin/repmgr"
assert_contains "#38: reuses the postgresql/repmgr ServiceAccount (workload identity)" "${pgbr_val}" "serviceAccountName: test-pg-repmgr"
assert_contains "#38: makes no API calls (no SA token)" "${pgbr_val}" "automountServiceAccountToken: false"
assert_contains "#38: keyType=shared wires the static S3 key from the secret" "${pgbr_val}" "name: PGBACKREST_REPO1_S3_KEY"
assert_contains "#38: schedule wired" "${pgbr_val}" 'schedule: "0 4 \* \* 0"'
assert_contains "#38: concurrencyPolicy inherited from pgbackrest.cronjob" "${pgbr_val}" "concurrencyPolicy: Forbid"
assert_contains "#38: workdir cap configurable" "$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --set pgbackrest.validation.workdirSizeLimit=20Gi --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)" "sizeLimit: 20Gi"
pgbr_val_limits=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --set pgbackrest.validation.activeDeadlineSeconds=7200 --set pgbackrest.validation.backoffLimit=3 --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)
assert_contains "#38: activeDeadlineSeconds override wired" "${pgbr_val_limits}" "activeDeadlineSeconds: 7200"
assert_contains "#38: backoffLimit override wired" "${pgbr_val_limits}" "backoffLimit: 3"
# keyType=auto must NOT emit static keys (relies on the SA's workload identity).
pgbr_val_auto=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set repmgr.enabled=true --set pgbackrest.s3.endpoint=https://s3.test --set pgbackrest.s3.bucket=b --set pgbackrest.s3.keyType=auto --set pgbackrest.validation.enabled=true --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)
assert_not_contains "#38: keyType=auto emits no static S3 key env" "${pgbr_val_auto}" "PGBACKREST_REPO1_S3_KEY"
# PITR target wiring + the guard that target is required once a targetType is set.
pgbr_val_pitr=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --set pgbackrest.validation.targetType=time --set-string 'pgbackrest.validation.target=2026-06-26 03:00:00+00' --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)
assert_contains "#38: PITR target value wired into the env" "${pgbr_val_pitr}" "2026-06-26 03:00:00+00"
# A targeted restore must promote, not pause: default recovery_target_action is `pause`.
assert_contains "#38: targeted restore promotes (not pause)" "${pgbr_script}" "target-action=promote"
# targetType set without target is rejected by the template fail guard (a schema if/then
# is not portable to helm 3.x). bogus targetType is rejected by the values.schema.json enum.
pgbr_val_badtarget=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --set pgbackrest.validation.targetType=time 2>&1 || true)
assert_contains "#38: targetType without target fails fast" "${pgbr_val_badtarget}" "validation.target is required"
pgbr_val_badtype=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.validation.enabled=true --set pgbackrest.validation.targetType=bogus 2>&1 || true)
assert_contains "#38: invalid targetType rejected" "${pgbr_val_badtype}" "must be one of"

# #226: first-class PITR restore resource -- replaces the hand-built `kubectl run
# --overrides` runbook. Restores over the LIVE data PVC (contrast #38, which restores into a
# throwaway) and never starts postgres: scaling the StatefulSet back up performs recovery.
assert_not_contains "#226: nothing rendered when restore is disabled (default)" "${pgbr_noval}" "component: pgbackrest-restore"
assert_not_contains "#226: restore.sh absent from the configmap when restore disabled" "${pgbr_noscript}" "restore.sh:"
pgbr_res=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --show-only templates/pgbackrest-restore-job.yaml 2>&1)
# Default mode is the suspended CronJob: `kubectl create job --from` clones it on demand.
assert_contains "#226: default mode renders a CronJob" "${pgbr_res}" "kind: CronJob"
assert_contains "#226: restore CronJob name is stable for kubectl create job --from" "${pgbr_res}" "name: test-pg-pgbackrest-restore"
# Load-bearing safety: enabling the feature must never restore anything by itself.
assert_contains "#226: restore CronJob is suspended" "${pgbr_res}" "suspend: true"
assert_contains "#226: restore CronJob schedule can never fire (Feb 31)" "${pgbr_res}" 'schedule: "0 0 31 2 \*"'
# The whole point: the live PVC + the repo config, mounted by the chart rather than by hand.
assert_contains "#226: mounts the live data PVC of replica 0" "${pgbr_res}" "claimName: data-test-pg-0"
assert_contains "#226: mounts the pgbackrest ConfigMap (repo settings + pg1-path)" "${pgbr_res}" "mountPath: /etc/pgbackrest/pgbackrest.conf"
# pg1-path in the mounted conf is ALREADY the live PGDATA, so (unlike #38) there must be no
# pg1-path override redirecting the restore somewhere else.
assert_not_contains "#226: no pg1-path override (the mounted conf already targets live PGDATA)" "${pgbr_res}" "PGBACKREST_PG1_PATH"
assert_contains "#226: PGDATA is the live data directory" "${pgbr_res}" "value: /var/lib/postgresql/data/pgdata"
assert_contains "#226: runs the mounted restore.sh" "${pgbr_res}" "/scripts/restore.sh"
assert_contains "#226: mounts restore.sh from the configmap" "${pgbr_res}" "key: restore.sh"
# Identity + security context inherited from the chart (the #38 plumbing).
assert_contains "#226: uses the repmgr image" "${pgbr_res}" "cagriekin/repmgr"
assert_contains "#226: reuses the postgresql/repmgr ServiceAccount (workload identity)" "${pgbr_res}" "serviceAccountName: test-pg-repmgr"
assert_contains "#226: makes no API calls (no SA token)" "${pgbr_res}" "automountServiceAccountToken: false"
assert_contains "#226: postgresql pod securityContext (fsGroup 103)" "${pgbr_res}" "fsGroup: 103"
assert_contains "#226: postgresql container securityContext (uid 101)" "${pgbr_res}" "runAsUser: 101"
assert_contains "#226: keyType=shared wires the static S3 key from the secret" "${pgbr_res}" "name: PGBACKREST_REPO1_S3_KEY"
assert_contains "#226: one pod attempt by default (no auto-retry over a half-written PGDATA)" "${pgbr_res}" "backoffLimit: 0"
# The restore script itself lives in the pgbackrest ConfigMap, gated on restore.enabled.
pgbr_res_script=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_contains "#226: restore.sh present in the configmap when enabled" "${pgbr_res_script}" "restore.sh:"
assert_contains "#226: script restores with --delta" "${pgbr_res_script}" "\-\-delta"
assert_contains "#226: script refuses to restore over a running postmaster" "${pgbr_res_script}" "postmaster.pid"
# It must NOT start postgres -- recovery happens on scale-up, under the chart entrypoint.
assert_not_contains "#226: script never starts postgres (pg_ctl start)" "${pgbr_res_script}" "pg_ctl -D"
assert_contains "#226: script puts the PG bin dir on PATH (pg_controldata is not on the default PATH)" "${pgbr_res_script}" "/usr/lib/postgresql"
# mode: job renders a bare Job (for `helm template -s ... | kubectl apply`), with a
# nameSuffix escape hatch because Jobs are immutable.
pgbr_res_job=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.mode=job --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: mode=job renders a Job" "${pgbr_res_job}" "kind: Job"
assert_not_contains "#226: mode=job renders no CronJob wrapper" "${pgbr_res_job}" "kind: CronJob"
assert_not_contains "#226: mode=job has no suspend field" "${pgbr_res_job}" "suspend:"
pgbr_res_suffix=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.mode=job --set pgbackrest.restore.nameSuffix=attempt2 --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: nameSuffix makes a rerun a distinct Job" "${pgbr_res_suffix}" "name: test-pg-pgbackrest-restore-attempt2"
# ...but the CronJob name must stay stable so the runbook command never changes.
pgbr_res_cj_suffix=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.nameSuffix=attempt2 --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: nameSuffix is ignored in cronjob mode (stable --from target)" "${pgbr_res_cj_suffix}" "name: test-pg-pgbackrest-restore"
assert_not_contains "#226: cronjob name carries no suffix" "${pgbr_res_cj_suffix}" "restore-attempt2"
# PITR target / backup-set / force / podOrdinal wiring.
pgbr_res_pitr=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.targetType=time --set-string 'pgbackrest.restore.target=2026-03-22 12:00:00+00' --set pgbackrest.restore.backupSet=20260322-010002F --set pgbackrest.restore.force=true --set pgbackrest.restore.podOrdinal=1 --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: PITR target value wired into the env" "${pgbr_res_pitr}" "2026-03-22 12:00:00+00"
assert_contains "#226: backupSet wired into the env" "${pgbr_res_pitr}" "20260322-010002F"
assert_contains "#226: force wired into the env" "${pgbr_res_pitr}" 'name: FORCE'
assert_contains "#226: podOrdinal selects that replica's PVC" "${pgbr_res_pitr}" "claimName: data-test-pg-1"
# A targeted restore must promote, not pause (default recovery_target_action is `pause`).
assert_contains "#226: targeted restore promotes (not pause)" "${pgbr_res_script}" "target-action=promote"
# Guards: target required with targetType (template fail; a schema if/then is not portable
# to helm 3.x), enum-checked targetType/mode, and the two prerequisites for restoring at all.
pgbr_res_badtarget=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.targetType=time 2>&1 || true)
assert_contains "#226: targetType without target fails fast" "${pgbr_res_badtarget}" "restore.target is required"
pgbr_res_badtype=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.targetType=bogus 2>&1 || true)
assert_contains "#226: invalid targetType rejected" "${pgbr_res_badtype}" "must be one of"
pgbr_res_badmode=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.mode=bogus 2>&1 || true)
assert_contains "#226: invalid mode rejected" "${pgbr_res_badmode}" "must be one of"
# nameSuffix lands in metadata.name, so a non-DNS-1123 value must be rejected at render
# time rather than by the API server mid-incident. The pattern also permits "" (the
# default): helm validates values.yaml against the schema on every render.
pgbr_res_badsuffix=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.restore.mode=job --set pgbackrest.restore.nameSuffix=Attempt_2 2>&1 || true)
# Match only "match pattern": helm 3.x (gojsonschema) reports "Does not match pattern" while
# helm 4 (santhosh-tekuri) reports "'Attempt_2' does not match pattern" -- CI pins helm 3.14,
# so a case-sensitive "does not match pattern" passes locally on helm 4 and fails there.
assert_contains "#226: non-DNS-1123 nameSuffix rejected" "${pgbr_res_badsuffix}" "match pattern"
assert_contains "#226: nameSuffix rejection names the offending value" "${pgbr_res_badsuffix}" "nameSuffix"
pgbr_res_nopersist=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set postgresql.persistence.enabled=false 2>&1 || true)
assert_contains "#226: restore without persistence fails fast (no PVC to restore into)" "${pgbr_res_nopersist}" "requires postgresql.persistence.enabled"
pgbr_res_nopgbr=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.restore.enabled=true 2>&1 || true)
assert_contains "#226: restore without pgbackrest fails fast" "${pgbr_res_nopgbr}" "requires pgbackrest.enabled"
# keyType=auto must NOT emit static keys (relies on the SA's workload identity).
pgbr_res_auto=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set repmgr.enabled=true --set pgbackrest.s3.endpoint=https://s3.test --set pgbackrest.s3.bucket=b --set pgbackrest.s3.keyType=auto --set pgbackrest.restore.enabled=true --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_not_contains "#226: keyType=auto emits no static S3 key env" "${pgbr_res_auto}" "PGBACKREST_REPO1_S3_KEY"
# The restore has to land where the data PVC can attach: a tainted/labelled database node
# pool the postgresql pods tolerate must be tolerated here too, or the restore sits Pending
# mid-incident. postgresql.affinity is deliberately not inherited (it spreads replicas).
pgbr_res_sched=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set postgresql.nodeSelector.workload=db --set 'postgresql.tolerations[0].key=db' --set 'postgresql.tolerations[0].operator=Exists' --set 'postgresql.tolerations[0].effect=NoSchedule' --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: inherits postgresql.nodeSelector" "${pgbr_res_sched}" "workload: db"
assert_contains "#226: inherits postgresql.tolerations" "${pgbr_res_sched}" "effect: NoSchedule"
assert_not_contains "#226: does not inherit postgresql.affinity (replica spreading is meaningless here)" "${pgbr_res_sched}" "affinity:"
# Repo encryption passphrase reaches the restore (an encrypted repo is undecryptable without it).
pgbr_res_enc=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.restore.enabled=true --set pgbackrest.repoEncryption.enabled=true --set pgbackrest.repoEncryption.existingSecret.name=cipher --show-only templates/pgbackrest-restore-job.yaml 2>&1)
assert_contains "#226: repo-encryption passphrase wired into the restore" "${pgbr_res_enc}" "PGBACKREST_REPO1_CIPHER_PASS"

# #266: bootstrap-from-backup -- an init container that seeds an EMPTY PGDATA on replica 0 from
# the repo, so a lost PVC recovers instead of silently initdb-ing an empty cluster. Contrast
# #226, which restores OVER live data under explicit operator control.
assert_not_contains "#266: nothing rendered when bootstrap is disabled (default)" "${pgbr_noval}" "pgbackrest-bootstrap"
assert_not_contains "#266: bootstrap.sh absent from the configmap when disabled" "${pgbr_noscript}" "bootstrap.sh:"
pgbr_boot=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --show-only templates/statefulset.yaml 2>&1)
assert_contains "#266: bootstrap init container renders when enabled" "${pgbr_boot}" "name: pgbackrest-bootstrap"
assert_contains "#266: bootstrap runs the mounted bootstrap.sh" "${pgbr_boot}" "/scripts/bootstrap.sh"
assert_contains "#266: bootstrap mounts the pgbackrest ConfigMap (repo settings + pg1-path)" "${pgbr_boot}" "mountPath: /etc/pgbackrest/pgbackrest.conf"
assert_contains "#266: bootstrap mounts bootstrap.sh from the configmap" "${pgbr_boot}" "key: bootstrap.sh"
assert_contains "#266: bootstrap targets the live PGDATA" "${pgbr_boot}" "value: /var/lib/postgresql/data/pgdata"
assert_contains "#266: keyType=shared wires the static S3 key into the bootstrap" "${pgbr_boot}" "name: PGBACKREST_REPO1_S3_KEY"
# Ordering is load-bearing: the bootstrap must populate PGDATA BEFORE repmgr-init inspects it
# (init-repmgr.sh defers when PG_VERSION exists) and AFTER fix-permissions has chowned the
# volume, or the restore cannot write. Compare line numbers in the rendered output.
boot_init_order=$(printf '%s\n' "${pgbr_boot}" | grep -nE "^        - name: (fix-permissions|pgbackrest-bootstrap|repmgr-init)$" | tr '\n' ' ')
assert_contains "#266: init order is fix-permissions -> pgbackrest-bootstrap -> repmgr-init" "${boot_init_order}" "fix-permissions.*pgbackrest-bootstrap.*repmgr-init"
# The script body carries the safety state machine; assert each branch exists so a future
# edit cannot quietly drop one.
pgbr_boot_script=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_contains "#266: bootstrap.sh present in the configmap when enabled" "${pgbr_boot_script}" "bootstrap.sh:"
assert_contains "#266: only replica 0 bootstraps (standbys clone from it)" "${pgbr_boot_script}" 'ORDINAL" != "0"'
assert_contains "#266: refuses to touch an initialized cluster" "${pgbr_boot_script}" "already holds an initialized cluster"
assert_contains "#266: skips when the completion marker is present" "${pgbr_boot_script}" "Bootstrap already completed"
assert_contains "#266: resumes an aborted bootstrap (unreadable pg_control)" "${pgbr_boot_script}" "resuming an aborted bootstrap"
assert_contains "#266: writes a completion marker inside PGDATA" "${pgbr_boot_script}" ".pgbackrest-bootstrap-complete"
# jq parses `pgbackrest info`; repmgr.image is values-configurable, so a missing jq must name
# itself rather than surface as "repository unreachable" during a healthy first install. The
# check must sit AFTER the early exits: a pod with an intact data directory needs no jq.
assert_contains "#266: names jq explicitly when it is missing" "${pgbr_boot_script}" "jq is required"
# EVERY early exit must precede the jq check, not just one of them: each returns before any JSON
# is parsed, so a pod that needs no repository lookup must still start on an image without jq.
# Checking only one boundary would let a regression slide the check up between the others.
jq_line=$(printf '%s\n' "${pgbr_boot_script}" | grep -n "jq is required" | cut -d: -f1)
boot_exit_ok=1
boot_exit_detail=""
for early in "standbys clone from the primary" "Bootstrap already completed" "already holds an initialized cluster"; do
  eline=$(printf '%s\n' "${pgbr_boot_script}" | grep -n "${early}" | head -1 | cut -d: -f1)
  if [ -z "${eline}" ] || [ -z "${jq_line}" ] || [ "${jq_line}" -le "${eline}" ]; then
    boot_exit_ok=0
    boot_exit_detail="${boot_exit_detail} [\"${early}\" at ${eline:-missing} vs jq at ${jq_line:-missing}]"
  fi
done
if [ "${boot_exit_ok}" = "1" ]; then
  pass "#266: jq check comes after every early exit (a pod needing no repo lookup starts without jq)"
else
  fail "#266: jq check comes after every early exit (a pod needing no repo lookup starts without jq)" "${boot_exit_detail}"
fi
assert_contains "#266: restores with --delta so a retry can resume" "${pgbr_boot_script}" "\-\-delta"
# It must NOT start postgres -- the entrypoint does that, and recovery follows on startup.
assert_not_contains "#266: bootstrap never starts postgres" "${pgbr_boot_script}" "pg_ctl -D"
# PITR/backup-set wiring + guards.
pgbr_boot_pitr=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set pgbackrest.bootstrap.targetType=time --set-string 'pgbackrest.bootstrap.target=2026-03-22 12:00:00+00' --set pgbackrest.bootstrap.backupSet=20260322-010002F --show-only templates/statefulset.yaml 2>&1)
assert_contains "#266: bootstrap PITR target wired into the env" "${pgbr_boot_pitr}" "2026-03-22 12:00:00+00"
assert_contains "#266: bootstrap backupSet wired into the env" "${pgbr_boot_pitr}" "20260322-010002F"
pgbr_boot_badtarget=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set pgbackrest.bootstrap.targetType=time 2>&1 || true)
assert_contains "#266: bootstrap targetType without target fails fast" "${pgbr_boot_badtarget}" "bootstrap.target is required"
pgbr_boot_badtype=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set pgbackrest.bootstrap.targetType=bogus 2>&1 || true)
assert_contains "#266: invalid bootstrap targetType rejected" "${pgbr_boot_badtype}" "must be one of"
pgbr_boot_nopersist=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set postgresql.persistence.enabled=false 2>&1 || true)
assert_contains "#266: bootstrap without persistence fails fast (would re-restore every restart)" "${pgbr_boot_nopersist}" "requires postgresql.persistence.enabled"
pgbr_boot_nopgbr=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.bootstrap.enabled=true 2>&1 || true)
assert_contains "#266: bootstrap without pgbackrest fails fast" "${pgbr_boot_nopgbr}" "requires pgbackrest.enabled"
# keyType=auto relies on the pod SA's workload identity -- no static keys anywhere.
pgbr_boot_auto=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set repmgr.enabled=true --set pgbackrest.s3.endpoint=https://s3.test --set pgbackrest.s3.bucket=b --set pgbackrest.s3.keyType=auto --set pgbackrest.bootstrap.enabled=true --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#266: keyType=auto emits no static S3 key env on the bootstrap" "${pgbr_boot_auto}" "PGBACKREST_REPO1_S3_KEY"
pgbr_boot_enc=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set pgbackrest.repoEncryption.enabled=true --set pgbackrest.repoEncryption.existingSecret.name=cipher --show-only templates/statefulset.yaml 2>&1)
assert_contains "#266: repo-encryption passphrase wired into the bootstrap" "${pgbr_boot_enc}" "PGBACKREST_REPO1_CIPHER_PASS"
# bootstrap and restore are orthogonal features and must be able to coexist.
pgbr_both=$(helm template test-pg "${CHART_DIR}" ${pgbr_args} --set pgbackrest.bootstrap.enabled=true --set pgbackrest.restore.enabled=true 2>&1)
assert_contains "#266: bootstrap + restore coexist (init container)" "${pgbr_both}" "name: pgbackrest-bootstrap"
assert_contains "#266: bootstrap + restore coexist (restore CronJob)" "${pgbr_both}" "component: pgbackrest-restore"

# #27: the backup + backup-validation Jobs run under a dedicated no-RBAC SA, not
# the namespace default.
backup_sa=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --show-only templates/backup-serviceaccount.yaml 2>&1)
assert_contains "#27: dedicated backup ServiceAccount renders" "${backup_sa}" "name: test-pg-backup"
assert_contains "#27: backup SA token is not automounted" "${backup_sa}" "automountServiceAccountToken: false"
backup_sa_refs=$(helm template test-pg "${CHART_DIR}" ${backup_val_args} --set backup.validation.enabled=true 2>&1 | grep -c "serviceAccountName: test-pg-backup")
assert_eq "#27: both backup Jobs reference the dedicated SA" "2" "${backup_sa_refs}"
assert_not_contains "#27: no backup SA when backup disabled" "$(helm template test-pg "${CHART_DIR}" --show-only templates/backup-serviceaccount.yaml 2>&1)" "kind: ServiceAccount"

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
# #34: non-AWS S3 knobs (uriStyle/verifyTls) are off at defaults (byte-stable for AWS),
# emitted only when set -- enables MinIO/Ceph-style endpoints for a CI-testable repo.
assert_not_contains "pgbackrest #34: no uri-style line at default (host)" "${pgbackrest}" "repo1-s3-uri-style"
assert_not_contains "pgbackrest #34: no verify-tls line at default" "${pgbackrest}" "repo1-storage-verify-tls"
pgbackrest_minio=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set pgbackrest.s3.uriStyle=path --set pgbackrest.s3.verifyTls=false \
  --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_contains "pgbackrest #34: uriStyle=path renders repo1-s3-uri-style=path" "${pgbackrest_minio}" "repo1-s3-uri-style=path"
assert_contains "pgbackrest #34: verifyTls=false renders repo1-storage-verify-tls=n" "${pgbackrest_minio}" "repo1-storage-verify-tls=n"

# #120: repository encryption is off by default and opt-in via repoEncryption.
assert_contains "pgbackrest #120: cipher-type none by default" "${pgbackrest}" "repo1-cipher-type=none"
assert_not_contains "pgbackrest #120: no cipher passphrase env by default" "${pgbackrest}" "PGBACKREST_REPO1_CIPHER_PASS"
pgbackrest_enc=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" \
  --set pgbackrest.repoEncryption.enabled=true --set pgbackrest.repoEncryption.existingSecret.name=cipher-sec 2>&1)
assert_contains "pgbackrest #120: cipher-type set when enabled" "${pgbackrest_enc}" "repo1-cipher-type=aes-256-cbc"
assert_not_contains "pgbackrest #120: passphrase never written into the ConfigMap" "${pgbackrest_enc}" "repo1-cipher-pass="
enc_env_count=$(printf '%s\n' "${pgbackrest_enc}" | grep -c "name: PGBACKREST_REPO1_CIPHER_PASS")
assert_eq "pgbackrest #120: passphrase env injected into postgresql + sidecar" "2" "${enc_env_count}"
assert_contains "pgbackrest #120: passphrase from the configured secret key" "${pgbackrest_enc}" "key: cipher-pass"
enc_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set pgbackrest.repoEncryption.enabled=true >/dev/null 2>&1 || enc_rc=$?
assert_eq "pgbackrest #120: enabled without a passphrase secret fails fast" "1" "$([ "${enc_rc}" -ne 0 ] && echo 1 || echo 0)"

# #117: readOnlyRootFilesystem on the auxiliary containers with dedicated
# securityContexts (exporter, backup, validation, pgbackrest runner), each paired
# with a writable /tmp emptyDir. (service-updater/pgpool-exporter share the
# postgresql/pgpool securityContext and are intentionally left for a follow-up.)
ro_exp=$(helm template test-pg "${CHART_DIR}" --set prometheusExporter.enabled=true --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
assert_contains "#117: exporter readOnlyRootFilesystem" "${ro_exp}" "readOnlyRootFilesystem: true"
assert_contains "#117: exporter has a writable /tmp" "${ro_exp}" "mountPath: /tmp"

# #204: under sslmode=verify-*, the exporter's CA volume must project only the (public)
# ca.crt at a world-readable mode (0444), not the whole secret at 0400 -- the exporter
# runs as a non-root UID with no fsGroup, so root-owned 0400 files were unreadable
# (pg_up=0). The server private key tls.key must not be mounted into the exporter.
tls_exp=$(helm template test-pg "${CHART_DIR}" --set prometheusExporter.enabled=true --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls --set prometheusExporter.sslmode=verify-ca --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
assert_contains "#204: exporter CA volume world-readable (0444)" "${tls_exp}" "defaultMode: 0444"
assert_contains "#204: exporter CA volume projects ca.crt" "${tls_exp}" "key: ca.crt"
assert_not_contains "#204: exporter CA volume does not mount server private key" "${tls_exp}" "key: tls.key"

# scope each per-container so the assert proves THAT container is read-only with a
# writable /tmp (a whole-render grep would let one container's /tmp satisfy the other).
ro_bk_cj=$(helm template test-pg "${CHART_DIR}" --set backup.enabled=true --set backup.s3.endpoint=https://e --set backup.s3.bucket=b --set backup.existingSecret.name=s --show-only templates/backup-cronjob.yaml 2>&1)
assert_contains "#117: backup container readOnlyRootFilesystem" "${ro_bk_cj}" "readOnlyRootFilesystem: true"
assert_contains "#117: backup container has a writable /tmp" "${ro_bk_cj}" "mountPath: /tmp"
ro_val_cj=$(helm template test-pg "${CHART_DIR}" --set backup.enabled=true --set backup.s3.endpoint=https://e --set backup.s3.bucket=b --set backup.existingSecret.name=s --set backup.validation.enabled=true --show-only templates/backup-validation-cronjob.yaml 2>&1)
assert_contains "#117: validation container readOnlyRootFilesystem" "${ro_val_cj}" "readOnlyRootFilesystem: true"
assert_contains "#117: validation container has a writable /tmp" "${ro_val_cj}" "mountPath: /tmp"
ro_pb=$(helm template test-pg "${CHART_DIR}" --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://e --set pgbackrest.s3.bucket=b --set pgbackrest.existingSecret.name=s --show-only templates/pgbackrest-cronjob.yaml 2>&1)
assert_contains "#117: pgbackrest runner readOnlyRootFilesystem" "${ro_pb}" "readOnlyRootFilesystem: true"
assert_contains "#117: pgbackrest runner HOME points at the writable /tmp" "${ro_pb}" "value: /tmp"

# pgBackRest: idle sidecar present in statefulset (exec target for CronJobs)
pgbackrest_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/statefulset.yaml 2>&1)
assert_contains "pgbackrest: sidecar present" "${pgbackrest_sts}" "name: pgbackrest"
assert_contains "pgbackrest: PGBACKREST_STANZA env var" "${pgbackrest_sts}" "PGBACKREST_STANZA"
assert_contains "pgbackrest: S3 key env var" "${pgbackrest_sts}" "PGBACKREST_REPO1_S3_KEY"
assert_contains "pgbackrest: config volume mount" "${pgbackrest_sts}" "pgbackrest-config"
# #145: pgbackrest.conf is a subPath mount (never live-updated), so the pod template
# must checksum it to roll the pod when the S3/retention config changes.
assert_contains "pgbackrest #145: pod template checksums the pgbackrest config" "${pgbackrest_sts}" "checksum/pgbackrest-config:"
pgbackrest_sts_changed=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --set pgbackrest.s3.bucket=other-bucket --show-only templates/statefulset.yaml 2>&1)
csum_base=$(printf '%s\n' "${pgbackrest_sts}" | grep "checksum/pgbackrest-config:")
csum_changed=$(printf '%s\n' "${pgbackrest_sts_changed}" | grep "checksum/pgbackrest-config:")
if [ -n "${csum_base}" ] && [ "${csum_base}" != "${csum_changed}" ]; then
  pass "pgbackrest #145: checksum changes when the pgbackrest config changes (pod rolls)"
else
  fail "pgbackrest #145: checksum changes when the pgbackrest config changes (pod rolls)" "base='${csum_base}' changed='${csum_changed}'"
fi
# default (pgbackrest disabled) must not emit the checksum
pgbackrest_off=$(helm template test-pg "${CHART_DIR}" --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "pgbackrest #145: no checksum when pgbackrest disabled" "${pgbackrest_off}" "checksum/pgbackrest-config:"

# #121: the primary lookup uses EndpointSlices (discovery.k8s.io), not the
# deprecated core Endpoints API, in both the CronJob script and the RBAC Role.
pgbackrest_cj=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-cronjob.yaml 2>&1)
assert_contains "pgbackrest #121: CronJob resolves primary via endpointslices" "${pgbackrest_cj}" "get endpointslices"
assert_not_contains "pgbackrest #121: CronJob no longer uses the Endpoints API" "${pgbackrest_cj}" 'get endpoints "'
pgbackrest_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-rbac.yaml 2>&1)
assert_contains "pgbackrest #121: RBAC grants discovery.k8s.io endpointslices" "${pgbackrest_rbac}" "discovery.k8s.io"
assert_not_contains "pgbackrest #121: RBAC drops the deprecated endpoints resource" "${pgbackrest_rbac}" 'resources: \["endpoints"\]'

# pgBackRest S3 key-type: default (shared) keeps static-key env + emits key-type.
assert_contains "pgbackrest: default key-type is shared" "${pgbackrest}" "repo1-s3-key-type=shared"

# keyType=auto (cloud workload identity): no static-key env, no existingSecret
# required, SA annotation flows to the postgresql pods' ServiceAccount.
pgb_auto=$(helm template test-pg "${CHART_DIR}" \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.amazonaws.com \
  --set pgbackrest.s3.bucket=test-backups \
  --set pgbackrest.s3.keyType=auto \
  --set 'postgresql.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::1:role/r' 2>&1)
assert_contains "pgbackrest auto: key-type is auto" "${pgb_auto}" "repo1-s3-key-type=auto"
assert_not_contains "pgbackrest auto: no static S3 key env" "${pgb_auto}" "PGBACKREST_REPO1_S3_KEY"
assert_contains "pgbackrest auto: pod SA carries IRSA annotation" "${pgb_auto}" "eks.amazonaws.com/role-arn: arn:aws:iam::1:role/r"

# keyType=auto renders without existingSecret.name (the required() is shared-only).
pgb_auto_noexsec_rc=0
helm template test-pg "${CHART_DIR}" \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.amazonaws.com \
  --set pgbackrest.s3.bucket=test-backups \
  --set pgbackrest.s3.keyType=auto >/dev/null 2>&1 || pgb_auto_noexsec_rc=$?
assert_eq "pgbackrest auto: no existingSecret required" "0" "${pgb_auto_noexsec_rc}"

# Invalid keyType fails fast.
pgb_bad_rc=0
helm template test-pg "${CHART_DIR}" \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.amazonaws.com \
  --set pgbackrest.s3.bucket=test-backups \
  --set pgbackrest.s3.keyType=bogus >/dev/null 2>&1 || pgb_bad_rc=$?
assert_eq "pgbackrest: invalid keyType fails fast" "1" "$([ "${pgb_bad_rc}" -ne 0 ] && echo 1 || echo 0)"
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

# pgBackRest: coexists with the HA agent
assert_contains "pgbackrest+repmgr: sidecar present" "${pgbackrest_sts}" "name: pgbackrest"
assert_contains "pgbackrest+repmgr: runs alongside the agent" "${pgbackrest_sts}" '"/usr/local/bin/entrypoint.sh", "agent"'
assert_not_contains "#286: pgbackrest render has no repmgrd sidecar" "${pgbackrest_sts}" "name: repmgrd"

# pgBackRest: CronJobs (one per backup type) drive scheduling.
pgbackrest_cron=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-cronjob.yaml 2>&1)
assert_contains "pgbackrest: full CronJob renders" "${pgbackrest_cron}" "name: test-pg-pgbackrest-full"
assert_contains "pgbackrest: diff CronJob renders" "${pgbackrest_cron}" "name: test-pg-pgbackrest-diff"
assert_contains "pgbackrest: CronJob uses kubectl image" "${pgbackrest_cron}" "alpine/k8s"
assert_contains "pgbackrest: CronJob runs as service account" "${pgbackrest_cron}" "serviceAccountName: test-pg-pgbackrest"
assert_contains "pgbackrest: CronJob resolves primary via endpoints" "${pgbackrest_cron}" "endpoints"
assert_contains "pgbackrest: CronJob execs into sidecar" "${pgbackrest_cron}" "exec \"\$PRIMARY\" -c pgbackrest"
assert_contains "pgbackrest: CronJob ensures stanza" "${pgbackrest_cron}" "stanza-create"
# #160: stanza-create must NOT be masked -- a real failure (S3 perms, kubectl exec
# error, needed stanza-upgrade) must surface, not be swallowed. Reject the common
# error-suppression idioms, not just the exact original token.
assert_not_contains "#160: stanza-create not masked with || true" "${pgbackrest_cron}" "stanza-create || true"
assert_not_contains "#160: stanza-create not masked with || :" "${pgbackrest_cron}" "stanza-create || :"
assert_not_contains "#160: stanza-create stderr not silenced" "${pgbackrest_cron}" "stanza-create 2>/dev/null"
assert_contains "pgbackrest: CronJob runs backup" "${pgbackrest_cron}" "type=\"\$BACKUP_TYPE\" backup"
assert_contains "pgbackrest: CronJob concurrency Forbid" "${pgbackrest_cron}" "concurrencyPolicy: Forbid"
# #155: the pgbackrest CronJob (which holds the exec-capable SA token) must be hardened
# like the chart's other pods (runAsNonRoot + drop ALL), not run as root.
assert_contains "#155: pgbackrest CronJob pod runs as non-root" "${pgbackrest_cron}" "runAsNonRoot: true"
assert_contains "#155: pgbackrest CronJob seccompProfile RuntimeDefault" "${pgbackrest_cron}" "type: RuntimeDefault"
assert_contains "#155: pgbackrest CronJob container drops capabilities" "${pgbackrest_cron}" "capabilities:"
assert_contains "#155: pgbackrest CronJob no privilege escalation" "${pgbackrest_cron}" "allowPrivilegeEscalation: false"
# assert_contains uses `grep` (regex), so escape the cron-spec stars.
assert_contains "pgbackrest: full CronJob carries full schedule" "${pgbackrest_cron}" 'schedule: "0 1 \* \* 0"'
assert_contains "pgbackrest: diff CronJob carries diff schedule" "${pgbackrest_cron}" 'schedule: "0 1 \* \* 1-6"'

# pgBackRest: RBAC for CronJob exec access.
pgbackrest_rbac=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-rbac.yaml 2>&1)
assert_contains "pgbackrest rbac: ServiceAccount renders" "${pgbackrest_rbac}" "kind: ServiceAccount"
assert_contains "pgbackrest rbac: Role renders" "${pgbackrest_rbac}" "kind: Role"
assert_contains "pgbackrest rbac: RoleBinding renders" "${pgbackrest_rbac}" "kind: RoleBinding"
assert_contains "pgbackrest rbac: pods/exec verb create" "${pgbackrest_rbac}" "pods/exec"
assert_contains "pgbackrest rbac: endpointslices list verb (#121)" "${pgbackrest_rbac}" "endpointslices"
# #134: pods get and pods/exec create must be scoped by resourceNames to the
# StatefulSet's deterministic pod names, not left namespace-wide (an unscoped
# Role lets a leaked SA token exec into every pod in the namespace). Both pod
# rules (pods, pods/exec) carry resourceNames. The EndpointSlice list (#121)
# cannot be resourceName-scoped (slice names are auto-generated) and reads only
# EndpointSlice metadata, so it is excluded from this count.
pgbackrest_rn_count=$(printf '%s\n' "${pgbackrest_rbac}" | grep -c 'resourceNames:')
assert_eq "pgbackrest rbac: pods+pods/exec resourceName-scoped (#134)" "2" "${pgbackrest_rn_count}"
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

# --- Resource name length guard (#158) ---

# Test: a fullnameOverride that pushes a Service name past 63 fails fast at render.
longname_svc=$(helm template test-pg "${CHART_DIR}" \
  --set fullnameOverride=averyveryverylongfullnameoverridethatreachessixtythreecharsabc 2>&1) && longname_svc_rc=0 || longname_svc_rc=$?
assert_eq "#158: render fails when a Service name exceeds 63 chars" "1" "${longname_svc_rc}"
assert_contains "#158: Service-length error names the 63 limit" "${longname_svc}" "limited to 63"

# Test: a name short enough for Services but too long for a CronJob (+pgbackrest) fails.
longname_cron=$(helm template test-pg "${CHART_DIR}" \
  --set fullnameOverride=shorterbutstilltoolongforcronjobxxxxxxx \
  --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://s --set pgbackrest.s3.bucket=b \
  --set pgbackrest.existingSecret.name=sec 2>&1) && longname_cron_rc=0 || longname_cron_rc=$?
assert_eq "#158: render fails when a CronJob name exceeds 52 chars" "1" "${longname_cron_rc}"
assert_contains "#158: CronJob-length error names the 52 limit" "${longname_cron}" "<= 52"

# Test: a name short enough for a Service (<=63) but too long for a Deployment-backed
# resource (pgpool/exporter, <=47 for the generated Pod name) fails.
longname_deploy=$(helm template test-pg "${CHART_DIR}" \
  --set fullnameOverride=fiftycharfullnameoverrideforpgpooldeploycheck00000 \
  --set pgpool.enabled=true 2>&1) && longname_deploy_rc=0 || longname_deploy_rc=$?
assert_eq "#158: render fails when a Deployment name exceeds 47 chars" "1" "${longname_deploy_rc}"
assert_contains "#158: Deployment-length error names the 47 limit" "${longname_deploy}" "<= 47"

# Test: a normal release name renders fine (guard is a no-op).
normalname=$(helm template test-pg "${CHART_DIR}" 2>&1) && normalname_rc=0 || normalname_rc=$?
assert_eq "#158: normal release name renders (guard no-op)" "0" "${normalname_rc}"
assert_not_contains "#158: no length error for a normal name" "${normalname}" "limited to 63"

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

# a matched rebump to a PG17 repmgr image renders (a supported selection since #269)
major_rebump=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.majorVersion=17 \
  --set repmgr.image.majorVersion=17 \
  --show-only templates/statefulset.yaml 2>&1) && major_rebump_rc=0 || major_rebump_rc=$?
assert_eq "major pin: matched repmgr.image.majorVersion rebump renders (#133)" "0" "${major_rebump_rc}"

# The OTHER direction of the mismatch. #133 only ever tested moving
# postgresql.majorVersion; the guard has to be symmetric, or parameterising the image
# (#269) could leave "repmgr image says 17, chart still thinks 18" -- which silently runs
# a PG17 server while every extension path the chart builds points at /18/.
major_mismatch_rev=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.image.majorVersion=17 \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 2>&1) && major_mismatch_rev_rc=0 || major_mismatch_rev_rc=$?
assert_eq "major pin: render fails when repmgr.image.majorVersion moves alone (#269)" "1" "${major_mismatch_rev_rc}"
assert_contains "major pin: reverse mismatch names both values (#269)" "${major_mismatch_rev}" "does not match repmgr.image.majorVersion"

# --- #269: PG17 is a first-class selection, not just a render that does not fail ---

# The values triple that selects PG17 (majors on both sides + the -pg17 image tag) must
# produce a StatefulSet that actually runs that image.
pg17_full=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.majorVersion=17 \
  --set postgresql.image.tag=17.10-trixie \
  --set repmgr.image.majorVersion=17 \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 \
  --show-only templates/statefulset.yaml 2>&1) && pg17_full_rc=0 || pg17_full_rc=$?
assert_eq "#269: PG17 selection renders" "0" "${pg17_full_rc}"
assert_contains "#269: PG17 selection uses the -pg17 repmgr image" "${pg17_full}" "cagriekin/repmgr:trixie-5.5.0-29-pg17"

# Extensions are where a wrong major becomes a crash-looping init container: the copy
# paths are built from postgresql.majorVersion, so they must follow the selection with no
# /18/ path left behind.
pg17_ext=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.extensions.enabled=true \
  --set postgresql.majorVersion=17 \
  --set postgresql.image.tag=17.10-trixie \
  --set repmgr.image.majorVersion=17 \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#269: PG17 extension lib path" "${pg17_ext}" "/usr/lib/postgresql/17/lib"
assert_contains "#269: PG17 extension share path" "${pg17_ext}" "/usr/share/postgresql/17/extension"
assert_not_contains "#269: no PG18 paths remain under a PG17 selection" "${pg17_ext}" "postgresql/18/"

# audit.enabled=true on PG17 must preload pgaudit exactly as on 18 -- the image asserts
# postgresql-17-pgaudit at build time (see images/repmgr/Dockerfile), so the chart side
# must not gate the preload on a major.
pg17_audit=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.audit.enabled=true \
  --set postgresql.majorVersion=17 \
  --set repmgr.image.majorVersion=17 \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#269: audit on PG17 preloads pgaudit" "${pg17_audit}" "shared_preload_libraries = 'repmgr,pgaudit'"

# --- #269 review: the TAG is what actually selects the major, so check it too ---

# The two majorVersion values are hand-typed claims; the -pgNN suffix is the artifact. A
# values file that moves one and not the other must fail at render, not run the wrong major.
tag_mismatch=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 2>&1) && tag_mismatch_rc=0 || tag_mismatch_rc=$?
assert_eq "#269: render fails when the -pg17 tag disagrees with majorVersion" "1" "${tag_mismatch_rc}"
assert_contains "#269: tag mismatch names the tag and the major" "${tag_mismatch}" "is built for PostgreSQL 17"

# The reverse: majors moved to 17 but the tag left on the unsuffixed (PG18) default. No
# suffix means nothing to compare, which is why PG_MAJOR is passed to the container -- the
# entrypoint's require_pg_bindir and the agent then refuse to start the wrong image.
pgmajor_env=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.majorVersion=17 --set repmgr.image.majorVersion=17 \
  --set repmgr.image.tag=trixie-5.5.0-29-pg17 \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#269: PG_MAJOR env follows repmgr.image.majorVersion" "${pgmajor_env}" "value: \"17\""
pgmajor_count=$(grep -c "name: PG_MAJOR" <<< "${pgmajor_env}")
assert_eq "#269: PG_MAJOR set on both repmgr-image containers (init + postgresql)" "2" "${pgmajor_count}"

# Standalone runs the official postgres image, which knows nothing of PG_MAJOR; setting it
# there would be misleading noise.
pgmajor_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#269: standalone does not set PG_MAJOR" "${pgmajor_standalone}" "name: PG_MAJOR"

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

# The StatefulSet runs replicaCount + 1 pods (ordinals 0..replicaCount), so the postStart
# discovery loop must reach ordinal replicaCount; replicaCount=2 disambiguates from the
# old off-by-one bound (#177). The repmgrd peer scan this used to also cover is gone (#286).
discovery_sts=$(helm template test-pg "${CHART_DIR}" \
  --set postgresql.replicaCount=2 \
  --set postgresql.lifecycle.postStart.additionalCommands="echo noop" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "discovery loops: scan ordinals 0..replicaCount" "${discovery_sts}" "seq 0 2"
assert_not_contains "postStart discovery: off-by-one bound is gone" "${discovery_sts}" "seq 0 1)"
assert_not_contains "peer scan: hardcoded seq 0 9 is gone" "${discovery_sts}" "seq 0 9"

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

# Test: imagePullSecrets propagates to statefulset, pgpool, exporter, backup
# cronjob, and the monitoring-user hook Job pod templates (one occurrence each)
ips_full=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --set 'imagePullSecrets[0].name=registry-cred' \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=test-backups \
  --set backup.existingSecret.name=s3-backup-creds \
  2>&1)
ips_count=$(printf '%s' "${ips_full}" | grep -c "name: registry-cred" || true)
assert_eq "imagePullSecrets: statefulset, pgpool, exporter, backup, monitoring-user all carry the secret" "5" "${ips_count}"

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
assert_contains "backup guard: --newer-than check present (scoped to the release subpath, #143)" "${guard_configmap}" 'mc find "s3/${S3_DIR}/" --name '"'"'backup_'
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

# #30: WAL-archiving health metrics from pg_stat_archiver, gated on pgbackrest
# (archive_mode is on only then) and scraped on the primary.
exporter_cm_arch=$(helm template test-pg "${CHART_DIR}" --set prometheusExporter.enabled=true \
  --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://e --set pgbackrest.s3.bucket=b \
  --set pgbackrest.existingSecret.name=s --show-only templates/prometheus-exporter-configmap.yaml 2>&1)
assert_contains "exporter cm #30: pg_wal_archive group present when pgbackrest enabled" "${exporter_cm_arch}" "pg_wal_archive:"
assert_contains "exporter cm #30: failed_count metric declared" "${exporter_cm_arch}" "failed_count:"
assert_contains "exporter cm #30: seconds_since_last_archived metric declared" "${exporter_cm_arch}" "seconds_since_last_archived:"
assert_contains "exporter cm #30: archiver query reads pg_stat_archiver" "${exporter_cm_arch}" "FROM pg_stat_archiver"
# absent when pgbackrest disabled (no archiving); exporter_cm above is full-test (pgbackrest off)
assert_not_contains "exporter cm #30: no pg_wal_archive group when pgbackrest disabled" "${exporter_cm}" "pg_wal_archive:"

# #28: least-privilege monitoring user. A post-install/upgrade hook Job creates a
# pg_monitor role; the exporter connects as it, not the postgres superuser.
exp_on="--set prometheusExporter.enabled=true"
mon_secret=$(helm template test-pg "${CHART_DIR}" ${exp_on} --show-only templates/secret.yaml 2>&1)
assert_contains "#28: secret carries monitoring-password when exporter enabled" "${mon_secret}" "monitoring-password:"
mon_job=$(helm template test-pg "${CHART_DIR}" ${exp_on} --show-only templates/monitoring-user-job.yaml 2>&1)
assert_contains "#28: monitoring-user hook Job present" "${mon_job}" "name: test-pg-monitoring-user"
assert_contains "#28: hook runs post-install,post-upgrade" "${mon_job}" "post-install,post-upgrade"
assert_contains "#28: hook grants pg_monitor (read-only)" "${mon_job}" "GRANT pg_monitor"
assert_contains "#28: hook role creation is idempotent (gexec guard)" "${mon_job}" "WHERE NOT EXISTS (SELECT FROM pg_roles"
assert_contains "#28: hook makes no API calls (no SA token)" "${mon_job}" "automountServiceAccountToken: false"
mon_exp=$(helm template test-pg "${CHART_DIR}" ${exp_on} --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
mon_user_block=$(printf '%s\n' "${mon_exp}" | sed -n '/name: POSTGRES_USER/,/POSTGRES_DATABASE/p')
assert_contains "#28: exporter connects as the monitoring user, not the superuser" "${mon_user_block}" 'value: "monitoring"'
assert_contains "#28: exporter password from the monitoring-password secret key" "${mon_user_block}" "key: monitoring-password"
# disabled -> no hook Job, no monitoring-password, exporter falls back to the superuser secret
mon_off_all=$(helm template test-pg "${CHART_DIR}" ${exp_on} --set prometheusExporter.monitoringUser.enabled=false 2>&1)
assert_not_contains "#28: no hook Job when monitoringUser disabled" "${mon_off_all}" "monitoring-user"
mon_off_exp=$(helm template test-pg "${CHART_DIR}" ${exp_on} --set prometheusExporter.monitoringUser.enabled=false --show-only templates/prometheus-exporter-deployment.yaml 2>&1)
mon_off_block=$(printf '%s\n' "${mon_off_exp}" | sed -n '/name: POSTGRES_USER/,/POSTGRES_DATABASE/p')
assert_contains "#28: disabled falls back to the superuser username key" "${mon_off_block}" "key: username"

# #218: declarative databases/roles/grants.
# default-empty -> no hook Job (byte-identical install).
assert_not_contains "#218: no databases-roles Job by default" "${minimal}" "component: databases-roles"
dr_args=(
  --set-json 'postgresql.roles=[{"name":"app","grants":[{"database":"appdb","privileges":["CONNECT"]},{"database":"appdb","schema":"public","objects":"ALL_TABLES","privileges":["SELECT","INSERT"]}]},{"name":"readers","login":false}]'
  --set-json 'postgresql.databases=[{"name":"appdb","owner":"app","extensions":["pg_stat_statements"]}]'
)
dr_job=$(helm template test-pg "${CHART_DIR}" "${dr_args[@]}" --show-only templates/databases-roles-job.yaml 2>&1)
assert_contains "#218: databases-roles hook Job present when set" "${dr_job}" "name: test-pg-databases-roles"
assert_contains "#218: is a post-install/upgrade hook" "${dr_job}" "post-install,post-upgrade"
assert_contains "#218: makes no API calls (no SA token)" "${dr_job}" "automountServiceAccountToken: false"
# roles: idempotent create + password via \getenv (never argv), login flags honored.
assert_contains "#218: CREATE ROLE is conditional/idempotent" "${dr_job}" "WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app')"
assert_contains "#218: login role gets LOGIN + password from env via getenv" "${dr_job}" "ALTER ROLE \"app\" WITH LOGIN PASSWORD :'p0'"
assert_contains "#218: password read from the environment with getenv (not argv)" "${dr_job}" "getenv p0 ACL_PASS_0"
assert_contains "#218: login:false yields a NOLOGIN role" "${dr_job}" "CREATE ROLE %I NOLOGIN', 'readers'"
# databases: idempotent create + owner, extensions, grants (incl. default privileges).
assert_contains "#218: CREATE DATABASE conditional with owner" "${dr_job}" "format('CREATE DATABASE %I OWNER %I', 'appdb', 'app')"
assert_contains "#218: per-database extension created idempotently" "${dr_job}" 'CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"'
assert_contains "#218: database-level grant" "${dr_job}" 'GRANT CONNECT ON DATABASE "appdb" TO "app"'
assert_contains "#218: ALL TABLES grant + default privileges for future objects" "${dr_job}" 'GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA "public" TO "app"; ALTER DEFAULT PRIVILEGES IN SCHEMA "public" GRANT SELECT, INSERT ON TABLES TO "app"'
# password plumbing: chart-generated key in the chart Secret, secretKeyRef env, none in a ConfigMap.
dr_secret=$(helm template test-pg "${CHART_DIR}" "${dr_args[@]}" --show-only templates/secret.yaml 2>&1)
assert_contains "#218: chart-generated role password persisted in the Secret" "${dr_secret}" "app-acl-password:"
assert_not_contains "#218: NOLOGIN role gets no generated password" "${dr_secret}" "readers-acl-password:"
assert_contains "#218: role password wired via secretKeyRef (not the ConfigMap)" "${dr_job}" "key: \"app-acl-password\""
# render guards (token denylist / identifiers / reserved / owner) -- enforced by fail, not schema.
g() { helm template test-pg "${CHART_DIR}" "$@" --show-only templates/databases-roles-job.yaml 2>&1; }
# database "postgres" is the default primary db -> passes the grant-db guard, reaching the privilege check.
assert_contains "#218 guard: privilege allowlist rejects arbitrary SQL keywords" "$(g --set-json 'postgresql.roles=[{"name":"app","grants":[{"database":"postgres","privileges":["DROP"]}]}]')" "allowlist"
assert_contains "#218 guard: reserved role name rejected (unconditional)" "$(g --set repmgr.enabled=false --set postgresql.replicaCount=0 --set-json 'postgresql.roles=[{"name":"repmgr"}]')" "reserved/internal"
assert_contains "#218 guard: duplicate role rejected" "$(g --set-json 'postgresql.roles=[{"name":"a"},{"name":"a"}]')" "duplicate"
assert_contains "#218 guard: owner must be a declared role or the primary user" "$(g --set-json 'postgresql.databases=[{"name":"d","owner":"ghost"}]')" "must be declared"
# new guards from the high-effort review: objects-needs-schema, grant-db existence, memberOf existence.
assert_contains "#218 guard: objects without schema rejected (no silent DB-level downgrade)" "$(g --set-json 'postgresql.databases=[{"name":"d"}]' --set-json 'postgresql.roles=[{"name":"app","grants":[{"database":"d","objects":"ALL_TABLES","privileges":["SELECT"]}]}]')" "requires a schema"
assert_contains "#218 guard: grant target database must be declared or primary" "$(g --set-json 'postgresql.roles=[{"name":"app","grants":[{"database":"ghostdb","privileges":["CONNECT"]}]}]')" "must be declared in postgresql.databases"
assert_contains "#218 guard: memberOf must be a declared or pg_* role" "$(g --set-json 'postgresql.roles=[{"name":"app","memberOf":["typo"]}]')" "must be a role declared"
assert_contains "#218: built-in pg_* role allowed in memberOf" "$(g --set-json 'postgresql.roles=[{"name":"app","memberOf":["pg_read_all_data"]}]')" "GRANT \"pg_read_all_data\" TO \"app\""
# "pattern" substring is stable across helm 3.x/4.x schema-error phrasings (the full
# message wording differs by version, as the enum message does).
assert_contains "#218 guard: invalid identifier rejected (schema pattern)" "$(g --set-json 'postgresql.databases=[{"name":"bad-name"}]')" "pattern"

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

# --- repmgr monitoring history (#286: removed with repmgrd) ---

# repmgr.monitoring_history was only ever written by repmgrd, and the daily
# `repmgr cluster cleanup --keep-history` prune ran in the repmgrd sidecar. Both are
# gone, so no render may carry either the loop or the retention env.
mhd_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#286: no monitoring-history cleanup loop" "${mhd_sts}" "cluster cleanup"
assert_not_contains "#286: no MONITORING_HISTORY_DAYS env" "${mhd_sts}" "MONITORING_HISTORY_DAYS"
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

# Test: rbac grants pods list (unscoped) for role labeling; get/patch are scoped
# to the StatefulSet pods and delete is gated on fence mode (detailed in the
# #154 least-privilege tests above)
ro_role=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_contains "readonly: rbac grants pods list for role labeling" "${ro_role}" 'verbs: \["list"\]'

# #286: the convergent pg-role labeling and the core/v1 PrimaryChanged audit Event were
# both service-updater shell. The agent does the labeling through the API (internal/k8s,
# Go tests) and records failover decisions in a structured audit log rather than Events --
# so the Role must no longer carry an `events` create grant at all.
ro_role_events=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --show-only templates/rbac.yaml 2>&1)
assert_not_contains "#286: Role grants no events create (agent uses an audit log)" "${ro_role_events}" 'resources: \["events"\]'
assert_not_contains "#286: Role has no unscoped configmaps get/create/patch rule" "${ro_role_events}" 'verbs: \["get", "create", "patch"\]'

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
# #31 parity: the backup-validation CronJob is a separate template; without the
# pgvector symlink the feature renders nothing despite the values claiming it runs.
pgv_val=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set backup.enabled=true --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=b --set backup.existingSecret.name=creds \
  --set backup.validation.enabled=true --show-only templates/backup-validation-cronjob.yaml 2>&1)
assert_contains "pgvector #31: backup-validation CronJob renders (symlink present)" "${pgv_val}" "app.kubernetes.io/component: backup-validation"
assert_contains "pgvector #31: validation runs as the official-postgres uid 999" "${pgv_val}" "runAsUser: 999"
# #38 parity: the pgbackrest PITR validation CronJob is a new template; without the
# pgvector symlink the feature renders nothing despite the values exposing it.
pgv_pgbr_val=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set pgbackrest.enabled=true --set repmgr.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.example.com --set pgbackrest.s3.bucket=b \
  --set pgbackrest.existingSecret.name=creds --set pgbackrest.validation.enabled=true \
  --show-only templates/pgbackrest-validation-cronjob.yaml 2>&1)
assert_contains "pgvector #38: pgbackrest-validation CronJob renders (symlink present)" "${pgv_pgbr_val}" "app.kubernetes.io/component: pgbackrest-validation"
assert_contains "pgvector #38: restores into a throwaway PGDATA, not the live dir" "${pgv_pgbr_val}" "value: /work/pgdata"
# validate.sh now lives in pgbackrest-configmap.yaml (also symlinked): a missing pgvector
# symlink for THAT template would pass the CronJob check above but break the Job at runtime.
pgv_pgbr_cm=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set pgbackrest.enabled=true --set repmgr.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.example.com --set pgbackrest.s3.bucket=b \
  --set pgbackrest.existingSecret.name=creds --set pgbackrest.validation.enabled=true \
  --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_contains "pgvector #38: validate.sh renders from the ConfigMap (symlink present)" "${pgv_pgbr_cm}" "validate.sh:"
assert_contains "pgvector #38: validate.sh body inherited (archive_mode safeguard)" "${pgv_pgbr_cm}" "archive_mode=off"
# #218 parity: the databases-roles hook Job is a new template; without the pgvector
# symlink the feature renders nothing despite the values exposing roles/databases.
pgv_dr=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set-json 'postgresql.roles=[{"name":"app","grants":[{"database":"appdb","privileges":["CONNECT"]}]}]' \
  --set-json 'postgresql.databases=[{"name":"appdb","owner":"app"}]' \
  --show-only templates/databases-roles-job.yaml 2>&1)
assert_contains "pgvector #218: databases-roles Job renders (symlink present)" "${pgv_dr}" "app.kubernetes.io/component: databases-roles"
assert_contains "pgvector #218: creates the declared database" "${pgv_dr}" "format('CREATE DATABASE %I OWNER %I', 'appdb', 'app')"
# #27 parity: the backup ServiceAccount is a separate template; its pgvector symlink
# must render or the backup Jobs fall back to the namespace default SA.
pgv_bksa=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set backup.enabled=true --set backup.s3.endpoint=https://s3.example.com \
  --set backup.s3.bucket=b --set backup.existingSecret.name=creds \
  --show-only templates/backup-serviceaccount.yaml 2>&1)
assert_contains "pgvector #27: backup ServiceAccount renders (symlink present)" "${pgv_bksa}" "kind: ServiceAccount"
assert_contains "pgvector #27: backup SA token not automounted" "${pgv_bksa}" "automountServiceAccountToken: false"
# #28 parity: the monitoring-user hook Job is a separate template; without the
# pgvector symlink the exporter would reference a pg_monitor user nothing creates.
pgv_mon=$(helm template test-pgv "${PGVECTOR_DIR}" --set prometheusExporter.enabled=true --show-only templates/monitoring-user-job.yaml 2>&1)
assert_contains "pgvector #28: monitoring-user hook Job renders (symlink present)" "${pgv_mon}" "app.kubernetes.io/component: monitoring-user"
assert_contains "pgvector #28: hook grants pg_monitor" "${pgv_mon}" "GRANT pg_monitor"

# --- pgvector agent-mode parity ---
# The agent-mode templates are symlinks to pg's, but pgvector/values.yaml is an
# independent copy: a missing repmgr.agent.* / monitoring value would break
# agent-mode rendering only for pgvector. Render the agent-mode paths against the
# pgvector chart to catch that #126-class gap, and confirm the agent is the default.
pgv_agent_rc=0
pgv_agent=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --show-only templates/statefulset.yaml --show-only templates/rbac.yaml 2>&1) || pgv_agent_rc=$?
assert_eq "pgvector agent: renders in agent mode" "0" "${pgv_agent_rc}"
assert_contains "pgvector agent: postgresql runs the agent arm" "${pgv_agent}" '"/usr/local/bin/entrypoint.sh", "agent"'
assert_not_contains "pgvector agent: no repmgrd sidecar" "${pgv_agent}" "name: repmgrd"
assert_contains "pgvector agent: rbac grants coordination.k8s.io leases" "${pgv_agent}" "coordination.k8s.io"
assert_contains "pgvector agent: lease scoped to <fullname>-leader" "${pgv_agent}" "test-pgv-pgvector-leader"
pgv_agent_hl=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --show-only templates/service-headless.yaml 2>&1)
assert_contains "pgvector agent: headless exposes agent-metrics port" "${pgv_agent_hl}" "name: agent-metrics"
pgv_agent_mon=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set repmgr.agent.monitoring.serviceMonitor.enabled=true \
  --set repmgr.agent.monitoring.prometheusRule.enabled=true \
  --show-only templates/agent-servicemonitor.yaml --show-only templates/agent-prometheusrule.yaml 2>&1)
assert_contains "pgvector agent: ServiceMonitor renders" "${pgv_agent_mon}" "kind: ServiceMonitor"
assert_contains "pgvector agent: PrometheusRule scoped to the pgvector headless Service" "${pgv_agent_mon}" "test-pgv-pgvector-headless"
# pgvector default is the agent (since 1.0.0), matching pg.
pgv_default=$(helm template test-pgv "${PGVECTOR_DIR}" --show-only templates/statefulset.yaml 2>&1)
assert_contains "pgvector: default runs the agent arm (1.0.0 default flip)" "${pgv_default}" '"/usr/local/bin/entrypoint.sh", "agent"'
assert_not_contains "pgvector: default has no repmgrd sidecar" "${pgv_default}" "name: repmgrd"
# #286: pgvector rejects the removed repmgrd key just as pg does (byte-identical templates).
pgv_repmgrd_rc=0
helm template test-pgv "${PGVECTOR_DIR}" --set repmgr.failoverMode=repmgrd >/dev/null 2>&1 || pgv_repmgrd_rc=$?
assert_eq "pgvector #286: repmgr.failoverMode is rejected" "1" "$([ "${pgv_repmgrd_rc}" -ne 0 ] && echo 1 || echo 0)"

# pgvector parity for the security/resource cluster: the templates are symlinked but the
# values are per-chart, so guard the #126-class "drop a key -> securityContext: null"
# regression on the symlink-fed, securityContext-bearing keys (#155/#118/#166).
pgv_pgbackrest_cron=$(helm template test-pgv "${PGVECTOR_DIR}" -f "${SCRIPT_DIR}/values-pgbackrest.yaml" --show-only templates/pgbackrest-cronjob.yaml 2>&1)
assert_contains "pgvector #155: pgbackrest CronJob runs non-root" "${pgv_pgbackrest_cron}" "runAsNonRoot: true"
assert_not_contains "pgvector #155: pgbackrest CronJob has no null securityContext" "${pgv_pgbackrest_cron}" "securityContext: null"
pgv_pgpool_svc=$(helm template test-pgv "${PGVECTOR_DIR}" --set pgpool.enabled=true --show-only templates/pgpool-service.yaml 2>&1)
assert_not_contains "pgvector #118: pgpool Service does not expose PCP 9898 by default" "${pgv_pgpool_svc}" "9898"
pgv_backup_cron=$(helm template test-pgv "${PGVECTOR_DIR}" --set backup.enabled=true --set backup.existingSecret.name=s --set backup.s3.endpoint=https://m --set backup.s3.bucket=b --show-only templates/backup-cronjob.yaml 2>&1)
assert_contains "pgvector #166: backup pod disables SA token automount" "${pgv_backup_cron}" "automountServiceAccountToken: false"

# --- full-chart review fixes (1.2.1) ---
# C1: pgvector/values.yaml was missing pgbackrest.s3.uriStyle/verifyTls, which the shared
# template dereferences -> pgvector rendered repo1-storage-verify-tls=n (S3 cert verification
# OFF) and an empty repo1-s3-uri-style=. Guard both, against the pgvector chart specifically.
pgv_pgbr_conf=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set pgbackrest.enabled=true --set pgbackrest.s3.keyType=auto \
  --set-string pgbackrest.s3.endpoint=s3.example.com --set-string pgbackrest.s3.bucket=b \
  --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_not_contains "C1 pgvector: pgbackrest does not disable S3 TLS verification" "${pgv_pgbr_conf}" "repo1-storage-verify-tls=n"
assert_not_contains "C1 pgvector: no empty repo1-s3-uri-style" "${pgv_pgbr_conf}" "repo1-s3-uri-style="

# H1/H2: the exporter NetworkPolicy must be named/select the pods' actual component label
# (postgres-exporter; the stale prometheus-exporter matched zero pods), and the postgresql
# NetworkPolicy must admit the monitoring-user hook Job on 5432 (else the install/upgrade
# hook fails under a default-deny CNI). pgpool is enabled so --show-only emits all NP docs
# (an empty middle doc otherwise collapses helm's multi-doc --show-only output).
netpol_fix=$(helm template test-pg "${CHART_DIR}" \
  --set prometheusExporter.enabled=true --set prometheusExporter.monitoringUser.enabled=true \
  --set pgpool.enabled=true --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "H1: exporter NetworkPolicy named -postgres-exporter" "${netpol_fix}" "name: test-pg-postgres-exporter"
assert_contains "H1: exporter NetworkPolicy selects postgres-exporter" "${netpol_fix}" "component: postgres-exporter"
assert_not_contains "H1: no stale prometheus-exporter label anywhere" "${netpol_fix}" "prometheus-exporter"
assert_contains "H2: postgresql NetworkPolicy admits monitoring-user" "${netpol_fix}" "component: monitoring-user"
# monitoring-user ingress is gated: absent when the monitoring user is disabled.
netpol_nomon=$(helm template test-pg "${CHART_DIR}" \
  --set prometheusExporter.enabled=true --set prometheusExporter.monitoringUser.enabled=false \
  --set pgpool.enabled=true --set networkPolicy.enabled=true \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_not_contains "H2: no monitoring-user ingress when disabled" "${netpol_nomon}" "component: monitoring-user"

# H8: values.schema.json enum guards reject typos at install time, not pod runtime.
schema_rc=0; helm template test-pg "${CHART_DIR}" --set prometheusExporter.sslmode=requir >/dev/null 2>&1 || schema_rc=$?
assert_gt "H8: bogus prometheusExporter.sslmode rejected by schema" "${schema_rc}" "0"
schema_rc=0; helm template test-pg "${CHART_DIR}" --set pgpool.tls.backendSslmode=bogus >/dev/null 2>&1 || schema_rc=$?
assert_gt "H8: bogus pgpool.tls.backendSslmode rejected by schema" "${schema_rc}" "0"
schema_rc=0; helm template test-pg "${CHART_DIR}" --set repmgr.agent.mechanism=patroni >/dev/null 2>&1 || schema_rc=$?
assert_gt "#287: bogus repmgr.agent.mechanism rejected by schema" "${schema_rc}" "0"
schema_rc=0; helm template test-pg "${CHART_DIR}" --set pgbackrest.s3.uriStyle=virtual >/dev/null 2>&1 || schema_rc=$?
assert_gt "H8: bogus pgbackrest.s3.uriStyle rejected by schema" "${schema_rc}" "0"
schema_rc=0; helm template test-pg "${CHART_DIR}" --set pgbackrest.repoEncryption.cipherType=bogus >/dev/null 2>&1 || schema_rc=$?
assert_gt "H8: bogus pgbackrest.repoEncryption.cipherType rejected by schema" "${schema_rc}" "0"

# --- low-priority review fixes (1.2.1) ---
# K8S-6: the agent ServiceMonitor selector is scoped to the postgresql component so it
# matches only the headless Service (which carries that label in its metadata + the
# agent-metrics port), not every Service in the release.
sm_scope=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.agent.monitoring.serviceMonitor.enabled=true \
  --show-only templates/agent-servicemonitor.yaml 2>&1)
assert_contains "K8S-6: agent ServiceMonitor selector scoped to postgresql component" "${sm_scope}" "app.kubernetes.io/component: postgresql"
hl_label=$(helm template test-pg "${CHART_DIR}" \
  --show-only templates/service-headless.yaml 2>&1)
# the label must live in metadata (the SM matches on metadata labels), i.e. above spec:
hl_meta=$(printf '%s\n' "${hl_label}" | sed -n '/^metadata:/,/^spec:/p')
assert_contains "K8S-6: headless Service carries component=postgresql in metadata" "${hl_meta}" "app.kubernetes.io/component: postgresql"
# K8S-5: one-shot Jobs/CronJobs carry the kube-linter probe waivers.
bk_waiver=$(helm template test-pg "${CHART_DIR}" --set backup.enabled=true \
  --set backup.existingSecret.name=s --set backup.s3.endpoint=https://m --set backup.s3.bucket=b \
  --show-only templates/backup-cronjob.yaml 2>&1)
assert_contains "K8S-5: backup CronJob has no-liveness-probe waiver" "${bk_waiver}" "ignore-check.kube-linter.io/no-liveness-probe"
assert_contains "K8S-5: backup CronJob has no-readiness-probe waiver" "${bk_waiver}" "ignore-check.kube-linter.io/no-readiness-probe"

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

# ======================================================================
# #110: client-connection TLS (PostgreSQL + PGPool + exporter sslmode)
# ======================================================================
# Off by default the render is byte-stable (the rest of this suite renders with
# TLS off); these assert the OFF render adds nothing, then exercise each knob.

# --- default (TLS off): no ssl, no cert mount, no TLS env ---
tls_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" 2>&1)
assert_not_contains "#110 off: no postgresql tls.conf ssl=on" "${tls_off}" "ssl = on"
assert_not_contains "#110 off: no postgresql-tls volume" "${tls_off}" "postgresql-tls"
assert_not_contains "#110 off: no TLS_REQUIRE_SSL env" "${tls_off}" "TLS_REQUIRE_SSL"

# --- Component 1: PostgreSQL server TLS (ssl=on via conf.d) ---
tls_cm=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#110 C1: tls.conf sets ssl = on" "${tls_cm}" "ssl = on"
assert_contains "#110 C1: tls.conf sets ssl_cert_file" "${tls_cm}" "ssl_cert_file = '/etc/postgresql/tls/tls.crt'"
assert_contains "#110 C1: tls.conf sets ssl_key_file" "${tls_cm}" "ssl_key_file = '/etc/postgresql/tls/tls.key'"
assert_not_contains "#110 C1: no ssl_ca_file without mTLS" "${tls_cm}" "ssl_ca_file = '"
# mTLS -> ssl_ca_file present (verifies client certs)
tls_cm_mtls=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.clientCertAuth=true \
  --set pgpool.enabled=false \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#110 C1: mTLS adds ssl_ca_file" "${tls_cm_mtls}" "ssl_ca_file = '/etc/postgresql/tls/ca.crt'"
# cert mount + volume (defaultMode 0400, BYO secret name)
tls_sts=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#110 C1: cert mounted at /etc/postgresql/tls" "${tls_sts}" "mountPath: /etc/postgresql/tls"
assert_contains "#110 C1: cert volume uses the BYO secret" "${tls_sts}" 'secretName: "pg-tls"'
assert_contains "#110 C1: cert volume defaultMode 0400" "${tls_sts}" "defaultMode: 0400"
# TLS-only install (no postgresql.configuration / pgbackrest): conf.d include must
# still be wired (checksum annotation + include_dir management) or ssl=on never applies.
assert_contains "#110 C1: TLS-only install keeps postgresql-config checksum" "${tls_sts}" "checksum/postgresql-config"
assert_contains "#110 C1: TLS-only install keeps the conf.d include" "${tls_sts}" "include_dir = '/etc/postgresql/conf.d'"
# standalone (repmgr off, replicaCount 0, persistence on): the cert VOLUME must
# render alongside its mount (the outer volumes gate must include tls.enabled).
tls_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.persistence.enabled=true \
  --show-only templates/statefulset.yaml 2>&1)
sa_mounts=$(printf '%s\n' "${tls_standalone}" | grep -c "name: postgresql-tls")
assert_eq "#110 C1: standalone+TLS emits both the tls mount AND volume" "2" "${sa_mounts}"
# fail-fast: tls.enabled without a Secret
tls_nosecret_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true >/dev/null 2>&1 || tls_nosecret_rc=$?
assert_eq "#110 C1: tls.enabled without existingSecret fails fast" "1" "$([ "${tls_nosecret_rc}" -ne 0 ] && echo 1 || echo 0)"

# --- Component 2: require/mTLS pg_hba env wiring (agent assembles pg_hba) ---
assert_contains "#110 C2: TLS_REQUIRE_SSL env present when tls.enabled" "${tls_sts}" "name: TLS_REQUIRE_SSL"
assert_contains "#110 C2: TLS_CLIENT_CERT_AUTH env present when tls.enabled" "${tls_sts}" "name: TLS_CLIENT_CERT_AUTH"
tls_require=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.require=true \
  --set prometheusExporter.enabled=true --set prometheusExporter.sslmode=require \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#110 C2: require -> TLS_REQUIRE_SSL true" "${tls_require}" 'name: TLS_REQUIRE_SSL
              value: "true"'
# monitoring user passed so the agent can exempt it from clientcert
tls_mon=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.clientCertAuth=true \
  --set prometheusExporter.enabled=true --set prometheusExporter.monitoringUser.enabled=true \
  --set prometheusExporter.sslmode=require \
  --set pgpool.enabled=false \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#110 C2: mTLS passes MONITORING_USER for exemption" "${tls_mon}" "name: MONITORING_USER"
# fail-fast: require/clientCertAuth need tls.enabled
tls_req_noenable_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.require=true >/dev/null 2>&1 || tls_req_noenable_rc=$?
assert_eq "#110 C2: require without tls.enabled fails fast" "1" "$([ "${tls_req_noenable_rc}" -ne 0 ] && echo 1 || echo 0)"
# fail-fast: require/mTLS need the agent-assembled pg_hba, so standalone rejects them
# (the stock image's md5-fallback line would bypass a hostssl rule).
tls_standalone_rc=0
helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.require=true >/dev/null 2>&1 || tls_standalone_rc=$?
assert_eq "#110 C2: require in standalone mode fails fast (HA-only)" "1" "$([ "${tls_standalone_rc}" -ne 0 ] && echo 1 || echo 0)"
# standalone with plain server TLS (no require) still renders ssl=on
tls_standalone_ok=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#110 C2: standalone allows optional server TLS (ssl=on)" "${tls_standalone_ok}" "ssl = on"

# --- Component 3: PGPool TLS (frontend + backend) ---
pp_tls=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "#110 C3: pgpool frontend ssl = on" "${pp_tls}" "ssl = on"
assert_contains "#110 C3: pgpool ssl_cert path" "${pp_tls}" "ssl_cert = '/usr/local/etc/tls/tls.crt'"
assert_not_contains "#110 C3: no ssl_ca_cert without frontend mTLS" "${pp_tls}" "ssl_ca_cert"
pp_tls_mtls=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --set pgpool.tls.clientCertAuth=true \
  --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "#110 C3: pgpool frontend mTLS adds ssl_ca_cert" "${pp_tls_mtls}" "ssl_ca_cert = '/usr/local/etc/tls/ca.crt'"
pp_dep=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --set pgpool.tls.backendSslmode=require \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "#110 C3: pgpool stages key as 0600" "${pp_dep}" "chmod 0600 /usr/local/etc/tls/tls.key"
assert_contains "#110 C3: pgpool-tls frontend volume" "${pp_dep}" "name: pgpool-tls"
assert_contains "#110 C3: backend PGSSLMODE set" "${pp_dep}" 'name: PGSSLMODE
              value: "require"'
assert_contains "#110 C3: pgpool-exporter SSLMODE follows frontend (require)" "${pp_dep}" 'name: SSLMODE
              value: "require"'
# backend client cert (for PostgreSQL mTLS passthrough)
pp_dep_bcc=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --set pgpool.tls.backendClientCert=pp-backend \
  --show-only templates/pgpool-deployment.yaml 2>&1)
assert_contains "#110 C3: backend client cert -> PGSSLCERT" "${pp_dep_bcc}" "name: PGSSLCERT"
assert_contains "#110 C3: backend client cert volume" "${pp_dep_bcc}" "name: pgpool-tls-backend"
# default off: pgpool ssl = off
pp_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --show-only templates/pgpool-configmap.yaml 2>&1)
assert_contains "#110 C3: pgpool off keeps ssl = off" "${pp_off}" "ssl = off"
# fail-fast: pgpool.tls.enabled without a Secret
pp_nosecret_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true >/dev/null 2>&1 || pp_nosecret_rc=$?
assert_eq "#110 C3: pgpool.tls.enabled without existingSecret fails fast" "1" "$([ "${pp_nosecret_rc}" -ne 0 ] && echo 1 || echo 0)"

# --- Component 4: exporter sslmode ---
exp_req=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set prometheusExporter.enabled=true --set prometheusExporter.sslmode=require 2>&1)
assert_contains "#110 C4: exporter DSN carries sslmode=require" "${exp_req}" "sslmode=require"
assert_contains "#110 C4: exporter auth_modules sslmode require" "${exp_req}" "sslmode: require"
assert_not_contains "#110 C4: require has no sslrootcert" "${exp_req}" "sslrootcert"
exp_verify=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set prometheusExporter.enabled=true --set prometheusExporter.sslmode=verify-ca 2>&1)
assert_contains "#110 C4: verify-ca adds sslrootcert" "${exp_verify}" "sslrootcert: /etc/postgres_exporter/tls/ca.crt"
assert_contains "#110 C4: verify-ca mounts the CA on the exporter" "${exp_verify}" "mountPath: /etc/postgres_exporter/tls"
# default off: sslmode disable
exp_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set prometheusExporter.enabled=true 2>&1)
assert_contains "#110 C4: exporter default sslmode=disable" "${exp_off}" "sslmode=disable"
# fail-fast: verify-* requires tls.enabled (CA comes from the server cert Secret)
exp_verify_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set prometheusExporter.enabled=true --set prometheusExporter.sslmode=verify-ca >/dev/null 2>&1 || exp_verify_rc=$?
assert_eq "#110 C4: verify-* without tls.enabled fails fast" "1" "$([ "${exp_verify_rc}" -ne 0 ] && echo 1 || echo 0)"

# --- Cross-component fail-fast guards ---
g_exp_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.require=true \
  --set prometheusExporter.enabled=true >/dev/null 2>&1 || g_exp_rc=$?
assert_eq "#110 guard: require + exporter sslmode=disable fails fast" "1" "$([ "${g_exp_rc}" -ne 0 ] && echo 1 || echo 0)"
g_pp_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.require=true \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --set pgpool.tls.backendSslmode=disable >/dev/null 2>&1 || g_pp_rc=$?
assert_eq "#110 guard: require + pgpool backendSslmode=disable fails fast" "1" "$([ "${g_pp_rc}" -ne 0 ] && echo 1 || echo 0)"
g_mtls_pp_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.clientCertAuth=true \
  --set prometheusExporter.enabled=true --set prometheusExporter.sslmode=require \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls >/dev/null 2>&1 || g_mtls_pp_rc=$?
assert_eq "#110 guard: mTLS + pgpool without backendClientCert fails fast" "1" "$([ "${g_mtls_pp_rc}" -ne 0 ] && echo 1 || echo 0)"
# clientCertAuth alone (require=false) also forces hostssl, so the sslmode guards must
# fire on it too -- else the exporter / pgpool backend silently break.
g_cca_exp_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.clientCertAuth=true --set postgresql.tls.require=false \
  --set prometheusExporter.enabled=true >/dev/null 2>&1 || g_cca_exp_rc=$?
assert_eq "#110 guard: clientCertAuth-only + exporter sslmode=disable fails fast" "1" "$([ "${g_cca_exp_rc}" -ne 0 ] && echo 1 || echo 0)"
g_cca_pp_rc=0
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --set postgresql.tls.clientCertAuth=true --set postgresql.tls.require=false \
  --set pgpool.enabled=true --set pgpool.tls.enabled=true --set pgpool.tls.existingSecret=pp-tls \
  --set pgpool.tls.backendClientCert=pp-backend \
  --set pgpool.tls.backendSslmode=disable >/dev/null 2>&1 || g_cca_pp_rc=$?
assert_eq "#110 guard: clientCertAuth-only + pgpool backendSslmode=disable fails fast" "1" "$([ "${g_cca_pp_rc}" -ne 0 ] && echo 1 || echo 0)"

# --- pgvector parity (templates are symlinks; values carry the TLS blocks) ---
pgv_tls=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=pg-tls \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#110 pgvector: server TLS renders ssl = on" "${pgv_tls}" "ssl = on"

# ======================================================================
# #29: cascading replication. Off by default (byte-stable); the agent gets the
# CASCADE_REPLICATION env only when the knob is on, and only in agent mode.
# ======================================================================
casc_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#29 off: no CASCADE_REPLICATION env by default" "${casc_off}" "CASCADE_REPLICATION"
casc_on=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.cascadingReplication=true \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#29 on: agent gets CASCADE_REPLICATION env" "${casc_on}" 'name: CASCADE_REPLICATION
              value: "true"'
# standalone: no agent, so the env is never emitted even if the knob is set
casc_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set repmgr.agent.cascadingReplication=true \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#29: standalone never gets CASCADE_REPLICATION (agent-only)" "${casc_standalone}" "CASCADE_REPLICATION"

# ======================================================================
# #288: MECHANISM must reach the INIT container, not just the postgresql one. Without it the
# init container's shell gate can never fire, so native standbys keep polling repmgr.nodes for
# a registration that never comes and sit in Init:CrashLoopBackOff forever. Occurrence count,
# not presence: getting it onto one container only is exactly the bug.
mech_native_res=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.agent.mechanism=native \
  --set postgresql.majorVersion=18 \
  --show-only templates/statefulset.yaml 2>&1)
assert_eq "#288: MECHANISM reaches both the init and postgresql containers" "2" \
  "$(printf '%s\n' "${mech_native_res}" | grep -c 'name: MECHANISM')"
# Specifically on repmgr-init, so a future refactor cannot satisfy the count with two copies
# on the same container.
# End anchor must render AFTER the start: fix-permissions is init container 1 and repmgr-init is
# container 4, so `/repmgr-init/,/fix-permissions/` ran to EOF and swallowed the postgresql
# container's MECHANISM too -- the assertion could not fail, which is the exact bug its own
# comment claimed to guard against. `- name: postgresql` is the next container after the init
# list, so it is a real terminator.
mech_init_block=$(printf '%s\n' "${mech_native_res}" | sed -n '/name: repmgr-init/,/^        - name: postgresql$/p')
assert_contains "#288: repmgr-init carries MECHANISM" "${mech_init_block}" "name: MECHANISM"
# And prove the range is actually bounded: exactly one MECHANISM inside it, not both.
assert_eq "#288: the repmgr-init range is bounded (one MECHANISM, not the whole render)" "1" \
  "$(printf '%s\n' "${mech_init_block}" | grep -c 'name: MECHANISM')"
# The default (repmgr) render must not gain the variable at all -- that is what keeps every
# existing release byte-identical.
mech_default_res=$(helm template test-pg "${CHART_DIR}" --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#288: the repmgr-mode render carries no MECHANISM" "${mech_default_res}" "name: MECHANISM"

# #287: experimental native HA mechanism selector. repmgr by default
# (byte-stable); the agent gets MECHANISM only when set to a non-default
# value, and only in agent mode (standalone has no agent to read it).
# ======================================================================
mech_off=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#287 default: no MECHANISM env" "${mech_off}" "MECHANISM"
mech_repmgr_explicit=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.mechanism=repmgr \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#287: explicit repmgr (the default) still emits no env (byte-stable)" "${mech_repmgr_explicit}" "MECHANISM"
mech_native=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.mechanism=native \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#287: agent gets MECHANISM=native env" "${mech_native}" 'name: MECHANISM
              value: "native"'
mech_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set repmgr.agent.mechanism=native \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#287: standalone never gets MECHANISM (agent-only)" "${mech_standalone}" "MECHANISM"

# #289: cascading replication makes a STANDBY an upstream, but native-mode slot ownership
# only ever runs on the primary -- so a cascading child would reference a slot that does
# not exist on its actual upstream and its walreceiver would refuse to start. Rejected at
# render time rather than shipping a combination that only breaks at runtime.
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.mechanism=native --set repmgr.agent.cascadingReplication=true \
  >/dev/null 2>&1 && mech_cascade_rc=0 || mech_cascade_rc=$?
assert_eq "#289: native + cascadingReplication fails the render" "1" "${mech_cascade_rc}"
mech_cascade_err=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.mechanism=native --set repmgr.agent.cascadingReplication=true 2>&1 || true)
assert_contains "#289: the error names both values and the fix" "${mech_cascade_err}" "cascadingReplication is not supported with repmgr.agent.mechanism: native"
# Neither flag alone may be affected -- this must forbid nothing that worked before.
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.mechanism=native >/dev/null 2>&1 && native_alone_rc=0 || native_alone_rc=$?
assert_eq "#289: native alone still renders" "0" "${native_alone_rc}"
helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.agent.cascadingReplication=true >/dev/null 2>&1 && cascade_alone_rc=0 || cascade_alone_rc=$?
assert_eq "#289: cascadingReplication alone (repmgr mode) still renders" "0" "${cascade_alone_rc}"

# ======================================================================
# #262: postgresql.extraVolumes / extraVolumeMounts / extraEnv passthrough,
# its render-time guards, and the repmgr shared_preload_libraries merge that
# the review surfaced (previously audit-gated -> silently dropped repmgr).
# ======================================================================
xv_args=(--set-json 'postgresql.extraVolumes=[{"name":"pgsodium-key","secret":{"secretName":"pgsodium-root-key","defaultMode":256}}]'
         --set-json 'postgresql.extraVolumeMounts=[{"name":"pgsodium-key","mountPath":"/etc/postgresql/pgsodium","readOnly":true}]'
         --set-json 'postgresql.extraEnv=[{"name":"SOME_FLAG","value":"1"},{"name":"KEY_REF","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]')

# Agent mode (the HA path the pgsodium failover case depends on)
xv_agent=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  "${xv_args[@]}" --show-only templates/statefulset.yaml 2>&1)
assert_contains "#262 agent: extra volume renders on the pod" "${xv_agent}" "secretName: pgsodium-root-key"
assert_contains "#262 agent: extra mount renders on the container" "${xv_agent}" "mountPath: /etc/postgresql/pgsodium"
assert_contains "#262 agent: extraEnv value form renders" "${xv_agent}" 'name: SOME_FLAG'
assert_contains "#262 agent: extraEnv valueFrom form renders" "${xv_agent}" 'secretKeyRef:'

# Default render must be byte-stable: nothing extra when the values are unset
xv_default=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-agent.yaml" \
  --show-only templates/statefulset.yaml 2>&1)
assert_not_contains "#262 off: no extra volume by default" "${xv_default}" "pgsodium-key"

# extraVolumes alone must open the volumes block with persistence ON (the new gate
# clause): with repmgr/tls/pgbackrest/extensions/configuration all off and persistence
# on, no other condition renders a `volumes:` block.
xv_gate=$(helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --set postgresql.persistence.enabled=true \
  --set-json 'postgresql.extraVolumes=[{"name":"pgsodium-key","emptyDir":{}}]' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#262 gate: extraVolumes alone opens the volumes block" "${xv_gate}" "pgsodium-key"

# --- guards: each plausible mistake must fail the render, not apply/runtime ---
guard_fails() { # name, extra helm args...
  local name="$1"; shift
  local rc=0
  helm template test-pg "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" "$@" >/dev/null 2>&1 || rc=$?
  assert_eq "${name}" "1" "$([ "${rc}" -ne 0 ] && echo 1 || echo 0)"
}
guard_fails "#262 guard: extraEnv as a map fails fast"          --set-json 'postgresql.extraEnv={"FOO":"bar"}'
guard_fails "#262 guard: extraVolumes as a map fails fast"      --set-json 'postgresql.extraVolumes={"name":"x"}'
guard_fails "#262 guard: extraVolumes without a name fails"     --set-json 'postgresql.extraVolumes=[{"emptyDir":{}}]'
guard_fails "#262 guard: extraVolumes colliding with data fails" --set-json 'postgresql.extraVolumes=[{"name":"data","emptyDir":{}}]'
guard_fails "#262 guard: extraVolumes colliding with repmgr-config fails" --set-json 'postgresql.extraVolumes=[{"name":"repmgr-config","emptyDir":{}}]'
guard_fails "#262 guard: duplicate extraVolumes name fails"     --set-json 'postgresql.extraVolumes=[{"name":"a","emptyDir":{}},{"name":"a","emptyDir":{}}]'
guard_fails "#262 guard: dangling extraVolumeMounts fails"      --set-json 'postgresql.extraVolumeMounts=[{"name":"typo","mountPath":"/x"}]'
guard_fails "#262 guard: extraVolumeMounts without mountPath fails" --set-json 'postgresql.extraVolumes=[{"name":"a","emptyDir":{}}]' --set-json 'postgresql.extraVolumeMounts=[{"name":"a"}]'
guard_fails "#262 guard: extraEnv reusing PGDATA fails"         --set-json 'postgresql.extraEnv=[{"name":"PGDATA","value":"/x"}]'
guard_fails "#262 guard: extraEnv reusing POSTGRES_PASSWORD fails" --set-json 'postgresql.extraEnv=[{"name":"POSTGRES_PASSWORD","value":"x"}]'
guard_fails "#262 guard: extraEnv reusing a disabled-feature name (REPMGR_DB) fails" --set-json 'postgresql.extraEnv=[{"name":"REPMGR_DB","value":"x"}]'
guard_fails "#262 guard: duplicate extraEnv name fails"         --set-json 'postgresql.extraEnv=[{"name":"A","value":"1"},{"name":"A","value":"2"}]'

# --- shared_preload_libraries: repmgr must survive an operator-set value with audit OFF ---
spl_repmgr=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=true --set repmgr.image.majorVersion=18 --set postgresql.majorVersion=18 \
  --set-string postgresql.configuration.shared_preload_libraries=pgsodium \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#262 preload: audit off keeps repmgr (merged)" "${spl_repmgr}" "shared_preload_libraries = 'repmgr,pgsodium'"
assert_not_contains "#262 preload: bare operator value is not left in custom.conf" "${spl_repmgr}" "shared_preload_libraries = 'pgsodium'"
# audit ON: unchanged behaviour (pgaudit.conf stays authoritative)
spl_audit=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=true --set repmgr.image.majorVersion=18 --set postgresql.majorVersion=18 \
  --set postgresql.audit.enabled=true \
  --set-string postgresql.configuration.shared_preload_libraries=pgsodium \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#262 preload: audit on merges repmgr + operator + pgaudit" "${spl_audit}" "shared_preload_libraries = 'repmgr,pgsodium,pgaudit'"
# standalone: nothing to preserve, operator value passes through untouched
spl_standalone=$(helm template test-pg "${CHART_DIR}" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set-string postgresql.configuration.shared_preload_libraries=pgsodium \
  --show-only templates/postgresql-configmap.yaml 2>&1)
assert_contains "#262 preload: standalone passes the value through" "${spl_standalone}" "shared_preload_libraries = 'pgsodium'"
assert_not_contains "#262 preload: standalone adds no repmgr" "${spl_standalone}" "repmgr,pgsodium"

# --- pgvector parity (symlinked templates + its own values defaults) ---
pgv_xv=$(helm template test-pgv "${PGVECTOR_DIR}" "${xv_args[@]}" \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#262 pgvector: extra volume renders" "${pgv_xv}" "secretName: pgsodium-root-key"
assert_contains "#262 pgvector: extra mount renders" "${pgv_xv}" "mountPath: /etc/postgresql/pgsodium"
pgv_guard_rc=0
helm template test-pgv "${PGVECTOR_DIR}" --set-json 'postgresql.extraEnv=[{"name":"PGDATA","value":"/x"}]' >/dev/null 2>&1 || pgv_guard_rc=$?
assert_eq "#262 pgvector: guards apply too" "1" "$([ "${pgv_guard_rc}" -ne 0 ] && echo 1 || echo 0)"

# --- #276 agent control REST API ---

ctl_base=(--set repmgr.enabled=true
  --set repmgr.image.majorVersion=18 --set postgresql.majorVersion=18)

# Default: the control API is OFF, and nothing about it appears anywhere. This is the
# byte-stability contract -- a release that does not enable it renders as before.
ctl_off=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" 2>&1)
assert_not_contains "#276 default: no control listener" "${ctl_off}" "agent-control"
assert_not_contains "#276 default: no CONTROL_ env" "${ctl_off}" "CONTROL_ENABLED"
assert_not_contains "#276 default: no control TLS mount" "${ctl_off}" "/etc/agent-control-tls"
assert_not_contains "#276 default: no batch RBAC" "${ctl_off}" 'resources: \["jobs"\]'
assert_not_contains "#276 default: no pods/log RBAC" "${ctl_off}" 'resources: \["pods/log"\]'

# Enabling it without the mTLS Secret must fail at RENDER, not leave the operator with
# a crash-looping database: the agent refuses to open an unauthenticated mutating port.
ctl_nosecret_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true >/dev/null 2>&1 || ctl_nosecret_rc=$?
assert_eq "#276 enabled without tls.existingSecret fails to render" "1" "$([ "${ctl_nosecret_rc}" -ne 0 ] && echo 1 || echo 0)"

# The control API is served by the Go agent, so standalone mode cannot offer it.
ctl_standalone_rc=0
helm template test-pg "${CHART_DIR}" --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  >/dev/null 2>&1 || ctl_standalone_rc=$?
assert_eq "#276 control in standalone mode fails to render" "1" "$([ "${ctl_standalone_rc}" -ne 0 ] && echo 1 || echo 0)"

# Sharing the metrics port would defeat the whole point of separate listeners: the
# NetworkPolicy admits every same-namespace pod to 9200 so scrapes work.
ctl_port_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set repmgr.agent.control.port=9200 >/dev/null 2>&1 || ctl_port_rc=$?
assert_eq "#276 control.port=9200 (the metrics port) fails to render" "1" "$([ "${ctl_port_rc}" -ne 0 ] && echo 1 || echo 0)"
ctl_port5432_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set repmgr.agent.control.port=5432 >/dev/null 2>&1 || ctl_port5432_rc=$?
assert_eq "#276 control.port=5432 fails to render" "1" "$([ "${ctl_port5432_rc}" -ne 0 ] && echo 1 || echo 0)"

# Enabled: port, env and the mTLS mount, and STILL no restore RBAC (separate opt-in).
ctl_on=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true \
  --set repmgr.agent.control.tls.existingSecret=ctl-tls 2>&1)
assert_contains "#276 enabled: control containerPort" "${ctl_on}" "name: agent-control"
assert_contains "#276 enabled: default port 9201" "${ctl_on}" "containerPort: 9201"
assert_contains "#276 enabled: CONTROL_ENABLED" "${ctl_on}" "CONTROL_ENABLED"
assert_contains "#276 enabled: CONTROL_ADDR is the control port" "${ctl_on}" '":9201"'
assert_contains "#276 enabled: server cert path" "${ctl_on}" "/etc/agent-control-tls/tls.crt"
assert_contains "#276 enabled: client CA path" "${ctl_on}" "/etc/agent-control-tls/ca.crt"
assert_contains "#276 enabled: TLS secret mounted" "${ctl_on}" 'secretName: "ctl-tls"'
assert_contains "#276 enabled: metrics port still present" "${ctl_on}" "name: agent-metrics"
# Enabling the API alone must NOT widen Kubernetes RBAC at all.
assert_not_contains "#276 enabled: no Job RBAC without restore" "${ctl_on}" 'resources: \["jobs"\]'
assert_not_contains "#276 enabled: no CronJob RBAC without restore" "${ctl_on}" 'resources: \["cronjobs"\]'
assert_not_contains "#276 enabled: restore env absent" "${ctl_on}" "CONTROL_RESTORE_ENABLED"

# A custom port must reach both the containerPort and CONTROL_ADDR.
ctl_port=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set repmgr.agent.control.port=9301 --show-only templates/statefulset.yaml 2>&1)
assert_contains "#276 custom port: containerPort" "${ctl_port}" "containerPort: 9301"
assert_contains "#276 custom port: CONTROL_ADDR" "${ctl_port}" '":9301"'

# CN allowlist passes through as a comma-separated list.
ctl_cns=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set 'repmgr.agent.control.allowedClientCNs={ops-admin,ci-bot}' \
  --show-only templates/statefulset.yaml 2>&1)
assert_contains "#276 allowlist: CNs joined" "${ctl_cns}" "ops-admin,ci-bot"

# --- restore triggering: the separate opt-in with the sharp RBAC ---

ctl_restore_args=("${ctl_base[@]}"
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls
  --set repmgr.agent.control.restore.enabled=true
  --set 'repmgr.agent.control.restore.allowedClientCNs={dba-break-glass}'
  --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://s3 --set pgbackrest.s3.bucket=b
  --set pgbackrest.s3.keyType=auto)

# Every dependency is a render guard, because each one makes the API unusable in a way
# that would otherwise only show up as a failed request during an incident.
for guard in \
  "--set repmgr.agent.control.enabled=false:control disabled" \
  "--set pgbackrest.restore.enabled=false:restore Job not rendered" \
  "--set pgbackrest.restore.mode=job:mode=job has no CronJob to clone" \
  "--set repmgr.agent.control.restore.allowedClientCNs=null:empty allowlist denies everyone"
do
  flag="${guard%%:*}"; why="${guard##*:}"
  rc=0
  # shellcheck disable=SC2086
  helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
    ${flag} >/dev/null 2>&1 || rc=$?
  assert_eq "#276 restore guard: ${why}" "1" "$([ "${rc}" -ne 0 ] && echo 1 || echo 0)"
done

# readPodLogs without restore is a mistake worth naming: it only widens RBAC.
ctl_logs_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set repmgr.agent.control.restore.readPodLogs=true >/dev/null 2>&1 || ctl_logs_rc=$?
assert_eq "#276 readPodLogs without restore fails to render" "1" "$([ "${ctl_logs_rc}" -ne 0 ] && echo 1 || echo 0)"

ctl_restore=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" \
  --set pgbackrest.restore.enabled=true 2>&1)
assert_contains "#276 restore: env enabled" "${ctl_restore}" "CONTROL_RESTORE_ENABLED"
assert_contains "#276 restore: allowlist passed" "${ctl_restore}" "dba-break-glass"
assert_contains "#276 restore: CronJob to clone named" "${ctl_restore}" "test-pg-pgbackrest-restore"
assert_contains "#276 restore: deterministic Job name" "${ctl_restore}" "test-pg-pgbackrest-restore-api"
assert_contains "#276 restore: pod ordinal passed for confirmation" "${ctl_restore}" "CONTROL_RESTORE_POD_ORDINAL"
# create on jobs cannot be resourceName-scoped; get/delete can, and must be.
assert_contains "#276 restore RBAC: create jobs" "${ctl_restore}" 'resources: \["jobs"\]'
assert_contains "#276 restore RBAC: get/delete scoped to the one Job" "${ctl_restore}" 'resourceNames: \["test-pg-pgbackrest-restore-api"\]'
assert_contains "#276 restore RBAC: CronJob get scoped" "${ctl_restore}" 'resourceNames: \["test-pg-pgbackrest-restore"\]'
# pods/log stays out unless explicitly asked for.
assert_not_contains "#276 restore RBAC: no pods/log by default" "${ctl_restore}" 'resources: \["pods/log"\]'
# The requester reaches the container via the downward API, for the record restore.sh
# writes onto the data volume.
assert_contains "#276 restore: requester env from pod annotation" "${ctl_restore}" "metadata.annotations\['pg-ha/requested-by'\]"

ctl_restore_logs=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" \
  --set pgbackrest.restore.enabled=true --set repmgr.agent.control.restore.readPodLogs=true 2>&1)
assert_contains "#276 readPodLogs: env set" "${ctl_restore_logs}" "CONTROL_RESTORE_READ_POD_LOGS"
assert_contains "#276 readPodLogs: pods/log granted" "${ctl_restore_logs}" 'resources: \["pods/log"\]'

# The restore program records its outcome on the data volume so it outlives the Job.
ctl_restore_sh=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" \
  --set pgbackrest.restore.enabled=true --show-only templates/pgbackrest-configmap.yaml 2>&1)
assert_contains "#276 restore.sh: writes a status record" "${ctl_restore_sh}" "pgbackrest-restore.status"
assert_contains "#276 restore.sh: atomic write via a temp file" "${ctl_restore_sh}" "STATUS_FILE}.tmp"
# The record is a cross-language contract: restore.sh (bash) writes it and the agent's
# internal/pgbackrest (Go) parses it. Assert every key the parser reads, so renaming one
# on either side is caught here rather than silently reporting an empty lastRestore.
for key in startedAt finishedAt stanza targetType target backupSet exitCode requestedBy clusterState checkpoint; do
  assert_contains "#276 restore.sh: records ${key}" "${ctl_restore_sh}" "${key}="
done
# The trap is installed AFTER the postmaster.pid interlock, so a refused attempt cannot
# clobber the record of the restore the data directory actually came from.
assert_contains "#276 restore.sh: outcome trap installed on EXIT" "${ctl_restore_sh}" "trap 'write_status"
# A FAILED attempt must not become the directory's provenance either: many failures copy
# nothing, so the previous record's descriptive fields are kept and the attempt is recorded
# separately. Without this a mistyped PITR target erases where the data really came from.
assert_contains "#276 restore.sh: reads the previous record on failure" "${ctl_restore_sh}" "prev_field"
for key in attemptedTargetType attemptedTarget attemptedBackupSet; do
  assert_contains "#276 restore.sh: records ${key} on failure" "${ctl_restore_sh}" "${key}="
done

# --- #279: the job-create grant and its admission-policy bound ---
#
# The cross-render invariant, which helm-unittest cannot express because it asserts one
# template at a time: any render carrying the unscopeable `create jobs` rule must also
# carry the ValidatingAdmissionPolicy that bounds it. A Role with the escalation primitive
# and no policy is the exact state #279 exists to make unreachable.
assert_contains "#279: the policy renders alongside the grant" "${ctl_restore}" "kind: ValidatingAdmissionPolicy"
assert_contains "#279: so does its binding" "${ctl_restore}" "kind: ValidatingAdmissionPolicyBinding"
assert_contains "#279: fails closed -- Ignore would silently re-open the hole" "${ctl_restore}" "failurePolicy: Fail"
assert_contains "#279: the binding denies rather than warns" "${ctl_restore}" 'validationActions: \["Deny"\]'
# Cluster-scoped objects, so the name must be unique per (namespace, release). The namespace
# and fullname are there to be read; the trailing hash of the "<namespace>/<fullname>" pair is
# what actually discriminates, because a bare hyphen join maps distinct pairs onto one name
# ("db" in "pg-prod" and "prod-db" in "pg"). Asserted as a shape, not a literal, so the
# assertion cannot pass on a name that dropped the hash.
assert_contains "#279: the cluster-scoped name carries a discriminating hash" "${ctl_restore}" \
  "name: default-test-pg-restore-guard-[0-9a-f]\{8\}$"
# The ambiguity itself: the two colliding (namespace, release) pairs must not produce one name.
vap_name() { helm template "$2" "${CHART_DIR}" "${ctl_restore_args[@]}" -n "$1" \
  --set pgbackrest.restore.enabled=true --show-only templates/agent-restore-admissionpolicy.yaml 2>/dev/null \
  | awk '/^kind: ValidatingAdmissionPolicy$/{f=1} f&&/^  name:/{print $2; exit}'; }
assert_not_eq "#279: a hyphen-ambiguous namespace/release pair does not collide" \
  "$(vap_name pg-prod db)" "$(vap_name pg prod-db)"
# Exactly one subject is policed. Every other creator of Jobs in the cluster -- humans,
# GitOps controllers, the CronJob controller -- must be untouched.
assert_contains "#279: policed subject is pinned exactly" "${ctl_restore}" \
  "request.userInfo.username == 'system:serviceaccount:default:test-pg-repmgr'"
ctl_vap_subject=$(grep -o 'expression: "request.userInfo.username[^"]*"' <<< "${ctl_restore}")
# Non-empty first: assert_not_contains passes on an empty haystack, so without this the check
# below stops testing anything the moment the template emits that scalar differently.
assert_contains "#279: the subject expression was actually extracted" "${ctl_vap_subject}" "userInfo"
assert_not_contains "#279: not a prefix/suffix match on the username" "${ctl_vap_subject}" "endsWith("
# The binding must NOT narrow by objectSelector: a Job created without the pg-ha/restore
# label would then simply not match the binding, and would be admitted -- making the label
# validation self-defeating. What the caller controls is checked by a validation, never by
# a match rule.
assert_not_contains "#279: the binding has no objectSelector" "${ctl_restore}" "objectSelector"

# The grant cannot be rendered without the bound unless the trade is stated in values.
ctl_vap_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set repmgr.agent.control.restore.admissionPolicy.enabled=false >/dev/null 2>&1 || ctl_vap_rc=$?
assert_eq "#279 guard: disabling the policy without acknowledging it fails to render" "1" \
  "$([ "${ctl_vap_rc}" -ne 0 ] && echo 1 || echo 0)"
ctl_vap_ack=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set repmgr.agent.control.restore.admissionPolicy.enabled=false \
  --set repmgr.agent.control.restore.admissionPolicy.acknowledgeUnbounded=true 2>&1)
assert_not_contains "#279 opt-out: acknowledged, so no policy is rendered" "${ctl_vap_ack}" "kind: ValidatingAdmissionPolicy"
assert_contains "#279 opt-out: the grant itself is unchanged" "${ctl_vap_ack}" 'resources: \["jobs"\]'

# Default install: the chart's only cluster-scoped objects stay absent, so a namespace-
# limited installer is unaffected by this feature existing.
assert_not_contains "#279 default: no cluster-scoped policy" "${minimal}" "ValidatingAdmissionPolicy"

# Drift guard, and the reason it earns a place here: the policy pins literal values that
# the restore CronJob renders independently. If the two ever disagree, failurePolicy: Fail
# turns that into every API-driven restore being denied -- a hard outage of the feature,
# discovered during an incident. Compare them directly instead.
ctl_vap=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
ctl_vap_cj=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --show-only templates/pgbackrest-restore-job.yaml 2>&1)
pinned() { grep -o "$1 == '[^']*'" <<< "${ctl_vap}" | head -1 | sed "s/.*'\(.*\)'/\1/"; }
# Numbers and booleans are interpolated into CEL unquoted, so they need their own extractor.
pinned_bare() { grep -o "$1 == [A-Za-z0-9]*" <<< "${ctl_vap}" | head -1 | sed "s/.* == //"; }
rendered() { grep -o "$1: [\"']\?[^\"']*" <<< "${ctl_vap_cj}" | head -1 | sed "s/^[^:]*: [\"']\?//"; }
# Each drift pair asserts non-emptiness first, so a renamed template or a moved expression
# fails loudly instead of comparing "" with "".
drift() {
  local what="$1" have="$2" want="$3"
  if [[ -z "${have}" || -z "${want}" ]]; then
    assert_eq "#279 drift: ${what} (both sides must be non-empty)" "non-empty/non-empty" "${have}/${want}"
  else
    assert_eq "#279 drift: ${what}" "${have}" "${want}"
  fi
}
drift "pinned ServiceAccount is the one the Job renders" \
  "$(rendered serviceAccountName)" "$(pinned 'variables.pod.serviceAccountName')"
drift "pinned image is the one the Job renders" \
  "$(rendered image)" "$(pinned 'c.image')"
drift "pinned data PVC is the one the Job mounts" \
  "$(rendered claimName)" "$(pinned 'v.persistentVolumeClaim.claimName')"
drift "pinned label is the one the jobTemplate carries" \
  "$(rendered 'pg-ha/restore')" "$(pinned "object.metadata.labels\['pg-ha/restore'\]")"
# The command, the securityContext values and the pod labels are pinned from the release's
# own values too, and a mismatch there denies every restore just as hard. Compared against
# the CronJob's independently rendered text rather than against a literal.
drift "pinned command is the one the Job runs" \
  "$(grep -o 'command: \[.*\]' <<< "${ctl_vap_cj}" | head -1 | sed 's/command: //')" \
  "$(grep -o "c.command == \[[^]]*\]" <<< "${ctl_vap}" | head -1 | sed "s/c.command == //;s/'/\"/g;s/,\"/, \"/g")"
drift "pinned container runAsUser is the one the Job runs as" \
  "$(grep -o 'runAsUser: [0-9]*' <<< "${ctl_vap_cj}" | head -1 | sed 's/runAsUser: //')" \
  "$(pinned_bare 'c.securityContext.runAsUser')"
drift "pinned pod component label is the one the pod template carries" \
  "$(grep -o 'app.kubernetes.io/component: pgbackrest-restore' <<< "${ctl_vap_cj}" | head -1 | sed 's/.*: //')" \
  "$(pinned "variables.podLabels\['app.kubernetes.io/component'\]")"
# The label lives on the jobTemplate, not only on the CronJob: `kubectl create job --from`
# and the agent's clone copy THOSE, and nothing else puts labels on the created Job.
assert_contains "#279: the jobTemplate carries the label the policy requires" "${ctl_vap_cj}" "pg-ha/restore: test-pg"
# The four labels the API SERVER stamps on a Job's pod template before validating admission
# runs. They must stay in the key allowlist: without them the pod-label rule denies every
# legitimate restore, which is how this rule was caught in the first place.
for k in batch.kubernetes.io/controller-uid batch.kubernetes.io/job-name controller-uid job-name; do
  assert_contains "#279: the pod-label allowlist tolerates the server-stamped ${k}" "${ctl_vap}" "'${k}'"
done
# Pins that exist to stop the Job being "run anything as the database's uid with the live
# PGDATA mounted", or a strictly larger privilege set than the token started with.
assert_contains "#279: nodeName is denied outright" "${ctl_vap}" '!has(variables.pod.nodeName)'
assert_contains "#279: manualSelector is denied" "${ctl_vap}" '!has(object.spec.manualSelector)'
assert_contains "#279: args are denied alongside the pinned command" "${ctl_vap}" '!has(c.args)'
assert_contains "#279: added capabilities are bounded" "${ctl_vap}" 'c.securityContext.capabilities.add'
# seccomp is pinned to the release's own: the command pin does not make code execution in the
# restore container impossible (bash reads $BASH_ENV, and PGDATA is mounted), so Unconfined
# would widen past "the privileges the token already holds".
assert_contains "#279: the pod seccomp profile is pinned to the release's own" "${ctl_vap}" \
  "variables.pod.securityContext.seccompProfile.type == 'RuntimeDefault'"
assert_contains "#279: a single pod only" "${ctl_vap}" 'object.spec.parallelism <= 1'
assert_contains "#279: requests and limits are required" "${ctl_vap}" "'cpu' in c.resources.limits"
# priorityClassName is pinned, unlike nodeSelector/tolerations: a priority class does not
# restrict placement, it lets the scheduler PREEMPT this release's own postgresql pods.
assert_contains "#279: priorityClassName is pinned, not just node placement" "${ctl_vap}" \
  '!has(variables.pod.priorityClassName)'
ctl_vap_pcn=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set pgbackrest.cronjob.priorityClassName=db-critical --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
assert_contains "#279: a values-set priority class widens the pin to exactly it" "${ctl_vap_pcn}" \
  "variables.pod.priorityClassName == 'db-critical'"
# A lifecycle hook runs caller-chosen code in the pinned container without touching command.
assert_contains "#279: lifecycle hooks are denied alongside the command pin" "${ctl_vap}" '!has(c.lifecycle)'
# An operator who empties the security contexts must not have their own restore denied:
# `containerSecurityContext: {}` renders `securityContext: {}`, which has() sees as PRESENT,
# so the no-values branch has to be "absent OR sets none of the pinned fields".
ctl_vap_emptysc=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set postgresql.containerSecurityContext=null --set postgresql.podSecurityContext=null \
  --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
assert_contains "#279: emptied security contexts tolerate a present-but-empty securityContext" \
  "${ctl_vap_emptysc}" '!has(c.securityContext) || (!has(c.securityContext.privileged)'
# Every value reaching a single-quoted CEL literal is charset-checked, not just the obvious
# ones: `RuntimeDefault'"'"' || true || '"'"'` renders a valid always-true expression.
for crafted in "postgresql.podSecurityContext.seccompProfile.type=x' || true || '" \
               "postgresql.containerSecurityContext.seccompProfile.type=x' || true || '" \
               "postgresql.containerSecurityContext.capabilities.add={SYS_ADMIN' || true || '}" \
               "pgbackrest.cronjob.priorityClassName=p' || true || '"; do
  rc=0
  crafted_out=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
    --set "${crafted}" 2>&1) || rc=$?
  assert_eq "#279: a CEL tautology through ${crafted%%=*} fails the render" "1" \
    "$([ "${rc}" -ne 0 ] && echo 1 || echo 0)"
  # Non-zero alone is not enough: a render that failed for an unrelated reason (a future
  # schema constraint on these values, say) would keep this passing while the charset guard
  # stopped being exercised.
  assert_contains "#279: ...and it is the CEL-literal guard that rejected it" "${crafted_out}" \
    "cannot be embedded in the restore admission policy"
done

# The security-context pins must MOVE with the values rather than being a fixed profile: a
# hardcoded hardened profile would deny the release's own restore Job the moment an operator
# legitimately changed the context, which is worse than no policy at all.
ctl_vap_priv=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set postgresql.containerSecurityContext.privileged=true \
  --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
assert_contains "#279: a values-set privileged container widens the pin, not denies the release" \
  "${ctl_vap_priv}" 'c.securityContext.privileged == true'

# With cloud workload identity the release reads no Secret at all. The rule must stay an
# ALLOWLIST of sources (fieldRef only) rather than degrading into "no secretKeyRef", which
# would ADMIT configMapKeyRef in exactly the keyless configuration -- the opposite of what
# its own message promises, and the branch CI never used to render.
assert_contains "#279 keyType=auto: only the downward API is permitted" "${ctl_vap}" \
  '!has(e.valueFrom) || (has(e.valueFrom.fieldRef))'
assert_not_contains "#279 keyType=auto: configMapKeyRef is not admitted by a bare negation" \
  "${ctl_vap}" '!has(e.valueFrom.secretKeyRef)'

# The >= 1.30 requirement is enforced by asking whether admissionregistration.k8s.io/v1
# EXISTS, not by comparing .Capabilities.KubeVersion: with no cluster that reports the HELM
# CLIENT's version (helm 3.14 says v1.29), so a semver floor fails every `helm template` run
# by an older helm regardless of the target cluster. Assert the accurate shape here; the
# negative case cannot be produced from the CLI, because --api-versions only ADDS to the
# default set, so it lives in tests/unit/restore_admissionpolicy_nocapability_test.yaml.
assert_contains "#279: the API-group precondition is what gates the policy" "$(cat "${CHART_DIR}/templates/agent-restore-admissionpolicy.yaml")" \
  'APIVersions.Has "admissionregistration.k8s.io/v1"'
assert_not_contains "#279: the client's own kube version does not gate the render" \
  "$(cat "${CHART_DIR}/templates/agent-restore-admissionpolicy.yaml")" 'semverCompare ">=1.30'
# An old helm renders it fine, which is the regression this replaced: no cluster, client
# version below the floor, policy still rendered.
ctl_vap_oldkube=$(helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --kube-version 1.29.0 --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
assert_contains "#279: a client reporting 1.29 with no cluster still renders the policy" \
  "${ctl_vap_oldkube}" "kind: ValidatingAdmissionPolicy"

# A value with a quote would produce a broken CEL expression -- or, crafted, a tautological
# one that looks installed and enforces nothing. Both must fail the render, not the apply.
ctl_vap_quote_rc=0
helm template test-pg "${CHART_DIR}" "${ctl_restore_args[@]}" --set pgbackrest.restore.enabled=true \
  --set pgbackrest.repoEncryption.enabled=true \
  --set "pgbackrest.repoEncryption.existingSecret.name=dba's-key" >/dev/null 2>&1 || ctl_vap_quote_rc=$?
assert_eq "#279: a value that cannot be embedded in a CEL literal fails the render" "1" \
  "$([ "${ctl_vap_quote_rc}" -ne 0 ] && echo 1 || echo 0)"
# Shared keys mean exactly one legitimate Secret; an encryption passphrase adds a second.
ctl_vap_shared=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set repmgr.agent.control.restore.enabled=true \
  --set 'repmgr.agent.control.restore.allowedClientCNs={dba}' \
  --set pgbackrest.enabled=true --set pgbackrest.s3.endpoint=https://s3 --set pgbackrest.s3.bucket=b \
  --set pgbackrest.s3.keyType=shared --set pgbackrest.existingSecret.name=s3creds \
  --set pgbackrest.repoEncryption.enabled=true --set pgbackrest.repoEncryption.existingSecret.name=cipher \
  --set pgbackrest.restore.enabled=true --show-only templates/agent-restore-admissionpolicy.yaml 2>&1)
assert_contains "#279 shared keys: only this release's own Secrets are reachable" "${ctl_vap_shared}" \
  "in \['s3creds','cipher'\]"

# --- NetworkPolicy: deny-by-default on the control port ---

ctl_np=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "#276 netpol: metrics port still admitted" "${ctl_np}" "port: 9200"
# No rule for the control port: this policy is an allowlist, so absence IS deny.
assert_not_contains "#276 netpol: control port has no default rule" "${ctl_np}" "port: 9201"

ctl_np_open=$(helm template test-pg "${CHART_DIR}" "${ctl_base[@]}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls \
  --set networkPolicy.enabled=true \
  --set-json 'networkPolicy.agentControl.extraIngress=[{"ports":[{"port":9201,"protocol":"TCP"}],"from":[{"podSelector":{"matchLabels":{"app":"ops"}}}]}]' \
  --show-only templates/networkpolicy.yaml 2>&1)
assert_contains "#276 netpol: extraIngress admits a named client" "${ctl_np_open}" "port: 9201"
assert_contains "#276 netpol: extraIngress carries its selector" "${ctl_np_open}" "app: ops"

# --- pgvector parity (symlinked templates, its own values defaults) ---

pgv_ctl_off=$(helm template test-pgv "${PGVECTOR_DIR}" 2>&1)
assert_not_contains "#276 pgvector default: no control listener" "${pgv_ctl_off}" "agent-control"
pgv_ctl_on=$(helm template test-pgv "${PGVECTOR_DIR}" \
  --set repmgr.agent.control.enabled=true --set repmgr.agent.control.tls.existingSecret=ctl-tls 2>&1)
assert_contains "#276 pgvector: control renders" "${pgv_ctl_on}" "name: agent-control"
pgv_ctl_rc=0
helm template test-pgv "${PGVECTOR_DIR}" \
  --set repmgr.agent.control.enabled=true >/dev/null 2>&1 || pgv_ctl_rc=$?
assert_eq "#276 pgvector: guards apply too" "1" "$([ "${pgv_ctl_rc}" -ne 0 ] && echo 1 || echo 0)"

end_suite
print_summary
