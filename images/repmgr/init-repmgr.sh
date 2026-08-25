#!/bin/bash
set -e

if [ "$(id -u)" = "0" ]; then
    exec gosu postgres "$0" "$@"
fi

# Shared topology/timeline helpers (tl_to_int, remote_node_timeline_int,
# local_node_timeline_int): one definition for the image's shell scripts (#177).
# Sourced BEFORE the PATH export below, which needs the PG_BINDIR it defines (#269).
source /usr/local/bin/repmgr-common.sh

# Stop here if PG_MAJOR (from the chart) names a major this image does not bundle, so the
# mismatch is reported once with both majors named instead of as a missing initdb (#269).
require_pg_bindir || exit 1

# pg_controldata / pg_ctl / repmgr's helpers live in the versioned bindir,
# which is not on the default PATH; without this the local-timeline read below
# silently fails and every standby restart does a full re-clone.
export PATH=$PATH:$PG_BINDIR

ORDINAL=${HOSTNAME##*-}
NODE_ID=$((ORDINAL + 1000))

if [ "$ORDINAL" = "0" ]; then
    NODE_TYPE="master"
else
    NODE_TYPE="standby"
fi

HEADLESS_SERVICE=${HEADLESS_SERVICE:?HEADLESS_SERVICE is required}
REPMGR_USER=${REPMGR_USER:-repmgr}
REPMGR_PASSWORD=${REPMGR_PASSWORD:?REPMGR_PASSWORD is required}
REPMGR_DB=${REPMGR_DB:-repmgr}
PGDATA=${PGDATA:-/var/lib/postgresql/data/pgdata}
NODE_FQDN="${HOSTNAME}.${HEADLESS_SERVICE}"

echo "Node: ${HOSTNAME}, Ordinal: ${ORDINAL}, Type: ${NODE_TYPE}, ID: ${NODE_ID}"

cat > /etc/repmgr/repmgr.conf << EOF
node_id=${NODE_ID}
node_name=${HOSTNAME}
conninfo='host=${NODE_FQDN} port=5432 user=${REPMGR_USER} password=${REPMGR_PASSWORD} dbname=${REPMGR_DB} connect_timeout=10'
data_directory='${PGDATA}'
pg_bindir='${PG_BINDIR}'
replication_user='${REPMGR_USER}'
replication_type='physical'
use_replication_slots=${USE_REPLICATION_SLOTS:-0}
failover='${REPMGR_FAILOVER:-automatic}'
promote_command='repmgr standby promote -f /etc/repmgr/repmgr.conf'
follow_command='repmgr standby follow -f /etc/repmgr/repmgr.conf --upstream-node-id=%n'
reconnect_attempts=3
reconnect_interval=10
monitoring_history=true
monitoring_history_keep=7
log_level=INFO
log_status_interval=10
service_start_command='${PG_BINDIR}/pg_ctl -D ${PGDATA} start'
service_stop_command='${PG_BINDIR}/pg_ctl -D ${PGDATA} stop'
service_restart_command='${PG_BINDIR}/pg_ctl -D ${PGDATA} restart'
service_reload_command='${PG_BINDIR}/pg_ctl -D ${PGDATA} reload'
EOF

echo "Generated repmgr.conf for ${NODE_TYPE} node"

find_current_primary() {
    BASE_NAME="${HOSTNAME%-*}"
    NODE_COUNT="${REPMGR_NODE_COUNT:-10}"
    case "$NODE_COUNT" in ''|*[!0-9]*) NODE_COUNT=10 ;; esac
    for i in $(seq 0 $((NODE_COUNT - 1))); do
        [ "$i" = "$ORDINAL" ] && continue
        PARTNER="${BASE_NAME}-${i}.${HEADLESS_SERVICE}"
        if PGPASSWORD="${REPMGR_PASSWORD}" pg_isready -h "${PARTNER}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" > /dev/null 2>&1; then
            IN_RECOVERY=$(PGPASSWORD="${REPMGR_PASSWORD}" psql -h "${PARTNER}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" -t -c "SELECT pg_is_in_recovery();" 2>/dev/null | xargs)
            if [ "$IN_RECOVERY" = "f" ]; then
                echo "$PARTNER"
                return 0
            fi
        fi
    done
    return 1
}

