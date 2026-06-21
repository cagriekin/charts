#!/bin/bash
# Live cascading-replication test (#29). With repmgr.agent.cascadingReplication=true the
# agent chains standbys by pod ordinal toward the leader so a standby may follow another
# standby instead of the primary (offloading the primary's WAL senders). 4 nodes are used
# so a cascade hop exists REGARDLESS of which ordinal the agent elects leader -- the test
# never skips to a false green. Proves: (1) the chain forms (some standby has a downstream
# that streams from it, not from the leader), (2) data propagates leader -> chain -> tail,
# and (3) re-homing -- killing an intermediate does NOT strand its downstream: the
# downstream's upstream moves to another LIVE node and it keeps receiving.
# Agent mode. OPT-IN / standalone: `make -C pg test-cascading-replication`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-cascading}"
RELEASE="${RELEASE:-pgcasc}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
PODS=("${FULLNAME}-0" "${FULLNAME}-1" "${FULLNAME}-2" "${FULLNAME}-3")

begin_suite "Cascading replication (#29)"

# downstreams_of <pod> -> space-separated application_names streaming FROM that pod
downstreams_of() {
  pg_exec "${NAMESPACE}" "$1" "SELECT string_agg(application_name, ' ') FROM pg_stat_replication" "testuser" "testdb" 2>/dev/null || echo ""
}
# is <pod> Running (for the re-home liveness check)
pod_running() { [ "$(kubectl get pod "$1" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; }

# --- install agent mode, replicaCount 3 (-> 4 pods), cascading on ---
# The chart default is a HARD per-node anti-affinity (one postgresql pod per node).
# The CI Kind cluster has only 3 worker nodes, so a 4th pod stays Pending forever and
# the install times out. 4 pods are required so a cascade hop exists for ANY elected
# leader ordinal (3 pods false-greens when the middle ordinal leads). Override with a
# SOFT anti-affinity so the 4 pods still spread but co-locate when nodes run out.
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.replicaCount=3 \
  --set repmgr.agent.cascadingReplication=true \
  --set-json 'postgresql.affinity={"podAntiAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":100,"podAffinityTerm":{"labelSelector":{"matchLabels":{"app.kubernetes.io/component":"postgresql"}},"topologyKey":"kubernetes.io/hostname"}}]}}' \
  --wait --timeout 12m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 4 700

# --- find the read-write leader (lease holder) ---
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

# --- data propagates from the leader to ALL standbys (the chain carries WAL end to end) ---
MARK="casc-$(date +%s)"
pg_exec "${NAMESPACE}" "${LEADER}" "CREATE TABLE IF NOT EXISTS casc (id serial PRIMARY KEY, v text); INSERT INTO casc (v) VALUES ('${MARK}')" "testuser" "testdb"
for pod in "${PODS[@]}"; do
  [[ "${pod}" == "${LEADER}" ]] && continue
  got=""; w=0
  while [[ ${w} -lt 90 ]]; do
    got=$(pg_exec "${NAMESPACE}" "${pod}" "SELECT v FROM casc WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${got}" == "${MARK}" ]] && break
    sleep 3; w=$((w + 3))
  done
  assert_eq "#29: data reached standby ${pod}" "${MARK}" "${got}"
done

# --- the chain formed: SOME standby (not the leader) has a downstream streaming from it.
#     With 4 nodes this holds for ANY elected leader ordinal, so this assertion ALWAYS
#     runs -- it cannot silently skip to green. INTERMEDIATE = a standby with a downstream;
#     CASCADED = the node streaming from it (an application_name = pod name). ---
echo "  Waiting for a cascade hop to form (a standby with a downstream) up to 150s..."
INTERMEDIATE=""; CASCADED=""; c=0
while [[ ${c} -lt 150 ]]; do
  for pod in "${PODS[@]}"; do
    [[ "${pod}" == "${LEADER}" ]] && continue
    downs=$(downstreams_of "${pod}")
    if [[ -n "${downs}" ]]; then
      for d in ${downs}; do
        [[ "${d}" == "${FULLNAME}-"* ]] || continue
        INTERMEDIATE="${pod}"; CASCADED="${d}"; break
      done
    fi
    [[ -n "${INTERMEDIATE}" ]] && break
  done
  [[ -n "${INTERMEDIATE}" ]] && { echo "  cascade hop: ${CASCADED} streams from ${INTERMEDIATE} (after ~${c}s)"; break; }
  sleep 5; c=$((c + 5))
done
assert_contains "#29: a standby acts as a cascade upstream (chain formed, not all from leader)" "${INTERMEDIATE:-none}" "${FULLNAME}"

if [[ -n "${INTERMEDIATE}" && -n "${CASCADED}" ]]; then
  # the cascaded node streams from the INTERMEDIATE, not directly from the leader
  on_leader=$(downstreams_of "${LEADER}")
  has_cascaded_on_leader=$(printf '%s\n' "${on_leader}" | grep -wc "${CASCADED}" || true)
  assert_eq "#29: cascaded ${CASCADED} does NOT stream directly from the leader (offloaded)" "0" "${has_cascaded_on_leader}"

  # --- re-homing: take the INTERMEDIATE down and prove its downstream re-homes to a
  #     DIFFERENT live node -- not merely that data still arrives, which the StatefulSet
  #     recreating the pod would mask. A deleted StatefulSet pod returns in ~40s and the
  #     chain just re-converges through it (the downstream reconnects to the SAME hostname,
  #     so the upstream never observably moves). local-path PVCs are node-pinned, so we
  #     cordon the intermediate's node first: the recreated pod cannot schedule and stays
  #     down for the whole observation window, forcing a real move. Uncordon after (also
  #     via an EXIT trap so a mid-test failure never leaves the node cordoned). ---
  node=$(kubectl get pod "${INTERMEDIATE}" -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  trap 'kubectl uncordon "${node}" >/dev/null 2>&1 || true' EXIT
  echo "  Cordoning ${node}, deleting intermediate ${INTERMEDIATE}; ${CASCADED} must re-home to a different live upstream..."
  kubectl cordon "${node}" >/dev/null 2>&1 || true
  kubectl delete pod "${INTERMEDIATE}" -n "${NAMESPACE}" --grace-period=10 --wait=false 2>/dev/null || true
  rehomed=""; rh=0
  while [[ ${rh} -lt 150 ]]; do
    for pod in "${PODS[@]}"; do
      [[ "${pod}" == "${INTERMEDIATE}" || "${pod}" == "${CASCADED}" ]] && continue
      pod_running "${pod}" || continue
      downs=$(downstreams_of "${pod}")
      if printf '%s\n' "${downs}" | grep -qw "${CASCADED}"; then rehomed="${pod}"; break; fi
    done
    [[ -n "${rehomed}" ]] && { echo "  ${CASCADED} re-homed to ${rehomed} (live, != ${INTERMEDIATE}) after ~${rh}s"; break; }
    sleep 5; rh=$((rh + 5))
  done
  assert_contains "#29: cascaded node re-homed to a different live upstream when its intermediate died" "${rehomed:-none}" "${FULLNAME}"

  # and it kept receiving: a marker written AFTER the intermediate died reaches it
  REMARK="casc-rehome-$(date +%s)"
  pg_exec "${NAMESPACE}" "${LEADER}" "INSERT INTO casc (v) VALUES ('${REMARK}')" "testuser" "testdb"
  arr=""; a=0
  while [[ ${a} -lt 90 ]]; do
    arr=$(pg_exec "${NAMESPACE}" "${CASCADED}" "SELECT v FROM casc WHERE v='${REMARK}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${arr}" == "${REMARK}" ]] && break
    sleep 3; a=$((a + 3))
  done
  assert_eq "#29: cascaded node keeps receiving after re-home (not stranded)" "${REMARK}" "${arr}"

  # let the intermediate recover and the chain re-converge before teardown
  kubectl uncordon "${node}" >/dev/null 2>&1 || true
  trap - EXIT
else
  skip "#29: cascaded node does NOT stream directly from the leader (no cascade hop formed)"
  skip "#29: cascaded node re-homed to a different live upstream when its intermediate died (no cascade hop)"
  skip "#29: cascaded node keeps receiving after re-home (no cascade hop)"
fi

end_suite
print_summary
