#!/bin/bash
# Live declarative databases/roles/grants test (#218). Installs pg with postgresql.roles +
# postgresql.databases; the post-install hook Job creates them on the primary. Verifies the
# grants are actually ENFORCED (allowed vs denied) and that a re-apply (helm upgrade, which
# re-runs the idempotent hook -- the same path a restore exercises) stays green.
# OPT-IN / standalone: `make -C pg test-databases-roles`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/helpers.sh"

NAMESPACE="${NAMESPACE:-pg-test-databases-roles}"
RELEASE="${RELEASE:-pgdr}"
VALUES="${SCRIPT_DIR}/values-databases-roles.yaml"
FULLNAME=$(resolve_fullname "${RELEASE}" "${CHART_DIR}" "${VALUES}")

begin_suite "Declarative databases/roles/grants (#218)"

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Installing pg with declarative roles/databases (post-install hook applies them)..."
# --wait blocks until the StatefulSet is ready AND the post-install hook Job completes, so
# once this returns the roles/databases/grants exist.
helm upgrade --install "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 6m
wait_for_pods_ready "${NAMESPACE}" "app.kubernetes.io/component=postgresql" 1 300
POD="${FULLNAME}-0"

# Superuser-created table in appdb.public; the hook's ALTER DEFAULT PRIVILEGES (run as the
# primary superuser) means tables that superuser creates are covered by the role grants.
echo "Creating a table in appdb as the superuser..."
pg_exec "${NAMESPACE}" "${POD}" "CREATE TABLE IF NOT EXISTS items (id serial PRIMARY KEY, v text)" "mainuser" "appdb"
pg_exec "${NAMESPACE}" "${POD}" "INSERT INTO items (v) VALUES ('seed')" "mainuser" "appdb"

# Fetch the chart-generated role passwords from the chart Secret.
APP_PW=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.app-acl-password}' | base64 -d)
ANALYST_PW=$(kubectl get secret "${FULLNAME}" -n "${NAMESPACE}" -o jsonpath='{.data.analyst-acl-password}' | base64 -d)

# Helper: run psql inside the primary as a given role over TCP (exercises pg_hba + the role
# password), capturing rc. Allowed ops exit 0; denied ops exit non-zero (permission denied).
run_as() { # role password db sql
  # Pass PGPASSWORD via env and exec psql directly (no nested bash -c re-quoting), so a
  # password containing shell metacharacters can't break the command.
  kubectl exec -n "${NAMESPACE}" "${POD}" -c postgresql -- \
    env PGPASSWORD="$2" psql -tA -h 127.0.0.1 -U "$1" -d "$3" -v ON_ERROR_STOP=1 -c "$4" 2>/dev/null
}

echo "Verifying 'app' (SELECT+INSERT granted)..."
app_select_rc=0; run_as app "${APP_PW}" appdb "SELECT count(*) FROM items" >/dev/null || app_select_rc=$?
assert_eq "#218: app can SELECT (granted)" "0" "${app_select_rc}"
app_insert_rc=0; run_as app "${APP_PW}" appdb "INSERT INTO items (v) VALUES ('by-app')" >/dev/null || app_insert_rc=$?
assert_eq "#218: app can INSERT (granted)" "0" "${app_insert_rc}"

echo "Verifying 'analyst' (SELECT only -> INSERT must be DENIED)..."
an_select_rc=0; run_as analyst "${ANALYST_PW}" appdb "SELECT count(*) FROM items" >/dev/null || an_select_rc=$?
assert_eq "#218: analyst can SELECT (granted)" "0" "${an_select_rc}"
an_insert_rc=0; run_as analyst "${ANALYST_PW}" appdb "INSERT INTO items (v) VALUES ('nope')" >/dev/null || an_insert_rc=$?
if [ "${an_insert_rc}" -ne 0 ]; then
  pass "#218: analyst INSERT is denied (least privilege)"
else
  fail "#218: analyst INSERT is denied (least privilege)" "INSERT succeeded but analyst was granted SELECT only"
fi

echo "Verifying database ownership + group membership..."
owner=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='appdb'" "mainuser" "maindb" | tr -d '[:space:]')
assert_eq "#218: appdb owned by app" "app" "${owner}"
member=$(pg_exec "${NAMESPACE}" "${POD}" "SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid JOIN pg_roles g ON g.oid=m.member WHERE r.rolname='readers' AND g.rolname='analyst'" "mainuser" "maindb" | tr -d '[:space:]')
assert_eq "#218: analyst is a member of readers" "1" "${member}"

# Idempotent re-apply (the restore-safety path): re-running the hook must not error and the
# grants must still hold.
echo "Re-applying via helm upgrade (re-runs the idempotent hook)..."
helm upgrade "${RELEASE}" "${CHART_DIR}" -n "${NAMESPACE}" -f "${VALUES}" --wait --timeout 6m
reapply_rc=0; run_as app "${APP_PW}" appdb "SELECT count(*) FROM items" >/dev/null || reapply_rc=$?
assert_eq "#218: grants still enforced after idempotent re-apply" "0" "${reapply_rc}"

end_suite
print_summary
