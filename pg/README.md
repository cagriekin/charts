# PostgreSQL with repmgr and PGPool-II

PostgreSQL Helm chart with repmgr for automatic failover and replication management, optional PGPool-II for connection pooling and read/write splitting.

## Features

- PostgreSQL 18.1 with configurable version
- Repmgr for automatic failover and replication management
- Service-updater sidecar for automatic primary service selector updates after failover
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

### PostgreSQL Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.image.repository` | PostgreSQL image repository | `postgres` |
| `postgresql.image.tag` | PostgreSQL image tag | `18.1-trixie` |
| `postgresql.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `postgresql.majorVersion` | PostgreSQL major version in `image.tag`; builds the extension paths (`/usr/lib/postgresql/<major>/lib`, `/usr/share/postgresql/<major>/extension`) when `extensions.enabled=true` | `"18"` |
| `postgresql.replicaCount` | Number of PostgreSQL replicas (total instances = replicaCount + 1); values > 0 require `repmgr.enabled=true` | `1` |
| `postgresql.database` | Database name | `postgres` |
| `postgresql.username` | Database username | `postgres` |
| `postgresql.resources.requests.cpu` | CPU request | `100m` |
| `postgresql.resources.requests.memory` | Memory request | `256Mi` |
| `postgresql.resources.limits.cpu` | CPU limit | `1000m` |
| `postgresql.resources.limits.memory` | Memory limit | `1Gi` |
| `postgresql.persistence.enabled` | Enable persistence | `true` |
| `postgresql.persistence.size` | Storage size | `10Gi` |
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

### Pod Disruption Budgets

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.podDisruptionBudget.enabled` | Enable PDB for PostgreSQL | `true` |
| `postgresql.podDisruptionBudget.minAvailable` | Minimum available pods | `1` |
| `pgpool.podDisruptionBudget.enabled` | Enable PDB for PGPool-II | `true` |
| `pgpool.podDisruptionBudget.minAvailable` | Minimum available pods | `1` |

### PostgreSQL Configuration

