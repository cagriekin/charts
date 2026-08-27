#!/bin/bash
# In-place migration of a LIVE repmgr cluster to the native mechanism (#292).
#
# This is the one suite that starts from a RELEASED chart rather than the local one. Everything
# else in the repmgr exit is greenfield -- a cluster created by 2.0.0 never sees repmgr -- but
# every existing consumer has repmgr state on disk, and `helm upgrade` must not re-clone them.
# On a multi-terabyte cluster a re-clone is hours of degraded HA and a real RPO window, so
# "it converges eventually" is not the same as "it migrated".
#
# What is on disk in a 1.x cluster, and therefore what this exercises:
#   - `shared_preload_libraries = 'repmgr'` INSIDE PGDATA (written at initdb by the old image,
#     cloned to every standby). The 2.0.0 image does not ship repmgr.so, so without #293's
#     strip every pod crash-loops at once and `helm rollback` does not fix it -- the line is in
#     the data directory, not the release.
#   - the repmgr database, role, extension and populated `repmgr.nodes` rows
#   - active `repmgr_slot_<node_id>` physical slots pinning WAL (node_id = ordinal + 1000)
#   - `primary_conninfo` written by `repmgr standby clone|follow`, in postgresql.auto.conf,
#     which PostgreSQL reads AFTER every include and therefore outranks the agent's fragment
#
# The proof of "no re-clone" is a SENTINEL FILE written into each pod's PGDATA before the
# upgrade. pg_basebackup wipes the target directory, so a surviving sentinel is direct evidence
# rather than an inference from timing or log scraping. System identifier and timeline are
# checked too: the identifier catches a re-initdb, the timeline catches an unnecessary promote.
#
# Standalone/opt-in (long-running): `make -C pg test-migrate-native`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-migrate-native}"
RELEASE="${RELEASE:-pgmignative}"
VALUES="${SCRIPT_DIR}/values-migrate-native.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
LEASE="${FULLNAME}-leader"
NODES=3
PGDATA_DIR="/var/lib/postgresql/data/pgdata"
# pg_controldata is NOT on PATH in the container -- the image keeps the server binaries in the
# versioned bindir, which is why the agent has a PGBindir() helper at all. Call it by full path.
PG_BINDIR_FMT="/usr/lib/postgresql/%s/bin"
SENTINEL="${PGDATA_DIR}/.migrate-native-sentinel"

# The released chart to migrate FROM. Pinned rather than "latest" so the suite tests a known
# starting shape; bump it deliberately when a newer 1.x becomes the realistic upgrade source.
FROM_CHART_VERSION="${FROM_CHART_VERSION:-1.17.0}"
HELM_REPO_NAME="${HELM_REPO_NAME:-cagriekin}"
HELM_REPO_URL="${HELM_REPO_URL:-https://cagriekin.github.io/charts}"

# The released IMAGE to migrate from. It must (a) contain repmgr, which the #290 image does not,
# and (b) NOT be the tag CI builds locally, or the freshly-built repmgr-free image loaded into
# KinD would shadow it and the "1.x" phase would run 2.0.0's image -- making the whole suite
# assert nothing. -32 is published for both majors and the chart pins -33, so they cannot
# collide. Derived per major here rather than pinned in the fixture on purpose: a fixture pin
# would fall under set-pg-major.sh's tag rewrite, which would drag the "from" image forward to
# the chart's own tag and destroy the premise.
FROM_IMAGE_BASE="${FROM_IMAGE_BASE:-trixie-5.5.0-32}"   # set-pg-major: keep (an older RELEASED image is the point)
PG_MAJOR=$(awk '/^(repmgr|ha):/{r=1} r&&/^    majorVersion:/{gsub(/"/,"",$2); print $2; exit}' "${CHART_DIR}/values.yaml")
[ -n "${PG_MAJOR}" ] || { echo "could not resolve majorVersion from ${CHART_DIR}/values.yaml" >&2; exit 1; }
# Normalize rather than append: set-pg-major.sh rewrites the keep-marked line above to
# <base>-pg<major>, so on a switched tree FROM_IMAGE_BASE already carries a suffix and a bare
# append would produce trixie-5.5.0-32-pg18-pg18. Strip then append, so both a clean checkout
# and a switched tree land on the same tag.
FROM_IMAGE_TAG="${FROM_IMAGE_BASE%-pg[0-9]*}-pg${PG_MAJOR}"
PG_BINDIR=$(printf "${PG_BINDIR_FMT}" "${PG_MAJOR}")
CONTROLDATA="${PG_BINDIR}/pg_controldata"

