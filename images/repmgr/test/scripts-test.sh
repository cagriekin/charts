#!/bin/bash
# Unit tests for the bash logic shipped in the repmgr image. No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ok()   { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

# --- syntax check every shipped script ---
for s in entrypoint.sh init-repmgr.sh repmgrd-entrypoint.sh service-updater.sh repmgr-common.sh; do
  if bash -n "${ROOT}/${s}" 2>/dev/null; then ok "bash -n ${s}"; else bad "bash -n ${s}"; fi
done

# --- #177: entrypoint + init-repmgr source the shared helper (one definition) ---
for s in entrypoint.sh init-repmgr.sh; do
  if grep -q "source /usr/local/bin/repmgr-common.sh" "${ROOT}/${s}"; then
    ok "#177: ${s} sources repmgr-common.sh"
  else
    bad "#177: ${s} does not source repmgr-common.sh"
  fi
done

# --- tl_to_int: WAL-filename timeline is HEX, must NOT be parsed as decimal ---
# Guards the #168 regression (a SQL ::int cast errored at TL 0x0A and was wrong
# from 0x10). The function now lives in the shared lib (#177); exercise it there.
sed -n '/^tl_to_int() {/,/^}/p' "${ROOT}/repmgr-common.sh" > /tmp/.tl_fn.sh
if [ ! -s /tmp/.tl_fn.sh ]; then bad "extract tl_to_int from repmgr-common.sh"; else
  ok "extract tl_to_int from repmgr-common.sh"
  # shellcheck disable=SC1091
  source /tmp/.tl_fn.sh
  check() { # check INPUT EXPECTED
    got=$(tl_to_int "$1")
    if [ "$got" = "$2" ]; then ok "tl_to_int '$1' -> '$2'"; else bad "tl_to_int '$1' -> '$got' (want '$2')"; fi
  }
  check 00000001 1       # TL 1
  check 00000009 9       # TL 9  (last timeline where hex == decimal)
  check 0000000A 10      # TL 10 -- a ::int cast ERRORS here
  check 00000010 16      # TL 16 -- a ::int cast yields 10 here
  check 000000FF 255
  check 0000ABCD 43981
  check "" ""            # unreadable -> empty
  check "0000000G" ""    # non-hex -> empty
fi
rm -f /tmp/.tl_fn.sh

# --- the shared timeline read must not reintroduce the ::int-on-hex parse ---
# The WAL-insert timeline read lives in repmgr-common.sh now (#177); guard it there.
if grep -q "from 1 for 8)::int" "${ROOT}/repmgr-common.sh"; then
  bad "repmgr-common.sh has no ::int-on-hex timeline cast"
else
  ok "repmgr-common.sh has no ::int-on-hex timeline cast"
fi

# --- #177: init-repmgr reads its LOCAL timeline via the shared helper (de-dup), and
# keeps a SYMMETRIC control-file comparison for the primary. The two sides MUST be read
# the same way: an immediate WAL-insert primary read against the offline control-file
# local read would wipe a standby that has followed a new timeline by streaming but not
# yet checkpointed it (both read the pre-checkpoint timeline and match). So init must use
# the shared local helper AND still query pg_control_checkpoint for the primary. ---
if grep -q "local_node_timeline_int" "${ROOT}/init-repmgr.sh"; then
  ok "#177: init-repmgr.sh reads its local timeline via the shared helper"
else
  bad "#177: init-repmgr.sh does not use the shared local timeline helper"
fi
if grep -q "FROM pg_control_checkpoint" "${ROOT}/init-repmgr.sh"; then
  ok "#177: init-repmgr.sh keeps the symmetric control-file primary read (no asymmetric re-clone)"
else
  bad "#177: init-repmgr.sh primary read is not the symmetric control-file read (asymmetry risks spurious re-clone)"
fi

