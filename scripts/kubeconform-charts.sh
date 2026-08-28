#!/usr/bin/env bash
# Render every installable chart and validate the manifests against real Kubernetes
# + CRD OpenAPI schemas with kubeconform (#193). Catches manifests that string-grep
# render tests cannot: wrong apiVersion/kind, misplaced or misspelled fields, bad
# types. Runs in CI (lint.yaml) and locally. Requires: helm, kubeconform.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
# shellcheck source=scripts/lib.sh
source "${REPO_ROOT}/scripts/lib.sh"

require_tool helm "https://helm.sh/docs/intro/install/"
require_tool kubeconform "https://github.com/yannh/kubeconform/releases (CI pins v0.6.7)"

# Kubernetes API version whose schemas the manifests are validated against. Bump
# deliberately (kept in step with the charts' documented minimum).
KUBE_VERSION="${KUBE_VERSION:-1.29.0}"

# Core/native kinds use kubeconform's bundled schemas (-schema-location default).
# CRD kinds (ServiceMonitor, PrometheusRule, KEDA ScaledObject, cert-manager
# Certificate, ...) are not built in; pull them from the community CRDs-catalog.
# A CRD with no catalog entry is skipped (-ignore-missing-schemas) rather than
# failing the gate, while every core kind and cataloged CRD is still validated.
CRD_CATALOG="https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

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
    echo "==> kubeconform: ${chart} [${label}]"
    profiles=$((profiles + 1))
    out=""
    if ! out=$(helm template "${chart}" "${chart}" "${args[@]}" \
        | kubeconform \
            -strict \
            -ignore-missing-schemas \
            -kubernetes-version "${KUBE_VERSION}" \
            -schema-location default \
            -schema-location "${CRD_CATALOG}" \
            -summary 2>&1); then
      rc=1
      failed=$((failed + 1))
    fi
    printf '%s\n' "${out}"
    # Vacuous-pass guard (#298 review): kubeconform validates what it is given, so an empty
    # render is "0 resources found ... Valid: 0" and exit ZERO. Require at least one resource,
    # so a chart or profile that renders nothing fails here instead of reporting a clean gate.
    found=$(printf '%s' "${out}" | sed -nE 's/^Summary: ([0-9]+) resources found.*/\1/p' | head -1)
    if [ -z "${found}" ] || [ "${found}" -eq 0 ]; then
      echo "FATAL: kubeconform validated NO resources from ${chart} [${label}] (summary: ${found:-absent})." >&2
      echo "       A gate that examines nothing must fail, not pass. If this fixture is a LAYER" >&2
      echo "       over another, declare its base in fixture_base() in scripts/lib.sh." >&2
      rc=1
      failed=$((failed + 1))
    fi
  done < <(chart_profiles "${chart}")
done
verdict "kubeconform" "$rc" "$( [ "$rc" -eq 0 ] && echo "${#charts[@]} charts, ${profiles} profiles, k8s ${KUBE_VERSION}" || echo "${failed} of ${profiles} profiles" )"
