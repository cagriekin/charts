# PostgreSQL 18 / 17 for Kubernetes HA

High-availability PostgreSQL image with automatic failover for Kubernetes StatefulSet deployments. Built on Debian Trixie. Failover is driven by a Go agent that holds a Kubernetes Lease and manages PostgreSQL directly -- `pg_ctl promote`, `pg_basebackup`, `pg_rewind`, `primary_conninfo` + `standby.signal`.

**repmgr is not installed.** It was, until chart 2.0.0; the agent replaced it. The `repmgr` database and role inside PostgreSQL remain, because the agent authenticates as that role for replication.

## Quick Reference

- **Source**: [GitHub](https://github.com/cagriekin/charts/tree/master/images/pg-ha)
- **Base image**: `debian:trixie-slim`
- **PostgreSQL**: 18 (default) or 17 — one major per tag, see [Tags](#tags)
- **pgaudit**: bundled (`postgresql-<major>-pgaudit`; opt-in audit logging)
- **Kubernetes**: 1.19+
- **Exposed port**: 5432

## Tags

Each release publishes one multi-arch (amd64/arm64) manifest per PostgreSQL major, all SBOM- and provenance-attested and cosign-signed:

| Tag | PostgreSQL |
|-----|------------|
| `trixie-pg18-<n>` | 18 |
| `trixie-pg17-<n>` | 17 |

The major is **in** the tag, and there is no unsuffixed "default major" alias: a pin always names
the major it wants. (The previous scheme was `trixie-<repmgr-version>-<n>` under
`cagriekin/repmgr`, keyed on a package this image no longer contains. That repository stays
published and frozen at its last tag, so existing digest pins keep resolving.)

A container announces its own major as `PG_MAJOR`, and the server binaries live in `/usr/lib/postgresql/$PG_MAJOR/bin`:

```bash
docker run --rm cagriekin/pg-ha:trixie-pg17-1 printenv PG_MAJOR   # -> 17
```

**The major is fixed at build time and cannot be changed for an existing data directory** — a PostgreSQL 18 server refuses to start on a PG17 `PGDATA`. Choose the major when you create the cluster.

## Multi-Mode Architecture

This image serves four distinct roles within a single Kubernetes StatefulSet pod, selected via the entrypoint argument:

```
entrypoint.sh <mode>
```

| Mode | Container Type | Purpose |
|------|----------------|---------|
| `agent` | Main container | The HA agent as PID 1: supervises PostgreSQL, holds the leader Lease, drives failover and Service routing |
| `initdb` | (exec'd by the agent) | Create a new cluster; the lease holder only |
| `init` | Init container | Verify the image bundles the requested PostgreSQL major, then exit |
| `postgres` | Main container | A plain single-node postmaster, bootstrapping its own cluster if empty. For direct use without the agent |

The `repmgrd` and `service-updater` sidecars are gone: the agent does both jobs in one process,
and it re-points the Services itself.

## Environment Variables

### Required (all modes)

| Variable | Description |
|----------|-------------|
| `REPMGR_PASSWORD` | Password for the repmgr replication user |

### postgres mode

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `POSTGRES_USER` | Yes | - | Application database user |
| `POSTGRES_PASSWORD` | Yes | - | Application database password |
| `POSTGRES_DB` | Yes | - | Application database name |
| `PGDATA` | No | `/var/lib/postgresql/data/pgdata` | Data directory path |
| `REPMGR_USER` | No | `repmgr` | Repmgr database user |
| `REPMGR_DB` | No | `repmgr` | Repmgr metadata database |

### init mode

Reads exactly one variable: it verifies the bundled PostgreSQL major and exits. It used to need
the credentials and the headless service to write `repmgr.conf`, poll for a registered primary
and clone; the agent owns all of that now.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PG_MAJOR` | No | the image's own `ENV PG_MAJOR` | Major to expect. A mismatch fails the pod in `Init`, rather than later as `initdb: command not found` |

## Volumes

| Path | Type | Purpose |
|------|------|---------|
| `/var/lib/postgresql/data` | PVC | PostgreSQL data directory |

## How It Works

### Cluster Bootstrap

1. **Init container** verifies this image bundles the PostgreSQL major you asked for, then exits.
2. **Main container** runs the agent as PID 1. It contends for a Kubernetes Lease; the holder
   creates the cluster (`initdb`, WAL settings, the application and `repmgr` roles/databases),
   and every other pod waits until the holder is serving and then clones with `pg_basebackup`
   through its own pre-created replication slot.
3. The agent supervises the postmaster from then on, publishes its role, and points the
   read-write Service at whichever pod holds the Lease.

### Automatic Failover

1. The primary's Lease renewal lapses; a standby acquires it.
2. The new holder promotes with `pg_ctl promote`, records a durable highwater marker, and only
   then re-points the read-write Service.
3. The remaining standbys re-point `primary_conninfo` at the new primary and reload.
4. A node that loses the Lease while read-write demotes itself, so two primaries cannot serve.

### Failed Primary Rejoin

1. The returning node compares its timeline and system identifier against the cluster's.
2. No divergence: it follows the current primary.
3. Diverged: `pg_rewind` onto the primary; only if that cannot proceed does it re-clone, and the
   old data directory is preserved as `.diverged.<timestamp>` until the clone succeeds.
4. A different system identifier is refused -- the agent will not join a foreign cluster.

### Scaling

- **Scale up**: the new pod clones from the current primary through a slot the primary
  pre-creates, so no WAL gap can open before the clone attaches.
- **Scale down**: the primary reclaims the departed ordinal's replication slot once its pod is
  gone, so nothing is left pinning WAL.
- **Stale PVC recovery**: the agent detects a timeline mismatch and rewinds, or re-clones.

## Deploying

Use the Helm chart. It is the supported way to run this image and the only one exercised by CI:

```bash
helm repo add cagriekin https://cagriekin.github.io/charts
helm install pg cagriekin/pg
```

The hand-written StatefulSet example that used to live here has been removed rather than
updated. It specified `repmgrd` and `service-updater` sidecars and an `/etc/repmgr` emptyDir --
none of which exist any more, so following it produced a pod that could not start. Drifting
faster than the image is what a duplicated spec does; the chart is generated from the same
source as the image and cannot drift.

For the values surface, the failover model, backup/restore and the alerting rules, see the
[pg chart README](https://github.com/cagriekin/charts/tree/master/pg).
