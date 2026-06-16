# etcd (bundled subchart)

A minimal 3-node etcd cluster used as the leadership store (DCS) for the
`pg`/`pgvector` charts' lease-based HA agent when `repmgr.agent.dcs.backend=etcd`.

This is a **dependency-only subchart** — it is not published or installed on its
own. It ships vendored inside the `pg` and `pgvector` chart packages (consumed via
`file://../etcd`, condition `etcd.enabled`) and is enabled from the parent chart:

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
(`<release>-etcd:2379`) automatically. For multiple databases, prefer one
shared/BYO etcd (`repmgr.agent.dcs.etcd.endpoints`) over a bundle per release.

## What it deploys

- A 3-node etcd StatefulSet (static bootstrap, `podManagementPolicy: Parallel`),
  headless (peer + client) and stable client Services, a PDB (`maxUnavailable: 1`),
  and per-member PVCs.
- The chart's hardened securityContext (runAsNonRoot, drop ALL caps,
  readOnlyRootFilesystem; `fsGroup` makes the data PVC writable).
- A default soft anti-co-location topology spread across nodes.
- An ingress NetworkPolicy (default on): only the etcd peer mesh and the agent
  (the parent's postgresql pods, same release) may reach the etcd ports.

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
| `networkPolicy.extraIngress` | extra client-port ingress (e.g. metrics scrape) | `[]` |

See the pg chart README ("Leadership backend") for the full agent-mode behavior.
