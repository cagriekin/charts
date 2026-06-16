{{- define "etcd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- /* Release-scoped fullname, e.g. <release>-etcd. As a subchart .Release.Name is
       the parent release, so the bundled etcd is named <parent-release>-etcd and the
       parent points the agent at <parent-release>-etcd:2379. */ -}}
{{- define "etcd.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "etcd.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "etcd.labels" -}}
app.kubernetes.io/name: {{ include "etcd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: etcd
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "etcd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "etcd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: etcd
{{- end -}}

{{- /* The static initial-cluster string: every member by its stable pod FQDN. etcd
       needs all peers listed for static bootstrap, and each member's --name must
       match its entry here (ETCD_NAME = the pod name). */ -}}
{{- define "etcd.initialCluster" -}}
{{- $full := include "etcd.fullname" . -}}
{{- $svc := printf "%s-headless" $full -}}
{{- $ns := .Release.Namespace -}}
{{- $domain := .Values.clusterDomain -}}
{{- $peer := .Values.peerPort | int -}}
{{- $parts := list -}}
{{- range $i := until (int .Values.replicaCount) -}}
{{- $parts = append $parts (printf "%s-%d=http://%s-%d.%s.%s.svc.%s:%d" $full $i $full $i $svc $ns $domain $peer) -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}
