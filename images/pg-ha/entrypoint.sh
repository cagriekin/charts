#!/bin/bash
set -e

SCRIPT_NAME=${1:-default}

# PG_BINDIR / require_pg_bindir (#269).
source /usr/local/bin/pg-common.sh

# The repmgr-only helpers that lived here are gone (#290): scan_peers,
# cluster_was_established, settle_scan_for_primary, reclone_preserving_old and
# primary_safety_guard. Every one of them existed to drive `repmgr node rejoin` /
# `repmgr standby clone` from the shell BEFORE the agent started, and every one was already
# skipped under the native mechanism. The agent owns all of it now: it decides bootstrap vs
# clone vs rejoin from the lease and the timeline (reconcile.Decide), clones with
# pg_basebackup and rewinds with pg_rewind, and it does so as a supervised child rather than
# from a pre-start shell that could not be fenced.
#
# bootstrap_initdb creates a brand-new cluster in an empty PGDATA: initdb, the base GUCs,
# pg_hba, and the managed roles/databases. Extracted into a function so it has exactly ONE
# implementation shared by both mechanisms (#288).
#
# WHO calls it matters: it must NOT run in the postgres|agent branch, because the init
# container does not clone, so every pod would reach it with an empty PGDATA and initdb its own
# independent cluster -- each with a different system_identifier, which assertSameCluster
# (invariant 9) then refuses to rejoin forever. The pod would be Running and never Ready,
# holding a bogus database: strictly worse than the Init:CrashLoopBackOff it replaced.
#
# So under native the AGENT decides, and only the lease holder ever gets here -- via
# `entrypoint.sh initdb` from the BootstrapInitdb action. Non-holders wait, then clone with
# pg_basebackup once the holder is open (reconcile.Decide already encodes exactly this).
bootstrap_initdb() {
    # The emptiness check lives INSIDE the function so BOTH callers are protected: in the
    # caller it would let a boot initdb straight over an existing data directory.
    if [ -s "$PGDATA/PG_VERSION" ]; then
        return 0
    fi

    # Credentials are validated here: AFTER the "already bootstrapped" return, and BEFORE the
    # first write to the volume (#290 review, both rounds).
    #
    # Resolved BEFORE the transient postmaster starts: after it, `docker run <img> postgres`
    # with neither set runs initdb, appends the GUCs, starts a postmaster and only THEN dies
    # on the unset-parameter check -- leaving PG_VERSION present, no completion
    # sentinel, and a postmaster killed uncleanly with the container; the next run then no-op'd
    # the bootstrap and served a cluster with no application roles.
    #
    # Hoisting them above the emptiness check overcorrected: it made an ALREADY-BOOTSTRAPPED
    # directory refuse to start without the passwords, breaking the upstream postgres image
    # contract that a password is required only on first init -- `docker start` on an existing
    # volume, or a compose file whose .env is absent on a later run, would die on a cluster that
    # needs no bootstrap. Here, nothing has touched the volume yet and there IS a bootstrap to
    # do, so the check is both safe and necessary.
    : "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required to bootstrap a new cluster}"
    : "${REPMGR_PASSWORD:?REPMGR_PASSWORD is required to bootstrap a new cluster (the replication role)}"

    # PGBACKREST_STANZA is validated HERE, with the other inputs, and not beside the
    # archive_command it feeds (#298 review). Same rule as the credentials above: every check
    # that can reject the bootstrap must run BEFORE the first write to the volume. Down at the
    # GUC append it ran after initdb had already created the cluster and the base GUCs were
    # written, so a bad stanza left PG_VERSION present with no completion sentinel -- and in
    # `postgres` mode bootstrap_or_discard_torn then discards and re-runs the whole initdb on
    # every restart, turning a one-line input error into a full-initdb-per-boot crash loop
    # instead of a refusal that never touches the volume.
    #
    # Why it is rejected at all: the value is interpolated into the single-quoted
    # archive_command/restore_command GUCs, so a single quote would close the GUC string and
    # hand the remainder to the archiver's /bin/sh. pgBackRest stanza names are alphanumeric
    # plus dash/underscore, so anything else is refused (#298 security review).
    if [ "${PGBACKREST_ENABLED:-}" = "true" ]; then
        case "${PGBACKREST_STANZA:-db}" in
            *[!A-Za-z0-9_-]*)
                echo "FATAL: PGBACKREST_STANZA=\"${PGBACKREST_STANZA}\" contains characters outside [A-Za-z0-9_-]; it is written into the archive_command/restore_command GUCs and must be a plain pgBackRest stanza name. Set pgbackrest.stanza (chart) / PGBACKREST_STANZA (direct image use) to a name matching that pattern." >&2
                exit 1
                ;;
        esac
    fi

    # REPMGR_USER must be a role of its OWN, checked before the first write (#298 review).
    #
    # Every CREATE/ALTER below is deliberately swallowed with `2>/dev/null || true`, and the
    # verification block at the end only asks whether the replication ROLE EXISTS -- the
    # password half was hardened for POSTGRES_USER (pg_authid.rolpassword IS NOT NULL) and not
    # for this one. So a name that collides with POSTGRES_USER, or with initdb's own bootstrap
    # superuser `postgres`, makes `CREATE USER "<repmgr>"` fail as "role already exists", leaves
    # that role holding the OTHER password (or none at all), and still reports bootstrap_ok=yes.
    # The completion sentinel then seals it in: bootstrap_initdb no-ops forever once PG_VERSION
    # exists, while the agent -- which authenticates as REPMGR_USER for every probe and for
    # pg_basebackup -- can never connect. The pod is Running/NotReady for good, recoverable only
    # by deleting the PVC, which is exactly the outcome the verification block exists to prevent.
    #
    # Refusing here costs nothing on the normal path and turns that silent wedge into a loud,
    # named failure before anything has touched the volume.
    _bootstrap_pg_user=${POSTGRES_USER:-postgres}
    _bootstrap_repmgr_user=${REPMGR_USER:-repmgr}
    if [ "$_bootstrap_repmgr_user" = "$_bootstrap_pg_user" ] || [ "$_bootstrap_repmgr_user" = "postgres" ]; then
        echo "FATAL: REPMGR_USER=\"${_bootstrap_repmgr_user}\" must be a role of its own: it may not equal POSTGRES_USER (\"${_bootstrap_pg_user}\") or initdb's bootstrap superuser \"postgres\". Sharing the name leaves the replication role holding a different password, which the HA agent can never authenticate with -- set ha.username (chart) / REPMGR_USER (direct image use) to a distinct name." >&2
        exit 1
    fi
    echo "Initializing PostgreSQL database..."
    # --auth-host=scram-sha-256, not md5 (#298 review). initdb's --auth-host both writes the
    # method into its own pg_hba (which this function overwrites a few lines below, so that half
    # is moot) AND sets `password_encryption` in postgresql.conf -- which is the half that
    # outlives the bootstrap and decides how EVERY password stored on this cluster afterwards is
    # hashed: an operator's `CREATE USER`, the databases-roles hook Job's roles, a later
    # `ALTER USER ... PASSWORD`. Leaving it at md5 meant a brand-new 2.0.0 cluster defaulted to a
    # hash deprecated since PostgreSQL 10. Safe by construction: this function only ever runs
    # against an EMPTY data directory, so no existing md5-hashed role is stranded, and 1.x
    # clusters keep their roles and the agent's md5->scram re-hash.
    initdb -D "$PGDATA" --auth-local=trust --auth-host=scram-sha-256

    cat >> "$PGDATA/postgresql.conf" << EOF
