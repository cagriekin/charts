#!/bin/bash
# Multi-replica pgBackRest PITR restore test (#226). The single-node suite
# (test-pgbackrest-restore.sh) proves the restore Job repairs replica 0. This one answers the
# question that only exists with a standby: after replica 0 is restored and promotes onto a
# NEW timeline, what happens to replica 1, whose PVC still holds PRE-restore data on the OLD
# timeline? The README claims repmgr rebuilds it -- this asserts that end to end, and if the
# standby cannot rejoin on its own it asserts the documented manual fallback (delete its PVC
# + pod) instead, so the docs can never drift from the real behaviour.
#
# OPT-IN / standalone: `make -C pg test-pgbackrest-restore-ha`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-pgbackrest-restore-ha}"
RELEASE="${RELEASE:-pgbrha}"
VALUES="${SCRIPT_DIR}/values-pgbackrest-minio-ha.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")

begin_suite "pgBackRest multi-replica PITR restore (#226)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
deploy_minio "${NAMESPACE}"

echo "Installing pg chart (primary + 1 standby) with pgbackrest + restore Job enabled..."
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
  -f "${VALUES}" \
  --set pgbackrest.restore.enabled=true \
  --wait --timeout 10m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 900
# DISCOVER the roles rather than assuming ordinal 0 (#288). Under `mechanism: native` the
# initial primary is whichever pod wins the lease -- the holder is what runs initdb -- and with
# podManagementPolicy Parallel that need not be pod-0. Verified live: on a fresh native install
# pod-1 took the lease, initdb'd, and pod-0 cloned FROM it. Assuming pod-0 encoded a repmgr
# implementation detail (init-repmgr.sh hardcoded ordinal 0 as master).
PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" 2)
# assert_not_eq refuses empty operands by design ("both values must be non-empty"), so an
# existence check has to be expressed as an equality on a derived flag.
assert_eq "baseline: a primary exists" "yes" "$([ -n "${PRIMARY}" ] && echo yes || echo no)"
# fail() only counts, it does not exit (pg/tests/helpers.sh). Without an explicit stop, an empty
# PRIMARY makes $(( 1 - PRIMARY_ORD )) evaluate the empty string as 0 and the next pg_exec runs
# `kubectl exec ""`, aborting under set -euo pipefail with an opaque kubectl error instead of the
# assertion above.
if [ -z "${PRIMARY}" ]; then
  echo "  no primary found; cannot continue" >&2
  print_summary
  exit 1
fi
PRIMARY_ORD="${PRIMARY##*-}"
STANDBY="${FULLNAME}-$(( 1 - PRIMARY_ORD ))"
echo "  discovered primary=${PRIMARY} standby=${STANDBY}"

# --- baseline: a genuine streaming pair, so the standby PVC really holds live data ---
is_primary=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT NOT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "baseline: ${PRIMARY} is the primary" "t" "${is_primary}"
is_standby=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT pg_is_in_recovery()" "testuser" "testdb")
assert_eq "baseline: ${STANDBY} is a standby" "t" "${is_standby}"

