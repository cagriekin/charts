#!/bin/bash
set -e

SCRIPT_NAME=${1:-default}

# Decimal value of an 8-hex-digit WAL-filename timeline. The first 8 chars of
# pg_walfile_name(pg_current_wal_lsn()) are the timeline in HEX, so a SQL
# `::int` cast is WRONG: it parses decimal, so '0000000A' (TL 10) errors and
# '00000010' (TL 16) yields 10. Decode the hex here. Empty/non-hex -> empty
# output (caller treats as unreadable). Kept in sync with the chart's
# service-updater tl_to_int.
tl_to_int() {
    case "$1" in ''|*[!0-9A-Fa-f]*) return 0 ;; esac
    echo $((16#$1))
}

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
    local i peer in_recovery remote_hex remote_tli
    for i in $(seq 0 $((node_count - 1))); do
        [ "$i" = "$ordinal" ] && continue
        peer="${base}-${i}.${HEADLESS_SERVICE}"
        PGPASSWORD="$REPMGR_PASSWORD" pg_isready -t 3 -h "$peer" -p 5432 -U "$ru" -d "$rd" >/dev/null 2>&1 || continue
        REACHED_ANY=1
        in_recovery=$(PGPASSWORD="$REPMGR_PASSWORD" psql -tAX -h "$peer" -p 5432 -U "$ru" -d "$rd" -c "SELECT pg_is_in_recovery();" 2>/dev/null) || in_recovery=""
        [ "$in_recovery" = "f" ] || continue
        FOUND_PRIMARY=1
        remote_hex=$(PGPASSWORD="$REPMGR_PASSWORD" psql -tAX -h "$peer" -p 5432 -U "$ru" -d "$rd" -c "SELECT substring(pg_walfile_name(pg_current_wal_lsn()) from 1 for 8);" 2>/dev/null) || remote_hex=""
        remote_tli=$(tl_to_int "$remote_hex")
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
primary_safety_guard() {
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
    local_tli=$(pg_controldata -D "$PGDATA" 2>/dev/null | awk -F: '/Latest checkpoint.s TimeLineID/{gsub(/[^0-9]/,"",$2);print $2}') || local_tli=""
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
        local conninfo="host=${NEWEST_PEER} port=5432 user=${ru} password=${REPMGR_PASSWORD} dbname=${rd} connect_timeout=10"
        # node rejoin needs a dormant node and rewinds via pg_rewind (PG18
        # initdb enables data checksums, so pg_rewind is available). It starts
        # the node to verify it attaches; stop it afterward so the postmaster
        # can run as the container's main process via the exec below.
        if repmgr -f /etc/repmgr/repmgr.conf node rejoin -d "$conninfo" --force-rewind --config-files=postgresql.conf,pg_hba.conf; then
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

case "$SCRIPT_NAME" in
    "postgres")
        export PATH=$PATH:/usr/lib/postgresql/18/bin
        PGDATA=${PGDATA:-/var/lib/postgresql/data/pgdata}
        export PGDATA

        if [ "$(id -u)" = "0" ]; then
            exec gosu postgres "$0" "$@"
        fi

        primary_safety_guard

        if [ ! -s "$PGDATA/PG_VERSION" ]; then
            echo "Initializing PostgreSQL database..."
            initdb -D "$PGDATA" --auth-local=trust --auth-host=md5

            cat >> "$PGDATA/postgresql.conf" << EOF
wal_level = replica
max_wal_senders = 10
wal_keep_size = 1GB
hot_standby = on
hot_standby_feedback = on
listen_addresses = '*'
shared_preload_libraries = 'repmgr'
EOF

            if [ "${PGBACKREST_ENABLED:-}" = "true" ]; then
                cat >> "$PGDATA/postgresql.conf" << PGBR
archive_mode = on
archive_command = 'pgbackrest --stanza=${PGBACKREST_STANZA:-db} archive-push %p'
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

            pg_ctl -D "$PGDATA" -w start

            REPMGR_USER=${REPMGR_USER:-repmgr}
            REPMGR_PASSWORD=${REPMGR_PASSWORD:?REPMGR_PASSWORD is required}
            REPMGR_DB=${REPMGR_DB:-repmgr}
            POSTGRES_USER=${POSTGRES_USER:-postgres}
            POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
            POSTGRES_DB=${POSTGRES_DB:-postgres}

            psql -U postgres -d postgres -c "CREATE DATABASE ${POSTGRES_DB};" 2>/dev/null || true
            psql -U postgres -d postgres -c "CREATE USER ${POSTGRES_USER} WITH SUPERUSER PASSWORD '${POSTGRES_PASSWORD}';" 2>/dev/null || true
            psql -U postgres -d postgres -c "ALTER USER ${POSTGRES_USER} WITH PASSWORD '${POSTGRES_PASSWORD}';" 2>/dev/null || true

            psql -U postgres -d postgres -c "CREATE DATABASE ${REPMGR_DB};" 2>/dev/null || true
            psql -U postgres -d postgres -c "CREATE USER ${REPMGR_USER} WITH SUPERUSER PASSWORD '${REPMGR_PASSWORD}';" 2>/dev/null || true
            psql -U postgres -d postgres -c "GRANT ALL PRIVILEGES ON DATABASE ${REPMGR_DB} TO ${REPMGR_USER};" 2>/dev/null || true
            psql -U postgres -d ${REPMGR_DB} -c "CREATE EXTENSION IF NOT EXISTS repmgr;" 2>/dev/null || true

            pg_ctl -D "$PGDATA" -w stop

            echo "PostgreSQL initialization complete"
        fi

        echo "Starting PostgreSQL..."
        exec postgres -D "$PGDATA"
        ;;
    "init")
        exec /usr/local/bin/init-repmgr.sh
        ;;
    "repmgrd")
        exec /usr/local/bin/repmgrd-entrypoint.sh
        ;;
    "service-updater")
        exec /usr/local/bin/service-updater.sh
        ;;
    *)
        echo "Usage: $0 {postgres|init|repmgrd|service-updater}"
        exit 1
        ;;
esac
