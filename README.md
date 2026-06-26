# Helm Charts

This repository houses the Helm charts I rely on across several projects. Each chart sits in its own directory and follows the standard Helm project structure.

## Repository Structure

- `pg/` – A highly-available PostgreSQL cluster. Uses repmgr for streaming replication and an agent for automatic failover, electing the primary through a distributed leadership store (DCS) that is either the Kubernetes Lease API or an etcd quorum. Provides optional PgPool-II for connection pooling and read/write routing, pgBackRest/pg_dump backups to S3, client and replication TLS, and a Prometheus metrics exporter.
- `pgvector/` – The same PostgreSQL cluster as `pg` with the pgvector extension enabled for vector similarity search. Carries all of pg's capabilities: replication and failover, the Kubernetes-Lease-or-etcd-quorum DCS, optional PgPool-II, S3 backups, TLS, and metrics.
- `etcd/` – A 3-node etcd cluster that provides the quorum-based leadership store (DCS) for the `pg`/`pgvector` failover agent. Run it bundled inside a database release, or standalone as a single shared coordination store for several databases.
- `kafka/` – An Apache Kafka cluster in KRaft mode (no ZooKeeper), with separate controller and broker StatefulSets, SASL authentication, declarative topic management, secrets, and a metrics exporter.
- `redis/` – A Redis deployment that can run standalone or as a Sentinel-managed high-availability replication set. Supports ACLs (with a chart-managed operator user), TLS, persistence, and a Prometheus metrics exporter.

## Usage

The charts in this repository are published as a Helm repository backed by GitHub Pages and chart archives attached to GitHub releases.

- **Add the Helm repository**

```bash
helm repo add cagriekin https://cagriekin.github.io/charts
helm repo update
```

- **Install a chart from the remote repo**

```bash
helm install my-pg cagriekin/pg -n your-namespace
helm install my-pgvector cagriekin/pgvector -n your-namespace
helm install my-kafka cagriekin/kafka -n your-namespace
helm install my-redis cagriekin/redis -n your-namespace
```

- **Develop or test charts locally**

```bash
helm lint ./pg
helm lint ./pgvector
helm lint ./kafka
helm lint ./redis
```

## Testing

Each chart has a Kind-based integration test suite. Tests require [Kind](https://kind.sigs.k8s.io/) and [Helm](https://helm.sh/).

```bash
# Run full test suite for a chart (creates cluster, tests, deletes cluster)
make -C pg test
make -C kafka test
make -C redis test

# Template/lint tests only (no cluster needed)
make -C pg test-template
make -C kafka test-template
make -C redis test-template
```

See each chart's README for the full list of available test targets.

## Contributing

1. Make your changes in the appropriate chart directory.
2. Update chart metadata (`Chart.yaml`) and documentation as needed.
3. Run `make -C <chart-folder> test-template` to verify the chart before committing.
4. Run `make -C <chart-folder> test` for full integration testing.

