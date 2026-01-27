# PostgreSQL with repmgr and PGPool-II

PostgreSQL Helm chart with repmgr for replication management and optional PGPool-II for query routing.

## Features

- PostgreSQL 18.1 with configurable version
- Repmgr for automatic failover and replication management
- Optional PGPool-II for read/write splitting
- Support for existing secrets or auto-generated passwords
- StatefulSet-based deployment with persistent storage
- Configurable resource limits and probes

## Installation

```bash
helm install my-postgres ./pg
```

### With Read Replicas

```bash
helm install my-postgres ./pg --set postgresql.replicaCount=3
```

### With PGPool-II Enabled

```bash
helm install my-postgres ./pg \
  --set postgresql.replicaCount=3 \
  --set pgpool.enabled=true
```

### With Existing Secret

```bash
kubectl create secret generic pg-secret \
  --from-literal=username=myuser \
  --from-literal=password=mypassword \
  --from-literal=database=mydb

helm install my-postgres ./pg \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret
```

Or with custom key names:

```bash
kubectl create secret generic pg-secret \
  --from-literal=user=myuser \
  --from-literal=pass=mypassword \
  --from-literal=db=mydb

helm install my-postgres ./pg \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret \
  --set postgresql.existingSecret.usernameKey=user \
  --set postgresql.existingSecret.passwordKey=pass \
  --set postgresql.existingSecret.databaseKey=db
```

## Configuration

### PostgreSQL Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.image.repository` | PostgreSQL image repository | `postgres` |
| `postgresql.image.tag` | PostgreSQL image tag | `18.1-trixie` |
| `postgresql.replicaCount` | Number of PostgreSQL instances | `1` |
| `postgresql.database` | Database name | `postgres` |
| `postgresql.username` | Database username | `postgres` |
| `postgresql.resources.requests.cpu` | CPU request | `100m` |
| `postgresql.resources.requests.memory` | Memory request | `256Mi` |
| `postgresql.resources.limits.cpu` | CPU limit | `1000m` |
| `postgresql.resources.limits.memory` | Memory limit | `1Gi` |
| `postgresql.persistence.enabled` | Enable persistence | `true` |
| `postgresql.persistence.size` | Storage size | `10Gi` |
| `postgresql.persistence.storageClass` | Storage class | `""` |

### Repmgr Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `repmgr.enabled` | Enable repmgr | `true` |
| `repmgr.image.repository` | Repmgr image repository | `bitnami/repmgr` |
| `repmgr.image.tag` | Repmgr image tag | `5.4.1` |
| `repmgr.resources.requests.cpu` | CPU request | `50m` |
| `repmgr.resources.requests.memory` | Memory request | `128Mi` |
| `repmgr.resources.limits.cpu` | CPU limit | `500m` |
| `repmgr.resources.limits.memory` | Memory limit | `512Mi` |

### PGPool-II Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pgpool.enabled` | Enable PGPool-II | `false` |
| `pgpool.image.repository` | PGPool-II image repository | `cagriekin/pgpool` |
| `pgpool.image.tag` | PGPool-II image tag | `4.7.0` |
| `pgpool.replicaCount` | Number of PGPool-II instances | `1` |
| `pgpool.numInitChildren` | Number of PGPool-II worker processes | `32` |
| `pgpool.maxPool` | Max cached connections per process | `4` |
| `pgpool.childLifeTime` | Worker process lifetime in seconds | `300` |
| `pgpool.connectionLifeTime` | Cached connection lifetime | `0` |
| `pgpool.clientIdleLimit` | Client idle timeout | `0` |
| `pgpool.logging.logConnections` | Log client connections | `false` |
| `pgpool.logging.logStatement` | Log SQL statements | `false` |
| `pgpool.logging.logPerNodeStatement` | Log backend routing | `false` |
| `pgpool.service.type` | Service type | `ClusterIP` |
| `pgpool.service.port` | Service port | `9999` |
| `pgpool.resources.requests.cpu` | CPU request | `100m` |
| `pgpool.resources.requests.memory` | Memory request | `128Mi` |
| `pgpool.resources.limits.cpu` | CPU limit | `500m` |
| `pgpool.resources.limits.memory` | Memory limit | `512Mi` |

### Secret Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.existingSecret.enabled` | Use existing secret | `false` |
| `postgresql.existingSecret.name` | Existing secret name | `""` |
| `postgresql.existingSecret.usernameKey` | Username key in secret | `username` |
| `postgresql.existingSecret.passwordKey` | Password key in secret | `password` |
| `postgresql.existingSecret.databaseKey` | Database key in secret | `database` |

When `postgresql.existingSecret.enabled` is `false`, a secret will be auto-generated with:
- `username`: Base64 encoded value from `postgresql.username`
- `password`: Random 32 character alphanumeric string
- `database`: Base64 encoded value from `postgresql.database`

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.annotations` | Global annotations | `{}` |

## Connecting to PostgreSQL

### Direct Connection to Primary

```bash
kubectl port-forward svc/my-postgres 5432:5432
psql -h localhost -U postgres -d postgres
```

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

Configuration options:
- `num_init_children`: Number of worker processes
- `max_pool`: Maximum cached connections per process
- `child_life_time`: Worker process lifetime
- `connection_life_time`: Connection reuse duration
- `client_idle_limit`: Client idle timeout

## Prometheus Exporter

This chart includes an optional PostgreSQL metrics exporter for Prometheus monitoring. The exporter runs as a single instance and can scrape metrics from all PostgreSQL instances (primary and replicas) using the multi-target pattern.

### Enable Exporter

```bash
helm install my-postgres ./pg \
  --set prometheusExporter.enabled=true \
  --set postgresql.replicaCount=3
```

### With ServiceMonitor (Prometheus Operator)

```bash
helm install my-postgres ./pg \
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

### Exporter Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `prometheusExporter.enabled` | Enable Prometheus exporter | `false` |
| `prometheusExporter.image.repository` | Exporter image repository | `quay.io/prometheuscommunity/postgres-exporter` |
| `prometheusExporter.image.tag` | Exporter image tag | `v0.15.0` |
| `prometheusExporter.service.port` | Exporter service port | `9116` |
| `prometheusExporter.serviceMonitor.enabled` | Create ServiceMonitor | `false` |
| `prometheusExporter.serviceMonitor.interval` | Scrape interval | `30s` |
| `prometheusExporter.serviceMonitor.scrapeTimeout` | Scrape timeout | `10s` |

## Upgrade

```bash
helm upgrade my-postgres ./pg
```
