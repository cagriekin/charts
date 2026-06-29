#!/bin/bash
set -euo pipefail

# Performance baseline: install a single standalone instance sized like a small
# production cache and run redis-benchmark against it from inside the pod (localhost,
# no network hop). This is a regression guard, not a precise number — it asserts a
# conservative throughput floor so a broken/throttled config is caught, and prints the
# measured rates for the sizing guide. Absolute numbers vary with the runner's CPU.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-redis-test-performance}"
RELEASE="${RELEASE:-redis-perf}"
# Conservative floor (requests/sec). Even a single throttled CI core clears this by an
# order of magnitude; tripping it means the instance is fundamentally misconfigured.
MIN_RPS="${MIN_RPS:-1000}"
# Keep the run short and deterministic; we want a signal, not an exhaustive sweep.
REQUESTS="${REQUESTS:-50000}"
CLIENTS="${CLIENTS:-50}"

FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-performance.yaml")

begin_suite "Performance Baseline (standalone redis-benchmark)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  -n "${NAMESPACE}" \
  -f "${SCRIPT_DIR}/values-performance.yaml" \
  --wait --timeout 5m

wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=redis" 1 300

POD="${FULLNAME}-0"

ready=$(kubectl get pod -n "${NAMESPACE}" "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="redis")].ready}')
assert_eq "redis container is ready" "true" "${ready}"

# Parse "<rps> requests per second" from redis-benchmark -q output. The -q progress line
# is rewritten in place with carriage returns and only the final summary carries the
# "requests per second" phrase, so strip \r and match that phrase directly. We invoke one
# test per call (-t set OR -t get), so there is exactly one summary line to match.
# Returns the integer rps (truncated) on stdout, or empty on failure.
extract_rps() {
  local output="$1"
  tr '\r' '\n' <<< "${output}" \
    | grep -oE '[0-9]+(\.[0-9]+)? requests per second' \
    | grep -oE '^[0-9]+' \
    | head -n1
}

assert_throughput() {
  local label="$1"      # human description
  local test_name="$2"  # redis-benchmark -t value (SET / GET)
  local pipeline="$3"   # -P value
  local output rps

  echo "  Running redis-benchmark: -t ${test_name,,} -n ${REQUESTS} -c ${CLIENTS} -P ${pipeline}"
  if ! output=$(kubectl exec -n "${NAMESPACE}" "${POD}" -c redis -- \
      redis-benchmark -h 127.0.0.1 -p 6379 -q -n "${REQUESTS}" -c "${CLIENTS}" \
      -P "${pipeline}" -t "${test_name,,}" 2>/dev/null); then
    fail "${label}" "redis-benchmark exec failed"
    return
  fi
  echo "${output}" | sed 's/^/      /'

  # `|| true`: extract_rps's grep pipeline exits non-zero on no-match, which under
  # `set -euo pipefail` would abort the whole suite at this assignment — before the
  # graceful guard below. Neutralize it so an unparseable result becomes a clean FAIL.
  rps=$(extract_rps "${output}") || true
  if [[ -z "${rps}" ]]; then
    fail "${label}" "could not parse throughput from benchmark output"
    return
  fi

  if [[ "${rps}" -ge "${MIN_RPS}" ]]; then
    pass "${label} (${rps} req/s >= ${MIN_RPS} floor)"
  else
    fail "${label}" "throughput ${rps} req/s below floor ${MIN_RPS}"
  fi
}

# Non-pipelined: latency-bound, the worst case for raw request rate.
assert_throughput "SET throughput (no pipeline)" "SET" 1
assert_throughput "GET throughput (no pipeline)" "GET" 1
# Pipelined: shows the headroom batching unlocks on the same instance.
assert_throughput "SET throughput (pipeline 16)" "SET" 16
assert_throughput "GET throughput (pipeline 16)" "GET" 16

# Sanity: the instance is actually serving and observable post-benchmark.
result=$(redis_exec "${NAMESPACE}" "${POD}" "PING")
assert_eq "PING returns PONG after load" "PONG" "${result}"

# Surface the realized memory footprint — useful context for the sizing guide.
# `|| true`: this is informational only; a grep no-match must not abort the suite
# (pipefail + set -e) after every assertion has already passed.
used_memory=$(redis_exec "${NAMESPACE}" "${POD}" "INFO memory" | grep "^used_memory_human:" | cut -d: -f2 | tr -d '\r') || true
echo "  used_memory after run: ${used_memory:-unknown}"

# No teardown here: like the other suites, the release/namespace are left for the
# ephemeral cluster teardown (make cluster-delete / clean). Reruns are idempotent
# (create-namespace is apply-based and the install is `helm upgrade --install`).

end_suite
print_summary
