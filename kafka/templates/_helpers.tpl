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
{{- $override := default "" .Values.fullnameOverride | trim -}}
{{- if eq $override "" -}}
{{- fail "fullnameOverride value is required for the kafka chart" -}}
{{- end -}}
{{- $override | trunc 63 | trimSuffix "-" -}}
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
{{- if .Values.kafka.auth.existingSecret }}
{{- $secret := lookup "v1" "Secret" (default "default" .Release.Namespace) .Values.kafka.auth.existingSecret }}
{{- if not $secret }}
{{- fail (printf "Kafka auth existingSecret %q not found in namespace %q" .Values.kafka.auth.existingSecret (default "default" .Release.Namespace)) }}
{{- end }}
{{- $key := default "username" (dig "existingSecretKeys" "username" .Values.kafka.auth) }}
{{- $data := index $secret.data $key }}
{{- if not $data }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- $data | b64dec -}}
{{- else }}
{{- .Values.kafka.auth.username | default "user1" -}}
{{- end }}
{{- end }}

{{/*
Return the Kafka auth password in plain text.
*/}}
{{- define "kafka.auth.password" -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- $secret := lookup "v1" "Secret" (default "default" .Release.Namespace) .Values.kafka.auth.existingSecret }}
{{- if not $secret }}
{{- fail (printf "Kafka auth existingSecret %q not found in namespace %q" .Values.kafka.auth.existingSecret (default "default" .Release.Namespace)) }}
{{- end }}
{{- $key := default "password" (dig "existingSecretKeys" "password" .Values.kafka.auth) }}
{{- $data := index $secret.data $key }}
{{- if not $data }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- $data | b64dec -}}
{{- else }}
{{- $password := default (include "kafka.kafka.password" .) .Values.kafka.auth.password -}}
{{- $password | b64dec -}}
{{- end }}
{{- end }}

{{/*
Return the secret key used for username retrieval.
*/}}
{{- define "kafka.auth.usernameKey" -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- default "username" (dig "existingSecretKeys" "username" .Values.kafka.auth) -}}
{{- else }}
username
{{- end }}
{{- end }}

{{/*
Return the secret key used for password retrieval.
*/}}
{{- define "kafka.auth.passwordKey" -}}
{{- if .Values.kafka.auth.existingSecret }}
{{- default "password" (dig "existingSecretKeys" "password" .Values.kafka.auth) -}}
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