wal_level = replica
max_wal_senders = 10
wal_keep_size = 1GB
hot_standby = on
hot_standby_feedback = on
listen_addresses = '*'
wal_log_hints = on
max_replication_slots = 10
max_slot_wal_keep_size = 4GB
EOF

    # No shared_preload_libraries line at all (#290/#293). Anything written here is baked into
    # the primary's postgresql.conf and cloned verbatim to every standby, so it outlives any
    # chart change and any helm rollback -- and can only be removed by rewriting each PGDATA.
    # The image ships no repmgr.so, so writing it would make every cluster this code creates
    # unstartable ("could not access file \"repmgr\""). The agent strips it from inherited
    # directories; nothing here writes it.

    if [ "${PGBACKREST_ENABLED:-}" = "true" ]; then
        # The stanza was validated at the top of this function, before initdb -- see the
        # comment there for why the check cannot live here, next to the value it guards.
        cat >> "$PGDATA/postgresql.conf" << PGBR
archive_mode = on
archive_command = 'pgbackrest --stanza=${PGBACKREST_STANZA:-db} archive-push %p'
restore_command = 'pgbackrest --stanza=${PGBACKREST_STANZA:-db} archive-get %f "%p"'
PGBR
    fi

    # The catch-all rules authenticate with scram-sha-256, not md5 (#298 review). Two reasons
    # this is safe to tighten rather than a compatibility risk:
    #
    #   - This function only ever runs against an EMPTY data directory, and it sets
    #     password_encryption = 'scram-sha-256' before creating any role, so every password
    #     that can be presented against these rules is stored as a SCRAM verifier. There is no
    #     md5-hashed role for an md5 rule to be needed by -- that case belongs to clusters
    #     initdb'd by chart 1.x, and their pg_hba is the AGENT's to write (it deliberately
    #     emits an md5-first compat form for exactly those roles; see pgconf.AssemblePgHba).
    #   - md5 here was actively worse than useless: PostgreSQL falls back to SCRAM anyway when
    #     the stored secret is a verifier, so the md5 method bought no compatibility on a fresh
    #     cluster while advertising a deprecated one to the network.
    #
    # In agent mode this file is overwritten by writePgHba before the first real start, so the
    # rules below are load-bearing only for `postgres` mode -- a direct image user with no agent,
    # which is precisely the caller that never gets a hardening pass and so should not be handed
    # md5. The transient bootstrap postmaster in this function listens on no TCP address at all
    # (listen_addresses=''), so no `host` rule here is reachable while it runs.
    cat > "$PGDATA/pg_hba.conf" << EOF
