# pg chart changelog

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
