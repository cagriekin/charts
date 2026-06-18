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

# Test: primary identification
is_primary=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "pod-0 is primary" "t" "${is_primary}"

for pod in "${POD_1}" "${POD_2}"; do
  is_replica=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT pg_is_in_recovery()" "testuser" "testdb")
  assert_eq "${pod} is replica" "t" "${is_replica}"
done

# Test: replication across all nodes
REPL_VALUE="full-replicated-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD_0}" "CREATE TABLE IF NOT EXISTS full_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_0}" "INSERT INTO full_test (value) VALUES ('${REPL_VALUE}')" "testuser" "testdb"

sleep 3

for pod in "${POD_1}" "${POD_2}"; do
  val=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT value FROM full_test WHERE value='${REPL_VALUE}'" "testuser" "testdb")
  assert_eq "data replicated to ${pod}" "${REPL_VALUE}" "${val}"
done

# Test: repmgr sees all 3 nodes (retry -- node registration may lag pod readiness)
node_count="0"
for i in $(seq 1 12); do
  node_count=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT count(*) FROM repmgr.nodes" "repmgr" "repmgr")
  if [[ "${node_count}" == "3" ]]; then
    break
  fi
  sleep 5
done
assert_eq "repmgr sees 3 nodes" "3" "${node_count}"

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
pgpool_result=$(kubectl exec -n "${NAMESPACE}" "${POD_0}" -c postgresql -- \
  bash -c "PGPASSWORD='${PG_PASSWORD}' psql -h '${pgpool_svc}' -p 9999 -U testuser -d testdb -t -A -c 'SELECT 1'" 2>/dev/null)
assert_eq "can query through pgpool" "1" "${pgpool_result}"

# Test: write through pgpool reaches primary
PGPOOL_VALUE="via-pgpool-$(date +%s)"
kubectl exec -n "${NAMESPACE}" "${POD_0}" -c postgresql -- \
  bash -c "PGPASSWORD='${PG_PASSWORD}' psql -h '${pgpool_svc}' -p 9999 -U testuser -d testdb -c \"INSERT INTO full_test (value) VALUES ('${PGPOOL_VALUE}')\"" 2>/dev/null
pgpool_write_val=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT value FROM full_test WHERE value='${PGPOOL_VALUE}'" "testuser" "testdb")
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
# so feed it that way and assert pcp_node_count returns the backend count (3)
# rather than an auth error. The trailing `|| pcp_count=auth-failed` keeps an auth
# failure a clean assertion FAIL instead of a set -e abort of the whole suite.
pcp_user=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-pgpool-admin" -o jsonpath='{.data.username}' | base64 -d)
pcp_pw=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}-pgpool-admin" -o jsonpath='{.data.password}' | base64 -d)
pcp_count=$(kubectl exec -n "${NAMESPACE}" "${pgpool_pod}" -c pgpool -- sh -c "
  printf '%s\n' 'localhost:9898:${pcp_user}:${pcp_pw}' > /tmp/.pcppass
  chmod 600 /tmp/.pcppass
  PCPPASSFILE=/tmp/.pcppass pcp_node_count -h localhost -p 9898 -U '${pcp_user}' -w
" 2>/dev/null | tail -1 | tr -d '[:space:]') || pcp_count="auth-failed"
assert_eq "pcp_node_count authenticates and returns backend count (#130)" "3" "${pcp_count}"

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

# Test: exporter returns metrics (use a temp pod to avoid curl dependency)
exporter_svc="${FULLNAME}-postgres-exporter.${NAMESPACE}.svc.cluster.local"
metrics_output=$(kubectl run "metrics-check-$(date +%s)" -n "${NAMESPACE}" --rm -i --restart=Never \
  --image=busybox:1.37 -- wget -qO- "http://${exporter_svc}:9116/metrics" 2>/dev/null \
  | grep -m1 '^pg_' || echo "")
assert_contains "exporter returns pg metrics" "${metrics_output}" "pg_"

end_suite
print_summary