# True when ANY peer answers as a live PostgreSQL node, primary or standby. On an EMPTY
# data directory that is the difference between "nothing exists yet" and "a cluster exists
# and this pod lost its disk": a reachable STANDBY is proof of an established cluster even
# in the window where the primary itself is unreachable, and initdb'ing next to it forks a
# divergent cluster. Only total silence from every peer is a genuine first install.
any_peer_reachable() {
    BASE_NAME="${HOSTNAME%-*}"
    NODE_COUNT="${REPMGR_NODE_COUNT:-10}"
    case "$NODE_COUNT" in ''|*[!0-9]*) NODE_COUNT=10 ;; esac
    for i in $(seq 0 $((NODE_COUNT - 1))); do
        [ "$i" = "$ORDINAL" ] && continue
        PARTNER="${BASE_NAME}-${i}.${HEADLESS_SERVICE}"
        if PGPASSWORD="${REPMGR_PASSWORD}" pg_isready -h "${PARTNER}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" > /dev/null 2>&1; then
            REACHABLE_PEER="$PARTNER"
            return 0
        fi
    done
    return 1
}

# A primary-state data directory (data present, no standby.signal) must NOT be
# re-cloned here. The old ordinal-based path would, on a full-cluster restart
# after a failover, clone a real primary's data (pod-1, newer timeline) onto a
# stale lower-timeline "primary" (pod-0 that came up first under OrderedReady)
# and destroy every post-failover commit (#125). The entrypoint guard scans
# peers and either resumes this node as primary or rewinds it FORWARD to a
# newer-timeline peer -- never backward -- so defer the decision to it.
if [ -s "${PGDATA}/PG_VERSION" ] && [ ! -f "${PGDATA}/standby.signal" ]; then
    echo "Primary-state data directory present; deferring start/rewind decision to the entrypoint guard"
    exit 0
fi

# An EMPTY data directory on ordinal 0 is not proof of a first install, and deciding it
# from the ordinal alone was a dead end. In agent mode the primary is lease-elected and
# podManagementPolicy is Parallel, so ordinal 0 is just another node: lose or recreate its
# PVC while the lease sits on another pod and the old branch below declared "First boot,
# postgres mode will initialize the database" and skipped the clone -- then entrypoint.sh's
# primary_safety_guard correctly refused to initdb next to an active primary, leaving the
# pod in CrashLoopBackOff with no automatic way out. Ordinal > 0 recovered from the same
# PVC loss cleanly, via the standby path below, purely because of its name.
#
# So decide from the cluster, not the ordinal, and reuse that same standby path: it already
# waits for a primary, waits for its repmgr registration and clones with retries. The
# entrypoint guard stays the second line of defence -- it is the one that reads the durable
# primary marker (this container has neither the marker env nor a reason to duplicate it),
# so a correlated restart in which no peer has answered yet still ends in its marker-aware
# settle-scan rather than in a divergent initdb here.
if [ "$NODE_TYPE" = "master" ] && [ ! -s "${PGDATA}/PG_VERSION" ]; then
    CURRENT_PRIMARY=$(find_current_primary) || true
    if [ -n "$CURRENT_PRIMARY" ]; then
        echo "Empty data directory while ${CURRENT_PRIMARY} is an active primary: PVC-loss recreate, not a first install; cloning as a standby"
        NODE_TYPE="standby"
    elif any_peer_reachable; then
        echo "Empty data directory and peer ${REACHABLE_PEER} is live but no primary is visible yet; waiting for one instead of initializing a divergent cluster"
        NODE_TYPE="standby"
    else
        echo "First boot, postgres mode will initialize the database"
        exit 0
    fi
fi