begin_suite "Migration: live repmgr cluster -> native, in place, no re-clone (#292)"

echo "  from: chart ${HELM_REPO_NAME}/pg ${FROM_CHART_VERSION}, image cagriekin/repmgr:${FROM_IMAGE_TAG}"
echo "  to:   local chart, image as pinned by ${CHART_DIR}/values.yaml"

kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout=5m
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm repo add "${HELM_REPO_NAME}" "${HELM_REPO_URL}" >/dev/null 2>&1 || true
helm repo update "${HELM_REPO_NAME}" >/dev/null

# ---------------------------------------------------------------------------
# Phase 1: a real 1.x cluster, repmgr-shaped on disk.
# ---------------------------------------------------------------------------
echo ""
echo "  Phase 1: installing the released ${FROM_CHART_VERSION} chart..."
helm upgrade --install "${RELEASE}" "${HELM_REPO_NAME}/pg" \
  --version "${FROM_CHART_VERSION}" \
  -n "${NAMESPACE}" -f "${VALUES}" \
  --set "repmgr.image.tag=${FROM_IMAGE_TAG}" \
  --set "repmgr.image.majorVersion=${PG_MAJOR}" \
  --set "postgresql.majorVersion=${PG_MAJOR}" \
  --wait --timeout 12m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" "${NODES}" 900

PRIMARY_BEFORE=$(discover_primary "${NAMESPACE}" "${FULLNAME}" "${NODES}")
[ -n "${PRIMARY_BEFORE}" ] || { echo "FATAL: no primary in the 1.x cluster" >&2; exit 1; }
echo "  primary (1.x): ${PRIMARY_BEFORE}"

# Prove the starting state really is repmgr-shaped. Without these the suite could pass on a
# cluster that was never repmgr in the first place, which is the failure mode that matters:
# a green run that proves nothing.
# Polled, not sampled once. Registration in repmgr.nodes is asynchronous relative to pod
# READINESS -- agent readiness is replication-aware, not registration-aware -- so a single read
# right after `helm --wait` is a race. It lost once, reading 1 row of 3 (#292 review); every
# other run read 3. The starting state being repmgr-shaped is the point of these checks, not how
# quickly it gets there, so waiting costs nothing and removes a flake that would fire in CI.
nodes_rows=""; waited=0
while [ ${waited} -lt 300 ]; do
  nodes_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" "SELECT count(*) FROM repmgr.nodes" "repmgr" "repmgr" | tr -d '[:space:]')
  [ "${nodes_rows}" = "${NODES}" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "1.x: repmgr.nodes is populated (${NODES} rows)" "${NODES}" "${nodes_rows}"

# Same race: a slot is created when the standby attaches, which trails readiness.
legacy_slots=0; waited=0
while [ ${waited} -lt 300 ]; do
  legacy_slots=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
    "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'repmgr_slot_%' AND active" | tr -d '[:space:]')
  [ "${legacy_slots}" = "$((NODES - 1))" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "1.x: a legacy repmgr slot is active for every standby" "$((NODES - 1))" "${legacy_slots}"

preload_before=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" "SHOW shared_preload_libraries")
assert_contains "1.x: shared_preload_libraries includes repmgr" "${preload_before}" "repmgr"

# The line is in PGDATA, not just in the running config -- that is what makes it survive a
# chart change, and what #293 has to strip.
preload_on_disk=$(kubectl exec -n "${NAMESPACE}" "${PRIMARY_BEFORE}" -c postgresql -- \
  grep -c "^[[:space:]]*shared_preload_libraries" "${PGDATA_DIR}/postgresql.conf" 2>/dev/null || echo 0)
assert_gt "1.x: the preload GUC is written into PGDATA's postgresql.conf" "${preload_on_disk}" 0

# ---------------------------------------------------------------------------
# Data + identity + the no-re-clone sentinel, captured per pod.
# ---------------------------------------------------------------------------
MV="migrate-native-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "CREATE TABLE IF NOT EXISTS migrate_native (id serial PRIMARY KEY, value text)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "INSERT INTO migrate_native (value) VALUES ('${MV}')" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" "CHECKPOINT" "postgres" "postgres" >/dev/null 2>&1 || true

declare -A SYSID_BEFORE TLI_BEFORE TLI_AFTER
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  # The sentinel is the direct evidence: pg_basebackup empties the target directory, so if this
  # file is still here afterwards the standby was not re-cloned. No log scraping, no timing.
  kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "printf '%s\n' '${MV}' > '${SENTINEL}'"
  ctl=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- "${CONTROLDATA}" "${PGDATA_DIR}")
  SYSID_BEFORE[$i]=$(printf '%s\n' "${ctl}" | awk -F: '/Database system identifier/{gsub(/ /,"",$2); print $2}')
  TLI_BEFORE[$i]=$(printf '%s\n' "${ctl}" | awk -F: '/Latest checkpoint.s TimeLineID/{gsub(/ /,"",$2); print $2}')
  [ -n "${SYSID_BEFORE[$i]}" ] || { echo "FATAL: no system identifier for ${pod}" >&2; exit 1; }
  echo "  ${pod}: sysid=${SYSID_BEFORE[$i]} tli=${TLI_BEFORE[$i]} sentinel written"
