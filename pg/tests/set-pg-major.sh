#!/bin/bash
# Retarget this WORKING TREE at one PostgreSQL major, so the whole live suite can run
# against PG17 as well as PG18 (#269) without a per-suite flag.
#
# Every suite resolves its images from pg/values.yaml plus its own pg/tests/values-*.yaml
# fixture, so rewriting those (rather than threading an override through 20+ scripts) is
# what makes the CI matrix a single axis. CI runs this in the checkout before invoking a
# suite; locally, run it, run the suites, then `git checkout -- pg/` to restore.
#
# It also aligns the fixtures' repmgr tag with pg/values.yaml, so the suites exercise the
# image CI just built from source instead of an older published one.
#
# Usage: bash pg/tests/set-pg-major.sh <major>     # 17 | 18
set -euo pipefail

MAJOR="${1:?usage: set-pg-major.sh <major>   (e.g. 17 or 18)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VALUES="${CHART_DIR}/values.yaml"
BASE_IMAGES="${SCRIPT_DIR}/ci-base-images.txt"

# The official postgres image for each supported major. Only used in standalone mode and
# by the TLS suite's psql client pod (in repmgr mode the server comes from the repmgr
# image), but it must be a tag CI pre-pulled -- an unlisted one would be fetched from
# Docker Hub mid-suite, or fail on a Kind node with no registry access.
case "$MAJOR" in
  18) PG_IMAGE_TAG="18.1-trixie" ;;
  17) PG_IMAGE_TAG="17.10-trixie" ;;
  *)  echo "unsupported major ${MAJOR}: add it to set-pg-major.sh (postgres image tag) and pg/tests/ci-base-images.txt first" >&2; exit 1 ;;
esac

# DEFAULT_MAJOR must match the Dockerfile's ARG PG_MAJOR default: that build is what the
# unsuffixed image tag holds, so only this major resolves without a -pgNN suffix.
DEFAULT_MAJOR="18"

# Base repmgr tag as the chart ships it (same resolution the CI build step uses, so the
# rewritten tag names an image that was actually built).
BASE_TAG=$(awk '/^repmgr:/{r=1} r&&/^    tag:/{gsub(/"/,"",$2); print $2; exit}' "$VALUES")
[ -n "$BASE_TAG" ] || { echo "could not resolve repmgr image tag from ${VALUES}" >&2; exit 1; }
BASE_TAG="${BASE_TAG%-pg[0-9]*}"   # tolerate a tree already switched to another major
if [ "$MAJOR" = "$DEFAULT_MAJOR" ]; then
  REPMGR_TAG="${BASE_TAG}"
else
  REPMGR_TAG="${BASE_TAG}-pg${MAJOR}"
fi

RENDER_SUITE="${SCRIPT_DIR}/test-template.sh"
render_suite_before=$(cksum "$RENDER_SUITE")

echo "=== set-pg-major: PostgreSQL ${MAJOR} ==="
echo "  repmgr image tag : ${REPMGR_TAG}"
echo "  postgres image   : postgres:${PG_IMAGE_TAG}"

grep -qxF "postgres:${PG_IMAGE_TAG}" "$BASE_IMAGES" || {
  echo "FATAL: postgres:${PG_IMAGE_TAG} is not in ${BASE_IMAGES}, so CI never pre-pulls it" >&2
  exit 1
}

# Files whose image references drive what the LIVE suites deploy.
#
# test-template.sh is deliberately excluded: it is a render-assertion suite that names
# both majors explicitly on purpose (PG18 defaults, and an explicit PG17 selection with
# a -pg17 tag). Rewriting it would make it assert whatever the tree happens to be
# switched to, silently turning "PG17 renders the -pg17 image" into a tautology.
mapfile -t FIXTURES < <(ls "${SCRIPT_DIR}"/values-*.yaml)
mapfile -t SUITES < <(ls "${SCRIPT_DIR}"/test-*.sh | grep -v '/test-template\.sh$')
TARGETS=("$VALUES" "${FIXTURES[@]}" "${SUITES[@]}")

