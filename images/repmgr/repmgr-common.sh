# Shared repmgr topology/timeline helpers, sourced (not executed) by entrypoint.sh and
# init-repmgr.sh. Having ONE definition in the image means a fix -- e.g. the #168 hex
# decode, or standardizing on the immediate WAL-insert timeline read -- can't be applied
# to only some copies (#177). The chart service-updater (rendered ConfigMap) and the Go
# agent keep their own copies of the same logic; this consolidates the image's two shell
# scripts, which had diverged (init-repmgr used the laggy pg_control_checkpoint read).
#
# Callers provide REPMGR_USER / REPMGR_PASSWORD / REPMGR_DB in the environment and have
# /usr/lib/postgresql/18/bin on PATH (for psql / pg_controldata).

# Decimal value of an 8-hex-digit WAL-filename timeline. The first 8 chars of
# pg_walfile_name(pg_current_wal_lsn()) are the timeline in HEX, so a SQL `::int` cast is
# WRONG: it parses decimal, so '0000000A' (TL 10) errors and '00000010' (TL 16) yields 10
# (#168). Decode the hex here. Empty/non-hex input -> empty output (caller treats it as
# unreadable). Kept in sync with the chart service-updater's tl_to_int and the Go agent.
tl_to_int() {
    case "$1" in ''|*[!0-9A-Fa-f]*) return 0 ;; esac
    echo $((16#$1))
}

# Decimal current timeline of the RUNNING node at host $1, read from the WAL insert
# position (pg_walfile_name(pg_current_wal_lsn())). This reflects a fast promotion
# immediately, whereas pg_control_checkpoint() keeps the pre-promotion timeline until the
# spread end-of-recovery checkpoint completes (minutes under load) -- which would let a
# stale primary look current. Empty output when unreachable/unreadable.
remote_node_timeline_int() {
    local host="$1" ru="${REPMGR_USER:-repmgr}" rd="${REPMGR_DB:-repmgr}" hex
    hex=$(PGPASSWORD="$REPMGR_PASSWORD" psql -tAX -h "$host" -p 5432 -U "$ru" -d "$rd" \
        -c "SELECT substring(pg_walfile_name(pg_current_wal_lsn()) from 1 for 8);" 2>/dev/null) || hex=""
    tl_to_int "$hex"
}

# Decimal timeline of this node's PGDATA ($1), read OFFLINE from the control file (the
# node is not running during the init container / the entrypoint guard). Empty output
# when unreadable.
local_node_timeline_int() {
    pg_controldata -D "$1" 2>/dev/null \
        | awk -F: '/Latest checkpoint.s TimeLineID/{gsub(/[^0-9]/,"",$2);print $2}'
}
