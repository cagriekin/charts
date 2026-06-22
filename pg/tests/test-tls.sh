#!/bin/bash
# Live client-connection TLS test (#110). Proves the runtime paths render tests cannot:
# PostgreSQL serving ssl=on, `require` rejecting non-TLS clients, mutual TLS rejecting an
# app user with no client cert WHILE the internal service users (the exempt superuser, the
# agent/repmgr, the exporter, pgpool) keep working, replication + failover under TLS, and
# PGPool serving over TLS. A second light install proves optional server TLS in repmgrd
# mode. OPT-IN / standalone (run in a clean cluster): `make -C pg test-tls`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

RELEASE="pgtls"
NAMESPACE="${NAMESPACE:-pg-test-tls}"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
RW_SVC="${FULLNAME}"            # primary (read-write) Service
RO_SVC="${FULLNAME}-readonly"
HEADLESS="${FULLNAME}-headless"
CERTDIR="$(mktemp -d)"
trap 'rm -rf "${CERTDIR}"' EXIT

begin_suite "Client-connection TLS for PostgreSQL + PGPool + exporter (#110)"

# --- CA + server cert (serverAuth, SANs for the Services + pod FQDNs + localhost) and a
#     client cert (clientAuth, CN=appuser) reused for the app-user mTLS case and the
#     pgpool backend client cert. ---
gen_ca() {
  openssl genpkey -algorithm RSA -out "${CERTDIR}/ca.key" >/dev/null 2>&1
  openssl req -x509 -new -key "${CERTDIR}/ca.key" -days 3650 -subj "/CN=pg-test-ca" -out "${CERTDIR}/ca.crt" >/dev/null 2>&1
}
gen_cert() { # gen_cert <name> <CN> <san-or-empty> <eku>
  local name="$1" cn="$2" san="$3" eku="$4"
  openssl genpkey -algorithm RSA -out "${CERTDIR}/${name}.key" >/dev/null 2>&1
  openssl req -new -key "${CERTDIR}/${name}.key" -subj "/CN=${cn}" -out "${CERTDIR}/${name}.csr" >/dev/null 2>&1
  { [[ -n "${san}" ]] && echo "subjectAltName=${san}"; echo "extendedKeyUsage=${eku}"; } > "${CERTDIR}/${name}.ext"
  openssl x509 -req -in "${CERTDIR}/${name}.csr" -CA "${CERTDIR}/ca.crt" -CAkey "${CERTDIR}/ca.key" \
    -CAcreateserial -days 3650 -extfile "${CERTDIR}/${name}.ext" -out "${CERTDIR}/${name}.crt" >/dev/null 2>&1
}
SVC_SAN="DNS:${RW_SVC},DNS:${RW_SVC}.${NAMESPACE}.svc,DNS:${RW_SVC}.${NAMESPACE}.svc.cluster.local"
SVC_SAN="${SVC_SAN},DNS:${RO_SVC},DNS:${RO_SVC}.${NAMESPACE}.svc,DNS:${RO_SVC}.${NAMESPACE}.svc.cluster.local"
SVC_SAN="${SVC_SAN},DNS:*.${HEADLESS}.${NAMESPACE}.svc.cluster.local,DNS:${FULLNAME}-pgpool,DNS:localhost,IP:127.0.0.1"
gen_ca
gen_cert server "${RW_SVC}" "${SVC_SAN}" "serverAuth,clientAuth"
gen_cert client "appuser"   ""           "clientAuth"
certs_ok=$([[ -s "${CERTDIR}/server.crt" && -s "${CERTDIR}/client.crt" ]] && echo ok || echo fail)
assert_eq "test certs generated (CA + server + client)" "ok" "${certs_ok}"

# --- namespace + cert Secrets ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
kubectl delete configmap "${FULLNAME}-primary" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
for s in postgresql-tls pgpool-tls pgpool-backend-tls client-tls; do
  kubectl delete secret "${s}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
