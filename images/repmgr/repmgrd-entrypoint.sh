#!/bin/bash
set -e

if [ "$(id -u)" = "0" ]; then
    exec gosu postgres "$0" "$@"
fi

if [ ! -f /etc/repmgr/repmgr.conf ]; then
    echo "ERROR: repmgr.conf not found at /etc/repmgr/repmgr.conf"
    exit 1
fi

PGDATA=${PGDATA:-/var/lib/postgresql/data/pgdata}

echo "Waiting for local PostgreSQL..."
for i in $(seq 1 120); do
    if pg_isready -h 127.0.0.1 -p 5432 > /dev/null 2>&1; then
        echo "PostgreSQL is ready"
        break
    fi
    if [ "$i" = "120" ]; then
        echo "ERROR: Timed out waiting for local PostgreSQL"
        exit 1
    fi
    sleep 2
done

# pg_isready (waited on above) only means postgres accepts connections, not
# that the postgresql container's init SQL (CREATE EXTENSION repmgr, repmgr
# user) has finished. Retry the role probe until it returns a definitive
# answer so a transient empty result does not send a primary down the standby
# branch.
IN_RECOVERY=""
for attempt in $(seq 1 30); do
    IN_RECOVERY=$(psql -h 127.0.0.1 -U postgres -d postgres -tAc "SELECT pg_is_in_recovery();" 2>/dev/null)
    case "$IN_RECOVERY" in t|f) break ;; esac
    sleep 2
done

if [ "$IN_RECOVERY" = "f" ]; then
    # Retry primary register (mirrors the standby path): under a slow start the
    # repmgr extension/user may not exist the instant postgres accepts
    # connections, and a single non-retried call under `set -e` would kill
    # repmgrd into a CrashLoopBackOff that outlives the install's wait.
    echo "Registering primary node..."
    for attempt in $(seq 1 30); do
        if repmgr -f /etc/repmgr/repmgr.conf primary register --force 2>/dev/null; then
            echo "Primary registered"
            break
        fi
        if [ "$attempt" = "30" ]; then
            echo "ERROR: Failed to register primary after 30 attempts"
            exit 1
        fi
        echo "Primary register attempt ${attempt} failed (postgres/repmgr extension may not be ready); retrying in 5s..."
        sleep 5
    done
else
    echo "Registering standby node..."
    for attempt in $(seq 1 30); do
        if repmgr -f /etc/repmgr/repmgr.conf standby register --force 2>/dev/null; then
            echo "Standby registered"
            break
        fi
        if [ "$attempt" = "30" ]; then
            echo "ERROR: Failed to register standby after 30 attempts"
            exit 1
        fi
        sleep 5
    done

    NODE_ID=$(grep 'node_id' /etc/repmgr/repmgr.conf | head -1 | sed 's/[^0-9]//g')
    PRIMARY_CONNINFO=$(grep 'conninfo' /etc/repmgr/repmgr.conf | head -1 | sed "s/.*'\\(.*\\)'.*/\\1/")
    PRIMARY_HOST=$(psql "${PRIMARY_CONNINFO}" -t -c "SELECT conninfo FROM repmgr.nodes WHERE type = 'primary' AND active = true LIMIT 1;" 2>/dev/null | grep -oP "host=\K[^ ']+") || true

    if [ -n "$PRIMARY_HOST" ]; then
        echo "Verifying standby registration on primary (node_id=${NODE_ID})..."
        for i in $(seq 1 30); do
            REG_TYPE=$(psql -h "$PRIMARY_HOST" -U repmgr -d repmgr -t -c "SELECT type FROM repmgr.nodes WHERE node_id = ${NODE_ID};" 2>/dev/null | xargs)
            if [ "$REG_TYPE" = "standby" ]; then
                echo "Primary confirms registration: type=standby"
                break
            fi
            if [ "$i" = "30" ]; then
                echo "ERROR: Primary does not show node ${NODE_ID} as standby (current type: ${REG_TYPE})"
                exit 1
            fi
            sleep 1
        done
    else
        echo "WARNING: Could not determine primary host, skipping registration check"
    fi
fi

echo "Starting repmgrd daemon..."

cleanup() {
    kill -TERM $REPMGRD_PID 2>/dev/null || true
    wait $REPMGRD_PID 2>/dev/null || true
    exit 0
}

trap cleanup SIGTERM SIGINT

repmgrd -f /etc/repmgr/repmgr.conf --daemonize=no &
REPMGRD_PID=$!

echo "repmgrd started with PID: $REPMGRD_PID"

wait $REPMGRD_PID
