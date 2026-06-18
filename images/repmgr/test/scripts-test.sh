#!/bin/bash
# Unit tests for the bash logic shipped in the repmgr image. No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ok()   { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

# --- syntax check every shipped script ---
for s in entrypoint.sh init-repmgr.sh repmgrd-entrypoint.sh service-updater.sh; do
  if bash -n "${ROOT}/${s}" 2>/dev/null; then ok "bash -n ${s}"; else bad "bash -n ${s}"; fi
done

# --- tl_to_int: WAL-filename timeline is HEX, must NOT be parsed as decimal ---
# Guards the #168 regression (a SQL ::int cast errored at TL 0x0A and was wrong
# from 0x10). Extract the shipped function and exercise the boundary cases.
sed -n '/^tl_to_int() {/,/^}/p' "${ROOT}/entrypoint.sh" > /tmp/.tl_fn.sh
if [ ! -s /tmp/.tl_fn.sh ]; then bad "extract tl_to_int from entrypoint.sh"; else
  ok "extract tl_to_int from entrypoint.sh"
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

# --- entrypoint must not reintroduce the ::int-on-hex parse ---
if grep -q "from 1 for 8)::int" "${ROOT}/entrypoint.sh"; then
  bad "entrypoint.sh has no ::int-on-hex timeline cast"
else
  ok "entrypoint.sh has no ::int-on-hex timeline cast"
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

echo "----"
[ "$fail" -eq 0 ] && echo "ALL TESTS PASSED" || echo "TESTS FAILED"
exit "$fail"
