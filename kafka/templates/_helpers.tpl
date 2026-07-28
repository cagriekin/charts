{{- define "kafka.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "kafka.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{- define "kafka.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "kafka.labels" -}}
{{ include "common.labels" . }}
app.kubernetes.io/part-of: {{ include "kafka.name" . }}
{{- end }}

{{- define "kafka.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{/*
Kafka metrics exporter pod spec.
In TLS mode the exporter authenticates to the broker INTERNAL (mTLS) listener
using the chart client certificate, whose principal is a super-user -- no SASL
credentials are needed. In insecure mode it connects to the CLIENT listener in
plaintext with no auth.
*/}}
{{- define "kafka.exporterPodSpec" -}}
{{- $fullname := include "kafka.fullname" . }}
{{- $namespace := default "default" .Release.Namespace }}
{{- $replicas := int .Values.kafka.broker.replicaCount }}
{{- $tls := .Values.kafka.tls.enabled }}
serviceAccountName: {{ include "kafka.serviceAccountName" . }}
{{- with .Values.exporters.kafka.priorityClassName }}
priorityClassName: {{ . | quote }}
{{- end }}
{{- with .Values.exporters.kafka.podSecurityContext }}
securityContext:
  {{- toYaml . | nindent 2 }}
{{- end }}
containers:
  - name: kafka-exporter
    image: "{{ .Values.exporters.kafka.image.registry }}/{{ .Values.exporters.kafka.image.repository }}:{{ .Values.exporters.kafka.image.tag }}"
    imagePullPolicy: {{ .Values.exporters.kafka.image.pullPolicy }}
    command:
      - /bin/sh
      - -c
      - |
        exec kafka_exporter \
          {{- range $i := until $replicas }}
          {{- if $tls }}
          --kafka.server={{ $fullname }}-kafka-broker-{{ $i }}.{{ $fullname }}-kafka-broker.{{ $namespace }}.svc.cluster.local:9094 \
          {{- else }}
          --kafka.server={{ $fullname }}-kafka-broker-{{ $i }}.{{ $fullname }}-kafka-broker.{{ $namespace }}.svc.cluster.local:9092 \
          {{- end }}
          {{- end }}
          {{- if $tls }}
          --tls.enabled \
          --tls.ca-file=/opt/kafka/tls-pem/{{ .Values.kafka.tls.caFilename }} \
          --tls.cert-file=/opt/kafka/tls-pem/{{ .Values.kafka.tls.certFilename }} \
          --tls.key-file=/opt/kafka/tls-pem/{{ .Values.kafka.tls.keyFilename }}
          {{- else }}
          --kafka.version=2.0.0
          {{- end }}
    ports:
      - name: metrics
        containerPort: 9308
        protocol: TCP
    resources:
      {{- toYaml .Values.exporters.kafka.resources | nindent 6 }}
    livenessProbe:
      httpGet:
        path: /metrics
        port: 9308
      initialDelaySeconds: 30
      periodSeconds: 10
    readinessProbe:
      httpGet:
        path: /metrics
        port: 9308
      initialDelaySeconds: 10
      periodSeconds: 5
    {{- with .Values.exporters.kafka.containerSecurityContext }}
    securityContext:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- if $tls }}
    volumeMounts:
      - name: kafka-tls-pem
        mountPath: /opt/kafka/tls-pem
        readOnly: true
    {{- end }}
{{- if $tls }}
volumes:
  - name: kafka-tls-pem
    secret:
      secretName: {{ include "kafka.tls.secretName" . }}
{{- end }}
{{- with .Values.exporters.kafka.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.exporters.kafka.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.exporters.kafka.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
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
Name of the chart-managed Secret holding per-user SASL passwords
(one key per user: "<username>-password").
*/}}
{{- define "kafka.auth.secretName" -}}
{{- printf "%s-kafka-secret" (include "kafka.fullname" .) -}}
{{- end }}

{{/*
Effective SASL mechanism for external clients.
*/}}
{{- define "kafka.auth.mechanism" -}}
{{- .Values.kafka.auth.saslMechanism | default "SCRAM-SHA-512" -}}
{{- end }}

{{/*
Lowercased mechanism, used in per-listener config keys (e.g. scram-sha-512).
*/}}
{{- define "kafka.auth.mechanismLower" -}}
{{- include "kafka.auth.mechanism" . | lower -}}
{{- end }}

{{/*
Resolve a chart-managed user's password at render time (used only by the
Secret template). Precedence: explicit password -> value persisted in the
existing chart Secret (so it is stable across upgrades) -> a fresh random
value on first install.
Usage: include "kafka.auth.userPassword" (dict "ctx" $ "user" $user)
*/}}
{{- define "kafka.auth.userPassword" -}}
{{- $ctx := .ctx -}}
{{- $user := .user -}}
{{- if $user.password -}}
{{- $user.password -}}
{{- else -}}
{{- $ns := default "default" $ctx.Release.Namespace -}}
{{- $key := printf "%s-password" $user.username -}}
{{- $existing := lookup "v1" "Secret" $ns (include "kafka.auth.secretName" $ctx) -}}
{{- if and $existing (hasKey ($existing.data | default dict) $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Principal derived from the chart's TLS client certificate (CN == fullname),
after ssl.principal.mapping.rules maps the DN down to the CN.
*/}}
{{- define "kafka.auth.certPrincipal" -}}
{{- printf "User:%s" (include "kafka.fullname" .) -}}
{{- end }}

{{/*
super.users list (semicolon-separated). Always includes the internal identity
(the cert principal under TLS/mTLS, or ANONYMOUS in insecure/no-TLS mode so
inter-broker traffic is authorized), plus any configured superUsers.
*/}}
{{- define "kafka.auth.superUsers" -}}
{{- $ctx := . -}}
{{- $list := list -}}
{{- if $ctx.Values.kafka.tls.enabled -}}
{{- $list = append $list (include "kafka.auth.certPrincipal" $ctx) -}}
{{- else -}}
{{- $list = append $list "User:ANONYMOUS" -}}
{{- end -}}
{{- range $ctx.Values.kafka.auth.authorization.superUsers -}}
{{- $list = append $list (printf "User:%s" .) -}}
{{- end -}}
{{- $list | uniq | join ";" -}}
{{- end }}

{{/*
Transport protocol for the external CLIENT listener.
*/}}
{{- define "kafka.clientProtocol" -}}
{{- if .Values.kafka.tls.enabled -}}SASL_SSL{{- else -}}SASL_PLAINTEXT{{- end -}}
{{- end }}

{{/*
Transport protocol for the internal (inter-broker) and controller listeners.
SSL enables mTLS via ssl.client.auth=required; PLAINTEXT is insecure.
*/}}
{{- define "kafka.internalProtocol" -}}
{{- if .Values.kafka.tls.enabled -}}SSL{{- else -}}PLAINTEXT{{- end -}}
{{- end }}

{{/*
Fail the render when TLS is disabled without an explicit insecure opt-in.
*/}}
{{- define "kafka.auth.validateTls" -}}
{{- if and (not .Values.kafka.tls.enabled) (not .Values.kafka.auth.allowInsecure) -}}
{{- fail "kafka.tls.enabled is false but kafka.auth.allowInsecure is not set. Production requires TLS: set kafka.tls.enabled=true (with kafka.tls.certManager.issuerRef.name or kafka.tls.existingSecret), or explicitly set kafka.auth.allowInsecure=true to run WITHOUT TLS (INSECURE: client credentials travel in cleartext and inter-broker/controller traffic is unauthenticated)." -}}
{{- end -}}
{{- end }}

{{/*
Generate Kafka controller quorum voters string for multi-controller KRaft.
Format: 1@controller-0.svc:9093,2@controller-1.svc:9093,3@controller-2.svc:9093
*/}}
{{- define "kafka.kafka.controllerQuorumVoters" -}}
{{- $fullname := include "kafka.fullname" . -}}
{{- $namespace := default "default" .Release.Namespace -}}
{{- $replicas := int .Values.kafka.controller.replicaCount -}}
{{- range $i := until $replicas -}}
{{- if $i }},{{ end -}}
{{ add $i 1 }}@{{ $fullname }}-kafka-controller-{{ $i }}.{{ $fullname }}-kafka-controller.{{ $namespace }}.svc.cluster.local:9093
{{- end -}}
{{- end }}

{{/*
NetworkPolicy peer that selects in-cluster Kafka pods of the given component(s),
in the release namespace. Used to pod-scope intra-cluster traffic instead of
allowing the whole namespace.
Usage: include "kafka.netpol.podPeer" (dict "ctx" . "components" (list "kafka-broker" "kafka-controller"))
*/}}
{{- define "kafka.netpol.podPeer" -}}
- podSelector:
    matchLabels:
      app.kubernetes.io/name: {{ include "kafka.name" .ctx }}
      app.kubernetes.io/instance: {{ .ctx.Release.Name }}
    matchExpressions:
      - key: app.kubernetes.io/component
        operator: In
        values:
        {{- range .components }}
          - {{ . }}
        {{- end }}
{{- end }}

{{/*
NetworkPolicy egress rule allowing DNS resolution (to the release namespace and
kube-system, where CoreDNS typically runs).
Usage: include "kafka.netpol.dnsEgress" (dict "ctx" .)
*/}}
{{- define "kafka.netpol.dnsEgress" -}}
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .ctx.Release.Namespace }}
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
  ports:
    - protocol: UDP
      port: 53
    - protocol: TCP
      port: 53
{{- end }}

