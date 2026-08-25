#!/bin/bash
set -e

SCRIPT_NAME=${1:-default}

# Shared topology/timeline helpers (tl_to_int, remote_node_timeline_int,
# local_node_timeline_int): one definition for the image's shell scripts so a fix
# can't land in only some copies (#177).
source /usr/local/bin/repmgr-common.sh

# Scan sibling StatefulSet pods for an active primary and the newest timeline
# seen. Sets REACHED_ANY/FOUND_PRIMARY/NEWEST_TLI/NEWEST_PEER. Timeline comes
# from the WAL insert position (pg_walfile_name(pg_current_wal_lsn())), which
# reflects a fast promotion immediately -- pg_control_checkpoint() keeps the
# pre-promotion timeline until the spread end-of-recovery checkpoint completes
# (minutes under load), which would let a stale primary slip through.
scan_peers() {
    REACHED_ANY=0; FOUND_PRIMARY=0; NEWEST_TLI=0; NEWEST_PEER=""
    local ru="${REPMGR_USER:-repmgr}" rd="${REPMGR_DB:-repmgr}"
    local ordinal="${HOSTNAME##*-}" base="${HOSTNAME%-*}"
    local node_count="${REPMGR_NODE_COUNT:-10}"
    case "$node_count" in ''|*[!0-9]*) node_count=10 ;; esac
    local i peer in_recovery remote_tli
    for i in $(seq 0 $((node_count - 1))); do
        [ "$i" = "$ordinal" ] && continue
        peer="${base}-${i}.${HEADLESS_SERVICE}"
        PGPASSWORD="$REPMGR_PASSWORD" pg_isready -t 3 -h "$peer" -p 5432 -U "$ru" -d "$rd" >/dev/null 2>&1 || continue
        REACHED_ANY=1
        in_recovery=$(PGPASSWORD="$REPMGR_PASSWORD" psql -tAX -h "$peer" -p 5432 -U "$ru" -d "$rd" -c "SELECT pg_is_in_recovery();" 2>/dev/null) || in_recovery=""
        [ "$in_recovery" = "f" ] || continue
        FOUND_PRIMARY=1
        remote_tli=$(remote_node_timeline_int "$peer")
        [ -n "$remote_tli" ] || continue
        if [ "$remote_tli" -gt "$NEWEST_TLI" ]; then NEWEST_TLI="$remote_tli"; NEWEST_PEER="$peer"; fi
    done
    return 0
}

# True when a primary was already recorded for this cluster, i.e. the durable
# #125 primary-marker ConfigMap exists. Distinguishes a genuine first install
# (no marker -> safe to initialize after one fast scan) from a PVC-loss recreate
# of an empty pod while a cluster already exists (marker present -> settle before
# concluding, so a briefly-unreachable primary is not missed and we don't initdb
# a divergent cluster, #170). Needs kubectl + the marker name/namespace; if
# either is absent (non-repmgr use, or kubectl/API unavailable) it returns
# false, preserving the prior single-scan behavior so a first install never
# pays the settle latency.
cluster_was_established() {
    [ -n "${PRIMARY_MARKER:-}" ] && [ -n "${NAMESPACE:-}" ] || return 1
    # Bounded: this runs before initdb on every fresh boot, so a throttled or
    # unreachable API (the same client-go rate limiting that stalls installs on
    # starved nodes) must not hang the guard. --request-timeout caps the API
    # call; the outer `timeout` caps DNS/dial hangs. On any timeout/error we
    # return non-zero -> fast single-scan path, never a stall before initdb.
    timeout 5 kubectl get configmap "$PRIMARY_MARKER" -n "$NAMESPACE" --request-timeout=3s >/dev/null 2>&1
}

