#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-full}"
RELEASE="${RELEASE:-pg-full}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-full-test.yaml")

begin_suite "Full Install (repmgr + pgpool + prometheus exporter)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-full-test.yaml" \
  --wait --timeout 10m

POD_0="${FULLNAME}-0"
POD_1="${FULLNAME}-1"
POD_2="${FULLNAME}-2"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600

# Test: all 3 pods running
for pod in "${POD_0}" "${POD_1}" "${POD_2}"; do
  pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{.status.phase}')
  assert_eq "pod ${pod} is Running" "Running" "${pod_phase}"
done

# Test: exactly one primary, and the other two are replicas.
#
# DISCOVERED, not assumed to be pod-0 (#290 review). Agent mode renders
# podManagementPolicy: Parallel and the initial primary is whoever wins the lease race, so
# asserting pod-0 flaked whenever pod-1 or pod-2 won. Everything below writes through
# ${PRIMARY} for the same reason -- a write to a standby fails read-only.
PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" 3 testuser testdb)
# NOT assert_not_eq with a literal "": that helper fails when EITHER operand is empty (#279,
# so a vacuous uniqueness check cannot pass silently), which made this fail on every run.
if [ -z "${PRIMARY}" ]; then
  fail "a primary is discoverable" "discover_primary returned nothing"
  end_suite
  print_summary
  exit 1
fi
pass "a primary is discoverable (${PRIMARY})"

replicas=0
for pod in "${POD_0}" "${POD_1}" "${POD_2}"; do
  [ "${pod}" = "${PRIMARY}" ] && continue
  is_replica=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT pg_is_in_recovery()" "testuser" "testdb")
  assert_eq "${pod} is replica" "t" "${is_replica}"
  [ "${is_replica}" = "t" ] && replicas=$((replicas + 1))
done
assert_eq "exactly two replicas alongside the primary" "2" "${replicas}"

# Test: replication across all nodes
REPL_VALUE="full-replicated-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS full_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO full_test (value) VALUES ('${REPL_VALUE}')" "testuser" "testdb"

sleep 3

for pod in "${POD_0}" "${POD_1}" "${POD_2}"; do
  [ "${pod}" = "${PRIMARY}" ] && continue
  val=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT value FROM full_test WHERE value='${REPL_VALUE}'" "testuser" "testdb")
  assert_eq "data replicated to ${pod}" "${REPL_VALUE}" "${val}"
done

# Test: the primary sees both standbys streaming (retry -- attachment lags pod readiness).
#
# This read repmgr.nodes until #290. That table can no longer exist: #288 stopped creating the
# extension under native, #294 made native the only mechanism, and #290 removed the package
# from the image entirely -- so the assertion was heading for a guaranteed failure the moment
# the chart's image tag was bumped. pg_stat_replication is the equivalent and a stronger one:
# a repmgr.nodes row proved only that a node had registered, whereas this proves it is actually
# replicating.
streaming="0"
for i in $(seq 1 12); do
  streaming=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" "repmgr" "repmgr" 2>/dev/null | xargs || echo "0")
  if [[ "${streaming}" == "2" ]]; then
    break
  fi
  sleep 5
done
assert_eq "the primary sees both standbys streaming" "2" "${streaming}"

# --- PGPool tests ---
echo ""
echo "  -- PGPool tests --"

wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-pgpool" 300

# Test: pgpool pod is running
pgpool_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=pgpool" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
pgpool_phase=$(kubectl get pod -n "${NAMESPACE}" "${pgpool_pod}" -o jsonpath='{.status.phase}')
assert_eq "pgpool pod is Running" "Running" "${pgpool_phase}"

# Test: pgpool service exists
pgpool_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-pgpool" -o jsonpath='{.spec.ports[?(@.name=="pgpool")].port}')
assert_eq "pgpool service port is 9999" "9999" "${pgpool_port}"

# PGPool uses md5 auth, so we need the password from the secret
PG_PASSWORD=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.data.password}' | base64 -d)

# Test: connect through pgpool
pgpool_svc="${FULLNAME}-pgpool.${NAMESPACE}.svc.cluster.local"
pgpool_result=$(kubectl exec -n "${NAMESPACE}" "${PRIMARY}" -c postgresql -- \
  bash -c "PGPASSWORD='${PG_PASSWORD}' psql -h '${pgpool_svc}' -p 9999 -U testuser -d testdb -t -A -c 'SELECT 1'" 2>/dev/null)
assert_eq "can query through pgpool" "1" "${pgpool_result}"

