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

```
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
| `redis.resources` | Requests/limits | see `values.yaml` |
| `redis.config.maxmemory` / `maxmemory-policy` | Memory cap / eviction | `200mb` / `allkeys-lru` |
| `redis.config.appendfsync` | AOF sync (`always`/`everysec`/`no`) | `everysec` |
| `exporter.enabled` | Prometheus exporter (sidecar in replication) | `true` |
| `exporter.serviceMonitor.enabled` / `exporter.prometheusRule.enabled` | ServiceMonitor / alerts | `true` / `false` |

## Monitoring

The `oliver006/redis_exporter` runs as a per-pod sidecar in replication so every member's
role and replication metrics are scraped. Enabling `exporter.prometheusRule.enabled` adds
HA alerts: `RedisDown`/`RedisNoMaster`, `RedisMultipleMasters` (split-brain),
`RedisReplicaDown`, `RedisReplicationLinkDown`, and `RedisWritesBlocked` (the
`min-replicas-to-write` tripwire). Standalone keeps the original single-instance alerts.

## Persistence

AOF is enabled by default; every write is appended and replayed on startup.

- **appendfsync**: `always` (safest), `everysec` (default), `no` (fastest).
- **RDB snapshots**: off by default (`save ""`); enable via `redis.config.rdbSnapshots`
  (list of `{seconds, changes}`) for faster restarts.
- **Data directory**: `/data`, backed by a PVC when `redis.persistence.enabled: true`.

## Memory Tuning

Set `redis.config.maxmemory` to ~80% of the container memory limit to leave room for AOF
rewrite buffers and fragmentation.

| Container Limit | Recommended maxmemory |
|----------------|-----------------------|
| 256Mi | 200mb |
| 512Mi | 400mb |
| 1Gi | 800mb |

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
```

Declarative unit tests run with `helm unittest -f 'tests/unit/*_test.yaml' redis`.

## Upgrade

```bash
helm upgrade cache ./redis
```
