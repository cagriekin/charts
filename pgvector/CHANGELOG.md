# pgvector chart changelog

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
