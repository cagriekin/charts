#!/bin/bash
# Retarget this WORKING TREE at one HA mechanism, so the whole live suite can run against
# `native` as well as `repmgr` (#288/#295) without a per-suite flag.
#
# Same idea as set-pg-major.sh, and the same reason: every suite resolves its values from
# pg/values.yaml plus its own pg/tests/values-*.yaml fixture, so rewriting the chart default
# here is what makes the CI matrix a single axis instead of an override threaded through 20+
# scripts. CI runs this in the checkout before invoking a suite; locally, run it, run the
# suites, then `git checkout -- pg/values.yaml` to restore.
#
# Usage: bash pg/tests/set-mechanism.sh <repmgr|native>
set -euo pipefail

MECH="${1:?usage: set-mechanism.sh <repmgr|native>}"
case "$MECH" in
  repmgr|native) ;;
  *) echo "unsupported mechanism ${MECH}: expected repmgr or native" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VALUES="${CHART_DIR}/values.yaml"

# No fixture may pin the mechanism itself. If one starts to, this overlay would silently not
# apply to that suite and the leg would test the wrong thing while reporting success -- the
# exact class of failure set-pg-major.sh's "rule matched nothing" check exists to prevent.
if grep -rln '^\s*mechanism:' "${SCRIPT_DIR}"/values-*.yaml 2>/dev/null; then
  echo "FATAL: the fixtures above set 'mechanism:' explicitly, so this overlay would not reach them." >&2
  echo "Either drop it from the fixture, or teach this script to rewrite fixtures too." >&2
  exit 1
fi

before="$(grep -c '^    mechanism: ' "${VALUES}" || true)"
if [ "${before}" != "1" ]; then
  echo "FATAL: expected exactly one 'mechanism:' line in ${VALUES}, found ${before}" >&2
  exit 1
fi
sed -i "s/^    mechanism: .*/    mechanism: ${MECH}/" "${VALUES}"

# Verify by RENDERING, not by trusting the sed. MECHANISM is emitted only when non-default, and
# it has to reach BOTH the postgresql container and repmgr-init -- the init container is the one
# that used to poll repmgr.nodes forever, so a leg where only the main container got the value
# would still crash-loop its standbys while looking correctly configured.
want=0
[ "${MECH}" = "native" ] && want=2
got="$(helm template ci "${CHART_DIR}" --set postgresql.majorVersion=18 \
        --show-only templates/statefulset.yaml 2>/dev/null | grep -c 'name: MECHANISM' || true)"
if [ "${got}" != "${want}" ]; then
  echo "FATAL: rendered ${got} MECHANISM env entries, want ${want} for mechanism=${MECH}" >&2
  exit 1
fi

echo "mechanism: ${MECH} (rendered ${got} MECHANISM env entries)"
echo "restore with: git checkout -- ${VALUES}"
