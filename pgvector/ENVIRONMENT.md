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

## repmgr (both failover modes, when `repmgr.enabled=true`)

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `REPMGR_USER` | string | yes | `repmgr.username` | entrypoint, init-repmgr, agent |
| `REPMGR_PASSWORD` | string | yes | secret (`repmgr-password`) | entrypoint, init-repmgr, agent |
| `REPMGR_DB` | string | yes | `repmgr.database` | entrypoint, init-repmgr, agent |
| `HEADLESS_SERVICE` | string | yes | `<fullname>-headless.<ns>.svc.cluster.local` | init-repmgr, agent (peer FQDNs) |
| `REPMGR_NODE_COUNT` | number | yes | `postgresql.replicaCount + 1` | init-repmgr, agent (peer enumeration) |
| `NAMESPACE` | string | yes | fieldRef `metadata.namespace` | guard, agent |
| `PRIMARY_MARKER` | string | yes | `<fullname>-primary` | guard, agent (#125 highwater) |

## HA only (`repmgr.enabled=true`)

The lease-based Go agent (`pg-ha-agent`, PID 1 in the postgresql container) reads
these; `config.Load` fail-fasts at boot if any is missing.

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `POD_NAME` | string | yes | fieldRef `metadata.name` (the Lease holder identity) | agent |
| `LEASE_NAME` | string | yes | `<fullname>-leader` | agent (leadership Lease) |
| `LEASE_DURATION` | duration | yes | `repmgr.agent.leaseDuration` (15s) | agent (leaderelection) |
| `RENEW_DEADLINE` | duration | yes | `repmgr.agent.renewDeadline` (10s) | agent |
| `RETRY_PERIOD` | duration | yes | `repmgr.agent.retryPeriod` (2s) | agent |
| `RECONCILE_INTERVAL` | duration | yes | `repmgr.agent.reconcileInterval` (5s) | agent (tick) |
| `MASTER_SERVICE` | string | yes | `<fullname>` (write Service whose selector the agent patches) | agent |
| `POD_SELECTOR` | string | yes | chart selector labels + `component=postgresql` | agent (pg-role labeling) |
| `DCS_BACKEND` | enum | yes | `repmgr.agent.dcs.backend` (`kubernetes`/`etcd`) | agent (leadership store) |
| `SPLIT_BRAIN_ACTION` | enum | yes | `repmgr.splitBrainDetection.action` (`log`/`fence`) | agent |
| `POD_CIDR` | CIDR | yes | `repmgr.agent.podCidr` (`10.0.0.0/8`) | agent (hardened pg_hba: trusted pod network) |
| `POSTGRESQL_PGHBA` | newline-list | no | `postgresql.pgHba` (joined) | agent (user pg_hba rules, above the catch-alls) |
| `CASCADE_REPLICATION` | boolean | no | `repmgr.agent.cascadingReplication` (`false`) | agent (cascading replication, #29; emitted only when true) |
| `MECHANISM` | enum | no | `repmgr.agent.mechanism` (`repmgr`/`native`) | agent (HA mechanics, #287, EXPERIMENTAL; emitted only when non-default), `init-repmgr.sh` and `entrypoint.sh` (#288 -- under `native` the init container skips repmgr.conf, the `repmgr.nodes` registration wait and the repmgr clone, and the stale-primary guard and `CREATE EXTENSION repmgr` are skipped) |

Lease timings must satisfy `LEASE_DURATION > RENEW_DEADLINE > RETRY_PERIOD`
(validated at boot). The agent also writes a `0600 ~/.pgpass` from `REPMGR_*` so a
passwordless `primary_conninfo` can authenticate streaming replication.

### etcd backend only (`repmgr.agent.dcs.backend=etcd`)

Required only when the leadership store is etcd; with the bundled etcd subchart
(`etcd.enabled=true`) the chart fills `ETCD_ENDPOINTS` automatically.

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `ETCD_ENDPOINTS` | csv | yes (etcd) | `repmgr.agent.dcs.etcd.endpoints`, or the bundled `<release>-etcd:2379` | agent (etcd client) |
| `ETCD_PREFIX` | string | yes (etcd) | `repmgr.agent.dcs.etcd.prefix` or `/pg-ha/<release>/` | agent (election key prefix) |
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
| `REPMGR_FAILOVER` | `init-repmgr.sh` (`automatic`/`manual`) | none — the agent always writes `failover=manual` into `repmgr.conf` at boot |

`MASTER_SERVICE` and `SPLIT_BRAIN_ACTION` were listed here too; both survive and are
consumed by the agent (see the HA table above).

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
