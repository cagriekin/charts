# Environment variables

Environment variables injected by the chart into the containers it runs. The
chart injects these from `values.yaml` / Kubernetes Secrets; they are not meant to
be set by hand. The `pgvector` chart shares these templates and injects the same
set.

Required/optional is from the consuming process's perspective at runtime. Secrets
(`*_PASSWORD`) come from the chart-managed Secret or `postgresql.existingSecret`.

## postgresql container (always)

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `PGDATA` | string | yes | `/var/lib/postgresql/data/pgdata` | postgres / entrypoint |
| `POSTGRES_USER` | string | yes | secret (`username`) | entrypoint, exporter, pgpool |
| `POSTGRES_PASSWORD` | string | yes | secret (`password`) | entrypoint |
| `POSTGRES_DB` | string | yes | secret (`database`) | entrypoint |
| `LD_LIBRARY_PATH` | string | no | `/usr/lib/postgresql/<major>/extra-lib` | dynamic linker (`postgresql.extensions.extraLibs`, #309; emitted only when non-empty) |

## repmgr (both failover modes, when `ha.enabled=true`)

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `REPMGR_USER` | string | yes | `ha.username` | entrypoint (creates the role), agent (replication auth) |
| `REPMGR_PASSWORD` | string | yes | secret (`repmgr-password`) | entrypoint (creates the role), agent (replication auth) |
| `REPMGR_DB` | string | yes | `ha.database` | entrypoint (creates the database), agent (`dbname` in `primary_conninfo`) |
| `HEADLESS_SERVICE` | string | yes | `<fullname>-headless.<ns>.svc.cluster.local` | agent (peer FQDNs) |
| `REPMGR_NODE_COUNT` | number | yes | `postgresql.replicaCount + 1` | agent (peer enumeration) |
| `NAMESPACE` | string | yes | fieldRef `metadata.namespace` | agent |
| `PRIMARY_MARKER` | string | yes | `<fullname>-primary` | agent (#125 highwater) |

## HA only (`ha.enabled=true`)

The lease-based Go agent (`pg-ha-agent`, PID 1 in the postgresql container) reads
these; `config.Load` fail-fasts at boot if any is missing.

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `POD_NAME` | string | yes | fieldRef `metadata.name` (the Lease holder identity) | agent |
| `LEASE_NAME` | string | yes | `<fullname>-leader` | agent (leadership Lease) |
| `LEASE_DURATION` | duration | yes | `ha.agent.leaseDuration` (15s) | agent (leaderelection) |
| `RENEW_DEADLINE` | duration | yes | `ha.agent.renewDeadline` (10s) | agent |
| `RETRY_PERIOD` | duration | yes | `ha.agent.retryPeriod` (2s) | agent |
| `RECONCILE_INTERVAL` | duration | yes | `ha.agent.reconcileInterval` (5s) | agent (tick) |
| `MASTER_SERVICE` | string | yes | `<fullname>` (write Service whose selector the agent patches) | agent |
| `POD_SELECTOR` | string | yes | chart selector labels + `component=postgresql` | agent (pg-role labeling) |
| `DCS_BACKEND` | enum | yes | `ha.agent.dcs.backend` (`kubernetes`/`etcd`) | agent (leadership store) |
| `POD_CIDR` | CIDR | yes | `ha.agent.podCidr` (`10.0.0.0/8`) | agent (hardened pg_hba: trusted pod network) |
| `POSTGRESQL_PGHBA` | newline-list | no | `postgresql.pgHba` (joined) | agent (user pg_hba rules, above the catch-alls) |
| `TLS_ENABLED` | boolean | no | `"true"` when `postgresql.tls.enabled` (emitted only then) | agent (#335: verifies from the running postmaster that `SHOW ssl` is actually `on`, and publishes `pg_ha_agent_tls_inactive` when it is not) |
| `TLS_REQUIRE_SSL` | boolean | no | `postgresql.tls.require` (emitted only under `postgresql.tls.enabled`) | agent (pg_hba: `hostssl` instead of `host` on the pod CIDR) |
| `TLS_CLIENT_CERT_AUTH` | boolean | no | `postgresql.tls.clientCertAuth` (emitted only under `postgresql.tls.enabled`) | agent (pg_hba: `clientcert=verify-ca` on the app catch-all, with per-user exemptions for the replication/superuser/monitoring roles) |
| `MONITORING_USER` | string | no | `prometheusExporter.monitoringUser.username` (emitted only when the exporter and its managed user are enabled, under `postgresql.tls.enabled`) | agent (pg_hba: exempts the exporter's role from `clientcert=verify-ca`, which it cannot present) |
| `MIGRATE_LEGACY_MD5_USERS` | boolean | no | `postgresql.migrateLegacyMd5Users` (always emitted) | agent (#199: re-hashes the managed users md5 -> scram-sha-256 once this node is serving read-write, replacing the postStart hook that never ran on an in-process promotion) |
| `CASCADE_REPLICATION` | boolean | no | `ha.agent.cascadingReplication` (`false`) | agent (cascading replication, #29; emitted only when true) |
| `SYNC_REPLICATION_SLOTS` | boolean | no | `ha.agent.syncReplicationSlots` (`false`) | agent (logical failover slot sync, #308; emitted only when true) |
| ~~`USE_REPLICATION_SLOTS`~~ | — | — | **removed in 2.0.0 (#290)** | Nothing. It configured the init container's `repmgr standby clone`; the agent owns cloning and always uses a slot |
| `MECHANISM` | enum | no | `ha.agent.mechanism` (`native` only) | agent (HA mechanics, #287). **Always emitted since #294** -- `native` is the default, and an agent built before #294 assumes `repmgr` when this is absent, so omitting it would run the removed mechanism during a two-step image-then-chart release. The shell no longer reads it -- the repmgr paths it gated are gone (#290). An explicit `repmgr` is rejected at render time |

Lease timings must satisfy `LEASE_DURATION > RENEW_DEADLINE > RETRY_PERIOD`
(validated at boot). The agent also writes a `0600 ~/.pgpass` from `REPMGR_*` so a
passwordless `primary_conninfo` can authenticate streaming replication.

### Not injected by the chart: `KUBECONFIG` (#317)

`KUBECONFIG` is the one variable the agent reads that the chart never sets. When it
is present the agent reaches the
apiserver through that kubeconfig instead of the in-cluster ServiceAccount; when it is
absent — the default — the in-cluster path is used exactly as before. It exists so a
cluster whose egress policy denies pod traffic to the apiserver can route the agent
through an in-cluster proxy, which needs a different **address** while still verifying
the apiserver's own **certificate** (`server:` + `tls-server-name:`).

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `KUBECONFIG` | path(s) | no | operator-supplied via `postgresql.extraEnv` (postgresql container) or `pgbackrest.extraEnv` (#323, every pgbackrest container); unset by default | agent (mutation client **and** the Lease DCS), `kubectl` — including the backup CronJob's |

The two supply routes are separate lists on purpose and a cluster that needs this route
needs it in both. `postgresql.extraEnv` reaches the postgresql container only; the
pgBackRest backup CronJob runs in its own pod and is an apiserver client in its own right
(it resolves the primary from EndpointSlices, then drives pgBackRest with `kubectl exec`),
so it takes `KUBECONFIG` from `pgbackrest.extraEnv` instead — which also carries it to the
`pgbackrest` sidecar, the `pgbackrest-bootstrap` init container, and the restore and
validation workloads. Injecting it through `postgresql.extraEnv` would additionally
keep the two `extraEnv` surfaces apart so a KUBECONFIG meant for pgBackRest cannot silently
redirect the agent's own API client.

Set but unreadable/malformed/contextless is a startup failure naming the file, never a
silent fall back to in-cluster. `~/.kube/config` is deliberately not consulted. The boot
log's `apiserver=` field records which route was taken. Note that `kubectl` is *not* as
strict — its deferred loader does fall back to in-cluster on an empty or contextless
kubeconfig — so the backup CronJob's `kubectl exec` may silently take the in-cluster route
while the agent refuses to boot. The entrypoint itself makes no apiserver calls any more
(#290 removed its `kubectl` settle guard). Only the postgresql container is
involved either way; the `repmgr-init` init container makes no apiserver calls. See the
chart README section *Routing the agent's apiserver traffic* for the kubeconfig to mount.

### etcd backend only (`ha.agent.dcs.backend=etcd`)

Required only when the leadership store is etcd; with the bundled etcd subchart
(`etcd.enabled=true`) the chart fills `ETCD_ENDPOINTS` automatically.

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `ETCD_ENDPOINTS` | csv | yes (etcd) | `ha.agent.dcs.etcd.endpoints`, or the bundled `<release>-etcd:2379` | agent (etcd client) |
| `ETCD_PREFIX` | string | yes (etcd) | `ha.agent.dcs.etcd.prefix` or `/pg-ha/<release>/` | agent (election key prefix) |
| `ETCD_TLS_CERT` / `ETCD_TLS_KEY` / `ETCD_TLS_CA` | path | no | `/etc/etcd-tls/{tls.crt,tls.key,ca.crt}` when `dcs.etcd.tls.secretName` set | agent (mutual TLS) |

`LEASE_DURATION` must be `>= 5s` in etcd mode (the etcd lease TTL is whole
seconds). TLS env is all-or-none; the secret must carry `tls.crt`, `tls.key`, `ca.crt`.

## Removed in 2.0.0 (#286)

The repmgrd failover path and its service-updater sidecar were removed, and with them
these variables. Nothing injects or reads them any more:

| Variable | Was consumed by | Replacement |
|----------|-----------------|-------------|
| `MONITORING_HISTORY_DAYS` | repmgrd sidecar (`repmgr cluster cleanup`) | none — only repmgrd wrote `repmgr.monitoring_history` |
| `PGPOOL_DEPLOYMENT` / `PGPOOL_SERVICE` / `PGPOOL_PORT` | service-updater (pgpool restart on failover) | none — PGPool-II follows the Services the agent re-points |
| `REPMGR_FAILOVER` | `init-repmgr.sh` (`automatic`/`manual`) | none — there is no repmgr and no `repmgr.conf`; the agent is the only failover path (#290) |
| `SPLIT_BRAIN_ACTION` | service-updater (`handle_split_brain()`) | none — the agent demotes on lease loss unconditionally, so `log`/`fence` selected the same behaviour; the env and its `ha.splitBrainDetection` value were removed |

`MASTER_SERVICE` was listed here too; it survives and is consumed by the agent (see
the HA table above).

## pgbackrest (when `pgbackrest.enabled=true`)

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `PGBACKREST_ENABLED` | bool | yes | `true` | entrypoint (archive/restore commands) |
| `PGBACKREST_STANZA` | string | yes | `pgbackrest.stanza` | entrypoint, pgbackrest sidecar |
| `PGBACKREST_REPO1_S3_*` | string | yes | `pgbackrest.existingSecret` | pgbackrest sidecar |

## metrics / exporters

The prometheus-exporter and pgpool containers receive credentials via their own
init-rendered config (see `prometheusExporter` / `pgpool` in `values.yaml`); no
additional process-level required variables beyond the secret-sourced credentials.