done
mk_secret() { # mk_secret <name> <cert-base>
  kubectl create secret generic "$1" -n "${NAMESPACE}" \
    --from-file=tls.crt="${CERTDIR}/$2.crt" --from-file=tls.key="${CERTDIR}/$2.key" \
    --from-file=ca.crt="${CERTDIR}/ca.crt" >/dev/null
}
mk_secret postgresql-tls    server   # PostgreSQL server cert
mk_secret pgpool-tls        server   # PGPool frontend server cert (reuse)
mk_secret pgpool-backend-tls client  # PGPool -> PostgreSQL backend client cert
mk_secret client-tls        client   # for the test client pod
sleep 2

helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" -f "${SCRIPT_DIR}/values-tls.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# A throwaway client pod (normal writable fs) with the client cert mounted; psql needs a
# 0600 key owned by the running user, so copy + chmod out of the read-only Secret mount.
kubectl run tlsclient -n "${NAMESPACE}" --image=postgres:18.1-trixie --restart=Never \
  --overrides='{"spec":{"containers":[{"name":"tlsclient","image":"postgres:18.1-trixie","imagePullPolicy":"IfNotPresent","command":["sleep","3600"],"volumeMounts":[{"name":"c","mountPath":"/certs","readOnly":true}]}],"volumes":[{"name":"c","secret":{"secretName":"client-tls"}}]}}' >/dev/null 2>&1 || true
kubectl wait --for=condition=Ready pod/tlsclient -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1
kubectl exec -n "${NAMESPACE}" tlsclient -- bash -c 'cp /certs/* /tmp/ && chmod 600 /tmp/tls.key' >/dev/null 2>&1

# run_client <user> <pass> <sslmode> <cert:yes|no> <sql> -> stdout+stderr, rc preserved
run_client() {
  local user="$1" pass="$2" mode="$3" cert="$4" sql="$5"
  local conn="host=${RW_SVC} port=5432 dbname=testdb user=${user} sslmode=${mode} sslrootcert=/tmp/ca.crt connect_timeout=10"
  [ "${cert}" = "yes" ] && conn="${conn} sslcert=/tmp/tls.crt sslkey=/tmp/tls.key"
  kubectl exec -n "${NAMESPACE}" tlsclient -- bash -c "PGPASSWORD='${pass}' psql \"${conn}\" -tAc \"${sql}\"" 2>&1
}

# --- settle the primary/standby pair ---
POD0="${FULLNAME}-0"; POD1="${FULLNAME}-1"
ssl_on=$(pg_exec "${NAMESPACE}" "${POD0}" "SHOW ssl" "testuser" "testdb" 2>/dev/null || echo "")
[ -z "${ssl_on}" ] && ssl_on=$(pg_exec "${NAMESPACE}" "${POD1}" "SHOW ssl" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#110: PostgreSQL started with ssl = on" "on" "${ssl_on}"

echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "#110: agent elected a primary under TLS" "${PRIMARY:-none}" "${FULLNAME}"

# the chart-generated superuser (POSTGRES_USER) password, from the chart Secret
PGPW=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.password}' | base64 -d)

# a non-exempt app role for the clientcert=verify-ca catch-all. The cluster runs
# password_encryption=md5 (legacy compat); the chart migrates its MANAGED users to scram,
# but a test-created role must be scram explicitly or the scram-sha-256 pg_hba method
# rejects its md5 verifier.
pg_exec "${NAMESPACE}" "${PRIMARY}" "SET password_encryption='scram-sha-256'; DROP ROLE IF EXISTS appuser; CREATE ROLE appuser LOGIN PASSWORD 'apppass'" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "GRANT ALL ON DATABASE testdb TO appuser" "testuser" "testdb"

