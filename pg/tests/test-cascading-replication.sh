#!/bin/bash
# Live cascading-replication test (#29). With repmgr.agent.cascadingReplication=true and
# 3 nodes, the standby two hops from the leader follows the INTERMEDIATE standby (pod-1),
# not the primary directly — offloading the primary's WAL senders into a chain. Proves:
# (1) the chain forms (the far standby streams from the intermediate, not the leader),
# (2) data propagates leader -> intermediate -> far standby, and (3) re-homing: killing
# the intermediate does NOT strand the far standby (it falls back to a live upstream and
# keeps receiving). Agent mode. OPT-IN / standalone: `make -C pg test-cascading-replication`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-cascading}"
RELEASE="${RELEASE:-pgcasc}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"

begin_suite "Cascading replication (#29)"

# --- install agent mode, replicaCount 2 (-> 3 pods), cascading on ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.replicaCount=2 \
  --set repmgr.agent.cascadingReplication=true \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600

# --- find the leader (lease holder that is actually read-write) ---
echo "  Waiting for a primary + lease holder to settle (up to 240s)..."
LEADER=""; s=0
while [[ ${s} -lt 240 ]]; do
  h=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ -n "${h}" ]]; then
    rw=$(pg_exec "${NAMESPACE}" "${h}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${rw}" == "t" ]] && { LEADER="${h}"; break; }
  fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a read-write leader" "${LEADER:-none}" "${FULLNAME}"

# The chain is by pod ordinal toward the leader. The INTERMEDIATE is always ordinal 1;
# the cascaded (far) node is the one two hops from the leader: ordinal 2 when the leader
# is 0, ordinal 0 when the leader is 2. When the leader is the middle (ordinal 1) both
# standbys are adjacent — no cascade is possible (correct), so those asserts are skipped.
INTERMEDIATE="${FULLNAME}-1"
lead_ord="${LEADER##*-}"
case "${lead_ord}" in
  0) CASCADED="${FULLNAME}-2" ;;
  2) CASCADED="${FULLNAME}-0" ;;
  *) CASCADED="" ;;
esac
echo "  leader=${LEADER} intermediate=${INTERMEDIATE} cascaded=${CASCADED:-<none: leader is middle ordinal>}"

# --- data propagates from the leader to ALL standbys (the chain carries WAL) ---
MARK="casc-$(date +%s)"
pg_exec "${NAMESPACE}" "${LEADER}" "CREATE TABLE IF NOT EXISTS casc (id serial PRIMARY KEY, v text); INSERT INTO casc (v) VALUES ('${MARK}')" "testuser" "testdb"
for pod in "${FULLNAME}-0" "${FULLNAME}-1" "${FULLNAME}-2"; do
  [[ "${pod}" == "${LEADER}" ]] && continue
  got=""; w=0
  while [[ ${w} -lt 60 ]]; do
    got=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT v FROM casc WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${got}" == "${MARK}" ]] && break
    sleep 3; w=$((w + 3))
  done
  assert_eq "#29: data reached standby ${pod}" "${MARK}" "${got}"
done

if [[ -n "${CASCADED}" ]]; then
  # --- the chain formed: the far standby streams from the INTERMEDIATE, not the leader.
  #     application_name in pg_stat_replication is the downstream pod name (agent sets it). ---
  echo "  Waiting for the cascade to form (${CASCADED} <- ${INTERMEDIATE}) up to 150s..."
  cascaded_off_intermediate=false; c=0
  while [[ ${c} -lt 150 ]]; do
    n=$(pg_exec "${NAMESPACE}" "${INTERMEDIATE}" "SELECT count(*) FROM pg_stat_replication WHERE application_name='${CASCADED}'" "testuser" "testdb" 2>/dev/null || echo "0")
    [[ "${n}" == "1" ]] && { cascaded_off_intermediate=true; echo "  cascade formed after ~${c}s"; break; }
    sleep 5; c=$((c + 5))
  done
  assert_eq "#29: far standby ${CASCADED} streams from the intermediate ${INTERMEDIATE}" "true" "${cascaded_off_intermediate}"
  # and it does NOT also stream directly from the leader (load really moved off the primary)
  on_leader=$(pg_exec "${NAMESPACE}" "${LEADER}" "SELECT count(*) FROM pg_stat_replication WHERE application_name='${CASCADED}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#29: far standby does NOT stream directly from the leader (offloaded)" "0" "${on_leader}"

  # --- re-homing: kill the intermediate. The far standby's upstream is gone, so the agent
  #     must re-home it to a live upstream (falls back to the leader) -- never stranded. ---
  if [[ "${cascaded_off_intermediate}" == "true" ]]; then
    echo "  Deleting the intermediate ${INTERMEDIATE} (re-homing the far standby)..."
    kubectl delete pod "${INTERMEDIATE}" -n "${NAMESPACE}" --grace-period=10 --wait=false 2>/dev/null || true
    REMARK="casc-rehome-$(date +%s)"
    pg_exec "${NAMESPACE}" "${LEADER}" "INSERT INTO casc (v) VALUES ('${REMARK}')" "testuser" "testdb"
    rehomed=""; rh=0
    while [[ ${rh} -lt 120 ]]; do
      rehomed=$(pg_exec "${NAMESPACE}" "${CASCADED}" "SELECT v FROM casc WHERE v='${REMARK}'" "testuser" "testdb" 2>/dev/null || echo "")
      [[ "${rehomed}" == "${REMARK}" ]] && { echo "  far standby kept receiving after ~${rh}s (re-homed, not stranded)"; break; }
      sleep 5; rh=$((rh + 5))
    done
    assert_eq "#29: far standby is NOT stranded when its upstream dies (re-homed + still receiving)" "${REMARK}" "${rehomed}"
  else
    skip "#29: far standby is NOT stranded when its upstream dies (cascade never formed)"
  fi
else
  skip "#29: far standby streams from the intermediate (leader is the middle ordinal -- no cascade)"
  skip "#29: far standby does NOT stream directly from the leader (leader is the middle ordinal)"
  skip "#29: far standby is NOT stranded when its upstream dies (leader is the middle ordinal)"
fi

end_suite
print_summary
