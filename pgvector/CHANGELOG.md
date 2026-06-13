# pgvector chart changelog

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

`helm upgrade my-release cagriekin/pg` is the entire migration; no pods roll with default values (the StatefulSet pod template is unchanged and the service-updater configmap is not checksummed into it). Because the running service-updater process does not re-read its script, pg-role labeling -- and therefore readonly endpoints -- only activates once the service-updater containers restart (next pod roll or container restart); until then, and with `postgresql.replicaCount: 0` permanently, the `<fullname>-readonly` Service exists but has no endpoints, which is the safe default (unlabeled pods are never selected, so reads can never hit the primary by accident). The RBAC change applies immediately.

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

`helm upgrade my-release cagriekin/pg` is the entire migration. With default values nothing rolls: `postgresql.majorVersion` defaults to "18", so every rendered manifest is byte-identical to the previous release (the affected paths only render when `postgresql.extensions.enabled=true`, and even then they resolve to the same /18/ paths). Users running a non-18 image with extensions enabled should set `postgresql.majorVersion` to match their image's major version; leaving it empty now fails the render with a clear error.

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

`helm upgrade my-release cagriekin/pg` is the entire migration. With repmgr enabled (the default) the StatefulSet pod template changes (new env var and startup script in the repmgrd sidecar), so the postgresql pods roll once via the normal rolling update; repmgr handles the failover as on any upgrade. The first prune of an existing oversized monitoring_history table happens within 24h of the new pods starting. With repmgr disabled nothing changes and no pods roll.

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

`helm upgrade my-release cagriekin/pg` is the entire migration. With the default `prometheusExporter.enabled=false` nothing is rendered and no pods roll. With the exporter enabled, the configmap change rolls only the exporter Deployment (via its checksum/config annotation); database pods do not roll and no values changes are required — the new metrics appear on the next scrape.

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

`helm upgrade my-release cagriekin/pg` is the entire migration. No pods roll with default values; with `backup.enabled=true` only the backup ConfigMap changes, which the next CronJob run picks up. No values changes are required. The backup job now fails (exit 1) instead of deleting when no backup newer than retentionDays is visible under the configured prefix — a condition that previously resulted in silent total deletion.

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
