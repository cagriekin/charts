# pg chart changelog

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