# --- managed users (postgres, repmgr) must be created with a SCRAM secret ---
# initdb --auth-host=md5 sets password_encryption=md5, but pg_hba requires
# scram-sha-256 for the pod network -- so a bare CREATE USER stores an MD5 secret
# that the scram rule rejects ("does not have a valid SCRAM secret"), a startup
# race that wedges repmgrd / the standby clone. The CREATE/ALTER USER for the
# managed users must force scram-sha-256 in-session.
create_repmgr_line=$(grep -E "CREATE USER \\\$\{REPMGR_USER\}" "${ROOT}/entrypoint.sh")
if printf '%s' "${create_repmgr_line}" | grep -q "password_encryption='scram-sha-256'"; then
  ok "entrypoint.sh creates the repmgr user with a SCRAM secret"
else
  bad "entrypoint.sh creates the repmgr user with a SCRAM secret"
fi
create_pg_line=$(grep -E "CREATE USER \\\$\{POSTGRES_USER\}" "${ROOT}/entrypoint.sh")
if printf '%s' "${create_pg_line}" | grep -q "password_encryption='scram-sha-256'"; then
  ok "entrypoint.sh creates the postgres user with a SCRAM secret"
else
  bad "entrypoint.sh creates the postgres user with a SCRAM secret"
fi

# --- #175: reclone_preserving_old must not destroy data before a successful clone ---
# rm -rf'ing PGDATA before the clone leaves an empty data dir if every clone
# attempt fails. Extract the shipped function and drive it with a failing and a
# succeeding clone stub, asserting the (diverged) data survives a failed clone.
sed -n '/^reclone_preserving_old() {/,/^}/p' "${ROOT}/entrypoint.sh" > /tmp/.rc_fn.sh
if [ ! -s /tmp/.rc_fn.sh ]; then bad "extract reclone_preserving_old from entrypoint.sh"; else
  ok "extract reclone_preserving_old from entrypoint.sh"

  # failure case: clone always fails -> returns 1 and the original data survives
  fwork=$(mktemp -d); export PGDATA="${fwork}/pgdata"
  mkdir -p "$PGDATA"; echo irreplaceable > "${PGDATA}/PG_VERSION"
  frc=0
  ( source /tmp/.rc_fn.sh
    REPMGR_PASSWORD=x REPMGR_USER=r REPMGR_DB=d
    repmgr() { return 1; }
    sleep() { :; }
    reclone_preserving_old testhost ) || frc=$?
  [ "$frc" -eq 1 ] && ok "#175: reclone returns failure when every clone attempt fails" \
                    || bad "#175: reclone rc=${frc} (want 1)"
  if grep -rq irreplaceable "${fwork}"/pgdata.diverged.* 2>/dev/null; then
    ok "#175: diverged data preserved on clone failure (no rm -rf before clone)"
  else
    bad "#175: diverged data lost on clone failure"
  fi
  rm -rf "$fwork"; unset PGDATA

  # success case: clone succeeds -> returns 0 and the aside backup is removed
  swork=$(mktemp -d); export PGDATA="${swork}/pgdata"
  mkdir -p "$PGDATA"; echo old > "${PGDATA}/PG_VERSION"
  src=0
  ( source /tmp/.rc_fn.sh
    REPMGR_PASSWORD=x REPMGR_USER=r REPMGR_DB=d
    repmgr() { echo cloned > "${PGDATA}/PG_VERSION"; return 0; }
    sleep() { :; }
    reclone_preserving_old testhost ) || src=$?
  [ "$src" -eq 0 ] && ok "#175: reclone returns success when clone succeeds" \
                   || bad "#175: reclone rc=${src} (want 0)"
  if ls -d "${swork}"/pgdata.diverged.* >/dev/null 2>&1; then
    bad "#175: aside backup not cleaned up after successful clone"
  else
    ok "#175: aside backup cleaned up after successful clone"
  fi
  rm -rf "$swork"; unset PGDATA
  rm -f /tmp/.rc_fn.sh
fi

