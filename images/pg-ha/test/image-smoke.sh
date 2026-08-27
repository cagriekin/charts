#!/bin/bash
# Smoke-test a BUILT pg-ha image: assert it really bundles the PostgreSQL major it claims,
# that everything the chart depends on is present and runnable (#269), and that repmgr is
# genuinely gone (#290).
#
# scripts-test.sh checks the shell logic without a container; this checks the artifact.
# The two together are what make `--build-arg PG_MAJOR=17` trustworthy: a build can
# succeed, ship a working PG17 server, and still be unusable because a per-major
# package resolved to a different version than the tag advertises.
#
# Usage: bash images/pg-ha/test/image-smoke.sh <image-ref> <expected-pg-major>
#   bash images/pg-ha/test/image-smoke.sh cagriekin/pg-ha:2.0.0-pg17 17
set -uo pipefail

IMAGE="${1:?usage: image-smoke.sh <image-ref> <expected-pg-major>}"
WANT_MAJOR="${2:?usage: image-smoke.sh <image-ref> <expected-pg-major>}"

# The repmgr version this used to derive from the tag is gone with the package (#290): there
# is no repmgr to hold to a version.
#
# The TAG SCHEME changed with it: pg-ha-image-publish.yaml takes a git tag `pg-ha-<version>` and
# publishes cagriekin/pg-ha:<version>-pg<major>. The version tracks the CHART release the image
# shipped with -- the number a consumer already reasons about -- and the PostgreSQL major stays
# in the tag because one build bundles exactly one.

fail=0
ok()  { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; fail=1; }

command -v docker >/dev/null 2>&1 || { echo "docker is required to smoke-test an image" >&2; exit 1; }

echo "=== SMOKE: ${IMAGE} (expecting PostgreSQL ${WANT_MAJOR}, and NO repmgr) ==="

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
# #290: repmgr must be ABSENT. Probed exactly as a container that never extends PATH would
# resolve it, plus the library, OS user and directories the package created.
# NOTE: this whole block is a single-quoted bash -c string. No apostrophes, no backticks.
echo "REPMGR_RESOLVED=$(PATH="$IMAGE_PATH" command -v repmgr 2>/dev/null || echo MISSING)"
echo "REPMGR_SO=$([ -f "/usr/lib/postgresql/${PG_MAJOR:-unset}/lib/repmgr.so" ] && echo PRESENT || echo ABSENT)"
echo "REPMGR_USER=$(id -u repmgr 2>/dev/null || echo ABSENT)"
REPMGR_DIRS_FOUND=$(ls -d /etc/repmgr /var/log/repmgr 2>/dev/null | tr "\n" "," | sed "s/,$//")
echo "REPMGR_DIRS=${REPMGR_DIRS_FOUND:-ABSENT}"
echo "PGBACKREST_VERSION=$(pgbackrest version 2>&1 | head -1)"
echo "JQ=$(command -v jq || echo MISSING)"
echo "GOSU=$(command -v gosu || echo MISSING)"
echo "CRON=$(command -v cron || echo MISSING)"
echo "KUBECTL=$(command -v kubectl 2>/dev/null || echo ABSENT)"
echo "AGENT=$([ -x /usr/local/bin/pg-ha-agent ] && echo yes || echo MISSING)"
# The postgres uid/gid are load-bearing OUTSIDE this image: the chart chowns PGDATA to a
# literal 101:103 and pins runAsUser/runAsGroup to the same. They are allocated by the apt
# run, so any change to the package set can move them (#290 dropped a package) -- and the
# failure would surface only after a tag bump, as "data directory has invalid permissions"
# on every pod, with nothing in CI having looked.
echo "POSTGRES_UID=$(id -u postgres 2>/dev/null || echo MISSING)"
echo "POSTGRES_GID=$(id -g postgres 2>/dev/null || echo MISSING)"
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

# #290: repmgr must be genuinely ABSENT, not merely unused. This is the acceptance criterion
# the issue states as `docker run <image> repmgr` failing -- an image that still carries the
# binary would let a re-introduced code path work in testing and fail only where the package
# was actually dropped.
for probe_key in REPMGR_RESOLVED; do
  got=$(val "$probe_key")
  case "$got" in
    MISSING) ok "#290: repmgr does not resolve on the default PATH (absent)" ;;
    *) bad "#290: repmgr is still installed at ${got}; the image is supposed to be repmgr-free" ;;
  esac
done
for probe_key in REPMGR_SO REPMGR_USER REPMGR_DIRS; do
  got=$(val "$probe_key")
  case "$got" in
    ABSENT) ok "#290: ${probe_key} is absent" ;;
    *) bad "#290: ${probe_key} is still present (${got})" ;;
  esac
done

# The chart hardcodes these two numbers (pg/values.yaml runAsUser/runAsGroup and the
# statefulset's `chown -R 101:103`), so the image has to keep its side of the bargain.
for pair in "POSTGRES_UID=101" "POSTGRES_GID=103"; do
  probe_key="${pair%%=*}"; want="${pair##*=}"
  got=$(val "$probe_key")
  if [ "$got" = "$want" ]; then
    ok "${probe_key} is ${want}, matching what the chart chowns PGDATA to"
  else
    bad "${probe_key} is ${got}, but the chart chowns PGDATA to 101:103 and pins runAsUser/runAsGroup -- every pod would fail at postmaster start with \"data directory has invalid permissions\"" "an apt package-set change can move a system uid; update the chart and this assertion together, deliberately"
  fi
done

pgbr=$(val PGBACKREST_VERSION)
case "$pgbr" in
  "pgBackRest"*) ok "pgbackrest present (${pgbr})" ;;
  *) bad "pgbackrest missing or not runnable" "$pgbr" ;;
esac

# #286: kubectl was installed ONLY for the repmgrd-mode service-updater sidecar. That
# sidecar is gone, the agent uses client-go inside its own binary, and the pgbackrest
# CronJobs run kubectl from their own alpine/k8s image -- so nothing in this image shells
# out to it. Assert it is absent: a reappearance is dead weight in the CVE surface.
kube=$(val KUBECTL)
case "$kube" in
  ABSENT) ok "kubectl is absent (removed with the service-updater, #286)" ;;
  *) bad "kubectl is back in the image; nothing here uses it (#286)" "$kube" ;;
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
