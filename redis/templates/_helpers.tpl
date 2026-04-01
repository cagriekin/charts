{{- define "redis.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "redis.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{- define "redis.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "redis.labels" -}}
{{ include "common.labels" . }}
{{- end }}

{{- define "redis.globalAnnotations" -}}
{{- with .Values.global.annotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "redis.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{- define "redis.exporterPodSpec" -}}
securityContext:
  {{- toYaml .Values.exporter.podSecurityContext | nindent 2 }}
containers:
  - name: redis-exporter
    image: "{{ .Values.exporter.image.repository }}:{{ .Values.exporter.image.tag }}"
    imagePullPolicy: {{ .Values.exporter.image.pullPolicy }}
    securityContext:
      {{- toYaml .Values.exporter.containerSecurityContext | nindent 6 }}
    env:
      - name: REDIS_ADDR
        value: "redis://{{ include "redis.fullname" . }}:{{ .Values.service.port }}"
{{- if .Values.redis.auth.enabled }}
      - name: REDIS_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ .Values.redis.auth.existingSecret.name }}
            key: {{ .Values.redis.auth.existingSecret.key }}
{{- end }}
    ports:
      - name: metrics
        containerPort: 9121
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
      {{- toYaml .Values.exporter.resources | nindent 6 }}
{{- end }}
