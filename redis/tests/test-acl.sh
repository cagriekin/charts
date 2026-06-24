#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

# Decode a key from the chart-generated Secret (keys may contain dashes).
get_secret() {
  kubectl get secret -n "$1" "$2" -o jsonpath="{.data['$3']}" | base64 -d
}

#######################################################################
# Part 1: standalone + a restricted `app` user (default operator)
#######################################################################
NS_SA="${NAMESPACE_SA:-redis-test-acl-sa}"
REL_SA="${RELEASE_SA:-redis-acl-sa}"
FULLNAME_SA=$(resolve_fullname "${REL_SA}" "${CHART_DIR}" "${SCRIPT_DIR}/values-acl.yaml")

begin_suite "ACL - standalone restricted app user"

kubectl create namespace "${NS_SA}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${REL_SA}" "${CHART_DIR}" \
  -n "${NS_SA}" -f "${SCRIPT_DIR}/values-acl.yaml" --wait --timeout 5m
wait_for_pods_ready "${NS_SA}" "app.kubernetes.io/component=redis" 1 300

POD_SA="${FULLNAME_SA}-0"
APP_PW=$(get_secret "${NS_SA}" "${FULLNAME_SA}" "acl-app-password")

# default user (full access) still works via the in-container password.
def_ping=$(kubectl exec -n "${NS_SA}" "${POD_SA}" -c redis -- \
  sh -c 'redis-cli -a "$REDIS_PASSWORD" PING' 2>/dev/null | tr -d '\r')
assert_eq "default user PING returns PONG" "PONG" "${def_ping}"

# app user: allowed on its own keyspace.
app_set=$(kubectl exec -n "${NS_SA}" "${POD_SA}" -c redis -- \
  redis-cli --user app -a "${APP_PW}" SET app:1 hello 2>/dev/null | tr -d '\r')
assert_eq "app user can SET app:1" "OK" "${app_set}"

app_get=$(kubectl exec -n "${NS_SA}" "${POD_SA}" -c redis -- \
  redis-cli --user app -a "${APP_PW}" GET app:1 2>/dev/null | tr -d '\r')
assert_eq "app user can GET app:1" "hello" "${app_get}"

# app user: denied outside its keyspace (~app:* only).
denied_key=$(kubectl exec -n "${NS_SA}" "${POD_SA}" -c redis -- \
  redis-cli --user app -a "${APP_PW}" SET other:1 x 2>&1 | tr -d '\r' || true)
assert_contains "app user denied keys outside ~app:*" "${denied_key}" "NOPERM"

# app user: denied admin commands (no @admin in its rules).
denied_cmd=$(kubectl exec -n "${NS_SA}" "${POD_SA}" -c redis -- \
  redis-cli --user app -a "${APP_PW}" CONFIG GET maxmemory 2>&1 | tr -d '\r' || true)
assert_contains "app user denied CONFIG (no @admin)" "${denied_cmd}" "NOPERM"

end_suite

#######################################################################
# Part 2: replication, locked-down `default` + custom operator `ops`
#######################################################################
NS_RE="${NAMESPACE_RE:-redis-test-acl-re}"
REL_RE="${RELEASE_RE:-redis-acl-re}"
FULLNAME_RE=$(resolve_fullname "${REL_RE}" "${CHART_DIR}" "${SCRIPT_DIR}/values-acl-replication.yaml")

begin_suite "ACL - replication locked default + operator"

kubectl create namespace "${NS_RE}" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "${REL_RE}" "${CHART_DIR}" \
  -n "${NS_RE}" -f "${SCRIPT_DIR}/values-acl-replication.yaml" --wait --timeout 5m
# All 3 pods becoming Ready already proves the operator-driven replication link
# (masteruser/masterauth) and Sentinel auth (auth-user/auth-pass) work end to end.
wait_for_pods_ready "${NS_RE}" "app.kubernetes.io/component=redis" 3 300

# Sentinel can move the master role to any pod, so don't assume -0 is the master --
# discover the pod currently reporting role:master. Authenticate as the operator (default is
# locked down) with an explicit -a, fetched from the Secret, rather than relying on the redis
# container's REDISCLI_AUTH env -- the test then stays correct regardless of that wiring.
# operatorUser=ops reuses the primary key (operatorPasswordSecret is unset).
OPS_PW=$(get_secret "${NS_RE}" "${FULLNAME_RE}" "redis-password")
acl_ops_cli() { kubectl exec -n "${NS_RE}" "$1" -c redis -- redis-cli --user ops -a "${OPS_PW}" --no-auth-warning "${@:2}"; }
acl_role() { acl_ops_cli "$1" INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^role:/{print $2}'; }
MASTER_POD=""
for i in 0 1 2; do
  if [ "$(acl_role "${FULLNAME_RE}-${i}")" = "master" ]; then MASTER_POD="${FULLNAME_RE}-${i}"; break; fi
done
if [ -z "${MASTER_POD}" ]; then
  fail "a pod reports role:master" "none of ${FULLNAME_RE}-{0,1,2} reported role:master"
  MASTER_POD="${FULLNAME_RE}-0"
fi

# Replication is live: the master reports at least one connected replica.
slaves=$(acl_ops_cli "${MASTER_POD}" INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^connected_slaves:/{print $2}')
if [[ "${slaves:-0}" -ge 1 ]]; then
  pass "operator-driven replication has >=1 connected replica"
else
  fail "operator-driven replication has >=1 connected replica" "connected_slaves='${slaves}'"
fi

# default user is locked down: it cannot write (rules ~app:* +@read only).
DEF_PW=$(get_secret "${NS_RE}" "${FULLNAME_RE}" "acl-default-password")
def_denied=$(kubectl exec -n "${NS_RE}" "${MASTER_POD}" -c redis -- \
  redis-cli --user default -a "${DEF_PW}" SET app:x 1 2>&1 | tr -d '\r' || true)
assert_contains "locked default user denied writes" "${def_denied}" "NOPERM"

# Exporter scrapes successfully as the operator user (redis_up 1).
up=$(kubectl exec -n "${NS_RE}" "${MASTER_POD}" -c redis-exporter -- \
  sh -c 'wget -qO- http://localhost:9121/metrics 2>/dev/null || curl -s http://localhost:9121/metrics' 2>/dev/null \
  | awk '/^redis_up /{print $2}' | tr -d '\r')
assert_eq "exporter scrapes redis as operator (redis_up=1)" "1" "${up}"

end_suite
print_summary
