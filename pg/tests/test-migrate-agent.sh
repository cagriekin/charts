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

# 1. install repmgrd mode (the legacy 0.x path; values-repmgr.yaml pins
# failoverMode: repmgrd now that agent is the 1.0.0 default)
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
AGENT_IMAGE_TAG="${AGENT_IMAGE_TAG:-trixie-5.5.0-26}"
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
  # Identify the primary by holder + not-in-recovery only -- do NOT require the
  # standby to be queryable. A wedged standby (#181) must still let us settle on the
  # primary so the streaming assertion below FAILS rather than the block skipping.
  if [[ "${r0}" == "f" && "${HOLDER}" == "${POD0}" ]]; then PRIMARY="${POD0}"; STANDBY="${POD1}"; fi
  if [[ "${r1}" == "f" && "${HOLDER}" == "${POD1}" ]]; then PRIMARY="${POD1}"; STANDBY="${POD0}"; fi
  if [[ -n "${PRIMARY}" ]]; then echo "  migrated after ${m}s: primary=${PRIMARY} holder=${HOLDER}"; break; fi
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

  # #181 regression: the migrated standby must actually re-establish STREAMING, not
  # just exist. Queried on the primary (always up), so a wedged standby FAILS here
  # rather than the block skipping.
  mstream=""; ms=0
  while [[ ${ms} -lt 120 ]]; do
    mstream=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT state FROM pg_stat_replication WHERE application_name='${STANDBY}'" "testuser" "testdb" 2>/dev/null || echo "")
    [[ "${mstream}" == "streaming" ]] && break
    sleep 5; ms=$((ms + 5))
  done
  assert_eq "#181: migrated standby is actively streaming" "streaming" "${mstream}"

  # #199 regression: the rejoined STANDBY must carry the md5-first pg_hba the agent
  # authors on every node. It formerly ended up SCRAM-only (the agent's boot write won
  # the race against the chart's postStart md5-fallback), which broke md5-password TCP
  # auth into the standby -- exporter pg_up=0, pgpool -readonly backend auth failure,
  # and a failover lockout if such a standby were promoted.
  standby_hba=$(kubectl exec -n "${NAMESPACE}" "${STANDBY}" -c postgresql -- bash -lc 'cat "$PGDATA"/pg_hba.conf' 2>/dev/null || echo "")
  assert_contains "#199: standby pg_hba is agent-authored" "${standby_hba}" "Managed by pg-ha-agent"
  md5hosts=$(printf '%s\n' "${standby_hba}" | grep -cE '^host[[:space:]].*[[:space:]]md5[[:space:]]*$' || true)
  assert_gt "#199: standby pg_hba carries md5 fallback host rules (not SCRAM-only)" "${md5hosts}" "0"
  # End-to-end: a managed user authenticates over TCP to the standby (the exact path that
  # failed in #199). Connect from the primary pod to the standby's headless FQDN.
  PGPASS=$(kubectl get secret -n "${NAMESPACE}" "${FULLNAME}" -o jsonpath='{.data.password}' | base64 -d)
  std_auth=$(kubectl exec -n "${NAMESPACE}" "${PRIMARY}" -c postgresql -- \
    env PGPASSWORD="${PGPASS}" psql -h "${STANDBY}.${FULLNAME}-headless.${NAMESPACE}.svc.cluster.local" \
    -U testuser -d testdb -tAc "SELECT 1" 2>/dev/null || echo "")
  assert_eq "#199: managed user authenticates over TCP to the standby" "1" "${std_auth}"

  echo "  Post-migration failover: deleting primary ${PRIMARY}..."
  kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --grace-period=30 --wait=false 2>/dev/null || true
  promoted=false; e=0
  while [[ ${e} -lt 120 ]]; do
    rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null || echo "")
    if [[ "${rec}" == "t" ]]; then promoted=true; echo "  post-migration failover after ${e}s (new primary = ${STANDBY})"; break; fi
    sleep 5; e=$((e + 5))
  done
  assert_eq "post-migration agent failover promotes the standby" "true" "${promoted}"

  # #199: the agent re-hashes md5 managed users to scram once a node STABLY becomes
  # primary (~1s after promotion). A 2-node failover can first surface a transient
  # read-write window during the rejoin/timeline-divergence dance, then settle on the
  # real primary after a re-promote -- so poll the current LEASE HOLDER (the agent's
  # authoritative primary) for convergence with a generous window, not the pod that
  # first reported not-in-recovery.
  if [[ "${promoted}" == "true" ]]; then
    scram=""; sc=0
    while [[ ${sc} -lt 150 ]]; do
      HP=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
      if [[ -n "${HP}" ]]; then
        # rolpassword for a SCRAM secret starts with the literal "SCRAM-SHA-256$" (uppercase);
        # LIKE is case-sensitive, so match 'SCRAM%', not 'scram%'.
        scram=$(pg_exec "${NAMESPACE}" "${HP}" "SELECT rolpassword LIKE 'SCRAM%' FROM pg_authid WHERE rolname='testuser'" "testuser" "testdb" 2>/dev/null || echo "")
        [[ "${scram}" == "t" ]] && break
      fi
      sleep 5; sc=$((sc + 5))
    done
    assert_eq "#199: managed users re-hashed to scram after promotion" "t" "${scram}"
  fi
else
  skip "after: lease holder is the primary (migration did not settle)"
  skip "data survives the repmgrd->agent migration (migration did not settle)"
  skip "#199: standby pg_hba is agent-authored (migration did not settle)"
  skip "#199: standby pg_hba carries md5 fallback host rules (migration did not settle)"
  skip "#199: managed user authenticates over TCP to the standby (migration did not settle)"
  skip "post-migration agent failover promotes the standby (migration did not settle)"
  skip "#199: managed users re-hashed to scram after promotion (migration did not settle)"
fi

end_suite
print_summary
