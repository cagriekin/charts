# --- Stage 1: build the Go HA agent (cross-compiled, static, no cgo) ---
# Digest-pinned (manifest-list digest, multi-arch). go1.25.12 carries the patched
# stdlib (crypto/tls GO-2026-5856 ECH leak, plus net/textproto, mime, crypto/x509,
# html/template, net) the agent links.
# Refresh with: docker buildx imagetools inspect golang:1.25-bookworm
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS agent-build
ARG TARGETARCH
# The digest pin above is only binding while THIS toolchain is the one that builds. With
# GOPROXY=off gone (#333), Go's default GOTOOLCHAIN=auto would silently download and run a
# newer toolchain over the network the moment go.mod's `go`/`toolchain` directive is bumped
# past 1.25.0, or a dependency requires one -- linking a stdlib nobody pinned or reviewed,
# and quietly retiring the patched-stdlib claim in the header comment above. `local` makes
# that a hard build failure instead, which is what the vendored build used to get for free.
ENV GOTOOLCHAIN=local
WORKDIR /src/agent
COPY agent/go.mod agent/go.sum ./
# Deps come from the module proxy, authenticated against go.sum -- which is what pins them;
# the vendor/ tree this replaced only changed where the bytes were stored (#333). Its own
# layer, before the source copy, so an agent code change reuses the cached download.
# -mod=readonly is the go1.16+ default, set explicitly because it is load-bearing here: a
# build that would need a go.mod edit fails instead of quietly resolving something new.
RUN GOFLAGS=-mod=readonly go mod download
COPY agent/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOFLAGS=-mod=readonly \
    go build -trimpath -ldflags="-s -w" -o /out/pg-ha-agent ./cmd/agent

# --- Stage 2: the runtime image (PostgreSQL + pgaudit + pgbackrest + the Go agent) ---
# Digest-pinned (manifest-list digest, multi-arch). Refresh with:
#   docker buildx imagetools inspect debian:trixie-slim
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e

# PostgreSQL major to build against (#269). The chart runs the server binaries from THIS
# image, so this arg -- not postgresql.image -- decides the cluster's major.
#
# The default is a CONVENIENCE now, not a contract (#290). It had to stay 18 because the
# unsuffixed published tag was built without a --build-arg and every chart pin resolved to it;
# under the new scheme there is no unsuffixed tag and every publish leg passes --build-arg
# explicitly, so nothing published depends on this value. It still sets what a bare
# `docker build` produces, and what an env-less runtime assumes, so keep it in step with the
# major the docs call default. See images/pg-ha/README.md.
ARG PG_MAJOR=18