# --- require: non-TLS rejected; exempt superuser over TLS without a cert works ---
disable_out=$(run_client testuser "${PGPW}" disable no "SELECT 1" || true)
assert_eq "#110 require: sslmode=disable is rejected" "0" "$(printf '%s' "${disable_out}" | grep -c "^1$" || true)"
exempt_out=$(run_client testuser "${PGPW}" require no "SELECT 1" || true)
assert_eq "#110 exemption: superuser over TLS (no client cert) succeeds" "1" "$(printf '%s' "${exempt_out}" | tr -d '[:space:]')"

# --- mutual TLS: a non-exempt app user is rejected without a cert, accepted with one ---
nocert_out=$(run_client appuser apppass require no "SELECT 1" || true)
assert_eq "#110 mTLS: app user without a client cert is rejected" "0" "$(printf '%s' "${nocert_out}" | grep -c "^1$" || true)"
cert_out=$(run_client appuser apppass require yes "SELECT 1" || true)
assert_eq "#110 mTLS: app user WITH a client cert succeeds" "1" "$(printf '%s' "${cert_out}" | tr -d '[:space:]')"

# --- the client VERIFIES the server cert: verify-ca (chain) and verify-full (hostname).
#     sslmode=require above does NOT validate the server cert; these do. testuser is the
#     exempt superuser, so no client cert is needed -- this isolates server verification. ---
vca_out=$(run_client testuser "${PGPW}" verify-ca no "SELECT 1" || true)
assert_eq "#110 verify-ca: client validates the server cert chain (CA)" "1" "$(printf '%s' "${vca_out}" | tr -d '[:space:]')"
# verify-full also checks the hostname against the cert SANs; the RW Service name is a SAN.
vfull_out=$(run_client testuser "${PGPW}" verify-full no "SELECT 1" || true)
assert_eq "#110 verify-full: client validates server cert hostname (SAN matches RW Service)" "1" "$(printf '%s' "${vfull_out}" | tr -d '[:space:]')"

# --- internals keep working under TLS: replication streams ---
FV="tls-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS tls_t (id serial PRIMARY KEY, v text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO tls_t (v) VALUES ('${FV}')" "testuser" "testdb"
sleep 3
repl=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM tls_t WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#110: replication streams under TLS" "${FV}" "${repl}"

# --- exporter scrapes over TLS as the (exempt) monitoring user: pg_up=1. The exporter's
#     metrics/telemetry port is 9116 (the scrape DSN connects to PostgreSQL over TLS). ---
exp_pod=$(kubectl get pods -n "${NAMESPACE}" -o name 2>/dev/null | grep -i exporter | head -1)
pg_up=""
if [ -n "${exp_pod}" ]; then
  kubectl wait --for=condition=Ready "${exp_pod}" -n "${NAMESPACE}" --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do
    pg_up=$(kubectl exec -n "${NAMESPACE}" "${exp_pod}" -- sh -c "wget -qO- http://127.0.0.1:9116/metrics 2>/dev/null | grep '^pg_up '" 2>/dev/null || true)
    [[ "${pg_up}" == *"pg_up 1"* ]] && break
    sleep 6
  done
fi
assert_contains "#110: exporter reports pg_up=1 over TLS (monitoring user exempt)" "${pg_up}" "pg_up 1"

# --- PGPool serves clients over TLS and routes to the backend over TLS (frontend :9999) ---
pp_out=$(kubectl exec -n "${NAMESPACE}" tlsclient -- bash -c \
  "PGPASSWORD='${PGPW}' psql \"host=${FULLNAME}-pgpool port=9999 dbname=testdb user=testuser sslmode=require sslrootcert=/tmp/ca.crt connect_timeout=15\" -tAc 'SELECT 1'" 2>&1 || true)
assert_eq "#110: client reaches PostgreSQL through PGPool over TLS" "1" "$(printf '%s' "${pp_out}" | tr -d '[:space:]')"

# --- failover under TLS: delete the primary, the standby promotes, data survives ---
echo "  Deleting primary ${PRIMARY} (failover under TLS)..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
promoted=false; e=0
while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  [[ "${rec}" == "t" ]] && { promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; }
  sleep 3; e=$((e + 3))
