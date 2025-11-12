# Kafka Helm Chart

This chart installs an Apache Kafka KRaft controller, brokers, and supporting components used by DamnThing's message bus.

## Components

- Kafka controller StatefulSet (single controller node)
- Kafka broker StatefulSet (configurable replica count)
- ConfigMaps for runtime configuration and JAAS credentials
- Kafka SASL secret
- Topic initialization job with documented topic metadata
- Kafka exporter Deployment, Service, and optional ServiceMonitor

## Values

Key options are summarized below; see `values.yaml` for the complete list.

| Key | Description | Default |
| --- | --- | --- |
| `fullnameOverride` | Base name applied to every rendered resource. REQUIRED and must be unique per namespace. | `""` (chart fails when left empty) |
| `kafka.controller.replicaCount` | Kafka controller replicas (KRaft mode) | `1` |
| `kafka.controller.persistence.storageClass` | StorageClass for controller PVC (empty uses cluster default) | `""` |
| `kafka.controller.persistence.size` | Requested storage capacity for the controller PVC | `1Gi` |
| `kafka.broker.replicaCount` | Kafka broker replicas | `2` |
| `kafka.broker.persistence.storageClass` | StorageClass for broker PVCs (empty uses cluster default) | `""` |
| `kafka.broker.persistence.size` | Requested storage capacity for each broker PVC | `1Gi` |
| `kafka.auth.username` | SASL/PLAIN username for broker and clients | `user1` |
| `kafka.auth.password` | SASL/PLAIN password (auto-generated when empty) | `""` |
| `kafka.auth.existingSecret` | Name of an existing secret providing SASL credentials (skips chart-managed secret; falls back to inline values when absent) | `""` |
| `kafka.auth.existingSecretKeys.username` | Key in the existing secret that stores the username | `username` |
| `kafka.auth.existingSecretKeys.password` | Key in the existing secret that stores the password | `password` |
| `kafka.autoCreateTopicsEnable` | Allow brokers to auto-create topics | `true` |
| `kafka.deleteTopicEnable` | Allow topic deletion | `true` |
| `kafka.topics` | Map of topic definitions keyed by topic name (supports `partitions`, `replicationFactor`, optional `config` map, and optional `metadata`) | `{}` |
| `exporters.kafka.enabled` | Deploy the Kafka metrics exporter | `true` |
| `serviceMonitors.enabled` | Create a Prometheus ServiceMonitor | `false` |
| `serviceMonitors.labels` | Extra labels applied to the ServiceMonitor when enabled | `{ release: kube-prometheus-stack }` |
| `kafka.controller.podSecurityContext` / `kafka.broker.podSecurityContext` | Enforced pod-level security settings (restricted profile defaults) | see `values.yaml` |
| `networkPolicy.enabled` | Restrict ingress/egress with generated NetworkPolicies | `true` |
| `autoscaling.broker.*` / `exporters.kafka.autoscaling.*` | HorizontalPodAutoscaler settings for brokers and exporter | disabled |
| `keda.broker.*` | Optional KEDA ScaledObject configuration for custom metrics based scaling | disabled |
| `kafka.*.jvm` | JVM heap and performance tuning options applied via environment variables | see `values.yaml` |

### Topic Specification

Each entry under `kafka.topics` follows this structure:

| Attribute | Type | Description |
| --- | --- | --- |
| `partitions` | integer | Partition count for the topic. Defaults to `1` when omitted. |
| `replicationFactor` | integer | Replication factor applied at creation time. Defaults to `1` when omitted. |
| `config` | map<string,string> | Optional per-topic broker configs mapped to the `--config` flags (e.g. `retention.ms`, `segment.ms`, `cleanup.policy`). |
| `metadata.description` | string | Human-friendly summary of the topic’s purpose. |
| `metadata.producers` | string[] | List of producer service names. |
| `metadata.consumers` | string[] | List of consumer service names. |
| `metadata.messageType` | string | Message schema identifier documented alongside the topic. |
| `metadata.partitioning` | string | Description of the partitioning strategy. |
| `metadata.purpose` | string | Concise explanation of how the topic is used end-to-end. |

All metadata is surfaced in `kafka-topics-configmap.yaml`, and every topic is created/validated by the `kafka-topic-init` job using the defined `partitions`, `replicationFactor`, and `config` values.

### Minimal Configuration Examples

Deploy with chart-managed credentials and a couple of declarative topics:

```yaml
# values-minimal.yaml
fullnameOverride: platform-kafka
kafka:
  auth:
    username: my-service
    password: superSecret123
  topics:
    user-events:
      partitions: 3
      replicationFactor: 2
      metadata:
        description: User-triggered lifecycle events
        producers:
          - webapp
        consumers:
          - audit-service
    dead-letter:
      partitions: 1
      replicationFactor: 2

exporters:
  kafka:
    enabled: false
```

Deploy pointing at an existing secret that already carries SASL credentials:

