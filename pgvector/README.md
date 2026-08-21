# PostgreSQL with pgvector

PostgreSQL Helm chart with pgvector extension for vector similarity search, repmgr for automatic failover and replication management, optional PGPool-II for connection pooling and read/write splitting.

This chart shares all templates with the [pg chart](../pg/) via symlinks. The only differences are the default image (`pgvector/pgvector`) and automatic `CREATE EXTENSION IF NOT EXISTS vector` on startup.

## Features

- PostgreSQL 18 with pgvector extension for vector similarity search
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
- pgBackRest for WAL-based incremental backups and point-in-time recovery

## Installation

```bash
helm repo add cagriekin https://cagriekin.github.io/charts
helm install my-pgvector cagriekin/pgvector
```

### With Read Replicas

```bash
helm install my-pgvector cagriekin/pgvector --set postgresql.replicaCount=3
```

### With PGPool-II Enabled

```bash
helm install my-pgvector cagriekin/pgvector \
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

helm install my-pgvector cagriekin/pgvector \
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

helm install my-pgvector cagriekin/pgvector \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret \
  --set postgresql.existingSecret.usernameKey=user \
  --set postgresql.existingSecret.passwordKey=pass \
  --set postgresql.existingSecret.databaseKey=db \
  --set postgresql.existingSecret.repmgrPasswordKey=repmgr-pass
```

## Using pgvector

After installation, the vector extension is automatically created. You can start using vector types immediately:

