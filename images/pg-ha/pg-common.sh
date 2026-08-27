# Mechanism-neutral shell helpers for this image's entrypoints.
#
# This replaces repmgr-common.sh (#290). That file bundled the PG_BINDIR derivation --
# needed by everything -- with repmgr topology/timeline helpers (tl_to_int,
# remote_node_timeline_int, local_node_timeline_int) whose only callers were the repmgr
# code paths deleted with the mechanism. What survives is the part that was never
# repmgr-specific.
#
# Sourced, not executed. PG_MAJOR comes from the image ENV (Dockerfile ARG PG_MAJOR);
# the fallback keeps an older env-less image working.

PG_MAJOR="${PG_MAJOR:-18}"

# Debian installs the server binaries per major under /usr/lib/postgresql/<major>/bin, and
# pg_ctl / pg_controldata / initdb / pg_basebackup are NOT on the default PATH (unlike the
# pg_wrapper-provided psql), so every caller needs this on PATH (#269).
PG_BINDIR="/usr/lib/postgresql/${PG_MAJOR}/bin"

# Refuse to run when PG_MAJOR names a major this image does not bundle. The chart passes
# PG_MAJOR from the image's majorVersion value, so this is where a values file asking for 17
# against a PG18 image (or vice versa) stops -- with both majors named. Without it the PATH
# export in each entrypoint would silently point nowhere and the failure would surface as
# `initdb: command not found` or, worse, a clone loop. Called by the entrypoints rather than
# at source time so the shell unit tests can source this file anywhere.
require_pg_bindir() {
    [ -x "${PG_BINDIR}/postgres" ] && return 0
    local installed
    installed=$(ls -1d /usr/lib/postgresql/*/ 2>/dev/null | sed 's#.*/postgresql/##; s#/$##' | paste -sd, -)
    echo "FATAL: PG_MAJOR=${PG_MAJOR} but ${PG_BINDIR}/postgres is missing; this image bundles PostgreSQL ${installed:-none}." >&2
    echo "       The image tag decides the major: set the chart's image majorVersion (and postgresql.majorVersion) to ${installed:-the bundled major}, or point the image tag at one built for ${PG_MAJOR} (e.g. a -pg${PG_MAJOR} tag)." >&2
    return 1
}
