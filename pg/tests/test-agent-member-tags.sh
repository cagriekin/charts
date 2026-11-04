#!/bin/bash
# Per-member tags (#275): pg-ha/nofailover and pg-ha/noloadbalance on the marker ConfigMap.
# On a 3-node cluster:
#   - a noloadbalance standby is relabeled pg-role=standby-noloadbalance and leaves the
#     -readonly Service's endpoints, the other standby stays, and untagging restores it;
#   - a switchover to a nofailover standby is not acted on (the primary keeps serving);
#   - when the primary is deleted, the untagged standby promotes and the tagged one never
#     holds the lease or leaves recovery;
#   - tagging the serving primary does not fail it over.
# Standalone, opt-in: `make -C pg test-agent-member-tags`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-agent-tags}"
RELEASE="${RELEASE:-pg-tags}"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"
MARKER="${FULLNAME}-primary"
PODS=("${FULLNAME}-0" "${FULLNAME}-1" "${FULLNAME}-2")
FAILOVER_BUDGET="${FAILOVER_BUDGET:-120}"

begin_suite "Agent per-member tags (#275)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Installing a 3-node agent cluster..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set postgresql.replicaCount=2 \
  --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600

in_recovery() { pg_exec "${NAMESPACE}" "$1" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo ""; }
holder() { kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo ""; }
role_label() { kubectl get pod "$1" -n "${NAMESPACE}" -o jsonpath='{.metadata.labels.pg-role}' 2>/dev/null || echo ""; }
pod_ip() { kubectl get pod "$1" -n "${NAMESPACE}" -o jsonpath='{.status.podIP}' 2>/dev/null || echo ""; }
readonly_ips() {
  kubectl get endpointslices -n "${NAMESPACE}" -l "kubernetes.io/service-name=${FULLNAME}-readonly" \
    -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{" "}{end}' 2>/dev/null || echo ""
}

