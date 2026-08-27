#!/bin/bash
set -e

SCRIPT_NAME=${1:-default}

# PG_BINDIR / require_pg_bindir (#269). The repmgr topology and timeline helpers this file
# used to source alongside them went with the mechanism (#290) -- their only callers were the
# repmgr paths removed below.
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
# WHO calls it differs, and that is the whole point. Under repmgr the postgres|agent branch
# calls it inline, as it always did. Under native it must NOT run there: the init container
# no longer clones, so every pod would reach it with an empty PGDATA and initdb its own
# independent cluster -- each with a different system_identifier, which assertSameCluster
# (invariant 9) then refuses to rejoin forever. The pod would be Running and never Ready,
# holding a bogus database: strictly worse than the Init:CrashLoopBackOff it replaced.
#
# So under native the AGENT decides, and only the lease holder ever gets here -- via
# `entrypoint.sh initdb` from the BootstrapInitdb action. Non-holders wait, then clone with
# pg_basebackup once the holder is open (reconcile.Decide already encodes exactly this).
bootstrap_initdb() {
    # The emptiness check lives INSIDE the function so BOTH callers are protected. It was
    # previously the caller's `if [ ! -s PG_VERSION ]`, and moving it out would have let a
    # boot initdb straight over an existing data directory.
    if [ -s "$PGDATA/PG_VERSION" ]; then
        return 0
    fi

    # Credentials are validated here: AFTER the "already bootstrapped" return, and BEFORE the
    # first write to the volume (#290 review, both rounds).
    #
    # They used to be resolved after the transient postmaster started, so `docker run <img>
    # postgres` with neither set ran initdb, appended the GUCs, started a postmaster and only
    # THEN died on the unset-parameter check -- leaving PG_VERSION present, no completion
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
    echo "Initializing PostgreSQL database..."
    initdb -D "$PGDATA" --auth-local=trust --auth-host=md5

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

    # No shared_preload_libraries line at all (#290). This used to append
    # `shared_preload_libraries = 'repmgr'` here, which #293 then had to strip back out of every
    # existing data directory -- because the line is baked into the primary's postgresql.conf and
    # cloned verbatim to every standby, so it outlives any chart change and any helm rollback.
    # The image no longer ships repmgr.so, so writing it would make every cluster this code
    # creates unstartable ("could not access file \"repmgr\""). The agent still strips it from
    # inherited directories; nothing writes it any more.

    if [ "${PGBACKREST_ENABLED:-}" = "true" ]; then
        cat >> "$PGDATA/postgresql.conf" << PGBR
archive_mode = on
archive_command = 'pgbackrest --stanza=${PGBACKREST_STANZA:-db} archive-push %p'
restore_command = 'pgbackrest --stanza=${PGBACKREST_STANZA:-db} archive-get %f "%p"'
PGBR
    fi

    cat > "$PGDATA/pg_hba.conf" << EOF
local   all             all                                     trust
local   replication     all                                     trust
host    all             all             127.0.0.1/32            trust
host    replication     all             127.0.0.1/32            trust
host    all             all             ::1/128                 trust
host    replication     all             10.0.0.0/8              scram-sha-256
host    all             all             10.0.0.0/8              scram-sha-256
host    replication     all             0.0.0.0/0               md5
host    all             all             0.0.0.0/0               md5
EOF


    # Wire in the chart's conf.d include (when the chart mounted it) before this function's own
    # pg_ctl start/stop below, not after. shared_preload_libraries is postmaster-only (no
    # reload); the chart's merged value (repmgr + operator extras/pgaudit) lives in conf.d,
    # previously only spliced in by the postStart hook once postgres was already accepting
    # connections -- too late to take effect without a second restart, which nothing forced on a
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
    # pg_hba (`host all all 0.0.0.0/0 md5`, SUPERUSER-exposing) for the rest of the pod's life,
    # because nothing on the clone path rewrites pg_hba, and its cloned postgresql.conf would
    # have no include_dir, inverting the conf.d precedence too. Under repmgr the window did not
    # exist: every standby was blocked until the primary registered in repmgr.nodes, which only
    # happens after the real start. Listening on no TCP address closes it entirely -- the local
    # psql calls below all use the unix socket.
    pg_ctl -D "$PGDATA" -w start -o "-c listen_addresses=''"

    REPMGR_USER=${REPMGR_USER:-repmgr}
    REPMGR_PASSWORD=${REPMGR_PASSWORD:?REPMGR_PASSWORD is required}
    REPMGR_DB=${REPMGR_DB:-repmgr}
    POSTGRES_USER=${POSTGRES_USER:-postgres}
    POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    POSTGRES_DB=${POSTGRES_DB:-postgres}

    # initdb --auth-host=md5 (above) writes password_encryption=md5 into
    # postgresql.conf, so a bare CREATE USER stores an MD5 secret. pg_hba.conf
    # (written above) requires scram-sha-256 for the 10.0.0.0/8 pod network,
    # and PostgreSQL never falls through on auth failure -- so an MD5-secret
    # repmgr user is rejected with "does not have a valid SCRAM secret" the
    # moment a standby clone or repmgrd connects over the pod network, before
    # the chart's postStart md5->scram migration has had a chance to run. That
    # is a startup race that crash-loops repmgrd / wedges the standby clone.
    # Create the managed users with a SCRAM secret directly (a session-scoped
    # SET applies to the CREATE/ALTER in that same psql -c session) -- the same end state the chart's
    # fix_user_auth migration drives them to, but race-free from first boot.
    # Legacy/app users keep the md5 default and the existing migration path.
    # SQL-quote the passwords (#298 review): a literal single quote in POSTGRES_PASSWORD /
    # REPMGR_PASSWORD ended the SQL string, the `2>/dev/null || true` swallowed the syntax
    # error, and -- because the verification below only asked about the repmgr objects -- a
    # POSTGRES_USER left with NO password was sealed in by the completion sentinel: initdb
    # ran with no --pwfile, pg_hba requires scram from the network, so application auth was
    # broken forever while the bootstrap never re-ran. Doubling the quote is the SQL escape;
    # the extended verification below still backstops anything else that slips through.
    pg_pw_sql=${POSTGRES_PASSWORD//\'/\'\'}
    repmgr_pw_sql=${REPMGR_PASSWORD//\'/\'\'}

    psql -U postgres -d postgres -c "CREATE DATABASE ${POSTGRES_DB};" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; CREATE USER ${POSTGRES_USER} WITH SUPERUSER PASSWORD '${pg_pw_sql}';" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; ALTER USER ${POSTGRES_USER} WITH PASSWORD '${pg_pw_sql}';" 2>/dev/null || true

    psql -U postgres -d postgres -c "CREATE DATABASE ${REPMGR_DB};" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; CREATE USER ${REPMGR_USER} WITH SUPERUSER PASSWORD '${repmgr_pw_sql}';" 2>/dev/null || true
    psql -U postgres -d postgres -c "GRANT ALL PRIVILEGES ON DATABASE ${REPMGR_DB} TO ${REPMGR_USER};" 2>/dev/null || true
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
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = '${REPMGR_USER}'" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not create the ${REPMGR_USER} role." >&2
        bootstrap_ok=no
    fi
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '${REPMGR_DB}'" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not create the ${REPMGR_DB} database." >&2
        bootstrap_ok=no
    fi
    # The POSTGRES side is load-bearing too (#298 review): the superuser must end up WITH a
    # password -- initdb ran with no --pwfile, so a swallowed CREATE/ALTER USER left it NULL
    # and every network login dead -- and the app database must exist. pg_authid, not
    # pg_roles: pg_roles.rolpassword always reads as NULL, even for superusers.
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_authid WHERE rolname = '${POSTGRES_USER}' AND rolpassword IS NOT NULL" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not leave the ${POSTGRES_USER} role with a password." >&2
        bootstrap_ok=no
    fi
    if ! psql -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '${POSTGRES_DB}'" 2>/dev/null | grep -q 1; then
        echo "FATAL: bootstrap did not create the ${POSTGRES_DB} database." >&2
        bootstrap_ok=no
    fi
    if [ "$bootstrap_ok" != "yes" ]; then
        echo "FATAL: not marking the bootstrap complete; this data directory will be discarded and re-bootstrapped on the next start. If it repeats, check POSTGRES_PASSWORD / REPMGR_PASSWORD for characters that break the SQL above (a literal single quote is the usual culprit) and the postgres server log for the swallowed error." >&2
        exit 1
    fi

    pg_ctl -D "$PGDATA" -w stop

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
        # agent and no HA. It DOES bootstrap its own cluster, because nothing else will (#290
        # review): the shared bootstrap_initdb used to run for every mode, and moving it under
        # the agent's control left this branch exec'ing a postmaster at an empty data directory,
        # which exits immediately. The chart never reaches here -- its own `postgres` command is
        # inside an agent-mode guard and therefore unreachable -- so this is purely for direct
        # image users, which is exactly who has no agent to do it for them.
        bootstrap_initdb
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