done

# Every standby must be streaming before we touch anything: the migration refuses a degraded
# cluster, and starting from one would make a failure ambiguous.
streaming_before=0; waited=0
while [ ${waited} -lt 300 ]; do
  streaming_before=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" | tr -d '[:space:]')
  [ "${streaming_before}" = "$((NODES - 1))" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "1.x: every standby is streaming before the migration" "$((NODES - 1))" "${streaming_before}"

# ---------------------------------------------------------------------------
# Phase 2: THE MIGRATION -- helm upgrade to the local chart.
#
# podManagementPolicy is Parallel in both (agent has been the 1.x default since 1.0.0), so no
# --cascade=orphan recreate is needed here; that is the repmgrd->agent migration, a different
# and already-removed path. The StatefulSet rolls pods highest-ordinal first, which is the
# standbys-before-primary order the issue asks for, for free.
# ---------------------------------------------------------------------------
echo ""
echo "  Phase 2: helm upgrade to the local chart (native mechanism, repmgr-free image)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 15m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" "${NODES}" 900

# Settle on a single primary that is also the lease holder. A migration that produced two
# writers, or a primary that is not the holder, is the one outcome no data check would catch.
PRIMARY=""; HOLDER=""; waited=0
while [ ${waited} -lt 600 ]; do
  HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" "${NODES}")
  if [ -n "${HOLDER}" ] && [ -n "${PRIMARY}" ] && [ "${HOLDER}" = "${PRIMARY}" ]; then break; fi
  sleep 10; waited=$((waited + 10))
done
assert_eq "post: the primary is the lease holder" "${HOLDER}" "${PRIMARY}"
[ -n "${PRIMARY}" ] || { echo "FATAL: no primary after the migration" >&2; exit 1; }

# Exactly one writer.
writers=0
for i in $(seq 0 $((NODES - 1))); do
  rec=$(pg_exec "${NAMESPACE}" "${FULLNAME}-${i}" "SELECT pg_is_in_recovery()" | tr -d '[:space:]')
  [ "${rec}" = "f" ] && writers=$((writers + 1))
done
assert_eq "post: exactly one writer (no split brain through the migration)" "1" "${writers}"

# ---------------------------------------------------------------------------
# The claims that make this a migration rather than a rebuild.
# ---------------------------------------------------------------------------
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  sent=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    cat "${SENTINEL}" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "post ${pod}: PGDATA sentinel survived (no re-clone)" "${MV}" "${sent}"

  ctl=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- "${CONTROLDATA}" "${PGDATA_DIR}")
  sysid=$(printf '%s\n' "${ctl}" | awk -F: '/Database system identifier/{gsub(/ /,"",$2); print $2}')
  tli=$(printf '%s\n' "${ctl}" | awk -F: '/Latest checkpoint.s TimeLineID/{gsub(/ /,"",$2); print $2}')
  assert_eq "post ${pod}: system identifier unchanged (no re-initdb)" "${SYSID_BEFORE[$i]}" "${sysid}"
  # NOT "timeline unchanged". That was this suite's first assertion and it was simply wrong:
  # a rolling upgrade replaces the PRIMARY's pod too, so the lease has to move and whoever
  # takes it promotes, which bumps the timeline by definition. Observed on the first live run:
  # TLI 1 -> 3 across the roll (two handoffs, since the pod that first took the lease was
  # itself still pending an update), with all three nodes ending consistent.
  #
  # What the issue actually needs from the timeline is that nobody was REWOUND or re-cloned to
  # get there, and that no node is stranded on an older one. The system identifier above covers
  # re-initdb, the sentinel covers re-clone, the .diverged check covers pg_rewind -- and the
  # cross-node consistency check after this loop covers stranding, which is the failure a
  # per-node "unchanged" test cannot see anyway.
  TLI_AFTER[$i]="${tli}"
  assert_eq "post ${pod}: timeline did not go backwards" "yes" \
    "$([ "${tli}" -ge "${TLI_BEFORE[$i]}" ] && echo yes || echo no)"

  # pg_rewind and a failed clone both leave these behind.
  # native.go names it "<datadir>.diverged.<ts>" -- a SIBLING of PGDATA, not a dotfile inside
  # its parent. An earlier draft globbed ${PGDATA_DIR}/../.diverged.*, which matches nothing and
  # would have asserted 0 forever.
  diverged=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "ls -d ${PGDATA_DIR}.diverged.* 2>/dev/null | wc -l" | tr -d '[:space:]')
  assert_eq "post ${pod}: no .diverged.* directory" "0" "${diverged}"
