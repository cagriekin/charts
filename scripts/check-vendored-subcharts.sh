#!/usr/bin/env bash
# Verify the vendored subchart archives (pg/charts/*.tgz, pgvector/charts/*.tgz)
# match their in-repo source charts. Catches a source edit that was never
# re-vendored with `helm dependency build`. Compares EXTRACTED contents, not raw
# .tgz bytes, because gzip embeds an mtime that changes on every repack.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${repo_root}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail=0

# Source subcharts vendored via file:// (name == source directory).
sources=(common etcd)
consumers=(pg pgvector redis)

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

    if diff -r "${fresh_extract}" "${vend_extract}" >/dev/null; then
      echo "OK: ${vendored} matches ${src}/"
    else
      echo "CONTENT DRIFT: ${vendored} is stale vs source ${src}/; run: helm dependency build ${consumer}"
      diff -r "${fresh_extract}" "${vend_extract}" || true
      fail=1
    fi
  done
done

exit "${fail}"
