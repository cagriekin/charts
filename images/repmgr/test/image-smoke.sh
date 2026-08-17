#!/bin/bash
# Smoke-test a BUILT repmgr image: assert it really bundles the PostgreSQL major it
# claims, and that everything the chart depends on is present and runnable (#269).
#
# scripts-test.sh checks the shell logic without a container; this checks the artifact.
# The two together are what make `--build-arg PG_MAJOR=17` trustworthy: a build can
# succeed, ship a working PG17 server, and still be unusable because a per-major
# package resolved to a different version than the tag advertises.
#
# Usage: bash images/repmgr/test/image-smoke.sh <image-ref> <expected-pg-major>
#   bash images/repmgr/test/image-smoke.sh cagriekin/repmgr:trixie-5.5.0-31-pg17 17
set -uo pipefail

IMAGE="${1:?usage: image-smoke.sh <image-ref> <expected-pg-major>}"
WANT_MAJOR="${2:?usage: image-smoke.sh <image-ref> <expected-pg-major>}"

# repmgr version to expect, DERIVED from the image tag (trixie-<repmgr>-<n>[-pgNN]) rather
# than hardcoded. PGDG packages repmgr per major and the Dockerfile pins no repmgr version,
# so 17 and 18 could carry different ones -- which would make the tag a lie. Deriving keeps
# the assertion meaningful when PGDG moves on: a tag claiming 5.5.0 that ships 5.6.0 fails
# with an obvious remedy (publish trixie-5.6.0-N), whereas a hardcoded expectation would
# block every build and every PR until the test file was edited. Tags that do not encode a
# version (local `repmgr:smoke-pg17` builds) fall back to the looser cross-check below.
if [ -z "${WANT_REPMGR:-}" ]; then
  WANT_REPMGR=$(printf '%s' "${IMAGE##*:}" | sed -nE 's/^trixie-([0-9]+\.[0-9]+\.[0-9]+)-.*/\1/p')
fi

fail=0
ok()  { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; fail=1; }

command -v docker >/dev/null 2>&1 || { echo "docker is required to smoke-test an image" >&2; exit 1; }

echo "=== SMOKE: ${IMAGE} (expecting PostgreSQL ${WANT_MAJOR}, repmgr ${WANT_REPMGR:-<derived from the running image>}) ==="

