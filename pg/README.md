# PostgreSQL with repmgr and ProxySQL

PostgreSQL Helm chart with repmgr for replication management and optional ProxySQL for query routing.

## Features

- PostgreSQL 18.1 with configurable version
- Repmgr for automatic failover and replication management
- Optional ProxySQL for read/write splitting
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

### With ProxySQL Enabled

```bash
helm install my-postgres ./pg \
  --set postgresql.replicaCount=3 \
  --set proxysql.enabled=true
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

### ProxySQL Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `proxysql.enabled` | Enable ProxySQL | `false` |
| `proxysql.image.repository` | ProxySQL image repository | `proxysql/proxysql` |
| `proxysql.image.tag` | ProxySQL image tag | `3.0.5-debian` |
| `proxysql.replicaCount` | Number of ProxySQL instances | `1` |
| `proxysql.threads` | Number of ProxySQL threads | `4` |
| `proxysql.maxConnections` | Maximum connections | `2048` |
| `proxysql.service.type` | Service type | `ClusterIP` |
| `proxysql.service.port` | Service port | `6033` |
| `proxysql.resources.requests.cpu` | CPU request | `100m` |
| `proxysql.resources.requests.memory` | Memory request | `128Mi` |
| `proxysql.resources.limits.cpu` | CPU limit | `500m` |
| `proxysql.resources.limits.memory` | Memory limit | `512Mi` |

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

### Through ProxySQL

```bash
kubectl port-forward svc/my-postgres-proxysql 6033:6033
psql -h localhost -p 6033 -U postgres -d postgres
```

## Replication Management

Repmgr manages replication automatically. To check cluster status:

```bash
kubectl exec -it my-postgres-0 -- repmgr -f /etc/repmgr/repmgr.conf cluster show
```

## ProxySQL Query Routing

When ProxySQL is enabled, it routes queries based on patterns using PostgreSQL-specific configuration (pgsql_servers, pgsql_users, pgsql_query_rules):

Query routing rules (evaluated in order):
1. **SELECT ... FOR UPDATE**: Routed to primary (hostgroup 0)
2. **SELECT queries**: Routed to replicas (hostgroup 1)
3. **All other queries** (catch-all): Routed to primary (hostgroup 0)

This ensures:
- Write operations (INSERT, UPDATE, DELETE, etc.) go to the primary
- Read-only SELECT queries are distributed to replicas
- Locking reads go to the primary
- Any other queries default to the primary

ProxySQL uses PostgreSQL-specific table names and configuration:
- `pgsql_servers` for backend server configuration
- `pgsql_users` for user authentication
- `pgsql_query_rules` for query routing rules
- `pgsql_variables` for ProxySQL PostgreSQL variables

The `server_version` in ProxySQL is automatically set to match the PostgreSQL version from `postgresql.image.tag`. For example, if `postgresql.image.tag` is `18.1-trixie`, ProxySQL will use `server_version="18.1.0"`.

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
