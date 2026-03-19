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

{{- define "kafka.exporterPodSpec" -}}
{{- $fullname := include "kafka.fullname" . }}
{{- $namespace := default "default" .Release.Namespace }}
{{- $replicas := int .Values.kafka.broker.replicaCount }}
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
        kafka_exporter \
          {{- range $i := until $replicas }}
          --kafka.server={{ $fullname }}-kafka-broker-{{ $i }}.{{ $fullname }}-kafka-broker.{{ $namespace }}.svc.cluster.local:9092 \
          {{- end }}
          --sasl.enabled \
          --sasl.mechanism=plain \
          --sasl.username="$KAFKA_SASL_USERNAME" \
          {{- if $.Values.kafka.tls.enabled }}
          --sasl.password="$KAFKA_SASL_PASSWORD" \
          --tls.enabled \
          --tls.ca-file=/opt/kafka/tls-pem/{{ $.Values.kafka.tls.caFilename }}
          {{- else }}
          --sasl.password="$KAFKA_SASL_PASSWORD"
          {{- end }}
    ports:
      - name: metrics
        containerPort: 9308
        protocol: TCP
    {{- if .Values.kafka.auth.existingSecret }}
    env:
      - name: KAFKA_SASL_USERNAME
        valueFrom:
          secretKeyRef:
            name: {{ include "kafka.auth.secretName" . }}
            key: {{ include "kafka.auth.usernameKey" . | trim }}
      - name: KAFKA_SASL_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "kafka.auth.secretName" . }}
            key: {{ include "kafka.auth.passwordKey" . | trim }}
    {{- else }}
    env:
      - name: KAFKA_SASL_USERNAME
        value: {{ include "kafka.auth.username" . | quote }}
      - name: KAFKA_SASL_PASSWORD
        value: {{ include "kafka.auth.password" . | quote }}
    {{- end }}
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
    {{- if $.Values.kafka.tls.enabled }}
    volumeMounts:
      - name: kafka-tls-pem
        mountPath: /opt/kafka/tls-pem
        readOnly: true
    {{- end }}
{{- if $.Values.kafka.tls.enabled }}
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
{{- if $secret }}
{{- $key := include "kafka.auth.usernameKey" . | trim }}
{{- if not (hasKey $secret.data $key) }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- index $secret.data $key | b64dec -}}
{{- else }}
{{- /* Lookup failed - likely RBAC issue. Use fallback; secret will be mounted at runtime. */}}
{{- $fallback -}}
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
{{- if $secret }}
{{- $key := include "kafka.auth.passwordKey" . | trim }}
{{- if not (hasKey $secret.data $key) }}
{{- fail (printf "Kafka auth existingSecret %q must contain key %q" .Values.kafka.auth.existingSecret $key) }}
{{- end }}
{{- index $secret.data $key | b64dec -}}
{{- else }}
{{- /* Lookup failed - likely RBAC issue. Use fallback; secret will be mounted at runtime. */}}
{{- $fallback | b64dec -}}
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

{{/*
Generate deterministic TLS PKCS12 store password.
*/}}
{{- define "kafka.tls.storePassword" -}}
{{- printf "%s-tls-store" .Release.Name | sha256sum | trunc 32 -}}
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
Validate TLS configuration. Called from the Certificate template so the
error surfaces during rendering.
*/}}
{{- define "kafka.tls.validate" -}}
{{- if .Values.kafka.tls.enabled -}}
{{- if and (not .Values.kafka.tls.existingSecret) (not .Values.kafka.tls.certManager.issuerRef.name) -}}
{{- fail "kafka.tls.enabled requires either kafka.tls.existingSecret or kafka.tls.certManager.issuerRef.name to be set" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Return the broker listener protocol based on TLS setting.
*/}}
{{- define "kafka.broker.listenerProtocol" -}}
{{- if .Values.kafka.tls.enabled -}}
SASL_SSL
{{- else -}}
SASL_PLAINTEXT
{{- end -}}
{{- end }}

{{/*
Return the controller listener security protocol based on TLS setting.
*/}}
{{- define "kafka.controller.listenerSecurityProtocol" -}}
{{- if .Values.kafka.tls.enabled -}}
SSL
{{- else -}}
PLAINTEXT
{{- end -}}
{{- end }}