# The restore Job writes into data-<fullname>-<pgbackrest.restore.podOrdinal>, which the chart
# renders at INSTALL time -- before any role is known. Re-point it at the discovered primary so
# the restore lands in the PVC this test is about to wipe.
if [ "${PRIMARY_ORD}" != "0" ]; then
  echo "  re-pointing the restore Job at ordinal ${PRIMARY_ORD}..."
  helm upgrade "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
    -f "${VALUES}" \
    --set pgbackrest.restore.enabled=true \
    --set pgbackrest.restore.podOrdinal="${PRIMARY_ORD}" \
    --wait --timeout 10m
  # podOrdinal is NOT Job-only: statefulset.yaml renders it as CONTROL_RESTORE_POD_ORDINAL in the
  # POD template, so this upgrade mutates the StatefulSet and rolls both pods (--wait only waits
  # for readiness). Under mechanism: native, restarting the primary is itself a failover trigger,
  # so the lease -- and the primary role -- can move during the upgrade. Re-discover instead of
  # trusting the pre-upgrade values, or every later assertion (including which PVC the restore
  # Job wipes) targets the wrong pod (#288 review).
  PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" 2)
  if [ -z "${PRIMARY}" ]; then
    echo "  no primary after the re-point upgrade; cannot continue" >&2
    print_summary
    exit 1
  fi
  if [ "${PRIMARY##*-}" != "${PRIMARY_ORD}" ]; then
    echo "  NOTE: the role moved to ${PRIMARY} during the upgrade; the restore Job still targets" \
         "ordinal ${PRIMARY_ORD}, so re-pointing it again"
    PRIMARY_ORD="${PRIMARY##*-}"
    helm upgrade "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" \
      -f "${VALUES}" \
      --set pgbackrest.restore.enabled=true \
      --set pgbackrest.restore.podOrdinal="${PRIMARY_ORD}" \
      --wait --timeout 10m
    PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" 2)
    # Same guard as the baseline discovery above (#288 review, round 3): an empty PRIMARY makes
    # PRIMARY_ORD empty, $(( 1 - PRIMARY_ORD )) silently evaluate to 1, and the next pg_exec run
    # `kubectl exec ""` -- aborting the suite under set -euo pipefail with an opaque kubectl
    # error rather than a legible failure.
    if [ -z "${PRIMARY}" ]; then
      fail "re-point: a primary exists after the second upgrade" "discover_primary returned nothing"
      print_summary
      exit 1
    fi
    PRIMARY_ORD="${PRIMARY##*-}"
  fi
  STANDBY="${FULLNAME}-$(( 1 - PRIMARY_ORD ))"
  echo "  after re-point: primary=${PRIMARY} standby=${STANDBY}"
fi

# --- first full backup: runs stanza-create, after which WAL archiving succeeds ---
echo "Triggering initial full backup (creates the stanza)..."
kubectl delete job pgbr-full -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-full --from=cronjob/"${FULLNAME}-pgbackrest-full"
full_rc=0
kubectl wait --for=condition=complete job/pgbr-full -n "${NAMESPACE}" --timeout=300s || full_rc=$?
if [ "${full_rc}" -ne 0 ]; then
  echo "  full-backup job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-full --tail=80 2>/dev/null || true
fi
assert_eq "initial full backup (stanza-create) succeeds" "0" "${full_rc}"
# Pre-stanza archive_command failures inflate failed_count; reset so the health check below
# measures only post-stanza pushes (mirrors test-pgbackrest-restore.sh).
pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_stat_reset_shared('archiver')" "testuser" "testdb" >/dev/null 2>&1 || true

# --- data written AFTER the backup, archived as WAL and streamed to the standby ---
echo "Writing post-backup data, archiving it as WAL and waiting for the standby to stream it..."
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE pitr_proof (id bigserial PRIMARY KEY, v text)" "testuser" "testdb"
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO pitr_proof (v) SELECT repeat('x',256) FROM generate_series(1,5000)" "testuser" "testdb"
seg=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_walfile_name(pg_current_wal_lsn())" "testuser" "testdb" | tr -d '[:space:]')
pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_switch_wal()" "testuser" "testdb" >/dev/null
for _ in $(seq 1 90); do
  last=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT COALESCE(last_archived_wal,'')" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  if [ -n "${last}" ] && [ ! "${last}" \< "${seg}" ]; then break; fi
  sleep 2
done
failed=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT failed_count FROM pg_stat_archiver" "testuser" "testdb" 2>/dev/null || echo "")
assert_eq "WAL archiving healthy before restore (no failed pushes)" "0" "${failed}"
# The standby must actually hold the pre-restore data: that is what makes its PVC "stale"
# rather than empty after the primary is restored and promotes onto a new timeline.
standby_rows=""
for _ in $(seq 1 30); do
  standby_rows=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${standby_rows}" = "5000" ] && break
  sleep 2
