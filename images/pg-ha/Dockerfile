# --- Stage 1: build the Go HA agent (cross-compiled, static, no cgo) ---
# Digest-pinned (manifest-list digest, multi-arch). go1.25.12 carries the patched
# stdlib (crypto/tls GO-2026-5856 ECH leak, plus net/textproto, mime, crypto/x509,
# html/template, net) the agent links.
# Refresh with: docker buildx imagetools inspect golang:1.25-bookworm
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS agent-build
ARG TARGETARCH
WORKDIR /src/agent
COPY agent/go.mod agent/go.sum ./
COPY agent/vendor ./vendor
COPY agent/ ./
# Hermetic, air-gappable build: vendored deps only, no module downloads
# (GOPROXY=off). -mod=vendor also fails if vendor/ drifts from go.mod.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOFLAGS=-mod=vendor GOPROXY=off \
    go build -trimpath -ldflags="-s -w" -o /out/pg-ha-agent ./cmd/agent

# --- Stage 2: the runtime image (PostgreSQL + pgaudit + pgbackrest + the Go agent) ---
# Digest-pinned (manifest-list digest, multi-arch). Refresh with:
#   docker buildx imagetools inspect debian:trixie-slim
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e

# PostgreSQL major to build against (#269). The chart runs the server
# binaries from THIS image, so this arg -- not postgresql.image -- decides the cluster's
# major. The default MUST stay 18: the unsuffixed published tag is built without a
# --build-arg and every existing chart pin resolves to it. Explicit per-major tags
# (<tag>-pg17 / <tag>-pg18) are published alongside it; see images/pg-ha/README.md.
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

# Add PostgreSQL APT repository
RUN curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/postgresql-keyring.gpg

RUN echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/postgresql.list

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
    cron \
    && rm -rf /var/lib/apt/lists/*

# Publish the major to the runtime: pg-common.sh derives PG_BINDIR from it for the shell
# entrypoint, and the Go agent reads it to locate postgres/pg_controldata. Both fall back to
# 18 when unset, so an older image without this ENV still works.
ENV PG_MAJOR=${PG_MAJOR}

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

# Default entrypoint (can be overridden for different use cases)
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]