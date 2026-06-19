#!/bin/bash
# Live etcd-mode test over MUTUAL TLS + per-tenant RBAC (#184). Proves the runtime
# paths that render tests cannot: the TLS handshake + etcdctl health probe under
# --client-cert-auth, the agent authenticating as its client-cert Common Name (no
# username), failover over mTLS, and that a tenant cert is authorized ONLY on its
# key prefix (a write outside it is denied). OPT-IN / standalone (deploys 3 etcd pods
# + a postgres pair): run in a clean cluster with `make -C pg test-agent-etcd-tls`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

# The release name is load-bearing: the agent's ETCD_PREFIX is /pg-ha/<release>/ and
# must match the RBAC tenant grant + the client-cert CN in the fixture.
RELEASE="pgetcdtls"
NAMESPACE="${NAMESPACE:-pg-test-agent-etcd-tls}"
TENANT_CN="pgetcdtls"
PREFIX="/pg-ha/pgetcdtls/"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
ETCD_FULL="${RELEASE}-etcd"
ETCD_STS="${ETCD_FULL}"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"
CERTDIR="$(mktemp -d)"
trap 'rm -rf "${CERTDIR}"' EXIT

begin_suite "Agent etcd DCS over mutual TLS + per-tenant RBAC (#184)"

# --- generate a test CA + server/admin/tenant certs (self-signed, openssl) ---
# server cert: SANs for the client Service + the wildcard headless peer domain +
# localhost/127.0.0.1 (the health probe hits localhost), and BOTH server- and
# client-auth usages (etcdctl health uses it as a client under client-cert-auth).
gen_ca() {
  openssl genpkey -algorithm RSA -out "${CERTDIR}/ca.key" >/dev/null 2>&1
  openssl req -x509 -new -key "${CERTDIR}/ca.key" -days 3650 \
    -subj "/CN=etcd-test-ca" -out "${CERTDIR}/ca.crt" >/dev/null 2>&1
}
# gen_cert <name> <CN> <san-or-empty> <eku>
gen_cert() {
  local name="$1" cn="$2" san="$3" eku="$4"
  openssl genpkey -algorithm RSA -out "${CERTDIR}/${name}.key" >/dev/null 2>&1
  openssl req -new -key "${CERTDIR}/${name}.key" -subj "/CN=${cn}" -out "${CERTDIR}/${name}.csr" >/dev/null 2>&1
  { [[ -n "${san}" ]] && echo "subjectAltName=${san}"; echo "extendedKeyUsage=${eku}"; } > "${CERTDIR}/${name}.ext"
  openssl x509 -req -in "${CERTDIR}/${name}.csr" -CA "${CERTDIR}/ca.crt" -CAkey "${CERTDIR}/ca.key" \
    -CAcreateserial -days 3650 -extfile "${CERTDIR}/${name}.ext" -out "${CERTDIR}/${name}.crt" >/dev/null 2>&1
}
SERVER_SAN="DNS:${ETCD_FULL},DNS:${ETCD_FULL}.${NAMESPACE}.svc,DNS:${ETCD_FULL}.${NAMESPACE}.svc.cluster.local,DNS:*.${ETCD_FULL}-headless.${NAMESPACE}.svc.cluster.local,DNS:localhost,IP:127.0.0.1"
gen_ca
gen_cert server "${ETCD_FULL}" "${SERVER_SAN}" "serverAuth,clientAuth"
gen_cert admin  "root"         ""               "clientAuth"
gen_cert client "${TENANT_CN}" ""               "clientAuth"
certs_ok=$([[ -s "${CERTDIR}/server.crt" && -s "${CERTDIR}/admin.crt" && -s "${CERTDIR}/client.crt" ]] && echo ok || echo fail)
assert_eq "test certs generated (CA + server + admin + tenant)" "ok" "${certs_ok}"

# --- clean + create namespace + cert Secrets ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete statefulset "${FULLNAME}" "${ETCD_STS}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
# The timeline-highwater marker is agent-created (not helm-managed), so it survives
# uninstall; a stale marker from a prior run's failover (timeline > the fresh PVCs')
# trips the #125 guard and the new primary refuses to start read-write. Drop it so a
# same-namespace re-run starts clean (CI runs in a fresh namespace and never hits this).
kubectl delete configmap "${FULLNAME}-primary" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
for s in etcd-server-tls etcd-admin-tls etcd-client-tls; do
  kubectl delete secret "${s}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
done
mk_secret() { # mk_secret <name> <cert-base>
  kubectl create secret generic "$1" -n "${NAMESPACE}" \
    --from-file=tls.crt="${CERTDIR}/$2.crt" \
    --from-file=tls.key="${CERTDIR}/$2.key" \
    --from-file=ca.crt="${CERTDIR}/ca.crt" >/dev/null
}
mk_secret etcd-server-tls server
mk_secret etcd-admin-tls  admin
mk_secret etcd-client-tls client
sleep 2

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  -f "${SCRIPT_DIR}/values-agent-etcd-tls.yaml" \
  --wait --timeout 10m

POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"

# --- the bundled etcd cluster forms over TLS (peer + client) ---
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=etcd" 3 300
etcd_ready=$(kubectl get statefulset "${ETCD_STS}" -n "${NAMESPACE}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
assert_eq "etcd: 3 members ready over TLS (exec health probe authenticates)" "3" "${etcd_ready}"

# the member serves https only: a plaintext etcdctl call must fail
plain_rc=0
kubectl exec -n "${NAMESPACE}" "${ETCD_FULL}-0" -c etcd -- \
  etcdctl --endpoints=http://127.0.0.1:2379 endpoint health >/dev/null 2>&1 || plain_rc=$?
assert_gt "etcd: plaintext client is rejected (TLS-only)" "${plain_rc}" "0"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# --- the agent authenticated to etcd over mTLS as its CN: leadership is in etcd ---
lease_found=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_eq "etcd mode: no apiserver leader Lease (leadership is in etcd, reached over mTLS)" "" "${lease_found}"

echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a primary via mTLS+CN auth (RBAC permits its prefix)" "${PRIMARY:-none}" "${FULLNAME}"

if [[ -n "${PRIMARY}" && -n "${STANDBY}" ]]; then
  FV="tls-$(date +%s)"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS tls_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
  pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO tls_test (value) VALUES ('${FV}')" "testuser" "testdb"
  sleep 3
  repl=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM tls_test WHERE value='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "data replicated to standby" "${FV}" "${repl}"

  echo "  Deleting primary ${PRIMARY} (mTLS failover)..."
  kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
  promoted=false; e=0
  while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "t" ]]; then promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; fi
    sleep 3; e=$((e + 3))
  done
  assert_eq "standby promoted on mTLS failover (leadership moved in etcd)" "true" "${promoted}"
  if [[ "${promoted}" == "true" ]]; then
    survived=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT value FROM tls_test WHERE value='${FV}'" "testuser" "testdb")
    assert_eq "data survives mTLS failover" "${FV}" "${survived}"
  else
    skip "data survives mTLS failover (failover did not complete)"
  fi
else
  skip "data replicated to standby (no primary settled)"
  skip "standby promoted on mTLS failover (no primary settled)"
  skip "data survives mTLS failover (no primary settled)"
fi

# --- RBAC: the tenant cert is authorized ONLY on its prefix ---
# One-shot pods present the SAME client cert the agent uses (CN=pgetcdtls) and run
# `etcdctl put` as the container command (the etcd image is distroless -- no shell, so
# no sleep+exec). They carry the same-release postgresql labels so the etcd
# NetworkPolicy admits them (a foreign label would be dropped and the put would time
# out -- which must NOT be mistaken for an RBAC denial; see the denial assert below).
# etcd maps the cert CN to its role: a put inside /pg-ha/pgetcdtls/ exits 0 (pod
# Succeeded), one outside is denied (pod Failed). This is the per-tenant isolation
# #184 exists for.
etcdctl_put_phase() { # <pod-name> <key> -> echoes terminal pod phase (pod left for log inspection)
  local name="$1" key="$2" p="" i=0
  kubectl delete pod "${name}" -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  cat <<EOF | kubectl apply -n "${NAMESPACE}" -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  labels:
    app.kubernetes.io/instance: ${RELEASE}
    app.kubernetes.io/component: postgresql
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: ${ETCD_IMAGE}
      env: [{ name: ETCDCTL_API, value: "3" }]
      command: ["/usr/local/bin/etcdctl","--cacert=/tls/ca.crt","--cert=/tls/tls.crt","--key=/tls/tls.key","--endpoints=https://${ETCD_FULL}:2379","--dial-timeout=10s","--command-timeout=20s","put","${key}","v"]
      volumeMounts: [{ name: tls, mountPath: /tls, readOnly: true }]
  volumes:
    - name: tls
      secret: { secretName: etcd-client-tls }
EOF
  while [[ ${i} -lt 90 ]]; do
    p=$(kubectl get pod "${name}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [[ "${p}" == "Succeeded" || "${p}" == "Failed" ]] && break
    sleep 2; i=$((i + 2))
  done
  echo "${p}"
}
in_phase=$(etcdctl_put_phase rbac-in "${PREFIX}_rbac_test")
assert_eq "RBAC: tenant cert may write inside its own prefix" "Succeeded" "${in_phase}"
out_phase=$(etcdctl_put_phase rbac-out "/pg-ha/other-tenant/x")
out_log=$(kubectl logs rbac-out -n "${NAMESPACE}" 2>/dev/null || echo "")
assert_eq "RBAC: out-of-prefix write is rejected (pod Failed)" "Failed" "${out_phase}"
# the rejection must be an RBAC permission denial, NOT a timeout/connectivity failure
# (both surface as a Failed pod -- assert the actual cause).
assert_contains "RBAC: rejection is a permission denial, not a timeout" "${out_log}" "permission denied"
kubectl delete pod rbac-in rbac-out -n "${NAMESPACE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true

end_suite
print_summary
