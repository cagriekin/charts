#!/usr/bin/env bash
# pg/templates and pgvector/templates must be byte-identical (#298 review).
#
# This was documented as a gate and was not one. `scripts/lib.sh`'s chart_profiles() gives
# pgvector `defaults` only, justified in its own comment by "its templates are byte-identical
# to pg's by invariant (`diff -r` is a gate)" -- and scripts/helm-unittest-charts.sh repeats the
# claim. Nothing ran the diff: it appeared only as a manual step in CLAUDE.md, and lint.yaml ran
# helm lint, vendored-subcharts, kubeconform, kube-linter and helm-unittest.
#
# The hole that leaves is quiet, which is why it needs a gate rather than a convention. pgvector
# has no KinD suites and no Makefile, a pgvector-only change does not trigger the pg suite
# matrix, and both profile-driven gates render pgvector at defaults only -- so ALL of pgvector's
# integration coverage is inherited from pg purely by virtue of the templates being identical.
# Add pg/templates/<new>.yaml and forget the pgvector symlink and the feature simply ships
# missing from pgvector, with every required check green.
#
# Today every pgvector template is a symlink into pg/templates, so a divergence cannot be
# introduced by EDITING one -- only by adding or deleting a file on one side. `diff -r` follows
# the symlinks and catches both.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if diff -r "${ROOT}/pg/templates" "${ROOT}/pgvector/templates"; then
  echo "=== template-parity: OK (pg/templates == pgvector/templates) ==="
  exit 0
fi

cat >&2 <<'EOF'

=== template-parity: FAILED ===
pg/templates and pgvector/templates diverged. pgvector is pg with a different image: it has no
KinD suites of its own, so its entire integration coverage rests on this invariant. Mirror the
change to both (pgvector/templates entries are symlinks into pg/templates -- add or remove the
symlink to match), then re-run this script.
EOF
exit 1
