# pg chart changelog

## Unreleased

## 1.1.4 - 2026-06-19

Bundled-etcd security (#184) and a monitoring-exporter `/probe` fix (#185). Bundles
`etcd` 0.1.3; image moves to `trixie-5.5.0-21` (adds the `pg-ha-agent rbac-bootstrap`
subcommand). The exporter ConfigMap changes when `prometheusExporter.monitoringUser`
is enabled (the default); no other rendered behavior change at defaults.

### Added

- **Bundled etcd transport TLS (`etcd.tls.*`) and per-tenant RBAC (`etcd.rbac.*`),
  for the shared-etcd topology (#184).** etcd can now serve client + peer TLS from a
  BYO cert Secret with `--client-cert-auth` (mutual TLS), and a post-install/upgrade
  Job grants each tenant (matched by its client-cert Common Name) a role with
  `readwrite` only on its key prefix — so one release cannot read or rewrite another's
  leadership keys. A consuming release's agent authenticates by CN with no change (its
  existing etcd client mTLS); in bundled mode the parent auto-switches the endpoint to
  `https` and fails the render if the agent's client Secret is missing under
  client-cert-auth. The RBAC bootstrap runs via a new `pg-ha-agent rbac-bootstrap`
  subcommand (the bundled etcd image is distroless, so the Job uses the agent image +
  the Go etcd Auth API rather than a shell + etcdctl). All flag-gated
  (`etcd.tls.enabled`/`etcd.rbac.enabled` default off; render byte-stable). Covered by
  a new live KinD suite (`make -C pg test-agent-etcd-tls`, wired into CI) that proves
  the TLS handshake, CN auth, failover over mTLS, and that a tenant cert is denied
  outside its prefix.

### Fixed

- **The least-privilege monitoring user (#28) broke the multi-target `/probe`
  scrape — every per-target `pg_up` was 0.** The exporter `auth_modules` DSN (used
  by `/probe`, unlike `DATA_SOURCE_NAME`) carried no database, so libpq defaulted
  `dbname` to the username `monitoring` and connected to a non-existent `monitoring`
  database (`pq: database "monitoring" does not exist`). It worked under the old
  superuser only because `dbname` then defaulted to the always-present `postgres`.
  The probe DSN now pins `dbname` to the configured database (substituted from
  `POSTGRES_DATABASE`, the same source as `DATA_SOURCE_NAME` and the only database
  the monitoring role is granted `CONNECT` on).

## 1.1.3 - 2026-06-19

Multi-pillar-review remediation of the 1.1.2 etcd changes. Refreshes the bundled
`etcd` subchart to 0.1.2. Docs/test-quality only; no image change (stays
`trixie-5.5.0-20`) and no rendered behavior change at defaults.

### Fixed

- **Standalone-etcd guide pointed at a Service that never exists.** The etcd
  README's shared-store wiring used `http://<release>-etcd.<ns>` but the client
  Service renders as `<release>-etcd-etcd`; the install now sets
  `fullnameOverride` so the documented endpoint matches (#183 follow-up).
- **etcd `image.tag` pin comment was stale** (claimed it matched the agent's
  vendored client, which moved to v3.5.31); corrected to note the server is held
  at v3.5.16 and 3.5.x client/server are wire-compatible.

### Security

- **Hardened the `etcd.networkPolicy.allowedClients` guidance.** The default
  podSelector matches any `app.kubernetes.io/component: postgresql` pod in an
  allow-listed namespace, and the bundled etcd is plaintext/no-auth — documented
  this exposure in `values.yaml` and recommend an instance-pinned selector plus
  client TLS (#184).

### Changed

- Release landing page no longer advertises the `common` library chart as
  installable; `helm repo add` alias unified to `cagriekin` across the READMEs.
- Test coverage for `allowedClients` tightened: the default-podSelector assertion
  is now scoped (was doc-wide/tautological), and the custom-podSelector branch,
  multi-entry rendering, and the missing-namespace fail-fast message are asserted.

## 1.1.2 - 2026-06-19

Refreshes the bundled `etcd` subchart to 0.1.1. Chart-only; no image change
(stays `trixie-5.5.0-20`) and no behavior change at defaults -- a `helm upgrade`
rolls nothing unless `etcd.enabled` and the new value is set.

### Added

- **`etcd.networkPolicy.allowedClients` for the bundled etcd (#183).** The etcd
  NetworkPolicy only admitted this release's own postgresql pods on the client
  port, so a shared/standalone etcd had no first-class way to allow clients in
  other namespaces (only raw `extraIngress` with hand-written selectors). Added a
  declarative `allowedClients: [{namespace, podSelector?}]` knob that opens the
  client port (2379) per namespace; `podSelector` defaults to the agent's
  `app.kubernetes.io/component: postgresql` label. Default `[]` (render
  byte-stable). The `etcd` chart is now also published standalone (0.1.1) so
  several `pg` releases can share one etcd; see its README.

## 1.1.1 - 2026-06-19

Security: bump the HA agent's vendored Go dependencies off two CVE-flagged
versions. Image moves to `trixie-5.5.0-20`. No chart-template or values changes
beyond the image tag; a `helm upgrade` rolls the pods once.

### Security

- **Bumped `google.golang.org/grpc` 1.59.0 -> 1.79.3 (CVE-2026-33186, critical)
  and `golang.org/x/oauth2` 0.21.0 -> 0.34.0 (CVE-2025-22868, high)** in the
  `pg-ha-agent` module. Both were transitive (grpc via `go.etcd.io/etcd/client/v3`,
  oauth2 via `k8s.io/client-go`); the etcd client is bumped 3.5.16 -> 3.5.31 within
  the stable 3.5 line so the grpc jump stays source-compatible, and oauth2 floats to
  3.5.31/grpc's required 0.34.0 (>= the 0.27.0 fix). `govulncheck` reported both as
  unreachable (no vulnerable symbol is called from the agent), so there was no
  exploit path in the running binary -- the bump clears the advisories and keeps the
  supply chain current. Re-vendored; hermetic build, `go mod verify`, `go vet`, unit
  tests, and `govulncheck` (now zero) all pass.

## 1.1.0 - 2026-06-19

### Added

- **WAL-archiving health metrics for the Prometheus exporter (#30).** When
  pgBackRest is enabled the exporter now serves a `pg_wal_archive` query group from
  `pg_stat_archiver` (scraped on the primary): `pg_wal_archive_failed_count`
  (archive_command failures), `pg_wal_archive_archived_count`,
  `pg_wal_archive_seconds_since_last_archived`, and
  `pg_wal_archive_seconds_since_last_failed`. Previously a stalled `archive_command`
  surfaced no metric and fired no alert. The query reads only `pg_stat_archiver`, so
  it needs no filesystem/superuser access (compatible with a future read-only
  monitoring user, #28).
- **Opt-in automated backup-validation CronJob (#31).** A new weekly CronJob
  (`backup.validation.enabled`, default off) downloads the latest `pg_dump` backup
  and restores it into a throwaway PostgreSQL inside the Job pod -- never the live
  database -- failing the Job (so it alerts) if `pg_restore --exit-on-error` trips (a
  restored database with no table-like relations is only a warning, since a
  schema/extension-only database restores cleanly). Nothing previously verified that backups were
  actually restorable beyond a TOC `pg_restore --list` check. Configurable
  `schedule`, `resources`, and `workdirSizeLimit` (the throwaway PGDATA emptyDir
  cap, default unbounded). It reuses the postgresql securityContext (runs
  initdb/postgres as the postgres uid) and the release-scoped S3 path.

### Security

- **The Prometheus exporter connects as a least-privilege `pg_monitor` user, not the
  postgres superuser (#28).** A post-install/post-upgrade hook Job creates a read-only
  monitoring role on the primary (it replicates to standbys) and the exporter
  authenticates as it. Chart-only -- works in both repmgr and standalone modes with no
  image change. Enabled by default (`prometheusExporter.monitoringUser.enabled`);
  disable to revert to the superuser. **Migration (existingSecret users):** because
  this is on by default, before upgrading add a `monitoring-password` key to your
  existing secret (key name overridable via
  `postgresql.existingSecret.monitoringPasswordKey`), or set
  `prometheusExporter.monitoringUser.enabled=false`. The chart references that key name
  (validated at render); a key missing from the secret itself surfaces at runtime as
  the exporter + hook Job failing to authenticate, not as a render error.
- **The backup and backup-validation Jobs run under a dedicated ServiceAccount, not
  the namespace default (#27).** A new no-RBAC `<fullname>-backup` ServiceAccount
  (its token is never mounted, #166) backs both Jobs, which talk only to PostgreSQL
  and S3. Previously they ran as the namespace default SA.
- **pgBackRest repository encryption is now an option (#120).** `repo1-cipher-type`
  was hardcoded to `none`. Set `pgbackrest.repoEncryption.enabled=true` (cipher
  `aes-256-cbc` by default) to encrypt the repository at rest in S3; the passphrase is
  read from `pgbackrest.repoEncryption.existingSecret` and supplied to pgbackrest via
  the `PGBACKREST_REPO1_CIPHER_PASS` env (postgresql container + sidecar), never
  written into the ConfigMap. Default stays unencrypted. A PITR restore pod must set
  the same env when restoring an encrypted repository.
- **All container images are digest-pinnable (#26).** Every image -- postgres, repmgr,
  pgpool, pgpool-exporter, prometheus-exporter, busybox, `mc`, and the pgBackRest
  CronJob runner -- now takes an optional `image.digest` (the repmgr image already
  did). When set it renders `repository:tag@digest`, so a mutable-tag repush cannot
  silently change the deployed image. Empty by default (pull by tag; render is
  byte-stable). Routed through a shared `pg.image` template helper.
- **`readOnlyRootFilesystem` on the auxiliary containers (#117).** The Prometheus
  exporter, the backup and backup-validation Jobs, and the pgBackRest CronJob runner
  now run with a read-only root filesystem, each paired with a writable `/tmp`
  emptyDir (and `HOME=/tmp` for the runner's kubectl cache). The service-updater and
  the pgpool metrics exporter share the postgresql/pgpool securityContext (whose main
  containers need a writable root), so hardening those needs a dedicated context --
  left as a follow-up.
- **The PgPool-II PCP admin port (9898) is no longer exposed on the Service by default
  (#118).** The pgpool Service published the PCP admin/control port cluster-wide, while
  the pgpool NetworkPolicy only admits 9999 — so with NetworkPolicy off the admin
  endpoint was reachable by any pod, and with it on the Service port was dead. It is now
  gated behind `pgpool.service.exposePcp` (default `false`). Enable it only if you run
  `pcp_*` commands against the Service, and pair it with a `pgpool.extraIngress` rule for
  9898 when NetworkPolicy is enabled.
- **The `fix-permissions` init container drops its excess capabilities (#162).** The
  chown init container legitimately needs root, but it inherited the runtime's full
  default capability set (SETUID, SETGID, NET_RAW, …). It now drops ALL and adds back
  only `CHOWN`, `DAC_OVERRIDE`, `FOWNER` (what `chown -R` / `chmod` need), matching the
  tighter pattern the chart's other root init container already uses.
- **The pgBackRest backup CronJob is now security-hardened (#155).** It was the one
  pod with zero hardening — running as the image default (root), full capability set,
  no seccomp profile, `allowPrivilegeEscalation` unset — yet it carries the
  exec-capable pgBackRest ServiceAccount token, and it failed admission in
  Pod-Security-`restricted` namespaces while the rest of the release deployed. It now
  applies `pgbackrest.cronjob.podSecurityContext` / `containerSecurityContext`
  (defaults: `runAsNonRoot`, `runAsUser: 65534`, `seccompProfile: RuntimeDefault`,
  `allowPrivilegeEscalation: false`, drop ALL capabilities), matching the chart's other
  pods. `alpine/k8s` runs `kubectl` fine as a non-root uid.
- **Pods that make no Kubernetes API calls no longer mount a ServiceAccount token
  (#166).** The pgpool Deployment, the prometheus-exporter Deployment, the backup
  CronJob, and the StatefulSet in standalone (`repmgr.enabled=false`) mode ran as the
  namespace default ServiceAccount with its token projected in — an unnecessary,
  valid API credential in pods that also hold the postgres superuser password. They
  now set `automountServiceAccountToken: false`. The repmgr StatefulSet keeps its
  token (the agent / service-updater genuinely call the API).
- **S3 credentials no longer passed on the `mc` command line (#167).** The backup
  job ran `mc alias set s3 <endpoint> <access-key> <secret-key>`, exposing both keys
  in the process argv (`/proc/<pid>/cmdline`, readable via `ps` by other users on the
  node, hostPID pods, and command-line-logging agents) on every scheduled run.
  Credentials are now supplied to `mc` via the `MC_HOST_s3` environment variable
  (percent-encoded so reserved characters in real keys survive), so they never appear
  in argv. Requires `backup.s3.endpoint` to include a scheme (`http://`/`https://`),
  which `mc` already required.

### Documentation

- **Corrected the false "clean deregistration" claim and documented scale-down ghost
  nodes (#139).** The README claimed the repmgrd preStop hook (`repmgr daemon stop`)
  performed "clean deregistration" — it does not; it only stops the daemon. Scaling
  `postgresql.replicaCount` down therefore leaves the removed nodes in `repmgr.nodes`
  as `active` ghosts (`repmgr cluster show` shows them failed; in repmgrd mode the
  survivors keep retrying the gone DNS names, adding failover-election delay). The
  README now describes the preStop hook accurately and adds a **Scaling down** section
  with the manual `repmgr standby unregister --node-id=<ordinal+1000>` cleanup.
  (Automatic deregistration is not yet implemented — tracked in #139.)
- **Clarified that `networkPolicy.postgresql.allowExternal=false` blocks the read-only
  Service (#148).** `allowExternal` gates direct client access to PostgreSQL on 5432,
  which is exactly the path the `<fullname>-readonly` Service (direct standby reads)
  uses — so with `allowExternal: false` those read connections silently time out while
  `kubectl get endpoints` looks healthy (PGPool on 9999 stays reachable, so read-write
  clients via PGPool are unaffected). Documented the interaction in the README and
  `values.yaml`, with a scoped `extraIngress` recipe to re-allow direct-5432 clients. No
  default behavior change.
- **The pgBackRest PITR restore runbook could not work as written (#149).** The
  documented restore pod mounted only the data PVC and set the S3 key env vars, but
  not the `<fullname>-pgbackrest` ConfigMap — which is the only place `pg1-path` and
  the `repo1-*` S3 settings live. `pgbackrest restore` therefore failed with
  `requires option: pg1-path` (and, once that was worked around, defaulted to a local
  posix repo, never finding the S3 backup). The runbook now mounts the ConfigMap at
  `/etc/pgbackrest/pgbackrest.conf`, sources the keys from the existing pgBackRest
  secret, sets the chart's `securityContext` (101:103) so restored files are owned
  correctly and the pod passes restricted PodSecurity/OpenShift, adds the required
  `--type=time` to the `restore --target` command, corrects the `keyType: auto`
  guidance (bind the restore pod to the `<fullname>-repmgr` SA, not the default), and
  uses the current image tag.
- **Corrected the pgBackRest troubleshooting pointer (#151).** The troubleshooting table
  told operators to check `pgbackrest-scheduler` logs — a container that does not exist
  (backups migrated from an in-pod scheduler sidecar to CronJobs). It now points at the
  `pgbackrest` sidecar on the primary and the `<fullname>-pgbackrest-full`/`-diff` CronJob
  pod logs.
- **Synced the documented `repmgr.image.tag` default with `values.yaml`.** The parameter
  table listed `trixie-5.5.0-16`; the chart ships `trixie-5.5.0-18`.

### Fixed

- **repmgr SCRAM startup deadlock eliminated (image `trixie-5.5.0-19`).** `initdb
  --auth-host=md5` makes the image write `password_encryption=md5`, so the bootstrap
  created the `repmgr`/`postgres` users with MD5 secrets -- but `pg_hba.conf` requires
  `scram-sha-256` for the `10.0.0.0/8` pod network. When a standby's `repmgr-init`
  clone or `repmgrd` connected over that network before the chart's postStart
  md5->scram migration had run, PostgreSQL rejected it with "does not have a valid
  SCRAM secret", crash-looping repmgrd / wedging the standby clone until
  `helm install --wait` timed out -- an intermittent CI/install failure. The image now
  creates the managed users with a SCRAM secret directly (the same end state the
  migration drives them to), so replication auth works from first boot regardless of
  migration timing. The global default stays `md5` for legacy/app users; the migration
  and the md5-above-scram `pg_hba` patch remain as a safety net.
- **Helper init images are now a single configurable value (#116).** The four
  busybox init containers (the StatefulSet `fix-permissions`/`setup-config`, and the
  pgpool and exporter config inits) hardcoded the busybox image -- inconsistently
  (`1.35` vs `1.37`) and with no override, blocking air-gapped/private-registry
  deployments. They now share a `busyboxImage` value (`repository`/`tag`/`pullPolicy`,
  default `busybox:1.37`). The pgBackRest CronJob (`pgbackrest.cronjob.image`) and the
  backup `mc` image (`backup.mc.image`) were already configurable.
- **Primary lookup uses EndpointSlice instead of the deprecated Endpoints API
  (#121).** The pgBackRest backup CronJob resolved the current primary with
  `kubectl get endpoints`; the core Endpoints API is deprecated in favor of
  EndpointSlice. The CronJob now lists the write Service's EndpointSlices
  (`discovery.k8s.io`, filtered to the Ready endpoint) and the Role grants
  `endpointslices` instead of `endpoints`. EndpointSlice names are auto-generated,
  so this is a namespace-scoped read of EndpointSlice metadata (list) rather than a
  resourceName-scoped get; the security-critical pods/exec scoping (#134) is
  unchanged.
- **Removed the dead `REPMGR_NODE_COUNT` env from the service-updater container
  (#177).** The service-updater script derives its peer-scan range from
  `replicaCount` at template time and never read the env, so it was dead config on
  that container. It stays on the `repmgr-init`/`postgresql`/`repmgrd` containers,
  which the image scripts do consume. (The image-side de-duplication of the
  triplicated peer-scan/timeline logic noted in #177 is a separate follow-up.)
- **Backup integrity check no longer buffers the whole dump to `/tmp` (#119).** The
  verify step did `mc cat > /tmp/verify_backup.dump` before `pg_restore --list`, writing
  the entire dump to the container's unbounded, unsized writable layer — a large
  database could hit node-disk eviction. It now streams the archive
  (`mc cat … | pg_restore --list`), so the TOC is checked without staging the dump
  locally.
- **Init containers now declare resource requests/limits (#153).** No init container
  set resources, so in a namespace with a `ResourceQuota` requiring requests/limits
  every pod of the chart was rejected at admission (Forbidden) unless a `LimitRange`
  happened to inject defaults — and the `repmgr-init` standby clone (a full
  `pg_basebackup`) ran with unbounded CPU/memory/IO. The lightweight inits (chown, cp,
  config-gen across the StatefulSet, pgpool/exporter Deployments, and backup CronJob)
  now use a small shared default; `repmgr-init` uses an overridable
  `repmgr.initContainerResources` (heavier, sized for the clone).
- **emptyDir volumes are now size-capped (#165).** None of the chart's emptyDirs set a
  `sizeLimit`, so a runaway volume — especially PGDATA when `persistence.enabled=false`
  — could fill the node's root filesystem and evict unrelated pods instead of being
  capped and evicted itself. Fixed caps are set on the config/tool/extension volumes
  (16Mi config, 128Mi backup tools, 1Gi extension trees), and the non-persistent data
  volume gets a configurable `postgresql.persistence.emptyDir.sizeLimit` (default empty
  = unbounded, preserving prior behavior; set e.g. `8Gi` for ephemeral use).
- **pgBackRest `stanza-create` no longer masks real failures (#160).** The pgBackRest
  backup CronJob ran `stanza-create || true`, which swallowed not just the benign
  "stanza already exists" case (`stanza-create` is natively idempotent and exits 0 then)
  but also genuine failures — S3 permission errors, a repo lock, a `kubectl exec`
  transport error, or a needed `stanza-upgrade` after a PG major upgrade — so the job
  proceeded to the backup step and failed there with a misleading downstream error
  instead of the root cause. Dropped `|| true`; under `set -eu -o pipefail` a real
  `stanza-create` failure now aborts the job at the right step with the actual message.
- **The postgres-exporter NetworkPolicy now has a cross-namespace scrape escape hatch
  (#147).** The exporter's 9116 metrics ingress admitted same-namespace pods only
  (`podSelector: {}`), and — unlike the postgresql/pgpool policies — had no `extraIngress`
  value, so a Prometheus in a separate monitoring namespace (the usual `ServiceMonitor`
  topology the chart ships) could not scrape it and there was no chart-supported fix
  short of disabling NetworkPolicy. Added `networkPolicy.prometheusExporter.extraIngress`
  / `extraEgress` (mirroring postgresql/pgpool) so a `namespaceSelector` rule can allow
  the scraper; documented the cross-namespace limitation (exporter 9116, pgpool 9719,
  agent 9200) in the README. No default behavior change.
- **postgres-exporter probes now detect a broken scrape pipeline (#146).** Both probes
  hit the landing page `/`, which returns 200 unconditionally — so a `queries.yaml` /
  collector regression that makes every scrape fail with HTTP 500 (as the chart's own
  0.5.73–0.5.81 regression did for nine releases) left the exporter pods Ready and never
  restarted while all DB metrics went dark. The liveness and readiness probes now hit
  `/metrics` (matching the pgpool exporter), which returns 500 on genuine exporter/
  registry breakage but 200 + `pg_up 0` on a database outage — so the probe catches
  config breakage without flapping when the DB is merely down.
- **pgpool PDB no longer wedges node drains on a single-replica install (#161).** The
  pgpool PodDisruptionBudget used `minAvailable: 1` with the default `pgpool.replicaCount:
  1`, so allowed disruptions were permanently 0 — `kubectl drain`, managed node upgrades,
  and autoscaler scale-down hung indefinitely on the pgpool node. It now uses
  `maxUnavailable: 1` + `unhealthyPodEvictionPolicy: AlwaysAllow` (mirroring the
  postgresql PDB): a single-replica pgpool can be evicted (it is stateless and simply
  reschedules), while a multi-replica pgpool keeps rolling protection (at most one pod
  down at a time). The shared `common.podDisruptionBudget` helper now renders exactly
  one of `minAvailable`/`maxUnavailable` (an explicit `minAvailable` wins, else
  `maxUnavailable`), so a partial override can no longer emit both — which the API
  rejects.
- **Numeric/boolean-looking env values no longer fail at apply (#156).** Several
  container env values (`REPMGR_USER`, `REPMGR_DB`, `PGBACKREST_STANZA`, `STANZA`,
  `SPLIT_BRAIN_ACTION`, the Service/marker/Lease names, FQDNs) were interpolated into
  `value:` without `| quote`. A value that YAML types as a number or bool (e.g.
  `repmgr.database=12345`, `pgbackrest.stanza=123`) rendered as a bare scalar that the
  API server rejects (`cannot unmarshal number into field of type string`) — passing
  `helm template`/`lint` but failing at apply. All
  user-facing env values are now `| quote`d (composite names via `printf … | quote`),
  matching the already-quoted `MONITORING_HISTORY_DAYS`/S3 envs.
- **Single quotes in `postgresql.configuration` / `pgpool.resetQueryList` no longer
  produce an invalid config (#157).** Both were interpolated naively into single-quoted
  conf lines, so a value containing a `'` (e.g. in `log_line_prefix` or
  `archive_command`) rendered a syntactically-invalid `custom.conf` — putting the
  postgres pods into CrashLoopBackOff after the config-checksum roll — or a broken
  `reset_query_list` that stopped pgpool from starting. Embedded single quotes are now
  doubled (`''`, the PostgreSQL/pgpool conf-lexer escape) in `postgresql.configuration`
  values, `pgpool.resetQueryList`, and the `archive_command` stanza. Values without
  quotes render unchanged. (Newline-bearing conf values remain unsupported — they fail
  the render rather than injecting a directive.)
- **Long release/fullname now fails fast instead of rendering invalid resource names
  (#158).** `pg.fullname` is capped at 63 but per-resource suffixes are appended after
  it, so a long `fullnameOverride` could render a Service name over 63 chars (rejected
  by the API server) or a CronJob name over ~52 chars (silently fails to spawn Jobs) —
  with no render-time hint. The chart now validates composed Service (≤63),
  Deployment-backed (≤47, for pgpool/exporter Pod names) and CronJob (≤52) names at
  render time and fails with a clear message naming the offending name and the current
  `pg.fullname` length. Truncation was rejected as unsafe on a stateful
  chart (two long names could collide on one StatefulSet/PVC). Normal names are
  unaffected (the guard is a no-op).
- **A failed `pg_dump` left a truncated dump masquerading as the newest backup
  (#159).** If `pg_dump` exited non-zero mid-stream (connection drop during failover,
  OOM), `mc pipe` finalized the truncated object at the canonical
  `backup_<ts>.dump` name and it remained the lexically-newest backup until the next
  successful run (~24h) — so an operator restoring "the latest" during an incident
  could pick a corrupt dump. The dump is now streamed to a `.tmp` staging object and
  published to the canonical name with `mc mv` only after the `pg_restore --list`
  integrity check passes, so a truncated dump never reaches the canonical name. An
  EXIT trap removes the staging object on failure, and retention also sweeps stale
  `.tmp` objects orphaned by a hard-killed run.
- **pgBackRest config changes (S3 endpoint/bucket/retention) didn't roll the pods
  (#145).** `pgbackrest.conf` is a subPath mount — which the kubelet never
  live-updates — and the StatefulSet pod template did not checksum the pgBackRest
  ConfigMap (only `postgresql-configmap`). So after a `helm upgrade` that repointed
  the repository, every running pod's `archive_command` and the pgbackrest sidecar
  kept writing to the OLD location until manually restarted — backups looked green
  while landing in the wrong place, discovered only at restore time. The pod template
  now carries a `checksum/pgbackrest-config` annotation, so any pgBackRest config
  change rolls the StatefulSet (one pod at a time) and the new config takes effect.
  Operator note: changing `pgbackrest.s3.*`/`pgbackrest.retention.*` now restarts the
  pods (previously a no-op); rolling the current primary triggers a controlled
  failover, the same as any rolling upgrade.
- **Backup retention could delete another release's dumps under a shared
  bucket/prefix (#143).** `pg_dump` backups were written to a flat
  `<bucket>/<prefix>/backup_<ts>.dump` with no release identity, and the retention
  `mc find ... --older-than --exec mc rm` ran recursively over the whole prefix with
  no name filter — so two releases (e.g. staging + prod) sharing one bucket/prefix
  each deleted the other's dumps older than their own `retentionDays`. Dumps are now
  namespaced per release under `<prefix>/<release-fullname>/` (mirroring the
  pgBackRest repo layout), and both the recent-backup guard and the retention delete
  are scoped to that subpath with a `--name 'backup_*.dump'` filter (so unrelated
  objects under the prefix are never touched either). Existing dumps under the old
  flat path are left in place (not migrated, not deleted); see the README restore
  section for the new path layout.

## 1.0.2

Bugfix for agent mode (the 1.0.0 default). Image moves to `trixie-5.5.0-18`. No
chart-template or values changes beyond the image tag; a `helm upgrade` rolls the
pods once.

### Fixed

- **Agent re-ran `repmgr standby follow` every reconcile tick on a healthy,
  already-streaming standby and logged an ERROR each time (#182).** The `Follow`
  executor latched its idempotency guard (`followUpstream`) only after
  `repmgr standby follow` returned success. On a standby that was already correctly
  streaming from the lease holder -- which is the steady state right after a
  repmgrd->agent migration (`primary_conninfo` persists across the roll) or a
  post-failover rejoin -- the command exits non-zero (`slot "..." already exists as
  an active slot` / `this server is not ahead`), so the guard never latched and the
  agent re-forked the failing command every ~5s. Replication was unaffected, but the
  ERROR spam (~1 every tick per standby) buried genuine errors and tripped log-based
  alerting. The agent now (1) skips `repmgr standby follow` entirely when it observes
  via `pg_stat_wal_receiver` that the standby is already streaming from the target,
  and (2) treats the benign "already following" repmgr exit as a successful no-op, so
  the guard latches and the command is not re-run. Repointing to a genuinely new
  upstream (after a leader change) still runs `follow`. Regression coverage: pg
  probe, mechanism, and act-path unit tests.

## 1.0.1

Bugfix for agent mode (the 1.0.0 default). Image moves to `trixie-5.5.0-17`.

### Fixed

- **Agent standby never re-established streaming after a failover / repmgrd->agent
  migration (#181).** The agent decided "running vs stopped" purely from SQL
  reachability and, when SQL was unreachable, read the role from `pg_controldata`.
  A freshly-cloned standby still rejecting connections (`the database system is
  starting up`) was therefore misclassified: right after `pg_basebackup` the control
  file still carries the source primary's `in production` state, so the agent saw a
  "stopped primary" and issued `RejoinForward`, which terminated the standby's
  walreceiver mid-stream; it then looped `StartLocal` on the recovering node. The
  standby never reached consistency and the cluster was left single-node.
  The agent now tracks **process liveness** (`Supervisor.Running()`) separately from
  SQL readiness: while its own postmaster is alive but not yet accepting connections
  (and not self-health-stuck), it **waits** for the node to reach a ready state
  instead of acting on the transient on-disk role. Self-health failover of a
  genuinely frozen primary is unaffected. Regression coverage: reconcile
  decision-table cases, plus `test-agent-failover` / `test-migrate-agent` now assert
  the rejoined standby is actively streaming (`pg_stat_replication`).



First major release. The lease-based Go agent (`pg-ha-agent`) is now the
**default** failover mode, and the `pg` and `pgvector` charts move to a single,
aligned 1.0.0 version line. The repmgr image is `trixie-5.5.0-16`, which bundles
the agent binary.

### BREAKING

- **`repmgr.failoverMode` defaults to `agent`** (was `repmgrd`). New installs run
  the lease-based agent. The legacy repmgrd + service-updater path remains
  available for one major cycle via `repmgr.failoverMode: repmgrd` (deprecated).
- Agent mode uses `podManagementPolicy: Parallel` (repmgrd uses `OrderedReady`),
  an **immutable** StatefulSet field, so switching an existing repmgrd release to
  agent mode requires a one-time `--cascade=orphan` StatefulSet recreate.
- Agent mode assembles a hardened `pg_hba.conf` (pod-CIDR + SCRAM, no implicit
  `0.0.0.0/0 md5`). Consumers who relied on the broad md5 rule must add explicit
  `postgresql.pgHba` rules before switching.
- The postgresql PodDisruptionBudget default is `maxUnavailable: 1` +
  `unhealthyPodEvictionPolicy: AlwaysAllow` (was `minAvailable: 1`); equivalent on
  a 2-pod cluster, strictly better for drains/upgrades (k8s >= 1.27).

### Migrating to 1.0.0

- **Stay on repmgrd (no behavior change):** set `repmgr.failoverMode: repmgrd` and
  `helm upgrade`. Pods roll once for image `trixie-5.5.0-16`; nothing else changes.
- **Adopt agent mode (default):** with a fresh backup and a healthy cluster, and
  ArgoCD auto-sync paused if used:
  1. `kubectl delete statefulset <release>-pg --cascade=orphan -n <ns>` (keeps pods
     + PVCs running; Helm re-adopts them).
  2. `helm upgrade <release> ... ` (recreates the StatefulSet as `Parallel`, adopts
     the orphaned pods, rolls them into agent mode). The migration guard stops a
     first-rolled standby from becoming a second writer.
  3. Verify: `kubectl get lease <release>-pg-leader` holder == the primary;
     `kubectl get endpoints <release>-pg` points at it; write a row; in staging
     trigger a failover and confirm the Lease moves and data survives.
  - Rollback is symmetric (flip back to `repmgrd` with the same `--cascade=orphan`
    recreate, then optionally `kubectl delete lease <release>-pg-leader`).
  - Full runbook (GitOps caveats, DR/PITR, pg_upgrade, the etcd DCS backend) is in
    the README.

The sections below describe the agent machinery introduced across the 0.5.x line
and now shipping as the 1.0.0 default; the repmgrd rendering is byte-stable.

### Added

- `repmgr.failoverMode: agent` runs a Go agent (`pg-ha-agent`) as PID 1 in the
  postgresql container. The agent holds a Kubernetes `coordination.k8s.io/v1`
  Lease (`<release>-pg-leader`) as the sole authority for which node is primary
  and drives repmgr as a pure mechanism (no repmgrd). This removes the
  hand-rolled split-brain handling and the repmgrd startup race at the source.
- Agent-mode wiring (gated, repmgrd path unchanged): `podManagementPolicy:
  Parallel`, the entrypoint `agent` arm, agent env (`LEASE_NAME`,
  `LEASE_DURATION`, `RENEW_DEADLINE`, `RETRY_PERIOD`, `RECONCILE_INTERVAL`,
  `POD_NAME`, `MASTER_SERVICE`, `POD_SELECTOR`, `DCS_BACKEND`,
  `SPLIT_BRAIN_ACTION`), a `9200` metrics/health port with an agent-heartbeat
  liveness probe, `coordination.k8s.io/leases` RBAC scoped to the leader Lease,
  pgpool backends fronting the RW/RO Services, and a `9200` NetworkPolicy
  ingress. The repmgrd sidecar, service-updater sidecar, and the service-updater
  ConfigMap are omitted in agent mode.
- `repmgr.agent.*` tunables (`leaseDuration`, `renewDeadline`, `retryPeriod`,
  `reconcileInterval`, `podCidr`).
- Agent operability: **cluster-identity safety** — a clone/follow/rewind from a
  peer of a different cluster is refused by comparing the PostgreSQL
  `system_identifier` (guards against a stale/misrouted/DR-restored peer);
  **maintenance mode** — `kubectl annotate configmap <release>-pg-primary
  pg-ha/pause=true` suspends automatic promote/demote/fence/self-health while the
  agent keeps serving; **controlled switchover** — `pg-ha/switchover-target=<pod>`
  hands the primary role to a caught-up, same-timeline standby; and on-DCS data
  (the marker + gossip) carries a `schemaVersion` so a mixed-version rolling agent
  upgrade is safe.
- Agent monitoring (opt-in, agent mode): `repmgr.agent.monitoring.serviceMonitor`
  and `repmgr.agent.monitoring.prometheusRule` ship a ServiceMonitor (scraping the
  agent's read-only metrics on `9200`) and example alert rules — no-leader,
  multiple-leaders (split-brain), agent-down, lease-renew-failing,
  reconcile-errors, flapping, and paused-too-long. Replication-lag alerting stays
  with the PostgreSQL exporter.
- etcd leadership backend (opt-in, agent mode): `repmgr.agent.dcs.backend: etcd`
  holds the leader lock in etcd instead of a Kubernetes Lease, so a control-plane
  outage no longer self-demotes the primary (only an etcd quorum loss does).
  Provide a **BYO/shared** etcd via `repmgr.agent.dcs.etcd.endpoints` (+ optional
  mutual TLS via `dcs.etcd.tls.secretName`), or set `etcd.enabled=true` to deploy a
  **bundled** 3-node etcd subchart that the agent auto-targets. In etcd mode the
  `coordination.k8s.io/leases` RBAC grant is dropped and egress to etcd `:2379` is
  opened. Default stays `kubernetes`.

### Notes

- Agent mode is opt-in and stays off by default — repmgrd installs are unchanged
  and need no action. It becomes the default at chart `1.0.0`. Graceful failover
  in agent mode is covered by the live KinD suite (`make -C pg test-agent`).
- **Migrating to agent mode:** `podManagementPolicy` is immutable, so switching an
  existing release needs a one-time `kubectl delete statefulset <release>-pg
  --cascade=orphan` (keeps pods + PVCs) then `helm upgrade --set
  repmgr.failoverMode=agent`. Full runbook + GitOps caveats in the README
  ("Failover modes" / "Migrating an existing release to agent mode"); injected env
  is catalogued in `ENVIRONMENT.md`.
- The new PostgreSQL settings (`wal_log_hints`, `max_replication_slots`,
  `max_slot_wal_keep_size`, `restore_command`) are applied at `initdb`, so they
  take effect on freshly-provisioned clusters only. An existing cluster keeps its
  current settings on upgrade; to get `wal_log_hints=on` (so `pg_rewind` works
  without data checksums) and the bounded slot cap, set them via
  `postgresql.configuration` or apply them manually and restart.

## 0.5.88

Bundles the stale-primary/HA hardening, operational fixes, fail-fast
validation, and RBAC scoping accumulated since 0.5.87. The repmgr image is
now `trixie-5.5.0-15`.

### Fixed

- WAL-filename timeline is decoded as hexadecimal, not a decimal `::int`
  cast that errored at timeline `0x0A` and was wrong from `0x10` — this had
  silently broken the whole stale-primary family past ~10 failovers (#168).
- Split-brain handling in the default `log` mode re-asserts the write
  selector toward the highest-timeline primary every tick, restoring the
  ArgoCD self-heal during a split-brain window (#169).
- A failed `pg_rewind` rejoin preserves the diverged data (moved aside)
  before re-cloning, instead of wiping PGDATA ahead of the clone (#175).
- The empty-data stale-primary guard only settles/retries the peer scan on a
  PVC-loss recreate (gated on the durable primary marker), adding no latency
  on a genuine first install; the settle breaks only on a confirmed active
  primary and the marker lookup is time-bounded (#170).
- The lone-primary marker guard fails closed: an equal-timeline different-node
  split-brain is refused rather than overwriting the marker (#171); an
  unreadable timeline holds the current selector even before the marker
  exists (#173); a corrupt non-numeric marker timeline is treated as an error,
  not as "no marker" (#174).
- Readonly-Service pods are labeled `pg-role=standby` only when actually in
  recovery; a reachable non-master that is not in recovery (a stale/divergent
  primary) is labeled `orphan` and kept out of read traffic (#140).
- `postStart` `additionalCommands` reach the discovered primary in repmgr mode
  (`PGPASSWORD` exported for the cross-pod connection; `PGHOST` exported as its
  own statement), fixing automatic extension creation / DDL that was a silent
  no-op (#127).
- Disabling pgbackrest after install neutralizes the persisted `archive_mode` /
  `archive_command`, preventing a permanently failing `archive_command` from
  blocking WAL recycling and filling the data PVC (#132).
- The service-updater seeds `LAST_MASTER` from the live write-Service selector,
  so it no longer rollout-restarts PGPool (severing pooled connections) on
  every install/upgrade/sidecar restart with no actual failover (#138).
- repmgr-mode `postgresql.pgHba` entries are inserted above the image's network
  catch-all rules (first-match-wins), not appended at EOF where they were
  unreachable (#144).
- The PGPool NetworkPolicy opens the metrics scrape port 9719 when
  `pgpool.metrics.enabled`, and `networkPolicy.*.extraIngress` entries are full
  ingress rules so they can open ports other than 5432 (#135, #136).
- `postgresql` egress to PGPool's backend port 9999 is allowed so the
  service-updater health check no longer perpetually rollout-restarts PGPool
  (#129).

### Added

- A `startupProbe` on the PostgreSQL container suspends liveness/readiness until
  PostgreSQL first accepts connections, so the stale-primary guard settle and
  crash-recovery WAL replay cannot be killed mid-startup into a CrashLoopBackOff
  (`postgresql.startupProbe.*`, #172, #141).
- `repmgr.image.majorVersion` declares the PostgreSQL major bundled in the
  repmgr image; a `postgresql.majorVersion` mismatch now fails at render in
  repmgr mode rather than crash-looping or silently running the wrong major
  (#133).

### Security

- The repmgr Role's `pods` `get`/`patch` are scoped to the StatefulSet pod
  names and `delete` is granted only in fence mode (`list` stays unscoped), so a
  leaked ServiceAccount token cannot manipulate arbitrary namespace pods (#154).
- The pgbackrest Role's `pods`/`pods/exec` are scoped to the StatefulSet pod
  names instead of namespace-wide (#134).
- `global.annotations` render under `metadata.annotations`, not `labels`, so
  non-label-safe values no longer break apply and reach annotation consumers
  (#128).

### Fail-fast validation

- `postgresql.existingSecret.enabled=true` with an empty name fails at render
  instead of producing an empty `secretKeyRef.name` (#137).
- `pgbackrest.enabled` with `repmgr.enabled=false` fails at render: the
  pgbackrest binary and `archive_command` run in the postgresql container, which
  is the plain postgres image in standalone mode (#142).

## Migrating from 0.5.87

`helm upgrade my-release cagriekin/pg` with image
`cagriekin/repmgr:trixie-5.5.0-15` is the migration; PostgreSQL pods roll once
for the new image tag, the new `startupProbe`, and the RBAC scoping. Note that
the new fail-fast guards (#133, #137, #142) reject previously-accepted but
broken configurations at render time — if an upgrade now fails to template,
the error message names the offending value.

## 0.5.87

### Fixed

- repmgr image bumped to `trixie-5.5.0-10`: the primary node is now
  registered with a retry loop (matching the standby path) and the
  role probe retries until definitive. Previously `repmgrd-entrypoint`
  ran a single `repmgr primary register` under `set -e`; on a slow or
  contended host that register could race the postgresql container's
  init SQL (`CREATE EXTENSION repmgr`, repmgr user) and fail,
  crash-looping repmgrd into a backoff that outlived the install wait
  and failed the deploy. No chart behavior change beyond the image tag.

## Migrating from 0.5.86

`helm upgrade my-release cagriekin/pg` with image
`cagriekin/repmgr:trixie-5.5.0-10` is the migration; PostgreSQL pods
roll once for the new image tag.

## 0.5.86

### Fixed

- A full-cluster restart after a failover no longer rolls the database
  back to the failover point and destroys the surviving newer data
  (#125). Under the default `OrderedReady`, the lowest-ordinal pod is
  recreated first and alone, so the stale ex-primary (older timeline)
  came up read-write and the real primary (newer timeline) then
  re-cloned from it. Three layers fix it:
  1. The repmgr image (tag `trixie-5.5.0-9`) no longer re-clones a
     primary-state data directory by ordinal in `init-repmgr.sh`; it
     defers to the entrypoint guard, which only ever rewinds FORWARD to
     a newer-timeline peer. This stops the backward clone that destroyed
     data and makes role follow data state, not pod ordinal.
  2. A node whose timeline is at least as high as every reachable
     primary stays a primary instead of cloning down to a stale one.
  3. The service-updater records the highest-timeline primary in a
     durable, runtime-owned ConfigMap (`<fullname>-primary`) and refuses
     to route writes to a lone primary below that highwater -- so the
     stale pod that boots first under OrderedReady is never selected,
     while a legitimate new failover (always a higher timeline) is not
     blocked. The marker is written via kubectl at runtime, not as a
     helm template, so `helm upgrade` / ArgoCD sync cannot reset it.

## Migrating from 0.5.85

`helm upgrade my-release cagriekin/pg` plus the new image tag
`cagriekin/repmgr:trixie-5.5.0-9` is the migration; PostgreSQL pods roll
once (new image, new `PRIMARY_MARKER` env). The repmgr Role gains
`configmaps` get/create/patch for the marker. Running repmgr on
`postgresql.persistence.enabled=false` remains unsupported for the
full-restart case (the data dir must survive). If the recorded
highest-timeline primary is ever permanently lost, the service-updater
logs the exact `kubectl delete configmap <fullname>-primary` command to
accept its data loss and resume.

## 0.5.85

### Fixed

- The service-updater no longer repoints the write Service to a
  resurrected stale primary (#124). `get_current_master` trusted each
  node's self-reported `repmgr.nodes` metadata and returned the first
  responder in ordinal order, so a stale ex-primary still claiming
  `type=primary` in its own metadata would win and the selector would
  flip to it (with the readonly Service then serving the other
  timeline) -- silent data divergence. Master determination now
  classifies nodes by actual role (`pg_is_in_recovery()`), and the
  selector moves only when exactly one live primary exists; two or more
  is treated as a split-brain and never used to repoint.
- Split-brain fence now selects the survivor by timeline then numeric
  LSN, not a lexicographic string compare (#131). `pg_current_wal_lsn()`
  returns unpadded hex, so `[[ a > b ]]` mis-ordered LSNs across
  digit-width boundaries (`9/..` vs `10/..`) and could keep the behind
  node while wiping the ahead one. Timeline now dominates the choice (a
  stale primary can hold a higher LSN on the old timeline than the
  promoted primary on the new one), LSN segments are compared with
  `16#` arithmetic, and every stale primary is fenced and deleted (not
  just the last one seen) so 3+ way split-brains fully resolve; each
  deleted pod rejoins as a standby via the image's pg_rewind guard.

## Migrating from 0.5.84

`helm upgrade my-release cagriekin/pg` is the entire migration. No
pods roll: the service-updater ConfigMap is not checksummed into the
StatefulSet pod template, so running sidecars pick up the new logic on
their next restart (or restart the StatefulSet to apply immediately).
The fixes are behavioral and only change what happens during a
stale-primary resurrection or a split-brain.

## 0.5.84

### Changed

- The stale-primary protection for issue #123 moved from a chart-side
  bash wrapper into the repmgr image entrypoint (image tag
  `trixie-5.5.0-8`), so it runs on every container start, including
  container-only restarts (CrashLoopBackOff, OOM, liveness kill) that
  never re-run the init container -- the exact gap that let a crashed
  primary resume read-write on a stale timeline after a standby was
  promoted. Detection now reads the peer's timeline from
  `pg_walfile_name(pg_current_wal_lsn())` (which reflects a fast
  promotion immediately, unlike `pg_control_checkpoint()` which lags by
  minutes under load), settles only while no peer is reachable so a
  healthy primary restart adds no latency, and fails closed when the
  local timeline is unreadable while a peer is primary. Repair is an
  in-place `repmgr node rejoin --force-rewind` (pg_rewind works because
  PostgreSQL 18 initdb enables data checksums by default), falling back
  to a full re-clone only if rewind fails; a node whose data is empty
  while the cluster already has a primary refuses to initialize a
  divergent database. The chart now invokes the image entrypoint
  directly, passes `REPMGR_NODE_COUNT` so the peer scan matches the
  cluster size, and the obsolete `repmgr.stalePrimary.action` value
  was removed.

## Migrating from 0.5.83

`helm upgrade my-release cagriekin/pg` is the entire migration; the
StatefulSet pod template changes (new image tag, clean entrypoint
command), so PostgreSQL pods roll once. Running repmgr on
`postgresql.persistence.enabled=false` (emptyDir) is not recommended:
a container restart still rejoins correctly, but a pod recreation loses
the data dir, and if a standby was promoted in the meantime the node
refuses to initialize rather than fork a divergent cluster -- use
persistent volumes so pg_rewind/clone can repair it.

## 0.5.83

### Fixed

- A crashed primary no longer resurrects as a stale read-write primary
  when only its container restarts (#123). Re-clone logic lives in the
  repmgr-init initContainer, which does not re-run on container-only
  restarts (CrashLoopBackOff, OOM kills), so after a standby promotion
  the old primary would come back read-write on the stale timeline —
  a split-brain under default values. The postgresql container start
  is now wrapped by a guard: before starting read-write with existing
  data, it scans peers for an active primary on a NEWER timeline
  (promotion always bumps the timeline, so newer-elsewhere is proof of
  staleness) and refuses to start. `repmgr.stalePrimary.action`
  controls recovery: `reclone` (default) deletes the own pod so the
  existing repmgr-init re-clone and repmgrd re-register pipeline
  repairs the node; `halt` crash-loops with the data directory left
  untouched for inspection. pg_rewind-based repair is not possible on
  these clusters (initdb runs without data checksums or
  wal_log_hints), so re-clone via pod recreation is the repair path.
  The guard also refuses to initialize a fresh database when the data
  directory is empty but a peer is already an active primary
  (reachable on ordinal 0 without persistent storage, whose init path
  assumes first-boot), which previously bootstrapped a brand-new
  divergent primary next to the live cluster. Automatic re-clone of
  ordinal 0 therefore requires persistent storage; without it the pod
  halts with a clear error instead of silently splitting the cluster.

## Migrating from 0.5.82

`helm upgrade my-release cagriekin/pg` is the entire migration.
The StatefulSet pod template changes (wrapped container command), so
PostgreSQL pods roll once. Behavior changes only in the stale-primary
scenario, which previously produced a silent split-brain.

## 0.5.82

### Fixed

- The prometheus exporter `/metrics` endpoint returned HTTP 500 on
  every scrape in the unpublished 0.5.73-0.5.81 versions: the #22
  custom query group was named `pg_replication`, colliding with the
  built-in replication collector's `pg_replication_lag_seconds`
  (the Prometheus registry rejects two metrics with the same name
  and different help text, failing the whole scrape). The group is
  now `pg_wal_replication` and no longer duplicates the built-in
  lag metric; the 0.5.73 notes were corrected in place.

## Migrating from 0.5.81

`helm upgrade my-release cagriekin/pg` is the entire migration.
With the exporter enabled the configmap change rolls only the
exporter Deployment; database pods do not roll.

## 0.5.81

### Added

- PGPool troubleshooting guide at the end of the README (mirrored in
  pgvector): isolating connectivity failures between PGPool-II and the
  backends, checking backend status via `SHOW pool_nodes` and the pcp
  commands authenticated from the pgpool admin Secret, post-failover
  recovery including the `PrimaryChanged` Kubernetes Events emitted by
  the service-updater, readonly Service endpoint checks, and log
  locations with common messages (#25).

## Migrating from 0.5.80

Documentation only; no rendered resources change and no pods roll.

## 0.5.80

### Added

- Multi-Zone Deployment section in the README (#24): the built-in
  hostname and zone anti-affinity defaults, enforcing a hard zone
  requirement via `postgresql.affinity` (which replaces the built-in
  rules wholesale), spreading PGPool-II with
  `pgpool.topologySpreadConstraints`, `WaitForFirstConsumer` storage
  classes and the zonal volume pinning trade-off, and routing reads
  across zones through the `<fullname>-readonly` Service.

## Migrating from 0.5.79

Documentation only; no rendered resources change and no pods roll.

## 0.5.79

### Added

- The service-updater sidecar now records a core/v1 Kubernetes Event
  (reason `PrimaryChanged`, type Normal) on the primary Service every
  time it actually changes the selector to a new primary (#23), with
  the old and new pod names in the message, so failover history is
  visible via `kubectl describe service` and `kubectl get events`.
  The Event is created strictly after a successful selector patch and
  is best-effort: creation failures are logged as warnings and never
  fail or delay failover. The repmgr Role additionally grants
  `create` on core `events`.

## Migrating from 0.5.78

`helm upgrade my-release` is the entire migration. No pods roll with default values: the service-updater ConfigMap is not checksummed into the StatefulSet pod template and the Role is patched in place. Running sidecars keep executing the already-loaded script, so PrimaryChanged events start appearing after each pod's next restart. No new values.

## 0.5.78

### Added

- Read-only replica Service `<fullname>-readonly` for routing read traffic to standbys (#17), rendered whenever repmgr is enabled. Service selectors are equality-only and cannot express "not the primary", so the service selects a new `pg-role: standby` pod label that the service-updater sidecar now converges on every postgresql pod each reconciliation tick (the resolved primary gets `pg-role: primary`, everything else `standby`; pods recreated or added by scale-up are picked up on the next tick). Pods without the label are never selected, so the primary can never leak into the readonly endpoints. The repmgr Role's pods rule gains `get`/`list`/`patch` alongside the existing `delete`.

## Migrating from 0.5.77

`helm upgrade my-release cagriekin/pg` is the entire migration; no pods roll with default values (the StatefulSet pod template is unchanged and the service-updater configmap is not checksummed into it). Because the running service-updater process does not re-read its script, pg-role labeling -- and therefore readonly endpoints -- only activates once the service-updater containers restart (next pod roll or container restart); until then, and with `postgresql.replicaCount: 0` permanently, the `<fullname>-readonly` Service exists but has no endpoints, which is the safe default (unlabeled pods are never selected, so reads can never hit the primary by accident). The RBAC change applies immediately.

## 0.5.77

### Changed

- PGPool admin (PCP) credentials moved out of the pgpool ConfigMap and
  into a Secret (#15). `pgpool.admin.username` / `pgpool.admin.password`
  (defaults `admin`/`admin`) render into a chart-managed Secret, or
  bring your own via
  `pgpool.admin.existingSecret.{enabled,name,usernameKey,passwordKey}`.
  The init container now generates `pcp.conf` from the Secret at pod
  startup, so the plaintext password no longer lands in a ConfigMap.
  The old `pgpool.adminUsername` / `pgpool.adminPassword` values were
  removed and fail rendering when still set, instead of being silently
  ignored.

## Migrating from 0.5.76

With default values (pgpool.enabled=false) nothing changes and no pods roll. With PGPool enabled, the pgpool Deployment rolls once on upgrade (pcp.conf left the ConfigMap, so the config checksum changes); PostgreSQL pods do not roll. pgpool.adminUsername/pgpool.adminPassword were renamed: anyone setting them must move to pgpool.admin.username/pgpool.admin.password or pgpool.admin.existingSecret — rendering fails fast with a pointer to the new keys until they do. PCP credentials themselves are unchanged for default installs (admin/admin, now stored in a Secret).

## 0.5.76

### Changed

- Extension paths are no longer hardcoded to PostgreSQL 18 (#18). The
  copy-base-ext/copy-ext init-container `cp` commands and the
  ext-lib/ext-share volumeMounts now derive
  `/usr/lib/postgresql/<major>/lib` and
  `/usr/share/postgresql/<major>/extension` from the new
  `postgresql.majorVersion` value (default `"18"`), validated via
  `required` when `postgresql.extensions.enabled=true`. Keep it in
  sync with `postgresql.image.tag` when running a different major.

## Migrating from 0.5.75

`helm upgrade my-release cagriekin/pg` is the entire migration. With default values nothing rolls: `postgresql.majorVersion` defaults to "18", so every rendered manifest is byte-identical to the previous release (the affected paths only render when `postgresql.extensions.enabled=true`, and even then they resolve to the same /18/ paths). Users running a non-18 image with extensions enabled should set `postgresql.majorVersion` to match their image's major version; leaving it empty now fails the render with a clear error.

## 0.5.75

### Added

- `repmgr.monitoringHistoryDays` (default `7`) bounds the
  `repmgr.monitoring_history` table (#19). repmgrd runs with
  `monitoring_history=true` but repmgr 5.x has no conf-based retention
  (the image's `monitoring_history_keep` line is silently ignored as an
  unknown parameter), so the table grew forever. The repmgrd sidecar now
  spawns a resilient background loop that once per day, on the primary
  only, runs `repmgr cluster cleanup --keep-history=<days>`; cleanup
  failures log a warning and never take down repmgrd.

## Migrating from 0.5.74

`helm upgrade my-release cagriekin/pg` is the entire migration. With repmgr enabled (the default) the StatefulSet pod template changes (new env var and startup script in the repmgrd sidecar), so the postgresql pods roll once via the normal rolling update; repmgr handles the failover as on any upgrade. The first prune of an existing oversized monitoring_history table happens within 24h of the new pods starting. With repmgr disabled nothing changes and no pods roll.

## 0.5.74

### Added

- Zone-aware pod anti-affinity on the postgresql StatefulSet (#16). The
  default affinity block now includes a preferred (soft) podAntiAffinity
  term on `topology.kubernetes.io/zone` (weight 100) alongside the
  existing required hostname term, so pods spread across availability
  zones when possible while hostname spreading stays mandatory.
  Single-zone clusters are unaffected (the zone rule is best-effort),
  and a user-supplied `postgresql.affinity` still replaces the default
  block wholesale.

## Migrating from 0.5.73

With default values the StatefulSet pod template changes (a new preferred zone anti-affinity term), so postgresql pods roll once on upgrade following the chart's update strategy. The new rule is preferred (soft): scheduling behavior only changes on multi-zone clusters where the scheduler will now favor spreading pods across zones; single-zone clusters schedule exactly as before. Releases that set postgresql.affinity are unaffected — their custom affinity still replaces the default block entirely. No values changes or manual action required.

## 0.5.73

### Added

- Replication recovery-state and WAL-apply metrics in the prometheus
  exporter (#22): a `pg_wal_replication` custom query group exposes
  `pg_wal_replication_in_recovery` (`pg_is_in_recovery()` as a gauge
  — summing `in_recovery == 0` across the release's instances detects
  split-brain) and `pg_wal_replication_receive_replay_lag_bytes`
  (receive/replay LSN diff, `0` on the primary), alongside the
  exporter's built-in `pg_replication_lag_seconds` and
  `pg_replication_is_replica`. The queries run on every instance via
  the exporter's multi-DSN `/metrics` and the per-pod `/probe`
  ServiceMonitor targets, so standby lag is directly visible. The
  custom group deliberately avoids the `pg_replication` namespace:
  registering a metric name the built-in replication collector
  already serves makes the registry reject every scrape with HTTP
  500.

## Migrating from 0.5.72

`helm upgrade my-release cagriekin/pg` is the entire migration. With the default `prometheusExporter.enabled=false` nothing is rendered and no pods roll. With the exporter enabled, the configmap change rolls only the exporter Deployment (via its checksum/config annotation); database pods do not roll and no values changes are required — the new metrics appear on the next scrape.

## 0.5.72

### Fixed

- The backup script now verifies at least one backup newer than
  `RETENTION_DAYS` exists under the S3 prefix before running retention
  cleanup (#21). Previously `mc find --older-than --exec rm` ran
  unconditionally, so if uploads had been broken (or landing under a
  different prefix) for longer than `backup.retentionDays`, cleanup
  deleted every remaining backup. When no recent backup is visible the
  job now exits 1 without deleting anything. In the normal flow the
  just-uploaded dump satisfies the check, so the guard only fires when
  something is genuinely wrong.

## Migrating from 0.5.71

`helm upgrade my-release cagriekin/pg` is the entire migration. No pods roll with default values; with `backup.enabled=true` only the backup ConfigMap changes, which the next CronJob run picks up. No values changes are required. The backup job now fails (exit 1) instead of deleting when no backup newer than retentionDays is visible under the configured prefix — a condition that previously resulted in silent total deletion.

## 0.5.71

### Changed

- Default `postgresql.livenessProbe.failureThreshold` raised from 6
  to 10 (#20). With the default `periodSeconds: 10` the kubelet now
  waits 100s of failed `pg_isready` checks before restarting
  PostgreSQL instead of 60s, so sustained heavy load no longer
  triggers false liveness restarts. The readiness probe defaults are
  unchanged.

## Migrating from 0.5.70

With default values the StatefulSet pod template changes (livenessProbe.failureThreshold 6 -> 10), so PostgreSQL pods WILL roll once on upgrade. No action is required; releases that already override postgresql.livenessProbe.failureThreshold in their own values are unaffected and do not roll because of this change.

## 0.5.70

### Added

- Complete Chart.yaml metadata (#114): `home`, `icon`, `sources`,
  `keywords` and `maintainers`, shown by Artifact Hub and
  `helm show chart`.

## Migrating from 0.5.69

`helm upgrade my-release cagriekin/pg` is the entire migration.
Metadata only; no rendered resources change and no pods roll.

## 0.5.69

### Fixed

- NetworkPolicy egress no longer hardcodes port 443 as the only
  external port (#113). The postgresql policy now also allows 6443
  (API servers on kubeadm-style clusters, used by the service-updater
  sidecar and lifecycle-hook kubectl) and, when pgBackRest is enabled,
  the port derived from `pgbackrest.s3.endpoint` (explicit port wins;
  otherwise `http://` maps to 80 and anything else to 443). Previously
  WAL archiving to S3 endpoints on non-443 ports (e.g. MinIO `:9000`)
  and kubectl against 6443 API servers were silently dropped.

### Added

- `networkPolicy.postgresql.extraEgress` and
  `networkPolicy.pgpool.extraEgress` (both default `[]`) for
  additional egress rules, mirroring the existing `extraIngress`.

## Migrating from 0.5.68

`helm upgrade my-release cagriekin/pg` is the entire migration. No
pods roll. With `networkPolicy.enabled=false` (the default) nothing
changes; with it enabled the postgresql policy additionally allows
egress to 6443 and the pgBackRest S3 endpoint port.

## 0.5.68

### Added

- Per-component `priorityClassName` support (#112):
  `postgresql.priorityClassName`, `pgpool.priorityClassName`,
  `prometheusExporter.priorityClassName`, `backup.priorityClassName`
  and `pgbackrest.cronjob.priorityClassName` (all default `""`), so
  the database StatefulSet can be scheduled at higher priority than
  stateless workloads and survive node-pressure evictions.

## Migrating from 0.5.67

`helm upgrade my-release cagriekin/pg` is the entire migration. With
the default empty values nothing is rendered and no pods roll.

## 0.5.67

### Added

- `imagePullSecrets` (top-level value, default `[]`) now propagates to
  every pod template (#111): the PostgreSQL StatefulSet, the pgpool
  and prometheus exporter Deployments, and the backup and pgBackRest
  CronJobs. Previously no pod template carried pull secrets, so none
  of the chart's images could come from a private registry.

## Migrating from 0.5.66

`helm upgrade my-release cagriekin/pg` is the entire migration. With
the default `imagePullSecrets: []` nothing is rendered and no pods
roll.

## 0.5.66

### Fixed

- Credentials containing special characters no longer corrupt pgpool
  and exporter configuration (#108). Placeholder substitution in the
  pgpool and exporter init containers used
  `sed -i "s/__X__/$VALUE/g"`, which corrupts or fails on `/`, `&`
  and `\`; both now use a byte-safe awk splice with values passed via
  the environment, plus context-appropriate escaping: backslash
  escaping for pgpool.conf strings (`\\`, `\'`) and pool_passwd
  fields (`\\`, `\:`), YAML quote doubling for postgres_exporter.yml.
  The pgpool check passwords moved out of pgpool.conf entirely: blank
  values make pgpool read pool_passwd, whose entry is now
  `TEXT`-prefixed -- unprefixed entries are taken as md5 hashes,
  which happened to work against md5 backends (repmgr image) but
  cannot answer the scram challenges of standalone official-image
  backends. The exporter `DATA_SOURCE_NAME` env built its URI from
  raw `$(VAR)` expansion, which `@`, `:`, `/`, `?`, `#` or `%` in
  credentials break; the init container now assembles the DSN with
  every credential byte percent-encoded and the exporter reads it
  from a file. Chart-generated passwords are alphanumeric and were
  unaffected; `existingSecret` passwords are arbitrary and hit all of
  these paths.
- pgpool probes now run a query through pgpool instead of a TCP
  connect (#122). A pgpool that rejects every session with "all
  backend nodes are down" still accepts TCP, so the old probes kept
  it Ready and never restarted it -- a permanent wedge reachable on
  any standalone install whose backend is unready for ~30s at
  startup (the repmgr flavor was masked by the service-updater
  restarting pgpool). A failing liveness now restarts pgpool, which
  rediscovers backends with a fresh status file.
- The pgpool pod template now carries a checksum of the pgpool
  configmap: pgpool.conf and pool_passwd are rendered into an
  emptyDir by the init container, so configmap changes previously
  never reached running pods.

## Migrating from 0.5.65

`helm upgrade my-release cagriekin/pg` is the entire migration. The
pgpool and exporter pod templates change, so those deployments roll
once; the PostgreSQL StatefulSet is untouched.

## 0.5.65

### Fixed

- Disabling `postgresql.configuration` and `pgbackrest` after they had
  been enabled bricked the cluster on the next pod restart (#107): the
  `include_dir = '/etc/postgresql/conf.d'` line appended to
  `postgresql.conf` persists in PGDATA, but the conf.d configmap mount
  is removed, and PostgreSQL refuses to start on a missing include_dir
  directory. The setup-config init container now always runs: it
  appends the line when either feature is enabled and strips a stale
  line when both are disabled.

## Migrating from 0.5.64

`helm upgrade my-release cagriekin/pg` is the entire migration. The
StatefulSet pod template changes, so pods roll once. If a cluster is
already crash-looping from this defect, upgrading to this version
repairs it: the init container strips the stale line before PostgreSQL
starts.

## 0.5.64

### Fixed

- The postgresql preStop hook no longer attempts to promote a standby;
  it now only stops PostgreSQL cleanly and leaves promotion to repmgrd
  (#102). The old hook's remote `pg_promote()` never actually ran (the
  image ships no `.pgpass` and pg_hba requires scram/md5 cross-pod, so
  the unauthenticated call failed silently behind `2>/dev/null`), and
  its verification loop polled local `pg_is_in_recovery()` -- always
  `f` on the old primary -- burning the full 30s on every primary
  shutdown. Repairing the call as #102 originally suggested proved
  worse than removing it: a raw `pg_promote()` bypasses repmgr.nodes
  metadata, the promoted node keeps `type='standby'`, and every
  repmgrd crash-loops on the stale metadata (reproduced in the upgrade
  test). repmgrd's own `promote_command` (`repmgr standby promote`)
  updates the metadata correctly and is the only promotion path the
  cluster has ever actually converged through.

## Migrating from 0.5.63

`helm upgrade my-release cagriekin/pg` is the entire migration. The
StatefulSet pod template changes, so pods roll once. Primary shutdown
during the roll is now ~30s faster (no dead verification loop);
failover behavior is unchanged because the removed promotion never
executed.

## 0.5.63

### Fixed

- postStart primary discovery scanned `seq 0 (replicaCount - 1)` while
  the StatefulSet runs `replicaCount + 1` pods, so
  `lifecycle.postStart.additionalCommands` was silently skipped
  whenever the primary was the last ordinal after a failover (#103).
  The loop now scans ordinals `0..replicaCount`, matching the
  service-updater.
- repmgrd pre-register role detection used
  `psql -h 127.0.0.1 -U postgres -d postgres`, which only worked
  because the image's initdb happens to create a `postgres` superuser
  and trust 127.0.0.1; it now uses the repmgr credentials already in
  the container env (#104).
- The repmgrd pre-register peer scan iterated a hardcoded `seq 0 9`,
  breaking primary discovery for clusters with more than 10 pods; the
  bound now derives from `replicaCount`. The type-backfill node id is
  read from the generated `/etc/repmgr/repmgr.conf` instead of
  re-deriving the image's `ordinal + 1000` convention, and the
  backfill is skipped with an explicit error if `node_id` cannot be
  parsed (#105).

## Migrating from 0.5.62

`helm upgrade my-release cagriekin/pg` is the entire migration. The
StatefulSet pod template changes, so pods roll once.

## 0.5.62

### Fixed

- `helm upgrade` no longer repoints the primary Service selector back
  to pod-0 (#109). The rendered Service now preserves the live
  `statefulset.kubernetes.io/pod-name` selector via `lookup`, mirroring
  the secret reuse pattern, falling back to pod-0 only at bootstrap.
  Previously every upgrade after a failover routed writes at a
  read-only standby until the service-updater's next tick (up to 30s)
  -- and with helm v4 (server-side apply) the upgrade failed outright
  with a field-manager conflict on `.spec.selector`, because the
  service-updater's `kubectl patch` owns the field once a failover has
  occurred. Rendering pipelines that never talk to the cluster
  (`helm template`, ArgoCD) still emit the pod-0 bootstrap selector;
  the service-updater re-asserts the correct primary on its next tick.

## Migrating from 0.5.61

`helm upgrade my-release cagriekin/pg` is the entire migration. If a
previous helm v4 upgrade already failed with
`conflict with "kubectl-patch" using v1: .spec.selector`, this version
resolves it: the rendered selector now matches the live value, so the
apply no longer conflicts.

## 0.5.61

### Fixed

- Rendering now fails fast when `repmgr.enabled=false` is combined with
  `postgresql.replicaCount > 0` (#106). The StatefulSet always runs
  `replicaCount + 1` pods; without repmgr those extra pods were
  independent PostgreSQL instances with their own PVCs and no
  replication, while the PGPool config labeled them `replica1..N` under
  `streaming_replication` clustering -- reads silently hit empty or
  diverged databases. Standalone mode requires
  `postgresql.replicaCount=0`.

## Migrating from 0.5.60

`helm upgrade my-release cagriekin/pg` is the entire migration for
repmgr deployments and single-instance standalone deployments. If your
values set `repmgr.enabled=false` with `postgresql.replicaCount > 0`,
the upgrade is rejected at template time: those extra pods were never
replicas, and any data written to them through PGPool load balancing
exists only on that pod. Recover that data before switching to
`repmgr.enabled=true` (which re-clones standbys from the primary) or
`postgresql.replicaCount=0` (which orphans the extra PVCs).

## 0.5.60

### Fixed

- Backup against TLS S3 endpoints (real AWS S3) failed with
  `x509: certificate signed by unknown authority`: the postgres image
  ships no CA bundle (`/etc/ssl/certs/ca-certificates.crt` absent in
  `postgres:18.1-trixie`). The 0.5.59 kind test used plain-HTTP MinIO,
  so the gap only surfaced in production. The mc-installer init
  container now also copies the mc image's CA bundle into the shared
  volume and the backup script exports `SSL_CERT_FILE` pointing at it.

## Migrating from 0.5.59

`helm upgrade my-release cagriekin/pg` is the entire migration. No PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover.

## 0.5.59

### Fixed

- Backup CronJob pods never started: the pod spec set `runAsNonRoot: true`
  without `runAsUser`, and both the postgres and minio/mc images default
  to root, so the kubelet rejected container creation with
  `CreateContainerConfigError` until the job hit `activeDeadlineSeconds`
  (`DeadlineExceeded`, no logs, no events by morning). Backup pod and
  container security contexts are now configurable via
  `backup.podSecurityContext` and `backup.containerSecurityContext`,
  defaulting to `runAsUser: 999` / `runAsGroup: 999` (the postgres uid in
  the official image).
- Wired `test-backup-restore` into the `test-cluster` Make target so the
  backup path is exercised by the standard test run.
- Generated secret rotated `password` and `repmgr-password` on every
  `helm upgrade` (`randAlphaNum` with no reuse), so any upgrade that
  added a standby deadlocked: the new pod mounted fresh credentials the
  running cluster did not have and `repmgr-init` looped on
  `password authentication failed` until the rollout timed out
  (`test-upgrade` failure, Ready 2/3). The secret template now reuses
  values from the live secret via `lookup` and only generates passwords
  that do not exist yet. Note: `lookup` returns nothing under
  `helm template`/`--dry-run`, so rendering pipelines that never talk to
  the cluster (e.g. ArgoCD) should keep using
  `postgresql.existingSecret`.

## Migrating from 0.5.58

`helm upgrade my-release cagriekin/pg` is the entire migration. No PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover.

## 0.5.58

### Fixed

- 0.5.57 dropped the image entrypoint's `Waiting for local PostgreSQL`
  and `primary register --force` steps along with the broken standby
  verify, so primary pods crashed at boot. Restore both in the chart
  wrapper: wait for local PG via `pg_isready`, then branch on
  `pg_is_in_recovery()` — `f` runs `primary register --force`, `t`
  runs the existing standby pre-register block. Standby gate changed
  from `ORDINAL != 0` to `IN_RECOVERY = t` so a failed-over pod-0
  rejoining as standby also takes the standby path.

## Migrating from 0.5.57

`helm upgrade my-release cagriekin/pg` is the entire migration. No PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover.

## 0.5.57

### Fixed

- Bypass the repmgr image's `repmgrd-entrypoint.sh` and exec `repmgrd`
  directly from the StatefulSet's repmgrd container command. The image
  entrypoint re-runs `standby register --force` (which on PG18 +
  repmgr 5.5.0-7 lands `type=''` again) and then verifies via
  `psql -h <primary> -U repmgr -d repmgr` without `PGPASSWORD`. Internal
  cluster traffic hits `scram-sha-256` in pg_hba, the verify query
  comes back empty, and the loop exits 1 with
  `Primary does not show node N as standby (current type: )`. The
  pre-register block in this chart already registers the standby and
  backfills `type='standby'`, so the image's register+verify is
  redundant.

## Migrating from 0.5.56

`helm upgrade my-release cagriekin/pg` is the entire migration. No PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover. Standby pods that were CrashLooping on repmgrd converge on
their next restart; unaffected clusters see no behaviour change.

## 0.5.56

### Fixed

- Defensive `UPDATE repmgr.nodes SET type='standby' WHERE node_id=<local>
  AND (type IS NULL OR type='')` after pre-register on the primary.
  Some PG18 + repmgr image combos report `standby registration complete`
  but leave the row's `type` empty, breaking the image's verify-loop
  with `Primary does not show node N as standby (current type: )`.
  WHERE clause makes the UPDATE a no-op on already-correctly-typed
  rows.
- Test: new `pg/tests/test-repmgr-chaos.sh` deletes the standby pod
  3 times and re-asserts `type='standby'` after each replacement —
  the post-restart re-occurrence shape that manual SQL fixes cannot
  cover. Wired into `Makefile` and CI.

## Migrating from 0.5.55

`helm upgrade my-release cagriekin/pg` is the entire migration. No PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover. Affected clusters converge `type='standby'` on the next
standby restart; unaffected clusters see no behaviour change.

## 0.5.55

### Fixed

- `fix_user_auth` postStart hook (0.5.54) used
  `psql -c "DO $$ ... :'u' ... $$;"`. `psql -c` does **not** perform
  `:'var'` substitution — per docs, the command must contain "no
  psql-specific features". The server received `:'u'` literally and
  rejected every call with `ERROR: syntax error at or near ":"`; the
  MD5→SCRAM rehash silently never ran. Connectivity kept working because
  the md5-fallback line in `pg_hba.conf` accepted the legacy hashes,
  masking the failure on noisy clusters.
  Fix: build the SQL into a bash variable and feed it to psql via
  here-string (`<<<`), which goes through the MainLoop reader where
  `:'var'` IS substituted. Values flow through per-session GUCs
  (`myvars.tgt_user` / `myvars.tgt_pass` — `user` would clash with the
  reserved keyword) read back inside the DO block via `current_setting()`.
  PG<14 skip, `format('%I/%L')` quoting, idempotent `rolpassword LIKE
  'md5%'` gate preserved. Failure now logs a loud line to pod stdout
  instead of being swallowed by `>/dev/null`.
- Standby `repmgr.nodes` row landed with `type=''` on the primary because
  `cagriekin/repmgr:trixie-5.5.0-7`'s entrypoint runs `repmgr standby
  register --force` without `--upstream-node-id`, crashing the standby
  with `Primary does not show node N as standby`. The chart's `repmgrd`
  container now pre-registers with an explicit upstream node_id and
  delegates to the image entrypoint. Workaround until the image ships
  the fix.

## Migrating from 0.5.54

`helm upgrade my-release cagriekin/pg` is the entire migration: no PVC
recreate, no StatefulSet recreate, no password rotation, no forced
failover, no new required `values.yaml` field, PG13 still skipped. The
MD5→SCRAM rehash completes on the first 0.5.55 pod start (idempotent on
re-run).