# One container run collects everything as KEY=value lines, so the assertions below are
# named and independent while paying the container startup cost once. Each probe falls
# back to MISSING/an error string rather than aborting, so a single missing binary does
# not hide every later result.
probe=$(docker run --rm --entrypoint bash "$IMAGE" -c '
set -u
BINDIR="/usr/lib/postgresql/${PG_MAJOR:-unset}/bin"
# Keep the image default PATH: some checks below must resolve binaries exactly as a
# container that never extends PATH would (see REPMGR_RESOLVED).
IMAGE_PATH="$PATH"
export PATH="$PATH:$BINDIR"
echo "ENV_PG_MAJOR=${PG_MAJOR:-unset}"
echo "BINDIR_EXISTS=$([ -d "$BINDIR" ] && echo yes || echo no)"
echo "POSTGRES_VERSION=$("$BINDIR/postgres" --version 2>&1 | head -1)"
echo "PG_CTL=$([ -x "$BINDIR/pg_ctl" ] && echo yes || echo MISSING)"
echo "PG_CONTROLDATA=$([ -x "$BINDIR/pg_controldata" ] && echo yes || echo MISSING)"
# repmgr/repmgrd are invoked bare (no PATH manipulation) by repmgrd-entrypoint.sh, so
# resolve them the same way that does -- via the default PATH.
echo "REPMGR_RESOLVED=$(PATH="$IMAGE_PATH" command -v repmgr 2>/dev/null || echo MISSING)"
echo "REPMGRD_RESOLVED=$(PATH="$IMAGE_PATH" command -v repmgrd 2>/dev/null || echo MISSING)"
echo "REPMGR_VERSION=$(repmgr --version 2>&1 | head -1)"
# repmgrd refuses to run as root even for --version, so ask as the user that actually
# runs it in the pod.
echo "REPMGRD_VERSION=$(gosu postgres repmgrd --version 2>&1 | head -1)"
echo "PGBACKREST_VERSION=$(pgbackrest version 2>&1 | head -1)"
echo "JQ=$(command -v jq || echo MISSING)"
echo "GOSU=$(command -v gosu || echo MISSING)"
echo "CRON=$(command -v cron || echo MISSING)"
echo "KUBECTL=$(kubectl version --client 2>&1 | head -1)"
echo "AGENT=$([ -x /usr/local/bin/pg-ha-agent ] && echo yes || echo MISSING)"
echo "PGAUDIT_SO=$([ -f "/usr/lib/postgresql/${PG_MAJOR:-unset}/lib/pgaudit.so" ] && echo yes || echo MISSING)"

# pgaudit must LOAD, not merely be installed: the chart adds it to
# shared_preload_libraries when audit.enabled=true, and PostgreSQL refuses to start if a
# preloaded library is absent -- so a successful start here is the real assertion. Then
# CREATE EXTENSION proves the control/SQL files shipped too, which a bare .so would not.
D=/tmp/smoke-pgdata
mkdir -p "$D" /tmp/smoke-sock && chown postgres:postgres "$D" /tmp/smoke-sock
if gosu postgres "$BINDIR/initdb" -D "$D" --auth-local=trust >/tmp/initdb.log 2>&1; then
    echo "INITDB=ok"
else
    echo "INITDB=failed"
    tail -3 /tmp/initdb.log | sed "s/^/  initdb: /"
fi
if gosu postgres "$BINDIR/pg_ctl" -D "$D" -w -t 60 \
     -o "-c shared_preload_libraries=pgaudit -c port=5433 -c unix_socket_directories=/tmp/smoke-sock" \
     start >/tmp/pgctl.log 2>&1; then
    echo "PGAUDIT_PRELOAD_START=ok"
    echo "SPL=$(gosu postgres psql -h /tmp/smoke-sock -p 5433 -tAX -d postgres -c "SHOW shared_preload_libraries" 2>&1 | tr -d "[:space:]")"
    echo "SERVER_VERSION_NUM=$(gosu postgres psql -h /tmp/smoke-sock -p 5433 -tAX -d postgres -c "SHOW server_version_num" 2>&1 | tr -d "[:space:]")"
    echo "CREATE_EXT=$(gosu postgres psql -h /tmp/smoke-sock -p 5433 -tAX -d postgres -c "CREATE EXTENSION pgaudit" 2>&1 | tr -d "[:space:]")"
    gosu postgres "$BINDIR/pg_ctl" -D "$D" -w -m immediate stop >/dev/null 2>&1
else
    echo "PGAUDIT_PRELOAD_START=failed"
    tail -5 /tmp/pgctl.log | sed "s/^/  pg_ctl: /"
fi
# Last line, unconditionally: its absence tells the caller the probe died partway (or the
# container never started) so every assertion must be treated as failed, not satisfied.
echo "PROBE_COMPLETE=ok"
' 2>&1)
rc=$?

# The probe ends with PROBE_COMPLETE=ok. Requiring that sentinel -- rather than just a
# zero exit or non-empty output -- is what stops a container that never started from
# producing a wall of PASS lines: with no key lines at all, every "is this present?"
# assertion below would otherwise read an empty value and be satisfied.
if ! printf '%s\n' "$probe" | grep -qx 'PROBE_COMPLETE=ok'; then
  echo "--- probe output ---"
  echo "$probe"
  echo "FAIL: the probe did not run to completion in ${IMAGE} (rc=${rc}); treating every check as failed"
  echo "SMOKE FAILED: ${IMAGE}"
  exit 1
fi
echo "--- probe output ---"
echo "$probe"
echo "--- assertions ---"

# An ABSENT key returns the literal <absent> rather than the empty string, so a probe
# line that never ran fails its assertion instead of quietly satisfying a != test.
val() {
  local line
  line=$(printf '%s\n' "$probe" | grep -m1 "^$1=") || { printf '%s' '<absent>'; return; }
  printf '%s' "${line#*=}"
}

# The image must announce its own major: the chart's repmgr.image.majorVersion is a
# render-time claim, while this ENV is what the entrypoint and agent actually follow.
got=$(val ENV_PG_MAJOR)
[ "$got" = "$WANT_MAJOR" ] \
  && ok "PG_MAJOR env is ${WANT_MAJOR}" \
  || bad "PG_MAJOR env is ${got}, want ${WANT_MAJOR}"

[ "$(val BINDIR_EXISTS)" = "yes" ] \
  && ok "versioned bindir /usr/lib/postgresql/${WANT_MAJOR}/bin exists" \
  || bad "versioned bindir for PG_MAJOR=${WANT_MAJOR} does not exist"

# The server binary itself, not the package metadata: this is the acceptance criterion
# ("postgres --version reports 17").
pgver=$(val POSTGRES_VERSION)
case "$pgver" in
  "postgres (PostgreSQL) ${WANT_MAJOR}."*) ok "postgres --version reports ${WANT_MAJOR} (${pgver})" ;;
  *) bad "postgres --version does not report ${WANT_MAJOR}" "$pgver" ;;
esac

svn=$(val SERVER_VERSION_NUM)
case "$svn" in
  "${WANT_MAJOR}"[0-9][0-9][0-9][0-9]) ok "running server reports server_version_num ${svn}" ;;
  *) bad "running server_version_num ${svn} is not PostgreSQL ${WANT_MAJOR}" ;;
esac

