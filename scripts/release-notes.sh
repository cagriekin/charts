#!/usr/bin/env bash
# Build a chart's release notes from the commits that actually touch that chart.
#
# Why this exists rather than `gh release create --generate-notes`: chart tags in this repo
# interleave (pg-1.5.0, redis-1.5.1, pg-1.9.0, ...), and --generate-notes diffs against the
# previous tag IN THE WHOLE REPO. That gets both halves wrong at once -- it picks the wrong
# range, and even with the right range it cannot filter by path, so a pg release listed six
# kafka PRs and compared itself against redis-1.5.1. Every release note published before this
# script was in some way about the wrong chart.
#
# So the range comes from the previous tag OF THE SAME CHART, and the entries are filtered to
# commits touching that chart's own files -- its directory plus what it vendors (common/, the
# etcd subchart) and, for pg/pgvector, the repmgr image whose tag they pin.
#
# Usage: scripts/release-notes.sh <chart> <tag>
#   e.g. scripts/release-notes.sh pg pg-1.10.0
# Writes the notes to stdout. Needs full history (fetch-depth: 0 in CI). `gh` is used only to
# resolve PR author logins; without it the notes are still correct, just unattributed.
set -euo pipefail

chart="${1:?usage: release-notes.sh <chart> <tag>}"
tag="${2:?usage: release-notes.sh <chart> <tag>}"
repo="${GITHUB_REPOSITORY:-cagriekin/charts}"

# Paths whose changes belong in this chart's notes. Kept in step with Chart.yaml dependencies
# and with images/repmgr, which pg/pgvector pin by tag rather than vendor.
paths=("${chart}/" "common/")
case "${chart}" in
  pg|pgvector) paths+=("etcd/" "images/repmgr/") ;;
esac

# The previous tag for THIS chart, by commit date. Tags are matched as <chart>-<semver> so
# that pg-* never matches pgvector-*.
prev=$(git for-each-ref --sort=creatordate --format='%(refname:short)' refs/tags \
  | grep -E "^${chart}-[0-9]+\.[0-9]+\.[0-9]+$" \
  | awk -v t="${tag}" '$0 == t {exit} {print}' \
  | tail -1)

# The tag may not exist yet: the workflow_dispatch path derives it from Chart.yaml and lets
# `gh release create` create it. Fall back to HEAD there. Without this the range would be
# unresolvable and `git log` would exit non-zero into an EMPTY notes body -- a silent wrong
# answer, which is the failure mode this whole script exists to remove.
endpoint="${tag}"
if ! git rev-parse -q --verify "${tag}^{commit}" >/dev/null; then
  echo "note: tag ${tag} does not exist yet; describing changes up to HEAD" >&2
  endpoint="HEAD"
fi

if [ -n "${prev}" ]; then
  range="${prev}..${endpoint}"
else
  range="${endpoint}"   # first release of this chart: everything up to the endpoint
fi

# One line per commit that touched any of this chart's paths. --no-merges because the history
# is squash-merged: the squash commit is the unit of change, and a merge commit would double
# up anything that did arrive as a true merge.
mapfile -t commits < <(git log --no-merges --format='%H' "${range}" -- "${paths[@]}")

bullets=""
for sha in "${commits[@]}"; do
  [ -n "${sha}" ] || continue
  subject=$(git log -1 --format='%s' "${sha}")
  # Squash-merge subjects end in (#<pr>); that number is the PR, not the issue the title cites.
  pr=$(sed -nE 's/.*\(#([0-9]+)\)[[:space:]]*$/\1/p' <<< "${subject}")
  entry="* ${subject}"
  if [ -n "${pr}" ]; then
    login=""
    if command -v gh >/dev/null 2>&1; then
      login=$(gh pr view "${pr}" --repo "${repo}" --json author --jq .author.login 2>/dev/null || true)
    fi
    [ -n "${login}" ] && entry="${entry} by @${login}"
    entry="${entry} in https://github.com/${repo}/pull/${pr}"
  fi
  bullets+="${entry}"$'\n'
done

# The hand-written CHANGELOG is the authority on what a version means, and it is already
# chart-scoped. Link it rather than reproducing it, so the notes cannot drift from it.
changelog_link=""
if [ -f "${chart}/CHANGELOG.md" ]; then
  changelog_link="See [${chart}/CHANGELOG.md](https://github.com/${repo}/blob/${tag}/${chart}/CHANGELOG.md) for the reasoning behind each change."
fi

echo "## What's Changed"
echo
if [ -n "${bullets}" ]; then
  printf '%s' "${bullets}"
else
  # Possible and not an error: a release cut to publish a re-packaged artifact, or a chart
  # version bumped by a change that lives entirely outside the paths above. Say so rather
  # than emitting an empty section that reads like a failure.
  echo "_No changes to \`${chart}\` since ${prev:-the start of the repository}; this release republishes the packaged chart._"
fi
if [ -n "${changelog_link}" ]; then
  echo
  echo "${changelog_link}"
fi
if [ -n "${prev}" ]; then
  echo
  echo "**Full Changelog**: https://github.com/${repo}/compare/${prev}...${tag}"
fi
