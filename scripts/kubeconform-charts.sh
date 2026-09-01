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
# VENDORED ONLY -- no remote schema location (#298). Fetching per-run made this gate, a REQUIRED
# check, fail for reasons that have nothing to do with the charts: kubeconform treats any non-404
# HTTP status from a location as a HARD ERROR rather than trying the next one, so a single bad CDN
# response reddened the build. Observed repeatedly as `error while downloading schema at
# .../certificate-cert-manager-v1.json - received HTTP status 400` on three kafka profiles, red on
# one run and green on the next with no code change, and reproducing identically on master.
#
# Keeping the remote catalog as a FALLBACK does not fix it: the locations are tried in order for
# every resource, so a kind that has to fall through -- ValidatingAdmissionPolicy at the older
# k8s version, which no catalog carries -- still reaches the broken location and still errors.
# Verified: with the catalog host unreachable the gate failed even though every CRD resolved
# locally.
#
# So the four schemas the charts render are checked in under scripts/crd-schemas, in the catalog's
# own <group>/<kind>_<version>.json layout. Refresh them with scripts/update-crd-schemas.sh. The
# coverage check below is what keeps that list honest: without a remote fallback, a chart that
# starts emitting a new CRD would otherwise be silently SKIPPED by -ignore-missing-schemas rather
# than validated, which is the "reads as coverage while providing none" failure this repo keeps
# finding in its own gates.
CRD_SCHEMAS="${REPO_ROOT}/scripts/crd-schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

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
# Every CRD kind the charts render must have a vendored schema, or it is silently skipped rather
# than validated (see the note above). Checked once, up front, against the same renders the loop
# below validates -- a missing entry names itself and the fix.
crd_missing=""
for _c in "${charts[@]}"; do
  while read -r _label _vfiles; do
    _args=()
    for _v in ${_vfiles}; do _args+=(-f "${_v}"); done
    helm template "${_c}" "${_c}" ${_args[@]+"${_args[@]}"} 2>/dev/null \
      | awk '/^apiVersion:/{av=$2} /^kind:/{if (av ~ /\//) print av" "$2}' \
      | sort -u \
      | while read -r _av _kind; do
          _group="${_av%%/*}"; _ver="${_av##*/}"
          case "${_group}" in
            apps|batch|policy|rbac.authorization.k8s.io|networking.k8s.io|admissionregistration.k8s.io|coordination.k8s.io|autoscaling|storage.k8s.io|apiextensions.k8s.io|scheduling.k8s.io|discovery.k8s.io|events.k8s.io|certificates.k8s.io|node.k8s.io|flowcontrol.apiserver.k8s.io) continue ;;
          esac
          _kl="$(printf '%s' "${_kind}" | tr '[:upper:]' '[:lower:]')"
          [ -f "${REPO_ROOT}/scripts/crd-schemas/${_group}/${_kl}_${_ver}.json" ] \
            || echo "  ${_group}/${_kl}_${_ver}.json (${_kind} in ${_c})"
        done
  done < <(chart_profiles "${_c}")
done > /tmp/kc-crd-missing.$$ 2>/dev/null
crd_missing="$(sort -u /tmp/kc-crd-missing.$$ 2>/dev/null || true)"; rm -f /tmp/kc-crd-missing.$$
if [ -n "${crd_missing}" ]; then
  cat >&2 <<EOF

=== kubeconform: FAILED (CRD schemas not vendored) ===
These CRD kinds are rendered by the charts but have no schema under scripts/crd-schemas, so
kubeconform would SKIP them instead of validating them:
${crd_missing}

Add them to the SCHEMAS list in scripts/update-crd-schemas.sh and run it.
EOF
  exit 1
fi

failed=0
profiles=0
skips=0
for chart in "${charts[@]}"; do
  while read -r label vfiles; do
    # Expanded below as ${args[@]+"${args[@]}"}, not "${args[@]}": bash only stopped
    # treating an empty array as UNSET in 4.4, and the `defaults` profile always has zero
    # entries -- so under `set -u` on macOS's system bash 3.2 this gate aborted on its very
    # first profile with `args[@]: unbound variable`, which reads as a chart failure rather
    # than a shell-version problem (#298 review). These scripts are documented as runnable
    # locally.
    args=()
    for v in ${vfiles}; do args+=(-f "${v}"); done
    echo "==> kubeconform: ${chart} [${label}]"
    profiles=$((profiles + 1))
    # helm's STDERR must not reach the manifest (#298 review, caught by this gate on itself).
    # An earlier form of this loop captured `2>&1`, and pgvector's render emits a
    # `level=INFO msg="found symbolic link..."` line per template file -- so those lines landed
    # inside the YAML and kubeconform reported `error unmarshalling resource` on every pgvector
    # profile. Keep stderr in its own file: needed for the diagnostic below, never for the input.
    if ! rendered="$(helm template "${chart}" "${chart}" ${args[@]+"${args[@]}"} 2>"${err_file}")"; then
      echo "FATAL: ${chart} [${label}] failed to render:" >&2
      cat "${err_file}" >&2
      echo "       If this fixture is a LAYER over another, declare its base in fixture_base()" >&2
      echo "       in scripts/lib.sh. A render failure is never a skip." >&2
      rc=1; failed=$((failed + 1))
      continue
    fi
    out=""
    # PER-PROFILE, not per invocation (#298): `failed` is compared against `profiles`, so
    # counting inside this two-version loop could report more failures than there are
    # profiles. Latch here, add ONE at the end of the profile.
    profile_failed=0
    # Same for the skip tally: both versions skip the SAME resources, so summing inside the
    # loop double-counts them. Take the worst single version and add it once.
    profile_skips=0
    for kv in "${KUBE_VERSION}" "${KUBE_VERSION_MAX}"; do
      if ! out=$(printf '%s' "${rendered}" \
          | kubeconform \
              -strict \
              -ignore-missing-schemas \
              -kubernetes-version "${kv}" \
              -schema-location default \
              -schema-location "${CRD_SCHEMAS}" \
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
        # `|| true` is load-bearing under `set -euo pipefail` (#298). This run is a
        # DIAGNOSTIC -- rc is already decided by the -strict run above -- but it exits
        # non-zero when the profile also holds an INVALID resource, and pipefail would carry
        # that into set -e: the gate died mid-loop, leaving the rest unvalidated and never
        # printing `verdict`. Any profile with a skip and a failure at once hits it.
        printf '%s' "${rendered}" \
          | kubeconform -ignore-missing-schemas -kubernetes-version "${kv}" \
              -schema-location default -schema-location "${CRD_SCHEMAS}" -verbose 2>&1 \
          | awk '/skipped$/{print "        - " $3 " " $4}' | sort -u >&2 || true
        if [ "${skipped}" -gt "${profile_skips}" ]; then profile_skips=${skipped}; fi
      fi
    done
    skips=$((skips + profile_skips))
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
