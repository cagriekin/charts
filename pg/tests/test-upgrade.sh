#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-upgrade}"
RELEASE="${RELEASE:-pg-upgrade}"
FULLNAME_FROM=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-upgrade-from.yaml")
FULLNAME_TO=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-upgrade-to.yaml")

begin_suite "Upgrade (repmgr 2-node -> 3-node with pgpool + exporter)"

# Start from a clean namespace. Previous runs on a long-lived cluster leave
# behind a release whose Service selector is owned by the service-updater's
# kubectl-patch field manager (helm v4 server-side apply conflicts on
# .spec.selector) and PVCs whose pvc-protection finalizers race a fresh
# install while the old pods drain.
kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout=5m
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# Step 1: Install with repmgr (2 replicas, persistence enabled)
echo "  Step 1: Installing repmgr (2 nodes)..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-upgrade-from.yaml" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

POD_0="${FULLNAME_FROM}-0"
result=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT 1" "testuser" "testdb")
assert_eq "repmgr install works" "1" "${result}"

# Write data before upgrade
UPGRADE_VALUE="pre-upgrade-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD_0}" "CREATE TABLE IF NOT EXISTS upgrade_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD_0}" "INSERT INTO upgrade_test (value) VALUES ('${UPGRADE_VALUE}')" "testuser" "testdb"

# Step 2: Upgrade to full (3 replicas + pgpool + exporter, same persistence)
echo ""
echo "  Step 2: Upgrading to full (3 nodes + pgpool + exporter)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-upgrade-to.yaml" \
  --wait --timeout 10m

POD_0="${FULLNAME_TO}-0"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_TO}-pgpool" 300
wait_for_deployment_ready "${NAMESPACE}" "${FULLNAME_TO}-postgres-exporter" 300

# Test: all 3 pg pods running AND READY.
#
# Readiness, not just phase: agent-mode readiness is replication-aware (#186), so a standby
# that is Running but not streaming reads ready=false. Asserting only .status.phase let a
# permanently-broken standby pass this suite -- pod-2 sat Running/ready=false, unable to
# replicate, and the suite reported success (#297).
# The postgresql container is selected BY NAME, not by index (#298 review). This step's upgrade
# adds the exporter sidecar, so index 0 is no longer guaranteed to be postgresql -- and a sidecar
# with no readiness probe reports ready=true, so an index that resolved to it would report Ready
# while the replication-aware postgresql probe was still failing. That is the same false pass
# #297 describes, reintroduced by the assertion meant to catch it.
for i in 0 1 2; do
  phase=$(kubectl get pod -n "${NAMESPACE}" "${FULLNAME_TO}-${i}" -o jsonpath='{.status.phase}')
  assert_eq "after upgrade: pod-${i} is Running" "Running" "${phase}"
  ready=$(kubectl get pod -n "${NAMESPACE}" "${FULLNAME_TO}-${i}" -o jsonpath='{.status.containerStatuses[?(@.name=="postgresql")].ready}')
  assert_eq "after upgrade: pod-${i} is Ready (replication-aware, #186)" "true" "${ready}"
done

# Every node must agree on the topology: a node whose repmgr.nodes copy predates its own
# registration cannot `repmgr standby follow` and never streams (#297). Assert each node
# is actually replicating. The original check read each node's own repmgr.nodes row; #294 removed
# repmgr.nodes, and gating it on chart_mechanism left it permanently skipped -- silently undoing
# the coverage restored for exactly this race. The native equivalent asks the question directly of
# the primary's live connection list: every standby must be present AND streaming, which is
# strictly stronger than "a row exists for itself".
NEW_PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME_TO}" 3 repmgr repmgr)
# See test-full.sh: assert_not_eq fails on an empty operand by design, so a literal "" here
# made the assertion fail unconditionally.
if [ -z "${NEW_PRIMARY}" ]; then
  fail "after upgrade: a primary is discoverable" "discover_primary returned nothing"
else
  pass "after upgrade: a primary is discoverable (${NEW_PRIMARY})"
fi
if [ -n "${NEW_PRIMARY}" ]; then
  streaming=""; s=0
  while [ "${s}" -lt 120 ]; do
    streaming=$(pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" \
      "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" repmgr repmgr 2>/dev/null | xargs || echo "")
    [ "${streaming}" = "2" ] && break
    sleep 5; s=$((s + 5))
  done
  assert_eq "after upgrade: the primary sees both standbys streaming (#297)" "2" "${streaming}"
  # And each standby individually, so "2 streaming" cannot be satisfied by one pod twice.
  for i in 0 1 2; do
    pod="${FULLNAME_TO}-${i}"
    [ "${pod}" = "${NEW_PRIMARY}" ] && continue
    seen=$(pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" \
      "SELECT count(*) FROM pg_stat_replication WHERE application_name='${pod}' AND state='streaming'" repmgr repmgr 2>/dev/null | xargs || echo "")
    assert_eq "after upgrade: ${pod} is streaming from the primary (#297)" "1" "${seen}"
  done
fi

# Test: pgpool running
pgpool_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=pgpool" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
pgpool_phase=$(kubectl get pod -n "${NAMESPACE}" "${pgpool_pod}" -o jsonpath='{.status.phase}')
assert_eq "pgpool pod is Running after upgrade" "Running" "${pgpool_phase}"

# Test: exporter running
exporter_pod=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=postgres-exporter" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
exporter_phase=$(kubectl get pod -n "${NAMESPACE}" "${exporter_pod}" -o jsonpath='{.status.phase}')
assert_eq "exporter pod is Running after upgrade" "Running" "${exporter_phase}"

# Test: data written before upgrade survives (persistence enabled)
survived=$(pg_exec "${NAMESPACE}" "${POD_0}" "SELECT value FROM upgrade_test WHERE value='${UPGRADE_VALUE}'" "testuser" "testdb")
assert_eq "data survives upgrade" "${UPGRADE_VALUE}" "${survived}"

end_suite
print_summary
