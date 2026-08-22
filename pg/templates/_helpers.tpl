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
{{- /* An empty tag must not render "repo:" -- and must not render a bare "repo" either.

       With a digest and no tag, `printf "%s:%s"` produced `repo:@sha256:...`, which containerd
       rejects as unparseable (InvalidImageName): the container never starts. That was
       reachable, because postgresql.extensions.image documents digest as the production pin
       and accepts it without a tag (#320).

       Dropping the colon whenever tag is falsy fixed that but introduced something worse
       (#320 review): a values file that CLEARS a tag -- `tag:` with no value, which is what a
       values-file merge produces -- then rendered a bare `repo`, i.e. an implicit :latest. On a
       StatefulSet with an existing PGDATA a future :latest is a different PostgreSQL major and
       the postmaster refuses to start; even on a fresh install the major is unpinned across
       restarts. The previous `repo:` at least failed fast and visibly. Every image in the chart
       routes through here, not just the extension one, so this is the wrong place to be
       permissive: an unpinned image is refused outright.

       So: digest alone is a complete reference; tag alone is the ordinary case; both together
       is legal and means "resolve this digest, the tag is decoration"; neither is an error. */ -}}
{{- define "pg.image" -}}
{{- if not .repository -}}
{{- /* An empty repository renders ":tag" or "@sha256:..." -- unparseable, the same
       InvalidImageName class as the empty-tag case below (#320 review). */ -}}
{{- fail "an image block has an empty repository, which renders an unparseable reference (\":tag\"). Set the repository, or leave the whole image block at its chart default." -}}
{{- end -}}
{{- if and (not .tag) (not .digest) -}}
{{- fail (printf "image %q has neither a tag nor a digest, which would deploy an implicit :latest -- unpinned across pod restarts, and on a StatefulSet with existing data a future :latest can be a different PostgreSQL major that refuses to start on it. Set a tag or a digest." (.repository | default "<empty repository>")) -}}
{{- end -}}
{{- if .tag -}}
{{- printf "%s:%s" .repository .tag -}}
{{- else -}}
{{- .repository -}}
{{- end -}}
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
    {{- if regexMatch "(?i)(trusted|allow-insecure|allow-weak|allow-downgrade-to-insecure) *=" $substituted -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].aptLine: %q sets an apt option that weakens or disables signature verification (trusted=/allow-insecure=/allow-weak=/allow-downgrade-to-insecure=) -- this makes the curl/gpg key-verification step above decorative and installs unsigned or weakly-signed packages as root; sign the source properly with signed-by= instead" $name $substituted) -}}
    {{- end -}}
    {{- /* #320: a PGDG source here is always fatal, and the failure gives no hint why.
           Both postgres:*-trixie and the repmgr image already configure
           apt.postgresql.org under their OWN keyring path, and this chart derives its
           keyring path from the entry name (pgchart-<name>-keyring.gpg) with no way to
           override it -- so apt sees two entries for the same repo with different
           Signed-By values and rejects the ENTIRE source list:
             E: Conflicting values set for option Signed-By regarding source
                http://apt.postgresql.org/pub/repos/apt/ trixie-pgdg
           The install then fails before it starts, and nothing points at the aptSources
           entry as the cause. Omitting it is correct: PGDG packages already resolve from
           the image's own configuration, which is exactly what `packages` relies on. */ -}}
    {{- /* Anchored on the URL AUTHORITY, not a bare substring (#320 review). A substring
           match also rejects a mirror that merely has the upstream name in its PATH --
           e.g. https://mirror.corp/apt.postgresql.org/pub/repos/apt -- which is exactly
           the internal-mirror case this whole change exists to enable, and the failure
           message would have told the operator to rely on "the image's own configuration",
           which points at the public host they cannot reach. */ -}}
    {{- /* Authority AND dist (#320 review). APT keys the Signed-By conflict on URI plus
           DIST, and the images configure only `<codename>-pgdg` -- so `trixie-pgdg-testing`
           or a `-pgdg-snapshot` suite does NOT conflict and is a legitimate way to install a
           newer extension build. Matching the authority alone hard-failed those, and the
           message told the operator to fall back on "the image's own configuration", which
           does not carry that suite at all. The dist is the token after the URL; require it
           to END in -pgdg. */ -}}
    {{- if regexMatch "(?i)://([^/ ]*@)?apt\\.postgresql\\.org([:/][^ ]*)? +[a-z0-9.]+-pgdg( |$)" $substituted -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].aptLine points at apt.postgresql.org (%q). Remove this entry: both the repmgr image and postgres:*-trixie already configure PGDG under their own keyring path, and adding a second entry for the same repo under this chart's keyring path makes apt reject the whole source list (\"E: Conflicting values set for option Signed-By regarding source http://apt.postgresql.org/pub/repos/apt/ ...\"), so the install fails before it starts. PGDG packages in postgresql.extensions.packages resolve from the image's own configuration -- aptSources is only for sources the images do NOT ship, e.g. repo.pigsty.io" $name $substituted) -}}
    {{- end -}}
    {{- $expectSignedBy := printf "signed-by=/usr/share/keyrings/pgchart-%s-keyring.gpg" $name -}}
    {{- if not (contains $expectSignedBy $substituted) -}}
      {{- fail (printf "postgresql.extensions.aptSources[%s].aptLine: %q must include %q -- without it apt has no way to know which key to trust and this fails later, at apply time, inside apt-get update (NO_PUBKEY), instead of at render time; keyUrl's key is dearmored to exactly that path" $name $substituted $expectSignedBy) -}}
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
       exists for exactly that reason: these are never legitimate extraLibs entries.
       extraLibs' own copies land in a volume physically separate from ext-lib
       (statefulset.yaml, ext-extra-lib) specifically so this denylist is the ONLY gate
       on what ends up on LD_LIBRARY_PATH -- ext-lib is also populated by the
       unvalidated *.so* glob copy, which this validator has no visibility into.

       Denylist is `ldd /usr/lib/postgresql/<major>/bin/postgres` (verified live against
       the debian:trixie-based postgres/repmgr images this chart ships by default) plus
       libpq (the dependency of libpqwalreceiver.so, the exact #302 ABI hazard) -- i.e.
       the full set of libraries the postmaster itself resolves, not just the historical
       libc family (review: an earlier, narrower list missed libzstd/liblz4/libxml2/
       libpam/libgssapi_krb5/libnuma/libldap/liburing/libsystemd/libxxhash/liblzma/
       libaudit/libkrb5/libk5crypto/libcom_err/libkrb5support/liblber/libsasl2/libcap/
       libcap-ng/libkeyutils, every one of which the postmaster actually links). This is
       the *current* comprehensive set for the shipped Debian trixie builds, not an
       eternal guarantee -- a future base-image bump could add a new dependency this
       list doesn't yet know about. Also requires the path to look like a shared
       library (basename ends `.so` or `.so.<digits>[.<digits>...]`, review): an
       absolute path with no such suffix (e.g. a bare directory, or an unrelated file
       like /etc/passwd) would otherwise pass the character-class check and only fail
       at cp time inside the init container, crash-looping the pod instead of failing
       the render. */ -}}
{{- define "pg.validateExtraLibs" -}}
{{- $libs := .Values.postgresql.extensions.extraLibs | default list -}}
{{- if $libs -}}
  {{- if not .Values.postgresql.extensions.enabled -}}
    {{- fail "postgresql.extensions.extraLibs is set but postgresql.extensions.enabled is false, so nothing would ever copy them. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- /* #320: satisfied by EITHER path. extraLibs names absolute paths inside whichever
         init container does the copying, so it reads from the prebuilt extension image
         just as well as from the apt-installed filesystem -- which is exactly what lets a
         working values file move from packages to image with no other edit. Requiring
         packages here would have made that migration impossible. */ -}}
  {{- if and (not (.Values.postgresql.extensions.packages | default list)) (not ((.Values.postgresql.extensions.image | default dict).repository | default "")) -}}
    {{- fail "postgresql.extensions.extraLibs is set but neither postgresql.extensions.packages nor postgresql.extensions.image.repository is -- extraLibs' copy step runs alongside one of those two (the apt-get install, or the prebuilt-image copy), so it has nothing to do without one of them." -}}
  {{- end -}}
  {{- $pathRe := "^/[A-Za-z0-9._/+-]*[A-Za-z0-9._+-]$" -}}
  {{- $soRe := "\\.so(\\.[0-9]+)*$" -}}
  {{- $denyRe := "^(lib(c|m|pthread|dl|rt|resolv|nsl|gcc_s|stdc\\+\\+|ssl|crypto|crypt|z|icu[A-Za-z0-9]*|zstd|lz4|xml2|pam|gssapi_krb5|numa|ldap|uring|systemd|xxhash|lzma|audit|krb5support|krb5|k5crypto|com_err|lber|sasl2|cap|keyutils|pq)(\\.so|[.-])|ld-linux)" -}}
  {{- $seenBase := dict -}}
  {{- range $p := $libs -}}
    {{- $path := $p | toString -}}
    {{- if not (regexMatch $pathRe $path) -}}
      {{- fail (printf "postgresql.extensions.extraLibs: invalid path %q -- must be an absolute path to a FILE (no trailing /) using only letters, digits, and the characters . _ / + - (it is interpolated into a shell command); no .. traversal, whitespace, quotes, backticks, $(), ;, &, |, or newlines" $path) -}}
    {{- end -}}
    {{- if regexMatch "(^|/)\\.\\.(/|$)" $path -}}
      {{- fail (printf "postgresql.extensions.extraLibs: invalid path %q -- must not contain a .. path segment" $path) -}}
    {{- end -}}
    {{- $base := base $path -}}
    {{- if not (regexMatch $soRe $base) -}}
      {{- fail (printf "postgresql.extensions.extraLibs: invalid path %q -- must name a shared library (basename ending .so or .so.<digits>...); a directory or unrelated file would pass the character check but fail the cp at init time, crash-looping the pod instead of failing the render" $path) -}}
    {{- end -}}
    {{- if regexMatch $denyRe $base -}}
      {{- fail (printf "postgresql.extensions.extraLibs: refusing %q -- copying a library the postgres server itself links (the full glibc/OpenSSL/Kerberos/LDAP/ICU/compression/audit set postgres:*-trixie and cagriekin/repmgr resolve at runtime, not just libc) risks silently shadowing the postgresql container's own runtime with a build from a different image (copy-base-ext and copy-ext can be different images); this value is for extension-specific dependencies (e.g. libsodium.so.23), not libraries the server itself already depends on" $base) -}}
    {{- end -}}
    {{- if hasKey $seenBase $base -}}
      {{- fail (printf "postgresql.extensions.extraLibs: duplicate basename %q -- two different source paths would both copy to the same destination filename in ext-extra-lib, and which one wins depends on copy-base-ext/copy-ext ordering (undefined behavior); use distinct filenames or drop the duplicate" $base) -}}
    {{- end -}}
    {{- $seenBase = set $seenBase $base true -}}
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

{{- /* #320: the copy command for the prebuilt-extension init container. Deliberately a
       separate helper from pg.extensionInstallCommand rather than a flag on it: that one
       renders a five-to-eight-step apt pipeline (PGVER pin, key fetch, sources.list write,
       two apt-get updates, install) and this one renders three `cp`s. Folding them together
       would put the shell that runs as root and the shell that does not behind one
       conditional, which is precisely the code you do not want to have to re-read to
       convince yourself the unprivileged path stays unprivileged.

       Always -n (no-clobber): see the copy-prebuilt-ext comment in statefulset.yaml -- this
       container is LAST and must only ADD what copy-base-ext/copy-ext did not provide.
       extraLibs paths are copied from THIS image's filesystem, so the same absolute paths
       the apt path uses work unchanged -- which is what lets an operator switch from
       packages to image without touching anything else in values. */ -}}
{{- define "pg.extensionPrebuiltCopyCommand" -}}
{{- $pgMajor := .pgMajor | toString -}}
{{- $cmdParts := list -}}
{{- $cmdParts = append $cmdParts (printf "cp -n /usr/lib/postgresql/%s/lib/*.so* /ext-lib/" $pgMajor) -}}
{{- $cmdParts = append $cmdParts (printf "cp -n /usr/share/postgresql/%s/extension/* /ext-share/" $pgMajor) -}}
{{- range $l := (.extraLibs | default list) -}}
  {{- $cmdParts = append $cmdParts (printf "cp -n %s /ext-extra-lib/" $l) -}}
{{- end -}}
{{- join " && " $cmdParts -}}
{{- end }}

{{- /* #320: postgresql.extensions.env / envFrom / extraVolumes / extraVolumeMounts exist so
       an install under a default-deny egress policy can be pointed at an in-cell apt mirror
       or proxy instead of permanently allowing apt.postgresql.org, repo.pigsty.io and
       deb.debian.org per tenant. They are rendered on the copy-ext/copy-base-ext init
       containers only, and only while packages is non-empty -- so the same
       "set but nothing would ever use it" rejection aptSources gets applies here, for the
       same reason: silently inert configuration in a values file is worse than a render
       error, because the operator concludes the proxy is in effect when it is not.

       extraVolumes names are checked against the chart's OWN volume names. A collision is
       not a merge -- the later entry in the pod's volumes list wins, so reusing `data` or
       `ext-lib` would replace the data volume or the extension tree with a ConfigMap, and
       nothing would report it until the pod was running.

       extraVolumeMounts mountPaths are checked against the three paths the install step
       itself writes: mounting over one (a ConfigMap, or anything read-only) shadows the
       tree this whole feature exists to populate, and the copy would either fail or write
       into a mount nothing reads. Every mount must also name a volume declared in
       extraVolumes, because a mount referencing an absent volume fails at APPLY time (the
       kubelet rejects the pod) rather than at render time. */ -}}
{{- define "pg.validateExtensionInitOverrides" -}}
{{- $ext := .Values.postgresql.extensions -}}
{{- $img := $ext.image | default dict -}}
{{- $repo := $img.repository | default "" | toString -}}
{{- if $repo -}}
  {{- /* #320: the prebuilt path and the apt path are mutually exclusive. Both populate the
         same ext-lib/ext-share volumes, and with `cp -n` in both the winner would be decided
         by init-container ORDER -- i.e. by an implementation detail of this template rather
         than by anything in the values file. That is the "which one wins" question the
         wal_level guard (#308) exists to make unaskable, and the answer here would be worse:
         a version-pinned package silently losing to whatever the image happened to contain.
         extraLibs is deliberately NOT rejected -- it names absolute paths inside whichever
         container does the copying, so it reads from the prebuilt image unchanged, which is
         what lets a working values file move from packages to image with no other edit. */ -}}
  {{- if not $ext.enabled -}}
    {{- fail "postgresql.extensions.image.repository is set but postgresql.extensions.enabled is false, so the init container that would copy from it is never rendered. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- if ($ext.packages | default list) -}}
    {{- fail "postgresql.extensions.image.repository and postgresql.extensions.packages are both set, and they are mutually exclusive: both populate the same ext-lib/ext-share volumes with a no-clobber copy, so which build of an extension actually wins would be decided by init-container order rather than by anything in this values file -- a version-pinned package could silently lose to whatever the image happens to contain. Use the image (packages are resolved once at build time, no apt on the pod-start path) OR packages (installed on every pod start), not both." -}}
  {{- end -}}
  {{- if ($ext.aptSources | default list) -}}
    {{- fail "postgresql.extensions.image.repository and postgresql.extensions.aptSources are both set. aptSources only exists to make an apt-get install find non-PGDG packages, and the prebuilt-image path runs no apt at all -- the source belongs in the image build instead (see images/pg-extensions/, APT_SOURCE_* build args)." -}}
  {{- end -}}
  {{- if not ($img.tag | default "" | toString) -}}
    {{- if not ($img.digest | default "" | toString) -}}
      {{- fail "postgresql.extensions.image.repository is set but neither tag nor digest is. An untagged reference resolves to :latest, which for an extension image means the extension .so files can change under a pod restart without anything in this release changing -- and an extension built for the wrong major does not load at all. Set a tag (\"{major}-v1\" substitutes postgresql.majorVersion) or a digest." -}}
    {{- end -}}
  {{- end -}}
{{- else -}}
  {{- /* #320 review: the whole image block is gated on repository, so a tag or digest set
         without it renders NOTHING -- no prebuilt container, no error -- and an operator who
         typoed the repository key concludes the prebuilt path is active while the pods are
         still taking the plain-copy one. Same reasoning as the env/aptSources rejections
         above: silently inert configuration in a values file is worse than a render error. */ -}}
  {{- if or ($img.tag | default "" | toString) ($img.digest | default "" | toString) -}}
    {{- fail "postgresql.extensions.image.tag/digest is set but postgresql.extensions.image.repository is empty, so no prebuilt-extension container is rendered at all and the setting is silently ignored. Set image.repository, or remove the tag/digest." -}}
  {{- end -}}
{{- end -}}
{{- $env := $ext.env | default list -}}
{{- $envFrom := $ext.envFrom | default list -}}
{{- $vols := $ext.extraVolumes | default list -}}
{{- $mounts := $ext.extraVolumeMounts | default list -}}
{{- $any := or (gt (len $env) 0) (gt (len $envFrom) 0) (gt (len $vols) 0) (gt (len $mounts) 0) -}}
{{- if $any -}}
  {{- if not $ext.enabled -}}
    {{- fail "postgresql.extensions.env/envFrom/extraVolumes/extraVolumeMounts are set but postgresql.extensions.enabled is false, so the init containers they configure are never rendered and nothing would use them. Set postgresql.extensions.enabled=true." -}}
  {{- end -}}
  {{- if not ($ext.packages | default list) -}}
    {{- fail "postgresql.extensions.env/envFrom/extraVolumes/extraVolumeMounts are set but postgresql.extensions.packages is empty. They configure the apt-get step (a proxy, a mirror sources.list), and with no packages there is no apt-get step -- the init containers take the plain-copy path and your proxy/mount would be silently ignored. Add at least one package, or remove these values." -}}
  {{- end -}}
{{- end -}}
{{- $reserved := list "data" "pg-run" "postgresql-config" "postgresql-tls" "ext-lib" "ext-share" "ext-extra-lib" "repmgr-config" "etcd-tls" "agent-control-tls" "pgbackrest" "pgbackrest-config" "pgbackrest-bootstrap-script" "service-updater-script" -}}
{{- /* postgresql.extraVolumes lands in the SAME pod volumes list (#320 review), so a name
       shared with it is a duplicate the API server rejects ("volumes[n].name: Duplicate
       value") -- the same apply-time-only failure class as the chart-volume collision
       above, just from the other operator-controlled list. */ -}}
{{- range $ov := (.Values.postgresql.extraVolumes | default list) -}}
  {{- $reserved = append $reserved ($ov.name | toString) -}}
{{- end -}}
{{- $declared := dict -}}
{{- range $v := $vols -}}
  {{- $n := $v.name | toString -}}
  {{- if not $n -}}
    {{- fail "postgresql.extensions.extraVolumes: every entry needs a name (it is what extraVolumeMounts references)" -}}
  {{- end -}}
  {{- if has $n $reserved -}}
    {{- fail (printf "postgresql.extensions.extraVolumes: name %q is one of this chart's own volume names (%s). Volume names are not merged -- the later entry wins -- so this would REPLACE the chart's volume (e.g. the data PVC, or the extension tree) with yours, and nothing would report it until the pod was running. Pick a different name." $n (join ", " $reserved)) -}}
  {{- end -}}
  {{- if hasKey $declared $n -}}
    {{- fail (printf "postgresql.extensions.extraVolumes: duplicate name %q -- the later entry would silently replace the earlier one" $n) -}}
  {{- end -}}
  {{- $declared = set $declared $n true -}}
{{- end -}}
{{- /* Destinations AND sources (#320 review). Mounting over /usr/share/postgresql/<major>/
       extension shadows where apt-get INSTALLS and where the cp READS, so copy-ext dutifully
       copies the ConfigMap's contents into ext-share instead of the extensions -- a silently
       extension-less cluster, which is the exact failure this feature exists to prevent, and
       it surfaces only at CREATE EXTENSION. */ -}}
{{- $installPaths := list "/ext-lib" "/ext-share" "/ext-extra-lib" -}}
{{- $srcMajor := .Values.postgresql.majorVersion | default "" | toString -}}
{{- if $srcMajor -}}
  {{- $installPaths = append $installPaths (printf "/usr/lib/postgresql/%s/lib" $srcMajor) -}}
  {{- $installPaths = append $installPaths (printf "/usr/share/postgresql/%s/extension" $srcMajor) -}}
{{- end -}}
{{- /* With aptSources set, the install step also WRITES
       /etc/apt/sources.list.d/pgchart-<name>.list and
       /usr/share/keyrings/pgchart-<name>-keyring.gpg (#320 review). A ConfigMap mounted over
       either directory is read-only, so that `echo >` / `gpg -o` gets EROFS, the && chain
       aborts, and copy-ext crash-loops -- and the README invites exactly that mount ("a
       replacement sources.list pointing at an internal mirror"), so the combination is likely
       rather than exotic. Only added when aptSources is in play: mounting a sources.list is
       precisely the right move when it is NOT, which is the whole point of the feature. */ -}}
{{- if (.Values.postgresql.extensions.aptSources | default list) -}}
  {{- $installPaths = append $installPaths "/etc/apt/sources.list.d" -}}
  {{- $installPaths = append $installPaths "/usr/share/keyrings" -}}
{{- end -}}
{{- $seenPaths := dict -}}
{{- range $m := $mounts -}}
  {{- $n := $m.name | toString -}}
  {{- if not (hasKey $declared $n) -}}
    {{- fail (printf "postgresql.extensions.extraVolumeMounts references volume %q, which is not declared in postgresql.extensions.extraVolumes. A mount naming an absent volume is rejected by the kubelet at apply time, not by helm at render time, so the pod would simply never start." $n) -}}
  {{- end -}}
  {{- $path := $m.mountPath | toString | trimSuffix "/" -}}
  {{- if not $path -}}
    {{- fail (printf "postgresql.extensions.extraVolumeMounts[%s]: mountPath is required" $n) -}}
  {{- end -}}
  {{- /* BOTH directions, not equality (#320 review). /ext-share/extension shadows the
         extension tree just as completely as /ext-share does -- and so does its PARENT,
         /usr/share/postgresql/<major>: the copy then fails with `cannot stat` and the init
         container crash-loops. Compared with a trailing slash on both sides so
         /ext-libs-of-mine and /usr/share/postgresql-other are not caught. */ -}}
  {{- range $ip := $installPaths -}}
    {{- if or (eq $path $ip) (hasPrefix (printf "%s/" $ip) $path) (hasPrefix (printf "%s/" $path) $ip) -}}
      {{- fail (printf "postgresql.extensions.extraVolumeMounts[%s].mountPath is %q, which is at or inside %q -- where the install step copies the extension files it just built. Mounting over it shadows the tree this feature exists to populate: the copy writes into your volume (or fails outright, if it is read-only) and the postgresql container reads an empty one. Mount your apt configuration somewhere under /etc/apt instead." $n $path $ip) -}}
    {{- end -}}
  {{- end -}}
  {{- /* Duplicate mountPath is rejected by the API SERVER, not by helm
         ("volumeMounts[n].mountPath: Invalid value: ... must be unique"), so it is the
         same class of apply-time-only failure as the undeclared-volume check above. */ -}}
  {{- if hasKey $seenPaths $path -}}
    {{- fail (printf "postgresql.extensions.extraVolumeMounts: duplicate mountPath %q (volumes %q and %q). Kubernetes requires mountPaths to be unique within a container and rejects the pod at apply time, so helm has to catch it first." $path (get $seenPaths $path) $n) -}}
  {{- end -}}
  {{- $seenPaths = set $seenPaths $path $n -}}
{{- end -}}
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
{{- $chartVolumes := list "data" "postgresql-config" "postgresql-tls" "ext-lib" "ext-share" "ext-extra-lib" "repmgr-config" "etcd-tls" "pg-run" "pgbackrest-config" -}}
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
      "CASCADE_REPLICATION" "MECHANISM" "POSTGRESQL_PGHBA" "TLS_REQUIRE_SSL" "TLS_CLIENT_CERT_AUTH"
      "MONITORING_USER" "MIGRATE_LEGACY_MD5_USERS" "SPLIT_BRAIN_ACTION"
      "DCS_BACKEND" "ETCD_ENDPOINTS" "ETCD_PREFIX" "ETCD_TLS_CERT" "ETCD_TLS_KEY" "ETCD_TLS_CA"
      "PGBACKREST_ENABLED" "PGBACKREST_STANZA" "PGBACKREST_REPO1_CIPHER_PASS"
      "PGBACKREST_REPO1_S3_KEY" "PGBACKREST_REPO1_S3_KEY_SECRET" "LD_LIBRARY_PATH" -}}
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

{{- /* #323: pgbackrest.extraEnv / extraVolumes / extraVolumeMounts. Same shape and the same
       reasoning as pg.validateExtraPassthrough (#262), but a WIDER blast radius: one list
       feeds five containers across four pod templates (the pgbackrest sidecar and the
       pgbackrest-bootstrap init container in the StatefulSet pod, the backup CronJob, the
       restore Job, the validation CronJob). So every reserved-name list below is the UNION
       over all of them -- a name that only collides in one of the five still breaks that one
       pod, and an operator who set it once would have no reason to suspect the pod they were
       not thinking about. */ -}}
{{- define "pg.validatePgbackrestPassthrough" -}}
{{- $pb := .Values.pgbackrest -}}
{{- /* g1: shape. `with` is not used -- an explicitly-set map must reach the check. */ -}}
{{- range $key := list "extraVolumes" "extraVolumeMounts" "extraEnv" -}}
  {{- $val := index $pb $key -}}
  {{- if and $val (not (kindIs "slice" $val)) -}}
    {{- fail (printf "pgbackrest.%s must be a list of objects, got %s. Use a YAML sequence of entries, each with a name (e.g. `pgbackrest.%s: [{name: my-entry, ...}]`), not a map." $key (kindOf $val) $key) -}}
  {{- end -}}
{{- end -}}
{{- $vols := $pb.extraVolumes | default list -}}
{{- $mounts := $pb.extraVolumeMounts | default list -}}
{{- $env := $pb.extraEnv | default list -}}
{{- /* Silently inert configuration is worse than a render error (same call as #320's
       env/aptSources gate): with pgbackrest disabled none of the five containers exists, so
       a KUBECONFIG set here would look configured and do nothing -- and the symptom of that
       is a tenant with no backups and nothing wrong in its status, which is exactly what
       #323 reported. */ -}}
{{- if and (or (gt (len $vols) 0) (gt (len $mounts) 0) (gt (len $env) 0)) (not $pb.enabled) -}}
  {{- fail "pgbackrest.extraEnv/extraVolumes/extraVolumeMounts are set but pgbackrest.enabled is false, so none of the pgbackrest containers they configure is rendered and nothing would use them. Set pgbackrest.enabled=true, or remove these values." -}}
{{- end -}}
{{- /* Chart-managed volume names across all four pod templates. The StatefulSet's own list is
       included in full -- pgbackrest.extraVolumes lands in that pod too (the sidecar and the
       bootstrap init container live there), where a duplicate name is either rejected by the
       API server or, for `data`, silently discarded in favour of the volumeClaimTemplate. */ -}}
{{- $chartVolumes := list
      "data" "postgresql-config" "postgresql-tls" "ext-lib" "ext-share" "ext-extra-lib"
      "repmgr-config" "etcd-tls" "agent-control-tls" "pg-run" "pgbackrest-config"
      "pgbackrest-bootstrap-script" "service-updater-script"
      "tmp" "work" "restore-script" "validate-script" -}}
{{- /* The two other operator-controlled lists that reach the SAME pod volumes list. A name
       shared with either is a duplicate the API server rejects at apply time
       ("volumes[n].name: Duplicate value"), i.e. render-clean and pod-never-starts. */ -}}
{{- range $ov := (.Values.postgresql.extraVolumes | default list) -}}
  {{- $chartVolumes = append $chartVolumes ($ov.name | default "" | toString) -}}
{{- end -}}
{{- /* Gated on enabled (#323 review): the extensions volumes render inside
       `if .Values.postgresql.extensions.enabled`, so with extensions off a leftover entry in
       the values file is inert and a collision against it would be a render failure citing a
       duplicate that cannot occur. */ -}}
{{- if ((.Values.postgresql.extensions).enabled) -}}
{{- range $ov := ((.Values.postgresql.extensions).extraVolumes | default list) -}}
  {{- $chartVolumes = append $chartVolumes ($ov.name | default "" | toString) -}}
{{- end -}}
{{- end -}}
{{- $volNames := list -}}
{{- range $i, $v := $vols -}}
  {{- $n := ($v.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "pgbackrest.extraVolumes[%d]: name is required (it is what extraVolumeMounts references)" $i) -}}{{- end -}}
  {{- if has $n $chartVolumes -}}
    {{- fail (printf "pgbackrest.extraVolumes[%d]: %q is already a volume name in one of the pods these volumes are added to (reserved: %s). Volume names are not merged -- a duplicate is rejected by the API server, and a duplicate %q would replace the chart's own volume -- so the render fails here instead. Pick a distinct name." $i $n (join ", " $chartVolumes) $n) -}}
  {{- end -}}
  {{- if has $n $volNames -}}{{- fail (printf "pgbackrest.extraVolumes[%d]: duplicate volume name %q -- the later entry would silently replace the earlier one" $i $n) -}}{{- end -}}
  {{- $volNames = append $volNames $n -}}
{{- end -}}
{{- /* Mount destinations that would shadow something one of the five containers depends on.
       Rejected when the mountPath IS one of these or CONTAINS it: over /work or /tmp it
       replaces a writable emptyDir with a read-only projection (pgbackrest's log/lock paths
       and kubectl's HOME cache both live there, and the failure surfaces as EROFS mid-run),
       and over /var/run/postgresql it hides the socket directory the sidecar shares with the
       postmaster. Nesting INSIDE these three is fine and is the normal case -- they are
       emptyDirs, so the kubelet creates the sub-mountpoint (a kubeconfig at /tmp/kube is a
       perfectly good place for it). */ -}}
{{- $shadow := list "/var/run/postgresql" "/work" "/tmp" -}}
{{- /* These get the strictest rule -- at, above, OR inside -- because nesting under them is
       broken too (#323 review). /scripts is a READ-ONLY configMap volume carrying restore.sh /
       validate.sh / bootstrap.sh, so the kubelet cannot create a nested mountpoint in it and
       the pod sticks in CreateContainerError; /etc/pgbackrest/pgbackrest.conf is a subPath
       FILE mount, so nothing can live under it; and PGDATA is the live data directory these
       containers restore into and then read with pg_controldata, where a projection anywhere
       inside is data corruption rather than a failed mount. */ -}}
{{- $noNest := list "/var/lib/postgresql/data" "/scripts" "/etc/pgbackrest/pgbackrest.conf" -}}
{{- $seenPaths := dict -}}
{{- range $i, $m := $mounts -}}
  {{- $n := ($m.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "pgbackrest.extraVolumeMounts[%d]: name is required" $i) -}}{{- end -}}
  {{- $rawPath := ($m.mountPath | default "") | toString -}}
  {{- if not $rawPath -}}{{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath is required" $i $n) -}}{{- end -}}
  {{- /* Checked BEFORE the trimSuffix below, which would turn "/" into "" and answer the most
         destructive possible input with "mountPath is required" (#323 review). */ -}}
  {{- if eq $rawPath "/" -}}
    {{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath is \"/\", which would mount over the container's entire root filesystem -- every binary the pgbackrest containers run included. Mount a specific directory instead." $i $n) -}}
  {{- end -}}
  {{- /* Absolute and normalized BEFORE the shadow comparisons below, which are prefix matches
         and would otherwise be trivially bypassable (#323 review): "//var/lib/postgresql/data"
         and "/tmp/../var/lib/postgresql/data" both miss the PGDATA guard as written, and the
         runtime resolves both straight onto the live data directory. A relative destination is
         refused outright -- runc rejects a non-absolute OCI mount destination, so the pod
         sticks in CreateContainerError with nothing in the render to explain it. */ -}}
  {{- if not (hasPrefix "/" $rawPath) -}}
    {{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath %q is not absolute. A container mount destination must start with \"/\" -- the runtime rejects a relative one, so the pod would stay in CreateContainerError with nothing in the rendered manifest to explain it." $i $n $rawPath) -}}
  {{- end -}}
  {{- if regexMatch "(^|/)[.][.]?(/|$)" $rawPath -}}
    {{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath %q contains a \".\" or \"..\" path segment. The chart compares this path against the directories the pgbackrest containers depend on, and an unnormalized path walks around those checks while the runtime still resolves it to the real target (e.g. /tmp/../var/lib/postgresql/data is PGDATA). Write the destination out in full." $i $n $rawPath) -}}
  {{- end -}}
  {{- /* Collapse repeated separators for the same reason: "//var/lib/postgresql/data" is the
         data directory to the runtime and a non-match to a prefix test. */ -}}
  {{- $path := regexReplaceAll "/+" $rawPath "/" | trimSuffix "/" -}}
  {{- if not (has $n $volNames) -}}
    {{- fail (printf "pgbackrest.extraVolumeMounts[%d]: no pgbackrest.extraVolumes entry named %q (declared: %s). Every extra mount needs a matching extra volume -- otherwise the kubelet rejects the pod at apply time with `volumeMounts[..].name: Not found`, so the CronJob renders, fires, and never runs. Check for a typo, or that you set extraVolumes (plural). Chart-managed volumes cannot be mounted here." $i $n (ternary "none" (join ", " $volNames) (empty $volNames))) -}}
  {{- end -}}
  {{- range $gp := $noNest -}}
    {{- if or (eq $path $gp) (hasPrefix (printf "%s/" $path) $gp) (hasPrefix (printf "%s/" $gp) $path) -}}
      {{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath %q is at, above or inside %q, which the pgbackrest containers depend on. Nesting under it does not work either: %q is the live data directory (a projection inside it shadows part of PGDATA for the restore and for the postmaster that starts on it), /scripts is a read-only configMap volume the kubelet cannot create a sub-mountpoint in (the pod sticks in CreateContainerError), and pgbackrest.conf is a file. Mount a path of your own instead -- e.g. /etc/apiserver-proxy for a kubeconfig." $i $n $path $gp "/var/lib/postgresql/data") -}}
    {{- end -}}
  {{- end -}}
  {{- range $gp := $shadow -}}
    {{- if or (eq $path $gp) (hasPrefix (printf "%s/" $path) $gp) -}}
      {{- fail (printf "pgbackrest.extraVolumeMounts[%d] (%s): mountPath %q is at or above %q, a writable scratch volume the pgbackrest containers already use (pgbackrest's log/lock paths, kubectl's HOME cache, the postmaster socket directory). Replacing it with a projection fails at RUN time, not at apply time. Mount a path of your own, or nest INSIDE it -- %q/mine is fine." $i $n $path $gp $gp) -}}
    {{- end -}}
  {{- end -}}
  {{- /* Duplicate mountPath is rejected by the API SERVER, not by helm
         ("volumeMounts[n].mountPath: Invalid value: ... must be unique"). */ -}}
  {{- if hasKey $seenPaths $path -}}
    {{- fail (printf "pgbackrest.extraVolumeMounts: duplicate mountPath %q (volumes %q and %q). Kubernetes requires mountPaths to be unique within a container and rejects the pod at apply time, so helm has to catch it first." $path (get $seenPaths $path) $n) -}}
  {{- end -}}
  {{- $seenPaths = set $seenPaths $path $n -}}
{{- end -}}
{{- /* Env names the chart sets on at least one of the five containers. Reserved
       UNCONDITIONALLY -- including the ones only a currently-disabled feature emits
       (repoEncryption, s3.keyType=shared) -- so a passthrough that works today cannot start
       silently shadowing a chart value after a later `helm upgrade` enables that feature. */ -}}
{{- $chartEnv := list
      "NAMESPACE" "PRIMARY_SVC" "STANZA" "BACKUP_TYPE" "HOME"
      "PGBACKREST_STANZA" "PGDATA" "PGBACKREST_PG1_PATH"
      "PGBACKREST_LOG_PATH" "PGBACKREST_LOCK_PATH"
      "TARGET_TYPE" "TARGET" "BACKUP_SET" "FORCE" "RESTORE_REQUESTED_BY"
      "RECOVERY_TIMEOUT" "POSTGRES_USER" "POSTGRES_DB"
      "PGBACKREST_REPO1_CIPHER_PASS" "PGBACKREST_REPO1_S3_KEY" "PGBACKREST_REPO1_S3_KEY_SECRET" -}}
{{- $envNames := list -}}
{{- range $i, $e := $env -}}
  {{- $n := ($e.name | default "") | toString -}}
  {{- if not $n -}}{{- fail (printf "pgbackrest.extraEnv[%d]: name is required" $i) -}}{{- end -}}
  {{- if has $n $chartEnv -}}
    {{- fail (printf "pgbackrest.extraEnv[%d]: %q is set by the chart on one or more of the pgbackrest containers and may not be overridden -- a duplicate env name is last-wins at runtime, so this would silently shadow the chart/Secret value (e.g. pointing a restore at the wrong stanza, or a backup at the wrong repository credentials). Use the chart's own value for this setting instead." $i $n) -}}
  {{- end -}}
  {{- if has $n $envNames -}}{{- fail (printf "pgbackrest.extraEnv[%d]: duplicate env name %q" $i $n) -}}{{- end -}}
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