# --- #170: empty-data settle is gated on the durable primary marker ---
# A genuine first install (no marker) must take the fast single scan; only a
# PVC-loss recreate (marker present) settles. This keeps the common path at the
# proven low latency -- an unconditional settle (the reverted -12 attempt) added
# ~30s to every fresh boot and destabilized slow-runner startup.
sed -n '/^cluster_was_established() {/,/^}/p' "${ROOT}/entrypoint.sh" > /tmp/.cwe.sh
if [ ! -s /tmp/.cwe.sh ]; then bad "extract cluster_was_established from entrypoint.sh"; else
  ok "extract cluster_was_established from entrypoint.sh"
  # timeout is stubbed to run its command (drop the duration) so the kubectl
  # stub function is exercised rather than the real binary.
  # marker present (kubectl get succeeds) -> established -> settle path
  if ( source /tmp/.cwe.sh; timeout() { shift; "$@"; }; kubectl() { return 0; }; PRIMARY_MARKER=m NAMESPACE=ns cluster_was_established ); then
    ok "#170: marker present -> cluster established (settle)"
  else
    bad "#170: marker present should be established"
  fi
  # marker absent (kubectl NotFound -> non-zero) -> not established -> fast scan
  if ( source /tmp/.cwe.sh; timeout() { shift; "$@"; }; kubectl() { return 1; }; PRIMARY_MARKER=m NAMESPACE=ns cluster_was_established ); then
    bad "#170: marker absent should NOT be established"
  else
    ok "#170: marker absent -> fast single scan"
  fi
  # bounded: if the kubectl call times out (throttled API), treat as not
  # established (fast path) -- never a stall before initdb
  if ( source /tmp/.cwe.sh; timeout() { return 124; }; kubectl() { return 0; }; PRIMARY_MARKER=m NAMESPACE=ns cluster_was_established ); then
    bad "#170: kubectl timeout should NOT be treated as established"
  else
    ok "#170: kubectl timeout -> fast single scan (bounded, no stall)"
  fi
  # unconfigured (no marker name / namespace) -> not established, never calls kubectl
  if ( source /tmp/.cwe.sh; timeout() { shift; "$@"; }; kubectl() { return 0; }; PRIMARY_MARKER="" NAMESPACE="" cluster_was_established ); then
    bad "#170: unconfigured should NOT be established"
  else
    ok "#170: unconfigured -> fast single scan (no kubectl dependency)"
  fi
  rm -f /tmp/.cwe.sh
fi
# structural: the empty-data branch must gate the settle on cluster_was_established
if grep -q 'if cluster_was_established; then' "${ROOT}/entrypoint.sh"; then
  ok "#170: empty-data settle gated on cluster_was_established"
else
  bad "#170: empty-data settle not marker-gated"
fi

# behavioral: the empty-data settle must NOT break early on a merely-reachable
# peer (a reachable standby is not proof the primary is gone), and must stop as
# soon as an active primary is found. Drives settle_scan_for_primary with a
# stubbed scan_peers.
sed -n '/^settle_scan_for_primary() {/,/^}/p' "${ROOT}/entrypoint.sh" > /tmp/.ssp.sh
if [ ! -s /tmp/.ssp.sh ]; then bad "extract settle_scan_for_primary from entrypoint.sh"; else
  ok "extract settle_scan_for_primary from entrypoint.sh"
  # peer reachable every scan but no primary -> must scan ALL attempts (the -14
  # bug broke after attempt 1 on REACHED_ANY and would then have initdb'd)
  noprimary_calls=$( ( source /tmp/.ssp.sh; sleep() { :; }
    CALLS=0
    scan_peers() { CALLS=$((CALLS+1)); REACHED_ANY=1; FOUND_PRIMARY=0; }
    REPMGR_STALE_CHECK_ATTEMPTS=5 settle_scan_for_primary >/dev/null 2>&1
    echo "$CALLS" ) )
  [ "$noprimary_calls" = "5" ] \
    && ok "#170: settle scans all attempts when no primary found (no early REACHED_ANY break)" \
    || bad "#170: settle stopped early (CALLS=${noprimary_calls}, want 5)"
  # primary appears on the 3rd scan -> stop exactly there
  primary_calls=$( ( source /tmp/.ssp.sh; sleep() { :; }
    CALLS=0
    scan_peers() { CALLS=$((CALLS+1)); REACHED_ANY=1; [ "$CALLS" -ge 3 ] && FOUND_PRIMARY=1 || FOUND_PRIMARY=0; }
    REPMGR_STALE_CHECK_ATTEMPTS=5 settle_scan_for_primary >/dev/null 2>&1
    echo "$CALLS" ) )
  [ "$primary_calls" = "3" ] \
    && ok "#170: settle stops as soon as an active primary is found" \
    || bad "#170: settle did not stop at primary (CALLS=${primary_calls}, want 3)"
  rm -f /tmp/.ssp.sh