{{/*
Generate Kafka cluster ID.
*/}}
{{- define "kafka.kafka.clusterId" -}}
{{- printf "%s-%s" .Release.Name "kafka-cluster" | sha256sum | trunc 22 -}}
{{- end }}

{{/*
Generate a content-based suffix for the Kafka bootstrap job so updates trigger a new job name.
*/}}
{{- define "kafka.topicInit.hash" -}}
{{- $payload := dict "chartVersion" .Chart.Version "image" (printf "%s/%s:%s" .Values.kafka.image.registry .Values.kafka.image.repository .Values.kafka.image.tag) "topics" .Values.kafka.topics "acls" .Values.kafka.auth.authorization.acls "users" (.Values.kafka.auth.users | default list) -}}
{{- toYaml $payload | sha256sum | trunc 10 | trimSuffix "-" -}}
{{- end }}

{{/*
Secret key under which the random TLS PKCS12 store password is persisted (in the
chart-managed Secret named by kafka.auth.secretName).
*/}}
{{- define "kafka.tls.storePasswordKey" -}}
tls-store-password
{{- end }}

{{/*
Resolve the TLS PKCS12 keystore/truststore password at render time (used only by
the Secret template). Random per install, persisted across upgrades via lookup of
the existing chart Secret. randAlphaNum keeps it free of sed/JAAS-special chars.
The value is never written into a ConfigMap: workloads read it from the Secret via
env (secretKeyRef) and substitute the PLACEHOLDER_STORE_PASSWORD marker at startup.
*/}}
{{- define "kafka.tls.storePasswordValue" -}}
{{- $ns := default "default" .Release.Namespace -}}
{{- $key := include "kafka.tls.storePasswordKey" . | trim -}}
{{- $existing := lookup "v1" "Secret" $ns (include "kafka.auth.secretName" .) -}}
{{- if and $existing (hasKey ($existing.data | default dict) $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end }}

{{/*
Env var injecting the TLS store password into a container from the chart Secret.
Usage: {{- include "kafka.tls.storePasswordEnv" . | nindent 12 }}
*/}}
{{- define "kafka.tls.storePasswordEnv" -}}
- name: KAFKA_TLS_STORE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "kafka.auth.secretName" . }}
      key: {{ include "kafka.tls.storePasswordKey" . | trim }}
{{- end }}

