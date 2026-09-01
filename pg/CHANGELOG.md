# pg chart changelog

## 2.0.0 - unreleased

### Changed (breaking)

- **An unquoted image tag is now refused; quote every `image.tag` (#298).** Any `image.tag` (or
  `image.digest`) that YAML parses as a number is rejected at render time instead of coerced. This
  **breaks values files that wrote `tag: 18` unquoted**, which rendered correctly in 1.x -- the
  fix is one character per site: `tag: "18"`. On the command line, shell quotes do not help
  (`--set x.tag="18"` is still typed as a number); use `--set-string`.

  The reason it cannot simply keep working: YAML parses `18`, `18.0` and `2.10` to the *same*
  `float64` values a template sees, so `18.0` arrives indistinguishable from `18` and `2.10` from
  `2.1`. A coercing chart therefore renders `repo:18` for a pin written `18.0`, and `repo:2.1`
  for `2.10` -- a render-clean manifest deploying an image the values file never named. There is
  no way to tell the safe case from the lossy one after parsing, so the chart refuses the whole
  class and says how to fix it. Affects `pg`, `pgvector` and `etcd`.

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

### Added

- **Two agent alerts for failures that were published but unwatched (#298).** The agent exports 21
  metric series; the bundled `PrometheusRule` rated or thresholded only nine of them, so the rest
  were graphable but never paged on. Two of those gaps hid conditions that are otherwise entirely
  silent -- the cluster keeps serving, every probe stays green, and nothing in a healthy pod's log
  mentions them:

  - `PGHAAgentMarkerTamperSuspected` (critical, 5m) on `pg_ha_agent_marker_tamper_suspected_total`.
    The primary marker is a ConfigMap any namespace writer can forge, and `unsafeToServe` trusts
    its recorded timeline: an implausible or unparseable highwater trips that guard on **every**
    node and freezes automatic promotion cluster-wide. The agent already failed closed there and
    counted the tick; nothing turned that counter into a page, so a frozen failover would surface
    for the first time when the primary died and no standby took over.
  - `PGHAReplicasNotStreaming` (warning, 15m) on the `pg_ha_agent_replicas_*` gauges: the primary
    sees fewer *identified* streaming standbys than there are live peer pods.
    `_replicas_unidentified` is subtracted, because `_replicas_streaming` includes it -- a
    streaming connection that maps to no pod would otherwise mask a genuinely missing replica
    one-for-one, silencing the alert at exactly the moment the topology view stopped being
    trustworthy. The 15m window rides out a rolling restart, and an apiserver blip cannot fire it
    because a failed pod list leaves the gauges unchanged rather than publishing a zero.

    This rule is **omitted from the rendered file** when `ha.agent.cascadingReplication` is on: a
    cascaded child streams from a peer and never reaches this primary's `pg_stat_replication`, so
    the agent does not measure the expected count there and the comparison could never be true.
    An alert that cannot fire reads as coverage while providing none -- the same failure mode
    #289's 16Gi slot threshold had -- so it is left out rather than shipped inert.

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


- **Topology comes from `pg_stat_replication`; `repmgr.nodes` is retired in native mode
  (#288).** `native` can now run a real multi-node cluster -- it could not before, at any
  `replicaCount > 0`.

  The blocker was never the topology read. It was the bootstrap: `init-repmgr.sh` polled
  `repmgr.nodes` for a primary to register itself, which nothing does under native, so every
  standby burned the ~240s timeout and sat in `Init:CrashLoopBackOff` forever while the primary
  came up fine. The init container now exits immediately under native, and **the agent owns the
  bootstrap**: the lease holder `initdb`s, every other pod waits and then clones with
  `pg_basebackup` through its own pre-created slot (#289). Whether to `initdb` is a cluster-wide
  decision and only the lease can make it happen exactly once -- if each pod decided for itself,
  every one would create its own cluster with its own `system_identifier` and `assertSameCluster`
  would refuse to rejoin any of them.

  Topology now reads the primary's live connection list. `repmgr.nodes` was a *cache* of
  self-reported metadata whose rows outlived the pods that wrote them (#139); a departed pod is
  simply absent from `pg_stat_replication`, so there is no durable row to strand. Rows map back
  to pods by `application_name` -- native now writes the pod name into `primary_conninfo` -- with
  the ordinal-named replication slot as a fallback for any standby cloned before this change,
  which still dials with libpq's default `walreceiver`. Exported as
  `pg_ha_agent_replicas_streaming`, `..._replicas_expected` and `..._replicas_unidentified`.
  Observe-only, deliberately: a standby's row vanishes the instant it disconnects, which is
  exactly the failover moment, so nothing in the promotion decision may consume it.

  A native cluster now has **no repmgr extension and no `repmgr.nodes` at all**. The repmgr
  database and role remain -- the agent authenticates as that role for every probe and for
  `pg_basebackup` -- and renaming them is #291. The pre-agent stale-primary guard is skipped
  under native too: it rejoins and re-clones through the repmgr CLI on a peer scan with no notion
  of the lease.

  The `#297` promote-registration gate is skipped rather than replaced. It guards against
  promoting a node no survivor can `repmgr standby follow`, which is a constraint of resolving an
  upstream by `node_id`; native follows by conninfo, so a native primary is followable the moment
  it promotes.

  Still EXPERIMENTAL. `cascadingReplication` remains render-rejected with native (slot ownership
  is primary-only), and an existing repmgr cluster cannot be flipped in place yet (#292). CI now
  runs 8 suites against both mechanisms (56 legs, up from 40).

  **Four of #288's changes reach `mechanism: repmgr` installs too**, i.e. every upgrade on the
  default path, not just native ones:

  - **Restore provenance is now a failover tiebreaker, ranked ABOVE LSN.** A volume restored
    more recently than its peers wins the election even when a peer holds more WAL -- without
    it, a PITR was routinely undone: the restored node is deliberately *behind*, so the stale
    peer promoted and the restored history was discarded (observed live). The claim expires
    once the restored node promotes and the highwater marker records its timeline, and a
    rewind or re-clone removes the record outright. Requires reachability: an unreachable
    restored peer gets no say in who serves.
  - **A stalled standby is now rewound or re-cloned automatically.** A standby with no
    walreceiver, no replay progress and a peer on a newer timeline is rejoined after ~3
    minutes (`RejoinForward`), where previously nothing escalated a non-holder standby. This
    is destructive by design -- it exists for a diverged standby that can never converge --
    so the trigger requires *both* an absent walreceiver and a frozen replay position, which
    is what tells a wedge apart from ordinary archive catch-up via `restore_command`.
  - **The postStart hook retries instead of giving up** when no primary is reachable over TCP,
    so `postgresql.lifecycle.postStart.additionalCommands` is no longer skipped silently on a
    fresh install. It waits up to 20s for a primary; while it waits the container is not yet
    Started, so probes have not begun and the pod is in no Service.
  - **The restore record gained `restoredAt` and `adoptedAt`.** This file is a contract between
    `restore.sh` and the agent: `restoredAt` survives a later *failed* attempt (a mistyped
    retry copies nothing and must not erase provenance), `adoptedAt` is stamped by the agent
    when the cluster adopts the restored history. Unknown keys are ignored on both sides, so a
    newer image and an older agent interoperate.

- **The agent owns physical replication slot lifecycle in native mode (#289).** Under the
  repmgr mechanism repmgr creates, names and attaches slots itself; native mode had nobody
  doing it, and an unowned slot is the most dangerous loose end in the exit -- an orphaned
  slot pins WAL on the primary forever and fills the volume, raising **no error at all**
  until the disk is full. repmgr mode is untouched (repmgr keeps owning slots there).

  Slots are named `pg_ha_slot_<pod ordinal>` -- ordinal-derived so a pod restart reattaches
  to the same slot instead of stranding one, and prefixed so ownership is decidable: the
  agent creates and drops only names it can prove it minted, never an operator's slot or a
  logical slot for a subscription.

  - **Create before clone.** `pg_basebackup` now streams through the node's own named slot,
    created on the upstream *first*, so no WAL gap can open between the base backup starting
    and the walreceiver attaching. Created idempotently via SQL rather than `pg_basebackup
    -C`: `-C` fails outright when the slot already exists (verified), which is the normal
    case for a re-clone -- so `-C` would break exactly the retry path that matters most.
    `primary_slot_name` in the managed fragment holds the slot for the ongoing stream. A
    rewind-based rejoin ensures its slot the same way, rather than relying on the new
    primary's reconcile having already run. The `WHERE NOT EXISTS` create guard is *not*
    atomic (verified: 40 of 40 concurrent pairs race on PostgreSQL 18) and two legitimate
    independent creators exist, so losing that race is treated as success -- the slot
    exists, which is the goal -- while any unrelated failure still surfaces.
  - **Reconcile on every primary tick.** The lease holder creates a slot for every expected
    peer ordinal and drops orphans. Creation is driven by the *expected* pod set
    (`REPMGR_NODE_COUNT`), not observed standbys, because a slot must exist before its
    standby streams; on the `Promote` path it is sequenced **ahead of the routing switch**,
    so surviving standbys never race slot creation when they follow the new primary.
  - **Dropping is decided from the LIVE pod set read from the Kubernetes API**, never from
    `REPMGR_NODE_COUNT`. That variable is baked into each pod's env at render time, so
    during a scale-up rollout every pod that has not rolled yet -- including, typically, the
    ordinal-0 primary, which the StatefulSet rolls last -- still holds the OLD count.
    Deciding ownership from it would make the stale primary classify a brand-new standby's
    just-created slot as a scale-down ghost and drop it, precisely while that standby is
    briefly inactive between `pg_basebackup` finishing and its postmaster reattaching --
    reintroducing the WAL gap this change exists to close. A failed pod list skips the drop
    pass for that tick rather than falling back to a guess.
  - **Never drops an active slot.** The `AND NOT active` predicate lives in the SQL, making
    the guard atomic with the drop -- a read-then-decide in Go would leave a window for a
    standby to reattach in between, and dropping an in-use slot breaks its replication.
    Verified live against a real streaming standby: the drop is refused, no error, and the
    standby keeps streaming. Reclaimed cases are an agent-minted slot for a **departed**
    ordinal, the primary's own slot (it does not stream from itself), and a legacy
    `repmgr_slot_*` for a departed ordinal -- native never streams through one, so it is
    dead weight, and this is what keeps a future repmgr->native migration (#292) from
    leaving a permanent orphan. Legacy slots are scoped to departed ordinals for the same
    reason live ones are: inactivity alone is not evidence a consumer is gone, it is also
    what a routine restart looks like, so a LIVE node's repmgr slot is left alone even
    mid-migration. An empty or failed pod list reclaims nothing but self.
  - **Only the lease holder mutates slots**, and `Decide` returns `NoOp` before any primary
    branch while paused, so a maintenance window cannot drop anything.
  - **`cascadingReplication` + `native` is rejected at render time.** Cascading makes a
    standby an upstream, but slot reconcile runs only on the primary, so a cascading child
    would name a slot that does not exist on its actual upstream and its walreceiver would
    refuse to start. Both flags are independently opt-in, so this forbids nothing that
    worked before.
  - **Observability, and this half is NOT native-only.** Three new gauges --
    `pg_ha_agent_replication_slots`, `..._replication_slots_inactive`,
    `..._replication_slot_max_retained_wal_bytes` -- plus two `PrometheusRule` alerts:
    `PGHAReplicationSlotRetainingWAL` (critical, over
    `repmgr.agent.monitoring.prometheusRule.slotRetainedWALBytes`, default 16Gi, for 15m)
    and `PGHAReplicationSlotInactive` (warning, 1h). This is the page that turns a silent
    disk-fill into a signal.

    Slot *ownership* is native-mode mechanics, but slot *observation* is not gated on the
    mechanism: the primary publishes these gauges under `repmgr` too. Repmgr mode has slots
    as well (`repmgr_slot_*`), they pin WAL in exactly the same silent way, and the chart
    renders these rules for every agent-mode release -- so gauges that only moved under
    `native` would have shipped an alert that can never fire, which reads as coverage while
    providing none. The agent still never *touches* a slot under the repmgr mechanism
    (repmgr owns lifecycle there); it reports what it sees, so a sustained breach in repmgr
    mode is the operator's to resolve.

    Aggregates rather than per-slot labels on purpose: this metrics surface is hand-written
    text with no per-series lifecycle, so a label per slot would leak a stale series on
    every drop.

  A second review pass corrected several defects in the above before release, each verified
  against real PostgreSQL 18 rather than reasoned about:

  - **The flagship retained-WAL alert could never fire.** The image sets
    `max_slot_wal_keep_size = 4GB` at initdb, so PostgreSQL never lets a slot fill the
    volume -- it INVALIDATES the slot instead (`wal_status = 'lost'`), and the standby behind
    it can then only recover by a full re-clone. Invalidation also nulls `restart_lsn`, so the
    retained-bytes gauge **collapses to zero at the exact moment the slot dies**: the metric
    inverts precisely when the failure lands. The default threshold was also 16Gi -- above
    both the 4GB cap and the default 10Gi data volume. Fixed by adding
    `pg_ha_agent_replication_slots_invalidated` and a `PGHAReplicationSlotInvalidated`
    (critical, 5m) rule for the outcome, and lowering `slotRetainedWALBytes` to **3Gi** so the
    retained-WAL rule works as the early warning ahead of it. A render-time test now fails if
    the default is ever raised to or above the cap.
  - **`IsDuplicateSlot` could never match.** `pg.OSExec` used `cmd.Output()`, and
    `exec.ExitError.Error()` renders only "exit status N" -- psql writes its diagnostics to
    stderr. So the create/create race documented above surfaced as a per-tick warning rather
    than the no-op it should be, and every probe failure anywhere in the agent logged an
    opaque "exit status 1" instead of PostgreSQL's reason. The production Exec now folds
    stderr into the error (stdout stays clean, since callers parse it as query output).
  - **A demoted primary kept publishing stale slot gauges.** Only the primary observes slots,
    but nothing retracted the figures on demotion -- and the alerts aggregate with `max()`
    across the release, so one ex-primary latched them on for the rest of its process
    lifetime. Non-primary actions now zero the gauges.
  - **An inactive slot reserving nothing is no longer counted as inactive.** A slot minted
    ahead of its standby has a NULL `restart_lsn`; counting it tripped
    `PGHAReplicationSlotInactive` ("so it is accumulating WAL") over a slot accumulating
    nothing -- and native pre-creates one per expected ordinal, so that was its DEFAULT state.
  - **The create and drop passes no longer fight.** Creation ran off `REPMGR_NODE_COUNT` while
    reclaim ran off the live pod set, so any ordinal inside the stale count with no live pod
    was created on one tick and dropped on the next -- for the whole duration of a scale-down
    rollout, since the StatefulSet rolls the ordinal-0 primary last. Creation now skips
    ordinals with no live pod (a pod that exists but is still cloning is already in that set,
    so slots are still minted before anything streams).
  - **Legacy `repmgr_slot_*` reclaim was too narrow to do its job.** Scoping it to departed
    ordinals left a repmgr->native migration (#292) with a permanent orphan for every node
    that survived the migration -- the exact case it exists to clean up. Any legacy slot is
    now reclaimable; the atomic `AND NOT active` in the drop, not the pod set, is what
    protects one still carrying a stream mid-migration.
  - **`Follow` now ensures its slot on the new upstream**, as `Clone` and the rewind rejoin
    already did. It was the one slot-using path that did not, and a walreceiver whose
    `primary_slot_name` is missing does not fall back to slotless streaming -- it errors and
    retries, so the standby streams nothing at all.
  - **A demoted primary now reclaims the slots it minted while it was primary.** Reconcile was
    primary-only, and `pg_basebackup`/`pg_rewind` both exclude `pg_replslot`, so an ex-primary
    kept every `pg_ha_slot_*` it had created -- inactive, since those standbys had moved to the
    new primary -- and an inactive slot restricts WAL removal on a standby exactly as it does
    on a primary, so its own `pg_wal` grew until `max_slot_wal_keep_size` invalidated them.
    Nor did it self-heal on a re-promotion: those ordinals have live pods by then, so the
    primary-side pod-set test reads the leftovers as live peers' slots. The slot pass now runs
    on the standby branch too (ahead of the follow latch, which returns early every tick in
    exactly that steady state). The standby policy needs no pod set: under `native` a standby is
    never an upstream, so every agent-minted slot found locally is a leftover. Listing slots on
    a standby also required a standby-safe query -- `pg_current_wal_lsn()` raises `recovery is in
    progress` there, which had been returning an EMPTY listing and hiding the leftovers -- so it
    now branches on `pg_is_in_recovery()` and uses the last received LSN. Verified on a real
    streaming PostgreSQL 18 pair: the leftover is reported with its reserved WAL, reclaimed, and
    the standby keeps streaming, while an active slot on the primary is still refused.
  - **The promote-path slot pass runs under its own sub-budget.** On the shared fence budget
    (5s on chart defaults, against a 10s per-psql timeout) one slow slot query could consume
    the whole window and leave the promote without its routing switch -- a write outage
    strictly worse than the WAL gap the call prevents.

  Verified end-to-end against real PostgreSQL 18, not only in unit tests: clone through a
  pre-created slot, the slot going active under a live standby, the drop guard refusing that
  active slot without error, an orphaned slot pinning 160MB of WAL after its standby left,
  and reclaim freeing it again.

- **`repmgr.agent.mechanism`: an experimental native HA mechanism, alongside repmgr
  (#287).** repmgr ([upstream development has stalled](https://github.com/EnterpriseDB/repmgr))
  is still the default and the only supported mechanism; this adds a second implementation
  behind a flag, not a replacement. The agent's reconcile loop imports only the `Mechanism`
  interface, never repmgr directly, and this is the seam that lets one be swapped for the
  other without touching policy (the Lease, the timeline/LSN election, fencing, and Service
  routing are unchanged either way).

  `repmgr.agent.mechanism: native` drives PostgreSQL's own tools directly instead of the
  repmgr CLI: `pg_ctl promote`, `pg_basebackup`, `pg_rewind`, and `primary_conninfo`/
  `standby.signal` written into an agent-owned config fragment inside `PGDATA`. Off by
  default (`mechanism: repmgr`; a default render is byte-identical to 1.x/2.0.0's repmgrd
  removal).

  **EXPERIMENTAL -- do not set `native` in production, and only at
  `postgresql.replicaCount: 0` (a lone primary) until `#288` lands.** `RegisterPrimary`/
  `RegisterStandby`/`Unregister` are no-ops (no topology source at all), and the chart's
  `repmgr-init` init container -- which bootstraps every standby's first clone by polling
  `repmgr.nodes` for the primary to register, before the Go agent ever runs -- has no
  `MECHANISM` awareness. Verified live: with any replicas, that poll times out and every
  standby sits `Init:CrashLoopBackOff` forever; the primary itself comes up and serves fine.
  `#289` (replication slot ownership) has since landed -- see the entry above -- leaving
  #288 as the sole remaining blocker. `#294` tracks promoting `native` to supported.

  Verified that native mode inherits the mechanism-agnostic safety behaviors already in
  reconcile/probe rather than needing its own copies -- in particular the `#297` scale-up
  registration gate, which reads `repmgr.nodes` and is designed to fail open when that read
  errors (an existing, unit-tested contract): native mode has no `repmgr.nodes` at all, so
  the gate is permanently inert under it and native promotes exactly as repmgr mode would if
  the registry were empty, rather than being silently blocked. See the
  [pg README](README.md#replication-mechanics-experimental-287) for the full writeup.

  **Review follow-up:** three real bugs surfaced building this out further and are fixed in
  the same change: (1) `act()` never applied a `Follow`'d config change for a mechanism that
  only writes files -- native's repoint was silently inert until an unrelated restart/reload
  happened to pick it up; `act()` now reloads the postmaster after every successful `Follow`
  (a harmless no-op for repmgr, which already applies its own repoint). (2) `Clone` used
  `pg_basebackup -R`, writing `primary_conninfo` into `postgresql.auto.conf` -- a second,
  higher-precedence location than the managed fragment `Follow` writes to (auto.conf is
  included last), so a standby re-pointed to a new upstream after its initial clone would
  keep silently streaming from the original source. `Clone` now calls `Follow` itself instead
  of `-R`, keeping one authoritative location. (3) `GenerateConfig` unconditionally wrote an
  empty `primary_conninfo`, run once per agent process boot -- including every pod restart on
  an already-following standby, not just a fresh node -- so a routine restart silently
  interrupted replication until the next tick's `Follow` re-established it; it now preserves
  whatever conninfo is already on disk. Also: `ensureInclude`'s file close error was
  previously dropped (deferred and ignored); it is now checked and wrapped.

### Changed

- **`pg.repmgrImage` is now `pg.haImage`.** The helper resolves `.Values.ha.image` and always
  did; the name had been a lie since #290 renamed the image to `cagriekin/pg-ha` and #294 deleted
  repmgr from it. It is the helper every workload's `image:` goes through, so a reader checking
  which image a container runs met the wrong name first. Renamed across all 10 call sites, along
  with ~35 comments and two operator-facing messages that still said "the repmgr image" --
  including the major-mismatch `fail`, which told operators the server "runs from the repmgr
  image". Renders are byte-identical apart from one intended admission-policy message.
- **Test-suite cleanup: the inert mechanism branches are gone.** `chart_mechanism()` always
  answered `native` after #294, so five suites carried `if native` gates whose `else` arms
  asserted repmgr-mode properties (`repmgr.nodes` rows) against clusters that have no such table
  -- unreachable code that read as coverage. The gates, the dead arms and the helper itself are
  deleted. `values-config-repmgr.yaml` is now `values-config-ha.yaml` for the same reason.
- **pgvector's unit suite no longer carries same-named copies of pg's.** `guards_test.yaml` there
  held a reworded subset of pg's cases (24 of 76) under pg's filename: it read as a mirror while
  being free to drift, and nothing checked. pg and pgvector templates are byte-identical by gate
  and pgvector's are symlinks into pg's, so pg's suite already covers the shared template logic;
  what belongs in pgvector is chart-specific. The two files are renamed `pgvector_*`, and
  `scripts/helm-unittest-charts.sh` now REFUSES a pgvector unit file that shares a name with a pg
  one without being a symlink to it.


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

### Fixed

- **The durable-timeline repair backs off on failure, heartbeats, and no longer logs a
  credential (#298 review).** Three defects in the restartpoint check added above:

  - A restartpoint that ERRORED retried on the normal 15-second interval, while only one that
    ran and did not move the timeline backed off to a minute. The commonest error is the
    10-second budget expiring on a restartpoint that is genuinely slow -- a full flush of dirty
    shared buffers on a standby saturated with replay I/O -- and cutting `psql` short does not
    abort the server-side checkpoint, so the node took a fresh forced flush every 15 seconds
    indefinitely: exactly the hammering the backoff was introduced to stop, reached by the one
    path that skipped it. The confirmation read failing (usually because that same flush ate
    the shared budget) had the same hole. Both now back off.
  - The check blocked the reconcile tick for up to its whole budget with no `metr.Beat()`.
    `tick` heartbeats once, at its top, and `/healthz` goes stale at `reconcileInterval * 3`
    (15s on chart defaults), at which point the kubelet SIGKILLs PID 1 and the postmaster it
    supervises. `verifyTLSActive` runs three lines earlier in the same tick and already allows
    itself 15 seconds; the two throttles (60s and 15s) coincide every fourth check, so the sum
    is reached in ordinary operation. It now beats before the checkpoint, the same way the
    clone path does.
  - The repmgr migration logged the inherited `primary_conninfo` verbatim in three places.
    That value is not the agent's own passwordless conninfo -- it comes out of
    `postgresql.auto.conf`, written by repmgr (whose `repmgr.conf` conninfo carries
    `password=`) or by a hand-run `ALTER SYSTEM` -- so a replication credential could reach
    the pod log and every sink downstream of it. It is redacted now. The slot pre-create also
    takes the process context, so a shutdown during the retry budget exits instead of being
    SIGKILLed.

- **A standby now records the timeline it is streaming, so a migrated cluster can fail over
  (#298, found by the first live run).** The promotion guards read DURABLE state, but a standby's
  control-file timeline advances only at a restartpoint -- up to `checkpoint_timeout` later, five
  minutes on chart defaults. So a node that has followed a promotion streams the new timeline
  while `pg_control` still records the old one. The reported timeline hides that *while* the
  walreceiver is attached, because it takes the greatest including `received_tli` -- and that term
  vanishes the instant the upstream dies, which is exactly when promotion is decided. The node
  then reads as below the marker highwater and refuses to promote.

  On a freshly migrated cluster every standby is in that state at once, so deleting the primary
  produced `refuse to promote: timeline below recorded highwater (#125)` on all of them, the
  ex-primary restarted (its data directory is intact) and reclaimed the lease, and the cluster
  could not fail over at all until something forced a checkpoint by hand. The agent now forces a
  restartpoint when a streaming standby's durable timeline lags -- measured against the same
  expression the guard reads, not the checkpoint timeline alone -- once every 15 seconds, backed
  off to once a minute when a restartpoint does not move it, and best-effort. The migration
  suite goes from a timeout mid-roll to 47/47, including a real post-migration failover.

- **The in-place repmgrd -> 2.0.0 roll no longer deadlocks (#298, found by the first live run).**
  Rolling a live `failoverMode: repmgrd` release stalled at the first replaced pod and never
  recovered. The StatefulSet replaces the highest ordinal first, so that pod comes up as the only
  agent in the cluster while the real primary is still a 1.x pod running repmgrd and holding no
  lease -- and the migration step that clears repmgr's recovery config assumed "the effective
  upstream is preserved either way, the agent derives it from the lease". With no agent-held
  lease there is no leader to derive it from: the reconcile loop returned `Wait` ("standby but no
  known leader"), nothing ever wrote the agent's own fragment, and the node was left with
  `standby.signal` and no `primary_conninfo` at all. PostgreSQL logged `specified neither
  "primary_conninfo" nor "restore_command"`, it never streamed, readiness never passed, the
  rollout stopped with the cluster half-migrated, and the agent livelocked on a 10s cycle
  (acquire the lease, refuse to promote on the equal-timeline guard, release, wait, re-acquire).

  The migration now carries the inherited upstream forward into the agent's own fragment instead
  of only deleting it, and pre-creates this node's replication slot on that upstream first --
  because the fragment the agent writes names its own ordinal slot, which does not exist on a
  still-repmgrd primary, and a walreceiver whose named slot is missing does not fall back to
  slotless streaming (it fails with `replication slot "..." does not exist` on a loop, which is
  the same stalled rollout by another route). The pre-create is retried and, if it still fails,
  logged at ERROR naming the slot to create by hand -- it is not self-healing, because the
  fragment the agent writes names that slot regardless. The carry-forward is skipped on a data
  directory with no `standby.signal`: a node repmgr promoted keeps a stale `primary_conninfo` in
  `postgresql.auto.conf`, and seeding it would point a primary at a dead upstream and strand an
  inactive slot pinning WAL on a peer. The
  KinD suite for this path now completes 42/43 where it previously timed out during the roll with
  no result at all.

- **A streaming standby is no longer reported on a stale timeline (#298, found by the KinD
  suites).** `Prober.StandbyTimeline` took the GREATER of the control file's checkpoint timeline
  and its `min_recovery_end_timeline`, on the stated premise that the latter "advances as the
  standby replays the timeline switch -- ahead of the checkpoint". The first live run of this code
  disproved that: after a controlled switchover the demoted node was healthy and streaming the
  new timeline (`pg_stat_wal_receiver.received_tli = 3`, and the primary listing it as
  `streaming`), while **both** control-file values still read 2 -- and both jumped to 3 only when
  a restartpoint was forced. So the reported timeline lagged by up to `checkpoint_timeout`
  (5 minutes on chart defaults).

  Two things that cost: the control API's `/v1/cluster` misreported a live member's timeline for
  minutes, and the reconcile decision computes "a peer is on a newer timeline" from this value --
  observed once as a `RejoinForward` chosen against a node that was already on the newer timeline
  and needed no rejoin, which is the needless-rewind hazard the classifier exists to avoid.
  `received_tli` is now a third input to the same `GREATEST`, which is safe precisely because a
  max can only raise the answer: when the walreceiver row is gone the term is 0 and the two
  persistent sources decide, exactly as before. Whether a node holds all committed WAL remains
  the separate LSN comparison, so this does not loosen the highwater guard.

- **The bootstrap's role verification also requires the role to be able to log in (#298
  review, image).** `rolpassword IS NOT NULL` alone is not the property that matters: what the
  block is really asking is whether the agent can authenticate as the role, and a role holding a
  password with no LOGIN satisfies a password-only check while nothing can connect as it. Both
  arms now also require `rolcanlogin`; every role the bootstrap creates itself is a `CREATE
  USER`, i.e. LOGIN, so the normal path is unaffected. Defence in depth rather than a reachable
  wedge being closed: the bootstrap runs only on an EMPTY data directory, so the only roles it
  can meet are the ones `initdb` just made. In particular this is *not* about a `pg_`-prefixed
  `ha.username`: tested against `postgres:18-trixie`, both `CREATE USER "pg_monitor"` and the
  paired `ALTER USER` are refused (`Cannot alter reserved roles`), so `rolpassword` stays NULL
  and the password arm already caught that case.

- **A non-string image tag is now refused at render time (#298 review, etcd 0.1.14).** An unquoted YAML tag
  (`tag: 18.0`) is a number, not text, and this took three rounds to get right: `printf "%s:%s"`
  rendered Go's error verb (`repo:%!s(float64=18)`), which at least failed loudly at the kubelet;
  `| toString` then made it *silent and wrong*, because Go renders a float in canonical form --
  `18.0` became `repo:18`, a floating patch instead of the pin that was written, and `2.10`
  became `repo:2.1`, a different tag that may well exist. A render-clean manifest deploying an
  image the values file never named is precisely the apply-time hazard the chart's render-time
  guards exist to pull forward, so `pg.image`, `etcd.image` and `pg.validateEtcdBootstrapImage`
  now refuse a non-string tag or digest outright. The refusal names the offending key and the
  shape of the fix -- quote it in a values file, and use `--set-string` on the command line,
  where shell quotes do not help because helm types `--set x.tag=18` as a number regardless --
  but never echoes the *coerced* value back as that fix: `18.0` reaches the chart already
  truncated to `18`, so "quote it as \"18\"" would have prescribed exactly the wrong pin. The
  bootstrap-image guard had to refuse rather than coerce because it renders ahead of every
  `pg.image` call: with `toString` there, the only thing an unquoted `ha.image.tag: 2.0` produced
  was its MISMATCH message naming `cagriekin/pg-ha:2`, a coordinate no values file ever wrote,
  and instructing the operator to copy it onto `etcd.rbac.bootstrapImage` -- the same
  wrong-remediation trap `pg.validateHaImageGeneration` documents. Only the repository is still
  coerced, in both helpers: it is the one half no guard types, and unlike a tag a numeric
  repository cannot silently name a different image that exists.

- **`postgresql.pgHba` entries no longer invert their order across pod starts (#298 review,
  standalone).** The postStart insert was made a single awk pass so the whole list lands in the
  operator's declared order -- which fixed the order WITHIN one batch and left it broken ACROSS
  starts. A rule the hook inserted earlier is itself a non-loopback `host` rule sitting above the
  original anchor, so it BECOMES the anchor; and the insert only carried the entries `grep -qF`
  found missing. So appending a rule to `postgresql.pgHba` in a later `helm upgrade` placed it
  ABOVE every rule declared before it. pg_hba is first-match-wins, so
  `["host all all 10.1.0.0/16 trust", "host all all 10.1.0.0/24 reject"]` -- with the reject
  appended later -- ended up REJECTING the /24 range the operator had trusted: the exact
  inversion the one-pass design was written to prevent, reached through a second `helm upgrade`.
  The hook now strips every declared rule and re-inserts the full list at the anchor, so the
  block is idempotent and its order authoritative on every start whatever it was before, and it
  is skipped entirely when the result is byte-identical. Strip and insert happen in ONE awk pass,
  with the anchor chosen BEFORE the strip (#298 review): a separate strip pre-pass could delete
  the anchor itself, because standalone runs the OFFICIAL postgres image, whose
  docker-entrypoint appends the file's only non-loopback host rule as single-spaced
  `host all all all <method>` -- the exact shape an operator writes. A `postgresql.pgHba` entry
  byte-identical to it was stripped, the insert then found nothing to anchor on, and NOT ONE of
  the entries was applied (the grep-filtered form it replaced at least applied the rest). The
  declared list now simply lands where the colliding rule was. Agent mode was never affected
  (`pgconf.AssemblePgHba` places the whole list itself, #144).

- **The bundled etcd RBAC bootstrap Job accepted a digest-only image pin and rendered an invalid
  reference (#298 review, etcd 0.1.9).** `pg.haImage` now delegates to `pg.image`, which supports
  a tag-less digest pin, and `pg.validateEtcdBootstrapImage` tells the operator to set
  `etcd.rbac.bootstrapImage` to the same repository/tag/digest as `ha.image`. Following that with
  a digest-only pin rendered `cagriekin/pg-ha:@sha256:...` -- the Job's image line concatenated
  `repository ":" tag` unconditionally -- which the kubelet rejects as `InvalidImageName`. The
  Job then never runs, so etcd auth and the per-release tenants are never provisioned, every
  agent's etcd dial is unauthenticated, no node wins the lease, and the release comes up with no
  primary and no write-Service endpoint: precisely the outcome that guard exists to prevent. The
  tag is now omitted when empty, matching `pg.image`.

  The etcd SERVER image carried the same expression and the same bug, which the first pass left
  behind (#298 review, etcd 0.1.10). `etcd.image.digest`'s own values comment offers it as the
  pin that "takes precedence over the tag", so `--set etcd.image.tag=""` is a reference an
  operator can reasonably ask for -- and it rendered `quay.io/coreos/etcd:@sha256:...`. That is
  the larger blast radius of the two: all three etcd pods go `InvalidImageName`, so the agents
  have no DCS at all rather than an unauthenticated one. Both image lines now omit an empty tag,
  and `etcd/tests/unit/render_test.yaml` pins the digest-only render.

  ...and omitting the empty tag traded a loud failure for a silent one, which the third pass
  closes (#298 review, etcd 0.1.11). `tag: "" digest: ""` no longer renders a bare
  `quay.io/coreos/etcd` -- an implicit `:latest`, unpinned across pod restarts, which for the DCS
  means a future etcd MAJOR landing on an existing member's data directory that it refuses to
  start on: all three pods down, no lease, no primary, and nothing said so at install time. The
  previous bare-colon form at least failed as `InvalidImageName`. `etcd.image` is now the single
  place both image lines render through (the two-copy shape is what made the first fix need a
  second pass), and it refuses an empty repository and a tag-less, digest-less reference exactly
  as `pg.image` does -- CLAUDE.md invariant 4, fail at render time rather than at the API server.
  It matters most for `etcd.rbac.bootstrapImage`, whose shipped default already has
  `digest: ""`: in a STANDALONE etcd install `pg.validateEtcdBootstrapImage` is not there to
  force it to equal a `ha.image` that `pg.image` has already refused to leave unpinned, so this
  chart is the only thing that can say no. `etcd/tests/unit/guards_test.yaml` pins both refusals
  and `render_test.yaml` now pins the bootstrap Job's digest-only render too, which was the fix
  that shipped first and had no render test at all.

  ...and the printf that consolidation introduced dropped a shape the expression it replaced
  handled (#298 review, etcd 0.1.12). `printf "%s:%s"` renders Go's `%!s(...)` error verb for a
  NON-STRING tag, while the old `{{ with .tag }}:{{ . }}{{ end }}` printed any scalar --
  and `values.schema.json` types no `image.tag` in either chart, so an unquoted YAML scalar is
  reachable: `etcd.image.tag: 3.5`, `busyboxImage.tag: 1.37` or `pgpool.image.tag: 4.6` (the
  spelling an operator reaches for) arrives as a float64 and rendered
  `quay.io/coreos/etcd:%!s(float64=3.5)`. That is a render-CLEAN manifest the kubelet then
  rejects as `InvalidImageName` -- exactly the failure both `etcd.image` and `pg.image` exist to
  pull forward to render time, reached through the guard rather than around it. Both helpers now
  REFUSE a non-string tag or digest (`toString` was the first attempt and is documented as a
  defect of its own under 2.0.0 above -- it made the failure silent and wrong),
  `etcd/tests/unit/render_test.yaml` pins the non-string-tag refusal, and
  `pg/tests/test-template.sh` asserts no `%!s(` ever reaches an `image:` line.
  `etcd/README.md` also now states the tag/digest rules on the `rbac.bootstrapImage` row, not
  only on the server `image` one.

- **`ha.agent: null` in standalone mode gave a raw nil pointer instead of the named error (#298
  review).** The `ha.agent is required when ha.enabled=true` guard was added for exactly this,
  but scoped to `pg.agentMode == true` -- correct in itself, since standalone runs no agent --
  while `statefulset.yaml`'s control-API validation dereferences `.Values.ha.agent.control`
  unconditionally. So `--set ha.enabled=false --set ha.agent=null` died with
  `nil pointer evaluating interface {}.control`, the message class that guard replaced, on the
  other half of the mode axis. `control`, its `restore` sub-block, and every sub-block
  dereferenced two levels deep (`control.tls`, `restore.admissionPolicy`) now default to an empty
  dict, which reads as "nothing set" -- so standalone renders and agent mode gets the named
  `fail` rather than a nil-pointer from one level further in (#298 review). `pg.controlRestoreEnabled`
  got the same treatment: `rbac.yaml` evaluates it before `statefulset.yaml`'s guards can speak,
  so `--set ha.agent.control=null` still died there with
  `nil pointer evaluating interface {}.enabled`. Agent mode still gets the
  `ha.agent is required` message first, so nothing there changes.

- **The transient bootstrap postmaster is stopped when `pg_ctl -w start` times out
  (`images/pg-ha/entrypoint.sh`, #298 review).** Both `stop` calls in `bootstrap_initdb` were
  hardened against exiting under `set -e` with a daemonized postmaster still alive; the `start`
  was the third call site and got neither guard. `pg_ctl -w start` daemonizes and then waits, and
  it exits non-zero at `PGCTLTIMEOUT` (60s -- nothing in this image lowers it) with the
  postmaster STILL RUNNING. Bare under `set -e` that left an orphan holding this script's stdout,
  which in agent mode (where the script is a captured child) blocks `act()` with `opMu` held for
  the whole `initdbBudget`, and which satisfies the chart's startupProbe -- `pg_isready` over the
  unix socket -- while listening on no TCP address, retiring the startup grace for a cluster that
  is not bootstrapped. It now escalates to an immediate stop and exits without the completion
  sentinel, so the next start discards the directory and re-creates it.

- **`bootstrap_initdb`'s repmgr role creation is paired with an `ALTER USER`, symmetrically with
  the superuser's (`images/pg-ha/entrypoint.sh`, #298 review).** psql runs without
  `ON_ERROR_STOP`, which is why the `POSTGRES_USER` block combines `CREATE USER` with an `ALTER
  USER ... PASSWORD`: a CREATE that fails because the role already exists still lets the ALTER
  set the password. The repmgr block had only the CREATE, so a pre-existing role would be left
  with the wrong password (or none), the failure swallowed by the deliberate `|| true`, and the
  `rolpassword IS NOT NULL` verification would then discard an otherwise-fine data directory
  rather than repair it. Defence in depth -- the collision guard and the reserved-name check
  cover the paths reachable today.

- **`scripts/check-template-parity.sh` now also refuses a pgvector template that is a COPY
  rather than a symlink (#298 review).** `diff -r` dereferences symlinks, so it compares content
  -- which is the point, but it means a template replaced by a byte-identical regular file keeps
  the gate green while being free to drift on the next edit, and pgvector has no KinD suites to
  notice. `scripts/helm-unittest-charts.sh` added exactly this copy-vs-symlink check for
  `pgvector/tests/unit`; the templates, where the whole inherited integration coverage lives,
  had no equivalent.


- **A demote that ended in a reaped SIGKILL is no longer read as "the writer may still be alive"
  (#298 review, agent).** `ChildPostmaster.Stop` returns `context.DeadlineExceeded` on TWO
  different arms: the one where the deadline expired, the SIGKILL landed and the child was
  **reaped** (`p.clear()` has run -- the writer is provably gone), and the one where SIGKILL was
  undeliverable to a process in uninterruptible sleep and the handle is deliberately kept
  ("leaving it supervised"). `err != nil` cannot separate them, and only `RestartLocal` and the
  control-API restart applied the test that can -- `errors.Is(err, context.DeadlineExceeded) &&
  !sup.Running()`. The two paths that did not both refuse to release the lease on the strength of
  it:

  - The **lost-leadership fence**. A SIGQUIT'd primary with a large shutdown checkpoint routinely
    outlives `renewDeadline` (10s on chart defaults), so on the ordinary apiserver-partition fence
    the demote completed, the postmaster was killed and reaped, and the branch still kept
    `servingRW` set. `SafeToRelease` then vetoed the release: the peer waited out the full
    `leaseDuration` instead of taking an immediate handoff, and the log said "this node may still
    be a read-write primary" about a pod with nothing running on it.
  - The **planned shutdown** (SIGTERM). A Fast/SIGINT shutdown of a large busy database
    legitimately outruns the 30s budget; the escalation killed and reaped it, and
    `clearServingRWForPlannedStepDown` was skipped, so the K8s backend kept the Lease and every
    peer paid a full `leaseDuration` of write outage -- exactly the outage the `dcsDone`
    shutdown ordering exists to eliminate, defeated on the ordinary slow-checkpoint termination.
    There is no next tick here to re-derive the latch.

  The discrimination is now one helper, `agent.stopProvedDead`, shared by all four call sites so
  they cannot drift again. Anything that is NOT a deadline expiry is still never treated as proof.

  The `ReleaseLease` and `Switchover` step-downs consult it too, for the latch only. They keep
  ABANDONING the handoff on any demote error -- a SIGKILL'd postmaster skipped its shutdown
  checkpoint, so promoting a peer now would drop WAL this node had written but not yet streamed,
  while abandoning loses nothing and the node comes back on the next tick -- but a reaped kill
  still proves the writer is gone, so `servingRW` is cleared instead of being left armed. Left
  armed it was the same spurious fence: a `Fast`/SIGINT shutdown of a loaded primary routinely
  outruns `renewDeadline` (10s on chart defaults), and a lease lapse before the next tick then ran
  `OnLost`'s fence branch on a pod with nothing running, incrementing `pg_ha_agent_fences_total`
  and paging `PGHAAgentFlapping`.

- **`POST /v1/reinitialize` re-checks the replica-only gate inside the reconcile goroutine (#298
  review, agent).** `handleReinitialize` gates carefully -- a live DCS lease read, the durable
  marker, the snapshot role -- but every one of those runs on the HTTP goroutine BEFORE the intent
  is queued, and the run loop's `select` may serve a tick first (the request also deliberately
  stays queued after the caller's context expires, so the window is not bounded by the HTTP
  timeout). The scenario is the ordinary failover: an operator reinitializes a healthy standby,
  the primary dies in that window, this node wins the lease and the next tick promotes it -- and
  `runIntent` then stopped and wiped the data directory of the cluster's **new primary**.
  `WipeDataDir`'s `postmaster.pid` interlock cannot help, because the demote immediately above
  removes it, and the highwater marker is left naming an empty node -- the unrecoverable outcome
  the handler's own comment cites. `runIntent` runs in the reconcile loop's own `select` -- the
  same goroutine as `tick()` -- and holds `opMu` for the whole call, so re-asserting the gate
  there is atomic against both the tick that promotes and the `OnLost` fence; the refusal
  precedes the stop, so a refused request touches nothing.

- **The control-API restart asserts the on-disk role before starting the postmaster (#298 review,
  agent).** `sup.Start` is a bare `postgres -D PGDATA`: it neither creates nor removes
  `standby.signal`, and this was the one start path that did not state the role it intended --
  every start the reconcile loop performs goes through `StartLocal` (which asserts the signal for
  standby-state data) or `StartRecovery` (which asserts it for a NON-HOLDER's primary-state data,
  "so its true position is observable without risking a second writer"). A fenced ex-primary is
  stopped with primary-state data, no `standby.signal` and no lease, and publishes role
  `unknown`, so `handleIntent`'s force gate -- which only bites while the node holds the lease --
  waved an unforced `POST /v1/restart` straight through and brought the node back READ-WRITE on
  the old timeline beside the real primary, until the next tick's `DemoteFence`: a two-writer
  window of up to one reconcile interval, reachable through the restore runbook's own hint ("POST
  /v1/restart to bring it back"), and reachable again on a directory whose signal was lost inside
  `RejoinForceRewind`'s window. The new assertion only ever ADDS the signal, never removes one:
  removing it would be a new way to start a node read-write without the `#125` highwater guard,
  so a lease holder's primary-state directory is left byte-identically alone, and an unreadable
  control file pins the node read-only rather than guessing.


- **`pg.haImage` resolves through `pg.image` instead of its own `printf` (#298 review).** It is the
  helper every HA workload's `image:` goes through -- the `postgresql` container, `repmgr-init`,
  the pgBackRest sidecar and bootstrap init container, the restore Job, the pgBackRest validation
  CronJob, and the CEL image pin in the restore admission policy -- and it carried a private copy
  of the exact `printf "%s:%s" repo tag` that `pg.image`'s own comment documents as broken. A
  cleared `ha.image.tag` (`tag:` with no value, which is what a values-file merge produces)
  rendered `cagriekin/pg-ha:%!s(<nil>)`, and clearing it while keeping the documented
  `ha.image.digest` supply-chain pin rendered `cagriekin/pg-ha:@sha256:...`; containerd rejects
  both as `InvalidImageName`, so every pod of the release failed to start on a render Helm had
  accepted. Delegating picks up `pg.image`'s guards -- an empty repository is refused, neither
  tag nor digest is refused (an implicit `:latest` on a StatefulSet that already holds a
  PostgreSQL major), and a digest-only pin renders `repository@digest`. Output is byte-identical
  for every input that worked before.

- **An empty `primary_conninfo` is left alone (#298 review, agent).** An empty value is a setting
  -- "do not stream" -- and it is the one `RemoveRecoveryConfig` refuses to overwrite for exactly
  that reason. The #308 `dbname` patch had no guard for it, so it produced a non-empty conninfo
  carrying no host, port or user, which libpq resolves over the local unix socket -- the
  standby's WAL receiver dialing its own postmaster -- and `changed=true` made the Follow branch
  reload it. Reachable after boot, since the strip is a one-time preflight: an operator pausing
  replication with `ALTER SYSTEM SET primary_conninfo = ''` had it silently un-paused into a
  self-connect loop, permanently, because the `dbname` check then matched and the line was never
  revisited.

- **`WaitJobGone` retries a transient apiserver error instead of giving up on it (#298 review,
  agent).** Only `IsNotFound` should end a wait loop; every other error now retries until the
  deadline and is named in the failure. One 500 in the window between the delete and the
  re-create aborted the wait, the error reached the control-API caller, and the operator's
  natural retry then hit "restore Job ... already exists: delete it first" -- leaving them to
  clean up by hand a Job the agent was already removing.

- **The etcd RBAC bootstrap probes every endpoint, and its backoff honours cancellation (#298
  review, agent).** It health-checked `endpoints[0]` only, so in the shared-etcd topology a
  healthy quorum looked dead whenever that one member was the one still forming: the post-install
  hook burned its full 90s and failed the release against a cluster answering fine on the other
  two. Every RBAC call afterwards goes through the balanced client, so any member answering is
  sufficient. The retry slept unconditionally, which also meant `helm upgrade --timeout` could
  not interrupt it.

- **A 1.x `ha.image.repository` is now refused at render time (#298 review).** The 2.0.0
  generation guard checked only the image TAG (`trixie-*`), and the commonest half-carried pin
  sets only the repository: a values file with `repmgr.image: {repository: cagriekin/repmgr,
  pullPolicy: Always}` merged through the alias while `ha.image.tag` stayed at the 2.0.0 default,
  and the render was clean -- every workload came out as `cagriekin/repmgr:2.0.0-pg18`, a
  coordinate that has never existed (that registry is frozen at its last `trixie-` tag), so every
  pod went ImagePullBackOff after an upgrade Helm had accepted. With the bundled etcd it was
  worse: the only error was `pg.validateEtcdBootstrapImage` telling the operator to point
  `etcd.rbac.bootstrapImage` at the same reference, which pinned *both* workloads to the missing
  image. Matched on the repository's last path segment, so a mirror is caught too; retag a
  private mirror of the 2.x image to something other than `.../repmgr`.

- **`ha.agent.cascadingReplication` is typed in `values.schema.json` (#298 review).** Its sibling
  `syncReplicationSlots` was; this was not, and `additionalProperties` is deliberately open. A
  quoted `"false"` (or a `--set-string`) is a non-empty string and therefore truthy to a Go
  template, so it silently enabled the cascading branch in the StatefulSet *and* dropped the
  `PGHAReplicasNotStreaming` alert, which is omitted under cascading because `replicas_expected`
  is 0 there. Both spellings (`ha.*` and the `repmgr.*` alias) are typed.

- **The dead HA arm in `pgpool-configmap.yaml` is gone (#298 review).** `pg.agentMode` is defined
  as `.Values.ha.enabled`, so the `{{ else if .Values.ha.enabled }}` that rendered the old
  per-pod `backend_hostname{0..N}` list could never be reached. The render is byte-identical; the
  branch is removed rather than left to read as coverage a maintainer would edit to no effect.

- **The `PGBACKREST_STANZA` guard runs before `initdb` (#298 review, image).** It sat beside the
  `archive_command` it protects, i.e. after the cluster had already been created and the base
  GUCs written -- so a stanza outside `[A-Za-z0-9_-]` left `PG_VERSION` present with no
  completion sentinel, and in the image's `postgres` mode the torn-bootstrap discard then re-ran
  the whole `initdb` on every restart. It is now checked with the credentials, before the first
  write to the volume.

- **`PGHAReplicationSlotInvalidated` could no longer fire for the case it was written for, so a
  new alert covers it (#298 review).** The invalidated-slot recycle observes `wal_status='lost'`
  and repairs the slot in the *same* tick, so the gauge is 1 for at most one scrape and 0
  thereafter -- the rule's `for: 5m` never elapses. That is the dead-alert failure this chart has
  shipped more than once, so the recycle now increments a counter,
  `pg_ha_agent_replication_slots_recycled_total`, and a new `PGHAReplicationSlotRecycled` rule
  alerts on its rate (the same shape the marker-tamper rule uses, for the same reason: the event
  is a transition and needs no clearing logic). The gauge rule stays, because an *active*
  invalidated slot, or one whose ordinal has no live pod, is never recycled. The new alert states
  the part that matters operationally: recycling restores the SLOT, not the STANDBY, which still
  needs a full re-clone -- the recycle only removes the dead slot that would have made the
  re-clone itself fail.

- **A failed slot recycle no longer points `synchronized_standby_slots` at a slot that does not
  exist (#298 review, agent).** The drop and the create are two statements in one call and
  `pg_drop_replication_slot` is not transactional, so a deadline, a dropped connection or an
  exhausted `max_replication_slots` between them removes the slot and returns an error -- while
  #308's owned-slot set is derived from the pre-pass snapshot, which still lists the name. Naming
  a nonexistent slot in that GUC is the one thing it must never do: the primary then refuses to
  release WAL and logs about it every checkpoint until the next tick re-reads the real list. Such
  a name is now dropped from the snapshot.

- **The control API's response-write deadline covers its detached work (#298 review, agent).** The
  restore-Job delete runs on a context detached from the request with its own budget, so the
  per-request ceiling does not bound it, while the reads *before* it are bounded by that ceiling
  and can consume most of it on a slow apiserver. The budget is now supplied by the caller that
  owns the constant rather than assumed to fit, so the two cannot drift.

### Testing

- **The pgHba upgrade test now runs the rendered hook instead of a copy of its logic (#298
  review).** The regression test added alongside the fix above reimplemented the hook's two awk
  passes inline, so editing the template could not fail it -- the same shape as the vacuous
  assertions fixed elsewhere in this section. It now extracts the hook from `helm template` and
  executes it, and refuses to assert anything if the extraction comes back empty.

  That refusal did not actually refuse (#298 review): it called `bad`, which this suite does not
  define -- helpers.sh has `fail` -- so bash printed "bad: command not found", the caller's
  `if _hba_hook && _hba_hook` swallowed the status, `FAIL_COUNT` stayed 0 and every assertion
  below was SKIPPED with the suite still reporting green. A `bash -n` failure on the extracted
  block skipped just as silently. Both now register a failure. Mutation-proven: emptying the
  extraction turns the suite red instead of shrinking it. The case where a declared rule collides
  with the anchor, and the standalone/agent-mode `--set ha.agent[.control[.restore]]=null`
  renders, are covered too.

- **Two more image-suite assertions were vacuous, both mutation-proven (#298 review).** The
  completion-sentinel check searched for the sentinel's NAME anywhere in `entrypoint.sh`, and that
  name appears twice -- the write in `bootstrap_initdb` and a read in `bootstrap_or_discard_torn`.
  With the write deleted, `grep | head -1` fell through to the read, which is also below the
  bootstrap's last step, so *both* assertions passed while nothing wrote a sentinel at all. The
  consequence of the regression it was meant to catch: the agent's torn-bootstrap discard keys on
  marker-present plus sentinel-absent, so it would wipe a healthy freshly-bootstrapped data
  directory on the next boot. The grep is now anchored to the write itself. Separately, the SCRAM
  check pulled three lines of context from *each* `psql` heredoc opener and concatenated them, so
  "the SET is present" and "this role's CREATE USER is present" could come from different blocks:
  removing `SET password_encryption` from only the repmgr heredoc left the assertion named for the
  repmgr user green, because the postgres heredoc supplied it. `password_encryption` is a SESSION
  setting, so the property is per-role and the test now extracts each heredoc separately.

- **Two assertion helpers were silently vacuous, in all three charts' shell suites (#298
  review).** `assert_contains` / `assert_not_contains` passed the needle to `grep -q` without a
  `--` terminator, so any needle beginning with a dash was parsed as an OPTION: grep matched
  nothing and exited non-zero, which made `assert_contains` fail spuriously and -- far worse --
  made `assert_not_contains` **pass unconditionally**. Every assertion of the natural form
  `- alert: X` (does this PrometheusRule rule render?) proved nothing. Fixed in `pg`, `kafka` and
  `redis`; fixing it immediately exposed one over-broad pg assertion that had never really run
  (it looked for a blanket `podSelector` anywhere in the NetworkPolicy, when the flag it tested
  governs only the 5432 ingress -- the agent metrics rule carries a deliberate one), now replaced
  with a comparison between the two renders. A new `assert_contains_literal` covers needles that
  are verbatim PromQL or YAML rather than patterns, since `[15m]` reads as a character class to a
  regex match.

- **An invalidated replication slot is now recycled instead of accepted (#298 review, agent).**
  `wal_status = 'lost'` means PostgreSQL destroyed the reservation because the slot passed
  `max_slot_wal_keep_size` (4GB in this image), and such a slot can never be acquired again --
  but the existence guard in both slot-ensure paths was satisfied by it, so the create was
  skipped with no error and every recovery route wedged: `Follow` pointed `primary_slot_name` at
  a slot the walreceiver cannot acquire, and the stall escalation handed the same name to
  `pg_basebackup --slot`, which fails at its WAL-stream connect *after* the data directory has
  been renamed aside to an unreaped `.diverged.<ts>` copy. The primary's reclaim pass could not
  help either, since it deliberately keeps any slot whose ordinal still has a live pod -- so the
  standby stayed out of the cluster, burning one preserved copy per attempt, until an operator
  dropped the slot by hand. Both paths now drop the dead reservation first, guarded on `NOT
  active` so only a slot nothing holds is ever removed -- and the primary's own create pass
  counts an invalidated slot as ABSENT, so it actually reaches that recycle instead of
  skipping the create because a (dead) slot by that name exists. A recycle whose create leg
  FAILS also drops the slot from the owned set the same pass returns: the drop and the create
  are one statement pair and `pg_drop_replication_slot` is not transactional, so a failure
  between them removes the slot -- and `synchronized_standby_slots` (#308) is reconciled from
  the pre-pass snapshot, which would otherwise keep naming it, which is the one thing that GUC
  must never do.

- **The cross-cluster guard will still work after 2038 (#298 review, agent).**
  `pg_control_system()` exposes `system_identifier` as a signed `int8` -- PostgreSQL has no
  unsigned types -- while `pg_controldata`, which the agent parses for the *local* identifier,
  prints the same 64 bits unsigned. `initdb` builds the identifier from `tv_sec << 32`, so every
  cluster created from 2038-01-19 on has the high bit set and renders negative over SQL. The
  peer-side parse rejected that, reported "unknown" for every peer, and `assertSameCluster` is
  fail-open on unknown -- so invariant 9 would have silently stopped refusing a misrouted pod
  from a different cluster as a clone or rewind source.

- **Two control-API fixes (#298 review, agent).** `allowedClientCNs` is documented as gating
  every route, but the restore feature-gate middleware sits outside the authorization check and
  so skipped it: a certificate the control CA signed whose CN is *not* on the list got a 501
  naming the release's pgbackrest configuration, where every other route answers 403 -- and the
  denied audit line and rejection counter never fired. Unrecognised CNs now fall through for
  their 403, while an authorized client still gets the actionable "not configured" answer. And
  the response-write deadline now covers the two detached intent legs a `replace` restore runs in
  series after the delete wait; on chart defaults their sum exceeded it, so the operator got a
  dropped connection instead of the Job name and next steps.

- **A read-write observation a demote overtook no longer resurrects the fence latch (#298
  review, agent).** The reconcile tick samples the local role with a multi-second, network-bound
  probe and then stores the derived value outside `opMu`, while the lost-leadership fence demotes
  under `opMu` and clears it -- so a tick that had seen a still-read-write postmaster could land
  its store *after* the fence, leaving the node marked a writer. That was harmless while the
  latch only gated the fence itself (the next tick re-derives it), and stopped being harmless
  once the same latch began gating the lock release: a stale value vetoes a release that was
  safe, so the peer waits out `leaseDuration` instead of milliseconds and the operator reads "may
  still be a read-write primary" about a fence that completed cleanly. Completed demotes now
  carry a generation the derivation checks, and only the read-write direction is gated -- a
  standby or a dead postmaster still clears the latch unconditionally. The generation is
  checked on both sides of the store, and bumped *before* the latch is cleared, because one
  check ahead of it is a check-then-act pair rather than an atomic decision: a demote landing
  between the check and the store slipped through and the stale value stood for a full
  reconcile interval.

- **The leader lock is no longer freed when the fence demote failed (#298 review, both DCS
  backends).** Freeing it is what turns a step-down into a millisecond handoff instead of one at
  TTL expiry, and that is safe on exactly one condition: the demote `OnLost` just performed
  completed. On the wedged-PV path it does not -- SIGKILL cannot reach a postmaster in
  uninterruptible sleep, so `Stop` returns an error and deliberately leaves the child supervised,
  and `OnLost` keeps the node marked read-write precisely because a writer may still be up.
  Handing the lock over in that state gave a peer immediate permission to promote beside a live
  writer, with none of the `LeaseDuration`/TTL margin an expiry-based handoff would have left.
  Both backends now ask the agent first (`Callbacks.SafeToRelease`): the Kubernetes backend skips
  emptying the Lease, and the etcd backend still orphans its session -- so the key expires on its
  own TTL -- but skips the immediate revoke. A completed fence still hands off at once. The veto
  applies only to an iteration that actually HELD the lock: `le.Run` and `runElection` both also
  return for one this node spent as a follower, and there the etcd session key is a queued
  candidate rather than the lock -- keeping it alive fences nothing and, because etcd orders
  candidates by create revision, blocks every peer behind it from becoming leader until its TTL
  runs out.

- **A promote that outlives the lease no longer claims the write Service (#298 review, agent).**
  `pg_ctl -w promote` is bounded only by `PGCTLTIMEOUT` (60s) while the default `LeaseDuration`
  is 15s, so on the ordinary failover case -- a standby with a large unreplayed backlog -- the
  lease can lapse and be won by a peer mid-promote. The branch then still advanced the highwater
  marker and pointed the write Service selector and `pg-role=primary` at a pod that no longer
  held the lease, on top of the genuine new primary; `OnLost` could not correct it first because
  it blocks on `opMu`, which is held for the whole reconcile action. Holdership is now re-checked
  before either claim, the same way the initdb path already did. The node stays marked
  read-write, so the lost-leadership fence still demotes it.

- **A rewind that cannot get a usable connection to its source no longer escalates to a full
  re-clone (#298 review, agent).** `RejoinForceRewind` classified the failure and then discarded
  the classification, so three consecutive ticks -- ~15s on chart defaults -- escalated a healthy,
  non-diverged standby to `ReclonePreserving`: a multi-hour base backup plus an unreaped
  `.diverged.<ts>` copy on the PVC. Both ways a rewind can fail before it ever examines a history
  are now exempt from that backstop: the transport never connected (`connection refused`, an
  unresolved pod name, libpq's `timeout expired`), and the source accepted the connection then
  refused the session (`sorry, too many clients already`, `the database system is starting up`, a
  missing `pg_hba.conf` entry, a rotated credential). The criterion is whether a re-clone would
  fix it, not whether the failure is transient -- a re-clone dials the same source with the same
  credentials through the same `pg_hba`, so it fails identically, which is why even the permanent
  rejections belong here. Refusals on the TARGET side (`target server must be shut down cleanly`,
  `wal_log_hints`, `needs to exit backup mode`) still count toward the backstop, because replacing
  the local data directory is exactly what resolves those.

- **Four smaller agent fixes (#298 review).** A restart that could not prove the postmaster is
  dead now reports an error instead of `nil` -- on a wedged PV, `Stop` deliberately leaves the
  child supervised and `Start` then returns "still running", so a single-node primary reported a
  successful restart every tick while the database was down. A successful rejoin re-latches its
  upstream, without which the next re-homing tick skipped `releaseSlotOnFormerUpstream` and left
  an inactive slot pinning WAL on the rejoin target. `ensureSlotOnUpstream`'s existence guard is
  scoped to `slot_type = 'physical'`, mirroring the same fix in `Prober.CreatePhysicalSlot` --
  and both statements now `RAISE` a message of their own on a non-physical slot holding the
  name, because scoping the guard alone changed nothing observable: the create then ran and
  PostgreSQL's own error is the very `replication slot "..." already exists` that
  `IsDuplicateSlot` treats as success, so a logical squatter stayed silent exactly as before.
  And a queued control intent floors its inherited request deadline, which was routinely
  already in the past by the time the loop dequeued it -- making a graceful `/v1/node/restart`
  a zero-grace SIGKILL of a read-write primary.

- **`DELETE /v1/restore` can now actually wait out a slow-terminating Job (#298 review, agent).**
  Both callers passed the HTTP request context, which the control API had already capped at 60s,
  so the declared 90s wait was cut short and the real reason replaced by a bare
  `context deadline exceeded` -- an intermittent 502 on the delete and on `{"replace": true}`.
  The wait is detached with its own budget and the per-request ceiling is set explicitly above it.

- **Five ways a second writer could appear, closed (#298 review, agent).** (1) `boot()` and
  `StartLocal`'s standby arm now *assert* `standby.signal` from the control-file state instead
  of trusting the file to be there: `InRecovery` is derived from `pg_controldata` alone, so the
  two can disagree -- most sharply inside `RejoinForceRewind`, which moves the signal aside for
  `pg_rewind` (minutes, deliberately unbounded) and restores it only via an in-process `defer`,
  so an OOM/eviction/node reboot in that window left "in archive recovery" with no signal file,
  which was then started read-write on the live primary's timeline. (2) `SetRecoverySignal`
  writes through `atomicfile` (fsync + directory fsync) like `Native.Follow` already did, so a
  power loss cannot durably persist the control file while losing the signal. (3) `Promote` arms
  the read-write latch *before* `pg_ctl -w promote` rather than after: a `PGCTLTIMEOUT` (60s)
  exit is non-zero while the promotion still completes, and an unarmed latch made the next lease
  loss take the "no fence needed" branch on a node that was becoming a writer. (4) The
  Kubernetes DCS no longer uses client-go's `ReleaseOnCancel`, which empties the Lease *before*
  the deferred `OnStoppedLeading` demote; the agent now frees the Lease itself after `Run`
  unwinds, preserving the fence ordering while still handing off in milliseconds. (5) Planned
  shutdown clears the latch on a completed demote and waits for the election to unwind before
  closing the DCS client, so a clean termination is no longer counted and logged as a
  split-brain fence, and etcd's lease revoke is no longer killed mid-flight (which cost a peer a
  full `LeaseDuration` of write outage on every planned primary restart).

- **Two credential leaks into logs, closed (#298 review, image + agent).** The transient
  bootstrap postmaster ran without `-l`, so it inherited the entrypoint's stdout with
  PostgreSQL's default `log_min_error_statement=error` -- and on a *default* install
  `CREATE USER "postgres" ... PASSWORD '<secret>'` always fails ("role already exists") and the
  server echoed a `STATEMENT:` line carrying the cleartext password to container stdout. It now
  starts with `log_statement=none -c log_min_error_statement=panic`. Separately, psql prints the
  error CONTEXT of a failed dynamic `ALTER USER ... PASSWORD`, which carries the SCRAM verifier;
  the re-hash folded that combined output into an error the agent logs verbatim, so a node that
  entered recovery mid-rehash wrote the superuser's or replication user's verifier into the pod
  log. Verifiers are now redacted before the output reaches an error.

- **`etcd.tls.enabled=true` with `clientCertAuth=false` no longer renders a cluster that can
  never elect (#298 review).** The bundled-etcd guard also required `clientCertAuth`, so an
  encrypt-only bundled etcd rendered clean with no `ETCD_TLS_CA` -- and clientv3 then verifies
  the etcd server certificate against the container's system root store, which holds neither the
  chart's nor cert-manager's CA. Every dial failed `x509: certificate signed by unknown
  authority`, no node won the lease, and the release came up with no primary and no write-Service
  endpoint. The guard is now keyed on `tls.enabled` alone and its message says why.

- **The `databases`/`roles` and audit-extension hook Jobs are allowed through the
  NetworkPolicy (#298 review).** Both `psql` to 5432 over the write Service exactly like the
  monitoring-user Job, and both were the only client components with no ingress rule of their
  own. The default `networkPolicy.postgresql.allowExternal: true` masked it behind a blanket
  `podSelector: {}`, so with `allowExternal: false` plus declared `postgresql.databases` /
  `postgresql.roles` (or `postgresql.audit.enabled`) a default-deny CNI dropped the Job: it
  burned its 60x5s `pg_isready` wait, exited non-zero, and `helm install`/`upgrade` failed on
  the hook, leaving the release `failed` with no declared role, database, or pgaudit extension.
  Each rule is gated on the feature that creates the Job.

- **A planned step-down is no longer counted or logged as a split-brain fence (#298 review,
  agent).** `dcs.OnLost` fires for a voluntary `Release()` too -- only `OnRenewFailure` is
  filtered to involuntary loss -- and it gates solely on the read-write latch, which neither
  `ReleaseLease` nor `Switchover` cleared before releasing. Every controlled switchover and
  self-health handoff therefore incremented `pg_ha_agent_fences_total` and logged "lost
  leadership; demoting (fence)": three of them in one maintenance window tripped the chart's
  `PGHAAgentFlapping` page for work an operator had asked for, and the log told whoever read it
  mid-incident that the node had been fenced. The latch is now cleared where -- and only where --
  a demote or force-stop has just returned nil; on the paths where nothing was stopped it stays
  armed, because an unreachable SQL probe on a live postmaster is exactly the uncertainty the
  fence exists for.

- **`postgresql.extensions.extraVolumes` no longer refuses the name `pgbackrest` (#298).** The
  reserved-name validator listed it among the chart's own volumes, but `pgbackrest` is the idle
  sidecar *container*'s name -- Kubernetes keeps container and volume names in separate
  namespaces, and no render emits a volume by that name. So the refusal denied an operator a
  name that collides with nothing, under a message citing "a chart-managed volume" that does not
  exist, and did it asymmetrically: `postgresql.extraVolumes` accepted the same name, and the
  pgbackrest passthrough validator never reserved it. Every name the chart does emit is still
  refused, at render time as before.

- **Requested server TLS is now verified against the running server, and fails closed
  (#335).** `postgresql.tls.enabled=true` could leave a pod serving plaintext with nothing
  reporting an error: the release goes Ready, the certificate is mounted, the ConfigMap
  contains `ssl = on`, and `SHOW ssl` returns `off`. Every signal an operator can read
  describes the configuration that was *rendered*; none of them describe the one the
  postmaster actually *loaded*. The reported trigger was a first-boot pod whose
  `postgresql.conf` never picked up the chart's `conf.d` include, but an `ssl` set through
  `postgresql.configuration`, or an `ALTER SYSTEM`, produces the identical silent outcome --
  so this checks the outcome rather than any one of the inputs.

  The readiness probe now asks the server itself and refuses readiness on a definitive
  `ssl = off`, in **both** agent and standalone mode. A not-ready pod leaves the
  client-facing Services, so nothing keeps talking plaintext to it, and a `RollingUpdate`
  stalls there rather than rolling the whole cluster into the broken state; replication and
  the agent's peer probes are unaffected, because the headless Service publishes not-ready
  addresses. An *unanswerable* probe is deliberately treated as uncertainty rather than as
  evidence of plaintext -- failing on it would turn a transient blip into a write outage.

  In agent mode the agent additionally verifies this from the postmaster once a minute and
  publishes **`pg_ha_agent_tls_inactive`** (`0`/`1` gauge, always `0` where TLS was never
  requested) plus an Error naming the cause and the fix, so the condition is alertable
  instead of being discovered from a client-side `server refused TLS connection`. A
  **`PGHAServerTLSInactive`** rule (critical, `for: 5m`) ships with
  `ha.agent.monitoring.prometheusRule`, rendered only where TLS was requested. It carries
  the new `TLS_ENABLED` env var, which is the operator's intent: it cannot be inferred from
  `TLS_REQUIRE_SSL`, whose `false` is indistinguishable from absent.

  Detection only, on purpose: the agent does **not** rewrite the `conf.d` include at runtime.
  Three writers already converge that line (the entrypoint at `initdb`, `finishInitdbNative`
  on a fresh native install, and the `setup-config` init container on every later boot), and
  appending `include_dir` to a running native node would place it after the agent's own
  `include`, handing an operator-declared `wal_log_hints`/`hot_standby` precedence over the
  agent's until the next config generation.

  No rendered change where `postgresql.tls.enabled` is unset.

- **`postgresql.extraVolumes` may no longer reuse `agent-control-tls` or
  `pgbackrest-bootstrap-script` (#298 review).** Both are real volumes in the postgresql pod,
  but neither was in `pg.validateExtraPassthrough`'s reserved list -- so
  `postgresql.extraVolumes: [{name: agent-control-tls, ...}]` rendered CLEAN and emitted the
  same volume name twice, and the API server then rejected the StatefulSet at apply time with
  `spec.template.spec.volumes[1].name: Duplicate value`. That is the render-clean /
  apply-broken class the guard exists to eliminate, and the sibling
  `pg.validatePgbackrestPassthrough` already carried the complete list for the same pod. Both
  names are now reserved unconditionally, so the reservation holds before a later upgrade
  enables `ha.agent.control.enabled` or `pgbackrest.bootstrap.enabled`.

- **The three chart-managed role names must now be distinct (#298 review).** `ha.username`
  equal to `postgresql.username` (or to `postgres`) made the entrypoint's second `CREATE USER`
  fail as "role already exists" -- swallowed, like every statement in that block -- so the
  replication role silently kept the superuser's password while the bootstrap still reported
  success and wrote its completion sentinel. The HA agent authenticates as that role for every
  probe and for `pg_basebackup`, so the cluster came up Running/NotReady with no path out but
  deleting the PVC. `prometheusExporter.monitoringUser.username` colliding with either was
  worse still: the monitoring hook Job runs `ALTER ROLE ... WITH LOGIN PASSWORD`
  unconditionally, so it OVERWROTE a working superuser or replication password minutes after a
  successful install, breaking auth cluster-wide or streaming replication on every standby at
  once. All four collisions now fail at render. `postgresql.username` may still be `postgres`
  -- that is the default and initdb creates it; for the other two, "already exists" is the
  failure.

- **A long clone or rewind no longer gets the pod killed mid-copy (#298 review).** The reconcile
  heartbeat was struck once per tick, at the top of `tick()`, while `pg_basebackup`, `pg_rewind`
  and `initdb` all run inside `act()` holding the loop's mutex -- so nothing beat for as long as
  one of them ran. `/healthz` goes stale at three reconcile intervals (15s on chart defaults) and
  the agent's liveness probe gives up after 10 failures at 10s spacing, so the kubelet SIGKILLed
  the container -- and the postmaster it supervises -- about 115 seconds into any clone. A
  `RejoinForward` that escalated to a preserving re-clone on a pod past its startupProbe was
  killed on every attempt, each one leaving another `.diverged.<ts>` copy that nothing reaps
  until the volume fills; a first clone of a database that takes longer than the startupProbe
  budget could never finish either. The heartbeat is now kept alive around exactly those four
  long operations -- deliberately not a free-running one, so a wedge anywhere else still goes
  stale and still gets restarted.

- **A rewind that cannot reach its target no longer leaves the node with no recovery config
  (#298 review).** `RejoinForceRewind` ran a fallible remote slot-ensure *before* `Follow`, on a
  data directory `pg_rewind` had just left in primary shape. A momentary blip against the
  freshly-promoted target -- the likeliest moment for one -- returned with the rewind done but
  neither `standby.signal` nor `primary_conninfo` written, and the next tick started a
  postmaster with no recovery configuration at all. `Follow` already ensures the slot, after
  creating `standby.signal`, which is the whole point of that ordering.

- **A password needing SASLprep is left as md5 rather than rehashed into a lockout (#298
  review).** The SCRAM verifier is built without SASLprep normalisation, which the server and
  libpq both apply -- so for a password carrying any non-ASCII byte the verifier written was one
  the user's own password could never match, and because the re-hash is gated on
  `rolpassword LIKE 'md5%'` it stopped matching the moment the bad verifier landed: the
  superuser and replication user locked out for good, streaming replication stopped
  cluster-wide. Such a password is now skipped with a warning, keeping the md5 hash that the
  agent's own md5-above-scram `pg_hba` line still authenticates. Chart-generated passwords are
  ASCII and unaffected.

- **`pg_hba.conf` keeps mode 0600 when `postgresql.pgHba` is set (#298 review).** The insert
  finished with `mv`, which replaces the inode, so the file listing every auth rule inherited
  0644 from the redirect instead of initdb's 0600.

- **`postgresql.extraEnv` can no longer shadow `PG_MAJOR` or the control-API variables (#298
  review).** `pg.validateExtraPassthrough`'s reserved list was missing both, so
  `extraEnv: [{name: PG_MAJOR, value: "17"}]` rendered clean and pointed the entrypoint's
  `require_pg_bindir` and the agent's boot check at a major the image does not bundle.

- **`ClearDebrisDataDir` fails closed on an unreadable `PG_VERSION` (#298 review).** Only
  "absent" proves absence; EIO on a degraded volume, ESTALE on an NFS-backed PV or ELOOP all
  read as "not present" and let it delete an initialized data directory -- the one it exists to
  protect, on the path that runs it in front of `pg_basebackup`.

- **Every demote is now bounded, so a wedged postmaster cannot strand the Lease (#298 review).**
  `ChildPostmaster.Stop` escalates to `SIGKILL` only when its context expires, and four demote
  call sites -- the `DemoteFence` soft fence, `ReleaseLease`, `Switchover`, and `rejoinOnto`'s
  pre-rewind stop -- passed the reconcile tick's deadline-less context. A fast shutdown that
  never completed (a wedged checkpoint, a backend stuck in the kernel) therefore never escalated:
  `act()` blocked with `opMu` held, the heartbeat stopped, `OnLost` could not fence, and the
  Lease was never released either -- so **no peer could take over at all** until the kubelet
  killed the container. All four are now bounded by `RenewDeadline`, matching the `OnLost` fence.

- **`postgresql.pgHba` entries keep the order they were written in (#298 review).** Each entry got
  its own insert pass anchored on the first `host` rule -- which, after the first insert, *is* the
  entry just inserted, so the list came out reversed. `pg_hba` is first-match-wins, so this was
  not cosmetic: `["host all admin ... trust", "host all all ... reject"]` put the reject above the
  trust and locked the admin role out. One ordered pass now inserts the whole block, and it anchors
  on the first NON-LOOPBACK `host` rule so initdb's `127.0.0.1/32` and `::1/128` trust lines stay
  above the operator's -- otherwise a catch-all that also matches loopback (`host all all all
  reject`) beat them and broke in-pod TCP clients, `kubectl port-forward` + psql among them. That
  matches both other authorities on this ordering: the 1.x template and the agent's own
  `AssemblePgHba`.

- **`etcd.rbac.bootstrapImage` must match `ha.image` by DIGEST too (#298 review).** The guard
  compared `repository:tag` only, while both image dicts carry a `digest` and both render it --
  so pinning `ha.image.digest` and leaving the bootstrap image's empty passed cleanly with the
  database pods on the pinned build and the RBAC-bootstrap Job on whatever the mutable tag
  resolved to. That is the exact "one agent build writes the etcd RBAC a different build then
  authenticates against" drift the guard exists to prevent, reached *through* it.

- **The postStart primary-discovery budget is the 20 seconds it always claimed (#298 review).** It
  counted 20 *iterations*, each probing every peer at `PGCONNECT_TIMEOUT=3`, so peers that
  black-hole rather than refuse (a NotReady node, a NetworkPolicy drop) cost a 4-pod cluster
  ~260s -- with the container held out of `Started` and every Service the whole time. Now a real
  wall-clock deadline, armed on the first no-primary iteration so it never eats the `pg_isready`
  wait.

- **A bootstrap killed mid-`initdb` can recover (#298 review).** `initdb` refuses a target that is
  not byte-empty while the caller's emptiness test is `PG_VERSION`, so a SIGKILL while initdb was
  still laying out subdirectories left PGDATA non-empty with no `PG_VERSION` -- and every later
  attempt failed on "directory exists but is not empty", forever. The agent now clears
  pre-`initdb` debris the way the clone path already did, and the entrypoint's torn-bootstrap
  discard accepts "non-empty" as well as `PG_VERSION` (still gated on the in-progress marker, so
  it can never touch a directory an older image created). Relatedly, a failed bootstrap now stops
  its transient postmaster before exiting: in agent mode the script's stdout is captured, and an
  orphan holding that pipe open blocked the agent for the whole five-minute bootstrap budget.

- **`pg_hba.conf` is written atomically (#298 review).** The boot path rewrote it with a plain
  truncating write immediately before starting the postmaster; a crash or ENOSPC mid-write left a
  pod unable to start on the very file it was repairing. It now goes through the same
  temp+fsync+rename helper as every other PGDATA config write.

- **The default render no longer emits an empty `volumes:` key (#298 review).** Its guard still
  listed `ha.enabled`, whose only volume (`repmgr-config`) this release deletes, so the chart
  default produced `volumes: null` in the pod spec. The guard now names the conditions that
  actually contribute a volume.

- **`initdb` no longer requests md5 (#298 review).** `--auth-host=md5` does two things: it writes
  the method into initdb's own `pg_hba` (which the entrypoint overwrites, so that half was moot)
  **and** it sets `password_encryption` in `postgresql.conf` -- which decides how every password
  stored on the cluster afterwards is hashed: an operator's `CREATE USER`, the databases-roles
  hook Job's roles, any later `ALTER USER ... PASSWORD`. A brand-new 2.0.0 cluster therefore
  defaulted to a hash deprecated since PostgreSQL 10, and the chart's own md5→scram migration
  existed to undo a default this line had just chosen. Now `--auth-host=scram-sha-256`. Safe by
  construction: `bootstrap_initdb` only runs against an EMPTY data directory, so no existing
  md5-hashed role is stranded, and 1.x clusters keep their roles and their migration path
  (the agent's re-hash is unaffected). The managed users' explicit per-statement
  `password_encryption` is kept as belt-and-braces, no longer as compensation for the default.
- **`postgresql.pgHba` was a silent no-op in standalone mode (#298 review).** The postStart hook
  inserted each entry with `sed -i '/host all all all scram-sha-256/i ...'`, and no `pg_hba.conf`
  this chart produces has ever contained that line -- the entrypoint writes column-aligned rules
  ending in `host    all    all    0.0.0.0/0    scram-sha-256`. So the insert matched nothing and
  a documented value did nothing, in the only mode where that hook runs. It now anchors on the
  first `host` rule whatever it says, writes via a temp file and `mv`, and prints a message
  naming the entry rather than skipping in silence when there is no rule to anchor on. Inserted
  *above* the catch-alls because pg_hba is first-match-wins, and below the `local ... trust`
  lines that local `psql` depends on. **Agent mode was never affected** -- there the rules go to
  the agent, which places them above the catch-alls itself (#144). The reason this survived is
  worth recording: the only test asserted the entry *appears in the rendered postStart script*,
  which is true whether or not the insert can ever fire, and a comment in the suite claimed that
  case covered the insert. It is now checked behaviourally -- the awk program is run against the
  real bootstrap `pg_hba.conf` and the rule's position is asserted.
- **README triage: removed the prose describing repmgr as a live mode (#298 review).** #294
  deleted the mechanism but left the documentation describing it, and the README is the authority
  on values for a published chart. Three claims were not merely stale but wrong: the extensions
  section named `cagriekin/repmgr` as the image `copy-base-ext` runs from (it is `cagriekin/pg-ha`
  since #290, across seven passages); the apiserver-routing section told operators `KUBECONFIG`
  reaches an entrypoint stale-primary guard that shells out to `kubectl`, and 2.0.0 removed both
  the guard and `kubectl` from the image; and the PostgreSQL-major section presented the
  `trixie-<repmgr>-<n>` tag table as current with a note about a scheme change "with the next
  image release" that had already happened. Also corrected: `postgresql.audit.enabled` and
  `ha.image.majorVersion` describing "repmgr mode"; the `#297` promote gate, which 2.0.0 deleted
  rather than kept inert; the slot-alerting section, which explained repmgr-mode slot behaviour at
  length; `ALWAYS_PRIMARY` documented as conditional when it is not; and the repmgr-upstream
  PostgreSQL 13–17 caveat, moot once repmgr went. Mentions that are still accurate are kept
  deliberately -- the `repmgr` role, database and `repmgr-password` Secret key keep their names
  (renaming them rewrites live clusters), as do the `repmgr-init` init container and the
  `-repmgr` ServiceAccount, and the migration and frozen-image compatibility notes are correct.
- **`appVersion` no longer claims a PostgreSQL minor the chart cannot guarantee.** `pg` pinned
  `"18.1"` while the HA image tag floats with upstream, the same reason `pgvector` was relaxed
  to `"18"` in #164. The `postgresql.image.tag` default is unchanged.
- **A step-down could be silently dropped, letting a node re-win the Lease it was handing
  over (#298 review).** Both DCS backends read the step-down cooldown at the top of the
  election loop and only then installed the cancel hook, so a `Release()` arriving in between
  -- and the cooldown wait is the wide part of that window -- armed a cooldown that had
  already been read and discarded, and found no cancel to call. The node then walked straight
  into the election. The hook is now installed before the cooldown is consulted, so every
  `Release()` either cancels a live election or lands in a gap the next iteration's re-read
  covers. Fixed symmetrically in the Kubernetes and etcd backends.
- **Peers are probed concurrently (#298 review).** Each probe is a `psql` connect bounded only
  by `PGCONNECT_TIMEOUT`, and they ran one after another -- so a tick cost (unreachable peers)
  × (connect timeout). On a 5-node cluster that just lost its network that is ~40s inside a
  loop whose interval is 5s, and `/healthz` goes stale at three intervals: the agent could be
  liveness-killed, taking its postmaster with it, precisely when peers were unreachable. The
  results are collected by ordinal, so peer order stays deterministic for the promote-distance
  ranking.
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

### Security

Findings from a security-only review of the 2.0.0 line (#298), fixed here. All are hardening;
the highest-impact require an attacker who already holds the agent's ServiceAccount token (a
compromised pg pod), which the agent's own RBAC scopes to the release's pods/marker/lease.

- **A forged peer restore timestamp can no longer promote a stale node.** The agent ranks a
  peer's gossiped `RestoredAt` (a pod annotation) ahead of WAL position, so a far-future value
  written onto a behind-but-reachable peer's annotation could hand it the lease and discard
  committed WAL. The value is now rejected unless it is a parseable RFC3339 timestamp no more
  than an hour ahead of now; anything else contributes no restore authority.

- **The lease holder identity is validated before it becomes a replication conninfo.** A
  follower built `host=<leader>.<headless>` straight from the Lease `holderIdentity`, so a
  value like `evil.svc port=5432 sslmode=disable x` injected libpq keywords and made the
  standby dial an attacker host with the replication password set. The identity, and every
  follow/clone/rejoin target, must now match this cluster's `<name>-<ordinal>` pod grammar.

- **A tampered primary marker is now detected and alarmed.** The marker ConfigMap's timeline
  is trusted as the promotion highwater; a forged or unparseable value freezes promotions
  cluster-wide. The agent keeps failing closed (the safe response) but now logs and increments
  a new `pg_ha_agent_marker_tamper_suspected_total` counter when the marker is unparseable or
  implausibly far above every observed node, so a tamper-induced outage is diagnosable.

- **Maintenance mode is no longer silent.** Entering/leaving pause (an in-namespace kill switch
  for automatic failover) is now logged on the transition. The lease-loss fence is unaffected
  by pause, so a paused primary that loses its lease still demotes.

- **Bootstrap passwords are no longer passed on a psql command line.** `CREATE/ALTER USER ...
  PASSWORD` ran via `psql -c`, exposing the app and replication passwords in `/proc/<pid>/cmdline`
  during bootstrap; they now go in on stdin.

- **Child processes no longer inherit the agent's raw credential env.** psql, pg_basebackup,
  pg_rewind and pgbackrest authenticate via `PGPASSWORD`/`PGPASSFILE`, so `REPMGR_PASSWORD` /
  `POSTGRES_PASSWORD` (and anything else carrying `PASSWORD`) are now stripped from their
  environment.

- **Role names are validated before reaching pg_hba.conf.** `REPMGR_USER` / `POSTGRES_USER` /
  `MONITORING_USER` are interpolated into pg_hba lines; a value with whitespace or a newline
  now fails at boot, matching the existing `POD_CIDR` check.

- **`PGBACKREST_STANZA` is validated before the archive_command GUC.** A single quote would
  close the GUC string and hand the rest to the archiver's shell; the stanza must now match
  `[A-Za-z0-9_-]+` or the boot fails.

- **`postgresql.pgHba` entries are validated at render time.** A quote or newline would corrupt
  the postStart hook (crash-looping the pod) or mangle pg_hba.conf; both are now rejected by
  `values.schema.json` and a `{{ fail }}` guard.

- **Security keys may no longer be set via the deprecated `repmgr.*` alias.** Because the alias
  overwrites `ha.*` even over `--set`, a stale value could silently downgrade a control:
  `ha.agent.podCidr`, `ha.agent.control.allowedClientCNs`, `ha.agent.control.restore.allowedClientCNs`
  and `ha.agent.control.restore.admissionPolicy.{enabled,acknowledgeUnbounded}` must now be set
  under `ha.*`; the render fails if they appear on `repmgr.*`.

- **The etcd RBAC-bootstrap Job drops its ServiceAccount token.** It talks only to etcd, so it
  now sets `automountServiceAccountToken: false` like every sibling one-shot Job (etcd 0.1.8).

- **Image supply-chain hardening.** The PGDG apt source and key fetch use HTTPS with a protocol
  pin (`--proto '=https'`); the pg-extensions build's `trusted=`/`allow-insecure=` guard is now
  case-insensitive and rejects a multi-line `APT_SOURCE_LINE`; and the five non-GitHub actions
  in the pg-ha publish workflow are pinned to commit SHAs.

### Testing

- **The cold-boot stage of the agent failover suite now always runs (#298 review).** A
  full-cluster restart -- both pods deleted at once, the cluster re-electing a single primary
  with data intact -- was gated behind `AGENT_COLDBOOT=1`, deferred when promote-from-recovery
  still had an unvalidated interaction with the repmgr catalog (a former primary brought up in
  recovery mode was left `type=primary` in `repmgr.nodes`). #294 deleted that mechanism and the
  image no longer creates the catalog, so the reason had evaporated while the gate stayed. It
  also made local and CI disagree -- the matrix set the flag, a developer's run did not -- and
  the three skips it produced were labelled "prior stage failed" even though the prior stages
  had passed, which is exactly how a stale gate goes unnoticed. Verified passing before the
  gate came out: 22/22, primary and lease holder re-settled in 10s.

- **Both render gates only ever checked each chart's DEFAULT render (#298 review).** kubeconform
  validated 9 resources for `pg`; kube-linter has no values flag at all, so directory mode could
  not see anything else. Every optional component was therefore unchecked by both: pgpool, the
  metrics exporter, pgBackRest's five containers, TLS, the etcd DCS, the restore workload, the
  hook Jobs. That is the wrong half to skip -- a violation in a default-on object is caught by a
  dozen other things, one in an optional object ships. Both gates now enumerate each chart's own
  `tests/values-*.yaml` fixtures and render every one (44 profiles across the five charts, ~13s).
  It also validates at **two** Kubernetes versions, because the documented minimum cannot
  validate kinds that did not exist yet: `ValidatingAdmissionPolicy` and its Binding -- the
  admission control guarding the destructive restore Job, the single most security-relevant
  object these charts emit -- were being SKIPPED at 1.29 with the skip invisible, since
  `-ignore-missing-schemas` treats an unknown core kind exactly like an uncataloged CRD. At 1.30
  both validate. Skips are now always reported by kind, so an unvalidated resource can never be
  silent again.
  Widening it immediately found two real policy violations, both fixed here:
  - the **etcd `rbac-bootstrap` Job** was missing the one-shot probe waiver that every sibling
    hook Job in `pg/templates` already carried. It only renders under `dcs.backend: etcd`, so the
    gate had never seen it. (etcd subchart `0.1.6` → `0.1.7`, re-vendored into both consumers.)
  - the **idle `pgbackrest` sidecar** had no probes and no waiver. The waiver added for it is
    gated on `pgbackrest.enabled` rather than unconditional, because kube-linter waives per
    OBJECT: an unconditional annotation would also stop the gate noticing if the `postgresql`
    container ever lost its own probes. Verified in both directions -- the default render carries
    no waiver and still catches missing `postgresql` probes.
  A profile that renders nothing now fails both gates, and a fixture that is deliberately a
  *layer* over another declares its base in `fixture_base()` rather than being silently skipped.
- **The policy gate could report a clean pass while linting nothing (#298 review).**
  `kube-linter lint <chart-dir>` renders the chart itself, and when that yields no objects it
  prints `Warning: no valid objects found.` and exits **zero** -- so a chart that stops rendering
  for kube-linter reports a clean policy gate while examining not one container. Found by
  building a throwaway chart whose container had no resources and no probes: it passed the gate,
  while the same manifest piped to `kube-linter lint -` produced all four violations. Both gates
  now fail when they examined nothing (kubeconform's equivalent is a `0 resources found`
  summary). Verified in both directions: a real chart with its resources removed is caught and
  reported as `FAILED (1 of 5 charts)`.
- **The gate scripts now fail fast on a missing tool, and always print a verdict line.** A
  missing `kube-linter`/`kubeconform` produced one `command not found` per chart and exit 1 --
  correct, but at a glance indistinguishable from real violations. They now stop immediately with
  an install hint and exit 127. Each gate also ends with a single `=== <gate>: OK|FAILED ===`
  line, because the exit status is the only unambiguous signal and it is the easiest thing for a
  caller to discard: piping a gate through `tail` and reading `$?` reports *tail's* status, which
  is how these gates were once reported as passing when they had not run at all. Shared helpers
  live in `scripts/lib.sh`.
- **An empty helm-unittest run is now a failure.** `scripts/helm-unittest-charts.sh` printed
  "No tests/unit/*_test.yaml suites found" to stderr and exited 0. A gate whose job is to run
  tests must not pass loudest at the moment it has stopped testing anything.
- **The `failoverMode: repmgrd` → 2.0.0 upgrade now has a KinD suite (#298 review).** It was the
  only 2.0.0 path that recreates a live StatefulSet, the one every remaining repmgrd consumer
  must follow, and the only one that can lose a cluster if it goes wrong -- and nothing tested
  it. `test-migrate-native.sh` covers agent(1.x) → agent(2.0.0) and says so in its own comments,
  so the gap read as covered from the suite list alone. `pg/tests/test-migrate-repmgrd.sh`
  installs the released 1.17.0 chart with `failoverMode: repmgrd` and an older published image
  (both sidecars, `OrderedReady`), then walks the documented runbook and asserts what no data
  check can: the orphaned pods are **adopted** by the recreated StatefulSet rather than rebuilt
  (pod UIDs across the recreate), the PVCs keep their identity, the database keeps serving during
  the orphan window, `podManagementPolicy` flips to `Parallel`, both sidecars go, the Lease
  appears and its holder is the primary, no node was re-cloned or re-initdb'd, and lease-based
  failover then works on the migrated cluster with the ex-primary rejoining. It also asserts the
  runbook's first step is enforced -- all five removed keys refuse to render -- before touching
  anything, so an operator who skips it finds out with their cluster intact. 46 assertions;
  wired into the `pg-test` matrix with the same 50-minute no-retry budget as `migrate-native`.
- **`config` and `special-chars` suites now run in CI (#298 review).** Both existed in
  `pg/Makefile` and in no matrix leg, so they only ran when someone remembered. `special-chars`
  is the pointed one: it covers identifiers and passwords with characters that break SQL, which
  is exactly the class of defect #298 found in `bootstrap_initdb`.
- **Two KinD readiness checks could pass while PostgreSQL was failing (#298 review).**
  `test-upgrade.sh` and `test-agent-failover.sh` read `containerStatuses[0].ready`, and index 0
  is not guaranteed to be the `postgresql` container -- a sidecar with no readiness probe
  reports `ready=true`. Both now select the container by name.
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

### Migrating from 1.x

**If you were on the default (agent):** delete `repmgr.failoverMode: agent` from your values
if you set it explicitly, then `helm upgrade` normally. Your StatefulSet is already
`Parallel`; there is no recreate and no behaviour change.

**If you pinned `repmgr.failoverMode: repmgrd`:** `podManagementPolicy` moves `OrderedReady`
→ `Parallel`, and that field is **immutable**, so the StatefulSet has to be recreated once
(zero data loss — pods and PVCs are kept):

```bash
# 1. Healthy cluster + a fresh backup first. GitOps: disable auto-sync for these steps.
kubectl delete statefulset <release>-pg -n <ns> --cascade=orphan
# 2. Remove every removed key from your values, then upgrade (recreates the STS as
#    Parallel and adopts the orphaned pods):
#    Every key 2.0.0 rejects has to go, not just this one (#298 review) -- a leftover
#    repmgr.serviceUpdater.*, repmgr.monitoringHistoryDays, repmgr.splitBrainDetection.* or
#    pgpool.autoFailback fails the render just as hard. See the "Removed values" table above.
helm upgrade <release> cagriekin/pg -n <ns>   # + your -f values, minus the removed keys
# 3. Verify:
kubectl get lease <release>-pg-leader -n <ns> -o jsonpath='{.spec.holderIdentity}'
kubectl get endpoints <release>-pg -n <ns>
```

Rollback is to chart `1.x` with `failoverMode: repmgrd` restored and the same
`--cascade=orphan` recreate.

Two further changes land for repmgrd users specifically: the agent assembles a pod-CIDR +
SCRAM `pg_hba.conf` with **no implicit `0.0.0.0/0 md5` catch-all** (add explicit
`postgresql.pgHba` rules first if you relied on it), and failover history moves from
`PrimaryChanged` Events to the agent's audit log and the `pg_ha_agent_*` metrics.

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

### Added

- **`pgbackrest.extraEnv` / `extraVolumes` / `extraVolumeMounts`: point the pgBackRest workloads
  at a non-default apiserver route or repository route (#323).** `postgresql` has had all three
  since #262; none of the pgBackRest workloads had any equivalent, and each carried one
  hardcoded `env:` block.

  That gap has teeth because the **backup CronJob is an apiserver client**: it resolves the
  current primary at fire time by listing EndpointSlices and then drives pgBackRest with
  `kubectl exec`, which is exactly what makes the schedule survive a failover. Where the pod's
  default route to `kubernetes.default.svc` is closed -- a CNI policy denying the
  `kube-apiserver` entity for a tenant namespace, say -- the schedule runs, `kubectl` times out,
  and no backup is ever taken. The agent could already escape this with `KUBECONFIG` and
  `postgresql.extraEnv` (#317); the CronJob could not.

  The second-order damage is worse than the missed backup: that CronJob is the chart's **only**
  caller of `stanza-create`, so the repository is never initialised and `archive_command` fails
  on every WAL segment from the moment the cluster starts -- `archived_count 0,
  failed_count 196` an hour into a database's life, on a cluster where every pod, Service and
  policy reads as correctly configured.

  All three reach **every** container that runs pgBackRest or drives it: the `pgbackrest`
  sidecar and the `pgbackrest-bootstrap` init container in the postgresql pod, the `full`/`diff`
  backup CronJobs, the restore Job/CronJob, and the validation CronJob. The sidecar is included
  deliberately -- `stanza-create` and `backup` actually execute there, via `kubectl exec` from
  the CronJob, so a value that reached the CronJob alone would route the `kubectl` call and
  leave the backup itself unchanged. The **postgresql container is excluded**, equally
  deliberately: it has `postgresql.extraEnv` of its own (also where `archive_command`'s
  pgBackRest invocation reads its environment), and a `KUBECONFIG` injected there would
  redirect the entrypoint's stale-primary guard (#170) as a side effect of configuring backups.

  Guarded at render time by `pg.validatePgbackrestPassthrough`, since each of these failures is
  otherwise apply-time or run-time only: setting any of the three while `pgbackrest.enabled` is
  false is **refused** rather than silently ignored; `extraVolumes` names are checked against
  the chart's own volumes in all four pods *and* against `postgresql.extraVolumes` /
  `postgresql.extensions.extraVolumes`, which land in the same postgresql pod volume list; every
  mount must reference a declared volume, must not repeat a `mountPath`, and must not shadow
  PGDATA (at, above or inside), `/etc/pgbackrest/pgbackrest.conf`, `/scripts`, `/work`, `/tmp`
  or `/var/run/postgresql`; and `extraEnv` may not reuse a chart-set name -- reserved as the
  union over all five containers and unconditionally, so a passthrough that works today cannot
  start shadowing a chart value after a later upgrade enables `repoEncryption` or switches
  `s3.keyType`.

  Two consequences documented in the README rather than left to be discovered.
  `extraVolumes` is pod-level on the **postgresql** pod as well (that is where the sidecar and
  the bootstrap init container live), so a ConfigMap or Secret that does not exist yet holds
  the database pods in `ContainerCreating` on the next roll, not just the backups. And with
  `repmgr.agent.control.restore` enabled, the #279 ValidatingAdmissionPolicy that bounds the
  agent's `create jobs` grant pins the restore Job's volume sources and env `valueFrom` names
  -- the chart folds these values into those pins, so the agent-driven restore keeps working,
  but only sources it can bind to a NAME (`emptyDir`, `configMap`, `secret`,
  `persistentVolumeClaim`; `fieldRef`/`secretKeyRef`/`configMapKeyRef` for env) can be pinned.
  Anything else is a render failure on purpose: admitting an unpinned source would reopen the
  door that policy exists to close, and omitting it would surface as `POST /v1/restore` denied
  at admission during an incident. The names reach single-quoted CEL literals, so they go
  through the same injection guard as every other interpolated value.

  `extraVolumeMounts` paths are normalized before those comparisons: a relative destination is
  refused (the runtime rejects it, so the pod would stay in `CreateContainerError` with nothing
  in the manifest to explain it), and `//var/lib/postgresql/data` or
  `/tmp/../var/lib/postgresql/data` are refused rather than walking around a prefix test onto
  the live data directory.

  No render change with the values unset.

## 1.15.0 - 2026-08-23

### Added

- **`postgresql.extensions.env` / `envFrom` / `extraVolumes` / `extraVolumeMounts`: point the
  extension install at your own apt mirror or proxy (#320).** `copy-base-ext` and `copy-ext`
  had no `env`, no `envFrom` and no volume beyond the three `emptyDir`s, so there was no
  supported way to keep the `apt-get` steps off the public internet.

  That matters under a per-namespace default-deny egress policy, where the cost is not the
  repeated work but the HOSTS: every external host the install touches has to sit in the
  platform's baseline allow, for every tenant, permanently. A Supabase-shaped package set
  needs three -- `apt.postgresql.org`, `repo.pigsty.io`, and `deb.debian.org` (the
  general-purpose Debian archive, for one 165 kB `libsodium23` that neither PGDG nor Pigsty
  ships). A single `http_proxy` now replaces all three, since apt honours it and it needs no
  source rewriting; `extraVolumes`/`extraVolumeMounts` cover what env cannot express -- an
  `/etc/apt/apt.conf.d` snippet or a replacement `sources.list`, which `aptSources` cannot
  provide because it only APPENDS source files and never rewrites the base sources the images
  ship.

  All four apply to **both** extension init containers and to **neither** the postgresql
  container: an `http_proxy` in the postmaster's own environment would silently redirect
  anything else that reads it, and an apt configuration mount there is meaningless. They
  render only while `packages` is non-empty (the plain-copy path runs no apt at all), and
  setting any of them with `packages` empty is **rejected at render time** rather than
  silently ignored -- an operator who believes the proxy is in effect when it is not has a
  worse problem than a failed render.

  Two further guards, both for failures that would otherwise surface only on a running pod.
  An `extraVolumes` entry reusing one of the chart's own volume names (`data`, `ext-lib`,
  `postgresql-config`, ...) is refused: volume names are not merged -- the later entry in the
  pod's list wins -- so it would REPLACE the data PVC or the extension tree with a ConfigMap.
  An `extraVolumeMounts` entry is refused if it mounts over `/ext-lib`, `/ext-share` or
  `/ext-extra-lib` (the trees the install step copies into, which the mount would shadow), or
  if it names a volume absent from `extraVolumes` -- the kubelet rejects that pod at apply
  time, so helm has to catch it first.

  This does not take the install off the pod-start path; a chart-built extension image
  (resolve the packages once, mount the result) remains the larger follow-up.

- **`postgresql.extensions.image`: a prebuilt extension image, so the install leaves the
  pod-start path entirely (#320).** The values above only redirect WHERE the per-start install
  fetches from; this removes it. The packages are resolved once at image build time (new build
  recipe in `images/pg-extensions/`) and a third init container, `copy-prebuilt-ext`, does a
  plain `cp` from that image.

  Two consequences matter more than the speed. There is **no egress on the pod-start path at
  all** -- not proxied, absent, so there is nothing to allow per tenant permanently. And there
  is **no root**: the apt path has to REPLACE `postgresql.containerSecurityContext` with
  `runAsUser: 0` because dpkg needs it, and a namespace enforcing the PSA `restricted` profile
  (or any `runAsNonRoot` admission policy) rejects that pod outright -- so this path works
  where the apt path cannot run at all.

  `packages` and `aptSources` are refused alongside `image`. They are not additive: both
  populate the same `ext-lib`/`ext-share` volumes with a no-clobber copy, so which build of an
  extension actually won would be decided by init-container order -- an implementation detail
  of the template -- rather than by anything in the values file, and a version-pinned package
  silently losing to whatever the image happened to contain is not a trade worth allowing. A
  non-PGDG source belongs in the image build instead (`APT_SOURCE_*` build args). `extraLibs`
  DOES still apply, reading from the prebuilt image's filesystem, so the same absolute paths
  work on either path and a working values file moves across with no other edit.

  Either `tag` or `digest` is required, refused at render time otherwise: an untagged
  reference resolves to `:latest`, which for an extension image means the `.so` files can
  change under a pod restart with nothing in the release changing, and an extension built for
  the wrong major does not load at all. `{major}` in `tag` substitutes
  `postgresql.majorVersion`, as in `packages` and `aptLine`.

  `copy-prebuilt-ext` runs LAST of the three extension init containers and copies with `cp -n`
  (no-clobber), for the same reason `copy-ext` does: `copy-base-ext` populated
  `ext-lib`/`ext-share` from the image that actually RUNS the server, and this is an
  independent build that can sit on a different postgres point release -- an unconditional
  copy would overwrite a core lib (e.g. `libpqwalreceiver.so`) with one the running postmaster
  never linked against (#302).

  It **adds** extensions; it does not upgrade one the server image already ships. The copy is
  no-clobber and runs last, so anything `copy-base-ext`/`copy-ext` already placed wins,
  silently. Concretely: the pgvector chart's `postgresql.image` is `pgvector/pgvector`, which
  ships `vector.so`, so a prebuilt image carrying a NEWER pgvector is a no-op -- change the
  server image for that instead. There is no safe alternative: clobbering the `.so` files would
  overwrite a core lib with a build the running postmaster never linked against (#302), and
  clobbering only the control/SQL files would leave the SQL definitions and the `.so` at
  different versions.

  The image build fails rather than producing a quietly useless artifact when `PACKAGES` is
  empty, the `APT_SOURCE_*` triple is partially set, `APT_SOURCE_LINE` carries no `signed-by=`,
  or the install leaves `/usr/share/postgresql/<major>/extension` empty -- that last one
  catching the mistake otherwise invisible until `CREATE EXTENSION` (a package name that
  exists but installs nothing for this major). CI builds it for both supported majors and runs
  the chart's own copy command verbatim against the result, so drift between the Dockerfile
  and `pg.extensionPrebuiltCopyCommand` cannot go unnoticed.

- **A `pgdg` entry in `postgresql.extensions.aptSources` is now refused at render time
  (#320).** It was always fatal and the failure named nothing useful. Both `postgres:*-trixie`
  and the `cagriekin/repmgr` image already configure `apt.postgresql.org` under their OWN
  keyring paths, and the chart derives its keyring path from the entry `name`
  (`pgchart-<name>-keyring.gpg`) with no override -- so apt sees two entries for the same repo
  with different `Signed-By` values and rejects the ENTIRE source list
  (`E: Conflicting values set for option Signed-By regarding source
  http://apt.postgresql.org/pub/repos/apt/ trixie-pgdg`), failing the install before it
  starts. Omitting the entry is correct: PGDG packages in `packages` resolve from the image's
  own configuration, which is what `packages` already relies on. The guard keys on the HOST,
  not the entry name, so any `aptLine` pointing at `apt.postgresql.org` is caught regardless
  of what it was called.

### Fixed

- **A digest-only image pin rendered an unparseable reference.** `pg.image` built the
  reference with an unconditional `printf "%s:%s"`, so a block with a `digest` and no `tag`
  produced `repo:@sha256:...` -- which containerd rejects (`InvalidImageName`), so the
  container never starts. That made the digest pin, which this chart recommends for
  production, the broken configuration. It now renders `repo@digest`.

**Migrating from 1.14.1:** for almost everyone, nothing to do -- the four override values
default to `[]`, `extensions.image.repository` defaults to `""`, and the default render is
byte-identical. Two changes can fail an upgrade that previously succeeded, both of them
turning a runtime failure into a render-time one:

- **Every image block now requires a tag or a digest, and a non-empty repository.**
  `pg.image` is shared by every image in the chart (postgresql, repmgr, pgpool and its
  exporter, busybox, the metrics exporter, mc, the pgbackrest CronJob), so a values file that
  CLEARS a tag without setting a digest -- `postgresql.image.tag: ""`, `pgpool.image.tag: ""`
  -- now fails at `helm upgrade`. It previously rendered `repo:`, which containerd rejected at
  pod start anyway; the difference is that the failure now names the value instead of showing
  up as `InvalidImageName` on a pod. Set a tag or a digest.
- **A `pgdg` entry in `aptSources` is rejected.** A values file with one now fails at render
  time instead of inside `apt-get update` on every pod start, so a release that was already
  broken this way surfaces at `helm upgrade`.

## 1.14.1 - 2026-08-22

### Changed

- **`repmgr.image.tag` -> `trixie-5.5.0-33` (#317).** The agent now honours `KUBECONFIG`,
  so its apiserver traffic can be routed through an in-cluster proxy. Chart-only change is
  the pinned tag; the behaviour ships in the image.

  Why it exists: the agent reads its primary marker and publishes gossip through the
  apiserver, so on a cluster whose egress policy denies pod traffic to the apiserver **no
  leader is elected and the cluster never gets a serving primary** -- while every pod,
  Service and policy looks correctly configured. Such a policy is not always fixable from
  the policy side: on Cilium deny wins within a tier (no allow rule re-opens the apiserver
  for one namespace) and reserved identities are compound (`reserved:host` and
  `reserved:kube-apiserver` sit on the same identity), so any topology reaching the
  apiserver via a real node IP cannot admit apiserver traffic for one workload without
  admitting node traffic for it. What remains is an in-cluster TCP proxy, which needs a
  different **address** while still verifying the apiserver's own **certificate** -- its
  SANs cover `kubernetes.default.svc` and the apiserver IPs, not the proxy Service.
  Overriding `KUBERNETES_SERVICE_HOST` retargets the dial but leaves no way to set
  `ServerName`, trading a routing failure for a verification failure; `server:` +
  `tls-server-name:` is the pair only a kubeconfig can express.

  **No new value.** Set the variable with `postgresql.extraEnv` and mount the kubeconfig
  with `postgresql.extraVolumes`/`extraVolumeMounts`, both of which already exist. Keep
  `tokenFile`/`certificate-authority` pointed at the ServiceAccount mount so the identity
  stays the pod's ServiceAccount and the chart's RBAC applies unchanged -- only the address
  moves. See the README ["Routing the agent's apiserver traffic"](README.md#routing-the-agents-apiserver-traffic--kubeconfig-317).

  Both apiserver clients take the route: the mutation client (write-Service selector,
  `pg-role` labels, primary marker) **and** the Lease-backed leader election when
  `repmgr.agent.dcs.backend=kubernetes`. Reaching one but not the other would elect a
  leader that cannot publish. `backend=etcd` is unaffected -- leadership never touches the
  apiserver in that mode.

  A `KUBECONFIG` that is set but unreadable, malformed, or contextless is a **startup
  failure naming the file**, not a silent fall back to in-cluster: falling back would
  reproduce the exact hang this escapes, with a kubeconfig mounted and apparently in
  effect. `~/.kube/config` is deliberately **not** consulted, so a stray file cannot
  silently redirect a production cluster. Note `kubectl` is not as strict -- its deferred
  loader does fall back -- so a broken mount degrades the entrypoint's #170 settle guard to
  its fail-open fast path while the agent refuses to boot; mount the kubeconfig from a
  ConfigMap that cannot vanish. The boot log's new `apiserver=` field records which route
  was taken (`kubeconfig <path>` or `in-cluster`), because a denied-egress hang and a
  misrouted kubeconfig otherwise log the same dial timeout.

**Migrating from 1.14.0:** nothing to do. With `KUBECONFIG` unset -- the default, and what
the chart renders -- the agent takes the in-cluster ServiceAccount path exactly as before;
the default render is byte-identical apart from the image tag, so `helm upgrade` rolls the
pods once for the new image and the agent re-establishes leadership with no manual step.

## 1.14.0 - 2026-08-21

### Added

- **`postgresql.extensions.aptSources` (#310).** Adds a non-PGDG apt source (e.g.
  [Pigsty](https://repo.pigsty.io), the only source for `pgsodium`, `supabase_vault`,
  `pg_graphql`, `pg_net`, `supautils`, `wrappers`, `pgjwt`, `pgmq`) inside
  `copy-ext`/`copy-base-ext`'s own throwaway filesystem, before the existing
  `postgresql.extensions.packages` apt-get install -- closing the gap where a
  Pigsty-only package name previously made `copy-base-ext`'s `apt-get install` fail
  outright (the `cagriekin/repmgr` image has no Pigsty source and isn't something a
  chart consumer builds), aborting before its extension-file copy ever ran. Each
  entry's key is dearmored to `/usr/share/keyrings/pgchart-<name>-keyring.gpg` and its
  line written to `/etc/apt/sources.list.d/pgchart-<name>.list` ahead of a second
  `apt-get update` -- the `pgchart-` prefix so an entry can never collide with a source
  the image already owns (the `cagriekin/repmgr` image's own PGDG source); `keyUrl`/
  `aptLine` are restricted to a narrow character allowlist at render time
  (`pg.validateExtensionAptSources`), since both are interpolated into a shell command.
  Requires `packages` to be non-empty. See the README ["Installing packages from a
  non-PGDG apt source"](README.md#installing-packages-from-a-non-pgdg-apt-source-310).
- **`postgresql.extensions.extraLibs` + automatic `LD_LIBRARY_PATH` (#309).** Some
  apt-installed extensions depend on a general-purpose shared library that Debian
  installs to the standard multiarch path (e.g. `libsodium.so.23`, needed by
  `pgsodium`/`supabase_vault`), never under `/usr/lib/postgresql/<major>/lib` where the
  existing extension-file copy reads from -- confirmed live, and true regardless of how
  broad that copy step's glob is. `extraLibs` is an explicit list of exact absolute FILE
  paths (no trailing `/`) to additionally copy into a **new, dedicated** `ext-extra-lib`
  volume -- deliberately kept separate from `ext-lib`, since `ext-lib` is also populated
  by the unvalidated `*.so*` glob copy (either image, #302) and pointing the search path
  there would extend the same ABI-shadowing hazard the denylist below exists to prevent
  to every file that glob happens to sweep in. The `postgresql` container gets
  `LD_LIBRARY_PATH=/usr/lib/postgresql/<major>/extra-lib` automatically whenever
  `extraLibs` is non-empty (not just `extensions.enabled`, so a release that doesn't use
  this feature gets no search-path change and no extra volume at all), since the copied
  file has no `RUNPATH`/`RPATH` and is otherwise never found by the dynamic linker
  (confirmed live end-to-end: fails to load without `LD_LIBRARY_PATH`, loads cleanly
  with it). Deliberately explicit, not an automatic `ldd`-and-copy-everything walk:
  `copy-base-ext`/`copy-ext` can be different image builds, and auto-copying a resolved
  dependency's transitive closure between them risks a runtime ABI mismatch -- every
  library the postmaster itself links is refused at render time (`pg.validateExtraLibs`)
  for exactly that reason: the full `ldd postgres` NEEDED set against the shipped
  `postgres:*-trixie`/`cagriekin/repmgr` images (glibc, OpenSSL, Kerberos, LDAP, ICU,
  zstd/lz4/xz, audit, `libcap`/`libcap-ng`/`libkeyutils`, ...), plus `libpq` (the
  dependency of `libpqwalreceiver.so`, the exact `#302` ABI hazard). An entry must also
  name a real shared library (basename ending `.so`/`.so.<N>`) and not duplicate another
  entry's destination filename -- both would otherwise pass validation and only fail at
  `cp` time, crash-looping the pod. Requires `packages` to be non-empty. See the README
  ["Copying a package's own shared-library
  dependencies"](README.md#copying-a-packages-own-shared-library-dependencies-309).
- **`aptSources[].aptLine` requires `signed-by=` matching its own entry, and rejects
  `trusted=` (review, hardening).** Without `signed-by=/usr/share/keyrings/pgchart-
  <name>-keyring.gpg` naming exactly the keyring the entry's own `keyUrl` is dearmored
  to, apt has no way to know which key to trust for that source -- previously this
  failed only later, at apply time, inside `apt-get update` (`NO_PUBKEY`), instead of at
  render time (CLAUDE.md invariant #4). `trusted=yes` (or any `trusted=` option) is
  rejected outright: it disables apt's signature check entirely, making the
  `curl`/`gpg` verification step above decorative and installing unsigned packages as
  root.
- **`aptSources[].keyUrl` allows `&`; `aptLine` allows `,` and fails on a leftover
  `{`/`}` (review).** `&` in `keyUrl` is needed for a standard keyserver lookup URL
  (`?op=get&search=...`) and is inert inside the double-quoted `curl` argument; `,` in
  `aptLine` is needed for the standard multi-arch option syntax (`[arch=amd64,arm64]`)
  and is inert inside the double-quoted `echo`. A `{`/`}` surviving `{major}`
  substitution in `aptLine` now fails the render instead of silently writing a
  literal, nonsensical placeholder (e.g. a typo'd `{MAJOR}`) into `sources.list.d` that
  would otherwise only fail later, at apply time, inside `apt-get update`.
- **`aptSources`' `curl` is pinned to `https` (review, hardening).** `--proto '=https'
  --proto-redir '=https'` on the key download, so a same-origin `https` -> `http`
  redirect can no longer fetch the key in plaintext. `keyUrl`'s own allowlist already
  forced the URL's own scheme to `https://`, but did not prevent a redirect.
- **`LD_LIBRARY_PATH` is now a chart-reserved `postgresql.extraEnv` name.** Consistent
  with every other chart-set env var (`pg.validateExtraPassthrough` reserves these
  unconditionally, even for a currently-disabled feature, so a passthrough that works
  today can't start silently shadowing a chart value after a later upgrade enables it).
  A `postgresql.extraEnv` entry named `LD_LIBRARY_PATH` now fails the render regardless
  of `extensions.extraLibs`.

### Fixed

- **`copy-ext`/`copy-base-ext`'s extension-file copy glob (`*.so`) missed versioned
  shared libraries a package places directly alongside its own extension modules
  (#309).** The glob is now `*.so*`, a strict superset of the old match, so this is a
  safe, unconditional fix rather than a new opt-in. Note this covers only libraries
  co-located under `/usr/lib/postgresql/<major>/lib` itself -- it does **not** reach a
  dependency Debian installs elsewhere (the motivating `libsodium.so.23` case); that
  needs `postgresql.extensions.extraLibs`, above.

## 1.13.1 - 2026-08-21

### Fixed

- **`repmgr.image.tag` default (`trixie-5.5.0-31`) predated the #311 agent changes
  (#314).** `trixie-5.5.0-31` was tagged before `dbname` primary_conninfo patching and
  `synchronized_standby_slots` reconciliation (#308/#311) landed, so a fresh 1.13.0
  install with `repmgr.agent.syncReplicationSlots: true` set `wal_level`/
  `sync_replication_slots` correctly via ConfigMap, but ran an agent binary that never
  patched `primary_conninfo` or reconciled `synchronized_standby_slots` -- the feature
  documented in the 1.13.0 release notes silently did nothing on the default image.
  Bumped the default to `trixie-5.5.0-32`, built from current `master` (confirmed via
  `grep -a` on the shipped binary: it now contains `synchronized_standby_slots` and
  `EnsurePrimaryConninfoDBName`/`dbNameReloadPending`). `etcd.rbac.bootstrapImage.tag`
  moves in lockstep, as always. No template or values-schema changes; existing
  `repmgr.image.tag` overrides are unaffected.

## 1.13.0 - 2026-08-21

### Added

- **First-class logical replication support and failover slot sync (#308).** Three
  pieces, needed together for a logical subscriber (`CREATE SUBSCRIPTION`, Debezium,
  etc.) to survive a primary failover:

  - **`postgresql.walLevel`** (enum `replica`|`logical`, default `replica`) is now the
    one authoritative source for `wal_level`, rendered independent of
    `pgbackrest.enabled` (its own render block, not `pgbackrest-archive.conf`) so it
    works whether or not backups are configured. Previously, `pgbackrest-archive.conf`
    hardcoded `wal_level = replica` whenever `pgbackrest.enabled`, silently overriding
    `postgresql.configuration.wal_level` because `include_dir` loads conf.d files in
    filename-sort order and that file sorted last.
  - The agent now patches `dbname` into `primary_conninfo` after every clone, follow,
    and rejoin (and deterministically at every cold start of a standby, not only on a
    repoint) -- repmgr's own clone/follow machinery never included it, which is
    harmless for physical replication but breaks PostgreSQL 17+'s
    `sync_replication_slots` worker (it requires `dbname` to be present). The initial
    `standby clone` (run by the `repmgr-init` init container) now also requests a
    physical replication slot in agent mode, matching the agent's own default, so a
    fresh standby's very first streaming connection is slotted from the start.
  - **`repmgr.agent.syncReplicationSlots`** (default `false`, agent-mode only,
    PostgreSQL 17+, requires `postgresql.walLevel: logical` -- enforced at render time):
    when true, sets `sync_replication_slots = on` and has the primary reconcile
    `synchronized_standby_slots` to its current, live standbys' physical replication
    slots on every tick -- so a logical failover slot
    (`CREATE SUBSCRIPTION ... WITH (failover = true)`) survives a promote instead of
    needing a full resync. The live standby set is derived from repmgr's own node
    registry (excluding genuine scale-down ghosts), not from momentary replication-slot
    activity, so a standby restart or brief network blip does not empty the GUC and
    temporarily strand the logical consumer. `synchronized_standby_slots`/
    `sync_replication_slots` do not exist before PostgreSQL 17, so a render-time guard
    rejects the combination with an older `postgresql.majorVersion` (agent mode only --
    the value is inert, and so not guarded, outside it). Verified live on a 3-node KinD
    cluster through install, promote/failover, and scale-down.

  See README ["Logical Replication"](README.md#logical-replication-308) for the full
  detail. The default render is byte-stable (nothing renders differently unless
  `postgresql.walLevel` or `repmgr.agent.syncReplicationSlots` is touched).

### Changed

- `pgbackrest-archive.conf` no longer hardcodes `max_wal_senders = 10` (#308) --
  redundant with the image's own initdb default, and it was silently clobbering a
  custom `postgresql.configuration.max_wal_senders` whenever pgBackRest was enabled.
- **Compatibility note:** `postgresql.configuration.wal_level` is no longer accepted;
  use `postgresql.walLevel` instead (#308). `wal_level` now has exactly one
  authoritative source. If you were setting `postgresql.configuration.wal_level`
  directly with `pgbackrest.enabled: false` (the only combination where it took
  effect), move the value to `postgresql.walLevel` (enum `replica`|`logical`) -- the
  old key now fails at render time with a guard message naming the fix. Everyone else
  is unaffected.

## 1.12.0 - 2026-08-18

### Added

- **`prometheusExporter.prometheusRule`: alert on stuck WAL archiving and pg_wal disk
  usage (#305).** When `archive_command` gets stuck (pgBackRest repository unreachable
  or full), PostgreSQL correctly refuses to recycle un-archived WAL, and nothing
  previously surfaced that growth before it filled the shared PGDATA/`pg_wal` volume.
  This is an **observability-only** fix -- it does not throttle writes or take any
  automatic corrective action when a threshold is crossed; that remains a separate,
  larger change to the Go failover agent's reconcile loop, deliberately out of scope
  here (see README ["WAL Disk Usage"](README.md#wal-disk-usage-305) for the full
  reasoning).

  `prometheusExporter.prometheusRule.enabled` (default `false`) ships a `PrometheusRule`
  wiring `PGWALArchiveFailing`/`PGWALArchiveStale` (the existing `pg_wal_archive_*`
  metrics from #30, collected since 1.x but never wired to an alert until now) and
  `PGWALSizeHigh` (`pg_wal_size_bytes`, the exporter's own **built-in** `wal` collector --
  see below) to configurable thresholds (`staleArchiveSeconds`, `walSizeBytesThreshold`)
  and `for` durations (`archiveFailingFor`/`archiveStaleFor`/`sizeHighFor`).

  **No new metric was added.** The original version of this change defined a chart-side
  `pg_wal_size` query group to report `pg_wal` disk usage. Live-testing against the
  actual shipped exporter image (`quay.io/prometheuscommunity/postgres-exporter:v0.19.1`)
  before merge caught that its `pg_wal_size_bytes` name collides with a metric the
  exporter's own built-in `wal` collector already emits (enabled by default) under the
  same name but different help text -- Prometheus client libraries reject that as a
  duplicate-metric registration, which fails the **entire** `/metrics` scrape, not just
  the new metric. The chart-defined query was deleted; `PGWALSizeHigh` alerts on the
  exporter's pre-existing `pg_wal_size_bytes` directly, so this ships with no new query
  and no new `pg_monitor` grant dependency.

  **Also from review:** the README/values now call out that `prometheusRule.enabled`
  alone is a no-op without `serviceMonitor.enabled` (or an equivalent scrape) actually
  feeding the exporter's metrics to Prometheus.

## 1.11.0 - 2026-08-17

### Added

- **`postgresql.extensions.packages`: install PGDG/Debian extension packages without a
  custom image (#303).** Generalizes the existing `postgresql.extensions.enabled`
  copy-based mechanism: `copy-ext`/`copy-base-ext` can now `apt-get install` a
  render-time-validated package list (`{major}`-substituted against
  `postgresql.majorVersion`, optionally version-pinned with apt's `=` syntax) into their
  own filesystem before the existing lib/share copy, so an extension the donor image
  never shipped (e.g. `postgresql-<major>-cron`) reaches the server the same way
  `postgresql.extensions.enabled` always has. Mechanically: neither init container mounts
  `ext-lib`/`ext-share` at the real native extension paths — only the main postgresql
  container does — so `apt-get install` writes real files there and the existing
  `cp`/`cp -n` (#302) step sweeps them up unchanged.

  Off by default (`packages: []`; a default render is byte-identical to 1.10.2). When
  set, the two init containers run root-transiently for the apt step only (capabilities
  narrowed, not unrestricted — confined to a throwaway container that persists nothing),
  with their own values-overridable `postgresql.extensions.installResources` rather than
  the shared, lighter `pg.initResources`. A render-time guard rejects any package entry
  containing shell metacharacters (the list is interpolated into an `apt-get install`
  shell command) and rejects `packages` set without `extensions.enabled: true`.

  **Review follow-up:** an unversioned PGDG extension dependency on `postgresql-<major>`
  is normally left alone by apt, but nothing stopped some *other* future package from
  declaring a stricter dependency and pulling in a newer `postgresql-<major>` as a side
  effect — silently swapping the very libs about to be copied for a build from a
  different point release than the postmaster this chart actually starts (the #302
  failure mode, one layer up). `copy-ext`/`copy-base-ext` now detect the already-installed
  `postgresql-<major>` version (`dpkg-query`) and pin it on the same `apt-get install`
  line, so apt either leaves it alone (the normal case, confirmed live) or fails the
  install outright — never a silent swap.

  See README ["Installing extensions without a custom image"](README.md#installing-extensions-without-a-custom-image)
  for a complete `pg_cron` example, why `postgresql.databases[].extensions` (not
  `postStart.additionalCommands`) is the right mechanism for the `CREATE EXTENSION` step
  even against the bootstrap database, the PGDG apt-source assumption for a non-default
  `postgresql.image`, the `networkPolicy.postgresql.extraEgress` this requires, and the
  explicit limitation for extensions with no Debian/PGDG package.

### Fixed

- **`shared_preload_libraries` never actually applied before the post-install hook Jobs
  ran on a fresh install (found while verifying #303's own `pg_cron` recipe).**
  `shared_preload_libraries` is a postmaster-restart-only parameter. On `helm install`,
  the repmgr image's entrypoint bakes `shared_preload_libraries = 'repmgr'` directly at
  `initdb`; the chart's merged value (repmgr + any operator-declared libraries + pgaudit)
  lives in a conf.d file that was only spliced into `postgresql.conf` by the `postStart`
  hook, after postgres was already accepting connections -- too late for a
  restart-only GUC, and nothing forced the restart a `helm upgrade`'s config-checksum
  rolling restart would have provided. The `databases-roles`/`audit-extension` hook Jobs
  then failed `CREATE EXTENSION ... must be loaded via shared_preload_libraries`, and
  (per `hook-delete-policy: before-hook-creation,hook-succeeded`) sat failed rather than
  retrying. Pre-existing, not introduced by #303 -- confirmed the identical failure on
  unmodified 1.10.2 with only `postgresql.audit.enabled: true` set.

  Fixed at the source: `repmgr.image.tag` bumped to `trixie-5.5.0-31`, whose entrypoint
  now wires the chart's conf.d `include_dir` into `postgresql.conf` at `initdb` time --
  before its own bootstrap `pg_ctl start` -- so the merged `shared_preload_libraries` is
  active from the very first postmaster start on a fresh install, no restart required.
  Verified live on a fresh `helm install` for both the `pg_cron` recipe above and
  `postgresql.audit.enabled: true`. `etcd.rbac.bootstrapImage.tag` bumped alongside it,
  same lockstep requirement as always.

## 1.10.2 - 2026-08-14

### Fixed

- **`copy-ext` could silently overwrite `copy-base-ext`'s libs with a mismatched build
  (#302).** When `repmgr.enabled=true` and `postgresql.extensions.enabled=true`,
  `copy-base-ext` populates `ext-lib`/`ext-share` from the repmgr image -- the image that
  actually runs the server -- and `copy-ext` then ran an unconditional wildcard `cp` from
  `postgresql.image` on top of it. If the two images drifted to different PostgreSQL point
  releases (`postgresql.image` is often pinned to a floating tag), `copy-ext` silently
  replaced core libs such as `libpqwalreceiver.so` with a mismatched build, and every
  freshly-created pod failed to stream replication (`undefined symbol`) -- `ext-lib`/
  `ext-share` are emptyDirs, so this hit on every pod, not just the first. `copy-ext`'s
  copy is now `cp -n` (no-clobber): it can only add files the repmgr image doesn't already
  provide (e.g. `vector.so`), never overwrite one.

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

### Added

- **`control.restore`'s job-create grant is now bounded by a `ValidatingAdmissionPolicy`**
  (#279), rendered by default whenever `repmgr.agent.control.restore.enabled` is set.

  1.9.0 shipped API-driven restore with an honest warning rather than a fix: triggering a
  restore needs `create` on `jobs`, RBAC **cannot** restrict a create by `resourceName`, and
  so the grant let anything holding the database pods' ServiceAccount token create arbitrary
  Jobs in the namespace — naming any ServiceAccount, with no separate permission check. On a
  token mounted beside a PostgreSQL that runs user-supplied SQL, that is a namespace-wide
  privilege-escalation primitive, and the documented advice was to leave the feature off.

  The missing restriction is content-based, which is a layer RBAC does not reach.
  `ValidatingAdmissionPolicy` is exactly that layer, in-tree and GA since Kubernetes 1.30 —
  no webhook, no serving certificate, nothing that can be down. The policy polices **one
  subject**, this release's ServiceAccount, and requires of every Job it creates:

  | Pinned | Why |
  |---|---|
  | `metadata.name` == the agent's one deterministic Job name | the `resourceName` restriction RBAC refused to give — `create` collapses to a single object |
  | `pg-ha/restore=<fullname>` label | provenance, and it fails closed if chart and policy drift |
  | `serviceAccountName` == the release's own SA | removes the escalation; what is left is "the privileges I already hold" |
  | `automountServiceAccountToken: false`, stated explicitly | makes SA-naming moot: the pod gets no token |
  | no `hostNetwork` / `hostPID` / `hostIPC`, no explicit `nodeName`, only this release's `priorityClassName` | no escape to the node, no placing the pod by hand, and no claiming a high-priority class to make the scheduler preempt this release's own postgresql pods |
  | the **pod's own labels**, limited to this release's restore labels plus the batch controller's | a restore pod labelled `component=postgresql` would join the write Service's endpoints — Ready immediately, since it has no readiness probe — and receive application traffic and credentials |
  | no `manualSelector` | the Job's selector and identity labels stay server-assigned |
  | one container, no init or ephemeral containers, running this release's repmgr image | bounds what code enters the cluster on this token |
  | `command` == this release's restore entrypoint, no `args`, no `lifecycle` hooks | the image pin alone is weak: the postgresql container already runs this image against this volume, so without this the Job is "run anything as the database's uid with the live PGDATA mounted" |
  | this release's pod and container `securityContext` | no `privileged`, no `runAsUser: 0`, no added capabilities — otherwise the Job is a strictly *larger* privilege set than the token started with |
  | requests and limits present; `parallelism`/`completions` ≤ 1 | the one permitted Job name is otherwise a repeatable way to fill the namespace quota and evict the database's own pods |
  | volumes limited to the three the restore template renders | with the token gone, mounting another workload's Secret was the remaining way out — this closes it, along with `hostPath` and projected tokens |
  | no `envFrom`; `valueFrom` limited to the downward API and the release's own Secrets | the same bound for env |

  The security contexts, resources and command are pinned to **what this release's values
  render**, not to a fixed hardened profile, so changing `postgresql.containerSecurityContext`
  moves the pin with it — a chart that denied its own restore Job would be worse than no
  policy.

  Every other creator of Jobs — humans, GitOps controllers, the CronJob controller, other
  workloads — is **unaffected**; that direction is asserted in the KinD suite alongside the
  denials, because a policy that broke every other controller in the namespace would be
  worse than the hole it closes.

  `failurePolicy: Fail` is load-bearing: under `Ignore` an evaluation failure would
  silently re-open the hole. In the same spirit the grant and its bound cannot be separated
  — rendering the RBAC without the policy **fails the render** unless you say so in values:

  ```yaml
  repmgr:
    agent:
      control:
        restore:
          enabled: true
          allowedClientCNs: [dba-break-glass]
          admissionPolicy:
            enabled: true            # default
            acknowledgeUnbounded: false
  ```

  **What it does not bound**, stated plainly because it decides whether the feature belongs
  on a given cluster: the restore *parameters*. Anything holding the token can still create
  the one permitted Job with its own `TARGET`/`BACKUP_SET`/`FORCE` — this release's own
  restore, over the live PGDATA, without presenting a certificate to the control API. A
  restore over the live data directory is the operation being exposed, so admission has
  nothing left to reject; pgBackRest's `postmaster.pid` interlock still means this needs the
  StatefulSet already scaled to 0.

  Nor is the command pin a sandbox: bash reads `$BASH_ENV`, and an actor who already runs code
  in the postgresql container can write a file into PGDATA, which this Job mounts. That
  reaches uid 101 with no token and only this release's volumes — the privileges already held,
  which is the bar — but the image, security-context, volume and env pins are what hold it,
  not the command pin alone.

  So the policy turns "namespace-wide privilege escalation from a SQL injection" into "an
  unauthenticated trigger for this release's own restore". That is a large reduction, and it
  is what makes the feature defensible rather than advisory — but where untrusted SQL runs
  and an unscheduled restore would itself be a serious incident, leaving `control.restore`
  off remains the right answer.

- `pgbackrest.restore.mode=cronjob` now renders `jobTemplate.metadata.labels` with
  `pg-ha/restore=<fullname>`. Both `kubectl create job --from=cronjob/...` and the control
  API's clone copy jobTemplate labels onto the Job they create, and nothing else does — so
  a cloned restore Job is now selectable (`kubectl get jobs -l pg-ha/restore=<fullname>`)
  where before it carried no labels at all. Deliberately *not* the full `app.kubernetes.io`
  set: a Job the agent creates is not Helm-managed, and claiming otherwise invites a GitOps
  controller to prune it mid-restore.

### Upgrading

- **Requires Kubernetes ≥ 1.30 and cluster-scoped `create` on
  `admissionregistration.k8s.io` (`ValidatingAdmissionPolicy`, `ValidatingAdmissionPolicy-
  Binding`) — but only if you enable `repmgr.agent.control.restore`.** These are this
  chart's first cluster-scoped objects; a default install renders none of them, so a
  namespace-limited installer is unaffected unless it opts into restore triggering.
- Already running `control.restore.enabled: true` on 1.9.0 and cannot render cluster-scoped
  objects (or are below 1.30, or manage one policy centrally)? Set
  `repmgr.agent.control.restore.admissionPolicy.enabled: false` **and**
  `acknowledgeUnbounded: true` to keep 1.9.0's behaviour, which the render otherwise
  refuses. Without the API the render **fails** rather than letting the apply abort halfway
  with "no matches for kind ValidatingAdmissionPolicy", so an upgrade cannot leave a release
  half-applied. The precondition checked is the presence of `admissionregistration.k8s.io/v1`,
  not the reported Kubernetes version: with no cluster to query, `.Capabilities.KubeVersion` is
  the *helm client's* built-in version (3.14 reports v1.29), so a version floor would break
  every `helm template` run by an older helm regardless of the target cluster.
- Values interpolated into the policy's CEL expressions (`pgbackrest.existingSecret.name`,
  `pgbackrest.repoEncryption.existingSecret.name`, the image reference, `fullnameOverride`)
  are now charset-validated at render time. A name containing a quote or whitespace fails
  the render with the offending value named, instead of producing a policy the API server
  rejects — or, worse, one whose validation is a tautology.
- A cluster whose **mutating** admission rewrites `Job` objects (sidecar injectors act on
  Pods and are unaffected) can make the cloned Job stop matching a pin. The denial names the
  exact field, and the same two values turn the policy off.

## 1.9.0 - 2026-08-01

### Added

- **Authenticated control REST API for the agent** (#276), off by default. Agent mode only.

  The existing pause and switchover runbooks are `kubectl annotate` calls on the marker
  ConfigMap. They stay the reference, and they need no extra machinery — but they cannot
  check a request before accepting it: `kubectl annotate ... switchover-target=<pod>`
  succeeds even when the pod does not exist, is on a divergent timeline, or is far behind,
  and you find out by reading logs. Nor can they tell you each member's replication
  position, or *why* the loop is not doing what you expected.

  ```yaml
  repmgr:
    failoverMode: agent
    agent:
      control:
        enabled: true
        tls:
          existingSecret: pg-control-tls    # tls.crt, tls.key, ca.crt — all three
        allowedClientCNs: [ops-admin]       # optional; empty = any cert the CA signed
  ```

  It is a **facade, not a second control plane**: pause/switchover write exactly the marker
  annotations kubectl writes, and the reconcile loop remains the sole authority for when
  anything happens — so kubectl and the API stay equivalent. What it adds is preflight
  validation, a synchronous answer, and state that existed nowhere else:
  `GET /v1/cluster` reports every member's timeline/LSN **and the loop's latest decision
  with its reason**, which was previously log-only.

  Surface: `GET /v1/status`, `GET /v1/cluster`, `POST /v1/pause`, `POST /v1/resume`,
  `POST /v1/switchover`, `DELETE /v1/switchover`, `POST /v1/restart`, `POST /v1/reload`,
  `POST /v1/reinitialize`, `GET /v1/backups`, and `GET`/`POST`/`DELETE /v1/restore`.

  `POST /v1/reinitialize` rebuilds a standby that cannot rejoin on its own, replacing the
  "delete the PVC and the pod" runbook: it stops PostgreSQL and empties the data directory,
  and the loop's ordinary empty-data clone path does the rebuild — no second clone
  implementation. **Replica only**: it refuses on the lease holder (checked against the
  lease, not a cached role), refuses a node running read-write without the lease, refuses
  while paused (a paused loop would never re-clone), and requires `force: true`. The wipe
  itself refuses anything that is not an initialized data directory, or that still has a
  `postmaster.pid`. No extra RBAC.

  Security posture:

  - **mTLS only** — no token, no password, no plaintext, TLS 1.3, and reads are
    authenticated too. Enabling it without all three TLS files **fails the render**, so you
    cannot end up with an unauthenticated mutating port. The material is re-read per
    handshake, so a rotated (cert-manager-renewed) Secret needs no pod restart.
  - **Its own port** (`9201`), never `9200`. The metrics port gains no route, and a render
    guard refuses `control.port: 9200`. Under `networkPolicy.enabled` the control port gets
    no ingress rule at all — deny-by-default in an allowlist policy — with
    `networkPolicy.agentControl.extraIngress` to admit a named client. `kubectl
    port-forward` bypasses the pod network on most CNIs, so it stays the path for humans.
  - **No control Service**: node-local verbs act on whichever pod answers, so each request
    must name the pod it addresses (`{"node": "..."}`) and is refused with 409 otherwise.
  - Structured audit line per mutating call (client CN, certificate fingerprint, serial),
    plus `pg-ha/paused-by` and `pg-ha/switchover-requested-by` on the marker so the
    provenance survives a pod restart and shows in `kubectl describe`.
  - New read-only metrics: `pg_ha_agent_control_requests_total`, `_control_rejected_total`,
    `_control_intents_total`, `_control_restore_requests_total`.

  Enabling the API adds **no Kubernetes RBAC**.

- **API-driven PITR restore** (`repmgr.agent.control.restore`, #276), a **separate** opt-in
  on top of the API, because it is the one verb that widens the database pods' privileges:
  `create` on `jobs` cannot be restricted by `resourceName`, so the grant lets anything
  holding the pods' ServiceAccount token create arbitrary Jobs in the namespace — and a
  Job's pod may name any ServiceAccount. That is a namespace-wide privilege-escalation
  primitive on a token mounted beside a PostgreSQL that runs user-supplied SQL, and the
  kubectl path (`kubectl create job --from=cronjob/...`) needs none of it. It is documented
  in the README and in `rbac.yaml` itself; leave it off unless the trade is worth it.

  What the grant buys is choosing the recovery point **in the request**
  (`targetType`/`target`/`backupSet` are applied to the Job the agent creates), so an
  operator can recover to an arbitrary timestamp with one call. The kubectl `mode: cronjob`
  runbook cannot: it needs the target in values and a `helm upgrade` first. Without that
  requirement, use the kubectl path and skip the RBAC entirely.

  What bounds it:

  - The Job is a **verbatim clone** of the rendered restore CronJob's `jobTemplate` —
    identical to `kubectl create job --from`, so image, ServiceAccount (token still
    unmounted), security contexts, volumes and secret references all come from the release.
    Only `TARGET_TYPE`/`TARGET`/`BACKUP_SET` are overridden, and an env backed by
    `valueFrom` is never overwritten (a `secretKeyRef` cannot become a request-supplied
    literal).
  - **Which volume is restored into is not an API decision** — that is
    `pgbackrest.restore.podOrdinal`, rendered into the Job; the body may only confirm it.
  - The API **never sets pgBackRest's `--force`**, so the `postmaster.pid` interlock stays
    armed; the stale-pid bypass remains a reviewable values change.
  - Requires `force: true`, `confirm: "<statefulset name>"`, a **second** restore-only CN
    allowlist, and a **paused** cluster; one call then verifies, stops the local postmaster
    and creates the Job.
  - It deliberately does **not** scale the StatefulSet — scaling to 0 deletes every agent,
    including the one that would report progress — and returns the remaining commands in
    `nextSteps`. Net effect: it removes the `kubectl create job --from` step, nothing more.
    Whether a scale-down is needed at all depends on scheduling: ReadWriteOnce binds a
    volume to a NODE, so a Job co-scheduled with the target pod starts immediately. What
    keeps a restore off a live data directory is the required pause plus the postmaster
    stop, not the scale-down.
  - **The runbook ends with `POST /v1/resume`, and it is not optional.** Maintenance mode
    makes the reconcile loop a no-op, so a restored node scaled back up while still paused
    never starts PostgreSQL and never goes Ready. The API will not clear an operator's
    pause on its own; `nextSteps` lists the resume as a required step.

  Progress and provenance: `restore.sh` now records the outcome of each restore attempt to
  `pgbackrest-restore.status` beside PGDATA (backup set, target, exit code, post-restore
  control-file state, requester). It outlives the Job and its logs, and the API returns it
  as `lastRestore` on `GET /v1/status` — which also doubles as a record of where a data
  directory came from. `GET /v1/status` additionally reports **WAL-replay progress**
  (`recovery.replayLsn`, `replayLagBytes`, `lastReplayTime`), which for a PITR is usually
  the phase you are waiting on. Live file-copy percentage needs
  `restore.readPodLogs: true`, which grants namespace-wide `get pods/log` and only helps
  when an agent is still running (`podOrdinal > 0`); it is off by default.

### Fixed (pre-release, from the review of this feature)

- The reinitialize replica-only gate now reads the leader lease **live from the DCS** and
  additionally requires that the durable primary marker not name this pod, instead of
  trusting the once-per-tick snapshot. The control listener starts before the first
  reconcile tick, when that snapshot is all-zero, so the previous check could have admitted
  a wipe of the actual primary during startup. A pod that has not published an observation
  yet is refused outright. The restart interlock for the serving primary reads the live
  lease for the same reason.
- `POST /v1/resume` and `POST /v1/reinitialize` now refuse while a restore Job is `pending`
  or `running` (fail-closed if that cannot be determined). The restore runbook ends in a
  resume, so resuming too early would have let the reconcile loop start PostgreSQL on a data
  directory pgbackrest was still rewriting.
- `POST /v1/restore` overrides only the recovery-point fields the request actually
  specifies. It previously blanked a `pgbackrest.restore.targetType`/`target` pinned in
  values whenever a request omitted them, restoring the latest backup and replaying all WAL
  instead of stopping at the reviewed point in time. The response now reports
  `effectiveTargetType`/`effectiveTarget`/`effectiveBackupSet`, read back from the created Job.
- The data-directory wipe now distinguishes a **stale** `postmaster.pid` (its process is
  gone -- what a crashed postmaster leaves behind, and precisely the replica reinitialize
  exists to rebuild) from a live one, which remains a hard refusal. An unreadable or
  malformed pid file is treated as live.
- `replace: true` waits for the previous restore Job to be fully deleted before creating its
  replacement. Foreground propagation returns while the object still exists behind its
  finalizer, so the create hit `AlreadyExists` -- after the handler had already stopped
  PostgreSQL.
- A failed restore no longer overwrites the data directory's provenance: `restore.sh` keeps
  the previous record's backup set and target (many failures copy nothing at all) and
  records the failed attempt separately as `attemptedTargetType`/`attemptedTarget`/
  `attemptedBackupSet`. A mistyped PITR target previously erased where the data really came from.
- `hasData` is reported only for the node answering the request and is absent for peers,
  rather than being emitted as `false` for every peer -- including a primary plainly holding
  data, and a replica whose re-clone had in fact completed.
- The restore feature gate now emits its audit line and increments
  `pg_ha_agent_control_rejected_total` itself. It short-circuits the authorization check by
  design (so a missing values flag reports as such rather than as a 403), which meant
  probing of the most destructive routes left no trace in the audit log or metrics.
- `GET /v1/restore` is feature-gated like the mutating restore routes: it needs the `get
  jobs` grant that is rendered only when restore is enabled, so it previously failed with a
  Forbidden error that read as a broken Role.
- The control server's response-write deadline is derived from the request budget instead of
  being a fixed 90s, so a long intent on a release with a wide reconcile interval returns
  its 504 or 200 rather than a dropped connection.
- The switchover preflight compares the candidate's timeline against the **lease holder**
  rather than against whichever pod answered the call. There is no control Service, so a
  request addressed to a standby that was itself behind on timelines would refuse a valid
  candidate and — the dangerous half — accept one on the stale timeline, yielding a `202`
  the loop then silently sat on. An unreadable primary timeline, an invisible lease holder
  and a cluster with no lease holder are now all refusals instead of a skipped comparison.
- Node-local operations now run under the **request's** deadline instead of the agent's
  process-lifetime context. A postmaster that ignored SIGINT would otherwise hold the
  operation mutex indefinitely: the reconcile loop stops ticking and the leadership fence
  blocks behind the same mutex, so a node that comes back read-write is never demoted. The
  stop now escalates to SIGKILL exactly as the fence path does; a restart still brings
  PostgreSQL back up afterwards, while a forced stop for a restore is reported as a failure
  (SIGKILL leaves the `postmaster.pid` that guards the data directory).
- The `lastRestore` record is removed when the data directory stops being what it describes
  — a reinitialize wipe, or a clone by the reconcile loop. It lives beside PGDATA so it can
  outlive the restore Job, which meant a wipe could not reach it and a rebuilt replica went
  on reporting a backup set as the provenance of data streamed from the primary.
- `restore.sh` keeps the attempted recovery point in separate variables instead of packing
  it into one `|`-delimited string, so an operator- or API-supplied target containing that
  character no longer corrupts the failure record.
- Request bodies with trailing content after the JSON object (`{"force":true} {...}`) are
  rejected rather than silently half-read, matching the existing unknown-field strictness.
- Dropped `truncated` from the restore-progress payload: nothing ever set it, so it
  advertised a signal the agent does not produce.

### Changed

- The agent's read-only `:9200` server now sets HTTP timeouts (read-header/read/write/idle)
  and a header-size cap. It had none, so a client opening connections without sending
  requests could accumulate goroutines in the process that supervises PostgreSQL.

## 1.8.1 - 2026-07-31

### Added

- **PostgreSQL 17 is selectable; 18 remains the default** (#269). The chart was already
  written to be major-agnostic — `postgresql.majorVersion` and `repmgr.image.majorVersion`
  both exist, and the render guard only enforces that they *agree* — but the repmgr image
  was built PG18-only. Because repmgr mode takes the server binaries from that image,
  pointing `postgresql.image` at another major was **silently inert**, and the only way to
  run one was to fork the image.

  The major is now a build argument (`PG_MAJOR`, default 18) and each release publishes one
  multi-arch, attested, cosign-signed manifest per major:

  | Tag | PostgreSQL |
  |-----|------------|
  | `trixie-5.5.0-29` | 18 — the default; what every unsuffixed pin resolves to |
  | `trixie-5.5.0-29-pg18` | 18, named explicitly |
  | `trixie-5.5.0-29-pg17` | 17 |

  To run 17, move `postgresql.majorVersion`, `repmgr.image.majorVersion` and
  `repmgr.image.tag` together — see
  [Choosing the PostgreSQL major](README.md#choosing-the-postgresql-major). Reasons it may
  matter: repmgr 5.5.0's upstream install requirements list PostgreSQL **13–17, not 18**
  (the 18 default rests on distro packaging rather than an upstream support claim); PGDG
  extension availability varies by major; and `pg_dump` output is not guaranteed to load
  into an older server, so the major a cluster is created on constrains where its data can
  later go. PostgreSQL 17 is supported upstream until 2029-11-08.

  **This is a create-time choice, not an upgrade path** — a new-major server refuses to
  start on an old-major `PGDATA`, so moving an existing cluster means a dump/restore into a
  fresh release.

- **Both majors run the whole live suite in CI**, not a spot-check: every suite (failover,
  chaos, pgBackRest restore, TLS, pgpool, etcd DCS, repmgrd→agent migration) now runs on 17
  as well as 18, via a `pg_major` matrix axis and `pg/tests/set-pg-major.sh`. Each published
  image is also started and verified before release — server version, `repmgr`/`repmgrd`
  resolving at 5.5.0, and `pgaudit` actually loading — so `audit.enabled=true` works on
  either major.

### Changed

- `repmgr.image.tag` / `etcd.rbac.bootstrapImage.tag` → `trixie-5.5.0-29`, which carries the
  `PG_MAJOR` parameterisation. **An unchanged values file produces an unchanged result**:
  the unsuffixed tag is still PostgreSQL 18, and the render is byte-identical apart from
  the tag itself.

### Fixed

- The major-mismatch render guard was only tested in one direction (moving
  `postgresql.majorVersion`). It is now asserted symmetrically, so moving
  `repmgr.image.majorVersion` alone — the direction the parameterisation makes possible —
  also fails fast instead of running one major while building extension paths for another.

- **The image tag now has to agree with the major it claims.** The `-pgNN` suffix is what
  actually selects the server major, but the guard above only compared two hand-typed
  `majorVersion` values — so moving the tag without the majors (or the reverse) rendered
  cleanly and ran the wrong major: a crash-looping extension init container with
  `extensions.enabled=true`, a silently wrong major without it. The render now fails when a
  `-pgNN` tag disagrees with `repmgr.image.majorVersion`, and `PG_MAJOR` is passed to every
  container running the repmgr image so the unsuffixed case is caught too — the entrypoint
  and the agent refuse to start an image that does not bundle the requested major, naming
  both sides.

- `etcd.bootstrapImage.tag` was never read: the etcd subchart takes it at
  `rbac.bootstrapImage`, so the RBAC-bootstrap Job stayed on the subchart's older default
  (`trixie-5.5.0-24`) while every other container moved with the chart. Anyone mirroring
  only the tags the chart names got ImagePullBackOff on the post-install Job, leaving etcd
  auth disabled and every agent with full-keyspace access. Now nested correctly, so one tag
  covers the whole render.

- The repmgr image's shell layer and Go agent hardcoded `/usr/lib/postgresql/18/bin` in
  seven places, so the bindir is now derived from `PG_MAJOR` (`repmgr-common.sh` exports
  `PG_BINDIR`; the agent derives it from `config.PGMajor`). The agent refuses to start when
  that bindir holds no `postgres` binary, rather than failing mid-reconcile after taking the
  lease.

- The agent's vendored `google.golang.org/grpc` (1.79.3 → 1.82.1) and `golang.org/x/text`
  (0.37.0 → 0.39.0) carried two advisories that `govulncheck` reports as reachable from
  the etcd DCS and Service-patch paths: **GO-2026-6061** (xDS RBAC / HTTP-2 transport) and
  **GO-2026-5970** (infinite loop on invalid input). Bumped and re-vendored; no agent
  source changed.

- The live suites pinned `repmgr.image.tag: trixie-5.5.0-27` in their fixtures while the
  chart shipped `-28`, so they pulled an older published image instead of the one CI
  builds from source. `set-pg-major.sh` now retargets every fixture at the chart's own
  tag before a suite runs, on both majors — so CI tests the image it built. The two suites
  that deliberately pin an *older released* image (the repmgrd→agent migration and the
  repmgrd TLS leg) are marked `set-pg-major: keep` and left alone on the default major, so
  retargeting does not quietly turn "upgrade from a published image" into "upgrade an image
  to itself"; on a non-default major, where no older published image exists, they are
  retargeted with a logged note that the coverage does not apply there.

## 1.8.0 - 2026-07-31

### Added

- **`pgbackrest.bootstrap.*` — automatic recovery from a lost PVC** (#266). Losing replica 0's
  volume was quietly the worst kind of outage: the pod came back, the entrypoint found an empty
  data directory, and it `initdb`'d a **brand new empty cluster**. The backups were intact in S3,
  the live database was empty, and nothing failed loudly. Set `pgbackrest.bootstrap.enabled=true`
  and an init container (before `repmgr-init`) seeds an *empty* PGDATA from this release's own
  repository; PostgreSQL then replays the archived WAL on startup and promotes. A lost volume
  self-heals with no operator action.

  It needs no changes to the repmgr image: once PGDATA is populated, `init-repmgr.sh` defers the
  start decision and the entrypoint skips `initdb`, so recovery follows the same path as a
  scale-up after the #226 restore Job.

  **Safe to leave enabled** — it only ever writes into an *empty* data directory:
  - empty + repository has backups → restore, write a completion marker, replay WAL on startup;
  - empty + repository has **no backup yet** → do nothing, so a normal first install proceeds
    (`restore` exits non-zero with no backup, and Kubernetes retries a failed init container
    forever, so this case is the difference between "works" and "no pod ever starts");
  - empty + repository **unreachable** → **fail loudly**, pod stays in `Init`. The backup state
    is unknown, and initializing an empty cluster then would destroy a probably-recoverable
    database. Reached only when PGDATA is empty, so a pod rescheduled with an intact volume
    never depends on S3 being reachable;
  - already initialized → refuse to touch it, marker or not. An ordinary restart, rollout or
    re-schedule can therefore never re-restore over a running cluster;
  - partially restored (an attempt that died mid-flight) → resume with `--delta`, which is what
    makes the init container safe for Kubernetes to retry;
  - any replica other than 0 → nothing; standbys are cloned from the primary by repmgr.

  The marker at `$PGDATA/.pgbackrest-bootstrap-complete` records the stanza, backup set, target
  and system identifier. It lives inside PGDATA on purpose: it shares the volume's lifecycle, so
  losing the volume clears it exactly when a fresh bootstrap becomes correct again.

  Orthogonal to `pgbackrest.restore` (#226) and combinable with it — `bootstrap` populates an
  empty directory automatically, `restore` overwrites a live one under operator control. Also
  supports `bootstrap.targetType`/`target` and `bootstrap.backupSet`, with fail-fast guards for
  the target pair and for the `pgbackrest.enabled` + `postgresql.persistence.enabled`
  prerequisites.

  Verified by `make -C pg test-pgbackrest-bootstrap`, which deletes replica 0's PVC outright and
  asserts the **same** cluster returns — the PostgreSQL system identifier is unchanged, which a
  fresh `initdb` could not produce — then restarts the pod and asserts the bootstrap does not run
  a second time.

## 1.7.0 - 2026-07-31

### Added

- **`pgbackrest.restore.*` — a first-class PITR restore resource** (#226). The manual
  restore runbook made the operator hand-build an entire pod spec via `kubectl run
  --overrides='{…}'`: ~30 lines of inline JSON reconstructing the ServiceAccount, security
  contexts, the data PVC mount, the pgbackrest ConfigMap mount and the S3 credential env —
  all of which the chart already knows how to render. Set `pgbackrest.restore.enabled=true`
  and it ships that spec for you, and the runbook becomes four ordinary commands:

  ```bash
  kubectl scale statefulset my-postgres-pg --replicas=0
  kubectl wait --for=delete pod/my-postgres-pg-0 --timeout=5m
  kubectl create job --from=cronjob/my-postgres-pg-pgbackrest-restore restore-now
  kubectl wait --for=condition=complete job/restore-now --timeout=30m
  kubectl scale statefulset my-postgres-pg --replicas=2
  ```

  The resource carries the `-repmgr` ServiceAccount (so `s3.keyType: auto` works) with its
  API token unmounted, the postgresql pod/container security contexts, the
  `data-<fullname>-0` PVC and `<fullname>-pgbackrest` ConfigMap mounts, the S3 and
  repo-encryption credentials, and runs `pgbackrest restore --delta`. It reuses the #38
  validation plumbing, but needs no `pg1-path` override: the mounted `pgbackrest.conf`
  already points at the live PGDATA.

  Notable properties:
  - **Enabling it restores nothing.** It renders an *inert* resource — by default a
    suspended CronJob carrying a schedule that can never fire — so it can be left enabled
    and cloned when disaster strikes, without a `helm upgrade` mid-incident. The
    destructive scale down/up stays an explicit operator action.
  - **Two delivery modes** (`restore.mode`): `cronjob` (default) for
    `kubectl create job --from`, where the resource belongs to the release and a GitOps
    controller sees no stray object; or `job`, a bare Job to render on demand with
    `helm template -s … | kubectl apply -f -` when the point-in-time target must be passed
    inline (`restore.nameSuffix` gives a retry a fresh name, since Jobs are immutable).
  - **It never starts PostgreSQL.** WAL replay and promotion happen when the StatefulSet is
    scaled back up, under the normal chart entrypoint.
  - **No Kubernetes API access is needed to be safe.** pgbackrest itself refuses to restore
    while `$PGDATA/postmaster.pid` exists, so "did you actually scale down?" is enforced
    without an RBAC-carrying token. `restore.force` covers the one legitimate exception, a
    stale pid file left by a crash.
  - PITR target wiring (`restore.targetType`/`target`, the same enum as `validation`, with
    a template `fail` for the "target required once targetType is set" rule), plus
    `restore.backupSet` (`pgbackrest --set`) to pin a specific backup set, and fail-fast
    guards for the two prerequisites (`pgbackrest.enabled`, `postgresql.persistence.enabled`).

  Covered by template tests and by two end-to-end suites, both of which destroy the data
  directory outright and recover it from S3 through the documented runbook:
  - `make -C pg test-pgbackrest-restore` — single node, plus the #38 validation phase;
  - `make -C pg test-pgbackrest-restore-ha` (new) — primary + streaming standby. It restores
    the primary out from under a live standby and confirms the standby rebuilds itself: the
    restored primary comes back on a new timeline, the standby's init container detects the
    mismatch (`Timeline mismatch (local: 1, primary: 2), re-cloning...`), re-clones via
    `repmgr standby clone`, and resumes streaming — with no PVC deletion and no operator
    action. The README documents this as verified behaviour rather than an assumption.

### Changed

- The README's Point-in-Time Recovery runbook is now the four-command flow above; the
  `kubectl run --overrides` pod spec is gone, and the duplicate (and misleading — it had no
  pod to run `pgbackrest` in) PITR recipe in the troubleshooting section now points at it.

## 1.6.0 - 2026-07-30

### Added

- **`postgresql.extraVolumes` / `postgresql.extraVolumeMounts` / `postgresql.extraEnv`**
  (#262) — generic pod-spec passthrough for the postgresql container. Lets an operator
  mount an arbitrary `Secret`/`ConfigMap` as a file that is byte-identical on the primary
  and every standby, without a per-use-case chart change. The motivating case is the
  pgsodium server root key (Supabase Vault) read via `pgsodium.getkey_script`: it must be
  identical across replicas so a promoted standby can still decrypt `supabase_vault` after
  a failover. `extraVolumes` render into the pod template; `extraVolumeMounts` and
  `extraEnv` onto the postgresql container. All default to `[]` (no change to rendered
  output when unset). See the README, "Mounting an extra file on every replica".

  The three values are validated at render time (`pg.validateExtraPassthrough`), keeping
  the chart's fail-fast convention — each plausible mistake would otherwise be a silent
  runtime failure or an apply-time API rejection:
  - each must be a **list** of objects (a map produced an opaque YAML parse error);
  - an `extraVolumes` name may not collide with a chart-managed volume — a `data`
    collision is silently discarded in favour of the volumeClaimTemplate (mount resolves
    into the PVC, expected file absent → CrashLoopBackOff with nothing to point at), and
    with persistence off the duplicate name is rejected by the API server;
  - every `extraVolumeMounts` entry must reference a declared `extraVolumes` entry
    (catches the `extraVolume:`/`extraVolumes:` typo, which `values.schema.json` cannot —
    `additionalProperties` is open — and which otherwise fails only at apply, leaving a
    live release in a failed state);
  - `extraEnv` may not reuse a chart-set env name (`PGDATA`, `POSTGRES_*`, `REPMGR_*`, …);
    duplicate env names are last-wins at runtime, so an override would silently shadow the
    chart/Secret value (wrong data directory, or cluster-wide auth failure).

### Fixed

- **An operator-set `postgresql.configuration.shared_preload_libraries` silently dropped
  `repmgr` whenever `postgresql.audit.enabled` was false**, disabling failover. The merge
  that preserves `repmgr` (and the operator's own libraries) previously ran only on the
  audit path, while `custom.conf` is loaded via `conf.d`'s `include_dir` *after* the repmgr
  image's own `postgresql.conf` — so a bare value overrode
  `shared_preload_libraries = 'repmgr'` and the postmaster started without the repmgr
  library: repmgrd/agent lost their repmgr functions and no standby was ever promoted.
  The merge is now unconditional in repmgr mode, emitted from an authoritative
  `repmgr-preload.conf` that sorts after `custom.conf`. Surfaced while reviewing #262,
  whose pgsodium use case requires exactly this setting. Behaviour is unchanged when audit
  is enabled, and in standalone mode (nothing to preserve) the operator's value still
  passes through `custom.conf` untouched.

## 1.5.0 - 2026-07-12

Requires the new repmgr image (`trixie-5.5.0-28`), which bundles the `pgaudit`
extension. The default `repmgr.image.tag` is bumped accordingly; pgaudit is inert
until `postgresql.audit.enabled` is set, so upgrading with audit off is a no-op
beyond the image pull.

### Added

- **pgaudit-based audit logging (`postgresql.audit.*`)** for compliance regimes
  (SOC 2, HIPAA, PCI-DSS, ISO 27001) that require a per-object record of who did
  what (#219). Opt-in and default-off: `audit.enabled=false` renders identically to
  1.4.3. When enabled, the chart adds `pgaudit` to `shared_preload_libraries`
  (keeping `repmgr` and any operator-declared libraries), renders the `pgaudit.*`
  GUCs into the postgresql ConfigMap, and creates the extension idempotently on the
  primary via a post-install/upgrade hook Job. Because `shared_preload_libraries` is
  a postmaster parameter, toggling audit triggers a controlled rolling restart via
  the existing config-checksum annotation. Knobs: `log` (audit classes),
  `logCatalog`, `logParameter`, `logRelation`, and `role` (object-level auditing via
  `pgaudit.role`). Requires `repmgr.enabled=true` (guarded at render time — the stock
  postgres image used in standalone mode has no pgaudit).

## 1.4.3 - 2026-07-11

Chart-only fix. No image change (`trixie-5.5.0-27`).

### Fixed

- **pgpool no longer wedges in a permanent crash loop after repeated container
  OOM kills.** pgpool allocates its connection-pool state as an `IPC_PRIVATE`
  SysV shared-memory segment. When the pgpool container is OOM-killed the pod (and
  its IPC namespace) survives, but SIGKILL can't run `shmctl(IPC_RMID)`, so each
  restart strands another ~136Mi segment. After enough cycles the pod cgroup fills
  and the kernel OOM-kills `runc init` before pgpool runs, leaving the pod
  recoverable only by deletion (#234). The pgpool container now runs `ipcrm -a`
  to reap orphaned segments before `exec`ing pgpool.

### Added

- **`pgpool.command`** — override the pgpool container startup command. Empty
  (default) keeps the chart's `ipcrm -a` reap + `exec pgpool` sequence (#234).

## 1.4.2 - 2026-07-05

Chart-only fix. No image change (`trixie-5.5.0-27`).

### Fixed

- **pgBackRest PITR restore-validation no longer fails when the primary raises a
  recovery-relevant GUC (e.g. `max_connections`).** PostgreSQL refuses to begin archive
  recovery with `hot_standby=on` unless the recovery instance's `max_connections`,
  `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions` and
  `max_locks_per_transaction` are each `>=` the value the primary had when the WAL was
  written (recorded in `pg_control`). Those tunables live in the chart's `conf.d` overlay,
  which `validate.sh` strips along with the `include_dir` line — so the throwaway instance
  fell back to initdb defaults (`max_connections=100`, etc.) and
  the postmaster died at startup with *"recovery aborted because of insufficient parameter
  settings"*, failing the validation Job even though the backup itself was fully restorable. The script now reads the required minimums straight from
  `pg_controldata` (which reports exactly the primary values recovery checks against) and
  passes them as `pg_ctl` startup overrides, so validation stays green and self-adjusts if
  the primary is ever retuned. Inherited by `pgvector` via its symlinked template.

## 1.4.1 - 2026-06-30

Chart-only fix. No image change (`trixie-5.5.0-27`).

### Fixed

- **Backup integrity check no longer aborts on dumps larger than the pipe buffer (#230).**
  The `backup.enabled` (pg_dump → S3) integrity step (`mc cat … | pg_restore --list`) ran
  under `set -o pipefail`. `pg_restore --list` reads only the header + TOC at the front of a
  `-Fc` dump, so on any dump exceeding the OS pipe buffer (~64 KB — i.e. any real database)
  it exits 0 while `mc cat` is still streaming; `mc cat` was then SIGPIPE-killed (exit 141)
  and `pipefail` propagated that, aborting the Job *before* the staged `.tmp` object was
  promoted to its canonical `backup_<ts>.dump` name — so no usable backup was ever published
  and the CronJob never recorded a success. The check now disables `errexit`/`pipefail`
  around the pipe and inspects both ends via `PIPESTATUS`: `pg_restore` must succeed and
  `mc cat` may only exit `141` (SIGPIPE) — a genuine `pg_restore` parse failure **or** a
  real `mc cat` S3-read error both stay fatal, so a damaged dump is never published as
  verified. The integration test now seeds a table whose
  `-Fc` dump exceeds the pipe buffer so the SIGPIPE path is covered in CI. Inherited by
  `pgvector` via its symlinked template.

## 1.4.0 - 2026-06-26

Chart-only feature. No image change (`trixie-5.5.0-27`).

### Added

- **Declarative databases, roles & grants (#218).** New `postgresql.roles[]` and
  `postgresql.databases[]` turn the chart into a self-serve platform database: a
  post-install/upgrade hook Job idempotently creates the declared roles (LOGIN/NOLOGIN,
  `memberOf` group membership), databases (with `owner` + per-database `extensions`), and
  grants (database-, schema-, and `ALL_TABLES`/`ALL_SEQUENCES`-level, including
  `ALTER DEFAULT PRIVILEGES` for future objects) on the primary — replicated to standbys,
  re-applied on every upgrade and after a restore. Default-empty, so a minimal install is
  byte-identical to before. Passwords are sourced from Secrets and read into the Job via
  psql `\getenv` (never argv, never the ConfigMap); a chart-generated, upgrade-persisted
  password is minted per LOGIN role unless an explicit `passwordSecret` is given. Render
  guards (`pg.validateDatabasesRoles`) enforce identifier safety, uniqueness, reserved-name
  protection, owner resolution, and a GRANT-privilege allowlist so a value can never inject
  SQL. **GitOps caveat:** under `helm template` (ArgoCD) set an explicit `passwordSecret.name`
  for every role, since the chart-generated password relies on a cluster `lookup` that is
  empty during render. Inherited by `pgvector` via its symlinked template.

## 1.3.0 - 2026-06-26

Chart-only feature. No image change (`trixie-5.5.0-27`).

### Added

- **Automated pgBackRest PITR restore-validation (#38).** New opt-in CronJob
  (`pgbackrest.validation.enabled`) that, on a schedule, restores the pgBackRest
  repository into a **throwaway PostgreSQL inside the Job pod** (never the live cluster),
  replays the archived WAL to a consistent state, runs a sanity query, and exits — so an
  unrestorable or corrupt repository raises an alert continuously instead of being
  discovered during a real disaster. Read-only against S3; the throwaway PGDATA lives on
  an emptyDir discarded when the pod ends. Runs from the repmgr image (pgbackrest +
  matching PostgreSQL major) as the postgresql pods' ServiceAccount (so `s3.keyType=auto`
  works), with its token unmounted (it never calls the Kubernetes API). Supports an
  optional PITR target (`validation.targetType`/`target`); the default restores the latest
  backup set and replays all WAL. This is the physical-backup counterpart to the existing
  `backup.validation` job, which only restore-tests the legacy `pg_dump` path. Covered by
  the new `test-pgbackrest-restore` integration test (restore + WAL replay on kind/MinIO).

## 1.2.7 - 2026-06-26

Chart-only fix for the legacy `backup.enabled` (pg_dump → S3) path. No image change
(`trixie-5.5.0-27`).

### Fixed

- **Backup to AWS S3 failed when the secret access key contained `/` or `+` (#221).**
  #167 moved S3 credentials out of the `mc` argv (where they were visible in
  `/proc/<pid>/cmdline`) by percent-encoding them into an `MC_HOST_<alias>` URL, but `mc`
  then signed SigV4 requests with the *encoded* secret — so any key containing URL-reserved
  characters (common in real AWS keys) produced `The request signature we calculated does
  not match the signature you provided` and every upload failed. `backup.sh` and
  `validate.sh` now write a `0600` JSON credential document and load it via `mc alias
  import`, which feeds the **raw** secret to the signer while still keeping credentials out
  of the process argv. The integration test now runs the full backup → validation → restore
  path against a secret key containing `/` and `+` so a regression fails in CI.

## 1.2.6 - 2026-06-26

Image security refresh: repmgr image `trixie-5.5.0-26` → `trixie-5.5.0-27`. No chart
template or behavior change — only the bundled image is updated.

### Fixed

- **Bundled `kubectl` upgraded `v1.31.3` → `v1.36.2` to clear its image-scan CVEs.**
  kubectl 1.31.3 linked Go 1.22.8 and `golang.org/x/net` 0.26.0, which Trivy flagged as
  1 Critical (`CVE-2025-68121`, Go stdlib) plus ~24 High (Go stdlib / `x/net` / `x/oauth2`
  / `moby/spdystream`). The current release links a patched Go 1.25 stdlib and recent
  modules. kubectl is only used by the service-updater for core verbs
  (`get`/`patch`/`label`/`exec`/`rollout`), so the version skew is immaterial.
- **Debian security updates now applied at build time (`apt-get upgrade`).** The
  digest-pinned base shipped stale pre-installed packages; the build now picks up released
  fixes such as `libssh2` `1.11.1-1+deb13u1` (`CVE-2026-7598`, `CVE-2026-55200`, High).

The remaining image-scan findings are Debian packages with no upstream fix yet (the Perl
`CVE-2026-42496` / `CVE-2026-8376` Criticals, plus `libexpat1`/`curl`/`gnupg`/`ncurses`
Highs); they are tracked and will clear on a future rebuild once Debian ships patches.

## 1.2.5 - 2026-06-25

Chart-only correctness fixes for four edge cases in the repmgr/pgBackRest paths
(#211, #212, #213, #214). No image change (`trixie-5.5.0-26`).

### Fixed

- **`fix_user_auth` md5→scram migration silently no-op'd for users whose password contains
  a single quote (#211).** The init script passed the password via `psql -v p=...`; a `'`
  in the value broke psql's `:'p'` literal quoting, so the rehash failed and the migration
  never converged (the md5 pg_hba fallback masked it, so connectivity was kept). The
  password is now read from the environment with `\getenv` — the same injection-safe
  pattern the monitoring-user job already uses — so any password value is handled. Only
  affected `existingSecret` passwords; the chart's `randAlphaNum` generator never emits `'`.
- **pgBackRest cronjob could run the backup against a standby and silently skip it (#212).**
  Primary discovery took the first Ready endpoint from the write Service's EndpointSlices;
  during a failover + scale-down collision, stale EndpointSlices can briefly expose more
  than one Ready endpoint, and with `backoffLimit: 0` a backup that landed on a read-only
  pod was silently dropped for that schedule. The cronjob now validates every Ready
  candidate with `pg_is_in_recovery()` and runs the backup only against the confirmed
  read-write primary. The probe is `timeout`-bounded (a wedged mid-shutdown candidate
  can't hang the run) and retried once; candidates are validated in a deterministic
  (sorted) order; and the EndpointSlice lookup degrades to the actionable "no ready
  endpoint" error instead of a bare `set -e` abort. To avoid regressing the common
  single-node case, when exactly one Ready endpoint exists and its recovery state is
  merely unreadable (not a confirmed standby), the backup proceeds against it — matching
  the prior behavior — rather than being skipped on a transient probe failure; a
  confirmed standby is still never backed up.
- **Inconsistent `required` guard on the pgBackRest S3 secret name (#213).**
  `PGBACKREST_REPO1_S3_KEY_SECRET` (init env) and both keys in the pgbackrest sidecar
  referenced `pgbackrest.existingSecret.name` without the `required` wrapper that
  `PGBACKREST_REPO1_S3_KEY` already had. Functionally safe (the first `required` failed
  render first), but `required` is now applied consistently to every reference.
- **Unbounded ephemeral PGDATA when `persistence.enabled=false` (#214).** With the default
  empty `persistence.emptyDir.sizeLimit`, the `data` volume rendered as `emptyDir: {}`, an
  unbounded ephemeral volume a runaway DB/WAL could use to fill the node and evict
  co-tenants. The volume is now always bounded: `emptyDir.sizeLimit` falls back to
  `persistence.size` (the cap already declared for persistent mode) when unset, then to a
  10Gi floor if `persistence.size` is also blank, so the cap is never null.

## 1.2.4 - 2026-06-24

Chart-only fix for agent-mode pgpool instability at `postgresql.replicaCount: 0` (#207).
No image change (`trixie-5.5.0-26`); only affects `pgpool.enabled` + `repmgr.enabled` +
`repmgr.failoverMode: agent` deployments running primary-only.

### Fixed

- **Agent-mode pgpool churned `restarting myself` and dropped live primary connections
  when `postgresql.replicaCount: 0` (#207).** The pgpool ConfigMap unconditionally
  configured a second backend (`backend_hostname1`) pointing at the `-readonly` Service.
  With zero standbys that Service has no endpoints, so pgpool health-checked a backend that
  could never come up, repeatedly fired failover/failback events, and restarted itself --
  tearing down every client connection to the healthy primary on each cycle. Clients saw
  `EOF`/`unable to read data from DB node 0` even though node 0 was fine. The RO backend is
  now emitted only when `replicaCount > 0`; primary-only agent mode renders as a single
  RW backend (weighted 1) -- a valid, stable single-backend router.

## 1.2.3 - 2026-06-23

Chart-only fix for a postgres-exporter TLS regression (#204). No image change
(`trixie-5.5.0-26`); only affects deployments with `prometheusExporter.enabled` and
`prometheusExporter.sslmode` set to `verify-ca`/`verify-full`.

### Fixed

- **Exporter could not read the TLS CA under `sslmode=verify-ca`/`verify-full`, so every
  scrape failed with `permission denied` and `pg_up` stayed `0` (#204).** The CA secret
  was mounted whole at `defaultMode: 0400` (owner-read only); the exporter runs as a
  non-root UID with no `fsGroup`, so the root-owned `ca.crt` was unreadable. The scrape
  target stayed `up` (the `/metrics` endpoint returns 200 with `pg_up 0`), making it a
  silent monitoring blackout. The exporter's TLS volume now projects **only** the public
  `ca.crt` at a world-readable `0444` -- no `fsGroup` needed. The server cert `tls.crt`
  and private key `tls.key` are no longer mounted into the exporter (it never read them;
  the monitoring user is exempt from client-cert auth). Regression from the #110 mTLS work.

## 1.2.2 - 2026-06-22

Fixes a `pg_hba.conf` dual-authorship bug that broke md5-password authentication on
agent-mode standbys (#199). Image moves to `trixie-5.5.0-26`.

### Fixed

- **Agent-mode standbys could end up with a SCRAM-only `pg_hba.conf`, breaking
  md5-password clients (#199).** The agent wrote a SCRAM-only `pg_hba.conf` every boot
  while the chart's postStart hook layered an md5-first fallback on top; the two raced,
  and on a rejoined standby the agent won, leaving it SCRAM-only. With legacy md5-stored
  passwords, every TCP password auth into the standby then failed (exporter `pg_up=0`,
  pgpool `-readonly` backend auth failure, and a failover lockout if such a standby were
  promoted). The agent is now the **single author** of `pg_hba.conf` in agent mode: it
  writes the md5-first compat form (an md5 line above each scram rule, on the pod CIDR
  only -- never the mTLS clientcert catch-all, never `0.0.0.0/0`) on every node, so
  primary and standby are byte-identical. The postStart md5-fallback + re-hash now run
  only in repmgrd mode.
- **md5->scram managed-user re-hash now runs on failover-promotion, not just at boot
  (#199).** The re-hash moved into the agent and runs on promotion/boot-primary, so a
  node promoted by an in-process failover (no container restart) converges its managed
  users (POSTGRES_USER, REPMGR_USER) to scram. Still gated by
  `postgresql.migrateLegacyMd5Users` (default true); the password never appears on argv.

### Changed

- repmgr image -> `trixie-5.5.0-26` (`repmgr.image.tag` and the bundled
  `etcd.bootstrapImage.tag` default). The agent-mode `pg_hba.conf` now includes the md5
  compat lines it previously received from the postStart hook (byte-identical across
  primary and standby).

## 1.2.1 - 2026-06-22

Chart-only bug fixes from a full-chart review. No image change (`trixie-5.5.0-25`);
no rendered change at defaults.

### Fixed

- **NetworkPolicy never matched the metrics exporter.** The exporter NetworkPolicy and
  the postgresql ingress allow-from rule selected `app.kubernetes.io/component:
  prometheus-exporter`, but the exporter pods are labeled `postgres-exporter`. Under a
  default-deny CNI the policy attached to zero pods, so the exporter got no egress (DNS,
  5432) and was not admitted to PostgreSQL — metrics silently broke whenever
  `networkPolicy.enabled` and `prometheusExporter.enabled` were both true. The policy is
  now named `<fullname>-postgres-exporter` and selects the correct label.
- **`helm install/upgrade` failed under NetworkPolicy when the monitoring user was
  enabled.** The postgresql NetworkPolicy ingress did not admit the `monitoring-user`
  post-install/upgrade hook Job, so its connection to 5432 was dropped on a default-deny
  CNI and the hook failed the release. Added a `monitoring-user` ingress allow-from rule
  (gated on `prometheusExporter.monitoringUser.enabled`).

- **Bundled etcd RBAC-bootstrap Job image tag pinned in lockstep with the repmgr image.**
  The `etcd.bootstrapImage.tag` override is set to `trixie-5.5.0-25` in values so the
  bundled etcd's bootstrap Job no longer lags the pg-ha-agent image (the subchart default
  could skew).

### Added

- **`values.schema.json` enum guards** for `prometheusExporter.sslmode`,
  `pgpool.tls.backendSslmode`, `pgbackrest.s3.uriStyle`, and
  `pgbackrest.repoEncryption.cipherType` — a typo in these now fails install-time
  validation instead of misconfiguring the exporter/pgpool/backup at pod runtime.

### Changed

- **Agent ServiceMonitor selector scoped to the postgresql component.** It now matches
  only the headless Service (which carries `app.kubernetes.io/component: postgresql` and
  the agent-metrics port) instead of every Service in the release.
- **kube-linter probe waivers on one-shot Jobs/CronJobs.** The backup, backup-validation,
  pgbackrest, and monitoring-user Jobs/CronJobs now carry
  `ignore-check.kube-linter.io/no-{liveness,readiness}-probe` annotations, matching the
  policy gate's documented convention for one-shot workloads.
- **Tighter chart package.** `.helmignore` now excludes `tests/`, `Makefile`, and
  `kind-config.yaml` (and pgvector gained a `.helmignore`), so chart development
  scaffolding no longer ships inside the released `.tgz`.
- `int`-coerced `postgresql.replicaCount` in the service-updater ConfigMap script, for
  consistency with the rest of the chart.
- Clarified several values.yaml/template comments: plain-English wording for the
  "off by default" TLS notes, the `monitoringHistoryDays` repmgrd-mode scope (agent mode
  writes no monitoring_history), and the exporter scrape-role note.

## 1.2.0 - 2026-06-21

Optional client-connection TLS for PostgreSQL, PGPool, and the metrics exporter (#110),
plus optional cascading replication (#29). Both off by default — no rendered change at
defaults. Image moves to `trixie-5.5.0-25`.

### Added

- **Optional cascading replication (#29, `repmgr.agent.cascadingReplication`, agent mode,
  default off).** A standby may stream from another standby — a chain by pod ordinal toward
  the primary — to offload the primary's WAL senders on larger clusters (`replicaCount >= 2`).
  The agent only follows a verifiably-safe same-timeline upstream and stays sticky on it,
  re-homing to the leader on any upstream failure/promotion, so a standby is never stranded
  and failover is not delayed. Off by default the render and follow behavior are byte-stable.
- **PostgreSQL server TLS (`postgresql.tls.enabled`).** Serves `ssl = on` from a BYO
  Secret (`postgresql.tls.existingSecret`, keys `tls.crt`/`tls.key`/`ca.crt`, mounted
  read-only at `/etc/postgresql/tls`, `defaultMode: 0400`) via a chart-managed
  `tls.conf` injected over the `conf.d` include. Works in both agent and repmgrd mode and
  in standalone (`replicaCount: 0`) installs.
- **Enforced TLS (`postgresql.tls.require`) and mutual TLS
  (`postgresql.tls.clientCertAuth`), agent mode only.** `require` makes the pod-CIDR
  client rule `hostssl` (rejects non-TLS clients); `clientCertAuth` additionally requires
  a client cert (`clientcert=verify-ca`) for app users. The chart's internal service users
  (the `repmgr` user, the superuser, and the monitoring user) are **exempted** from the
  client-cert requirement so the agent prober, repmgr, the exporter, and PGPool keep
  working. Loopback and the `host replication` rule are never converted — **replication
  stays plaintext on the pod network** (a documented non-goal; repmgr/agent replication
  conninfo carries no `sslmode`).
- **PGPool TLS (`pgpool.tls.*`).** Frontend `ssl = on` (`existingSecret`, optional
  `clientCertAuth`), backend TLS to PostgreSQL via `backendSslmode`
  (`disable|prefer|require|verify-ca|verify-full`), and `backendClientCert` so PGPool can
  present a client cert to the backends under PostgreSQL mTLS.
- **Exporter `sslmode` (`prometheusExporter.sslmode`).** `disable|require|verify-ca|
  verify-full` for the metrics exporter's connection; `verify-*` mounts the CA from the
  server-cert Secret and sets `sslrootcert`.
- **Fail-fast guards.** Each `tls.enabled` requires its `existingSecret`;
  `require`/`clientCertAuth` require `tls.enabled` and agent mode; `require` requires the
  exporter and PGPool backend `sslmode >= require`; mTLS requires a CA and (with PGPool) a
  PGPool backend client cert; `verify-*` requires `postgresql.tls.enabled`.

### Fixed

- The outer `volumes:`/`annotations:` gates on the StatefulSet now include
  `postgresql.tls.enabled`, so a TLS-only install (no `postgresql.configuration`/
  `pgbackrest`) renders the cert volume (not just its mount) and the config checksum that
  rolls the pod when the cert/config changes.

### Notes

- repmgrd mode supports only **optional server TLS** (`ssl = on`); `require`/`clientCertAuth`
  are agent-mode only (repmgrd's md5-fallback `pg_hba` line would bypass a `hostssl` rule)
  and the render fails fast if requested there.
- PostgreSQL reloads `ssl_*` on SIGHUP, not when the mounted Secret changes — run
  `kubectl rollout restart` after rotating the cert Secret.

## 1.1.8 - 2026-06-21

Quiets the etcd RBAC health-probe noise (#187). Bundles `etcd` 0.1.5; image moves to
`trixie-5.5.0-24`. Only affects the opt-in shared-etcd TLS+RBAC path; no rendered change
at defaults.

### Fixed

- **etcd RBAC health probe no longer spams `cannot find a user for permission check`
  (#187).** With the bundled etcd's `tls.clientCertAuth` + `rbac.enabled`, the
  liveness/readiness probe presents the server cert, whose CN maps to no registered etcd
  user, so etcd logged this ERROR on every probe interval. (The probe still passed and
  quorum was still verified — `etcdctl endpoint health` treats permission-denied as
  healthy — so this was log noise, not a broken health check.) The `rbac-bootstrap` Job
  now provisions a dedicated **read-only health user** (read on the single `health` key
  the probe ranges), and the etcd server cert's CN is set to it
  (`etcd.rbac.healthCheckCN`, default `etcd-healthcheck`), so the probe authenticates
  cleanly and the log clears. The server cert CN is otherwise unused (clients verify by
  SAN), so this costs no new Secret. **Action for existing shared-etcd TLS+RBAC users:**
  reissue the etcd server cert with `CN=etcd-healthcheck` (see the etcd chart README).

## 1.1.7 - 2026-06-20

Fixes an agent-mode rolling-restart deadlock (#186). Image moves to
`trixie-5.5.0-23`. No rendered change in repmgrd mode; agent mode gains a
replication-aware standby readiness probe (below).

### Fixed

- **A rolling restart of a 2-node agent-mode cluster could deadlock with no writable
  primary (#186).** A within-1.x image bump triggers a StatefulSet `RollingUpdate`;
  two gaps combined to strand the cluster (data was safe, but writes stopped until
  manual intervention). Both are now fixed:
  - **Replication-aware standby readiness.** A standby reported `Ready` on bare
    `pg_isready` — before its clone/rejoin was durably streaming. Since `RollingUpdate`
    advances to the next pod only once the current one is Ready, this let the
    controller roll the primary / clone-source while a standby was still cloning,
    interrupting the clone. Agent-mode readiness is now role-aware: a primary stays
    `pg_isready`, but a standby is Ready only once its walreceiver reports
    `status='streaming'` (`pg_stat_wal_receiver`). The rolling update now serializes
    safely with no timing dependence. repmgrd-mode readiness is unchanged (byte-stable).
  - **Release the lease when the node cannot serve.** The `#170` PVC-loss guard
    correctly refuses to `initdb` an empty node, but the agent kept holding the
    leadership lease in that `Wait` state, blocking a data-bearing peer from promoting.
    An empty-data lease holder whose durable primary-marker names a *different* node now
    releases the lease so that recorded primary acquires and serves (the step-down
    cooldown lets it win before the empty node re-contends). A node whose own data was
    lost (the marker names itself) still settles as before. Downgrades any residual
    interruption from a manual-intervention deadlock to a few-second self-heal.
  - Covered by a new live `test-agent-rolling` suite (wired into CI) that rolling-restarts
    a 2-node agent cluster and asserts a single writable primary + re-streaming standby,
    no manual intervention.

## 1.1.6 - 2026-06-20

Restores efficient stale-primary recovery (#178) and automatically cleans up the
ghost `repmgr.nodes` rows a scale-down used to leave behind (#139). Image moves to
`trixie-5.5.0-22`; bundles `etcd` 0.1.4 (bootstrap-image tag lockstep only). No
rendered change in agent mode (the default); repmgrd mode gains the service-updater
cleanup described below.

### Fixed

- **Stale-primary recovery now rewinds with `pg_rewind` instead of always falling
  back to a full re-clone (#178).** The container-restart guard ran `repmgr node
  rejoin --force-rewind` with the repmgr password inlined in the `-d` connection
  string, but repmgr opens a *separate* replication connection to the rejoin target
  for the rewind and the inline password did not carry into it — so the rewind
  failed with "unable to establish a replication connection to the rejoin target
  node" and every stale-primary rejoin did an O(database-size) base backup. The
  guard now passes the credential via `PGPASSWORD` (as the clone path already did),
  so a diverged ex-primary rewinds forward onto the surviving primary's timeline.
  Data safety is unchanged (the re-clone fallback remains for genuine rewind
  failures); this only restores the efficient path on large databases. The live
  failover suite now asserts the rewind path engages.
  - Working rewind exposed a latent agent-failover bug it had been masking: the
    agent read a standby's timeline from the control file (`pg_control_checkpoint`),
    which only advances at a restartpoint, so a node that had followed a newly
    promoted primary onto a higher timeline by streaming — but not yet checkpointed
    — reported the old timeline and was wrongly rejected by the `#125` highwater guard
    on a subsequent failover (it could acquire the lease but refused to promote, so
    leadership never moved). The full re-clone used to hide this by copying the
    primary's control file at the new timeline. The agent now reads a standby's
    timeline as `GREATEST(checkpoint timeline, pg_control_recovery.min_recovery_end_timeline)`
    — the recovery-end timeline advances as the standby replays the timeline switch and
    persists in the control file after the upstream dies (the failover moment), so a
    streaming-caught-up standby reports its true timeline and promotes; data
    completeness remains the separate LSN/most-advanced-replica check.
- **Scaling `postgresql.replicaCount` down no longer leaves permanent ghost rows in
  `repmgr.nodes` (#139).** The StatefulSet trims the highest ordinals, but their
  `repmgr.nodes` records used to remain `active=true` forever — repmgrd kept retrying
  their now-dead conninfo, `repmgr cluster show` reported a permanently-failed node,
  and every failover paid a connect-timeout per ghost. The primary now reconciles
  `repmgr.nodes` against the live ordinal range on each tick and unregisters records
  for pods the StatefulSet no longer runs (agent mode: the lease-holding primary;
  repmgrd mode: the master's service-updater, which now mounts `repmgr.conf`). The
  discriminator is purely the ordinal (`node_id - 1000 > replicaCount`), never
  reachability, so a momentarily-down live node is never unregistered, and a
  scaled-down *primary* row is left for an operator (`repmgr standby unregister`
  refuses primary rows) rather than dropped. Covered by a new live `test-scaledown`
  suite (wired into CI). The manual `repmgr standby unregister` step previously
  documented for the 1.1.0 README is no longer required.
- **The repmgr image's two shell scripts now share the timeline helpers (#177).**
  `entrypoint.sh` and `init-repmgr.sh` had each carried their own copy of the timeline
  decode and reads; they now source a single `repmgr-common.sh` (`tl_to_int` + the
  WAL-insert and offline control-file reads), so a fix to the hex decode can't land in
  only some copies. `init-repmgr.sh` keeps its intentionally **symmetric** control-file
  timeline comparison (both the local node and the primary are read the same way), which
  avoids needlessly re-cloning a standby that has followed a new timeline by streaming
  but not yet checkpointed it.

## 1.1.5 - 2026-06-19

A monitoring-exporter `/probe` fix (#185). Chart-only; no image change (stays
`trixie-5.5.0-21`). The exporter ConfigMap changes when
`prometheusExporter.monitoringUser` is enabled (the default).

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

## 1.1.4 - 2026-06-19

Bundled-etcd security (#184). Bundles `etcd` 0.1.3; image moves to
`trixie-5.5.0-21` (adds the `pg-ha-agent rbac-bootstrap` subcommand). No rendered
behavior change at defaults.

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
