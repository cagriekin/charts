# Redis Helm Chart

Single-instance Redis with AOF persistence and optional Prometheus metrics exporter.

## Installation

```bash
helm install my-redis ./redis
```

### With Custom Configuration

```bash
helm install my-redis ./redis \
  --set redis.config.maxmemory="500mb" \
  --set redis.config.maxmemory-policy="volatile-lru"
```

### With Authentication

```bash
kubectl create secret generic redis-auth \
  --from-literal=redis-password=mysecretpassword

helm install my-redis ./redis \
  --set redis.auth.enabled=true \
  --set redis.auth.existingSecret.name=redis-auth
```

### Without Exporter

```bash
helm install my-redis ./redis \
  --set exporter.enabled=false
```

## Configuration

### Redis Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.image.repository` | Redis image repository | `redis` |
| `redis.image.tag` | Redis image tag | `8.6.2-trixie` |
| `redis.persistence.enabled` | Enable persistence | `true` |
| `redis.persistence.storageClass` | Storage class | `""` |
| `redis.persistence.size` | Storage size | `1Gi` |
| `redis.auth.enabled` | Enable Redis authentication | `false` |
| `redis.auth.existingSecret.name` | Secret containing Redis password | `""` |
| `redis.auth.existingSecret.key` | Key in secret for password | `redis-password` |
| `redis.config.maxmemory` | Max memory | `200mb` |
| `redis.config.maxmemory-policy` | Eviction policy | `allkeys-lru` |
| `redis.config.appendfsync` | AOF sync mode (`always`, `everysec`, `no`) | `everysec` |
| `redis.config.no-appendfsync-on-rewrite` | Skip fsync during AOF rewrite | `yes` |
| `redis.config.auto-aof-rewrite-percentage` | Trigger AOF rewrite at this growth percentage | `100` |
| `redis.config.auto-aof-rewrite-min-size` | Minimum AOF size before rewrite triggers | `64mb` |
| `redis.config.rdbSnapshots` | Optional RDB snapshot schedule (list of {seconds, changes}) | `[]` |
| `redis.resources.requests.cpu` | CPU request | `50m` |
| `redis.resources.requests.memory` | Memory request | `64Mi` |
| `redis.resources.limits.cpu` | CPU limit | `200m` |
| `redis.resources.limits.memory` | Memory limit | `256Mi` |
| `redis.podSecurityContext` | Pod-level securityContext | `{fsGroup: 999, runAsNonRoot: true, seccompProfile.type: RuntimeDefault}` |
| `redis.containerSecurityContext` | Container-level securityContext | `{runAsUser: 999, runAsGroup: 999, allowPrivilegeEscalation: false, capabilities.drop: [ALL]}` |
| `redis.terminationGracePeriodSeconds` | Time allowed for graceful shutdown | `300` |
| `redis.podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `false` |
| `redis.podDisruptionBudget.minAvailable` | Minimum available pods during disruption | `1` |

### Exporter Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `exporter.enabled` | Enable Redis exporter | `true` |
| `exporter.image.repository` | Exporter image | `oliver006/redis_exporter` |
| `exporter.image.tag` | Exporter tag | `v1.67.0` |
| `exporter.service.port` | Exporter port | `9121` |
| `exporter.serviceMonitor.enabled` | Create ServiceMonitor | `true` |
| `exporter.serviceMonitor.interval` | Scrape interval | `30s` |
| `exporter.prometheusRule.enabled` | Create PrometheusRule with default alerts | `false` |
| `exporter.prometheusRule.additionalLabels` | Additional labels for PrometheusRule | `{}` |
| `exporter.prometheusRule.rules` | Custom alert rules (overrides defaults when non-empty) | `[]` |

### Service Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `service.type` | Service type | `ClusterIP` |
| `service.port` | Service port | `6379` |

### NetworkPolicy Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `networkPolicy.enabled` | Enable NetworkPolicy resources | `false` |
| `networkPolicy.allowExternal` | Allow ingress from any pod in namespace | `true` |
| `networkPolicy.extraIngress` | Additional ingress rules | `[]` |

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.annotations` | Annotations applied to all resources | `{}` |

## Important: Single-Instance Only

This chart deploys a **single Redis instance**. It does not provide high availability, replication, or automatic failover. If Redis becomes unavailable, it will be unavailable until the pod restarts.