Runtime configuration can be injected without rebuilding images. Settings are written to a ConfigMap, mounted into the pod, and loaded via `include_dir`.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.configuration` | Map of postgresql.conf parameters | `{}` |
| `postgresql.pgHba` | List of pg_hba.conf entries injected via postStart | `[]` |
| `postgresql.extensions.enabled` | Enable extensions support | `false` |
| `postgresql.lifecycle.postStart.additionalCommands` | Shell commands to run after PostgreSQL is ready | `""` |
| `postgresql.migrateLegacyMd5Users` | Re-hash MD5 user passwords to SCRAM on PG14+ | `true` |
| `postgresql.nodeSelector` | Node selector for PostgreSQL pods | `{}` |
| `postgresql.tolerations` | Tolerations for PostgreSQL pods | `[]` |

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
| `repmgr.image.tag` | Repmgr image tag | `trixie-5.5.0-7` |
| `repmgr.image.pullPolicy` | Image pull policy | `IfNotPresent` |
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

When repmgr is enabled, a preStop lifecycle hook stops PostgreSQL cleanly (`pg_ctl stop -m fast`) before pod termination. If the terminated pod was the primary, repmgrd on a standby detects the outage and promotes via its `promote_command`, which also updates repmgr metadata; the hook deliberately does not promote out-of-band, since a raw `pg_promote()` would leave repmgr.nodes stale and strand every repmgrd. The `terminationGracePeriodSeconds` controls how long Kubernetes waits for the shutdown to complete.

When repmgr is enabled, two sidecars run alongside PostgreSQL in each pod:

- **repmgrd**: monitors replication and triggers automatic failover when the primary becomes unavailable. Has a preStop hook that runs `repmgr daemon stop` for clean deregistration.
- **service-updater**: watches repmgr cluster state and patches the Kubernetes Service selector to point to the current primary, then restarts PGPool-II if enabled. Also maintains a `pg-role` label (`primary`/`standby`) on every postgresql pod each cycle, which the `<fullname>-readonly` service selects (`pg-role: standby`) to route read traffic to replicas; pods without the label (fresh, recreated or scaled-up) are excluded until labeled. Has a preStop hook that sleeps 5s to allow in-flight patches to complete. Includes a liveness probe that checks for a heartbeat file updated each loop iteration (fails if no update within 120s). Performs split-brain detection each cycle by querying all nodes for `pg_is_in_recovery()` -- if multiple primaries are found, takes the configured action (`log` or `fence`).

**Split-brain detection**: In a 2-node cluster, network partitions can cause both nodes to believe they are the primary. The service-updater detects this by checking all nodes each monitoring cycle. With `action: log` (default), it logs a critical warning. With `action: fence`, it compares WAL LSN positions and terminates connections on the stale primary. For production deployments, use 3+ nodes to reduce split-brain risk.

### PGPool-II Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.enabled` | Enable PGPool-II | `false` |
| `pgpool.image.repository` | PGPool-II image repository | `cagriekin/pgpool` |
| `pgpool.image.tag` | PGPool-II image tag | `4.7.1` |
| `pgpool.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `pgpool.replicaCount` | Number of PGPool-II instances | `1` |
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
| `networkPolicy.postgresql.allowExternal` | Allow ingress to PostgreSQL from any pod in the namespace | `true` |
| `networkPolicy.postgresql.extraIngress` | Additional ingress rules for PostgreSQL | `[]` |
| `networkPolicy.postgresql.extraEgress` | Additional egress rules for PostgreSQL | `[]` |
| `networkPolicy.pgpool.extraIngress` | Additional ingress rules for PGPool-II | `[]` |
| `networkPolicy.pgpool.extraEgress` | Additional egress rules for PGPool-II | `[]` |

When enabled, NetworkPolicies restrict traffic:
- **PostgreSQL**: ingress on 5432 from peer pods, PGPool, Prometheus exporter, backup jobs, and optionally all namespace pods. Egress allows DNS, peer replication, 443 and 6443 (S3 over HTTPS and the Kubernetes API server), and the port of `pgbackrest.s3.endpoint` when pgBackRest is enabled.
- **PGPool**: ingress on 9999 from namespace pods. Egress only to PostgreSQL on 5432.
- **Prometheus exporter**: ingress on 9116 from namespace pods. Egress only to PostgreSQL on 5432.

## Connecting to PostgreSQL

### Direct Connection to Primary

```bash
kubectl port-forward svc/my-postgres 5432:5432
psql -h localhost -U postgres -d postgres
```

### Read-Only Connection to Replicas

When repmgr is enabled, a `<fullname>-readonly` service routes only to standby pods (selected via the `pg-role: standby` label maintained by the service-updater sidecar):

```bash
kubectl port-forward svc/my-postgres-readonly 5432:5432
psql -h localhost -U postgres -d postgres
```

With `postgresql.replicaCount: 0` the service exists but has no endpoints.

### Through PGPool-II

```bash
kubectl port-forward svc/my-postgres-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres
```

## Replication Management

Repmgr manages replication automatically. To check cluster status:

```bash
kubectl exec -it my-postgres-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

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

There is no `postgresql.topologySpreadConstraints` value; zone placement of PostgreSQL pods is controlled through affinity only. PGPool-II supports `pgpool.topologySpreadConstraints` (as in the example above) and, like PostgreSQL, has a default hostname anti-affinity that `pgpool.affinity` replaces wholesale.

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
        - my-postgres-0.my-postgres-headless.default.svc.cluster.local:5432
        - my-postgres-1.my-postgres-headless.default.svc.cluster.local:5432
        - my-postgres-2.my-postgres-headless.default.svc.cluster.local:5432
    metrics_path: /probe
    params:
      auth_module: [postgres]
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: my-postgres-postgres-exporter.default.svc.cluster.local:9116
```

### Replication Metrics

The exporter ships a custom `pg_replication` query group that is evaluated on every instance (primary and standbys):

| Metric | Description |
|--------|-------------|
| `pg_replication_lag_seconds` | Seconds since the last replayed transaction on a standby; `0` on the primary |
| `pg_replication_in_recovery` | `1` when the instance is in recovery (standby), `0` when it is a primary |
| `pg_replication_receive_replay_lag_bytes` | Bytes of WAL received from the primary but not yet replayed; `0` on the primary |

Alert on `pg_replication_lag_seconds` to catch a standby falling behind, and on `sum(pg_replication_in_recovery == 0) > 1` across the instances of one release to detect split-brain (two primaries). Note that `pg_replication_lag_seconds` also grows on a healthy standby while the primary is idle (no transactions to replay), so combine it with `pg_replication_receive_replay_lag_bytes` when tuning alert thresholds.

### Exporter Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `prometheusExporter.enabled` | Enable Prometheus exporter | `false` |
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
kubectl create job --from=cronjob/my-postgres-backup manual-backup
```

