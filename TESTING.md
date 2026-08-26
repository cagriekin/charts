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

## 3. Integration (real cluster — KinD)

The `<chart>/tests/test-*.sh` suites (driven by each chart's `Makefile`) cover what the
static and policy layers cannot: failover, rolling restart, TLS, backup/restore, and the
**behavioral tests of rendered shell scripts** (e.g. the pg entrypoint's stale-primary guard
and the pgbackrest CronJob's primary discovery). See `<chart>/Makefile` for targets.

## Known coverage gaps

Recorded here so a gap is a decision rather than a surprise:

- **Agent-mode scale-up of a live cluster** (`postgresql.replicaCount` N → N+1) is not
  exercised by any suite. The `upgrade` suite covered it only while it ran in repmgrd mode;
  agent mode hits the race in #297, which restores the coverage as part of its fix.

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

## The mechanism axis (#288/#295)

The pg suites run against both HA mechanisms. `pg/tests/set-mechanism.sh <repmgr|native>`
retargets the working tree, exactly as `set-pg-major.sh` does for the PostgreSQL major, and
verifies by RENDERING that `MECHANISM` reached both the `postgresql` and `repmgr-init`
containers -- a leg where only the main container got it would still crash-loop its standbys
while looking correctly configured.

```bash
bash pg/tests/set-mechanism.sh native
make -C pg cluster-create
make -C pg test-agent test-scaledown test-config-failover
git checkout -- pg/values.yaml            # restore
```

Suites branch on `chart_mechanism()` (`pg/tests/helpers.sh`), reading the tree rather than an
env var a local run would forget to export.

**Tiering.** 21 suites x 2 majors x 2 mechanisms would be 84 legs; the matrix is **58**. A suite
never *loses* its current mechanism -- repmgr is still the chart default, so all 42 pre-existing
legs stay byte-identical and native is added only where the mechanism's own verbs are exercised.

| Both mechanisms (8) | Why |
|---|---|
| `agent` | install, failover, cold boot |
| `agent-etcd` | the lease now gates initdb ordering |
| `agent-rolling` | follow / rejoin |
| `pgpool-failover` | promote + Service routing |
| `config-failover` | that `conf.d` survives an agent-owned initdb |
| `scaledown` | residue: no orphaned slots, no repmgr metadata |
| `pgbackrest-restore-ha` | archive/restore on a fresh native install, plus standby re-clone |
| `databases-roles` | `replicaCount: 0` -- cheapest proof the agent-owned initdb built the app DB/role |

| Default (repmgr) only (13) | Why |
|---|---|
| `minimal` | `repmgr.enabled: false` -- no agent, so the axis is meaningless |
| `agent-control`, `agent-control-restore` | the control API is orthogonal to the mechanism |
| `full` | broad smoke; verbs covered by `agent` / `agent-rolling` |
| `upgrade` | upgrades from an older *published* image, repmgr by construction; the native analogue is #292's in-place migration |
| `agent-etcd-tls` | etcd TLS is orthogonal; `agent-etcd` covers the DCS axis |
| `tls` | PostgreSQL TLS is orthogonal to how replication is driven |
| `cascading-replication` | **render-rejected** with native (#289: slot ownership is primary-only) -- the one honest gap |
| `sync-replication-slots` | **render-rejected** with native (#308/#288: the reconcile reads `repmgr.nodes` and names slots `repmgr_slot_<node_id>`, neither of which exists there) |
| `backup-restore`, `backup-concurrent`, `pgbackrest-restore`, `pgbackrest-bootstrap` | pgBackRest mechanics are mechanism-independent |

The `mechanism` axis is a **real** matrix key, not an `include:` addition: `mechanism` would not
be an original key, so `{suite: agent, mechanism: native}` would add the key to the existing
agent combinations instead of creating new ones -- silently converting the repmgr legs rather
than adding to them. The `pg-tests-passed` gate needs no change: for a matrix job
`needs.suite.result` is success only when every leg succeeded.
