#!/usr/bin/env bash
# Verify the default rendered output of pg and pgvector has not drifted versus a
# baseline git ref (default origin/master), so a change meant for one code path cannot
# silently move the default one. Since #286 there is a single failover path (the agent),
# so a bare render exercises it.
#
# Intended changes (a version bump, an image-tag bump, a deliberate default change)
# WILL show as a diff -- review them. This is a manual aid, not a hard CI gate.
# The randomly-generated Secret passwords are excluded (non-deterministic by design).
#
#   scripts/check-byte-stable.sh [ref]    # ref defaults to origin/master
#
# NOTE: across the 1.x -> 2.0.0 boundary this diffs heavily by design (repmgrd and its
# sidecars were removed). Compare against a 2.x ref for a meaningful result.
set -euo pipefail

ref="${1:-origin/master}"
root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
wt="${tmp}/base"
trap 'git -C "${root}" worktree remove --force "${wt}" 2>/dev/null || true; rm -rf "${tmp}"' EXIT

git -C "${root}" worktree add -q --detach "${wt}" "${ref}"

filt() { grep -vE '^[[:space:]]+(password|repmgr-password):'; }

# render writes chart's default manifest to out_file, or fails LOUDLY (#298 review).
#
# The previous form was `helm template ... 2>/dev/null | filt > file`, which under
# `set -euo pipefail` turned a render failure into a SILENT exit: helm's diagnostic went to
# /dev/null, filt (a grep) saw empty input and exited 1, pipefail propagated it, and set -e
# killed the script before it printed anything at all -- while the EXIT trap quietly removed
# the worktree. `make -C pg byte-stable REF=<ref>` then looked exactly like "no drift".
# Capture helm's status and its stderr explicitly, and only filter output we know we got.
render() {
  local chart_dir="$1" out_file="$2" raw
  if ! raw="$(helm template rel "${chart_dir}" 2>"${tmp}/render.err")"; then
    echo "FATAL: helm template failed for ${chart_dir}:" >&2
    cat "${tmp}/render.err" >&2
    return 1
  fi
  printf '%s\n' "${raw}" | filt > "${out_file}" || true
}

rc=0
for chart in pg pgvector; do
  render "${root}/${chart}" "${tmp}/${chart}-wt.yaml"
  render "${wt}/${chart}"   "${tmp}/${chart}-base.yaml"
  if diff -u "${tmp}/${chart}-base.yaml" "${tmp}/${chart}-wt.yaml" > "${tmp}/${chart}.diff"; then
    echo "OK: ${chart} default render unchanged vs ${ref}"
  else
    echo "DRIFT: ${chart} default render changed vs ${ref} (review -- intended bumps are expected):"
    cat "${tmp}/${chart}.diff"
    rc=1
  fi
done
exit "${rc}"
