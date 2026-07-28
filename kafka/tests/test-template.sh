#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

begin_suite "Helm Template Rendering"

# Lint tests
lint_output=$(helm lint "${CHART_DIR}" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with default values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with minimal values passes" "0" "${lint_rc}"

lint_output=$(helm lint "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1) && lint_rc=0 || lint_rc=$?
assert_eq "helm lint with full values passes" "0" "${lint_rc}"

# Render minimal template (TLS on via chart self-signed CA)
minimal=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)

assert_contains "minimal: controller statefulset present" "${minimal}" "kafka-controller"
assert_contains "minimal: broker statefulset present" "${minimal}" "kafka-broker"
assert_contains "minimal: controller service present" "${minimal}" "kind: Service"
assert_contains "minimal: secret created" "${minimal}" "kind: Secret"
assert_contains "minimal: per-user password key" "${minimal}" "testuser-password"
assert_contains "minimal: configmap created" "${minimal}" "kind: ConfigMap"
assert_not_contains "minimal: no exporter deployment" "${minimal}" "kafka-exporter"
assert_not_contains "minimal: no topics in configmap" "${minimal}" "test-topic"

# Render full template (TLS on via chart self-signed CA)
full=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)

assert_contains "full: controller statefulset present" "${full}" "kafka-controller"
assert_contains "full: broker statefulset present" "${full}" "kafka-broker"
assert_contains "full: exporter deployment present" "${full}" "kafka-exporter"
assert_contains "full: topic init job present" "${full}" "kafka-topic-init"
assert_contains "full: topics configmap present" "${full}" "kafka-topics"
assert_contains "full: exporter service present" "${full}" "port: 9308"
assert_contains "full: controller port 9093" "${full}" "port: 9093"
assert_contains "full: broker port 9092" "${full}" "port: 9092"
assert_contains "full: internal listener port 9094" "${full}" "port: 9094"
assert_contains "full: SASL_SSL client listener" "${full}" "SASL_SSL"
assert_contains "full: authorizer enabled" "${full}" "StandardAuthorizer"
assert_contains "full: job provisions SCRAM users" "${full}" "kafka-configs.sh"
assert_contains "full: job applies ACLs" "${full}" "kafka-acls.sh"
assert_contains "full: serviceaccount created" "${full}" "kind: ServiceAccount"
assert_contains "full: broker replicas 2" "${full}" "replicas: 2"

# Render default template and verify enterprise secure defaults
defaults=$(helm template test-kafka "${CHART_DIR}" 2>&1)

assert_contains "defaults: controller replicas 3" "${defaults}" "replicas: 3"
assert_contains "defaults: terminationGracePeriodSeconds set" "${defaults}" "terminationGracePeriodSeconds: 120"
assert_contains "defaults: headless controller service" "${defaults}" "clusterIP: None"
assert_contains "defaults: broker config replication factor" "${defaults}" "default.replication.factor=3"
assert_contains "defaults: broker config min ISR" "${defaults}" "min.insync.replicas=2"
assert_contains "defaults: log retention configured" "${defaults}" "log.retention.hours=168"
assert_contains "defaults: auto create topics disabled" "${defaults}" "auto.create.topics.enable=false"
# secure-by-default
assert_contains "defaults: TLS on, SASL_SSL client listener" "${defaults}" "CLIENT:SASL_SSL"
assert_contains "defaults: mTLS on internal listener" "${defaults}" "listener.name.internal.ssl.client.auth=required"
assert_contains "defaults: SCRAM-SHA-512 mechanism" "${defaults}" "sasl.enabled.mechanisms=SCRAM-SHA-512"
assert_contains "defaults: StandardAuthorizer" "${defaults}" "StandardAuthorizer"
assert_contains "defaults: deny by default" "${defaults}" "allow.everyone.if.no.acl.found=false"
assert_contains "defaults: self-signed Issuer created" "${defaults}" "test-kafka-kafka-selfsigned"
assert_contains "defaults: CA Issuer created" "${defaults}" "test-kafka-kafka-ca"
assert_contains "defaults: leaf Certificate created" "${defaults}" "kind: Certificate"
assert_contains "defaults: tls-init init container present" "${defaults}" "tls-init"

# --- TLS with an operator-supplied cert-manager issuer ---
tls_cm=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.certManager.issuerRef.name=my-issuer 2>&1)

assert_contains "tls-cm: Certificate resource created" "${tls_cm}" "kind: Certificate"
assert_contains "tls-cm: issuer name set" "${tls_cm}" "name: my-issuer"
assert_contains "tls-cm: wildcard controller SAN" "${tls_cm}" "*.test-kafka-kafka-controller"
assert_contains "tls-cm: wildcard broker SAN" "${tls_cm}" "*.test-kafka-kafka-broker"
assert_contains "tls-cm: broker listener uses SASL_SSL" "${tls_cm}" "SASL_SSL"
assert_contains "tls-cm: controller protocol uses SSL" "${tls_cm}" "CONTROLLER:SSL"
assert_contains "tls-cm: ssl keystore configured" "${tls_cm}" "ssl.keystore.type=PKCS12"
assert_contains "tls-cm: exporter mTLS cert flag present" "${tls_cm}" "tls.cert-file"
assert_not_contains "tls-cm: no chart self-signed issuer when external issuer set" "${tls_cm}" "kafka-selfsigned"

# --- TLS with existing secret (no cert-manager Certificate) ---
tls_es=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.existingSecret=my-tls-secret 2>&1)

assert_not_contains "tls-existing: no Certificate resource" "${tls_es}" "kind: Certificate"
assert_not_contains "tls-existing: no self-signed issuer" "${tls_es}" "kafka-selfsigned"
assert_contains "tls-existing: volumes reference existing secret" "${tls_es}" "secretName: my-tls-secret"
assert_contains "tls-existing: SASL_SSL listeners" "${tls_es}" "SASL_SSL"

# --- Bare defaults render (self-signed CA), no fail-fast ---
helm template test-kafka "${CHART_DIR}" >/dev/null 2>&1 && ss_rc=0 || ss_rc=$?
assert_eq "self-signed: bare defaults render successfully" "0" "${ss_rc}"

# --- Insecure opt-out ---
insecure=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.enabled=false \
  --set kafka.auth.allowInsecure=true 2>&1)

assert_contains "insecure: SASL_PLAINTEXT client listener" "${insecure}" "CLIENT:SASL_PLAINTEXT"
assert_contains "insecure: ANONYMOUS super-user" "${insecure}" "User:ANONYMOUS"
assert_not_contains "insecure: no Certificate" "${insecure}" "kind: Certificate"
assert_not_contains "insecure: no tls-init" "${insecure}" "tls-init"

# --- Guard: TLS disabled without the insecure opt-in fails ---
guard=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.enabled=false 2>&1) && guard_rc=0 || guard_rc=$?

assert_eq "guard: template fails when TLS disabled without allowInsecure" "1" "${guard_rc}"
assert_contains "guard: error mentions allowInsecure" "${guard}" "kafka.auth.allowInsecure"

end_suite
print_summary
