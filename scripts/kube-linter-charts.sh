#!/usr/bin/env bash
# Policy-as-code gate (#193): lint every installable chart against the CLAUDE.md Helm
# standards encoded in .kube-linter.yaml (resource requests/limits + liveness/readiness
# probes). kube-linter renders the chart itself. Runs in CI (lint.yaml) and locally.
# Requires: kube-linter.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

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
for chart in "${charts[@]}"; do
  echo "==> kube-linter: ${chart}"
  if ! kube-linter lint "${chart}" --config "${CONFIG}"; then
    rc=1
  fi
done
exit "$rc"