```sql
-- Create a table with vector column
CREATE TABLE items (
  id SERIAL PRIMARY KEY,
  embedding vector(1536)
);

-- Insert vectors
INSERT INTO items (embedding) VALUES ('[1,2,3,...]');

-- Find similar vectors
SELECT * FROM items ORDER BY embedding <-> '[1,2,3,...]' LIMIT 5;
```

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
| `postgresql.image.repository` | PostgreSQL image repository | `pgvector/pgvector` |
| `postgresql.image.tag` | PostgreSQL image tag. Must bundle the same PostgreSQL point release as `repmgr.image.tag` (#302) — `copy-ext`'s no-clobber copy keeps a drifted image from corrupting the running server, but `CREATE EXTENSION vector` still needs a matching point release to load safely. Bump only in lockstep with `repmgr.image`. | `0.8.5-pg18-trixie` |
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
| `postgresql.persistence.emptyDir.sizeLimit` | `sizeLimit` for the non-persistent (`persistence.enabled=false`) PGDATA emptyDir; empty = unbounded | `""` |
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
| `postgresql.extraVolumes` | Extra pod-level volumes — mount a file identically on every replica (e.g. a pgsodium key); see the [pg chart README](../pg/README.md#mounting-an-extra-file-on-every-replica) | `[]` |
| `postgresql.extraVolumeMounts` | Extra mounts for the postgresql container; each must reference a `postgresql.extraVolumes` entry | `[]` |
| `postgresql.extraEnv` | Extra env vars for the postgresql container (supports `value` and `valueFrom`); may not reuse a chart-set name | `[]` |
| `postgresql.extensions.enabled` | Enable extensions support | `true` |
| `postgresql.extensions.packages` | Debian/PGDG packages to `apt-get install` before the copy step, for *additional* extensions beyond `vector` (`pg_cron`, …); `{major}` substitutes `postgresql.majorVersion`; see the [pg chart README](../pg/README.md#installing-extensions-without-a-custom-image) | `[]` |
| `postgresql.extensions.installResources` | Resources for the apt-get step (only rendered while `packages` is non-empty) | `100m/128Mi` req, `1/512Mi` limit |
| `postgresql.audit.enabled` | Enable pgaudit audit logging (requires repmgr mode; see [Audit logging](#audit-logging-pgaudit)) | `false` |
| `postgresql.audit.log` | pgaudit session classes: `read,write,function,role,ddl,misc,misc_set,all` (negate with `-`) | `"ddl, role, write"` |
| `postgresql.audit.logCatalog` | Audit `pg_catalog` statements | `false` |
| `postgresql.audit.logParameter` | Log statement parameter values (may contain PII/secrets) | `false` |
| `postgresql.audit.logRelation` | Log the fully-qualified relation per affected table | `false` |
| `postgresql.audit.role` | Optional `pgaudit.role` for object-level auditing (empty = session-only) | `""` |
| `postgresql.lifecycle.postStart.additionalCommands` | Shell commands to run after PostgreSQL is ready | pgvector CREATE EXTENSION |
| `postgresql.migrateLegacyMd5Users` | Re-hash MD5 user passwords to SCRAM on PG14+ | `true` |
| `postgresql.nodeSelector` | Node selector for PostgreSQL pods | `{}` |
| `postgresql.tolerations` | Tolerations for PostgreSQL pods | `[]` |
| `postgresql.topologySpreadConstraints` | Spread constraints added alongside the built-in affinity (e.g. a hard zone spread) | `[]` |
| `postgresql.serviceAccount.annotations` | Annotations on the postgresql pods' ServiceAccount (cloud workload identity for keyless pgBackRest S3) | `{}` |
| `postgresql.walLevel` | `replica` or `logical` (#308). The only place to set `wal_level` -- see the [pg chart README](../pg/README.md#logical-replication-308) | `replica` |

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

### Repmgr Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `repmgr.enabled` | Enable repmgr | `true` |
| `repmgr.image.repository` | Repmgr image repository | `cagriekin/repmgr` |
| `repmgr.image.tag` | Repmgr image tag. Unsuffixed = the default major (18); `-pg18` / `-pg17` select one explicitly | `trixie-5.5.0-32` |
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
| `repmgr.serviceUpdater.resources.requests.cpu` | Service-updater CPU request | `50m` |
| `repmgr.serviceUpdater.resources.requests.memory` | Service-updater memory request | `64Mi` |
| `repmgr.serviceUpdater.resources.limits.memory` | Service-updater memory limit | `128Mi` |
| `repmgr.initContainerResources` | Resources for the `repmgr-init` standby-clone init container (raise for large databases) | `requests: 100m/128Mi, limits: 1/1Gi` |
| `repmgr.splitBrainDetection.action` | Action on split-brain: `log` (alert only) or `fence` (terminate stale primary) | `log` |

When repmgr is enabled, a preStop lifecycle hook stops PostgreSQL cleanly (`pg_ctl stop -m fast`) before pod termination. If the terminated pod was the primary, repmgrd on a standby detects the outage and promotes via its `promote_command`, which also updates repmgr metadata; the hook deliberately does not promote out-of-band, since a raw `pg_promote()` would leave repmgr.nodes stale and strand every repmgrd. The `terminationGracePeriodSeconds` controls how long Kubernetes waits for the shutdown to complete.

When repmgr is enabled, two sidecars run alongside PostgreSQL in each pod:

- **repmgrd**: monitors replication and triggers automatic failover when the primary becomes unavailable
- **service-updater**: watches repmgr cluster state and patches the Kubernetes Service selector to point to the current primary, then restarts PGPool-II if enabled. Also maintains a `pg-role` label (`primary`/`standby`) on every postgresql pod each cycle, which the `<fullname>-readonly` service selects (`pg-role: standby`) to route read traffic to replicas

### Choosing the PostgreSQL major

**PostgreSQL 18 is the default. PostgreSQL 17 is selectable.** In repmgr mode the server binaries come from the repmgr image (shared with the `pg` chart), so the major is decided by which repmgr image you run — `postgresql.image` has no effect on the running server. The image is published unsuffixed (= 18) plus `-pg18` / `-pg17`; all are multi-arch, attested and cosign-signed.

Four values move together, and the chart refuses to render if the two majors disagree in either direction:

```yaml
postgresql:
  majorVersion: "17"
  image:
    tag: pg17-trixie           # the pgvector image for the SAME major
repmgr:
  image:
    tag: trixie-5.5.0-32-pg17
    majorVersion: "17"
```

**`postgresql.image.tag` matters here even in repmgr mode** — unlike in the `pg` chart. This chart ships `postgresql.extensions.enabled=true`, and the `copy-ext` init container copies `/usr/lib/postgresql/<major>/lib` and `/usr/share/postgresql/<major>/extension` **out of the pgvector image** into the server container, which is how `vector` reaches a server that runs from the repmgr image. Those paths are built from `postgresql.majorVersion`, so the pgvector image must be the matching major (`pgvector/pgvector:pg17-trixie`) or the copy finds nothing and `CREATE EXTENSION vector` fails.

As in the `pg` chart, a `-pgNN` tag that disagrees with `repmgr.image.majorVersion` fails the render, and `PG_MAJOR` is passed to the containers running the repmgr image so an unsuffixed-tag mismatch is refused at startup rather than running the wrong major.

This is a **create-time** choice: the chart has no in-place major upgrade, so moving an existing cluster between majors means a logical dump/restore into a fresh release. Note that repmgr 5.5.0's upstream install requirements list PostgreSQL 13–17, not 18 — select 17 if you need an upstream-sanctioned pairing. For the full rationale, the tag table, and the `pg_dump` considerations, see the [pg chart README — Choosing the PostgreSQL major](../pg/README.md#choosing-the-postgresql-major).

### Failover modes: lease-based `agent` (default) and legacy `repmgrd`

`repmgr.failoverMode` selects how failover is decided:

- **`agent`** (default since `1.0.0`): a Go agent (`pg-ha-agent`) runs as PID 1 in the postgresql container and holds a Kubernetes `coordination.k8s.io/v1` Lease (`<fullname>-leader`) as the **sole authority** for which pod is primary, driving repmgr as a pure mechanism (`failover=manual`, no repmgrd).
- **`repmgrd`** (legacy, opt-in): the repmgrd + service-updater sidecars described above. Unchanged behavior; supported for one major cycle (deprecated). Pin `repmgr.failoverMode: repmgrd` to stay on it.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `repmgr.failoverMode` | `agent` or `repmgrd` | `agent` |
| `repmgr.agent.leaseDuration` | Lease TTL | `15s` |
| `repmgr.agent.renewDeadline` | Holder self-demotes if it cannot renew within this | `10s` |
| `repmgr.agent.retryPeriod` | Lease acquire/renew retry interval | `2s` |
| `repmgr.agent.reconcileInterval` | Reconcile tick interval | `5s` |
| `repmgr.agent.podCidr` | Pod CIDR trusted in the agent's hardened SCRAM-only pg_hba (no `0.0.0.0/0 md5`); set to your cluster's pod CIDR if outside `10.0.0.0/8` | `10.0.0.0/8` |

Must satisfy `leaseDuration > renewDeadline > retryPeriod`; widen for managed clouds (e.g. `30s/20s/4s`). This chart shares pg's templates and agent — see the [pg chart README](../pg/README.md#failover-modes-lease-based-agent-default-and-legacy-repmgrd) for the full agent-mode behavior and the **migration runbook** (the immutable `podManagementPolicy` change requires a one-time `kubectl delete statefulset --cascade=orphan` + `helm upgrade`). See `ENVIRONMENT.md` for the injected-variable catalog.

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
| `pgpool.service.exposePcp` | Expose the PgPool-II PCP admin port (9898) on the Service. Off by default; enable only if you run `pcp_*` against the Service (and add a `pgpool.extraIngress` rule for 9898 under NetworkPolicy). | `false` |
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

## Connecting to PostgreSQL

### Direct Connection to Primary

```bash
kubectl port-forward svc/my-pgvector 5432:5432
psql -h localhost -U postgres -d postgres
```

### Read-Only Connection to Replicas

When repmgr is enabled, a `<fullname>-readonly` service routes only to standby pods, selected via the `pg-role: standby` label. In repmgrd mode the service-updater sidecar maintains the label; in agent mode the agent does, with a 3-way classification (in-recovery -> `standby`; reachable-but-not-in-recovery -> `orphan`, kept OUT of the read pool; unreachable -> left untouched):

```bash
kubectl port-forward svc/my-pgvector-readonly 5432:5432
psql -h localhost -U postgres -d postgres
```

With `postgresql.replicaCount: 0` the service exists but has no endpoints.

> **NetworkPolicy note.** The read-only Service connects clients *directly* to standbys
> on 5432, so when `networkPolicy.enabled: true` with
> `networkPolicy.postgresql.allowExternal: false`, these read connections are blocked
> (PGPool on 9999 stays reachable, so read-write clients via PGPool are unaffected).
> Re-allow your read clients with a scoped `networkPolicy.postgresql.extraIngress` rule
> on port 5432 (add a `namespaceSelector` for cross-namespace clients).

### Through PGPool-II

```bash
kubectl port-forward svc/my-pgvector-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres
```

## Audit logging (pgaudit)

Opt-in, [pgaudit](https://github.com/pgaudit/pgaudit)-based audit logging for compliance
regimes (SOC 2, HIPAA, PCI-DSS, ISO 27001). Off by default — inherited unchanged from the
pg chart's symlinked templates.

```yaml
postgresql:
  audit:
    enabled: true
    log: "ddl, role, write"
    role: ""   # optional pgaudit.role for object-level auditing
```

When enabled, the chart adds `pgaudit` to `shared_preload_libraries` (preserving `repmgr`),
renders the `pgaudit.*` GUCs, and creates the extension idempotently on the primary via a
post-install/upgrade hook Job.

- **Requires `repmgr.enabled: true`** — the `cagriekin/repmgr` image bundles pgaudit;
  standalone mode uses the stock `postgres` image (no pgaudit) and fails a render guard.
- **Enabling audit restarts PostgreSQL** (`shared_preload_libraries` is a postmaster
  parameter) via the config-checksum rolling restart — no manual step.
- **Classes** (`log`): `read,write,function,role,ddl,misc,misc_set,all`, each negatable with
  a leading `-`. **Retention** is your platform's job: pgaudit writes to the server log
  (stderr → `kubectl logs`); ship it to Loki/ELK/CloudWatch with immutable retention.

See the [pg chart README](../pg/README.md#audit-logging-pgaudit) for full detail.

## Replication Management

Repmgr manages replication automatically. To check cluster status:

```bash
kubectl exec -it my-pgvector-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

### Scaling down

Scaling `postgresql.replicaCount` **down** removes the highest-ordinal pods. The primary
now **automatically unregisters** the removed nodes from `repmgr.nodes` (#139), so
`repmgr cluster show` no longer lists them as failed and failover elections do not retry
the gone DNS names. Reconciliation is keyed on the ordinal (node id = `ordinal + 1000`),
never on reachability, so a momentarily-down live node is never unregistered; cleanup
completes within ~a minute of the rolled pods settling. If the removed node was the
*current primary*, unregister it by hand (`repmgr standby unregister` refuses primary
rows). See the [pg chart README — Scaling down](../pg/README.md#scaling-down) for detail.

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
          app.kubernetes.io/name: pgvector
          app.kubernetes.io/instance: my-pgvector
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
              app.kubernetes.io/name: pgvector
              app.kubernetes.io/instance: my-pgvector
              app.kubernetes.io/component: postgresql
          topologyKey: topology.kubernetes.io/zone
```

Setting `postgresql.affinity` replaces the entire built-in affinity block, including the hostname rule. With a required zone rule that is harmless (distinct zones imply distinct nodes), but any other custom affinity should re-add the hostname rule explicitly. A required zone rule also caps the cluster size: total instances (`replicaCount + 1`) must not exceed the number of zones or the surplus pods stay Pending.

`postgresql.topologySpreadConstraints` (default `[]`) adds spread constraints alongside the built-in affinity without replacing it — set a hard zone spread (`whenUnsatisfiable: DoNotSchedule`) here to keep the hostname anti-affinity intact. PGPool-II supports `pgpool.topologySpreadConstraints` (as in the example above) and, like PostgreSQL, has a default hostname anti-affinity that `pgpool.affinity` replaces wholesale.

### Cloud preset (`values-cloud.yaml`)

The chart ships an opt-in `values-cloud.yaml` overlay with opinionated multi-AZ production settings, so you do not have to assemble them by hand:

```bash
helm install my-pgvector cagriekin/pgvector -f values-cloud.yaml [-f your-values.yaml]
```

It sets `replicaCount: 2` (3 instances), a hard `DoNotSchedule` zone spread, a `WaitForFirstConsumer` `storageClass` placeholder, and the managed-cloud agent lease timings (`30s`/`20s`/`4s`). Do not use it on single-zone / kind / dev clusters — the hard spread leaves pods Pending when there are fewer schedulable zones than replicas. The base `values.yaml` stays dev/CI-friendly; this preset is the production opt-in.

### Storage Classes

Use a storage class with `volumeBindingMode: WaitForFirstConsumer`. It delays PV provisioning until the pod is scheduled, so each volume is created in the zone the scheduler picked. With `Immediate` binding the PV is provisioned first, in an arbitrary zone, and the pod may become unschedulable when that zone conflicts with the affinity rules.

Cloud block volumes are zonal, which pins each instance to its volume's zone permanently: after a zone outage, pods from that zone cannot reschedule elsewhere (availability relies on repmgr promoting a standby in a surviving zone), and relocating a standby requires deleting its PVC together with the pod so it re-provisions and re-clones in the new zone.

With repmgr enabled, the `<fullname>-readonly` service (see [Read-Only Connection to Replicas](#read-only-connection-to-replicas)) selects all standby pods, so read traffic is distributed across the standbys in every zone.

## Prometheus Exporter

This chart includes an optional PostgreSQL metrics exporter for Prometheus monitoring. The exporter runs as a single instance and can scrape metrics from all PostgreSQL instances (primary and replicas) using the multi-target pattern.

> **Cross-namespace scraping with NetworkPolicy.** When `networkPolicy.enabled: true`,
> the metric-port ingress rules (exporter 9116, pgpool 9719, agent 9200) admit
> same-namespace pods only. A Prometheus in a separate monitoring namespace must be
> allowed via a `namespaceSelector` — use `networkPolicy.prometheusExporter.extraIngress`
> for 9116 (and `networkPolicy.pgpool.extraIngress` / `networkPolicy.postgresql.extraIngress`
> for 9719 / 9200), e.g.:
>
> ```yaml
> networkPolicy:
>   prometheusExporter:
>     extraIngress:
>       - ports: [{ port: 9116, protocol: TCP }]
>         from:
>           - namespaceSelector:
>               matchLabels:
>                 kubernetes.io/metadata.name: monitoring
> ```

### Enable Exporter

```bash
helm install my-pgvector cagriekin/pgvector \
  --set prometheusExporter.enabled=true \
  --set postgresql.replicaCount=3
```

### With ServiceMonitor (Prometheus Operator)

```bash
helm install my-pgvector cagriekin/pgvector \
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
        - my-pgvector-0.my-pgvector-headless.default.svc.cluster.local:5432
        - my-pgvector-1.my-pgvector-headless.default.svc.cluster.local:5432
        - my-pgvector-2.my-pgvector-headless.default.svc.cluster.local:5432
    metrics_path: /probe
    params:
      auth_module: [postgres]
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: my-pgvector-postgres-exporter.default.svc.cluster.local:9116
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

Unlike the counters above, `pg_wal_size_bytes` reflects actual bytes on disk regardless of *why* WAL is being retained. It comes from the exporter's own **built-in** `wal` collector, not a chart-defined query (see the pg chart README for why -- a chart-defined query under the same metric name collided with it and broke the entire scrape):

| Metric | Description |
|--------|-------------|
| `pg_wal_size_bytes` | Total bytes currently used by `pg_wal` on disk |
| `pg_wal_segments` | Number of WAL segments currently in `pg_wal` |

`pg_wal` shares the single PGDATA volume (`postgresql.persistence.size`) — there is no separate WAL volume/tablespace. **This chart ships observability for that condition only** — a shipped alert (below) — not an automatic write-throttle or backpressure mechanism.

### WAL Alert Rules

Set `prometheusExporter.prometheusRule.enabled: true` to ship a `PrometheusRule` (requires the Prometheus Operator CRDs). This is a no-op unless `prometheusExporter.serviceMonitor.enabled` (or an equivalent scrape) is also on.

| Alert | Condition | Requires |
|-------|-----------|----------|
| `PGWALArchiveFailing` | `rate(pg_wal_archive_failed_count[5m]) > 0` for `archiveFailingFor` (default `5m`) | `pgbackrest.enabled` |
| `PGWALArchiveStale` | `pg_wal_archive_seconds_since_last_archived > staleArchiveSeconds` for `archiveStaleFor` (default `15m`) | `pgbackrest.enabled` |
| `PGWALSizeHigh` | `pg_wal_size_bytes > walSizeBytesThreshold` for `sizeHighFor` (default `15m`) | — |

See pg's README for the full alert-tuning discussion.

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

Automated database backups can be enabled to run `pg_dump` on a schedule and upload compressed dumps to S3-compatible storage (AWS S3, MinIO, Wasabi, etc.). The backup job connects to the primary via the main service, so it works correctly with repmgr failover.

### Enable Backup

```bash
kubectl create secret generic s3-backup-creds \
  --from-literal=access-key-id=YOUR_ACCESS_KEY \
  --from-literal=secret-access-key=YOUR_SECRET_KEY

helm install my-pgvector cagriekin/pgvector \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://minio.example.com \
  --set backup.s3.bucket=pgvector-backups \
  --set backup.existingSecret.name=s3-backup-creds
```

### Manual Trigger

```bash
kubectl create job --from=cronjob/my-pgvector-backup manual-backup
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
release's own backups, then restore the chosen one (replace `<release>-pgvector`
with your release's fullname):

```bash
mc ls s3/pgvector-backups/backups/<release>-pgvector/
mc cp s3/pgvector-backups/backups/<release>-pgvector/backup_20250101_020000.dump /tmp/backup.dump
pg_restore -h localhost -U postgres -d postgres /tmp/backup.dump
```

Dumps taken before the per-release-path change live at the **old flat path**
`s3/<bucket>/<prefix>/backup_*.dump` (no `<release-fullname>/` segment). They are not
migrated and are no longer covered by automatic retention, so list and restore them
directly there (`mc ls s3/pgvector-backups/backups/`), and delete them manually once obsolete.

## pgBackRest (PITR)

pgBackRest provides WAL-based incremental backups for point-in-time recovery. When enabled, WAL segments are continuously archived from the primary to S3, and scheduled full/differential backups run automatically. This allows restoring the database to any point in time within the retention window.

Requires `repmgr.enabled: true` (pgBackRest is installed in the repmgr image).

### Enable pgBackRest

```bash
kubectl create secret generic s3-backup-creds \
  --from-literal=access-key-id=YOUR_ACCESS_KEY \
  --from-literal=secret-access-key=YOUR_SECRET_KEY

helm install my-pgvector cagriekin/pgvector \
  --set pgbackrest.enabled=true \
  --set pgbackrest.s3.endpoint=https://s3.eu-central-1.amazonaws.com \
  --set pgbackrest.s3.bucket=pgvector-backups \
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
| `pgbackrest.validation.enabled` | Enable the automated PITR restore-validation CronJob (#38) — restores the repo into a throwaway PostgreSQL, replays WAL, validates, exits. See the [pg chart README](../pg/README.md#automated-pitr-restore-validation-38) | `false` |
| `pgbackrest.validation.schedule` | Cron schedule for the validation job | `` `0 4 * * 0` `` |
| `pgbackrest.validation.targetType` | PITR target type (`pgbackrest --type`): `""` (latest) \| `time` \| `xid` \| `name` \| `lsn`; `target` required when set | `""` |
| `pgbackrest.validation.target` | PITR target value for the type above | `""` |
| `pgbackrest.validation.recoveryTimeout` | Seconds `pg_ctl` waits for WAL replay + promotion before failing the Job | `1800` |
| `pgbackrest.validation.workdirSizeLimit` | `sizeLimit` for the throwaway restored-PGDATA emptyDir; empty = node-disk bounded | `""` |
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

Verified end to end by the pg chart's `test-pgbackrest-bootstrap` suite, which deletes replica 0's PVC
outright and asserts the *same* cluster returns — the PostgreSQL system identifier is unchanged,
which a fresh `initdb` could not produce — then restarts the pod and asserts the bootstrap does
**not** run a second time.

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
kubectl exec -it my-pgvector-0 -- pgbackrest --stanza=db info
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
kubectl scale statefulset my-pgvector --replicas=0
kubectl wait --for=delete pod/my-pgvector-0 --timeout=5m

# 2. Restore (stanza = pgbackrest.stanza, default "db").
kubectl create job --from=cronjob/my-pgvector-pgbackrest-restore restore-now
kubectl wait --for=condition=complete job/restore-now --timeout=30m

# 3. CONFIRM the restore succeeded -- must print 1. Do not continue otherwise.
#    (`--for=condition=complete` above only ever succeeds, so a failed attempt leaves it
#    blocked until the timeout instead of returning; if it seems stuck, check here.)
kubectl get job restore-now -o jsonpath='{.status.succeeded}{"\n"}'

# 4. Only now scale back up: the pods replay the archived WAL and promote.
kubectl scale statefulset my-pgvector --replicas=2
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
helm get values my-pgvector > /tmp/v.yaml
helm template my-pgvector cagriekin/pgvector -f /tmp/v.yaml \
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
kubectl scale statefulset my-pgvector --replicas=1              # remove the standby pod
kubectl delete pvc data-my-pgvector-1                          # returns once it is really gone
kubectl scale statefulset my-pgvector --replicas=2              # clones fresh from the primary
```

If the cluster **crashed** rather than being scaled down cleanly, a stale
`postmaster.pid` is left in PGDATA and pgbackrest refuses to restore — the interlock that
lets this Job run with no Kubernetes API access at all. Confirm every postgresql pod is
gone, then set `pgbackrest.restore.force=true`.

#### Agent mode: clear the stale leadership state before scaling up

In agent mode the Lease and the highwater-marker ConfigMap survive the scale-to-0
but describe the *pre-restore* cluster, so a leftover marker makes the agents
refuse to promote onto the rewound (lower) timeline. Before scaling back up:

```bash
kubectl delete lease     my-pgvector-leader   -n <ns> --ignore-not-found
kubectl delete configmap my-pgvector-primary  -n <ns> --ignore-not-found
```

The same applies to a major-version (`pg_upgrade`) upgrade, which is a primary-first
manual operation: pause the agent first (`kubectl annotate configmap
my-pgvector-primary pg-ha/pause=true --overwrite`) so it does not fail over
mid-upgrade, then resume. See the pg chart's README (Point-in-Time Recovery and
major-version upgrade sections) for the full agent-mode runbook.

### Upgrading Existing Clusters

Enabling pgBackRest on an existing cluster sets `archive_mode = on` in postgresql.conf. This change requires a PostgreSQL restart. The pods will restart automatically on the next helm upgrade since the StatefulSet spec changes, but `archive_mode` only takes effect after the restart.

## Upgrade and migration

```bash
helm repo update
helm upgrade my-pgvector cagriekin/pgvector   # add -f your-values.yaml
```

`pgvector` tracks `pg` in lockstep — same version, image, and agent; the earlier 0.6.x ↔ 0.5.x split unified at `1.0.0` (current: `1.13.1`, image `trixie-5.5.0-32`). Within the 1.x line `helm upgrade` rolls the pods once and needs no manual step. The default failover mode is `agent` since `1.0.0` (it was `repmgrd` through 0.x); **only when crossing from a 0.x release** does adopting the agent default need the one-time `--cascade=orphan` recreate — pin `repmgr.failoverMode: repmgrd` to defer. Read the `Migrating from X.Y.Z` entries in [`CHANGELOG.md`](CHANGELOG.md) between your version and the target. For the **compatibility matrix, the version model, and the full 0.x → 1.x migration runbook**, see the [pg chart README — Upgrade and migration](../pg/README.md#upgrade-and-migration) (this chart shares pg's templates and agent).

## pgvector Resources

- [pgvector GitHub](https://github.com/pgvector/pgvector)
- [pgvector Documentation](https://github.com/pgvector/pgvector#getting-started)

## Troubleshooting PGPool

### Connectivity: PGPool-II or the Backend?

When clients cannot connect, isolate the failing layer first. Query through PGPool-II:

```bash
kubectl port-forward svc/my-pgvector-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres -c "SELECT 1"
```

Then bypass PGPool-II and query the primary Service directly:

```bash
kubectl port-forward svc/my-pgvector 5432:5432
psql -h localhost -p 5432 -U postgres -d postgres -c "SELECT 1"
```

If only the PGPool-II path fails, check backend status and logs below. If both fail, troubleshoot PostgreSQL itself first.

Check that the Services have endpoints (`my-pgvector-readonly` exists when repmgr is enabled):

```bash
kubectl get endpoints my-pgvector my-pgvector-pgpool my-pgvector-readonly
```

The PGPool-II readiness probe runs `SELECT 1` through port 9999 rather than a TCP check, so PGPool-II pods turn unready and drop out of the Service whenever they cannot serve queries from at least one backend. Empty `my-pgvector-pgpool` endpoints therefore usually point at a backend or authentication problem, not at the Service. Restarts of the pgpool Deployment pods have the same root cause: the liveness probe runs the same query and restarts a wedged PGPool-II after about 60 seconds.

If reads through the `my-pgvector-readonly` Service do not reach standbys, the problem is the `pg-role` labels rather than PGPool-II: the Service selects `pg-role: standby`, which the service-updater re-applies every cycle, and pods stay absent from its endpoints until labeled (fresh installs, recreated or scaled-up pods).

### Checking Backend Status

`SHOW pool_nodes` through port 9999 reports each backend as PGPool-II sees it. `pool_hba.conf` trusts local connections inside the pod, so no password is needed:

```bash
kubectl exec -it deploy/my-pgvector-pgpool -c pgpool -- \
  sh -c 'psql -h 127.0.0.1 -p 9999 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SHOW pool_nodes;"'
```

The PCP admin interface on port 9898 (exposed on the pgpool Service only when `pgpool.service.exposePcp=true`) provides the same data. It authenticates against `pcp.conf`, which the init container generates from the admin Secret: the chart-managed `my-pgvector-pgpool-admin` (keys `username`/`password`, populated from `pgpool.admin.username`/`pgpool.admin.password`), or your own Secret when `pgpool.admin.existingSecret.enabled` is set. Retrieve the password, then run the pcp commands (they prompt for it):

```bash
kubectl get secret my-pgvector-pgpool-admin -o jsonpath='{.data.password}' | base64 -d
kubectl exec -it deploy/my-pgvector-pgpool -c pgpool -- pcp_node_count -h localhost -p 9898 -U admin
kubectl exec -it deploy/my-pgvector-pgpool -c pgpool -- pcp_node_info -h localhost -p 9898 -U admin 0
```

Changing the chart-managed credentials rolls the Deployment via the Secret checksum annotation; rotating an existing Secret requires `kubectl rollout restart deployment my-pgvector-pgpool`, because `pcp.conf` is generated at pod start.

Node IDs follow the StatefulSet ordinals: node 0 is `my-pgvector-0`, node 1 is `my-pgvector-1`, and so on.

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
2. On a primary change it runs `kubectl rollout restart deployment my-pgvector-pgpool`, so PGPool-II restarts with a fresh backend status file and rediscovers the topology.
3. The same sidecar probes PGPool-II through its Service every 30 seconds and forces a rollout restart after 3 consecutive failures.
4. Independently, the PGPool-II liveness probe restarts any instance that cannot serve queries for about 60 seconds.

If clients still reach a stale topology (for example writes failing with read-only errors), apply the manual equivalent:

```bash
kubectl rollout restart deployment my-pgvector-pgpool
```

Failover history is recorded as Kubernetes Events: on every primary change the service-updater emits a `PrimaryChanged` Event attached to the primary Service, and its container logs on the PostgreSQL pods carry the same transition:

```bash
kubectl get events --field-selector reason=PrimaryChanged
kubectl describe service my-pgvector
kubectl logs my-pgvector-0 -c service-updater | grep "Master change"
```

Events are pruned by the cluster's event TTL (one hour by default), so the service-updater logs are the longer-lived record.

### Logs

PGPool-II logs to stderr, so everything is available through the container logs:

```bash
kubectl logs deploy/my-pgvector-pgpool -c pgpool
```

Verbosity is controlled by the `pgpool.logging.*` values: `logConnections` (default `true`), `logStatement` (log every client query), `logPerNodeStatement` (log which backend each query was routed to), and `logMinMessages` (default `warning`; `debug1` and below add internal detail). Changing them rolls the Deployment automatically via the config checksum annotation.

| Message | Meaning |
|---------|---------|
| `failed to connect to PostgreSQL server` / `health check retrying` | A backend is unreachable. The node is marked `down` after `pgpool.healthCheck.maxRetries` retries (default 10, every 3 seconds). |
| `degenerate backend request ... is canceled because failover is disallowed` | Expected. All backends are flagged `DISALLOW_TO_FAILOVER` (or `ALWAYS_PRIMARY` without repmgr): repmgr owns failover, and the service-updater restarts PGPool-II afterwards instead of letting it detach nodes itself. |
| `all backend nodes are down` | No backend is reachable and clients are rejected. The liveness probe restarts PGPool-II, which retries discovery; if the message persists, check the PostgreSQL pods. |
| `authentication failed` / `password mismatch` | Remote clients authenticate with md5 against `pool_passwd`, which contains only the chart's PostgreSQL user. Other database users cannot authenticate through PGPool-II while `pgpool.allowClearTextFrontendAuth` is `false` (default); either connect them directly to PostgreSQL or set it to `true` so PGPool-II can request their password in clear text and forward it to the backend. |
