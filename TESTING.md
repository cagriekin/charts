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
| **kube-linter** | The mandatory CLAUDE.md Helm standards as policy-as-code: resource requests/limits, and liveness/readiness probes. Config: `.kube-linter.yaml` (only these checks; legitimate exceptions are waived per-object with an `ignore-check.kube-linter.io/<check>` annotation). | `bash scripts/kube-linter-charts.sh` |

## 3. Integration (real cluster — KinD)

The `<chart>/tests/test-*.sh` suites (driven by each chart's `Makefile`) cover what the
static and policy layers cannot: failover, rolling restart, TLS, backup/restore, and the
**behavioral tests of rendered shell scripts** (e.g. the pg service-updater's split-brain
selector, LSN/timeline comparators, marker guards). See `<chart>/Makefile` for targets.

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
