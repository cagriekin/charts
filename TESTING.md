# Testing

The charts are tested in layers, fast to slow. The first three run on every PR (the
`lint` required check); the integration layer runs per-chart in KinD.

## 1. Static / render-time (no cluster, seconds)

| Tool | What it checks | Run locally |
| --- | --- | --- |
| `helm lint` | Chart templating + metadata | `helm lint <chart>` |
| `values.schema.json` | Fail-fast on bad input values at render time (enum/type guards on the typo-prone fields; `additionalProperties` stays open) | `helm template <chart>` (validates automatically) |
| **kubeconform** | Rendered manifests validate against real Kubernetes + CRD OpenAPI schemas (CRDs via the datreeio catalog) | `bash scripts/kubeconform-charts.sh` |
| **helm-unittest** | Declarative render unit tests — structural assertions on parsed-manifest paths, per case | `bash scripts/helm-unittest-charts.sh` |

## 2. Policy / security (on rendered output)

| Tool | What it checks | Run locally |
| --- | --- | --- |
| **kube-linter** | The mandatory documented Helm standards as policy-as-code: resource requests/limits, and liveness/readiness probes. Config: `.kube-linter.yaml` (only these checks; legitimate exceptions are waived per-object with an `ignore-check.kube-linter.io/<check>` annotation). | `bash scripts/kube-linter-charts.sh` |

Both gates render **every profile**, not just each chart's defaults (#298 review): the chart's
own `tests/values-*.yaml` fixtures are enumerated and rendered in turn, so pgpool, the metrics
exporter, pgBackRest's five containers, TLS, the etcd DCS, the restore workload and the hook
Jobs are all checked. Defaults-only was the wrong half to cover -- a violation in a default-on
object is caught by a dozen other things; one in an optional object ships. Widening it
immediately found two: the etcd `rbac-bootstrap` Job was missing the one-shot probe waiver every
sibling Job already had, and the idle `pgbackrest` sidecar had none either.

A profile that renders **nothing** fails both gates. kube-linter prints
`Warning: no valid objects found.` and exits zero, and kubeconform reports `0 resources found`
and exits zero, so without that guard a chart which stopped rendering would report a clean gate
having examined nothing. If a fixture is deliberately a *layer* over another (rendering it alone
trips a render-time validator), declare its base in `fixture_base()` in `scripts/lib.sh` rather
than letting it be skipped.

## 3. Integration (real cluster — KinD)

The `<chart>/tests/test-*.sh` suites (driven by each chart's `Makefile`) cover what the
static and policy layers cannot: failover, rolling restart, TLS, backup/restore, and the
**behavioral tests of rendered shell scripts** (e.g. the pgbackrest CronJob's primary
discovery). See `<chart>/Makefile` for targets.

Two suites are **not hermetic** against the local tree, and are the only ones that are not:
`test-migrate-native.sh` and `test-migrate-repmgrd.sh` install a RELEASED chart and a RELEASED
image from the network before upgrading to the local one, because an upgrade path can only be
tested from a real starting state. They cover the two migrations a 1.x consumer can be on:

| Suite | From | Recreate | What it proves |
|-------|------|----------|----------------|
| `test-migrate-native` | 1.x default (agent) | none — `Parallel` in both | in-place, no re-clone (#292) |
| `test-migrate-repmgrd` | 1.x `failoverMode: repmgrd` | `--cascade=orphan`, `OrderedReady` → `Parallel` | the orphaned pods are ADOPTED, not rebuilt (#298) |

If you add a third, give it the same treatment: pin the from-version and use an older published
image tag that CI does not build locally, or the freshly built image shadows it and the "old"
phase silently runs the new code, making the whole suite assert nothing.

## Known coverage gaps

Recorded here so a gap is a decision rather than a surprise:

- **Agent-mode scale-up of a live cluster** (`postgresql.replicaCount` N → N+1) is not
  exercised by any suite. The `upgrade` suite covered it only while it ran in repmgrd mode;
  agent mode hits the race in #297, which restores the coverage as part of its fix.
- ~~**The `failoverMode: repmgrd` → 2.0.0 upgrade**, the only path in 2.0.0 that recreates a
  live StatefulSet and the one every remaining repmgrd consumer must follow, had no coverage at
  all.~~ Closed by `test-migrate-repmgrd.sh` (#298 review). Worth keeping visible as the shape
  of gap that hides best: `test-migrate-native.sh` existed, was named for migration, and
  covered a different one — so the gap read as covered from the suite list alone.

## Per-chart test layout

```
<chart>/
  tests/
    unit/*_test.yaml     # helm-unittest: declarative render assertions (layer 1)
    test-*.sh            # bash: KinD integration + behavioral render tests (layer 3)
    values-*.yaml        # shared input fixtures (used by both unit and bash)
  values.schema.json     # input validation (layer 1)
```

## Why `test-template.sh` still exists alongside helm-unittest

helm-unittest asserts on **parsed YAML paths**. A meaningful subset of the bash render
tests cannot be expressed that way and remains in `test-template.sh`:

- **Behavioral tests of rendered shell** — extract a function/line from a rendered
  ConfigMap or `command:` and execute it (the pg HA logic: `lsn_gt`, `tl_to_int`,
  `handle_split_brain`, `read_marker`/`evaluate_lone_primary`, `urlencode`, pg_hba
  insertion order). No render-assertion tool can run these.
- **Occurrence counts** (`grep -c` across documents/containers), **line-ordering**
  within a script, and **cross-render comparisons** (e.g. a checksum annotation changing
  between two renders).

The helm-unittest suites cover the **structural** render assertions; the bash suites keep
the behavioral and cross-render coverage. Both share the `tests/values-*.yaml` fixtures.
</content>

## The mechanism axis: removed (#294)

There was one, from #288/#295, while the repmgr-to-native transition was in flight: a real
`mechanism: [repmgr, native]` matrix key with 11 exclusions, 62 legs, and a
`pg/tests/set-mechanism.sh` that retargeted the working tree. All of it is gone. `native` is the
only mechanism, so the matrix is **21 suites x 2 majors = 42 legs, no exclusions**, and the chart
emits `MECHANISM` unconditionally rather than only at a non-default value -- an image built before
#294 assumes repmgr when the variable is absent, so omitting it at the new default would run the
removed mechanism during the image-then-chart release this repo does.

The `pg-tests-passed` gate needed no change: for a matrix job `needs.suite.result` is success
only when every leg succeeded.

Two leftovers you may still meet:

- `chart_mechanism()` in `pg/tests/helpers.sh` reads the tree's `repmgr.agent.mechanism` and now
  always answers `native`, so the branches in `test-scaledown.sh`, `test-agent-rolling.sh`,
  `test-sync-replication-slots.sh`, `test-upgrade.sh` and `test-pgbackrest-restore-ha.sh` are
  inert. Each correctly takes its native side, but they are dead weight -- collapse them rather
  than re-gating them.
- A fixture pinning an older published `repmgr.image.tag` tests that published image instead of
  the one built from `images/pg-ha`. `values-agent.yaml` and `values-repmgr.yaml` were unpinned
  when this silently broke the native legs; eleven other fixtures still carry a pin.
