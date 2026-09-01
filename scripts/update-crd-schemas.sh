#!/usr/bin/env bash
# Refresh the vendored CRD JSON schemas that scripts/kubeconform-charts.sh validates against.
#
# They are vendored because the gate is a REQUIRED check and fetching them per-run made it fail
# for reasons unrelated to the charts: kubeconform treats any non-404 HTTP status from a schema
# location as a hard error rather than trying the next one, so a single bad CDN response (observed
# repeatedly as HTTP 400 on the cert-manager Certificate schema) turned the build red on one run
# and green on the next (#298).
#
# Run this when a chart starts emitting a new CRD kind, or to pick up an upstream schema change.
# The layout mirrors the catalog's own <group>/<kind>_<version>.json, which is also the shape of
# kubeconform's -schema-location template -- the local copies are the ONLY location that gate
# consults, there is no remote fallback behind them.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib.sh
source "${ROOT}/scripts/lib.sh"
# Guarded up front, because both absences otherwise MISREPORT themselves: a missing curl reports
# `HTTP 000` (reads as a network outage) and a missing python3 reports "response is not JSON"
# (reads as a bad upstream schema), since the stderr that would name the real cause is discarded.
require_tool curl "https://curl.se/download.html"
require_tool python3 "https://www.python.org/downloads/"
DEST="${ROOT}/scripts/crd-schemas"
CATALOG="https://raw.githubusercontent.com/datreeio/CRDs-catalog/main"

# Every CRD kind the charts render, in <group>/<kind>_<apiVersion> form. Do not derive this by
# hand: scripts/kubeconform-charts.sh checks every rendered profile against this directory and
# FAILS naming the exact <group>/<kind>_<version>.json any chart needs and this list is missing.
SCHEMAS=(
  "cert-manager.io/certificate_v1"
  "cert-manager.io/issuer_v1"
  "monitoring.coreos.com/prometheusrule_v1"
  "monitoring.coreos.com/servicemonitor_v1"
)

rc=0
for s in "${SCHEMAS[@]}"; do
  out="${DEST}/${s}.json"
  mkdir -p "$(dirname "${out}")"
  tmp="$(mktemp)"
  # Removed on any exit path, including Ctrl-C mid-download.
  trap 'rm -f "${tmp}"' EXIT
  code="$(curl -sS -o "${tmp}" -w '%{http_code}' "${CATALOG}/${s}.json" || echo 000)"
  if [ "${code}" != "200" ] || [ ! -s "${tmp}" ]; then
    echo "FAILED ${s}: HTTP ${code}" >&2
    rm -f "${tmp}"; rc=1; continue
  fi
  # Must parse as JSON, or a captured error page would be vendored as a "schema" that silently
  # validates nothing.
  if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "${tmp}" 2>/dev/null; then
    echo "FAILED ${s}: response is not JSON" >&2
    rm -f "${tmp}"; rc=1; continue
  fi
  mv "${tmp}" "${out}"
  echo "OK ${s} ($(wc -c < "${out}") bytes)"
done
exit "${rc}"
