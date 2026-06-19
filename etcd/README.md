# etcd

A minimal 3-node etcd cluster used as the leadership store (DCS) for the
`pg`/`pgvector` charts' lease-based HA agent when `repmgr.agent.dcs.backend=etcd`.

It is used two ways:

## 1. Bundled per release

Ships vendored inside the `pg` and `pgvector` chart packages (consumed via
`file://../etcd`, condition `etcd.enabled`) and enabled from the parent chart:

```yaml
# in pg / pgvector values
repmgr:
  agent:
    dcs:
      backend: etcd        # leadership in etcd instead of a Kubernetes Lease
etcd:
  enabled: true            # deploy this bundled cluster (no external etcd needed)
```

The parent then points the agent at the bundled cluster's client Service
(`<release>-etcd:2379`) automatically.

## 2. Standalone / shared

For several databases, prefer **one shared etcd** over a bundle per release. Install
this chart on its own and point each `pg`/`pgvector` release at it (leave
`etcd.enabled=false` on the parents):

```bash
helm repo add cagriekin-charts https://cagriekin.github.io/charts
helm install platform-etcd cagriekin-charts/etcd -n platform \
  --set 'networkPolicy.allowedClients[0].namespace=backend' \
  --set 'networkPolicy.allowedClients[1].namespace=sentry'
```

```yaml
# in each pg / pgvector release's values
repmgr:
  agent:
    dcs:
      backend: etcd
      etcd:
        endpoints: ["http://platform-etcd.platform.svc:2379"]
```

`networkPolicy.allowedClients` opens the client port (2379) to postgresql pods in the
named namespaces — the supported alternative to hand-writing cross-namespace
`extraIngress` selectors. `podSelector` is optional per entry and defaults to the
agent's `app.kubernetes.io/component: postgresql` label.

## What it deploys

- A 3-node etcd StatefulSet (static bootstrap, `podManagementPolicy: Parallel`),
  headless (peer + client) and stable client Services, a PDB (`maxUnavailable: 1`),
  and per-member PVCs.
- The chart's hardened securityContext (runAsNonRoot, drop ALL caps,
  readOnlyRootFilesystem; `fsGroup` makes the data PVC writable).
- A default soft anti-co-location topology spread across nodes.
- An ingress NetworkPolicy (default on): only the etcd peer mesh and the agent
  (the parent's postgresql pods, same release, plus any `networkPolicy.allowedClients`
  namespaces) may reach the etcd ports.

## Security

The bundled cluster runs **plaintext** within the pod network (the leadership data
is non-secret — pod names + a key prefix), isolated by the NetworkPolicy above. It
takes effect only on a NetworkPolicy-enforcing CNI. For a TLS-secured store, use a
BYO/shared etcd with `repmgr.agent.dcs.etcd.tls` instead of this bundle.

## Key values

| Key | Description | Default |
|-----|-------------|---------|
| `replicaCount` | etcd members (keep odd for quorum) | `3` |
| `image.repository` / `image.tag` | etcd image | `quay.io/coreos/etcd` / `v3.5.16` |
| `clientPort` / `peerPort` | etcd ports | `2379` / `2380` |
| `persistence.enabled` / `persistence.size` | data PVC | `true` / `2Gi` |
| `resources` | requests/limits | `100m`/`128Mi` … `1`/`512Mi` |
| `podDisruptionBudget.maxUnavailable` | PDB | `1` |
| `topologySpreadConstraints` | override the default soft hostname spread | `[]` |
| `networkPolicy.enabled` | ingress lockdown (needs a NP-enforcing CNI) | `true` |
| `networkPolicy.allowedClients` | cross-namespace client allow-list for a shared etcd (`[{namespace, podSelector?}]`) | `[]` |
| `networkPolicy.extraIngress` | extra client-port ingress (e.g. metrics scrape) | `[]` |

See the pg chart README ("Leadership backend") for the full agent-mode behavior.
