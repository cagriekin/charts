#!/bin/bash
# Regression for #108: existingSecret passwords are arbitrary bytes. The
# password below carries every class that broke the old plumbing: sed
# replacement metacharacters (/ & \), URI-reserved characters (@ : ? # %)
# and quoting characters (' ").
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-special-chars}"
RELEASE="${RELEASE:-pg-special}"
SPECIAL_PASSWORD='p@/s&\:?#%'\''"w0rd'
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-special-chars.yaml")

begin_suite "Special-character credentials (existingSecret, pgpool + exporter)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# monitoring-password is part of the BYO secret, not an afterthought (#298, found by the first
# live run). prometheusExporter is enabled in this fixture, and postgresql.existingSecret's
# monitoringPasswordKey DEFAULTS to `monitoring-password` -- so the helper's `required` guard is
# satisfied by the default and the render is clean, while the exporter's init container then asks
# this Secret for a key it does not have. That fails at APPLY time
# (Init:CreateContainerConfigError), so `helm --wait` sat for its full timeout and the suite died
# before its first assertion. It gets the special-character password too, which is the point of
# the suite: the monitoring credential goes through the same substitution as the others.
kubectl create secret generic pg-special-creds -n "${NAMESPACE}" \
  --from-literal=username=testuser \
  --from-literal=password="${SPECIAL_PASSWORD}" \
  --from-literal=database=testdb \
  --from-literal=monitoring-password="${SPECIAL_PASSWORD}" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-special-chars.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-pgpool" 300
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-postgres-exporter" 300

POD="${FULLNAME}-0"

# Direct auth over TCP via the primary service: connections from a pod IP
# match the scram rule, unlike 127.0.0.1 which initdb trusts. env(1)
# passes the password as raw argv, no shell quoting layer.
direct=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
  env PGPASSWORD="${SPECIAL_PASSWORD}" psql -h "${FULLNAME}.${NAMESPACE}.svc.cluster.local" \
  -U testuser -d testdb -t -A -c "SELECT 1" 2>/dev/null)
assert_eq "direct psql auth with special-char password" "1" "${direct}"

# Through pgpool: validates pool_passwd splice (frontend auth) and the
# quote-doubled pgpool.conf credentials (health checks, backend auth)
pgpool_svc="${FULLNAME}-pgpool.${NAMESPACE}.svc.cluster.local"
via_pgpool=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
  env PGPASSWORD="${SPECIAL_PASSWORD}" psql -h "${pgpool_svc}" -p 9999 \
  -U testuser -d testdb -t -A -c "SELECT 1" 2>/dev/null)
assert_eq "query through pgpool with special-char password" "1" "${via_pgpool}"

# Exporter scrape: validates the percent-encoded DSN file and the
# quote-doubled postgres_exporter.yml
# Scraped from the already-running pod over bash's /dev/tcp, not from a throwaway
# `kubectl run --image=busybox` (#298, found by the first live run; same fix as test-full.sh).
# That pod needs an image the cluster may not be able to pull, and the `2>/dev/null || echo ""`
# turned a failed fetch into an empty string -- indistinguishable here from an exporter that
# cannot authenticate, which is exactly what this suite is trying to detect.
exporter_svc="${FULLNAME}-postgres-exporter"
pg_up=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- bash -c \
  "exec 3<>/dev/tcp/${exporter_svc}/9116; printf 'GET /metrics HTTP/1.0\r\n\r\n' >&3; grep '^pg_up' <&3" 2>/dev/null || echo "")
if [ -z "${pg_up}" ]; then
  fail "exporter scrape returned nothing" "could not reach ${exporter_svc}:9116 from ${POD}; the assertion below would prove nothing"
fi
assert_contains "exporter connects with encoded DSN (pg_up 1)" "${pg_up}" "pg_up 1"

end_suite
print_summary