fi

# --- agent failover mode: entrypoint dispatches "agent" -> pg-ha-agent ---
if grep -qF '"postgres"|"agent")' "${ROOT}/entrypoint.sh" && grep -qF 'exec /usr/local/bin/pg-ha-agent' "${ROOT}/entrypoint.sh"; then
  ok "entrypoint dispatches agent mode to pg-ha-agent"
else
  bad "entrypoint does not dispatch agent mode to pg-ha-agent"
fi

# --- init-repmgr honors REPMGR_FAILOVER (manual in agent mode) ---
if grep -qF 'REPMGR_FAILOVER:-automatic' "${ROOT}/init-repmgr.sh"; then
  ok "init-repmgr.sh honors REPMGR_FAILOVER"
else
  bad "init-repmgr.sh does not honor REPMGR_FAILOVER"
fi

# --- #308: init-repmgr honors USE_REPLICATION_SLOTS (the chart sets it only in agent
# mode, matching the agent's own regenerated repmgr.conf so a fresh standby's very
# first clone gets a physical slot instead of only its second repmgr operation) ---
if grep -qF 'use_replication_slots=${USE_REPLICATION_SLOTS:-0}' "${ROOT}/init-repmgr.sh"; then
  ok "#308: init-repmgr.sh honors USE_REPLICATION_SLOTS"
else
  bad "#308: init-repmgr.sh does not honor USE_REPLICATION_SLOTS"
fi

