#!/usr/bin/env bash
# Policy-as-code gate (#193): lint every installable chart against the documented Helm
# standards encoded in .kube-linter.yaml (resource requests/limits + liveness/readiness
# probes). kube-linter renders the chart itself. Runs in CI (lint.yaml) and locally.
# Requires: kube-linter.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
# shellcheck source=scripts/lib.sh
source "${REPO_ROOT}/scripts/lib.sh"

require_tool kube-linter "https://github.com/stackrox/kube-linter/releases (CI pins v0.7.1)"

CONFIG="${REPO_ROOT}/.kube-linter.yaml"

charts=()
for chart_yaml in */Chart.yaml; do
  dir="$(dirname "$chart_yaml")"
  # library charts (e.g. common) render nothing installable.
  if [ "$(awk '/^type:/{print $2}' "$chart_yaml")" = "library" ]; then
    continue
  fi
  charts+=("$dir")
done

if [ ${#charts[@]} -eq 0 ]; then
  echo "No installable charts found" >&2
  exit 1
fi

rc=0
failed=0
for chart in "${charts[@]}"; do
  echo "==> kube-linter: ${chart}"
  out=""
  if ! out=$(kube-linter lint "${chart}" --config "${CONFIG}" 2>&1); then
    rc=1
    failed=$((failed + 1))
  fi
  printf '%s\n' "${out}"
  # A VACUOUS PASS is the failure mode that matters here (#298 review). `kube-linter lint <dir>`
  # renders the chart itself, and when that produces nothing it prints
  # "Warning: no valid objects found." and exits ZERO -- so a chart that stops rendering for
  # kube-linter (a required value, a values.yaml rename, a template error it swallows) reports a
  # clean policy gate while examining not one container. Found live: a throwaway chart with a
  # container carrying no resources and no probes passed this gate, and the same manifest piped
  # to `kube-linter lint -` produced all four violations.
  if printf '%s' "${out}" | grep -q "no valid objects found"; then
    echo "FATAL: kube-linter rendered NO objects from ${chart} -- this chart was not linted." >&2
    echo "       A gate that examines nothing must fail, not pass." >&2
    rc=1
    failed=$((failed + 1))
  fi
done
verdict "kube-linter" "$rc" "$( [ "$rc" -eq 0 ] && echo "${#charts[@]} charts" || echo "${failed} of ${#charts[@]} charts" )"
