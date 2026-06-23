# Changelog

## 1.2.0

ACL support (non-breaking; `redis.auth.acl.enabled` defaults `false`, so existing installs
are unchanged).

### Added
- Redis ACL users via `redis.auth.acl` (#87): layer named, per-command/per-key users on top
  of password auth, in both `standalone` and `replication`. Passwords stay out of the
  ConfigMap (rendered into `redis.conf` at runtime from Secret-backed env).
- A chart-managed **operator user** (`redis.auth.acl.operatorUser`) — the privileged identity
  used for replication (`masteruser`/`masterauth`), Sentinel (`auth-user`/`auth-pass`), the
  exporter (`REDIS_USER`), and probes; lets you lock down the `default` user.
- Fail-fast validation of ACL config (auth prerequisite, username charset, no
  `on`/`off`/`nopass`/`reset*`/`>`/`#` in `rules`, operator/`default` interlocks).

## 1.1.0

Operability and production-hardening (non-breaking; defaults unchanged).

### Added
- Fail-fast validation that `redis.config.maxmemory` does not exceed
  `redis.resources.limits.memory` (#82).
- `imagePullSecrets` applied to every pod — redis, sentinel, exporter, and the
  bootstrap init container (#84).
- Configurable production `redis.conf` settings: `loglevel`, `databases`,
  `slowlog-log-slower-than`, `slowlog-max-len`, `client-output-buffer-limit`, an
  explicit `dir /data`, and configurable `timeout` / `tcp-keepalive` (#81, #78).
- Complete `Chart.yaml` metadata: `home`, `icon`, `sources`, `maintainers` (#85).

## 1.0.0

High-availability replication via Redis Sentinel. **Breaking**: the default
`architecture` is now `replication`.

### Added
- `architecture: standalone | replication` toggle. `replication` runs a Redis master
  plus replicas with a Sentinel sidecar in every pod and automatic failover.
- Sentinel-aware client support: a `<fullname>-sentinel` Service on `:26379` for
  `SENTINEL get-master-addr-by-name`, and a headless Service for stable per-pod DNS.
- Failover-safe bootstrap (`redis-bootstrap` init container): discovers the live master
  from the Sentinels and joins as a replica, so a restarted ex-master never resurrects
  read-write. Config is rendered into an in-memory (`tmpfs`) volume so runtime rewrites
  never persist secrets to disk.
- Stable-DNS announcing (`replica-announce-ip`, `sentinel announce-ip` +
  `resolve-hostnames`/`announce-hostnames`) so pod-IP churn never strands the cluster.
- Write safety: `min-replicas-to-write` / `min-replicas-max-lag` (default 1 / 10).
- Secure-by-default: `redis.auth.enabled` and `networkPolicy.enabled` now default `true`.
  When no `existingSecret` is given the chart generates a random password Secret
  (persisted across upgrades via `lookup`).
- In-transit TLS (`tls.*`): TLS-only Redis + Sentinel + replication, optional mutual TLS
  (`tls.clientCertAuth`), exporter over TLS.
- Per-pod metrics: the exporter runs as a sidecar in replication so role/replication
  metrics exist per member; new HA alerts (master down, split-brain, replica/link down,
  writes-blocked tripwire).
- Scheduling knobs (`redis.affinity` default podAntiAffinity, `topologySpreadConstraints`,
  `nodeSelector`, `tolerations`, `priorityClassName`, `podAnnotations`) and
  `networkPolicy.extraEgress`. New `values-cloud.yaml` overlay.
- Live test suites: `test-replication.sh`, `test-failover.sh`, `test-tls.sh`.

### Changed
- **BREAKING**: default `architecture` is `replication` (3 pods + Sentinel). Pin
  `architecture: standalone` to keep the previous single-instance behavior.
- **BREAKING**: `auth.enabled` and `networkPolicy.enabled` default `true`.
- PodDisruptionBudget default shape is now `maxUnavailable: 1` +
  `unhealthyPodEvictionPolicy: AlwaysAllow` (was `minAvailable: 1`), and it is now enabled
  by default in every architecture (set `redis.podDisruptionBudget.enabled: false` to opt out).
- Re-vendored the `common` subchart (adds `unhealthyPodEvictionPolicy` support).

### Migration to 1.0.0
- To keep the old single Redis instance:
  `--set architecture=standalone --set redis.auth.enabled=false --set networkPolicy.enabled=false`.
- To adopt HA, plan for 3 pods on 3 nodes (default hard anti-affinity) and update clients
  to be Sentinel-aware (connect to `<release>-redis-sentinel:26379`).

### Known limitations
- Full-cluster cold boot seeds `pod-0` as master; if it was not the last master, writes
  not yet replicated to it can be lost. Steady-state failover is bounded by
  `min-replicas-to-write` and Sentinel selecting the most up-to-date replica. A durable
  last-master marker is tracked as a follow-up.