# Install system dependencies. `apt-get upgrade` first so the digest-pinned base
# picks up Debian security updates released after the base snapshot (e.g. the
# libssh2 CVE-2026-7598 / CVE-2026-55200 fix in 1.11.1-1+deb13u1) instead of
# shipping the stale pre-installed versions.
RUN apt-get update && apt-get upgrade -y && apt-get install -y \
    curl \
    gnupg \
    lsb-release \
    ca-certificates \
    jq \
    gosu \
    && rm -rf /var/lib/apt/lists/*

# Add PostgreSQL APT repository. --proto '=https' --proto-redir '=https' pins the key
# fetch to HTTPS end to end (#298 security review): this keyring is the root of trust for
# every package installed below, so an HTTPS->HTTP redirect must never be able to fetch it
# over cleartext. The apt source is HTTPS too -- integrity is enforced by signed-by either
# way, but HTTP would let an on-path observer see package selection and serve stale indexes.
RUN curl -fsSL --proto '=https' --proto-redir '=https' https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/postgresql-keyring.gpg

RUN echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/postgresql.list

# Install PostgreSQL for PG_MAJOR. postgresql-<major>-pgaudit ships the
# pgaudit extension used by the chart's opt-in compliance audit logging (#219); it is
# inert unless the chart adds it to shared_preload_libraries, so it costs nothing when
# audit is off.
#
# Every per-major package is checked for an installation CANDIDATE before installing,
# so an unavailable one fails the build here with the package named. pgaudit is the
# reason this matters: `apt-get install` of a set is atomic, but a future PGDG gap for
# a selected major would otherwise be discovered as audit.enabled=true producing
# SILENTLY ABSENT audit logs at runtime instead of a loud build failure. No gap is
# currently known for 17 or 18 -- this guards the failure MODE, not its likelihood.
RUN apt-get update && \
    for pkg in postgresql-${PG_MAJOR} postgresql-${PG_MAJOR}-pgaudit; do \
        apt-cache policy "$pkg" | grep -qE '^[[:space:]]*Candidate: [0-9]' || { \
            echo "FATAL: no installation candidate for ${pkg} (PG_MAJOR=${PG_MAJOR}) in the PGDG repository for $(lsb_release -cs)" >&2; \
            exit 1; \
        }; \
    done && \
    apt-get install -y \
    postgresql-${PG_MAJOR} \
    postgresql-${PG_MAJOR}-pgaudit \
    pgbackrest \
    && rm -rf /var/lib/apt/lists/*

# Publish the major to the runtime: pg-common.sh derives PG_BINDIR from it for the shell
# entrypoint, and the Go agent reads it to locate postgres/pg_controldata. Both fall back to
# 18 when unset, so an older image without this ENV still works.
ENV PG_MAJOR=${PG_MAJOR}

# NOTE: cron is deliberately NOT installed either (#298 review). It came in with the upstream
# repmgr-docker image, where a cron job pruned repmgr's monitoring_history; that table went with
# the mechanism (#294) and nothing in this image or in pg/templates runs cron or crontab any
# more. The chart's own scheduled work (pgbackrest backups, backup validation) is Kubernetes
# CronJobs, which never enter this container's init system. Same argument as kubectl below: an
# installed daemon nothing invokes is CVE surface for nothing.

# NOTE: kubectl is deliberately NOT installed. It existed only for the repmgrd-mode
# service-updater sidecar, which went away with repmgrd (#286). The agent talks to the
# API server through client-go in its own binary, and the pgbackrest CronJobs run
# kubectl from their own alpine/k8s image -- so nothing in this image shells out to it.

# The data directory. The `repmgr` OS user, /etc/repmgr and /var/log/repmgr went with the
# package (#290): nothing in this image reads a repmgr.conf or writes a repmgr log any more.
# The `repmgr` DATABASE and ROLE inside PostgreSQL are a different thing and still exist --
# the agent authenticates as that role for replication (renaming them is #291).
RUN mkdir -p /var/lib/postgresql/data && \
    chown -R postgres:postgres /var/lib/postgresql

# Copy configuration and scripts
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
# Sourced (not executed) by entrypoint.sh -- PG_BINDIR / require_pg_bindir (#269). Replaced
# repmgr-common.sh, whose topology/timeline helpers only the deleted repmgr paths used. No +x.
COPY pg-common.sh /usr/local/bin/pg-common.sh

# Make scripts executable
RUN chmod +x /usr/local/bin/entrypoint.sh

# Go HA agent binary (used by the entrypoint "agent" failover mode)
COPY --from=agent-build /out/pg-ha-agent /usr/local/bin/pg-ha-agent
RUN chmod +x /usr/local/bin/pg-ha-agent

# Set working directory
WORKDIR /var/lib/postgresql

# PostgreSQL
EXPOSE 5432

# SIGINT, not the Docker default SIGTERM (#298 review), matching the upstream `postgres`
# image. In `postgres` mode entrypoint.sh ends in `exec postgres -D "$PGDATA"`, so the
# postmaster really is PID 1 -- and to a postmaster SIGTERM means SMART shutdown: it waits for
# every client session to disconnect. `docker stop` on a container with one idle psql attached
# therefore hangs the full stop timeout, gets SIGKILLed, and the next start does crash
# recovery. SIGINT is FAST shutdown: disconnect clients, checkpoint, exit cleanly.
#
# The chart is unaffected either way -- it runs `agent`, where PID 1 is the Go agent, which
# handles SIGTERM itself and stops its postgres child. This is for direct image use, which
# entrypoint.sh's postgres branch documents as a supported surface.
STOPSIGNAL SIGINT

# Default entrypoint (can be overridden for different use cases)
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]