done

# Nobody stranded on an older timeline -- the failure a per-node "unchanged" check cannot see,
# because a standby that cannot follow the new timeline is one that will eventually need the
# re-clone this whole migration exists to avoid.
#
# Measured with pg_stat_wal_receiver.received_tli, NOT pg_controldata. This suite first compared
# pg_controldata across nodes and failed with "got: 1 2" on a perfectly healthy cluster:
# "Latest checkpoint's TimeLineID" is as of the last CHECKPOINT, so a standby that has switched
# timelines but not yet checkpointed still reports the old one. Live proof from that run --
# pod-2 read checkpoint_tli=2 while received_tli=4 and the primary listed it as streaming. The
# receiver's view is the authoritative "what timeline am I actually following".
# Both sides use the same GREATEST() of the three timeline sources, the pattern
# test-pgbackrest-restore-ha.sh already established. Reading the primary from
# pg_control_checkpoint() alone was wrong for exactly the reason the standby side avoids it: on
# promotion PostgreSQL writes XLOG_END_OF_RECOVERY and only REQUESTS the checkpoint, so a
# just-promoted primary can still report the pre-promotion TLI while every standby correctly
# reports the new one -- which would fail every standby on a healthy cluster.
tl_expr="SELECT GREATEST((SELECT timeline_id FROM pg_control_checkpoint()), COALESCE((SELECT min_recovery_end_timeline FROM pg_control_recovery()), 0), COALESCE((SELECT received_tli FROM pg_stat_wal_receiver), 0))"
primary_tli=""
for _ in $(seq 1 30); do
  primary_tli=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "${tl_expr}" | tr -d '[:space:]')
  [ -n "${primary_tli}" ] && [ "${primary_tli}" != "0" ] && break
  sleep 4
done
assert_eq "post: the primary's timeline is resolvable" "yes" \
  "$([ -n "${primary_tli}" ] && [ "${primary_tli}" != "0" ] && echo yes || echo no)"
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  [ "${pod}" = "${PRIMARY}" ] && continue
  node_tli=""
  for _ in $(seq 1 30); do
    node_tli=$(pg_exec "${NAMESPACE}" "${pod}" "${tl_expr}" | tr -d '[:space:]')
    [ "${node_tli}" = "${primary_tli}" ] && break
    sleep 4
  done
  assert_eq "post ${pod}: follows the primary's timeline (${primary_tli}), not stranded" \
    "${primary_tli}" "${node_tli}"
done

# #293: the preload line must be gone from PGDATA, on every node -- not merely absent from the
# running config, or the next restart would resurrect it.
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  left=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "grep -c \"^[[:space:]]*shared_preload_libraries.*repmgr\" ${PGDATA_DIR}/postgresql.conf 2>/dev/null || true" | tr -d '[:space:]')
  assert_eq "post ${pod}: repmgr stripped from PGDATA's postgresql.conf (#293)" "0" "${left:-0}"
  auto_left=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "grep -c \"^[[:space:]]*\\(primary_conninfo\\|primary_slot_name\\)\" ${PGDATA_DIR}/postgresql.auto.conf 2>/dev/null || true" | tr -d '[:space:]')
  assert_eq "post ${pod}: repmgr recovery config cleared from auto.conf (#292)" "0" "${auto_left:-0}"
done

running_preload=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SHOW shared_preload_libraries")
assert_not_contains "post: the running config no longer preloads repmgr" "${running_preload}" "repmgr"

# Data survived, and the cluster still accepts writes.
rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "SELECT count(*) FROM migrate_native WHERE value='${MV}'" "testuser" "testdb" | tr -d '[:space:]')
assert_eq "post: pre-migration data is present" "1" "${rows}"
pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "INSERT INTO migrate_native (value) VALUES ('post-${MV}')" "testuser" "testdb" >/dev/null
post_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM migrate_native" "testuser" "testdb" | tr -d '[:space:]')
assert_eq "post: the migrated primary accepts writes" "2" "${post_rows}"