# Assert the value the probe emits on success ("yes" / an absolute path). Testing
# `!= MISSING` would pass for an absent or empty key -- which is exactly how a skipped
# pgaudit check could report PASS while audit logging was silently unavailable.
for probe_key in PG_CTL PG_CONTROLDATA AGENT PGAUDIT_SO; do
  got=$(val "$probe_key")
  [ "$got" = "yes" ] \
    && ok "${probe_key} present" \
    || bad "${probe_key} missing from the image" "got ${got}"
done

for probe_key in JQ GOSU CRON; do
  got=$(val "$probe_key")
  case "$got" in
    /*) ok "${probe_key} on PATH (${got})" ;;
    *)  bad "${probe_key} not on PATH (the chart's scripts call it)" "got ${got}" ;;
  esac
done

# repmgrd-entrypoint.sh and the chart's repmgrd container invoke `repmgrd` with no PATH
# manipulation, so bare resolution must work for whichever major is installed.
for probe_key in REPMGR_RESOLVED REPMGRD_RESOLVED; do
  got=$(val "$probe_key")
  case "$got" in
    /*) ok "${probe_key} resolves on the default PATH (${got})" ;;
    *)  bad "${probe_key} does not resolve on the default PATH; the repmgrd container would fail to start" "got ${got}" ;;
  esac
done

# A different repmgr version per major would make the trixie-<repmgr>-<n> tag scheme
# misleading, and puts the cluster on a version the chart was never tested against.
repmgr_v=$(val REPMGR_VERSION)
repmgrd_v=$(val REPMGRD_VERSION)
if [ -n "$WANT_REPMGR" ]; then
  # The tag names a version, so hold the image to it: a mismatch means the tag lies, and
  # the remedy is to publish under the version PGDG now ships.
  for probe_key in REPMGR_VERSION REPMGRD_VERSION; do
    got=$(val "$probe_key")
    case "$got" in
      *"${WANT_REPMGR}"*) ok "${probe_key} is ${WANT_REPMGR} (${got})" ;;
      *) bad "${probe_key} is not ${WANT_REPMGR} as the tag ${IMAGE##*:} advertises; publish under the version PGDG now ships (or pass WANT_REPMGR)" "$got" ;;
    esac
  done
else
  # Untagged/local build: no version to hold it to, but the two binaries must still agree
  # and report something -- a repmgr/repmgrd split would break failover in ways no other
  # check here would notice.
  case "$repmgr_v" in
    "repmgr "[0-9]*) ok "repmgr reports a version (${repmgr_v})" ;;
    *) bad "repmgr did not report a version" "$repmgr_v" ;;
  esac
  case "$repmgrd_v" in
    "repmgrd "[0-9]*) ok "repmgrd reports a version (${repmgrd_v})" ;;
    *) bad "repmgrd did not report a version" "$repmgrd_v" ;;
  esac
  [ "${repmgr_v#repmgr }" = "${repmgrd_v#repmgrd }" ] \
    && ok "repmgr and repmgrd are the same version (${repmgr_v#repmgr })" \
    || bad "repmgr and repmgrd disagree on version" "${repmgr_v} vs ${repmgrd_v}"
  echo "note: ${IMAGE##*:} encodes no repmgr version, so the exact-version check was skipped"
fi

pgbr=$(val PGBACKREST_VERSION)
case "$pgbr" in
  "pgBackRest"*) ok "pgbackrest present (${pgbr})" ;;
  *) bad "pgbackrest missing or not runnable" "$pgbr" ;;
esac

kube=$(val KUBECTL)
case "$kube" in
  *Client*|*version*) ok "kubectl client present (${kube})" ;;
  *) bad "kubectl missing or not runnable" "$kube" ;;
esac

[ "$(val INITDB)" = "ok" ] \
  && ok "initdb creates a cluster with the bundled binaries" \
  || bad "initdb failed"

# The acceptance criterion: pgaudit LOADS on this major when audit.enabled=true.
[ "$(val PGAUDIT_PRELOAD_START)" = "ok" ] \
  && ok "server starts with shared_preload_libraries=pgaudit" \
  || bad "server refused to start with pgaudit preloaded -- audit.enabled=true would crash-loop on PG${WANT_MAJOR}"

spl=$(val SPL)
case "$spl" in
  *pgaudit*) ok "pgaudit is loaded in the running server (${spl})" ;;
  *) bad "pgaudit is not in the running server's shared_preload_libraries" "$spl" ;;
esac

ext=$(val CREATE_EXT)
case "$ext" in
  *CREATEEXTENSION*) ok "CREATE EXTENSION pgaudit succeeds" ;;
  *) bad "CREATE EXTENSION pgaudit failed -- the extension SQL/control files are absent" "$ext" ;;
esac

echo "----"
[ "$fail" -eq 0 ] && echo "SMOKE PASSED: ${IMAGE} is PostgreSQL ${WANT_MAJOR}" \
                  || echo "SMOKE FAILED: ${IMAGE}"
exit "$fail"
