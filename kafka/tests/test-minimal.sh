#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-kafka-test-minimal}"
RELEASE="${RELEASE:-kafka-minimal}"
FULLNAME="${RELEASE}-kafka"

begin_suite "Minimal Install (1 controller + 1 broker)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-minimal.yaml" \
  --wait --timeout 5m

CONTROLLER="${FULLNAME}-kafka-controller-0"
BROKER="${FULLNAME}-kafka-broker-0"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=kafka-controller" 1 300
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=kafka-broker" 1 300

# Test: controller is running
ctrl_phase=$(kubectl get pod -n "${NAMESPACE}" "${CONTROLLER}" -o jsonpath='{.status.phase}')
assert_eq "controller pod is Running" "Running" "${ctrl_phase}"

# Test: broker is running
broker_phase=$(kubectl get pod -n "${NAMESPACE}" "${BROKER}" -o jsonpath='{.status.phase}')
assert_eq "broker pod is Running" "Running" "${broker_phase}"

# Test: controller service exists
ctrl_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-kafka-controller" -o jsonpath='{.spec.ports[0].port}')
assert_eq "controller service port is 9093" "9093" "${ctrl_port}"

# Test: broker service exists (headless)
broker_svc_ip=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-kafka-broker" -o jsonpath='{.spec.clusterIP}')
assert_eq "broker service is headless" "None" "${broker_svc_ip}"

# Test: secret exists
secret_exists=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-secret" -o name 2>/dev/null || echo "")
assert_contains "kafka secret exists" "${secret_exists}" "secret"

# Helper: prepare JAAS config for kafka CLI commands
BROKER_SVC="${BROKER}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"
KAFKA_CLI_SETUP='
  KAFKA_USERNAME=$(cat /opt/kafka/secrets/username)
  KAFKA_PASSWORD=$(cat /opt/kafka/secrets/password)
  mkdir -p /tmp/kafka-config
  cp /opt/kafka/config/kraft/client.properties /tmp/kafka-config/
  sed -e "s|PLACEHOLDER_USERNAME|${KAFKA_USERNAME}|g" -e "s|PLACEHOLDER_PASSWORD|${KAFKA_PASSWORD}|g" \
    /opt/kafka/config/kraft/client_jaas.conf.template > /tmp/kafka-config/client_jaas.conf
  export KAFKA_OPTS="-Djava.security.auth.login.config=/tmp/kafka-config/client_jaas.conf"
'

# Test: produce and consume a message via SASL (unique topic per run)
TEST_TOPIC="test-$(date +%s)"
TEST_VALUE="hello-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  echo '${TEST_VALUE}' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} \
    --topic ${TEST_TOPIC} \
    --producer.config /tmp/kafka-config/client.properties
" 2>/dev/null

consumed=$(kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  timeout 30 /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server ${BROKER_SVC} \
    --topic ${TEST_TOPIC} \
    --from-beginning \
    --max-messages 1 \
    --consumer.config /tmp/kafka-config/client.properties 2>/dev/null
" 2>/dev/null || echo "")
assert_eq "can produce and consume message" "${TEST_VALUE}" "${consumed}"

# Test: list topics
topics_output=$(kubectl exec -n "${NAMESPACE}" "${BROKER}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server ${BROKER_SVC} \
    --list \
    --command-config /tmp/kafka-config/client.properties 2>/dev/null
" 2>/dev/null || echo "")
assert_contains "auto-created topic exists" "${topics_output}" "${TEST_TOPIC}"

end_suite
print_summary
