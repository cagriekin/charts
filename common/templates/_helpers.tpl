{{/*
Expand the name of the chart.
Usage: include "common.name" (dict "Chart" .Chart "Values" .Values)
*/}}
{{- define "common.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Usage: include "common.fullname" (dict "Chart" .Chart "Values" .Values "Release" .Release)
*/}}
{{- define "common.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
Usage: include "common.chart" .
*/}}
{{- define "common.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels.
Usage: include "common.selectorLabels" .
*/}}
{{- define "common.selectorLabels" -}}
app.kubernetes.io/name: {{ include "common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Common labels.
Usage: include "common.labels" .
*/}}
{{- define "common.labels" -}}
helm.sh/chart: {{ include "common.chart" . }}
{{ include "common.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Exporter Service resource.
Usage:
  include "common.exporterService" (dict
    "ctx"             .
    "component"       "redis-exporter"
    "labels"          "redis.labels"
    "selectorLabels"  "redis.selectorLabels"
    "port"            .Values.exporter.service.port
    "serviceType"     .Values.exporter.service.type
    "annotations"     .Values.exporter.service.annotations
  )
*/}}
{{- define "common.exporterService" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "common.fullname" .ctx }}-{{ .nameSuffix | default .component }}
  labels:
    {{- include .labels .ctx | nindent 4 }}
    app.kubernetes.io/component: {{ .component }}
  {{- with .annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ .serviceType | default "ClusterIP" }}
  ports:
    - name: metrics
      port: {{ .port }}
      targetPort: metrics
      protocol: TCP
  selector:
    {{- include .selectorLabels .ctx | nindent 4 }}
    app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Exporter Deployment resource.
Usage:
  include "common.exporterDeployment" (dict
    "ctx"             .
    "component"       "redis-exporter"
    "labels"          "redis.labels"
    "selectorLabels"  "redis.selectorLabels"
    "replicas"        1
    "podAnnotations"  (dict)
    "podSpec"         "redis.exporterPodSpec"
  )
Optional: "nameSuffix" overrides the resource name suffix (defaults to component).
*/}}
{{- define "common.exporterDeployment" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "common.fullname" .ctx }}-{{ .nameSuffix | default .component }}
  labels:
    {{- include .labels .ctx | nindent 4 }}
    app.kubernetes.io/component: {{ .component }}
spec:
  replicas: {{ .replicas | default 1 }}
  selector:
    matchLabels:
      {{- include .selectorLabels .ctx | nindent 6 }}
      app.kubernetes.io/component: {{ .component }}
  template:
    metadata:
      labels:
        {{- include .selectorLabels .ctx | nindent 8 }}
        app.kubernetes.io/component: {{ .component }}
      {{- with .podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      {{- include .podSpec .ctx | nindent 6 }}
{{- end }}
