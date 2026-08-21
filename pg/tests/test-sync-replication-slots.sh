#!/bin/bash
# Live logical-replication-slot-sync test (#308). Agent mode, postgresql.walLevel:
# logical, repmgr.agent.syncReplicationSlots: true, pgbackrest OFF (proves wal_level no
# longer depends on pgbackrest.enabled -- an earlier revision of this feature silently
# did nothing without it). Proves, against a real cluster:
#   1. Fresh install: wal_level = logical, sync_replication_slots = on, dbname present
#      in every standby's primary_conninfo, and synchronized_standby_slots on the
#      primary already names both standbys' physical slots.
#   2. Forced failover: the new primary re-reconciles synchronized_standby_slots to its
#      live standby, and the rejoined ex-primary (now a standby) gets dbname patched
#      into its fresh primary_conninfo via the rejoin path, not just clone/follow.
#   3. Scale-down: the departed standby's slot is dropped from synchronized_standby_slots
#      -- no stale entry naming a slot that no longer exists.
# Agent mode. OPT-IN / standalone: `make -C pg test-sync-replication-slots`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-sync-slots}"
RELEASE="${RELEASE:-pgsyncslots}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"

begin_suite "Logical replication slot sync (#308)"

# --- install: 1 primary + 2 standbys, walLevel=logical, syncReplicationSlots=true ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.replicaCount=2 \
  --set postgresql.walLevel=logical \
  --set repmgr.agent.syncReplicationSlots=true \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 700

echo "  Waiting for a primary + lease holder to settle (up to 120s)..."
PRIMARY=""; s=0
while [[ ${s} -lt 120 ]]; do
  h=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ -n "${h}" ]]; then
    rw=$(pg_exec "${NAMESPACE}" "${h}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${rw}" == "t" ]] && { PRIMARY="${h}"; break; }
  fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a read-write primary" "${PRIMARY:-none}" "${FULLNAME}"

ALL_PODS=("${FULLNAME}-0" "${FULLNAME}-1" "${FULLNAME}-2")
STANDBYS=()
for p in "${ALL_PODS[@]}"; do
  [[ "$p" != "${PRIMARY}" ]] && STANDBYS+=("$p")
done

# --- 1a: wal_level, sync_replication_slots (independent of pgbackrest, which is off) ---
wal_level=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW wal_level" "testuser" "testdb")
assert_eq "primary: wal_level = logical (pgbackrest is OFF in this fixture)" "logical" "${wal_level}"
sync_worker=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW sync_replication_slots" "testuser" "testdb")
assert_eq "primary: sync_replication_slots = on" "on" "${sync_worker}"

# --- 1b: dbname present in every standby's primary_conninfo (patched post-clone) ---
for standby in "${STANDBYS[@]}"; do
  conninfo=$(kubectl exec -n "${NAMESPACE}" "${standby}" -c postgresql -- \
    sh -c "grep primary_conninfo /var/lib/postgresql/data/pgdata/postgresql.auto.conf" 2>/dev/null || echo "")
  assert_contains "${standby}: primary_conninfo carries dbname after initial clone" "${conninfo}" "dbname="
done

# --- 1c: synchronized_standby_slots already names both standbys' physical slots ---
echo "  Waiting for synchronized_standby_slots to include both standbys (up to 60s)..."
want_count=${#STANDBYS[@]}
s=0; slots=""
while [[ ${s} -lt 60 ]]; do
  slots=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW synchronized_standby_slots" "testuser" "testdb" 2>/dev/null || echo "")
  got=$(echo "${slots}" | tr ',' '\n' | grep -c "repmgr_slot_" || true)
  [[ "${got}" -ge "${want_count}" ]] && break
  sleep 5; s=$((s + 5))
done
assert_eq "primary: synchronized_standby_slots names both standby slots" "${want_count}" "$(echo "${slots}" | tr ',' '\n' | grep -c "repmgr_slot_" || true)"

# --- 2: forced failover -- delete the primary, wait for a standby to promote ---
OLD_PRIMARY="${PRIMARY}"
echo "  Deleting primary ${OLD_PRIMARY} (graceful SIGTERM handoff)..."
kubectl delete pod "${OLD_PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true

echo "  Waiting for a new primary + lease holder to settle (up to 120s)..."
NEW_PRIMARY=""; s=0
while [[ ${s} -lt 120 ]]; do
  h=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ -n "${h}" && "${h}" != "${OLD_PRIMARY}" ]]; then
    rw=$(pg_exec "${NAMESPACE}" "${h}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${rw}" == "t" ]] && { NEW_PRIMARY="${h}"; break; }
  fi
  sleep 5; s=$((s + 5))
