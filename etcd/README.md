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
helm repo add cagriekin https://cagriekin.github.io/charts
helm install platform-etcd cagriekin/etcd -n platform \
  --set fullnameOverride=platform-etcd \
  --set 'networkPolicy.allowedClients[0].namespace=backend' \
  --set 'networkPolicy.allowedClients[1].namespace=sentry'
```

`fullnameOverride` pins the client Service name (otherwise it is `<release>-etcd`,
e.g. `platform-etcd-etcd`); the endpoint below must match whichever name renders.

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

By default the cluster runs **plaintext** within the pod network (the leadership
data is non-secret — pod names + a key prefix), isolated by the NetworkPolicy above
(which takes effect only on a NetworkPolicy-enforcing CNI). That is fine for a
single-release bundle. For a **shared/standalone** etcd that holds several releases'
leadership keys, enable transport TLS so only cert-holding clients can connect.

### Transport TLS

`tls.enabled` makes etcd serve client (and peer) TLS from a Secret, with
`--client-cert-auth` (mutual TLS) so a client without a cert signed by the CA cannot
connect — the real access control across releases, where the NetworkPolicy only gates
pod reachability.

```yaml
tls:
  enabled: true
  existingSecret: etcd-server-tls   # keys: tls.crt, tls.key, ca.crt
  clientCertAuth: true
  peer:
    enabled: true                   # encrypt the member mesh too
```

The server certificate (e.g. a cert-manager `Certificate`) **must**:

- list SANs for the client Service (`<release>-etcd`, `<release>-etcd.<ns>.svc.<domain>`),
  the headless peer FQDNs (`<release>-etcd-<0..N>.<release>-etcd-headless.<ns>.svc.<domain>`),
  and `127.0.0.1` (the health probe connects to localhost);
- carry **both** server-auth and client-auth usages — the readiness/liveness probes run
  `etcdctl endpoint health` with this cert as a client. With cert-manager:
  `usages: [server auth, client auth]`.

The parent `pg`/`pgvector` agent then connects over `https` with its own client cert via
`repmgr.agent.dcs.etcd.tls.secretName`; with the bundled etcd the parent auto-switches the
endpoint to `https` and fails the render if that client Secret is missing under
client-cert-auth.

### Per-tenant isolation (RBAC)

Transport TLS gates *who can connect*, but any cert-holding client can still read or
rewrite the whole keyspace. For a shared store, `rbac.enabled` adds **per-prefix
isolation**: each tenant gets an etcd user (matched by its client-cert **Common Name**)
and a role granting `readwrite` only on its key prefix, so one release cannot touch
another's leadership keys. Auth is by CN — there are no passwords, and the agent needs
no change beyond a client cert whose CN equals its tenant `commonName`.

```yaml
tls:
  enabled: true
  existingSecret: etcd-server-tls
  clientCertAuth: true            # required for RBAC (the CN is the user)
rbac:
  enabled: true
  adminSecret: etcd-admin-tls     # admin client cert; its CN MUST be "root"
  tenants:
    - commonName: backend-pg      # = CN in the backend release's agent client cert
      prefix: /pg-ha/backend/     # = that release's ETCD_PREFIX
    - commonName: sentry-pg
      prefix: /pg-ha/sentry/
```

A post-install/upgrade Job creates the users/roles and enables auth (all idempotent,
so it reconciles the tenant list on every upgrade). The admin Secret's cert CN must be
`root` (etcd's reserved superuser, which `auth enable` requires). At least one
`tenants` entry is required — enabling auth with no tenant user would lock every
CN-authenticated agent out (the chart fails the render if `tenants` is empty). Because
the bundled etcd image is distroless (no shell; it ships etcd + etcdctl only), the Job
runs `pg-ha-agent rbac-bootstrap` from the `rbac.bootstrapImage` (the pg-ha-agent/repmgr
image by default) and drives the etcd Auth API via the Go client; keep that image's tag
in lockstep with your `pg`/`pgvector`
release. Each consuming release sets `repmgr.agent.dcs.etcd.tls.secretName` to a client
cert whose **CN matches its `commonName`** and `repmgr.agent.dcs.etcd.prefix` to the
matching `prefix`.

> Enabling auth is **cluster-global and not auto-reverted**: removing a tenant from the
> list does not revoke its access (delete the user manually), and disabling `rbac`
> later requires `etcdctl auth disable` as root.

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
| `tls.enabled` | serve client + peer TLS (shared etcd) | `false` |
| `tls.existingSecret` | server cert Secret (`tls.crt`, `tls.key`, `ca.crt`) | `""` |
| `tls.clientCertAuth` | require clients to present a trusted cert (mutual TLS) | `true` |
| `tls.peer.enabled` / `tls.peer.existingSecret` | encrypt the member mesh / its cert Secret | `true` / `""` |
| `rbac.enabled` | per-tenant key-prefix isolation (needs `tls.enabled` + `clientCertAuth`) | `false` |
| `rbac.adminSecret` | admin client cert Secret (CN must be `root`) | `""` |
| `rbac.tenants` | per-tenant grants (`[{commonName, prefix}]`) | `[]` |
| `rbac.bootstrapImage` | image running `pg-ha-agent rbac-bootstrap` (etcd image has no shell) | `cagriekin/repmgr:trixie-5.5.0-23` |
| `rbac.resources` | bootstrap Job container resources | small requests/limits |
| `networkPolicy.enabled` | ingress lockdown (needs a NP-enforcing CNI) | `true` |
| `networkPolicy.allowedClients` | cross-namespace client allow-list for a shared etcd (`[{namespace, podSelector?}]`) | `[]` |
| `networkPolicy.extraIngress` | extra client-port ingress (e.g. metrics scrape) | `[]` |

See the pg chart README ("Leadership backend") for the full agent-mode behavior.
