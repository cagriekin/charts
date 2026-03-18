#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-kafka-test-full}"
RELEASE="${RELEASE:-kafka-full}"
FULLNAME="${RELEASE}-kafka"

begin_suite "Full Install (1 controller + 2 brokers + exporter + topics)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --wait --timeout 10m

CONTROLLER="${FULLNAME}-kafka-controller-0"
BROKER_0="${FULLNAME}-kafka-broker-0"
BROKER_1="${FULLNAME}-kafka-broker-1"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=kafka-controller" 1 300
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=kafka-broker" 2 300

# Test: all pods running
ctrl_phase=$(kubectl get pod -n "${NAMESPACE}" "${CONTROLLER}" -o jsonpath='{.status.phase}')
assert_eq "controller pod is Running" "Running" "${ctrl_phase}"

for pod in "${BROKER_0}" "${BROKER_1}"; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "${pod} is Running" "Running" "${phase}"
done

# Helper: prepare JAAS config for kafka CLI commands
BROKER_SVC="${BROKER_0}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"
KAFKA_CLI_SETUP='
  KAFKA_USERNAME=$(cat /opt/kafka/secrets/username)
  KAFKA_PASSWORD=$(cat /opt/kafka/secrets/password)
  mkdir -p /tmp/kafka-config
  cp /opt/kafka/config/kraft/client.properties /tmp/kafka-config/
  sed -e "s|PLACEHOLDER_USERNAME|${KAFKA_USERNAME}|g" -e "s|PLACEHOLDER_PASSWORD|${KAFKA_PASSWORD}|g" \
    /opt/kafka/config/kraft/client_jaas.conf.template > /tmp/kafka-config/client_jaas.conf
  export KAFKA_OPTS="-Djava.security.auth.login.config=/tmp/kafka-config/client_jaas.conf"
'

# Test: declared topics were created (wait for topic init job which is deleted on success)
echo "  Waiting for declared topics to appear..."
topics_output=""
topic_timeout=120
topic_elapsed=0
while [[ ${topic_elapsed} -lt ${topic_timeout} ]]; do
  topics_output=$(kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
    ${KAFKA_CLI_SETUP}
    /opt/kafka/bin/kafka-topics.sh \
      --bootstrap-server ${BROKER_SVC} \
      --list \
      --command-config /tmp/kafka-config/client.properties 2>/dev/null
  " 2>/dev/null || echo "")
  if grep -q "test-topic-1" <<< "${topics_output}" && grep -q "test-topic-2" <<< "${topics_output}"; then
    break
  fi
  sleep 5
  topic_elapsed=$((topic_elapsed + 5))
done
assert_contains "test-topic-1 exists" "${topics_output}" "test-topic-1"
assert_contains "test-topic-2 exists" "${topics_output}" "test-topic-2"

# Test: topic partitions correct
t1_partitions=$(kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server ${BROKER_SVC} \
    --describe --topic test-topic-1 \
    --command-config /tmp/kafka-config/client.properties 2>/dev/null \
    | grep 'PartitionCount' | sed 's/.*PartitionCount:[[:space:]]*//' | cut -f1
" 2>/dev/null || echo "")
assert_eq "test-topic-1 has 3 partitions" "3" "${t1_partitions}"

# Test: cross-broker produce/consume (unique topic per run)
CROSS_TOPIC="cross-test-$(date +%s)"
TEST_VALUE="cross-broker-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  echo '${TEST_VALUE}' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} \
    --topic ${CROSS_TOPIC} \
    --producer.config /tmp/kafka-config/client.properties
" 2>/dev/null

BROKER_1_SVC="${BROKER_1}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"
consumed=$(kubectl exec -n "${NAMESPACE}" "${BROKER_1}" -- bash -c "
  ${KAFKA_CLI_SETUP}
  timeout 30 /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server ${BROKER_1_SVC} \
    --topic ${CROSS_TOPIC} \
    --from-beginning \
    --max-messages 1 \
    --consumer.config /tmp/kafka-config/client.properties 2>/dev/null
" 2>/dev/null || echo "")
assert_eq "cross-broker produce/consume works" "${TEST_VALUE}" "${consumed}"

# --- Exporter tests ---
echo ""
echo "  -- Exporter tests --"

wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-kafka-exporter" 300

exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=kafka-exporter" \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "exporter pod is Running" "Running" "${exporter_phase}"

exporter_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-kafka-exporter" -o jsonpath='{.spec.ports[0].port}')
assert_eq "exporter service port is 9308" "9308" "${exporter_port}"

# Test: exporter returns kafka metrics (use a temp pod with curl since kafka image lacks it)
exporter_svc="${FULLNAME}-kafka-exporter.${NAMESPACE}.svc.cluster.local"
metrics_output=$(kubectl run curl-test -n "${NAMESPACE}" --rm -i --restart=Never \
  --image=busybox:1.37 -- wget -qO- "http://${exporter_svc}:9308/metrics" 2>/dev/null \
  | grep -m1 '^kafka_' || echo "")
assert_contains "exporter returns kafka metrics" "${metrics_output}" "kafka_"

end_suite
print_summary
