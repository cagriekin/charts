#!/bin/bash
# Live config-persistence-across-failover test (#33). Proves that a custom
# postgresql.configuration value (injected via the conf.d include + ConfigMap, mounted
# on every pod in the StatefulSet) survives a primary failover: the promoted standby
# applies the SAME custom setting, so a failover does not silently revert to defaults.
# Agent mode (the default). OPT-IN / standalone: `make -C pg test-config-failover`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-config-failover}"
RELEASE="${RELEASE:-pgcfgfo}"
FAILOVER_BUDGET="${FAILOVER_BUDGET:-90}"
# distinctive non-default value so a revert-to-default is unmistakable
WORK_MEM="17MB"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-agent.yaml")
LEASE="${FULLNAME}-leader"

begin_suite "Config persistence across failover (#33)"

# --- install agent mode (replicaCount 1 -> 2 pods) with a custom postgresql.conf value ---
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-agent.yaml" \
  --set-string "postgresql.configuration.work_mem=${WORK_MEM}" \
  --wait --timeout 10m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600
POD0="${FULLNAME}-0"; POD1="${FULLNAME}-1"

# --- settle the primary/standby pair ---
echo "  Waiting for a single primary to settle (up to 240s)..."
PRIMARY=""; STANDBY=""; s=0
while [[ ${s} -lt 240 ]]; do
  r0=$(pg_exec "${NAMESPACE}" "${POD0}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  r1=$(pg_exec "${NAMESPACE}" "${POD1}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  if [[ "${r0}" == "f" && "${r1}" == "t" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; break; fi
  if [[ "${r1}" == "f" && "${r0}" == "t" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; break; fi
  sleep 5; s=$((s + 5))
done
assert_contains "agent elected a primary" "${PRIMARY:-none}" "${FULLNAME}"

# --- the custom config is live on the primary (conf.d include applied) ---
wm_primary=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW work_mem" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#33: custom work_mem live on the primary (pre-failover)" "${WORK_MEM}" "${wm_primary}"
# the standby carries the same ConfigMap, so it already has the value before promotion
wm_standby=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SHOW work_mem" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "#33: custom work_mem also present on the standby" "${WORK_MEM}" "${wm_standby}"

# a marker to confirm the new primary is writable after failover
FV="cfg-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE IF NOT EXISTS cfg_fo (id serial PRIMARY KEY, v text); INSERT INTO cfg_fo (v) VALUES ('${FV}')" "testuser" "testdb"
sleep 3

# --- fail the primary over ---
echo "  Deleting primary ${PRIMARY} (failover)..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
promoted=false; e=0
while [[ ${e} -lt ${FAILOVER_BUDGET} ]]; do
  rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
  holder=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [[ "${rec}" == "t" && "${holder}" == "${STANDBY}" ]]; then promoted=true; echo "  failover after ${e}s (new primary = ${STANDBY})"; break; fi
  sleep 3; e=$((e + 3))
done
assert_eq "#33: standby promoted on failover" "true" "${promoted}"

if [[ "${promoted}" == "true" ]]; then
  # --- config PERSISTS: the promoted standby still applies the custom value ---
  wm_new=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SHOW work_mem" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#33: custom work_mem persists on the promoted primary (did not revert to default)" "${WORK_MEM}" "${wm_new}"
  # the new primary is writable + pre-failover data survived. Run the INSERT and the
  # SELECT as SEPARATE pg_exec calls: a combined "INSERT ...; SELECT ..." emits the
  # INSERT command tag on stdout, which would corrupt the exact-equality assertion.
  pg_exec "${NAMESPACE}" "${STANDBY}" "INSERT INTO cfg_fo (v) VALUES ('after')" "testuser" "testdb"
  after_w=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT v FROM cfg_fo WHERE v='${FV}'" "testuser" "testdb" 2>/dev/null || echo "")
  assert_eq "#33: new primary is writable + pre-failover data survived" "${FV}" "${after_w}"

  # --- re-clone coverage: the ex-primary rejoins as a standby (re-cloned/re-attached to
  # the new primary) and must ALSO carry the custom config -- the ConfigMap is mounted on
  # every pod, so a freshly (re)cloned node gets the value independent of the basebackup. ---
  echo "  Waiting for ${PRIMARY} to rejoin as a standby (up to 300s)..."
  rejoined=false; r=0
  while [[ ${r} -lt 300 ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${rec}" == "t" ]] && { rejoined=true; echo "  ${PRIMARY} rejoined as standby after ~${r}s"; break; }
    sleep 10; r=$((r + 10))
  done
  assert_eq "#33: ex-primary rejoined as a standby (re-cloned from new primary)" "true" "${rejoined}"
  if [[ "${rejoined}" == "true" ]]; then
    wm_reclone=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW work_mem" "testuser" "testdb" 2>/dev/null || echo "")
    assert_eq "#33: re-cloned standby inherits the custom work_mem" "${WORK_MEM}" "${wm_reclone}"
  else
    skip "#33: re-cloned standby inherits the custom work_mem (rejoin did not complete)"
  fi
else
  skip "#33: custom work_mem persists on the promoted primary (failover did not complete)"
  skip "#33: new primary is writable + pre-failover data survived (failover did not complete)"
  skip "#33: ex-primary rejoined as a standby (re-cloned from new primary) (failover did not complete)"
  skip "#33: re-cloned standby inherits the custom work_mem (failover did not complete)"
fi

end_suite
print_summary