if [ "$NODE_TYPE" = "master" ]; then
    # Data exists (the empty case above either flipped this node to the standby path or
    # exited). Check if another node was promoted while this one was down.
    echo "Data directory exists, checking for post-failover scenario..."
    CURRENT_PRIMARY=$(find_current_primary) || true
    if [ -n "$CURRENT_PRIMARY" ]; then
        echo "Another primary found at ${CURRENT_PRIMARY}, re-cloning as standby..."
        rm -rf "${PGDATA:?}"/*
        for attempt in $(seq 1 5); do
            if PGPASSWORD="${REPMGR_PASSWORD}" repmgr -h "${CURRENT_PRIMARY}" -U "${REPMGR_USER}" -d "${REPMGR_DB}" -f /etc/repmgr/repmgr.conf standby clone --force; then
                echo "Re-cloned as standby from ${CURRENT_PRIMARY}"
                exit 0
            fi
            echo "Clone attempt ${attempt} failed, retrying in 5s..."
            sleep 5
        done
        echo "ERROR: All clone attempts failed"
        exit 1
    fi

    echo "No other primary found, continuing as primary"
    exit 0
fi

# Standby path: wait for primary, then clone if needed
wait_for_primary() {
    BASE_NAME="${HOSTNAME%-*}"
    echo "Searching for primary node..."
    for i in $(seq 1 120); do
        PRIMARY_FQDN=$(find_current_primary) || true
        if [ -n "$PRIMARY_FQDN" ]; then
            echo "Primary found at ${PRIMARY_FQDN}"
            return 0
        fi
        # Fall back to ordinal 0 on first boot -- but never when THIS pod is ordinal 0.
        # Reachable now via the empty-data path above; probing our own not-yet-started
        # postmaster would only burn the loop.
        FQDN_0="${BASE_NAME}-0.${HEADLESS_SERVICE}"
        if [ "$ORDINAL" != "0" ] && PGPASSWORD="${REPMGR_PASSWORD}" pg_isready -h "${FQDN_0}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" > /dev/null 2>&1; then
            if PGPASSWORD="${REPMGR_PASSWORD}" psql -h "${FQDN_0}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" -c "SELECT 1;" > /dev/null 2>&1; then
                PRIMARY_FQDN="$FQDN_0"
                echo "Primary found at ${PRIMARY_FQDN}"
                return 0
            fi
        fi
        if [ "$i" = "120" ]; then
            echo "ERROR: Timed out waiting for primary"
            return 1
        fi
        sleep 2
    done
}

wait_for_primary
PRIMARY_FQDN=${PRIMARY_FQDN:?PRIMARY_FQDN must be set by wait_for_primary}

echo "Waiting for primary to be registered in repmgr..."
for i in $(seq 1 120); do
    REGISTERED=$(PGPASSWORD="${REPMGR_PASSWORD}" psql -h "${PRIMARY_FQDN}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" -t -c "SELECT count(*) FROM repmgr.nodes WHERE type = 'primary' AND active = true;" 2>/dev/null | xargs)
    if [ "$REGISTERED" = "1" ]; then
        echo "Primary is registered"
        break
    fi
    if [ "$i" = "120" ]; then
        echo "ERROR: Timed out waiting for primary registration"
        exit 1
    fi
    sleep 2
done

if [ -s "${PGDATA}/PG_VERSION" ]; then
    # Both timelines are read from the CONTROL FILE, symmetrically: LOCAL offline via the
    # shared helper (#177), PRIMARY via pg_control_checkpoint() on the running primary.
    # They MUST be read the same way. A standby that has followed onto a new timeline by
    # streaming but not yet run its first restartpoint still shows the OLD control-file
    # timeline; a fast promotion likewise defers the primary's checkpoint, so the primary's
    # control-file timeline also lags -- both report the pre-checkpoint timeline and match
    # (skip). Using the immediate WAL-insert read for the primary while LOCAL stays the
    # control-file read would be asymmetric and wipe such a caught-up standby with a
    # needless full re-clone. A genuinely-behind standby still catches up via streaming; a
    # diverged primary-state node is handled by the entrypoint guard, not here.
    LOCAL_TIMELINE=$(local_node_timeline_int "${PGDATA}")
    PRIMARY_TIMELINE=$(PGPASSWORD="${REPMGR_PASSWORD}" psql -h "${PRIMARY_FQDN}" -p 5432 -U "${REPMGR_USER}" -d "${REPMGR_DB}" -t -c "SELECT timeline_id FROM pg_control_checkpoint();" 2>/dev/null | xargs)

    if [ -n "$LOCAL_TIMELINE" ] && [ "$LOCAL_TIMELINE" = "$PRIMARY_TIMELINE" ]; then
        echo "Data directory exists and timeline matches ($LOCAL_TIMELINE), skipping clone"
        exit 0
    fi

    echo "Timeline mismatch (local: ${LOCAL_TIMELINE:-?}, primary: ${PRIMARY_TIMELINE:-?}), re-cloning..."
    rm -rf "${PGDATA:?}"/*
fi

echo "Cloning from primary at ${PRIMARY_FQDN}..."
for attempt in $(seq 1 5); do
    if PGPASSWORD="${REPMGR_PASSWORD}" repmgr -h "${PRIMARY_FQDN}" -U "${REPMGR_USER}" -d "${REPMGR_DB}" -f /etc/repmgr/repmgr.conf standby clone --force; then
        echo "Clone successful"
        exit 0
    fi
    echo "Clone attempt ${attempt} failed, retrying in 5s..."
    sleep 5
done

echo "ERROR: All clone attempts failed"
exit 1