# Apply one sed rule across TARGETS and fail when it matched nothing. Silent no-ops are
# the failure that matters here: a renamed key or retagged fixture would leave a "PG17"
# leg quietly running PG18 and reporting green, which is worse than no PG17 coverage.
# Comment lines are skipped: several suites cite an old image tag as the context for a
# regression they guard ("... under PG18 + cagriekin/repmgr:trixie-5.5.0-11"), and
# rewriting that history would make the comment claim something untrue.
apply() {
  local what="$1" pattern="$2" script="$3" hits=0 f n
  for f in "${TARGETS[@]}"; do
    [ -f "$f" ] || continue
    # Count only the lines sed will actually rewrite, so a pattern that survives just
    # inside comments still counts as "matched nothing".
    n=$(grep -vE '^[[:space:]]*#' "$f" | grep -cE "$pattern" || true)
    hits=$((hits + n))
    [ "$n" -gt 0 ] && sed -i -E "/^[[:space:]]*#/! ${script}" "$f"
  done
  if [ "$hits" -eq 0 ]; then
    echo "FATAL: rule '${what}' matched nothing (pattern: ${pattern}); set-pg-major.sh is stale" >&2
    exit 1
  fi
  echo "  ${what}: ${hits} occurrence(s)"
}

# postgresql.majorVersion and repmgr.image.majorVersion. The render guard
# (statefulset.yaml) fails unless the two agree, so they move together -- and only
# pg/values.yaml declares them; the fixtures inherit these defaults.
apply "majorVersion" \
  '^([[:space:]]+)majorVersion: "[0-9]+"' \
  's/^([[:space:]]+)majorVersion: "[0-9]+"/\1majorVersion: "'"${MAJOR}"'"/'

# repmgr image tags: repmgr.image.tag, etcd.bootstrapImage.tag, and the tags a few suites
# pin inline (test-tls's repmgrd release, test-migrate-agent's "from" image -- which must
# be the same major, or the migration would restart a PG17 PGDATA under a PG18 server).
apply "repmgr image tag" \
  'trixie-[0-9]+\.[0-9]+\.[0-9]+-[0-9]+(-pg[0-9]+)?' \
  's/trixie-[0-9]+\.[0-9]+\.[0-9]+-[0-9]+(-pg[0-9]+)?/'"${REPMGR_TAG}"'/g'

# postgres image tags (postgresql.image.tag + the TLS suite's client pod).
apply "postgres image tag" \
  '[0-9]+\.[0-9]+-trixie' \
  's/[0-9]+\.[0-9]+-trixie/'"${PG_IMAGE_TAG}"'/g'

# The render suite must come through untouched (see the TARGETS note). Checked rather
# than assumed because the damage is invisible: a glob that starts matching it would
# rewrite its explicit "-pg17" expectations to whatever the tree was switched to, and the
# assertions would still pass -- as tautologies.
if [ "$(cksum "$RENDER_SUITE")" != "$render_suite_before" ]; then
  echo "FATAL: $(basename "$RENDER_SUITE") was rewritten; it must stay out of TARGETS so its explicit per-major assertions keep meaning something" >&2
  exit 1
fi

# Prove the render followed, so a rule that matched the wrong thing cannot pass silently.
if command -v helm >/dev/null 2>&1; then
  rendered=$(helm template verify "$CHART_DIR" --show-only templates/statefulset.yaml)
  if ! grep -qF "cagriekin/repmgr:${REPMGR_TAG}" <<<"$rendered"; then
    echo "FATAL: rendered StatefulSet does not use cagriekin/repmgr:${REPMGR_TAG}" >&2
    exit 1
  fi
  ext=$(helm template verify "$CHART_DIR" --set postgresql.extensions.enabled=true \
    --show-only templates/statefulset.yaml)
  if ! grep -qF "/usr/lib/postgresql/${MAJOR}/lib" <<<"$ext"; then
    echo "FATAL: rendered extension paths are not /usr/lib/postgresql/${MAJOR}/lib" >&2
    exit 1
  fi
  echo "  verified: render uses cagriekin/repmgr:${REPMGR_TAG} and PG${MAJOR} extension paths"
else
  echo "  (helm not found: skipping the render verification)"
fi

echo "=== set-pg-major: tree now targets PostgreSQL ${MAJOR} ==="
if [ "$MAJOR" != "$DEFAULT_MAJOR" ]; then
  # test-template.sh asserts the shipped PG18 defaults (extension paths, the major-pin
  # guard), so it belongs on an unswitched tree -- CI runs it in the build job before the
  # matrix fans out. Say so rather than let it fail confusingly for a local run.
  echo "note: run 'make -C pg test-template' BEFORE switching; it asserts the PG${DEFAULT_MAJOR} defaults."
  echo "note: restore with: git checkout -- pg/values.yaml pg/tests"
fi