# Replication re-established on every standby, through the NATIVE naming.
streaming=0; waited=0
while [ ${waited} -lt 480 ]; do
  streaming=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" | tr -d '[:space:]')
  [ "${streaming}" = "$((NODES - 1))" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "post: every standby is streaming again" "$((NODES - 1))" "${streaming}"

# Slots: the native ones exist and are active, and no legacy slot is left pinning WAL. A
# legacy slot that is merely INACTIVE is the dangerous residue -- it pins WAL forever and
# raises no error until the volume fills.
native_active=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'pg_ha_slot_%' AND active" | tr -d '[:space:]')
assert_eq "post: a native slot is active for every standby" "$((NODES - 1))" "${native_active}"

# Legacy-slot residue is checked on EVERY node, not just the current primary -- and that is the
# whole point of the check. Legacy repmgr_slot_* slots only ever existed on the 1.x PRIMARY, and
# the roll necessarily moves the lease, so the post-migration primary is usually a node that
# never had one. Querying it alone made "no legacy slot survived" pass by construction while a
# DEMOTED ex-primary could still hold inactive repmgr_slot_* pinning WAL on its own volume --
# which is precisely the residue reclaimableOnStandby() exists to clear (#292 review).
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  slot_legacy=""; waited=0
  # Reclaim happens on the owning node's own tick, so give it a few cycles rather than racing it.
  while [ ${waited} -lt 180 ]; do
    slot_legacy=$(pg_exec "${NAMESPACE}" "${pod}" \
      "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'repmgr_slot_%'" | tr -d '[:space:]')
    [ "${slot_legacy}" = "0" ] && break
    sleep 10; waited=$((waited + 10))
  done
  assert_eq "post ${pod}: no legacy repmgr slot survived (no orphan pinning WAL)" "0" "${slot_legacy}"
  slot_inactive=$(pg_exec "${NAMESPACE}" "${pod}" \
    "SELECT count(*) FROM pg_replication_slots WHERE NOT active" | tr -d '[:space:]')
  assert_eq "post ${pod}: no inactive slot" "0" "${slot_inactive}"
done

# ...and explicitly on the node that WAS the 1.x primary, named rather than inferred, so the
# coverage cannot be lost to a future refactor of the loop above.
exprimary_legacy=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'repmgr_slot_%'" | tr -d '[:space:]')
assert_eq "post: the 1.x primary (${PRIMARY_BEFORE}) kept no legacy slot" "0" "${exprimary_legacy}"

# The catalog is deliberately NOT cleaned up by the upgrade: dropping the extension, database
# and role is irreversible and must stay an operator decision (#292). Assert it is still there,
# so an accidental automatic cleanup would fail this suite rather than surprise a consumer.
ext_left=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "SELECT count(*) FROM pg_extension WHERE extname='repmgr'" "repmgr" "repmgr" | tr -d '[:space:]')
assert_eq "post: the repmgr extension is left in place (cleanup is opt-in)" "1" "${ext_left}"
role_left=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "SELECT count(*) FROM pg_roles WHERE rolname='repmgr'" | tr -d '[:space:]')
assert_eq "post: the repmgr role is left in place (the agent authenticates as it)" "1" "${role_left}"

# Idempotence: the migration runs on every boot, so a second roll must be a no-op rather than a
# second migration. This is the crash-safety property the issue asks for, in its cheapest form.
echo ""
echo "  Phase 3: rolling the pods again -- the migration must be a no-op the second time..."
kubectl rollout restart statefulset "${FULLNAME}" -n "${NAMESPACE}"
kubectl rollout status statefulset "${FULLNAME}" -n "${NAMESPACE}" --timeout=15m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" "${NODES}" 900

PRIMARY2=$(discover_primary "${NAMESPACE}" "${FULLNAME}" "${NODES}")
[ -n "${PRIMARY2}" ] || { echo "FATAL: no primary after the second roll" >&2; exit 1; }
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  ctl=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- "${CONTROLDATA}" "${PGDATA_DIR}")
  sysid=$(printf '%s\n' "${ctl}" | awk -F: '/Database system identifier/{gsub(/ /,"",$2); print $2}')
  assert_eq "re-roll ${pod}: system identifier still unchanged" "${SYSID_BEFORE[$i]}" "${sysid}"
  sent=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    cat "${SENTINEL}" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "re-roll ${pod}: sentinel still present (still no re-clone)" "${MV}" "${sent}"
done
rows2=$(pg_exec "${NAMESPACE}" "${PRIMARY2}" "SELECT count(*) FROM migrate_native" "testuser" "testdb" | tr -d '[:space:]')
assert_eq "re-roll: data intact" "2" "${rows2}"

end_suite
print_summary
