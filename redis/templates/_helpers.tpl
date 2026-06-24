{{- define "redis.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "redis.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{- define "redis.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "redis.labels" -}}
{{ include "common.labels" . }}
{{- end }}

{{- define "redis.globalAnnotations" -}}
{{- with .Values.global.annotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "redis.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{- define "redis.headless" -}}
{{- printf "%s-headless" (include "redis.fullname" .) }}
{{- end }}

{{- define "redis.sentinelServiceName" -}}
{{- printf "%s-sentinel" (include "redis.fullname" .) }}
{{- end }}

{{- /* Stable DNS suffix for per-pod addresses Sentinel announces:
       <pod>.<fullname>-headless.<namespace>.svc.<clusterDomain> */ -}}
{{- define "redis.headlessDomain" -}}
{{- printf "%s.%s.svc.%s" (include "redis.headless" .) .Release.Namespace .Values.clusterDomain }}
{{- end }}

{{- define "redis.sentinelMasterName" -}}
{{- .Values.sentinel.masterName }}
{{- end }}

{{- /* Secret holding the redis password: a provided existingSecret, else the
       chart-generated Secret named after the release (see secret.yaml). */ -}}
{{- define "redis.secretName" -}}
{{- if .Values.redis.auth.existingSecret.name }}
{{- .Values.redis.auth.existingSecret.name }}
{{- else }}
{{- include "redis.fullname" . }}
{{- end }}
{{- end }}

{{- define "redis.sentinelSecretName" -}}
{{- if .Values.sentinel.auth.existingSecret.name }}
{{- .Values.sentinel.auth.existingSecret.name }}
{{- else }}
{{- include "redis.secretName" . }}
{{- end }}
{{- end }}

{{- define "redis.sentinelSecretKey" -}}
{{- if .Values.sentinel.auth.existingSecret.name }}
{{- .Values.sentinel.auth.existingSecret.key }}
{{- else }}
{{- .Values.redis.auth.existingSecret.key }}
{{- end }}
{{- end }}

{{- /* ACL operator user: the single privileged identity the chart itself authenticates as
       (replication masteruser, Sentinel auth-user, exporter, probes). "default" => the chart
       keeps using the primary password (requirepass) and emits no operator-specific wiring. */ -}}
{{- /* The privileged identity the chart authenticates as (replication masteruser/masterauth,
       Sentinel auth-user, exporter REDIS_USER, probes). Meaningful only when ACL is enabled;
       returns "default" otherwise so every `eq/ne operatorUser "default"` call site collapses
       to the no-ACL path and never wires OPERATOR_ or REDIS_USER env against undefined or a
       user that does not exist. A non-default operatorUser while ACL is off is rejected by
       redis.validate. */ -}}
{{- define "redis.operatorUser" -}}
{{- if .Values.redis.auth.acl.enabled }}
{{- .Values.redis.auth.acl.operatorUser | default "default" }}
{{- else }}
{{- "default" }}
{{- end }}
{{- end }}

{{- /* True (string) only when ACL is on AND a users[] entry redefines the `default` user, in
       which case requirepass is dropped in favor of an explicit `user default` line. */ -}}
{{- define "redis.aclDefinesDefault" -}}
{{- $found := false }}
{{- if .Values.redis.auth.acl.enabled }}
{{- range .Values.redis.auth.acl.users }}
{{- if eq .name "default" }}{{- $found = true }}{{- end }}
{{- end }}
{{- end }}
{{- $found }}
{{- end }}

{{- /* Secret name/key for the operator user's password: a provided operatorPasswordSecret,
       else reuse the redis auth Secret/key (one privileged password reusing the primary). */ -}}
{{- define "redis.operatorSecretName" -}}
{{- if .Values.redis.auth.acl.operatorPasswordSecret.name }}
{{- .Values.redis.auth.acl.operatorPasswordSecret.name }}
{{- else }}
{{- include "redis.secretName" . }}
{{- end }}
{{- end }}

{{- define "redis.operatorSecretKey" -}}
{{- if .Values.redis.auth.acl.operatorPasswordSecret.key }}
{{- .Values.redis.auth.acl.operatorPasswordSecret.key }}
{{- else }}
{{- .Values.redis.auth.existingSecret.key }}
{{- end }}
{{- end }}

{{- /* Per-user password Secret name/key. Takes (dict "ctx" $ "user" $u). Empty
       passwordSecret.name reuses the redis auth Secret; empty key defaults to
       acl-<name>-password (the key the chart generates in secret.yaml). */ -}}
{{- define "redis.aclUserSecretName" -}}
{{- if and .user.passwordSecret .user.passwordSecret.name }}
{{- .user.passwordSecret.name }}
{{- else }}
{{- include "redis.secretName" .ctx }}
{{- end }}
{{- end }}

{{- define "redis.aclUserSecretKey" -}}
{{- if and .user.passwordSecret .user.passwordSecret.key }}
{{- .user.passwordSecret.key }}
{{- else }}
{{- printf "acl-%s-password" .user.name }}
{{- end }}
{{- end }}

{{- /* Container env exposing the operator password (when operatorUser != default) and one
       ACL_PASS_<i> per ACL user, sourced from Secrets. Rendered into the bootstrap init
       container and the standalone redis container, which append the `user` lines. */ -}}
{{- define "redis.aclEnv" -}}
{{- if .Values.redis.auth.acl.enabled }}
{{- if ne (include "redis.operatorUser" .) "default" }}
- name: OPERATOR_USER
  value: {{ .Values.redis.auth.acl.operatorUser | quote }}
- name: OPERATOR_PWD
  valueFrom:
    secretKeyRef:
      name: {{ include "redis.operatorSecretName" . }}
      key: {{ include "redis.operatorSecretKey" . }}
{{- end }}
{{- range $i, $u := .Values.redis.auth.acl.users }}
- name: ACL_PASS_{{ $i }}
  valueFrom:
    secretKeyRef:
      name: {{ include "redis.aclUserSecretName" (dict "ctx" $ "user" $u) }}
      key: {{ include "redis.aclUserSecretKey" (dict "ctx" $ "user" $u) }}
{{- end }}
{{- end }}
{{- end }}

{{- /* `--user <operatorUser> ` (with a trailing space) for in-pod redis-cli when the chart
       authenticates as a non-default operator; empty otherwise. There is no REDISCLI_USER
       env, so the username must be passed on the command line. */ -}}
{{- define "redis.cliUserOpt" -}}
{{- if and .Values.redis.auth.acl.enabled (ne (include "redis.operatorUser" .) "default") }}
{{- printf "--user %s " .Values.redis.auth.acl.operatorUser }}
{{- end }}
{{- end }}

{{- /* Standalone startup wrapper (ACL only): standalone has no bootstrap init container, so
       it renders redis.conf into a writable copy, appends requirepass (unless `default` is
       redefined) + the operator and ACL `user` lines (passwords via env), then execs. */ -}}
{{- define "redis.standaloneStartScript" -}}
cp /etc/redis/redis.conf /config-rw/redis.conf
{
{{- if ne (include "redis.aclDefinesDefault" .) "true" }}
  echo "requirepass ${REDIS_PASSWORD}"
{{- end }}
{{- if ne (include "redis.operatorUser" .) "default" }}
  echo "user {{ .Values.redis.auth.acl.operatorUser }} on >${OPERATOR_PWD} ~* &* +@all"
{{- end }}
{{- range $i, $u := .Values.redis.auth.acl.users }}
  echo "user {{ $u.name }} on >${ACL_PASS_{{ $i }}} {{ $u.rules }}"
{{- end }}
} >> /config-rw/redis.conf
exec redis-server /config-rw/redis.conf
{{- end }}

{{- /* Total redis pods = replicas + 1 master. */ -}}
{{- define "redis.podCount" -}}
{{- add (int .Values.redis.replicaCount) 1 }}
{{- end }}

{{- /* Sentinel quorum: explicit override (incl. an out-of-range value, which the
       validate guard then rejects), else a strict majority of the pods. */ -}}
{{- define "redis.quorum" -}}
{{- if ne (toString .Values.sentinel.quorum) "" }}
{{- .Values.sentinel.quorum }}
{{- else }}
{{- add (div (int (include "redis.podCount" .)) 2) 1 }}
{{- end }}
{{- end }}

{{- /* Render a numeric value as a plain integer, never scientific notation. Helm parses
       an unquoted YAML number as float64, and toString of one >= 1e7 yields e.g. "3e+08",
       which redis-server rejects and the byte parser mangles. Strings pass through. */ -}}
{{- define "redis.intval" -}}
{{- if or (kindIs "float64" .) (kindIs "int" .) (kindIs "int64" .) }}{{- int64 . -}}{{- else }}{{- . -}}{{- end -}}
{{- end }}

{{- /* Parse a memory quantity to bytes. Accepts a bare number (bytes), redis units
       (k/kb/m/mb/g/gb, *b = binary, bare = decimal) and k8s units (Ki/Mi/Gi/Ti/Pi binary,
       K/M/G/T/P decimal) -- collapsed to one lowercased map. A numeric (unquoted) input is
       used directly to avoid float64 scientific-notation corruption. Fails fast on an
       unparseable number or an unsupported unit (no silent fall-through to bytes). */ -}}
{{- define "redis.bytes" -}}
{{- if or (kindIs "float64" .) (kindIs "int" .) (kindIs "int64" .) -}}
{{- int64 . -}}
{{- else -}}
{{- $s := trim (toString .) -}}
{{- $num := regexReplaceAll "[^0-9.]" $s "" -}}
{{- $unit := lower (trim (regexReplaceAll "[0-9.]" $s "")) -}}
{{- if not (regexMatch "^[0-9]+(\\.[0-9]+)?$" $num) -}}
{{- fail (printf "redis: cannot parse memory quantity %q" $s) -}}
{{- end -}}
{{- $mult := 0.0 -}}
{{- if eq $unit "" }}{{- $mult = 1.0 -}}
{{- else if eq $unit "k" }}{{- $mult = 1000.0 -}}
{{- else if or (eq $unit "ki") (eq $unit "kb") }}{{- $mult = 1024.0 -}}
{{- else if eq $unit "m" }}{{- $mult = 1000000.0 -}}
{{- else if or (eq $unit "mi") (eq $unit "mb") }}{{- $mult = 1048576.0 -}}
{{- else if eq $unit "g" }}{{- $mult = 1000000000.0 -}}
{{- else if or (eq $unit "gi") (eq $unit "gb") }}{{- $mult = 1073741824.0 -}}
{{- else if eq $unit "t" }}{{- $mult = 1000000000000.0 -}}
{{- else if or (eq $unit "ti") (eq $unit "tb") }}{{- $mult = 1099511627776.0 -}}
{{- else if eq $unit "p" }}{{- $mult = 1000000000000000.0 -}}
{{- else if or (eq $unit "pi") (eq $unit "pb") }}{{- $mult = 1125899906842624.0 -}}
{{- else }}{{- fail (printf "redis: unsupported memory unit %q in %q (use bytes or k/kb/ki, m/mb/mi, g/gb/gi, t/tb/ti, p/pb/pi)" $unit $s) -}}
{{- end -}}
{{- mulf (float64 $num) $mult | floor | int64 -}}
{{- end -}}
{{- end }}

{{- /* Fail-fast validation. Called at the top of statefulset.yaml. */ -}}
{{- define "redis.validate" -}}
{{- if not (has .Values.architecture (list "standalone" "replication")) }}
{{- fail "architecture must be 'standalone' or 'replication'" }}
{{- end }}
{{- if eq .Values.architecture "replication" }}
{{- if lt (int .Values.redis.replicaCount) 2 }}
{{- fail "architecture=replication requires redis.replicaCount >= 2 (>= 3 pods) so Sentinel keeps quorum after losing one pod; use architecture=standalone for a single instance" }}
{{- end }}
{{- $pods := int (include "redis.podCount" .) }}
{{- $quorum := int (include "redis.quorum" .) }}
{{- if or (lt $quorum 1) (gt $quorum $pods) }}
{{- fail (printf "sentinel.quorum (%d) must be between 1 and the total pod count (%d)" $quorum $pods) }}
{{- end }}
{{- if ge (int (index .Values.redis.config "min-replicas-to-write")) $pods }}
{{- fail "redis.config.min-replicas-to-write must be < total pods (redis.replicaCount + 1), else the master can never accept writes" }}
{{- end }}
{{- end }}
{{- if .Values.tls.enabled }}
{{- if eq .Values.architecture "standalone" }}
{{- fail "tls.enabled requires architecture=replication: TLS is wired into the replication StatefulSet (cert volume, --tls probes, per-pod-FQDN SAN model). Use architecture=replication, or terminate TLS at a proxy in front of standalone." }}
{{- end }}
{{- if not .Values.tls.existingSecret }}
{{- fail "tls.enabled requires tls.existingSecret (a Secret with keys tls.crt, tls.key, ca.crt)" }}
{{- end }}
{{- end }}
{{- if and .Values.tls.clientCertAuth (not .Values.tls.enabled) }}
{{- fail "tls.clientCertAuth requires tls.enabled" }}
{{- end }}
{{- $memLimit := dig "resources" "limits" "memory" "" .Values.redis }}
{{- $maxmemory := index .Values.redis.config "maxmemory" }}
{{- if and $memLimit $maxmemory }}
{{- $maxBytes := int64 (include "redis.bytes" $maxmemory) }}
{{- $limitBytes := int64 (include "redis.bytes" $memLimit) }}
{{- if gt $maxBytes $limitBytes }}
{{- fail (printf "redis.config.maxmemory (%s) exceeds redis.resources.limits.memory (%s); set maxmemory to ~80%% of the limit to leave headroom for AOF rewrite buffers and fragmentation" (include "redis.intval" $maxmemory) $memLimit) }}
{{- end }}
{{- end }}
{{- if .Values.redis.auth.acl.enabled }}
{{- if not .Values.redis.auth.enabled }}
{{- fail "redis.auth.acl.enabled requires redis.auth.enabled" }}
{{- end }}
{{- $op := include "redis.operatorUser" . }}
{{- $names := list }}
{{- range .Values.redis.auth.acl.users }}
{{- if not (regexMatch "^[A-Za-z0-9_.-]+$" .name) }}
{{- fail (printf "redis.auth.acl: user name %q must match ^[A-Za-z0-9_.-]+$ (no spaces or shell-significant characters)" .name) }}
{{- end }}
{{- if has .name $names }}
{{- fail (printf "redis.auth.acl: duplicate user name %q" .name) }}
{{- end }}
{{- $names = append $names .name }}
{{- $rules := lower (.rules | default "") }}
{{- if regexMatch "(^|[[:space:]])(on|off|nopass|reset|resetpass|resetkeys|resetchannels|clearselectors)([[:space:]]|$)" $rules }}
{{- fail (printf "redis.auth.acl: user %q rules must contain permission tokens only; the chart manages enablement (drop on/off/nopass/reset*)" .name) }}
{{- end }}
{{- if regexMatch "(^|[[:space:]])[>#]" $rules }}
{{- fail (printf "redis.auth.acl: user %q rules must not embed a password (>...) or hash (#...); set passwordSecret instead" .name) }}
{{- end }}
{{- end }}
{{- if and (ne $op "default") (has $op $names) }}
{{- fail (printf "redis.auth.acl: operatorUser %q must not also appear in users[]; the chart manages the operator user with full permissions (~* &* +@all)" $op) }}
{{- end }}
{{- if and (has "default" $names) (eq $op "default") }}
{{- fail "redis.auth.acl: redefining the `default` user requires a separate operatorUser; the chart authenticates as operatorUser for replication, failover, the exporter, and probes, so locking down `default` while it is the operator would break them" }}
{{- end }}
{{- /* BYO Secret: secret.yaml generates no ACL passwords when existingSecret.name is set,
       so every ACL identity must point at a key the user pre-populated, or the pods fail at
       startup on a missing secretKeyRef. Require explicit names rather than silently
       defaulting to acl-<name>-password keys that will not exist in the supplied Secret. */}}
{{- if .Values.redis.auth.existingSecret.name }}
{{- if and (ne $op "default") (not .Values.redis.auth.acl.operatorPasswordSecret.name) }}
{{- fail "redis.auth.acl: with redis.auth.existingSecret.name set, operatorPasswordSecret.name must also be set (the chart writes no operator password into a user-supplied Secret)" }}
{{- end }}
{{- range .Values.redis.auth.acl.users }}
{{- if not (and .passwordSecret .passwordSecret.name) }}
{{- fail (printf "redis.auth.acl: with redis.auth.existingSecret.name set, user %q must set passwordSecret.name (the chart writes no per-user password into a user-supplied Secret)" .name) }}
{{- end }}
{{- end }}
{{- end }}
{{- else }}
{{- /* ACL knobs set while disabled would feed undefined OPERATOR_ and ACL_PASS_ env into the
       bootstrap and exporter wiring. Fail fast. Checks the raw value, since redis.operatorUser
       reports "default" whenever ACL is off. */}}
{{- if ne (.Values.redis.auth.acl.operatorUser | default "default") "default" }}
{{- fail "redis.auth.acl.operatorUser is non-default but redis.auth.acl.enabled is false; enable ACL or leave operatorUser as 'default'" }}
{{- end }}
{{- if .Values.redis.auth.acl.users }}
{{- fail "redis.auth.acl.users is set but redis.auth.acl.enabled is false; set redis.auth.acl.enabled=true or remove the users" }}
{{- end }}
{{- end }}
{{- end }}

{{- /* Failover-safe bootstrap. Runs in the redis-bootstrap init container: discovers
       the current master from the Sentinels and renders the per-pod redis.conf +
       sentinel.conf into the writable /config-rw before redis/sentinel start. */ -}}
{{- define "redis.bootstrapScript" -}}
set -eu

POD_FQDN="${POD_NAME}.${HEADLESS_DOMAIN}"
SEED_MASTER_FQDN="${FULLNAME}-0.${HEADLESS_DOMAIN}"

TLS_OPTS=""
if [ "${TLS_ENABLED}" = "true" ]; then
  TLS_OPTS="--tls --cert /etc/redis/tls/tls.crt --key /etc/redis/tls/tls.key --cacert /etc/redis/tls/ca.crt"
fi

# Ask every reachable Sentinel (the Service, then each peer pod) for the master.
discover_master_once() {
  REDISCLI_AUTH="${SENTINEL_PASSWORD:-}"
  export REDISCLI_AUTH
  endpoints="${SENTINEL_SERVICE}"
  i=0
  while [ "$i" -lt "${NODE_COUNT}" ]; do
    endpoints="${endpoints} ${FULLNAME}-${i}.${HEADLESS_DOMAIN}"
    i=$((i + 1))
  done
  for ep in $endpoints; do
    # -t 2 bounds the connect: redis-cli defaults to no timeout, so an endpoint that
    # does not exist yet (cold boot) would otherwise block on the OS TCP timeout.
    addr=$(redis-cli -t 2 -h "$ep" -p "${SENTINEL_PORT}" $TLS_OPTS \
             sentinel get-master-addr-by-name "${MASTER_NAME}" 2>/dev/null | head -1 || true)
    if [ -n "$addr" ]; then
      printf '%s\n' "$addr"
    fi
  done
  unset REDISCLI_AUTH
}

# Settle over a few rounds and take the most-agreed answer (a just-restarted Sentinel
# may briefly report a stale master, so do not trust the first reply). Only a pod-0 with
# NO existing data is a fresh seed and probes briefly before seeding itself; every other
# case (any replica, or a pod-0 that already has data = a restart) waits longer for the
# master to reappear, so a transient discovery failure never resurrects a stale master.
MASTER=""
HAS_DATA=no
if [ -d /data/appendonlydir ] || [ -f /data/dump.rdb ] || [ -f /data/appendonly.aof ]; then
  HAS_DATA=yes
fi
if [ "${POD_NAME}" = "${FULLNAME}-0" ] && [ "$HAS_DATA" = "no" ]; then ATTEMPTS=3; else ATTEMPTS=30; fi
attempt=0
while [ "$attempt" -lt "$ATTEMPTS" ]; do
  answers=$(discover_master_once)
  if [ -n "$answers" ]; then
    MASTER=$(printf '%s\n' "$answers" | sort | uniq -c | sort -rn | head -1 | awk '{print $2}')
    break
  fi
  attempt=$((attempt + 1))
  sleep 2
done

# Render redis.conf.
cp /config-ro/redis.conf /config-rw/redis.conf
{
  echo "replica-announce-ip ${POD_FQDN}"
  echo "replica-announce-port ${REDIS_PORT}"
{{- if ne (include "redis.aclDefinesDefault" .) "true" }}
  if [ -n "${REDIS_PASSWORD:-}" ]; then
    echo "requirepass ${REDIS_PASSWORD}"
  fi
{{- end }}
{{- if eq (include "redis.operatorUser" .) "default" }}
  if [ -n "${REDIS_PASSWORD:-}" ]; then
    echo "masterauth ${REDIS_PASSWORD}"
  fi
{{- else }}
  echo "masteruser ${OPERATOR_USER}"
  echo "masterauth ${OPERATOR_PWD}"
{{- end }}
{{- if .Values.redis.auth.acl.enabled }}
{{- if ne (include "redis.operatorUser" .) "default" }}
  echo "user {{ .Values.redis.auth.acl.operatorUser }} on >${OPERATOR_PWD} ~* &* +@all"
{{- end }}
{{- range $i, $u := .Values.redis.auth.acl.users }}
  echo "user {{ $u.name }} on >${ACL_PASS_{{ $i }}} {{ $u.rules }}"
{{- end }}
{{- end }}
} >> /config-rw/redis.conf

if [ -n "$MASTER" ] && [ "$MASTER" != "$POD_FQDN" ]; then
  # A live master exists and it is not me: join as its replica.
  echo "replicaof ${MASTER} ${REDIS_PORT}" >> /config-rw/redis.conf
  MASTER_FQDN="$MASTER"
elif [ -z "$MASTER" ] && [ "$POD_NAME" != "${FULLNAME}-0" ]; then
  # True cold boot and I am not the seed: replicate pod-0.
  echo "replicaof ${SEED_MASTER_FQDN} ${REDIS_PORT}" >> /config-rw/redis.conf
  MASTER_FQDN="${SEED_MASTER_FQDN}"
else
  # I am the master (Sentinel reports me, or cold-boot seed pod-0).
  MASTER_FQDN="${POD_FQDN}"
fi

# Render sentinel.conf (per-master directives must follow the monitor line).
cp /config-ro/sentinel.conf /config-rw/sentinel.conf
{
  echo "sentinel announce-ip ${POD_FQDN}"
  echo "sentinel announce-port ${SENTINEL_PORT}"
  if [ -n "${SENTINEL_PASSWORD:-}" ]; then
    echo "requirepass ${SENTINEL_PASSWORD}"
  fi
  echo "sentinel monitor ${MASTER_NAME} ${MASTER_FQDN} ${REDIS_PORT} ${QUORUM}"
{{- if eq (include "redis.operatorUser" .) "default" }}
  if [ -n "${REDIS_PASSWORD:-}" ]; then
    echo "sentinel auth-pass ${MASTER_NAME} ${REDIS_PASSWORD}"
  fi
{{- else }}
  echo "sentinel auth-user ${MASTER_NAME} ${OPERATOR_USER}"
  echo "sentinel auth-pass ${MASTER_NAME} ${OPERATOR_PWD}"
{{- end }}
  echo "sentinel down-after-milliseconds ${MASTER_NAME} ${DOWN_AFTER_MS}"
  echo "sentinel failover-timeout ${MASTER_NAME} ${FAILOVER_TIMEOUT}"
  echo "sentinel parallel-syncs ${MASTER_NAME} ${PARALLEL_SYNCS}"
} >> /config-rw/sentinel.conf
{{- end }}

{{- define "redis.exporterPodSpec" -}}
securityContext:
  {{- toYaml .Values.exporter.podSecurityContext | nindent 2 }}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
containers:
  - name: redis-exporter
    image: "{{ .Values.exporter.image.repository }}:{{ .Values.exporter.image.tag }}"
    imagePullPolicy: {{ .Values.exporter.image.pullPolicy }}
    securityContext:
      {{- toYaml .Values.exporter.containerSecurityContext | nindent 6 }}
    env:
      - name: REDIS_ADDR
        value: "{{ if .Values.tls.enabled }}rediss{{ else }}redis{{ end }}://{{ include "redis.fullname" . }}:{{ .Values.service.port }}"
{{- if .Values.redis.auth.enabled }}
{{- if ne (include "redis.operatorUser" .) "default" }}
      - name: REDIS_USER
        value: {{ .Values.redis.auth.acl.operatorUser | quote }}
      - name: REDIS_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "redis.operatorSecretName" . }}
            key: {{ include "redis.operatorSecretKey" . }}
{{- else }}
      - name: REDIS_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "redis.secretName" . }}
            key: {{ .Values.redis.auth.existingSecret.key }}
{{- end }}
{{- end }}
{{- if .Values.tls.enabled }}
      - name: REDIS_EXPORTER_TLS_CLIENT_KEY_FILE
        value: /etc/redis/tls/tls.key
      - name: REDIS_EXPORTER_TLS_CLIENT_CERT_FILE
        value: /etc/redis/tls/tls.crt
      - name: REDIS_EXPORTER_TLS_CA_CERT_FILE
        value: /etc/redis/tls/ca.crt
{{- end }}
    ports:
      - name: metrics
        containerPort: 9121
        protocol: TCP
    livenessProbe:
      httpGet:
        path: /
        port: metrics
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      httpGet:
        path: /
        port: metrics
      initialDelaySeconds: 5
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    resources:
      {{- toYaml .Values.exporter.resources | nindent 6 }}
{{- if .Values.tls.enabled }}
    volumeMounts:
      - name: tls
        mountPath: /etc/redis/tls
        readOnly: true
volumes:
  - name: tls
    secret:
      secretName: {{ .Values.tls.existingSecret }}
{{- end }}
{{- end }}