```yaml
# values-existing-secret.yaml
fullnameOverride: shared-message-bus
kafka:
  auth:
    existingSecret: kafka-shared-creds
    existingSecretKeys:
      username: sasl-user
      password: sasl-pass
```

### Security Hardening

- Pods for the controller, brokers, and exporter default to the Kubernetes restricted PodSecurity profile (non-root UID/GID 1000, `RuntimeDefault` seccomp) with container-level safeguards (no privilege escalation, all capabilities dropped).
- Each workload has an opt-in PodDisruptionBudget enabled by default; adjust `maxUnavailable` (or `minAvailable`) under the corresponding values block when tuning availability.
- Namespaced NetworkPolicies are rendered when `networkPolicy.enabled` is true, scoping ingress to namespace-local producers/consumers. Override the `ingress`/`egress` arrays per component to allow cross-namespace clients or additional control-plane access.

### Scaling and JVM Guidance

- JVM heap and performance flags are controlled via `kafka.controller.jvm` and `kafka.broker.jvm`. Defaults align with the bundled resource requests/limits (512Mi for the controller, 1Gi for brokers). Update these values alongside `resources` when sizing brokers for larger workloads.
- `autoscaling.broker` enables an `autoscaling/v2` HPA targeting the broker StatefulSet. Set CPU and/or memory utilisation thresholds explicitly; the template fails fast if neither is provided.
- The Kafka controller template currently supports a single controller replica. Setting `kafka.controller.replicaCount` to anything other than `1` now fails fast to avoid producing an invalid KRaft layout.
- `exporters.kafka.autoscaling` mirrors the HPA support for the exporter Deployment.
- A `keda.broker` block allows hooking Kafka into custom metrics-based scaling. Supply at least one trigger definition (for example a Prometheus alert on consumer lag) and ensure the KEDA CRDs are installed in the target cluster.

## Operational Runbooks

### Bootstrap

1. Set `fullnameOverride` to the release-specific base name; the chart fails fast when it is left empty.
2. Provision the target namespace with restricted PodSecurity labels (`enforce=restricted`, `audit=restricted`) before installing the chart.
3. Either leave `kafka.auth.password` empty to let the chart derive a deterministic password, or provide `kafka.auth.password`/`kafka.auth.existingSecret`. When `existingSecret` is supplied, the chart attempts to lookup the secret during template rendering. If lookup fails (e.g., due to RBAC restrictions in ArgoCD's repo server), provide `kafka.auth.username` and `kafka.auth.password` explicitly in values as a workaround—these will be used for ConfigMaps while deployments still reference the secret at runtime.
4. Confirm storage classes exist for the requested controller and broker `persistence.storageClass` values (or leave empty to use the cluster default).
5. Override `kafka.topics` with the initial topic catalogue, including metadata to drive the topic-init job and documentation configmap.
6. Run `helm install` with the desired overrides, then wait for the controller and brokers to become Ready before onboarding producers/consumers.
7. The topic init job runs as both a Helm hook (`post-install,post-upgrade`) and an Argo CD `PostSync` hook; it keeps a content-hashed name and the controller deletes any previous run before creating a new one, so upgrades always launch a fresh job without immutable selector issues.

### Topic Management

- Declaratively manage topics through `values.yaml` overrides; each change triggers the topic-init job to reconcile topic partitions, replication factor, and broker configs.
- Update `metadata` fields to keep the topics configmap authoritative for producers/consumers and schema references.
- Disable `kafka.autoCreateTopicsEnable` in production to avoid drift from untracked, automatically created topics; rely on chart-driven reconciliation instead.

### Disaster Recovery

- Take periodic volume snapshots for controller and broker PVCs using your storage provider’s tooling; record the cluster ID output from `include "kafka.kafka.clusterId"`.
- Before restore, scale the StatefulSets to zero, recover PVCs from snapshot, and ensure secrets/configmaps match the backed-up state.
- For cross-cluster migrations, seed a new cluster with identical `clusterId` and credentials, then replicate topics via MirrorMaker2 or consumer-driven replay.

## Troubleshooting

- Pods stuck in `CrashLoopBackOff` due to filesystem permission errors typically indicate custom images incompatible with the restricted security context; override the `*.containerSecurityContext` values as needed.
- If clients cannot reach brokers after install, review the generated NetworkPolicies and extend `networkPolicy.broker.ingress` to include the producing namespaces.
- Autoscaling template failures during `helm template` or `helm install` usually mean CPU/memory targets were omitted—set at least one threshold for each enabled autoscaler.
- KEDA scaling requires the CRDs and operator to be present; verify `kubectl get scaledobjects` works cluster-wide before enabling `keda.broker`.

## Compatibility Matrix

| Chart Version | Kafka Version | Kubernetes Versions | Notes |
| --- | --- | --- | --- |
| 0.1.2 | 4.1.0 | 1.27 – 1.30 | Validated with restricted PodSecurity admission, requires Helm 3.9+ for `fail` templating |

