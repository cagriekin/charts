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

kubectl create secret generic pg-special-creds -n "${NAMESPACE}" \
  --from-literal=username=testuser \
  --from-literal=password="${SPECIAL_PASSWORD}" \
  --from-literal=database=testdb \
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
exporter_svc="${FULLNAME}-postgres-exporter.${NAMESPACE}.svc.cluster.local"
pg_up=$(kubectl run "special-metrics-$(date +%s)" -n "${NAMESPACE}" --rm -i --restart=Never \
  --image=busybox:1.37 -- wget -qO- "http://${exporter_svc}:9116/metrics" 2>/dev/null \
  | grep '^pg_up' || echo "")
assert_contains "exporter connects with encoded DSN (pg_up 1)" "${pg_up}" "pg_up 1"

end_suite
print_summary