{{/*
Return the TLS secret name. Uses existingSecret if set, otherwise the
cert-manager-managed secret name.
When TLS is enabled, exactly one of existingSecret or certManager.issuerRef.name
must be provided.
*/}}
{{- define "kafka.tls.secretName" -}}
{{- if .Values.kafka.tls.existingSecret -}}
{{- .Values.kafka.tls.existingSecret -}}
{{- else -}}
{{- printf "%s-kafka-tls" (include "kafka.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Whether the chart provisions its own self-signed CA (no external issuer and no
externally-supplied TLS secret). Returns a non-empty string when true.
*/}}
{{- define "kafka.tls.selfSigned" -}}
{{- if and .Values.kafka.tls.enabled (not .Values.kafka.tls.existingSecret) (not .Values.kafka.tls.certManager.issuerRef.name) -}}
true
{{- end -}}
{{- end }}

{{/*
issuerRef block for the leaf (server/client) Certificate. Uses the operator's
issuer when kafka.tls.certManager.issuerRef.name is set, otherwise the
chart-managed self-signed CA Issuer.
*/}}
{{- define "kafka.tls.issuerRef" -}}
{{- if .Values.kafka.tls.certManager.issuerRef.name -}}
name: {{ .Values.kafka.tls.certManager.issuerRef.name }}
kind: {{ .Values.kafka.tls.certManager.issuerRef.kind }}
group: {{ .Values.kafka.tls.certManager.issuerRef.group }}
{{- else -}}
name: {{ include "kafka.fullname" . }}-kafka-ca
kind: Issuer
group: cert-manager.io
{{- end -}}
{{- end }}

{{/*
Retained as a no-op: TLS configuration no longer requires an external issuer or
existingSecret (the chart supplies a self-signed CA by default). The
tls-disabled guard lives in kafka.auth.validateTls.
*/}}
{{- define "kafka.tls.validate" -}}
{{- end }}
