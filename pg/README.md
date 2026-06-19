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
| `postgresql.majorVersion` | PostgreSQL major version in `image.tag`; builds the extension paths (`/usr/lib/postgresql/<major>/lib`, `/usr/share/postgresql/<major>/extension`) when `extensions.enabled=true`. In repmgr mode the server runs from the repmgr image and is pinned to `repmgr.image.majorVersion` (currently `18`) regardless of `postgresql.image`; the chart fails to render if the two majors differ. | `"18"` |
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
| `postgresql.extensions.enabled` | Enable extensions support | `false` |
| `postgresql.lifecycle.postStart.additionalCommands` | Shell commands to run after PostgreSQL is ready | `""` |
| `postgresql.migrateLegacyMd5Users` | Re-hash MD5 user passwords to SCRAM on PG14+ | `true` |
| `postgresql.nodeSelector` | Node selector for PostgreSQL pods | `{}` |
| `postgresql.tolerations` | Tolerations for PostgreSQL pods | `[]` |
| `postgresql.topologySpreadConstraints` | Spread constraints added alongside the built-in affinity (e.g. a hard zone spread) | `[]` |
| `postgresql.serviceAccount.annotations` | Annotations on the postgresql pods' ServiceAccount (cloud workload identity for keyless pgBackRest S3) | `{}` |

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
| `repmgr.image.tag` | Repmgr image tag | `trixie-5.5.0-19` |
| `repmgr.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `repmgr.image.majorVersion` | PostgreSQL major bundled in the repmgr image. In repmgr mode the server always runs this major; `postgresql.majorVersion` must match or the chart fails to render. Bump with `repmgr.image.tag` when moving to an image built for a new PG major. | `"18"` |
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

- **repmgrd**: monitors replication and triggers automatic failover when the primary becomes unavailable. Has a preStop hook that runs `repmgr daemon stop` so the shutting-down node's daemon does not trigger a spurious failover during pod termination. (This stops the daemon only — it does **not** unregister the node from `repmgr.nodes`; see [Scaling down](#scaling-down) for the ghost-node cleanup.)
- **service-updater**: watches repmgr cluster state and patches the Kubernetes Service selector to point to the current primary, then restarts PGPool-II if enabled. Also maintains a `pg-role` label (`primary`/`standby`) on every postgresql pod each cycle, which the `<fullname>-readonly` service selects (`pg-role: standby`) to route read traffic to replicas; pods without the label (fresh, recreated or scaled-up) are excluded until labeled. Has a preStop hook that sleeps 5s to allow in-flight patches to complete. Includes a liveness probe that checks for a heartbeat file updated each loop iteration (fails if no update within 120s). Performs split-brain detection each cycle by querying all nodes for `pg_is_in_recovery()` -- if multiple primaries are found, takes the configured action (`log` or `fence`).

**Split-brain detection**: In a 2-node cluster, network partitions can cause both nodes to believe they are the primary. The service-updater detects this by checking all nodes each monitoring cycle. With `action: log` (default), it logs a critical warning. With `action: fence`, it compares WAL LSN positions and terminates connections on the stale primary. For production deployments, use 3+ nodes to reduce split-brain risk.

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

Must satisfy `leaseDuration > renewDeadline > retryPeriod`. For managed clouds, widen them (e.g. `30s/20s/4s`) so a brief apiserver blip does not trip an unnecessary demote. Note: with the Kubernetes Lease backend, a control-plane outage longer than `renewDeadline` is itself a write outage (the healthy primary self-demotes on losing apiserver contact, and no standby can acquire until the control plane returns); this is the safe choice under an asymmetric partition.

In agent mode the agent also fronts the read/write split: `pgpool` (if enabled) points at the RW (`<fullname>`) and RO (`<fullname>-readonly`) Services with failover off, and the agent maintains the Service selector and `pg-role` labels itself (no repmgrd/service-updater sidecars).

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

## Replication Management

Repmgr manages replication automatically. To check cluster status:

```bash
kubectl exec -it my-postgres-pg-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

### Scaling down