For HA requirements, consider Redis Sentinel or Redis Cluster deployments.

### RTO/RPO

| Metric | Value | Notes |
|--------|-------|-------|
| **RTO** | ~30-90 seconds | Pod restart time (image pull + AOF replay). Depends on AOF file size. |
| **RPO with `appendfsync always`** | 0 seconds | Every write is fsynced. Higher write latency. |
| **RPO with `appendfsync everysec`** | Up to 1 second | Default. Loses at most 1 second of writes on crash. |
| **RPO with `appendfsync no`** | Up to 30 seconds | OS decides when to flush. Not recommended for durability. |
| **RPO with RDB only** | Up to snapshot interval | Depends on `rdbSnapshots` schedule. |

## Persistence

AOF (Append-Only File) is enabled by default. Every write operation is appended to the AOF log, which is replayed on startup to rebuild the dataset.

- **appendfsync modes**: `always` (safest, slowest), `everysec` (default, good balance), `no` (fastest, least durable)
- **AOF rewrite**: Redis periodically rewrites the AOF to compact it. Controlled by `auto-aof-rewrite-percentage` and `auto-aof-rewrite-min-size`.
- **RDB snapshots**: Disabled by default (`save ""`). Can be enabled alongside AOF via `redis.config.rdbSnapshots` for faster restarts (Redis loads RDB first, then replays AOF delta).
- **Data directory**: `/data` inside the container, backed by a PersistentVolumeClaim when `redis.persistence.enabled: true`.

### Backup and Restore

**Manual backup:**
```bash
kubectl exec -n <namespace> <pod> -c redis -- redis-cli BGSAVE
kubectl cp <namespace>/<pod>:/data/dump.rdb ./dump.rdb -c redis
```

**Backup AOF:**
```bash
kubectl cp <namespace>/<pod>:/data/appendonlydir ./appendonlydir -c redis
```

**Restore:**
```bash
kubectl scale statefulset <fullname> -n <namespace> --replicas=0
kubectl cp ./dump.rdb <namespace>/<pod>:/data/dump.rdb -c redis
kubectl scale statefulset <fullname> -n <namespace> --replicas=1
```

## Memory Tuning

Set `redis.config.maxmemory` to approximately 80% of the container memory limit to leave room for AOF rewrite buffers, fragmentation, and Redis overhead.

| Container Limit | Recommended maxmemory |
|----------------|-----------------------|
| 256Mi | 200mb |
| 512Mi | 400mb |
| 1Gi | 800mb |
| 2Gi | 1600mb |

When `maxmemory` is reached, Redis applies the configured `maxmemory-policy` to evict keys. The default `allkeys-lru` evicts the least recently used keys.

## Troubleshooting

| Symptom | Likely Cause | Resolution |
|---------|-------------|------------|
| Pod OOMKilled | maxmemory too close to container limit | Reduce `maxmemory` to ~80% of container memory limit |
| Slow startup after restart | Large AOF file replay | Enable RDB snapshots alongside AOF for faster recovery. Consider `auto-aof-rewrite-min-size`. |
| AOF corruption | Unclean shutdown | Run `redis-check-aof --fix /data/appendonlydir/appendonly.aof.1.incr.aof` inside the container |
| Probe failures after enabling auth | Probes not using password | Ensure `redis.auth.enabled: true` is set -- probes automatically include `-a "$REDIS_PASSWORD"` |
| Exporter showing no metrics | Exporter can't reach Redis | Check exporter logs. If auth is enabled, verify the exporter has the correct secret configured. |
| Data loss on restart | Persistence disabled | Ensure `redis.persistence.enabled: true` and a storage class is available |

## Testing

Tests require [Kind](https://kind.sigs.k8s.io/) and [Helm](https://helm.sh/) installed locally.

```bash
# Run everything (creates cluster, runs tests, deletes cluster)
make test

# Template/lint tests only (no cluster needed)
make test-template

# Create cluster, then run individual suites
make cluster-create
make test-minimal       # single redis instance, set/get
make test-full          # redis + exporter + metrics check
make test-persistence   # AOF persistence survives pod restart
make cluster-delete
```

## Upgrade

```bash
helm upgrade my-redis ./redis
```