done
assert_eq "standby streamed the pre-restore data (its PVC is stale, not empty)" "5000" "${standby_rows}"
tl_before=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT timeline_id FROM pg_control_checkpoint()" "testuser" "testdb" | tr -d '[:space:]')

# =============================================================================
# Disaster + recovery: destroy ONLY the primary's data directory, restore it with the
# chart's Job, and bring the pair back. The standby's PVC is deliberately left intact.
# =============================================================================
echo "Scaling the StatefulSet to 0..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=0
kubectl wait --for=delete pod/"${PRIMARY}" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true
kubectl wait --for=delete pod/"${STANDBY}" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true

echo "Destroying the PRIMARY's data directory (the standby's PVC is left untouched)..."
kubectl delete pod pg-wipe -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl run pg-wipe -n "${NAMESPACE}" --restart=Never --image=busybox:1.37 \
  --overrides="{\"spec\":{\"containers\":[{\"name\":\"pg-wipe\",\"image\":\"busybox:1.37\",\"command\":[\"sh\",\"-c\",\"rm -rf /data/pgdata && echo wiped\"],\"securityContext\":{\"runAsUser\":0},\"volumeMounts\":[{\"name\":\"data\",\"mountPath\":\"/data\"}]}],\"volumes\":[{\"name\":\"data\",\"persistentVolumeClaim\":{\"claimName\":\"data-${FULLNAME}-0\"}}]}}" >/dev/null
wipe_rc=0
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/pg-wipe -n "${NAMESPACE}" --timeout=180s >/dev/null 2>&1 || wipe_rc=$?
assert_eq "primary data directory wiped (disaster simulated)" "0" "${wipe_rc}"
kubectl delete pod pg-wipe -n "${NAMESPACE}" --wait=false >/dev/null 2>&1 || true

echo "Running the chart's restore Job..."
kubectl delete job pgbr-restore -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create job -n "${NAMESPACE}" pgbr-restore --from=cronjob/"${FULLNAME}-pgbackrest-restore"
res_rc=2
for _ in $(seq 1 120); do
  succeeded=$(kubectl get job pgbr-restore -n "${NAMESPACE}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "")
  failed_j=$(kubectl get job pgbr-restore -n "${NAMESPACE}" -o jsonpath='{.status.failed}' 2>/dev/null || echo "")
  [ "${succeeded:-0}" -ge 1 ] 2>/dev/null && { res_rc=0; break; }
  [ "${failed_j:-0}" -ge 1 ] 2>/dev/null && { res_rc=1; break; }
  sleep 3
done
if [ "${res_rc}" -ne 0 ]; then
  echo "  restore job did not complete; logs:"; kubectl logs -n "${NAMESPACE}" job/pgbr-restore --tail=200 2>/dev/null || true
fi
assert_eq "restore job completes" "0" "${res_rc}"

echo "Scaling back up to primary + standby..."
kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=2

# --- the restored primary becomes ready first ---
# Not an ordering guarantee: this fixture runs agent mode (Parallel) since #286, so both
# pods start together. It is the restored pod that wins the Lease and serves first, because
# the standby still has to clone -- hence waiting for exactly 1 ready pod here.
prim_rc=0
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 600 || prim_rc=$?
assert_eq "restored primary becomes ready" "0" "${prim_rc}"
restored_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "primary recovered the data from S3 (5000 rows)" "5000" "${restored_rows}"
# POLL for the promotion, do not read once (#288). A PITR restore advances the timeline when
# recovery ENDS and the node promotes -- and under native that happens in the agent's reconcile
# loop AFTER the pod reports Ready, so a single read races it and returns the pre-restore
# timeline. Under repmgr the restore-and-promote completed inside the entrypoint before readiness,
# which is why one read was enough there. Reading early does double damage: the assertion fails,
# and the stale value is then carried into the standby comparison below, which reported the
# standby as WRONG for being correctly ahead of it.
tl_after=""
for _ in $(seq 1 45); do
  tl_after=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT timeline_id FROM pg_control_checkpoint()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ -n "${tl_after}" ] && [ "${tl_after}" -gt "${tl_before:-1}" ] 2>/dev/null && break
  sleep 4
