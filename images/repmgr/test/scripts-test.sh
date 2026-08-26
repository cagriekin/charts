#!/bin/bash
# Unit tests for the bash logic shipped in the repmgr image. No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ok()   { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

# --- syntax check every shipped script ---
for s in entrypoint.sh pg-common.sh; do
  if bash -n "${ROOT}/${s}" 2>/dev/null; then ok "bash -n ${s}"; else bad "bash -n ${s}"; fi
done

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

# --- agent failover mode: entrypoint dispatches "agent" -> pg-ha-agent ---
if grep -qF '"postgres"|"agent")' "${ROOT}/entrypoint.sh" && grep -qF 'exec /usr/local/bin/pg-ha-agent' "${ROOT}/entrypoint.sh"; then
  ok "entrypoint dispatches agent mode to pg-ha-agent"
else
  bad "entrypoint does not dispatch agent mode to pg-ha-agent"
fi

# --- #269: the PG major must not be hardcoded anywhere in the shipped shell layer ---
# The whole point of the PG_MAJOR build arg is that one image build can be PG17 or PG18.
# A single re-hardcoded /usr/lib/postgresql/<major>/bin would send a PG17 image at a
# bindir that does not exist -- so scan every shipped file rather than the ones that
# happened to need fixing.
# repmgr.conf was in this list until #290 deleted it. `2>/dev/null || true` meant grep's
# exit-2 for the missing file was swallowed, so the scan silently covered one file fewer than
# the assertion claimed -- and would equally have hidden a real grep failure.
hardcoded=$(grep -rn '/usr/lib/postgresql/1[0-9]' \
  "${ROOT}"/*.sh "${ROOT}/Dockerfile" 2>/dev/null || true)
if [ -z "$hardcoded" ]; then
  ok "#269: no hardcoded versioned bindir in the shipped scripts or Dockerfile"
else
  bad "#269: hardcoded versioned bindir found" "$hardcoded"
fi

# --- #269: PG_BINDIR is defined once, in the shared helper ---
if grep -qE '^PG_BINDIR="/usr/lib/postgresql/\$\{PG_MAJOR\}/bin"$' "${ROOT}/pg-common.sh" \
   && grep -qE '^PG_MAJOR="\$\{PG_MAJOR:-18\}"$' "${ROOT}/pg-common.sh"; then
  ok "#269/#290: pg-common.sh derives PG_BINDIR from PG_MAJOR (default 18)"
else
  bad "#269/#290: pg-common.sh does not define PG_MAJOR/PG_BINDIR as expected"
fi

# --- #269: require_pg_bindir refuses a major the image does not bundle ---
# The chart passes PG_MAJOR from repmgr.image.majorVersion, so this function is where a
# values file asking for a major the image was not built with stops. Behavioral, not
# structural: a bogus major must fail and the message must name both sides, because the
# alternative failure mode is an empty PATH element and a confusing "initdb: not found".
bogus=$( PG_MAJOR=999 bash -c 'source '"${ROOT}"'/pg-common.sh; require_pg_bindir' 2>&1 )
bogus_rc=$?
if [ "$bogus_rc" -ne 0 ]; then
  ok "#269: require_pg_bindir fails for a major the image does not bundle"
else
  bad "#269: require_pg_bindir accepted PG_MAJOR=999"
fi
if grep -q 'PG_MAJOR=999' <<<"$bogus" && grep -qi 'majorVersion' <<<"$bogus"; then
  ok "#269: require_pg_bindir names the requested major and the values to fix"
else
  bad "#269: require_pg_bindir message is not actionable" "$bogus"
fi

# The entrypoint must CALL it -- an unused guard is no guard. One script now, not two:
# init-repmgr.sh went with the mechanism (#290) and its major check moved into the
# entrypoint's own `init` mode.
if grep -q 'require_pg_bindir' "${ROOT}/entrypoint.sh"; then
  ok "#269: entrypoint.sh calls require_pg_bindir"
else
  bad "#269: entrypoint.sh does not call require_pg_bindir"
fi

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
_bi_out=$(PGDATA="${_bi_tmp}/pgdata" POSTGRES_PASSWORD=x REPMGR_PASSWORD=y bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { echo INITDB-RAN; }; pg_ctl() { :; }; psql() { :; }
  bootstrap_initdb 2>/dev/null' || true)
if printf '%s' "${_bi_out}" | grep -q INITDB-RAN; then
  bad "#288: bootstrap_initdb ran initdb over a populated PGDATA"
else
  ok "#288: bootstrap_initdb skipped a populated PGDATA (behavioural)"
fi
rm -f "${_bi_tmp}/pgdata/PG_VERSION"
_bi_out=$(PGDATA="${_bi_tmp}/pgdata" POSTGRES_PASSWORD=x REPMGR_PASSWORD=y bash -c '
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

# --- #290: the image is repmgr-free, and stays that way ---
# Structural guards on the shipped layer. The runtime proof (`docker run <image> repmgr`
# failing, no repmgr.so, no repmgr user or dirs) belongs to the image-smoke test; these are
# what catch a re-introduction in review, before anything is built.

# No shipped script may invoke the repmgr CLI. Comments are excluded: several of them cite
# the deleted commands to explain what the agent replaced.
cli_hits=0
for s in entrypoint.sh pg-common.sh; do
  n=$(grep -vE '^[[:space:]]*#' "${ROOT}/${s}" | grep -cE '(^|[^[:alnum:]_])repmgr[[:space:]]+(standby|node|primary|cluster|service|daemon)([[:space:]]|$)' || true)
  cli_hits=$((cli_hits + n))
done
if [ "$cli_hits" -eq 0 ]; then
  ok "#290: no shipped script invokes the repmgr CLI"
else
  bad "#290: a shipped script still invokes the repmgr CLI (${cli_hits} occurrence(s))"
fi

# The deleted files must stay deleted.
for gone in init-repmgr.sh repmgr-common.sh repmgr.conf; do
  if [ -e "${ROOT}/${gone}" ]; then
    bad "#290: ${gone} is back; its work belongs to the agent"
  else
    ok "#290: ${gone} is gone"
  fi
done

# The Dockerfile must not reinstall the package or recreate the user/dirs/config. Comment
# lines are skipped -- the file explains what was removed and why.
df=$(grep -vE '^[[:space:]]*#' "${ROOT}/Dockerfile")
for pat in 'postgresql-\$\{PG_MAJOR\}-repmgr' 'useradd .*repmgr' '/etc/repmgr' '/var/log/repmgr' 'repmgr\.conf'; do
  if grep -qE "$pat" <<<"$df"; then
    bad "#290: Dockerfile still references ${pat}"
  else
    ok "#290: Dockerfile has no ${pat}"
  fi
done

# The entrypoint must not write the repmgr preload GUC. #293 exists because this line, once
# written, is baked into PGDATA and cloned to every standby -- so re-adding it here would
# silently make every new cluster unstartable on this image.
if grep -vE '^[[:space:]]*#' "${ROOT}/entrypoint.sh" | grep -qE "shared_preload_libraries[[:space:]]*=[[:space:]]*'repmgr'"; then
  bad "#290/#293: entrypoint.sh writes the repmgr preload GUC into PGDATA"
else
  ok "#290/#293: entrypoint.sh writes no repmgr preload GUC"
fi

# ...nor create the extension. The repmgr DATABASE and ROLE are a different matter and must
# SURVIVE: the agent authenticates as that role for replication and names that database in
# primary_conninfo (renaming them is #291).
if grep -vE '^[[:space:]]*#' "${ROOT}/entrypoint.sh" | grep -q 'CREATE EXTENSION IF NOT EXISTS repmgr'; then
  bad "#290: entrypoint.sh still creates the repmgr extension"
else
  ok "#290: entrypoint.sh does not create the repmgr extension"
fi
for keep in 'CREATE DATABASE \${REPMGR_DB}' 'CREATE USER \${REPMGR_USER}'; do
  if grep -qE "$keep" "${ROOT}/entrypoint.sh"; then
    ok "#290: still creates ${keep} (native needs the role and database for replication auth)"
  else
    bad "#290: ${keep} was removed; the agent cannot authenticate for replication without it"
  fi
done

# `init` mode must be the cheap major check, not a bootstrap: no clone, no registration poll.
init_block=$(sed -n '/^    "init")/,/^        ;;/p' "${ROOT}/entrypoint.sh")
if grep -q 'require_pg_bindir' <<<"$init_block" && grep -q 'exit 0' <<<"$init_block"; then
  ok "#290: init mode checks the PG major and exits 0"
else
  bad "#290: init mode is not the reduced major check"
fi
if grep -vE '^[[:space:]]*#' <<<"$init_block" | grep -qE 'pg_basebackup|repmgr|pg_ctl'; then
  bad "#290: init mode still does bootstrap work; the agent owns clone and initdb"
else
  ok "#290: init mode does no bootstrap work"
fi


# --- #290: bootstrap_initdb validates credentials BEFORE touching the volume ---
# It used to resolve them after starting the transient postmaster, so `docker run <img>
# postgres` with neither set ran initdb, appended GUCs, started a postmaster and only then
# died on the unset-parameter check -- leaving PG_VERSION present, no completion sentinel, and
# a postmaster killed with the container. The next run then no-op'd the bootstrap and served a
# cluster with no application roles.
_cred_tmp=$(mktemp -d)
mkdir -p "${_cred_tmp}/pgdata"
_cred_out=$(PGDATA="${_cred_tmp}/pgdata" bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { echo INITDB-RAN; }; pg_ctl() { echo PGCTL-RAN; }; psql() { :; }
  bootstrap_initdb' 2>&1 || true)
if printf '%s' "${_cred_out}" | grep -qE 'INITDB-RAN|PGCTL-RAN'; then
  bad "#290: bootstrap_initdb wrote to the volume before validating credentials" "${_cred_out}"
else
  ok "#290: bootstrap_initdb refuses before initdb when a password is unset"
fi
if printf '%s' "${_cred_out}" | grep -q 'POSTGRES_PASSWORD is required'; then
  ok "#290: the refusal names the missing variable"
else
  bad "#290: the refusal does not name the missing variable" "${_cred_out}"
fi
rm -rf "${_cred_tmp}"

echo "----"
[ "$fail" -eq 0 ] && echo "ALL TESTS PASSED" || echo "TESTS FAILED"
exit "$fail"
