{{/*
Expand the name of the chart.
*/}}
{{- define "pgvector.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "pgvector.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "pgvector.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "pgvector.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "pgvector.labels" -}}
helm.sh/chart: {{ include "pgvector.chart" . }}
{{ include "pgvector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "pgvector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pgvector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Create the name of the service account
*/}}
{{- define "pgvector.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pgvector.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end -}}

{{/*
Generate deterministic PostgreSQL password when not provided
*/}}
{{- define "pgvector.postgresql.password" -}}
{{- $salt := "pgvector-postgresql-password-salt" -}}
{{- $input := printf "%s-%s-%s" .Release.Name .Chart.Name $salt -}}
{{- $hash := $input | sha256sum -}}
{{- $hash | b64enc -}}
{{- end -}}

{{/*
Name of the PostgreSQL secret
*/}}
{{- define "pgvector.postgresql.secretName" -}}
{{- if .Values.postgresql.existingSecret }}
{{- .Values.postgresql.existingSecret -}}
{{- else -}}
{{- printf "%s-postgresql-secret" (include "pgvector.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the secret providing postgres-url
*/}}
{{- define "pgvector.postgresql.urlSecretName" -}}
{{- if .Values.postgresql.existingSecret }}
{{- printf "%s-postgresql-url" (include "pgvector.fullname" .) -}}
{{- else -}}
{{- include "pgvector.postgresql.secretName" . -}}
{{- end -}}
{{- end -}}

{{/*
Computed PostgreSQL connection string
*/}}
{{- define "pgvector.postgresql.connectionString" -}}
{{- $user := .Values.postgresql.postgresUser -}}
{{- $password := .Values.postgresql.postgresPassword -}}
{{- $database := .Values.postgresql.postgresDatabase -}}
{{- if .Values.postgresql.existingSecret }}
  {{- $secret := (lookup "v1" "Secret" .Release.Namespace .Values.postgresql.existingSecret) -}}
  {{- if $secret }}
    {{- if and (not $user) (hasKey $secret.data "postgres-user") }}
      {{- $user = (index $secret.data "postgres-user" | b64dec) -}}
    {{- end }}
    {{- if and (not $password) (hasKey $secret.data "postgres-password") }}
      {{- $password = (index $secret.data "postgres-password" | b64dec) -}}
    {{- end }}
    {{- if and (not $database) (hasKey $secret.data "postgres-database") }}
      {{- $database = (index $secret.data "postgres-database" | b64dec) -}}
    {{- end }}
  {{- end }}
{{- end }}
{{- $user = required "postgresql.postgresUser must be provided (either via values or existing secret key postgres-user)" $user -}}
{{- $password = default (include "pgvector.postgresql.password" .) $password -}}
{{- $database = required "postgresql.postgresDatabase must be provided (either via values or existing secret key postgres-database)" $database -}}
{{- $host := printf "%s-postgresql-master.%s.svc.cluster.local" (include "pgvector.fullname" .) .Release.Namespace -}}
{{- printf "postgresql://%s:%s@%s:5432/%s?sslmode=disable" $user $password $host $database -}}
{{- end -}}

{{/*
Name of the backup secret
*/}}
{{- define "pgvector.backup.secretName" -}}
{{- printf "%s-backup" (include "pgvector.fullname" .) -}}
{{- end -}}

