# Environment Variables

Environment variables injected into the chart's containers. All are set by the chart
from values or the downward API; none are read from a `.env` file. Required variables
are validated at render time (`helm fail`) or are always present by construction.

## Bootstrap init container (`redis-bootstrap`, replication mode)

Discovers the current master and renders the per-pod `redis.conf` / `sentinel.conf`.

| Variable | Type | Required | Source | Consumer |
|----------|------|----------|--------|----------|
| `POD_NAME` | string | yes | downward API `metadata.name` | `bootstrap.sh` (own FQDN) |
| `FULLNAME` | string | yes | `redis.fullname` | `bootstrap.sh` (peer names) |
| `HEADLESS_DOMAIN` | string | yes | `<fullname>-headless.<ns>.svc.<clusterDomain>` | `bootstrap.sh` (FQDNs) |
| `SENTINEL_SERVICE` | string | yes | `<fullname>-sentinel` | `bootstrap.sh` (discovery) |
| `NODE_COUNT` | number | yes | `redis.replicaCount + 1` | `bootstrap.sh` (peer scan) |
| `MASTER_NAME` | string | yes | `sentinel.masterName` | `bootstrap.sh`, sentinel.conf |
| `QUORUM` | number | yes | computed or `sentinel.quorum` | sentinel.conf `monitor` |
| `REDIS_PORT` | number | yes | `service.port` | redis.conf / sentinel.conf |
| `SENTINEL_PORT` | number | yes | `sentinel.service.port` | discovery, sentinel.conf |
| `DOWN_AFTER_MS` | number | yes | `sentinel.downAfterMilliseconds` | sentinel.conf |
| `FAILOVER_TIMEOUT` | number | yes | `sentinel.failoverTimeout` | sentinel.conf |
| `PARALLEL_SYNCS` | number | yes | `sentinel.parallelSyncs` | sentinel.conf |
| `TLS_ENABLED` | boolean | yes | `tls.enabled` | `bootstrap.sh` (redis-cli TLS) |
| `REDIS_PASSWORD` | string | when `redis.auth.enabled` | secretKeyRef | redis.conf `requirepass`/`masterauth`, `auth-pass` |
| `SENTINEL_PASSWORD` | string | when `redis.auth.enabled` | secretKeyRef | sentinel.conf `requirepass`, redis-cli discovery |

## `redis` container

| Variable | Type | Required | Source | Consumer |
|----------|------|----------|--------|----------|
| `REDISCLI_AUTH` | string | when `redis.auth.enabled` | secretKeyRef (redis password) | probes, preStop, redis-cli |
| `REDIS_PASSWORD` | string | standalone + auth | secretKeyRef | `redis-server --requirepass` (standalone only) |

## `sentinel` container (replication mode)

| Variable | Type | Required | Source | Consumer |
|----------|------|----------|--------|----------|
| `REDISCLI_AUTH` | string | when `redis.auth.enabled` | secretKeyRef (sentinel password) | sentinel probes / redis-cli |

## `redis-exporter` (sidecar in replication, Deployment in standalone)

| Variable | Type | Required | Source | Consumer |
|----------|------|----------|--------|----------|
| `POD_NAME` | string | replication | downward API `metadata.name` | builds the per-pod `REDIS_ADDR` |
| `REDIS_ADDR` | string | yes | `redis[s]://<addr>:<port>` | exporter scrape target |
| `REDIS_PASSWORD` | string | when `redis.auth.enabled` | secretKeyRef | exporter auth |
| `REDIS_EXPORTER_TLS_CLIENT_KEY_FILE` | string | when `tls.enabled` | `/etc/redis/tls/tls.key` | exporter TLS |
| `REDIS_EXPORTER_TLS_CLIENT_CERT_FILE` | string | when `tls.enabled` | `/etc/redis/tls/tls.crt` | exporter TLS |
| `REDIS_EXPORTER_TLS_CA_CERT_FILE` | string | when `tls.enabled` | `/etc/redis/tls/ca.crt` | exporter TLS |
