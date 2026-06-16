#!/bin/bash
# Migration test (Part F6): repmgrd -> agent via the documented --cascade=orphan
# StatefulSet recreate -- the exact path the 1.0.0 default-flip forces on every
# consumer. Install repmgrd, write data, orphan-delete the STS (podManagementPolicy
# is immutable), helm upgrade into agent mode, and assert the pods re-adopt + roll
# to agent, the leadership Lease appears, data survives, and failover then works.
# Standalone/opt-in (long-running): run `make -C pg test-migrate-agent`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-migrate}"
RELEASE="${RELEASE:-pgmig}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-repmgr.yaml")
LEASE="${FULLNAME}-leader"
POD0="${FULLNAME}-0"
POD1="${FULLNAME}-1"

begin_suite "Migration: repmgrd -> agent (--cascade=orphan recreate, Part F6)"

kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout=5m
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# 1. install the DEFAULT (repmgrd) mode
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" -f "${SCRIPT_DIR}/values-repmgr.yaml" --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

before=$(kubectl get pod -n "${NAMESPACE}" "${POD0}" -o jsonpath='{.spec.containers[*].name}')
assert_contains "before: repmgrd sidecar present (repmgrd mode)" "${before}" "repmgrd"
lease_before=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_eq "before: no leader Lease (repmgrd mode)" "" "${lease_before}"

# data on the repmgrd primary (ordinal-0 under OrderedReady)
MV="migrate-$(date +%s)"
pg_exec "${NAMESPACE}" "${POD0}" "CREATE TABLE IF NOT EXISTS migrate_test (id serial PRIMARY KEY, value text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${POD0}" "INSERT INTO migrate_test (value) VALUES ('${MV}')" "testuser" "testdb"
sleep 3

# 2. THE MIGRATION: podManagementPolicy is immutable, so orphan-delete the STS
# (pods + PVCs keep running), then flip failoverMode -- helm recreates the STS as
# Parallel and adopts the orphaned pods, rolling them to agent mode.
echo "  Orphan-deleting the StatefulSet (keeps pods + PVCs)..."
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --cascade=orphan
# A real 1.0.0 migration also bumps the repmgr image to an agent-capable tag (the
# pre-agent image has no `agent` entrypoint arm). values-repmgr.yaml installs the
# released image; bump it to the agent-capable tag as part of the failoverMode flip.
AGENT_IMAGE_TAG="${AGENT_IMAGE_TAG:-trixie-5.5.0-16}"
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" -f "${SCRIPT_DIR}/values-repmgr.yaml" \
  --set repmgr.failoverMode=agent --set "repmgr.image.tag=${AGENT_IMAGE_TAG}" --wait --timeout 10m

# 3. wait for both pods to roll to agent mode (no repmgrd sidecar) AND a single
# primary == lease holder to settle (this also proves the roll produced no second
# writer -- the migration guard).
echo "  Waiting for the pods to roll to agent mode + a primary==holder (up to 480s)..."
PRIMARY=""; STANDBY=""; HOLDER=""; m=0
while [[ ${m} -lt 480 ]]; do
  c0=$(kubectl get pod -n "${NAMESPACE}" "${POD0}" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null || echo "")
  c1=$(kubectl get pod -n "${NAMESPACE}" "${POD1}" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null || echo "")
  if [[ -z "${c0}" || -z "${c1}" || "${c0}" == *repmgrd* || "${c1}" == *repmgrd* ]]; then sleep 10; m=$((m + 10)); continue; fi
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  PRIMARY=""; STANDBY=""
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; fi
  if [[ -n "${PRIMARY}" && "${HOLDER}" == "${PRIMARY}" ]]; then echo "  migrated after ${m}s: primary=${PRIMARY} holder=${HOLDER}"; break; fi
  sleep 10; m=$((m + 10))
done

after=$(kubectl get pod -n "${NAMESPACE}" "${POD0}" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null || echo "")
assert_contains "after: postgresql container present" "${after}" "postgresql"
assert_not_contains "after: repmgrd sidecar removed (agent mode)" "${after}" "repmgrd"
lease_after=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_contains "after: leader Lease created (agent mode)" "${lease_after}" "lease"

if [[ -n "${PRIMARY}" && -n "${STANDBY}" ]]; then
  assert_eq "after: lease holder is the primary" "${PRIMARY}" "${HOLDER}"
  survived=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT value FROM migrate_test WHERE value='${MV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "data survives the repmgrd->agent migration" "${MV}" "${survived}"

  echo "  Post-migration failover: deleting primary ${PRIMARY}..."
  kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
  promoted=false; e=0
  while [[ ${e} -lt 120 ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "t" ]]; then promoted=true; echo "  post-migration failover after ${e}s (new primary = ${STANDBY})"; break; fi
    sleep 5; e=$((e + 5))
  done
  assert_eq "post-migration agent failover promotes the standby" "true" "${promoted}"
else
  skip "after: lease holder is the primary (migration did not settle)"
  skip "data survives the repmgrd->agent migration (migration did not settle)"
  skip "post-migration agent failover promotes the standby (migration did not settle)"
fi

end_suite
print_summary
