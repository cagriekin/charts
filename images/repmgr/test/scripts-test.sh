#!/bin/bash
# Unit tests for the bash logic shipped in the repmgr image. No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ok()   { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

# --- syntax check every shipped script ---
for s in entrypoint.sh init-repmgr.sh repmgr-common.sh; do
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

# --- #288: the native mechanism must bypass every repmgr-specific bootstrap step ---
# The repmgr.nodes registration wait is what made native unusable with replicas: nothing ever
# registers under native, so the poll burned its full ~240s and exited 1, leaving every standby
# in Init:CrashLoopBackOff forever. The gate has to sit BEFORE the repmgr.conf heredoc, because
# entrypoint.sh's stale-primary guard keys on that file existing.
init_native_gate_line=$(grep -n 'MECHANISM:-repmgr' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
init_conf_line=$(grep -n 'cat > /etc/repmgr/repmgr.conf' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
init_poll_line=$(grep -n 'FROM repmgr.nodes' "${ROOT}/init-repmgr.sh" | head -1 | cut -d: -f1)
if [ -n "${init_native_gate_line}" ] && [ -n "${init_conf_line}" ] && [ "${init_native_gate_line}" -lt "${init_conf_line}" ]; then
  ok "#288: init-repmgr.sh gates on MECHANISM before writing repmgr.conf"
else
  bad "#288: init-repmgr.sh does not gate on MECHANISM before writing repmgr.conf (gate=${init_native_gate_line:-none} conf=${init_conf_line:-none})"
fi
if [ -n "${init_native_gate_line}" ] && [ -n "${init_poll_line}" ] && [ "${init_native_gate_line}" -lt "${init_poll_line}" ]; then
  ok "#288: the native gate precedes the repmgr.nodes registration poll"
else
  bad "#288: the repmgr.nodes poll is still reachable under native (gate=${init_native_gate_line:-none} poll=${init_poll_line:-none})"
fi

# NOT re-testing the gate expression by hand-copying it: `MECHANISM=x bash -c 'if [ ... ]'`
# asserts that bash evaluates a literal this test wrote, which is true regardless of what the
# shipped script says. The positional greps above carry the real weight, and bootstrap_initdb
# is exercised behaviourally further down by sourcing the function out of entrypoint.sh.

# --- #288: the stale-primary guard is repmgr-only ---
# It shells out to `repmgr node rejoin` / `repmgr standby clone` before the agent starts; under
# native the agent owns both, with the Lease as the authority for who is primary.
if sed -n '/^primary_safety_guard()/,/^}/p' "${ROOT}/entrypoint.sh" | grep -q 'MECHANISM:-repmgr'; then
  ok "#288: primary_safety_guard is gated on MECHANISM"
else
  bad "#288: primary_safety_guard still runs repmgr rejoin under native"
fi

# --- #288: the repmgr EXTENSION is skipped under native, but the DB and ROLE are NOT ---
# The agent connects as REPMGR_USER for every probe and for pg_basebackup, and
# primary_conninfo carries dbname=REPMGR_DB, so dropping those would break native outright.
# Only the extension (which creates the nodes table this issue retires) is skipped.
# Line-based: the CREATE EXTENSION must sit immediately inside the native gate, not merely
# somewhere in the same file.
# NEAREST-PRECEDING gate, not the first in the file: there is now more than one `!= "native"`
# block (shared_preload_libraries has its own), and `head -1` picked the wrong one.
ext_line=$(grep -n 'CREATE EXTENSION IF NOT EXISTS repmgr' "${ROOT}/entrypoint.sh" | head -1 | cut -d: -f1)
ext_ok=no
for g in $(grep -n '!= "native"' "${ROOT}/entrypoint.sh" | cut -d: -f1); do
  if [ -n "${ext_line}" ] && [ "${ext_line}" -gt "${g}" ] && [ $((ext_line - g)) -le 3 ]; then ext_ok=yes; fi
done
if [ "${ext_ok}" = "yes" ]; then
  ok "#288: CREATE EXTENSION repmgr is skipped under native"
else
  bad "#288: CREATE EXTENSION repmgr is not gated on MECHANISM (ext=${ext_line:-none})"
fi
for keep in "CREATE DATABASE \${REPMGR_DB}" "CREATE USER \${REPMGR_USER}"; do
  if grep -qF "${keep}" "${ROOT}/entrypoint.sh"; then
    ok "#288: still creates ${keep} under both mechanisms (native needs it for replication auth)"
  else
    bad "#288: ${keep} was removed -- native connects as that role to that database"
  fi
done

# --- #288: initdb has exactly ONE call site, and native must not reach it inline ---
# The regression this guards: with the init container no longer cloning under native, an
# inline initdb on any empty PGDATA means every pod creates its own cluster with its own
# system_identifier -- and assertSameCluster (invariant 9) then refuses to rejoin any of them,
# so pods sit Running-but-never-Ready holding bogus databases. Strictly worse than the
# Init:CrashLoopBackOff it replaced.
if [ "$(grep -c 'initdb -D' "${ROOT}/entrypoint.sh")" = "1" ]; then
  ok "#288: initdb has exactly one call site"
else
  bad "#288: initdb has $(grep -c 'initdb -D' "${ROOT}/entrypoint.sh") call sites; it must live only in bootstrap_initdb"
fi
if sed -n '/^bootstrap_initdb() {/,/^}/p' "${ROOT}/entrypoint.sh" | grep -q 'initdb -D'; then
  ok "#288: the initdb call site is inside bootstrap_initdb"
else
  bad "#288: initdb is not inside bootstrap_initdb"
fi
# The function must refuse to touch a populated data directory, whichever caller invokes it.
if sed -n '/^bootstrap_initdb() {/,/^}/p' "${ROOT}/entrypoint.sh" | grep -q 'if \[ -s "$PGDATA/PG_VERSION" \]'; then
  ok "#288: bootstrap_initdb no-ops on an existing data directory"
else
  bad "#288: bootstrap_initdb would initdb over existing data"
fi
# Behavioural, against the SHIPPED function rather than a hand-copied expression: source it
# out of the script and drive it with stubs, both ways round.
_bi_tmp=$(mktemp -d)
mkdir -p "${_bi_tmp}/pgdata"
echo 18 > "${_bi_tmp}/pgdata/PG_VERSION"
_bi_out=$(PGDATA="${_bi_tmp}/pgdata" bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { echo INITDB-RAN; }; pg_ctl() { :; }; psql() { :; }
  bootstrap_initdb 2>/dev/null' || true)
if printf '%s' "${_bi_out}" | grep -q INITDB-RAN; then
  bad "#288: bootstrap_initdb ran initdb over a populated PGDATA"
else
  ok "#288: bootstrap_initdb skipped a populated PGDATA (behavioural)"
fi
rm -f "${_bi_tmp}/pgdata/PG_VERSION"
_bi_out=$(PGDATA="${_bi_tmp}/pgdata" bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { echo INITDB-RAN; }; pg_ctl() { :; }; psql() { :; }
  bootstrap_initdb 2>/dev/null' || true)
if printf '%s' "${_bi_out}" | grep -q INITDB-RAN; then
  ok "#288: bootstrap_initdb initdbs an empty PGDATA (behavioural)"
else
  bad "#288: bootstrap_initdb did not initdb an empty PGDATA"
fi
rm -rf "${_bi_tmp}"
# The agent invokes it through a dispatch mode, so that mode must exist and be advertised.
if grep -q '"initdb")' "${ROOT}/entrypoint.sh"; then
  ok "#288: entrypoint.sh has an initdb dispatch mode for the agent"
else
  bad "#288: no initdb dispatch mode; the agent cannot bootstrap the lease holder"
fi
if grep -q 'postgres|agent|init|initdb' "${ROOT}/entrypoint.sh"; then
  ok "#288: the usage string lists the initdb mode"
else
  bad "#288: the usage string does not list the initdb mode"
fi

# --- #288: repmgr.so is preloaded only under the repmgr mechanism ---
# A native cluster has no repmgr extension, so preloading the library is pure liability -- and
# the line is baked into the primary's postgresql.conf and cloned to every standby, which would
# make every native cluster created by this code unstartable the moment #290/#294 drop repmgr
# from the image.
spl_line=$(grep -n "shared_preload_libraries = 'repmgr'" "${ROOT}/entrypoint.sh" | head -1 | cut -d: -f1)
# Line-based, like the CREATE EXTENSION check: the nearest preceding native gate must be within
# a couple of lines, i.e. the write really sits inside it.
spl_gates=$(grep -n '!= "native"' "${ROOT}/entrypoint.sh" | cut -d: -f1)
spl_ok=no
for g in ${spl_gates}; do
  if [ -n "${spl_line}" ] && [ "${spl_line}" -gt "${g}" ] && [ $((spl_line - g)) -le 2 ]; then spl_ok=yes; fi
done
if [ "${spl_ok}" = "yes" ]; then
  ok "#288: shared_preload_libraries=repmgr is gated on the mechanism"
else
  bad "#288: shared_preload_libraries=repmgr is not mechanism-gated (line=${spl_line:-none}, gates=${spl_gates})"
fi

# --- #288: the transient bootstrap postmaster must not be network-reachable ---
# Between CREATE USER ${REPMGR_USER} and the stop at the end of bootstrap_initdb it would
# otherwise be a reachable, authenticable primary reporting pg_is_in_recovery()=false -- and
# under native a non-holder's next tick would BootstrapClone from it, inheriting the legacy
# `host all all 0.0.0.0/0 md5` pg_hba for the pod's whole life (nothing on the clone path
# rewrites pg_hba) plus a postgresql.conf with no include_dir.
if sed -n '/^bootstrap_initdb() {/,/^}/p' "${ROOT}/entrypoint.sh" | grep -q "listen_addresses=''"; then
  ok "#288: the bootstrap postmaster listens on no TCP address"
else
  bad "#288: the bootstrap postmaster is network-reachable during role creation"
fi

# --- #288: bootstrap_initdb's completion sentinel is written LAST ---
# The agent pairs an in-progress marker beside PGDATA with this sentinel inside it: marker
# present and sentinel absent means the bootstrap was killed partway (the kubelet can do this --
# the transient `pg_ctl start` satisfies the chart's startupProbe while the agent is inside the
# exec and not beating /healthz) and the directory must be discarded. That inference only holds
# if the sentinel is written after the LAST thing the bootstrap does, so a half-bootstrapped
# directory can never carry it.
sentinel_line=$(grep -n 'pg-ha-bootstrap-complete' "${ROOT}/entrypoint.sh" | head -1 | cut -d: -f1)
if [ -z "$sentinel_line" ]; then
  bad "#288: bootstrap_initdb writes no completion sentinel (the agent cannot tell a torn bootstrap from a finished one)"
else
  ok "#288: bootstrap_initdb writes a completion sentinel"
  # Every step of the bootstrap must precede it: the role/database psql calls and the stop.
  last_step=$(grep -n 'CREATE USER ${REPMGR_USER}\|pg_ctl -D "$PGDATA" -w stop' "${ROOT}/entrypoint.sh" | tail -1 | cut -d: -f1)
  if [ -n "$last_step" ] && [ "$sentinel_line" -gt "$last_step" ]; then
    ok "#288: the completion sentinel is written after the bootstrap's last step"
  else
    bad "#288: the completion sentinel is not last (sentinel=${sentinel_line}, last step=${last_step:-none}); a killed bootstrap could carry it"
  fi
fi

echo "----"
[ "$fail" -eq 0 ] && echo "ALL TESTS PASSED" || echo "TESTS FAILED"
exit "$fail"
