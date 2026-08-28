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
# shellcheck source=scripts/lib.sh
source "${REPO_ROOT}/scripts/lib.sh"

require_tool helm "https://helm.sh/docs/intro/install/"
# The plugin, not a binary: `command -v` cannot see it, so ask helm.
if ! helm plugin list 2>/dev/null | awk '{print $1}' | grep -qx unittest; then
  echo "FATAL: the helm-unittest plugin is not installed, so this gate did NOT run." >&2
  echo "       install: helm plugin install https://github.com/helm-unittest/helm-unittest" >&2
  echo "       Treat this as a FAILED gate, not a passed one." >&2
  exit 127
fi

rc=0
ran=0
charts=0
failed=0
for chart_yaml in */Chart.yaml; do
  dir="$(dirname "$chart_yaml")"
  if compgen -G "${dir}/tests/unit/*_test.yaml" >/dev/null; then
    echo "==> helm unittest: ${dir}"
    ran=1
    charts=$((charts + 1))
    if ! helm unittest -f 'tests/unit/*_test.yaml' "${dir}"; then
      rc=1
      failed=$((failed + 1))
    fi
  fi
done

# No suites found is a FAILURE, not a quiet note (#298 review). This gate exists to run tests;
# "there were none" means the glob or the layout moved, and reporting success would mean the
# gate passes loudest exactly when it has stopped testing anything.
if [ "$ran" -eq 0 ]; then
  echo "FATAL: no tests/unit/*_test.yaml suites found -- this gate tested NOTHING." >&2
  rc=1
fi
verdict "helm-unittest" "$rc" "$( [ "$rc" -eq 0 ] && echo "${charts} charts" || echo "${failed} of ${charts} charts" )"
