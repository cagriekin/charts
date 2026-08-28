#!/bin/bash
# The documented `failoverMode: repmgrd` (chart 1.x) -> 2.0.0 upgrade (#298 review).
#
# Why this suite exists: this is the only 2.0.0 upgrade path that RECREATES a live StatefulSet,
# it is the one every remaining repmgrd consumer must follow, and until now nothing tested it.
# test-migrate-native.sh covers agent(1.x) -> agent(2.0.0) and says so in its own comments --
# "podManagementPolicy is Parallel in both, so no --cascade=orphan recreate is needed here; that
# is the repmgrd->agent migration, a different thing". That different thing is this file.
#
# The runbook under test is the one printed in pg/README.md and both CHANGELOGs:
#
#   1. kubectl delete statefulset <release>-pg --cascade=orphan   # pods + PVCs keep running
#   2. helm upgrade ... minus every removed key                   # recreates STS as Parallel,
#                                                                 # adopts the orphaned pods
#   3. verify the Lease holder and the write Service
#
# What makes it dangerous, and therefore what is asserted:
#   - `podManagementPolicy` is IMMUTABLE. repmgrd mode rendered OrderedReady; the agent needs
#     Parallel. That is the only reason the recreate exists.
#   - Between step 1 and step 2 the pods are ORPHANED -- running with no controller. A recreate
#     that fails to adopt them does not error; it creates NEW pods alongside, and the operator
#     discovers it as a duplicate primary. Pod UIDs before/after are the direct evidence, so
#     adoption is proven rather than inferred from "the pods are Ready".
#   - The cluster crosses from a repmgrd-driven failover model to a lease-driven one WHILE its
#     data directory is still repmgr-shaped, so it inherits every #292/#293 hazard on top.
#
# Standalone/opt-in (long-running): `make -C pg test-migrate-repmgrd`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-migrate-repmgrd}"
RELEASE="${RELEASE:-pgmigrepmgrd}"
VALUES="${SCRIPT_DIR}/values-migrate-repmgrd.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")
LEASE="${FULLNAME}-leader"
NODES=3
PGDATA_DIR="/var/lib/postgresql/data/pgdata"
PG_BINDIR_FMT="/usr/lib/postgresql/%s/bin"
SENTINEL="${PGDATA_DIR}/.migrate-repmgrd-sentinel"

# The released chart to migrate FROM. Pinned, not "latest": the suite tests a known starting
# shape. Bump deliberately when a newer 1.x becomes the realistic upgrade source.
FROM_CHART_VERSION="${FROM_CHART_VERSION:-1.17.0}"
HELM_REPO_NAME="${HELM_REPO_NAME:-cagriekin}"
HELM_REPO_URL="${HELM_REPO_URL:-https://cagriekin.github.io/charts}"

# The released IMAGE to migrate from. It must contain repmgrd AND the service-updater scripts --
# 2.0.0's image has neither -- and it must NOT be a tag CI builds locally, or the freshly-built
# image loaded into KinD would shadow it and the "1.x" phase would silently run 2.0.0. Same tag
# and same reasoning as test-migrate-native.sh.
FROM_IMAGE_BASE="${FROM_IMAGE_BASE:-trixie-5.5.0-32-pg17}"   # set-pg-major: keep (an older RELEASED image is the point)
PG_MAJOR=$(awk '/^(repmgr|ha):/{r=1} r&&/^    majorVersion:/{gsub(/"/,"",$2); print $2; exit}' "${CHART_DIR}/values.yaml")
[ -n "${PG_MAJOR}" ] || { echo "could not resolve majorVersion from ${CHART_DIR}/values.yaml" >&2; exit 1; }
FROM_IMAGE_TAG="${FROM_IMAGE_BASE%-pg[0-9]*}-pg${PG_MAJOR}"
PG_BINDIR=$(printf "${PG_BINDIR_FMT}" "${PG_MAJOR}")
CONTROLDATA="${PG_BINDIR}/pg_controldata"

begin_suite "Migration: failoverMode repmgrd (1.x) -> 2.0.0, orphan-recreate, pods adopted"

echo "  from: chart ${HELM_REPO_NAME}/pg ${FROM_CHART_VERSION} (failoverMode=repmgrd), image cagriekin/repmgr:${FROM_IMAGE_TAG}"
echo "  to:   local chart, image as pinned by ${CHART_DIR}/values.yaml"

kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout=5m
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm repo add "${HELM_REPO_NAME}" "${HELM_REPO_URL}" >/dev/null 2>&1 || true
helm repo update "${HELM_REPO_NAME}" >/dev/null

# ---------------------------------------------------------------------------
# Phase 1: a real repmgrd-mode 1.x cluster.
# ---------------------------------------------------------------------------
echo ""
echo "  Phase 1: installing the released ${FROM_CHART_VERSION} chart with failoverMode=repmgrd..."
helm upgrade --install "${RELEASE}" "${HELM_REPO_NAME}/pg" \
  --version "${FROM_CHART_VERSION}" \
  -n "${NAMESPACE}" -f "${VALUES}" \
  --set "repmgr.failoverMode=repmgrd" \
  --set "repmgr.image.tag=${FROM_IMAGE_TAG}" \
  --set "repmgr.image.majorVersion=${PG_MAJOR}" \
  --set "postgresql.majorVersion=${PG_MAJOR}" \
  --wait --timeout 15m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" "${NODES}" 900

# Prove the starting shape really is repmgrd mode. Without these the suite could pass against an
# agent-mode cluster, which would make every assertion below meaningless -- a green run proving
# nothing, the exact failure this whole suite exists to close.
pmp_before=$(kubectl get statefulset "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.podManagementPolicy}')
assert_eq "1.x: podManagementPolicy is OrderedReady (the immutable field forcing the recreate)" \
  "OrderedReady" "${pmp_before}"

for c in repmgrd service-updater; do
  present=$(kubectl get pod "${FULLNAME}-0" -n "${NAMESPACE}" \
    -o jsonpath="{.spec.containers[?(@.name=='${c}')].name}" | tr -d '[:space:]')
  assert_eq "1.x: the ${c} sidecar is running" "${c}" "${present}"
done

# No Lease in repmgrd mode: repmgrd itself decides failover there. Its ABSENCE is what makes
# "the Lease exists and has a holder" after the upgrade a real transition rather than a value
# that was already true.
lease_before=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_eq "1.x: no leader Lease exists yet (repmgrd owns failover)" "" "${lease_before}"

PRIMARY_BEFORE=$(discover_primary "${NAMESPACE}" "${FULLNAME}" "${NODES}")
[ -n "${PRIMARY_BEFORE}" ] || { echo "FATAL: no primary in the 1.x cluster" >&2; exit 1; }
echo "  primary (1.x, repmgrd): ${PRIMARY_BEFORE}"

# Polled, not sampled once: registration in repmgr.nodes trails pod READINESS, so a single read
# straight after `helm --wait` is a race that test-migrate-native.sh has already lost once.
nodes_rows=""; waited=0
while [ ${waited} -lt 300 ]; do
  nodes_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" "SELECT count(*) FROM repmgr.nodes" "repmgr" "repmgr" | tr -d '[:space:]' || echo "")
  [ "${nodes_rows}" = "${NODES}" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "1.x: repmgr.nodes is populated (${NODES} rows)" "${NODES}" "${nodes_rows}"

streaming_before=0; waited=0
while [ ${waited} -lt 300 ]; do
  streaming_before=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" | tr -d '[:space:]' || echo "")
  [ "${streaming_before}" = "$((NODES - 1))" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "1.x: every standby is streaming before the migration" "$((NODES - 1))" "${streaming_before}"

# ---------------------------------------------------------------------------
# Data, identity, PVC and pod UIDs -- captured before anything is touched.
#
# The pod UID is the load-bearing capture. "Zero data loss -- pods and PVCs are kept" is the
# runbook's central promise, and a recreate that failed to adopt would leave the data intact
# while replacing the pods, so no data check can tell the two apart.
# ---------------------------------------------------------------------------
MV="migrate-repmgrd-$(date +%s)"
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "CREATE TABLE IF NOT EXISTS migrate_repmgrd (id serial PRIMARY KEY, value text)" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "INSERT INTO migrate_repmgrd (value) VALUES ('${MV}')" "testuser" "testdb" >/dev/null
pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" "CHECKPOINT" "postgres" "postgres" >/dev/null 2>&1 || true

declare -A POD_UID_BEFORE PVC_UID_BEFORE SYSID_BEFORE
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  POD_UID_BEFORE[$i]=$(kubectl get pod "${pod}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}')
  PVC_UID_BEFORE[$i]=$(kubectl get pvc "data-${pod}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}')
  kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "printf '%s\n' '${MV}' > '${SENTINEL}'"
  ctl=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- "${CONTROLDATA}" "${PGDATA_DIR}")
  SYSID_BEFORE[$i]=$(printf '%s\n' "${ctl}" | awk -F: '/Database system identifier/{gsub(/ /,"",$2); print $2}')
  [ -n "${SYSID_BEFORE[$i]}" ] || { echo "FATAL: no system identifier for ${pod}" >&2; exit 1; }
  echo "  ${pod}: uid=${POD_UID_BEFORE[$i]:0:8} pvc=${PVC_UID_BEFORE[$i]:0:8} sysid=${SYSID_BEFORE[$i]}"
done

# ---------------------------------------------------------------------------
# Step 0 of the runbook, asserted rather than assumed: the upgrade MUST fail while a removed
# key is still in the values. This is what makes "delete the keys first" an instruction rather
# than advice -- and it runs before the destructive step, so an operator who skips it learns at
# render time with their cluster untouched.
# ---------------------------------------------------------------------------
echo ""
echo "  Runbook step 0: the upgrade must refuse to render while a removed key is set..."
for k in repmgr.failoverMode=repmgrd repmgr.serviceUpdater.enabled=true \
         repmgr.monitoringHistoryDays=7 repmgr.splitBrainDetection.action=log \
         pgpool.autoFailback=true; do
  if helm template "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" -f "${VALUES}" \
       --set "${k}" >/dev/null 2>&1; then
    fail "removed key ${k} still renders in 2.0.0 (the runbook's 'delete the keys' step is unenforced)"
  else
    pass "2.0.0 refuses to render with ${k} still set"
  fi
done

# ---------------------------------------------------------------------------
# Step 1: orphan-delete the StatefulSet. The pods and PVCs must survive with no controller.
# ---------------------------------------------------------------------------
echo ""
echo "  Runbook step 1: kubectl delete statefulset --cascade=orphan..."
kubectl delete statefulset "${FULLNAME}" -n "${NAMESPACE}" --cascade=orphan

sts_gone=$(kubectl get statefulset "${FULLNAME}" -n "${NAMESPACE}" -o name 2>/dev/null || echo "")
assert_eq "orphaned: the StatefulSet is gone" "" "${sts_gone}"

# The pods must still be RUNNING, not merely present: the promise is that the database stays up
# through the recreate. A pod that is Terminating here means the delete was not --cascade=orphan.
orphan_running=0
for i in $(seq 0 $((NODES - 1))); do
  phase=$(kubectl get pod "${FULLNAME}-${i}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  [ "${phase}" = "Running" ] && orphan_running=$((orphan_running + 1))
done
assert_eq "orphaned: every pod is still Running with no controller" "${NODES}" "${orphan_running}"

# And still serving. This is the window an operator is most exposed in, so prove the database
# is actually up rather than that the pod object exists.
orphan_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY_BEFORE}" \
  "SELECT count(*) FROM migrate_repmgrd WHERE value='${MV}'" "testuser" "testdb" | tr -d '[:space:]' || echo "")
assert_eq "orphaned: the primary still serves queries" "1" "${orphan_rows}"

for i in $(seq 0 $((NODES - 1))); do
  uid=$(kubectl get pod "${FULLNAME}-${i}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  assert_eq "orphaned: ${FULLNAME}-${i} is the same pod (uid unchanged)" "${POD_UID_BEFORE[$i]}" "${uid}"
done

# ---------------------------------------------------------------------------
# Step 2: helm upgrade to the local chart -- the recreate that has to ADOPT.
#
# No --wait. The recreated StatefulSet adopts pods that are already Running, and the agent then
# has to take over a cluster whose data directory is still repmgr-shaped; helm's readiness view
# during that adoption is not the signal worth blocking on, and a --wait timeout here would
# report "upgrade failed" for what is really a slow convergence. The explicit waits below are
# the ones that matter.
# ---------------------------------------------------------------------------
echo ""
echo "  Runbook step 2: helm upgrade to the local chart (agent mode, Parallel STS)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" -f "${VALUES}" --timeout 15m

pmp_after=""; waited=0
while [ ${waited} -lt 120 ]; do
  pmp_after=$(kubectl get statefulset "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.podManagementPolicy}' 2>/dev/null || echo "")
  [ -n "${pmp_after}" ] && break
  sleep 5; waited=$((waited + 5))
done
assert_eq "recreated: podManagementPolicy is now Parallel" "Parallel" "${pmp_after}"

# THE assertion this suite exists for: the recreated controller adopted the orphans instead of
# building new pods beside them. Checked BEFORE waiting for readiness, so a failure here is
# reported as "not adopted" rather than as a readiness timeout ten minutes later.
# Read in ONE snapshot: the roll starts within seconds of the upgrade, and per-pod `kubectl get`
# calls would sample different moments of it. Both facts come from the same snapshot so
# "adopted" and "still the original pod" describe the same instant.
snap=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/component=postgresql \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.uid}{" "}{.metadata.ownerReferences[?(@.kind=="StatefulSet")].name}{"\n"}{end}')
adopted=0; retained=0
for i in $(seq 0 $((NODES - 1))); do
  line=$(printf '%s\n' "${snap}" | awk -v p="${FULLNAME}-${i}" '$1==p')
  owner=$(printf '%s' "${line}" | awk '{print $3}')
  uid=$(printf '%s' "${line}" | awk '{print $2}')
  [ "${owner}" = "${FULLNAME}" ] && adopted=$((adopted + 1))
  [ "${uid}" = "${POD_UID_BEFORE[$i]}" ] && retained=$((retained + 1))
done
assert_eq "recreated: every pod is owned by the new StatefulSet" "${NODES}" "${adopted}"
# The pods the new controller took over are the ORIGINAL ones -- this is what distinguishes
# adoption from "the orphans died and were rebuilt". Not all ${NODES} necessarily: the roll onto
# the 2.0.0 spec begins immediately and legitimately replaces pods, so a pod that has already
# been rolled has a new UID through no fault of the adoption. Requiring the primary's ordinal to
# still be original would be a coin flip; requiring at least one is not, because a failed
# adoption replaces ALL of them at once.
assert_gt "recreated: the adopted pods are the original ones (uid retained, not rebuilt)" "${retained}" 0
echo "  adopted ${adopted}/${NODES}, of which ${retained} still carry their original pod UID"

# No extra pods: an adoption failure shows up as duplicates, and a duplicate ordinal-0 on the
# same PVC is the worst outcome this flow can produce.
pod_count=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/component=postgresql \
  --no-headers 2>/dev/null | wc -l | tr -d '[:space:]')
assert_eq "recreated: exactly ${NODES} postgresql pods (no duplicates alongside the orphans)" "${NODES}" "${pod_count}"

# ---------------------------------------------------------------------------
# Now wait for the ROLL, and wait for it explicitly.
#
# Adoption is not the end of the upgrade, and conflating the two cost this suite its first run.
# The adopted pods still carry the 1.x pod spec -- old image, repmgrd and service-updater
# sidecars, `entrypoint.sh postgres` -- so the StatefulSet immediately rolls every one of them
# to the new template. wait_for_pods_ready alone is useless here: the adopted pods are already
# Ready, so it returned in "0s elapsed" while the roll had not started, and the first PGDATA read
# then hit a pod mid-restart ("container not found"). `rollout status` waits on
# updatedReplicas/currentRevision, which is the actual question.
#
# This is also the honest boundary of the runbook's "pods are kept" promise: --cascade=orphan
# keeps them RUNNING through the StatefulSet recreation, so the database stays up and the PVCs
# stay bound. The rolling update that follows is an ordinary rolling restart and does replace
# the pods -- which is why the PVC identity above, not pod identity, is what proves no data was
# lost, and why the sentinel below is what proves no re-clone.
echo "  ...adopted; waiting for the StatefulSet to roll the pods onto the 2.0.0 spec..."
kubectl rollout status statefulset "${FULLNAME}" -n "${NAMESPACE}" --timeout=15m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" "${NODES}" 900

# The PVCs must be the same objects: a new PVC means a new empty volume, which is the data loss
# the runbook promises does not happen.
for i in $(seq 0 $((NODES - 1))); do
  pvc_uid=$(kubectl get pvc "data-${FULLNAME}-${i}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  assert_eq "post: data-${FULLNAME}-${i} is the same PVC (uid unchanged)" "${PVC_UID_BEFORE[$i]}" "${pvc_uid}"
done

# ---------------------------------------------------------------------------
# Step 3: the agent owns failover now.
# ---------------------------------------------------------------------------
PRIMARY=""; HOLDER=""; waited=0
while [ ${waited} -lt 600 ]; do
  HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  PRIMARY=$(discover_primary "${NAMESPACE}" "${FULLNAME}" "${NODES}")
  if [ -n "${HOLDER}" ] && [ -n "${PRIMARY}" ] && [ "${HOLDER}" = "${PRIMARY}" ]; then break; fi
  sleep 10; waited=$((waited + 10))
done
[ -n "${PRIMARY}" ] || { echo "FATAL: no primary after the migration" >&2; exit 1; }
assert_eq "post: the leader Lease now exists and its holder is the primary" "${HOLDER}" "${PRIMARY}"
echo "  primary (2.0.0, agent): ${PRIMARY}"

writers=0
for i in $(seq 0 $((NODES - 1))); do
  rec=$(pg_exec "${NAMESPACE}" "${FULLNAME}-${i}" "SELECT pg_is_in_recovery()" | tr -d '[:space:]' || echo "")
  [ "${rec}" = "f" ] && writers=$((writers + 1))
done
assert_eq "post: exactly one writer (the orphan window produced no second primary)" "1" "${writers}"

# The sidecars the migration removes. Asserted on the live pod spec, not on a render: the
# recreate rewrote the spec of pods that were already running, and "the template no longer has
# them" would be true even if the running pods kept them.
for c in repmgrd service-updater; do
  left=$(kubectl get pod "${FULLNAME}-0" -n "${NAMESPACE}" \
    -o jsonpath="{.spec.containers[?(@.name=='${c}')].name}" | tr -d '[:space:]')
  assert_eq "post: the ${c} sidecar is gone from the running pod" "" "${left}"
done
# `entrypoint.sh agent` rather than `entrypoint.sh postgres`: in agent mode the Go agent is PID 1
# and supervises the postmaster, and the argument is the only thing in the pod spec that says so.
# Indexed, not the whole list: kubectl renders an array-valued field as JSON, so a substring
# match against the joined form would depend on that quoting.
agent_mode=$(kubectl get pod "${FULLNAME}-0" -n "${NAMESPACE}" \
  -o jsonpath='{.spec.containers[?(@.name=="postgresql")].command[1]}' 2>/dev/null || echo "")
assert_eq "post: the postgresql container runs \`entrypoint.sh agent\` (agent is PID 1)" "agent" "${agent_mode}"

# ---------------------------------------------------------------------------
# It was a migration, not a rebuild -- same claims as test-migrate-native.sh, which this flow
# inherits every one of on top of the recreate.
# ---------------------------------------------------------------------------
for i in $(seq 0 $((NODES - 1))); do
  pod="${FULLNAME}-${i}"
  sent=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    cat "${SENTINEL}" 2>/dev/null | tr -d '[:space:]' || echo "")
  assert_eq "post ${pod}: PGDATA sentinel survived (no re-clone)" "${MV}" "${sent}"

  ctl=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- "${CONTROLDATA}" "${PGDATA_DIR}")
  sysid=$(printf '%s\n' "${ctl}" | awk -F: '/Database system identifier/{gsub(/ /,"",$2); print $2}')
  assert_eq "post ${pod}: system identifier unchanged (no re-initdb)" "${SYSID_BEFORE[$i]}" "${sysid}"

  # #293: the preload line lives in the DATA directory, so the repmgr-free image cannot start
  # without it having been stripped. On this path that is doubly load-bearing: repmgrd mode is
  # where the line was written in the first place.
  left=$(kubectl exec -n "${NAMESPACE}" "${pod}" -c postgresql -- \
    sh -c "grep -c \"^[[:space:]]*shared_preload_libraries.*repmgr\" ${PGDATA_DIR}/postgresql.conf 2>/dev/null || true" | tr -d '[:space:]' || echo "")
  assert_eq "post ${pod}: repmgr stripped from PGDATA's postgresql.conf (#293)" "0" "${left:-0}"
done

rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "SELECT count(*) FROM migrate_repmgrd WHERE value='${MV}'" "testuser" "testdb" | tr -d '[:space:]' || echo "")
assert_eq "post: pre-migration data is present" "1" "${rows}"
pg_exec "${NAMESPACE}" "${PRIMARY}" \
  "INSERT INTO migrate_repmgrd (value) VALUES ('post-${MV}')" "testuser" "testdb" >/dev/null
post_rows=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT count(*) FROM migrate_repmgrd" "testuser" "testdb" | tr -d '[:space:]' || echo "")
assert_eq "post: the migrated primary accepts writes" "2" "${post_rows}"

streaming=0; waited=0
while [ ${waited} -lt 480 ]; do
  streaming=$(pg_exec "${NAMESPACE}" "${PRIMARY}" \
    "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'" | tr -d '[:space:]' || echo "")
  [ "${streaming}" = "$((NODES - 1))" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "post: every standby is streaming again" "$((NODES - 1))" "${streaming}"

# Step 3 of the runbook as written: the write Service points at the holder. In repmgrd mode the
# service-updater sidecar maintained this; the agent has to have taken the job over.
svc_target=""; waited=0
while [ ${waited} -lt 180 ]; do
  svc_target=$(kubectl get endpoints "${FULLNAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.subsets[0].addresses[0].targetRef.name}' 2>/dev/null || echo "")
  [ "${svc_target}" = "${PRIMARY}" ] && break
  sleep 10; waited=$((waited + 10))
done
assert_eq "post: the write Service points at the primary (the agent took the updater's job)" \
  "${PRIMARY}" "${svc_target}"

# ---------------------------------------------------------------------------
# The point of the whole exercise: lease-based failover actually works on the migrated cluster.
# A migration that lands a cluster which cannot fail over has moved the problem, not solved it,
# and nothing above would notice -- every check so far describes a cluster at rest.
# ---------------------------------------------------------------------------
echo ""
echo "  Post-migration: deleting the primary to prove agent failover works on a migrated cluster..."
kubectl delete pod "${PRIMARY}" -n "${NAMESPACE}" --wait=false

NEW_PRIMARY=""; waited=0
while [ ${waited} -lt 300 ]; do
  NEW_HOLDER=$(kubectl get lease "${LEASE}" -n "${NAMESPACE}" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || echo "")
  if [ -n "${NEW_HOLDER}" ] && [ "${NEW_HOLDER}" != "${PRIMARY}" ]; then
    rec=$(pg_exec "${NAMESPACE}" "${NEW_HOLDER}" "SELECT pg_is_in_recovery()" | tr -d '[:space:]' || echo "")
    [ "${rec}" = "f" ] && { NEW_PRIMARY="${NEW_HOLDER}"; break; }
  fi
  sleep 10; waited=$((waited + 10))
done
assert_eq "failover: a different pod took the Lease and promoted" "yes" \
  "$([ -n "${NEW_PRIMARY}" ] && echo yes || echo no)"

if [ -n "${NEW_PRIMARY}" ]; then
  fo_rows=$(pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" "SELECT count(*) FROM migrate_repmgrd" "testuser" "testdb" | tr -d '[:space:]' || echo "")
  assert_eq "failover: data survived the post-migration failover" "2" "${fo_rows}"
  pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" \
    "INSERT INTO migrate_repmgrd (value) VALUES ('failover-${MV}')" "testuser" "testdb" >/dev/null
  fo_after=$(pg_exec "${NAMESPACE}" "${NEW_PRIMARY}" "SELECT count(*) FROM migrate_repmgrd" "testuser" "testdb" | tr -d '[:space:]' || echo "")
  assert_eq "failover: the new primary accepts writes" "3" "${fo_after}"

  # And the ex-primary rejoins rather than needing an operator. This is the path #298 found had
  # never actually run pg_rewind in native mode; on a migrated cluster it is the first failover
  # the node has ever seen.
  rejoined=""; waited=0
  while [ ${waited} -lt 420 ]; do
    rejoined=$(pg_exec "${NAMESPACE}" "${PRIMARY}" "SELECT pg_is_in_recovery()" | tr -d '[:space:]' || echo "")
    [ "${rejoined}" = "t" ] && break
    sleep 10; waited=$((waited + 10))
  done
  assert_eq "failover: the ex-primary rejoined as a standby" "t" "${rejoined}"
fi

end_suite
print_summary
