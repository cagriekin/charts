#!/bin/bash
# Regression trap for the PG18 + repmgr "empty type column" bug.
# Installs 1+1 repmgr, deletes the standby pod 3 times, re-asserts
# repmgr.nodes.type='standby' after each replacement.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-repmgr-chaos}"
RELEASE="${RELEASE:-pg-chaos}"
VALUES="${SCRIPT_DIR}/values-repmgr.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")

begin_suite "Repmgr Chaos Restart (delete standby pod x 3, re-assert type='standby')"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm uninstall "${RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pvc -n "${NAMESPACE}" --all --wait=false 2>/dev/null || true
sleep 3

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${VALUES}" \
  --wait --timeout 10m

POD_PRIMARY="${FULLNAME}-0"
POD_REPLICA="${FULLNAME}-1"

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

# Baseline check before any chaos
baseline_type=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT type FROM repmgr.nodes WHERE node_id=1001" "repmgr" "repmgr")
assert_eq "baseline: standby row has type='standby'" "standby" "${baseline_type}"

for round in 1 2 3; do
  echo "  --- round ${round}: deleting ${POD_REPLICA} ---"
  kubectl delete pod -n "${NAMESPACE}" "${POD_REPLICA}" --wait=true > /dev/null
  # StatefulSet immediately re-creates the pod with the same name.
  # Wait until both pods are Ready again.
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600

  # The pod must reach Running, NOT CrashLoopBackOff
  pod_phase=$(kubectl get pod -n "${NAMESPACE}" "${POD_REPLICA}" -o jsonpath='{.status.phase}')
  assert_eq "round ${round}: ${POD_REPLICA} Running after restart" "Running" "${pod_phase}"

  waiting_reasons=$(kubectl get pod -n "${NAMESPACE}" "${POD_REPLICA}" -o jsonpath='{.status.containerStatuses[*].state.waiting.reason}')
  assert_not_contains "round ${round}: no CrashLoopBackOff" "${waiting_reasons}" "CrashLoopBackOff"

  # The row on the primary must still read type='standby' — the chart's
  # post-register UPDATE backfill should converge it after each restart.
  standby_type=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT type FROM repmgr.nodes WHERE node_id=1001" "repmgr" "repmgr")
  assert_eq "round ${round}: standby type='standby'" "standby" "${standby_type}"

  standby_active=$(pg_exec "${NAMESPACE}" "${POD_PRIMARY}" "SELECT active FROM repmgr.nodes WHERE node_id=1001" "repmgr" "repmgr")
  assert_eq "round ${round}: standby active=true" "t" "${standby_active}"

  # Replication still flowing
  is_replica=$(pg_exec "${NAMESPACE}" "${POD_REPLICA}" "SELECT pg_is_in_recovery()" "testuser" "testdb")
  assert_eq "round ${round}: replica still in recovery" "t" "${is_replica}"
done

end_suite
print_summary