done
assert_eq "#110: standby promoted on failover under TLS" "true" "${promoted}"
if [[ "${promoted}" == "true" ]]; then
  survived=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM tls_t WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#110: data survived TLS failover" "${FV}" "${survived}"
fi

# ======================================================================
# repmgrd mode: optional server TLS (ssl=on). require/mTLS is render-guarded
# off in repmgrd; this proves the server-TLS conf.d path works there too.
# ======================================================================
RELEASE2="pgtlsr"; NS2="${NAMESPACE}-repmgrd"
F2=$(resolve_fullname "${RELEASE2}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
kubectl create namespace "${NS2}" --dry-run=client -o yaml | kubectl apply -f -
kubectl delete secret postgresql-tls -n "${NS2}" --ignore-not-found >/dev/null 2>&1 || true
kubectl create secret generic postgresql-tls -n "${NS2}" \
  --from-file=tls.crt="${CERTDIR}/server.crt" --from-file=tls.key="${CERTDIR}/server.key" \
  --from-file=ca.crt="${CERTDIR}/ca.crt" >/dev/null
helm upgrade --install "${RELEASE2}" "${CHART_DIR}" -n "${NS2}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.failoverMode=repmgrd \
  --set repmgr.image.tag=trixie-5.5.0-26 \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=postgresql-tls \
  --wait --timeout 10m
wait_for_pods_ready "${NS2}" "app.kubernetes.io/component=postgresql" 2 600
ssl_r=$(pg_exec "${NS2}" "${F2}-0" "SHOW ssl" "testuser" "testdb" 2>/dev/null || echo "")
[ "${ssl_r}" != "on" ] && ssl_r=$(pg_exec "${NS2}" "${F2}-1" "SHOW ssl" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#110 repmgrd: optional server TLS works (ssl = on)" "on" "${ssl_r}"

# ======================================================================
# Standalone (repmgr off, replicaCount 0): server TLS over the conf.d include with NO
# repmgr/agent. Exercises the volumes:/annotations: gate fix live -- the cert volume must
# render alongside its mount and the single postgres pod must start with ssl=on.
# ======================================================================
RELEASE3="pgtlss"; NS3="${NAMESPACE}-standalone"
F3=$(resolve_fullname "${RELEASE3}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
kubectl create namespace "${NS3}" --dry-run=client -o yaml | kubectl apply -f -
kubectl delete secret postgresql-tls -n "${NS3}" --ignore-not-found >/dev/null 2>&1 || true
kubectl create secret generic postgresql-tls -n "${NS3}" \
  --from-file=tls.crt="${CERTDIR}/server.crt" --from-file=tls.key="${CERTDIR}/server.key" \
  --from-file=ca.crt="${CERTDIR}/ca.crt" >/dev/null
helm upgrade --install "${RELEASE3}" "${CHART_DIR}" -n "${NS3}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set repmgr.enabled=false --set postgresql.replicaCount=0 \
  --set postgresql.tls.enabled=true --set postgresql.tls.existingSecret=postgresql-tls \
  --wait --timeout 8m
wait_for_pods_ready "${NS3}" "app.kubernetes.io/component=postgresql" 1 300
ssl_s=$(pg_exec "${NS3}" "${F3}-0" "SHOW ssl" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#110 standalone: server TLS works with no repmgr (ssl = on)" "on" "${ssl_s}"

# Coverage boundary (logged, not silently skipped): pgpool.tls.backendClientCert is
# render-verified (PGSSLCERT/PGSSLKEY env + staged cert) but not exercised end-to-end
# here -- routing a NON-exempt user through PGPool would need that user in pgpool's
# pool_passwd, which the chart generates only for its managed (exempt) users.
echo "  NOTE: pgpool backendClientCert mTLS passthrough is render-covered only (see comment)."

end_suite
print_summary
