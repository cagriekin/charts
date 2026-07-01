# Redis Helm Chart

Redis with AOF persistence, in two architectures:

- **`replication`** (default): a Redis master plus replicas with a **Redis Sentinel**
  sidecar in every pod for quorum-based automatic failover. Clients are Sentinel-aware.
- **`standalone`**: a single Redis instance (the pre-1.0.0 behavior).

Optional Prometheus metrics, NetworkPolicy, PodDisruptionBudget and in-transit TLS.

> **Upgrading from 0.x?** 1.0.0 changes the default to `replication` and turns auth and
> NetworkPolicy on by default. See [Migrating to 1.0.0](#migrating-to-100).

## Installation

### HA replication (default)

```bash
helm install cache ./redis
```

This deploys 3 pods (1 master + 2 replicas), each running a Redis Sentinel sidecar, plus
a generated password Secret. Connect Sentinel-aware clients to the Sentinel Service:

```text
host: cache-redis-sentinel:26379    # SENTINEL get-master-addr-by-name mymaster
master name: mymaster
password: kubectl get secret cache-redis -o jsonpath='{.data.redis-password}' | base64 -d
```

Example (redis-py):

```python
from redis.sentinel import Sentinel
s = Sentinel([("cache-redis-sentinel", 26379)], sentinel_kwargs={"password": PW})
master = s.master_for("mymaster", password=PW)   # writes
replica = s.slave_for("mymaster", password=PW)   # reads
```

### Standalone (single instance)

```bash
helm install cache ./redis --set architecture=standalone
```

### Cloud / multi-zone

```bash
helm install cache ./redis -f redis/values-cloud.yaml
```

## Architecture

### Services (replication)

| Service | Port | Purpose |
|---------|------|---------|
| `<release>-redis-sentinel` | 26379 | **Client entry point.** Sentinel discovery (`get-master-addr-by-name`). |
| `<release>-redis-headless` | 6379/26379 | Stable per-pod DNS for replication + Sentinel gossip. |
| `<release>-redis` | 6379 | Plain all-pods Service for **reads** (round-robins to master + replicas). |

Writes must go to the master discovered via Sentinel — the plain `<release>-redis`
Service does not track the master. Replicas may serve slightly stale reads.

`<release>-redis` is the chart fullname. Note Helm collapses it to just `<release>` when
the release name already contains `redis` (e.g. release `my-redis` → Services `my-redis`,
`my-redis-sentinel`, `my-redis-headless`). The examples above use release `cache` so the
`<release>-redis-*` form is literal.

### How failover stays correct

- **Stable DNS:** every node announces its headless FQDN (`replica-announce-ip`,
  `sentinel announce-ip` + `resolve-hostnames`/`announce-hostnames`), so a pod restarting
  with a new IP is never tracked at a stale address.
- **Failover-safe bootstrap:** an init container asks the Sentinels who the master is and
  joins as its replica before Redis starts, so a restarted ex-master never resurrects
  read-write. Only `pod-0` seeds as master on a genuine cold boot.
- **Writable config on tmpfs:** Redis and Sentinel rewrite their own config at runtime, so
  the chart renders config into an in-memory volume (secrets never persist to node disk).
- **Write safety:** `min-replicas-to-write 1` makes the master refuse writes when it has no
  healthy replica, shrinking the data-loss window on failover.

### RTO / RPO

| Metric | Value | Notes |
|--------|-------|-------|
| Failover RTO | ~`downAfterMilliseconds` + a few s | Sentinel detects + promotes (default ~5-15s). |
| RPO (steady-state failover) | seconds | Async replication; bounded by `min-replicas-to-write` + Sentinel picking the most up-to-date replica. |
| RPO (full-cluster cold boot) | up to last-master delta | `pod-0` is seeded master; if it was not the last master, un-replicated writes can be lost. See [Known limitations](#known-limitations). |
| Standalone RTO | ~30-90s | Pod restart + AOF replay. |

## Configuration

### Architecture & replication

| Parameter | Description | Default |
|-----------|-------------|---------|
| `architecture` | `standalone` or `replication` | `replication` |
| `clusterDomain` | Cluster DNS domain for per-pod FQDNs | `cluster.local` |
| `redis.replicaCount` | Number of replicas (total pods = +1). Replication needs ≥2. | `2` |
| `redis.config.min-replicas-to-write` | Master refuses writes below this many replicas | `1` |
| `redis.config.min-replicas-max-lag` | Max replica lag (s) to count toward the above | `10` |
| `redis.config.replica-read-only` | Replicas reject writes | `yes` |

### Sentinel

| Parameter | Description | Default |
|-----------|-------------|---------|
| `sentinel.masterName` | Sentinel master group name | `mymaster` |
| `sentinel.quorum` | Sentinels that must agree to fail over (empty = strict majority) | `""` |
| `sentinel.downAfterMilliseconds` | Time before a master is considered down | `5000` |
| `sentinel.failoverTimeout` | Failover timeout (ms) | `60000` |
| `sentinel.parallelSyncs` | Replicas reconfigured in parallel during failover | `1` |
| `sentinel.image.*` / `sentinel.resources` | Sentinel image (reuses redis) / resources | redis image |

### Auth & TLS

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.auth.enabled` | Require a password | `true` |
| `redis.auth.existingSecret.name` | BYO Secret; empty = chart generates one | `""` |
| `redis.auth.existingSecret.key` | Password key | `redis-password` |
| `sentinel.auth.existingSecret.name` | Separate Sentinel password (empty = reuse redis) | `""` |
| `tls.enabled` | TLS-only Redis + Sentinel + replication | `false` |
| `tls.existingSecret` | Secret with `tls.crt`, `tls.key`, `ca.crt` | `""` |
| `tls.clientCertAuth` | Require client certificates (mutual TLS) | `false` |

TLS requires `architecture: replication` (the cert volume, `--tls` probes and per-pod-FQDN
SAN model are wired into the replication StatefulSet); enabling it with `standalone` is
rejected at render time. For a standalone instance, terminate TLS at a proxy in front of it.

**TLS certificate SANs** must cover the per-pod headless FQDNs
(`*.<release>-redis-headless.<ns>.svc.<clusterDomain>`), the `<release>-redis-sentinel`
and `<release>-redis` Service names, and `127.0.0.1`/`localhost` (the in-pod probes), or
peers and probes will fail verification.

The Sentinel port `26379` is the client discovery endpoint, so it is reachable by clients
(in-namespace via `allowExternal`, cross-namespace via `networkPolicy.extraIngress`). It is
also Sentinel's admin port — keep `redis.auth.enabled: true` (the default) so it requires a
password; disabling auth while `allowExternal: true` leaves cluster reconfiguration open to
any in-namespace pod.

### ACL

Redis ACLs layer named, per-command/per-key users on top of the password auth above
(`redis.auth.acl`, requires `redis.auth.enabled`). Works in both `standalone` and
`replication`.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.auth.acl.enabled` | Enable ACL users | `false` |
| `redis.auth.acl.operatorUser` | Privileged identity the chart authenticates as | `default` |
| `redis.auth.acl.operatorPasswordSecret.name` / `.key` | Operator password source (empty = reuse the redis auth Secret) | `""` |
| `redis.auth.acl.users[].name` | ACL username (`^[A-Za-z0-9_.-]+$`) | — |
| `redis.auth.acl.users[].rules` | Permission tokens only (no `on`/password) | `""` |
| `redis.auth.acl.users[].passwordSecret.name` / `.key` | Per-user password source | `""` |

```yaml
redis:
  auth:
    acl:
      enabled: true
      users:
        - name: app
          rules: "~app:* &app:* +@read +@write -@dangerous"
          passwordSecret:
            key: app-acl-password   # empty name => chart generates this key
```

**`rules` carries permission tokens only** — key patterns (`~app:*`), channel patterns
(`&app:*`), and command rules (`+@read`, `-@dangerous`). The chart emits `on` and the
password itself, so do **not** put `on`/`off`/`nopass`/`reset*`/`>password`/`#hash` in
`rules` (rejected at render time). Pub/sub needs an explicit `&<pattern>` because Redis
defaults to `resetchannels`.

**Operator user.** The chart authenticates as `operatorUser` for replication
(`masteruser`/`masterauth`), Sentinel (`auth-user`/`auth-pass`), the exporter, and the
liveness/readiness/preStop probes. Its permissions are **chart-managed and always full**
(`~* &* +@all`) — you pick only its name and password source, never its rules, because a
restricted operator silently breaks replication, failover, and metrics. The default
`operatorUser: default` keeps today's behavior exactly (the `default` user via
`requirepass`). **To lock down the `default` user, define it under `users[]` and set
`operatorUser` to a separate name** — the chart then drops `requirepass` and renders an
explicit `user default` line. (Locking down `default` while it is still the operator is
rejected.)

**Passwords.** Each user's password comes from a Secret. With an empty `passwordSecret.name`
the chart generates a random value (key `acl-<name>-password` unless you set `.key`) and
persists it across upgrades via `lookup`. Under render-only pipelines (e.g. ArgoCD) `lookup`
is empty, so **set `passwordSecret.name` (and `operatorPasswordSecret.name`) for every ACL
user**, or each sync regenerates the passwords and locks out clients — the same caveat as
`redis.auth.existingSecret.name`.

**Runtime changes are not persisted.** There is no `aclfile`; the rendered config is the
source of truth, so `ACL SETUSER`/`ACL SAVE` at runtime are lost on restart — change users
through values + `helm upgrade`. **Rotating a password** is a deliberate operation: during
the rolling restart a re-elected master may briefly hold the new password while a
not-yet-restarted replica still presents the old `masterauth`, so rotate during a quiet
window.

### Scheduling, PDB, NetworkPolicy

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.affinity` | Override; empty = hard per-node anti-affinity + soft zone spread | `{}` |
| `redis.topologySpreadConstraints` | Extra spread constraints | `[]` |
| `redis.nodeSelector` / `redis.tolerations` / `redis.priorityClassName` | Scheduling | `{}` / `[]` / `""` |
| `redis.podDisruptionBudget.enabled` | Enable PDB | `true` |
| `redis.podDisruptionBudget.maxUnavailable` | Max unavailable (preserves quorum) | `1` |
| `networkPolicy.enabled` | Enable NetworkPolicies | `true` |
| `networkPolicy.allowExternal` | Allow ingress from any pod in the namespace | `true` |
| `networkPolicy.extraIngress` / `extraEgress` | Additional rules (rule-level) | `[]` |

### Image, persistence, resources, exporter

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.image.repository` / `redis.image.tag` | Redis image | `redis` / `8.6.2-trixie` |
| `redis.persistence.enabled` / `size` / `storageClass` | Persistence | `true` / `1Gi` / `""` |
| `redis.persistence.retain` | Keep data PVCs on uninstall / scale-down (`false` = auto-delete) | `true` |
| `redis.resources` | Requests/limits | see `values.yaml` |
| `redis.config.maxmemory` / `maxmemory-policy` | Memory cap / eviction | `200mb` / `allkeys-lru` |
| `redis.config.appendfsync` | AOF sync (`always`/`everysec`/`no`) | `everysec` |
| `exporter.enabled` | Prometheus exporter (sidecar in replication) | `true` |
| `exporter.serviceMonitor.enabled` / `exporter.prometheusRule.enabled` | ServiceMonitor / alerts | `true` / `false` |
| `exporter.serviceMonitor.interval` / `scrapeTimeout` | Prometheus scrape interval / timeout | `30s` / `10s` |
| `exporter.connectionTimeout` | Go-duration connect timeout to Redis (empty = exporter default 15s) | `""` |
| `exporter.logFormat` | Exporter log format: `txt` (logfmt) or `json` | `txt` |
| `exporter.includeConfigMetrics` | Include `CONFIG GET` key metrics | `false` |
| `exporter.includeSystemMetrics` | Include system-level metrics from INFO (process cpu/mem) | `false` |
| `exporter.exportClientList` | Export per-client metrics via `CLIENT LIST` (expensive) | `false` |
| `exporter.disableExporterMetrics` | Disable go-runtime metrics from the exporter itself | `false` |
| `exporter.extraEnvVars` | Additional env vars for the exporter container (supports `value` and `valueFrom`) | `[]` |
| `exporter.dashboards.enabled` | Ship the Grafana dashboard as a sidecar-discoverable ConfigMap | `false` |
| `exporter.dashboards.label` / `labelValue` | Label the Grafana sidecar watches for | `grafana_dashboard` / `"1"` |
| `exporter.dashboards.namespace` | ConfigMap namespace (empty = release namespace) | `""` |
| `exporter.dashboards.additionalLabels` / `annotations` | Extra labels / annotations (e.g. `grafana_folder`) | `{}` / `{}` |

## Monitoring

The `oliver006/redis_exporter` runs as a per-pod sidecar in replication so every member's
role and replication metrics are scraped. Enabling `exporter.prometheusRule.enabled` adds
HA alerts: `RedisDown`/`RedisNoMaster`, `RedisMultipleMasters` (split-brain),
`RedisReplicaDown`, `RedisReplicationLinkDown`, and `RedisWritesBlocked` (the
`min-replicas-to-write` tripwire). Standalone keeps the original single-instance alerts.

### Exporter tuning

All new options apply to both architectures (replication sidecar and standalone Deployment):

- **`connectionTimeout`** — if your Redis is slow to respond during startup or network
  hiccups cause false scrape timeouts, raise this (e.g. `"30s"`).
- **`includeConfigMetrics`** — adds a small set of metrics from `CONFIG GET` (useful for
  auditing live config without `redis-cli`).
- **`includeSystemMetrics`** — adds `redis_used_cpu_sys`/`redis_used_cpu_user` from `INFO`.
- **`exportClientList`** — exports per-client connection metrics via `CLIENT LIST`; avoid on
  instances with many connected clients (O(n) overhead per scrape).
- **`disableExporterMetrics`** — suppresses go-runtime and process metrics emitted by the
  exporter process itself; useful when you want only Redis metrics in the scraped output.
- **`extraEnvVars`** — pass any other `REDIS_EXPORTER_*` flag (e.g. a Lua script path via
  `REDIS_EXPORTER_SCRIPT`, or `REDIS_EXPORTER_MAX_DISTINCT_KEY_GROUPS`).

### Grafana dashboard

A ready-made dashboard ships with the chart (`dashboards/redis-dashboard.json`): memory and
fragmentation, hit/miss ratio, connected/blocked clients, command throughput and per-command
latency, keyspace and evicted/expired keys, AOF/RDB persistence status, and replication
health. Set `exporter.dashboards.enabled: true` to render it as a ConfigMap that the Grafana
sidecar ([kiwigrid/k8s-sidecar](https://github.com/kiwigrid/k8s-sidecar), bundled with the
Grafana and kube-prometheus-stack charts) auto-imports:

```yaml
exporter:
  dashboards:
    enabled: true
    # Match whatever your Grafana install watches for; "1" is the common default.
    label: grafana_dashboard
    labelValue: "1"
    # Optional: drop it in a Grafana folder and/or a namespace the sidecar watches.
    annotations:
      grafana_folder: Redis
    # namespace: monitoring
```

The dashboard has `datasource`, `namespace`, `service`, and `pod` template variables, so one
dashboard covers both standalone and replication and any number of releases — pick the release
via `service` and drill into individual members via `pod`. It is gated on `exporter.enabled`;
with the exporter off, nothing is rendered.

For the sidecar to import it automatically, three things must line up — otherwise the
ConfigMap is created but silently ignored:

1. **The dashboard sidecar is enabled.** In kube-prometheus-stack that is
   `grafana.sidecar.dashboards.enabled: true` (default on); a plain Grafana with no sidecar
   never sees the ConfigMap — import the JSON through the Grafana UI instead.
2. **The label matches.** `exporter.dashboards.label`/`labelValue` must equal what the sidecar
   watches for (`grafana.sidecar.dashboards.label`/`labelValue`). The defaults here
   (`grafana_dashboard: "1"`) match the kube-prometheus-stack default.
3. **The namespace is watched.** By default the sidecar only watches its own namespace. If
   Grafana and this release live in different namespaces, either run the sidecar with
   `searchNamespace: ALL` (or a list including this one), or set `exporter.dashboards.namespace`
   to a namespace the sidecar watches. The `grafana_folder` annotation likewise only takes
   effect when the sidecar has folder-annotation support enabled (on by default in
   kube-prometheus-stack).

> **Scrape labels.** Panels and variables filter on the `namespace`, `service`, and `pod`
> labels — the target labels the Prometheus Operator adds when scraping through the chart's
> ServiceMonitor (`exporter.serviceMonitor.enabled`, default `true`). This is the same scrape
> path the bundled alerts assume. If you scrape with a plain `scrape_config` or Prometheus
> Agent instead, relabel those three labels onto the series or the panels read empty. When
> `exporter.dashboards.namespace` points the ConfigMap at a namespace outside the release,
> `helm uninstall` will not reap it — remove it yourself.

## Persistence

AOF is enabled by default; every write is appended and replayed on startup.

- **appendfsync**: `always` (safest), `everysec` (default), `no` (fastest).
- **RDB snapshots**: off by default (`save ""`); enable via `redis.config.rdbSnapshots`
  (list of `{seconds, changes}`) for faster restarts.
- **Data directory**: `/data`, backed by a PVC when `redis.persistence.enabled: true`.

### PVC lifecycle & reclaim behavior

Persistent volumes are provisioned from the StatefulSet's `volumeClaimTemplates`, one PVC
per pod named `data-<release>-redis-<ordinal>` (e.g. `data-myredis-redis-0`). Because the
PVCs are created by the StatefulSet controller — not rendered by Helm — **`helm uninstall`
does not delete them**, and neither does deleting the StatefulSet directly. This is
deliberate: your data (AOF/RDB) survives an accidental uninstall or a chart re-install.

**Retention is controlled by `redis.persistence.retain` (default `true`):**

- **`retain: true` (default)** — PVCs are kept when the release is uninstalled and when
  replicas are scaled down. The chart emits no `persistentVolumeClaimRetentionPolicy`, so
  it relies on Kubernetes' own default (`Retain` for both `whenDeleted` and `whenScaled`).
  Rendered output is unchanged from earlier chart versions.
- **`retain: false`** — the StatefulSet is rendered with
  `persistentVolumeClaimRetentionPolicy: {whenDeleted: Delete, whenScaled: Delete}`, so
  PVCs are garbage-collected on uninstall **and** when you scale `replicaCount` down. Use
  this only for ephemeral or dev installs where losing the data is acceptable. Requires
  Kubernetes 1.27+ (the feature is stable since 1.32; on older clusters the field is
  ignored and PVCs are retained).

> The retention policy governs only controller/uninstall-driven deletion. It does not
> protect against manually deleting a PVC, and it does not affect the underlying
> PersistentVolume's own reclaim policy (see below).

**Manual cleanup.** With the default `retain: true`, reclaim the storage yourself after an
uninstall:

```sh
# list the orphaned claims for a release
kubectl get pvc -l app.kubernetes.io/instance=<release> -n <namespace>

# delete them (irreversible — this frees the underlying PV per its reclaim policy)
kubectl delete pvc -l app.kubernetes.io/instance=<release> -n <namespace>
```

Whether the backing PersistentVolume (and cloud disk) is then deleted or kept depends on
the **PV reclaim policy** set by your `storageClass` — usually `Delete` for dynamically
provisioned cloud volumes, `Retain` if you want the disk to outlive the PVC. Check with
`kubectl get pv` / `kubectl get storageclass -o wide`.

**Recovering data from an orphaned PVC.** A reinstall with the same release name and the
same PVC naming (same ordinals, same `storageClass`/`size`) will bind to the existing
`data-<release>-redis-<ordinal>` claims and start from their AOF/RDB — no extra steps. To
recover into a *different* release, pre-create PVCs with the new release's expected names
bound to the retained PVs (set the PV's `claimRef` accordingly), or copy the data out of a
temporary pod that mounts the old PVC before deleting it.

## Sizing & performance

Redis is fast and lightweight; the usual constraint is **memory**, not CPU. Start small and
scale the memory limit (and `maxmemory` with it) to your working set.

### Memory

Set `redis.config.maxmemory` to ~80% of the container memory limit. The remaining headroom
absorbs AOF rewrite buffers, replication backlog, client output buffers, and allocator
fragmentation — without it the pod gets OOMKilled before `maxmemory-policy` can evict.

| Container limit (`redis.resources.limits.memory`) | Recommended `maxmemory` |
|----------------|-----------------------|
| 256Mi | 200mb |
| 512Mi | 400mb |
| 1Gi | 800mb |
| 4Gi | 3200mb |

When the working set exceeds `maxmemory`, the `maxmemory-policy` decides behavior: the
default `allkeys-lru` evicts least-recently-used keys (cache mode); `noeviction` rejects
writes instead (datastore mode — use when every key must survive). Watch `evicted_keys` and
`mem_fragmentation_ratio` (exposed by the exporter) in production.

### CPU

Command execution is **single-threaded**, so one core handles a very high request rate and
extra cores do *not* raise single-instance throughput. The `1` vCPU default request/limit is
ample for most workloads. Give more CPU only when you also run heavy background work (RDB
`save`/AOF rewrite forks, TLS termination, the exporter sidecar) or enable Redis I/O threads.
Scale **read** capacity horizontally with `architecture: replication` (replicas serve reads),
not by adding cores.

### Connections

`maxclients` defaults to 10000. Each idle connection costs a few KB plus its output buffer;
thousands of connections from an app without pooling can dominate memory. Prefer a client-side
connection pool, and bound noisy clients with `redis.config.client-output-buffer-limit`.

### Throughput baseline

`make -C redis test-performance` runs `redis-benchmark` against a small standalone instance
(1 vCPU / 512Mi, AOF fsync deferred) and asserts a conservative floor. It's a regression guard
and a starting point — **measure on your own hardware with your own value sizes**, since rates
swing widely with CPU, payload size, pipelining, TLS, and `appendfsync`. Rough expectations on
a single modern core:

| Scenario | Order of magnitude |
|----------|--------------------|
| `SET`/`GET`, no pipelining | tens of thousands of ops/s (latency-bound) |
| `SET`/`GET`, pipelined (`-P 16`) | hundreds of thousands to millions of ops/s |
| `appendfsync always` | substantially lower writes (one fsync per write) |
| TLS enabled | lower, from per-connection handshake + encryption cost |

Pipelining and a connection pool usually buy more than any server-side tuning. To benchmark a
live release yourself:

```bash
kubectl exec -n <ns> <release>-redis-0 -c redis -- \
  redis-benchmark -h 127.0.0.1 -q -n 100000 -c 50 -P 16 -t set,get
```

### Replication & persistence overhead

Each replica holds a full copy of the dataset, so HA (`replication`, 3 pods by default) needs
~3× the memory of a single instance. `appendfsync everysec` (default) costs little; `always`
trades throughput for the smallest durability window. RDB `save` and AOF rewrite fork the
process and briefly raise memory via copy-on-write — another reason for the 20% headroom above.

## Migrating to 1.0.0

1.0.0 flips the default to `replication` and enables auth + NetworkPolicy. On
`helm upgrade` an existing 0.x install scales to 3 pods, gains a Sentinel sidecar, and
starts requiring a password.

- **Keep the old single instance** (a PodDisruptionBudget is now on by default in every
  architecture, so disable it too for an exact match):
  ```bash
  helm upgrade cache ./redis \
    --set architecture=standalone \
    --set redis.auth.enabled=false \
    --set networkPolicy.enabled=false \
    --set redis.podDisruptionBudget.enabled=false
  ```
- **Adopt HA:** ensure 3 schedulable nodes (default hard per-node anti-affinity), make
  clients Sentinel-aware against `<release>-redis-sentinel:26379`, and read the generated
  password from the `<release>-redis` Secret (or set `redis.auth.existingSecret`).

## Known limitations

On a **full-cluster cold boot** (every pod down at once), the bootstrap seeds `pod-0` as
master. If `pod-0` was not the most recent master, writes that had not replicated to it can
be lost. Steady-state Sentinel failover is bounded (`min-replicas-to-write` + most
up-to-date replica selection). A durable last-master marker is tracked as a follow-up.

## Troubleshooting

| Symptom | Likely Cause | Resolution |
|---------|-------------|------------|
| Pods Pending | Hard anti-affinity needs one node per pod | Add nodes, or override `redis.affinity`. |
| Master refuses writes | `min-replicas-to-write` and no healthy replica | Restore a replica, or lower the setting for availability. |
| Client can't find master | Not Sentinel-aware | Use a Sentinel client against `<release>-redis-sentinel:26379`. |
| TLS handshake/verify errors | Cert SANs don't cover the FQDNs | Reissue the cert with the SANs listed under [Auth & TLS](#auth--tls). |
| Pod OOMKilled | `maxmemory` too close to the limit | Lower `maxmemory` to ~80% of the limit. |

## Testing

Requires [Kind](https://kind.sigs.k8s.io/) and [Helm](https://helm.sh/).

```bash
make -C redis test            # full: create cluster, run all suites, delete cluster
make -C redis test-template   # helm lint + template assertions (no cluster)
make -C redis test-ha         # replication + Sentinel failover (needs a cluster)
make -C redis test-tls        # replication-over-TLS smoke (opt-in)
make -C redis test-performance # redis-benchmark throughput baseline (opt-in; needs a cluster)
```

Declarative unit tests run with `helm unittest -f 'tests/unit/*_test.yaml' redis`.

## Upgrade

```bash
helm upgrade cache ./redis
```
