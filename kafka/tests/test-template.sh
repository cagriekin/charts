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

# Render minimal template
minimal=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-minimal.yaml" 2>&1)

assert_contains "minimal: controller statefulset present" "${minimal}" "kafka-controller"
assert_contains "minimal: broker statefulset present" "${minimal}" "kafka-broker"
assert_contains "minimal: controller service present" "${minimal}" "kind: Service"
assert_contains "minimal: secret created" "${minimal}" "kind: Secret"
assert_contains "minimal: configmap created" "${minimal}" "kind: ConfigMap"
assert_not_contains "minimal: no exporter deployment" "${minimal}" "kafka-exporter"
assert_not_contains "minimal: no topics in configmap" "${minimal}" "test-topic"

# Render full template
full=$(helm template test-kafka "${CHART_DIR}" -f "${SCRIPT_DIR}/values-full-test.yaml" 2>&1)

assert_contains "full: controller statefulset present" "${full}" "kafka-controller"
assert_contains "full: broker statefulset present" "${full}" "kafka-broker"
assert_contains "full: exporter deployment present" "${full}" "kafka-exporter"
assert_contains "full: topic init job present" "${full}" "kafka-topic-init"
assert_contains "full: topics configmap present" "${full}" "kafka-topics"
assert_contains "full: exporter service present" "${full}" "port: 9308"
assert_contains "full: controller port 9093" "${full}" "port: 9093"
assert_contains "full: broker port 9092" "${full}" "port: 9092"
assert_contains "full: SASL configured" "${full}" "SASL_PLAINTEXT"
assert_contains "full: serviceaccount created" "${full}" "kind: ServiceAccount"
assert_contains "full: broker replicas 2" "${full}" "replicas: 2"

# Render default template and verify enterprise defaults
defaults=$(helm template test-kafka "${CHART_DIR}" 2>&1)

assert_contains "defaults: controller replicas 3" "${defaults}" "replicas: 3"
assert_contains "defaults: terminationGracePeriodSeconds set" "${defaults}" "terminationGracePeriodSeconds: 120"
assert_contains "defaults: headless controller service" "${defaults}" "clusterIP: None"
assert_contains "defaults: broker config replication factor" "${defaults}" "default.replication.factor=3"
assert_contains "defaults: broker config min ISR" "${defaults}" "min.insync.replicas=2"
assert_contains "defaults: log retention configured" "${defaults}" "log.retention.hours=168"
assert_contains "defaults: auto create topics disabled" "${defaults}" "auto.create.topics.enable=false"

# --- TLS with cert-manager ---
tls_cm=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.enabled=true \
  --set kafka.tls.certManager.issuerRef.name=my-issuer 2>&1)

assert_contains "tls-cm: Certificate resource created" "${tls_cm}" "kind: Certificate"
assert_contains "tls-cm: issuer name set" "${tls_cm}" "name: my-issuer"
assert_contains "tls-cm: wildcard controller SAN" "${tls_cm}" "*.test-kafka-kafka-kafka-controller"
assert_contains "tls-cm: wildcard broker SAN" "${tls_cm}" "*.test-kafka-kafka-kafka-broker"
assert_contains "tls-cm: broker listener uses SASL_SSL" "${tls_cm}" "SASL_SSL"
assert_contains "tls-cm: controller protocol uses SSL" "${tls_cm}" "CONTROLLER:SSL"
assert_contains "tls-cm: ssl keystore configured" "${tls_cm}" "ssl.keystore.type=PKCS12"
assert_contains "tls-cm: ssl truststore configured" "${tls_cm}" "ssl.truststore.type=PKCS12"
assert_contains "tls-cm: tls-init init container present" "${tls_cm}" "tls-init"
assert_contains "tls-cm: tls PEM volume mount present" "${tls_cm}" "kafka-tls-pem"
assert_contains "tls-cm: exporter tls flag present" "${tls_cm}" "tls.enabled"
assert_contains "tls-cm: exporter ca-file flag present" "${tls_cm}" "tls.ca-file"
assert_not_contains "tls-cm: no SASL_PLAINTEXT when TLS enabled" "${tls_cm}" "SASL_PLAINTEXT"

# --- TLS with existing secret (no cert-manager Certificate) ---
tls_es=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.enabled=true \
  --set kafka.tls.existingSecret=my-tls-secret 2>&1)

assert_not_contains "tls-existing: no Certificate resource" "${tls_es}" "kind: Certificate"
assert_contains "tls-existing: volumes reference existing secret" "${tls_es}" "secretName: my-tls-secret"
assert_contains "tls-existing: SASL_SSL listeners" "${tls_es}" "SASL_SSL"

# --- TLS fail-fast: enabled without secret or issuer ---
tls_fail=$(helm template test-kafka "${CHART_DIR}" \
  --set kafka.tls.enabled=true 2>&1) && tls_fail_rc=0 || tls_fail_rc=$?

assert_eq "tls-fail: template fails when no secret or issuer" "1" "${tls_fail_rc}"
assert_contains "tls-fail: error mentions required config" "${tls_fail}" "kafka.tls.existingSecret or kafka.tls.certManager.issuerRef.name"

# --- TLS disabled: no TLS artifacts ---
assert_not_contains "defaults: no Certificate when TLS disabled" "${defaults}" "kind: Certificate"
assert_not_contains "defaults: no tls-init when TLS disabled" "${defaults}" "tls-init"
assert_not_contains "defaults: no ssl keystore when TLS disabled" "${defaults}" "ssl.keystore"
assert_not_contains "defaults: no SASL_SSL when TLS disabled" "${defaults}" "SASL_SSL"

end_suite
print_summary
