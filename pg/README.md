# Highly-available PostgreSQL with PGPool-II

PostgreSQL Helm chart with native streaming replication and a lease-based Go failover agent, optional PGPool-II for connection pooling and read/write splitting. (repmgr drove replication through 1.x; 2.0.0 replaced it with the agent and removed it from the image — see [Upgrading to 2.0.0](#upgrading-to-200-repmgrd-removed).)

## Features

- PostgreSQL 18.1 with configurable version
- Lease-based HA agent (`pg-ha-agent`, PID 1 in the postgresql container) for automatic failover, replication management, and primary Service selector updates — no sidecars
- Stale-primary protection: a crashed primary that restarts after a standby was promoted rejoins as a standby (via pg_rewind) instead of resuming read-write on a divergent timeline
- Read-only `<fullname>-readonly` service targeting standby pods for read scaling (repmgr mode)
- Optional PGPool-II for connection pooling and read/write splitting
- Support for existing secrets or auto-generated passwords
- StatefulSet-based deployment with persistent storage
- PostgreSQL configuration injection via ConfigMap (postgresql.conf overrides, pg_hba.conf entries)
- PostStart lifecycle hooks with primary-aware execution in repmgr setups
- Pod disruption budgets for safe node drains
- Configurable update strategy, resource limits, probes, and affinity
- Prometheus exporters for PostgreSQL and PGPool-II metrics with ServiceMonitor support
- Automated S3 backups via CronJob with retention management

## Installation

```bash
helm repo add cagriekin https://cagriekin.github.io/charts
helm install my-postgres cagriekin/pg
```

### With Read Replicas

```bash
helm install my-postgres cagriekin/pg --set postgresql.replicaCount=3
```

### With PGPool-II Enabled

```bash
helm install my-postgres cagriekin/pg \
  --set postgresql.replicaCount=3 \
  --set pgpool.enabled=true
```

### With Existing Secret

```bash
kubectl create secret generic pg-secret \
  --from-literal=username=myuser \
  --from-literal=password=mypassword \
  --from-literal=database=mydb \
  --from-literal=repmgr-password=myrepmgrpassword

helm install my-postgres cagriekin/pg \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret
```

Or with custom key names:

```bash
kubectl create secret generic pg-secret \
  --from-literal=user=myuser \
  --from-literal=pass=mypassword \
  --from-literal=db=mydb \
  --from-literal=repmgr-pass=myrepmgrpassword

helm install my-postgres cagriekin/pg \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret \
  --set postgresql.existingSecret.usernameKey=user \
  --set postgresql.existingSecret.passwordKey=pass \
  --set postgresql.existingSecret.databaseKey=db \
  --set postgresql.existingSecret.repmgrPasswordKey=repmgr-pass
```

Without an existing secret the chart generates random passwords on install
and reuses them on subsequent upgrades by looking up the live secret.
Helm's `lookup` returns nothing under `helm template`/`--dry-run`, so
rendering pipelines that never talk to the cluster (e.g. ArgoCD) must use
`postgresql.existingSecret` to keep credentials stable.

## Configuration

### Common Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `imagePullSecrets` | Pull secrets applied to every pod template (StatefulSet, pgpool and exporter Deployments, backup and pgBackRest CronJobs) | `[]` |
| `busyboxImage.repository` | Image for the helper init containers (permission fixups, config copy/templating) across the StatefulSet, pgpool, and exporter pods; override for air-gapped/private registries | `busybox` |
| `busyboxImage.tag` | Helper init image tag | `1.37` |
| `busyboxImage.pullPolicy` | Helper init image pull policy | `IfNotPresent` |
| `busyboxImage.digest` | Optional digest pin (`sha256:...`), appended as `repository:tag@digest` | `""` |

> **Pinning images by digest (#26).** Every image block — `postgresql.image`,
> `ha.image`, `pgpool.image`, `pgpool.metrics.image`, `prometheusExporter.image`,
> `busyboxImage`, `backup.mc.image`, and `pgbackrest.cronjob.image` — accepts an
> optional `digest` (e.g. `sha256:…`). When set, the image is rendered as
> `repository:tag@digest` so a mutable-tag repush cannot silently change what runs.
> Empty (default) pulls by tag.

### PostgreSQL Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.image.repository` | PostgreSQL image repository | `postgres` |
| `postgresql.image.tag` | PostgreSQL image tag | `18.1-trixie` |
| `postgresql.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `postgresql.majorVersion` | PostgreSQL major version in `image.tag`; builds the extension paths (`/usr/lib/postgresql/<major>/lib`, `/usr/share/postgresql/<major>/extension`) when `extensions.enabled=true`. In repmgr mode the server runs from the repmgr image and follows `ha.image.majorVersion` regardless of `postgresql.image`; the chart fails to render if the two majors differ. Set both to `"17"` (with a `-pg17` repmgr tag) to run PostgreSQL 17 — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major). | `"18"` |
| `postgresql.replicaCount` | Number of PostgreSQL replicas (total instances = replicaCount + 1); values > 0 require `ha.enabled=true` | `1` |
| `postgresql.database` | Database name | `postgres` |
| `postgresql.username` | Database username | `postgres` |
| `postgresql.resources.requests.cpu` | CPU request | `100m` |
| `postgresql.resources.requests.memory` | Memory request | `256Mi` |
| `postgresql.resources.limits.cpu` | CPU limit | `1000m` |
| `postgresql.resources.limits.memory` | Memory limit | `1Gi` |
| `postgresql.persistence.enabled` | Enable persistence | `true` |
| `postgresql.persistence.size` | Storage size | `10Gi` |
| `postgresql.persistence.emptyDir.sizeLimit` | `sizeLimit` for the non-persistent (`persistence.enabled=false`) PGDATA emptyDir; empty = fall back to `persistence.size` (never unbounded) | `""` |
| `postgresql.persistence.storageClass` | Storage class | `""` |
| `postgresql.updateStrategy.type` | StatefulSet update strategy | `RollingUpdate` |
| `postgresql.updateStrategy.rollingUpdate.partition` | Partition for rolling update | `0` |
| `postgresql.podAnnotations` | Annotations for PostgreSQL pods | `{}` |
| `postgresql.priorityClassName` | priorityClassName for PostgreSQL pods | `""` |
| `postgresql.affinity` | Affinity rules for PostgreSQL pods | `{}` |
| `postgresql.annotations` | Additional annotations | `{}` |
| `postgresql.podSecurityContext` | Pod-level securityContext for StatefulSet | `{fsGroup: 103, runAsNonRoot: true, seccompProfile.type: RuntimeDefault}` |
| `postgresql.containerSecurityContext` | Container-level securityContext for all PostgreSQL containers | `{runAsUser: 101, runAsGroup: 103, allowPrivilegeEscalation: false, capabilities.drop: [ALL]}` |

### Liveness and Readiness Probes

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.livenessProbe.enabled` | Enable liveness probe | `true` |
| `postgresql.livenessProbe.initialDelaySeconds` | Initial delay | `30` |
| `postgresql.livenessProbe.periodSeconds` | Check interval | `10` |
| `postgresql.livenessProbe.timeoutSeconds` | Timeout | `5` |
| `postgresql.livenessProbe.failureThreshold` | Failure threshold | `10` |
| `postgresql.readinessProbe.enabled` | Enable readiness probe | `true` |
| `postgresql.readinessProbe.initialDelaySeconds` | Initial delay | `5` |
| `postgresql.readinessProbe.periodSeconds` | Check interval | `10` |
| `postgresql.readinessProbe.timeoutSeconds` | Timeout | `5` |
| `postgresql.readinessProbe.failureThreshold` | Failure threshold | `6` |
| `postgresql.startupProbe.enabled` | Enable startup probe (suspends liveness/readiness until PostgreSQL first accepts connections, so the repmgr stale-primary guard and crash recovery are not killed mid-startup) | `true` |
| `postgresql.startupProbe.periodSeconds` | Check interval | `10` |
| `postgresql.startupProbe.timeoutSeconds` | Timeout | `5` |
| `postgresql.startupProbe.failureThreshold` | Failure threshold (`periodSeconds` x this = total startup budget, 600s) | `60` |

### Pod Disruption Budgets

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.podDisruptionBudget.enabled` | Enable PDB for PostgreSQL | `true` |
| `postgresql.podDisruptionBudget.maxUnavailable` | Max pods unavailable during a voluntary disruption | `1` |
| `postgresql.podDisruptionBudget.unhealthyPodEvictionPolicy` | Allow evicting not-yet-Ready pods so a stuck pod cannot wedge a drain (k8s >=1.27) | `AlwaysAllow` |
| `pgpool.podDisruptionBudget.enabled` | Enable PDB for PGPool-II | `true` |
| `pgpool.podDisruptionBudget.minAvailable` | Minimum available pods | `1` |

### PostgreSQL Configuration

Runtime configuration can be injected without rebuilding images. Settings are written to a ConfigMap, mounted into the pod, and loaded via `include_dir`.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.configuration` | Map of postgresql.conf parameters | `{}` |
| `postgresql.pgHba` | List of pg_hba.conf entries injected via postStart | `[]` |
| `postgresql.extraVolumes` | Extra pod-level volumes (see [Mounting an extra file on every replica](#mounting-an-extra-file-on-every-replica)) | `[]` |
| `postgresql.extraVolumeMounts` | Extra mounts for the postgresql container; each must reference a `postgresql.extraVolumes` entry | `[]` |
| `postgresql.extraEnv` | Extra env vars for the postgresql container (supports `value` and `valueFrom`); may not reuse a chart-set name | `[]` |
| `postgresql.extensions.enabled` | Enable extensions support | `false` |
| `postgresql.extensions.packages` | Debian/PGDG packages to `apt-get install` before the copy step, so extensions absent from the donor image (`pg_cron`, `postgis`, …) install without a custom image; `{major}` substitutes `postgresql.majorVersion` (see [Installing extensions without a custom image](#installing-extensions-without-a-custom-image)) | `[]` |
| `postgresql.extensions.aptSources` | Non-PGDG apt sources (e.g. Pigsty) to add before installing `packages`, for extensions PGDG doesn't package (see [Installing packages from a non-PGDG apt source](#installing-packages-from-a-non-pgdg-apt-source-310)) | `[]` |
| `postgresql.extensions.image.repository` | Prebuilt extension image — packages resolved once at build time, so pods do a plain `cp` with no apt and no egress on the pod-start path (see [Taking the install off the pod-start path](#taking-the-install-off-the-pod-start-path-320)) | `""` |
| `postgresql.extensions.image.tag` | Tag for the above; `{major}` substitutes `postgresql.majorVersion`. A tag or digest is required | `""` |
| `postgresql.extensions.image.digest` | Digest pin for the above (recommended for production) | `""` |
| `postgresql.extensions.image.pullPolicy` | Pull policy for the above | `IfNotPresent` |
| `postgresql.extensions.env` | core-v1 `EnvVar` list for the two extension init containers — chiefly `http_proxy`, so the install goes through your own apt mirror and no external host needs opening (see [Pointing the extension install at an apt mirror or proxy](#pointing-the-extension-install-at-an-apt-mirror-or-proxy-320)) | `[]` |
| `postgresql.extensions.envFrom` | Same as `env`, as core-v1 `EnvFromSource` — a ConfigMap/Secret of proxy settings shared across releases | `[]` |
| `postgresql.extensions.extraVolumes` | core-v1 `Volume` list added to the pod for the extension init containers, for what `env` can't express: an `apt.conf.d` snippet or a replacement `sources.list` | `[]` |
| `postgresql.extensions.extraVolumeMounts` | Where `extraVolumes` land inside both extension init containers | `[]` |
| `postgresql.extensions.extraLibs` | Exact absolute FILE paths (no trailing `/`) to additionally copy into a dedicated volume, for a package's own shared-library dependency Debian installs outside the Postgres extension dir (e.g. `libsodium.so.23`; see [Copying a package's own shared-library dependencies](#copying-a-packages-own-shared-library-dependencies-309)) | `[]` |
| `postgresql.extensions.installResources` | Resources for the apt-get step (only rendered while `packages` is non-empty) | `100m/128Mi` req, `1/512Mi` limit |
| `postgresql.audit.enabled` | Enable pgaudit audit logging (requires repmgr mode; see [Audit logging](#audit-logging-pgaudit)) | `false` |
| `postgresql.audit.log` | pgaudit session classes: `read,write,function,role,ddl,misc,misc_set,all` (negate with `-`) | `"ddl, role, write"` |
| `postgresql.audit.logCatalog` | Audit `pg_catalog` statements | `false` |
| `postgresql.audit.logParameter` | Log statement parameter values (may contain PII/secrets) | `false` |
| `postgresql.audit.logRelation` | Log the fully-qualified relation per affected table | `false` |
| `postgresql.audit.role` | Optional `pgaudit.role` for object-level auditing (empty = session-only) | `""` |
| `postgresql.lifecycle.postStart.additionalCommands` | Shell commands to run after PostgreSQL is ready | `""` |
| `postgresql.migrateLegacyMd5Users` | Re-hash MD5 user passwords to SCRAM on PG14+ | `true` |
| `postgresql.nodeSelector` | Node selector for PostgreSQL pods | `{}` |
| `postgresql.tolerations` | Tolerations for PostgreSQL pods | `[]` |
| `postgresql.topologySpreadConstraints` | Spread constraints added alongside the built-in affinity (e.g. a hard zone spread) | `[]` |
| `postgresql.serviceAccount.annotations` | Annotations on the postgresql pods' ServiceAccount (cloud workload identity for keyless pgBackRest S3) | `{}` |
| `postgresql.walLevel` | `replica` or `logical` (#308; see [Logical Replication](#logical-replication-308)). The **only** place to set `wal_level` — setting it in `postgresql.configuration` instead fails at render time | `replica` |

Example:

```yaml
postgresql:
  configuration:
    max_connections: "200"
    shared_buffers: "1GB"
    work_mem: "32MB"
  pgHba:
    - "host all all 10.244.0.0/16 md5"
    - "host replication repmgr 10.0.0.0/8 trust"
  lifecycle:
    postStart:
      additionalCommands: |
        psql -U postgres -d "$POSTGRES_DB" -c "CREATE EXTENSION IF NOT EXISTS vector;" > /dev/null 2>&1
```

When `ha.enabled` is true, `additionalCommands` automatically discover the current primary and execute against it, so DDL statements like `CREATE EXTENSION` work correctly regardless of which pod the hook runs on (including standbys after a failover).

### Logical Replication (#308)

Set `postgresql.walLevel: logical` for a logical-replication subscriber (`CREATE SUBSCRIPTION`, Debezium, or any other decoder on a replication slot). `logical` is a strict superset of `replica` and works regardless of `pgbackrest.enabled`/`archive_mode=on` — the two are unrelated concerns. `wal_level` is a postmaster parameter — the change rolls the pods via the existing configmap-checksum annotation, the same way any other `postgresql.configuration` change does.

**Capacity.** Every physical standby consumes one `max_wal_senders` slot (and, in agent mode, one `max_replication_slots` entry — see `ha.agent.syncReplicationSlots` below); every logical subscriber consumes one more of each. The image's own initdb default is `max_wal_senders = 10` / `max_replication_slots = 10` (unaffected by `postgresql.walLevel`), which now flows through uncontested instead of being silently re-asserted by `pgbackrest-archive.conf` — so raise both via `postgresql.configuration` if `replicaCount` plus your logical subscriber count would otherwise exhaust the default.

**This is the only place to set `wal_level`.** `pgbackrest.enabled` used to render a hardcoded `wal_level = replica` into its own `pgbackrest-archive.conf`, which sorts after `custom.conf` under `include_dir` and would silently win over a `postgresql.configuration.wal_level` you set yourself — that coupling is gone (`postgresql.walLevel` now has its own render block, independent of `pgbackrest.enabled`), but the chart still rejects `wal_level` in `postgresql.configuration` at render time and tells you to set `postgresql.walLevel` instead, so there is exactly one source of truth regardless of pgBackRest's state.

A logical subscriber must connect to the **write Service** (`<fullname>:5432`), not Pgpool — Pgpool's query routing is built for physical replicas, not for holding a replication slot's connection open.

**Surviving a failover: `ha.agent.syncReplicationSlots`.** A plain logical slot does not survive the primary moving — `synchronized_standby_slots` (PostgreSQL 17+) is what lets a **failover** slot (`CREATE SUBSCRIPTION ... WITH (failover = true)`) be synced to a standby so it's still there after a promote, but it names physical replication slots, and PostgreSQL 17+'s `sync_replication_slots` worker (the standby-side process that keeps the failover slot in sync) additionally requires `dbname` in `primary_conninfo`, which repmgr's own clone/follow machinery never sets.

The chart and agent (failover mode `agent` only) handle both automatically when `ha.agent.syncReplicationSlots: true`:

- the agent patches `dbname` into `primary_conninfo` after every clone, follow, and rejoin (a no-op if it's already present, and harmless for physical-only replication either way — it ships unconditionally, not gated behind this value);
- `sync_replication_slots = on` is set in `postgresql.conf` (inert on a primary; needed on any node that may run the slot-sync worker as a standby);
- the primary reconciles `synchronized_standby_slots` to its current, live standbys' physical replication slot(s) on every tick it serves — through scale-up, scale-down, and promote — so the set is never stale.

Requires `postgresql.walLevel: logical` (above) — enforced at render time, not a harmless no-op without it: the `sync_replication_slots` worker this enables on every standby fails its own startup validation below `wal_level: logical` and PostgreSQL restarts it forever, logging the failure on a fixed interval. See [issue #308](https://github.com/cagriekin/charts/issues/308).

### Installing extensions without a custom image

`postgresql.extensions.packages` extends the existing copy-based extension mechanism (`postgresql.extensions.enabled`) with an `apt-get install` step, run inside the throwaway `copy-ext`/`copy-base-ext` init containers before they copy `/usr/lib/postgresql/<major>/lib` and `/usr/share/postgresql/<major>/extension` into the running server. Neither container mounts those emptyDirs at the real native paths — only the main postgresql container does — so `apt-get install` lands real files at the paths the existing copy step already sweeps, and a PGDG/Debian-packaged extension the donor image never shipped (`pg_cron` is the motivating example) reaches the server without building a custom image.

Complete `pg_cron` example:

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    packages:
      - "postgresql-{major}-cron"
  configuration:
    shared_preload_libraries: pg_cron
    cron.database_name: postgres   # the bootstrap database (postgresql.database)
  databases:
    - name: postgres               # the bootstrap database's own name is a valid target:
      extensions:                  # CREATE DATABASE is idempotent (skips if it exists),
        - pg_cron                  # and CREATE EXTENSION still runs against it
```

Declaring `postgres` (or whatever `postgresql.database` is) under `postgresql.databases[]` works because the databases-roles hook Job's `CREATE DATABASE` is already conditional (`WHERE NOT EXISTS ... \gexec`) — it no-ops when the database already exists — and the extension/grant step then runs against that database by name regardless of whether the Job created it. This is preferable to `postgresql.lifecycle.postStart.additionalCommands` for this case: it's already regex-validated (no injection surface) and runs once via a proper Helm hook Job rather than raw shell on every pod boot.

The chart merges nothing into `shared_preload_libraries` for HA any more (a native cluster has no repmgr extension), so your value passes through as written — except `pgaudit`, which is still merged when audit logging is on. Declaring `repmgr` yourself **fails the render** (#293). See [Mounting an extra file on every replica](#mounting-an-extra-file-on-every-replica) below.

**Version pinning.** Append `=version` in apt syntax, e.g. `"postgresql-{major}-cron=1.6.4-1"`. `{major}` is substituted with `postgresql.majorVersion` at render time, so a package list survives a later major bump without editing (confirm the new major has a PGDG build of the same extension before bumping, though).

**PGDG apt-source assumption.** `copy-base-ext` runs from the `cagriekin/repmgr` image, which configures the PGDG apt repository itself at build time — package installs there are reliable whenever repmgr mode is on. `copy-ext` runs from whatever `postgresql.image` you set (default: the official `postgres:18.1-trixie` Docker Hub image); this chart does not verify that image has PGDG configured. This matters most in **standalone mode** (`ha.enabled: false`), where `copy-ext` is your only extension source; in repmgr mode, `copy-base-ext` is a confirmed-good fallback even if `copy-ext`'s apt step comes up short.

**NetworkPolicy.** With `networkPolicy.enabled: true`, egress is closed by default except DNS, PostgreSQL, and 443/6443. `apt-get update`/`install` talks to `apt.postgresql.org` over **port 80** (plain HTTP; the apt source itself is signature-verified via a keyring already baked into the image, so this is not a TLS downgrade of package integrity). Add it via the existing egress hook:

```yaml
networkPolicy:
  postgresql:
    extraEgress:
      - ports:
          - port: 80
            protocol: TCP
```

This egress is needed on **every** pod (re)start, not just the first install: `ext-lib`/`ext-share` are `emptyDir`s, so `copy-ext`/`copy-base-ext` (and the apt-get step) re-run from scratch on a crash, eviction, or rolling update, same as the plain-copy path always has. Cutting egress to `apt.postgresql.org` after a successful install does not affect the already-running server, but a pod that then restarts for any reason will not come back up — plan for persistent, not just one-time, access in an air-gapped or tightly-firewalled cluster.

**Pod Security Admission.** The apt step's init containers run `runAsUser: 0` (dpkg needs root to write `/var/lib/dpkg` and run maintainer scripts) — this is opt-in only while `packages` is set, but a namespace enforcing the PSA **restricted** profile (or any `runAsNonRoot` admission policy) will reject the pod outright. The rest of the chart runs as uid 101; `packages` is not compatible with a `restricted`-labeled namespace.

**Limitation.** This mechanism only helps for extensions that have a Debian package on *some* apt source `copy-ext`/`copy-base-ext` can reach — PGDG by default, or a source added via `postgresql.extensions.aptSources` (below). Extensions with no Debian package anywhere — mostly private/internal ones — are **not** solved by `packages`; those still require a custom image with the extension compiled in.

### Installing packages from a non-PGDG apt source (#310)

Several real-world extensions — `pgsodium`, `supabase_vault`, `pg_graphql`, `pg_net`, `supautils`, `wrappers`, `pgjwt`, `pgmq` — aren't PGDG-packaged at all; they're only available via [Pigsty's apt repo](https://repo.pigsty.io) (`repo.pigsty.io/apt/pgsql/<codename>`). `postgresql.extensions.aptSources` adds a source like that inside `copy-ext`/`copy-base-ext`'s own throwaway filesystem, before the `apt-get install` step above — so a package that isn't on PGDG installs the same way a PGDG one does, without needing it pre-baked into either image (including the `cagriekin/repmgr` image copy-base-ext runs from, which has no Pigsty source and isn't something a chart consumer builds).

`pgsodium`/`supabase_vault` also need `postgresql.extensions.extraLibs` (next section) to actually start — `aptSources` + `packages` alone gets the extension `.so` copied, but not its own runtime dependency, so read that section too before using this example for real.

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    aptSources:
      - name: pigsty
        keyUrl: https://repo.pigsty.io/key
        aptLine: "deb [signed-by=/usr/share/keyrings/pgchart-pigsty-keyring.gpg] https://repo.pigsty.io/apt/pgsql/trixie trixie main"
    packages:
      - "postgresql-{major}-pgsodium"
      - "postgresql-{major}-vault"
```

`trixie` above is the Debian codename the image actually ships (the official `postgres:18.1-trixie` image, by default) — unlike `{major}` in `packages`/`aptLine`, this isn't derived from any chart value, since the chart has no notion of "Debian codename" independent of the image tag; write the one your `postgresql.image`/`ha.image` actually use. `{major}` in `aptLine` still substitutes `postgresql.majorVersion`, same as in `packages`.

Each entry is dearmored to `/usr/share/keyrings/pgchart-<name>-keyring.gpg` and written to `/etc/apt/sources.list.d/pgchart-<name>.list` via `curl | gpg --dearmor` before `apt-get update` runs again — the `pgchart-` prefix means an entry can never collide with a source the image already owns (the `cagriekin/repmgr` image's own PGDG source is `postgresql-keyring.gpg`/`postgresql.list`); `name` must still be unique across your own `aptSources` entries, and both `keyUrl` and `aptLine` are restricted to a narrow character allowlist at render time (`pg.validateExtensionAptSources`), since both are interpolated into a shell command. An entry is rejected outright if `packages` is empty — `aptSources` exists only to make packages from that source installable, so it has nothing to do without at least one.

`curl`/`gnupg`/`ca-certificates` are installed on demand (a no-op if already present, which the `cagriekin/repmgr` image already is) — only when at least one `aptSources` entry is set, so the default `packages`-only path incurs no extra apt-get calls. Pigsty serves over HTTPS (port 443), and the chart's default `networkPolicy` already opens 443/6443 with no destination restriction (S3 endpoints and cloud API servers need the same), so no `extraEgress` addition is needed for it — unlike `apt.postgresql.org`, which needs the port-80 addition above.

### Taking the install off the pod-start path (#320)

Everything above still installs on **every pod (re)start**: `ext-lib`, `ext-share` and
`ext-extra-lib` are `emptyDir`s, so nothing is cached between restarts, and `env`/`extraVolumes`
only change *where* that per-start install fetches from. `postgresql.extensions.image` removes
it entirely — the packages are resolved once, at image build time, and the pods do a plain `cp`.

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    image:
      repository: registry.internal/pg-extensions
      tag: "{major}-supabase"      # {major} substitutes postgresql.majorVersion
      # digest: sha256:...         # recommended for production
    extraLibs:
      - /usr/lib/x86_64-linux-gnu/libsodium.so.23
```

Build it from [`images/pg-extensions/`](../images/pg-extensions/) with your own package list.
There is no published tag: the useful set is per-deployment — a Supabase-shaped set looks
nothing like a PostGIS one — so build it in your own CI and push it to your own registry. That
is also where the egress belongs, once at build time rather than on every pod start in every
tenant namespace.

Two consequences beyond speed, both of which matter more than the speed:

- **No egress on the pod-start path at all.** Not redirected through a proxy — absent. Nothing
  to allow, per tenant, permanently.
- **No root, so no PSA exemption.** The apt path has to *replace*
  `postgresql.containerSecurityContext` with `runAsUser: 0`, because dpkg needs it to write
  `/var/lib/dpkg` and run maintainer scripts — and a namespace enforcing the PSA `restricted`
  profile (or any `runAsNonRoot` admission policy) rejects that pod outright. A `cp` needs
  none of it, so this path keeps the unprivileged context and **works where the apt path
  cannot run at all**.

`packages` and `aptSources` must be empty alongside `image`, and both combinations are refused
at render time. They are not additive: both paths populate the same `ext-lib`/`ext-share`
volumes with a no-clobber copy, so which build of an extension actually won would be decided by
init-container order — an implementation detail of the template — rather than by anything in the
values file. A version-pinned package silently losing to whatever the image happened to contain
is not a trade worth allowing. A non-PGDG source belongs in the image build instead
(`APT_SOURCE_*` build args).

`extraLibs` **does** still apply, reading from the prebuilt image's own filesystem. That is
deliberate: the same absolute paths work on either path, so a working values file moves from
`packages` to `image` with no other edit.

Either `tag` or `digest` is required — refused at render time otherwise. An untagged reference
resolves to `:latest`, which for an extension image means the `.so` files can change under a pod
restart with nothing in the release changing, and an extension built for the wrong major does not
load at all.

**It can add an extension, but it cannot upgrade one the server image already ships.** The
copy is no-clobber and runs last, so anything `copy-base-ext`/`copy-ext` already put in
`ext-lib`/`ext-share` wins — silently, with no render error and no log line. The concrete case
is the **pgvector chart**, whose `postgresql.image` is `pgvector/pgvector` and therefore ships
`vector.so`/`vector.control`: pointing `extensions.image` at a build carrying a *newer* pgvector
is a complete no-op. Use this to add extensions the server images don't have; to change the
version of one they do, change the server image.

There is no safe way around it. Clobbering the `.so` files would overwrite a core lib with a
build the running postmaster never linked against (#302), and clobbering only the control/SQL
files would leave the SQL definitions and the `.so` at different versions — worse than either.

The copy runs in a third init container, `copy-prebuilt-ext`, **last** of the three and with
`cp -n` (no-clobber) — same reason `copy-ext` is: `copy-base-ext` populated `ext-lib`/`ext-share`
from the image that actually *runs* the server, and this is an independent build that can sit on
a different postgres point release, so an unconditional copy would overwrite a core lib (e.g.
`libpqwalreceiver.so`) with one the running postmaster never linked against (#302).

### Pointing the extension install at an apt mirror or proxy (#320)

`copy-base-ext` and `copy-ext` run `apt-get update` + `apt-get install` on **every pod (re)start** — `ext-lib`, `ext-share` and `ext-extra-lib` are `emptyDir`s, so nothing is cached between restarts. That is twice per pod, times every replica, on every crash, eviction, rolling update and scale-up.

Under a per-namespace default-deny egress policy that repetition is not the main cost — the *hosts* are. Every external host the install touches has to sit in the platform's baseline allow, for every tenant, permanently. A Supabase-shaped package set touches three: `apt.postgresql.org`, `repo.pigsty.io`, and — because `postgresql-<major>-pgsodium` depends on `libsodium23`, which neither PGDG nor Pigsty ships — `deb.debian.org`, the general-purpose Debian archive, for one 165 kB package.

`postgresql.extensions.env` is usually all it takes, because apt honours `http_proxy` and it needs no source rewriting:

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    packages:
      - "postgresql-{major}-cron"
    env:
      - name: http_proxy
        value: http://apt-proxy.infra.svc:3142
      - name: https_proxy
        value: http://apt-proxy.infra.svc:3142
      - name: no_proxy
        value: .svc,.cluster.local
```

Use `envFrom` instead when the same settings are shared by every release in the namespace, and `valueFrom` (`secretKeyRef`) when the proxy URL carries credentials — don't inline those, values files get committed.

When `env` isn't enough — you need a real apt configuration file, or a `sources.list` replacement pointing at an internal mirror, because `aptSources` only **appends** source files and cannot rewrite the base sources the images ship — mount one:

```yaml
postgresql:
  extensions:
    extraVolumes:
      - name: apt-proxy-conf
        configMap:
          name: apt-proxy-conf     # key 01proxy: Acquire::http::Proxy "http://apt-proxy.infra.svc:3142";
    extraVolumeMounts:
      - name: apt-proxy-conf
        mountPath: /etc/apt/apt.conf.d/01proxy
        subPath: 01proxy
        readOnly: true
```

All four values apply to **both** extension init containers and to **neither** the postgresql container: an `http_proxy` in the postmaster's own environment would silently redirect anything else that reads it, and an apt configuration mount there is meaningless. They are also rendered only while `packages` is non-empty — the plain-copy path runs no apt at all — and setting any of them with `packages` empty is **rejected at render time** rather than silently ignored, because an operator who believes the proxy is in effect when it isn't has a worse problem than a failed render.

Two more render-time guards, both for failures that would otherwise surface only on a running pod. An `extraVolumes` entry reusing one of the chart's own volume names (`data`, `ext-lib`, `postgresql-config`, …) is refused: volume names are not merged — the later entry in the pod's list wins — so it would **replace** the data PVC or the extension tree with your ConfigMap. And an `extraVolumeMounts` entry is refused if it mounts over `/ext-lib`, `/ext-share` or `/ext-extra-lib` (the trees the install step copies into, which the mount would shadow), or if it names a volume absent from `extraVolumes` (the kubelet rejects that pod at apply time, so helm has to catch it first).

This redirects the per-start install; it does not remove it. To take the install off the pod-start path entirely, see [Taking the install off the pod-start path](#taking-the-install-off-the-pod-start-path-320) above.

#### Don't declare a `pgdg` entry in `aptSources`

It is always fatal, and it's now refused at render time. Both `postgres:*-trixie` and the `cagriekin/repmgr` image already configure `apt.postgresql.org` under their **own** keyring paths, and the chart derives its keyring path from the entry `name` (`/usr/share/keyrings/pgchart-<name>-keyring.gpg`) with no way to override it. apt sees two entries for the same repo with different `Signed-By` values and rejects the **entire** source list:

```text
E: Conflicting values set for option Signed-By regarding source http://apt.postgresql.org/pub/repos/apt/ trixie-pgdg
```

so the install fails before it starts, and the apt error names no values key. Omitting the entry is correct — PGDG packages in `packages` resolve from the image's own configuration, which is exactly what `packages` relies on. The guard keys on the **host**, not the entry name, so any `aptLine` pointing at `apt.postgresql.org` is caught regardless of what you called it.

### Copying a package's own shared-library dependencies (#309)

An apt-installed extension can depend on a general-purpose shared library that is **not itself a Postgres extension module** — `pgsodium`/`supabase_vault` need `libsodium.so.23`, a plain SONAME-versioned runtime library, not something `CREATE EXTENSION` ever loads directly. Debian packages ship libraries like this under the standard multiarch path (`/usr/lib/x86_64-linux-gnu/libsodium.so.23` — confirmed live), **never** under `/usr/lib/postgresql/<major>/lib` where the extension-file copy step reads from. Widening that copy step's glob (`*.so*`, not `*.so`) only helps a package that places a versioned library *directly alongside its own extension modules*; it does nothing for a dependency Debian installs to a different directory entirely, no matter how broad the glob is made. `libsodium.so.23` is exactly this case, and the copy step alone — however broad — cannot fix it.

`postgresql.extensions.extraLibs` closes that gap: an explicit list of exact absolute FILE paths (no trailing `/`; inside `copy-ext`/`copy-base-ext`'s own filesystem, where `packages` already installed them) to additionally copy, alongside the normal extension files:

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    aptSources:
      - name: pigsty
        keyUrl: https://repo.pigsty.io/key
        aptLine: "deb [signed-by=/usr/share/keyrings/pgchart-pigsty-keyring.gpg] https://repo.pigsty.io/apt/pgsql/trixie trixie main"
    packages:
      - "postgresql-{major}-pgsodium"
      - "postgresql-{major}-vault"
    extraLibs:
      - /usr/lib/x86_64-linux-gnu/libsodium.so.23
```

Each `extraLibs` entry lands in its own dedicated volume — `ext-extra-lib`, mounted at `/usr/lib/postgresql/<major>/extra-lib` on the `postgresql` container — **not** in `ext-lib` alongside the plain extension-file copy. This is deliberate: `ext-lib` is also populated by the unvalidated `*.so*` glob copy (either image), so a dependency landing there would sit in the same place as files this chart never inspects; keeping `extraLibs`' own copies physically separate means the only files that can ever appear in `ext-extra-lib` are ones that passed `pg.validateExtraLibs`.

Copying the file alone is still not enough: `pgsodium.so` carries `libsodium.so.23` as a `NEEDED` entry with **no `RUNPATH`/`RPATH`** (confirmed live via `readelf -d`), so once Postgres `dlopen()`s `pgsodium.so` itself (which always succeeds — it's given as an absolute path), resolving *that library's own* dependency on `libsodium.so.23` still goes through the normal dynamic-linker search order, which does not include `/usr/lib/postgresql/<major>/extra-lib` by default. The chart closes this second half automatically: whenever `postgresql.extensions.extraLibs` is non-empty, the `postgresql` container gets `LD_LIBRARY_PATH=/usr/lib/postgresql/<major>/extra-lib`, so a dependency copied there is actually found. Verified end-to-end (`libsodium.so.23` removed from every default search path, present only via a copy at that one location): the module fails to load without `LD_LIBRARY_PATH` and loads cleanly with it.

Gated specifically on `extraLibs`, not on bare `extensions.enabled`: `LD_LIBRARY_PATH` takes priority over the default loader directories for *every* symbol resolution in the process, not just this one dependency, so a release that doesn't use this feature gets no search-path change and no extra volume at all — byte-stable default render.

`extraLibs` is deliberately **explicit, not automatic** — it does not walk `ldd` and copy every transitive dependency a package pulls in. `copy-base-ext` (the `cagriekin/repmgr` image) and `copy-ext` (`postgresql.image`) can be different image builds; silently auto-copying a resolved `libc`/`libssl`/etc. from one into the other risks shadowing the *running* container's own runtime with a build from a different image — an ABI hazard, not a convenience. Every library the postmaster itself links (confirmed live via `ldd postgres` against the shipped `postgres:*-trixie`/`cagriekin/repmgr` images — the full glibc/OpenSSL/Kerberos/LDAP/ICU/compression/audit set, not just `libc`) plus `libpq` (the dependency of `libpqwalreceiver.so`, the exact ABI hazard `#302` exists for) is refused at render time (`pg.validateExtraLibs`) for exactly that reason; `extraLibs` is for extension-specific dependencies like `libsodium.so.23`, never for a library the server already depends on. This is the *current* denylist for the images this chart ships by default, not an eternal guarantee — a future base-image bump could add a dependency it doesn't yet know about. An entry is rejected if `packages` is empty, same reasoning as `aptSources`; an entry whose basename doesn't look like a shared library (no `.so`/`.so.<N>` suffix) is rejected too, so a directory or unrelated file fails the render instead of crash-looping the pod; two entries copying to the same destination filename are also rejected, since which one wins would otherwise depend on `copy-base-ext`/`copy-ext` ordering.

`extraLibs` copies from **each image's own filesystem** — `copy-base-ext` copies from wherever the path resolves inside the `cagriekin/repmgr` image, `copy-ext` from wherever it resolves inside `postgresql.image`. Both must actually have the file at that path (both images are Debian trixie-based by default, so this is normally a non-issue); if they ever diverge in base OS/codename, a path that only exists in one image's build makes `copy-base-ext`'s `&&`-chained `cp` hard-fail the whole command.

**Security note.** `keyUrl` is pinned to `https://` and the rendered `curl` call is pinned with `--proto '=https' --proto-redir '=https'`, so the key can never be fetched (or redirected to) over plaintext HTTP. There's no fingerprint pinning beyond that, though: the key is trusted purely on TLS to whatever host `keyUrl` names, and that host's maintainer scripts then run as root during `apt-get install`. Adding a non-PGDG source is a real trust-boundary expansion — from PGDG/Debian to any host an operator names — same as adding any apt source to any Debian system; there's no way around this that doesn't require re-hosting the key.

### Mounting an extra file on every replica

Some extensions read a key or config file from disk that **must be byte-identical on the primary and every standby** — otherwise a promoted standby behaves differently after a failover. The canonical case is **pgsodium** (the basis of Supabase Vault): its server root key is loaded by `pgsodium.getkey_script`, and if a standby has a different key it cannot decrypt `supabase_vault` secrets once promoted — a silent, post-failover data-availability failure.

Mount the file with `postgresql.extraVolumes` + `postgresql.extraVolumeMounts`. Because these render into the StatefulSet pod template, every replica gets the same file, and it survives failover and a `pg_rewind` rejoin:

```yaml
postgresql:
  extraVolumes:
    - name: pgsodium-key
      secret:
        secretName: pgsodium-root-key
        defaultMode: 0400
        items:
          - key: getkey.sh
            path: getkey.sh
  extraVolumeMounts:
    - name: pgsodium-key
      mountPath: /etc/postgresql/pgsodium
      readOnly: true
  configuration:
    shared_preload_libraries: pgsodium
    pgsodium.getkey_script: /etc/postgresql/pgsodium/getkey.sh
```

There is nothing to merge for HA — a native cluster has no repmgr extension — so your value passes through unchanged, and **declaring `repmgr` yourself now fails the render** (#293). That is deliberate: because the value loads via `include_dir`, it would override the image's own native gate and preload `repmgr.so` onto a cluster with nothing to use it, which becomes an every-pod crash-loop the moment the repmgr-free image ships. The message names the value and the fix. `$libdir/repmgr` and `repmgr.so` are rejected the same way — PostgreSQL resolves all three to the same library.

`postgresql.extraEnv` does the same for environment variables and accepts both `value` and `valueFrom`.

These three values are validated at render time, so a mistake fails `helm install`/`upgrade` with a clear message instead of at apply time or silently at runtime:

- each must be a **list** of objects (a map is a common slip and would otherwise produce an opaque YAML parse error);
- an `extraVolumes` name may not collide with a chart-managed volume (`data`, `postgresql-config`, `postgresql-tls`, `ext-lib`, `ext-share`, `ext-extra-lib`, `repmgr-config`, `etcd-tls`, `pg-run`, `pgbackrest-config`) — a `data` collision is silently dropped in favour of the volumeClaimTemplate;
- every `extraVolumeMounts` entry must reference a declared `extraVolumes` entry (catches the `extraVolume:`/`extraVolumes:` typo, which the API server would otherwise reject only at apply time);
- `extraEnv` may not reuse a chart-set env name (`PGDATA`, `POSTGRES_*`, `REPMGR_*`, …) — duplicate env names are last-wins at runtime, so an override would silently shadow the chart/Secret value.

### Repmgr Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `ha.enabled` | Enable HA (the lease-based agent + replication). `false` is standalone: one stock-postgres pod, `replicaCount` must be `0` | `true` |
| `ha.image.repository` | HA image repository — the PostgreSQL + failover-agent image. Moving to `cagriekin/pg-ha` once that image is published (#290); `cagriekin/repmgr` stays published and frozen so existing pins keep resolving | `cagriekin/repmgr` |
| `ha.image.tag` | HA image tag. Unsuffixed = the default major (18); `-pg18` / `-pg17` select one explicitly | `trixie-5.5.0-33` |
| `ha.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `ha.image.majorVersion` | PostgreSQL major bundled in the repmgr image. In repmgr mode the server always runs this major; `postgresql.majorVersion` must match or the chart fails to render. Move it together with `ha.image.tag` (`17` ⇄ `-pg17`) — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major). | `"18"` |
| `ha.username` | PostgreSQL role the agent authenticates as for probes, `pg_basebackup`, and `primary_conninfo`. Still named `repmgr` for continuity: renaming it rewrites a live cluster's role, so it is out of scope for #291 | `repmgr` |
| `ha.database` | Database the agent connects to for those probes. Named `repmgr` for the same continuity reason as `ha.username` | `repmgr` |
| `ha.terminationGracePeriodSeconds` | Time allowed for graceful shutdown and failover | `120` |
| `ha.resources.requests.cpu` | CPU request | `50m` |
| `ha.resources.requests.memory` | Memory request | `128Mi` |
| `ha.resources.limits.cpu` | CPU limit | `500m` |
| `ha.resources.limits.memory` | Memory limit | `512Mi` |
| `ha.splitBrainDetection.action` | What the agent does when it is read-write without holding the lease: `log` (record and demote) or `fence` (demote and refuse to serve until the lease is reacquired). Both act locally via `pg_ctl`; neither needs pod-delete RBAC | `log` |
| `ha.initContainerResources` | Resources for the `repmgr-init` standby-clone init container (heavier than the shared init default; raise for large databases) | `requests: 100m/128Mi, limits: 1/1Gi` |

There is **no preStop hook** in HA mode. The agent runs as PID 1 and owns SIGTERM: it releases
the Lease first, then stops its PostgreSQL child. A preStop `pg_ctl stop` would race that
supervisor and stop PostgreSQL before the Lease was released.
`ha.terminationGracePeriodSeconds` controls how long Kubernetes waits for that shutdown.

When repmgr is enabled there are **no HA sidecars**: the `pg-ha-agent` binary is PID 1 in the
`postgresql` container and does all of it — holds the Lease, decides promotion by timeline and
LSN, drives replication through repmgr as a pure mechanism, patches the Service selector to the
current primary, and maintains the `pg-role` label (`primary`/`standby`/`orphan`) that the
`<fullname>-readonly` Service selects on. The repmgrd and service-updater sidecars that used to
do this were removed in **2.0.0** (#286); see
[Upgrading to 2.0.0](#upgrading-to-200-repmgrd-removed).

**Split-brain handling**: leadership is a Kubernetes Lease, so two pods cannot both hold it. If a
pod finds itself read-write *without* the Lease — the window a partition can open — it acts on
`ha.splitBrainDetection.action`: `log` records the condition and demotes; `fence` demotes and
refuses to serve until the Lease is reacquired. Both are local operations (`pg_ctl`), so the Role
grants no pod-delete permission. For production, 3+ nodes still reduce partition risk.

### Choosing the PostgreSQL major

**PostgreSQL 18 is the default. PostgreSQL 17 is selectable.** In repmgr mode the server binaries come from the **repmgr image**, not from `postgresql.image` — so the major is decided by which repmgr image you run, and setting `postgresql.image` to another major has no effect on the running server. The image is published per major:

| Tag | PostgreSQL | Use |
|-----|------------|-----|
| `trixie-<repmgr>-<n>` | 18 | The default. What every unsuffixed pin resolves to — unchanged by this feature |
| `trixie-<repmgr>-<n>-pg18` | 18 | The same build, named explicitly |
| `trixie-<repmgr>-<n>-pg17` | 17 | PostgreSQL 17 |

All three are multi-arch (amd64/arm64), SBOM- and provenance-attested, and cosign-signed exactly like the unsuffixed tag.

> **Changing with the next image release (#290).** The image no longer contains repmgr, so the
> tag scheme stops being keyed on a repmgr version. The new image is `cagriekin/pg-ha`, published
> as `<version>-pg18` and `<version>-pg17` — the major is **in** the tag, and there is no
> unsuffixed "default major" alias, so a pin always names the major it wants and
> `ha.image.majorVersion` cross-checks it. The table above describes the image this chart
> pins **today**; `cagriekin/repmgr` stays published and frozen at its last tag, so existing
> digest pins keep resolving. The chart moves in a separate step, after the new image exists.

Three values move **together**; the chart refuses to render if the two majors disagree (in either direction), because a mismatch would silently run one major while building extension paths for another:

```yaml
postgresql:
  majorVersion: "17"
  image:
    tag: 17.10-trixie      # only used in standalone mode / for the extension copy
repmgr:
  image:
    tag: trixie-5.5.0-33-pg17
    majorVersion: "17"
```

The chart checks the claim rather than trusting it: a `-pgNN` tag that disagrees with `ha.image.majorVersion` **fails the render**, and `PG_MAJOR` is passed to every container running the repmgr image — so if the majors are moved while the tag is left on the unsuffixed default (which carries no suffix to compare), the entrypoint and the agent refuse to start, naming both the requested and the bundled major. A wrong-major cluster is therefore a loud failure at install time, not a discovery months later.

Standalone mode (`ha.enabled=false`) is unconstrained: there is no repmgr image in play, so `postgresql.image` alone decides the major.

**This is a create-time choice, not an upgrade path.** The chart has no in-place major upgrade: changing the major of an existing cluster would start a new-major server on an old-major `PGDATA`, which refuses to boot. Moving an existing cluster between majors means a logical dump/restore into a fresh release.

Reasons to pick 17 deliberately:

- **repmgr upstream does not list 18.** repmgr's [install requirements](https://www.repmgr.org/docs/current/install-requirements.html) for 5.5.0 (2024-11-24, still the newest release) name PostgreSQL **13–17**. The image builds `postgresql-18-repmgr` from PGDG, and distro packagers do compile 5.5.0 against 18 — but the honest statement is that the PG18 default rests on **distro packaging rather than an upstream support claim**. If you need an upstream-sanctioned repmgr/PostgreSQL pairing, select 17.
- **Extension availability varies by major** in PGDG, so a required extension can force a major.
- **`pg_dump` output is not guaranteed to load into an older server** ([docs](https://www.postgresql.org/docs/18/app-pgdump.html)), so the major a cluster is created on constrains where its data can later go. If you may need to move data to an older-major server, choose deliberately now.
- **PostgreSQL 17 is supported upstream until 2029-11-08**, so it is a long-lived choice, not a stopgap.

Both majors run the **whole** live test suite in CI (failover, pgBackRest restore/bootstrap, TLS, pgpool, etcd DCS, migration), and each published image is started and checked before release — including that `pgaudit` loads, so [audit logging](#audit-logging-pgaudit) works on either major.

### Failover: the lease-based agent

A Go agent (`pg-ha-agent`) runs as PID 1 in the postgresql container and holds a Kubernetes
`coordination.k8s.io/v1` Lease (`<fullname>-leader`) as the **sole authority** for which pod is
primary, driving repmgr as a pure mechanism (`failover=manual`). The Lease is what makes
split-brain structurally impossible rather than something to detect and repair.

This is the only failover path. It has been the default since `1.0.0`; the legacy repmgrd +
service-updater sidecars were removed in **2.0.0**, and `repmgr.failoverMode` is now rejected at
render time — see [Upgrading to 2.0.0](#upgrading-to-200-repmgrd-removed).

| Parameter | Description | Default |
|-----------|-------------|---------|
| `ha.agent.leaseDuration` | Lease TTL; a challenger cannot acquire until this elapses since the last renew | `15s` |
| `ha.agent.renewDeadline` | Holder self-demotes if it cannot renew within this | `10s` |
| `ha.agent.retryPeriod` | Lease acquire/renew retry interval | `2s` |
| `ha.agent.reconcileInterval` | Reconcile tick interval | `5s` |
| `ha.agent.podCidr` | Pod CIDR trusted in the agent's hardened SCRAM-only pg_hba (no `0.0.0.0/0 md5`); set to your cluster's pod CIDR if outside `10.0.0.0/8` | `10.0.0.0/8` |
| `ha.agent.cascadingReplication` | Let a standby stream from another standby (a chain by pod ordinal toward the primary) to offload the primary's WAL senders. Default off; meaningful at `replicaCount >= 2` (3+ nodes). The agent only picks a verifiably-safe same-timeline upstream and re-homes to the leader if it fails/promotes, so failover is not delayed and a standby is never stranded. | `false` |
| `ha.agent.syncReplicationSlots` | Reconcile `synchronized_standby_slots` to the live standby set on every primary tick, so a logical failover slot survives a promote. Default off; requires PostgreSQL 17+ and `postgresql.walLevel: logical` (#308; see [Logical Replication](#logical-replication-308)). | `false` |
| `ha.agent.mechanism` | `native` — the agent drives `pg_ctl`/`pg_basebackup`/`pg_rewind` and writes `primary_conninfo`/`standby.signal` itself. The only accepted value: the `repmgr` mechanism was removed in 2.0.0 (#294) and is **rejected at render time**, so a stale pin fails loudly instead of being ignored. See [Replication Mechanics](#replication-mechanics-experimental-287) below. | `native` |

Must satisfy `leaseDuration > renewDeadline > retryPeriod` — **enforced at render time** since 2.0.0 (#291): client-go requires it and the agent refuses to start otherwise, so a violating triple used to render cleanly and then CrashLoopBackOff every pod at once. The realistic way to produce one is mixing the `repmgr.agent.*` alias with `ha.agent.*` across two values files — these three keys are cross-validated, so taking one from a legacy file and two from a newer `-f` yields a combination neither file contains. Set all three under the same name. A value that is not a Go duration (`15`, `20 s`) is rejected too, and the `etcd` DCS backend additionally requires `leaseDuration >= 5s` — its lease TTL is whole seconds. Units are case-sensitive (`15S` is an error), an empty value is rejected, and `reconcileInterval` is checked the same way even though it is not part of the ordering. Values Go accepts are accepted: `.5s`, `5.s`, `+15s`, `1m30s`, `2h45m`, `15µs`. For managed clouds, widen them (e.g. `30s/20s/4s`) so a brief apiserver blip does not trip an unnecessary demote. Note: with the Kubernetes Lease backend, a control-plane outage longer than `renewDeadline` is itself a write outage (the healthy primary self-demotes on losing apiserver contact, and no standby can acquire until the control plane returns); this is the safe choice under an asymmetric partition.

The agent also fronts the read/write split: `pgpool` (if enabled) points at the RW (`<fullname>`) and RO (`<fullname>-readonly`) Services with failover off, and the agent maintains the Service selector and `pg-role` labels itself. With `postgresql.replicaCount: 0` (primary-only) there are no standbys, so pgpool configures only the RW backend and runs as a single-backend router — the RO backend is omitted to avoid health-checking an endpointless Service (#207).

### Replication Mechanics (EXPERIMENTAL, #287)

`native` is the only replication mechanic as of 2.0.0 (#294), and the default. The agent drives PostgreSQL's own tools directly: `pg_ctl promote`, `pg_basebackup`, `pg_rewind`, and `primary_conninfo`/`standby.signal` written into an agent-owned config fragment inside `PGDATA`. Topology comes from `pg_stat_replication`, and the agent owns physical replication slot lifecycle.

The `repmgr` mechanic — which shelled out to the repmgr CLI and depended on the `repmgr.nodes` table — is gone, along with repmgr itself ([upstream development had stalled](https://github.com/EnterpriseDB/repmgr)); the image no longer contains it (#290). `ha.agent.mechanism: repmgr` is rejected at render time rather than deleted, so a values file copied from 1.x fails with a message naming the migration. The `Mechanism` interface it was driven through (`images/pg-ha/agent/internal/mechanism`) remains, and the reconcile loop still imports only that interface, so a second implementation stays addressable.

**An existing 1.x cluster is repmgr-shaped on disk and cannot be flipped in place yet (#292).** Until that ships, upgrading a live HA cluster needs the runbook below.

The Lease, the timeline/LSN election, fencing, and Service routing are unchanged from 1.x — policy was never mechanism-specific, which is what made the swap possible.

**Bootstrap is the agent's, and the lease decides.** Under `repmgr`, the `repmgr-init` init container clones every standby before the Go agent runs. Under `native` it exits immediately: it has nothing to do, and the step it used to perform in the middle — polling `repmgr.nodes` until a primary registered itself — is what made native unusable with replicas, because nothing ever registers, so every standby burned the poll's ~240s timeout and sat in `Init:CrashLoopBackOff` forever.

What replaces it:

| Node | Fresh install under `native` |
|---|---|
| Lease holder | `initdb` (via `entrypoint.sh initdb`), then serves |
| Everyone else | wait, then `pg_basebackup` from the holder through their own pre-created slot (#289) |

Whether to `initdb` at all is a **cluster-wide** decision and the lease is the only thing that can make it happen exactly once. If each pod decided for itself, every one would create its own cluster with its own `system_identifier`, and `assertSameCluster` (invariant 9) would then refuse to rejoin any of them — pods `Running`, never `Ready`, each holding a database nothing else recognises. `reconcile.Decide` already encoded this: `BootstrapInitdb` only for the holder with empty data and no reachable primary, `BootstrapClone` for everyone else.

**Topology comes from `pg_stat_replication`, not `repmgr.nodes`.** That table was a *cache* of self-reported metadata: nodes wrote their own rows, rows outlived the pods that wrote them (#139's ghosts), and it could disagree with both the lease and the observed positions. The primary's live connection list cannot go stale that way — a departed pod is simply absent, so there is no durable row to strand. A row is mapped back to a pod by `application_name` (native writes the pod name into `primary_conninfo`; repmgr writes `node_name`, the same string), falling back to the ordinal-named replication slot for any standby cloned before #288, which still dials with libpq's default `walreceiver`. Exported as `pg_ha_agent_replicas_streaming`, `..._replicas_expected` and `..._replicas_unidentified` — that last one matters, because without it an incomplete topology view would read as healthy.

This is **observe-only**. Nothing in the promotion decision consumes it, deliberately: a standby's row vanishes the instant it disconnects, which is exactly the failover moment a promotion is being decided. Absence means "not streaming right now", never "this node does not exist".

**A native cluster has no repmgr extension and no `repmgr.nodes` at all.** The repmgr *database* and *role* do remain — the agent authenticates as that role for every probe and for `pg_basebackup`, and `primary_conninfo` carries `dbname=repmgr`. Renaming them out of the repmgr namespace is #291. The pre-agent stale-primary guard (`entrypoint.sh`'s `primary_safety_guard`) is likewise skipped under `native`: it rejoins and re-clones through the repmgr CLI on the strength of a peer scan that has no notion of the lease, and under native the agent owns both, never starting the postmaster before deciding.

**One recovery path an operator has to know about.** If a native cluster loses the PVC of the
node its highwater marker names, `Decide` returns `Wait` — "empty data with a marker present;
settle before initdb (#170)" — which under repmgr was unreachable, because the entrypoint had
already `initdb`'d by then. Under native nothing else can create the cluster, so the node waits
indefinitely rather than forking a divergent one. That is the safe answer, but it needs a human:
delete the `<fullname>-primary` ConfigMap to let the holder bootstrap again, after confirming no
surviving node holds newer data.

Remaining gaps before #294 can promote `native` to supported and flip the default:

- **`cascadingReplication` works with `native` since #294.** Creation was never the gap: every slot-using native path (`Clone`, `Follow`, `RejoinForceRewind`) ensures this node's slot on whichever upstream it actually points at, so a cascading child self-provisions on its parent. The *reclaim* policy was the gap, and it is now cascade-aware on both sides — see the slot-ownership section below.
- **An existing repmgr cluster cannot be flipped in place yet (#292).** `native` is for fresh installs until that lands.

The `#297` scale-up-race protection is a good example of a repmgr-mode safety behaviour that needs no native equivalent: it refuses to promote a node with no `repmgr.nodes` record, because repmgr resolves a follow target by `node_id` out of that table and an unregistered primary is one no survivor could follow. Native follows by conninfo — the lease holder's identity plus the headless FQDN — so a native primary is followable the moment it promotes, and the gate is skipped rather than replaced.

#### Replication slot ownership (#289)

**The agent owns physical slot lifecycle** (1.x left it to repmgr, which minted `repmgr_slot_<node_id>` itself), because an unowned slot is the most dangerous loose end in the exit: an orphaned slot pins WAL on the primary forever and fills the data volume, and it raises no error at all until the disk is full.

Slots are named `pg_ha_slot_<pod ordinal>` — ordinal-derived, so a pod restart reattaches to the same slot instead of stranding one and reserving a second, and prefixed so ownership is decidable. The agent creates and drops **only names it minted**; an operator's own slot, or a logical slot backing a subscription, is never touched.

| Phase | Behavior |
|-------|----------|
| Before a clone | The slot is created on the upstream *first*, then `pg_basebackup --slot` streams through it — so no WAL gap can open between the base backup starting and the walreceiver attaching. `primary_slot_name` holds it for the ongoing stream. |
| Every primary tick | The lease holder creates a slot for every **expected** peer ordinal (`replicaCount + 1`, not observed standbys — a slot must exist *before* its standby streams) and drops orphans. Dropping is decided from the **live pod set read from the Kubernetes API**, never from `REPMGR_NODE_COUNT` — that variable is baked into each pod at render time, so during a scale-up rollout a not-yet-rolled primary still holds the old count and would drop a brand-new standby's slot. A failed pod list skips the drop pass for that tick. |
| On promote | Slot creation is sequenced **ahead of the routing switch**, so surviving standbys never race slot creation when they follow the new primary — but under its own sub-budget (half the fence budget), so a slow slot query cannot spend the promote's whole window and leave the cutover unfunded. |
| Active slots | **Never dropped.** The `AND NOT active` predicate is in the SQL, so the guard is atomic with the drop; a read-then-decide would leave a window for a standby to reattach in between. Inactivity alone is never treated as evidence a consumer is gone — that is also what a routine restart looks like — which is why liveness is decided by pod existence, not by the slot's `active` flag. |
| Paused | No slot mutation at all — `Decide` returns `NoOp` before any primary branch while `pg-ha/pause` is set. |

Reclaimed as orphans: an agent-minted slot for a **departed** ordinal (no such pod exists), the primary's own slot (it does not stream from itself), and **any** legacy `repmgr_slot_*` — native never streams through one, so every one is dead weight the moment a cluster is on this mechanism, which is what stops a repmgr→native migration (#292) from leaving a permanent orphan behind every surviving node. What makes reclaiming a legacy slot safe is the atomic `AND NOT active` in the drop, not the pod set: a node still carrying its stream through a repmgr slot mid-migration holds it *active*, so the drop is refused until that stream has genuinely moved to its `pg_ha_slot_` replacement. An empty or failed pod list reclaims nothing but self and legacy slots, so a misread can never make every standby's slot look orphaned.

**A demoted primary reclaims what it minted.** The slot pass also runs on the standby branch, because nothing else removed those slots: reconcile is primary-only, `pg_basebackup` and `pg_rewind` both exclude `pg_replslot`, and a plain follow touches nothing. So an ex-primary kept every `pg_ha_slot_*` it created — now inactive, since those standbys stream from the new primary — and **an inactive slot restricts WAL removal on a standby exactly as it does on a primary**, so its own `pg_wal` grew until `max_slot_wal_keep_size` invalidated them. It did not self-heal on a later re-promotion either: by then those ordinals have live pods again, so the primary-side pod-set test reads them as live peers' slots and leaves them.

The standby policy depends on whether a standby can legitimately *be* an upstream, i.e. on `cascadingReplication`. With it **off**, a standby is never an upstream — its own slot lives on its upstream, not locally — so every agent-minted slot found locally is a leftover and there is no pod set to consult. With it **on** that premise is false: its children's slots live there, and reclaiming them every tick would delete exactly what cascading depends on (a child whose walreceiver is reconnecting is inactive for that instant, and `AND NOT active` would let the drop through). So the primary's own predicate is used instead, keeping any slot whose ordinal still has a live pod. `AND NOT active` is still what makes either safe. Listing slots on a standby needs a different reference LSN, since `pg_current_wal_lsn()` raises `recovery is in progress` there; the query branches on `pg_is_in_recovery()` and uses the last *received* LSN instead. Verified on a real streaming pair: a leftover slot on a live standby reports its reserved WAL, is reclaimed, and the standby keeps streaming — while an active slot on the primary is still refused.

Slot creation is driven by the expected pod set but **skipped for ordinals with no live pod**, so the create pass agrees with the drop pass. Judging the two against different sources made them fight: after a `replicaCount` 2→1 scale-down the ordinal-0 primary still holds the old `REPMGR_NODE_COUNT` until the StatefulSet rolls it *last*, so it created a slot on one tick and reclaimed it on the next for the whole rollout. A pod that has been created but is still cloning is already in the live set, so its slot is still minted before it streams — and `Clone`, `Follow` and a rewind-based rejoin each ensure their own slot on the upstream, so even a tick of latency is covered.

`ha.agent.cascadingReplication` works with `mechanism: native` since #294. Two changes made it safe. The primary no longer **pre-creates** a slot per live ordinal when cascading is on: a cascaded standby streams from a peer, so the slot minted on the primary would sit inactive forever and retain WAL until `max_slot_wal_keep_size` invalidated it — the exact failure slot ownership exists to prevent. Nothing is lost, because followers self-provision on their real upstream. And a standby no longer reclaims every agent-minted slot it finds, since its children's slots live there.

A cascaded standby also **releases its own slot on the upstream it leaves**. This is not cosmetic: every standby's first clone comes from the primary (so it provisions its slot there), and cascading then re-homes it onto an intermediate — leaving an inactive, WAL-retaining slot on the primary that the primary's own deliberately-conservative reclaim will never touch. A live three-tier cluster accumulated one per cascaded node before this was added. The owner cleaning up after itself is the only fix that needs no cluster-wide view: the upstream cannot tell "my child moved" from "my child is restarting", which is exactly why its own policy has to stay conservative.

One cost remains: a **demoted** primary running with cascading on keeps the slots it minted for peers that now stream from the new primary, because their pods are still live. Those go inactive and hold WAL on that node. It is bounded — the image sets `max_slot_wal_keep_size = 4GB` at initdb, so PostgreSQL invalidates such a slot rather than filling the volume, and `PGHAReplicationSlotInvalidated` reports it. Telling "this child is momentarily disconnected" from "this peer moved to another upstream" needs a cluster-wide view of who follows whom that no single node has, and holding bounded WAL beats dropping a slot a returning child still needs (that costs it a re-clone).

**Alerting — and this half is NOT native-only.** Everything above describes slot *ownership*, which is native-mode mechanics. Slot *observation* is not gated on the mechanism: the primary publishes the gauges on every tick under `repmgr` too. Repmgr mode has slots as well (`repmgr_slot_*`), they pin WAL in exactly the same silent way, and the chart renders these rules for every agent-mode release — so gauges that only moved under `native` would ship an alert that can never fire, which reads as coverage while providing none. The agent still never *touches* a slot under the repmgr mechanism (repmgr owns lifecycle there); it only reports what it sees, so a sustained breach in repmgr mode is yours to resolve with the query below.

Slot state is exported on the agent's metrics endpoint as `pg_ha_agent_replication_slots`, `pg_ha_agent_replication_slots_inactive`, and `pg_ha_agent_replication_slot_max_retained_wal_bytes`, with two rules under `ha.agent.monitoring.prometheusRule.enabled`:

- `PGHAReplicationSlotInvalidated` (critical, 5m) — PostgreSQL has **killed** a slot's WAL reservation for exceeding `max_slot_wal_keep_size`. **This is the one that fires on chart defaults.** The image sets `max_slot_wal_keep_size = 4GB` at initdb, so PostgreSQL never lets a slot fill the volume — it invalidates the slot instead, and the standby behind it can then only recover by a **full re-clone**. Verified against PostgreSQL 18: invalidation also nulls `restart_lsn`, so the retained-bytes gauge **collapses to zero at the exact moment the slot dies** — which is why retained-WAL alerting alone cannot see this outcome and it needs its own rule.
- `PGHAReplicationSlotRetainingWAL` (critical, 15m) — a slot has held back more than `ha.agent.monitoring.prometheusRule.slotRetainedWALBytes` (default **3Gi**). This is the *early warning* before the invalidation above, so the threshold has to sit **below** the 4GB cap to be reachable at all. Raise it only if you also raise `max_slot_wal_keep_size` through `postgresql.configuration`.
- `PGHAReplicationSlotInactive` (warning, 1h) — a slot has had no consumer for an hour. Expected briefly during a standby restart or re-clone; sustained means a standby is down, or a slot the agent does not own is orphaned and needs a human.

To find a culprit by hand:

```sql
SELECT slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained
FROM pg_replication_slots ORDER BY 3 DESC;
```

### Migrating a repmgrd release (chart 1.x) to 2.0.0

`podManagementPolicy` was `OrderedReady` in repmgrd mode and is `Parallel` for the agent. The
field is **immutable** on an existing StatefulSet, so a release that was pinned to
`failoverMode: repmgrd` needs a one-time recreate (zero data loss — pods and PVCs are kept):

```bash
# 1. Healthy cluster + a fresh backup first. GitOps: disable auto-sync for these steps.
# 2. Orphan-delete the StatefulSet (keeps pods + PVCs running; Helm re-adopts them):
kubectl delete statefulset <release>-pg -n <ns> --cascade=orphan
# 3. Remove `repmgr.failoverMode` from your values (2.0.0 rejects it), then upgrade.
#    This recreates the STS as Parallel and adopts the orphaned pods:
helm upgrade <release> cagriekin/pg -n <ns>   # + your -f values, minus failoverMode
# 4. Verify:
kubectl get lease <release>-pg-leader -n <ns> -o jsonpath='{.spec.holderIdentity}'  # == the primary pod
kubectl get endpoints <release>-pg -n <ns>                                          # points at it
```

Rollback is to chart `1.x` with `repmgr.failoverMode: repmgrd` restored and the same
`--cascade=orphan` recreate, then optionally `kubectl delete lease <release>-pg-leader -n <ns>`.

If you were already on the default (agent) — which has been the default since `1.0.0` — there is
**no recreate**: your StatefulSet is already `Parallel`. Just delete `repmgr.failoverMode: agent`
from your values if you set it explicitly, since 2.0.0 rejects the key either way.

GitOps/ArgoCD: the Lease, the primary-marker ConfigMap, and the write-Service `.spec.selector` are runtime-owned by the agent — `ignoreDifferences` on the Service selector and do not prune the Lease/marker, or auto-sync will fight the agent. Set `postgresql.existingSecret.enabled=true` (the `lookup`-based password generation returns nil under ArgoCD).

### Maintenance mode (pause)

For planned work that would otherwise trigger an unwanted failover (a deliberate primary restart, a node drain, a PostgreSQL minor-version restart), put the agent in **maintenance mode**: it keeps renewing the Lease and serving, but suspends all automatic promote / demote / fence / self-health actions (it only observes). Toggle it with an annotation on the primary-marker ConfigMap:

```bash
# Pause (before the planned operation):
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/pause=true --overwrite
# Resume (after):
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/pause-
```

While paused, `pg_ha_agent_is_paused` reads `1`. Pausing does not stop the cluster from serving — it only stops the agent from reacting to faults, so a genuine failure during the window will NOT fail over until you resume. In particular, if the primary itself wedges or dies while paused, the agent keeps renewing the Lease and the write Service keeps pointing at it; there is no automatic failover until you remove the annotation. (There is no split-brain risk: a real Lease loss still fences via the leader-election callback.) Keep maintenance windows short and watch the cluster while paused.

### Controlled switchover

To hand the primary role to a specific standby on purpose (e.g. to move the primary off a node you are about to drain), annotate the marker with the target pod:

```bash
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/switchover-target=<release>-pg-1 --overwrite
```

The serving primary waits until that target is a **caught-up, same-timeline standby** (its replay LSN has reached the primary's WAL position — invariant 8), then clears the annotation (one-shot) and steps down so the target promotes. If the target is lagging or unreachable, the primary keeps serving and retries — it never steps down onto a behind standby, so committed data is not discarded. The graceful step-down flushes WAL to the connected target, making the handoff near-zero-RPO in practice.

Caveats: this is a planned handoff layered on the lease election, not a fenced zero-RPO transaction — the directed target promotes deterministically on a two-pod cluster; with three or more pods the most-advanced standby wins the freed lease (usually but not necessarily the named target). For strict RPO=0 use synchronous replication (not enabled in this chart).

### Control REST API (agent mode) — `ha.agent.control`

The pause and switchover runbooks above are `kubectl annotate` calls: they work, they are
the reference, and they need no extra machinery. What they cannot do is **check the
request before accepting it**. `kubectl annotate ... switchover-target=<pod>` succeeds
even when the pod does not exist, is on a divergent timeline, or is 4 GB behind — you
find out by tailing logs. It also cannot tell you the cluster's per-member replication
position, or *why* the agent is not doing what you expected.

The optional control API closes both gaps. It is off by default:

```yaml
repmgr:
  agent:
    control:
      enabled: true
      tls:
        existingSecret: pg-control-tls    # keys: tls.crt, tls.key, ca.crt (all three)
      allowedClientCNs: [ops-admin]       # optional; empty = any cert the CA signed
```

It is a **facade, not a second control plane**: `POST /v1/pause` and `POST /v1/switchover`
write exactly the marker annotations the runbooks above write, and the reconcile loop
remains the sole authority for when anything happens. kubectl and the API stay
equivalent, so nothing you learn about one is wasted on the other.

| Endpoint | Scope | What it does |
|----------|-------|--------------|
| `GET /v1/status` | this pod | Role, timeline, LSN, lease holder, pause/switchover intents (read fresh), WAL-replay progress, and the last restore recorded on this data volume |
| `GET /v1/cluster` | this pod's view | Every member's position, plus the reconcile loop's latest decision and its reason |
| `POST /v1/pause` / `POST /v1/resume` | cluster | Maintenance mode; idempotent, and it warns when the cluster is already degraded |
| `POST /v1/switchover` | cluster | Handoff request, **rejected up front** unless the candidate is a reachable standby on the **current primary's** timeline and the cluster is not paused |
| `DELETE /v1/switchover` | cluster | Cancel a pending request |
| `POST /v1/restart` / `POST /v1/reload` | this pod | Restart or SIGHUP the supervised postmaster, via the reconcile loop |
| `POST /v1/reinitialize` | this pod | **Replica only.** Discard this standby's data directory so the loop re-clones it from the primary |
| `GET /v1/backups` | this pod | `pgbackrest info` (requires `pgbackrest.enabled`) |
| `GET`/`POST`/`DELETE /v1/restore` | this pod | PITR restore — see below; `POST` needs its own opt-in |

**Security.** mTLS is the only authentication mode: there is no token, no password, no
plaintext, and reads are authenticated too. TLS 1.3 is required. Enabling the API without
a Secret containing all three of `tls.crt`, `tls.key`, `ca.crt` **fails the render** — you
cannot end up with an unauthenticated mutating port. The server re-reads the files per
handshake, so a rotated (e.g. cert-manager-renewed) Secret takes effect without a pod
restart. Every mutating call emits a structured audit line carrying the client
certificate's CN, fingerprint and serial, and pause/switchover additionally stamp
`pg-ha/paused-by` / `pg-ha/switchover-requested-by` on the marker, so the provenance
survives a pod restart and shows in `kubectl describe`.

The API listens on its own port (`9201` by default) and **never** on `9200` — keeping
them apart is what lets a NetworkPolicy admit Prometheus to the metrics port and nobody
to the control port. With `networkPolicy.enabled=true` the control port gets **no ingress
rule at all**, which in an allowlist policy means deny-by-default; admit a specific
client with `networkPolicy.agentControl.extraIngress`. There is deliberately **no
ClusterIP Service**: node-local verbs act on whichever pod answers, and a load-balanced
control endpoint would eventually restart the primary during an incident. Every node-local
request must name the pod it is addressing (`{"node": "<release>-pg-0"}`) and is refused
with 409 otherwise.

Reaching it as a human:

```bash
kubectl port-forward -n <ns> pod/<release>-pg-0 9201:9201
curl --cacert ca.crt --cert ops-admin.crt --key ops-admin.key \
  https://127.0.0.1:9201/v1/status | jq

# The kubectl runbooks' equivalents, with preflight:
curl -X POST ... https://127.0.0.1:9201/v1/pause
curl -X POST ... -d '{"leader":"<release>-pg-0","candidate":"<release>-pg-1"}' \
  https://127.0.0.1:9201/v1/switchover
```

`port-forward` reaches the pod through the kubelet rather than the pod network, so on
most CNIs it keeps working under a full deny policy — which is the intended path for
humans. The server certificate therefore needs SANs for both `127.0.0.1`/`localhost`
and the headless pod FQDNs (`<release>-pg-0.<release>-pg-headless.<ns>.svc.cluster.local`)
if in-cluster automation calls it too.

`GET /v1/cluster` includes the loop's `lastDecision` (action, target, reason). That is
**diagnostic output, not a stable contract** — action names and wording change without a
version bump.

The switchover preflight measures the candidate against the **lease holder**, not against
the pod you happen to be talking to. That matters because there is no control Service: a
call addressed to a standby that is itself behind on timelines would otherwise refuse valid
candidates and, worse, accept one on the stale timeline. If the primary is not visible in
the answering pod's view of the cluster, the request is refused rather than measured
against a substitute.

New metrics on the read-only port: `pg_ha_agent_control_requests_total`,
`_control_rejected_total`, `_control_intents_total`, `_control_restore_requests_total`.
A refused control call is therefore alertable without exposing the control port to your
scraper.

**If the API dies, the database does not.** A control listener that cannot be constructed
at startup is fatal — you never get a running agent with a silently missing API — but one
that fails *later* is logged and not restarted, on the grounds that HA should survive the
loss of its management surface. The asymmetry is easy to miss during an incident: a healthy,
serving cluster can have no control port at all. If you automate against the API, alert on
`pg_ha_agent_control_requests_total` going flat rather than assuming an unreachable port
means the pod is down.

#### Rebuilding a broken standby — `POST /v1/reinitialize`

When a standby cannot rejoin on its own — a diverged timeline, a corrupt local copy — the
manual fix is to delete its PVC and its pod and let the StatefulSet re-create it. This
endpoint does the same thing without touching Kubernetes objects: it stops PostgreSQL and
empties the data directory, and the reconcile loop's ordinary "empty data, not the chosen
primary → clone from the lease holder" path rebuilds it. There is deliberately no second
clone implementation — the rebuild runs through exactly the path a brand-new replica uses.

```bash
curl -X POST ... -d '{"node":"<release>-pg-1","force":true}' \
  https://127.0.0.1:9201/v1/reinitialize
```

It needs no extra RBAC, and it is guarded three ways:

- **Replica only**, established from two independent live sources rather than from cached
  state: the leader lease is read straight from the DCS, and the durable primary marker must
  not name this pod. Wiping the primary would discard the cluster's only copy of committed
  writes. To rebuild a primary, hand the role away with `POST /v1/switchover` first, or
  recover it from a backup with `POST /v1/restore` (which has its own confirmations and its
  own authz verb). A node running read-write *without* the lease is refused too: the loop is
  about to demote or fence it, and a destructive local action must not race that. A pod that
  has not completed its first reconcile tick is also refused — its role is not yet
  established, and unpopulated state is not evidence of safety.
- **Not while a restore is in flight**, and **not while paused**: a paused loop would never
  re-clone, leaving the replica empty and stopped.
- **`force: true` and the pod named**, like every node-local verb.

The wipe itself refuses unless the directory really is an initialized PostgreSQL data
directory, and it empties the directory rather than removing it (it is a volume mount). A
`postmaster.pid` whose process is **still running** is a hard refusal; one left behind by a
crashed postmaster is recognised as stale and does not block the rebuild — which matters,
because a crashed replica is exactly what this endpoint is for, and only a clean shutdown
removes that file.

It returns `202`: the wipe is immediate, but the clone takes as long as the database is
large and the loop performs it. Watch `GET /v1/status` — `local.hasData` goes true when the
clone lands, then `local.role` becomes `standby`. (`hasData` is reported only for the node
answering the request; it is absent for peers in `GET /v1/cluster`, because a cross-pod probe
cannot observe another member's data directory.)

#### API-driven PITR restore — `ha.agent.control.restore`

`POST /v1/restore` triggers the chart's [pgBackRest restore Job](#point-in-time-recovery).
It is a **separate opt-in from the rest of the API**, because it is the only part of the API
that widens the database pods' Kubernetes privileges at all:

> Creating a Job requires `create` on `jobs`, and RBAC **cannot** restrict `create` by
> `resourceName`. So that grant, *by itself*, lets anything holding the database pods'
> ServiceAccount token create arbitrary Jobs in the namespace — and a Job's pod may name any
> ServiceAccount, with no separate permission check. That makes it a namespace-wide
> privilege-escalation primitive, on a token mounted next to a PostgreSQL that runs
> user-supplied SQL.

Since 1.10.0 that grant does not travel alone: a
[`ValidatingAdmissionPolicy`](#bounding-the-job-create-grant--admissionpolicy) bounds it by
**content**, which is the restriction RBAC has no way to express. Nothing else in the API
adds any RBAC at all.

**What the grant buys** is choosing the recovery point *in the request*: `targetType`,
`target` and `backupSet` are applied to the Job the agent creates, so an operator can
recover to an arbitrary timestamp during an incident with one call. The kubectl
`mode: cronjob` runbook cannot do that — it requires the target in values and a
`helm upgrade` before cloning. If you do not need request-time target selection, leave this
off and use the kubectl path, which needs none of the RBAC above.

```yaml
repmgr:
  agent:
    control:
      restore:
        enabled: true
        allowedClientCNs: [dba-break-glass]   # required; empty denies everyone
```

It requires `pgbackrest.enabled`, `pgbackrest.restore.enabled` and
`pgbackrest.restore.mode=cronjob` (all render-guarded), and `allowedClientCNs` here is a
**second, restore-only** authz list: a client cleared for pause/switchover cannot
overwrite a data directory.

The two lists **compose** — `control.allowedClientCNs` is the door to the API and
`control.restore.allowedClientCNs` an extra lock on this one verb — so when the outer list
is non-empty a restore client must appear in **both**. A 403 tells you which list refused
the call.

##### Bounding the job-create grant — `admissionPolicy`

`allowedClientCNs` bounds who may *ask the API* for a restore. It says nothing about what
anyone holding the pods' ServiceAccount token can do with `create jobs` directly, and
neither can RBAC: authorization sees a verb and a resource, never the object's content.

So the chart renders a `ValidatingAdmissionPolicy` and its binding alongside the grant —
in-tree, **GA since Kubernetes 1.30**, so there is no webhook to deploy, no serving
certificate to rotate, and nothing here that can be *down*. It polices exactly one subject,
this release's ServiceAccount, and requires of every Job that subject creates:

| Pinned | What it takes away |
|---|---|
| `metadata.name` == `<release>-pg-pgbackrest-restore-api` | the `resourceName` restriction RBAC refused to give: `create` collapses to one object. `generateName` cannot dodge it — validating admission sees the final name |
| label `pg-ha/restore=<release>-pg` | provenance; fails closed if chart and policy ever drift |
| `serviceAccountName` == `<release>-pg-repmgr` | the escalation itself — what is left is "the privileges the token already holds" |
| `automountServiceAccountToken: false`, stated explicitly (absent is a denial) | makes naming a ServiceAccount moot: the pod gets no token |
| no `hostNetwork` / `hostPID` / `hostIPC`, no explicit `nodeName`, and only this release's `priorityClassName` | escape to the node; placing the pod by hand instead of through the scheduler; and claiming a high-priority class so the scheduler **preempts this release's own postgresql pods** (referencing a PriorityClass needs no permission). `nodeSelector`/`tolerations` stay unpinned — inherited from `postgresql.*` so the restore can land where the volume attaches, and unlike priority they can only *restrict* placement |
| the **pod's own labels**: only this release's restore labels plus the batch controller's | joining this release's Service endpoints. A restore pod labelled `component=postgresql` would be added to the write Service — with no readiness probe it is Ready at once — and would receive application traffic and its credentials |
| no `manualSelector` | a caller-supplied Job selector and identity labels |
| exactly one container, no init or ephemeral containers, running this release's repmgr image | what code enters the cluster on this token |
| `command` == this release's restore entrypoint, no `args`, no `lifecycle` hooks | the image pin alone is weak — the postgresql container already runs this image against this volume. Without this, the Job is "run anything as the database's uid with the live PGDATA mounted", which destroys data with no privilege escalation at all. A `postStart` exec hook would sidestep a `command`-only pin |
| this release's **pod and container `securityContext`** (`privileged`, `allowPrivilegeEscalation`, `runAsUser`, `runAsNonRoot`, added capabilities) | a root/privileged container with `CAP_SYS_ADMIN` — which reads every other pod's token off the kubelet and is *strictly more* than the token started with |
| CPU and memory requests and limits present; `parallelism` and `completions` ≤ 1 | one permitted Job name as a repeatable way to fill the namespace quota and evict the database's own pods |
| volumes limited to the three the restore template renders (`emptyDir`, ConfigMap `<release>-pg-pgbackrest`, PVC `data-<release>-pg-<podOrdinal>`) | with the token gone this is the one that matters most: arbitrary Secret mounts, `hostPath`, and projected service-account tokens all fall out as denials |
| no `envFrom`; `valueFrom` limited to the downward API and this release's own Secrets | the same bound for env — a key from any other Secret or ConfigMap |

The security contexts, resources and command are pinned to **what this release's own values
render**, not to a fixed hardened profile: change `postgresql.containerSecurityContext` and
the pin moves with it. A chart that denied its own restore Job would be worse than no policy,
so every pin is derived from the same values the `jobTemplate` is.

**Everyone else is untouched.** Humans, GitOps controllers, the CronJob controller and every
other workload create Jobs exactly as before; the policy matches on
`request.userInfo.username` and nothing else. That direction is asserted in the integration
suite next to the denials, because a policy that broke every other controller in the
namespace would be worse than the hole it closes.

##### What this does *not* bound

Be clear-eyed about the residual, because it decides whether this feature belongs on your
cluster:

- **The restore parameters are not bounded, and cannot be.** Anything holding the token can
  still create the one permitted Job with its own `TARGET`, `BACKUP_SET` and `FORCE` — a real
  restore of this release, over the live PGDATA, without presenting a client certificate to
  the control API. `allowedClientCNs` guards the API; it cannot guard `create jobs`. A
  restore over the live data directory *is* the operation being exposed, so admission has
  nothing left to reject. pgBackRest still refuses to restore while `postmaster.pid` exists,
  which in practice means this needs the StatefulSet already scaled to 0.
- **The command pin is not a sandbox.** Bash reads `$BASH_ENV`, and an actor who already runs
  code in the postgresql container can write a file into PGDATA — which this Job mounts. So
  code execution inside the restore container is reachable. What it reaches is uid 101 with no
  token and only this release's volumes: the privileges already held, which is the bar this
  policy is written to. The image, security-context, volume and env pins are what hold that
  bar — not the command pin alone.
- **It is not a check that the Job matches the release in full.** CEL sees only the admission
  request, so the verbatim-`jobTemplate` clone remains what guarantees the rest. This is
  defence in depth on top of that.

So the policy turns "namespace-wide privilege escalation from a SQL injection" into "an
unauthenticated trigger for this release's own restore". That is a large reduction and the
reason the feature is defensible at all — but on a cluster where untrusted SQL runs and an
unscheduled restore would itself be a serious incident, **leaving `control.restore` off is
still the right answer.**

`failurePolicy: Fail` is deliberate — under `Ignore`, an evaluation failure would silently
re-open the hole while the grant stayed rendered. For the same reason the grant and its bound
cannot be separated by accident: **rendering the RBAC without the policy fails the render**
unless you acknowledge the trade in values.

```yaml
repmgr:
  agent:
    control:
      restore:
        enabled: true
        allowedClientCNs: [dba-break-glass]
        admissionPolicy:
          enabled: true               # default
          acknowledgeUnbounded: false # required to be true if you set enabled: false
```

Before you enable restore triggering, two operational consequences:

- **These are this chart's only cluster-scoped objects.** The installing identity needs
  cluster-scoped `create` on `admissionregistration.k8s.io`
  (`ValidatingAdmissionPolicy`, `ValidatingAdmissionPolicyBinding`), and the cluster must be
  **≥ 1.30**. That is enforced by asking whether `admissionregistration.k8s.io/v1` exists, not
  by comparing the reported Kubernetes version: with no cluster to query, `.Capabilities.
  KubeVersion` is the *helm client's own* built-in version, so a version floor would fail
  every `helm template` run by a slightly older helm (3.14 reports v1.29) no matter what the
  target cluster is. Asking for the API group is also the more accurate question — it is false
  where 1.30+ has the group disabled via `--runtime-config`. Either way the *render* fails,
  rather than the apply failing halfway. A default install renders neither object, so a namespace-limited installer
  is unaffected until it opts in. The names carry the namespace and release for readability
  and a hash of the pair for uniqueness (`<namespace>-<release>-pg-restore-guard-<hash>`);
  the hash is the part that matters, because a plain hyphen join is ambiguous when either
  segment contains hyphens. The binding is scoped to the release namespace by
  `kubernetes.io/metadata.name` — a label the API server sets itself, so it cannot be left
  off.
- **Mutating admission that rewrites `Job` objects can make the clone stop matching a pin.**
  Sidecar injectors act on *Pods* and are unaffected, but a policy engine that mutates pod
  controllers may not be. The denial names the exact field, so you will know immediately
  rather than during an incident.

Legitimate reasons to turn it off — a cluster below 1.30, a namespace-limited installer, one
policy managed centrally by a platform team, or simply accepting the unbounded grant as
1.9.0 required — are all real. Set `admissionPolicy.enabled: false` **and**
`acknowledgeUnbounded: true`; the second is what keeps the decision reviewable in values
rather than invisible in a Role.

What it does and does not control:

- The Job is a **verbatim clone** of the rendered CronJob's `jobTemplate` — identical to
  `kubectl create job --from`, so image, ServiceAccount (with its token still not
  mounted), security contexts, volumes and secret references all come from the release.
  The agent overrides only `TARGET_TYPE`, `TARGET` and `BACKUP_SET`, and it refuses to
  overwrite any env backed by `valueFrom` (it cannot turn a `secretKeyRef` into a literal).
  It overrides **only what the request specifies**: omit `targetType`/`target` and the Job
  keeps whatever `pgbackrest.restore.targetType`/`target` the release pinned, rather than
  being blanked to "latest" — the response reports the values actually in effect as
  `effectiveTargetType`/`effectiveTarget`/`effectiveBackupSet`, so you can see which
  recovery point is really about to be applied.
- **Which volume is restored into is not an API decision.** That is
  `pgbackrest.restore.podOrdinal`, rendered into the Job; a `podOrdinal` in the body may
  only *confirm* it (409 on mismatch). The request must also be addressed to the pod that
  owns that volume.
- The API **never sets pgBackRest's `--force`**, which bypasses the `postmaster.pid`
  interlock — the last guard against restoring over a live volume. If you genuinely need
  the stale-pid bypass, set `pgbackrest.restore.force=true` in values, where it is
  reviewable.
- Destructive by declaration: `force: true` and `confirm: "<statefulset name>"` are both
  required, and the cluster must already be **paused** — an active reconcile loop would
  restart the postmaster the restore needs stopped. (The exact confirm value is whatever
  `GET /v1/status` reports as `cluster`; it is `<release>-pg` unless the release name
  already contains the chart name.)

One call performs the whole safe sequence: verify paused → verify the confirmations →
stop the local postmaster → create the Job, returning `202` with the Job name.

**Do not forget the `POST /v1/resume` at the end.** Maintenance mode makes the reconcile
loop a no-op, so a restored node that is scaled back up while still paused never starts
PostgreSQL and never goes Ready — the cluster stays down until you resume. The API will not
clear your pause on its own, and `nextSteps` lists the resume as an explicit step.

**But not before the restore finishes.** `POST /v1/resume` is refused with 409 while the
restore Job is `pending` or `running`: resuming then would let the loop start PostgreSQL on
a data directory pgbackrest is still rewriting. Wait for `GET /v1/restore` to report
`succeeded` (or `failed`), or abandon the restore with `DELETE /v1/restore` first. The same
interlock guards `POST /v1/reinitialize`.

Two limits on that interlock, both worth knowing before you rely on it:

- **It sees only the Job the API creates** (`<release>-pg-pgbackrest-restore-api`). A
  restore you started the kubectl way —
  `kubectl create job --from=cronjob/<release>-pg-pgbackrest-restore restore-now` — carries
  a different name, and finding it would need `list jobs`, which this chart deliberately
  does not grant. So **do not mix the two paths**: if you trigger a restore with kubectl,
  clear the pause with kubectl too, and do not use `POST /v1/resume` or
  `POST /v1/reinitialize` until the Job is done. The same applies in reverse — clearing the
  pause with `kubectl annotate ... pg-ha/pause-` bypasses the check the API would have made.
- **It fails closed**, so with restore enabled a transient apiserver error makes
  `POST /v1/resume` answer `502` and the cluster stays paused (and therefore will not fail
  over) until the Job read succeeds. Deliberate — the alternative is starting PostgreSQL on
  a directory that may still be being rewritten — but it is a runtime dependency resume does
  not otherwise have.

```bash
curl -X POST ... -d '{}' https://127.0.0.1:9201/v1/pause
curl -X POST ... https://127.0.0.1:9201/v1/restore \
  -d '{"node":"<release>-pg-0","confirm":"<release>-pg","force":true,
       "targetType":"time","target":"2026-08-01 09:55:00+00"}'
```

**What it deliberately does not do: scale the StatefulSet.** Scaling to 0 deletes every
agent — including the one that would report progress — so it stays an operator step and the
response hands back the commands in `nextSteps`.

Whether you need it depends on scheduling, and this is worth knowing before an incident:
**ReadWriteOnce binds a volume to a node, not to a pod.** A restore Job that the scheduler
places on the same node as the target pod therefore starts *immediately*, with the
StatefulSet still scaled up — that is safe here only because this flow already stopped the
postmaster and required the cluster to be paused. **That pair, not the scale-down, is what
keeps a restore off a live data directory.** A Job placed on any other node sits `Pending`
on the volume until you scale down, which is what the `hint` explains.

With standbys, scale down regardless: they must not stream from a primary being rewritten
underneath them, and they re-clone onto the new timeline afterwards (see
[the multi-replica restore behaviour](#point-in-time-recovery)).

Progress, honestly:

- If the Job had to wait for the volume, `GET /v1/restore` reports `pending` and explains
  why in `hint` — "the StatefulSet is still scaled up, so the volume cannot attach", the
  most common mistake. Once you scale down to free it there is **no agent alive** to ask,
  so nothing reports the copy's progress from then on.
- After scale-up, `GET /v1/status` reports **WAL-replay progress** (`recovery.replayLsn`,
  `recovery.replayLagBytes`, and `recovery.lastReplayTime` — directly comparable to a
  `targetType: time` target). For a PITR this is usually the phase you are actually
  waiting on.
- `lastRestore` reports the **outcome** — which backup set, which target, exit code,
  post-restore control-file state, who requested it — read from a record `restore.sh`
  writes onto the data volume. It outlives the Job and its logs, so it answers "what
  happened to my restore?" later, and doubles as provenance for where this PGDATA came
  from. The agent removes that record when the directory stops being what it describes (a
  `POST /v1/reinitialize` wipe, or a clone by the reconcile loop), so a rebuilt replica
  never reports a backup set as the origin of data it streamed from the primary.
- Live file-copy percentage is available only via `restore.readPodLogs: true`, which adds
  `get pods/log` **namespace-wide** (the Job's pod name is generated, so it cannot be
  scoped) and only helps when some agent is still running, i.e. `podOrdinal > 0`. Off by
  default; the signals above need no extra privilege.

### Monitoring the agent (agent mode)

The agent serves read-only Prometheus metrics on port `9200` (`pg_ha_agent_is_leader`, `_is_paused`, `_renew_failures_total`, `_promotions_total`, `_demotes_total`, `_fences_total`, `_reconcile_errors_total`, `_recovery_starts_total`). With the Prometheus Operator installed:

```yaml
repmgr:
  agent:
    monitoring:
      serviceMonitor: { enabled: true }   # scrape the agent metrics off the headless Service
      prometheusRule: { enabled: true }   # example alerts (no-leader, split-brain, renew-failure, flapping, agent-down, paused-too-long)
```

The bundled `PrometheusRule` covers leadership/fencing health only; row-level **replication lag** alerts come from the PostgreSQL exporter (`prometheusExporter.enabled`).

### Leadership backend: Kubernetes Lease (default) or etcd (agent mode)

By default the leader Lease lives in the Kubernetes apiserver. A sustained control-plane outage longer than `renewDeadline` therefore causes a write outage by itself (the healthy primary self-demotes on losing apiserver contact, and no standby can acquire until the control plane returns). To decouple leadership from the control plane, point the agent at an existing **etcd** cluster:

```yaml
repmgr:
  agent:
    leaseDuration: 15s            # must be >= 5s for etcd (the lease TTL is whole seconds)
    dcs:
      backend: etcd
      etcd:
        endpoints: ["https://etcd-0.etcd:2379", "https://etcd-1.etcd:2379"]
        prefix: ""                # defaults to /pg-ha/<release>/
        tls:
          secretName: etcd-client-tls   # optional mutual TLS; Secret must carry tls.crt, tls.key, AND ca.crt
```

The TLS secret must contain all three keys (`tls.crt`, `tls.key`, `ca.crt`). cert-manager `Certificate` secrets include `ca.crt`, but a plain `kubectl create secret tls` does **not** — add `ca.crt` explicitly, or the agent fails fast at startup (it reads all three).

With etcd, a Kubernetes control-plane outage no longer demotes the primary — only an etcd quorum loss does (so etcd must be operated for HA). Failover-time **routing** (the write-Service selector patch) still uses the apiserver, but during a no-failover outage kube-proxy holds the last endpoints. The chart drops the `coordination.k8s.io/leases` RBAC grant and opens egress to etcd `:2379` automatically in this mode. Pick the backend at install time; switching it on a live cluster is a controlled re-election (treat like a planned failover).

Two ways to provide etcd:

- **BYO/shared** (recommended, especially for several databases against one platform etcd): set `ha.agent.dcs.etcd.endpoints` as above and leave `etcd.enabled=false`.
- **Bundled** (self-contained, for an install with no existing etcd): set `etcd.enabled=true` and leave `endpoints` empty — the chart deploys a 3-node etcd cluster (`<release>-etcd`) and points the agent at it automatically. Adds 3 stateful pods (`+~0.3 CPU / 0.4Gi` requested, small SSD PVCs). The bundled etcd runs plaintext within the pod network (isolate it with a NetworkPolicy; the leadership data is non-secret); tune it under the `etcd:` values key (`replicaCount`, `resources`, `persistence`, `topologySpreadConstraints`). For a TLS-secured store, use a BYO/shared etcd with `dcs.etcd.tls`.

> Agent mode is opt-in and validated by the chart's live failover suite (graceful failover: a standby promotes, the write Service repoints, the ex-primary rejoins read-only). See `ENVIRONMENT.md` for the full injected-variable catalog.

### Routing the agent's apiserver traffic — `KUBECONFIG` (#317)

The agent reads its primary marker and publishes gossip through the apiserver, so a cluster whose egress policy denies pod traffic to the apiserver **never elects a leader and never gets a serving primary** — while every pod, Service and policy looks correctly configured. The symptom is a repeating pair of warnings against the apiserver ClusterIP followed by `action=Wait ... no leader yet`:

```text
level=WARN msg="read marker" err="... dial tcp 10.96.0.1:443: i/o timeout"
level=WARN msg="publish status (gossip)" err="... dial tcp 10.96.0.1:443: i/o timeout"
level=INFO msg="reconcile decision" hold_lease=false action=Wait reason="... no leader yet"
```

Some policies cannot be re-opened from the policy side. On Cilium, deny wins within a tier — so no allow rule admits the apiserver for one namespace — and reserved identities are compound (`reserved:host` and `reserved:kube-apiserver` sit on the same identity), so any topology that reaches the apiserver via a real node IP cannot admit apiserver traffic for one workload without admitting node traffic for it. What remains is an in-cluster TCP proxy.

The agent therefore honours **`KUBECONFIG`** (repmgr image `trixie-5.5.0-33` or newer, pinned by this chart since 1.14.1). No new chart value is involved — set the variable and mount the file with the passthrough that already exists:

```yaml
postgresql:
  extraEnv:
    - name: KUBECONFIG
      value: /etc/apiserver-proxy/kubeconfig
  extraVolumes:
    - name: apiserver-proxy
      configMap:
        name: apiserver-proxy-kubeconfig
  extraVolumeMounts:
    - name: apiserver-proxy
      mountPath: /etc/apiserver-proxy
      readOnly: true
```

```yaml
# the mounted kubeconfig: a different ADDRESS, the apiserver's own CERTIFICATE
apiVersion: v1
kind: Config
current-context: proxy
clusters:
  - name: proxy
    cluster:
      server: https://apiserver-proxy.kube-system.svc:8443
      tls-server-name: kubernetes.default.svc          # what the cert is verified against
      certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
contexts:
  - name: proxy
    context: {cluster: proxy, user: sa}
users:
  - name: sa
    user:
      tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
```

Keep `tokenFile`/`certificate-authority` pointed at the ServiceAccount mount as above: the identity stays the pod's ServiceAccount, so the chart's RBAC still applies unchanged and there is no second credential to rotate. Only the **address** moves.

Why a kubeconfig and not `KUBERNETES_SERVICE_HOST`: the apiserver's serving certificate has SANs for `kubernetes.default.svc…` and the apiserver IPs, not for the proxy Service. Overriding the host retargets the dial but leaves no way to set the name TLS verifies against, so it trades a routing failure for a verification failure. `tls-server-name` is the half only a kubeconfig can express.

Notes:

- Both apiserver clients take this route — the mutation client (write-Service selector, `pg-role` labels, primary marker) **and** the Lease-backed leader election when `dcs.backend=kubernetes`. Reaching one but not the other would elect a leader that cannot publish. `dcs.backend=etcd` is unaffected, since leadership does not use the apiserver at all in that mode.
- A `KUBECONFIG` that is set but unreadable, malformed, or contextless is a **startup failure naming the file**, not a silent fall back to in-cluster — falling back would reproduce the exact hang this escapes, with a kubeconfig mounted and apparently in effect.
- The boot log records which route was taken, so this is answerable without a debugger: `msg="starting pg-ha-agent" ... apiserver="kubeconfig /etc/apiserver-proxy/kubeconfig"` (or `apiserver=in-cluster`).
- With `KUBECONFIG` unset — the default — behaviour is byte-identical to before: the in-cluster ServiceAccount plus `KUBERNETES_SERVICE_HOST`/`_PORT`. `~/.kube/config` is deliberately *not* consulted, so a stray file in the image or on a mounted home cannot silently redirect a production cluster.
- The entrypoint's stale-primary guard (`#170`) shells out to `kubectl`, which honours `KUBECONFIG` natively, so it follows the same route with no extra wiring. It runs in the **postgresql** container (`entrypoint.sh` `postgres`/`agent` mode), which is where `postgresql.extraEnv`/`extraVolumeMounts` land; the `repmgr-init` init container makes no apiserver calls at all, so it needs none of this.
- The pgBackRest **backup CronJob** is a separate apiserver client with a separate pod, and it
  is not covered by `postgresql.extraEnv` — it has its own passthrough, `pgbackrest.extraEnv`
  ([#323](#routing-the-backup-cronjobs-apiserver-traffic--pgbackrestextraenv-323)). A cluster
  that needs this route needs it in both places.
- **`kubectl` and the agent disagree on a *broken* kubeconfig.** The stale-primary guard's `kubectl` uses client-go's deferred loader, which *does* silently fall back to in-cluster when the merged kubeconfig is missing, empty, or has no current context — so on the very clusters this feature is for, a broken mount makes the guard time out and take its documented fail-open fast path (a single peer scan instead of the settle-retry) while the agent refuses to boot. The peer scan still refuses to `initdb` next to a reachable primary, so this narrows a safety margin rather than removing it, but it is the reason to mount the kubeconfig from a ConfigMap that cannot vanish and to check the `apiserver=` log line after the first roll.

### Routing the backup CronJob's apiserver traffic — `pgbackrest.extraEnv` (#323)

The pgBackRest **backup CronJob is an apiserver client**. It resolves the current primary at
fire time by listing EndpointSlices and then drives pgBackRest with `kubectl exec` — which is
what makes a schedule survive a failover, and what makes it fail on a cluster where the pod's
route to `kubernetes.default.svc` is closed:

```text
couldn't get current server API group list: Get "https://10.96.0.1:443/api?timeout=32s":
dial tcp 10.96.0.1:443: i/o timeout
```

The second-order damage is the one to watch for. That CronJob is also the **only** caller of
`stanza-create` in the chart, so a blocked apiserver means the repository is never initialised
and `archive_command` fails on every WAL segment from the moment the cluster starts —
`archived_count 0, failed_count 196` within the hour, on a cluster whose pods, Services and
policies all read as correctly configured.

The escape is the one `postgresql.extraEnv` already gave the agent in
[#317](#routing-the-agents-apiserver-traffic--kubeconfig-317), now available on the pgBackRest
side too:

```yaml
pgbackrest:
  extraEnv:
    - name: KUBECONFIG
      value: /etc/apiserver-proxy/kubeconfig
  extraVolumes:
    - name: apiserver-proxy
      configMap:
        name: apiserver-proxy-kubeconfig
  extraVolumeMounts:
    - name: apiserver-proxy
      mountPath: /etc/apiserver-proxy
      readOnly: true
```

Use the same kubeconfig shape as #317 (a different **address**, the apiserver's own
certificate, `tls-server-name`, and the ServiceAccount's own token file — so RBAC is unchanged
and there is no second credential to rotate).

**What the three values reach.** All of them apply to every container that runs the pgBackRest
binary or drives it, which is more than the CronJob:

| | `pgbackrest.extraEnv` / `extraVolumeMounts` | `pgbackrest.extraVolumes` |
|---|---|---|
| `pgbackrest` sidecar (postgresql pod) | ✅ | pod-level ✅ |
| `pgbackrest-bootstrap` init container (postgresql pod) | ✅ | same pod |
| backup CronJobs (`full`, `diff`) | ✅ | ✅ |
| restore Job / CronJob | ✅ | ✅ |
| validation CronJob | ✅ | ✅ |
| **postgresql container** | ❌ — use `postgresql.extraEnv` | — |

The sidecar matters as much as the CronJob: `stanza-create` and `backup` actually execute
*there*, via `kubectl exec` from the CronJob. A setting that reached the CronJob alone would
route the `kubectl` call and leave the backup itself unchanged — so a proxy or a private CA for
the S3 repository has to land on both, and it does.

The **postgresql container is deliberately excluded**. It has `postgresql.extraEnv` of its own,
which is also where `archive_command`'s own pgBackRest invocation reads its environment; and a
`KUBECONFIG` injected there would redirect the entrypoint's stale-primary guard (#170) as a
side effect of configuring backups. Keep the two lists separate even when they carry the same
values.

> **If you are routing the S3 repository** — a proxy, a private CA — rather than the apiserver,
> `pgbackrest.extraEnv` alone is **not enough**. `archive_command` runs pgBackRest inside the
> postgresql container, so it reads `postgresql.extraEnv`. Set it in both lists, or you get
> working backups, restores and validation while `archive-push` fails on every segment and WAL
> accumulates in `pg_wal` until the volume fills. The apiserver case (`KUBECONFIG`) is the
> opposite: there the postgresql container must *not* get it.

**`extraVolumes` reaches the database pods too**, since the sidecar and the bootstrap init
container live there. A ConfigMap or Secret that does not exist yet therefore holds the
**postgresql** pods in `ContainerCreating` on the next roll, not just the backups — create it
before the upgrade, or mark the source `optional: true`.

**With `ha.agent.control.restore` enabled**, the `#279` ValidatingAdmissionPolicy that
bounds the agent's `create jobs` grant pins the restore Job's volume sources and env
`valueFrom` names. The chart folds these values into those pins automatically, so the
agent-driven restore keeps working — but only sources it can bind to a *name* can be pinned
(`emptyDir`, `configMap`, `secret`, `persistentVolumeClaim`; `fieldRef`, `secretKeyRef`,
`configMapKeyRef` for env). Anything else (`projected`, `csi`, `hostPath`, `resourceFieldRef`)
is a **render failure**, deliberately: admitting an unpinned source would reopen the door that
policy exists to close, and leaving it out would surface as `POST /v1/restore` denied at
admission during an incident.

**Guarded at render time**, since every one of these failures is otherwise apply-time or
run-time only:

- Setting any of the three while `pgbackrest.enabled` is `false` is **refused**, not ignored —
  silently inert backup configuration is precisely the failure mode above.
- `extraVolumes` names are checked against the chart's own volumes in *all four* pods, and
  against `postgresql.extraVolumes` / `postgresql.extensions.extraVolumes`, which land in the
  same postgresql pod volume list (a duplicate name is rejected by the API server).
- Every `extraVolumeMounts` entry must reference a declared `extraVolumes` entry (the kubelet
  rejects the rest at apply time — the CronJob would render, fire, and never run), must not
  repeat a `mountPath`, and must not shadow a path the containers depend on. At, above **or
  inside** for PGDATA (a projection inside it shadows part of the directory the restore writes),
  `/scripts` (a read-only configMap volume — the kubelet cannot create a nested mountpoint and
  the pod sticks in `CreateContainerError`) and `/etc/pgbackrest/pgbackrest.conf` (a file). At
  or above only for `/work`, `/tmp` and `/var/run/postgresql`, which are writable `emptyDir`s —
  nesting inside them is the normal case, so `/tmp/kube` is fine. So is a sibling such as
  `/etc/pgbackrest/conf.d`, and `mountPath: /` is refused by name.
- `extraEnv` may not reuse a name the chart sets on any of the containers (`PGBACKREST_*`,
  `STANZA`, `TARGET`, `HOME`, …), including names only a currently-disabled feature emits — so
  a passthrough that works today cannot start silently shadowing a chart value after a later
  `helm upgrade` enables `repoEncryption` or switches `s3.keyType`.

If you are patching the rendered CronJobs in an operator today to inject this, these values
replace that: a post-render patch matched on the `-pgbackrest` ServiceAccount breaks silently
if the chart renames it, and "silently" there means a tenant with no backups and nothing wrong
in its status.

### PGPool-II Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.enabled` | Enable PGPool-II | `false` |
| `pgpool.image.repository` | PGPool-II image repository | `cagriekin/pgpool` |
| `pgpool.image.tag` | PGPool-II image tag | `4.7.1` |
| `pgpool.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `pgpool.replicaCount` | Number of PGPool-II instances | `1` |
| `pgpool.command` | Override the pgpool container startup command. Empty uses the chart default, which runs `ipcrm -a` to reap SysV shmem orphaned by a prior OOM-killed pgpool before `exec`ing pgpool (prevents a cross-restart shmem-accumulation crash loop). Keep the `ipcrm` reap and `exec` if you override. | `[]` |
| `pgpool.numInitChildren` | Number of worker processes | `32` |
| `pgpool.maxPool` | Max cached connections per process | `4` |
| `pgpool.childLifeTime` | Worker process lifetime in seconds | `300` |
| `pgpool.connectionLifeTime` | Cached connection lifetime in seconds | `600` |
| `pgpool.clientIdleLimit` | Client idle timeout in seconds | `300` |
| `pgpool.resetQueryList` | Queries to run when returning connection to pool | `ABORT; RESET ALL; DEALLOCATE ALL` |
| `pgpool.failOverOnBackendError` | Trigger failover on backend errors | `false` |
| `pgpool.allowClearTextFrontendAuth` | Allow clear-text password authentication from clients | `false` |
| `pgpool.admin.username` | PGPool-II admin (PCP) user, stored in the chart-managed Secret | `admin` |
| `pgpool.admin.password` | PGPool-II admin (PCP) password, stored in the chart-managed Secret | `admin` |
| `pgpool.admin.existingSecret.enabled` | Use an existing Secret for the admin credentials instead of the chart-managed one | `false` |
| `pgpool.admin.existingSecret.name` | Name of the existing Secret (required when enabled) | `""` |
| `pgpool.admin.existingSecret.usernameKey` | Key in the existing Secret containing the admin username | `username` |
| `pgpool.admin.existingSecret.passwordKey` | Key in the existing Secret containing the admin password | `password` |
| `pgpool.service.type` | Service type | `ClusterIP` |
| `pgpool.service.port` | Service port | `9999` |
| `pgpool.service.exposePcp` | Expose the PgPool-II PCP admin port (9898) on the Service. Off by default (admin endpoint); enable only if you run `pcp_*` against the Service, and add a `pgpool.extraIngress` rule for 9898 under NetworkPolicy. | `false` |
| `pgpool.resources.requests.cpu` | CPU request | `100m` |
| `pgpool.resources.requests.memory` | Memory request | `128Mi` |
| `pgpool.resources.limits.cpu` | CPU limit | `500m` |
| `pgpool.resources.limits.memory` | Memory limit | `512Mi` |
| `pgpool.podSecurityContext` | Pod-level securityContext for PGPool-II | `{runAsNonRoot: true, seccompProfile.type: RuntimeDefault}` |
| `pgpool.containerSecurityContext` | Container-level securityContext for PGPool-II | `{runAsUser: 999, runAsGroup: 999, allowPrivilegeEscalation: false, capabilities.drop: [ALL]}` |
| `pgpool.podAnnotations` | Annotations for PGPool-II pods | `{}` |
| `pgpool.priorityClassName` | priorityClassName for PGPool-II pods | `""` |
| `pgpool.affinity` | Affinity rules for PGPool-II pods | `{}` |
| `pgpool.topologySpreadConstraints` | Topology spread constraints | `[]` |
| `pgpool.nodeSelector` | Node selector for PGPool-II pods | `{}` |
| `pgpool.tolerations` | Tolerations for PGPool-II pods | `[]` |
| `pgpool.logging.logConnections` | Log client connections | `true` |
| `pgpool.logging.logStatement` | Log SQL statements | `false` |
| `pgpool.logging.logPerNodeStatement` | Log backend routing | `false` |
| `pgpool.logging.logMinMessages` | Minimum log message level | `warning` |

#### TCP Keepalive

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.tcpKeepalive.idle` | Seconds before sending keepalive | `10` |
| `pgpool.tcpKeepalive.interval` | Seconds between keepalive probes | `3` |
| `pgpool.tcpKeepalive.count` | Failed probes before disconnect | `5` |

#### Health Check

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.healthCheck.period` | Health check interval in seconds | `10` |
| `pgpool.healthCheck.timeout` | Health check timeout in seconds | `30` |
| `pgpool.healthCheck.maxRetries` | Max retries before marking backend down | `10` |
| `pgpool.healthCheck.retryDelay` | Seconds between retries | `3` |

#### PGPool-II Metrics Exporter

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.metrics.enabled` | Enable pgpool2_exporter sidecar | `false` |
| `pgpool.metrics.image.repository` | Exporter image | `pgpool/pgpool2_exporter` |
| `pgpool.metrics.image.tag` | Exporter image tag | `1.2.2` |
| `pgpool.metrics.resources.requests.cpu` | CPU request | `50m` |
| `pgpool.metrics.resources.requests.memory` | Memory request | `64Mi` |
| `pgpool.metrics.resources.limits.cpu` | CPU limit | `200m` |
| `pgpool.metrics.resources.limits.memory` | Memory limit | `128Mi` |
| `pgpool.metrics.livenessProbe.initialDelaySeconds` | Liveness initial delay | `30` |
| `pgpool.metrics.livenessProbe.periodSeconds` | Liveness check interval | `30` |
| `pgpool.metrics.livenessProbe.timeoutSeconds` | Liveness timeout | `15` |
| `pgpool.metrics.livenessProbe.failureThreshold` | Liveness failure threshold | `5` |
| `pgpool.metrics.readinessProbe.initialDelaySeconds` | Readiness initial delay | `10` |
| `pgpool.metrics.readinessProbe.periodSeconds` | Readiness check interval | `30` |
| `pgpool.metrics.readinessProbe.timeoutSeconds` | Readiness timeout | `15` |
| `pgpool.metrics.readinessProbe.failureThreshold` | Readiness failure threshold | `3` |

### Secret Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.existingSecret.enabled` | Use existing secret | `false` |
| `postgresql.existingSecret.name` | Existing secret name | `""` |
| `postgresql.existingSecret.usernameKey` | Username key in secret | `username` |
| `postgresql.existingSecret.passwordKey` | Password key in secret | `password` |
| `postgresql.existingSecret.databaseKey` | Database key in secret | `database` |
| `postgresql.existingSecret.repmgrPasswordKey` | Repmgr password key in secret | `repmgr-password` |
| `postgresql.existingSecret.monitoringPasswordKey` | Monitoring-user password key in secret (required when `prometheusExporter.monitoringUser.enabled`) | `monitoring-password` |

When `postgresql.existingSecret.enabled` is `false`, a secret will be auto-generated with:
- `username`: Base64 encoded value from `postgresql.username`
- `password`: Random 32 character alphanumeric string
- `database`: Base64 encoded value from `postgresql.database`
- `repmgr-password`: Random 32 character alphanumeric string (when repmgr is enabled)

### Service Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `service.type` | Primary and read-only service type | `ClusterIP` |
| `service.port` | Primary and read-only service port | `5432` |
| `nameOverride` | Override chart name | `""` |
| `fullnameOverride` | Override full release name | `""` |

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.annotations` | Global annotations applied to all resources | `{}` |

### NetworkPolicy Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `networkPolicy.enabled` | Enable NetworkPolicy resources for pod isolation | `false` |
| `networkPolicy.postgresql.allowExternal` | Allow ingress to PostgreSQL on 5432 from any pod in the namespace. This is the path the read-only Service (`<fullname>-readonly`, direct standby reads) relies on — see the caveat below before setting `false`. | `true` |
| `networkPolicy.postgresql.extraIngress` | Additional ingress rules for PostgreSQL (full ingress-rule objects with their own `from`/`ports`, like `extraEgress`; appended at the rules level) | `[]` |
| `networkPolicy.postgresql.extraEgress` | Additional egress rules for PostgreSQL | `[]` |
| `networkPolicy.pgpool.extraIngress` | Additional ingress rules for PGPool-II (full ingress-rule objects with their own `from`/`ports`) | `[]` |
| `networkPolicy.pgpool.extraEgress` | Additional egress rules for PGPool-II | `[]` |
| `networkPolicy.prometheusExporter.extraIngress` | Additional ingress rules for the postgres-exporter (full ingress-rule objects). Use this to allow a Prometheus in another namespace to scrape 9116 — see the cross-namespace note below. | `[]` |
| `networkPolicy.prometheusExporter.extraEgress` | Additional egress rules for the postgres-exporter | `[]` |

When enabled, NetworkPolicies restrict traffic:
- **PostgreSQL**: ingress on 5432 from peer pods, PGPool, Prometheus exporter, backup jobs, and optionally all namespace pods. Egress allows DNS, peer replication, 443 and 6443 (S3 over HTTPS and the Kubernetes API server), and the port of `pgbackrest.s3.endpoint` when pgBackRest is enabled.
- **PGPool**: ingress on 9999 from namespace pods. Egress only to PostgreSQL on 5432.
- **Prometheus exporter**: ingress on 9116 from namespace pods. Egress only to PostgreSQL on 5432.

> **Cross-namespace metric scraping.** The metric-port ingress rules (exporter 9116,
> pgpool 9719, agent 9200) admit *same-namespace* pods only. A Prometheus in a separate
> monitoring namespace (the usual `ServiceMonitor` topology) must be allowed explicitly
> via a `namespaceSelector`. The exporter now has its own `extraIngress`:
>
> ```yaml
> networkPolicy:
>   prometheusExporter:
>     extraIngress:
>       - ports:
>           - port: 9116
>             protocol: TCP
>         from:
>           - namespaceSelector:
>               matchLabels:
>                 kubernetes.io/metadata.name: monitoring
> ```
>
> Use `networkPolicy.pgpool.extraIngress` (9719) and `networkPolicy.postgresql.extraIngress`
> (9200) the same way for the pgpool and agent metric ports.

> **`allowExternal: false` and the read-only Service.** `allowExternal` gates *direct*
> client access to PostgreSQL on 5432. PGPool (9999) is always reachable in-namespace,
> so read-write clients going through PGPool are unaffected. But the
> `<fullname>-readonly` Service connects clients *directly* to standbys on 5432, so with
> `allowExternal: false` those read connections are blocked while `kubectl get endpoints`
> still looks healthy (connections simply time out). To keep direct-5432 clients
> (read-only consumers, or apps connecting straight to the primary) working under
> `allowExternal: false`, add a scoped `extraIngress` rule allowing your client pods,
> e.g.:
>
> ```yaml
> networkPolicy:
>   postgresql:
>     allowExternal: false
>     extraIngress:
>       - ports:
>           - port: 5432
>             protocol: TCP
>         from:
>           - podSelector:
>               matchLabels:
>                 app: my-read-client
> ```
>
> For clients in another namespace, add a `namespaceSelector` (alone, or combined with
> `podSelector` in the same `from` entry) — `podSelector` matches the policy's own
> namespace only.

## Databases & roles

By default the chart provisions exactly one database and one superuser
(`postgresql.database` / `postgresql.username`). To run it as a **platform database**
serving several apps/teams, declare additional databases, roles, and grants — a
post-install/upgrade hook Job (`<release>-pg-databases-roles`) applies them idempotently
on the primary (they replicate to standbys), re-running on every `helm upgrade` and after
a restore. Default-empty, so a minimal install is unaffected.

```yaml
postgresql:
  roles:
    - name: app
      # LOGIN role. Empty passwordSecret.name => the chart generates a password and
      # persists it in the chart Secret under key "app-acl-password" (survives upgrades).
      passwordSecret: { name: "", key: "" }
      grants:
        - { database: app, privileges: [CONNECT] }
        - { database: app, schema: public, privileges: [USAGE] }
        - { database: app, schema: public, objects: ALL_TABLES, privileges: [SELECT, INSERT, UPDATE, DELETE] }
    - name: analyst
      memberOf: [readers]
      grants:
        - { database: app, schema: public, objects: ALL_TABLES, privileges: [SELECT] }
    - name: readers
      login: false          # NOLOGIN group role
  databases:
    - name: app
      owner: app            # must be a role in roles[] or the primary user
      extensions: [pgvector]
```

**Grant forms** (each entry targets one `database`; `privileges` are GRANT keywords only,
allowlist-validated — never arbitrary SQL):

| `schema` | `objects` | Emits |
|---|---|---|
| — | — | `GRANT … ON DATABASE` |
| set | — | `GRANT … ON SCHEMA` |
| set | `ALL_TABLES` / `ALL_SEQUENCES` | `GRANT … ON ALL <objs> IN SCHEMA` **plus** `ALTER DEFAULT PRIVILEGES` so future objects inherit it |

**Rules enforced at render time** (`helm` fails fast, before anything is applied, so a typo
never reaches the cluster as a half-failed hook): names match `^[A-Za-z_][A-Za-z0-9_]*$`; no
reserved/internal names (`postgres`, `repmgr`, the monitoring user, the primary user,
`template0/1` — reserved unconditionally, even when repmgr/the exporter are disabled);
unique; `owner` must be a declared role or the primary user; a grant's `database` must be a
declared database or the primary database; `memberOf` targets must be a declared role or a
built-in `pg_*` role; a grant with `objects` must also set `schema` (an object grant has no
database-level meaning); privileges from the allowlist (`CONNECT, CREATE, TEMP, USAGE,
SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, EXECUTE, MAINTAIN, ALL`).

**Passwords** never touch the ConfigMap — they are read into the Job from a Secret via psql
`\getenv`. A chart-generated password is minted per LOGIN role unless you set an explicit
`passwordSecret.name`/`key` (read from your own Secret).

> **The Secret is the source of truth for role passwords.** The hook re-applies it (`ALTER
> ROLE … PASSWORD`) on every upgrade, so rotate by updating the Secret and re-running
> `helm upgrade` — an out-of-band `ALTER ROLE` done directly in PostgreSQL will be reverted
> on the next upgrade (same model as the primary and monitoring passwords).

> **GitOps / render-only (ArgoCD):** always set an explicit `passwordSecret.name` for every
> LOGIN role. The chart-generated password relies on a cluster `lookup` that is empty under
> `helm template`, so a generated password would regenerate on every sync and lock the role
> out. With `postgresql.existingSecret.enabled`, an explicit `passwordSecret` is required.

> The hook runs the declared state idempotently; it **creates and grants**, it does not drop
> roles/databases you remove from values (that would risk data loss) — drop those by hand.

## Connecting to PostgreSQL

### Direct Connection to Primary

```bash
kubectl port-forward svc/my-postgres-pg 5432:5432
psql -h localhost -U postgres -d postgres
```

### Read-Only Connection to Replicas

When repmgr is enabled, a `<fullname>-readonly` service routes only to standby pods, selected via the `pg-role: standby` label. The agent maintains the label, with a 3-way classification (in-recovery -> `standby`; reachable-but-not-in-recovery -> `orphan`, kept OUT of the read pool so a divergent node never serves stale reads; unreachable -> left untouched):

```bash
kubectl port-forward svc/my-postgres-pg-readonly 5432:5432
psql -h localhost -U postgres -d postgres
```

With `postgresql.replicaCount: 0` the service exists but has no endpoints.

### Through PGPool-II

```bash
kubectl port-forward svc/my-postgres-pg-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres
```

## TLS / encrypted client connections

Optional, off by default (no rendered change at defaults). Bring your own certificate in a
Secret — the chart does not generate one (no cert-manager dependency).

```yaml
postgresql:
  tls:
    enabled: true
    existingSecret: postgresql-tls   # keys: tls.crt, tls.key, ca.crt
    require: false                   # reject non-TLS clients (agent mode only)
    clientCertAuth: false            # mutual TLS for app users (agent mode only)
```

The server certificate **must** list SANs for the names clients connect to: the
read-write Service (`<release>-pg`), the read-only Service (`<release>-pg-readonly`), the
PGPool Service (`<release>-pg-pgpool`) if used, and the headless pod FQDNs
(`*.<release>-pg-headless.<ns>.svc.cluster.local`). `ca.crt` is used only under
`clientCertAuth` (to verify client certs) and by `verify-*` clients (to verify the server).

**Server TLS** (`enabled` alone) sets `ssl = on`; clients may opt in with
`sslmode=require`. **`require`** makes the pod-CIDR client rule `hostssl`, rejecting
non-TLS connections. **`clientCertAuth`** additionally requires app users to present a
client cert signed by `ca.crt`. The chart's internal service users (the `repmgr` user, the
superuser, and the monitoring user) are **exempted** from the client-cert requirement, so
the HA agent, repmgr, the exporter, and PGPool keep working; under `require` those
components reach the server over TLS via libpq's default negotiation.

When `require` is on, the exporter and the PGPool backend must also speak TLS
(`prometheusExporter.sslmode >= require`, `pgpool.tls.backendSslmode >= require`); under
`clientCertAuth` with PGPool, PGPool needs a backend client cert
(`pgpool.tls.backendClientCert`) for app-user passthrough. The render fails fast if these
are inconsistent.

```yaml
# PGPool TLS (frontend to clients + backend to PostgreSQL)
pgpool:
  tls:
    enabled: true
    existingSecret: pgpool-tls
    backendSslmode: require
    backendClientCert: pgpool-backend-tls   # required under PostgreSQL clientCertAuth
prometheusExporter:
  sslmode: require                          # disable|require|verify-ca|verify-full
```

Caveats:

- **Replication stays plaintext** on the pod network — `require`/`clientCertAuth` never
  convert the `host replication` or loopback `pg_hba` rules (repmgr/agent replication
  conninfo carries no `sslmode`). This is a deliberate non-goal.
- **`require`/`clientCertAuth` require `ha.enabled=true`.** They depend on the
  agent-assembled `pg_hba` (hostssl with no md5 fallback), and standalone mode runs the stock
  postgres image's own `pg_hba`. For standalone use `postgresql.tls.enabled` for optional server TLS; enforced TLS needs
  agent mode (the render fails fast otherwise).
- **Exporter `verify-full`:** the exporter scrapes each pod at its short headless name
  (`<release>-pg-<i>.<release>-pg-headless`), so `prometheusExporter.sslmode=verify-full`
  requires those short per-pod names in the server cert SANs. `verify-ca` (recommended for
  the exporter) verifies the cert chain without the hostname check and needs no extra SANs.
- **Cert rotation:** PostgreSQL reloads `ssl_*` on SIGHUP, not when the mounted Secret
  changes. Run `kubectl rollout restart statefulset/<release>-pg` after rotating the
  cert Secret.

## Audit logging (pgaudit)

Opt-in, [pgaudit](https://github.com/pgaudit/pgaudit)-based audit logging for compliance
regimes (SOC 2, HIPAA, PCI-DSS, ISO 27001) that need a per-object record of *who did what*.
Off by default — with `audit.enabled: false` the rendered manifests are unchanged.

```yaml
postgresql:
  audit:
    enabled: true
    log: "ddl, role, write"   # pgaudit session classes; see below
    logCatalog: false
    logParameter: false        # parameters can contain PII/secrets — enable deliberately
    logRelation: false
    role: ""                   # optional pgaudit.role for object-level auditing
```

**What the chart does when enabled:** adds `pgaudit` to `shared_preload_libraries`
(preserving `repmgr` for the 1.x repmgr mechanism — native has no repmgr
extension to preserve (#293) — and any libraries you set in `postgresql.configuration` —
they are merged), renders the `pgaudit.*` GUCs into the postgresql ConfigMap, and
creates the extension idempotently on the primary via a post-install/upgrade hook Job.

- **Requires `ha.enabled: true`.** Audit logging needs the `cagriekin/repmgr` image,
  which bundles the `pgaudit` extension. Standalone mode (`ha.enabled: false`) uses the
  stock `postgres` image, which has no pgaudit, so a bare `shared_preload_libraries=pgaudit`
  would crash-loop the postmaster. The chart **fails fast** at render time in that case; to
  audit in standalone mode, supply a `postgresql.image` that ships pgaudit.
- **Enabling audit restarts PostgreSQL.** `shared_preload_libraries` is a postmaster
  parameter, so it only takes effect after a restart. Toggling `audit.enabled` changes the
  ConfigMap checksum, which the StatefulSet's `checksum/postgresql-config` annotation turns
  into a controlled rolling restart — no manual step needed.
- **Log classes** (`log`): comma-separated `read`, `write`, `function`, `role`, `ddl`,
  `misc`, `misc_set`, `all`. Prefix a class with `-` to subtract from `all`
  (e.g. `all, -misc`). Validated against this allowlist at render time.
- **Object-level auditing** (`role`): set `pgaudit.role` to an existing role; any object it
  is `GRANT`ed on is audited regardless of the class list. Leave empty for session-only
  logging. To audit objects in databases other than the primary, add `pgaudit` to that
  database's `postgresql.databases[].extensions`.
- **Where the records go, and retention.** pgaudit writes to the PostgreSQL server log
  (stderr → `kubectl logs`). The chart emits the records; **tamper-evident retention is your
  platform's job** — ship the pod logs to Loki / ELK / CloudWatch with an
  append-only/immutable retention policy. Runtime `ALTER SYSTEM` / session GUC changes are
  **not** persisted; this declarative config is the source of truth.

## Replication Management

The agent manages replication automatically. There is no `repmgr` CLI to consult any more
(#290) — inspect the cluster through PostgreSQL itself and the Lease.

Who is primary, authoritatively:

```bash
kubectl get lease my-postgres-pg-leader -o jsonpath='{.spec.holderIdentity}'
```

Who is streaming, from the primary's own connection list:

```bash
kubectl exec -it my-postgres-pg-0 -- psql -U repmgr -d repmgr \
  -c "SELECT application_name, state, sync_state, replay_lag FROM pg_stat_replication"
```

Replication slots and the WAL each is holding:

```bash
kubectl exec -it my-postgres-pg-0 -- psql -U repmgr -d repmgr \
  -c "SELECT slot_name, active, wal_status, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained FROM pg_replication_slots"
```

The agent also exports this as metrics — `pg_ha_agent_replicas_streaming`,
`pg_ha_agent_replicas_expected`, `pg_ha_agent_replication_slots` and the slot-WAL gauges — with
shipped PrometheusRules, which is the better place to watch it from than a shell.

### Scaling down

Scaling `postgresql.replicaCount` **down** removes the highest-ordinal pods. There is no
`repmgr.nodes` registry to strand rows in; what a scale-down leaves behind is a **replication
slot**, and the primary reclaims the departed ordinal's slot once its pod is gone (#289). The
discriminator is the ordinal, never reachability, so a momentarily-down *live* node never has its
slot dropped. Verify with the slot query above — no slot should remain for a removed ordinal.

> The removed pods' PVCs are retained (StatefulSet does not delete them); reclaim them manually
> if desired. If the node being removed is the *current primary* (possible when a prior failover
> left the primary on a high ordinal), the StatefulSet still removes it and the remaining nodes
> elect a new primary through the Lease — no manual unregistration step exists or is needed.

## PGPool-II Connection Pooling and Load Balancing

When PGPool-II is enabled, it provides connection pooling and load balancing:

**Load Balancing:**
- PGPool-II distributes SELECT queries across primary and replica nodes
- Write operations (INSERT, UPDATE, DELETE, DDL) are automatically routed to the primary
- Queries within explicit transactions go to the primary to maintain consistency

**Connection Pooling:**
- Reduces connection overhead by reusing database connections
- Configurable pool size per worker process
- Connection lifetime and idle timeout controls

**High Availability:**
- Monitors backend health with periodic health checks
- Automatically detects streaming replication status
- Fails over to replicas when primary becomes unavailable

## Multi-Zone Deployment

By default the PostgreSQL StatefulSet schedules with a required pod anti-affinity rule on `kubernetes.io/hostname` (one instance per node) and a preferred rule (weight 100) on `topology.kubernetes.io/zone`, so instances spread across zones when zones are available but still schedule on single-zone clusters.

Example values for a three-zone cluster:

```yaml
postgresql:
  replicaCount: 2          # 3 instances total: 1 primary + 2 standbys, one per zone
  persistence:
    storageClass: zonal-ssd  # use a WaitForFirstConsumer storage class, see below

pgpool:
  enabled: true
  replicaCount: 3
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/name: pg
          app.kubernetes.io/instance: my-postgres
          app.kubernetes.io/component: pgpool
```

The `app.kubernetes.io/instance` label must match the Helm release name.

### Zone Anti-Affinity

The built-in zone rule is preferred, not required: if a zone is down or full, instances still schedule into the remaining zones. To make zone spread mandatory, set `postgresql.affinity`:

```yaml
postgresql:
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/name: pg
              app.kubernetes.io/instance: my-postgres
              app.kubernetes.io/component: postgresql
          topologyKey: topology.kubernetes.io/zone
```

Setting `postgresql.affinity` replaces the entire built-in affinity block, including the hostname rule. With a required zone rule that is harmless (distinct zones imply distinct nodes), but any other custom affinity should re-add the hostname rule explicitly. A required zone rule also caps the cluster size: total instances (`replicaCount + 1`) must not exceed the number of zones or the surplus pods stay Pending.

`postgresql.topologySpreadConstraints` (default `[]`) adds spread constraints alongside the built-in affinity without replacing it — set a hard zone spread (`whenUnsatisfiable: DoNotSchedule`) here to keep the hostname anti-affinity intact. PGPool-II supports `pgpool.topologySpreadConstraints` (as in the example above) and, like PostgreSQL, has a default hostname anti-affinity that `pgpool.affinity` replaces wholesale.

### Cloud preset (`values-cloud.yaml`)

The chart ships an opt-in `values-cloud.yaml` overlay with opinionated multi-AZ production settings, so you do not have to assemble them by hand:

```bash
helm install my-pg cagriekin/pg -f values-cloud.yaml [-f your-values.yaml]
```

It sets `replicaCount: 2` (3 instances), a hard `DoNotSchedule` zone spread, a `WaitForFirstConsumer` `storageClass` placeholder, and the managed-cloud agent lease timings (`30s`/`20s`/`4s`). Do not use it on single-zone / kind / dev clusters — the hard spread leaves pods Pending when there are fewer schedulable zones than replicas. The base `values.yaml` stays dev/CI-friendly; this preset is the production opt-in.

### Storage Classes

Use a storage class with `volumeBindingMode: WaitForFirstConsumer`. It delays PV provisioning until the pod is scheduled, so each volume is created in the zone the scheduler picked. With `Immediate` binding the PV is provisioned first, in an arbitrary zone, and the pod may become unschedulable when that zone conflicts with the affinity rules.

Cloud block volumes (EBS, GCE PD, Azure Disk) are zonal, which pins each instance to its volume's zone permanently:

- After a zone outage, pods from that zone cannot reschedule elsewhere; availability relies on repmgr promoting a standby in a surviving zone.
- Deleting a pod never moves it to another zone. To relocate a standby, delete its PVC together with the pod; the recreated pod provisions a new volume in its new zone and re-clones from the primary.

### Routing Reads Across Zones

With repmgr enabled, the `<fullname>-readonly` service (see [Read-Only Connection to Replicas](#read-only-connection-to-replicas)) selects all standby pods, so read traffic is distributed across the standbys in every zone. Cross-zone traffic charges apply unless topology-aware routing is configured cluster-side; the chart does not set topology annotations on the service.

## Prometheus Exporter

This chart includes an optional PostgreSQL metrics exporter for Prometheus monitoring. The exporter runs as a single instance and can scrape metrics from all PostgreSQL instances (primary and replicas) using the multi-target pattern.

### Enable Exporter

```bash
helm install my-postgres cagriekin/pg \
  --set prometheusExporter.enabled=true \
  --set postgresql.replicaCount=3
```

### With ServiceMonitor (Prometheus Operator)

```bash
helm install my-postgres cagriekin/pg \
  --set prometheusExporter.enabled=true \
  --set prometheusExporter.serviceMonitor.enabled=true \
  --set postgresql.replicaCount=3
```

The ServiceMonitor automatically configures Prometheus to scrape all PostgreSQL instances through the exporter using the `/probe` endpoint.

### Manual Prometheus Configuration

If not using Prometheus Operator, add this to your Prometheus `scrape_configs`:

```yaml
scrape_configs:
  - job_name: 'postgres'
    static_configs:
      - targets:
        - my-postgres-pg-0.my-postgres-pg-headless.default.svc.cluster.local:5432
        - my-postgres-pg-1.my-postgres-pg-headless.default.svc.cluster.local:5432
        - my-postgres-pg-2.my-postgres-pg-headless.default.svc.cluster.local:5432
    metrics_path: /probe
    params:
      auth_module: [postgres]
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: my-postgres-pg-postgres-exporter.default.svc.cluster.local:9116
```

### Replication Metrics

The exporter's built-in replication collector provides `pg_replication_lag_seconds` (seconds since the last replayed transaction on a standby) and `pg_replication_is_replica`. The chart adds a custom `pg_wal_replication` query group evaluated on every instance (primary and standbys):

| Metric | Description |
|--------|-------------|
| `pg_wal_replication_in_recovery` | `1` when the instance is in recovery (standby), `0` when it is a primary |
| `pg_wal_replication_receive_replay_lag_bytes` | Bytes of WAL received from the primary but not yet replayed; `0` on the primary |

Alert on `pg_replication_lag_seconds` to catch a standby falling behind, and on `sum(pg_wal_replication_in_recovery == 0) > 1` across the instances of one release to detect split-brain (two primaries). Note that `pg_replication_lag_seconds` also grows on a healthy standby while the primary is idle (no transactions to replay), so combine it with `pg_wal_replication_receive_replay_lag_bytes` when tuning alert thresholds.

### WAL Archiving Metrics

When pgBackRest is enabled, the exporter adds a `pg_wal_archive` query group from `pg_stat_archiver`, evaluated **only on the primary** (archiving happens there; a standby emits no `pg_wal_archive_*` series):

| Metric | Description |
|--------|-------------|
| `pg_wal_archive_archived_count` | WAL segments successfully archived since the last stats reset |
| `pg_wal_archive_failed_count` | Failed WAL archive attempts (`archive_command` failures) since the last stats reset |
| `pg_wal_archive_seconds_since_last_archived` | Seconds since the last successful WAL archive (`-1` if none yet) |
| `pg_wal_archive_seconds_since_last_failed` | Seconds since the last failed WAL archive attempt (`-1` if none) |

Alert on `rate(pg_wal_archive_failed_count[5m]) > 0` to catch a failing `archive_command` — this is the actionable signal. `pg_wal_archive_seconds_since_last_archived` also grows on an idle primary that has no WAL to archive, so alert on it only in conjunction with a WAL-generation signal (e.g. it rising while `pg_wal_archive_archived_count` stays flat), not on its own.

### WAL Disk Usage (#305)

Unlike the counters above, `pg_wal_size_bytes` reflects actual bytes on disk regardless of *why* WAL is being retained — a stuck `archive_command`, a lagging replication slot, or a slow standby. It comes from the exporter's own **built-in** `wal` collector (enabled by default in `quay.io/prometheuscommunity/postgres-exporter`, not a chart-defined query), so it is emitted on every instance without any chart configuration, regardless of `pgbackrest.enabled`:

| Metric | Description |
|--------|-------------|
| `pg_wal_size_bytes` | Total bytes currently used by `pg_wal` on disk |
| `pg_wal_segments` | Number of WAL segments currently in `pg_wal` |

> **Why isn't this a chart-defined query group like the others?** It was, briefly — and it broke the exporter. A custom query naming its metric `pg_wal_size_bytes` collided with the exporter's built-in metric of the exact same name but different help text; Prometheus client libraries reject that as a duplicate-metric registration, which fails the **entire** `/metrics` scrape, not just the new metric. Confirmed live against the shipped `v0.19.1` image before this was caught. The fix was to delete the chart-defined query and alert on the metric the exporter already produces.

`pg_wal` shares the single PGDATA volume (`postgresql.persistence.size`) — there is no separate WAL volume/tablespace. If `archive_command` gets stuck (repository unreachable or full), PostgreSQL correctly refuses to recycle un-archived WAL, and `pg_wal_size_bytes` will climb without bound until the volume fills and the instance stops accepting writes. **This chart ships observability for that condition only** — a shipped alert (below) so you can page and act manually — not an automatic write-throttle or backpressure mechanism. Bounding the *action*, not just visibility into the problem, is tracked as a follow-up.

### WAL Alert Rules

Set `prometheusExporter.prometheusRule.enabled: true` to ship a `PrometheusRule` (requires the Prometheus Operator CRDs) wiring the metrics above to alerts. **This is a no-op unless something actually scrapes the exporter** — the rule alerts on metrics the exporter emits, so it does nothing on its own; enable `prometheusExporter.serviceMonitor.enabled` too (or point your own scrape config at it) or the alerts will simply never fire.

| Alert | Condition | Requires |
|-------|-----------|----------|
| `PGWALArchiveFailing` | `rate(pg_wal_archive_failed_count[5m]) > 0` for `archiveFailingFor` (default `5m`) | `pgbackrest.enabled` |
| `PGWALArchiveStale` | `pg_wal_archive_seconds_since_last_archived > staleArchiveSeconds` for `archiveStaleFor` (default `15m`) | `pgbackrest.enabled` |
| `PGWALSizeHigh` | `pg_wal_size_bytes > walSizeBytesThreshold` for `sizeHighFor` (default `15m`) | — |

`PGWALArchiveFailing` is the most actionable of the three — it fires as soon as `archive-push` starts failing, before any WAL has had time to accumulate. `PGWALArchiveStale` and `PGWALSizeHigh` are backstops with the same idle-primary caveat as the metrics they read; tune `staleArchiveSeconds`/`walSizeBytesThreshold` to your WAL generation rate and `postgresql.persistence.size` headroom, and tighten the `*For` durations (Prometheus duration syntax) if that volume is small enough that 15m is too long to wait before paging.

### Exporter Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `prometheusExporter.enabled` | Enable Prometheus exporter | `false` |
| `prometheusExporter.monitoringUser.enabled` | Create a read-only `pg_monitor` role (via a post-install hook Job) and have the exporter authenticate as it instead of the postgres superuser (#28) | `true` |
| `prometheusExporter.monitoringUser.username` | Name of the monitoring role | `monitoring` |

> **Monitoring-user notes.** The `pg_monitor` role is created by a post-install/upgrade
> hook Job, so on a *fresh* install the exporter may log auth failures for a few seconds
> until the hook completes — it recovers on its own. If you rotate the
> `monitoring-password` in an `existingSecret`, the running exporter keeps the old value
> until restarted (env is read at pod start): run
> `kubectl rollout restart deployment/<release>-postgres-exporter` after the upgrade
> (same as the pgpool credential note below).
| `prometheusExporter.image.repository` | Exporter image repository | `quay.io/prometheuscommunity/postgres-exporter` |
| `prometheusExporter.image.tag` | Exporter image tag | `v0.19.1` |
| `prometheusExporter.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `prometheusExporter.podSecurityContext` | Pod-level securityContext for exporter | `{runAsNonRoot: true, seccompProfile.type: RuntimeDefault}` |
| `prometheusExporter.containerSecurityContext` | Container-level securityContext for exporter containers | `{runAsUser: 65534, runAsGroup: 65534, allowPrivilegeEscalation: false, capabilities.drop: [ALL]}` |
| `prometheusExporter.podAnnotations` | Annotations for exporter pods | `{}` |
| `prometheusExporter.priorityClassName` | priorityClassName for exporter pods | `""` |
| `prometheusExporter.service.type` | Exporter service type | `ClusterIP` |
| `prometheusExporter.service.port` | Exporter service port | `9116` |
| `prometheusExporter.service.annotations` | Exporter service annotations | `{}` |
| `prometheusExporter.resources.requests.cpu` | CPU request | `50m` |
| `prometheusExporter.resources.requests.memory` | Memory request | `64Mi` |
| `prometheusExporter.resources.limits.cpu` | CPU limit | `200m` |
| `prometheusExporter.resources.limits.memory` | Memory limit | `128Mi` |
| `prometheusExporter.serviceMonitor.enabled` | Create ServiceMonitor | `false` |
| `prometheusExporter.serviceMonitor.interval` | Scrape interval | `30s` |
| `prometheusExporter.serviceMonitor.scrapeTimeout` | Scrape timeout | `10s` |
| `prometheusExporter.serviceMonitor.additionalLabels` | Additional labels on ServiceMonitor | `{}` |
| `prometheusExporter.prometheusRule.enabled` | Ship the `PrometheusRule` covering the WAL alerts above (#305) | `false` |
| `prometheusExporter.prometheusRule.additionalLabels` | Additional labels on PrometheusRule | `{}` |
| `prometheusExporter.prometheusRule.staleArchiveSeconds` | `PGWALArchiveStale` threshold, in seconds | `900` |
| `prometheusExporter.prometheusRule.walSizeBytesThreshold` | `PGWALSizeHigh` threshold, in bytes | `5368709120` (5Gi) |
| `prometheusExporter.prometheusRule.archiveFailingFor` | `PGWALArchiveFailing` `for` duration | `5m` |
| `prometheusExporter.prometheusRule.archiveStaleFor` | `PGWALArchiveStale` `for` duration | `15m` |
| `prometheusExporter.prometheusRule.sizeHighFor` | `PGWALSizeHigh` `for` duration | `15m` |

## Backup

Automated database backups can be enabled to run `pg_dump` on a schedule and upload compressed dumps to S3-compatible storage (AWS S3, MinIO, Wasabi, etc.). The backup job connects to the primary via the main service, so it works correctly with repmgr failover. After upload, the backup is verified by downloading and running `pg_restore --list` to confirm it is a valid custom-format dump.

### Enable Backup

```bash
kubectl create secret generic s3-backup-creds \
  --from-literal=access-key-id=YOUR_ACCESS_KEY \
  --from-literal=secret-access-key=YOUR_SECRET_KEY

helm install my-postgres cagriekin/pg \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://minio.example.com \
  --set backup.s3.bucket=pg-backups \
  --set backup.existingSecret.name=s3-backup-creds
```

### Manual Trigger

```bash
kubectl create job --from=cronjob/my-postgres-pg-backup manual-backup
```

### Backup Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `backup.enabled` | Enable backup CronJob | `false` |
| `backup.schedule` | Cron schedule | `0 2 * * *` |
| `backup.s3.endpoint` | S3-compatible endpoint URL | `""` |
| `backup.s3.bucket` | S3 bucket name | `""` |
| `backup.s3.prefix` | Key prefix within bucket. Dumps are stored under `<prefix>/<release-fullname>/`, so multiple releases can safely share one bucket/prefix (retention only ever touches a release's own subpath). | `backups` |
| `backup.existingSecret.name` | Secret containing S3 credentials | `""` |
| `backup.existingSecret.accessKeyIdKey` | Key for access key ID in secret | `access-key-id` |
| `backup.existingSecret.secretAccessKeyKey` | Key for secret access key in secret | `secret-access-key` |
| `backup.mc.image.repository` | MinIO client image for the mc-installer init container | `minio/mc` |
| `backup.mc.image.tag` | MinIO client image tag | `RELEASE.2024-11-21T17-21-54Z` |
| `backup.mc.image.pullPolicy` | MinIO client image pull policy | `IfNotPresent` |
| `backup.podSecurityContext` | Backup pod security context | `runAsNonRoot: true`, `seccompProfile: RuntimeDefault` |
| `backup.containerSecurityContext` | Backup container security context | `runAsUser: 999`, `runAsGroup: 999`, no privilege escalation, all capabilities dropped |
| `backup.activeDeadlineSeconds` | Job timeout in seconds | `3600` |
| `backup.backoffLimit` | Number of retries before marking job as failed | `1` |
| `backup.retentionDays` | Days to retain backups before cleanup | `7` |
| `backup.priorityClassName` | priorityClassName for backup job pods | `""` |
| `backup.resources.requests.cpu` | CPU request | `100m` |
| `backup.resources.requests.memory` | Memory request | `256Mi` |
| `backup.resources.limits.cpu` | CPU limit | `500m` |
| `backup.resources.limits.memory` | Memory limit | `512Mi` |
| `backup.validation.enabled` | Enable the weekly restore-validation CronJob (restores the latest dump into a throwaway PostgreSQL in the Job pod and fails on a bad restore) | `false` |
| `backup.validation.schedule` | Cron schedule for the validation job | `` `0 3 * * 0` `` |
| `backup.validation.workdirSizeLimit` | `sizeLimit` for the throwaway PGDATA + downloaded-dump emptyDir; must exceed the restored DB size; empty = unbounded | `""` |
| `backup.validation.resources.requests.cpu` | Validation job CPU request | `200m` |
| `backup.validation.resources.requests.memory` | Validation job memory request | `256Mi` |
| `backup.validation.resources.limits.cpu` | Validation job CPU limit | `1` |
| `backup.validation.resources.limits.memory` | Validation job memory limit | `1Gi` |

### Restore

Dumps are namespaced per release under `<prefix>/<release-fullname>/`. List the
release's own backups, then restore the chosen one (replace `<release>-pg` with
your release's fullname):

```bash
mc ls s3/pg-backups/backups/<release>-pg/
mc cp s3/pg-backups/backups/<release>-pg/backup_20250101_020000.dump /tmp/backup.dump
pg_restore -h localhost -U postgres -d postgres /tmp/backup.dump
```

Dumps taken before the per-release-path change live at the **old flat path**
`s3/<bucket>/<prefix>/backup_*.dump` (no `<release-fullname>/` segment). They are not
migrated and are no longer covered by automatic retention, so list and restore them
directly there (`mc ls s3/pg-backups/backups/`), and delete them manually once obsolete.

## pgBackRest (PITR)

pgBackRest provides WAL-based incremental backups for point-in-time recovery. When enabled, WAL segments are continuously archived from the primary to S3, and scheduled full/differential backups run automatically. This allows restoring the database to any point in time within the retention window.

Requires `ha.enabled: true` (pgBackRest is installed in the repmgr image).

### Enable pgBackRest

```bash
kubectl create secret generic s3-backup-creds \
  --from-literal=access-key-id=YOUR_ACCESS_KEY \
  --from-literal=secret-access-key=YOUR_SECRET_KEY

helm install my-postgres cagriekin/pg \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.eu-central-1.amazonaws.com \
  --set pgbackrest.s3.bucket=pg-backups \
  --set pgbackrest.s3.region=eu-central-1 \
  --set pgbackrest.existingSecret.name=s3-backup-creds
```

### How It Works

- **WAL archiving**: The primary continuously archives WAL segments to S3 via `archive_command`. Standbys do not archive (PostgreSQL default with `archive_mode = on`).
- **Full backups**: Weekly (default Sunday 1am) via a CronJob that execs into the pgbackrest sidecar on the current primary.
- **Differential backups**: Daily (default Mon-Sat 1am) via a separate CronJob. Only changed blocks since the last full backup are stored.
- **Failover**: After repmgr promotes a standby, the new primary starts archiving WAL and running backups automatically.
- **Verification**: After each backup, `pgbackrest info` confirms the backup was recorded in the repository.

### pgBackRest Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgbackrest.enabled` | Enable pgBackRest | `false` |
| `pgbackrest.stanza` | pgBackRest stanza name | `db` |
| `pgbackrest.extraEnv` | Extra env vars for **every** pgBackRest container — the sidecar and `pgbackrest-bootstrap` init container in the postgresql pod, the backup CronJobs, the restore workload, the validation CronJob (supports `value` and `valueFrom`); may not reuse a chart-set name ([#323](#routing-the-backup-cronjobs-apiserver-traffic--pgbackrestextraenv-323)) | `[]` |
| `pgbackrest.extraVolumes` | Extra pod-level volumes on each of those pods; names may not collide with a chart volume in any of them, nor with `postgresql.extraVolumes` | `[]` |
| `pgbackrest.extraVolumeMounts` | Extra mounts on each of those containers; each must reference a `pgbackrest.extraVolumes` entry and may not shadow PGDATA, `/etc/pgbackrest/pgbackrest.conf`, `/scripts`, `/work` or `/tmp` | `[]` |
| `pgbackrest.s3.endpoint` | S3-compatible endpoint URL | `""` |
| `pgbackrest.s3.bucket` | S3 bucket name | `""` |
| `pgbackrest.s3.region` | S3 region | `us-east-1` |
| `pgbackrest.s3.prefix` | Key prefix within bucket | `pgbackrest` |
| `pgbackrest.s3.keyType` | `shared` (static keys from `existingSecret`) or `auto` (cloud workload identity) | `shared` |
| `pgbackrest.existingSecret.name` | Secret containing S3 credentials (required when `keyType: shared`) | `""` |
| `pgbackrest.existingSecret.accessKeyIdKey` | Key for access key ID in secret | `access-key-id` |
| `pgbackrest.existingSecret.secretAccessKeyKey` | Key for secret access key in secret | `secret-access-key` |
| `pgbackrest.repoEncryption.enabled` | Encrypt the pgBackRest repository at rest in S3 (`repo1-cipher-type`). Passphrase via `PGBACKREST_REPO1_CIPHER_PASS` env, never the ConfigMap. Fixed for the repo's life. | `false` |
| `pgbackrest.repoEncryption.cipherType` | Cipher when encryption is enabled | `aes-256-cbc` |
| `pgbackrest.repoEncryption.existingSecret.name` | Secret holding the repository passphrase (required when encryption is enabled) | `""` |
| `pgbackrest.repoEncryption.existingSecret.passphraseKey` | Key for the passphrase in that secret | `cipher-pass` |
| `pgbackrest.retention.full` | Number of full backups to retain | `4` |
| `pgbackrest.retention.diff` | Number of differential backups to retain | `14` |
| `pgbackrest.schedule.full` | Cron schedule for full backups | `0 1 * * 0` |
| `pgbackrest.schedule.diff` | Cron schedule for differential backups | `0 1 * * 1-6` |
| `pgbackrest.resources.requests.cpu` | Sidecar CPU request | `100m` |
| `pgbackrest.resources.requests.memory` | Sidecar memory request | `256Mi` |
| `pgbackrest.resources.limits.cpu` | Sidecar CPU limit | `1000m` |
| `pgbackrest.resources.limits.memory` | Sidecar memory limit | `1Gi` |
| `pgbackrest.cronjob.image.repository` | CronJob image | `alpine/k8s` |
| `pgbackrest.cronjob.image.tag` | CronJob image tag | `1.31.3` |
| `pgbackrest.cronjob.concurrencyPolicy` | CronJob concurrency policy | `Forbid` |
| `pgbackrest.cronjob.backoffLimit` | Job backoff limit | `0` |
| `pgbackrest.cronjob.activeDeadlineSeconds` | Job timeout | `21600` |
| `pgbackrest.cronjob.successfulJobsHistoryLimit` | Successful job history limit | `3` |
| `pgbackrest.cronjob.failedJobsHistoryLimit` | Failed job history limit | `3` |
| `pgbackrest.cronjob.priorityClassName` | priorityClassName for pgBackRest job pods | `""` |
| `pgbackrest.cronjob.resources.requests.cpu` | CronJob CPU request | `50m` |
| `pgbackrest.cronjob.resources.requests.memory` | CronJob memory request | `64Mi` |
| `pgbackrest.cronjob.resources.limits.cpu` | CronJob CPU limit | `200m` |
| `pgbackrest.cronjob.resources.limits.memory` | CronJob memory limit | `128Mi` |
| `pgbackrest.cronjob.podSecurityContext` | Pod securityContext for the pgBackRest CronJob | `runAsNonRoot: true`, `runAsUser: 65534`, `seccompProfile: RuntimeDefault` |
| `pgbackrest.cronjob.containerSecurityContext` | Container securityContext for the pgBackRest CronJob | `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]` |
| `pgbackrest.validation.enabled` | Enable the automated PITR restore-validation CronJob (#38) — restores the repo into a throwaway PostgreSQL, replays WAL, validates, exits | `false` |
| `pgbackrest.validation.schedule` | Cron schedule for the validation job | `` `0 4 * * 0` `` |
| `pgbackrest.validation.targetType` | PITR target type (`pgbackrest --type`): `""` (latest) \| `time` \| `xid` \| `name` \| `lsn`. `target` is required when set | `""` |
| `pgbackrest.validation.target` | PITR target value for the type above (e.g. `2026-06-26 03:00:00+00`) | `""` |
| `pgbackrest.validation.recoveryTimeout` | Seconds `pg_ctl` waits for WAL replay + promotion before failing the Job | `1800` |
| `pgbackrest.validation.workdirSizeLimit` | `sizeLimit` for the throwaway restored-PGDATA emptyDir; must exceed the DB size; empty = node-disk bounded | `""` |
| `pgbackrest.validation.activeDeadlineSeconds` | Validation Job timeout | `3600` |
| `pgbackrest.validation.backoffLimit` | Validation Job backoff limit | `1` |
| `pgbackrest.validation.resources.*` | Validation Job resource requests/limits | `200m`/`256Mi` … `1`/`1Gi` |
| `pgbackrest.restore.enabled` | Render the first-class PITR restore resource (#226) — restores over the **live** data PVC. Inert until you clone/apply it | `false` |
| `pgbackrest.restore.mode` | `cronjob` (suspended CronJob, clone with `kubectl create job --from`) \| `job` (bare Job, render with `helm template -s`) | `cronjob` |
| `pgbackrest.restore.targetType` | PITR target type (`pgbackrest --type`): `""` (latest) \| `time` \| `xid` \| `name` \| `lsn`. `target` is required when set | `""` |
| `pgbackrest.restore.target` | PITR target value for the type above (e.g. `2026-03-22 12:00:00+00`); implies `--target-action=promote` | `""` |
| `pgbackrest.restore.backupSet` | `pgbackrest --set`: restore a specific backup set label from `pgbackrest info` instead of the latest | `""` |
| `pgbackrest.restore.force` | `pgbackrest --force`: restore despite a **stale** `postmaster.pid` left by a crash. Leave `false` unless every postgresql pod is confirmed gone | `false` |
| `pgbackrest.restore.podOrdinal` | Ordinal of the replica PVC (`data-<fullname>-<n>`) restored into; leave `0` | `0` |
| `pgbackrest.restore.nameSuffix` | `mode: job` only — appended to the Job name so a retry does not collide with the completed Job | `""` |
| `pgbackrest.restore.activeDeadlineSeconds` | Restore Job timeout | `21600` |
| `pgbackrest.restore.backoffLimit` | Restore Job backoff limit (0 = one attempt; a half-written PGDATA should not be retried automatically) | `0` |
| `pgbackrest.restore.resources.*` | Restore Job resource requests/limits | `200m`/`256Mi` … `1`/`1Gi` |
| _scheduling_ | The restore pod inherits `postgresql.nodeSelector` and `postgresql.tolerations` so it lands where the data PVC can attach (`postgresql.affinity` is not inherited) | — |
| `pgbackrest.bootstrap.enabled` | Seed an **empty** PGDATA on replica 0 from this release's repo via an init container (#266) — a lost PVC recovers instead of initdb-ing an empty cluster | `false` |
| `pgbackrest.bootstrap.targetType` | PITR target type for the bootstrap: `""` (latest) \| `time` \| `xid` \| `name` \| `lsn`. `target` required when set. Applies to **every** fresh bootstrap, not once | `""` |
| `pgbackrest.bootstrap.target` | PITR target value for the type above | `""` |
| `pgbackrest.bootstrap.backupSet` | `pgbackrest --set`: bootstrap from a specific backup set label instead of the latest | `""` |
| `pgbackrest.bootstrap.resources.*` | Bootstrap init-container resource requests/limits | `200m`/`256Mi` … `1`/`1Gi` |

### Bootstrap from backup: automatic recovery from a lost PVC (#266)

Without this, losing replica 0's PVC is quietly the worst kind of outage. The pod comes back,
the entrypoint finds an empty data directory, and it **`initdb`s a brand new empty cluster** —
your backups are safe in S3, but the live database is empty and nothing has failed loudly.

`pgbackrest.bootstrap.enabled=true` adds an init container that runs before `repmgr-init` and
seeds an *empty* PGDATA from this release's own repository. PostgreSQL then replays the archived
WAL on startup and promotes, so a lost volume self-heals with no operator action:

```yaml
pgbackrest:
  enabled: true
  bootstrap:
    enabled: true
```

**It is safe to leave enabled.** It only ever writes into an *empty* data directory:

| Data directory state | What the bootstrap does |
|---|---|
| Empty, repository has backups | Restores, writes a completion marker, lets postgres replay WAL |
| Empty, repository has **no** backup yet | Nothing — a normal first install proceeds and `initdb` runs |
| Empty, repository **unreachable** | **Fails loudly** and the pod stays in `Init` |
| Already initialized | Nothing — refuses to touch it, whatever the marker says |
| Partially restored (aborted attempt) | Resumes the restore (`--delta`) |
| Any replica other than 0 | Nothing — standbys are cloned from the primary by repmgr |

That third row is deliberate: if the repository cannot be reached, the state of the backups is
*unknown*, and initializing an empty cluster then would destroy a database that is probably
still recoverable. Failing to start is the safe outcome. (Reached only when PGDATA is empty, so
a pod rescheduled with an intact volume never depends on S3 being up.)

The completion marker lives at `$PGDATA/.pgbackrest-bootstrap-complete` and records the stanza,
backup set, target and system identifier used. Because it lives inside PGDATA it shares the
volume's lifecycle — losing the volume clears it exactly when a fresh bootstrap becomes correct
again, with no external state to drift.

This is the automatic counterpart to the restore Job above; they are orthogonal and can both be
enabled:

| | `bootstrap` | `restore` |
|---|---|---|
| Writes into | An **empty** PGDATA only | A **live** PGDATA (destructive) |
| Triggered by | Pod startup, automatically | An operator (`kubectl create job`) |
| Use it for | A lost or replaced PVC | Rolling the database back to a point in time |

Setting `bootstrap.targetType`/`target` pins the bootstrap to a point in time — but note it
applies to **every** fresh bootstrap of this release, not just the next one, so leave both empty
unless you really want every rebuilt replica 0 to land on that same target.

Verified end to end by `make -C pg test-pgbackrest-bootstrap`, which deletes replica 0's PVC
outright and asserts the *same* cluster returns — the PostgreSQL system identifier is unchanged,
which a fresh `initdb` could not produce — then restarts the pod and asserts the bootstrap does
**not** run a second time.

### Automated PITR restore-validation (#38)

A backup you have never restored is a hope, not a backup. Set `pgbackrest.validation.enabled=true` to add a CronJob that, on a schedule (default weekly, `0 4 * * 0`), **continuously proves the repository is restorable**:

1. Restores the pgBackRest repository into a **throwaway PostgreSQL inside the Job pod** — `PGBACKREST_PG1_PATH` is overridden to an `emptyDir`, so the live data directory is never touched.
2. Starts that instance so it replays the archived WAL (`restore_command` → `pgbackrest archive-get`) and promotes to read-write — failing the Job if recovery does not complete within `recoveryTimeout`.
3. Runs a sanity query (confirms `pg_is_in_recovery()` is false and counts relations), then exits. The `emptyDir` is discarded with the pod.

It is read-only against S3 and fully decoupled from the live cluster. By default it restores the **latest** backup set and replays all WAL; set `validation.targetType`/`validation.target` to validate recovery to a specific point in time instead. The Job runs from the repmgr image (it ships `pgbackrest` and the matching PostgreSQL major) under the postgresql pods' ServiceAccount — so `s3.keyType: auto` works with no extra setup — with its API token unmounted.

This complements `backup.validation` (which restore-tests the legacy `pg_dump` path): this one exercises the pgBackRest repository **and** the WAL archive, i.e. the actual PITR mechanism. The `make -C pg test-pgbackrest-restore` integration test drives the whole restore + WAL-replay path against an in-cluster MinIO.

> Failures surface as a failed Job. Alert on it via `kube_job_failed{job_name=~".*-pgbackrest-validation.*"}` (kube-state-metrics) or a CronJob-failure alert, the same way you would the backup jobs.

### Keyless backups (cloud workload identity)

Instead of static S3 keys, set `pgbackrest.s3.keyType: auto` to use the cloud credential chain (AWS IRSA, GKE Workload Identity, or an EC2 instance profile). No `existingSecret` is needed; annotate the postgresql pods' ServiceAccount (`<fullname>-repmgr`, the identity the pgbackrest sidecar runs under) via `postgresql.serviceAccount.annotations`:

```yaml
pgbackrest:
  enabled: true
  s3:
    keyType: auto            # use the cloud credential chain, not static keys
    endpoint: https://s3.<region>.amazonaws.com
    bucket: my-backups

postgresql:
  serviceAccount:
    annotations:
      # EKS (IRSA):
      eks.amazonaws.com/role-arn: arn:aws:iam::<account>:role/<role>
      # GKE (Workload Identity):
      # iam.gke.io/gcp-service-account: <gsa>@<project>.iam.gserviceaccount.com
```

### Check Backup Status

```bash
kubectl exec -it my-postgres-pg-0 -- pgbackrest --stanza=db info
```

### Point-in-Time Recovery

Set `pgbackrest.restore.enabled=true` and the chart renders a ready-made restore resource
(#226) carrying everything a restore needs — the `-repmgr` ServiceAccount (so
`s3.keyType: auto` works, with its API token unmounted), the postgresql security contexts,
the `data-<fullname>-0` PVC and `<fullname>-pgbackrest` ConfigMap mounts, the S3 /
repo-encryption credentials, and `pgbackrest restore --delta`. Nothing to hand-build.

Enabling it is safe: what it renders is **inert** — by default a *suspended* CronJob that
can never fire. It restores only when you clone it. Leave it enabled so a disaster does not
also require a `helm upgrade`.

The restore never starts PostgreSQL. Recovery (WAL replay + promotion) happens when you
scale the StatefulSet back up, under the normal chart entrypoint — so the destructive
scale down/up stays an explicit, manual decision:

```bash
# 1. Stop the cluster. pgbackrest refuses to restore while postmaster.pid exists, so this
#    is not optional -- wait for the pods to actually terminate.
kubectl scale statefulset my-postgres-pg --replicas=0
kubectl wait --for=delete pod/my-postgres-pg-0 --timeout=5m

# 2. Restore (stanza = pgbackrest.stanza, default "db").
kubectl create job --from=cronjob/my-postgres-pg-pgbackrest-restore restore-now
kubectl wait --for=condition=complete job/restore-now --timeout=30m

# 3. CONFIRM the restore succeeded -- must print 1. Do not continue otherwise.
#    (`--for=condition=complete` above only ever succeeds, so a failed attempt leaves it
#    blocked until the timeout instead of returning; if it seems stuck, check here.)
kubectl get job restore-now -o jsonpath='{.status.succeeded}{"\n"}'

# 4. Only now scale back up: the pods replay the archived WAL and promote.
kubectl scale statefulset my-postgres-pg --replicas=2
```

**Do not skip step 3.** A failed restore leaves PGDATA half-written; scaling up onto it
starts PostgreSQL against an incomplete data directory and turns a recoverable incident into
data loss. `backoffLimit` is `0` (one pod attempt), so failure is final and immediate —
diagnose with `kubectl logs job/restore-now`, fix the cause, delete the Job and create it
again. Rerunning is safe: `--delta` re-restores from scratch.

With no target set this restores the **latest** backup set and replays all archived WAL —
the disaster-recovery case. For a *point in time*, set the target first:

```yaml
pgbackrest:
  restore:
    enabled: true
    targetType: time                     # "" (latest) | time | xid | name | lsn
    target: "2026-03-22 12:00:00+00"     # required once targetType is set
    # backupSet: "20260322-010002F"      # optional: pin a specific set from `pgbackrest info`
```

A target always implies `--target-action=promote`: PostgreSQL's default
`recovery_target_action` is `pause`, which would leave the cluster in recovery, never
accepting connections.

`mode: job` renders a bare Job instead, for passing an arbitrary target inline without
touching the release (useful when the values live in Git and you cannot wait for a commit):

```bash
helm get values my-postgres > /tmp/v.yaml
helm template my-postgres cagriekin/pg -f /tmp/v.yaml \
  --set pgbackrest.restore.enabled=true --set pgbackrest.restore.mode=job \
  --set pgbackrest.restore.targetType=time \
  --set-string 'pgbackrest.restore.target=2026-03-22 12:00:00+00' \
  -s templates/pgbackrest-restore-job.yaml | kubectl apply -f -
```

Two caveats for this mode: the Job is not part of the release, so a GitOps controller may
report or prune it; and Jobs are immutable, so a second attempt needs
`--set pgbackrest.restore.nameSuffix=attempt2`.

**Standbys rebuild themselves — no extra step.** A PITR restore leaves the restored primary
on a *new* timeline, while each standby's PVC still holds pre-restore data on the old one. On
scale-up the standby's init container detects exactly that (`Timeline mismatch (local: 1,
primary: 2), re-cloning...`) and re-clones from the restored primary via `repmgr standby
clone` (`pg_basebackup`), then resumes streaming on the new timeline. No PVC deletion, no
operator action. This is verified end to end by `make -C pg test-pgbackrest-restore-ha`,
which restores a primary out from under a live standby and asserts the pair comes back
streaming.

Only if a standby *does* get stuck on the old timeline, force a fresh clone by hand. Scale it
away **before** deleting its PVC: deleting the PVC while its pod still exists leaves it
`Terminating` behind the `pvc-protection` finalizer, and the recreated pod can re-bind that
same volume — bringing the standby back on exactly the stale data you meant to discard.

```bash
kubectl scale statefulset my-postgres-pg --replicas=1              # remove the standby pod
kubectl delete pvc data-my-postgres-pg-1                          # returns once it is really gone
kubectl scale statefulset my-postgres-pg --replicas=2              # clones fresh from the primary
```

If the cluster **crashed** rather than being scaled down cleanly, a stale
`postmaster.pid` is left in PGDATA and pgbackrest refuses to restore — the interlock that
lets this Job run with no Kubernetes API access at all. Confirm every postgresql pod is
gone, then set `pgbackrest.restore.force=true`.

#### Agent mode: clear the stale leadership state before scaling up

The Lease and the highwater-marker
ConfigMap survive the scale-to-0 but now describe the *pre-restore* cluster. Before
scaling back up, delete both so the restored data re-elects cleanly:

```bash
kubectl delete lease     my-postgres-pg-leader   -n <ns> --ignore-not-found
kubectl delete configmap my-postgres-pg-primary  -n <ns> --ignore-not-found
```

Why: the marker records the highest timeline the old cluster ever reached. A PITR
restore rewinds to an earlier point on a lower timeline, so a leftover marker would
make every agent refuse to promote (the #125 stale-primary guard, working as
intended) and the cluster would never come up. Deleting the marker lets the
restored primary set a fresh highwater. The Lease is deleted so leadership is
decided by the restored set, not a holder annotation from the old generation.

If the restore produced a cluster with a **new** PostgreSQL system identifier
(e.g. restored into a different release name), the agent's cluster-identity guard
(invariant 9) will correctly refuse to clone/rejoin a leftover pod from the old
cluster — delete any such orphaned pods/PVCs rather than letting them rejoin.

### PostgreSQL major-version upgrade (agent mode)

`pg_upgrade` is a manual, primary-first operation that the agent would otherwise
fight (it sees the primary stop and would fail over). Use maintenance mode to
suspend automatic failover for the window:

```bash
# 1. Fresh backup. Pause the agent (it keeps renewing the Lease + serving, but
#    will not promote/demote/fence on its own):
kubectl annotate configmap my-postgres-pg-primary -n <ns> pg-ha/pause=true --overwrite

# 2. Perform the major upgrade primary-first per the PostgreSQL pg_upgrade
#    procedure (new-major image, pg_upgrade against the primary's PGDATA), then
#    rebuild each standby from the upgraded primary (delete its PVC + pod so the
#    agent re-clones it on the new major).

# 3. Resume automatic failover:
kubectl annotate configmap my-postgres-pg-primary -n <ns> pg-ha/pause-

# 4. Verify: kubectl get lease my-postgres-pg-leader holder == the upgraded primary;
#    a standby promotes on a test failover.
```

While paused, a genuine primary failure will NOT fail over until you resume, so
keep the window short and watch the cluster (see [Maintenance mode](#maintenance-mode-pause--agent-mode)).

### Upgrading Existing Clusters

Enabling pgBackRest on an existing cluster sets `archive_mode = on` in postgresql.conf. This change requires a PostgreSQL restart. The pods will restart automatically on the next helm upgrade since the StatefulSet spec changes, but `archive_mode` only takes effect after the restart.

## Testing

Tests require [Kind](https://kind.sigs.k8s.io/) and [Helm](https://helm.sh/) installed locally.

```bash
# Run everything (creates cluster, runs tests, deletes cluster)
make test

# Template/lint tests only (no cluster needed)
make test-template

# Create cluster, then run individual suites
make cluster-create
make test-minimal           # standalone postgres, no repmgr
make test-full              # repmgr + pgpool + prometheus exporter
make test-upgrade           # upgrade path with data persistence
make test-agent             # lease-based agent: install + failover (AGENT_COLDBOOT=1 adds cold boot)
make test-agent-etcd        # agent with the bundled etcd DCS backend
make test-scaledown         # #139 ghost-node cleanup after a replicaCount scale-down
make cluster-delete

# Run the core cluster suites in parallel
make -j4 test-cluster

# Confirm the default render has not drifted vs a baseline ref
make byte-stable REF=origin/master
```

## Failover RTO/RPO

### Recovery Time Objective (RTO)

With repmgr enabled, automatic failover completes in approximately 30-60 seconds:

1. **Detection** (~`leaseDuration`, default 15s): the dead primary stops renewing the Lease, so no challenger can acquire it until the TTL elapses.
2. **Election + promotion** (~5-10s): a standby acquires the Lease, compares timelines and LSNs across the reachable candidates, and the most-advanced one promotes.
3. **Service update** (~1 reconcile tick, default 5s): the new leader patches the write-Service selector to itself and re-labels the pods for the readonly Service.

Tightening `ha.agent.leaseDuration` shortens detection at the cost of tolerating less apiserver latency; see the agent tunables above.

The `terminationGracePeriodSeconds` (default 120s) controls the maximum time allowed for graceful failover during planned drains (e.g., node upgrades).

### Recovery Point Objective (RPO)

| Backup Method | RPO | Notes |
|---------------|-----|-------|
| Streaming replication (async) | Seconds of lag | Default. RPO depends on replication lag. Monitor with `pg_stat_replication`. |
| Streaming replication (sync) | Zero | Set `synchronous_commit = on` and configure `synchronous_standby_names` in `postgresql.configuration`. Adds write latency. |
| pgBackRest PITR | Up to last archived WAL segment | Continuous WAL archiving. RPO depends on `archive_timeout` (default 60s). |
| pg_dump S3 backup | Up to last backup interval | Default daily at 2am. Not suitable for near-zero RPO. |

## Recovery Runbooks

> Failover is driven by the Kubernetes Lease. A primary failure is handled automatically (a
> standby wins the Lease and promotes; the agent repoints the Service selector), and split-brain
> is prevented at the source — a node serves read-write only while it holds the Lease. See also
> [Maintenance mode](#maintenance-mode-pause), [Controlled switchover](#controlled-switchover),
> and [Point-in-Time Recovery](#point-in-time-recovery).

### Primary Failure (Automatic Failover)

No action required if repmgr is enabled. The sequence is:
1. The failed primary stops renewing the Lease; it expires after `leaseDuration`
2. A standby acquires the Lease and promotes (most-advanced timeline/LSN wins)
3. The new leader patches the write-Service selector and re-labels the pods

Verify with:
```bash
kubectl get lease <release>-pg-leader -n <namespace> -o jsonpath='{.spec.holderIdentity}'
kubectl exec -n <namespace> <pod> -c postgresql -- psql -U repmgr -d repmgr -c "SELECT application_name, state FROM pg_stat_replication"
```

### Rejoin Failed Primary as Standby

After a failover, the old primary must rejoin as a standby:
```bash
kubectl delete pod <old-primary-pod> -n <namespace>
```
The StatefulSet recreates the pod, and the repmgr entrypoint automatically registers it as a standby and clones from the new primary.

### Restore from pg_dump Backup

```bash
mc cp s3/<bucket>/<prefix>/backup_<timestamp>.dump /tmp/backup.dump
pg_restore -h <host> -U <user> -d <database> --clean --if-exists /tmp/backup.dump
```

### Point-in-Time Recovery (pgBackRest)

Enable `pgbackrest.restore.enabled` and use the chart's restore Job — see
[Point-in-Time Recovery](#point-in-time-recovery) above for the full runbook (scale to 0 →
`kubectl create job --from=cronjob/<fullname>-pgbackrest-restore` → scale up), the
point-in-time target values, and the agent-mode leadership cleanup.

### Split-Brain

There is no split-brain recovery runbook, because leadership is a Kubernetes Lease and two pods
cannot hold it at once. If a pod ever finds itself read-write without the Lease — the window an
asymmetric partition can open — the agent acts on `ha.splitBrainDetection.action`: `log`
records it and demotes, `fence` demotes and refuses to serve until the Lease is reacquired.
Both are local (`pg_ctl`) operations that need no manual step.

Confirm which pod legitimately holds leadership with:
```bash
kubectl get lease <release>-pg-leader -n <namespace> -o jsonpath='{.spec.holderIdentity}'
```

### Complete Cluster Rebuild

As a last resort:
```bash
kubectl scale statefulset <fullname> -n <namespace> --replicas=0
kubectl delete pvc -n <namespace> -l app.kubernetes.io/component=postgresql
kubectl scale statefulset <fullname> -n <namespace> --replicas=<count>
```
Then restore from the latest backup using one of the methods above.

## Troubleshooting

| Symptom | Likely Cause | Resolution |
|---------|-------------|------------|
| Replication lag increasing | Slow network or standby under load | Check `pg_stat_replication` on primary. Consider increasing `wal_sender_timeout`. |
| Failover not triggering | Agent paused, or no standby can acquire the Lease | Check `pg_ha_agent_is_paused` and remove the `pg-ha/pause` annotation if set. Otherwise check the agent logs (`kubectl logs <pod> -c postgresql`) and that the apiserver is reachable. |
| Service not updating after failover | Agent cannot patch the Service | Check the agent logs on the Lease holder and the Role's `services` get/patch grant. |
| PGPool returning errors after failover | Stale pooled connections to the old primary | The agent repoints the Services; pgpool has failover disabled by design. Restart it if connections are wedged: `kubectl rollout restart deployment <fullname>-pgpool` |
| WAL archiving failing (pgBackRest) | S3 credentials or connectivity | Check the `pgbackrest` sidecar logs on the primary pod and the `<fullname>-pgbackrest-full`/`-diff` CronJob pod logs. Verify S3 endpoint and credentials. |
| Backup job hanging | S3 unreachable | `activeDeadlineSeconds` (default 3600s) will terminate the job. Check S3 connectivity. |
| Split-brain detected in logs | Network partition | The agent demotes (or fences) the leaseless node itself. See [Split-Brain](#split-brain). |

## Upgrade and migration

### Version model

Each chart is tagged `<chart>-<version>` (e.g. `pg-1.1.0`); `pg` and `pgvector` are released in lockstep — same version, same image, same agent. They **unified at `1.0.0`**: the earlier `0.5.x`/`0.6.x` split ended there and both charts now share a single version line. The `common` and `etcd` charts are vendored dependencies — they ship inside the `pg`/`pgvector` packages and are not released on their own.

### Compatibility matrix

| `pg` / `pgvector` | repmgr image | PostgreSQL | Kubernetes |
|-------------------|--------------|-----------|-----------|
| 1.14.1 *(current)* | `trixie-5.5.0-33` (`-pg18` / `-pg17`) | 18.x (default) or 17.x — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major) | ≥ 1.21 (PDB `policy/v1`); ≥ 1.27 for the agent-mode PDB `unhealthyPodEvictionPolicy` |
| 1.13.1 – 1.14.0 | `trixie-5.5.0-32` | 18.x (default) or 17.x | as above |
| 1.11.0 – 1.13.0 | `trixie-5.5.0-31` | 18.x (default) or 17.x | as above |
| 1.10.1 – 1.10.2 | `trixie-5.5.0-30` | 18.x (default) or 17.x | as above |
| 1.8.1 – 1.10.0 | `trixie-5.5.0-29` | 18.x (default) or 17.x | as above |
| 1.5.0 – 1.8.0 | `trixie-5.5.0-28` | 18.x | as above |
| 1.2.6 – 1.4.x | `trixie-5.5.0-27` | 18.x | as above |
| 1.2.2 – 1.2.5 | `trixie-5.5.0-26` | 18.x | as above |
| 1.2.0 – 1.2.1 | `trixie-5.5.0-25` | 18.x | as above |
| 1.0.0 – 1.1.8 | `trixie-5.5.0-16` … `-24` | 18.x | as above |
| 0.5.88 / 0.6.90 *(last 0.x)* | `trixie-5.5.0-15` | 18.x | ≥ 1.21 (PDB `policy/v1`) |

Extras: agent monitoring (`ha.agent.monitoring.*`) needs the Prometheus Operator CRDs; the etcd backend (`ha.agent.dcs.backend: etcd`) needs an etcd ≥ 3.5 (BYO/shared) or the bundled etcd subchart (`etcd.enabled=true`).

### Routine upgrade (within 1.x)

```bash
helm repo update
helm upgrade my-postgres cagriekin/pg   # add -f your-values.yaml
```

Within the 1.x line the default is agent mode, and successive releases (e.g. `1.0.0` → `1.14.1`) are backward-compatible: `helm upgrade` rolls the pods once for the new image (`trixie-5.5.0-33` at 1.14.1) and the agent re-establishes leadership with no manual step. **Read every `Migrating from X.Y.Z` entry in [`CHANGELOG.md`](CHANGELOG.md) between your current version and the target** — some releases (credential, `pg_hba`, or image changes) carry one-time steps. The CHANGELOG keeps an unbroken trail back through the 0.x line.

### Upgrading to 2.0.0 (repmgrd removed)

**2.0.0 removes the legacy `failoverMode: repmgrd` path** — the repmgrd sidecar, the
service-updater sidecar, and their values (#286). The lease-based agent, default since `1.0.0`,
is now the only failover path.

**If you were on the default (agent):** delete `repmgr.failoverMode: agent` from your values if
you set it explicitly, and upgrade normally. No StatefulSet recreate, no behaviour change.

**If you pinned `failoverMode: repmgrd`:** follow
[Migrating a repmgrd release (chart 1.x) to 2.0.0](#migrating-a-repmgrd-release-chart-1x-to-200).
`podManagementPolicy` goes `OrderedReady` → `Parallel` and is immutable, so it needs a one-time
`--cascade=orphan` recreate. Two further behaviour changes land with it:

- **Hardened `pg_hba`.** The agent assembles a pod-CIDR + SCRAM `pg_hba.conf` with no implicit
  `0.0.0.0/0 md5` catch-all. If you relied on the broad md5 rule, add explicit
  `postgresql.pgHba` rules **before** upgrading.
- **No `PrimaryChanged` Events.** The agent records failover decisions in a structured audit log
  on the pod instead, and the Role no longer requests the `events` create grant.

#### Renamed values: `repmgr.*` → `ha.*` (#291)

The top-level `repmgr:` block is now `ha:`. Nothing nested changed — only the block's own name:

```yaml
# 1.x                              # 2.0.0
repmgr:                            ha:
  enabled: true                      enabled: true
  username: repmgr                   username: repmgr
  agent:                             agent:
    mechanism: native                  mechanism: native
```

**Nothing is required of you in 2.0.0.** Every `repmgr.*` key still works: `pg.normalizeValues`
merges the `repmgr:` block over the `ha:` defaults key by key, so an untouched 1.x values file
installs unchanged and `--set repmgr.agent.leaseDuration=20s` still lands. Both spellings are
schema-validated, so a typo or a bad enum still fails the render either way, and keys that were
*removed* rather than renamed fail under either name. `helm upgrade` prints a notice when it
sees the old block.

#### The one rule that will surprise you: `repmgr.*` wins, from any source

Where the same key is set under **both** spellings, the `repmgr.*` value wins — and it wins over
Helm's own precedence order, not within it. By the time the chart runs, Helm has already
collapsed chart defaults, every `-f` and every `--set` into one map, so the merge cannot tell an
operator's value from a chart default. It only sees two spellings and always prefers the
deprecated one, because that is the only rule that keeps a released 1.x file working — the `ha.*`
side is where the chart's own defaults live, so preferring it would let a default silently beat a
value you set.

The consequence, spelled out because it is genuinely counter-intuitive:

```bash
# legacy.yaml still has  repmgr.agent.leaseDuration: 15s
helm upgrade r cagriekin/pg -f legacy.yaml --set ha.agent.leaseDuration=30s   # renders 15s, NOT 30s
```

A `--set`, which Helm normally gives the highest precedence of all, is discarded here. The same
applies to a later `-f` — order does not matter, the old spelling wins either way. This cannot be
caught at render time: detecting it needs to know which of the two values you actually supplied,
and Helm exposes no provenance to a template, so a chart-side `fail` would either fire on every
legitimate alias use or not at all.

**So migrate a key by *moving* it, never by adding the new spelling alongside the old one.**
Delete each `repmgr.*` key in the same edit that adds its `ha.*` replacement, and the surprise
cannot arise. Renaming the whole block at once — the fastest route, below — sidesteps it entirely.

The alias exists so this rename is not a second breaking change stacked on 2.0.0's real one.
**It is removed in the next major** — rename the block before then:

```bash
helm get values -o yaml -n <namespace> <release> > values.yaml   # -o yaml is not optional
# change the top-level "repmgr:" key to "ha:"; leave everything under it alone
```

`-o yaml` matters: the default output prefixes a `USER-SUPPLIED VALUES:` line, and the schema
deliberately leaves `additionalProperties` open, so that stray top-level key is accepted in
silence rather than rejected.

Why rename at all: after #290 the image contains no repmgr — no binary, no extension, no
`repmgr.conf` — and the agent replicates through `pg_stat_replication` and its own slots. A block
named `repmgr` sized the resources of, and configured the credentials for, something that is not
installed. Three keys keep the word legitimately, because they name real PostgreSQL objects the
agent still authenticates as rather than the tool: `ha.username`, `ha.database`, and the
`repmgr-password` Secret key. Renaming those touches live clusters' credentials and roles, so it
is deliberately not part of this change.

**Removed values** — each is rejected at render time with a message naming the fix, so a stale
values file fails the upgrade rather than silently deploying something else:

| Removed | Why | Action |
|---------|-----|--------|
| `repmgr.failoverMode` | Only one failover path remains | Delete the key |
| `repmgr.serviceUpdater.*` | The sidecar it sized is gone | Delete the key |
| `repmgr.monitoringHistoryDays` | Pruned `repmgr.monitoring_history`, which only repmgrd wrote | Delete the key |
| `pgpool.autoFailback` | Rendered PGPool's `auto_failback`, which only applied to the repmgrd failover flow | Delete the key |

### Crossing the 0.x → 1.x boundary

Coming from a 0.x release, the `podManagementPolicy` and `pg_hba` notes above apply, plus:
the postgresql PodDisruptionBudget defaults to `maxUnavailable: 1` +
`unhealthyPodEvictionPolicy: AlwaysAllow` (was `minAvailable: 1`) — equivalent on a 2-pod
cluster, strictly better for drains/upgrades on k8s ≥ 1.27.

## Troubleshooting PGPool

### Connectivity: PGPool-II or the Backend?

When clients cannot connect, isolate the failing layer first. Query through PGPool-II:

```bash
kubectl port-forward svc/my-postgres-pg-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres -c "SELECT 1"
```

Then bypass PGPool-II and query the primary Service directly:

```bash
kubectl port-forward svc/my-postgres-pg 5432:5432
psql -h localhost -p 5432 -U postgres -d postgres -c "SELECT 1"
```

If only the PGPool-II path fails, check backend status and logs below. If both fail, troubleshoot PostgreSQL itself first (see the recovery runbooks above).

Check that the Services have endpoints (`my-postgres-pg-readonly` exists when repmgr is enabled):

```bash
kubectl get endpoints my-postgres-pg my-postgres-pg-pgpool my-postgres-pg-readonly
```

The PGPool-II readiness probe runs `SELECT 1` through port 9999 rather than a TCP check, so PGPool-II pods turn unready and drop out of the Service whenever they cannot serve queries from at least one backend. Empty `my-postgres-pg-pgpool` endpoints therefore usually point at a backend or authentication problem, not at the Service. Restarts of the pgpool Deployment pods have the same root cause: the liveness probe runs the same query and restarts a wedged PGPool-II after about 60 seconds.

If reads through the `my-postgres-pg-readonly` Service do not reach standbys, the problem is the `pg-role` labels rather than PGPool-II: the Service selects `pg-role: standby`, which the agent re-applies every reconcile tick, and pods stay absent from its endpoints until labeled (fresh installs, recreated or scaled-up pods).

### Checking Backend Status

`SHOW pool_nodes` through port 9999 reports each backend as PGPool-II sees it. `pool_hba.conf` trusts local connections inside the pod, so no password is needed:

```bash
kubectl exec -it deploy/my-postgres-pg-pgpool -c pgpool -- \
  sh -c 'psql -h 127.0.0.1 -p 9999 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SHOW pool_nodes;"'
```

The PCP admin interface on port 9898 (exposed on the pgpool Service only when `pgpool.service.exposePcp=true`) provides the same data. It authenticates against `pcp.conf`, which the init container generates from the admin Secret: the chart-managed `my-postgres-pg-pgpool-admin` (keys `username`/`password`, populated from `pgpool.admin.username`/`pgpool.admin.password`), or your own Secret when `pgpool.admin.existingSecret.enabled` is set. Retrieve the password, then run the pcp commands (they prompt for it):

```bash
kubectl get secret my-postgres-pg-pgpool-admin -o jsonpath='{.data.password}' | base64 -d
kubectl exec -it deploy/my-postgres-pg-pgpool -c pgpool -- pcp_node_count -h localhost -p 9898 -U admin
kubectl exec -it deploy/my-postgres-pg-pgpool -c pgpool -- pcp_node_info -h localhost -p 9898 -U admin 0
```

Changing the chart-managed credentials rolls the Deployment via the Secret checksum annotation; rotating an existing Secret requires `kubectl rollout restart deployment my-postgres-pg-pgpool`, because `pcp.conf` is generated at pod start.

Node IDs follow the StatefulSet ordinals: node 0 is `my-postgres-pg-0`, node 1 is `my-postgres-pg-1`, and so on.

| Column | Meaning |
|--------|---------|
| `status` | `up`: attached, receives traffic. `waiting`: attached, no connection established yet. `down`: detached after `pgpool.healthCheck.maxRetries` consecutive health check failures; no traffic is routed to it. |
| `role` | `primary` or `standby` as detected by the streaming replication check. If this disagrees with the Lease holder, restart PGPool-II. |
| `replication_delay` | Standby lag in bytes. |
| `select_cnt` | SELECT queries routed to the node; confirms load balancing is working. |

PGPool-II has failover disabled by design (the agent owns it and re-points the Services), so backends are not detached on a primary change. If a backend is stuck detached, reattach it with `pcp_attach_node -h localhost -p 9898 -U admin <node-id>` or restart the Deployment.

### Recovering After Failover

With repmgr enabled, PGPool-II needs no failover handling of its own:

1. The agent on the new leader repoints the RW (`<fullname>`) and RO (`<fullname>-readonly`) Service selectors.
2. PGPool-II points at those Services, not at pod IPs, so it follows the change without reconfiguration — which is why all backends are flagged `DISALLOW_TO_FAILOVER`.
3. The PGPool-II liveness probe restarts any instance that cannot serve queries for about 60 seconds.

If clients still reach a stale topology (for example writes failing with read-only errors) — usually
pooled connections held open to the old primary — restart the Deployment:

```bash
kubectl rollout restart deployment my-postgres-pg-pgpool
```

Failover history lives in the agent's structured audit log on the PostgreSQL pods (the
`PrimaryChanged` core/v1 Events that the service-updater used to emit went away with it in 2.0.0):

```bash
kubectl logs my-postgres-pg-0 -c postgresql | grep -i 'promote\|demote\|lease'
kubectl describe service my-postgres-pg
```

Unlike Events — pruned by the cluster's event TTL, one hour by default — the log lives as long as
the pod, and `pg_ha_agent_*` metrics carry the same transitions for longer retention.

### Logs

PGPool-II logs to stderr, so everything is available through the container logs:

```bash
kubectl logs deploy/my-postgres-pg-pgpool -c pgpool
```

Verbosity is controlled by the `pgpool.logging.*` values: `logConnections` (default `true`), `logStatement` (log every client query), `logPerNodeStatement` (log which backend each query was routed to), and `logMinMessages` (default `warning`; `debug1` and below add internal detail). Changing them rolls the Deployment automatically via the config checksum annotation.

| Message | Meaning |
|---------|---------|
| `failed to connect to PostgreSQL server` / `health check retrying` | A backend is unreachable. The node is marked `down` after `pgpool.healthCheck.maxRetries` retries (default 10, every 3 seconds). |
| `degenerate backend request ... is canceled because failover is disallowed` | Expected. All backends are flagged `DISALLOW_TO_FAILOVER` (or `ALWAYS_PRIMARY` without repmgr): the agent owns failover and re-points the Services, so PGPool-II must not detach nodes itself. |
| `all backend nodes are down` | No backend is reachable and clients are rejected. The liveness probe restarts PGPool-II, which retries discovery; if the message persists, check the PostgreSQL pods. |
| `authentication failed` / `password mismatch` | Remote clients authenticate with md5 against `pool_passwd`, which contains only the chart's PostgreSQL user. Other database users cannot authenticate through PGPool-II while `pgpool.allowClearTextFrontendAuth` is `false` (default); either connect them directly to PostgreSQL or set it to `true` so PGPool-II can request their password in clear text and forward it to the backend. |
