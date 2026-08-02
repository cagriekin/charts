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

# Both sides must be non-empty: "two things differ" is a vacuous assertion when either is
# the empty string, which is exactly how a renamed template or a moved value turns a
# uniqueness check into a no-op (#279).
assert_not_eq() {
  local description="$1"
  local a="$2"
  local b="$3"
  if [[ -z "${a}" || -z "${b}" ]]; then
    fail "${description}" "both values must be non-empty (a='${a}' b='${b}')"
  elif [[ "${a}" != "${b}" ]]; then
    pass "${description}"
  else
    fail "${description}" "both values are '${a}'"
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

assert_gt() {
  local description="$1"
  local actual="$2"
  local threshold="$3"
  if [[ "${actual}" -gt "${threshold}" ]]; then
    pass "${description}"
  else
    fail "${description}" "expected > ${threshold}, got ${actual}"
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
      | grep -c "True" || true)

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

pg_exec() {
  local namespace="$1"
  local pod="$2"
  local query="$3"
  local user="${4:-testuser}"
  local db="${5:-testdb}"

  kubectl exec -n "${namespace}" "${pod}" -c postgresql -- \
    psql -U "${user}" -d "${db}" -t -A -c "${query}" 2>/dev/null
}

# Deploy the in-cluster MinIO the pgbackrest suites back their repository with: a
# self-signed TLS endpoint (Service :443 -> container :9000), the S3 credential Secret the
# values fixtures reference, and the bucket. pgbackrest's verify-tls is off in those
# fixtures, so the cert only has to exist. Idempotent -- safe to rerun in a live namespace.
# Usage: deploy_minio <namespace> [bucket]
deploy_minio() {
  local namespace="$1"
  local bucket="${2:-pgbackrest-test}"
  local certdir
  certdir="$(mktemp -d)"

  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=minio" \
    -addext "subjectAltName=DNS:minio" \
    -keyout "${certdir}/private.key" -out "${certdir}/public.crt" >/dev/null 2>&1
  kubectl create secret generic minio-tls -n "${namespace}" \
    --from-file=public.crt="${certdir}/public.crt" --from-file=private.key="${certdir}/private.key" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl create secret generic s3-backup-creds -n "${namespace}" \
    --from-literal=access-key-id=minioadmin --from-literal=secret-access-key=minioadmin \
    --dry-run=client -o yaml | kubectl apply -f -
  rm -rf "${certdir}"

  echo "Deploying MinIO (TLS on :9000, Service exposes :443 -> 9000)..."
  kubectl apply -n "${namespace}" -f - <<'MINIO'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: minio/minio:RELEASE.2025-02-18T16-25-55Z
          args: ["server", "/data", "--certs-dir", "/certs"]
          env:
            - { name: MINIO_ROOT_USER, value: minioadmin }
            - { name: MINIO_ROOT_PASSWORD, value: minioadmin }
          ports: [{ containerPort: 9000 }]
          volumeMounts:
            - { name: certs, mountPath: /certs, readOnly: true }
          readinessProbe:
            httpGet: { path: /minio/health/ready, port: 9000, scheme: HTTPS }
            initialDelaySeconds: 5
            periodSeconds: 5
      volumes:
        - name: certs
          secret: { secretName: minio-tls, defaultMode: 0444 }
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 443, targetPort: 9000 }]
MINIO
  wait_for_deployment_ready "${namespace}" "minio" 180

  echo "Creating bucket ${bucket}..."
  kubectl delete pod mc-setup -n "${namespace}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl run mc-setup -n "${namespace}" --restart=Never --image=minio/mc:RELEASE.2024-11-21T17-21-54Z \
    --command -- sh -c "mc --insecure alias set s3 https://minio:443 minioadmin minioadmin && mc --insecure mb s3/${bucket} || true"
  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/mc-setup -n "${namespace}" --timeout=120s
  kubectl delete pod mc-setup -n "${namespace}" --wait=false
}

resolve_fullname() {
  local release="$1"
  local chart_dir="$2"
  local values_file="${3:-}"
  local values_flag=""
  if [[ -n "${values_file}" ]]; then
    values_flag="-f ${values_file}"
  fi
  # awk must consume all input: an early `exit` closes the pipe while helm
  # is still writing, killing it with SIGPIPE (141) under pipefail
  helm template "${release}" "${chart_dir}" ${values_flag} 2>/dev/null \
    | awk '/^kind: StatefulSet/{found=1} found && !done && /^  name:/{print $2; done=1}'
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
