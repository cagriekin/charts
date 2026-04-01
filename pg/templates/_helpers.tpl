{{- define "pg.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "pg.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{- define "pg.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "pg.labels" -}}
{{ include "common.labels" . }}
{{- with .Values.global.annotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "pg.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{- define "pg.secretName" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.name }}
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

{{- define "pg.backupSecretName" -}}
{{- .Values.backup.existingSecret.name }}
{{- end }}

{{- define "pg.backupAccessKeyIdKey" -}}
{{- .Values.backup.existingSecret.accessKeyIdKey }}
{{- end }}

{{- define "pg.backupSecretAccessKeyKey" -}}
{{- .Values.backup.existingSecret.secretAccessKeyKey }}
{{- end }}

{{- define "pg.preStop" -}}
preStop:
  exec:
    command:
      - /bin/bash
      - -c
      - |
        ROLE=$(psql -U "$REPMGR_USER" -d "$REPMGR_DB" -t -A -c \
          "SELECT type FROM repmgr.nodes WHERE active = true AND node_name = '$(hostname)'" 2>/dev/null)
        if [ "$ROLE" = "primary" ]; then
          STANDBY_HOST=$(psql -U "$REPMGR_USER" -d "$REPMGR_DB" -t -A -c \
            "SELECT conninfo FROM repmgr.nodes WHERE type = 'standby' AND active = true ORDER BY priority LIMIT 1" 2>/dev/null \
            | sed -n 's/.*host=\([^ ]*\).*/\1/p')
          if [ -n "$STANDBY_HOST" ]; then
            psql -U "$REPMGR_USER" -d "$REPMGR_DB" -h "$STANDBY_HOST" \
              -c "SELECT pg_promote()" 2>/dev/null
            for i in $(seq 1 30); do
              IS_STANDBY=$(psql -U "$REPMGR_USER" -d "$REPMGR_DB" -t -A -c \
                "SELECT pg_is_in_recovery()" 2>/dev/null)
              if [ "$IS_STANDBY" = "t" ]; then
                break
              fi
              sleep 1
            done
          fi
        fi
        pg_ctl stop -D "$PGDATA" -m fast -w -t 30
{{- end }}

{{- define "pg.exporterPodSpec" -}}
securityContext:
  {{- toYaml .Values.prometheusExporter.podSecurityContext | nindent 2 }}
initContainers:
  - name: init-config
    image: busybox:1.35
    securityContext:
      {{- toYaml .Values.prometheusExporter.containerSecurityContext | nindent 6 }}
    command:
      - /bin/sh
      - -c
      - |
        cp /config/postgres_exporter.yml /etc/postgres_exporter/postgres_exporter.yml
        sed -i "s/__POSTGRES_USER__/$POSTGRES_USER/g" /etc/postgres_exporter/postgres_exporter.yml
        sed -i "s/__POSTGRES_PASSWORD__/$POSTGRES_PASSWORD/g" /etc/postgres_exporter/postgres_exporter.yml
    env:
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
    args:
      - --config.file=/etc/postgres_exporter/postgres_exporter.yml
      - --web.listen-address=:9116
      - --web.telemetry-path=/metrics
      - --log.level=info
    env:
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
      - name: POSTGRES_DATABASE
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretDatabaseKey" . }}
      - name: DATA_SOURCE_NAME
        value: "{{ range $i := until (int (add .Values.postgresql.replicaCount 1)) }}{{ if $i }},{{ end }}postgresql://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@{{ include "pg.fullname" $ }}-{{ $i }}.{{ include "pg.fullname" $ }}-headless:5432/$(POSTGRES_DATABASE)?sslmode=disable{{ end }}"
    ports:
      - name: metrics
        containerPort: 9116
        protocol: TCP
    livenessProbe:
      httpGet:
        path: /
        port: metrics
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      httpGet:
        path: /
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
volumes:
  - name: config
    configMap:
      name: {{ include "pg.fullname" . }}-postgres-exporter
  - name: exporter-config
    emptyDir: {}
{{- end }}
