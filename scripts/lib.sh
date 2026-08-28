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
