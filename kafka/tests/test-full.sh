#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-kafka-test-full}"
RELEASE="${RELEASE:-kafka-full}"
FULLNAME="${RELEASE}"

# On any failure (e.g. a helm --wait timeout), dump cluster state so CI logs are
# self-diagnosing instead of a bare "timed out waiting for the condition".
dump_diagnostics() {
  local rc=$?
  [ "${rc}" -eq 0 ] && return 0
  echo "=== FAILURE DIAGNOSTICS (rc=${rc}, namespace ${NAMESPACE}) ==="
  kubectl get pods,jobs,certificate,issuer -n "${NAMESPACE}" 2>&1 || true
  kubectl get events -n "${NAMESPACE}" --sort-by=.lastTimestamp 2>&1 | tail -25 || true
  for p in $(kubectl get pods -n "${NAMESPACE}" -o name 2>/dev/null); do
    echo "--- ${p} ---"
    kubectl describe "${p}" -n "${NAMESPACE}" 2>&1 | sed -n '/Events:/,$p' | tail -12 || true
    kubectl logs "${p}" -n "${NAMESPACE}" --all-containers --tail=40 2>&1 | tail -40 || true
  done
}
trap dump_diagnostics EXIT

begin_suite "Full Install (1 controller + 2 brokers + exporter + topics + ACLs, TLS/mTLS/SCRAM)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# TLS on by default (chart self-signed CA). The bootstrap Job provisions SCRAM
# users, declared topics, and ACLs before helm returns.
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --wait --timeout 12m

CONTROLLER="${FULLNAME}-kafka-controller-0"
BROKER_0="${FULLNAME}-kafka-broker-0"
BROKER_1="${FULLNAME}-kafka-broker-1"

ctrl_phase=$(kubectl get pod -n "${NAMESPACE}" "${CONTROLLER}" -o jsonpath='{.status.phase}')
assert_eq "controller pod is Running" "Running" "${ctrl_phase}"

for pod in "${BROKER_0}" "${BROKER_1}"; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "${pod} is Running" "Running" "${phase}"
done

TESTUSER_PW=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-secret" -o jsonpath='{.data.testuser-password}' | base64 -d)
APPUSER_PW=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-kafka-secret" -o jsonpath='{.data.appuser-password}' | base64 -d)

# Write a SASL_SSL/SCRAM client config for a given user inside a pod. The PKCS12
# store password is ephemeral and per-pod, so read it from the pod we write into.
write_client_props() {
  local pod="$1" user="$2" pw="$3"
  kubectl exec -n "${NAMESPACE}" "${pod}" -- bash -c "SP=\$(cat /opt/kafka/tls/store-pass); cat > /tmp/${user}.properties <<EOF
security.protocol=SASL_SSL
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username=\"${user}\" password=\"${pw}\";
ssl.truststore.location=/opt/kafka/tls/truststore.p12
ssl.truststore.password=\${SP}
ssl.truststore.type=PKCS12
EOF"
}

write_client_props "${BROKER_0}" testuser "${TESTUSER_PW}"
write_client_props "${BROKER_1}" testuser "${TESTUSER_PW}"
write_client_props "${BROKER_0}" appuser "${APPUSER_PW}"

BROKER_SVC="${BROKER_0}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"
BROKER_1_SVC="${BROKER_1}.${FULLNAME}-kafka-broker.${NAMESPACE}.svc.cluster.local:9092"

# Declared topics were created by the bootstrap Job.
topics_output=$(kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server ${BROKER_SVC} --list \
    --command-config /tmp/testuser.properties 2>/dev/null
" || echo "")
assert_contains "test-topic-1 exists" "${topics_output}" "test-topic-1"
assert_contains "test-topic-2 exists" "${topics_output}" "test-topic-2"

# test-topic-1 has 3 partitions
t1_partitions=$(kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server ${BROKER_SVC} --describe --topic test-topic-1 \
    --command-config /tmp/testuser.properties 2>/dev/null \
    | grep 'PartitionCount' | sed 's/.*PartitionCount:[[:space:]]*//' | cut -f1
" || echo "")
assert_eq "test-topic-1 has 3 partitions" "3" "${t1_partitions}"

# Cross-broker produce (broker-0) / consume (broker-1) as the superuser
CROSS_TOPIC="cross-test-$(date +%s)"
TEST_VALUE="cross-broker-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  echo '${TEST_VALUE}' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} --topic ${CROSS_TOPIC} \
    --producer.config /tmp/testuser.properties
"
consumed=$(kubectl exec -n "${NAMESPACE}" "${BROKER_1}" -- bash -c "
  timeout 60 /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server ${BROKER_1_SVC} --topic ${CROSS_TOPIC} \
    --from-beginning --max-messages 1 \
    --consumer.config /tmp/testuser.properties 2>/dev/null
" || echo "")
assert_eq "cross-broker produce/consume works" "${TEST_VALUE}" "${consumed}"

# --- ACL enforcement: appuser has a producer role on test-topic-1 ---
acl_value="acl-$(date +%s)"
acl_rc=0
kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  echo '${acl_value}' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} --topic test-topic-1 \
    --producer.config /tmp/appuser.properties
" >/dev/null 2>&1 || acl_rc=$?
assert_eq "appuser (producer ACL) can write to test-topic-1" "0" "${acl_rc}"

# appuser has NO ACL on test-topic-2, so a write must be denied. Note that
# kafka-console-producer exits 0 even on an authorization failure, so detect the
# error message rather than the process exit code.
deny_out=$(kubectl exec -n "${NAMESPACE}" "${BROKER_0}" -- bash -c "
  echo 'nope' | /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server ${BROKER_SVC} --topic test-topic-2 \
    --producer.config /tmp/appuser.properties 2>&1
" || true)
if echo "${deny_out}" | grep -qiE 'authorization failed|TOPIC_AUTHORIZATION_FAILED'; then
  pass "appuser without ACL is denied on test-topic-2"
else
  fail "appuser without ACL is denied on test-topic-2" "${deny_out}"
fi

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

exporter_svc="${FULLNAME}-kafka-exporter"
# The exporter can restart a few times on a fresh cluster until the broker DNS
# resolves, so poll /metrics until kafka_ series appear (bounded).
metrics_output=""
for _ in $(seq 1 12); do
  kubectl port-forward -n "${NAMESPACE}" "svc/${exporter_svc}" 19308:9308 >/dev/null 2>&1 &
  PF_PID=$!
  sleep 3
  metrics_output=$( { curl -sf "http://127.0.0.1:19308/metrics" 2>/dev/null || wget -qO- "http://127.0.0.1:19308/metrics" 2>/dev/null; } | grep -m1 '^kafka_' || echo "")
  kill "${PF_PID}" 2>/dev/null || true
  wait "${PF_PID}" 2>/dev/null || true
  [ -n "${metrics_output}" ] && break
  sleep 5
done
assert_contains "exporter returns kafka metrics" "${metrics_output}" "kafka_"

end_suite
print_summary