# Bounded settle for the empty-data path: re-scan peers until an active primary
# is found (caller then refuses to initdb) or the attempts are exhausted. Unlike
# the existing-data path it must NOT stop early just because some peer is
# reachable -- on an EMPTY data dir a reachable standby is not proof the primary
# is gone, and the primary may be transiently unreachable in any single scan
# window; stopping there would initdb a divergent cluster (#170). Sets the same
# FOUND_PRIMARY/NEWEST_PEER globals scan_peers does.
settle_scan_for_primary() {
    local attempts="${REPMGR_STALE_CHECK_ATTEMPTS:-5}" attempt
    case "$attempts" in ''|*[!0-9]*) attempts=5 ;; esac
    for attempt in $(seq 1 "$attempts"); do
        scan_peers
        [ "$FOUND_PRIMARY" = "1" ] && break
        [ "$attempt" -lt "$attempts" ] && { echo "stale-primary guard (empty data, primary marker present): no active primary found yet (attempt ${attempt}/${attempts}); settling 3s" >&2; sleep 3; }
    done
}

# Re-clone PGDATA from $1 (primary host) WITHOUT destroying the current data
# until the clone succeeds. rm -rf'ing PGDATA before the clone leaves an empty
# data dir with no recoverable copy if every clone attempt fails (#175). Instead
# move the contents to a sibling backup on the same volume (a fast rename),
# clone into the emptied PGDATA, drop the backup only after a successful clone,
# and keep it for manual recovery on failure. Returns 0 on success, 1 on
# failure. Costs up to ~2x PGDATA disk during the re-clone.
reclone_preserving_old() {
    local primary="$1"
    local ru="${REPMGR_USER:-repmgr}" rd="${REPMGR_DB:-repmgr}"
    local backup="${PGDATA%/}.diverged.$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$backup"
    ( shopt -s dotglob nullglob; mv "${PGDATA}"/* "$backup"/ 2>/dev/null ) || true
    local a
    for a in $(seq 1 5); do
        if PGPASSWORD="$REPMGR_PASSWORD" repmgr -h "$primary" -U "$ru" -d "$rd" -f /etc/repmgr/repmgr.conf standby clone --force; then
            rm -rf "$backup"
            return 0
        fi
        echo "stale-primary guard: clone attempt ${a} from ${primary} failed; retrying in 5s" >&2
        sleep 5
    done
    echo "stale-primary guard: re-clone from ${primary} failed; diverged data preserved at ${backup} for manual recovery" >&2
    return 1
}

# Prevent a former primary from resuming read-write on a stale timeline after a
# standby was promoted while this node's CONTAINER (not pod) was down -- the
# init container, which holds the re-clone logic, does not re-run on a
# container-only restart (CrashLoopBackOff, OOM, liveness kill). Repmgr-managed
# nodes only; no-op for standalone use of the image.
#
# Repmgr-mechanism only (#288). Its rejoin and re-clone paths shell out to `repmgr node
# rejoin` and `repmgr standby clone`, which do not exist as concepts under native mode --
# there the agent owns both (RejoinForceRewind via pg_rewind, Clone via pg_basebackup) and
# does them from its own reconcile loop with the Lease as the authority for who is primary.
# Running this first would rewind or wipe a data directory on the strength of a peer scan
# that has no notion of the lease, which is exactly the divergence the agent exists to
# prevent.
#
# Now that native mode can actually run a multi-node cluster, "harmless because native only
# ever runs a lone primary" no longer holds, so the gate is explicit rather than relying on
# repmgr.conf being absent (init-repmgr.sh does skip writing it under native, and the
# file check below still stands as a second line of defence).
primary_safety_guard() {
    if [ "${MECHANISM:-repmgr}" = "native" ]; then
        echo "stale-primary guard: skipped (MECHANISM=native; the agent owns rejoin and re-clone)"
        return 0
    fi
    [ -f /etc/repmgr/repmgr.conf ] || return 0
    [ -n "${HEADLESS_SERVICE:-}" ] || return 0
    [ -n "${REPMGR_PASSWORD:-}" ] || return 0
    [ -f "$PGDATA/standby.signal" ] && return 0   # already a standby; init/repmgrd own recovery

    local ru="${REPMGR_USER:-repmgr}" rd="${REPMGR_DB:-repmgr}"

    if [ ! -s "$PGDATA/PG_VERSION" ]; then
        # Empty data dir. Initializing here while a peer is already primary forks
        # a divergent cluster (#125), so refuse if any peer is primary. A genuine
        # first install has no marker yet -> a single fast scan keeps install
        # latency low (the common path is unchanged). Only when the durable
        # primary marker shows a cluster was already established -- i.e. this
        # empty pod is a PVC-loss recreate -- do we settle/retry the scan, so a
        # briefly-unreachable primary is not missed in one scan window (#170).
        # The settle therefore never delays a first install. (If an operator has
        # deleted the marker to deliberately accept data loss, this falls to the
        # fast path -- the documented escape hatch.)
        if cluster_was_established; then
            settle_scan_for_primary
        else
            scan_peers
        fi
        if [ "$FOUND_PRIMARY" = "1" ]; then
            echo "FATAL: data directory is empty but ${NEWEST_PEER:-a peer} is an active primary; refusing to initialize a divergent database. Recreate this pod with persistent storage, or clone it manually." >&2
            exit 1
        fi
        return 0
    fi

    # Existing data that would start read-write. Settle only while NO peer is
    # reachable (correlated restart): if peers answer and none is a newer
    # primary, this node is healthy and starts immediately (no latency added).
    local attempts="${REPMGR_STALE_CHECK_ATTEMPTS:-5}" attempt
    case "$attempts" in ''|*[!0-9]*) attempts=5 ;; esac
    for attempt in $(seq 1 "$attempts"); do
        scan_peers
        [ "$NEWEST_TLI" -gt 0 ] && break
        [ "$REACHED_ANY" = "1" ] && break
        [ "$attempt" -lt "$attempts" ] && { echo "stale-primary guard: no peer reachable yet (attempt ${attempt}/${attempts}); settling 3s" >&2; sleep 3; }
    done

    local local_tli
    local_tli=$(local_node_timeline_int "$PGDATA")
    case "$local_tli" in
        ''|*[!0-9]*)
            if [ "$NEWEST_TLI" -gt 0 ]; then
                echo "FATAL: cannot read local timeline while ${NEWEST_PEER} is an active primary on timeline ${NEWEST_TLI}; refusing to start read-write" >&2
                exit 1
            fi
            return 0 ;;
    esac

    if [ "$NEWEST_TLI" -gt "$local_tli" ]; then
        echo "stale-primary guard: ${NEWEST_PEER} is primary on timeline ${NEWEST_TLI}, local timeline is ${local_tli}; rejoining as standby" >&2
        local conninfo="host=${NEWEST_PEER} port=5432 user=${ru} dbname=${rd} connect_timeout=10"
        # node rejoin needs a dormant node and rewinds via pg_rewind (PG18
        # initdb enables data checksums, so pg_rewind is available). It starts
        # the node to verify it attaches; stop it afterward so the postmaster
        # can run as the container's main process via the exec below.
        #
        # repmgr opens a SEPARATE replication connection to the rejoin target for
        # the rewind; a password inlined in the -d conninfo does not carry into it,
        # so the rewind fails with "unable to establish a replication connection to
        # the rejoin target node" and the guard always fell back to a full re-clone
        # (#178). Supply the credential via PGPASSWORD (libpq uses it for every
        # connection, including the replication one and pg_rewind's source-server),
        # exactly as the clone path above does -- and keep the secret out of argv.
        if PGPASSWORD="$REPMGR_PASSWORD" repmgr -f /etc/repmgr/repmgr.conf node rejoin -d "$conninfo" --force-rewind --config-files=postgresql.conf,pg_hba.conf; then
            pg_ctl -D "$PGDATA" -m fast -w stop >/dev/null 2>&1 || true
            echo "stale-primary guard: rejoin complete; starting as standby" >&2
        else
            echo "stale-primary guard: pg_rewind rejoin failed; falling back to full re-clone from ${NEWEST_PEER}" >&2
            pg_ctl -D "$PGDATA" -m immediate -w stop >/dev/null 2>&1 || true
            reclone_preserving_old "$NEWEST_PEER" || { echo "FATAL: re-clone failed after rejoin failure" >&2; exit 1; }
        fi
    fi
    return 0
}

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
    # repmgr-mode boot initdb straight over an existing data directory.
    if [ -s "$PGDATA/PG_VERSION" ]; then
        return 0
    fi
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

    # #288/#293: repmgr's shared library is preloaded only under the repmgr mechanism. A native
    # cluster has no repmgr extension (the CREATE EXTENSION below is skipped), so preloading
    # repmgr.so is pure liability -- and this line is baked into the primary's postgresql.conf
    # and then cloned verbatim to every standby, so it would make every native cluster created
    # by this code UNSTARTABLE ("could not access file \"repmgr\"") the moment #290/#294 drop
    # the repmgr package from the image. Removing it from an EXISTING data directory, and the
    # render-time guard, remain #293's half.
    if [ "${MECHANISM:-repmgr}" != "native" ]; then
        echo "shared_preload_libraries = 'repmgr'" >> "$PGDATA/postgresql.conf"
    else
        echo "MECHANISM=native: not preloading repmgr.so (no repmgr extension on this cluster, #288)"
    fi

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
    psql -U postgres -d postgres -c "CREATE DATABASE ${POSTGRES_DB};" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; CREATE USER ${POSTGRES_USER} WITH SUPERUSER PASSWORD '${POSTGRES_PASSWORD}';" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; ALTER USER ${POSTGRES_USER} WITH PASSWORD '${POSTGRES_PASSWORD}';" 2>/dev/null || true

    psql -U postgres -d postgres -c "CREATE DATABASE ${REPMGR_DB};" 2>/dev/null || true
    psql -U postgres -d postgres -c "SET password_encryption='scram-sha-256'; CREATE USER ${REPMGR_USER} WITH SUPERUSER PASSWORD '${REPMGR_PASSWORD}';" 2>/dev/null || true
    psql -U postgres -d postgres -c "GRANT ALL PRIVILEGES ON DATABASE ${REPMGR_DB} TO ${REPMGR_USER};" 2>/dev/null || true
    # The repmgr DATABASE and ROLE above are created under BOTH mechanisms, on purpose.
    # #288 asked for native bootstrap to create none of the three, but the role and the
    # database are load-bearing in native mode too: the agent connects as REPMGR_USER for
    # every probe and for pg_basebackup, and primary_conninfo carries dbname=REPMGR_DB.
    # Dropping them would break native outright. Renaming them out of the repmgr
    # namespace is #291 (ha.* values), not this issue.
    #
    # The EXTENSION is the part native genuinely never uses -- it creates the repmgr
    # schema and the nodes table this issue exists to stop reading. Skipping it means a
    # native-mode cluster has no repmgr.nodes at all, so there is no stale cache for
    # anything to fall back to by accident.
    if [ "${MECHANISM:-repmgr}" != "native" ]; then
        psql -U postgres -d ${REPMGR_DB} -c "CREATE EXTENSION IF NOT EXISTS repmgr;" 2>/dev/null || true
    else
        echo "MECHANISM=native: skipping CREATE EXTENSION repmgr (the nodes table is not a topology source any more, #288)"
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

        primary_safety_guard

        # repmgr mode initdbs inline exactly as before (the function no-ops when PGDATA
        # already holds a cluster). Native mode must NOT: the init container no longer clones,
        # so every pod would arrive here empty and create its own cluster with its own
        # system_identifier, which assertSameCluster refuses to rejoin -- Running, never Ready,
        # holding a bogus database. Under native the agent decides: the lease holder runs
        # `entrypoint.sh initdb`, everyone else waits and then clones (#288).
        if [ "${MECHANISM:-repmgr}" != "native" ]; then
            bootstrap_initdb
        elif [ ! -s "$PGDATA/PG_VERSION" ]; then
            echo "MECHANISM=native: empty data directory; deferring to the agent (initdb if lease holder, else clone) (#288)"
        fi

        # agent mode: hand off to the Go HA agent as PID 1; it generates
        # repmgr.conf (failover=manual), starts/supervises PostgreSQL, and runs the
        # lease-based failover loop. postgres mode runs the postmaster directly
        # (repmgrd-mode path).
        if [ "$SCRIPT_NAME" = "agent" ]; then
            echo "Starting pg-ha-agent (PID 1; the agent manages PostgreSQL)..."
            exec /usr/local/bin/pg-ha-agent
        fi

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
        exec /usr/local/bin/init-repmgr.sh
        ;;
    *)
        # repmgrd / service-updater were removed with the repmgrd failover path (#286).
        echo "Usage: $0 {postgres|agent|init|initdb}"
        exit 1
        ;;
esac
