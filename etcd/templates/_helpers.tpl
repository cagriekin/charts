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
{{- if not .repository -}}
{{- fail "etcd: an image block has an empty repository, which renders an unparseable reference (\":tag\"). Set the repository, or leave the whole image block at its chart default." -}}
{{- end -}}
{{- if and (not .tag) (not .digest) -}}
{{- fail (printf "etcd: image %q has neither a tag nor a digest, which would deploy an implicit :latest -- unpinned across pod restarts, so a future build silently replaces the one this release was tested with (for the etcd SERVER that is a new major landing on an existing member's data directory, which it refuses to start on; for rbac.bootstrapImage it is one agent build writing the etcd RBAC that another then authenticates against). Set image.tag, or image.digest alone (a digest is a complete pin on its own)." (.repository | toString)) -}}
{{- end -}}
{{- /* toString on the tag, because %s on a NON-STRING renders Go's error verb rather than
       the value (#298 review). The expression this helper replaced was `{{ with .tag }}:{{ . }}{{ end }}`,
       which prints any scalar; values.schema.json does not type image.tag, so an unquoted
       YAML scalar is reachable -- `--set image.tag=3` or `tag: 3.5` in a values file arrives
       as int64/float64 and rendered `quay.io/coreos/etcd:%!s(float64=3.5)`, an InvalidImageName
       at the kubelet: exactly the failure this helper exists to move to render time. The fail
       message above already does this to .repository; the reference itself has to as well. */ -}}
{{- if .tag -}}
{{- printf "%s:%s" (.repository | toString) (.tag | toString) -}}
{{- else -}}
{{- .repository -}}
{{- end -}}
{{- with .digest }}@{{ . }}{{- end -}}
{{- end -}}