local   all             all                                     trust
local   replication     all                                     trust
host    all             all             127.0.0.1/32            trust
host    replication     all             127.0.0.1/32            trust
host    all             all             ::1/128                 trust
host    replication     all             10.0.0.0/8              scram-sha-256
host    all             all             10.0.0.0/8              scram-sha-256
host    replication     all             0.0.0.0/0               scram-sha-256
host    all             all             0.0.0.0/0               scram-sha-256
EOF


    # Wire in the chart's conf.d include (when the chart mounted it) before this function's own
    # pg_ctl start/stop below, not after. shared_preload_libraries is postmaster-only (no
    # reload); the chart's merged value (operator extras/pgaudit) lives in conf.d, and the
    # postStart hook alone would splice it in only once postgres is already accepting
    # connections -- too late to take effect without a second restart, which nothing forces on a
    # fresh install (the config checksum -> rolling restart wiring only helps a later
    # `helm upgrade`, since there is no prior pod to roll on `helm install`) (#303). A directory
    # check, not an env var, because it needs no chart-side signal: the mount is present here iff
    # the chart rendered postgresql-configmap.yaml.
    #
    # Under native the agent ALSO ensures this line (ensureConfdInclude, #288) -- it has to,
    # because on a native fresh install PGDATA does not exist when setup-config runs. Both are
    # idempotent; this one keeps the repmgr path working exactly as it does on 1.x.
    if [ -d /etc/postgresql/conf.d ]; then
        echo "include_dir = '/etc/postgresql/conf.d'" >> "$PGDATA/postgresql.conf"
    fi

    # SOCKET-ONLY, deliberately (#288 review). This transient postmaster exists to create the
    # app/repmgr roles and databases; between `CREATE USER ${REPMGR_USER}` below and the stop at
    # the end of this function it would otherwise be a reachable, authenticable primary reporting
    # pg_is_in_recovery() = false -- and under native a non-holder's very next tick would see it
    # as the live primary and BootstrapClone from it. That standby would inherit the legacy
    # pg_hba (`host all all 0.0.0.0/0`, SUPERUSER-exposing -- md5 until #298 tightened it to
    # scram-sha-256, which narrows the method but not the exposure) for the rest of the pod's life,
    # because nothing on the clone path rewrites pg_hba, and its cloned postgresql.conf would
    # have no include_dir, inverting the conf.d precedence too. Under repmgr the window did not
    # exist: every standby was blocked until the primary registered in repmgr.nodes, which only
    # happens after the real start. Listening on no TCP address closes it entirely -- the local
    # psql calls below all use the unix socket.
    # log_statement/log_min_error_statement are load-bearing, not tidiness (#298 review).
    # pg_ctl is deliberately run without -l, so this transient postmaster inherits THIS
    # script's stdout/stderr (see the stop at the end of the function), and PostgreSQL's
    # defaults are logging_collector=off, log_destination=stderr,
    # log_min_error_statement=error -- so any statement that raises ERROR is echoed back
    # with a `STATEMENT:` line carrying the statement verbatim. On a DEFAULT install that is
    # guaranteed, not hypothetical: postgresql.username is `postgres`, initdb has already
    # created that bootstrap superuser, so `CREATE USER "postgres" ... PASSWORD '<secret>'`
    # fails with "role already exists" and the server logs the cleartext password. The
    # `2>/dev/null` on the psql calls below silences only the CLIENT; the server writes its
    # own copy, which in postgres mode lands on container stdout and in initdb mode is
    # captured by the agent's CombinedOutput and re-emitted at Info level. Moving the
    # secrets off argv (#298) closed the smaller channel and left this one open.
    pg_ctl -D "$PGDATA" -w start -o "-c listen_addresses='' -c log_statement=none -c log_min_error_statement=panic"

    REPMGR_USER=${REPMGR_USER:-repmgr}
    REPMGR_PASSWORD=${REPMGR_PASSWORD:?REPMGR_PASSWORD is required}
    REPMGR_DB=${REPMGR_DB:-repmgr}
    POSTGRES_USER=${POSTGRES_USER:-postgres}
    POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    POSTGRES_DB=${POSTGRES_DB:-postgres}

    # Force scram-sha-256 per statement, belt-and-braces over the cluster default. pg_hba
    # requires scram from the pod network and PostgreSQL never falls through on auth failure,
    # so a role holding an MD5 secret is rejected with "does not have a valid SCRAM secret"
    # the moment a standby clone connects -- a startup race that wedged the clone. Stating it
    # at creation keeps that true even if the default were changed back.
    # SQL-quote the passwords (#298 review): a literal single quote in POSTGRES_PASSWORD /
    # REPMGR_PASSWORD ended the SQL string, the `2>/dev/null || true` swallowed the syntax
    # error, and -- because the verification below only asked about the repmgr objects -- a
    # POSTGRES_USER left with NO password was sealed in by the completion sentinel: initdb
    # ran with no --pwfile, pg_hba requires scram from the network, so application auth was
    # broken forever while the bootstrap never re-ran. Doubling the quote is the SQL escape;
    # the extended verification below still backstops anything else that slips through.
    pg_pw_sql=${POSTGRES_PASSWORD//\'/\'\'}
    repmgr_pw_sql=${REPMGR_PASSWORD//\'/\'\'}
    # QUOTE the identifiers, and quote them everywhere (#298 review). Unquoted, PostgreSQL folds
    # an identifier to lower case: POSTGRES_USER=MyApp created role `myapp`, while the
    # verification below asks pg_authid for rolname = 'MyApp' -- so it failed, exited before the
    # sentinel, and the agent discarded the fresh directory and re-bootstrapped with the same
    # env: a permanent loop whose FATAL hint blames password quoting. (1.x had the identical
    # unquoted CREATEs but no verification, so the same values booted; this was a 2.0.0
    # regression.) Folding is wrong on its own terms too: libpq sends `-U MyApp` verbatim and the
    # server compares it exactly, so a folded role could never be logged into, and
    # primary_conninfo's dbname=REPMGR_DB has the same problem. Double any embedded double quote,
    # which is the identifier escape, so a name cannot terminate the quoted identifier.
    # ...and single-quote-escaped copies for the VERIFICATION queries below, where the same
    # names appear as string literals rather than identifiers.
    pg_user_lit=${POSTGRES_USER//\'/\'\'}
    pg_db_lit=${POSTGRES_DB//\'/\'\'}
    repmgr_user_lit=${REPMGR_USER//\'/\'\'}
    repmgr_db_lit=${REPMGR_DB//\'/\'\'}
    pg_user_id=${POSTGRES_USER//\"/\"\"}
    pg_db_id=${POSTGRES_DB//\"/\"\"}
    repmgr_user_id=${REPMGR_USER//\"/\"\"}
    repmgr_db_id=${REPMGR_DB//\"/\"\"}

    # The password-bearing statements go in on STDIN, not `psql -c` (#298 security review):
    # a `-c "... PASSWORD '...'"` argument puts the cleartext password in the process's argv,
    # readable from /proc/<pid>/cmdline by any same-uid process or a `ps` on the node for the
    # life of the call. A heredoc keeps it off argv. psql does not stop on error by default
    # (no ON_ERROR_STOP), so combining CREATE and the password-resetting ALTER in one session
    # keeps the prior behaviour: a CREATE that fails because the role already exists still
    # lets the ALTER reset the password. Statements with no secret stay as -c.
    psql -U postgres -d postgres -c "CREATE DATABASE \"${pg_db_id}\";" 2>/dev/null || true
    psql -U postgres -d postgres 2>/dev/null <<SQL || true
SET password_encryption='scram-sha-256';
CREATE USER "${pg_user_id}" WITH SUPERUSER PASSWORD '${pg_pw_sql}';
ALTER USER "${pg_user_id}" WITH PASSWORD '${pg_pw_sql}';
SQL

    psql -U postgres -d postgres -c "CREATE DATABASE \"${repmgr_db_id}\";" 2>/dev/null || true
    psql -U postgres -d postgres 2>/dev/null <<SQL || true
SET password_encryption='scram-sha-256';
CREATE USER "${repmgr_user_id}" WITH SUPERUSER PASSWORD '${repmgr_pw_sql}';
SQL
    psql -U postgres -d postgres -c "GRANT ALL PRIVILEGES ON DATABASE \"${repmgr_db_id}\" TO \"${repmgr_user_id}\";" 2>/dev/null || true
    # The repmgr DATABASE and ROLE above are created under BOTH mechanisms, on purpose.
    # #288 asked for native bootstrap to create none of the three, but the role and the
    # database are load-bearing in native mode too: the agent connects as REPMGR_USER for
    # every probe and for pg_basebackup, and primary_conninfo carries dbname=REPMGR_DB.
    # Dropping them would break native outright. Renaming them out of the repmgr
    # namespace is #291 (ha.* values), not this issue.
    #
    # No CREATE EXTENSION repmgr (#290): the extension does not exist in this image, and the
    # nodes table it created stopped being a topology source in #288. A cluster inherited from a
    # 1.x release still HAS the extension -- dropping it is the operator's opt-in cleanup step
    # (#292), never something an upgrade does on its own.

    # Verify the bootstrap actually produced the load-bearing objects, while the postmaster is
    # still UP to be asked (#294 review). Every role/database step above ends in
    # `2>/dev/null || true` -- deliberately, so a re-run over an existing object is not fatal --
    # which makes a genuine failure indistinguishable from an idempotent no-op. The completion
    # sentinel written after the stop below then told discardTornInitdb "this bootstrap
    # completed", so it KEPT a data directory with no repmgr role: bootstrap_initdb no-ops on it
    # forever (PG_VERSION exists) while the agent can never authenticate -- exactly the
    # Running/NotReady-until-someone-deletes-the-PVC state that sentinel exists to prevent.
    #
    # Every load-bearing object is checked: the agent connects as REPMGR_USER for every
    # probe and for pg_basebackup, primary_conninfo carries dbname=REPMGR_DB, and the
    # POSTGRES_USER/POSTGRES_DB pair is what applications authenticate against. Failing here
    # exits before the sentinel is written, so the next boot discards the torn directory and
    # starts over -- a loud crash-loop naming the cause, rather than a silent wedge.
    bootstrap_ok=yes
    # pg_authid + `rolpassword IS NOT NULL`, symmetrically with the POSTGRES_USER arm below
    # (#298 review). Existence alone is not the property the agent needs: it authenticates as
    # this role over TCP for every probe and for pg_basebackup, so a role that exists WITHOUT
    # a password is as dead to it as one that was never created.
    #
    # The case that gets here is a REPMGR_USER colliding with a role initdb already made. The
    # guard at the top of bootstrap_initdb catches the two names it can name (POSTGRES_USER and
    # `postgres`), but PostgreSQL reserves the whole `pg_` prefix -- pg_monitor,
    # pg_read_all_data, pg_signal_backend and friends all exist in a fresh cluster and are
    # NOLOGIN with no password. `CREATE USER "pg_monitor" ...` fails ("role name pg_monitor is
    # reserved"), the failure is swallowed by the deliberate `2>/dev/null || true`, and an
    # existence-only check then reports bootstrap_ok=yes -- sealing the completion sentinel over
    # a cluster the agent can never log in to. pg_roles.rolpassword always reads NULL, hence
    # pg_authid.
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_authid WHERE rolname = '${repmgr_user_lit}' AND rolpassword IS NOT NULL" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not leave the ${REPMGR_USER} role with a password (a name PostgreSQL already owns -- anything starting with 'pg_' is reserved -- cannot be created as the replication role; pick a different ha.username / REPMGR_USER)." >&2
        bootstrap_ok=no
    fi
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '${repmgr_db_lit}'" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not create the ${REPMGR_DB} database." >&2
        bootstrap_ok=no
    fi
    # The POSTGRES side is load-bearing too (#298 review): the superuser must end up WITH a
    # password -- initdb ran with no --pwfile, so a swallowed CREATE/ALTER USER left it NULL
    # and every network login dead -- and the app database must exist. pg_authid, not
    # pg_roles: pg_roles.rolpassword always reads as NULL, even for superusers.
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_authid WHERE rolname = '${pg_user_lit}' AND rolpassword IS NOT NULL" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not leave the ${POSTGRES_USER} role with a password." >&2
        bootstrap_ok=no
    fi
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '${pg_db_lit}'" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not create the ${POSTGRES_DB} database." >&2
        bootstrap_ok=no
    fi
    if [ "$bootstrap_ok" != "yes" ]; then
        # STOP the transient postmaster before exiting (#298). `pg_ctl -w start` daemonizes
        # one that inherits this script's stdout, and in agent mode this script is a captured
        # child (Cmd.Output) -- so an orphan holds that pipe open past EOF and blocks act()
        # with opMu held, for the whole initdbBudget. Immediate, since this cluster is about
        # to be discarded; best-effort, so a failure here cannot mask the FATAL below.
        pg_ctl -D "$PGDATA" -m immediate -w stop || true
        echo "FATAL: not marking the bootstrap complete; this data directory will be discarded and re-bootstrapped on the next start. If it repeats, check POSTGRES_PASSWORD / REPMGR_PASSWORD and the user/database NAMES for characters that break the SQL above, and the postgres server log for the swallowed error." >&2
        exit 1
    fi

    # Escalate rather than exit with it still up -- same orphan hazard as the FATAL arm above
    # (#298). Bare under `set -e` this could exit with the postmaster alive, because
    # `pg_ctl -w stop` fails on PGCTLTIMEOUT (60s) on a contended node long before the cluster
    # is down. Exiting without the sentinel is right: the next start discards and re-creates.
    if ! pg_ctl -D "$PGDATA" -w stop; then
        pg_ctl -D "$PGDATA" -m immediate -w stop || true
        echo "FATAL: could not stop the transient bootstrap postmaster; not marking the bootstrap complete, so this data directory will be discarded and re-created on the next start." >&2
        exit 1
    fi

    # LAST action, and the only positive evidence that this multi-step bootstrap finished
    # (#288 review). initdb writes a perfectly valid pg_control within its first second, so
    # "the directory looks like a cluster" cannot tell a COMPLETE bootstrap apart from one
    # killed between the postmaster start above and the role/database creation -- and that
    # half-bootstrapped state is unrecoverable: bootstrap_initdb no-ops on it forever
    # (PG_VERSION exists), while the agent can never authenticate as REPMGR_USER, so the pod
    # comes up Running/NotReady until someone deletes the PVC. The kill is reachable because
    # `pg_ctl start` above satisfies the chart's startupProbe (`pg_isready` with no -h is
    # answered over the unix socket), which retires the startup grace and arms the liveness
    # probe while the agent is still inside this exec and not beating /healthz.
    #
    # The agent pairs this with its own in-progress marker beside PGDATA: marker present and
    # this file absent = torn, discard and start over. Written after the stop so it can never
    # be mistaken for a state a running postmaster is still mutating. Cloned to standbys along
    # with the rest of PGDATA, which is correct -- their directory did come from a completed
    # bootstrap.
    : > "$PGDATA/.pg-ha-bootstrap-complete"

    echo "PostgreSQL initialization complete"
}

# bootstrap_or_discard_torn wraps bootstrap_initdb for `postgres` mode, where a torn bootstrap
# would otherwise seal in permanently (#298). bootstrap_initdb no-ops on any PGDATA that already
# has PG_VERSION, so an initdb that ran and then died before writing the completion sentinel --
# the FATAL verification exit inside the function, a SIGKILL mid-bootstrap, a container lost
# between the two -- left a cluster with no application role and no application database, which
# every later start then served happily and forever. In agent mode the agent's discardTornInitdb
# already recovers exactly this; nothing in `postgres` mode read the sentinel at all.
#
# The agent's evidence rule is mirrored deliberately, including its safety property: the
# IN-PROGRESS marker is REQUIRED, not merely an absent completion sentinel. A data directory
# created by an older image has PG_VERSION, no sentinel and no marker, and must never be wiped on
# that basis -- "I cannot see proof it finished" is not proof that it did not. Both marker paths
# match the agent's (initdbMarkerPath / bootstrapCompletePath) so the two modes cannot disagree
# about what torn means.
#
# The in-progress marker lives BESIDE PGDATA rather than inside it, for two reasons: initdb
# refuses a target directory that is not empty, and it has to survive the discard below.
bootstrap_or_discard_torn() {
    pgdata_parent=$(dirname "$PGDATA")
    initdb_marker="${pgdata_parent}/.pg-ha-initdb-in-progress"
    # PG_VERSION present OR merely non-empty (#298 review). Requiring PG_VERSION left a
    # hole: a bootstrap SIGKILLed while initdb was still laying out subdirectories, or a
    # previous discard whose `rm -rf` partly failed (the failure is swallowed just below),
    # leaves PGDATA non-empty with no PG_VERSION -- and initdb refuses a non-empty
    # target, so bootstrap_initdb's own emptiness check passes it straight through to a
    # permanent "directory exists but is not empty" crash-loop with no path out. The
    # IN-PROGRESS MARKER is still required, so this can never fire on a directory some
    # older image created; and with no PG_VERSION there is by definition no cluster to lose.
    if [ -f "$initdb_marker" ] && [ ! -f "$PGDATA/.pg-ha-bootstrap-complete" ] &&
        { [ -s "$PGDATA/PG_VERSION" ] || [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ]; }; then
        echo "WARNING: discarding a data directory left by an interrupted bootstrap (completion sentinel absent) so it can be created again (#298)" >&2
        # The `|| true` is for the globs, not for rm's errors: on a directory that is empty
        # (or holds only entries one glob misses) the unmatched pattern reaches rm as a
        # literal and it exits non-zero under `set -e` for nothing. It therefore also
        # swallows a REAL failure -- EPERM on a root-owned file a 1.x init container left
        # behind, EROFS on a remounted volume -- so the result is VERIFIED below rather than
        # assumed (#298 review). Without that check the discard silently removed nothing,
        # bootstrap_initdb then ran `initdb -D` against a still-populated directory, and the
        # container crash-looped forever on `directory "..." exists but is not empty` with the
        # actual cause (the rm that failed) printed nowhere. `ls -A` is the check rather than
        # the globs' own success, because `.[!.]*` also misses a `..`-prefixed entry.
        rm -rf "${PGDATA:?}"/* "${PGDATA:?}"/.[!.]* 2>/dev/null || true
        if [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
            echo "FATAL: could not empty ${PGDATA} to re-run the interrupted bootstrap; these entries survived the discard (check ownership/permissions on the data volume -- a 1.x release ran its init container as root):" >&2
            ls -A "$PGDATA" >&2
            exit 1
        fi
    fi
    if [ ! -s "$PGDATA/PG_VERSION" ]; then
        mkdir -p "$pgdata_parent"
        : > "$initdb_marker"
    fi
    bootstrap_initdb
    # Cleared only after bootstrap_initdb RETURNS. It exits non-zero on a failed verification,
    # so the marker survives exactly the case it exists for.
    rm -f "$initdb_marker"
}

case "$SCRIPT_NAME" in
    "postgres"|"agent")
        require_pg_bindir || exit 1
        export PATH=$PATH:$PG_BINDIR
        PGDATA=${PGDATA:-/var/lib/postgresql/data/pgdata}
        export PGDATA

        if [ "$(id -u)" = "0" ]; then
            exec gosu postgres "$0" "$@"
        fi

        # agent mode: hand off to the Go HA agent as PID 1; it writes its own config, starts and
        # supervises PostgreSQL, and runs the lease-based failover loop.
        #
        # It must NOT initdb here (#288/#290): the agent decides, and only the lease holder
        # bootstraps -- via `entrypoint.sh initdb` from the BootstrapInitdb action -- while every
        # other pod waits and then clones with pg_basebackup. Doing it in this shell would give
        # each pod its own cluster with its own system_identifier, which assertSameCluster
        # (invariant 9) then refuses to rejoin forever: Running, never Ready, holding a bogus
        # database.
        if [ "$SCRIPT_NAME" = "agent" ]; then
            if [ ! -s "$PGDATA/PG_VERSION" ]; then
                echo "empty data directory; deferring to the agent (initdb if lease holder, else clone) (#288)"
            fi
            echo "Starting pg-ha-agent (PID 1; the agent manages PostgreSQL)..."
            exec /usr/local/bin/pg-ha-agent
        fi

        # postgres mode: a plain single-node postmaster, for running this image directly with no
        # agent and no HA. It DOES bootstrap its own cluster, because nothing else will (#290):
        # bootstrap_initdb is otherwise the agent's to call, which would leave this branch
        # exec'ing a postmaster at an empty data directory, exiting immediately. The chart never
        # reaches here -- its own `postgres` command is
        # inside an agent-mode guard and therefore unreachable -- so this is purely for direct
        # image users, which is exactly who has no agent to do it for them.
        bootstrap_or_discard_torn
        echo "Starting PostgreSQL..."
        exec postgres -D "$PGDATA"
        ;;
    "initdb")
        # Invoked by the agent under native mode (#288) for the lease holder only.
        require_pg_bindir || exit 1
        export PATH=$PATH:$PG_BINDIR
        PGDATA=${PGDATA:-/var/lib/postgresql/data/pgdata}
        export PGDATA
        if [ "$(id -u)" = "0" ]; then
            exec gosu postgres "$0" "$@"
        fi
        bootstrap_initdb
        ;;
    "init")
        # The init container (#290). init-repmgr.sh is gone: everything past its
        # `MECHANISM=native` early-exit was repmgr work -- writing repmgr.conf, polling
        # repmgr.nodes for a registered primary, cloning with `repmgr standby clone` -- and the
        # agent does all of it now, from inside the postgresql container where it can be fenced.
        #
        # What remains is worth keeping as its own container: fail the pod HERE, in Init, when
        # PG_MAJOR names a major this image does not bundle. Reaching the main container first
        # would surface the same mismatch as `initdb: command not found` or a clone loop (#269).
        require_pg_bindir || exit 1
        echo "init: PostgreSQL ${PG_MAJOR} binaries present at ${PG_BINDIR}; the agent owns bootstrap and cloning (#288/#290)."
        exit 0
        ;;
    *)
        # repmgrd / service-updater were removed with the repmgrd failover path (#286).
        echo "Usage: $0 {postgres|agent|init|initdb}"
        exit 1
        ;;
esac
