# --- Stage 1: build the Go HA agent (cross-compiled, static, no cgo) ---
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS agent-build
ARG TARGETARCH
WORKDIR /src/agent
COPY agent/go.mod agent/go.sum ./
RUN go mod download
COPY agent/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/pg-ha-agent ./cmd/agent

# --- Stage 2: the runtime image (PostgreSQL 18 + repmgr + pgbackrest + agent) ---
FROM debian:trixie-slim

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

# Install kubectl for service updater sidecar
RUN ARCH=$(dpkg --print-architecture) && \
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/${ARCH}/kubectl" && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/

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