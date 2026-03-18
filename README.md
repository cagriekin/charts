# Helm Charts

This repository houses the Helm charts I rely on across several projects. Each chart sits in its own directory and follows the standard Helm project structure.

## Repository Structure

- `pg/` – Helm chart for deploying PostgreSQL with repmgr replication, optional ProxySQL for query routing, and Prometheus metrics exporter.
- `pgvector/` – Helm chart for deploying a PostgreSQL cluster with pgvector, pgBouncer, HAProxy, and auxiliary resources.
- `kafka/` – Helm chart for deploying Apache Kafka using KRaft mode, including controller and broker StatefulSets, SASL authentication, configurable topics, metrics exporter, secrets, and related Kubernetes resources.
- `redis/` – Helm chart for deploying Redis, including configuration, persistence, and metrics where applicable.

## Usage

The charts in this repository are published as a Helm repository backed by GitHub Pages and chart archives attached to GitHub releases.

- **Add the Helm repository**

```bash
helm repo add cagriekin-charts https://cagriekin.github.io/charts
helm repo update
```

- **Install a chart from the remote repo**

```bash
helm install my-pg cagriekin-charts/pg -n your-namespace
helm install my-pgvector cagriekin-charts/pgvector -n your-namespace
helm install my-kafka cagriekin-charts/kafka -n your-namespace
helm install my-redis cagriekin-charts/redis -n your-namespace
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