# settle_roles <budget>: sets PRIMARY and STANDBYS once exactly one node is read-write, holds
# the lease, and the other two are in recovery.
settle_roles() {
  local budget="$1" elapsed=0 p r rw h
  while [[ ${elapsed} -lt ${budget} ]]; do
    PRIMARY=""; STANDBYS=(); rw=0
    for p in "${PODS[@]}"; do
      r=$(in_recovery "${p}")
      if [[ "${r}" == "f" ]]; then PRIMARY="${p}"; rw=$((rw + 1)); elif [[ "${r}" == "t" ]]; then STANDBYS+=("${p}"); fi
    done
    h=$(holder)
    if [[ ${rw} -eq 1 && ${#STANDBYS[@]} -eq 2 && "${h}" == "${PRIMARY}" ]]; then return 0; fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

# wait_label <pod> <value> <budget>
wait_label() {
  local e=0
  while [[ ${e} -lt $3 ]]; do
    [[ "$(role_label "$1")" == "$2" ]] && return 0
    sleep 3; e=$((e + 3))
  done
  return 1
}

# wait_endpoint <ip> <present|absent> <budget>
wait_endpoint() {
  local e=0 ips
  while [[ ${e} -lt $3 ]]; do
    ips=" $(readonly_ips) "
    if [[ "$2" == "present" && "${ips}" == *" $1 "* ]]; then return 0; fi
    if [[ "$2" == "absent" && "${ips}" != *" $1 "* ]]; then return 0; fi
    sleep 3; e=$((e + 3))
  done
  return 1
}

echo "Waiting for the roles to settle..."
if settle_roles 300; then
  pass "install: one primary holds the lease (${PRIMARY}), two standbys (${STANDBYS[*]})"
else
  fail "install: one primary holds the lease and two standbys stream" "primary=${PRIMARY:-none} standbys=${STANDBYS[*]:-none} holder=$(holder)"
  end_suite; print_summary; exit 1
fi
S1="${STANDBYS[0]}"
S2="${STANDBYS[1]}"

# --- noloadbalance ---
for s in "${S1}" "${S2}"; do
  wait_endpoint "$(pod_ip "${s}")" present 120 \
    && pass "baseline: ${s} is a -readonly endpoint" \
    || fail "baseline: ${s} is a -readonly endpoint" "endpoints: $(readonly_ips)"
done
echo "Tagging ${S1} noloadbalance..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/noloadbalance="${S1}" --overwrite >/dev/null
wait_label "${S1}" standby-noloadbalance 60 \
  && pass "#275: the primary relabels ${S1} pg-role=standby-noloadbalance" \
  || fail "#275: the primary relabels ${S1} pg-role=standby-noloadbalance" "pg-role=$(role_label "${S1}")"
wait_endpoint "$(pod_ip "${S1}")" absent 60 \
  && pass "#275: ${S1} leaves the -readonly Service" \
  || fail "#275: ${S1} leaves the -readonly Service" "endpoints: $(readonly_ips)"
assert_eq "#275: the untagged standby keeps pg-role=standby" "standby" "$(role_label "${S2}")"
wait_endpoint "$(pod_ip "${S2}")" present 30 \
  && pass "#275: the untagged standby stays a -readonly endpoint" \
  || fail "#275: the untagged standby stays a -readonly endpoint" "endpoints: $(readonly_ips)"
assert_eq "#275: the tagged standby still replicates" "t" "$(in_recovery "${S1}")"
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/noloadbalance- >/dev/null
wait_label "${S1}" standby 60 \
  && pass "#275: removing the tag restores pg-role=standby" \
  || fail "#275: removing the tag restores pg-role=standby" "pg-role=$(role_label "${S1}")"
wait_endpoint "$(pod_ip "${S1}")" present 60 \
  && pass "#275: ... and the -readonly endpoint" \
  || fail "#275: ... and the -readonly endpoint" "endpoints: $(readonly_ips)"

# --- nofailover: switchover to a tagged standby is not acted on ---
echo "Tagging ${S1} nofailover and requesting a switchover to it..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/nofailover="${S1}" --overwrite >/dev/null
assert_contains "#275: the agent logs the tag change" \
  "$(sleep 15; kubectl logs -n "${NAMESPACE}" "${PRIMARY}" -c postgresql --since=2m 2>/dev/null | grep 'member tags' | grep -m1 "nofailover=\[${S1}\]" || true)" \
  "nofailover=.${S1}"
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/switchover-target="${S1}" --overwrite >/dev/null
moved=false; elapsed=0
while [[ ${elapsed} -lt 45 ]]; do
  [[ "$(holder)" != "${PRIMARY}" || "$(in_recovery "${PRIMARY}")" != "f" ]] && { moved=true; break; }
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "#275: a switchover to a nofailover standby is not acted on (45s)" "false" "${moved}"
assert_eq "#275: ... and the request stays pending" "${S1}" \
  "$(kubectl get configmap "${MARKER}" -n "${NAMESPACE}" -o jsonpath='{.metadata.annotations.pg-ha/switchover-target}' 2>/dev/null)"
assert_contains "#275: ... with the tag named in the decision" \
  "$(kubectl logs -n "${NAMESPACE}" "${PRIMARY}" -c postgresql --since=2m 2>/dev/null | grep -m1 'tagged pg-ha/nofailover' || true)" \
  "nofailover"
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/switchover-target- >/dev/null

# --- nofailover: failover skips the tagged standby ---
MARK="before-failover-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS member_tags (v text)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO member_tags VALUES ('${MARK}')" "testuser" "testdb" >/dev/null
for s in "${S1}" "${S2}"; do
  e=0
  while [[ ${e} -lt 60 ]]; do
    [[ "$(pg_exec "${NAMESPACE}" "${s}" "SELECT v FROM member_tags WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || true)" == "${MARK}" ]] && break
    sleep 2; e=$((e + 2))
  done
done
OLD_PRIMARY="${PRIMARY}"
echo "Deleting primary ${OLD_PRIMARY}: ${S2} must promote, ${S1} (nofailover) must not..."
kubectl delete pod "${OLD_PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false >/dev/null 2>&1 || true
promoted=false; tagged_held=false; tagged_rw=false; elapsed=0
while [[ ${elapsed} -lt ${FAILOVER_BUDGET} ]]; do
  h=$(holder)
  [[ "${h}" == "${S1}" ]] && tagged_held=true
  [[ "$(in_recovery "${S1}")" == "f" ]] && tagged_rw=true
  if [[ "${h}" == "${S2}" && "$(in_recovery "${S2}")" == "f" ]]; then promoted=true; echo "  ${S2} promoted after ${elapsed}s"; break; fi
  sleep 2; elapsed=$((elapsed + 2))
done
assert_eq "#275: the untagged standby promotes" "true" "${promoted}"
assert_eq "#275: the nofailover standby never left recovery" "false" "${tagged_rw}"
assert_eq "#275: the nofailover standby was never seen holding the lease" "false" "${tagged_held}"
assert_eq "#275: no data lost across the failover" "${MARK}" \
  "$(pg_exec "${NAMESPACE}" "${S2}" "SELECT v FROM member_tags WHERE v='${MARK}'" "testuser" "testdb" 2>/dev/null || true)"
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 3 600
settle_roles 300 || true
assert_eq "#275: ${S2} is the primary after the old one returns" "${S2}" "${PRIMARY}"
assert_eq "#275: the former primary rejoined as a standby" "t" "$(in_recovery "${OLD_PRIMARY}")"
assert_eq "#275: the nofailover standby is still a standby" "t" "$(in_recovery "${S1}")"

# --- tagging the serving primary does not fail it over ---
echo "Tagging the serving primary ${S2} nofailover as well..."
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/nofailover="${S1},${S2}" --overwrite >/dev/null
moved=false; elapsed=0
while [[ ${elapsed} -lt 45 ]]; do
  [[ "$(holder)" != "${S2}" || "$(in_recovery "${S2}")" != "f" ]] && { moved=true; break; }
  sleep 5; elapsed=$((elapsed + 5))
done
assert_eq "#275: a tagged primary keeps serving (45s)" "false" "${moved}"
kubectl annotate configmap "${MARKER}" -n "${NAMESPACE}" pg-ha/nofailover- >/dev/null

end_suite
print_summary
