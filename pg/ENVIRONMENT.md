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
| `SYNC_REPLICATION_SLOTS` | boolean | no | `repmgr.agent.syncReplicationSlots` (`false`) | agent (logical failover slot sync, #308; emitted only when true) |
| `USE_REPLICATION_SLOTS` | boolean | no | `"1"` (always; agent is the only failover path since 2.0.0) | repmgr-init (initial `standby clone`, #308; matches the agent's own regenerated `repmgr.conf`) |
| `MECHANISM` | enum | no | `repmgr.agent.mechanism` (`native` only) | agent (HA mechanics, #287). **Always emitted since #294**, not only at a non-default value: `native` is now the default, and an agent built before #294 assumes `repmgr` when this is absent, so omitting it would run the removed mechanism during a two-step image-then-chart release. Also read by `init-repmgr.sh` and `entrypoint.sh` (#288 -- under `native` the init container skips repmgr.conf, the `repmgr.nodes` registration wait and the repmgr clone, and the stale-primary guard and `CREATE EXTENSION repmgr` are skipped). An explicit `repmgr` is rejected at render time |
| `PG_MAJOR` | digits | no | image `ENV` from the Dockerfile's `ARG PG_MAJOR` (`18`) | agent (versioned bindir `/usr/lib/postgresql/<major>/bin`, #269) |
| `PGBACKREST_ENABLED` | boolean | no | `"true"` when `pgbackrest.enabled` | agent (control API backup routes) |
| `PGBACKREST_STANZA` | string | no | `pgbackrest.stanza` | agent (`pgbackrest info`), archive_command |

Lease timings must satisfy `LEASE_DURATION > RENEW_DEADLINE > RETRY_PERIOD`
(validated at boot). The agent also writes a `0600 ~/.pgpass` from `REPMGR_*` so a
passwordless `primary_conninfo` can authenticate streaming replication.

### Not injected by the chart: `KUBECONFIG` (#317)

`KUBECONFIG` is the one variable the agent reads that the chart never sets. When it
is present the agent (and the entrypoint's `kubectl` stale-primary guard) reaches the
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
redirect the entrypoint's stale-primary guard, which is why the chart keeps them apart.

Set but unreadable/malformed/contextless is a startup failure naming the file, never a
silent fall back to in-cluster. `~/.kube/config` is deliberately not consulted. The boot
log's `apiserver=` field records which route was taken. Note that `kubectl` is *not* as
strict — its deferred loader does fall back to in-cluster on an empty or contextless
kubeconfig — so a broken mount degrades the entrypoint's `#170` settle guard to its
fail-open fast path while the agent refuses to boot. Only the postgresql container is
involved either way; the `repmgr-init` init container makes no apiserver calls. See the
chart README section *Routing the agent's apiserver traffic* for the kubeconfig to mount.

### control API only (`repmgr.agent.control.enabled=true`)

Emitted only when the control REST API (#276) is on, and validated at boot: enabling it
without all three TLS files, or restore without an allowlist, is a single fail-fast error
(the chart also render-guards both, so the failure normally arrives at `helm upgrade`).

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `CONTROL_ENABLED` | boolean | no | `"true"` when `repmgr.agent.control.enabled` | agent (control listener) |
| `CONTROL_ADDR` | host:port | no | `:<repmgr.agent.control.port>` (`:9201`) | agent (never the 9200 metrics port) |
| `CONTROL_TLS_CERT` | path | yes¹ | `/etc/agent-control-tls/tls.crt` | agent (server identity) |
| `CONTROL_TLS_KEY` | path | yes¹ | `/etc/agent-control-tls/tls.key` | agent |
| `CONTROL_TLS_CA` | path | yes¹ | `/etc/agent-control-tls/ca.crt` | agent (verifies CLIENT certificates) |
| `CONTROL_ALLOWED_CNS` | csv | no | `repmgr.agent.control.allowedClientCNs` | agent (empty = any cert the CA signed) |
| `CONTROL_RESTORE_ENABLED` | boolean | no | `repmgr.agent.control.restore.enabled` | agent (`POST /v1/restore`) |
| `CONTROL_RESTORE_ALLOWED_CNS` | csv | yes² | `repmgr.agent.control.restore.allowedClientCNs` | agent (separate authz verb; empty denies everyone) |
| `CONTROL_RESTORE_CRONJOB` | string | yes² | `<fullname>-pgbackrest-restore` | agent (the jobTemplate it clones) |
| `CONTROL_RESTORE_JOB_NAME` | string | yes² | `<fullname>-pgbackrest-restore-api` | agent (deterministic, so RBAC get/delete can be resourceName-scoped) |
| `CONTROL_RESTORE_POD_ORDINAL` | integer | yes² | `pgbackrest.restore.podOrdinal` | agent (confirm-only: the request may echo it, never change it) |
| `CONTROL_RESTORE_READ_POD_LOGS` | boolean | no | `repmgr.agent.control.restore.readPodLogs` (`false`) | agent (live copy progress; adds namespace-wide `get pods/log`) |

¹ Required when `CONTROL_ENABLED` — mTLS is the only authentication mode, so the agent
refuses to boot rather than open an unauthenticated mutating port.
² Required when `CONTROL_RESTORE_ENABLED`.

The restore outcome record the API reads back as `lastRestore` is not an env var: it is
`<dirname of PGDATA>/pgbackrest-restore.status`, written by the chart's `restore.sh` onto
the data volume so it outlives the Job. The agent **removes** it when the data directory
stops being what it describes — a `POST /v1/reinitialize` wipe, or a clone by the reconcile
loop — so it never reports a backup set as the provenance of data cloned from a peer.

Two runtime behaviours worth knowing before an incident:

- With `CONTROL_RESTORE_ENABLED`, **`POST /v1/resume` reads the restore Job on every call**
  and fails closed: a transient apiserver error answers `502` and the cluster stays paused
  (so no failover) until the read succeeds. That is deliberate — resuming while pgbackrest
  is rewriting the data directory is the worse outcome — but it does make resume depend on
  the apiserver, which it does not when restore is off. Note the check reads **only**
  `CONTROL_RESTORE_JOB_NAME`, the Job the API creates: a restore started by hand
  (`kubectl create job --from=cronjob/<fullname>-pgbackrest-restore`) has a different name
  and is not seen, so keep the cluster paused for the whole of a hand-started restore and
  clear the pause the same way you set it.
- A control listener that **fails at startup is fatal** (the agent will not run with a
  silently missing API), but one that dies *later* is logged and not retried: HA is left
  intact rather than taking the database down with the API. The asymmetry means a healthy
  cluster can be serving with no control surface, so alert on
  `pg_ha_agent_control_requests_total` going flat if you automate against the API.

### etcd backend only (`repmgr.agent.dcs.backend=etcd`)

Required only when the leadership store is etcd; `config.Load` fail-fasts on a
missing endpoint/prefix in that mode. With the bundled etcd subchart
(`etcd.enabled=true`) the chart fills `ETCD_ENDPOINTS` automatically.

| Variable | Type | Required | Default / source | Consumer |
|----------|------|----------|------------------|----------|
| `ETCD_ENDPOINTS` | csv | yes (etcd) | `repmgr.agent.dcs.etcd.endpoints`, or the bundled `<release>-etcd:2379` | agent (etcd client) |
| `ETCD_PREFIX` | string | yes (etcd) | `repmgr.agent.dcs.etcd.prefix` or `/pg-ha/<release>/` | agent (election key prefix) |
| `ETCD_TLS_CERT` | path | no | `/etc/etcd-tls/tls.crt` when `dcs.etcd.tls.secretName` set | agent (mutual TLS) |
| `ETCD_TLS_KEY` | path | no | `/etc/etcd-tls/tls.key` when `dcs.etcd.tls.secretName` set | agent (mutual TLS) |
| `ETCD_TLS_CA` | path | no | `/etc/etcd-tls/ca.crt` when `dcs.etcd.tls.secretName` set | agent (mutual TLS) |

`LEASE_DURATION` must be `>= 5s` in etcd mode (the etcd lease TTL is whole
seconds). TLS env is all-or-none; the secret must carry `tls.crt`, `tls.key`, and
`ca.crt`.

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
