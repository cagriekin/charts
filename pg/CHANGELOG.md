# pg chart changelog

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
