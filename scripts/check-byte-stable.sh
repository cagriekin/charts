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

rc=0
for chart in pg pgvector; do
  helm template rel "${root}/${chart}" 2>/dev/null | filt > "${tmp}/${chart}-wt.yaml"
  helm template rel "${wt}/${chart}"  2>/dev/null | filt > "${tmp}/${chart}-base.yaml"
  if diff -u "${tmp}/${chart}-base.yaml" "${tmp}/${chart}-wt.yaml" > "${tmp}/${chart}.diff"; then
    echo "OK: ${chart} repmgrd default render unchanged vs ${ref}"
  else
    echo "DRIFT: ${chart} repmgrd default render changed vs ${ref} (review -- intended bumps are expected):"
    cat "${tmp}/${chart}.diff"
    rc=1
  fi
done
exit "${rc}"
