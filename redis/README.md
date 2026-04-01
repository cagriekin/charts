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

## Testing

Tests require [Kind](https://kind.sigs.k8s.io/) and [Helm](https://helm.sh/) installed locally.

```bash
# Run everything (creates cluster, runs tests, deletes cluster)
make test

# Template/lint tests only (no cluster needed)
make test-template

# Create cluster, then run individual suites
make cluster-create
make test-minimal    # single redis instance, set/get
make test-full       # redis + exporter + metrics check
make cluster-delete
```

## Upgrade

```bash
helm upgrade my-redis ./redis
```
