{{/*
Expand the name of the chart.
*/}}
{{- define "kafka.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kafka.fullname" -}}
{{- $override := default "" (.Values.fullnameOverride | trim) -}}
{{- if ne $override "" -}}
{{- $override | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "kafka.name" . -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kafka.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kafka.labels" -}}
helm.sh/chart: {{ include "kafka.chart" . }}
{{ include "kafka.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: {{ include "kafka.name" . }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kafka.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kafka.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kafka.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kafka.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Return the Kafka auth secret name (existing or managed by chart).
*/}}
{{- define "kafka.auth.secretName" -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- .Values.kafka.auth.existingSecret }}
{{- else }}
{{- printf "%s-kafka-secret" (include "kafka.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Return the Kafka auth username.
*/}}
{{- define "kafka.auth.username" -}}
{{- $fallback := .Values.kafka.auth.username | default "user1" -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- $secret := lookup "v1" "Secret" (default "default" .Release.Namespace) .Values.kafka.auth.existingSecret }}
{{- $key := include "kafka.auth.usernameKey" . | trim }}
{{- if $secret }}
{{- if and $secret.data (hasKey $secret.data $key) }}
{{- index $secret.data $key | b64dec -}}
{{- else }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- else }}
{{- fail (printf "Kafka auth existingSecret %q not found in namespace %q" .Values.kafka.auth.existingSecret (default "default" .Release.Namespace)) }}
{{- end }}
{{- else }}
{{- $fallback -}}
{{- end }}
{{- end }}

{{/*
Return the Kafka auth password in plain text.
*/}}
{{- define "kafka.auth.password" -}}
{{- $fallback := default (include "kafka.kafka.password" .) .Values.kafka.auth.password -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- $secret := lookup "v1" "Secret" (default "default" .Release.Namespace) .Values.kafka.auth.existingSecret }}
{{- $key := include "kafka.auth.passwordKey" . | trim }}
{{- if $secret }}
{{- if and $secret.data (hasKey $secret.data $key) }}
{{- index $secret.data $key | b64dec -}}
{{- else }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- else }}
{{- fail (printf "Kafka auth existingSecret %q not found in namespace %q" .Values.kafka.auth.existingSecret (default "default" .Release.Namespace)) }}
{{- end }}
{{- else }}
{{- $fallback | b64dec -}}
{{- end }}
{{- end }}

{{/*
Return the secret key used for username retrieval.
*/}}
{{- define "kafka.auth.usernameKey" -}}
{{- $keys := .Values.kafka.auth.existingSecretKeys -}}
{{- if and .Values.kafka.auth.existingSecret $keys (hasKey $keys "username") }}
{{- index $keys "username" -}}
{{- else }}
username
{{- end }}
{{- end }}

{{/*
Return the secret key used for password retrieval.
*/}}
{{- define "kafka.auth.passwordKey" -}}
{{- $keys := .Values.kafka.auth.existingSecretKeys -}}
{{- if and .Values.kafka.auth.existingSecret $keys (hasKey $keys "password") }}
{{- index $keys "password" -}}
{{- else }}
password
{{- end }}
{{- end }}

{{/*
Generate Kafka controller quorum voters.
*/}}
{{- define "kafka.kafka.controllerQuorumVoters" -}}
{{- $fullname := include "kafka.fullname" . -}}
{{- printf "1@%s-kafka-controller.%s.svc.cluster.local:9093" $fullname (default "default" .Release.Namespace) -}}
{{- end }}

{{/*
Generate Kafka cluster ID.
*/}}
{{- define "kafka.kafka.clusterId" -}}
{{- printf "%s-%s" .Release.Name "kafka-cluster" | sha256sum | trunc 22 -}}
{{- end }}

{{/*
Generate deterministic Kafka password when not provided.
*/}}
{{- define "kafka.kafka.password" -}}
{{- $salt := "kafka-password-salt" -}}
{{- $input := printf "%s-%s-%s" .Release.Name .Chart.Name $salt -}}
{{- $hash := $input | sha256sum -}}
{{- $hash | b64enc -}}
{{- end }}

{{/*
Generate a content-based suffix for the Kafka topic init job so updates trigger a new job name.
*/}}
{{- define "kafka.topicInit.hash" -}}
{{- $payload := dict "chartVersion" .Chart.Version "image" (printf "%s/%s:%s" .Values.kafka.image.registry .Values.kafka.image.repository .Values.kafka.image.tag) "topics" .Values.kafka.topics -}}
{{- toYaml $payload | sha256sum | trunc 10 | trimSuffix "-" -}}
{{- end }}

