#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-tls}"
RELEASE="${RELEASE:-redis-tls}"
VALUES="${SCRIPT_DIR}/values-tls.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
HEADLESS="${FULLNAME}-headless"
DOMAIN="${NAMESPACE}.svc.cluster.local"
TLS_CLI="--tls --cert /etc/redis/tls/tls.crt --key /etc/redis/tls/tls.key --cacert /etc/redis/tls/ca.crt"

begin_suite "Replication over TLS"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Self-signed CA + server cert. SANs cover the per-pod headless FQDNs, the read/sentinel
# Services, and 127.0.0.1/localhost (the in-container probes connect to localhost).
cat > "${TMP}/san.cnf" <<EOF
[req]
distinguished_name = req
[v3_ca]
basicConstraints = CA:TRUE
[v3]
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth, clientAuth
subjectAltName = @alt
[alt]
DNS.1 = *.${HEADLESS}.${DOMAIN}
DNS.2 = ${HEADLESS}.${DOMAIN}
DNS.3 = ${FULLNAME}.${DOMAIN}
DNS.4 = ${FULLNAME}-sentinel.${DOMAIN}
DNS.5 = localhost
IP.1 = 127.0.0.1
EOF

openssl genrsa -out "${TMP}/ca.key" 2048 2>/dev/null
openssl req -x509 -new -nodes -key "${TMP}/ca.key" -subj "/CN=redis-test-ca" -days 3 \
  -extensions v3_ca -config "${TMP}/san.cnf" -out "${TMP}/ca.crt" 2>/dev/null
openssl genrsa -out "${TMP}/tls.key" 2048 2>/dev/null
openssl req -new -key "${TMP}/tls.key" -subj "/CN=${FULLNAME}" -out "${TMP}/tls.csr" 2>/dev/null
openssl x509 -req -in "${TMP}/tls.csr" -CA "${TMP}/ca.crt" -CAkey "${TMP}/ca.key" -CAcreateserial \
  -days 3 -extensions v3 -extfile "${TMP}/san.cnf" -out "${TMP}/tls.crt" 2>/dev/null

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic redis-tls-cert -n "${NAMESPACE}" \
  --from-file=tls.crt="${TMP}/tls.crt" \
  --from-file=tls.key="${TMP}/tls.key" \
  --from-file=ca.crt="${TMP}/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 8m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 3 480

# TLS connectivity on the master.
result=$(kubectl exec -n "${NAMESPACE}" "${FULLNAME}-0" -c redis -- sh -c "redis-cli ${TLS_CLI} -p 6379 ping" 2>/dev/null | tr -d '\r')
assert_eq "redis answers over TLS" "PONG" "${result}"

# Replication formed over TLS.
role1=$(kubectl exec -n "${NAMESPACE}" "${FULLNAME}-1" -c redis -- sh -c "redis-cli ${TLS_CLI} -p 6379 INFO replication" 2>/dev/null | tr -d '\r' | awk -F: '/^role:/{print $2}')
assert_eq "replica connected over TLS" "slave" "${role1}"

# Plaintext is refused (TLS-only: port 0).
if kubectl exec -n "${NAMESPACE}" "${FULLNAME}-0" -c redis -- sh -c "redis-cli -p 6379 ping" 2>&1 | grep -qi "PONG"; then
  fail "plaintext refused on TLS-only port" "got a PONG without TLS"
else
  pass "plaintext refused on TLS-only port"
fi

end_suite
print_summary
