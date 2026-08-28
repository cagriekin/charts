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
# ...and a SECOND, newer target, because the minimum cannot validate kinds that did not exist
# yet (#298 review). Found live: `ValidatingAdmissionPolicy` and its Binding -- the admission
# control guarding the destructive restore Job, i.e. the single most security-relevant object
# the charts emit -- were being SKIPPED at 1.29 and the skip was invisible, because
# -ignore-missing-schemas treats an unknown core kind exactly like an uncataloged CRD. At 1.30
# every resource in that profile validates. Both passes run: the minimum keeps compatibility
# honest, the newer one is the only place the newest kinds get checked at all.
KUBE_VERSION_MAX="${KUBE_VERSION_MAX:-1.30.0}"

# Core/native kinds use kubeconform's bundled schemas (-schema-location default).
# CRD kinds (ServiceMonitor, PrometheusRule, KEDA ScaledObject, cert-manager
# Certificate, ...) are not built in; pull them from the community CRDs-catalog.
# A CRD with no catalog entry is skipped (-ignore-missing-schemas) rather than
# failing the gate, while every core kind and cataloged CRD is still validated.
CRD_CATALOG="https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

err_file="$(mktemp)"
trap 'rm -f "${err_file}"' EXIT

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
skips=0
for chart in "${charts[@]}"; do
  while read -r label vfiles; do
    args=()
    for v in ${vfiles}; do args+=(-f "${v}"); done
    echo "==> kubeconform: ${chart} [${label}]"
    profiles=$((profiles + 1))
    # helm's STDERR must not reach the manifest (#298 review, caught by this gate on itself).
    # An earlier form of this loop captured `2>&1`, and pgvector's render emits a
    # `level=INFO msg="found symbolic link..."` line per template file -- so those lines landed
    # inside the YAML and kubeconform reported `error unmarshalling resource` on every pgvector
    # profile. Keep stderr in its own file: needed for the diagnostic below, never for the input.
    if ! rendered="$(helm template "${chart}" "${chart}" "${args[@]}" 2>"${err_file}")"; then
      echo "FATAL: ${chart} [${label}] failed to render:" >&2
      cat "${err_file}" >&2
      echo "       If this fixture is a LAYER over another, declare its base in fixture_base()" >&2
      echo "       in scripts/lib.sh. A render failure is never a skip." >&2
      rc=1; failed=$((failed + 1))
      continue
    fi
    out=""
    # PER-PROFILE, not per invocation (#298 review). `failed` is compared against `profiles`
    # in the verdict line, and incrementing it inside this two-iteration k8s-version loop --
    # and again in the vacuous guard below -- made one broken profile report
    # "FAILED (2 of 44 profiles)", or 3 of 44 if it also rendered nothing. `failed` could
    # even exceed `profiles`. Latch it here and add ONE at the end of the profile.
    profile_failed=0
    for kv in "${KUBE_VERSION}" "${KUBE_VERSION_MAX}"; do
      if ! out=$(printf '%s' "${rendered}" \
          | kubeconform \
              -strict \
              -ignore-missing-schemas \
              -kubernetes-version "${kv}" \
              -schema-location default \
              -schema-location "${CRD_CATALOG}" \
              -summary 2>&1); then
        rc=1
        profile_failed=1
      fi
      printf 'k8s %s: %s\n' "${kv}" "${out}"
      # A SKIP is never silent again. -ignore-missing-schemas is there for genuinely uncataloged
      # CRDs, and it stays -- but it cannot double as a way for an unvalidated CORE kind to pass
      # unnoticed, which is exactly what happened to ValidatingAdmissionPolicy. Reported, not
      # failed: whether a CRD is in the community catalog is a network fact, not a chart defect.
      skipped=$(printf '%s' "${out}" | sed -nE 's/.*Skipped: ([0-9]+).*/\1/p' | head -1)
      if [ -n "${skipped}" ] && [ "${skipped}" -gt 0 ]; then
        echo "  NOTE: ${skipped} resource(s) NOT validated at k8s ${kv} (no schema):" >&2
        # `|| true` is load-bearing under `set -euo pipefail` (#298 review). This second
        # kubeconform run is a DIAGNOSTIC -- rc is already decided by the -strict run above --
        # but it exits non-zero whenever the profile also contains an INVALID resource, and
        # pipefail carries that past awk/sort straight into set -e. The gate then died here,
        # mid-loop: every remaining profile went unvalidated and `verdict` never printed, so
        # the run ended with NOTE lines and no FAILED line -- exactly the silent-pass
        # misreading the verdict line exists to prevent. Every profile with a skip and a
        # failure at once hits it, and pg [values-agent-control-restore] always has skips.
        printf '%s' "${rendered}" \
          | kubeconform -ignore-missing-schemas -kubernetes-version "${kv}" \
              -schema-location default -schema-location "${CRD_CATALOG}" -verbose 2>&1 \
          | awk '/skipped$/{print "        - " $3 " " $4}' | sort -u >&2 || true
        skips=$((skips + skipped))
      fi
    done
    # Vacuous-pass guard (#298 review): kubeconform validates what it is given, so an empty
    # render is "0 resources found ... Valid: 0" and exit ZERO. Require at least one resource,
    # so a chart or profile that renders nothing fails here instead of reporting a clean gate.
    found=$(printf '%s' "${out}" | sed -nE 's/^Summary: ([0-9]+) resources found.*/\1/p' | head -1)
    if [ -z "${found}" ] || [ "${found}" -eq 0 ]; then
      echo "FATAL: kubeconform validated NO resources from ${chart} [${label}] (summary: ${found:-absent})." >&2
      echo "       A gate that examines nothing must fail, not pass. If this fixture is a LAYER" >&2
      echo "       over another, declare its base in fixture_base() in scripts/lib.sh." >&2
      rc=1
      profile_failed=1
    fi
    [ "${profile_failed}" -eq 0 ] || failed=$((failed + 1))
  done < <(chart_profiles "${chart}")
done
verdict "kubeconform" "$rc" "$( [ "$rc" -eq 0 ] && echo "${#charts[@]} charts, ${profiles} profiles x k8s ${KUBE_VERSION}+${KUBE_VERSION_MAX}, ${skips} unvalidated" || echo "${failed} of ${profiles} profiles" )"