done
assert_contains "a standby was promoted to primary" "${NEW_PRIMARY:-none}" "${FULLNAME}"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 300

# --- 2a: the new primary re-reconciles synchronized_standby_slots to its live standby ---
echo "  Waiting for the new primary to reconcile synchronized_standby_slots (up to 60s)..."
s=0; new_slots=""
while [[ ${s} -lt 60 ]]; do
  new_slots=$(pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" "SHOW synchronized_standby_slots" "testuser" "testdb" 2>/dev/null || echo "")
  [[ -n "${new_slots}" ]] && break
  sleep 5; s=$((s + 5))
done
assert_gt "new primary: synchronized_standby_slots is non-empty after failover" "$(echo "${new_slots}" | tr ',' '\n' | grep -c "repmgr_slot_" || true)" "0"

# --- 2b: the rejoined ex-primary (now a standby) has dbname in its fresh primary_conninfo ---
echo "  Waiting for the rejoined ex-primary to become a standby again (up to 120s)..."
s=0; rejoined=false
while [[ ${s} -lt 120 ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${OLD_PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  [[ "${rec}" == "t" ]] && { rejoined=true; break; }
  sleep 5; s=$((s + 5))
done
assert_eq "ex-primary rejoined as a standby" "true" "${rejoined}"
rejoin_conninfo=$(kubectl exec -n "${NAMESPACE}" "${OLD_PRIMARY}" -c postgresql -- \
  sh -c "grep primary_conninfo /var/lib/postgresql/data/pgdata/postgresql.auto.conf" 2>/dev/null || echo "")
assert_contains "rejoined ex-primary: primary_conninfo carries dbname" "${rejoin_conninfo}" "dbname="

# --- 3: scale down to 1 standby -- the departed standby's slot must be dropped ---
echo "  Scaling down to 1 standby..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.replicaCount=1 \
  --set postgresql.walLevel=logical \
  --set repmgr.agent.syncReplicationSlots=true \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 300

# The remaining standby's slot, computed from the pod-name->node_id convention
# (node_id = 1000 + ordinal) rather than re-querying the primary -- an identity check,
# not just a count, so a stale departed slot lingering alongside (or instead of) the
# live standby's slot is caught, not just a coincidentally-correct total.
CURRENT_PRIMARY=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
REMAINING_STANDBY=""
for p in "${FULLNAME}-0" "${FULLNAME}-1"; do
  [[ "${p}" != "${CURRENT_PRIMARY}" ]] && REMAINING_STANDBY="${p}"
done
STANDBY_ORDINAL="${REMAINING_STANDBY##*-}"
EXPECTED_SLOT="repmgr_slot_$((1000 + STANDBY_ORDINAL))"

echo "  Waiting for synchronized_standby_slots to name exactly ${EXPECTED_SLOT} (up to 60s)..."
s=0; shrunk_slots=""
while [[ ${s} -lt 60 ]]; do
  shrunk_slots=$(pg_exec "${NAMESPACE}" "${CURRENT_PRIMARY}" "SHOW synchronized_standby_slots" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]')
  [[ "${shrunk_slots}" == "${EXPECTED_SLOT}" ]] && break
  sleep 5; s=$((s + 5))
done
assert_eq "primary: synchronized_standby_slots names exactly the remaining standby's slot" "${EXPECTED_SLOT}" "${shrunk_slots}"

end_suite
print_summary