# Test: write through pgpool reaches primary
PGPOOL_VALUE="via-pgpool-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${PRIMARY}" -c postgresql -- \
  bash -c "PGPASSWORD='${PG_PASSWORD}' psql -h '${pgpool_svc}' -p 9999 -U testuser -d testdb -c \"INSERT INTO full_test (value) VALUES ('${PGPOOL_VALUE}')\"" 2>/dev/null
pgpool_write_val=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT value FROM full_test WHERE value='${PGPOOL_VALUE}'" "testuser" "testdb")
assert_eq "write through pgpool persisted on primary" "${PGPOOL_VALUE}" "${pgpool_write_val}"

# Test: pgpool metrics sidecar
pgpool_containers=$(kubectl get pod -n "${NAMESPACE}" "${pgpool_pod}" -o jsonpath='{.spec.containers[*].name}')
assert_contains "pgpool has metrics sidecar" "${pgpool_containers}" "pgpool-exporter"

# Test: the PCP admin port (9898) is NOT exposed on the Service by default (#118).
# It is opt-in via pgpool.service.exposePcp; the pcp_* tools still reach it on the
# container port (localhost) inside the pod, validated next.
pcp_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-pgpool" -o jsonpath='{.spec.ports[?(@.name=="pcp")].port}')
assert_eq "pgpool PCP port not exposed on Service by default (#118)" "" "${pcp_port}"

# Test: PCP admin auth works end-to-end (#130). pcp.conf must hash the admin
# password as md5; under the old sha256 every pcp_* command failed auth. pgpool's
# pcp tools take the password from a .pcppass file (PCPPASSFILE), not PCPPASSWORD,
# so feed it that way and assert pcp_node_count returns the backend count rather
# than an auth error.
#
# The count is 2, not one-per-pod: the agent fronts the read/write split, so pgpool's
# backends are the RW Service (<fullname>, ALWAYS_PRIMARY) and the RO Service
# (<fullname>-readonly) -- two regardless of replicaCount. It was 3 here while this
# fixture pinned failoverMode: repmgrd, which listed one backend per pod (#286). The trailing `|| pcp_count=auth-failed` keeps an auth
# failure a clean assertion FAIL instead of a set -e abort of the whole suite.
pcp_user=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-pgpool-admin" -o jsonpath='{.data.username}' | base64 -d)
pcp_pw=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-pgpool-admin" -o jsonpath='{.data.password}' | base64 -d)
pcp_count=$(kubectl exec -n "${NAMESPACE}" "${pgpool_pod}" -c pgpool -- sh -c "
  printf '%s\n' 'localhost:9898:${pcp_user}:${pcp_pw}' > /tmp/.pcppass
  chmod 600 /tmp/.pcppass
  PCPPASSFILE=/tmp/.pcppass pcp_node_count -h localhost -p 9898 -U '${pcp_user}' -w
" 2>/dev/null | tail -1 | tr -d '[:space:]') || pcp_count="auth-failed"
assert_eq "pcp_node_count authenticates and returns backend count (#130)" "2" "${pcp_count}"

# --- Prometheus exporter tests ---
echo ""
echo "  -- Prometheus Exporter tests --"

wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME}-postgres-exporter" 300

# Test: exporter pod is running
exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=postgres-exporter" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "prometheus exporter pod is Running" "Running" "${exporter_phase}"

# Test: exporter service exists
exporter_port=$(kubectl get svc -n "${NAMESPACE}" "${FULLNAME}-postgres-exporter" -o jsonpath='{.spec.ports[0].port}')
assert_eq "exporter service port is 9116" "9116" "${exporter_port}"

# Test: exporter scrapes succeed as the least-privilege monitoring user (#28).
# pg_up=1 proves the exporter connected and ran a query as the pg_monitor role --
# a plain '^pg_' match would still pass with pg_up 0 (auth failed). Grepping only
# pg_up lines, then asserting none carry a " 0" value, confirms every target is up.
exporter_svc="${FULLNAME}-postgres-exporter.${NAMESPACE}.svc.cluster.local"
metrics_output=$(kubectl run "metrics-check-$(date +%s)" -n "${NAMESPACE}" --rm -i --restart=Never \
  --image=busybox:1.37 -- wget -qO- "http://${exporter_svc}:9116/metrics" 2>/dev/null \
  | grep '^pg_up' || echo "")
assert_contains "exporter returns pg_up metric" "${metrics_output}" "pg_up"
assert_not_contains "exporter scrapes succeed as the monitoring user, no pg_up 0 (#28)" "${metrics_output}" " 0"

end_suite
print_summary
