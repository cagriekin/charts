{{- define "etcd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- /* Release-scoped fullname, e.g. <release>-etcd. As a subchart .Release.Name is
       the parent release, so the bundled etcd is named <parent-release>-etcd and the
       parent points the agent at <parent-release>-etcd:2379. */ -}}
{{- define "etcd.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "etcd.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "etcd.labels" -}}
app.kubernetes.io/name: {{ include "etcd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: etcd
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "etcd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "etcd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: etcd
{{- end -}}

{{- /* http or https for the client URLs (tls.enabled) and the peer mesh
       (tls.enabled AND tls.peer.enabled). */ -}}
{{- define "etcd.clientScheme" -}}
{{- if .Values.tls.enabled -}}https{{- else -}}http{{- end -}}
{{- end -}}
{{- define "etcd.peerScheme" -}}
{{- if and .Values.tls.enabled .Values.tls.peer.enabled -}}https{{- else -}}http{{- end -}}
{{- end -}}

{{- /* Peer cert Secret, defaulting to the client/server Secret when unset. */ -}}
{{- define "etcd.peerSecret" -}}
{{- .Values.tls.peer.existingSecret | default .Values.tls.existingSecret -}}
{{- end -}}

{{- /* The static initial-cluster string: every member by its stable pod FQDN. etcd
       needs all peers listed for static bootstrap, and each member's --name must
       match its entry here (ETCD_NAME = the pod name). The peer scheme follows
       tls.peer (https when the mesh is encrypted). */ -}}
{{- define "etcd.initialCluster" -}}
{{- $full := include "etcd.fullname" . -}}
{{- $svc := printf "%s-headless" $full -}}
{{- $ns := .Release.Namespace -}}
{{- $domain := .Values.clusterDomain -}}
{{- $peer := .Values.peerPort | int -}}
{{- $scheme := include "etcd.peerScheme" . -}}
{{- $parts := list -}}
{{- range $i := until (int .Values.replicaCount) -}}
{{- $parts = append $parts (printf "%s-%d=%s://%s-%d.%s.%s.svc.%s:%d" $full $i $scheme $full $i $svc $ns $domain $peer) -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}

{{- /* Image reference for an image dict {repository, tag, digest?} -- in ONE place, because
       this chart has TWO of them and the digest-only fix had to be made twice (#298 review:
       "Fixing only the bootstrap Job's copy of this expression left the larger blast radius in
       place"). A third copy is how the next one gets missed.

       Both malformed shapes fail at RENDER time rather than at the kubelet, matching
       pg/templates/_helpers.tpl's pg.image, which refuses exactly these two:

         - an EMPTY REPOSITORY renders ":tag" or "@sha256:...", which is unparseable.

         - NEITHER TAG NOR DIGEST renders a bare repository, i.e. an implicit :latest. Omitting
           an empty tag is what makes a digest-only pin work, but it also turned
           `tag: "" digest: ""` from a loud kubelet InvalidImageName into a silent floating tag
           -- and values.yaml invites half of that ("Set to \"\" to pull by tag only" on the
           digest). For the DCS a floating tag means a future etcd MAJOR landing on an existing
           member's data directory, which it refuses to start on: all three pods down, no lease,
           the release comes up with no primary. Failing the render is the chart's contract for
           a dangerous input (see CLAUDE.md invariant 4). */ -}}
{{- define "etcd.image" -}}
{{- /* A non-string tag or digest is REFUSED, not coerced (#298 review). Two earlier rounds got
       this wrong in opposite directions: `printf "%s:%s"` rendered Go's error verb
       (`repo:%!s(float64=3.5)`), which at least failed loudly at the kubelet, and `| toString`
       then made it silent and WRONG -- Go renders a float in canonical form, so `tag: 18.0`
       becomes `repo:18` (a floating patch instead of the pin the operator wrote) and
       `tag: 2.10` becomes `repo:2.1`, a different tag that may well exist. A render-clean
       manifest deploying an image the values file never named is exactly the apply-time hazard
       invariant 4 says to catch at render time, so fail here and say how to fix it. */ -}}
{{- if and .tag (not (kindIs "string" .tag)) -}}
{{- fail (printf "etcd: image tag %v for repository %v is a %s, not a string: an image tag is text, and an unquoted YAML scalar is a number -- `tag: 18.0` arrives here as 18 and `tag: 2.10` as 2.1, so the value printed above may ALREADY have lost digits, and rendering it would deploy a different image or none at all. Quote the tag you actually meant in the values file (tag: \"...\"); on the command line shell quotes are not enough (helm types `--set x.tag=18` as a number regardless) -- use --set-string." .tag (.repository | toString) (kindOf .tag)) -}}
{{- end -}}
{{- if and .digest (not (kindIs "string" .digest)) -}}
{{- fail (printf "etcd: image digest %v for repository %v is a %s, not a string: quote it in the values file, or pass --set-string on the command line." .digest (.repository | toString) (kindOf .digest)) -}}
{{- end -}}

{{- if not .repository -}}
{{- fail "etcd: an image block has an empty repository, which renders an unparseable reference (\":tag\"). Set the repository, or leave the whole image block at its chart default." -}}
{{- end -}}
{{- if and (not .tag) (not .digest) -}}
{{- fail (printf "etcd: image %q has neither a tag nor a digest, which would deploy an implicit :latest -- unpinned across pod restarts, so a future build silently replaces the one this release was tested with (for the etcd SERVER that is a new major landing on an existing member's data directory, which it refuses to start on; for rbac.bootstrapImage it is one agent build writing the etcd RBAC that another then authenticates against). Set image.tag, or image.digest alone (a digest is a complete pin on its own)." (.repository | toString)) -}}
{{- end -}}
{{- /* toString on the REPOSITORY, and only there, exactly as pg.image does: the tag and the
       digest are already known to be strings (refused above), but %s on a non-string renders
       Go's error verb rather than the value, and the repository is the one half no guard types
       -- `%s` on it would emit `%!s(float64=3.5):tag`, a render-CLEAN manifest the kubelet
       then rejects as InvalidImageName. Coerced rather than refused because, unlike a tag, a
       numeric repository cannot silently name a DIFFERENT image that exists. */ -}}
{{- if .tag -}}
{{- printf "%s:%s" (.repository | toString) .tag -}}
{{- else -}}
{{- .repository | toString -}}
{{- end -}}
{{- with .digest }}@{{ . }}{{- end -}}
{{- end -}}