done
assert_gt "recovery advanced the primary's timeline (so the standby's PVC is now diverged)" "${tl_after:-0}" "${tl_before:-1}"

# --- THE question: does the standby rejoin on its own, with stale data on the old timeline? ---
echo "Waiting for the standby to rejoin the restored primary (it holds pre-restore data on timeline ${tl_before})..."
standby_auto=""
for _ in $(seq 1 100); do
  ready=$(kubectl get pod "${STANDBY}" -n "${NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
  if [ "${ready}" = "True" ]; then
    rec=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
    [ "${rec}" = "t" ] && { standby_auto="yes"; break; }
  fi
  sleep 6
done

if [ "${standby_auto}" = "yes" ]; then
  # README claim confirmed: repmgr rebuilt/re-homed the standby with no operator action.
  pass "standby rejoined the restored primary automatically (no manual PVC deletion)"
else
  # Honest fallback: record that automatic rejoin did NOT happen, then verify the manual
  # step the README documents actually fixes it. Either way the docs match reality.
  fail "standby rejoined the restored primary automatically (no manual PVC deletion)" \
    "standby ${STANDBY} did not become a healthy standby on its own; asserting the documented manual re-clone instead"
  echo "  Standby state at give-up:"
  kubectl get pod "${STANDBY}" -n "${NAMESPACE}" -o wide 2>/dev/null || true
  kubectl logs -n "${NAMESPACE}" "${STANDBY}" -c postgresql --tail=40 2>/dev/null || true
  echo "Applying the documented fallback: drop the standby's PVC so it clones fresh..."
  # Order matters, and NOT the obvious way round. Deleting the PVC while pod-1 still exists
  # leaves it Terminating behind the kubernetes.io/pvc-protection finalizer; the StatefulSet
  # then recreates pod-1, which can re-bind that same PVC before deletion completes -- the
  # standby would come back on its STALE pre-restore data and the assertions below could
  # still pass, reporting a re-clone that never happened. So scale the standby away first,
  # delete the PVC, confirm it is actually GONE, and only then let it be recreated.
  old_pvc_uid=$(kubectl get pvc "data-${FULLNAME}-1" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=1
  kubectl wait --for=delete pod/"${STANDBY}" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true
  kubectl delete pvc "data-${FULLNAME}-1" -n "${NAMESPACE}" --wait=true --timeout=300s >/dev/null 2>&1 || true
  kubectl wait --for=delete pvc/"data-${FULLNAME}-1" -n "${NAMESPACE}" --timeout=300s 2>/dev/null || true
  kubectl scale statefulset "${FULLNAME}" -n "${NAMESPACE}" --replicas=2
  reclone_rc=0
  wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 2 600 || reclone_rc=$?
  assert_eq "documented fallback works: standby re-clones after PVC deletion" "0" "${reclone_rc}"
  # Prove the standby is on a genuinely NEW volume: a re-bound stale PVC keeps its UID, and
  # would let the checks below pass on pre-restore data.
  new_pvc_uid=$(kubectl get pvc "data-${FULLNAME}-1" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  if [ -n "${old_pvc_uid}" ] && [ "${old_pvc_uid}" = "${new_pvc_uid}" ]; then
    fail "fallback provisioned a fresh PVC" "PVC data-${FULLNAME}-1 kept uid ${new_pvc_uid}: the stale volume was re-bound, not replaced"
  else
    pass "fallback provisioned a fresh PVC (uid changed)"
  fi
fi

# --- whichever path got us here, the pair must be a healthy streaming cluster ---
is_standby_after=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT pg_is_in_recovery()" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
assert_eq "standby is in recovery (streaming from the restored primary)" "t" "${is_standby_after}"
standby_rows_after=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT count(*) FROM pitr_proof" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
# NOTE: this row count is 5000 both BEFORE and AFTER the restore (see the pre-restore assertion
# above), so it proves the standby is queryable -- NOT that it has adopted the restored history.
# A standby still replaying its stale timeline-1 copy passes this. The post-restore write below
# is what actually proves convergence; keep that one as the authoritative gate.
assert_eq "standby serves the restored data (5000 rows)" "5000" "${standby_rows_after}"
# The load-bearing check that it is really attached to THIS primary on the NEW timeline:
# a standby stuck on the old timeline would not appear in pg_stat_replication.
repl_count=""
for _ in $(seq 1 30); do
  repl_count=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM pg_stat_replication" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${repl_count}" = "1" ] && break
  sleep 4
done
assert_eq "primary sees exactly 1 streaming standby" "1" "${repl_count}"
# Read the timeline the way the AGENT reads it (internal/pg/probe.go StandbyTimeline), and poll:
# pg_control_checkpoint() alone is not sufficient and this assertion was relying on an accident.
# A standby that has followed onto a higher timeline BY STREAMING has not necessarily
# checkpointed yet, so its control-file timeline still reads the old value;
# min_recovery_end_timeline is the durable record of the furthest timeline received during
# recovery and advances as the switch is replayed. Under repmgr this passed because the init
# container re-cloned from the restored primary, so the control file carried the new timeline
# straight out of the base backup -- under native the agent rewinds and follows, so it lags until
# the next restartpoint. GREATEST() is exactly what the agent uses, and for the same reason.
#
# pg_stat_wal_receiver.received_tli is folded in as the third source because BOTH control-file
# fields are restartpoint-gated, which made this assertion timing-dependent: the same commit
# passed, failed with actual='1', then passed again on three consecutive native runs. A streaming
# standby knows its timeline the instant the walreceiver reconnects on it, long before that value
# is written to the control file, so received_tli is what converges first and what actually
# answers "did the standby follow onto the new timeline". It is NULL when no walreceiver exists
# (a detached standby), hence the COALESCE -- the control-file terms still carry those cases.
standby_tl=""
for _ in $(seq 1 30); do
  standby_tl=$(pg_exec "${NAMESPACE}" "${STANDBY}" \
    "SELECT GREATEST((SELECT timeline_id FROM pg_control_checkpoint()), COALESCE((SELECT min_recovery_end_timeline FROM pg_control_recovery()), 0), COALESCE((SELECT received_tli FROM pg_stat_wal_receiver), 0))" \
    "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${standby_tl}" = "${tl_after}" ] && break
  sleep 4
done
assert_eq "standby is on the primary's post-restore timeline" "${tl_after}" "${standby_tl}"
# repmgr's own view: one primary, one active standby -- no orphaned/duplicate rows after
# the restore rewrote the primary's identity.
if [ "$(chart_mechanism)" = "native" ]; then
  # #288: a native cluster has no repmgr extension and no repmgr.nodes, so the same
  # "one primary, one active standby, no orphans after the restore rewrote the primary's
  # identity" property is asserted against the primary's own connection list instead.
  streaming=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" "repmgr" "repmgr" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "#288: the restored primary sees exactly one streaming standby" "1" "${streaming}"
  ext=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM pg_extension WHERE extname='repmgr'" "repmgr" "repmgr" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "#288: the restored native cluster has no repmgr extension" "0" "${ext}"
else
  repmgr_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT type, active FROM repmgr.nodes ORDER BY node_id" "repmgr" "repmgr" 2>/dev/null || echo "")
  assert_contains "repmgr sees the restored primary" "${repmgr_rows}" "primary|t"
  assert_contains "repmgr sees an active standby" "${repmgr_rows}" "standby|t"
fi

# --- new writes replicate: the cluster is functional, not just structurally present ---
echo "  write test starts at $(date -u +%H:%M:%SZ) (primary=${PRIMARY} standby=${STANDBY})"
pg_exec "${NAMESPACE}" "${PRIMARY}" "CREATE TABLE post_restore_write (id int)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY}" "INSERT INTO post_restore_write VALUES (42)" "testuser" "testdb" >/dev/null
write_lsn=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_current_wal_lsn()" "postgres" "postgres" 2>/dev/null | tr -d '[:space:]' || echo "?")
echo "  write committed at LSN ${write_lsn}"
replicated=""
# 180s, not 60s. This assertion is the cluster's real post-restore CONVERGENCE gate, and
# convergence after a PITR is slow: measured at 58s of a 60s budget on a passing repmgr run, so
# the old budget failed roughly half the time. The signature is unmistakable in the diagnostics
# below -- the standby reports state='streaming' while its replay LSN sits AHEAD of the primary's
# and frozen (e.g. replay=0/E0000A0 vs a write at 0/D01C6D0), because the restore REWOUND the
# primary and the standby's stale PVC still holds more WAL on the old timeline. The receiver is
# attached; replay simply cannot advance until the rejoin converges it onto the new timeline.
for i in $(seq 1 90); do
  replicated=$(pg_exec "${NAMESPACE}" "${STANDBY}" "SELECT id FROM post_restore_write" "testuser" "testdb" 2>/dev/null | tr -d '[:space:]' || echo "")
  [ "${replicated}" = "42" ] && break
  # Every 5th attempt, say WHERE the standby is. A bare 60s timeout cannot distinguish "the
  # standby is down", "it is up but replaying from the archive instead of streaming", and "it is
  # streaming but stuck" -- and those have completely different causes (#288 review).
  if [ $(( i % 5 )) -eq 0 ]; then
    echo "    t+$(( i * 2 ))s $(date -u +%H:%M:%SZ) standby: $(pg_exec "${NAMESPACE}" "${STANDBY}" \
      "SELECT COALESCE((SELECT status FROM pg_stat_wal_receiver),'no-receiver')||' replay='||COALESCE(pg_last_wal_replay_lsn()::text,'-')||' recv='||COALESCE(pg_last_wal_receive_lsn()::text,'-')" \
      "postgres" "postgres" 2>&1 | tr -d '\n' | tail -c 120)"
  fi
  sleep 2
done
echo "  write test ended at $(date -u +%H:%M:%SZ) after $(( i * 2 ))s; replicated='${replicated}'"
if [ "${replicated}" != "42" ]; then
  # The write itself cannot have failed silently: pg_exec runs psql, which exits 1 on a SQL
  # error, and this script is set -e -- so reaching here means the primary accepted the DDL and
  # the standby did not receive it. Dump both ends before the namespace is torn down, otherwise
  # a CI failure here is unactionable (the pods are gone by the time anyone looks).
  echo "  --- replication diagnostics (standby did not receive the write) ---"
  echo "  primary=${PRIMARY} standby=${STANDBY} mechanism=$(chart_mechanism)"
  echo "  primary pg_stat_replication:"
  pg_exec "${NAMESPACE}" "${PRIMARY}" \
    "SELECT application_name, state, sync_state, sent_lsn, write_lsn, replay_lsn FROM pg_stat_replication" \
    "postgres" "postgres" 2>&1 | sed 's/^/    /' || true
  echo "  primary current LSN / role:"
  pg_exec "${NAMESPACE}" "${PRIMARY}" \
    "SELECT NOT pg_is_in_recovery() AS is_primary, pg_current_wal_lsn()" "postgres" "postgres" 2>&1 | sed 's/^/    /' || true
  echo "  standby receiver / replay position:"
  pg_exec "${NAMESPACE}" "${STANDBY}" \
    "SELECT status, sender_host, received_tli, latest_end_lsn FROM pg_stat_wal_receiver" \
    "postgres" "postgres" 2>&1 | sed 's/^/    /' || true
  pg_exec "${NAMESPACE}" "${STANDBY}" \
    "SELECT pg_is_in_recovery(), pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn(), pg_is_wal_replay_paused()" \
    "postgres" "postgres" 2>&1 | sed 's/^/    /' || true
  echo "  standby agent log (tail):"
  kubectl logs -n "${NAMESPACE}" "${STANDBY}" -c postgresql --tail=25 2>&1 | sed 's/^/    /' || true
fi
assert_eq "post-restore writes replicate to the standby" "42" "${replicated}"

end_suite
print_summary