Scaling `postgresql.replicaCount` **down** removes the highest-ordinal pods and their
PVCs, but the chart does **not** automatically unregister those nodes from
`repmgr.nodes`. The removed nodes linger as `active`, so `repmgr cluster show` reports
them as permanently failed and, in repmgrd mode, the surviving nodes keep retrying the
(now-gone) DNS names — adding connection-timeout delay to failover elections. This is a
known gap (#139); after scaling down, manually unregister each removed ordinal from the
current primary (node id = `ordinal + 1000`, e.g. ordinal 2 → node id `1002`):

```bash
# for each removed ordinal N (>= the new replicaCount):
kubectl exec -it my-postgres-pg-0 -- \
  repmgr -f /etc/repmgr/repmgr.conf standby unregister --node-id=$((N + 1000))
kubectl exec -it my-postgres-pg-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
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

PITR requires manual intervention:

```bash
# 1. Scale down the StatefulSet
kubectl scale statefulset my-postgres-pg --replicas=0

# 2. Run a restore pod that mounts the data PVC AND the pgbackrest ConfigMap.
# The repo settings (repo1-type=s3, endpoint, bucket, path) and pg1-path live ONLY
# in the <fullname>-pgbackrest ConfigMap, so it MUST be mounted at
# /etc/pgbackrest/pgbackrest.conf or pgbackrest fails with "requires option: pg1-path"
# and would default to a local posix repo (never finding the S3 backup).
# Replace YOUR_PGBACKREST_SECRET with your pgbackrest.existingSecret.name.
# The securityContext (101:103) matches the chart so restored files are owned
# correctly and the pod is accepted under restricted PodSecurity / OpenShift SCC.
# keyType: auto (IRSA / Workload Identity)? Drop the env block, add
# "serviceAccountName": "my-postgres-pg-repmgr" to the spec (that SA carries the
# cloud-role annotation; the namespace default SA does not), and ensure the pod lands
# where your IRSA/WI webhook injects the token; pgbackrest then uses the credential chain.
kubectl run pg-restore --rm -it \
  --image=cagriekin/repmgr:trixie-5.5.0-18 \
  --overrides='{ "spec": { "securityContext": { "runAsUser": 101, "runAsGroup": 103, "fsGroup": 103, "runAsNonRoot": true, "seccompProfile": { "type": "RuntimeDefault" } }, "containers": [{ "name": "restore", "image": "cagriekin/repmgr:trixie-5.5.0-18", "command": ["bash"], "stdin": true, "tty": true, "securityContext": { "allowPrivilegeEscalation": false, "capabilities": { "drop": ["ALL"] } }, "volumeMounts": [{ "name": "data", "mountPath": "/var/lib/postgresql/data" }, { "name": "pgbackrest-config", "mountPath": "/etc/pgbackrest/pgbackrest.conf", "subPath": "pgbackrest.conf", "readOnly": true }], "env": [{ "name": "PGBACKREST_REPO1_S3_KEY", "valueFrom": { "secretKeyRef": { "name": "YOUR_PGBACKREST_SECRET", "key": "access-key-id" } } }, { "name": "PGBACKREST_REPO1_S3_KEY_SECRET", "valueFrom": { "secretKeyRef": { "name": "YOUR_PGBACKREST_SECRET", "key": "secret-access-key" } } }] }], "volumes": [{ "name": "data", "persistentVolumeClaim": { "claimName": "data-my-postgres-pg-0" } }, { "name": "pgbackrest-config", "configMap": { "name": "my-postgres-pg-pgbackrest" } }] } }'

# 3. Inside the restore pod, run (stanza = pgbackrest.stanza, default "db").
# --type=time is REQUIRED with --target (pgbackrest rejects --target otherwise);
# use --type=xid|name|lsn for other recovery targets. Leave the existing data dir in
# place so --delta restores only changed files (a wiped PGDATA disables --delta).
pgbackrest --stanza=db restore \
  --type=time \
  --target="2026-03-22 12:00:00+00" \
  --target-action=promote \
  --delta

# 4. Scale back up
kubectl scale statefulset my-postgres-pg --replicas=2
```

Repmgr will automatically rebuild standbys from the restored primary.

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
| WAL archiving failing (pgBackRest) | S3 credentials or connectivity | Check the `pgbackrest` sidecar logs on the primary pod and the `<fullname>-pgbackrest-full`/`-diff` CronJob pod logs. Verify S3 endpoint and credentials. |
| Backup job hanging | S3 unreachable | `activeDeadlineSeconds` (default 3600s) will terminate the job. Check S3 connectivity. |
| Split-brain detected in logs | Network partition | Follow the split-brain recovery runbook above. |

## Upgrade and migration

### Version model

Each chart is released independently and tagged `<chart>-<version>` (e.g. `pg-0.5.89`); `pg` and `pgvector` are released in lockstep (same image, same agent), and both **unify at `1.0.0`** (the `0.5.x`/`0.6.x` split ends there). The `common` and `etcd` charts are vendored dependencies — they ship inside the `pg`/`pgvector` packages, not released on their own.

### Compatibility matrix

| `pg` | `pgvector` | repmgr image | PostgreSQL | Kubernetes |
|------|-----------|--------------|-----------|-----------|
| 0.5.89 | 0.6.91 | `trixie-5.5.0-16` | 18.x | ≥ 1.21 (PDB `policy/v1`) |
| 1.0.0 *(planned)* | 1.0.0 *(planned)* | `trixie-5.5.0-16`+ | 18.x | ≥ 1.21 |

Extras: agent monitoring (`repmgr.agent.monitoring.*`) needs the Prometheus Operator CRDs; the etcd backend (`repmgr.agent.dcs.backend: etcd`) needs an etcd ≥ 3.5 (BYO/shared) or the bundled etcd subchart (`etcd.enabled=true`).

### Routine upgrade (within 0.x)

```bash
helm repo update
helm upgrade my-postgres cagriekin/pg   # add -f your-values.yaml
```

Within the 0.x line the default failover mode was `repmgrd`, so those upgrades were unaffected and the repmgrd rendering stayed byte-stable. **Read every `Migrating from X.Y.Z` entry in [`CHANGELOG.md`](CHANGELOG.md) between your current version and the target** — some releases (credential, pg_hba, or image changes) have one-time steps. The CHANGELOG carries an unbroken trail back to 0.5.71.

### Migrating to 1.0.0 (the breaking change)

As of `1.0.0` the default `failoverMode` is `agent` (it was `repmgrd` through 0.x). Because the agent's `podManagementPolicy: Parallel` is immutable on an existing StatefulSet, upgrading an existing repmgrd install across the `1.0.0` boundary and adopting the new default requires a one-time `kubectl delete statefulset <release>-pg --cascade=orphan` + `helm upgrade` recreate — see **[Migrating an existing release to agent mode](#migrating-an-existing-release-to-agent-mode)** for the full `--cascade=orphan` runbook and the GitOps caveats. To defer the move, pin `repmgr.failoverMode: repmgrd` (the legacy path stays supported for one major cycle, then deprecation); that upgrade needs no recreate.

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
