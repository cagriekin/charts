# pgvector chart changelog

## 0.6.59

### Fixed

- `fix_user_auth` postStart hook (0.6.58) embedded `:'u'` / `:'p'` inside a
  dollar-quoted DO block where psql substitution does not run, so the
  MD5→SCRAM rehash silently failed server-side and never executed. Values
  now flow through per-session GUCs read via `current_setting()`. PG<14
  skip, `format('%I/%L')` quoting, and `rolpassword LIKE 'md5%'`
  idempotency gate preserved.
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