# --- #269: the PG major must not be hardcoded anywhere in the shipped shell layer ---
# The whole point of the PG_MAJOR build arg is that one image build can be PG17 or PG18.
# A single re-hardcoded /usr/lib/postgresql/<major>/bin would send a PG17 image at a
# bindir that does not exist -- so scan every shipped file rather than the ones that
# happened to need fixing.
hardcoded=$(grep -rn '/usr/lib/postgresql/1[0-9]' \
  "${ROOT}"/*.sh "${ROOT}/repmgr.conf" "${ROOT}/Dockerfile" 2>/dev/null || true)
if [ -z "$hardcoded" ]; then
  ok "#269: no hardcoded versioned bindir in the shipped scripts/conf"
else
  bad "#269: hardcoded versioned bindir found" "$hardcoded"
fi

# --- #269: PG_BINDIR is defined once, in the shared helper ---
if grep -qE '^PG_BINDIR="/usr/lib/postgresql/\$\{PG_MAJOR\}/bin"$' "${ROOT}/repmgr-common.sh" \
   && grep -qE '^PG_MAJOR="\$\{PG_MAJOR:-18\}"$' "${ROOT}/repmgr-common.sh"; then
  ok "#269: repmgr-common.sh derives PG_BINDIR from PG_MAJOR (default 18)"
else
  bad "#269: repmgr-common.sh does not define PG_MAJOR/PG_BINDIR as expected"
fi

# --- #269: init-repmgr must source the helper BEFORE it uses PG_BINDIR on PATH ---
# Ordering is load-bearing, not style: sourcing after the export would put an empty
# path element on PATH, and pg_controldata would silently not be found -- which is
# exactly the failure that makes every standby restart do a full re-clone.
src_line=$(grep -n 'source /usr/local/bin/repmgr-common.sh' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
path_line=$(grep -n 'export PATH=.*PG_BINDIR' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
if [ -n "$src_line" ] && [ -n "$path_line" ] && [ "$src_line" -lt "$path_line" ]; then
  ok "#269: init-repmgr.sh sources repmgr-common.sh before exporting PATH"
else
  bad "#269: init-repmgr.sh PATH export does not follow the helper source (source=${src_line:-none} path=${path_line:-none})"
fi

# --- #269: the generated repmgr.conf must carry the derived bindir ---
if grep -qF "pg_bindir='\${PG_BINDIR}'" "${ROOT}/init-repmgr.sh"; then
  ok "#269: generated repmgr.conf uses \$PG_BINDIR"
else
  bad "#269: generated repmgr.conf does not use \$PG_BINDIR"
fi
for cmd in start stop restart reload; do
  if grep -qF "service_${cmd}_command='\${PG_BINDIR}/pg_ctl" "${ROOT}/init-repmgr.sh"; then
    ok "#269: service_${cmd}_command uses \$PG_BINDIR"
  else
    bad "#269: service_${cmd}_command does not use \$PG_BINDIR"
  fi
done

# --- #269: require_pg_bindir refuses a major the image does not bundle ---
# The chart passes PG_MAJOR from repmgr.image.majorVersion, so this function is where a
# values file asking for a major the image was not built with stops. Behavioral, not
# structural: a bogus major must fail and the message must name both sides, because the
# alternative failure mode is an empty PATH element and a confusing "initdb: not found".
bogus=$( PG_MAJOR=999 bash -c 'source '"${ROOT}"'/repmgr-common.sh; require_pg_bindir' 2>&1 )
bogus_rc=$?
if [ "$bogus_rc" -ne 0 ]; then
  ok "#269: require_pg_bindir fails for a major the image does not bundle"
else
  bad "#269: require_pg_bindir accepted PG_MAJOR=999"
fi
if grep -q 'PG_MAJOR=999' <<<"$bogus" && grep -qi 'repmgr.image.majorVersion' <<<"$bogus"; then
  ok "#269: require_pg_bindir names the requested major and the values to fix"
else
  bad "#269: require_pg_bindir message is not actionable" "$bogus"
fi

# Both entrypoints must CALL it -- an unused guard is no guard.
for s in entrypoint.sh init-repmgr.sh; do
  if grep -q 'require_pg_bindir' "${ROOT}/${s}"; then
    ok "#269: ${s} calls require_pg_bindir"
  else
    bad "#269: ${s} does not call require_pg_bindir"
  fi
done

# --- #269: the unsuffixed published image tag must keep meaning PG18 ---
# Existing chart pins (repmgr.image.tag without a -pgNN suffix) resolve to the image
# built with no --build-arg, so flipping this default would silently move every
# existing installation to another major on the next image refresh.
if grep -qE '^ARG PG_MAJOR=18$' "${ROOT}/Dockerfile"; then
  ok "#269: Dockerfile defaults ARG PG_MAJOR to 18"
else
  bad "#269: Dockerfile ARG PG_MAJOR default is not 18"
fi

# --- #269: the major must reach the runtime as an ENV ---
# The shell layer and the Go agent both read PG_MAJOR from the container env; without
# this ENV a PG17 build would fall back to the 18 default at runtime.
if grep -qE '^ENV PG_MAJOR=\$\{PG_MAJOR\}$' "${ROOT}/Dockerfile"; then
  ok "#269: Dockerfile exports PG_MAJOR to the runtime env"
else
  bad "#269: Dockerfile does not export PG_MAJOR as an ENV"
fi

# --- #269: per-major packages are checked for a candidate before install ---
# A missing postgresql-<major>-pgaudit must fail the BUILD; discovered at runtime it
# would mean audit.enabled=true produces silently absent audit logs.
if grep -qF 'apt-cache policy' "${ROOT}/Dockerfile" \
   && grep -qF 'postgresql-${PG_MAJOR}-pgaudit' "${ROOT}/Dockerfile"; then
  ok "#269: Dockerfile asserts per-major package availability before install"
else
  bad "#269: Dockerfile does not assert per-major package availability"
fi

# --- #303 follow-up: conf.d must be wired in before the FIRST pg_ctl start ---
# shared_preload_libraries is postmaster-only (no reload). The chart's merged value
# (repmgr + operator extras/pgaudit) lives in conf.d; previously only the chart's
# postStart hook spliced in the include_dir line, after postgres was already
# accepting connections -- too late for a postmaster-only GUC, and nothing forces a
# second restart on a fresh `helm install` (the config-checksum rolling restart only
# helps a later `helm upgrade`). entrypoint.sh must wire it in at initdb time,
# before its own bootstrap pg_ctl start below, so the merged preload list is active
# from the very first postmaster start.
confd_line=$(grep -n "include_dir = '/etc/postgresql/conf.d'" "${ROOT}/entrypoint.sh" | grep -v "PGDATA\"" | head -1 | cut -d: -f1)
guard_line=$(grep -n "if \[ -d /etc/postgresql/conf.d \]; then" "${ROOT}/entrypoint.sh" | head -1 | cut -d: -f1)
first_start_line=$(grep -n 'pg_ctl -D "\$PGDATA" -w start' "${ROOT}/entrypoint.sh" | head -1 | cut -d: -f1)
if [ -n "$guard_line" ] && [ -n "$confd_line" ]; then
  ok "#303: entrypoint.sh guards the conf.d include on the mount actually existing"
else
  bad "#303: entrypoint.sh does not guard the conf.d include on the mount existing"
fi
if [ -n "$confd_line" ] && [ -n "$first_start_line" ] && [ "$confd_line" -lt "$first_start_line" ]; then
  ok "#303: entrypoint.sh wires conf.d in before the bootstrap pg_ctl start"
else
  bad "#303: entrypoint.sh does not wire conf.d in before the bootstrap pg_ctl start"
fi

# --- an empty PGDATA on ordinal 0 must be decided from the CLUSTER, not the ordinal ---
# Regression guard for the PVC-loss dead end: init-repmgr.sh derived NODE_TYPE from the
# ordinal, so an empty data directory on pod-0 printed "First boot, postgres mode will
# initialize the database" and skipped the clone; entrypoint.sh's primary_safety_guard then
# refused to initdb next to the active primary, so the pod crash-looped with no way out.
# The empty-data branch must therefore consult the cluster BEFORE it can conclude "first
# boot", and it must be able to reach the standby clone path from ordinal 0.
empty_branch=$(grep -n 'if \[ "\$NODE_TYPE" = "master" \] && \[ ! -s "\${PGDATA}/PG_VERSION" \]; then' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
first_boot_line=$(grep -n 'First boot, postgres mode will initialize the database' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
probe_line=$(grep -n 'CURRENT_PRIMARY=$(find_current_primary)' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
if [ -n "$empty_branch" ] && [ -n "$probe_line" ] && [ -n "$first_boot_line" ] && \
   [ "$probe_line" -gt "$empty_branch" ] && [ "$probe_line" -lt "$first_boot_line" ]; then
  ok "init-repmgr.sh probes for an existing primary before declaring an empty pod-0 a first boot"
else
  bad "init-repmgr.sh can still declare an empty pod-0 a first boot without probing the cluster (empty=${empty_branch:-none} probe=${probe_line:-none} first_boot=${first_boot_line:-none})"
fi
if grep -q 'NODE_TYPE="standby"' "${ROOT}/init-repmgr.sh"; then
  ok "init-repmgr.sh can route an empty ordinal-0 pod onto the standby clone path"
else
  bad "init-repmgr.sh has no way to route an empty ordinal-0 pod onto the standby clone path"
fi

# The ordinal-0 fallback inside wait_for_primary must never probe the pod running it:
# ordinal 0 can now reach that loop, and its own postmaster is not up yet.
if grep -q 'if \[ "\$ORDINAL" != "0" \] && PGPASSWORD="\${REPMGR_PASSWORD}" pg_isready -h "\${FQDN_0}"' "${ROOT}/init-repmgr.sh"; then
  ok "init-repmgr.sh does not let ordinal 0 wait on its own postmaster"
else
  bad "init-repmgr.sh lets ordinal 0 probe itself in the wait_for_primary fallback"
fi


# --- ordinal 0 holding STANDBY data must not take the destructive master path ---
# A data directory carrying standby.signal is not primary-state, so the "Primary-state
# data directory present" guard does not exit, and the empty-data branch does not apply
# either. NODE_TYPE was still "master", so ordinal 0 fell into the master block, which
# rm -rf's PGDATA and full-clones. That fires on EVERY restart of a healthy pod-0 standby
# -- the steady state this fix creates -- while ordinal >0 in the identical state takes
# the timeline-compare fast path further down and skips the clone entirely. It is also
# unrecoverable when every clone attempt fails, since it deletes before it clones.
signal_flip=$(awk '/standby[.]signal/{c=NR} /NODE_TYPE="standby"/{if (c && NR-c<=2) {print c; exit}}' "${ROOT}/init-repmgr.sh")
master_block=$(grep -n '^if \[ "\$NODE_TYPE" = "master" \]; then' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
if [ -n "$signal_flip" ] && [ -n "$master_block" ] && [ "$signal_flip" -lt "$master_block" ]; then
  ok "init-repmgr.sh routes standby-state data to the standby path before the master block"
else
  bad "init-repmgr.sh lets ordinal 0 with standby.signal reach the rm -rf master path (flip=${signal_flip:-none} master=${master_block:-none})"
fi
# --- any_peer_reachable: a live STANDBY is proof a cluster exists ---
# The empty-data path treats "no peer answered at all" as a genuine first install, so this
# is the function that decides whether an empty pod-0 initdb's. Exercise it for real with a
# stubbed pg_isready rather than trusting the grep above.
sed -n '/^any_peer_reachable() {/,/^}/p' "${ROOT}/init-repmgr.sh" > /tmp/.apr_fn.sh
if [ ! -s /tmp/.apr_fn.sh ]; then bad "extract any_peer_reachable from init-repmgr.sh"; else
  ok "extract any_peer_reachable from init-repmgr.sh"
  # shellcheck disable=SC1091
  source /tmp/.apr_fn.sh
  HOSTNAME=pg-0 ; ORDINAL=0 ; HEADLESS_SERVICE=h ; REPMGR_USER=u ; REPMGR_PASSWORD=p
  REPMGR_DB=d ; REPMGR_NODE_COUNT=3
  # LIVE_PEER is the one host the stub answers for; empty means nothing answers.
  # pg_isready is called as: pg_isready -h <host> -p 5432 -U <user> -d <db>
  pg_isready() { [ -n "${LIVE_PEER:-}" ] && [ "$2" = "${LIVE_PEER}" ]; }
  LIVE_PEER="pg-1.h" ; REACHABLE_PEER=""
  if any_peer_reachable && [ "$REACHABLE_PEER" = "pg-1.h" ]; then
    ok "any_peer_reachable finds a live peer and reports which one"
  else
    bad "any_peer_reachable missed a live peer (REACHABLE_PEER='${REACHABLE_PEER:-}')"
  fi
  LIVE_PEER="" ; REACHABLE_PEER=""
  if any_peer_reachable; then
    bad "any_peer_reachable claims a peer is live when none answers"
  else
    ok "any_peer_reachable reports no peer when the cluster is silent (a genuine first install)"
  fi
  # Its own ordinal must be skipped, or pod-0 would find "itself" and never initdb.
  LIVE_PEER="pg-0.h" ; REACHABLE_PEER=""
  if any_peer_reachable; then
    bad "any_peer_reachable counted the local node as a peer"
  else
    ok "any_peer_reachable skips the local node"
  fi
  unset -f pg_isready
fi
rm -f /tmp/.apr_fn.sh

echo "----"
[ "$fail" -eq 0 ] && echo "ALL TESTS PASSED" || echo "TESTS FAILED"
exit "$fail"
