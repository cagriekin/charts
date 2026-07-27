#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-kafka-test-minimal}"
RELEASE="${RELEASE:-kafka-minimal}"
FULLNAME="${RELEASE}"
USER_NAME="testuser"

begin_suite "Minimal Install (1 controller + 1 broker, TLS/mTLS/SCRAM)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# TLS is on by default (chart-managed self-signed CA via cert-manager). The
# post-install bootstrap Job provisions the SCRAM users before helm returns.
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --wait --timeout 8m

CONTROLLER="${FULLNAME}-kafka-controller-0"
BROKER="${FULLNAME}-kafka-broker-0"

ctrl_phase=$(kubectl get pod -n "${NAMESPACE}" "${CONTROLLER}" -o jsonpath='{.status.phase}')
assert_eq "controller pod is Running" "Running" "${ctrl_phase}"

broker_phase=$(kubectl get pod -n "${NAMESPACE}" "${BROKER}" -o jsonpath='{.status.phase}')
assert_eq "broker pod is Running" "Running" "${broker_phase}"

# controller service exists on 9093
ctrl_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-kafka-controller" -o jsonpath='{.spec.ports[0].port}')
assert_eq "controller service port is 9093" "9093" "${ctrl_port}"

# broker service is headless
broker_svc_ip=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-kafka-broker" -o jsonpath='{.spec.clusterIP}')
assert_eq "broker service is headless" "None" "${broker_svc_ip}"

# per-user password secret exists
secret_key=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-secret" -o jsonpath="{.data.${USER_NAME}-password}" 2>/dev/null || echo "")
if [[ -n "${secret_key}" ]]; then pass "per-user SCRAM secret key present"; else fail "per-user SCRAM secret key present"; fi

# TLS materials issued by cert-manager
tls_secret=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-tls" -o name 2>/dev/null || echo "")
assert_contains "cert-manager issued the broker TLS secret" "${tls_secret}" "secret"

# --- SASL_SSL + SCRAM produce/consume as a client user ---
BROKER_SVC="${BROKER}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"
PASSWORD=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-secret" -o jsonpath="{.data.${USER_NAME}-password}" | base64 -d)
# PKCS12 store password is deterministic: sha256("<release>-tls-store")[:32]
STOREPASS=$(printf '%s' "${RELEASE}-tls-store" | sha256sum | cut -c1-32)

# Write a SASL_SSL/SCRAM client config inside the broker pod (reuses the pod's
# mTLS truststore, which trusts the chart CA).
kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "cat > /tmp/client.properties <<EOF
security.protocol=SASL_SSL
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username=\"${USER_NAME}\" password=\"${PASSWORD}\";
ssl.truststore.location=/opt/kafka/tls/truststore.p12
ssl.truststore.password=${STOREPASS}
ssl.truststore.type=PKCS12
EOF"

TEST_TOPIC="test-$(date +%s)"
TEST_VALUE="hello-$(date +%s)"

kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  echo '${TEST_VALUE}' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} \
    --topic ${TEST_TOPIC} \
    --producer.config /tmp/client.properties
"

consumed=$(kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  timeout 60 /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server ${BROKER_SVC} \
    --topic ${TEST_TOPIC} \
    --from-beginning \
    --max-messages 1 \
    --consumer.config /tmp/client.properties 2>/dev/null
" || echo "")
assert_eq "can produce and consume over SASL_SSL/SCRAM" "${TEST_VALUE}" "${consumed}"

topics_output=$(kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server ${BROKER_SVC} \
    --list \
    --command-config /tmp/client.properties 2>/dev/null
" || echo "")
assert_contains "auto-created topic exists" "${topics_output}" "${TEST_TOPIC}"

end_suite
print_summary
