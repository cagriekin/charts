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
{{- with .Values.global.annotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "redis.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}
