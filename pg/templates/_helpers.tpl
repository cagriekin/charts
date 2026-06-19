{{- define "pg.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "pg.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{/*
Validate composed resource names against Kubernetes limits (#158). pg.fullname is
capped at 63, but per-resource suffixes are appended AFTER it, so a long
fullnameOverride can push a Service name past 63 (RFC1035 label) or a CronJob name
past ~52 (a CronJob must leave room for the generated -<timestamp> Job and -<hash>
Pod name suffixes). Truncating composed names is unsafe on a STATEFUL chart -- two
long names could collide on one StatefulSet/PVC -- so fail fast at render time with a
clear hint instead of a confusing API rejection at apply / first scheduled run.
*/}}
{{/*
Small default resources for the lightweight init containers (chown, cp, config-gen).
Init containers without requests/limits make every pod Forbidden in ResourceQuota-
enforced namespaces (#153). repmgr-init (the standby clone) is heavier and uses its own
values-overridable repmgr.initContainerResources instead.
*/}}
{{- define "pg.initResources" -}}
requests:
  cpu: 10m
  memory: 16Mi
limits:
  cpu: 100m
  memory: 64Mi
{{- end -}}

{{- define "pg.validateResourceNames" -}}
{{- $f := include "pg.fullname" . -}}
{{- /* Plain Services (and the base name): RFC1035 label, max 63. */ -}}
{{- $services := list $f (printf "%s-headless" $f) (printf "%s-readonly" $f) -}}
{{- range $n := $services -}}
{{- if gt (len $n) 63 -}}
{{- fail (printf "\n\nresource name %q is %d chars, but Kubernetes Service names are limited to 63 (RFC1035 label). Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- /* Deployment-backed names (pgpool, exporter) are ALSO Services, but the binding
limit is the Deployment's generated Pod name <name>-<rs-hash>-<suffix>: the ~16-char
ReplicaSet-hash + Pod suffix must fit under 63, so the name itself must be <= 47. */ -}}
{{- $deployments := list -}}
{{- if .Values.pgpool.enabled -}}{{- $deployments = append $deployments (printf "%s-pgpool" $f) -}}{{- end -}}
{{- if .Values.prometheusExporter.enabled -}}{{- $deployments = append $deployments (printf "%s-postgres-exporter" $f) -}}{{- end -}}
{{- range $n := $deployments -}}
{{- if gt (len $n) 47 -}}
{{- fail (printf "\n\nDeployment name %q is %d chars, but must be <= 47 to leave room for the generated ReplicaSet-hash + Pod name suffixes (a 63-char Pod name limit). Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- /* CronJobs: max 52, to leave room for the generated -<timestamp> Job suffix. */ -}}
{{- $cronjobs := list -}}
{{- if .Values.backup.enabled -}}{{- $cronjobs = append $cronjobs (printf "%s-backup" $f) -}}{{- end -}}
{{- if .Values.pgbackrest.enabled -}}{{- $cronjobs = concat $cronjobs (list (printf "%s-pgbackrest-full" $f) (printf "%s-pgbackrest-diff" $f)) -}}{{- end -}}
{{- range $n := $cronjobs -}}
{{- if gt (len $n) 52 -}}
{{- fail (printf "\n\nCronJob name %q is %d chars, but must be <= 52 to leave room for the generated Job/Pod name suffixes. Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pg.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "pg.labels" -}}
{{ include "common.labels" . }}
{{- end }}

{{- define "pg.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{/*
Global annotations applied to every resource's metadata. Returns the
YAML for .Values.global.annotations, or "" when unset. Call sites guard
with `{{- with (include "pg.annotations" .) }}` so a resource stays
annotation-free when no global annotations are configured. (Previously
these were wrongly merged into pg.labels and rendered as metadata.labels,
which both broke apply for non-label-safe values and hid them from
annotation consumers -- #128.)
*/}}
{{- define "pg.annotations" -}}
{{- with .Values.global.annotations }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{- /* repmgr image reference: repository:tag, with @digest appended when set so a
       digest pin (supply-chain) overrides the mutable tag. */ -}}
{{- define "pg.repmgrImage" -}}
{{- printf "%s:%s" .Values.repmgr.image.repository .Values.repmgr.image.tag -}}
{{- with .Values.repmgr.image.digest }}@{{ . }}{{- end -}}
{{- end -}}

{{- define "pg.secretName" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- required "postgresql.existingSecret.name is required when postgresql.existingSecret.enabled is true" .Values.postgresql.existingSecret.name }}
{{- else }}
{{- include "pg.fullname" . }}
{{- end }}
{{- end }}

{{- define "pg.secretUsernameKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.usernameKey }}
{{- else }}
{{- "username" }}
{{- end }}
{{- end }}

{{- define "pg.secretPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.passwordKey }}
{{- else }}
{{- "password" }}
{{- end }}
{{- end }}

{{- define "pg.secretDatabaseKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.databaseKey }}
{{- else }}
{{- "database" }}
{{- end }}
{{- end }}

{{- define "pg.secretRepmgrPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.repmgrPasswordKey }}
{{- else }}
{{- "repmgr-password" }}
{{- end }}
{{- end }}

{{- define "pg.secretMonitoringPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- required "postgresql.existingSecret.monitoringPasswordKey is required when prometheusExporter.monitoringUser.enabled and postgresql.existingSecret.enabled" .Values.postgresql.existingSecret.monitoringPasswordKey }}
{{- else }}
{{- "monitoring-password" }}
{{- end }}
{{- end }}

{{- define "pg.pgpoolAdminSecretName" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- required "pgpool.admin.existingSecret.name is required when pgpool.admin.existingSecret.enabled is true" .Values.pgpool.admin.existingSecret.name }}
{{- else }}
{{- include "pg.fullname" . }}-pgpool-admin
{{- end }}
{{- end }}

{{- define "pg.pgpoolAdminUsernameKey" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- .Values.pgpool.admin.existingSecret.usernameKey }}
{{- else }}
{{- "username" }}
{{- end }}
{{- end }}

{{- define "pg.pgpoolAdminPasswordKey" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- .Values.pgpool.admin.existingSecret.passwordKey }}
{{- else }}
{{- "password" }}
{{- end }}
{{- end }}

{{- define "pg.backupSecretName" -}}
{{- .Values.backup.existingSecret.name }}
{{- end }}

{{- define "pg.backupAccessKeyIdKey" -}}
{{- .Values.backup.existingSecret.accessKeyIdKey }}
{{- end }}

{{- define "pg.backupSecretAccessKeyKey" -}}
{{- .Values.backup.existingSecret.secretAccessKeyKey }}
{{- end }}

{{/*
Port implied by an S3 endpoint. Accepts host, host:port, or
scheme://host[:port]; an explicit port wins, otherwise the scheme
decides (http 80, anything else 443).
*/}}
{{- define "pg.s3EndpointPort" -}}
{{- $hostport := regexReplaceAll "^[a-zA-Z][a-zA-Z0-9+.-]*://" . "" -}}
{{- $hostport = splitList "/" $hostport | first -}}
{{- if regexMatch ":[0-9]+$" $hostport -}}
{{- splitList ":" $hostport | last -}}
{{- else if hasPrefix "http://" . -}}
80
{{- else -}}
443
{{- end -}}
{{- end }}

{{- define "pg.preStop" -}}
preStop:
  exec:
    command:
      - /bin/bash
      - -c
      - |
        # Stop cleanly and let repmgrd on a standby own the failover:
        # its promote_command (repmgr standby promote) updates
        # repmgr.nodes metadata, which a raw SQL-level promotion issued
        # from this hook cannot do -- the promoted node would keep
        # type='standby', and every repmgrd then exits on the stale
        # metadata instead of converging.
        pg_ctl stop -D "$PGDATA" -m fast -w -t 30
{{- end }}

{{- define "pg.exporterPodSpec" -}}
# The exporter scrapes PostgreSQL only, never the Kubernetes API, so don't mount an SA token (#166).
automountServiceAccountToken: false
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.prometheusExporter.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
securityContext:
  {{- toYaml .Values.prometheusExporter.podSecurityContext | nindent 2 }}
initContainers:
  - name: init-config
    image: "{{ .Values.busyboxImage.repository }}:{{ .Values.busyboxImage.tag }}"
    imagePullPolicy: {{ .Values.busyboxImage.pullPolicy }}
    securityContext:
      {{- toYaml .Values.prometheusExporter.containerSecurityContext | nindent 6 }}
    command:
      - /bin/sh
      - -c
      - |
        # Placeholders sit inside single-quoted YAML scalars: double any
        # embedded quote, then splice byte-for-byte (sed replacement
        # corrupts values containing / & or \).
        SUB_USER=$(printf '%s' "$POSTGRES_USER" | sed "s/'/''/g")
        SUB_PASS=$(printf '%s' "$POSTGRES_PASSWORD" | sed "s/'/''/g")
        export SUB_USER SUB_PASS
        awk '
          function splice(s, ph, val,   out, i) {
            out = ""
            while (i = index(s, ph)) { out = out substr(s, 1, i - 1) val; s = substr(s, i + length(ph)) }
            return out s
          }
          { $0 = splice($0, "__POSTGRES_USER__", ENVIRON["SUB_USER"])
            $0 = splice($0, "__POSTGRES_PASSWORD__", ENVIRON["SUB_PASS"])
            print }
        ' /config/postgres_exporter.yml > /etc/postgres_exporter/postgres_exporter.yml
        # URI userinfo cannot carry @ : / etc. raw and Kubernetes $(VAR)
        # expansion cannot encode, so the DSN is assembled here with every
        # credential byte percent-encoded (over-encoding is valid in URIs).
        enc() { printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n' | sed 's/../%&/g'; }
        ENC_USER=$(enc "$POSTGRES_USER")
        ENC_PASS=$(enc "$POSTGRES_PASSWORD")
        ENC_DB=$(enc "$POSTGRES_DATABASE")
        printf '%s' "{{ range $i := until (int (add .Values.postgresql.replicaCount 1)) }}{{ if $i }},{{ end }}postgresql://${ENC_USER}:${ENC_PASS}@{{ include "pg.fullname" $ }}-{{ $i }}.{{ include "pg.fullname" $ }}-headless:5432/${ENC_DB}?sslmode=disable{{ end }}" > /etc/postgres_exporter/dsn
    env:
{{- if .Values.prometheusExporter.monitoringUser.enabled }}
      # #28: scrape as the least-privilege pg_monitor role (created by the
      # monitoring-user hook Job), not the postgres superuser.
      - name: POSTGRES_USER
        value: {{ .Values.prometheusExporter.monitoringUser.username | quote }}
      - name: POSTGRES_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretMonitoringPasswordKey" . }}
{{- else }}
      - name: POSTGRES_USER
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretUsernameKey" . }}
      - name: POSTGRES_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretPasswordKey" . }}
{{- end }}
      - name: POSTGRES_DATABASE
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretDatabaseKey" . }}
    resources:
      {{- include "pg.initResources" . | nindent 6 }}
    volumeMounts:
      - name: config
        mountPath: /config
      - name: exporter-config
        mountPath: /etc/postgres_exporter
containers:
  - name: postgres-exporter
    image: "{{ .Values.prometheusExporter.image.repository }}:{{ .Values.prometheusExporter.image.tag }}"
    imagePullPolicy: {{ .Values.prometheusExporter.image.pullPolicy }}
    securityContext:
      {{- toYaml .Values.prometheusExporter.containerSecurityContext | nindent 6 }}
    command:
      - /bin/sh
      - -c
      - |
        DATA_SOURCE_NAME="$(cat /etc/postgres_exporter/dsn)" exec /bin/postgres_exporter \
          --config.file=/etc/postgres_exporter/postgres_exporter.yml \
          --extend.query-path=/config/queries.yaml \
          --web.listen-address=:9116 \
          --web.telemetry-path=/metrics \
          --log.level=info
    ports:
      - name: metrics
        containerPort: 9116
        protocol: TCP
    livenessProbe:
      httpGet:
        # /metrics (not the always-200 landing page /) so the probe fails on a broken
        # scrape pipeline -- a queries.yaml/collector regression returns 500 here while
        # / stays 200 (#146). A DB outage returns 200 + pg_up 0, so this does not flap.
        path: /metrics
        port: metrics
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      httpGet:
        # /metrics (not the always-200 landing page /) so the probe fails on a broken
        # scrape pipeline -- a queries.yaml/collector regression returns 500 here while
        # / stays 200 (#146). A DB outage returns 200 + pg_up 0, so this does not flap.
        path: /metrics
        port: metrics
      initialDelaySeconds: 5
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    resources:
      {{- toYaml .Values.prometheusExporter.resources | nindent 6 }}
    volumeMounts:
      - name: exporter-config
        mountPath: /etc/postgres_exporter
      # queries.yaml carries no credential placeholders, so it is read
      # straight from the configmap instead of the init-processed copy
      - name: config
        mountPath: /config
volumes:
  - name: config
    configMap:
      name: {{ include "pg.fullname" . }}-postgres-exporter
  - name: exporter-config
    emptyDir:
      sizeLimit: 16Mi
{{- end }}

{{/*
Failover mode (repmgrd default | agent). Fails fast on an unknown value.
*/}}
{{- define "pg.failoverMode" -}}
{{- $m := .Values.repmgr.failoverMode | default "repmgrd" -}}
{{- if not (or (eq $m "repmgrd") (eq $m "agent")) -}}
{{- fail (printf "repmgr.failoverMode must be 'repmgrd' or 'agent', got %q" $m) -}}
{{- end -}}
{{- $m -}}
{{- end -}}

{{/*
pg.agentMode / pg.repmgrdMode render the string "true"/"false". Call sites gate
with: {{- if eq (include "pg.agentMode" .) "true" }}
*/}}
{{- define "pg.agentMode" -}}
{{- and .Values.repmgr.enabled (eq (include "pg.failoverMode" .) "agent") -}}
{{- end -}}

{{- define "pg.repmgrdMode" -}}
{{- and .Values.repmgr.enabled (eq (include "pg.failoverMode" .) "repmgrd") -}}
{{- end -}}

{{- /* True in agent mode when the leadership backend is etcd (repmgr.agent.dcs.backend
       == "etcd"), false otherwise. Nil-safe at every level so a partial overlay does
       not nil-pointer; defaults to the kubernetes backend. */ -}}
{{- define "pg.agentEtcdMode" -}}
{{- if eq (include "pg.agentMode" .) "true" -}}
{{- $agent := .Values.repmgr.agent | default dict -}}
{{- $dcs := $agent.dcs | default dict -}}
{{- eq ($dcs.backend | default "kubernetes") "etcd" -}}
{{- else -}}
false
{{- end -}}
{{- end -}}
