# pg-ha: PostgreSQL HA Image

> Consolidated into this monorepo at `images/pg-ha/` on 2026-06-15 (formerly the standalone
> `repmgr-docker` repository, retained as the historical archive). Published from here via
> `.github/workflows/pg-ha-image-publish.yaml` on `pg-ha-*` tags; the pg/pgvector chart CI
> builds the image from this source (`pg-test.yaml`) rather than pulling it.

PostgreSQL with pgBackRest, pgaudit, cron and the Go HA agent, on Debian Trixie. Designed for Kubernetes StatefulSet deployments with automatic failover and WAL-based incremental backups.

**repmgr is no longer installed** (#290). The agent drives replication directly -- `pg_ctl promote`, `pg_basebackup`, `pg_rewind`, `primary_conninfo` + `standby.signal` -- so the package, the `repmgr` OS user, `/etc/repmgr` and `repmgr.conf` are all gone. The `repmgr` DATABASE and ROLE inside PostgreSQL remain: the agent authenticates as that role for replication (renaming them is #291).

## PostgreSQL major (`PG_MAJOR`)

The major is a **build argument**, defaulting to 18, and one build ships exactly one major (`postgresql-<major>` and `-pgaudit` from PGDG). Supported: **18** (default) and **17**.

```bash
docker build -t cagriekin/pg-ha:2.0.0-pg18 .                        # PostgreSQL 18
docker build --build-arg PG_MAJOR=17 -t cagriekin/pg-ha:2.0.0-pg17 .
```

Published tags per release: `cagriekin/pg-ha:2.0.0-pg18` and `cagriekin/pg-ha:2.0.0-pg17`, from a single git tag `pg-ha-2.0.0` (#290).

Adopting a new image in the charts is a **four-key** edit in each of `pg/values.yaml` and `pgvector/values.yaml`, not two: `ha.image.repository`, `ha.image.tag`, `etcd.rbac.bootstrapImage.repository` and `etcd.rbac.bootstrapImage.tag`. The bundled etcd's RBAC-bootstrap Job runs `pg-ha-agent rbac-bootstrap` from that second pin, so a two-key edit leaves one agent build writing the etcd RBAC that a different build then authenticates against. `pg.validateEtcdBootstrapImage` fails the render when they disagree, so the omission is caught rather than shipped (#291).

The scheme changed with the repmgr removal. It was `trixie-<repmgr-version>-<n>` under
`cagriekin/repmgr`, keyed on a package this image no longer contains -- so a published tag
advertised a version that was not in it. It is now keyed on the PostgreSQL major, which is what
one build actually bundles, and that major is **in** the tag rather than in an optional suffix.
There is no unsuffixed "default major" alias any more: a pin has to say which major it wants,
and the chart's own `majorVersion` guard cross-checks it. `cagriekin/repmgr` stays published and
frozen at its last tag -- nothing is retagged or deleted, because consumers pin those by digest.

At runtime the major is exported as `ENV PG_MAJOR`, which is how the shell layer (`pg-common.sh` derives `PG_BINDIR` from it) and the Go agent (`config.PGMajor` → `PGBindir()`) find the versioned `/usr/lib/postgresql/<major>/bin`. Nothing hardcodes a major; the agent refuses to start if the bindir its `PG_MAJOR` implies holds no `postgres` binary.

The build **fails** if any per-major package has no installation candidate (checked with `apt-cache policy` before installing). That check exists for `pgaudit`: discovered at runtime instead, a missing `postgresql-<major>-pgaudit` would mean the chart's `audit.enabled=true` produces *silently absent* audit logs rather than a loud failure.

Both majors are verified by `test/image-smoke.sh <image-ref> <major>`, which starts the built image and asserts the server version, that `pgaudit` actually loads via `shared_preload_libraries`, that the `postgres` uid/gid are the 101:103 the chart chowns PGDATA to, and that repmgr is genuinely **absent** (binary, `repmgr.so`, OS user and directories):

```bash
bash test/image-smoke.sh cagriekin/pg-ha:2.0.0-pg17 17
```

## Execution Modes

All modes are invoked via `/usr/local/bin/entrypoint.sh <mode>`:

| Mode | Purpose | Container Type |
|------|---------|----------------|
| `agent` | The Go HA agent as PID 1: it starts and supervises PostgreSQL, holds the leader Lease, and drives failover. What the chart renders. | Main container |
| `initdb` | Create a new cluster. Invoked *by the agent*, on the lease holder only. | (exec'd by the agent) |
| `init` | Verify the image bundles the PostgreSQL major the chart asked for, and exit. | Init container |
| `postgres` | A plain single-node postmaster, bootstrapping its own cluster if PGDATA is empty. For direct, non-chart use; the chart never renders it. | Main container |

`repmgrd` and `service-updater` were removed with the repmgrd failover path (#286); `init` no
longer writes `repmgr.conf` or clones (#290) -- the agent owns bootstrap and cloning, from
inside the main container where it can be fenced.

## Environment Variables

### postgres mode

| Variable | Required | Description |
|----------|----------|-------------|
| `PGDATA` | No | Data directory (default: `/var/lib/postgresql/data/pgdata`) |
| `POSTGRES_USER` | Yes | Application database user |
| `POSTGRES_PASSWORD` | Yes | Application database password |
| `POSTGRES_DB` | Yes | Application database name |
| `REPMGR_USER` | No | Repmgr user (default: `repmgr`) |
| `REPMGR_PASSWORD` | Yes | Repmgr password |
| `REPMGR_DB` | No | Repmgr database (default: `repmgr`) |
| `PGBACKREST_ENABLED` | No | Set to `true` to enable WAL archiving via pgBackRest during initdb |
| `PGBACKREST_STANZA` | No | pgBackRest stanza name (default: `db`). Only used when `PGBACKREST_ENABLED=true` |

### init mode

Reads exactly one variable (#290). It used to need the credentials, the headless service and the
node count to write `repmgr.conf`, poll `repmgr.nodes` and clone; it now verifies the bundled
PostgreSQL major and exits.

| Variable | Required | Description |
|----------|----------|-------------|
| `PG_MAJOR` | No | PostgreSQL major to expect (default: the image's own `ENV PG_MAJOR`). Mismatch fails the pod in `Init` rather than later as `initdb: command not found` |

### Removed modes

`repmgrd` and `service-updater` are gone (#286): the lease-based agent replaced both, so there
is no failover daemon to register and no Service selector for a sidecar to patch. The agent
re-points the Services itself.

## How It Works

### Initial Deployment

1. **Init container** (`init` mode): verifies this image bundles the PostgreSQL major the chart
   asked for, then exits 0. That is all it does since #290 -- it used to write `repmgr.conf`,
   poll `repmgr.nodes` for a registered primary and clone with `repmgr standby clone`.

2. **Main container** (`agent` mode): the Go HA agent runs as PID 1. It contends for the leader
   Lease, and from that decides what this node is:
   - Lease holder with an empty data directory: runs `entrypoint.sh initdb` to create the
     cluster (base GUCs, `pg_hba`, the app and `repmgr` roles/databases), then starts and
     supervises the postmaster.
   - Anyone else with an empty data directory: waits until the holder is serving, then clones
     with `pg_basebackup` through its own pre-created replication slot.
   - Existing data: starts it, as primary or standby according to the Lease and the on-disk
     state, and reconciles every tick.

   Everything the sidecars used to do is in this one process: it re-points the Services on
   failover, and it fences itself on lease loss.

### Failover

1. The agent on each standby renews the Lease; when the primary's renewal lapses, one standby
   acquires it.
2. The new holder promotes with `pg_ctl promote`, writes the highwater marker and re-points the
   read-write Service -- in that order, so routing never precedes a durable promotion.
3. The other standbys see the new holder and re-point `primary_conninfo` at it, reloading rather
   than restarting.
4. The demoted node, if it returns, rejoins by `pg_rewind` when its timeline diverged, or simply
   follows when it did not.

### Failed Primary Rejoin

1. The agent reads `pg_controldata` and compares its timeline and system identifier against the
   cluster's.
2. Same cluster, no divergence: it follows the current primary.
3. Diverged: `pg_rewind` onto the primary, and only if that cannot proceed does it re-clone --
   preserving the old data directory as `.diverged.<ts>` until the clone succeeds.
4. A different system identifier is refused outright; the agent will not join a foreign cluster.

### Scale Up/Down

- **Scale up**: the new pod's agent clones from the current primary through its own slot, which
  the primary pre-creates so no WAL gap can open before the clone attaches.
- **Scale down**: the primary reclaims the departed ordinal's replication slot once its pod is
  gone, so nothing is left pinning WAL. There is no `repmgr.nodes` to strand rows in.
- **Scale back up with stale PVC**: the agent compares its timeline against the primary's and rewinds with `pg_rewind`, re-cloning only if that cannot proceed (#290: the init container no longer does this)

## Volumes

| Path | Purpose |
|------|---------|
| `/var/lib/postgresql/data` | PostgreSQL data directory (PVC) |

`/etc/repmgr` is gone (#290): nothing writes a `repmgr.conf` any more, so the emptyDir the init
container used to share with the main one is no longer mounted.

## pgBackRest Integration

When `PGBACKREST_ENABLED=true` is set during first boot (initdb), the entrypoint appends `archive_mode = on` and `archive_command` to postgresql.conf. pgBackRest is pre-installed in the image for use by:

- **archive_command**: PostgreSQL forks pgBackRest to push WAL segments to S3
- **Scheduler sidecar**: Uses pgBackRest to run full/differential backups on a cron schedule
- **Restore**: One-off pods can use pgBackRest to restore to a point in time

pgBackRest configuration (`/etc/pgbackrest/pgbackrest.conf`) and S3 credentials are injected by the Helm chart via ConfigMap and environment variables.

## Building

```bash
docker build -t cagriekin/pg-ha:2.0.0-pg18 .                        # PostgreSQL 18
docker build --build-arg PG_MAJOR=17 -t cagriekin/pg-ha:2.0.0-pg17 .
```

## Compatibility

- PostgreSQL 18 (default) or 17, selected with `--build-arg PG_MAJOR` — see [PostgreSQL major](#postgresql-major-pg_major)
- pgBackRest (latest from PostgreSQL APT repository)
- pgaudit (`postgresql-<major>-pgaudit`; opt-in compliance audit logging)
- Debian Trixie
- Kubernetes 1.19+
