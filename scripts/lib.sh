#!/usr/bin/env bash
# Shared preflight + verdict helpers for the gate scripts in this directory.
#
# Both halves exist because of a real misdiagnosis (#298 review). The scripts already handled
# failure correctly -- each accumulates an rc and exits with it -- but two things made a failure
# easy to misread:
#
#   1. A MISSING TOOL produced one "kube-linter: command not found" per chart and exit 1. Not
#      silent, but at a glance indistinguishable from real policy violations, and it means the
#      chart loop runs to completion doing nothing useful first.
#   2. The exit status is the ONLY unambiguous signal, and it is the thing a caller most easily
#      discards: `bash scripts/kube-linter-charts.sh | tail -5; echo $?` reports tail's status,
#      not the gate's. That is exactly how these gates were once reported as passing when they
#      had not run at all.
#
# So: fail fast with an actionable message when a tool is absent, and always print a terminal
# verdict line that survives being piped through tail/grep.

# require_tool <command> <install-hint>
# Fails immediately (exit 127, the shell's own not-found status) rather than letting the work
# loop discover it once per chart.
require_tool() {
  local tool="$1" hint="${2:-}"
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "FATAL: ${tool} is not installed, so this gate did NOT run." >&2
    [ -n "${hint}" ] && echo "       install: ${hint}" >&2
    echo "       Treat this as a FAILED gate, not a passed one." >&2
    exit 127
  fi
}

# verdict <gate-name> <rc> [detail]
# Prints the single line worth grepping for and returns rc, so callers can `exit "$(...)"`-style
# chain or just let it be the script's last statement.
verdict() {
  local gate="$1" rc="$2" detail="${3:-}"
  if [ "${rc}" -eq 0 ]; then
    echo "=== ${gate}: OK${detail:+ (${detail})} ==="
  else
    echo "=== ${gate}: FAILED${detail:+ (${detail})} ===" >&2
  fi
  return "${rc}"
}

# chart_profiles <chart-dir>
# Emits one line per render profile: "<label> <values-file>..." (space separated; fixture paths
# contain no spaces). The first is always the chart's DEFAULT render with no values file.
#
# Why the gates need this at all (#298 review): kubeconform and kube-linter were both validating
# only each chart's default render -- 9 resources for pg. Every optional component was therefore
# unchecked by both gates: pgpool, the metrics exporter, pgBackRest's five containers, TLS,
# the etcd DCS, the restore workload, the hook Jobs. That is the wrong half to skip. A schema or
# policy violation in a default-on object gets caught by a hundred other things; one in an
# optional object ships, and the charts' own `tests/values-*.yaml` already describe exactly those
# configurations.
#
# pgvector and etcd ship no fixtures, so they get defaults only. For pgvector that is acceptable
# rather than a gap: its templates are byte-identical to pg's by invariant, so pg's profiles
# exercise the same template code. That invariant is enforced by scripts/check-template-parity.sh,
# which lint.yaml runs -- it was cited here as "a gate" for a while before it was actually wired
# up as one (#298 review), which is precisely the shape of hole this defaults-only shortcut opens
# if the diff is ever left unrun.
chart_profiles() {
  local chart="$1" f name base
  echo "defaults"
  for f in "${chart}"/tests/values-*.yaml; do
    [ -e "${f}" ] || continue
    name="$(basename "${f}" .yaml)"
    base="$(fixture_base "${chart}" "$(basename "${f}")")"
    echo "${name} ${base}${base:+ }${f}"
  done
}

# fixture_base <chart-dir> <fixture-basename>
# Some fixtures are deliberately a LAYER over another rather than a standalone configuration,
# and rendering them alone fails a render-time validator by design. They are declared here so a
# fixture that fails to render for any OTHER reason is a gate failure rather than a silent skip
# -- skipping quietly is the same vacuous-pass shape these gates were just fixed for.
fixture_base() {
  local chart="$1" fixture="$2"
  # The case labels are "<chart-dir>/<fixture-basename>", NOT a repo path -- the fixtures
  # themselves live under <chart-dir>/tests/. Pasting the real path here silently matches
  # nothing, and the resulting FATAL points right back at this function (#298 review).
  case "${chart}/${fixture}" in
    # #323's passthrough fixture sets pgbackrest.extraEnv/extraVolumes without enabling
    # pgbackrest; standalone it hits the "set but pgbackrest.enabled is false" validator.
    pg/values-pgbackrest-extra.yaml) echo "${chart}/tests/values-pgbackrest.yaml" ;;
    *) echo "" ;;
  esac
}
