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

# `grep -q --`, not `grep -q` (#298 review). Without the terminator a needle that begins with
# a dash is parsed as an OPTION: grep exits 2 having matched nothing, so assert_contains reported
# a spurious failure and -- far worse -- assert_not_contains reported a spurious PASS. Any
# assertion of the form `- alert: X` (the natural way to test that a PrometheusRule rule is or is
# not rendered) was silently vacuous. Regex semantics are deliberately preserved: existing
# assertions rely on them, and `--` only stops option parsing.
assert_contains() {
  local description="$1"
  local haystack="$2"
  local needle="$3"
  if grep -q -- "${needle}" <<< "${haystack}"; then
    pass "${description}"
  else
    fail "${description}" "output does not contain '${needle}'"
  fi
}

# Fixed-string variant, for needles that are literal YAML or PromQL rather than patterns
# (#298 review). assert_contains is a REGEX match and stays that way -- existing assertions
# depend on it -- but a PromQL needle like `rate(x{...}[15m]) > 0` contains `[15m]`, which BRE
# reads as a character class matching one of `1`, `5`, `m`. It therefore does not match the very
# text it was copied from, and the assertion fails for a reason that has nothing to do with the
# render. Use this whenever the needle is text to be found verbatim.
assert_contains_literal() {
  local description="$1"
  local haystack="$2"
  local needle="$3"
  if grep -qF -- "${needle}" <<< "${haystack}"; then
    pass "${description}"
  else
    fail "${description}" "output does not contain (literal) '${needle}'"
  fi
}

assert_not_contains() {
  local description="$1"
  local haystack="$2"
  local needle="$3"
  if grep -q -- "${needle}" <<< "${haystack}"; then
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

  # bitnamilegacy, not minio/minio: MinIO withdrew its images from Docker Hub and quay.io
  # (#348, #353). The Bitnami image reads the same public.crt/private.key pair from /certs
  # and serves TLS when MINIO_SCHEME=https; it runs as uid 1001 with its data under
  # /bitnami/minio/data, so that path gets a writable emptyDir.
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
          image: bitnamilegacy/minio:2025.7.23-debian-12-r5
          env:
            - { name: MINIO_ROOT_USER, value: minioadmin }
            - { name: MINIO_ROOT_PASSWORD, value: minioadmin }
            - { name: MINIO_SCHEME, value: https }
          ports: [{ containerPort: 9000 }]
          volumeMounts:
            - { name: certs, mountPath: /certs, readOnly: true }
            - { name: data, mountPath: /bitnami/minio/data }
          readinessProbe:
            httpGet: { path: /minio/health/ready, port: 9000, scheme: HTTPS }
            initialDelaySeconds: 5
            periodSeconds: 5
      volumes:
        - name: certs
          secret: { secretName: minio-tls, defaultMode: 0444 }
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 443, targetPort: 9000 }]
MINIO
  wait_for_deployment_ready "${namespace}" "minio" 180

  # rclone, not mc (#353): the remote is defined from its environment, and bucket
  # creation needs the bucket check ON (the chart's Jobs turn it off; they never create).
  echo "Creating bucket ${bucket}..."
  kubectl delete pod s3-setup -n "${namespace}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl run s3-setup -n "${namespace}" --restart=Never --image=rclone/rclone:1.71.2 \
    --env=RCLONE_CONFIG=/dev/null --env=RCLONE_NO_CHECK_CERTIFICATE=true \
    --env=RCLONE_CONFIG_S3_TYPE=s3 --env=RCLONE_CONFIG_S3_PROVIDER=Other \
    --env=RCLONE_CONFIG_S3_ENDPOINT=https://minio:443 \
    --env=RCLONE_CONFIG_S3_ACCESS_KEY_ID=minioadmin --env=RCLONE_CONFIG_S3_SECRET_ACCESS_KEY=minioadmin \
    --command -- rclone mkdir "s3:${bucket}"
  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/s3-setup -n "${namespace}" --timeout=120s
  kubectl delete pod s3-setup -n "${namespace}" --wait=false
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


# discover_primary echoes the pod name that is currently NOT in recovery, or "" if none is
# (#288). Under `repmgr.agent.mechanism: native` the initial primary is decided by the LEASE
# RACE, not by ordinal: the lease holder is what runs initdb, and with podManagementPolicy
# Parallel any pod can win it. Under repmgr, init-repmgr.sh hardcoded ordinal 0 as master, so
# suites could assume pod-0 -- that assumption is a repmgr implementation detail and does not
# hold on the native path.
#
# Usage: discover_primary <namespace> <fullname> <replica-count> [user] [db]
discover_primary() {
  local ns="$1" fullname="$2" count="$3" user="${4:-testuser}" db="${5:-testdb}"
  local i rec
  for i in $(seq 0 $((count - 1))); do
    rec=$(pg_exec "${ns}" "${fullname}-${i}" "SELECT pg_is_in_recovery()" "${user}" "${db}" 2>/dev/null | tr -d '[:space:]')
    if [ "${rec}" = "f" ]; then echo "${fullname}-${i}"; return 0; fi
  done
  echo ""
}

