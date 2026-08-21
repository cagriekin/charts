# PostgreSQL with repmgr and PGPool-II

PostgreSQL Helm chart with repmgr for automatic failover and replication management, optional PGPool-II for connection pooling and read/write splitting.

## Features

- PostgreSQL 18.1 with configurable version
- Repmgr for automatic failover and replication management
- Service-updater sidecar for automatic primary service selector updates after failover
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
> `repmgr.image`, `pgpool.image`, `pgpool.metrics.image`, `prometheusExporter.image`,
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
| `postgresql.majorVersion` | PostgreSQL major version in `image.tag`; builds the extension paths (`/usr/lib/postgresql/<major>/lib`, `/usr/share/postgresql/<major>/extension`) when `extensions.enabled=true`. In repmgr mode the server runs from the repmgr image and follows `repmgr.image.majorVersion` regardless of `postgresql.image`; the chart fails to render if the two majors differ. Set both to `"17"` (with a `-pg17` repmgr tag) to run PostgreSQL 17 — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major). | `"18"` |
| `postgresql.replicaCount` | Number of PostgreSQL replicas (total instances = replicaCount + 1); values > 0 require `repmgr.enabled=true` | `1` |
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

When `repmgr.enabled` is true, `additionalCommands` automatically discover the current primary and execute against it, so DDL statements like `CREATE EXTENSION` work correctly regardless of which pod the hook runs on (including standbys after a failover).

### Logical Replication (#308)

Set `postgresql.walLevel: logical` for a logical-replication subscriber (`CREATE SUBSCRIPTION`, Debezium, or any other decoder on a replication slot). `logical` is a strict superset of `replica` and works regardless of `pgbackrest.enabled`/`archive_mode=on` — the two are unrelated concerns. `wal_level` is a postmaster parameter — the change rolls the pods via the existing configmap-checksum annotation, the same way any other `postgresql.configuration` change does.

**Capacity.** Every physical standby consumes one `max_wal_senders` slot (and, in agent mode, one `max_replication_slots` entry — see `repmgr.agent.syncReplicationSlots` below); every logical subscriber consumes one more of each. The image's own initdb default is `max_wal_senders = 10` / `max_replication_slots = 10` (unaffected by `postgresql.walLevel`), which now flows through uncontested instead of being silently re-asserted by `pgbackrest-archive.conf` — so raise both via `postgresql.configuration` if `replicaCount` plus your logical subscriber count would otherwise exhaust the default.

**This is the only place to set `wal_level`.** `pgbackrest.enabled` used to render a hardcoded `wal_level = replica` into its own `pgbackrest-archive.conf`, which sorts after `custom.conf` under `include_dir` and would silently win over a `postgresql.configuration.wal_level` you set yourself — that coupling is gone (`postgresql.walLevel` now has its own render block, independent of `pgbackrest.enabled`), but the chart still rejects `wal_level` in `postgresql.configuration` at render time and tells you to set `postgresql.walLevel` instead, so there is exactly one source of truth regardless of pgBackRest's state.

A logical subscriber must connect to the **write Service** (`<fullname>:5432`), not Pgpool — Pgpool's query routing is built for physical replicas, not for holding a replication slot's connection open.

**Surviving a failover: `repmgr.agent.syncReplicationSlots`.** A plain logical slot does not survive the primary moving — `synchronized_standby_slots` (PostgreSQL 17+) is what lets a **failover** slot (`CREATE SUBSCRIPTION ... WITH (failover = true)`) be synced to a standby so it's still there after a promote, but it names physical replication slots, and PostgreSQL 17+'s `sync_replication_slots` worker (the standby-side process that keeps the failover slot in sync) additionally requires `dbname` in `primary_conninfo`, which repmgr's own clone/follow machinery never sets.

The chart and agent (failover mode `agent` only) handle both automatically when `repmgr.agent.syncReplicationSlots: true`:

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

