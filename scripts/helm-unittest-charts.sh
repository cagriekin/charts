#!/usr/bin/env bash
# Run helm-unittest suites for every chart that ships tests/unit/*_test.yaml (#193).
# These are declarative render unit tests asserting on parsed-manifest paths. They
# complement (do not yet fully replace) each chart's bash tests/test-template.sh, which
# still covers what helm-unittest cannot: behavioral tests of rendered shell scripts,
# occurrence counts, line-ordering, and cross-render comparisons. Requires the
# helm-unittest plugin. Runs in CI (lint.yaml) and locally.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

rc=0
ran=0
for chart_yaml in */Chart.yaml; do
  dir="$(dirname "$chart_yaml")"
  if compgen -G "${dir}/tests/unit/*_test.yaml" >/dev/null; then
    echo "==> helm unittest: ${dir}"
    ran=1
    helm unittest -f 'tests/unit/*_test.yaml' "${dir}" || rc=1
  fi
done

if [ "$ran" -eq 0 ]; then
  echo "No tests/unit/*_test.yaml suites found" >&2
fi
exit "$rc"
