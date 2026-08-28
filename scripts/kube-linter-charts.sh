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
profiles=0
for chart in "${charts[@]}"; do
  while read -r label vfiles; do
    args=()
    for v in ${vfiles}; do args+=(-f "${v}"); done
    echo "==> kube-linter: ${chart} [${label}]"
    profiles=$((profiles + 1))
    # Rendered through helm and piped in, not `kube-linter lint <chart-dir>` (#298 review).
    # kube-linter has no values flag, so directory mode can only ever see the DEFAULT render --
    # which meant every optional container (pgpool, the exporter, pgBackRest's five, the restore
    # workload, the hook Jobs) went unchecked by the very gate that enforces "every container has
    # requests and limits, every long-running container has probes". Those are exactly the
    # containers most likely to be missing them, because they are the ones a reviewer sees least.
    # Per-object `ignore-check.kube-linter.io/*` waivers are annotations, so they travel in the
    # rendered manifest and keep working identically.
    out=""
    if ! out=$( { helm template "${chart}" "${chart}" "${args[@]}" \
        | kube-linter lint - --config "${CONFIG}"; } 2>&1 ); then
      rc=1
      failed=$((failed + 1))
    fi
    printf '%s\n' "${out}"
    # A VACUOUS PASS is the failure mode that matters here. kube-linter prints
    # "Warning: no valid objects found." and exits ZERO on empty input, so a profile that renders
    # nothing reports a clean policy gate while examining not one container. Found live: a
    # throwaway chart whose container had no resources and no probes passed this gate, while the
    # same manifest piped to `kube-linter lint -` produced all four violations.
    if printf '%s' "${out}" | grep -q "no valid objects found"; then
      echo "FATAL: kube-linter examined NO objects from ${chart} [${label}]." >&2
      echo "       A gate that examines nothing must fail, not pass. If this fixture is a LAYER" >&2
      echo "       over another, declare its base in fixture_base() in scripts/lib.sh." >&2
      rc=1
      failed=$((failed + 1))
    fi
  done < <(chart_profiles "${chart}")
done
verdict "kube-linter" "$rc" "$( [ "$rc" -eq 0 ] && echo "${#charts[@]} charts, ${profiles} profiles" || echo "${failed} of ${profiles} profiles" )"
