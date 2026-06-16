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
| `NAMESPACE` | string | yes | fieldRef `metadata.namespace` | guard, agent, service-updater |
| `PRIMARY_MARKER` | string | yes | `<fullname>-primary` | guard, agent, service-updater (#125 highwater) |

## agent mode only (`repmgr.failoverMode=agent`)

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
| `DCS_BACKEND` | enum | yes | `kubernetes` (`etcd` planned) | agent |
| `SPLIT_BRAIN_ACTION` | enum | yes | `repmgr.splitBrainDetection.action` (`log`/`fence`) | agent |

Lease timings must satisfy `LEASE_DURATION > RENEW_DEADLINE > RETRY_PERIOD`
(validated at boot). The agent also writes a `0600 ~/.pgpass` from `REPMGR_*` so a
passwordless `primary_conninfo` can authenticate streaming replication.

## repmgrd mode only (`repmgr.failoverMode=repmgrd`, the default)

The service-updater sidecar additionally consumes (in addition to the repmgr set):

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `MASTER_SERVICE` | string | yes | `<fullname>` | service-updater (selector patch) |
| `SPLIT_BRAIN_ACTION` | enum | yes | `repmgr.splitBrainDetection.action` | service-updater |
| `MONITORING_HISTORY_DAYS` | number | yes | `repmgr.monitoringHistoryDays` | repmgrd sidecar (cleanup) |
| `PGPOOL_DEPLOYMENT` / `PGPOOL_SERVICE` / `PGPOOL_PORT` | string/number | when `pgpool.enabled` | `<fullname>-pgpool*` | service-updater |

`REPMGR_FAILOVER` (`automatic`/`manual`) is honored by `init-repmgr.sh` when set;
the chart leaves it default (`automatic`) and the agent rewrites `repmgr.conf` to
`failover=manual` at boot in agent mode.

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