In repmgr mode, `shared_preload_libraries: pg_cron` is merged with `repmgr` (and `pgaudit`, if audit is on) automatically — declare only your own libraries, per [Mounting an extra file on every replica](#mounting-an-extra-file-on-every-replica) below.

**Version pinning.** Append `=version` in apt syntax, e.g. `"postgresql-{major}-cron=1.6.4-1"`. `{major}` is substituted with `postgresql.majorVersion` at render time, so a package list survives a later major bump without editing (confirm the new major has a PGDG build of the same extension before bumping, though).

**PGDG apt-source assumption.** `copy-base-ext` runs from the `cagriekin/repmgr` image, which configures the PGDG apt repository itself at build time — package installs there are reliable whenever repmgr mode is on. `copy-ext` runs from whatever `postgresql.image` you set (default: the official `postgres:18.1-trixie` Docker Hub image); this chart does not verify that image has PGDG configured. This matters most in **standalone mode** (`repmgr.enabled: false`), where `copy-ext` is your only extension source; in repmgr mode, `copy-base-ext` is a confirmed-good fallback even if `copy-ext`'s apt step comes up short.

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

**Limitation.** This mechanism only helps for extensions that have a PGDG or Debian package. A small number of extensions — mostly private/internal ones, or ones never packaged for Debian/PGDG — have no such package and are **not** solved by `packages`; those still require a custom image with the extension compiled in.

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

In repmgr mode the chart merges `repmgr` into `shared_preload_libraries` for you — declare only your own libraries. (A bare value in `configuration` is loaded via `include_dir` *after* the image's own `postgresql.conf`, so without that merge it would override the image's `shared_preload_libraries = 'repmgr'` and silently disable failover.)

`postgresql.extraEnv` does the same for environment variables and accepts both `value` and `valueFrom`.

These three values are validated at render time, so a mistake fails `helm install`/`upgrade` with a clear message instead of at apply time or silently at runtime:

- each must be a **list** of objects (a map is a common slip and would otherwise produce an opaque YAML parse error);
- an `extraVolumes` name may not collide with a chart-managed volume (`data`, `postgresql-config`, `postgresql-tls`, `ext-lib`, `ext-share`, `repmgr-config`, `etcd-tls`, `pg-run`, `pgbackrest-config`, `service-updater-script`) — a `data` collision is silently dropped in favour of the volumeClaimTemplate;
- every `extraVolumeMounts` entry must reference a declared `extraVolumes` entry (catches the `extraVolume:`/`extraVolumes:` typo, which the API server would otherwise reject only at apply time);
- `extraEnv` may not reuse a chart-set env name (`PGDATA`, `POSTGRES_*`, `REPMGR_*`, …) — duplicate env names are last-wins at runtime, so an override would silently shadow the chart/Secret value.

### Repmgr Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `repmgr.enabled` | Enable repmgr | `true` |
| `repmgr.image.repository` | Repmgr image repository | `cagriekin/repmgr` |
| `repmgr.image.tag` | Repmgr image tag. Unsuffixed = the default major (18); `-pg18` / `-pg17` select one explicitly | `trixie-5.5.0-31` |
| `repmgr.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `repmgr.image.majorVersion` | PostgreSQL major bundled in the repmgr image. In repmgr mode the server always runs this major; `postgresql.majorVersion` must match or the chart fails to render. Move it together with `repmgr.image.tag` (`17` ⇄ `-pg17`) — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major). | `"18"` |
| `repmgr.username` | Repmgr database user | `repmgr` |
| `repmgr.database` | Repmgr database name | `repmgr` |
| `repmgr.monitoringHistoryDays` | Days of `repmgr.monitoring_history` to retain; pruned daily on the primary via `repmgr cluster cleanup` | `7` |
| `repmgr.terminationGracePeriodSeconds` | Time allowed for graceful shutdown and failover | `120` |
| `repmgr.resources.requests.cpu` | CPU request | `50m` |
| `repmgr.resources.requests.memory` | Memory request | `128Mi` |
| `repmgr.resources.limits.cpu` | CPU limit | `500m` |
| `repmgr.resources.limits.memory` | Memory limit | `512Mi` |
| `repmgr.splitBrainDetection.action` | Action on split-brain: `log` (alert only) or `fence` (terminate stale primary) | `log` |
| `repmgr.serviceUpdater.resources.requests.cpu` | Service-updater CPU request | `50m` |
| `repmgr.serviceUpdater.resources.requests.memory` | Service-updater memory request | `64Mi` |
| `repmgr.serviceUpdater.resources.limits.memory` | Service-updater memory limit | `128Mi` |
| `repmgr.initContainerResources` | Resources for the `repmgr-init` standby-clone init container (heavier than the shared init default; raise for large databases) | `requests: 100m/128Mi, limits: 1/1Gi` |

When repmgr is enabled, a preStop lifecycle hook stops PostgreSQL cleanly (`pg_ctl stop -m fast`) before pod termination. If the terminated pod was the primary, repmgrd on a standby detects the outage and promotes via its `promote_command`, which also updates repmgr metadata; the hook deliberately does not promote out-of-band, since a raw `pg_promote()` would leave repmgr.nodes stale and strand every repmgrd. The `terminationGracePeriodSeconds` controls how long Kubernetes waits for the shutdown to complete.

When repmgr is enabled, two sidecars run alongside PostgreSQL in each pod:

- **repmgrd**: monitors replication and triggers automatic failover when the primary becomes unavailable. Has a preStop hook that runs `repmgr daemon stop` so the shutting-down node's daemon does not trigger a spurious failover during pod termination. (This stops the daemon only; the master's service-updater handles unregistering nodes that a scale-down removed — see [Scaling down](#scaling-down).)
- **service-updater**: watches repmgr cluster state and patches the Kubernetes Service selector to point to the current primary, then restarts PGPool-II if enabled. Also maintains a `pg-role` label (`primary`/`standby`) on every postgresql pod each cycle, which the `<fullname>-readonly` service selects (`pg-role: standby`) to route read traffic to replicas; pods without the label (fresh, recreated or scaled-up) are excluded until labeled. Has a preStop hook that sleeps 5s to allow in-flight patches to complete. Includes a liveness probe that checks for a heartbeat file updated each loop iteration (fails if no update within 120s). Performs split-brain detection each cycle by querying all nodes for `pg_is_in_recovery()` -- if multiple primaries are found, takes the configured action (`log` or `fence`).

**Split-brain detection**: In a 2-node cluster, network partitions can cause both nodes to believe they are the primary. The service-updater detects this by checking all nodes each monitoring cycle. With `action: log` (default), it logs a critical warning. With `action: fence`, it compares WAL LSN positions and terminates connections on the stale primary. For production deployments, use 3+ nodes to reduce split-brain risk.

### Choosing the PostgreSQL major

**PostgreSQL 18 is the default. PostgreSQL 17 is selectable.** In repmgr mode the server binaries come from the **repmgr image**, not from `postgresql.image` — so the major is decided by which repmgr image you run, and setting `postgresql.image` to another major has no effect on the running server. The image is published per major:

| Tag | PostgreSQL | Use |
|-----|------------|-----|
| `trixie-<repmgr>-<n>` | 18 | The default. What every unsuffixed pin resolves to — unchanged by this feature |
| `trixie-<repmgr>-<n>-pg18` | 18 | The same build, named explicitly |
| `trixie-<repmgr>-<n>-pg17` | 17 | PostgreSQL 17 |

All three are multi-arch (amd64/arm64), SBOM- and provenance-attested, and cosign-signed exactly like the unsuffixed tag.

Three values move **together**; the chart refuses to render if the two majors disagree (in either direction), because a mismatch would silently run one major while building extension paths for another:

```yaml
postgresql:
  majorVersion: "17"
  image:
    tag: 17.10-trixie      # only used in standalone mode / for the extension copy
repmgr:
  image:
    tag: trixie-5.5.0-31-pg17
    majorVersion: "17"
```

The chart checks the claim rather than trusting it: a `-pgNN` tag that disagrees with `repmgr.image.majorVersion` **fails the render**, and `PG_MAJOR` is passed to every container running the repmgr image — so if the majors are moved while the tag is left on the unsuffixed default (which carries no suffix to compare), the entrypoint and the agent refuse to start, naming both the requested and the bundled major. A wrong-major cluster is therefore a loud failure at install time, not a discovery months later.

Standalone mode (`repmgr.enabled=false`) is unconstrained: there is no repmgr image in play, so `postgresql.image` alone decides the major.

**This is a create-time choice, not an upgrade path.** The chart has no in-place major upgrade: changing the major of an existing cluster would start a new-major server on an old-major `PGDATA`, which refuses to boot. Moving an existing cluster between majors means a logical dump/restore into a fresh release.

Reasons to pick 17 deliberately:

- **repmgr upstream does not list 18.** repmgr's [install requirements](https://www.repmgr.org/docs/current/install-requirements.html) for 5.5.0 (2024-11-24, still the newest release) name PostgreSQL **13–17**. The image builds `postgresql-18-repmgr` from PGDG, and distro packagers do compile 5.5.0 against 18 — but the honest statement is that the PG18 default rests on **distro packaging rather than an upstream support claim**. If you need an upstream-sanctioned repmgr/PostgreSQL pairing, select 17.
- **Extension availability varies by major** in PGDG, so a required extension can force a major.
- **`pg_dump` output is not guaranteed to load into an older server** ([docs](https://www.postgresql.org/docs/18/app-pgdump.html)), so the major a cluster is created on constrains where its data can later go. If you may need to move data to an older-major server, choose deliberately now.
- **PostgreSQL 17 is supported upstream until 2029-11-08**, so it is a long-lived choice, not a stopgap.

Both majors run the **whole** live test suite in CI (failover, pgBackRest restore/bootstrap, TLS, pgpool, etcd DCS, migration), and each published image is started and checked before release — including that `pgaudit` loads, so [audit logging](#audit-logging-pgaudit) works on either major.

### Failover modes: lease-based `agent` (default) and legacy `repmgrd`

`repmgr.failoverMode` selects how failover is decided:

- **`agent`** (default since `1.0.0`): a Go agent (`pg-ha-agent`) runs as PID 1 in the postgresql container and holds a Kubernetes `coordination.k8s.io/v1` Lease (`<fullname>-leader`) as the **sole authority** for which pod is primary, driving repmgr as a pure mechanism (`failover=manual`, no repmgrd). The Lease replaces hand-rolled split-brain handling and removes the repmgrd startup race.
- **`repmgrd`** (legacy, opt-in): the repmgrd + service-updater sidecars described above. Unchanged behavior; supported for one major cycle (deprecated). Pin `repmgr.failoverMode: repmgrd` to stay on it.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `repmgr.failoverMode` | `repmgrd` or `agent` | `repmgrd` |
| `repmgr.agent.leaseDuration` | Lease TTL; a challenger cannot acquire until this elapses since the last renew | `15s` |
| `repmgr.agent.renewDeadline` | Holder self-demotes if it cannot renew within this | `10s` |
| `repmgr.agent.retryPeriod` | Lease acquire/renew retry interval | `2s` |
| `repmgr.agent.reconcileInterval` | Reconcile tick interval | `5s` |
| `repmgr.agent.podCidr` | Pod CIDR trusted in the agent's hardened SCRAM-only pg_hba (no `0.0.0.0/0 md5`); set to your cluster's pod CIDR if outside `10.0.0.0/8` | `10.0.0.0/8` |
| `repmgr.agent.cascadingReplication` | Let a standby stream from another standby (a chain by pod ordinal toward the primary) to offload the primary's WAL senders. Default off; agent mode only; meaningful at `replicaCount >= 2` (3+ nodes). The agent only picks a verifiably-safe same-timeline upstream and re-homes to the leader if it fails/promotes, so failover is not delayed and a standby is never stranded. | `false` |
| `repmgr.agent.syncReplicationSlots` | Reconcile `synchronized_standby_slots` to the live standby set on every primary tick, so a logical failover slot survives a promote. Default off; agent mode only; requires PostgreSQL 17+ and `postgresql.walLevel: logical` (#308; see [Logical Replication](#logical-replication-308)). | `false` |

Must satisfy `leaseDuration > renewDeadline > retryPeriod`. For managed clouds, widen them (e.g. `30s/20s/4s`) so a brief apiserver blip does not trip an unnecessary demote. Note: with the Kubernetes Lease backend, a control-plane outage longer than `renewDeadline` is itself a write outage (the healthy primary self-demotes on losing apiserver contact, and no standby can acquire until the control plane returns); this is the safe choice under an asymmetric partition.

In agent mode the agent also fronts the read/write split: `pgpool` (if enabled) points at the RW (`<fullname>`) and RO (`<fullname>-readonly`) Services with failover off, and the agent maintains the Service selector and `pg-role` labels itself (no repmgrd/service-updater sidecars). With `postgresql.replicaCount: 0` (primary-only) there are no standbys, so pgpool configures only the RW backend and runs as a single-backend router — the RO backend is omitted to avoid health-checking an endpointless Service (#207).

### Migrating an existing release to agent mode

`podManagementPolicy` differs by mode (`OrderedReady` for repmgrd, `Parallel` for agent) and is **immutable** on an existing StatefulSet, so switching an existing release needs a one-time recreate (zero data loss — pods and PVCs are kept):

```bash
# 1. Healthy cluster + a fresh backup first. GitOps: disable auto-sync for these steps.
# 2. Orphan-delete the StatefulSet (keeps pods + PVCs running; Helm re-adopts them):
kubectl delete statefulset <release>-pg -n <ns> --cascade=orphan
# 3. Upgrade into agent mode (recreates the STS as Parallel, adopts the orphaned pods):
helm upgrade <release> cagriekin/pg -n <ns> --set repmgr.failoverMode=agent  # + your -f values
# 4. Verify:
kubectl get lease <release>-pg-leader -n <ns> -o jsonpath='{.spec.holderIdentity}'  # == the primary pod
kubectl get endpoints <release>-pg -n <ns>                                          # points at it
# Rollback is symmetric: --set repmgr.failoverMode=repmgrd with the same --cascade=orphan recreate,
# then optionally: kubectl delete lease <release>-pg-leader -n <ns>
```

GitOps/ArgoCD: the Lease, the primary-marker ConfigMap, and the write-Service `.spec.selector` are runtime-owned by the agent — `ignoreDifferences` on the Service selector and do not prune the Lease/marker, or auto-sync will fight the agent. Set `postgresql.existingSecret.enabled=true` (the `lookup`-based password generation returns nil under ArgoCD).

### Maintenance mode (pause) — agent mode

For planned work that would otherwise trigger an unwanted failover (a deliberate primary restart, a node drain, a PostgreSQL minor-version restart), put the agent in **maintenance mode**: it keeps renewing the Lease and serving, but suspends all automatic promote / demote / fence / self-health actions (it only observes). Toggle it with an annotation on the primary-marker ConfigMap:

```bash
# Pause (before the planned operation):
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/pause=true --overwrite
# Resume (after):
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/pause-
```

While paused, `pg_ha_agent_is_paused` reads `1`. Pausing does not stop the cluster from serving — it only stops the agent from reacting to faults, so a genuine failure during the window will NOT fail over until you resume. In particular, if the primary itself wedges or dies while paused, the agent keeps renewing the Lease and the write Service keeps pointing at it; there is no automatic failover until you remove the annotation. (There is no split-brain risk: a real Lease loss still fences via the leader-election callback.) Keep maintenance windows short and watch the cluster while paused.

### Controlled switchover (agent mode)

To hand the primary role to a specific standby on purpose (e.g. to move the primary off a node you are about to drain), annotate the marker with the target pod:

```bash
kubectl annotate configmap <release>-pg-primary -n <ns> pg-ha/switchover-target=<release>-pg-1 --overwrite
```

The serving primary waits until that target is a **caught-up, same-timeline standby** (its replay LSN has reached the primary's WAL position — invariant 8), then clears the annotation (one-shot) and steps down so the target promotes. If the target is lagging or unreachable, the primary keeps serving and retries — it never steps down onto a behind standby, so committed data is not discarded. The graceful step-down flushes WAL to the connected target, making the handoff near-zero-RPO in practice.

Caveats: this is a planned handoff layered on the lease election, not a fenced zero-RPO transaction — the directed target promotes deterministically on a two-pod cluster; with three or more pods the most-advanced standby wins the freed lease (usually but not necessarily the named target). For strict RPO=0 use synchronous replication (not enabled in this chart).

### Control REST API (agent mode) — `repmgr.agent.control`

The pause and switchover runbooks above are `kubectl annotate` calls: they work, they are
the reference, and they need no extra machinery. What they cannot do is **check the
request before accepting it**. `kubectl annotate ... switchover-target=<pod>` succeeds
even when the pod does not exist, is on a divergent timeline, or is 4 GB behind — you
find out by tailing logs. It also cannot tell you the cluster's per-member replication
position, or *why* the agent is not doing what you expected.

The optional control API closes both gaps. It is off by default:

```yaml
repmgr:
  failoverMode: agent
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

#### API-driven PITR restore — `repmgr.agent.control.restore`

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

- **BYO/shared** (recommended, especially for several databases against one platform etcd): set `repmgr.agent.dcs.etcd.endpoints` as above and leave `etcd.enabled=false`.
- **Bundled** (self-contained, for an install with no existing etcd): set `etcd.enabled=true` and leave `endpoints` empty — the chart deploys a 3-node etcd cluster (`<release>-etcd`) and points the agent at it automatically. Adds 3 stateful pods (`+~0.3 CPU / 0.4Gi` requested, small SSD PVCs). The bundled etcd runs plaintext within the pod network (isolate it with a NetworkPolicy; the leadership data is non-secret); tune it under the `etcd:` values key (`replicaCount`, `resources`, `persistence`, `topologySpreadConstraints`). For a TLS-secured store, use a BYO/shared etcd with `dcs.etcd.tls`.

> Agent mode is opt-in and validated by the chart's live failover suite (graceful failover: a standby promotes, the write Service repoints, the ex-primary rejoins read-only). See `ENVIRONMENT.md` for the full injected-variable catalog.

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
| `pgpool.autoFailback` | Automatically reattach recovered backends | `true` |
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

When repmgr is enabled, a `<fullname>-readonly` service routes only to standby pods, selected via the `pg-role: standby` label. In repmgrd mode the service-updater sidecar maintains the label; in agent mode the agent does, with a 3-way classification (in-recovery -> `standby`; reachable-but-not-in-recovery -> `orphan`, kept OUT of the read pool so a divergent node never serves stale reads; unreachable -> left untouched):

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
- **`require`/`clientCertAuth` are agent-mode only** (`repmgr.failoverMode=agent`). In
  repmgrd mode use `postgresql.tls.enabled` for optional server TLS; enforced TLS needs
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
(preserving `repmgr` and any libraries you set in `postgresql.configuration` —
they are merged), renders the `pgaudit.*` GUCs into the postgresql ConfigMap, and
creates the extension idempotently on the primary via a post-install/upgrade hook Job.

- **Requires `repmgr.enabled: true`.** Audit logging needs the `cagriekin/repmgr` image,
  which bundles the `pgaudit` extension. Standalone mode (`repmgr.enabled: false`) uses the
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

Repmgr manages replication automatically. To check cluster status:

```bash
kubectl exec -it my-postgres-pg-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

### Scaling down

Scaling `postgresql.replicaCount` **down** removes the highest-ordinal pods. The
primary now **automatically unregisters** the removed nodes from `repmgr.nodes` (#139),
so `repmgr cluster show` no longer reports them as permanently failed and failover
elections do not retry the gone DNS names. The primary reconciles `repmgr.nodes` against
the live ordinal range on each tick (agent mode: the lease-holding primary; repmgrd
mode: the master's service-updater), keyed on the ordinal (node id = `ordinal + 1000`),
never on reachability — so a momentarily-down *live* node is never unregistered. Cleanup
typically completes within a minute of the rolled pods settling; verify with:

```bash
kubectl exec -it my-postgres-pg-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

> The removed pods' PVCs are retained (StatefulSet does not delete them); reclaim them
> manually if desired. If the node being removed is the *current primary* (possible when
> a prior failover left the primary on a high ordinal), `repmgr standby unregister`
> refuses to drop a primary row — unregister it by hand after the cluster settles:
> `kubectl exec -it my-postgres-pg-0 -- repmgr -f /etc/repmgr/repmgr.conf standby unregister --node-id=$((N + 1000))`.

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

Requires `repmgr.enabled: true` (pgBackRest is installed in the repmgr image).

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

In agent mode (`repmgr.failoverMode: agent`) the Lease and the highwater-marker
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
make test-repmgr-failover   # repmgrd: primary + replica, then kill primary -> promote
make test-repmgr-chaos      # repmgrd: chaos restart regression
make test-full              # repmgr + pgpool + prometheus exporter
make test-upgrade           # upgrade path with data persistence
make test-agent             # lease-based agent: install + failover (AGENT_COLDBOOT=1 adds cold boot)
make test-agent-etcd        # agent with the bundled etcd DCS backend
make test-migrate-agent     # repmgrd -> agent --cascade=orphan migration
make cluster-delete

# Run the core cluster suites in parallel
make -j4 test-cluster

# Confirm the legacy repmgrd render has not drifted vs a baseline ref
make byte-stable REF=origin/master
```

## Failover RTO/RPO

### Recovery Time Objective (RTO)

With repmgr enabled, automatic failover completes in approximately 30-60 seconds:

1. **Detection** (~10-30s): repmgrd detects primary unavailability based on `health_check_interval` and `reconnect_attempts` in the repmgr configuration.
2. **Promotion** (~5-10s): repmgrd promotes the highest-priority standby via its `promote_command` (`repmgr standby promote`).
3. **Service update** (~5-15s): service-updater detects the new primary and patches the Kubernetes Service selector. PGPool-II is restarted if enabled.

The `terminationGracePeriodSeconds` (default 120s) controls the maximum time allowed for graceful failover during planned drains (e.g., node upgrades).

### Recovery Point Objective (RPO)

| Backup Method | RPO | Notes |
|---------------|-----|-------|
| Streaming replication (async) | Seconds of lag | Default. RPO depends on replication lag. Monitor with `pg_stat_replication`. |
| Streaming replication (sync) | Zero | Set `synchronous_commit = on` and configure `synchronous_standby_names` in `postgresql.configuration`. Adds write latency. |
| pgBackRest PITR | Up to last archived WAL segment | Continuous WAL archiving. RPO depends on `archive_timeout` (default 60s). |
| pg_dump S3 backup | Up to last backup interval | Default daily at 2am. Not suitable for near-zero RPO. |

## Recovery Runbooks

> The runbooks below describe **repmgrd mode** (the legacy, opt-in path). In **agent mode** (the default since 1.0.0)
> (`repmgr.failoverMode: agent`) failover is driven by the Kubernetes Lease, not
> repmgrd/service-updater: a primary failure is handled automatically (a standby
> wins the Lease and promotes; the agent repoints the Service selector), and
> split-brain is prevented at the source (a node serves read-write only while it
> holds the Lease), so the Split-Brain Recovery runbook does not apply. For
> agent-mode operations see [Maintenance mode](#maintenance-mode-pause--agent-mode),
> [Controlled switchover](#controlled-switchover-agent-mode), and the agent-mode
> notes in [Point-in-Time Recovery](#point-in-time-recovery).

### Primary Failure (Automatic Failover)

No action required if repmgr is enabled. The sequence is:
1. repmgrd detects primary failure
2. Standby is promoted automatically
3. service-updater patches the Kubernetes Service
4. PGPool-II restarts to pick up the new backend topology

Verify with:
```bash
kubectl exec -n <namespace> <pod> -c postgresql -- repmgr cluster show
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

### Split-Brain Recovery

If split-brain is detected (multiple primaries logged by service-updater):

1. Identify which node has the most recent data:
```bash
kubectl exec -n <namespace> <pod-0> -c postgresql -- psql -U postgres -c "SELECT pg_current_wal_lsn();"
kubectl exec -n <namespace> <pod-1> -c postgresql -- psql -U postgres -c "SELECT pg_current_wal_lsn();"
```

2. Stop the stale primary (lower LSN):
```bash
kubectl exec -n <namespace> <stale-pod> -c postgresql -- pg_ctl stop -D /var/lib/postgresql/data/pgdata -m fast
```

3. Delete the stale pod to let it rejoin as standby:
```bash
kubectl delete pod <stale-pod> -n <namespace>
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
| Failover not triggering | repmgrd not detecting failure | Check repmgrd logs. Verify `health_check_interval` and `reconnect_attempts`. |
| Service not updating after failover | service-updater stuck or crashed | Check service-updater logs. Liveness probe should restart it if stuck. |
| PGPool returning errors after failover | PGPool not restarted | service-updater should restart PGPool. Check service-updater logs. Manual restart: `kubectl rollout restart deployment <fullname>-pgpool` |
| WAL archiving failing (pgBackRest) | S3 credentials or connectivity | Check the `pgbackrest` sidecar logs on the primary pod and the `<fullname>-pgbackrest-full`/`-diff` CronJob pod logs. Verify S3 endpoint and credentials. |
| Backup job hanging | S3 unreachable | `activeDeadlineSeconds` (default 3600s) will terminate the job. Check S3 connectivity. |
| Split-brain detected in logs | Network partition | Follow the split-brain recovery runbook above. |

## Upgrade and migration

### Version model

Each chart is tagged `<chart>-<version>` (e.g. `pg-1.1.0`); `pg` and `pgvector` are released in lockstep — same version, same image, same agent. They **unified at `1.0.0`**: the earlier `0.5.x`/`0.6.x` split ended there and both charts now share a single version line. The `common` and `etcd` charts are vendored dependencies — they ship inside the `pg`/`pgvector` packages and are not released on their own.

### Compatibility matrix

| `pg` / `pgvector` | repmgr image | PostgreSQL | Kubernetes |
|-------------------|--------------|-----------|-----------|
| 1.11.0 *(current)* | `trixie-5.5.0-31` (`-pg18` / `-pg17`) | 18.x (default) or 17.x — see [Choosing the PostgreSQL major](#choosing-the-postgresql-major) | ≥ 1.21 (PDB `policy/v1`); ≥ 1.27 for the agent-mode PDB `unhealthyPodEvictionPolicy` |
| 1.10.1 – 1.10.2 | `trixie-5.5.0-30` | 18.x (default) or 17.x | as above |
| 1.8.1 – 1.10.0 | `trixie-5.5.0-29` | 18.x (default) or 17.x | as above |
| 1.5.0 – 1.8.0 | `trixie-5.5.0-28` | 18.x | as above |
| 1.2.6 – 1.4.x | `trixie-5.5.0-27` | 18.x | as above |
| 1.2.2 – 1.2.5 | `trixie-5.5.0-26` | 18.x | as above |
| 1.2.0 – 1.2.1 | `trixie-5.5.0-25` | 18.x | as above |
| 1.0.0 – 1.1.8 | `trixie-5.5.0-16` … `-24` | 18.x | as above |
| 0.5.88 / 0.6.90 *(last 0.x)* | `trixie-5.5.0-15` | 18.x | ≥ 1.21 (PDB `policy/v1`) |

Extras: agent monitoring (`repmgr.agent.monitoring.*`) needs the Prometheus Operator CRDs; the etcd backend (`repmgr.agent.dcs.backend: etcd`) needs an etcd ≥ 3.5 (BYO/shared) or the bundled etcd subchart (`etcd.enabled=true`).

### Routine upgrade (within 1.x)

```bash
helm repo update
helm upgrade my-postgres cagriekin/pg   # add -f your-values.yaml
```

Within the 1.x line the default is agent mode, and successive releases (e.g. `1.0.0` → `1.11.0`) are backward-compatible: `helm upgrade` rolls the pods once for the new image (`trixie-5.5.0-31` at 1.11.0) and the agent re-establishes leadership with no manual step. **Read every `Migrating from X.Y.Z` entry in [`CHANGELOG.md`](CHANGELOG.md) between your current version and the target** — some releases (credential, `pg_hba`, or image changes) carry one-time steps. The CHANGELOG keeps an unbroken trail back through the 0.x line.

### Crossing the 0.x → 1.x boundary (agent mode is now the default)

This applies only when upgrading **from a 0.x release**. Since `1.0.0` the default `failoverMode` is `agent` (it was `repmgrd` through 0.x). Three consumer-visible changes land at the boundary:

1. **Immutable `podManagementPolicy`.** Agent mode uses `Parallel` (repmgrd used `OrderedReady`), which cannot be changed in place. Adopting agent mode on an existing repmgrd install needs a one-time `kubectl delete statefulset <release>-pg --cascade=orphan` + `helm upgrade` recreate — full runbook and GitOps caveats in **[Migrating an existing release to agent mode](#migrating-an-existing-release-to-agent-mode)**.
2. **Hardened `pg_hba`.** Agent mode assembles a pod-CIDR + SCRAM `pg_hba.conf` with no implicit `0.0.0.0/0 md5` catch-all. If you relied on the broad md5 rule, add explicit `postgresql.pgHba` rules **before** switching.
3. **PDB default.** The postgresql PodDisruptionBudget defaults to `maxUnavailable: 1` + `unhealthyPodEvictionPolicy: AlwaysAllow` (was `minAvailable: 1`) — equivalent on a 2-pod cluster, strictly better for drains/upgrades on k8s ≥ 1.27.

To defer the move, pin `repmgr.failoverMode: repmgrd`: the legacy path stays supported for one major cycle (then deprecation per the policy below) and that upgrade needs no recreate.

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

If reads through the `my-postgres-pg-readonly` Service do not reach standbys, the problem is the `pg-role` labels rather than PGPool-II: the Service selects `pg-role: standby`, which the service-updater (repmgrd mode) or the agent (agent mode) re-applies every cycle, and pods stay absent from its endpoints until labeled (fresh installs, recreated or scaled-up pods).

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
| `role` | `primary` or `standby` as detected by the streaming replication check. If this disagrees with `repmgr cluster show`, restart PGPool-II. |
| `replication_delay` | Standby lag in bytes. |
| `select_cnt` | SELECT queries routed to the node; confirms load balancing is working. |

A recovered backend is reattached automatically when `pgpool.autoFailback` is `true` (default). Otherwise reattach it with `pcp_attach_node -h localhost -p 9898 -U admin <node-id>` or restart the Deployment.

### Recovering After Failover

With repmgr enabled the chart automates PGPool-II recovery:

1. The service-updater sidecar repoints the primary Service selector to the new primary pod.
2. On a primary change it runs `kubectl rollout restart deployment my-postgres-pg-pgpool`, so PGPool-II restarts with a fresh backend status file and rediscovers the topology.
3. The same sidecar probes PGPool-II through its Service every 30 seconds and forces a rollout restart after 3 consecutive failures.
4. Independently, the PGPool-II liveness probe restarts any instance that cannot serve queries for about 60 seconds.

If clients still reach a stale topology (for example writes failing with read-only errors), apply the manual equivalent:

```bash
kubectl rollout restart deployment my-postgres-pg-pgpool
```

Failover history is recorded as Kubernetes Events: on every primary change the service-updater emits a `PrimaryChanged` Event attached to the primary Service, and its container logs on the PostgreSQL pods carry the same transition:

```bash
kubectl get events --field-selector reason=PrimaryChanged
kubectl describe service my-postgres-pg
kubectl logs my-postgres-pg-0 -c service-updater | grep "Master change"
```

Events are pruned by the cluster's event TTL (one hour by default), so the service-updater logs are the longer-lived record.

### Logs

PGPool-II logs to stderr, so everything is available through the container logs:

```bash
kubectl logs deploy/my-postgres-pg-pgpool -c pgpool
```

Verbosity is controlled by the `pgpool.logging.*` values: `logConnections` (default `true`), `logStatement` (log every client query), `logPerNodeStatement` (log which backend each query was routed to), and `logMinMessages` (default `warning`; `debug1` and below add internal detail). Changing them rolls the Deployment automatically via the config checksum annotation.

| Message | Meaning |
|---------|---------|
| `failed to connect to PostgreSQL server` / `health check retrying` | A backend is unreachable. The node is marked `down` after `pgpool.healthCheck.maxRetries` retries (default 10, every 3 seconds). |
| `degenerate backend request ... is canceled because failover is disallowed` | Expected. All backends are flagged `DISALLOW_TO_FAILOVER` (or `ALWAYS_PRIMARY` without repmgr): repmgr owns failover, and the service-updater restarts PGPool-II afterwards instead of letting it detach nodes itself. |
| `all backend nodes are down` | No backend is reachable and clients are rejected. The liveness probe restarts PGPool-II, which retries discovery; if the message persists, check the PostgreSQL pods. |
| `authentication failed` / `password mismatch` | Remote clients authenticate with md5 against `pool_passwd`, which contains only the chart's PostgreSQL user. Other database users cannot authenticate through PGPool-II while `pgpool.allowClearTextFrontendAuth` is `false` (default); either connect them directly to PostgreSQL or set it to `true` so PGPool-II can request their password in clear text and forward it to the backend. |