### Backup Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `backup.enabled` | Enable backup CronJob | `false` |
| `backup.schedule` | Cron schedule | `0 2 * * *` |
| `backup.s3.endpoint` | S3-compatible endpoint URL | `""` |
| `backup.s3.bucket` | S3 bucket name | `""` |
| `backup.s3.prefix` | Key prefix within bucket | `backups` |
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

### Restore

```bash
mc cp s3/pg-backups/backups/backup_20250101_020000.dump /tmp/backup.dump
pg_restore -h localhost -U postgres -d postgres /tmp/backup.dump
```

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
| `pgbackrest.existingSecret.name` | Secret containing S3 credentials | `""` |
| `pgbackrest.existingSecret.accessKeyIdKey` | Key for access key ID in secret | `access-key-id` |
| `pgbackrest.existingSecret.secretAccessKeyKey` | Key for secret access key in secret | `secret-access-key` |
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

### Check Backup Status

```bash
kubectl exec -it my-postgres-0 -- pgbackrest --stanza=db info
```

### Point-in-Time Recovery

PITR requires manual intervention:

```bash
# 1. Scale down the StatefulSet
kubectl scale statefulset my-postgres --replicas=0

# 2. Run a restore pod mounting the data PVC
kubectl run pg-restore --rm -it \
  --image=cagriekin/repmgr:trixie-5.5.0-7 \
  --overrides='{ "spec": { "containers": [{ "name": "restore", "image": "cagriekin/repmgr:trixie-5.5.0-7", "command": ["bash"], "stdin": true, "tty": true, "volumeMounts": [{ "name": "data", "mountPath": "/var/lib/postgresql/data" }], "env": [{ "name": "PGBACKREST_REPO1_S3_KEY", "value": "YOUR_KEY" }, { "name": "PGBACKREST_REPO1_S3_KEY_SECRET", "value": "YOUR_SECRET" }] }], "volumes": [{ "name": "data", "persistentVolumeClaim": { "claimName": "data-my-postgres-0" } }] } }'

# 3. Inside the restore pod, run:
pgbackrest --stanza=db restore \
  --target="2026-03-22 12:00:00+00" \
  --target-action=promote \
  --delta

# 4. Scale back up
kubectl scale statefulset my-postgres --replicas=2
```

Repmgr will automatically rebuild standbys from the restored primary.

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
make test-repmgr            # primary + replica with repmgr
make test-failover           # kill primary, verify promotion
make test-full              # repmgr + pgpool + prometheus exporter
make test-upgrade           # upgrade path with data persistence
make cluster-delete

# Run cluster tests in parallel
make -j4 test-cluster
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

1. Stop the PostgreSQL pod:
```bash
kubectl scale statefulset <fullname> -n <namespace> --replicas=0
```

2. Run PITR restore:
```bash
pgbackrest --stanza=db --type=time "--target=2025-01-15 12:00:00" restore
```

3. Scale back up:
```bash
kubectl scale statefulset <fullname> -n <namespace> --replicas=<original-count>
```

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
| WAL archiving failing (pgBackRest) | S3 credentials or connectivity | Check pgbackrest-scheduler logs. Verify S3 endpoint and credentials. |
| Backup job hanging | S3 unreachable | `activeDeadlineSeconds` (default 3600s) will terminate the job. Check S3 connectivity. |
| Split-brain detected in logs | Network partition | Follow the split-brain recovery runbook above. |

## Upgrade

```bash
helm upgrade my-postgres cagriekin/pg
```

## Troubleshooting PGPool

### Connectivity: PGPool-II or the Backend?

When clients cannot connect, isolate the failing layer first. Query through PGPool-II:

```bash
kubectl port-forward svc/my-postgres-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres -c "SELECT 1"
```

Then bypass PGPool-II and query the primary Service directly:

```bash
kubectl port-forward svc/my-postgres 5432:5432
psql -h localhost -p 5432 -U postgres -d postgres -c "SELECT 1"
```

If only the PGPool-II path fails, check backend status and logs below. If both fail, troubleshoot PostgreSQL itself first (see the recovery runbooks above).

Check that the Services have endpoints (`my-postgres-readonly` exists when repmgr is enabled):

```bash
kubectl get endpoints my-postgres my-postgres-pgpool my-postgres-readonly
```

The PGPool-II readiness probe runs `SELECT 1` through port 9999 rather than a TCP check, so PGPool-II pods turn unready and drop out of the Service whenever they cannot serve queries from at least one backend. Empty `my-postgres-pgpool` endpoints therefore usually point at a backend or authentication problem, not at the Service. Restarts of the pgpool Deployment pods have the same root cause: the liveness probe runs the same query and restarts a wedged PGPool-II after about 60 seconds.

