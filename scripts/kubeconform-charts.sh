#!/usr/bin/env bash
# Render every installable chart and validate the manifests against real Kubernetes
# + CRD OpenAPI schemas with kubeconform (#193). Catches manifests that string-grep
# render tests cannot: wrong apiVersion/kind, misplaced or misspelled fields, bad
# types. Runs in CI (lint.yaml) and locally. Requires: helm, kubeconform.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

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
for chart in "${charts[@]}"; do
  echo "==> kubeconform: ${chart}"
  if ! helm template "${chart}" "${chart}" \
      | kubeconform \
          -strict \
          -ignore-missing-schemas \
          -kubernetes-version "${KUBE_VERSION}" \
          -schema-location default \
          -schema-location "${CRD_CATALOG}" \
          -summary; then
    rc=1
  fi
done
exit "$rc"