# --- #350 probe lab ---
probe_lab_350() {
  local ns="$1" pod="$2"
  # --- #350 probe lab: the mechanism, not the rendered text ---------------------------------
  # Run the pod's ACTUAL startup and readiness commands (read back from the live pod spec) inside
  # a throwaway pod of the same image, first against a socket-only postmaster started exactly the
  # way the bootstrap starts its transient one (listen_addresses=''), then against one that
  # listens on loopback. The socket-only server must FAIL both probes -- while a bare pg_isready,
  # the pre-#350 shape, passes it, which is the regression this proves closed -- and the loopback
  # server must PASS both, the readiness one as a primary (pg_is_in_recovery = f). PGHOST points
  # the bare pg_isready at the lab's socket directory; `-h 127.0.0.1` overrides it, as in the pod.
  echo "Running the #350 probe lab..."
  lab_image=$(kubectl get pod -n "${ns}" "${pod}" -o jsonpath='{.spec.containers[?(@.name=="postgresql")].image}')
  lab_startup=$(kubectl get pod -n "${ns}" "${pod}" -o jsonpath='{.spec.containers[?(@.name=="postgresql")].startupProbe.exec.command[2]}')
  lab_readiness=$(kubectl get pod -n "${ns}" "${pod}" -o jsonpath='{.spec.containers[?(@.name=="postgresql")].readinessProbe.exec.command[2]}')
  assert_contains "#350 lab: the live startup command asks loopback" "${lab_startup}" "pg_isready -h 127.0.0.1"
  assert_contains "#350 lab: the live readiness command asks loopback first" "${lab_readiness}" "pg_isready -h 127.0.0.1"
  # The container starts as root and the script runs as the image's own `postgres` user via
  # runuser: initdb refuses root, and it also refuses a uid the image has no passwd entry for
  # (the chart's default 101 has none in the stock image, whose postgres is 999).
  lab_overrides=$(jq -cn --arg img "${lab_image}" --arg st "${lab_startup}" --arg rd "${lab_readiness}" '{
    spec: {
      restartPolicy: "Never",
      containers: [{
        name: "lab", image: $img, command: ["sleep", "900"],
        env: [{name: "POSTGRES_USER", value: "postgres"}, {name: "POSTGRES_DB", value: "postgres"},
              {name: "PGHOST", value: "/tmp/lab/run"}, {name: "STARTUP_CMD", value: $st}, {name: "READINESS_CMD", value: $rd}],
        resources: {requests: {cpu: "100m", memory: "128Mi"}, limits: {cpu: "500m", memory: "256Mi"}}
      }]
    }}')
  kubectl delete pod probe-lab-350 -n "${ns}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl run probe-lab-350 -n "${ns}" --image="${lab_image}" --restart=Never --overrides="${lab_overrides}" >/dev/null
  if ! kubectl wait --for=condition=Ready pod/probe-lab-350 -n "${ns}" --timeout=180s >/dev/null 2>&1; then
    echo "  probe lab pod did not become Ready (image=${lab_image}):"
    kubectl get pod -n "${ns}" probe-lab-350 -o wide 2>&1 | tail -1
    kubectl describe pod -n "${ns}" probe-lab-350 2>&1 | sed -n '/^Events:/,$p' | tail -8
  fi
  # The lab script is a plain heredoc (no nested quoting) so a developer can dry-run the
  # same text against an image with `docker run --entrypoint bash <img> -s`.
  local lab_script
  read -r -d '' lab_script <<'LAB' || true
set -u
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | head -1); export PATH="$PGBIN:$PATH"
export PGDATA=/tmp/lab/pgdata; mkdir -p /tmp/lab/run
initdb -D "$PGDATA" -U postgres --auth-local=trust --auth-host=trust >/dev/null 2>&1 || { echo "initdb=fail"; exit 0; }
probe() { bash -c "$2" >/dev/null 2>&1 && echo "$1=pass" || echo "$1=fail"; }
# exactly the bootstrap's transient shape: listen_addresses='' (no TCP), socket only
pg_ctl -D "$PGDATA" -w -o "-c listen_addresses='' -c unix_socket_directories=/tmp/lab/run" start >/dev/null 2>&1 || { echo "transient=fail"; exit 0; }
probe bare-pg_isready-vs-transient 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
probe startup-vs-transient "$STARTUP_CMD"
probe readiness-vs-transient "$READINESS_CMD"
pg_ctl -D "$PGDATA" -w -m fast stop >/dev/null 2>&1
pg_ctl -D "$PGDATA" -w -o "-c listen_addresses='127.0.0.1' -c unix_socket_directories=/tmp/lab/run" start >/dev/null 2>&1 || { echo "loopback=fail"; exit 0; }
probe startup-vs-loopback "$STARTUP_CMD"
probe readiness-vs-loopback "$READINESS_CMD"
pg_ctl -D "$PGDATA" -w -m fast stop >/dev/null 2>&1
LAB
  lab_out=$(kubectl exec -i -n "${ns}" probe-lab-350 -- runuser -u postgres -- bash -s <<< "${lab_script}" 2>&1)
  kubectl delete pod probe-lab-350 -n "${ns}" --wait=false >/dev/null 2>&1 || true
  lab_result() { printf '%s\n' "${lab_out}" | grep -E "^$1=" | head -1 | cut -d= -f2; }
  assert_eq "#350 lab: a bare pg_isready (the old probe shape) IS satisfied by a socket-only postmaster" "pass" "$(lab_result bare-pg_isready-vs-transient)"
  assert_eq "#350 lab: the startup probe is NOT satisfied by a socket-only postmaster" "fail" "$(lab_result startup-vs-transient)"
  assert_eq "#350 lab: the readiness probe is NOT satisfied by a socket-only postmaster" "fail" "$(lab_result readiness-vs-transient)"
  assert_eq "#350 lab: the startup probe passes once loopback listens" "pass" "$(lab_result startup-vs-loopback)"
  assert_eq "#350 lab: the readiness probe passes on a loopback-listening primary" "pass" "$(lab_result readiness-vs-loopback)"
}
