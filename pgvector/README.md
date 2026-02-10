# PostgreSQL with pgvector

PostgreSQL Helm chart with pgvector extension for vector similarity search and optional PGPool-II for connection pooling.

## Features

- PostgreSQL 18.1 with pgvector extension
- Vector similarity search capabilities
- Optional PGPool-II for connection pooling and load balancing
- Support for existing secrets or auto-generated passwords
- StatefulSet-based deployment with persistent storage
- Configurable resource limits and probes
- Optional Prometheus exporter with ServiceMonitor support
- Automated S3 backups via CronJob with retention management

## Installation

```bash
helm install my-pgvector ./pgvector
```

### With Multiple Replicas

```bash
helm install my-pgvector ./pgvector --set postgresql.replicaCount=3
```

### With PGPool-II Enabled

```bash
helm install my-pgvector ./pgvector \
  --set postgresql.replicaCount=3 \
  --set pgpool.enabled=true
```

### With Existing Secret

```bash
kubectl create secret generic pg-secret \
  --from-literal=username=myuser \
  --from-literal=password=mypassword \
  --from-literal=database=mydb

helm install my-pgvector ./pgvector \
  --set postgresql.existingSecret.enabled=true \
  --set postgresql.existingSecret.name=pg-secret
```

## Using pgvector

After installation, connect to PostgreSQL and the vector extension will be automatically created:

```sql
-- The extension is already created via CREATE EXTENSION IF NOT EXISTS vector;
-- You can start using vector types immediately

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

This chart extends the base PostgreSQL chart. See the [pg chart documentation](../pg/README.md) for full configuration options.

### Key Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.image.repository` | PostgreSQL image repository | `pgvector/pgvector` |
| `postgresql.image.tag` | PostgreSQL image tag | `pg18-trixie` |
| `postgresql.pgvector.enabled` | Enable pgvector extension | `true` |
| `postgresql.replicaCount` | Number of PostgreSQL instances | `1` |
| `pgpool.enabled` | Enable PGPool-II | `false` |
| `pgpool.service.port` | PGPool-II service port | `9999` |

For complete configuration options, refer to the values.yaml file or the base pg chart.

## Connecting to PostgreSQL

### Direct Connection

```bash
kubectl port-forward svc/my-pgvector 5432:5432
psql -h localhost -U postgres -d postgres
```

### Through PGPool-II

```bash
kubectl port-forward svc/my-pgvector-pgpool 9999:9999
psql -h localhost -p 9999 -U postgres -d postgres
```

## Backup

Automated database backups can be enabled to run `pg_dump` on a schedule and upload compressed dumps to S3-compatible storage. See the [pg chart backup documentation](../pg/README.md#backup) for full details.

```bash
kubectl create secret generic s3-backup-creds \
  --from-literal=access-key-id=YOUR_ACCESS_KEY \
  --from-literal=secret-access-key=YOUR_SECRET_KEY

helm install my-pgvector ./pgvector \
  --set backup.enabled=true \
  --set backup.s3.endpoint=https://minio.example.com \
  --set backup.s3.bucket=pgvector-backups \
  --set backup.existingSecret.name=s3-backup-creds
```

## pgvector Resources

- [pgvector GitHub](https://github.com/pgvector/pgvector)
- [pgvector Documentation](https://github.com/pgvector/pgvector#getting-started)
