#!/bin/bash
set -euo pipefail

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
TEST_SUITE=""

begin_suite() {
  TEST_SUITE="$1"
  echo "=== SUITE: ${TEST_SUITE} ==="
}

end_suite() {
  echo "--- ${TEST_SUITE}: ${PASS_COUNT} passed, ${FAIL_COUNT} failed, ${SKIP_COUNT} skipped ---"
  echo ""
}

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  echo "  PASS: $1"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  echo "  FAIL: $1"
  if [[ -n "${2:-}" ]]; then
    echo "        $2"
  fi
}

skip() {
  SKIP_COUNT=$((SKIP_COUNT + 1))
  echo "  SKIP: $1"
}

assert_eq() {
  local description="$1"
  local expected="$2"
  local actual="$3"
  if [[ "${expected}" == "${actual}" ]]; then
    pass "${description}"
  else
    fail "${description}" "expected='${expected}' actual='${actual}'"
  fi
}

assert_contains() {
  local description="$1"
  local haystack="$2"
  local needle="$3"
  if grep -q "${needle}" <<< "${haystack}"; then
    pass "${description}"
  else
    fail "${description}" "output does not contain '${needle}'"
  fi
}

assert_not_contains() {
  local description="$1"
  local haystack="$2"
  local needle="$3"
  if grep -q "${needle}" <<< "${haystack}"; then
    fail "${description}" "output should not contain '${needle}'"
  else
    pass "${description}"
  fi
}

wait_for_pods_ready() {
  local namespace="$1"
  local label_selector="$2"
  local expected_count="$3"
  local timeout="${4:-300}"
  local interval=5
  local elapsed=0

  echo "  Waiting for ${expected_count} pod(s) with selector '${label_selector}' in ns '${namespace}'..."
  while [[ ${elapsed} -lt ${timeout} ]]; do
    local ready_count
    ready_count=$(kubectl get pods -n "${namespace}" -l "${label_selector}" \
      --field-selector=status.phase=Running \
      -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
      | grep -c "True" || echo "0")

    if [[ "${ready_count}" -ge "${expected_count}" ]]; then
      echo "  All ${expected_count} pod(s) ready (${elapsed}s elapsed)"
      return 0
    fi
    sleep ${interval}
    elapsed=$((elapsed + interval))
  done

  echo "  Timed out waiting for pods (${timeout}s)"
  kubectl get pods -n "${namespace}" -l "${label_selector}" -o wide 2>/dev/null || true
  return 1
}

wait_for_deployment_ready() {
  local namespace="$1"
  local deployment="$2"
  local timeout="${3:-300}"

  echo "  Waiting for deployment '${deployment}' in ns '${namespace}'..."
  if kubectl rollout status deployment/"${deployment}" -n "${namespace}" --timeout="${timeout}s" 2>/dev/null; then
    echo "  Deployment '${deployment}' ready"
    return 0
  fi

  echo "  Timed out waiting for deployment '${deployment}'"
  kubectl get deployment "${deployment}" -n "${namespace}" -o wide 2>/dev/null || true
  return 1
}

redis_exec() {
  local namespace="$1"
  local pod="$2"
  local cmd="$3"

  kubectl exec -n "${namespace}" "${pod}" -c redis -- redis-cli ${cmd} 2>/dev/null
}

resolve_fullname() {
  local release="$1"
  local chart_dir="$2"
  local values_file="${3:-}"
  local values_flag=""
  if [[ -n "${values_file}" ]]; then
    values_flag="-f ${values_file}"
  fi
  helm template "${release}" "${chart_dir}" ${values_flag} 2>/dev/null \
    | awk '/^kind: StatefulSet/{found=1} found && /^  name:/{print $2; exit}'
}

print_summary() {
  local total=$((PASS_COUNT + FAIL_COUNT + SKIP_COUNT))
  echo "========================================"
  echo "TOTAL: ${total} | PASS: ${PASS_COUNT} | FAIL: ${FAIL_COUNT} | SKIP: ${SKIP_COUNT}"
  echo "========================================"
  if [[ ${FAIL_COUNT} -gt 0 ]]; then
    return 1
  fi
  return 0
}
