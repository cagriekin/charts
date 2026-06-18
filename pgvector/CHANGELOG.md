# pgvector chart changelog

## Unreleased

### Security

- **The PgPool-II PCP admin port (9898) is no longer exposed on the Service by default
  (#118).** The pgpool Service published the PCP admin/control port cluster-wide while
  the pgpool NetworkPolicy only admits 9999. It is now gated behind
  `pgpool.service.exposePcp` (default `false`); enable it only if you run `pcp_*`
  commands against the Service (and add a `pgpool.extraIngress` rule for 9898 under
  NetworkPolicy).
- **The `fix-permissions` init container drops its excess capabilities (#162).** The
  chown init container needs root but inherited the full default capability set; it now
  drops ALL and adds back only `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, matching the chart's
  other root init container.
- **The pgBackRest backup CronJob is now security-hardened (#155).** It was the one
  pod with zero hardening (ran as root, full caps, no seccomp) yet carries the
  exec-capable pgBackRest SA token, and failed admission in Pod-Security-`restricted`
  namespaces. It now applies `pgbackrest.cronjob.podSecurityContext` /
  `containerSecurityContext` (defaults: `runAsNonRoot`, `runAsUser: 65534`,
  `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`, drop ALL),
  matching the chart's other pods.
- **Pods that make no Kubernetes API calls no longer mount a ServiceAccount token
  (#166).** The pgpool Deployment, the prometheus-exporter Deployment, the backup
  CronJob, and the StatefulSet in standalone (`repmgr.enabled=false`) mode now set
  `automountServiceAccountToken: false` (they ran as the default SA with its token
  projected in, an unnecessary credential). The repmgr StatefulSet keeps its token (the
  agent / service-updater call the API).
- **S3 credentials no longer passed on the `mc` command line (#167).** The backup
  job ran `mc alias set s3 <endpoint> <access-key> <secret-key>`, exposing both keys
  in the process argv (`/proc/<pid>/cmdline`, readable via `ps`) on every scheduled
  run. Credentials are now supplied to `mc` via the `MC_HOST_s3` environment variable
  (percent-encoded), so they never appear in argv. Requires `backup.s3.endpoint` to
  include a scheme (`http://`/`https://`), which `mc` already required.

### Documentation

- **Documented scale-down ghost nodes in `repmgr.nodes` (#139).** Scaling
  `postgresql.replicaCount` down removes the highest-ordinal pods but does not
  unregister them from `repmgr.nodes`, so they linger as `active` ghosts (`repmgr
  cluster show` shows them failed; in repmgrd mode survivors keep retrying the gone DNS
  names). After scaling down, manually unregister each removed ordinal from the primary:
  `repmgr -f /etc/repmgr/repmgr.conf standby unregister --node-id=<ordinal+1000>`.
  (Automatic deregistration is not yet implemented — tracked in #139.)
- **Clarified that `networkPolicy.postgresql.allowExternal=false` blocks the read-only
  Service (#148).** `allowExternal` gates direct client access to PostgreSQL on 5432 —
  the path the `<fullname>-readonly` Service (direct standby reads) uses — so with
  `allowExternal: false` those read connections silently time out while endpoints look
  healthy (PGPool on 9999 stays reachable, so read-write clients via PGPool are
  unaffected). Documented in `values.yaml` with a scoped `extraIngress` recipe to
  re-allow direct-5432 clients. No default behavior change.
- **The pgBackRest PITR restore runbook could not work as written (#149).** The
  documented restore pod mounted only the data PVC and set the S3 key env vars, but
  not the `<fullname>-pgbackrest` ConfigMap — the only place `pg1-path` and the
  `repo1-*` S3 settings live — so `pgbackrest restore` failed with `requires option:
  pg1-path` and would default to a local posix repo. The runbook now mounts the
  ConfigMap at `/etc/pgbackrest/pgbackrest.conf`, sources the keys from the existing
  pgBackRest secret, sets the chart's `securityContext` (101:103), adds the required
  `--type=time` to the `restore --target` command, corrects the `keyType: auto`
  guidance (bind to the `<fullname>-repmgr` SA, not the default), and uses the current
  image tag.
- **Brought the pgvector README into parity with the pg README (#152).** The pgvector
  README is an independent copy that had fallen behind: the entire NetworkPolicy section
  and ~32 parameter rows backed by `values.yaml` — security contexts (postgresql / pgpool /
  exporter), `repmgr.splitBrainDetection.action`, pgpool clear-text auth / topology spread /
  node placement / metrics probes / `logMinMessages`, postgresql md5→scram migration / node
  placement / SA annotations, and backup `mc` image / security contexts / `activeDeadlineSeconds`
  / `backoffLimit` — were undocumented, some referenced by the README's own prose. Ported
  the missing section and rows verbatim from pg (defaults are identical).
- **Fixed pgBackRest docs that described the removed scheduler-sidecar architecture
  (#151).** "How It Works" still credited a `pgbackrest-scheduler` sidecar and the parameter
  table labeled `pgbackrest.resources` as "Scheduler sidecar" while omitting every
  `pgbackrest.cronjob.*` tunable. Updated the prose to the CronJob-exec architecture and
  added the `pgbackrest.cronjob.*` rows.
- **Corrected the documented `pgpool.image.tag` default (#150).** The README listed `4.7.0`;
  the chart ships `cagriekin/pgpool:4.7.1` (the pg README was already correct). Also synced
  the `repmgr.image.tag` row to the shipped `trixie-5.5.0-18`.
- **Fixed five CHANGELOG migration commands that named the wrong chart (#163).** Five
  "Migrating from" notes were copy-pasted from pg and read `helm upgrade my-release
  cagriekin/pg`; running that against a pgvector release would swap the chart. Corrected to
  `cagriekin/pgvector`.
- **Relaxed the `appVersion`/README PostgreSQL-version claim from `18.1` to `18` (#164).**
  The default image tag `pg18-trixie` floats with upstream pgvector publishing and pins only
  the PostgreSQL major (no upstream tag pins the minor), so the `18.1` appVersion (stamped
  into `app.kubernetes.io/version`) and the README claim could silently diverge from the
  deployed minor. Both now state `18`, matching what the tag guarantees.

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
- **Backup integrity check no longer buffers the whole dump to `/tmp` (#119).** The
  verify step wrote the entire dump to the container's unbounded writable layer before
  `pg_restore --list`; a large DB could hit node-disk eviction. It now streams
  (`mc cat … | pg_restore --list`).
- **Init containers now declare resource requests/limits (#153).** No init container set
  resources, so in a namespace with a `ResourceQuota` every pod was rejected at admission
  unless a `LimitRange` injected defaults, and the `repmgr-init` clone ran unbounded. The
  lightweight inits now use a small shared default; `repmgr-init` uses an overridable
  `repmgr.initContainerResources`.
- **emptyDir volumes are now size-capped (#165).** No emptyDir set a `sizeLimit`, so a
  runaway volume — especially PGDATA when `persistence.enabled=false` — could fill the
  node and evict unrelated pods. Fixed caps are set on the config/tool/extension volumes
  (16Mi/128Mi/1Gi), and the non-persistent data volume gets a configurable
  `postgresql.persistence.emptyDir.sizeLimit` (default empty = unbounded).
- **pgBackRest `stanza-create` no longer masks real failures (#160).** The backup
  CronJob ran `stanza-create || true`, which swallowed not just the benign "stanza
  already exists" case (`stanza-create` is idempotent and exits 0 then) but also genuine
  failures (S3 permissions, repo lock, `kubectl exec` errors, a needed `stanza-upgrade`),
  so the job failed later at the backup step with a misleading error. Dropped `|| true`;
  under `set -eu -o pipefail` a real failure now aborts at the right step.
- **The postgres-exporter NetworkPolicy now has a cross-namespace scrape escape hatch
  (#147).** The exporter's 9116 metrics ingress admitted same-namespace pods only and,
  unlike the postgresql/pgpool policies, had no `extraIngress` value, so a Prometheus in
  a separate monitoring namespace could not scrape it. Added
  `networkPolicy.prometheusExporter.extraIngress` / `extraEgress` so a `namespaceSelector`
  rule can allow the scraper. No default behavior change.
- **postgres-exporter probes now detect a broken scrape pipeline (#146).** Both probes
  hit the always-200 landing page `/`, so a `queries.yaml`/collector regression that
  makes every scrape return HTTP 500 left the exporter pods Ready and never restarted
  while DB metrics went dark. The liveness and readiness probes now hit `/metrics`
  (matching the pgpool exporter): 500 on genuine exporter/registry breakage, but 200 +
  `pg_up 0` on a database outage, so it catches config breakage without flapping when
  the DB is merely down.
- **pgpool PDB no longer wedges node drains on a single-replica install (#161).** The
  pgpool PodDisruptionBudget used `minAvailable: 1` with the default `pgpool.replicaCount:
  1`, so allowed disruptions were permanently 0 and `kubectl drain` / node upgrades /
  autoscaler scale-down hung on the pgpool node. It now uses `maxUnavailable: 1` +
  `unhealthyPodEvictionPolicy: AlwaysAllow` (mirroring the postgresql PDB): a
  single-replica pgpool can be evicted (stateless, reschedules), while a multi-replica
  pgpool keeps rolling protection. The shared `common.podDisruptionBudget` helper now
  renders exactly one of `minAvailable`/`maxUnavailable`, so a partial override can no
  longer emit both (which the API rejects).
- **Numeric/boolean-looking env values no longer fail at apply (#156).** Several
  container env values (`REPMGR_USER`, `REPMGR_DB`, `PGBACKREST_STANZA`, `STANZA`,
  `SPLIT_BRAIN_ACTION`, the Service/marker/Lease names, FQDNs) were interpolated into
  `value:` without `| quote`, so a value YAML-typed as a number/bool (e.g.
  `repmgr.database=12345`, `pgbackrest.stanza=123`) rendered as a bare scalar the API
  server rejects (`cannot unmarshal number into field of type string`) — passing
  `helm template`/`lint` but failing at apply. All user-facing env values are now
  `| quote`d (composite names via `printf … | quote`).
- **Single quotes in `postgresql.configuration` / `pgpool.resetQueryList` no longer
  produce an invalid config (#157).** Both were interpolated naively into single-quoted
  conf lines, so a value containing a `'` rendered a syntactically-invalid `custom.conf`
  (postgres CrashLoopBackOff after the config roll) or a broken `reset_query_list`
  (pgpool fails to start). Embedded single quotes are now doubled (`''`, the
  PostgreSQL/pgpool conf-lexer escape) in `postgresql.configuration` values,
  `pgpool.resetQueryList`, and the `archive_command` stanza. Values without quotes
  render unchanged. (Newline-bearing conf values remain unsupported — they fail the
  render rather than injecting a directive.)
- **Long release/fullname now fails fast instead of rendering invalid resource names
  (#158).** `pg.fullname` is capped at 63 but per-resource suffixes are appended after
  it, so a long `fullnameOverride` could render a Service name over 63 chars or a
  CronJob name over ~52 chars with no render-time hint. The chart now validates
  composed Service (≤63), Deployment-backed (≤47, for pgpool/exporter Pod names) and
  CronJob (≤52) names at render time and fails with a clear message. Truncation was rejected as unsafe on a stateful chart (collision risk).
  Normal names are unaffected.
- **A failed `pg_dump` left a truncated dump masquerading as the newest backup
  (#159).** If `pg_dump` exited non-zero mid-stream, `mc pipe` finalized the truncated
  object at the canonical `backup_<ts>.dump` name and it stayed the newest backup until
  the next successful run, so an operator restoring "the latest" could pick a corrupt
  dump. The dump is now streamed to a `.tmp` staging object and published with `mc mv`
  only after the `pg_restore --list` integrity check passes; an EXIT trap removes the
  staging object on failure and retention sweeps stale `.tmp` objects.
- **pgBackRest config changes (S3 endpoint/bucket/retention) didn't roll the pods
  (#145).** `pgbackrest.conf` is a subPath mount (never live-updated by the kubelet)
  and the StatefulSet pod template did not checksum the pgBackRest ConfigMap, so after
  a `helm upgrade` that repointed the repository the pods kept archiving WAL + running
  backups against the OLD location until manually restarted. The pod template now
  carries a `checksum/pgbackrest-config` annotation, so any pgBackRest config change
  rolls the StatefulSet and the new config takes effect. Operator note: changing
  `pgbackrest.s3.*`/`pgbackrest.retention.*` now restarts the pods (previously a
  no-op); rolling the current primary triggers a controlled failover, the same as any
  rolling upgrade.
- **Backup retention could delete another release's dumps under a shared
  bucket/prefix (#143).** `pg_dump` backups were written to a flat
  `<bucket>/<prefix>/backup_<ts>.dump` with no release identity, and the retention
  `mc find ... --older-than --exec mc rm` ran recursively over the whole prefix with
  no name filter — so two releases sharing one bucket/prefix each deleted the other's
  dumps older than their own `retentionDays`. Dumps are now namespaced per release
  under `<prefix>/<release-fullname>/` (mirroring the pgBackRest repo layout), and
  both the recent-backup guard and the retention delete are scoped to that subpath
  with a `--name 'backup_*.dump'` filter. Existing dumps under the old flat path are
  left in place (not migrated, not deleted); see the README restore section for the
  new path layout.

## 1.0.2

Bugfix for agent mode (the 1.0.0 default), in lockstep with pg 1.0.2. Image moves
to `trixie-5.5.0-18`. No chart-template or values changes beyond the image tag; a
`helm upgrade` rolls the pods once.

### Fixed

- **Agent re-ran `repmgr standby follow` every reconcile tick on a healthy,
  already-streaming standby and logged an ERROR each time (#182).** The `Follow`
  executor latched its idempotency guard (`followUpstream`) only after
  `repmgr standby follow` returned success. On a standby already correctly streaming
  from the lease holder -- the steady state right after a repmgrd->agent migration
  (`primary_conninfo` persists across the roll) or a post-failover rejoin -- the
  command exits non-zero (`slot "..." already exists as an active slot` / `this
  server is not ahead`), so the guard never latched and the agent re-forked the
  failing command every ~5s. Replication was unaffected, but the ERROR spam buried
  genuine errors and tripped log-based alerting. The agent now (1) skips
  `repmgr standby follow` when it observes via `pg_stat_wal_receiver` that the
  standby is already streaming from the target, and (2) treats the benign "already
  following" repmgr exit as a successful no-op. Repointing to a genuinely new
  upstream (after a leader change) still runs `follow`. (Issue reported against
  pgvector 1.0.1 / `trixie-5.5.0-17`.)

## 1.0.1

Bugfix for agent mode (the 1.0.0 default), in lockstep with pg 1.0.1. Image moves
to `trixie-5.5.0-17`.

### Fixed

- **Agent standby never re-established streaming after a failover / repmgrd->agent
  migration (#181).** A freshly-cloned, still-recovering standby was misclassified
  as a stopped primary (SQL unreachable + `pg_controldata` still showing the source's
  `in production` state), so the agent issued `RejoinForward` and killed the
  standby's walreceiver, then looped `StartLocal`; the cluster was left single-node.
  The agent now tracks process liveness separately from SQL readiness and waits for a
  starting node to become ready instead of acting on its transient on-disk role. See
  the pg 1.0.1 entry for full detail; the charts share the agent.



First major release, in lockstep with pg 1.0.0 (the two charts now share a single
1.0.0 version line). The lease-based Go agent (`pg-ha-agent`) is now the
**default** failover mode. The repmgr image is `trixie-5.5.0-16`.

### BREAKING

- **`repmgr.failoverMode` defaults to `agent`** (was `repmgrd`). The legacy
  repmgrd path remains available via `repmgr.failoverMode: repmgrd` (deprecated,
  one major cycle).
- Agent mode uses `podManagementPolicy: Parallel` (immutable), so switching an
  existing repmgrd release needs a one-time `--cascade=orphan` StatefulSet recreate.
- Agent mode ships a hardened `pg_hba.conf` (pod-CIDR + SCRAM, no `0.0.0.0/0 md5`);
  add explicit `postgresql.pgHba` rules if you relied on the broad md5 rule.
- postgresql PDB default is `maxUnavailable: 1` + `unhealthyPodEvictionPolicy:
  AlwaysAllow` (was `minAvailable: 1`; k8s >= 1.27).

### Migrating to 1.0.0

- **Stay on repmgrd:** set `repmgr.failoverMode: repmgrd` and `helm upgrade` (pods
  roll once for image `trixie-5.5.0-16`; no other change).
- **Adopt agent mode (default):** fresh backup, then
  `kubectl delete statefulset <release>-pgvector --cascade=orphan -n <ns>` followed
  by `helm upgrade` (recreates the StatefulSet as `Parallel`, adopts the orphaned
  pods). Verify the `<release>-pgvector-leader` Lease holder is the primary. See
  the pg chart README for the full agent-mode runbook (it applies identically).

The sections below describe the agent machinery now shipping as the 1.0.0 default;
the repmgrd rendering is byte-stable.

### Added

- `repmgr.failoverMode: agent` — a Go agent (`pg-ha-agent`) running as PID 1 in
  the postgresql container holds a Kubernetes `coordination.k8s.io/v1` Lease as
  the sole authority for which node is primary and drives repmgr as a pure
  mechanism (no repmgrd). See the pg 0.5.89 changelog for the full agent-mode
  wiring (this chart's templates are shared with pg). It becomes the default at
  chart `1.0.0`.
- `repmgr.agent.*` tunables (`leaseDuration`, `renewDeadline`, `retryPeriod`,
  `reconcileInterval`, `podCidr`) and `repmgr.agent.monitoring.*` (opt-in
  ServiceMonitor + example PrometheusRule for the agent metrics).
- Agent operability (shared with pg): cluster-identity safety
  (`system_identifier` check before clone/follow/rewind), maintenance mode
  (`pg-ha/pause` annotation), controlled switchover (`pg-ha/switchover-target`
  annotation), and a `schemaVersion` on the on-DCS data for safe mixed-version
  agent upgrades. See the pg 0.5.89 changelog for details.
- etcd leadership backend (opt-in): `repmgr.agent.dcs.backend: etcd` (BYO/shared
  via `dcs.etcd.endpoints`, or a bundled 3-node etcd subchart via `etcd.enabled=true`)
  decouples leadership from the Kubernetes control plane. Default stays `kubernetes`.
  See the pg 0.5.89 changelog for details.

### Notes

- Opt-in; repmgrd installs need no action. **Migrating to agent mode** needs a
  one-time `kubectl delete statefulset <release>-pgvector --cascade=orphan` then
  `helm upgrade --set repmgr.failoverMode=agent` (`podManagementPolicy` is
  immutable). Runbook + GitOps caveats: README "Failover modes"; injected env:
  `ENVIRONMENT.md`.

## 0.6.90

Bundles the stale-primary/HA hardening, operational fixes, fail-fast
validation, and RBAC scoping accumulated since 0.6.89. The repmgr image is
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
  own statement), fixing the automatic `CREATE EXTENSION vector` and any user
  DDL that was a silent no-op (#127).
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
- Backups render with `pgbackrest`/scheduled `pg_dump` enabled: the missing
  `backup.mc` image and the backup container securityContexts no longer produce
  a nil-pointer at template time (#126).

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

## Migrating from 0.6.89

`helm upgrade my-release cagriekin/pgvector` with image
`cagriekin/repmgr:trixie-5.5.0-15` is the migration; PostgreSQL pods roll once
for the new image tag, the new `startupProbe`, and the RBAC scoping. Note that
the new fail-fast guards (#133, #137, #142) reject previously-accepted but
broken configurations at render time — if an upgrade now fails to template,
the error message names the offending value.

## 0.6.89

### Fixed

- repmgr image bumped to `trixie-5.5.0-10`: the primary node is now
  registered with a retry loop (matching the standby path) and the
  role probe retries until definitive. Previously `repmgrd-entrypoint`
  ran a single `repmgr primary register` under `set -e`; on a slow or
  contended host that register could race the postgresql container's
  init SQL (`CREATE EXTENSION repmgr`, repmgr user) and fail,
  crash-looping repmgrd into a backoff that outlived the install wait
  and failed the deploy. No chart behavior change beyond the image tag.

## Migrating from 0.6.88

`helm upgrade my-release cagriekin/pgvector` with image
`cagriekin/repmgr:trixie-5.5.0-10` is the migration; PostgreSQL pods
roll once for the new image tag.

## 0.6.88

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

## Migrating from 0.6.87

`helm upgrade my-release cagriekin/pgvector` plus the new image tag
`cagriekin/repmgr:trixie-5.5.0-9` is the migration; PostgreSQL pods roll
once (new image, new `PRIMARY_MARKER` env). The repmgr Role gains
`configmaps` get/create/patch for the marker. Running repmgr on
`postgresql.persistence.enabled=false` remains unsupported for the
full-restart case (the data dir must survive). If the recorded
highest-timeline primary is ever permanently lost, the service-updater
logs the exact `kubectl delete configmap <fullname>-primary` command to
accept its data loss and resume.

## 0.6.87

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

## Migrating from 0.6.86

`helm upgrade my-release cagriekin/pgvector` is the entire migration. No
pods roll: the service-updater ConfigMap is not checksummed into the
StatefulSet pod template, so running sidecars pick up the new logic on
their next restart (or restart the StatefulSet to apply immediately).
The fixes are behavioral and only change what happens during a
stale-primary resurrection or a split-brain.

## 0.6.86

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

## Migrating from 0.6.85

`helm upgrade my-release cagriekin/pgvector` is the entire migration; the
StatefulSet pod template changes (new image tag, clean entrypoint
command), so PostgreSQL pods roll once. Running repmgr on
`postgresql.persistence.enabled=false` (emptyDir) is not recommended:
a container restart still rejoins correctly, but a pod recreation loses
the data dir, and if a standby was promoted in the meantime the node
refuses to initialize rather than fork a divergent cluster -- use
persistent volumes so pg_rewind/clone can repair it.

## 0.6.85

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

## Migrating from 0.6.84

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
The StatefulSet pod template changes (wrapped container command), so
PostgreSQL pods roll once. Behavior changes only in the stale-primary
scenario, which previously produced a silent split-brain.

## 0.6.84

### Fixed

- The prometheus exporter `/metrics` endpoint returned HTTP 500 on
  every scrape in the unpublished 0.6.75-0.6.83 versions: the #22
  custom query group was named `pg_replication`, colliding with the
  built-in replication collector's `pg_replication_lag_seconds`
  (the Prometheus registry rejects two metrics with the same name
  and different help text, failing the whole scrape). The group is
  now `pg_wal_replication` and no longer duplicates the built-in
  lag metric; the 0.6.75 notes were corrected in place.

## Migrating from 0.6.83

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
With the exporter enabled the configmap change rolls only the
exporter Deployment; database pods do not roll.

## 0.6.83

### Added

- PGPool troubleshooting guide at the end of the README (mirrored in
  pgvector): isolating connectivity failures between PGPool-II and the
  backends, checking backend status via `SHOW pool_nodes` and the pcp
  commands authenticated from the pgpool admin Secret, post-failover
  recovery including the `PrimaryChanged` Kubernetes Events emitted by
  the service-updater, readonly Service endpoint checks, and log
  locations with common messages (#25).

## Migrating from 0.6.82

Documentation only; no rendered resources change and no pods roll.

## 0.6.82

### Added

- Multi-Zone Deployment section in the README (#24): the built-in
  hostname and zone anti-affinity defaults, enforcing a hard zone
  requirement via `postgresql.affinity` (which replaces the built-in
  rules wholesale), spreading PGPool-II with
  `pgpool.topologySpreadConstraints`, `WaitForFirstConsumer` storage
  classes and the zonal volume pinning trade-off, and routing reads
  across zones through the `<fullname>-readonly` Service.

## Migrating from 0.6.81

Documentation only; no rendered resources change and no pods roll.

## 0.6.81

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

## Migrating from 0.6.80

`helm upgrade my-release` is the entire migration. No pods roll with default values: the service-updater ConfigMap is not checksummed into the StatefulSet pod template and the Role is patched in place. Running sidecars keep executing the already-loaded script, so PrimaryChanged events start appearing after each pod's next restart. No new values.

## 0.6.80

### Added

- Read-only replica Service `<fullname>-readonly` for routing read traffic to standbys (#17), rendered whenever repmgr is enabled. Service selectors are equality-only and cannot express "not the primary", so the service selects a new `pg-role: standby` pod label that the service-updater sidecar now converges on every postgresql pod each reconciliation tick (the resolved primary gets `pg-role: primary`, everything else `standby`; pods recreated or added by scale-up are picked up on the next tick). Pods without the label are never selected, so the primary can never leak into the readonly endpoints. The repmgr Role's pods rule gains `get`/`list`/`patch` alongside the existing `delete`.

## Migrating from 0.6.79

`helm upgrade my-release cagriekin/pgvector` is the entire migration; no pods roll with default values (the StatefulSet pod template is unchanged and the service-updater configmap is not checksummed into it). Because the running service-updater process does not re-read its script, pg-role labeling -- and therefore readonly endpoints -- only activates once the service-updater containers restart (next pod roll or container restart); until then, and with `postgresql.replicaCount: 0` permanently, the `<fullname>-readonly` Service exists but has no endpoints, which is the safe default (unlabeled pods are never selected, so reads can never hit the primary by accident). The RBAC change applies immediately.

## 0.6.79

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

## Migrating from 0.6.78

With default values (pgpool.enabled=false) nothing changes and no pods roll. With PGPool enabled, the pgpool Deployment rolls once on upgrade (pcp.conf left the ConfigMap, so the config checksum changes); PostgreSQL pods do not roll. pgpool.adminUsername/pgpool.adminPassword were renamed: anyone setting them must move to pgpool.admin.username/pgpool.admin.password or pgpool.admin.existingSecret — rendering fails fast with a pointer to the new keys until they do. PCP credentials themselves are unchanged for default installs (admin/admin, now stored in a Secret).

## 0.6.78

### Changed

- Extension paths are no longer hardcoded to PostgreSQL 18 (#18). The
  copy-base-ext/copy-ext init-container `cp` commands and the
  ext-lib/ext-share volumeMounts now derive
  `/usr/lib/postgresql/<major>/lib` and
  `/usr/share/postgresql/<major>/extension` from the new
  `postgresql.majorVersion` value (default `"18"`), validated via
  `required` when `postgresql.extensions.enabled=true`. Keep it in
  sync with `postgresql.image.tag` when running a different major.

## Migrating from 0.6.77

`helm upgrade my-release cagriekin/pgvector` is the entire migration. With default values nothing rolls: `postgresql.majorVersion` defaults to "18", so every rendered manifest is byte-identical to the previous release (the affected paths only render when `postgresql.extensions.enabled=true`, and even then they resolve to the same /18/ paths). Users running a non-18 image with extensions enabled should set `postgresql.majorVersion` to match their image's major version; leaving it empty now fails the render with a clear error.

## 0.6.77

### Added

- `repmgr.monitoringHistoryDays` (default `7`) bounds the
  `repmgr.monitoring_history` table (#19). repmgrd runs with
  `monitoring_history=true` but repmgr 5.x has no conf-based retention
  (the image's `monitoring_history_keep` line is silently ignored as an
  unknown parameter), so the table grew forever. The repmgrd sidecar now
  spawns a resilient background loop that once per day, on the primary
  only, runs `repmgr cluster cleanup --keep-history=<days>`; cleanup
  failures log a warning and never take down repmgrd.

## Migrating from 0.6.76

`helm upgrade my-release cagriekin/pgvector` is the entire migration. With repmgr enabled (the default) the StatefulSet pod template changes (new env var and startup script in the repmgrd sidecar), so the postgresql pods roll once via the normal rolling update; repmgr handles the failover as on any upgrade. The first prune of an existing oversized monitoring_history table happens within 24h of the new pods starting. With repmgr disabled nothing changes and no pods roll.

## 0.6.76

### Added

- Zone-aware pod anti-affinity on the postgresql StatefulSet (#16). The
  default affinity block now includes a preferred (soft) podAntiAffinity
  term on `topology.kubernetes.io/zone` (weight 100) alongside the
  existing required hostname term, so pods spread across availability
  zones when possible while hostname spreading stays mandatory.
  Single-zone clusters are unaffected (the zone rule is best-effort),
  and a user-supplied `postgresql.affinity` still replaces the default
  block wholesale.

## Migrating from 0.6.75

With default values the StatefulSet pod template changes (a new preferred zone anti-affinity term), so postgresql pods roll once on upgrade following the chart's update strategy. The new rule is preferred (soft): scheduling behavior only changes on multi-zone clusters where the scheduler will now favor spreading pods across zones; single-zone clusters schedule exactly as before. Releases that set postgresql.affinity are unaffected — their custom affinity still replaces the default block entirely. No values changes or manual action required.

## 0.6.75

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

## Migrating from 0.6.74

`helm upgrade my-release cagriekin/pgvector` is the entire migration. With the default `prometheusExporter.enabled=false` nothing is rendered and no pods roll. With the exporter enabled, the configmap change rolls only the exporter Deployment (via its checksum/config annotation); database pods do not roll and no values changes are required — the new metrics appear on the next scrape.

## 0.6.74

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

## Migrating from 0.6.73

`helm upgrade my-release cagriekin/pgvector` is the entire migration. No pods roll with default values; with `backup.enabled=true` only the backup ConfigMap changes, which the next CronJob run picks up. No values changes are required. The backup job now fails (exit 1) instead of deleting when no backup newer than retentionDays is visible under the configured prefix — a condition that previously resulted in silent total deletion.

## 0.6.73

### Changed

- Default `postgresql.livenessProbe.failureThreshold` raised from 6
  to 10 (#20). With the default `periodSeconds: 10` the kubelet now
  waits 100s of failed `pg_isready` checks before restarting
  PostgreSQL instead of 60s, so sustained heavy load no longer
  triggers false liveness restarts. The readiness probe defaults are
  unchanged.

## Migrating from 0.6.72

With default values the StatefulSet pod template changes (livenessProbe.failureThreshold 6 -> 10), so PostgreSQL pods WILL roll once on upgrade. No action is required; releases that already override postgresql.livenessProbe.failureThreshold in their own values are unaffected and do not roll because of this change.

## 0.6.72

### Added

- Complete Chart.yaml metadata (#114): `home`, `icon`, `sources` and
  `maintainers` alongside the existing `keywords`, shown by Artifact
  Hub and `helm show chart`.

## Migrating from 0.6.71

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
Metadata only; no rendered resources change and no pods roll.

## 0.6.71

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

## Migrating from 0.6.70

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
No pods roll. With `networkPolicy.enabled=false` (the default) nothing
changes; with it enabled the postgresql policy additionally allows
egress to 6443 and the pgBackRest S3 endpoint port.

## 0.6.70

### Added

- Per-component `priorityClassName` support (#112, shared templates
  with the pg chart): `postgresql.priorityClassName`,
  `pgpool.priorityClassName`, `prometheusExporter.priorityClassName`,
  `backup.priorityClassName` and
  `pgbackrest.cronjob.priorityClassName` (all default `""`).

## Migrating from 0.6.69

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
With the default empty values nothing is rendered and no pods roll.

## 0.6.69

### Added

- `imagePullSecrets` (top-level value, default `[]`) now propagates to
  every pod template (#111, shared templates with the pg chart): the
  PostgreSQL StatefulSet, the pgpool and prometheus exporter
  Deployments, and the backup and pgBackRest CronJobs.

## Migrating from 0.6.68

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
With the default `imagePullSecrets: []` nothing is rendered and no
pods roll.

## 0.6.68

### Fixed

- Credentials containing special characters no longer corrupt pgpool
  and exporter configuration (#108, shared templates with the pg
  chart): placeholder substitution is now a byte-safe awk splice with
  context-appropriate escaping, pgpool check passwords come from a
  `TEXT`-prefixed pool_passwd instead of pgpool.conf strings, and the
  exporter DSN is assembled with percent-encoded credentials in an
  init container instead of raw `$(VAR)` expansion.
- pgpool probes now run a query through pgpool instead of a TCP
  connect (#122), so a backends-down wedge fails liveness and
  self-heals, and the pgpool pod template carries a configmap
  checksum so config changes roll the pods. See the pg chart 0.5.66
  notes for details.

## Migrating from 0.6.67

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
The pgpool and exporter pod templates change, so those deployments
roll once; the PostgreSQL StatefulSet is untouched.

## 0.6.67

### Fixed

- Disabling `postgresql.configuration` and `pgbackrest` after they had
  been enabled bricked the cluster on the next pod restart (#107,
  shared template with the pg chart): the persisted `include_dir` line
  pointed at the removed conf.d mount. The setup-config init container
  now always runs and strips the stale line when both features are
  disabled.

## Migrating from 0.6.66

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
The StatefulSet pod template changes, so pods roll once. Clusters
already crash-looping from this defect are repaired by the upgrade.

## 0.6.66

### Fixed

- The postgresql preStop hook no longer attempts to promote a standby
  (#102, shared template with the pg chart); it only stops PostgreSQL
  cleanly and leaves promotion to repmgrd. The old remote
  `pg_promote()` never executed (silent auth failure), and making it
  work bypasses repmgr metadata and crash-loops every repmgrd; see the
  pg chart 0.5.64 notes.

## Migrating from 0.6.65

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
The StatefulSet pod template changes, so pods roll once. Primary
shutdown during the roll is now ~30s faster; failover behavior is
unchanged because the removed promotion never executed.

## 0.6.65

### Fixed

- Primary discovery and repmgrd pre-register fixes shared with the pg
  chart (#103, #104, #105): the postStart discovery loop now scans all
  `replicaCount + 1` ordinals instead of stopping one short, repmgrd
  role detection uses repmgr credentials instead of a hardcoded
  `psql -U postgres`, the peer scan bound derives from `replicaCount`
  instead of a hardcoded `seq 0 9`, and the type-backfill node id is
  read from the generated `repmgr.conf` instead of re-deriving the
  `ordinal + 1000` convention.

## Migrating from 0.6.64

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
The StatefulSet pod template changes, so pods roll once.

## 0.6.64

### Fixed

- `helm upgrade` no longer repoints the primary Service selector back
  to pod-0 (#109, shared template with the pg chart). The rendered
  Service preserves the live `statefulset.kubernetes.io/pod-name`
  selector via `lookup`, falling back to pod-0 only at bootstrap.
  Previously every upgrade after a failover routed writes at a
  read-only standby until the service-updater's next tick, and helm v4
  upgrades failed outright with a field-manager conflict on
  `.spec.selector`.

## Migrating from 0.6.63

`helm upgrade my-release cagriekin/pgvector` is the entire migration;
see the pg chart 0.5.62 notes for the helm v4 conflict details.

## 0.6.63

### Fixed

- Rendering now fails fast when `repmgr.enabled=false` is combined with
  `postgresql.replicaCount > 0` (#106, shared template with the pg
  chart). Without repmgr the extra StatefulSet pods were independent
  PostgreSQL instances with no replication, silently serving empty or
  diverged data through PGPool. Standalone mode requires
  `postgresql.replicaCount=0`.

## Migrating from 0.6.62

`helm upgrade my-release cagriekin/pgvector` is the entire migration
for repmgr deployments and single-instance standalone deployments.
Values combining `repmgr.enabled=false` with
`postgresql.replicaCount > 0` are now rejected at template time; see
the pg chart 0.5.61 migration notes for recovery guidance.

## 0.6.62

### Fixed

- 0.6.61 dropped the image entrypoint's `Waiting for local PostgreSQL`
  and `primary register --force` steps along with the broken standby
  verify, so primary pods crashed at boot. Restore both in the chart
  wrapper: wait for local PG via `pg_isready`, then branch on
  `pg_is_in_recovery()` — `f` runs `primary register --force`, `t`
  runs the existing standby pre-register block. Standby gate changed
  from `ORDINAL != 0` to `IN_RECOVERY = t` so a failed-over pod-0
  rejoining as standby also takes the standby path.

## Migrating from 0.6.61

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
No PVC recreate, no StatefulSet recreate, no password rotation, no
forced failover.

## 0.6.61

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

## Migrating from 0.6.60

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
No PVC recreate, no StatefulSet recreate, no password rotation, no
forced failover. Standby pods that were CrashLooping on repmgrd
converge on their next restart; unaffected clusters see no behaviour
change.

## 0.6.60

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

## Migrating from 0.6.59

`helm upgrade my-release cagriekin/pgvector` is the entire migration.
No PVC recreate, no StatefulSet recreate, no password rotation, no
forced failover. Affected clusters converge `type='standby'` on the
next standby restart; unaffected clusters see no behaviour change.

## 0.6.59

### Fixed

- `fix_user_auth` postStart hook (0.6.58) used
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

## Migrating from 0.6.58

`helm upgrade my-release cagriekin/pgvector` is the entire migration:
no PVC recreate, no StatefulSet recreate, no password rotation, no
forced failover, no new required `values.yaml` field, PG13 still
skipped. The MD5→SCRAM rehash completes on the first 0.6.59 pod start
(idempotent on re-run).
