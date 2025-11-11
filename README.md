# Helm Charts

This repository houses the Helm charts I rely on across several projects. Each chart sits in its own directory and follows the standard Helm project structure.

## Repository Structure

- `pgvector/` – Helm chart for deploying a PostgreSQL cluster with pgvector, pgBouncer, HAProxy, and auxiliary resources.
- `kafka/` – Helm chart for deploying Apache Kafka using KRaft mode, including controller and broker StatefulSets, SASL authentication, configurable topics, metrics exporter, secrets, and related Kubernetes resources.

## Usage

Add the repo locally, package charts, or install them directly from this workspace:

```bash
helm repo index .
helm install pgvector ./pgvector -n your-namespace
```

## Contributing

1. Make your changes in the appropriate chart directory.
2. Update chart metadata (`Chart.yaml`) and documentation as needed.
3. Run `helm lint <chart-folder>` to verify the chart before committing.

