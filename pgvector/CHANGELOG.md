# pgvector chart changelog

## 0.6.59

### Fixed

- **`fix_user_auth` MD5→SCRAM migration was a silent no-op (regression from
  0.6.58 / PR #99).** The postStart hook embedded `:'u'` and `:'p'` inside a
  dollar-quoted `DO $$ … $$;` block. psql variable substitution is not
  performed inside dollar-quoted constants, so every invocation failed server
  side with `ERROR: syntax error at or near ":"` and the rehash never ran.
  Auth kept working only because the md5-fallback line that 0.6.58 writes
  into `pg_hba.conf` still accepted the legacy MD5 hashes. The migration now
  hoists the values into per-session GUCs in regular SQL and reads them back
  via `current_setting()` inside the DO block. `RAISE NOTICE` skip-on-PG<14
  branch, `format('%I … %L', …)` identifier/literal quoting, and the
  idempotent `rolpassword LIKE 'md5%'` gate are preserved unchanged.

- **Standby `repmgr.nodes` row landed with `type=''` on the primary (image-side
  bug, worked around in the chart).** The image's `repmgrd-entrypoint.sh`
  calls `repmgr standby register --force` without `--upstream-node-id`. On
  PG18 + repmgr 5.5.0 this leaves the standby's row on the primary with
  `type=''`, and the image's own verify-loop then trips with `ERROR: Primary
  does not show node N as standby (current type: )` and crashes the pod
  (~5-minute CrashLoopBackOff). The chart now wraps the `repmgrd` container
  command with a pre-register step that resolves the primary's `node_id` by
  polling sibling pods, runs `repmgr standby register --upstream-node-id=…
  --force`, and then `exec`s the image entrypoint, which becomes an
  idempotent no-op UPDATE. The wrapper only runs on standby pods
  (ordinal > 0); the primary pod path is unchanged.

  The upstream fix lives in `cagriekin/repmgr-docker`. Once a tagged image
  with the same fix is published, bump `repmgr.image.tag` accordingly and
  drop the wrapper (kept as a `command:` block; deletion is mechanical).

## Migrating from 0.6.58

`helm upgrade my-release cagriekin/pgvector` is the entire migration. No
operator action required, with these guarantees:

- **No PVC recreation.** StatefulSet `volumeClaimTemplates` is unchanged.
- **No StatefulSet recreation.** Only `template.spec.containers[].command`
  for the `repmgrd` container and inline postStart script content for the
  `postgresql` container change — both are in-place rollable fields.
- **No password rotation.** `postgres`, `repmgr`, and `pgpool` keep their
  existing plaintext passwords. Only their `pg_authid.rolpassword` hash type
  may flip from MD5 to SCRAM on PG14+ when
  `postgresql.migrateLegacyMd5Users: true` (the default, unchanged).
- **No forced failover.** Standby pods restart in-place; the primary pod is
  not touched first. Rolling restart respects `terminationGracePeriodSeconds`.
- **No new required `values.yaml` field.** Both fixes are unconditional on
  the code path that was already taking effect in 0.6.58.
- **PG13 is still skipped.** The `RAISE NOTICE 'Skipping md5->scram migration
  on PG < 14'` branch is preserved.

If you previously installed 0.6.58 and saw `ERROR: syntax error at or near
":"` in postgres logs, that line should disappear after the upgrade. The
managed users (`POSTGRES_USER`, `REPMGR_USER`) will then complete the
MD5→SCRAM rehash on the first pod start under 0.6.59 (idempotent — running
again is a silent no-op).
