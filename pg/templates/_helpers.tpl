{{- define "pg.name" -}}
{{- include "common.name" . }}
{{- end }}

{{- define "pg.fullname" -}}
{{- include "common.fullname" . }}
{{- end }}

{{/*
Validate composed resource names against Kubernetes limits (#158). pg.fullname is
capped at 63, but per-resource suffixes are appended AFTER it, so a long
fullnameOverride can push a Service name past 63 (RFC1035 label) or a CronJob name
past ~52 (a CronJob must leave room for the generated -<timestamp> Job and -<hash>
Pod name suffixes). Truncating composed names is unsafe on a STATEFUL chart -- two
long names could collide on one StatefulSet/PVC -- so fail fast at render time with a
clear hint instead of a confusing API rejection at apply / first scheduled run.
*/}}
{{/*
Small default resources for the lightweight init containers (chown, cp, config-gen).
Init containers without requests/limits make every pod Forbidden in ResourceQuota-
enforced namespaces (#153). repmgr-init (the standby clone) is heavier and uses its own
values-overridable repmgr.initContainerResources instead.
*/}}
{{- define "pg.initResources" -}}
requests:
  cpu: 10m
  memory: 16Mi
limits:
  cpu: 100m
  memory: 64Mi
{{- end -}}

{{- define "pg.validateResourceNames" -}}
{{- $f := include "pg.fullname" . -}}
{{- /* Plain Services (and the base name): RFC1035 label, max 63. */ -}}
{{- $services := list $f (printf "%s-headless" $f) (printf "%s-readonly" $f) -}}
{{- range $n := $services -}}
{{- if gt (len $n) 63 -}}
{{- fail (printf "\n\nresource name %q is %d chars, but Kubernetes Service names are limited to 63 (RFC1035 label). Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- /* Deployment-backed names (pgpool, exporter) are ALSO Services, but the binding
limit is the Deployment's generated Pod name <name>-<rs-hash>-<suffix>: the ~16-char
ReplicaSet-hash + Pod suffix must fit under 63, so the name itself must be <= 47. */ -}}
{{- $deployments := list -}}
{{- if .Values.pgpool.enabled -}}{{- $deployments = append $deployments (printf "%s-pgpool" $f) -}}{{- end -}}
{{- if .Values.prometheusExporter.enabled -}}{{- $deployments = append $deployments (printf "%s-postgres-exporter" $f) -}}{{- end -}}
{{- range $n := $deployments -}}
{{- if gt (len $n) 47 -}}
{{- fail (printf "\n\nDeployment name %q is %d chars, but must be <= 47 to leave room for the generated ReplicaSet-hash + Pod name suffixes (a 63-char Pod name limit). Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- /* CronJobs: max 52, to leave room for the generated -<timestamp> Job suffix. */ -}}
{{- $cronjobs := list -}}
{{- if .Values.backup.enabled -}}{{- $cronjobs = append $cronjobs (printf "%s-backup" $f) -}}{{- end -}}
{{- if .Values.pgbackrest.enabled -}}{{- $cronjobs = concat $cronjobs (list (printf "%s-pgbackrest-full" $f) (printf "%s-pgbackrest-diff" $f)) -}}{{- end -}}
{{- range $n := $cronjobs -}}
{{- if gt (len $n) 52 -}}
{{- fail (printf "\n\nCronJob name %q is %d chars, but must be <= 52 to leave room for the generated Job/Pod name suffixes. Shorten the release name or fullnameOverride (pg.fullname is currently %q, %d chars)." $n (len $n) $f (len $f)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pg.chart" -}}
{{- include "common.chart" . }}
{{- end }}

{{- define "pg.labels" -}}
{{ include "common.labels" . }}
{{- end }}

{{- define "pg.selectorLabels" -}}
{{- include "common.selectorLabels" . }}
{{- end }}

{{/*
Global annotations applied to every resource's metadata. Returns the
YAML for .Values.global.annotations, or "" when unset. Call sites guard
with `{{- with (include "pg.annotations" .) }}` so a resource stays
annotation-free when no global annotations are configured. (Previously
these were wrongly merged into pg.labels and rendered as metadata.labels,
which both broke apply for non-label-safe values and hid them from
annotation consumers -- #128.)
*/}}
{{- define "pg.annotations" -}}
{{- with .Values.global.annotations }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{- /* repmgr image reference: repository:tag, with @digest appended when set so a
       digest pin (supply-chain) overrides the mutable tag. */ -}}
{{- define "pg.repmgrImage" -}}
{{- printf "%s:%s" .Values.repmgr.image.repository .Values.repmgr.image.tag -}}
{{- with .Values.repmgr.image.digest }}@{{ . }}{{- end -}}
{{- end -}}

{{- /* PG_MAJOR for every container that runs the repmgr image (#269). The image sets
       this ENV itself from its build arg; declaring it here makes the CHART's claim
       authoritative instead, so a values file that asks for a major the image does not
       bundle fails loudly -- the entrypoint's require_pg_bindir and the agent's boot
       check both resolve their bindir from it. Without this the image would only ever
       validate itself, and a chart/image disagreement (e.g. majorVersion 17 against the
       unsuffixed PG18 tag) would run the wrong major silently. Repmgr mode only: in
       standalone mode the server is the official postgres image, which ignores it. */ -}}
{{- define "pg.pgMajorEnv" -}}
{{- if .Values.repmgr.enabled -}}
- name: PG_MAJOR
  value: {{ required "repmgr.image.majorVersion is required" .Values.repmgr.image.majorVersion | quote }}
{{- end -}}
{{- end -}}

{{- /* Generic image reference (#26): pass an image dict {repository, tag, digest?};
       renders repository:tag, with @digest appended when set so a digest pin overrides
       the mutable tag. */ -}}
{{- define "pg.image" -}}
{{- printf "%s:%s" .repository .tag -}}
{{- with .digest }}@{{ . }}{{- end -}}
{{- end -}}

{{- define "pg.secretName" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- required "postgresql.existingSecret.name is required when postgresql.existingSecret.enabled is true" .Values.postgresql.existingSecret.name }}
{{- else }}
{{- include "pg.fullname" . }}
{{- end }}
{{- end }}

{{- define "pg.secretUsernameKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.usernameKey }}
{{- else }}
{{- "username" }}
{{- end }}
{{- end }}

{{- define "pg.secretPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.passwordKey }}
{{- else }}
{{- "password" }}
{{- end }}
{{- end }}

{{- define "pg.secretDatabaseKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.databaseKey }}
{{- else }}
{{- "database" }}
{{- end }}
{{- end }}

{{- define "pg.secretRepmgrPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- .Values.postgresql.existingSecret.repmgrPasswordKey }}
{{- else }}
{{- "repmgr-password" }}
{{- end }}
{{- end }}

{{- define "pg.secretMonitoringPasswordKey" -}}
{{- if .Values.postgresql.existingSecret.enabled }}
{{- required "postgresql.existingSecret.monitoringPasswordKey is required when prometheusExporter.monitoringUser.enabled and postgresql.existingSecret.enabled" .Values.postgresql.existingSecret.monitoringPasswordKey }}
{{- else }}
{{- "monitoring-password" }}
{{- end }}
{{- end }}

{{- /* Name of the Secret holding the shared S3 access/secret key for pgBackRest
       (pgbackrest.s3.keyType=shared). Single source of truth for the `required`
       guard so the four secretKeyRef sites can't drift apart. */ -}}
{{- define "pg.pgbackrestS3SecretName" -}}
{{- required "pgbackrest.existingSecret.name is required (pgbackrest.s3.keyType=shared); use keyType=auto for cloud workload identity" .Values.pgbackrest.existingSecret.name -}}
{{- end }}

{{- /* Declarative databases/roles (#218): chart-managed LOGIN roles whose password the
       chart generates+persists (passwordSecret.name empty). These get a generated key in
       the chart Secret and a secretKeyRef env in the hook Job. */ -}}
{{- define "pg.aclManagedRoles" -}}
{{- $out := list -}}
{{- range $r := .Values.postgresql.roles -}}
{{- if and (ne $r.login false) (not (($r.passwordSecret).name)) -}}
{{- $out = append $out $r.name -}}
{{- end -}}
{{- end -}}
{{- $out | join "," -}}
{{- end }}

{{- /* Secret key holding a chart-generated ACL role password. */ -}}
{{- define "pg.aclRoleSecretKey" -}}
{{- printf "%s-acl-password" . -}}
{{- end }}

{{- /* Build a single GRANT statement for one role grant (#218). Input dict: "g" (grant),
       "role" (role name). Identifiers are validated elsewhere to ^[A-Za-z_][A-Za-z0-9_]*$
       so double-quoting is injection-safe; privileges are allowlist-validated keywords. */ -}}
{{- define "pg.aclGrantSql" -}}
{{- $g := .g -}}{{- $role := .role -}}
{{- $privs := $g.privileges | join ", " | upper -}}
{{- $schema := $g.schema | default "" -}}
{{- $objects := $g.objects | default "" -}}
{{- if and $schema (eq $objects "ALL_TABLES") -}}
GRANT {{ $privs }} ON ALL TABLES IN SCHEMA "{{ $schema }}" TO "{{ $role }}"; ALTER DEFAULT PRIVILEGES IN SCHEMA "{{ $schema }}" GRANT {{ $privs }} ON TABLES TO "{{ $role }}"
{{- else if and $schema (eq $objects "ALL_SEQUENCES") -}}
GRANT {{ $privs }} ON ALL SEQUENCES IN SCHEMA "{{ $schema }}" TO "{{ $role }}"; ALTER DEFAULT PRIVILEGES IN SCHEMA "{{ $schema }}" GRANT {{ $privs }} ON SEQUENCES TO "{{ $role }}"
{{- else if $schema -}}
GRANT {{ $privs }} ON SCHEMA "{{ $schema }}" TO "{{ $role }}"
{{- else -}}
GRANT {{ $privs }} ON DATABASE "{{ $g.database }}" TO "{{ $role }}"
{{- end -}}
{{- end }}

{{- /* Validate postgresql.roles[] / postgresql.databases[] (#218): identifier safety,
       uniqueness, reserved names, owner resolution, and a GRANT-privilege allowlist so a
       value can never smuggle arbitrary SQL into the hook Job. */ -}}
{{- define "pg.validateDatabasesRoles" -}}
{{- $idRe := "^[A-Za-z_][A-Za-z0-9_]*$" -}}
{{- $privs := list "CONNECT" "CREATE" "TEMP" "TEMPORARY" "USAGE" "SELECT" "INSERT" "UPDATE" "DELETE" "TRUNCATE" "REFERENCES" "TRIGGER" "EXECUTE" "MAINTAIN" "ALL" -}}
{{- $objs := list "" "ALL_TABLES" "ALL_SEQUENCES" -}}
{{- /* Reserve the chart-internal identifiers UNCONDITIONALLY (even when repmgr / the
       monitoring user are currently disabled): a declared role colliding with one that a
       later `helm upgrade` enables would produce conflicting role management. */ -}}
{{- $reserved := list "postgres" "template0" "template1" .Values.postgresql.username .Values.repmgr.username .Values.prometheusExporter.monitoringUser.username -}}
{{- /* Precompute the declared role names and the databases a grant may target (declared
       databases + the primary database) so memberOf/grant refs can be checked at render
       time -- they may reference an entry declared later in the list. */ -}}
{{- $declaredRoles := list -}}
{{- range $r := .Values.postgresql.roles -}}{{- $declaredRoles = append $declaredRoles ($r.name | toString) -}}{{- end -}}
{{- $allowedDbs := list (.Values.postgresql.database | toString) -}}
{{- range $d := .Values.postgresql.databases -}}{{- $allowedDbs = append $allowedDbs ($d.name | toString) -}}{{- end -}}
{{- $roleNames := list -}}
{{- range $r := .Values.postgresql.roles -}}
  {{- $n := $r.name | toString -}}
  {{- if not (regexMatch $idRe $n) }}{{- fail (printf "postgresql.roles: invalid role name %q (must match %s)" $n $idRe) }}{{- end -}}
  {{- if has $n $reserved }}{{- fail (printf "postgresql.roles: %q is a reserved/internal role name and may not be redefined" $n) }}{{- end -}}
  {{- if has $n $roleNames }}{{- fail (printf "postgresql.roles: duplicate role name %q" $n) }}{{- end -}}
  {{- $roleNames = append $roleNames $n -}}
  {{- if and (ne $r.login false) (not (($r.passwordSecret).name)) $.Values.postgresql.existingSecret.enabled -}}
    {{- fail (printf "postgresql.roles[%s]: with postgresql.existingSecret.enabled the chart cannot generate a password; set an explicit passwordSecret.name/key" $n) -}}
  {{- end -}}
  {{- range $m := ($r.memberOf | default list) -}}
    {{- $mn := $m | toString -}}
    {{- if not (regexMatch $idRe $mn) }}{{- fail (printf "postgresql.roles[%s].memberOf: invalid role name %q" $n $mn) }}{{- end -}}
    {{- /* must be a declared role or a PostgreSQL predefined (pg_*) role -- otherwise the
           GRANT <group> TO <role> aborts the hook with "role does not exist". */ -}}
    {{- if and (not (has $mn $declaredRoles)) (not (hasPrefix "pg_" $mn)) }}{{- fail (printf "postgresql.roles[%s].memberOf %q must be a role declared in postgresql.roles[] or a built-in pg_* role" $n $mn) }}{{- end -}}
  {{- end -}}
  {{- range $g := ($r.grants | default list) -}}
    {{- $gdb := $g.database | toString -}}
    {{- if not (regexMatch $idRe $gdb) }}{{- fail (printf "postgresql.roles[%s].grants: invalid database %q" $n $gdb) }}{{- end -}}
    {{- if not (has $gdb $allowedDbs) }}{{- fail (printf "postgresql.roles[%s].grants: database %q must be declared in postgresql.databases[] or be the primary database (else the grant aborts the hook)" $n $gdb) }}{{- end -}}
    {{- if and $g.schema (not (regexMatch $idRe ($g.schema | toString))) }}{{- fail (printf "postgresql.roles[%s].grants: invalid schema %q" $n $g.schema) }}{{- end -}}
    {{- if not (has ($g.objects | default "" | toString) $objs) }}{{- fail (printf "postgresql.roles[%s].grants.objects must be one of \"\", ALL_TABLES, ALL_SEQUENCES (got %q)" $n $g.objects) }}{{- end -}}
    {{- if and ($g.objects | default "") (not $g.schema) }}{{- fail (printf "postgresql.roles[%s].grants: objects=%s requires a schema (an object-level grant has no database-level meaning)" $n $g.objects) }}{{- end -}}
    {{- if not $g.privileges }}{{- fail (printf "postgresql.roles[%s].grants: privileges is required" $n) }}{{- end -}}
    {{- range $p := $g.privileges -}}
      {{- if not (has (upper ($p | toString)) $privs) }}{{- fail (printf "postgresql.roles[%s].grants: privilege %q not in the allowlist (%s)" $n $p (join ", " $privs)) }}{{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $dbNames := list -}}
{{- range $d := .Values.postgresql.databases -}}
  {{- $n := $d.name | toString -}}
  {{- if not (regexMatch $idRe $n) }}{{- fail (printf "postgresql.databases: invalid database name %q (must match %s)" $n $idRe) }}{{- end -}}
  {{- if has $n (list "template0" "template1") }}{{- fail (printf "postgresql.databases: %q is reserved" $n) }}{{- end -}}
  {{- if has $n $dbNames }}{{- fail (printf "postgresql.databases: duplicate database name %q" $n) }}{{- end -}}
  {{- $dbNames = append $dbNames $n -}}
  {{- if $d.owner -}}
    {{- $o := $d.owner | toString -}}
    {{- if not (regexMatch $idRe $o) }}{{- fail (printf "postgresql.databases[%s].owner: invalid role name %q" $n $o) }}{{- end -}}
    {{- if and (not (has $o $roleNames)) (ne $o $.Values.postgresql.username) }}{{- fail (printf "postgresql.databases[%s].owner %q must be declared in postgresql.roles[] or be the primary user" $n $o) }}{{- end -}}
  {{- end -}}
  {{- range $e := ($d.extensions | default list) -}}
    {{- if not (regexMatch "^[A-Za-z_][A-Za-z0-9_-]*$" ($e | toString)) }}{{- fail (printf "postgresql.databases[%s].extensions: invalid extension name %q" $n $e) }}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{- /* True when the operator declared shared_preload_libraries in
       postgresql.configuration (matched case-insensitively -- PostgreSQL GUC names are
       case-insensitive, so a value under e.g. `Shared_Preload_Libraries` must still be
       found or it would be silently dropped from the merge). */ -}}
{{- define "pg.userSetSharedPreloadLibraries" -}}
{{- range $k, $v := (.Values.postgresql.configuration | default dict) -}}
  {{- if eq (lower ($k | toString)) "shared_preload_libraries" -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- /* True when the chart -- not custom.conf -- owns shared_preload_libraries, i.e. the
       merged value is emitted from an authoritative conf.d file that sorts after
       custom.conf. That is the case whenever a library must be preserved across an
       operator-declared value: repmgr (replication) and/or pgaudit (#219). In standalone
       mode with audit off there is nothing to preserve, so the operator's value passes
       through custom.conf untouched. */ -}}
{{- define "pg.chartOwnsSharedPreloadLibraries" -}}
{{- if or .Values.postgresql.audit.enabled (and .Values.repmgr.enabled (eq (include "pg.userSetSharedPreloadLibraries" .) "true")) -}}true{{- end -}}
{{- end -}}

{{- /* The authoritative shared_preload_libraries value. Rendered into a conf.d file that
       sorts after custom.conf under include_dir and therefore wins -- so it MUST
       reassemble the FULL list, not just the chart's own libraries.
         - repmgr is kept whenever repmgr.enabled: replication and the repmgr GUCs depend
           on the preload, and the repmgr image entrypoint writes
           `shared_preload_libraries = 'repmgr'` into PGDATA/postgresql.conf, which any
           conf.d value overrides. Dropping it disables failover (#262 review).
         - pgaudit is appended when audit.enabled (#219).
         - Libraries the operator declared in postgresql.configuration are merged in
           (comma-split, trimmed, de-duplicated) so the chart never silently drops a
           preload the operator asked for.
       Originally audit-only (pg.auditSharedPreloadLibraries); generalized because the
       audit-gated merge meant an operator-set value with audit OFF silently dropped
       repmgr and broke HA failover. */ -}}
{{- /* #288: `repmgr` is prepended only under the repmgr MECHANISM, not merely when
       repmgr.enabled. Under `native` there is no repmgr extension on the cluster, so preloading
       repmgr.so is dead weight -- and because these fragments load via conf.d's include_dir they
       would OVERRIDE the entrypoint's native gate, putting the library back on a cluster that
       has nothing to use it. Benign only while the package is still in the image: it makes every
       native cluster unstartable ("could not access file \"repmgr\"") the moment #290/#294 drop
       it. Removing the line from an EXISTING data directory stays #293's half. */ -}}
{{- define "pg.sharedPreloadLibraries" -}}
{{- $libs := list -}}
{{- $user := "" -}}
{{- range $k, $v := (.Values.postgresql.configuration | default dict) -}}
  {{- if eq (lower ($k | toString)) "shared_preload_libraries" -}}{{- $user = $v | toString -}}{{- end -}}
{{- end -}}
{{- range $l := splitList "," $user -}}
  {{- $t := trim $l -}}
  {{- if and $t (not (has $t $libs)) -}}{{- $libs = append $libs $t -}}{{- end -}}
{{- end -}}
{{- if and .Values.repmgr.enabled (ne ((.Values.repmgr.agent).mechanism | default "repmgr") "native") (not (has "repmgr" $libs)) -}}{{- $libs = prepend $libs "repmgr" -}}{{- end -}}
{{- if and .Values.postgresql.audit.enabled (not (has "pgaudit" $libs)) -}}{{- $libs = append $libs "pgaudit" -}}{{- end -}}
{{- join "," $libs -}}
{{- end -}}

{{- /* #219: validate postgresql.audit. Guards: (g1) audit requires repmgr mode -- the
       cagriekin/repmgr image bundles pgaudit; standalone uses the stock postgres image
       (no pgaudit) and a bare shared_preload_libraries=pgaudit would crash-loop the
       postmaster. (g2) log must be non-empty and every class in the allowlist (negatable
       with a leading -). (g3) role, if set, must be a valid identifier. Class + role
       validation also blocks smuggling a `'` that would break out of the single-quoted
       GUC in pgaudit.conf. */ -}}
{{- define "pg.validateAudit" -}}
{{- if .Values.postgresql.audit.enabled -}}
  {{- if not .Values.repmgr.enabled -}}
    {{- fail "postgresql.audit.enabled requires repmgr.enabled=true: audit logging needs the cagriekin/repmgr image, which bundles pgaudit. Standalone mode uses the stock postgres image (no pgaudit) and would fail to start on shared_preload_libraries. To audit in standalone mode, build a postgresql.image that ships pgaudit." -}}
  {{- end -}}
  {{- $allowed := list "read" "write" "function" "role" "ddl" "misc" "misc_set" "all" -}}
  {{- $log := .Values.postgresql.audit.log | default "" | toString -}}
  {{- if not (trim $log) -}}{{- fail "postgresql.audit.log must not be empty when audit.enabled (e.g. \"ddl, role, write\" or \"all\")" -}}{{- end -}}
  {{- range $c := splitList "," $log -}}
    {{- $t := trim $c -}}
    {{- /* Reject empty segments (stray/trailing/double comma). The raw log string is
           rendered verbatim into `pgaudit.log = '...'`, so a value like "ddl," would
           pass a skip-empties check yet leave an empty class token that pgaudit
           rejects at postmaster start -- a config the chart accepted but that breaks
           the server. Fail fast at render time instead. */ -}}
    {{- if not $t -}}{{- fail (printf "postgresql.audit.log: empty class segment in %q (check for a stray, leading, trailing, or doubled comma)" $log) -}}{{- end -}}
    {{- $cls := lower (trimPrefix "-" $t) -}}
    {{- if not (has $cls $allowed) -}}{{- fail (printf "postgresql.audit.log: %q is not a valid pgaudit class (allowed: %s, each optionally prefixed with - to subtract)" $t (join ", " $allowed)) -}}{{- end -}}
  {{- end -}}
  {{- $role := .Values.postgresql.audit.role | default "" | toString -}}
  {{- if and $role (not (regexMatch "^[A-Za-z_][A-Za-z0-9_]*$" $role)) -}}
    {{- fail (printf "postgresql.audit.role: invalid role name %q (must match ^[A-Za-z_][A-Za-z0-9_]*$)" $role) -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{- /* #262: validate the postgresql.extraVolumes / extraVolumeMounts / extraEnv
       passthrough. These are spliced verbatim into the pod spec, so without guards a
       plausible mistake becomes a silent runtime failure or an apply-time apiserver
       rejection instead of a render error -- against the chart's fail-fast convention.
       Guards:
        (g1) each value must be a LIST of objects. A map (the shape operators reach for,
             e.g. `extraEnv: {FOO: bar}`) makes toYaml emit a mapping at the sequence
             indent, so helm aborts with an opaque "did not find expected '-' indicator"
             YAML parse error naming neither the value nor the required shape.
        (g2) every entry needs a name (it is the k8s join key for volume<->mount).
        (g3) extraVolumes names may not collide with a chart-managed volume. A collision
             on `data` is the dangerous one: with persistence on, the volumeClaimTemplate
             wins and the pod-spec volume is silently discarded (the mount then resolves
             into the PVC and the expected file is simply absent -> CrashLoopBackOff with
             nothing to point at); with persistence off the pod spec carries a duplicate
             name and the apiserver rejects the StatefulSet.
        (g4) extraVolumeMounts must reference a declared extraVolumes entry. Chart-owned
             volumes are deliberately NOT mountable here -- they are internal, and
             allowing them would reintroduce the ambiguity g3 exists to prevent. This
             catches the sibling-key typo (`extraVolume:` for `extraVolumes:`), which
             values.schema.json cannot (additionalProperties is open) and which otherwise
             renders cleanly and fails only at apply, leaving a live release failed.
        (g5) extraEnv may not reuse a chart-managed env name. Duplicate env keys are legal
             YAML and last-wins in the runtime, so a PGDATA or POSTGRES_PASSWORD override
             would silently win over the chart/Secret value -- pointing the postmaster at
             the wrong data directory or breaking auth cluster-wide. */ -}}
{{- define "pg.validateExtraPassthrough" -}}
{{- $chartVolumes := list "data" "postgresql-config" "postgresql-tls" "ext-lib" "ext-share" "repmgr-config" "etcd-tls" "pg-run" "pgbackrest-config" -}}
{{- /* Env vars the chart sets on the postgresql container (see statefulset.yaml). Reserved
       UNCONDITIONALLY -- including the ones only a currently-disabled feature emits -- so a
       passthrough that works today cannot start silently shadowing a chart value after a
       later `helm upgrade` enables that feature. */ -}}
{{- $chartEnv := list
      "PGDATA" "POSTGRES_USER" "POSTGRES_PASSWORD" "POSTGRES_DB"
      "REPMGR_USER" "REPMGR_PASSWORD" "REPMGR_DB" "REPMGR_NODE_COUNT" "HEADLESS_SERVICE"
      "NAMESPACE" "PRIMARY_MARKER" "POD_NAME" "POD_SELECTOR" "POD_CIDR" "MASTER_SERVICE"
      "LEASE_NAME" "LEASE_DURATION" "RENEW_DEADLINE" "RETRY_PERIOD" "RECONCILE_INTERVAL"
      "CASCADE_REPLICATION" "MECHANISM" "POSTGRESQL_PGHBA" "TLS_REQUIRE_SSL" "TLS_CLIENT_CERT_AUTH"
      "MONITORING_USER" "MIGRATE_LEGACY_MD5_USERS" "SPLIT_BRAIN_ACTION"
      "DCS_BACKEND" "ETCD_ENDPOINTS" "ETCD_PREFIX" "ETCD_TLS_CERT" "ETCD_TLS_KEY" "ETCD_TLS_CA"
      "PGBACKREST_ENABLED" "PGBACKREST_STANZA" "PGBACKREST_REPO1_CIPHER_PASS"
      "PGBACKREST_REPO1_S3_KEY" "PGBACKREST_REPO1_S3_KEY_SECRET" -}}
{{- /* g1: shape. `with` is not used -- an explicitly-set map must reach the check. */ -}}
{{- range $key := list "extraVolumes" "extraVolumeMounts" "extraEnv" -}}
  {{- $val := index $.Values.postgresql $key -}}
  {{- if and $val (not (kindIs "slice" $val)) -}}
    {{- fail (printf "postgresql.%s must be a list of objects, got %s. Use a YAML sequence of entries, each with a name (e.g. `postgresql.%s: [{name: my-entry, ...}]`), not a map." $key (kindOf $val) $key) -}}
  {{- end -}}
{{- end -}}
{{- $volNames := list -}}
{{- range $i, $v := (.Values.postgresql.extraVolumes | default list) -}}
  {{- $n := ($v.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "postgresql.extraVolumes[%d]: name is required" $i) -}}{{- end -}}
  {{- if has $n $chartVolumes -}}
    {{- fail (printf "postgresql.extraVolumes[%d]: %q is a chart-managed volume name and may not be reused (reserved: %s). With persistence enabled a %q volume is silently discarded in favour of the volumeClaimTemplate; with persistence disabled the duplicate name is rejected by the API server. Pick a distinct name." $i $n (join ", " $chartVolumes) $n) -}}
  {{- end -}}
  {{- if has $n $volNames -}}{{- fail (printf "postgresql.extraVolumes[%d]: duplicate volume name %q" $i $n) -}}{{- end -}}
  {{- $volNames = append $volNames $n -}}
{{- end -}}
{{- range $i, $m := (.Values.postgresql.extraVolumeMounts | default list) -}}
  {{- $n := ($m.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "postgresql.extraVolumeMounts[%d]: name is required" $i) -}}{{- end -}}
  {{- if not ($m.mountPath | default "") -}}{{- fail (printf "postgresql.extraVolumeMounts[%d] (%s): mountPath is required" $i $n) -}}{{- end -}}
  {{- if not (has $n $volNames) -}}
    {{- fail (printf "postgresql.extraVolumeMounts[%d]: no postgresql.extraVolumes entry named %q (declared: %s). Every extra mount needs a matching extra volume -- otherwise the API server rejects the StatefulSet at apply time with `volumeMounts[..].name: Not found`. Check for a typo, or that you set extraVolumes (plural). Chart-managed volumes cannot be mounted here." $i $n (ternary "none" (join ", " $volNames) (empty $volNames))) -}}
  {{- end -}}
{{- end -}}
{{- $envNames := list -}}
{{- range $i, $e := (.Values.postgresql.extraEnv | default list) -}}
  {{- $n := ($e.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "postgresql.extraEnv[%d]: name is required" $i) -}}{{- end -}}
  {{- if has $n $chartEnv -}}
    {{- fail (printf "postgresql.extraEnv[%d]: %q is set by the chart on the postgresql container and may not be overridden -- a duplicate env name is last-wins at runtime, so this would silently shadow the chart/Secret value (e.g. pointing the postmaster at the wrong PGDATA, or breaking cluster-wide auth). Use the chart's own value for this setting instead." $i $n) -}}
  {{- end -}}
  {{- if has $n $envNames -}}{{- fail (printf "postgresql.extraEnv[%d]: duplicate env name %q" $i $n) -}}{{- end -}}
  {{- $envNames = append $envNames $n -}}
{{- end -}}
{{- end }}

{{- define "pg.pgpoolAdminSecretName" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- required "pgpool.admin.existingSecret.name is required when pgpool.admin.existingSecret.enabled is true" .Values.pgpool.admin.existingSecret.name }}
{{- else }}
{{- include "pg.fullname" . }}-pgpool-admin
{{- end }}
{{- end }}

{{- define "pg.pgpoolAdminUsernameKey" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- .Values.pgpool.admin.existingSecret.usernameKey }}
{{- else }}
{{- "username" }}
{{- end }}
{{- end }}

{{- define "pg.pgpoolAdminPasswordKey" -}}
{{- if .Values.pgpool.admin.existingSecret.enabled }}
{{- .Values.pgpool.admin.existingSecret.passwordKey }}
{{- else }}
{{- "password" }}
{{- end }}
{{- end }}

{{- define "pg.backupSecretName" -}}
{{- .Values.backup.existingSecret.name }}
{{- end }}

{{- define "pg.backupAccessKeyIdKey" -}}
{{- .Values.backup.existingSecret.accessKeyIdKey }}
{{- end }}

{{- define "pg.backupSecretAccessKeyKey" -}}
{{- .Values.backup.existingSecret.secretAccessKeyKey }}
{{- end }}

{{/*
Port implied by an S3 endpoint. Accepts host, host:port, or
scheme://host[:port]; an explicit port wins, otherwise the scheme
decides (http 80, anything else 443).
*/}}
{{- define "pg.s3EndpointPort" -}}
{{- $hostport := regexReplaceAll "^[a-zA-Z][a-zA-Z0-9+.-]*://" . "" -}}
{{- $hostport = splitList "/" $hostport | first -}}
{{- if regexMatch ":[0-9]+$" $hostport -}}
{{- splitList ":" $hostport | last -}}
{{- else if hasPrefix "http://" . -}}
80
{{- else -}}
443
{{- end -}}
{{- end }}

{{- define "pg.exporterPodSpec" -}}
# The exporter scrapes PostgreSQL only, never the Kubernetes API, so don't mount an SA token (#166).
automountServiceAccountToken: false
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.prometheusExporter.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
securityContext:
  {{- toYaml .Values.prometheusExporter.podSecurityContext | nindent 2 }}
initContainers:
  - name: init-config
    image: {{ include "pg.image" .Values.busyboxImage | quote }}
    imagePullPolicy: {{ .Values.busyboxImage.pullPolicy }}
    securityContext:
      {{- toYaml .Values.prometheusExporter.containerSecurityContext | nindent 6 }}
    command:
      - /bin/sh
      - -c
      - |
        # Placeholders sit inside single-quoted YAML scalars: double any
        # embedded quote, then splice byte-for-byte (sed replacement
        # corrupts values containing / & or \).
        SUB_USER=$(printf '%s' "$POSTGRES_USER" | sed "s/'/''/g")
        SUB_PASS=$(printf '%s' "$POSTGRES_PASSWORD" | sed "s/'/''/g")
        SUB_DB=$(printf '%s' "$POSTGRES_DATABASE" | sed "s/'/''/g")
        export SUB_USER SUB_PASS SUB_DB
        awk '
          function splice(s, ph, val,   out, i) {
            out = ""
            while (i = index(s, ph)) { out = out substr(s, 1, i - 1) val; s = substr(s, i + length(ph)) }
            return out s
          }
          { $0 = splice($0, "__POSTGRES_USER__", ENVIRON["SUB_USER"])
            $0 = splice($0, "__POSTGRES_PASSWORD__", ENVIRON["SUB_PASS"])
            $0 = splice($0, "__POSTGRES_DB__", ENVIRON["SUB_DB"])
            print }
        ' /config/postgres_exporter.yml > /etc/postgres_exporter/postgres_exporter.yml
        # URI userinfo cannot carry @ : / etc. raw and Kubernetes $(VAR)
        # expansion cannot encode, so the DSN is assembled here with every
        # credential byte percent-encoded (over-encoding is valid in URIs).
        enc() { printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n' | sed 's/../%&/g'; }
        ENC_USER=$(enc "$POSTGRES_USER")
        ENC_PASS=$(enc "$POSTGRES_PASSWORD")
        ENC_DB=$(enc "$POSTGRES_DATABASE")
        printf '%s' "{{ range $i := until (int (add .Values.postgresql.replicaCount 1)) }}{{ if $i }},{{ end }}postgresql://${ENC_USER}:${ENC_PASS}@{{ include "pg.fullname" $ }}-{{ $i }}.{{ include "pg.fullname" $ }}-headless:5432/${ENC_DB}?sslmode={{ $.Values.prometheusExporter.sslmode }}{{ if has $.Values.prometheusExporter.sslmode (list "verify-ca" "verify-full") }}&sslrootcert=/etc/postgres_exporter/tls/ca.crt{{ end }}{{ end }}" > /etc/postgres_exporter/dsn
    env:
{{- if .Values.prometheusExporter.monitoringUser.enabled }}
      # Scrape as the least-privilege pg_monitor role (created by the monitoring-user
      # hook Job), not the postgres superuser (#28).
      - name: POSTGRES_USER
        value: {{ .Values.prometheusExporter.monitoringUser.username | quote }}
      - name: POSTGRES_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretMonitoringPasswordKey" . }}
{{- else }}
      - name: POSTGRES_USER
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretUsernameKey" . }}
      - name: POSTGRES_PASSWORD
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretPasswordKey" . }}
{{- end }}
      - name: POSTGRES_DATABASE
        valueFrom:
          secretKeyRef:
            name: {{ include "pg.secretName" . }}
            key: {{ include "pg.secretDatabaseKey" . }}
    resources:
      {{- include "pg.initResources" . | nindent 6 }}
    volumeMounts:
      - name: config
        mountPath: /config
      - name: exporter-config
        mountPath: /etc/postgres_exporter
      - name: tmp
        mountPath: /tmp
containers:
  - name: postgres-exporter
    image: {{ include "pg.image" .Values.prometheusExporter.image | quote }}
    imagePullPolicy: {{ .Values.prometheusExporter.image.pullPolicy }}
    securityContext:
      {{- toYaml .Values.prometheusExporter.containerSecurityContext | nindent 6 }}
    command:
      - /bin/sh
      - -c
      - |
        DATA_SOURCE_NAME="$(cat /etc/postgres_exporter/dsn)" exec /bin/postgres_exporter \
          --config.file=/etc/postgres_exporter/postgres_exporter.yml \
          --extend.query-path=/config/queries.yaml \
          --web.listen-address=:9116 \
          --web.telemetry-path=/metrics \
          --log.level=info
    ports:
      - name: metrics
        containerPort: 9116
        protocol: TCP
    livenessProbe:
      httpGet:
        # /metrics (not the always-200 landing page /) so the probe fails on a broken
        # scrape pipeline -- a queries.yaml/collector regression returns 500 here while
        # / stays 200 (#146). A DB outage returns 200 + pg_up 0, so this does not flap.
        path: /metrics
        port: metrics
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      httpGet:
        # /metrics (not the always-200 landing page /) so the probe fails on a broken
        # scrape pipeline -- a queries.yaml/collector regression returns 500 here while
        # / stays 200 (#146). A DB outage returns 200 + pg_up 0, so this does not flap.
        path: /metrics
        port: metrics
      initialDelaySeconds: 5
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    resources:
      {{- toYaml .Values.prometheusExporter.resources | nindent 6 }}
    volumeMounts:
      - name: exporter-config
        mountPath: /etc/postgres_exporter
      # queries.yaml carries no credential placeholders, so it is read
      # straight from the configmap instead of the init-processed copy
      - name: config
        mountPath: /config
      - name: tmp
        mountPath: /tmp
{{- if and .Values.postgresql.tls.enabled (has .Values.prometheusExporter.sslmode (list "verify-ca" "verify-full")) }}
      # CA to verify the PostgreSQL server cert under sslmode=verify-* (#110).
      - name: postgresql-tls
        mountPath: /etc/postgres_exporter/tls
        readOnly: true
{{- end }}
volumes:
  - name: config
    configMap:
      name: {{ include "pg.fullname" . }}-postgres-exporter
  - name: exporter-config
    emptyDir:
      sizeLimit: 16Mi
  - name: tmp
    emptyDir:
      sizeLimit: 64Mi
{{- if and .Values.postgresql.tls.enabled (has .Values.prometheusExporter.sslmode (list "verify-ca" "verify-full")) }}
  - name: postgresql-tls
    secret:
      secretName: {{ .Values.postgresql.tls.existingSecret | quote }}
      # Project only the (public) CA at a world-readable mode (#204). Secret-volume
      # files are owned root:root; the exporter runs as a non-root UID with no fsGroup,
      # so the previous 0400 left ca.crt unreadable (sslmode=verify-* scrapes failed with
      # "permission denied", pg_up=0). The exporter verifies the server cert with ca.crt
      # only -- it needs neither tls.crt nor the server private key tls.key, so they are
      # no longer mounted.
      items:
        - key: ca.crt
          path: ca.crt
      defaultMode: 0444
{{- end }}
{{- end }}

{{- /* The repmgrd failover path was removed in 2.0.0 (#286): the lease-based Go agent has
       been the default since 1.0.0 and repmgrd was deprecated for one major cycle. The keys
       that only ever configured repmgrd are gone, and a values file still carrying them
       would otherwise deploy an agent cluster while its author believes repmgrd is running
       -- silence here is the dangerous outcome, so fail at render time (invariant 4). */ -}}
{{- define "pg.validateRemovedRepmgrdValues" -}}
{{- if hasKey .Values.repmgr "failoverMode" -}}
{{- $m := .Values.repmgr.failoverMode | toString -}}
{{- if eq $m "agent" -}}
{{- fail "repmgr.failoverMode was removed in chart 2.0.0: the lease-based agent is now the only failover path, so `failoverMode: agent` no longer means anything. Delete this key -- nothing else changes for you, and the agent is still tuned under repmgr.agent.*." -}}
{{- else -}}
{{- fail (printf "repmgr.failoverMode was removed in chart 2.0.0 (got %q): repmgrd and its service-updater sidecar are gone, and the lease-based agent is now the only failover path. Deleting this key switches this release to the agent -- which also flips the StatefulSet's podManagementPolicy from OrderedReady to Parallel, and that field is IMMUTABLE. Read the 2.0.0 upgrade note in CHANGELOG.md before upgrading: the StatefulSet has to be recreated with `kubectl delete sts <name> --cascade=orphan`." $m) -}}
{{- end -}}
{{- end -}}
{{- if hasKey .Values.repmgr "serviceUpdater" -}}
{{- fail "repmgr.serviceUpdater.* was removed in chart 2.0.0: the service-updater sidecar only existed to reconcile PGPool backends after a repmgrd failover, and the agent does that itself. Delete this key." -}}
{{- end -}}
{{- if hasKey .Values.repmgr "monitoringHistoryDays" -}}
{{- fail "repmgr.monitoringHistoryDays was removed in chart 2.0.0: it pruned repmgr.monitoring_history, which only repmgrd ever wrote. Delete this key." -}}
{{- end -}}
{{- if hasKey .Values.pgpool "autoFailback" -}}
{{- fail "pgpool.autoFailback was removed in chart 2.0.0: it rendered PGPool's auto_failback, which only applied to the repmgrd failover flow. The agent fronts the Services and re-points them itself, so PGPool never fails a backend over. Delete this key." -}}
{{- end -}}
{{- end -}}

{{/*
pg.agentMode renders the string "true"/"false". Call sites gate with:
  {{- if eq (include "pg.agentMode" .) "true" }}
Since 2.0.0 the agent is the only failover path, so this is exactly "HA is enabled"
(repmgr.enabled=false is the standalone, single-node, stock-postgres-image mode). The
helper is kept rather than inlined so the ~20 call sites keep reading as a mode check.
*/}}
{{- define "pg.agentMode" -}}
{{- .Values.repmgr.enabled -}}
{{- end -}}

{{- /* The single condition under which the `create jobs` grant is rendered (#276). Both
       rbac.yaml (the grant) and agent-restore-admissionpolicy.yaml (the bound on it) gate
       on THIS helper rather than on the value directly, so the grant and its guard cannot
       drift apart -- a Role that carries the escalation primitive without the policy that
       bounds it is the failure mode #279 exists to prevent. */ -}}
{{- define "pg.controlRestoreEnabled" -}}
{{- and (eq (include "pg.agentMode" .) "true") .Values.repmgr.agent.control.restore.enabled -}}
{{- end -}}

{{- /* Name of the ValidatingAdmissionPolicy (and its binding) that bounds the restore
       Job-create grant (#279). Both objects are CLUSTER-scoped, so the name has to be
       unique per (namespace, release): pg.fullname alone is only namespace-unique, and two
       releases in different namespaces would otherwise fight over one policy object.

       The namespace and fullname are there to be read, but they are NOT what makes the
       name unique -- joining them with a hyphen is ambiguous, because both segments may
       contain hyphens themselves ("db" in namespace "pg-prod" and "prod-db" in namespace
       "pg" both produce pg-prod-db-restore-guard). The trailing hash of the unambiguous
       "<namespace>/<fullname>" pair is the actual discriminator. That matters more than it
       looks: on a collision the second install fails with an ownership error, and an
       operator who resolves it by adoption -- or who later uninstalls the first release --
       leaves the survivor's `create jobs` grant with no policy bounding it, which is the
       exact state #279 exists to make unreachable.

       Worst case is 63 (namespace) + 1 + 63 (fullname) + 14 + 9 = 150 characters, inside
       the 253-character DNS-subdomain limit these kinds validate against, so nothing is
       truncated (truncation would reintroduce collisions). */ -}}
{{- define "pg.restoreGuardName" -}}
{{- $fullname := include "pg.fullname" . -}}
{{- printf "%s-%s-restore-guard-%s" .Release.Namespace $fullname (printf "%s/%s" .Release.Namespace $fullname | sha256sum | trunc 8) -}}
{{- end -}}

{{- /* The restore container's command, defined once (#279). pgbackrest-restore-job.yaml
       renders it as YAML flow style and agent-restore-admissionpolicy.yaml pins it as a
       CEL list literal (the same text with CEL's single quotes), so the policy cannot
       drift from the command it is meant to pin. */ -}}
{{- define "pg.restoreCommandJSON" -}}
["/bin/bash", "/scripts/restore.sh"]
{{- end -}}

{{- /* CEL fragment pinning one scalar field to exactly what the chart renders (#279):
       "present and equal" when the release's values set the field, "absent" when they do
       not -- which is precisely the shape the rendered Job has, so the policy can never
       deny the release's own restore. Args: (list <CEL path> <values dict> <key>). */ -}}
{{- define "pg.celScalarPin" -}}
{{- $path := index . 0 -}}
{{- $src := index . 1 -}}
{{- $key := index . 2 -}}
{{- if hasKey $src $key -}}
{{- $v := index $src $key -}}
{{- if kindIs "string" $v -}}
(has({{ $path }}) && {{ $path }} == '{{ $v }}')
{{- else -}}
(has({{ $path }}) && {{ $path }} == {{ $v }})
{{- end -}}
{{- else -}}
!has({{ $path }})
{{- end -}}
{{- end -}}

{{- /* Reject values that cannot be safely interpolated into a single-quoted CEL string
       literal (#279). agent-restore-admissionpolicy.yaml embeds names straight into CEL
       expressions -- 'PGBACKREST_REPO1_S3_KEY' style Secret names, the image reference,
       the namespace, the fullname -- and CEL has no escaping helper on the helm side. A
       value containing an apostrophe produces a syntactically broken expression that
       renders, lints and unit-tests clean and only fails when the API server parses the
       policy; a crafted value (`x' || true || '`) would instead produce a TAUTOLOGICAL
       validation, leaving a policy that looks installed and enforces nothing. Both are
       exactly the apply-time failure the chart's render-time-validation rule exists to
       prevent, so the charset is checked here instead.

       The allowed set is the union of what Kubernetes object names and OCI image
       references need: alphanumerics plus . _ - / : @ (no quotes, no backslash, no
       whitespace). Callers pass a list of (label, value) pairs so the message can say
       which input was wrong. */ -}}
{{- define "pg.validateCelLiterals" -}}
{{- range $pair := . -}}
{{- $label := index $pair 0 -}}
{{- $value := index $pair 1 | toString -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._:/@-]*$" $value) -}}
{{- fail (printf "%s is %q, which cannot be embedded in the restore admission policy's CEL expressions (#279): it must match ^[A-Za-z0-9][A-Za-z0-9._:/@-]*$ (alphanumerics and . _ - / : @). Quotes, whitespace and backslashes would either break the policy at apply time or silently turn a validation into a tautology. Fix the value, or disable the policy deliberately with repmgr.agent.control.restore.admissionPolicy.enabled=false plus acknowledgeUnbounded=true" $label $value) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /* True in agent mode when the leadership backend is etcd (repmgr.agent.dcs.backend
       == "etcd"), false otherwise. Nil-safe at every level so a partial overlay does
       not nil-pointer; defaults to the kubernetes backend. */ -}}
{{- define "pg.agentEtcdMode" -}}
{{- if eq (include "pg.agentMode" .) "true" -}}
{{- $agent := .Values.repmgr.agent | default dict -}}
{{- $dcs := $agent.dcs | default dict -}}
{{- eq ($dcs.backend | default "kubernetes") "etcd" -}}
{{- else -}}
false
{{- end -}}
{{- end -}}
