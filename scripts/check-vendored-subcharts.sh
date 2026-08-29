#!/usr/bin/env bash
# Verify the vendored subchart archives (pg/, pgvector/, redis/, kafka/ charts/*.tgz)
# match their in-repo source charts. Catches a source edit that was never
# re-vendored with `helm dependency build`. Compares EXTRACTED contents, not raw
# .tgz bytes, because gzip embeds an mtime that changes on every repack.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${repo_root}"
# shellcheck source=scripts/lib.sh
source "${repo_root}/scripts/lib.sh"

require_tool helm "https://helm.sh/docs/intro/install/"
require_tool tar "your distribution's coreutils/tar package"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail=0
# Vacuous-pass guard (#298 review), the same one helm-unittest / kube-linter / kubeconform
# gained in this change and this script did not. BOTH skip paths below are silent: a missing
# source directory (`[ -d "${src}" ] || continue`) and a consumer with no matching archive
# (`[ ${#present[@]} -gt 0 ] || continue`). Rename or move common/ or etcd/, or drop a
# consumer's charts/ directory, and the pair simply vanishes from the check -- with every
# pair gone, `fail` stays 0 and the verdict prints OK having compared nothing.
checked=0

# Source subcharts vendored via file:// (name == source directory).
sources=(common etcd)
# EVERY chart that vendors one, kafka included (#298 review). kafka was missing here, and
# the version-drift arm cannot substitute for it: common/ was edited without a version bump,
# so kafka/charts/common-0.1.0.tgz kept the right NAME while its contents went stale (it had
# lost the annotations support and the #161 one-of minAvailable/maxUnavailable PDB fix), and
# this gate printed OK. A chart absent from this list is a chart whose vendored artifacts are
# not gated at all -- add new consumers here when they gain a charts/ directory.
consumers=(pg pgvector redis kafka)

for src in "${sources[@]}"; do
  [ -d "${src}" ] || continue
  ver="$(awk '/^version:/ {print $2; exit}' "${src}/Chart.yaml")"

  fresh_dir="${tmp}/fresh-${src}"
  mkdir -p "${fresh_dir}"
  helm package "${src}" -d "${fresh_dir}" >/dev/null
  fresh_extract="${tmp}/x-fresh-${src}"
  mkdir -p "${fresh_extract}"
  tar xzf "${fresh_dir}/${src}-${ver}.tgz" -C "${fresh_extract}"

  for consumer in "${consumers[@]}"; do
    shopt -s nullglob
    present=("${consumer}"/charts/"${src}"-*.tgz)
    shopt -u nullglob
    [ ${#present[@]} -gt 0 ] || continue

    vendored="${consumer}/charts/${src}-${ver}.tgz"
    if [ ! -f "${vendored}" ]; then
      echo "VERSION DRIFT: ${consumer} vendors ${present[*]} but ${src}/Chart.yaml is ${ver}; run: helm dependency build ${consumer}"
      fail=1
      continue
    fi

    vend_extract="${tmp}/x-vend-${consumer}-${src}"
    mkdir -p "${vend_extract}"
    tar xzf "${vendored}" -C "${vend_extract}"
    checked=$((checked + 1))

    if diff -r "${fresh_extract}" "${vend_extract}" >/dev/null; then
      echo "OK: ${vendored} matches ${src}/"
    else
      echo "CONTENT DRIFT: ${vendored} is stale vs source ${src}/; run: helm dependency build ${consumer}"
      diff -r "${fresh_extract}" "${vend_extract}" || true
      fail=1
    fi
  done
done

if [ "${checked}" -eq 0 ]; then
  echo "FATAL: compared NO vendored archives (sources: ${sources[*]}; consumers: ${consumers[*]})." >&2
  echo "       A gate that examines nothing must fail, not pass. Either a source chart" >&2
  echo "       directory was renamed/moved, or no consumer still vendors one -- fix the" >&2
  echo "       sources/consumers lists above." >&2
  fail=1
fi

verdict "vendored-subcharts" "${fail}" "${checked} archives"