If reads through the `my-postgres-readonly` Service do not reach standbys, the problem is the `pg-role` labels rather than PGPool-II: the Service selects `pg-role: standby`, which the service-updater re-applies every cycle, and pods stay absent from its endpoints until labeled (fresh installs, recreated or scaled-up pods).

### Checking Backend Status

`SHOW pool_nodes` through port 9999 reports each backend as PGPool-II sees it. `pool_hba.conf` trusts local connections inside the pod, so no password is needed:

```bash
kubectl exec -it deploy/my-postgres-pgpool -c pgpool -- \
  sh -c 'psql -h 127.0.0.1 -p 9999 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SHOW pool_nodes;"'
```

The PCP admin interface on port 9898 (also exposed on the pgpool Service) provides the same data. It authenticates against `pcp.conf`, which the init container generates from the admin Secret: the chart-managed `my-postgres-pgpool-admin` (keys `username`/`password`, populated from `pgpool.admin.username`/`pgpool.admin.password`), or your own Secret when `pgpool.admin.existingSecret.enabled` is set. Retrieve the password, then run the pcp commands (they prompt for it):

```bash
kubectl get secret my-postgres-pgpool-admin -o jsonpath='{.data.password}' | base64 -d
kubectl exec -it deploy/my-postgres-pgpool -c pgpool -- pcp_node_count -h localhost -p 9898 -U admin
kubectl exec -it deploy/my-postgres-pgpool -c pgpool -- pcp_node_info -h localhost -p 9898 -U admin 0
```

Changing the chart-managed credentials rolls the Deployment via the Secret checksum annotation; rotating an existing Secret requires `kubectl rollout restart deployment my-postgres-pgpool`, because `pcp.conf` is generated at pod start.

Node IDs follow the StatefulSet ordinals: node 0 is `my-postgres-0`, node 1 is `my-postgres-1`, and so on.

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
2. On a primary change it runs `kubectl rollout restart deployment my-postgres-pgpool`, so PGPool-II restarts with a fresh backend status file and rediscovers the topology.
3. The same sidecar probes PGPool-II through its Service every 30 seconds and forces a rollout restart after 3 consecutive failures.
4. Independently, the PGPool-II liveness probe restarts any instance that cannot serve queries for about 60 seconds.

If clients still reach a stale topology (for example writes failing with read-only errors), apply the manual equivalent:

```bash
kubectl rollout restart deployment my-postgres-pgpool
```

Failover history is recorded as Kubernetes Events: on every primary change the service-updater emits a `PrimaryChanged` Event attached to the primary Service, and its container logs on the PostgreSQL pods carry the same transition:

```bash
kubectl get events --field-selector reason=PrimaryChanged
kubectl describe service my-postgres
kubectl logs my-postgres-0 -c service-updater | grep "Master change"
```

Events are pruned by the cluster's event TTL (one hour by default), so the service-updater logs are the longer-lived record.

### Logs

PGPool-II logs to stderr, so everything is available through the container logs:

```bash
kubectl logs deploy/my-postgres-pgpool -c pgpool
```

Verbosity is controlled by the `pgpool.logging.*` values: `logConnections` (default `true`), `logStatement` (log every client query), `logPerNodeStatement` (log which backend each query was routed to), and `logMinMessages` (default `warning`; `debug1` and below add internal detail). Changing them rolls the Deployment automatically via the config checksum annotation.

| Message | Meaning |
|---------|---------|
| `failed to connect to PostgreSQL server` / `health check retrying` | A backend is unreachable. The node is marked `down` after `pgpool.healthCheck.maxRetries` retries (default 10, every 3 seconds). |
| `degenerate backend request ... is canceled because failover is disallowed` | Expected. All backends are flagged `DISALLOW_TO_FAILOVER` (or `ALWAYS_PRIMARY` without repmgr): repmgr owns failover, and the service-updater restarts PGPool-II afterwards instead of letting it detach nodes itself. |
| `all backend nodes are down` | No backend is reachable and clients are rejected. The liveness probe restarts PGPool-II, which retries discovery; if the message persists, check the PostgreSQL pods. |
| `authentication failed` / `password mismatch` | Remote clients authenticate with md5 against `pool_passwd`, which contains only the chart's PostgreSQL user. Other database users cannot authenticate through PGPool-II while `pgpool.allowClearTextFrontendAuth` is `false` (default); either connect them directly to PostgreSQL or set it to `true` so PGPool-II can request their password in clear text and forward it to the backend. |
