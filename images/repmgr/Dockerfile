# --- Stage 1: build the Go HA agent (cross-compiled, static, no cgo) ---
# Digest-pinned (manifest-list digest, multi-arch). Go 1.25 carries the patched
# stdlib (net/textproto, mime, crypto/x509, html/template, net) the agent links.
# Refresh with: docker buildx imagetools inspect golang:1.25-bookworm
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm@sha256:bbb255b0e131db500cf0520adc97441d2260cf629c7fa7e39e025ddf53995a24 AS agent-build
ARG TARGETARCH
WORKDIR /src/agent
COPY agent/go.mod agent/go.sum ./
COPY agent/vendor ./vendor
COPY agent/ ./
# Hermetic, air-gappable build: vendored deps only, no module downloads
# (GOPROXY=off). -mod=vendor also fails if vendor/ drifts from go.mod.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOFLAGS=-mod=vendor GOPROXY=off \
    go build -trimpath -ldflags="-s -w" -o /out/pg-ha-agent ./cmd/agent

# --- Stage 2: the runtime image (PostgreSQL 18 + repmgr + pgbackrest + agent) ---
# Digest-pinned (manifest-list digest, multi-arch). Refresh with:
#   docker buildx imagetools inspect debian:trixie-slim
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e

# Install system dependencies
RUN apt-get update && apt-get install -y \
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

# Install PostgreSQL and repmgr
RUN apt-get update && apt-get install -y \
    postgresql-18 \
    postgresql-18-repmgr \
    pgbackrest \
    cron \
    && rm -rf /var/lib/apt/lists/*

# Install kubectl for the service-updater sidecar (repmgrd mode). Pinned version
# (matches client-go's API minor) + checksum verification -- no floating stable.txt.
ARG KUBECTL_VERSION=v1.31.3
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl.sha256" -o kubectl.sha256 && \
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c - && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/ && \
    rm kubectl.sha256

# Create repmgr user and directories
RUN useradd -r -s /bin/bash repmgr && \
    mkdir -p /var/lib/postgresql/data && \
    mkdir -p /var/log/repmgr && \
    mkdir -p /etc/repmgr && \
    chown -R postgres:postgres /var/lib/postgresql && \
    chown -R repmgr:repmgr /var/log/repmgr && \
    chown -R postgres:postgres /etc/repmgr

# Copy configuration and scripts
COPY repmgr.conf /etc/repmgr/repmgr.conf.template
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY init-repmgr.sh /usr/local/bin/init-repmgr.sh
COPY service-updater.sh /usr/local/bin/service-updater.sh
COPY repmgrd-entrypoint.sh /usr/local/bin/repmgrd-entrypoint.sh

# Make scripts executable
RUN chmod +x /usr/local/bin/entrypoint.sh \
    /usr/local/bin/init-repmgr.sh \
    /usr/local/bin/service-updater.sh \
    /usr/local/bin/repmgrd-entrypoint.sh

# Go HA agent binary (used by the entrypoint "agent" failover mode)
COPY --from=agent-build /out/pg-ha-agent /usr/local/bin/pg-ha-agent
RUN chmod +x /usr/local/bin/pg-ha-agent

# Set working directory
WORKDIR /var/lib/postgresql

# Expose repmgr ports
EXPOSE 5432

# Default entrypoint (can be overridden for different use cases)
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]