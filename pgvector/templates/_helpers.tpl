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
{{- $salt := "All that we have is that shout into the wind. How we live. How we go. And how we stand before we fall. Karnus Au Bellona." -}}
{{- $input := printf "%s-%s-%s-%s" .Release.Name .Chart.Name .Release.Namespace $salt -}}
{{- $hash := $input | sha256sum -}}
{{- $hash | trunc 32 -}}
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
Name of the backup secret
*/}}
{{- define "pgvector.backup.secretName" -}}
{{- printf "%s-backup" (include "pgvector.fullname" .) -}}
{{- end -}}

{{/*
Get PostgreSQL username from values or default
*/}}
{{- define "pgvector.postgresql.username" -}}
{{- .Values.postgresql.postgresUser | default "postgres" -}}
{{- end -}}

{{/*
Get PostgreSQL database from values or default
*/}}
{{- define "pgvector.postgresql.database" -}}
{{- .Values.postgresql.postgresDatabase | default "postgres" -}}
{{- end -}}

{{/*
Replication username (hardcoded)
*/}}
{{- define "pgvector.postgresql.replicationUser" -}}
replication
{{- end -}}

{{/*
Generate deterministic replication password
*/}}
{{- define "pgvector.postgresql.replicationPassword" -}}
{{- $salt := "Replication password for streaming replication. The path of the righteous man is beset on all sides." -}}
{{- $input := printf "%s-%s-%s-replication-password" .Release.Name .Chart.Name $salt -}}
{{- $hash := $input | sha256sum -}}
{{- $hash | trunc 32 -}}
{{- end -}}

{{/*
Get resolved PostgreSQL password from values or generated
*/}}
{{- define "pgvector.postgresql.resolvedPassword" -}}
{{- if .Values.postgresql.postgresPassword }}
{{- .Values.postgresql.postgresPassword -}}
{{- else -}}
{{- include "pgvector.postgresql.password" . -}}
{{- end -}}
{{- end -}}

{{/*
Alias for PostgreSQL secret name (for backward compatibility)
*/}}
{{- define "pgvector.postgresql.urlSecretName" -}}
{{- include "pgvector.postgresql.secretName" . -}}
{{- end -}}

{{/*
Generate PostgreSQL connection string
*/}}
{{- define "pgvector.postgresql.connectionString" -}}
{{- $user := include "pgvector.postgresql.username" . -}}
{{- $password := include "pgvector.postgresql.resolvedPassword" . -}}
{{- $database := include "pgvector.postgresql.database" . -}}
{{- $host := printf "%s-postgresql-master.%s.svc.cluster.local" (include "pgvector.fullname" .) .Release.Namespace -}}
{{- printf "postgresql://%s:%s@%s:5432/%s?sslmode=disable" $user $password $host $database -}}
{{- end -}}

