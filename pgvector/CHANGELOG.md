# pgvector chart changelog

## 2.0.0 - unreleased

### Added

- **In-place migration of a live repmgr cluster to the native mechanism (#292).** `helm upgrade`
  from any 1.x release now migrates an existing cluster without a re-clone: timeline and system
  identifier preserved, no switchover, no `--cascade=orphan` recreate. At size this is the
  difference between a rolling restart and hours of degraded HA with a real RPO window.

  What happens to the repmgr state a 1.x cluster carries on disk: the
  `shared_preload_libraries = 'repmgr'` line inside PGDATA is stripped before the postmaster
  starts (#293 -- load-bearing, since the 2.0.0 image has no `repmgr.so` and the line survives a
  `helm rollback` because it lives in the data directory); `primary_conninfo` /
  `primary_slot_name` are cleared from `postgresql.auto.conf` so the agent's own fragment becomes
  authoritative; legacy `repmgr_slot_<node_id>` slots are left alone while streaming and dropped
  only once that standby has attached to its `pg_ha_slot_<ordinal>` replacement, under
  `AND NOT active`. The repmgr database, role and extension are **left in place** -- dropping
  them is irreversible and is what closes off rollback, so it stays an opt-in operator step.

  New runbook in `pg/README.md`: preconditions, the upgrade, per-node verification, the
  cluster-wide `pg-ha/pause` marker, rollback (including the one thing rollback does not undo --
  the preload strip), and the opt-in `DROP EXTENSION` snippet with an explicit warning against
  dropping the role or database, both of which the agent still depends on.

  Verified on a real cluster: 49/49, released 1.17.0 on trixie-5.5.0-32-pg18 (which still
  contains repmgr) upgraded in place to the local chart on the repmgr-free image.

  Every defect the live runs and review rounds surfaced was in the SUITE, not the chart -- the
  migration code was correct from the start and the work went into making the proof trustworthy:
  `pg_controldata` is not on `PATH` in the image (the server binaries live in the versioned
  bindir, which is why the agent has a `PGBindir()` helper), and "timeline unchanged" was the
  wrong assertion -- a rolling upgrade replaces the primary's pod, so the lease must move and
  whoever takes it promotes. The observed roll went TLI 1 -> 3, two handoffs, with all three
  nodes ending consistent. What the suite asserts now is that no node went BACKWARDS and that
  no node is stranded on an older timeline -- the failure that matters, since a standby that
  cannot follow eventually needs the very re-clone this avoids. No-rewind and no-re-clone stay
  covered by the system identifier, the sentinel and the `.diverged` check.

  Third: that stranding check must read `pg_stat_wal_receiver.received_tli`, not
  `pg_controldata`. "Latest checkpoint's TimeLineID" is as of the last CHECKPOINT, so a standby
  that has switched timelines but not yet checkpointed still reports the old one -- the check
  failed with "got: 1 2" on a healthy cluster where the lagging node read checkpoint_tli=2 while
  received_tli=4 and the primary listed it as streaming.


  Two assertions were vacuous in the case that mattered. The legacy-slot check ("no orphan
  pinning WAL") queried only the POST-migration primary -- but legacy `repmgr_slot_*` only ever
  existed on the 1.x primary, and the roll moves the lease, so it ran on a node that never had
  one and passed by construction while a demoted ex-primary could still hold inactive slots
  pinning WAL on its own volume. It now covers every node plus an explicitly named check on the
  1.x primary. And the stranding check read the primary from `pg_control_checkpoint()` while
  reading standbys from `received_tli` for the very reason the control file lags: a just-promoted
  primary has only REQUESTED its checkpoint, so it can report the old timeline and fail every
  standby on a healthy cluster. Both sides now use the `GREATEST()` of all three sources.

  The phase-1 preconditions polled nothing and raced: `repmgr.nodes` read 1 row of 3 on one run,
  because registration is asynchronous relative to pod READINESS (agent readiness is
  replication-aware, not registration-aware). All three now converge before asserting.

  CI wiring: the leg gets `timeout_minutes: 55` -- it installs a released chart, upgrades, rolls
  again, and the run step retries once, which cannot fit the shared 30-minute cap, and a timeout
  kills the required check with no diagnosis. The released from-image is pre-pulled on the runner
  for this leg only rather than added to `ci-base-images.txt`, which is bundled into every leg:
  that would ship ~200MB to all 44 legs to serve 2, the waste the per-major bundles exist to
  avoid. Left alone, each of the 3 KinD workers would pull it anonymously from Docker Hub
  mid-suite.

  The runbook's rollback section was rewritten against a tested rollback rather than a guess, and
  both of its claims turned out to be wrong:

  - **`helm rollback` needs `--force-conflicts` on any agent-mode release.** A plain rollback
    fails with a field-manager conflict on `Service.spec.selector`: the agent owns that field (it
    points the Service at the current primary) and Helm 4 applies server-side, so the two
    contend. `--force` / `--force-replace` is not the answer -- it is deprecated AND rejected
    outright alongside server-side apply. This is not specific to the migration; it applies to
    rolling back any agent-mode release, and it was previously only a comment in a test suite.
  - **Restoring `shared_preload_libraries` before rolling back is neither possible nor needed.**
    `ALTER SYSTEM` writes `postgresql.auto.conf`, which is inside PGDATA and which 2.0.0's agent
    strips on every boot -- so any restart of a still-2.0.0 pod silently reverted it. And it is
    unnecessary: a migrated 3-node cluster rolled back to 1.17.0 with the preload absent came up
    with all pods Ready, one primary, both standbys streaming, data intact, `repmgr.nodes`
    queryable and `repmgr cluster show` correct. The GUC was only ever required by repmgrd, the
    daemon 2.0.0 removed -- which incidentally answers the question #293 left open.

  The runbook had four defects that would have misled an operator: bare `pg_controldata` (not on
  PATH -- the image keeps server binaries in the versioned bindir), `<fullname>-0` hardcoded as
  the primary in five places (the primary is not ordinal-bound, so the checks would have reported
  a false clean bill), the rollback steps ordered against their own "do this BEFORE rolling back"
  instruction, and a bare `ALTER SYSTEM SET shared_preload_libraries = 'repmgr'` that silently
  drops `pgaudit` (auto.conf is read after the chart's fragment and overrides it).

  New KinD suite `test-migrate-native` (`make -C pg test-migrate-native`, and a matrix leg on
  both majors): installs the released 1.17.0 chart on an older released image that still contains
  repmgr, proves the starting state really is repmgr-shaped, then upgrades to the local chart and
  asserts no re-clone, an unchanged system identifier, no node stranded on an older timeline, data intact, every standby
  streaming, native slots active, no legacy slot left pinning WAL, the catalog untouched, and
  that a second roll is a no-op. "No re-clone" is proved by a sentinel file written into each
  PGDATA -- `pg_basebackup` wipes the target directory, so a surviving sentinel is direct
  evidence rather than an inference from logs or timing.

### Fixed

- **The md5→scram re-hash no longer sends a plaintext password to the server (#298 review).**
  It already kept the secret off argv, but it set it as a SQL literal
  (`SET myvars.tgt_pass = ...`), and a top-level `SET` is logged verbatim under
  `log_statement = 'all'` or `log_min_duration_statement = 0` -- so any cluster with statement
  logging on wrote the superuser and replication passwords into the PostgreSQL log in cleartext
  and shipped them wherever those logs go. The agent now computes the SCRAM-SHA-256 verifier
  itself and sends only that, which PostgreSQL stores verbatim, so the plaintext never leaves
  the agent process. The verifier builder is asserted against the RFC 7677 known-answer vector.
  As defence in depth the session also lowers `log_statement` / `log_min_duration_statement` /
  `log_min_error_statement` before anything carrying the verifier is sent.
- **Dead template branches removed: `repmgr-preload.conf` could not render (#298 review).**
  `pg.chartOwnsSharedPreloadLibraries` still carried a `mechanism != native` clause, and no
  render can satisfy it -- `repmgr` survives in the schema enum only so the removed-value
  validator can reject it by name. The clause was therefore unreachable, and so was the whole
  `repmgr-preload.conf` block, whose guard reduced to `not X and X`. The `repmgr` prepend in
  `pg.sharedPreloadLibraries` is deleted for the same reason. No render moves; the tests
  asserting the file is absent were passing vacuously and now pass for the right reason.
- **`postgres` mode sealed in a torn bootstrap forever (#298 review).** `bootstrap_initdb`
  no-ops on any PGDATA that already carries `PG_VERSION`, so an initdb that ran and then died
  before writing its completion sentinel -- the function's own FATAL verification exit, a
  SIGKILL mid-bootstrap, a container lost between the two -- left a cluster with no application
  role and no application database that every later start served happily. Agent mode already
  recovered this (`discardTornInitdb`); nothing in `postgres` mode read the sentinel. It now
  mirrors the agent's rule exactly, including the safety property that matters most: the
  IN-PROGRESS marker is required evidence, so a data directory created by an older image
  (`PG_VERSION`, no sentinel, no marker) is never touched -- absence of proof that a bootstrap
  finished is not proof that it did not. Only affects running the image directly; the chart
  never reaches this branch.
- **The bootstrap `pg_hba.conf` no longer offers `md5` to the network (#298 review).** Both
  `0.0.0.0/0` catch-all rules now require `scram-sha-256`. `bootstrap_initdb` runs only against an
  EMPTY data directory and creates every role under `password_encryption = 'scram-sha-256'`, so
  there was no md5-hashed role for an md5 rule to serve -- and PostgreSQL negotiates SCRAM anyway
  once the stored secret is a verifier, so the rule bought no compatibility while advertising a
  deprecated method. Matters most for `postgres` mode (running the image directly, no agent): in
  agent mode the file is overwritten by the agent's own hardened `pg_hba` before the first real
  start, which is exactly why that mode was never the one at risk. Clusters initdb'd by chart 1.x
  are unaffected -- their md5-first compatibility rules are the agent's to write, and it still
  does.
- **A stranger's `repmgr_slot_*` could be reclaimed as a departed node (#298 review).** The
  legacy slot-name to ordinal mapping was bounded below but not above, so an operator's own
  hand-made `repmgr_slot_9999` mapped to ordinal 8999 -- an ordinal no pod can ever hold, which
  therefore read as DEPARTED and made the slot droppable. Now bounded at both ends: a number is
  only this chart's `node_id` while `node_id - 1000` stays under 1000, past which the ids would
  run into a second base range and stop being unique.
- **A blackholed upstream could get the agent liveness-killed.** `pg_basebackup`, `pg_rewind` and
  the slot-create `psql` addressed their peer with `-h/-p/-U`, which carries no `connect_timeout`,
  and libpq's default is unlimited -- so a dead node whose pod had not been evicted (address still
  resolving, nothing answering) held the reconcile goroutine for the kernel's ~127s of SYN retries
  with `opMu` taken and no heartbeat. `/healthz` goes stale after `reconcileInterval*3`, so the
  kubelet SIGKILLed an agent that was PID 1 over a healthy postmaster, and repeated for as long as
  the partition lasted. All three now carry `PGCONNECT_TIMEOUT` from the peer's own
  `ConnectTimeout`, and the slot-create query additionally has a 30s total deadline for a server
  that connects and then never answers. `pg_basebackup` deliberately keeps no total deadline: a
  large base backup legitimately runs for hours.
- **A transient `pg_rewind` failure re-cloned a healthy standby.** Divergence was inferred by
  exclusion -- anything not on an 8-entry connection-failure whitelist became
  `ErrRewindDiverged` -- so a rotated password, a missing `pg_hba` entry, "the database system is
  starting up", an exhausted connection pool or a `restore_command` error moved `PGDATA` aside and
  re-ran a full base backup on a node whose history was fine, leaving another `.diverged.<ts>`
  copy each time. Divergence is now detected positively (pg_rewind's own "could not find common
  ancestor of the source and target cluster's timelines") and everything unrecognised is retried
  instead of escalated: a genuinely stuck node stays behind and logs pg_rewind's message verbatim,
  which is the recoverable side of the trade.
- **`pg_rewind` had never actually run in native mode without pgBackRest, so every graceful
  failover paid a full base backup.** `RejoinForceRewind` passed `--restore-target-wal`
  unconditionally, and that is a request pg_rewind refuses outright -- before doing any work --
  when the target has no `restore_command`, which the chart sets only when pgbackrest is enabled:
  `pg_rewind: error: "restore_command" is not set in the target cluster`. The old classifier read
  that refusal as divergence, so the caller "recovered" by re-cloning the entire node, and the
  rewind path looked healthy while never executing. Found live in the failover suite once the
  classification above stopped hiding it. pg_rewind is now retried without the flag when it
  reports exactly that, using its own diagnostic rather than a chart value as the authority --
  so it stays right if `restore_command` appears or disappears later. Measured on KinD: an
  ex-primary now rejoins in ONE reconcile tick instead of a full `pg_basebackup`.
- A non-divergence `pg_rewind` failure that never clears no longer wedges a node out of the
  cluster: after three consecutive such failures against the same target, `rejoinOnto` escalates
  to the data-preserving re-clone. Fail-safe classification needs this backstop -- retry the
  cheap thing a few times, then pay for the expensive one that always converges. The streak is
  per-target and now also clears on a SUCCESSFUL rewind: without that, two failures followed by
  a success left the counter armed, so the next unrelated blip against the same primary read as
  the third consecutive failure and bought a full re-clone for a transient error.
- **`standby.signal` is now written before the fallible steps of a follow.** A slot-create blip
  after a COMPLETED multi-hour clone left the directory in primary shape (the source's
  "in production" `pg_control`, no `standby.signal`), which the next tick read as a diverged
  ex-primary: `pg_rewind` refuses a target that was not shut down cleanly, so the finished clone
  was moved aside and the entire base backup re-run. Ordering it first is also the safer failure
  in the other direction -- `standby.signal` without `primary_conninfo` waits for WAL, whereas
  `primary_conninfo` without `standby.signal` starts a second read-write primary.
- **A `Wait` tick leaked a WAL-pinning replication slot in cascade topologies.** `act()` cleared
  the follow latch on every action other than `Follow`, including `Wait` and `NoOp` -- both of
  which are "observe again, touch nothing", and one of which Decide labels "keep the current
  upstream". A single no-leader `Wait` (routine after `ReleaseOnCancel` empties the Lease) blanked
  the former upstream, so the next `Follow` could not drop this node's slot on the intermediate it
  had left, and the inactive slot pinned WAL there until `max_slot_wal_keep_size` invalidated it.
  The same field carries `cascadeFollowTarget`'s anti-thrash stickiness, which lost its hysteresis
  the same way. `Wait` and `NoOp` no longer clear it.
- **An uppercase character in a user or database name made the bootstrap unpassable.** The
  `CREATE USER`/`CREATE DATABASE`/`GRANT` statements used unquoted identifiers, which PostgreSQL
  folds to lower case, while #294's verification step compared `pg_authid.rolname` against the raw
  env value -- so `POSTGRES_USER=MyApp` created `myapp`, failed verification, exited before the
  completion sentinel, and the agent discarded and re-bootstrapped the fresh directory forever
  (with a FATAL hint that blamed password quoting). Folding was wrong on its own terms too: libpq
  sends `-U MyApp` verbatim and the server compares it exactly. Identifiers are now quoted (with
  embedded quotes doubled) and the verification queries use single-quote-escaped literals. 1.x had
  the same unquoted statements but no verification, so this was a 2.0.0 regression.
- **`mechanism.OSRunner` now sets `WaitDelay`,** matching `pg.OSExec`. Without it a cancelled
  command's grandchild holding the output pipe (`pg_basebackup -X stream` forks exactly such a WAL
  receiver) blocked `Wait` forever with `opMu` held, starving `dcs.OnLost`'s demote until the
  kubelet's SIGKILL -- the #288 fencing hazard, on the one exec path that had not been given the
  fix.
- **Render-time guards for three upgrade traps** (invariant 4). A 1.x `trixie-*` HA image tag is
  now rejected: chart 2.0.0's `repmgr-init` passes only `PG_MAJOR`, while a 1.x image's
  `entrypoint.sh init` hard-fails on the unset `HEADLESS_SERVICE`/`REPMGR_PASSWORD`, so every pod
  went `Init:CrashLoopBackOff` after an upgrade helm accepted silently. `pg.validateLeaseTimings`
  now also enforces client-go's `renewDeadline > 1.2 x retryPeriod` jitter bound, which the plain
  ordering rule does not imply (15s/12s/10s rendered clean and then refused to boot on every pod).
  And that whole validator is now HA-only, matching its siblings: standalone renders no agent, so
  a leftover `ha.agent.*` timing there -- a bare int from a 1.x file, say -- no longer blocks the
  install.
- **The `#182` no-thrash regression assertion was vacuous.** It grepped the agent log for
  `repmgr standby follow:`, a string that survives only in Go comments and is emitted by nothing,
  so the check could never fail and per-tick follow churn went unguarded. It now samples the
  standby's walsender `backend_start` across ~5 steady-state ticks -- mechanism-independent, and
  guarded by a non-empty assertion so it cannot go vacuous again.

- `pg/tests/set-pg-major.sh` normalized deliberate older-release image pins (`set-pg-major: keep`)
  asymmetrically: untouched on the default major, but rewritten to the freshly-built tag on a
  non-default one, on the premise that "a non-default major has no older PUBLISHED image". That
  premise expired with #269's per-major publishing -- every release from `trixie-5.5.0-29` onward
  exists as both `-pg17` and `-pg18` -- so the rule destroyed the coverage it was protecting: on a
  PG17 leg the migration suite would have started from the repmgr-FREE image, making its "install
  a 1.x cluster" phase not a repmgr cluster at all. Keep-marked pins now keep their base and move
  only the major suffix, on every major, which is idempotent and needs no staleness check.

### Changed (breaking)

- **The `repmgr:` values block is now `ha:`** (#291). Nothing nested moved -- only the block's
  own name. Every `repmgr.*` key still works for the whole 2.0.0 line: `pg.normalizeValues`
  merges the `repmgr:` block over the `ha:` defaults key by key, so an untouched 1.x values
  file installs unchanged, `--set repmgr.agent.leaseDuration=20s` still lands, and a file
  mixing the two spellings resolves per key with the `repmgr.*` value winning -- it is the one
  the operator set; the `ha.*` side is chart defaults. Both spellings are schema-validated
  (the `repmgr` schema is generated from the `ha` shape and asserted identical), so a bad enum
  or a wrong type still fails the render whichever name is used. `helm upgrade` prints a
  deprecation notice when it sees the old block.

  Marked breaking because the values API's canonical name changed, and because **the alias is
  removed in the next major** -- rename the top-level key before then. It is a rename and not
  a break *today*, which is the point: 2.0.0 already carries one real break (repmgrd removal)
  and stacking a mandatory values edit on top of it buys nothing.

  Why rename at all: after #290 the image contains no repmgr -- no binary, no extension, no
  `repmgr.conf` -- and the agent replicates through `pg_stat_replication` and slots it owns.
  A block named `repmgr` was sizing the resources of, and holding the credentials for,
  something no longer installed.

  Three names keep the word on purpose, because they identify real PostgreSQL objects rather
  than the tool: `ha.username`, `ha.database`, and the `repmgr-password` Secret key. Renaming
  those rewrites a live cluster's role and credential, which is a data-plane migration, not a
  values rename -- deliberately out of scope here. `REPMGR_*` env vars are likewise unchanged;
  `pg/ENVIRONMENT.md` records which of them the agent still reads and which are now inert.

  Review follow-ups on the rename itself, each a defect the first pass shipped:

  - The removed-key guards (`failoverMode`, `serviceUpdater.*`, `monitoringHistoryDays`) now
    fire under **both** spellings. Reading only `repmgr.*` made them skippable by taking the
    very rename the upgrade notice recommends: a 1.x file carrying `failoverMode: repmgrd`,
    with its block renamed to `ha:`, rendered clean and deployed an agent cluster to an
    operator who believed repmgrd was running -- straight into the immutable
    `podManagementPolicy` trap the guard exists to warn about.
  - Three templates (`networkpolicy.yaml`, `service-headless.yaml`, `databases-roles-job.yaml`)
    read `.Values.ha` transitively without normalizing first. In a full render this worked only
    because `statefulset.yaml` sorts earlier and mutates `.Values` for every later file; rendered
    alone, as helm-unittest does, they ignored the alias entirely.
  - `set-pg-major.sh` and the pg-test workflow both resolved the HA image from `pg/values.yaml`
    by anchoring on `^repmgr:`, and hard-exit when that misses. Both accept either spelling now.
    The same scripts hardcoded the image *repository*, so the #290 rename would have ended in
    `FATAL: rendered StatefulSet does not use cagriekin/repmgr:...`; the repository is read from
    the chart and rewritten in fixtures alongside the tag, and the two-step is verified end to
    end under both majors.
  - `etcd.rbac.bootstrapImage` pinned only the tag, leaving the repository on the etcd subchart's
    default. Harmless while both said `cagriekin/repmgr`; under the new tag scheme it names a
    coordinate that cannot exist. Both halves are pinned now.

  Second review round, all documentation/behavioural-surprise rather than logic:

  - **`repmgr.*` beats `ha.*` from ANY source, including `--set`** -- Helm collapses defaults,
    every `-f` and every `--set` into one map before the chart runs, so the merge cannot tell
    an operator's value from a chart default and always prefers the deprecated spelling. So
    `-f legacy.yaml --set ha.agent.leaseDuration=30s` renders the legacy value, discarding a
    `--set` that Helm normally ranks highest. This cannot be caught at render time (a template
    gets no provenance, so a `fail` would fire on every legitimate alias use or none), so it is
    documented in the README, NOTES.txt and both values.yaml, with the instruction stated as
    MOVE a key rather than duplicate it -- and pinned by tests, so flipping the merge direction
    fails loudly instead of silently changing what a released values file resolves to.
  - The README rename runbook gave `helm get values` without `-o yaml`, the exact command
    NOTES.txt warns against (the default output prefixes `USER-SUPPLIED VALUES:`, which the
    deliberately-open `additionalProperties` then accepts in silence).
  - NOTES.txt is byte-identical across both charts, so its cross-reference now names the pg
    chart README explicitly; the section it points at does not exist in pgvector's.
  - The tag-scheme comment tables in `set-pg-major.sh` and the pg-test workflow described
    `trixie-pg<major>-<n>` as the new scheme, which the classifier below them rejects -- it
    keys on a leading semver, so that shape would fall to the legacy arm and reduce to bare
    `trixie`. Not live breakage, but exactly the desync those comments exist to prevent.
  - Both values.yaml files still instructed the deprecated spelling in their own how-to prose
    (the etcd DCS walkthrough among them), so following the file the chart treats as the
    authority on its input surface tripped the deprecation notice the same change adds.

  Third review round:

  - **`ha.agent` lease timings are now validated at render time.** client-go requires
    `leaseDuration > renewDeadline > retryPeriod` and the agent refuses to start otherwise, but
    nothing checked it before the API server, so a violating triple rendered cleanly and then
    CrashLoopBackOff'd every postgresql pod at once with no primary. The reachable path is not
    three bad numbers typed by hand -- it is MIXING the `repmgr.agent.*` alias with `ha.agent.*`
    across two values files: these keys are cross-validated, so one timing from a 1.x file plus
    two from a newer `-f` yields a triple neither file contains. The chart's own
    `values-cloud.yaml` was the most reachable instance. The guard skips (rather than fails) a
    duration shape it cannot parse, because `time.ParseDuration` accepts compound forms and
    rejecting input the agent would have accepted would be a new bug, not a guard.
  - `release.yaml` now excludes `pg-ha-*`. The new image tag `pg-ha-X.Y.Z` matches the chart
    release glob, which the old dot-free `pg-ha-<n>` did not -- that is why only `trixie-*` was
    excluded. Pushing `pg-ha-2.0.0` would have fired the chart release workflow too, which
    resolves `chart="pg-ha"` and dies on the missing Chart.yaml.
  - `set-pg-major.sh` derives the leftover scanners' repository alternation from the chart
    instead of hardcoding the two Docker Hub names, so pointing `ha.image.repository` at a
    mirror or fork cannot make the scanners match nothing and report a silent green.

  Fourth review round, all on the lease guard added in the third:

  - **An unparseable duration skipped validation instead of failing it.** `ha.agent.leaseDuration:
    15` or `"20 s"` is not a Go duration -- `time.ParseDuration` rejects it, so `config.Load`
    fail-fasts and every pod CrashLoopBackOffs -- but the guard's parser returned "" for it and
    the caller shrugged, reproducing the exact render-clean/apply-broken outcome the guard was
    added to close. `""` now means "not a duration" and is a hard failure; the parser also
    handles the COMPOUND forms it previously skipped (`1m30s`, `2h45m`) by summing their pairs,
    so nothing valid is rejected and nothing invalid is waved through. The doc comment claiming
    `1.5h` was unparseable was wrong (the optional fraction already matched it), which meant one
    test was passing on the ordering result rather than on the skip it claimed to assert.
  - **The etcd DCS lease floor is now checked at render time.** `config.go` requires
    `LEASE_DURATION >= 5s` under `ha.agent.dcs.backend: etcd` because the lease TTL is whole
    seconds; a triple like 3s/2s/1s satisfies the ordering, rendered clean, and refused to boot.
  - `values-cloud.yaml` gained a layering note. Because `repmgr.*` wins key by key, an operator
    whose own file still spells the block `repmgr:` and sets any of the three timings gets a
    mixture when they add `-f values-cloud.yaml` -- now rejected loudly with the fix named,
    where before this PR it silently resolved to the overlay's values. The overlay is
    deliberately NOT dual-spelled: shipping the deprecated block in an example would trip the
    very notice it should demonstrate the absence of.
  - `set-pg-major.sh`'s repository-rewrite rule now uses the derived alternation like the
    scanners do, so a fork or mirror that retargeted values.yaml and its fixtures no longer hits
    `rule 'HA image repository' matched nothing`. Verified against a simulated private registry.

  Not changed: the `mechanism != native` branches in `pg.chartOwnsSharedPreloadLibraries` and
  `pg.sharedPreloadLibraries`. They are unreachable in a real render for the same reason as the
  agent branch this PR deleted, but the two are not equivalent -- the agent branch emitted
  *wrong remediation advice*, which is worse than dead, while these two produce correct behaviour
  for a state that can no longer occur. Deleting them would touch the subtle #262/#293 preload
  logic for no behavioural gain.

  Fifth review round -- the duration guard diverged from time.ParseDuration in BOTH directions,
  and each direction was its own bug:

  - **Too permissive (three holes, each reproducing the outage the guard exists to prevent).**
    The parser lower-cased its input, so `15S` and `1M30S` passed -- Go's units are
    case-SENSITIVE and both are errors. `| default "15s"` collapsed an explicitly empty value
    into the default, so an empty `leaseDuration` validated as 15s while the StatefulSet emitted
    `value: ""`, which Go also rejects. And `reconcileInterval` was not checked at all, though
    `config.Load` parses it with the same helper. All three rendered cleanly and then refused the
    agent's boot on every pod.
  - **Too strict (turning working 1.x values into upgrade failures).** `.5s`, `5.s` and `+15s`
    are valid Go durations and were rejected, as was microseconds spelled U+03BC. The grammar now
    mirrors `time.ParseDuration`: optional leading sign, one or more `<number><unit>` pairs with
    either side of the decimal point omissible, and all three microsecond spellings.
  - **`etcd.rbac.bootstrapImage` must now match `ha.image` at render time.** The bundled etcd's
    RBAC-bootstrap Job runs `pg-ha-agent rbac-bootstrap`, so a mismatch has one agent build
    writing the etcd RBAC that a different build then authenticates against. This change pinned
    both halves by hand but nothing enforced the lockstep, so the next image bump would have
    silently reintroduced the drift. Adopting a new image is therefore a FOUR-key edit per chart,
    now stated in `images/pg-ha/README.md` and enforced by `pg.validateEtcdBootstrapImage`.
    Only checked when the bundled etcd is enabled -- with an external etcd the Job never renders.

- **The HA image is versioned with the chart** (#290/#291). Tags are now
  `cagriekin/pg-ha:<chart-version>-pg<major>` (e.g. `2.0.0-pg18`, `2.0.0-pg17`), published from
  the git tag `pg-ha-<version>`, replacing `cagriekin/repmgr:trixie-<repmgr>-<n>`. The old
  scheme was keyed on a repmgr version the image no longer contains. The version an image
  carries is the chart version it shipped with; a chart-only patch does not force an image
  rebuild, so the two can legitimately differ by a patch. `cagriekin/repmgr` stays published
  and frozen at its last tag, so existing pins keep resolving. There is no unsuffixed
  "default major" alias -- a pin names its major, and `ha.image.majorVersion` cross-checks it
  at render time. The default pin is `cagriekin/pg-ha:2.0.0-pg18` (`ha.image` and
  `etcd.rbac.bootstrapImage` moved together), published from git tag `pg-ha-2.0.0`.

### Added

- **Topology from `pg_stat_replication`; `repmgr.nodes` retired in native mode (#288).**
  Inherited from pg's symlinked templates and shared agent. `native` can now run a real
  multi-node cluster: the init container no longer polls `repmgr.nodes` (which nothing writes
  under native, so every standby used to sit in `Init:CrashLoopBackOff`), the lease holder
  `initdb`s and the rest clone via `pg_basebackup`, and a native cluster carries no repmgr
  extension at all. Still EXPERIMENTAL.

  Four of #288's changes also reach `mechanism: repmgr` installs, i.e. every upgrade on the
  default path: restore provenance became a failover tiebreaker ranked above LSN; a standby with
  no walreceiver and a frozen replay position is now rewound or re-cloned automatically after ~3
  minutes; the postStart hook retries for up to 20s instead of skipping `additionalCommands`
  silently (which on this chart is `CREATE EXTENSION vector`); and the restore record gained
  `restoredAt` / `adoptedAt`. See the [pg 2.0.0 changelog](../pg/CHANGELOG.md) for the detail.

### Removed (breaking)

- **Removed the provably-inert `#297` promote gate.** It read `repmgr.nodes` to refuse promoting a
  node no survivor could `repmgr standby follow`; nothing populated its inputs once #294 deleted
  the repmgr mechanism, so it was unreachable code claiming to guard a promotion path (with eight
  test cases asserting behaviour on inputs no observer could produce). Native follows by conninfo,
  so a native primary is followable the moment it promotes. `Prober.RegisteredNodeIDs`,
  `Observation.RegistryRead`/`LocalRegistered` and `PeerState.Registered` went with it.
- **Removed `ha.splitBrainDetection` (and the `repmgr.splitBrainDetection` alias).** The
  `log`/`fence` choice selected between behaviours that were already identical: the code that
  distinguished them was the service-updater's `handle_split_brain()`, deleted with repmgrd
  (#286), and the agent always demotes itself when it is read-write without holding the lease.
  The key, the `SPLIT_BRAIN_ACTION` env it rendered, and the agent's dead validation of it are
  gone; a values file still setting either spelling fails at render time with a message naming
  the fix. The protection itself is unchanged -- the demote is unconditional.
- **`native` is now the only replication mechanism, and the default** (#294). The `repmgr`
  mechanism -- which shelled out to the repmgr CLI for `standby clone`, `standby follow`,
  `standby promote` and `node rejoin`, and depended on the `repmgr.nodes` table -- is gone.

  `repmgr.agent.mechanism` was introduced in this same unreleased major (#287) and was never
  published, so **no released values file can be pinning it**. The key survives rather than
  being deleted, so a stale `repmgr` copied from a pre-release branch fails the render with a
  message naming the migration instead of being silently ignored, and so the `Mechanism` seam
  stays addressable if a second implementation is ever wanted.

  **This changes what a fresh install does.** The agent now drives PostgreSQL directly:
  `pg_ctl promote`, `pg_basebackup`, `pg_rewind`, and `primary_conninfo` + `standby.signal`.
  Topology comes from `pg_stat_replication` rather than a self-reported catalog, the bootstrap
  is the agent's (the lease holder runs `initdb`; every other pod waits, then clones through
  its own pre-created replication slot), and the agent owns physical slot lifecycle. Policy is
  untouched: the Lease is still the sole authority for who is primary, and the timeline/LSN
  election, fencing and Service routing are unchanged.

  A native cluster has no `repmgr` extension and no `repmgr.nodes` at all. The `repmgr`
  database and role remain, because the agent authenticates as that role for replication;
  renaming them is #291.

  **An existing 1.x cluster is still repmgr-shaped on disk and cannot be flipped in place
  yet** (#292). Until that ships, `native` is for fresh installs — read the upgrade note
  before upgrading a live HA cluster.

  Removed with the mechanism: the `#297` promote-registration gate (its premise was a repmgr
  metadata requirement), the `#139` `repmgr.nodes` ghost-row cleanup (there is no registry to
  strand rows in — the *slot* residue a scale-down leaves is still reclaimed), `RegisterPrimary`
  / `RegisterStandby` / `Unregister` from the `Mechanism` interface, the reclone-on-missing-
  local-record escalation (`repmgr standby follow` needed a row it could not obtain without
  replication; native has no equivalent deadlock), and the node-id offset's last propagating
  consumers. The offset itself stays, for exactly one reader: reclaiming
  `repmgr_slot_<node_id>` orphans a repmgr-created cluster leaves behind.

  `pg_ha_agent_replicas_expected` is now measured on every install rather than only under
  `native`; its help text changed accordingly.

  CI drops the mechanism axis with it: 21 suites x 2 majors = 42 legs, no exclusions, down
  from 62 with 11.

- **`repmgr.failoverMode: repmgrd` and the repmgrd + service-updater sidecars are gone**
  (#286). The lease-based Go agent has been the default since `1.0.0` and `values.yaml`
  marked repmgrd deprecated "for one major cycle"; that cycle is over. The agent is now
  the only failover path.

  What this removes: the `repmgrd` container, the `service-updater` container and the
  ConfigMap carrying its script, `repmgrd-entrypoint.sh` and `service-updater.sh` from the
  image, kubectl from the image (the service-updater was its only consumer — the pgbackrest
  CronJobs run kubectl from their own `alpine/k8s` image), and `shareProcessNamespace` from
  the pod (it existed so repmgrd could signal postgres across containers).

  The Role loses two grants with them. `pods` **`delete`** is gone entirely — it was only
  ever granted under `splitBrainDetection.action=fence` for the service-updater's
  split-brain net (#154), and the agent soft-fences locally via `pg_ctl`; `fence` still
  works and still needs no pod-delete privilege. The pgpool `deployments` get/patch grant
  is gone too: the service-updater restarted pgpool on failover, whereas the agent
  re-points the Services that pgpool already targets. The `events` create grant follows —
  the agent records failover decisions in a structured audit log, so the `PrimaryChanged`
  core/v1 Events are no longer emitted.

  The postgresql **NetworkPolicy** loses its egress rule to pgpool's backend port (9999).
  It existed only so the service-updater could health-check pgpool from the database pods
  (#129); nothing in those pods connects to pgpool now, and traffic only flows the other
  way. The pgpool policy's own ingress is unchanged.

  **Removed values.** Each is rejected at render time with a message naming the fix, so a
  stale values file fails the upgrade instead of silently deploying something else:

  | Removed | Why |
  |---------|-----|
  | `repmgr.failoverMode` | Only one failover path remains |
  | `repmgr.serviceUpdater.*` | The sidecar it sized no longer exists |
  | `repmgr.monitoringHistoryDays` | Pruned `repmgr.monitoring_history`, which only repmgrd wrote |
  | `pgpool.autoFailback` | Rendered PGPool's `auto_failback`, which only applied to the repmgrd failover flow |

### Fixed

- **`shared_preload_libraries = 'repmgr'` is now removed from an existing data directory
  under `repmgr.agent.mechanism: native`** (#293), and a node whose configuration asks for a
  library this image does not ship refuses to start with a message naming the migration.

  The line is appended to `$PGDATA/postgresql.conf` by the image entrypoint at initdb time
  and then cloned verbatim to every standby, so **every cluster any 1.x chart installed
  carries it inside the data directory**. That matters because `shared_preload_libraries` is
  a postmaster parameter living on the PVC, not in the release: when the repmgr-free image
  arrives (#290) a cluster still requesting `repmgr.so` does not degrade, it refuses to
  start — on every pod simultaneously — and `helm rollback` cannot fix it, because the
  offending line is not in anything Helm owns. The removal therefore has to ship, and take
  effect, a release *before* the image that makes it mandatory.

  The agent now strips `repmgr` from that list on every boot, before it starts the
  postmaster, preserving any other libraries and their load order and dropping the
  assignment entirely when `repmgr` was the only entry. It converges rather than requiring
  orchestration: a standby cloned from a not-yet-cleaned source is cleaned on its own next
  restart, and a cluster that skips this release and jumps straight to the repmgr-free image
  is still cleaned before its first start.

  **Only native nodes are touched, deliberately.** A cluster that can run the repmgr-free
  image is native by definition (#294 removes the repmgr mechanism), so cleaning native
  nodes is sufficient — while a node still on `mechanism: repmgr` keeps its preload, because
  the repmgr extension's own functions are what would be at stake there and nothing is
  gained by removing it. Verified on a live cluster: the preload is a repmgrd requirement,
  and every repmgr verb the agent drives (`standby clone`, `standby follow`, `standby
  promote`, `primary register`) works without it — but native-only makes that finding moot
  rather than load-bearing, which is the safer place for it to be.

  Two guards come with it. `postgresql.configuration.shared_preload_libraries` containing
  `repmgr` under `mechanism: native` is now **refused at render time**: that value reaches
  the postmaster through `conf.d`'s `include_dir`, so it overrides the image's own native
  gate and would put the library back on a cluster that has nothing to use it. And if any
  configuration still requests `repmgr` while `repmgr.so` is genuinely absent, the agent
  fails startup naming this migration step instead of leaving PostgreSQL to emit
  `could not access file "repmgr"` in an unexplained crash-loop.

  `wal_log_hints`, `max_replication_slots` and `max_slot_wal_keep_size` — written by the
  same initdb block — are untouched: `pg_rewind` needs the first and agent-owned slots
  (#289) build on the others.

  Also under `mechanism: native`, an operator-declared `shared_preload_libraries` now passes
  through `custom.conf` instead of being re-emitted from `repmgr-preload.conf`. That file
  exists solely to preserve `repmgr` across an operator value, which native has nothing to
  preserve. The default (repmgr) render is unchanged.

- **A scale-up could wedge the cluster with no serving primary** (#286). Adding a replica to
  a live HA cluster (`postgresql.replicaCount` N → N+1) could leave the former primary
  unable to rejoin, so nothing served writes until it was manually repaired.

  The new pod can win the Lease before it has ever registered as a standby — it is created
  concurrently with the rolling restart the `REPMGR_NODE_COUNT` change triggers, so there is
  a window where no other pod holds the Lease. Once it promotes, the surviving nodes hold no
  `repmgr.nodes` record for it, and the agent drove repmgr by node id alone:

  ```
  action=Follow target=pg-2  reason="standby; follow the lease holder"
  ERROR repmgr standby follow: unable to find record for intended upstream node 1002
  WARN  repmgr standby register: unable to connect to the primary database
  ```

  Both failures are the same root cause: repmgr resolved the upstream through this node's
  own copy of `repmgr.nodes`, which is only as fresh as its replication and during a
  scale-up predates the current primary entirely. The register failed too, because that copy
  still named a former primary that is now read-only — so the node could not even record
  itself, and retried forever.

  `Follow` and `RegisterStandby` now take the upstream's **connection**, derived from the
  Lease holder's pod name, and address it with `-h/-p/-U/-d`. The Lease is always current, so
  repmgr talks to the real primary instead of consulting stale metadata. The password still
  travels via `PGPASSWORD` only, never argv (#167) — asserted by a test.

  This bug predates 2.0.0 and affects every 1.x release in agent mode; it went unnoticed
  because the only suite that scales a live cluster up ran in repmgrd mode (`OrderedReady`,
  which serialises pod creation and closes the window). Moving that suite to the agent as
  part of this change is what surfaced it, and it is now the regression test.

  **Release prerequisite:** this fix is in the agent binary, so `repmgr.image.tag` must be
  republished and bumped past `trixie-5.5.0-29` before 2.0.0 ships. CI builds the image from
  source, so the suites exercise the fix on this branch regardless.

### Added

- **The agent owns physical replication slot lifecycle in native mode (#289).** Inherited
  from pg's symlinked agent/templates. repmgr mode is untouched (repmgr keeps owning slots
  there). Under `mechanism: native` the agent creates `pg_ha_slot_<ordinal>` on the upstream
  before every clone and rejoin (so `pg_basebackup`/`pg_rewind` stream through it and no WAL
  gap can open), reconciles slots on every primary tick, and drops orphans -- never an
  active one, never one belonging to a pod that still exists (decided from the live
  Kubernetes pod list, not the render-time `REPMGR_NODE_COUNT`, which is stale on a
  not-yet-rolled pod during a scale-up), and never while paused. `cascadingReplication` is
  rejected at render time together with `native`. Three new metrics plus two `PrometheusRule`
  alerts (`PGHAReplicationSlotRetainingWAL`, `PGHAReplicationSlotInactive`) make an
  orphaned slot -- which otherwise pins WAL and fills the volume with no error until the
  disk is full -- alertable. Those are **not** native-only: ownership is, but the gauges
  they read are published under `repmgr` too (where the agent reports slots and never
  reclaims them), because an alert that can only fire under an experimental flag reads as
  coverage while providing none. New value:
  `repmgr.agent.monitoring.prometheusRule.slotRetainedWALBytes` (default **3Gi**).

  A third alert, `PGHAReplicationSlotInvalidated`, is the one that fires on chart defaults:
  the image caps slots with `max_slot_wal_keep_size = 4GB`, so PostgreSQL invalidates a
  neglected slot rather than letting it fill the volume -- and invalidation nulls
  `restart_lsn`, so the retained-WAL gauge collapses to zero at exactly that moment. The
  retained-WAL threshold is therefore the early warning and must stay below the 4GB cap. See
  the [pg chart README](../pg/README.md#replication-slot-ownership-289) for the full second
  review pass (stderr-blind error matching, stale gauges on a demoted primary, create/drop
  oscillation, legacy-slot reclaim scope, `Follow` slot ensure, promote sub-budget, and a
  demoted primary reclaiming the slots it minted -- which also needed a standby-safe slot
  query, since `pg_current_wal_lsn()` raises `recovery is in progress` on a standby).

- **`repmgr.agent.mechanism`: an experimental native HA mechanism, alongside repmgr
  (#287).** Inherited from pg's symlinked agent/templates. Off by default
  (`mechanism: repmgr`); `native` drives PostgreSQL's own tools directly instead of the
  repmgr CLI. **EXPERIMENTAL -- do not set in production, and only at
  `postgresql.replicaCount: 0` until `#288` lands** (verified live: any replicas leave every
  standby permanently `Init:CrashLoopBackOff`, since the shared `repmgr-init` init container
  has no `MECHANISM` awareness). See the
  [pg chart README](../pg/README.md#replication-mechanics-experimental-287) for the full
  detail, including three review-follow-up bug fixes to `Follow`/`Clone`/`GenerateConfig`.

### Migrating from 1.x

**If you were on the default (agent):** delete `repmgr.failoverMode: agent` from your values
if you set it explicitly, then `helm upgrade` normally. Your StatefulSet is already
`Parallel`; there is no recreate and no behaviour change.

**If you pinned `repmgr.failoverMode: repmgrd`:** `podManagementPolicy` moves `OrderedReady`
→ `Parallel`, and that field is **immutable**, so the StatefulSet has to be recreated once
(zero data loss — pods and PVCs are kept):

```bash
# 1. Healthy cluster + a fresh backup first. GitOps: disable auto-sync for these steps.
kubectl delete statefulset <release>-pgvector -n <ns> --cascade=orphan
# 2. Remove repmgr.failoverMode from your values, then upgrade (recreates the STS as
#    Parallel and adopts the orphaned pods):
helm upgrade <release> cagriekin/pgvector -n <ns>   # + your -f values, minus failoverMode
# 3. Verify:
kubectl get lease <release>-pgvector-leader -n <ns> -o jsonpath='{.spec.holderIdentity}'
kubectl get endpoints <release>-pgvector -n <ns>
```

Rollback is to chart `1.x` with `failoverMode: repmgrd` restored and the same
`--cascade=orphan` recreate.

Two further changes land for repmgrd users specifically: the agent assembles a pod-CIDR +
SCRAM `pg_hba.conf` with **no implicit `0.0.0.0/0 md5` catch-all** (add explicit
`postgresql.pgHba` rules first if you relied on it), and failover history moves from
`PrimaryChanged` Events to the agent's audit log and the `pg_ha_agent_*` metrics.

### Changed

- `test-scaledown` (the #139 ghost-node regression) was ported from repmgrd mode to the
  agent; the agent's `cleanupGhostNodes` runs the same `repmgr standby unregister` on the
  lease holder. The `repmgr-failover`, `repmgr-chaos`, `config-repmgr` and `migrate-agent`
  suites were removed with the mode they tested.
- The `upgrade` suite no longer scales the cluster up across the upgrade: both of its fixtures
  install 3 nodes, so it covers the upgrade itself (adding pgpool and the exporter, rolling the
  pods, preserving data) but not a scale-up. Moving it from repmgrd to the agent surfaced a
  **pre-existing** agent-mode race in which a new pod can win the Lease before it has
  registered in `repmgr.nodes`, after which no survivor can follow it and the cluster is left
  with no serving primary. That is tracked in #297, which restores the scale-up as part of its
  fix. **Known coverage gap, stated rather than hidden:** no suite currently exercises an
  agent-mode scale-up of a live cluster, and the race affects every 1.x release in agent mode
  (a backport decision is recorded on #297).
- `scripts/check-repmgrd-byte-stable.sh` is now `scripts/check-byte-stable.sh` and diffs the
  default render (there is no second mode to pin). Across the 1.x → 2.0.0 boundary it diffs
  heavily by design; compare against a 2.x ref for a meaningful result.
## 1.17.0 - 2026-08-26

Ships repmgr image `trixie-5.5.0-34`. Two HA-availability fixes, both of which could
leave a cluster with no serving primary and no automatic way back.

### Fixed

- **The etcd DCS no longer produces zombie candidates or phantom leadership (#326, #327).**
  `EtcdDCS.runElection` called `Election.Campaign` unguarded. etcd's `Campaign` puts a
  lease-bound candidate key and then blocks in `waitDeletes()` watching only lower-revision
  keys; session expiry is not one of its exits (documented upstream, unchanged between
  v3.5.12 and the vendored v3.5.31, so there is nothing upstream to adopt).

  Two failures followed. If the lease lapsed mid-campaign, etcd deleted the node's candidate
  key while `Campaign` stayed blocked forever: the node held no lease and no key, was absent
  from the election, and the `Run` loop never re-contended. A standby in that state cannot
  take over, so the cluster silently has **no failover**. And when the leader's key was
  eventually deleted, `waitDeletes` returned nil, so `Campaign` returned "you are leader"
  while the node held nothing — the agent then acted as leader concurrently with the peer
  holding the real lease.

  Observed in the field: a two-node cluster whose primary self-fenced on a transient etcd
  blip, with the standby already a zombie candidate. No peer took over for two hours, and
  the eventual double-leadership drove a rejoin that emptied the standby's data directory.

  The campaign is now bound to the session: it is cancelled when the lease lapses, so the
  `Run` loop starts a fresh iteration, and liveness is re-checked before leadership is
  reported. The lapse path deliberately does not wait for `Campaign` to unwind — its cancel
  path calls `Resign` on the client context, which carries no deadline and defaults to
  `WaitForReady(true)`, so it can block for the whole outage.

  `k8sdcs` is unaffected: client-go `leaderelection` owns renewal and cannot reach this state.

- **An empty data directory on ordinal 0 now clones instead of crash-looping (#325).** The
  init container derived its role from the pod ordinal, so a pod-0 that lost or recreated its
  PVC declared "First boot" and skipped the clone; the entrypoint guard then correctly refused
  to `initdb` next to an active primary, leaving the pod in CrashLoopBackOff with no automatic
  way out. An ordinal > 0 recovered from the identical failure, purely because of its name.
  The role is now decided from the cluster — probe for a primary, then for any live peer —
  and reuses the existing standby clone path.

- **Data carrying `standby.signal` is treated as standby-state whatever the ordinal (#325).**
  Otherwise ordinal 0 fell through to the master block, which `rm -rf`s PGDATA and takes a full
  base backup — on every restart of a healthy pod-0 standby, and unrecoverable if every clone
  attempt failed, since it deletes before it clones.

- **A standby that cannot reach a primary defers instead of failing (#325).** With `set -e`, a
  bare `wait_for_primary` returning 1 exits the init container. Now that ordinal 0 can reach
  that path, hard-failing would deadlock a cold boot: under `OrderedReady` pod-0 is recreated
  alone and blocks pod-1, so it waits ~240s for a primary that cannot exist, exits, and repeats
  while the real primary is never created. Existing data now defers to the entrypoint guard and
  starts in recovery, as the master block did. Only an empty directory still fails.

### Known gaps

- `Leader()` can still name a node that has already gone, and the `observe` loop does not
  restart after a watch disruption. `Election.Observe` never reports a deletion, so clearing
  the cache needs a prefix watch for DELETE events or `el.Leader()` polling. Tracked on #326.
- A repeatedly failing etcd session is not logged and the client is not redialled, so a wedged
  etcd client is invisible. Tracked on #326.

## 1.16.0 - 2026-08-25

Inherited from pg: same templates. `pgbackrest` gains `extraEnv`, `extraVolumes` and
`extraVolumeMounts` (#323), reaching every pgBackRest container -- the sidecar and
`pgbackrest-bootstrap` init container in the postgresql pod, the backup CronJobs, the restore
workload and the validation CronJob -- and deliberately not the postgresql container, which has
`postgresql.extraEnv` of its own. The case it exists for: the backup CronJob is an apiserver
client (EndpointSlice lookup + `kubectl exec`), so on a cluster whose egress policy denies pod
traffic to the apiserver it takes no backup -- and, being the chart's only caller of
`stanza-create`, leaves the repository uninitialised so `archive_command` fails on every WAL
segment from the start. `KUBECONFIG` plus a mounted kubeconfig now escapes that here the way
`postgresql.extraEnv` already did for the agent (#317). Validated at render time; no render
change with the values unset. See the pg chart README.

## 1.15.0 - 2026-08-23

Inherited from pg: same templates, same extension init containers. `postgresql.extensions`
gains `env`, `envFrom`, `extraVolumes` and `extraVolumeMounts` (#320), so the `apt-get` steps
in `copy-ext`/`copy-base-ext` can be pointed at an in-cell apt mirror or proxy -- under a
default-deny egress policy the cost of the install is the external HOSTS it forces into the
platform's baseline allow (`apt.postgresql.org`, `repo.pigsty.io`, and `deb.debian.org` for a
single transitive `libsodium23`), and one `http_proxy` replaces all three. All four apply to
both init containers and to neither the postgresql container, render only while `packages` is
non-empty, and are rejected at render time when they would be silently inert or would shadow
the chart's own volumes and install paths. A `pgdg` entry in `aptSources` is now refused too:
both images already configure PGDG under their own keyring, so a second entry made apt reject
the whole source list.

`postgresql.extensions.image` goes further and takes the install off the pod-start path
entirely: the packages are resolved once at build time (recipe in `images/pg-extensions/`) and
a third init container does a plain `cp`, so there is no egress at pod start at all and no
root -- which means it works in a PSA-`restricted` namespace, where the apt path cannot run.
Mutually exclusive with `packages`/`aptSources`; `extraLibs` still applies, reading from the
prebuilt image. See the [pg 1.15.0 changelog](../pg/CHANGELOG.md) and the
[pg chart README](../pg/README.md#pointing-the-extension-install-at-an-apt-mirror-or-proxy-320).

**Migrating from 1.14.1:** for almost everyone, nothing to do; the default render is
byte-identical. Two inherited changes can fail an upgrade that previously succeeded, both
turning a runtime failure into a render-time one: every image block now requires a tag or a
digest and a non-empty repository (so a values file that clears a tag without setting a digest
fails at `helm upgrade` instead of producing an `InvalidImageName` pod), and a `pgdg` entry in
`aptSources` is rejected. A digest-only pin, which previously rendered the unparseable
`repo:@sha256:...`, now works.

## 1.14.1 - 2026-08-22

Inherited from pg: same agent and image. See the [pg 1.14.1 changelog](../pg/CHANGELOG.md)
for the full detail.

### Changed

- **`repmgr.image.tag` -> `trixie-5.5.0-33` (#317).** The agent now honours `KUBECONFIG`,
  so its apiserver traffic can be routed through an in-cluster proxy -- for clusters whose
  egress policy denies pod traffic to the apiserver outright, where the agent otherwise
  never elects a leader and the cluster never gets a serving primary. Reaching a proxy
  needs a different **address** while still verifying the apiserver's own **certificate**,
  which only a kubeconfig can express (`server:` + `tls-server-name:`); overriding
  `KUBERNETES_SERVICE_HOST` breaks verification instead. **No new value**: set it with
  `postgresql.extraEnv` and mount the file with `postgresql.extraVolumes`/
  `extraVolumeMounts`. Both apiserver clients take the route -- the mutation client and the
  Lease-backed leader election. A set-but-unusable `KUBECONFIG` is a startup failure naming
  the file, never a silent fall back to in-cluster; `~/.kube/config` is not consulted. The
  boot log's new `apiserver=` field records the route. See the
  [pg chart README](../pg/README.md#routing-the-agents-apiserver-traffic--kubeconfig-317).

**Migrating from 1.14.0:** nothing to do. With `KUBECONFIG` unset -- the default -- the
in-cluster path is used exactly as before; `helm upgrade` rolls the pods once for the new
image.

## 1.14.0 - 2026-08-21

Inherited from pg's symlinked templates (`_helpers.tpl`, `statefulset.yaml`) and mirrored
`values.yaml`/`values.schema.json`. See the [pg 1.14.0 changelog](../pg/CHANGELOG.md) for
the full detail.

### Added

- **`postgresql.extensions.aptSources` (#310).** Adds a non-PGDG apt source (e.g. Pigsty)
  inside `copy-ext`/`copy-base-ext` before the `packages` apt-get install, for extensions
  PGDG doesn't package (`pgsodium`, `supabase_vault`, ...). Keyring/list files get a
  `pgchart-` prefix so they can't collide with a source the image already owns. Requires
  `packages` to be non-empty. See the [pg chart README](../pg/README.md#installing-packages-from-a-non-pgdg-apt-source-310).
- **`postgresql.extensions.extraLibs` + automatic `LD_LIBRARY_PATH` (#309).** Copies an
  exact, explicit path (e.g. `/usr/lib/x86_64-linux-gnu/libsodium.so.23`) into a new,
  dedicated `ext-extra-lib` volume -- kept separate from `ext-lib`, which is also populated by the unvalidated
  `*.so*` glob copy -- for a package's own shared-library dependency Debian installs
  outside the Postgres extension dir. The `postgresql` container gets
  `LD_LIBRARY_PATH=/usr/lib/postgresql/<major>/extra-lib` automatically whenever
  `extraLibs` is non-empty (not just `extensions.enabled`, to avoid widening the
  search path for releases that don't need it) so the copied file is actually found.
  Requires `packages` to be non-empty; every library the postmaster itself links is
  refused (the full `ldd postgres` NEEDED set, not just `libc`), the path must name a
  real shared library, and duplicate destination basenames are rejected too.
  `aptSources`' `curl` is now pinned to `https` (`--proto`/`--proto-redir`),
  `keyUrl`/`aptLine` allow `&`/`,` respectively, and `aptLine` must include
  `signed-by=` matching its own entry's keyring (`trusted=` is rejected outright).
  `LD_LIBRARY_PATH` is now a chart-reserved `postgresql.extraEnv` name. See the
  [pg chart README](../pg/README.md#copying-a-packages-own-shared-library-dependencies-309).

### Fixed

- **The extension-file copy glob (`*.so`) missed versioned shared libraries a package
  places directly alongside its own extension modules (#309).** Now `*.so*`. This does
  not, by itself, reach a dependency Debian installs elsewhere (the `libsodium.so.23`
  case) -- that needs `extraLibs`, above.

## 1.13.1 - 2026-08-21

### Fixed

- **`repmgr.image.tag` default (`trixie-5.5.0-31`) predated the #311 agent changes
  (#314).** Bumped to `trixie-5.5.0-32`, built from current `master`. See the
  [pg 1.13.1 changelog](../pg/CHANGELOG.md) for the full detail.

## 1.13.0 - 2026-08-21

Inherited from pg's symlinked templates (`postgresql-configmap.yaml`, `statefulset.yaml`,
`_helpers.tpl`) and mirrored `values.yaml`/`values.schema.json`. See the
[pg 1.13.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **First-class logical replication support and failover slot sync (#308).**
  `postgresql.walLevel` (enum `replica`|`logical`, default `replica`) is now the one
  authoritative source for `wal_level`; the agent patches `dbname` into
  `primary_conninfo` after clone/follow/rejoin (and deterministically at every cold
  start of a standby); `repmgr.agent.syncReplicationSlots` (default `false`, agent-mode
  + PostgreSQL 17+, requires `postgresql.walLevel: logical`) reconciles
  `synchronized_standby_slots` to the live standby set (repmgr's own node registry, not
  momentary replication-slot activity) on every primary tick. See the
  [pg chart README](../pg/README.md#logical-replication-308) for the full detail.

### Changed

- `pgbackrest-archive.conf` no longer hardcodes `max_wal_senders = 10` (#308).
- **Compatibility note:** `postgresql.configuration.wal_level` is no longer accepted;
  use `postgresql.walLevel` instead (#308). See the pg chart changelog for the
  migration note.

## 1.12.0 - 2026-08-18

Inherited from pg's symlinked `prometheus-exporter-configmap.yaml` and new
`prometheus-exporter-prometheusrule.yaml`. See the
[pg 1.12.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **`prometheusExporter.prometheusRule`: alert on stuck WAL archiving and pg_wal disk
  usage (#305).** Observability only -- no automatic write-throttle. An opt-in
  `PrometheusRule` wiring the exporter's own built-in `pg_wal_size_bytes` metric and the
  existing (previously unused) `pg_wal_archive` failure/staleness metrics to alerts. No
  new query or grant -- a chart-defined `pg_wal_size` query group was tried and reverted
  after live-testing caught it colliding with the exporter's built-in metric of the same
  name, which broke the entire scrape. See the
  [pg chart README](../pg/README.md#wal-disk-usage-305) for the full detail.

## 1.11.0 - 2026-08-17

Inherited from pg's symlinked `statefulset.yaml`. See the
[pg 1.11.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **`postgresql.extensions.packages`: install PGDG/Debian extension packages without a
  custom image (#303).** `copy-ext` can now `apt-get install` a package list
  (`{major}`-substituted, optionally version-pinned) before its existing `cp -n` copy, so
  an extension beyond `vector` that this chart's donor image never shipped (e.g.
  `postgresql-<major>-cron`) reaches the server without a custom image. Off by default;
  a default render is byte-identical to 1.10.2. See the
  [pg chart README](../pg/README.md#installing-extensions-without-a-custom-image) for a
  complete example.

### Fixed

- **`shared_preload_libraries` never actually applied before the post-install hook Jobs
  ran on a fresh install.** Pre-existing, not introduced by this release's change above.
  See the [pg 1.11.0 changelog](../pg/CHANGELOG.md) for the full root-cause detail.
  `repmgr.image.tag` bumped to `trixie-5.5.0-31` (`etcd.rbac.bootstrapImage.tag`
  alongside it); no chart-side change needed since the fix lives entirely in the image's
  `initdb`-time config.

## 1.10.2 - 2026-08-14

Inherited from pg's symlinked `statefulset.yaml`. See the
[pg 1.10.2 changelog](../pg/CHANGELOG.md) for the full detail.

### Fixed

- **`copy-ext` could silently overwrite `copy-base-ext`'s libs with a mismatched build
  (#302).** Hit in production: this chart's `postgresql.image` (previously the floating
  `pg18-trixie` tag) had drifted to a newer PostgreSQL point release than the pinned
  `repmgr.image`, and `copy-ext`'s unconditional `cp` replaced the running server's
  `libpqwalreceiver.so`, breaking replication on every freshly-created pod. `copy-ext`
  now uses `cp -n` (no-clobber).

### Changed

- **`postgresql.image.tag` default pinned from the floating `pg18-trixie` to
  `0.8.5-pg18-trixie` (#302),** matching `repmgr.image`'s default point release (18.4).
  `cp -n` above means this image can no longer clobber the running server's libs
  regardless, but a matching point release is still required for `CREATE EXTENSION
  vector` (built against 18.4 headers) to load safely. Bump only in lockstep with
  `repmgr.image`.

## 1.10.1 - 2026-08-11

### Fixed

- **Backport (#297): a scale-up could leave the cluster without a serving primary, or with a
  standby that never replicates.** Both affect agent mode -- the default since `1.0.0` -- from
  that release onward. Fixed on the 2.0.0 line first; backported here because they are
  data-availability bugs in a shipped release.

  Adding a replica (`postgresql.replicaCount` N -> N+1) also changes `REPMGR_NODE_COUNT`, which
  rolls every pod, so the new pod is created while the others restart. Two things could go wrong:

  1. **No serving primary.** The new pod could win the Lease before registering in
     `repmgr.nodes`. Once it promoted, no survivor held a record for it, so none could
     `repmgr standby follow` it -- it failed `unable to find record for intended upstream node`
     on every reconcile tick, and nothing served writes. *Fix:* a promote candidate now reads
     `repmgr.nodes` and, if it has no row of its own while a registered and reachable peer
     exists, releases the Lease instead of promoting. Conservative by design -- an unreadable
     registry, a cluster where nobody is registered, or an unreachable peer all still promote,
     because serving with degraded metadata beats refusing to serve.
  2. **A standby that never replicates.** The new pod's own `repmgr.nodes` copy is a snapshot of
     the primary taken *before* it registered, so it holds no row for itself and cannot obtain
     one -- receiving it requires replicating, and repointing replication is what needs it. It
     failed `unable to retrieve record for local node` forever, `Running` but never `Ready`.
     *Fix:* that specific error now triggers a re-clone from the current primary, replacing data
     and metadata together.

  The re-clone is scoped by error string, not by state, and the distinction matters: a missing
  **upstream** record is the ordinary post-failover case where the target has simply not promoted
  yet, and escalating there would demote and re-clone a healthy standby. A unit test asserts the
  upstream variant does not escalate.

  **Image:** the fix is in the agent binary, so this release republishes the repmgr image as
  `trixie-5.5.0-30` and pins `repmgr.image.tag` to it (along with `etcd.rbac.bootstrapImage.tag`,
  which runs the same image and must move in lockstep). A 1.10.1 left pointing at `-29` would
  have carried the chart change without the fix.

  Verification: reproduced and fixed on a live KinD cluster on the 2.0.0 line, whose `upgrade`
  suite scales an agent-mode cluster up. **1.x's own `upgrade` suite pins `failoverMode: repmgrd`**
  (whose `OrderedReady` serialises pod creation and closes the window), so the 1.x suites prove
  no regression rather than proving the fix. The agent binary is identical to the verified one.

## 1.10.0 - 2026-08-02

Inherited from pg's symlinked templates and the shared repmgr image. See the
[pg 1.10.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **A `ValidatingAdmissionPolicy` bounding `repmgr.agent.control.restore`'s job-create
  grant** (#279), rendered by default when that feature is enabled. `create` on `jobs`
  cannot be scoped by `resourceName`, so 1.9.0's grant was a namespace-wide
  privilege-escalation primitive on a token that sits beside user-supplied SQL. The policy
  restricts by **content** instead — one permitted Job name, this release's ServiceAccount
  with no mounted token, no host namespaces, no `nodeName` and only this release's
  `priorityClassName` (a higher-priority class would let the scheduler preempt this release's
  own pods), the pod's own labels (so the restore pod cannot join this release's Service
  endpoints), one container on this release's image running this release's restore command with
  no args and no lifecycle hooks, this release's pod and container
  security contexts (no privileged / root / added-capability container), requests and limits,
  a single pod, and only this release's volumes and Secrets — and polices only this release's
  ServiceAccount, leaving every other Job creator untouched. `failurePolicy: Fail`, and
  rendering the grant without the policy fails the render unless
  `admissionPolicy.acknowledgeUnbounded: true` says so deliberately.

  What it does **not** bound, stated plainly: the restore *parameters*. Anything holding the
  token can still run this release's own restore over the live PGDATA without presenting a
  certificate to the control API — that is the operation being exposed. Where untrusted SQL
  runs and an unscheduled restore would itself be an incident, leaving `control.restore` off
  remains the right answer.
- The restore CronJob's `jobTemplate` now carries `pg-ha/restore=<fullname>`, so a cloned
  restore Job is selectable where before it carried no labels.

### Upgrading

- Enabling `repmgr.agent.control.restore` now requires **Kubernetes ≥ 1.30** and
  cluster-scoped `create` on `admissionregistration.k8s.io`. A default install renders no
  cluster-scoped objects, so nothing else changes. Without the API the **render** fails,
  rather than the apply aborting halfway; the precondition checked is the presence of
  `admissionregistration.k8s.io/v1`, not the reported version, because with no cluster to
  query `.Capabilities.KubeVersion` is the helm client's own. To keep 1.9.0's behaviour, set
  `admissionPolicy.enabled: false` **and** `acknowledgeUnbounded: true`.
- Secret names and image references interpolated into the policy's CEL expressions are now
  charset-validated at render time, so a name containing a quote fails the render with the
  value named instead of producing a policy the API server rejects — or one whose validation
  is a tautology.

## 1.9.0 - 2026-08-01

Inherited from pg's symlinked templates and the shared repmgr image. See the
[pg 1.9.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- Authenticated control REST API for the agent (`repmgr.agent.control`, #276), off by
  default and agent mode only: mTLS-only on its own port (9201, never the 9200 metrics
  port), deny-by-default under `networkPolicy.enabled`, with pause / switchover / restart /
  reload plus reads that expose per-member position and the reconcile loop's latest
  decision. A facade over the same marker annotations `kubectl annotate` writes.
- API-driven PITR restore (`repmgr.agent.control.restore`, #276) as a separate opt-in — it
  grants the pods `create` on `jobs`, which RBAC cannot scope by name. Off by default.
- `restore.sh` records each restore attempt's outcome beside PGDATA, so it survives the Job
  and doubles as provenance for the data directory.

## 1.8.1 - 2026-07-31

Inherited from pg's symlinked templates and the shared repmgr image. See the
[pg 1.8.1 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **PostgreSQL 17 is selectable; 18 remains the default** (#269). The repmgr image — which
  supplies the server binaries in repmgr mode — now takes the major as a build argument, and
  each release publishes `trixie-5.5.0-29` (18, the default and what unsuffixed pins
  resolve to) plus `-pg18` / `-pg17`.

  For this chart the pgvector image must move too: `postgresql.extensions` copies the
  `vector` extension out of `postgresql.image` into the server container, so a PG17 cluster
  needs `postgresql.image.tag: pg17-trixie` alongside the two `majorVersion` values and the
  `-pg17` repmgr tag. See
  [Choosing the PostgreSQL major](README.md#choosing-the-postgresql-major).

  **Create-time choice, not an upgrade path** — a new-major server refuses to start on an
  old-major `PGDATA`.

### Changed

- `repmgr.image.tag` → `trixie-5.5.0-29`. **An unchanged values file produces an unchanged
  result**: the unsuffixed tag is still PostgreSQL 18.

### Fixed

- The render now fails when a `-pgNN` image tag disagrees with `repmgr.image.majorVersion`,
  and `PG_MAJOR` is passed to every container running the repmgr image, so a mismatch the
  tag cannot express (majors moved to 17 against the unsuffixed PG18 tag) is refused at
  startup with both majors named instead of running the wrong major silently.

- The bundled etcd's RBAC-bootstrap Job never picked up the chart's tag override: the
  subchart reads it at `rbac.bootstrapImage`, so a top-level `etcd.bootstrapImage` was
  ignored and the Job stayed on `trixie-5.5.0-24`.

## 1.8.0 - 2026-07-31

Inherited from pg's symlinked templates (`statefulset.yaml`, `pgbackrest-configmap.yaml`). See
the [pg 1.8.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **`pgbackrest.bootstrap.*` — automatic recovery from a lost PVC** (#266). Previously, losing
  replica 0's volume meant the pod came back and `initdb`'d a brand new **empty** cluster while
  the backups sat intact in S3 — silent, with nothing failing loudly. With
  `pgbackrest.bootstrap.enabled=true` an init container seeds the empty data directory from this
  release's own pgBackRest repository and PostgreSQL replays the archived WAL on startup.

  Safe to leave enabled: it only ever writes into an *empty* PGDATA and refuses to touch an
  initialized one, so a restart or rollout can never re-restore over a running cluster. An empty
  repository is a no-op (a normal first install proceeds), while an *unreachable* repository
  fails loudly rather than silently initializing an empty cluster. Only replica 0 bootstraps;
  standbys are cloned from it by repmgr. Supports `bootstrap.targetType`/`target` and
  `bootstrap.backupSet`.

## 1.7.0 - 2026-07-31

Inherited from pg's symlinked templates (`pgbackrest-restore-job.yaml`,
`pgbackrest-configmap.yaml`). See the [pg 1.7.0 changelog](../pg/CHANGELOG.md) for the full
detail.

### Added

- **`pgbackrest.restore.*` — a first-class PITR restore resource** (#226). Replaces the
  hand-built `kubectl run --overrides='{…}'` restore pod (~30 lines of inline JSON) with a
  chart-rendered one: scale to 0 →
  `kubectl create job --from=cronjob/<fullname>-pgbackrest-restore` → wait for the Job to
  complete → scale back up. It
  carries the `-repmgr` ServiceAccount (so `s3.keyType: auto` works, API token unmounted),
  the postgresql security contexts, the `data-<fullname>-0` PVC and pgbackrest ConfigMap
  mounts, the S3 / repo-encryption credentials, and `pgbackrest restore --delta`.

  Enabling it restores nothing: it renders an inert resource (by default a suspended
  CronJob that can never fire), so it can be left on and cloned when needed. It never
  starts PostgreSQL — WAL replay and promotion happen on scale-up. `restore.mode: job`
  renders a bare Job instead, for passing a point-in-time target inline via
  `helm template -s`. Supports `restore.targetType`/`target`, `restore.backupSet` and
  `restore.force`, with fail-fast guards for the target/targetType pair and for the
  `pgbackrest.enabled` + `postgresql.persistence.enabled` prerequisites.

  Standbys need no extra step: the restored primary returns on a new timeline, and each
  standby's init container detects the mismatch and re-clones itself from it (verified in
  the pg chart's `test-pgbackrest-restore-ha` suite, which covers these shared templates).

### Changed

- The README's Point-in-Time Recovery runbook is now that four-command flow; the
  `kubectl run --overrides` pod spec is gone.

## 1.6.0 - 2026-07-30

Inherited from pg's symlinked templates (`statefulset.yaml`, `postgresql-configmap.yaml`,
`_helpers.tpl`). See the [pg 1.6.0 changelog](../pg/CHANGELOG.md) for the full detail.

### Added

- **`postgresql.extraVolumes` / `postgresql.extraVolumeMounts` / `postgresql.extraEnv`**
  (#262) — generic pod-spec passthrough for the postgresql container, for mounting a file
  that must be byte-identical on the primary and every standby (e.g. the pgsodium server
  root key behind Supabase Vault, which a promoted standby needs to decrypt
  `supabase_vault` after a failover). All default to `[]`, so rendered output is unchanged
  when unset, and all three are validated at render time (list shape, no chart-managed
  volume-name collision, every mount must reference a declared volume, no reuse of a
  chart-set env name).

### Fixed

- **An operator-set `postgresql.configuration.shared_preload_libraries` silently dropped
  `repmgr` whenever `postgresql.audit.enabled` was false**, disabling failover. The merge
  preserving `repmgr` now runs unconditionally in repmgr mode. This matters more here than
  in pg: pgvector operators commonly set `shared_preload_libraries` to load extensions.

## 1.5.0 - 2026-07-12

Inherited from pg's symlinked templates. Requires the new repmgr image
(`trixie-5.5.0-28`), which bundles the `pgaudit` extension; the default
`repmgr.image.tag` is bumped accordingly. pgaudit is inert until
`postgresql.audit.enabled` is set.

### Added

- **pgaudit-based audit logging (`postgresql.audit.*`)** for compliance regimes
  (SOC 2, HIPAA, PCI-DSS, ISO 27001) — opt-in, default-off. See the pg CHANGELOG for
  full detail (#219).

## 1.4.3 - 2026-07-11

Chart-only fix inherited from pg's symlinked templates. No image change
(`trixie-5.5.0-27`). pgpool now reaps orphaned SysV shmem (`ipcrm -a`) before
starting, preventing a crash loop after repeated container OOM kills, and adds a
`pgpool.command` override. See the pg CHANGELOG for full detail (#234).

## 1.4.2 - 2026-07-05

Chart-only fix inherited from pg's symlinked templates. No image change
(`trixie-5.5.0-27`). See the pg CHANGELOG for full detail.

### Fixed

- **pgBackRest PITR restore-validation no longer fails when the primary raises a
  recovery-relevant GUC (e.g. `max_connections`).** The throwaway recovery instance started
  with initdb defaults (`max_connections=100`) after `validate.sh` strips the `conf.d`
  overlay, so archive recovery aborted with *"recovery aborted because of insufficient
  parameter settings"* whenever the primary's value was higher — failing the validation Job
  despite a fully restorable backup. The script now reads the required minimums from
  `pg_controldata` and passes them as `pg_ctl` startup overrides.

## 1.4.1 - 2026-06-30

Chart-only fix inherited from pg's symlinked templates. No image change. See the pg
CHANGELOG for full detail.

### Fixed

- **Backup integrity check no longer aborts on dumps larger than the pipe buffer (#230).**
  The `backup.enabled` (pg_dump → S3) integrity step (`mc cat … | pg_restore --list`) was
  SIGPIPE-killed and aborted by `pipefail` on any dump exceeding the ~64 KB pipe buffer
  (any real database), before the staged `.tmp` object was promoted — so no backup was ever
  published. The check now inspects both ends of the pipe via `PIPESTATUS`: `pg_restore`
  must succeed and `mc cat` may only exit `141` (SIGPIPE), so a real read error stays fatal.

## 1.4.0 - 2026-06-26

Chart-only feature inherited from pg's symlinked templates. No image change
(`trixie-5.5.0-27`). See the pg CHANGELOG for full detail.

### Added

- **Declarative databases, roles & grants (#218).** `postgresql.roles[]` /
  `postgresql.databases[]` idempotently create roles, databases (with `owner` +
  per-database `extensions`), and grants via a post-install/upgrade hook Job on the
  primary. Default-empty; passwords sourced from Secrets (never the ConfigMap); identifiers
  and privileges are guard-validated. See the pg chart README "Databases & roles".

## 1.3.0 - 2026-06-26

Chart-only feature inherited from pg's symlinked templates. No image change
(`trixie-5.5.0-27`). See the pg CHANGELOG for full detail.

### Added

- **Automated pgBackRest PITR restore-validation (#38).** Opt-in CronJob
  (`pgbackrest.validation.enabled`) that restores the pgBackRest repository into a
  throwaway PostgreSQL, replays archived WAL, validates, and exits — proving the backups
  are restorable without touching the live cluster. Supports an optional PITR target;
  defaults to restoring the latest backup + all WAL.

## 1.2.7 - 2026-06-26

Chart-only fix inherited from pg's symlinked templates, for the legacy `backup.enabled`
(pg_dump → S3) path. No image change (`trixie-5.5.0-27`). See the pg CHANGELOG for full
detail.

### Fixed

- **Backup to AWS S3 failed when the secret access key contained `/` or `+` (#221).**
  Credentials are now loaded via `mc alias import` from a `0600` JSON document (feeding the
  raw secret to the SigV4 signer) instead of a percent-encoded `MC_HOST` URL, which `mc`
  signed with the encoded secret and rejected. Credentials still never appear in the process
  argv.

## 1.2.6 - 2026-06-26

Image security refresh: repmgr image `trixie-5.5.0-26` → `trixie-5.5.0-27`. No chart
template or behavior change. See the pg CHANGELOG for full detail.

### Fixed

- **Bundled `kubectl` upgraded `v1.31.3` → `v1.36.2`**, clearing its image-scan CVEs
  (1 Critical + ~24 High in the old Go 1.22 / `x/net` 0.26 build).
- **Debian security updates applied at build time (`apt-get upgrade`)**, picking up fixes
  such as `libssh2` `1.11.1-1+deb13u1` (`CVE-2026-7598`, `CVE-2026-55200`).

## 1.2.5 - 2026-06-25

Chart-only correctness fixes inherited from pg's symlinked templates (#211, #212, #213,
#214). No image change (`trixie-5.5.0-26`). See the pg CHANGELOG for full detail.

### Fixed

- **md5→scram rehash broke on passwords containing a single quote (#211).** `fix_user_auth`
  now reads credentials from the environment via psql `\getenv` instead of `-v`, so any
  password value is handled.
- **pgBackRest backup could target a standby and silently skip it (#212).** The cronjob now
  validates the read-write primary with `pg_is_in_recovery()` (timeout-bounded, retried,
  deterministic selection), with a single-endpoint fallback so a lone primary is never
  skipped on a transient probe failure.
- **Inconsistent `required` guard on the pgBackRest S3 secret name (#213).** Now applied
  consistently across all references via a shared helper.
- **Unbounded ephemeral PGDATA emptyDir when `persistence.enabled=false` (#214).** The cap
  now falls back to `persistence.size`, then a 10Gi floor, so it is never null.

## 1.2.4 - 2026-06-24

Chart-only fix for agent-mode pgpool instability at `postgresql.replicaCount: 0` (#207).
No image change (`trixie-5.5.0-26`). Shares pg's pgpool template (symlinked); see the pg
CHANGELOG for detail.

### Fixed

- **Agent-mode pgpool churned `restarting myself` and dropped live primary connections
  when `postgresql.replicaCount: 0` (#207).** The pgpool ConfigMap unconditionally
  configured a second backend at the `-readonly` Service, which has zero endpoints with no
  standbys, so pgpool health-checked an unreachable backend and repeatedly restarted. The
  RO backend is now emitted only when `replicaCount > 0`; primary-only agent mode renders a
  single, stable RW backend.

## 1.2.3 - 2026-06-23

Chart-only fix for a postgres-exporter TLS regression (#204). No image change
(`trixie-5.5.0-26`). Shares pg's exporter template; see the pg CHANGELOG for detail.

### Fixed

- **Exporter could not read the TLS CA under `sslmode=verify-ca`/`verify-full`
  (`permission denied`, `pg_up=0`) (#204).** The CA secret was mounted whole at
  `defaultMode: 0400`, unreadable by the exporter's non-root UID (no `fsGroup`). The TLS
  volume now projects only the public `ca.crt` at `0444`; `tls.crt`/`tls.key` are no
  longer mounted. Regression from the #110 mTLS work.

## 1.2.2 - 2026-06-22

Fixes the agent-mode `pg_hba.conf` dual-authorship bug (#199); image moves to
`trixie-5.5.0-26`. Shares pg's agent + templates; see the pg CHANGELOG for detail.

### Fixed

- **Agent-mode standbys could end up SCRAM-only, breaking md5-password auth (#199).**
  The agent is now the single author of `pg_hba.conf` (md5-first compat on every node, so
  primary and standby match); the postStart md5-fallback + re-hash run only in repmgrd
  mode. The md5->scram managed-user re-hash moved into the agent and runs on
  promotion/boot-primary, gated by `postgresql.migrateLegacyMd5Users` (default true).

## 1.2.1 - 2026-06-22

Chart-only bug fixes from a full-chart review. No image change (`trixie-5.5.0-25`);
no rendered change at defaults. Shares pg's templates; see the pg CHANGELOG for detail.

### Fixed

- **pgBackRest silently disabled S3 TLS verification (pgvector-specific).** `values.yaml`
  was missing `pgbackrest.s3.uriStyle` and `pgbackrest.s3.verifyTls`, which the shared
  template dereferences. With pgBackRest + S3 enabled, pgvector rendered
  `repo1-storage-verify-tls=n` (cert verification off) and an empty `repo1-s3-uri-style=`.
  Added both keys (`uriStyle: host`, `verifyTls: true`) to match pg.
- **NetworkPolicy never matched the metrics exporter** (shared template; selected
  `prometheus-exporter` instead of the pods' `postgres-exporter` label). Fixed.
- **`helm install/upgrade` failed under NetworkPolicy with the monitoring user enabled**
  (the `monitoring-user` hook Job was not admitted to PostgreSQL). Fixed.

### Added

- **`values.schema.json` enum guards** for `prometheusExporter.sslmode`,
  `pgpool.tls.backendSslmode`, `pgbackrest.s3.uriStyle`, and
  `pgbackrest.repoEncryption.cipherType`.
- **`.helmignore`** (the chart had none), excluding `tests/`, `Makefile`,
  `kind-config.yaml`, and a stray `test.yaml` from the released `.tgz`.

### Changed

- **Bundled etcd RBAC-bootstrap Job image tag** pinned to `trixie-5.5.0-25` via the
  `etcd.bootstrapImage.tag` override, in lockstep with the repmgr image.
- **Agent ServiceMonitor selector scoped** to the postgresql component (matches only the
  headless Service). kube-linter probe waivers added to the one-shot Jobs/CronJobs.
  Shared with pg; see the pg CHANGELOG.
- Corrected misleading values.yaml comments: `pgpool.tls.clientCertAuth` validates a
  client cert only if the frontend presents one (it does not require one), and
  `monitoringHistoryDays` pruning applies only in repmgrd mode.

## 1.2.0 - 2026-06-21

Optional client-connection TLS for PostgreSQL, PGPool, and the metrics exporter (#110),
plus optional cascading replication (#29). Both off by default — no rendered change at
defaults. Image moves to `trixie-5.5.0-25`.
pgvector shares pg's templates and agent; see the pg CHANGELOG for the full detail.

### Added

- **Optional cascading replication (#29, `repmgr.agent.cascadingReplication`, agent mode,
  default off).** A standby may stream from another standby (a pod-ordinal chain toward the
  primary) to offload the primary's WAL senders; the agent only follows a verifiably-safe
  same-timeline upstream and re-homes to the leader on failure, never stranding a standby.
  Byte-stable when off. Shares pg's agent; see the pg CHANGELOG.
- **PostgreSQL server TLS** (`postgresql.tls.enabled`, BYO `existingSecret`), **enforced
  TLS** (`postgresql.tls.require`) and **mutual TLS** (`postgresql.tls.clientCertAuth`,
  agent mode only, with internal service users exempted from the client-cert requirement).
- **PGPool TLS** (`pgpool.tls.*`: frontend + backend, with a backend client cert for
  PostgreSQL mTLS) and a configurable exporter **`prometheusExporter.sslmode`**.
- Fail-fast guards for every TLS combination.

### Notes

- Replication stays plaintext on the pod network (documented non-goal). repmgrd mode
  supports only optional server TLS; `require`/`clientCertAuth` are agent-mode only.
- Reload `ssl_*` with `kubectl rollout restart` after rotating the cert Secret.

## 1.1.8 - 2026-06-21

Quiets the etcd RBAC health-probe noise (#187). Bundles `etcd` 0.1.5; image moves to
`trixie-5.5.0-24`. Only affects the opt-in shared-etcd TLS+RBAC path; no rendered change
at defaults. See the pg CHANGELOG for detail.

### Fixed

- **etcd RBAC health probe no longer spams `cannot find a user for permission check`
  (#187):** the probe presents the server cert, whose CN mapped to no etcd user, so etcd
  logged this ERROR every probe interval (the probe still passed — `etcdctl endpoint
  health` treats permission-denied as healthy — so it was log noise, not a broken check).
  The `rbac-bootstrap` Job now provisions a read-only health user (read on the `health`
  key) and the etcd server cert's CN is set to it (`etcd.rbac.healthCheckCN`, default
  `etcd-healthcheck`), so the probe authenticates cleanly and the log clears. Existing
  shared-etcd TLS+RBAC users must reissue the etcd server cert with `CN=etcd-healthcheck`.

## 1.1.7 - 2026-06-20

Fixes an agent-mode rolling-restart deadlock (#186). Image moves to
`trixie-5.5.0-23`. pgvector shares pg's templates and image; see the pg CHANGELOG for
detail.

### Fixed

- **A rolling restart of a 2-node agent-mode cluster could deadlock with no writable
  primary (#186):** two fixes — (1) **replication-aware standby readiness**, so a
  standby is Ready only once its walreceiver is `streaming`, stopping `RollingUpdate`
  from rolling the primary/clone-source mid-clone (primary readiness and repmgrd mode
  unchanged); and (2) an empty-data lease holder whose durable marker names a
  *different* primary now **releases the lease** so the data-bearing primary promotes,
  turning the deadlock into a few-second self-heal. Covered by a new live
  `test-agent-rolling` suite.

## 1.1.6 - 2026-06-20

Restores efficient stale-primary recovery (#178) and auto-cleans ghost
`repmgr.nodes` rows on scale-down (#139). Image moves to `trixie-5.5.0-22`; bundles
`etcd` 0.1.4 (bootstrap-image tag lockstep only). No rendered change in agent mode
(the default). pgvector shares pg's templates and image; see the pg CHANGELOG for
detail.

### Fixed

- **Stale-primary recovery now rewinds with `pg_rewind` instead of always falling
  back to a full re-clone (#178):** the container-restart guard's `repmgr node
  rejoin --force-rewind` now supplies the repmgr password via `PGPASSWORD` so
  repmgr can open the replication connection the rewind needs, instead of an inline
  conninfo password that did not reach it. Data safety is unchanged (the re-clone
  fallback remains); this restores the efficient O(diverged-WAL) path over an
  O(database-size) base backup on large databases. Working rewind also exposed a
  latent agent-failover bug: the agent read a standby's timeline from the laggy
  control file, so a streaming-caught-up standby was wrongly rejected by the `#125`
  highwater guard on failover; it now reads
  `GREATEST(checkpoint timeline, pg_control_recovery.min_recovery_end_timeline)`. See
  the pg CHANGELOG for detail.
- **Scaling `postgresql.replicaCount` down no longer leaves permanent ghost rows in
  `repmgr.nodes` (#139):** the primary now reconciles `repmgr.nodes` against the live
  ordinal range each tick and unregisters records for pods the StatefulSet no longer
  runs (agent mode: the lease-holding primary; repmgrd mode: the master's
  service-updater). Keyed on the ordinal, never reachability, so a momentarily-down
  live node is never touched. The manual `repmgr standby unregister` cleanup is no
  longer required.
- **Image shell scripts share the timeline helpers (#177):** `entrypoint.sh` and
  `init-repmgr.sh` now source a single `repmgr-common.sh` (`tl_to_int` + the timeline
  reads), so a fix can't land in only some copies. `init-repmgr.sh` keeps its symmetric
  control-file timeline comparison, avoiding a needless re-clone of a streaming-caught-up
  standby.

## 1.1.5 - 2026-06-19

A monitoring-exporter `/probe` fix (#185). Chart-only; no image change. See the pg
CHANGELOG for detail; pgvector shares pg's templates.

### Fixed

- **Monitoring user (#28) multi-target `/probe` scrape returned `pg_up=0` on every
  target.** The exporter `auth_modules` probe DSN had no database, so libpq used the
  username `monitoring` as the dbname (`database "monitoring" does not exist`). The
  probe now pins `dbname` to the configured database. Shared fix with the pg chart
  (templates are symlinked); see the pg CHANGELOG for detail.

## 1.1.4 - 2026-06-19

Bundled-etcd security (#184). Bundles `etcd` 0.1.3; image moves to
`trixie-5.5.0-21` (adds the `pg-ha-agent rbac-bootstrap` subcommand). No rendered
behavior change at defaults. See the pg CHANGELOG for detail; pgvector shares pg's
templates and the bundled etcd.

### Added

- **Bundled etcd transport TLS (`etcd.tls.*`) + per-tenant RBAC (`etcd.rbac.*`) for a
  shared etcd (#184):** mutual TLS + a CN-keyed bootstrap Job (running
  `pg-ha-agent rbac-bootstrap`) granting each tenant readwrite only on its key prefix.
  Flag-gated (default off, render byte-stable); a consuming release's agent
  authenticates by client-cert CN with no change.

## 1.1.3 - 2026-06-19

Multi-pillar-review remediation of the 1.1.2 etcd changes. Refreshes the bundled
`etcd` subchart to 0.1.2. Docs/test-quality only; no image change (stays
`trixie-5.5.0-20`) and no rendered behavior change at defaults. See the pg
CHANGELOG for the full list; pgvector shares pg's templates and the bundled etcd.

### Fixed

- Standalone-etcd guide endpoint corrected (Service is `<release>-etcd-etcd`;
  install now sets `fullnameOverride`); stale etcd `image.tag` pin comment fixed.

### Security

- Hardened `etcd.networkPolicy.allowedClients` guidance (plaintext/no-auth etcd;
  recommend an instance-pinned podSelector + client TLS, #184).

## 1.1.2 - 2026-06-19

Refreshes the bundled `etcd` subchart to 0.1.1. Chart-only; no image change
(stays `trixie-5.5.0-20`) and no behavior change at defaults.

### Added

- **`etcd.networkPolicy.allowedClients` for the bundled etcd (#183).** Declarative
  cross-namespace client allow-list (`[{namespace, podSelector?}]`) on the etcd
  client port, replacing hand-written `extraIngress` selectors for a shared etcd.
  Default `[]` (render byte-stable). The `etcd` chart is now also published
  standalone (0.1.1) so several releases can share one etcd; see its README.

## 1.1.1 - 2026-06-19

Security: bump the HA agent's vendored Go dependencies off two CVE-flagged
versions. Image moves to `trixie-5.5.0-20`. No chart-template or values changes
beyond the image tag; a `helm upgrade` rolls the pods once. (Shares pg's agent and
image; see the pg CHANGELOG for the dependency-level detail.)

### Security

- **Bumped `google.golang.org/grpc` 1.59.0 -> 1.79.3 (CVE-2026-33186, critical)
  and `golang.org/x/oauth2` 0.21.0 -> 0.34.0 (CVE-2025-22868, high)** in the
  `pg-ha-agent` module (both transitive; etcd client bumped 3.5.16 -> 3.5.31 to keep
  the grpc jump source-compatible). `govulncheck` reported both as unreachable, so
  there was no exploit path in the running binary; the bump clears the advisories.

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
