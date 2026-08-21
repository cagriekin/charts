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
{{- if and .Values.repmgr.enabled (not (has "repmgr" $libs)) -}}{{- $libs = prepend $libs "repmgr" -}}{{- end -}}
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

{{- /* #303: postgresql.extensions.packages is apt-get argv, space-joined into a single
       `sh -c` string ahead of the existing extension-copy command in copy-ext/
       copy-base-ext (statefulset.yaml). A malicious or fat-fingered entry could inject
       shell -- backticks, $(), ;, &, |, quotes, whitespace/newlines all terminate or
       extend the intended `apt-get install` argv. Restrict to the character class
       legitimate Debian/PGDG package names and version pins actually use (letters,
       digits, + . ~ : = _ -), plus the literal `{major}` placeholder token substituted
       with postgresql.majorVersion at render time, and fail the render otherwise --
       mirrors pg.validateDatabasesRoles's identifier guard above. */ -}}
{{- define "pg.validateExtensionPackages" -}}
{{- $pkgs := .Values.postgresql.extensions.packages | default list -}}
{{- if $pkgs -}}
  {{- if not .Values.postgresql.extensions.enabled -}}
    {{- fail "postgresql.extensions.packages is set but postgresql.extensions.enabled is false, so nothing would ever install them. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- $re := "^[A-Za-z0-9][A-Za-z0-9+.~:=_-]*(\\{major\\}[A-Za-z0-9+.~:=_-]*)*$" -}}
  {{- range $p := $pkgs -}}
    {{- $s := $p | toString -}}
    {{- if not (regexMatch $re $s) -}}
      {{- fail (printf "postgresql.extensions.packages: invalid entry %q -- must be a plain Debian/PGDG package name, optionally with an =version pin, using only letters, digits, and the characters + . ~ : = _ - (plus the literal {major} placeholder substituted with postgresql.majorVersion); no whitespace, quotes, backticks, $(), ;, &, |, or newlines (the list is interpolated into an apt-get install shell command)." $s) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{- /* #310: postgresql.extensions.aptSources lets a values-driven install add a
       non-PGDG apt source (e.g. Pigsty, for pgsodium/supabase_vault/pg_graphql/...)
       inside copy-ext/copy-base-ext's own throwaway filesystem before their apt-get
       install, closing the exact gap postgresql.extensions.packages already exists to
       close for PGDG-only packages. Same injection surface as
       pg.validateExtensionPackages -- keyUrl and aptLine both get interpolated into a
       shell command (curl | gpg --dearmor, and echo > sources.list.d), so both are
       restricted to an allowlisted character class and fail the render otherwise
       (comma is allowed in aptLine -- shell-inert inside the double-quoted echo, and
       needed for the standard multi-arch option syntax, e.g. `[arch=amd64,arm64]`).
       name is rendered as `pgchart-<name>-keyring.gpg` / `pgchart-<name>.list`, not the
       bare name, so it can never collide with a source the image itself already owns
       (the repmgr image's own PGDG source is postgresql-keyring.gpg/postgresql.list,
       images/repmgr/Dockerfile) -- duplicate *entries* within aptSources itself still
       fail, since that's always a typo, never intentional. Pointless (and therefore
       rejected) without postgresql.extensions.packages: aptSources' only consumer is
       the apt-get install step gated on packages being non-empty -- see
       pg.extensionInstallCommand below. keyUrl allows & (review): it's rendered
       double-quoted in the curl argument (pg.extensionInstallCommand), where & is
       inert, and a standard keyserver lookup URL (?op=get&search=...) needs it. A
       leftover { or } in aptLine after {major} substitution fails the render (review):
       the only recognized placeholder is the literal {major} token, so anything else
       braced is a typo (e.g. {MAJOR}) that would otherwise render a literal,
       nonsensical {...} into sources.list.d and only fail later, at apply time, inside
       apt-get update. */ -}}
{{- define "pg.validateExtensionAptSources" -}}
{{- $srcs := .Values.postgresql.extensions.aptSources | default list -}}
{{- if $srcs -}}
  {{- if not .Values.postgresql.extensions.enabled -}}
    {{- fail "postgresql.extensions.aptSources is set but postgresql.extensions.enabled is false, so nothing would ever use it. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- if not (.Values.postgresql.extensions.packages | default list) -}}
    {{- fail "postgresql.extensions.aptSources is set but postgresql.extensions.packages is empty -- aptSources only exists to make packages from a non-PGDG source installable, so it has nothing to do without at least one package that needs it." -}}
  {{- end -}}
  {{- $pgMajor := .Values.postgresql.majorVersion | toString -}}
  {{- $nameRe := "^[A-Za-z0-9_-]+$" -}}
  {{- $keyUrlRe := "^https://[A-Za-z0-9._~:/?#%&=-]+$" -}}
  {{- $aptLineRe := "^[A-Za-z0-9 ./:=,_+~%\\[\\]{}@-]+$" -}}
  {{- $seen := dict -}}
  {{- range $s := $srcs -}}
    {{- $name := $s.name | toString -}}
    {{- if not (regexMatch $nameRe $name) -}}
      {{- fail (printf "postgresql.extensions.aptSources: invalid name %q -- must match ^[A-Za-z0-9_-]+$ (used to build a keyring and sources.list.d filename)" $name) -}}
    {{- end -}}
    {{- if hasKey $seen $name -}}
      {{- fail (printf "postgresql.extensions.aptSources: duplicate name %q -- each entry must have a unique name (it becomes /etc/apt/sources.list.d/pgchart-<name>.list, and a duplicate silently overwrites the earlier entry)" $name) -}}
    {{- end -}}
    {{- $seen = set $seen $name true -}}
    {{- $keyUrl := $s.keyUrl | toString -}}
    {{- if not (regexMatch $keyUrlRe $keyUrl) -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].keyUrl: invalid value %q -- must be an https:// URL using only letters, digits, and the characters . _ ~ : / ? # %% & = - (it is interpolated into a shell command); no whitespace, quotes, backticks, $(), ;, |, or newlines" $name $keyUrl) -}}
    {{- end -}}
    {{- $aptLine := $s.aptLine | toString -}}
    {{- if not (regexMatch $aptLineRe $aptLine) -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].aptLine: invalid value %q -- must use only letters, digits, spaces, and the characters . / : = , + ~ %% [ ] { } @ - (the literal {major} placeholder is substituted with postgresql.majorVersion); no quotes, backticks, $(), ;, &, |, or newlines (it is interpolated into a shell command)" $name $aptLine) -}}
    {{- end -}}
    {{- $substituted := $aptLine | replace "{major}" $pgMajor -}}
    {{- if regexMatch "[{}]" $substituted -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].aptLine: %q still contains a { or } after substituting {major} -- the only recognized placeholder is the literal {major} token; apt source lines have no legitimate use for braces otherwise" $name $substituted) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{- /* #309: postgresql.extensions.extraLibs copies additional files -- by exact,
       user-specified path -- out of copy-ext/copy-base-ext's own filesystem into
       /ext-extra-lib, for a transitive shared-library dependency an apt-installed
       extension needs that does NOT live under /usr/lib/postgresql/<major>/lib
       (Debian's normal package layout puts general-purpose libraries, as opposed to
       Postgres extension modules, under the multiarch path instead, e.g. libsodium23
       installs /usr/lib/x86_64-linux-gnu/libsodium.so.23 -- confirmed live -- which the
       extension-copy step, however broad its glob, never reads from). Paired with
       LD_LIBRARY_PATH on the postgresql container (statefulset.yaml), which is the
       other half of the fix: dlopen()ing the extension .so by absolute path always
       succeeds regardless of search paths, but THAT library's own NEEDED entries
       (e.g. pgsodium.so -> libsodium.so.23) still resolve via the normal dynamic-linker
       search order, and confirmed-empirically neither carries a RUNPATH/RPATH, so
       without both halves of this fix the copied dependency is simply never found.
       Explicit and values-driven, not an automatic `ldd`-and-copy-everything walk:
       copy-base-ext (repmgr image) and copy-ext (postgresql.image) can be different
       image builds, so blindly copying a resolved dependency's transitive closure risks
       silently shadowing the RUNNING container's own libc/libstdc++/etc. with a build
       from the OTHER image -- an ABI hazard, not a convenience. The denylist below
       exists for exactly that reason: these are never legitimate extraLibs entries
       (review: widened past the original libc-family set to also cover libssl,
       libcrypto, libz, the libicu family, libcrypt, and ld-linux, since the running
       postgres server itself links all of these -- shadowing any of them is just as
       dangerous as shadowing libc). extraLibs' own copies land in a volume physically
       separate from
       ext-lib (statefulset.yaml, ext-extra-lib) specifically so this denylist is the
       ONLY gate on what ends up on LD_LIBRARY_PATH -- ext-lib is also populated by the
       unvalidated *.so* glob copy, which this validator has no visibility into. */ -}}
{{- define "pg.validateExtraLibs" -}}
{{- $libs := .Values.postgresql.extensions.extraLibs | default list -}}
{{- if $libs -}}
  {{- if not .Values.postgresql.extensions.enabled -}}
    {{- fail "postgresql.extensions.extraLibs is set but postgresql.extensions.enabled is false, so nothing would ever copy them. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- if not (.Values.postgresql.extensions.packages | default list) -}}
    {{- fail "postgresql.extensions.extraLibs is set but postgresql.extensions.packages is empty -- extraLibs' copy step only runs alongside the packages apt-get install, so it has nothing to do without at least one package that needs it." -}}
  {{- end -}}
  {{- $pathRe := "^/[A-Za-z0-9._/+-]*[A-Za-z0-9._+-]$" -}}
  {{- $denyRe := "^(lib(c|m|pthread|dl|rt|resolv|nsl|gcc_s|stdc\\+\\+|ssl|crypto|crypt|z|icu[A-Za-z0-9]*)(\\.so|[.-])|ld-linux)" -}}
  {{- range $p := $libs -}}
    {{- $path := $p | toString -}}
    {{- if not (regexMatch $pathRe $path) -}}
      {{- fail (printf "postgresql.extensions.extraLibs: invalid path %q -- must be an absolute path to a FILE (no trailing /) using only letters, digits, and the characters . _ / + - (it is interpolated into a shell command); no .. traversal, whitespace, quotes, backticks, $(), ;, &, |, or newlines" $path) -}}
    {{- end -}}
    {{- if regexMatch "(^|/)\\.\\.(/|$)" $path -}}
      {{- fail (printf "postgresql.extensions.extraLibs: invalid path %q -- must not contain a .. path segment" $path) -}}
    {{- end -}}
    {{- $base := base $path -}}
    {{- if regexMatch $denyRe $base -}}
      {{- fail (printf "postgresql.extensions.extraLibs: refusing %q -- copying a core runtime library the postgres server itself links (libc/libm/libpthread/libdl/librt/libresolv/libnsl/libgcc_s/libstdc++/libssl/libcrypto/libcrypt/libz/libicu*/ld-linux) risks silently shadowing the postgresql container's own runtime with a build from a different image (copy-base-ext and copy-ext can be different images); this value is for extension-specific dependencies (e.g. libsodium.so.23), not core-runtime libraries" $base) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{- /* #309/#310: builds the copy-base-ext/copy-ext init container command: optionally
       add postgresql.extensions.aptSources ahead of the apt-get install (#310), then
       apt-get install the pinned postgresql-<major> + packages (#303), then copy the
       extension files out, then copy any postgresql.extensions.extraLibs (#309) into
       the SEPARATE ext-extra-lib volume by their exact given path (statefulset.yaml
       mounts it and points LD_LIBRARY_PATH there -- physically apart from ext-lib so
       the *.so* glob's unvalidated copies never end up on that search path). The
       *.so* glob (not *.so) additionally covers a versioned SONAME a package places
       directly alongside the extension modules under /usr/lib/postgresql/<major>/lib
       itself -- a strict superset of the old match, so safe unconditionally -- but is
       NOT what makes a multiarch-path dependency like libsodium.so.23 work; extraLibs
       (above) plus LD_LIBRARY_PATH is. noClobber selects copy-ext's `cp -n` (#302): it
       must never overwrite a lib copy-base-ext already placed from the repmgr image.
       curl is pinned to https (review): keyUrl's own character allowlist already
       forces the scheme, but -L would otherwise still follow a same-origin
       https->http redirect and silently fetch the key in plaintext. */ -}}
{{- define "pg.extensionInstallCommand" -}}
{{- $pgMajor := .pgMajor | toString -}}
{{- $pkgs := .pkgs -}}
{{- $cpFlag := "" -}}
{{- if .noClobber }}{{- $cpFlag = "-n " }}{{- end -}}
{{- $srcSteps := list -}}
{{- range $s := (.aptSources | default list) -}}
  {{- $aptLine := $s.aptLine | toString | replace "{major}" $pgMajor -}}
  {{- $srcSteps = append $srcSteps (printf "curl -fsSL --proto '=https' --proto-redir '=https' \"%s\" | gpg --dearmor -o /usr/share/keyrings/pgchart-%s-keyring.gpg" $s.keyUrl $s.name) -}}
  {{- $srcSteps = append $srcSteps (printf "echo \"%s\" > /etc/apt/sources.list.d/pgchart-%s.list" $aptLine $s.name) -}}
{{- end -}}
{{- $cmdParts := list "apt-get update" -}}
{{- if $srcSteps -}}
  {{- $cmdParts = append $cmdParts "apt-get install -y --no-install-recommends curl ca-certificates gnupg" -}}
  {{- $cmdParts = concat $cmdParts $srcSteps -}}
  {{- $cmdParts = append $cmdParts "apt-get update" -}}
{{- end -}}
{{- $cmdParts = append $cmdParts (printf "apt-get install -y --no-install-recommends ${PGVER:+\"postgresql-%s=$PGVER\"} %s" $pgMajor (join " " $pkgs)) -}}
{{- $cmdParts = append $cmdParts (printf "cp %s/usr/lib/postgresql/%s/lib/*.so* /ext-lib/" $cpFlag $pgMajor) -}}
{{- $cmdParts = append $cmdParts (printf "cp %s/usr/share/postgresql/%s/extension/* /ext-share/" $cpFlag $pgMajor) -}}
{{- range $l := (.extraLibs | default list) -}}
  {{- $cmdParts = append $cmdParts (printf "cp %s%s /ext-extra-lib/" $cpFlag $l) -}}
{{- end -}}
{{- printf "PGVER=$(dpkg-query -W postgresql-%s 2>/dev/null | cut -f2); %s" $pgMajor (join " && " $cmdParts) -}}
{{- end }}

{{- /* #308: wal_level has exactly one authoritative source: postgresql.walLevel.
       pgbackrest-archive.conf (postgresql-configmap.yaml) renders it there, and that file
       sorts after custom.conf under conf.d's include_dir, so a bare
       postgresql.configuration.wal_level would be silently overridden back to
       postgresql.walLevel's value (replica, by default) the moment pgbackrest.enabled is
       true -- exactly the confusing "which one wins, and does it depend on
       pgbackrest.enabled" footgun this issue is about. Reject it outright, unconditionally
       (not just when pgbackrest is on), so the answer is never "it depends": there is
       only ever one way to set this GUC. */ -}}
{{- define "pg.validateWalLevel" -}}
{{- range $key, $_ := (.Values.postgresql.configuration | default dict) }}
{{- if eq (lower ($key | toString)) "wal_level" }}
{{- fail "postgresql.configuration.wal_level is set, but wal_level has a single authoritative source: postgresql.walLevel (enum: replica|logical). Move the value there instead." }}
{{- end }}
{{- end }}
{{- end }}

{{- /* #308: synchronized_standby_slots and the sync_replication_slots worker do not
       exist before PostgreSQL 17 -- an unrecognized GUC in a conf.d file makes the
       postmaster refuse to start, crash-looping every pod. postgresql.majorVersion is a
       freeform string (no schema enum), so nothing else catches
       syncReplicationSlots=true paired with an older major before it reaches a running
       cluster; fail at render time instead. Scoped to agent mode, matching the
       postgresql-configmap.yaml/statefulset.yaml render condition -- the value is
       already a no-op outside agent mode (see repmgr.agent.cascadingReplication for the
       same pattern), so it should not block an unrelated repmgrd-mode/older-major
       install that merely left the value set from a prior config. */}}
{{- define "pg.validateSyncReplicationSlotsMajor" -}}
{{- if and (eq (include "pg.agentMode" .) "true") .Values.repmgr.agent.syncReplicationSlots }}
{{- if lt (atoi (toString .Values.postgresql.majorVersion)) 17 }}
{{- fail (printf "repmgr.agent.syncReplicationSlots requires PostgreSQL 17+ (synchronized_standby_slots and the sync_replication_slots worker were introduced in 17), but postgresql.majorVersion=%q. Set postgresql.majorVersion to \"17\" or \"18\", or set repmgr.agent.syncReplicationSlots to false." (toString .Values.postgresql.majorVersion)) }}
{{- end }}
{{- end }}
{{- end }}

{{- /* #308: the sync_replication_slots worker PostgreSQL starts on every standby when
       syncReplicationSlots=true also requires wal_level >= logical (ValidateSlotSyncParams);
       with wal_level=replica (the chart default) it is NOT a harmless no-op -- the worker
       fails its own startup validation and PostgreSQL respawns it on a fixed interval
       forever, so every standby logs a repeating "wal_level" error. Same agent-mode
       scoping as pg.validateSyncReplicationSlotsMajor. */}}
{{- define "pg.validateSyncReplicationSlotsWalLevel" -}}
{{- if and (eq (include "pg.agentMode" .) "true") .Values.repmgr.agent.syncReplicationSlots }}
{{- if ne (.Values.postgresql.walLevel | default "replica") "logical" }}
{{- fail (printf "repmgr.agent.syncReplicationSlots requires postgresql.walLevel: logical (the sync_replication_slots worker it enables on every standby fails its own startup validation below that, and PostgreSQL restarts it forever, logging the failure on a fixed interval), but postgresql.walLevel=%q. Set postgresql.walLevel to \"logical\", or set repmgr.agent.syncReplicationSlots to false." (.Values.postgresql.walLevel | default "replica")) }}
{{- end }}
{{- end }}
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
{{- $chartVolumes := list "data" "postgresql-config" "postgresql-tls" "ext-lib" "ext-share" "ext-extra-lib" "repmgr-config" "etcd-tls" "pg-run" "pgbackrest-config" "service-updater-script" -}}
{{- /* Env vars the chart sets on the postgresql container (see statefulset.yaml). Reserved
       UNCONDITIONALLY -- including the ones only a currently-disabled feature emits -- so a
       passthrough that works today cannot start silently shadowing a chart value after a
       later `helm upgrade` enables that feature. */ -}}
{{- $chartEnv := list
      "PGDATA" "POSTGRES_USER" "POSTGRES_PASSWORD" "POSTGRES_DB"
      "REPMGR_USER" "REPMGR_PASSWORD" "REPMGR_DB" "REPMGR_NODE_COUNT" "HEADLESS_SERVICE"
      "NAMESPACE" "PRIMARY_MARKER" "POD_NAME" "POD_SELECTOR" "POD_CIDR" "MASTER_SERVICE"
      "LEASE_NAME" "LEASE_DURATION" "RENEW_DEADLINE" "RETRY_PERIOD" "RECONCILE_INTERVAL"
      "CASCADE_REPLICATION" "SYNC_REPLICATION_SLOTS" "POSTGRESQL_PGHBA" "TLS_REQUIRE_SSL" "TLS_CLIENT_CERT_AUTH"
      "MONITORING_USER" "MIGRATE_LEGACY_MD5_USERS" "SPLIT_BRAIN_ACTION"
      "DCS_BACKEND" "ETCD_ENDPOINTS" "ETCD_PREFIX" "ETCD_TLS_CERT" "ETCD_TLS_KEY" "ETCD_TLS_CA"
      "PGBACKREST_ENABLED" "PGBACKREST_STANZA" "PGBACKREST_REPO1_CIPHER_PASS"
      "PGBACKREST_REPO1_S3_KEY" "PGBACKREST_REPO1_S3_KEY_SECRET" "MONITORING_HISTORY_DAYS"
      "LD_LIBRARY_PATH" -}}
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

{{- define "pg.preStop" -}}
preStop:
  exec:
    command:
      - /bin/bash
      - -c
      - |
        # Stop cleanly and let repmgrd on a standby own the failover:
        # its promote_command (repmgr standby promote) updates
        # repmgr.nodes metadata, which a raw SQL-level promotion issued
        # from this hook cannot do -- the promoted node would keep
        # type='standby', and every repmgrd then exits on the stale
        # metadata instead of converging.
        pg_ctl stop -D "$PGDATA" -m fast -w -t 30
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

{{/*
Failover mode (repmgrd default | agent). Fails fast on an unknown value.
*/}}
{{- define "pg.failoverMode" -}}
{{- $m := .Values.repmgr.failoverMode | default "repmgrd" -}}
{{- if not (or (eq $m "repmgrd") (eq $m "agent")) -}}
{{- fail (printf "repmgr.failoverMode must be 'repmgrd' or 'agent', got %q" $m) -}}
{{- end -}}
{{- $m -}}
{{- end -}}

{{/*
pg.agentMode / pg.repmgrdMode render the string "true"/"false". Call sites gate
with: {{- if eq (include "pg.agentMode" .) "true" }}
*/}}
{{- define "pg.agentMode" -}}
{{- and .Values.repmgr.enabled (eq (include "pg.failoverMode" .) "agent") -}}
{{- end -}}

{{- define "pg.repmgrdMode" -}}
{{- and .Values.repmgr.enabled (eq (include "pg.failoverMode" .) "repmgrd") -}}
{{- end -}}

{{- /* The single condition under which postgresql-configmap.yaml renders a ConfigMap at
       all (#308). statefulset.yaml needs this SAME condition in seven places -- the
       checksum annotation, the postgresql-config volume mount (twice, for postStart
       include_dir wiring on two different code paths), and the volume definition itself
       -- and duplicating the raw boolean expression at each site is exactly how the
       postgresql.walLevel / repmgr.agent.syncReplicationSlots additions below ended up
       missed at first: the ConfigMap's own guard was updated but the volume MOUNT guard
       was not, so the rendered ConfigMap existed but was never attached to the pod (a
       live KinD suite run caught it -- wal_level and sync_replication_slots silently
       stayed at their defaults with no error). Every one of those seven sites -- and the
       ConfigMap's own top-level `if` -- must use this helper instead of repeating the
       condition, so a future addition to the list cannot drift the same way. */ -}}
{{- define "pg.postgresqlConfigRenders" -}}
{{- /* `if`, not a bare `or` -- Sprig's `or` returns the first truthy ARGUMENT (here,
       postgresql.configuration itself, a map, when non-empty), not a boolean, so
       stringifying its result directly would print the map's Go representation instead
       of "true" and every `eq (include ...) "true"` call site would always be false.
       `if` properly coerces any type's truthiness and lets this emit a literal "true"
       or empty string, matching what those call sites actually compare against. */ -}}
{{- if or .Values.postgresql.configuration .Values.pgbackrest.enabled .Values.postgresql.tls.enabled .Values.postgresql.audit.enabled (ne (.Values.postgresql.walLevel | default "replica") "replica") (and (eq (include "pg.agentMode" .) "true") .Values.repmgr.agent.syncReplicationSlots) -}}
true
{{- end -}}
{{- end -}}

{{- /* The single condition under which the `create jobs` grant is rendered (#276). Both
       rbac.yaml (the grant) and agent-restore-admissionpolicy.yaml (the bound on it) gate
       on THIS helper rather than on the value directly, so the grant and its guard cannot
       drift apart -- a Role that carries the escalation primitive without the policy that
       bounds it is the failure mode #279 exists to prevent. */ -}}
{{- define "pg.controlRestoreEnabled" -}}
{{- and .Values.repmgr.enabled (eq (include "pg.agentMode" .) "true") .Values.repmgr.agent.control.restore.enabled -}}
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
