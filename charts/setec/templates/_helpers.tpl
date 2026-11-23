{{/*
Expand the name of the chart.
*/}}
{{- define "setec.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited.
If release name contains chart name it will be used as a full name.
*/}}
{{- define "setec.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "setec.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every rendered object.
*/}}
{{- define "setec.labels" -}}
helm.sh/chart: {{ include "setec.chart" . }}
{{ include "setec.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: setec
{{- end -}}

{{/*
Selector labels — the stable subset used on Deployments and Services.
*/}}
{{- define "setec.selectorLabels" -}}
app.kubernetes.io/name: {{ include "setec.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
setec.mergedNodeSelector renders one node selector from the fixed keys of a
component and the keys that an install adds. Call it with
(dict "fixed" <map> "user" <map>). A fixed key wins over the same key in
"user", so an install cannot clear the arch or the OS gate of a component
(docs/design/runtime.md).
*/}}
{{- define "setec.mergedNodeSelector" -}}
{{- toYaml (merge (dict) .fixed (.user | default dict)) -}}
{{- end -}}

{{/*
Name of the ServiceAccount the Deployment uses. If serviceAccount.create
is true and no explicit name is given, fall back to the full chart name.
*/}}
{{- define "setec.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "setec.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
setec.diskBuilderServiceAccountName is the ServiceAccount of the disk Job
Pods: the given name, else "<fullname>-disk-builder" when the chart creates it.
It is empty when the chart creates none and the install names none, so the
Pods use the default ServiceAccount of the namespace.
*/}}
{{- define "setec.diskBuilderServiceAccountName" -}}
{{- $sa := .Values.launcher.diskBuilder.serviceAccount -}}
{{- if $sa.name -}}
{{- $sa.name -}}
{{- else if $sa.create -}}
{{- printf "%s-disk-builder" (include "setec.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Validate snapshots.entropyReseed (setec#72): only "require" (fail-closed
active reseed on restore) and "off" (explicit opt-out, passive
virtio-rng only) are meaningful; anything else would silently change
the security posture.
*/}}
{{- define "setec.validateEntropyReseed" -}}
{{- $mode := .Values.snapshots.entropyReseed | default "require" -}}
{{- if not (has $mode (list "require" "off")) -}}
{{- fail (printf "snapshots validation: snapshots.entropyReseed=%q is invalid; use \"require\" (fail-closed active reseed on restore, default) or \"off\" (explicit opt-out, passive virtio-rng only)." $mode) -}}
{{- end -}}
{{- end -}}

{{/*
setec.trustDomain is the SPIFFE trust domain of this install (ADR-0164):
global.spire.trustDomain when a parent chart sets it, else
credentials.spiffe.trustDomain. It is empty when neither is set.
*/}}
{{- define "setec.trustDomain" -}}
{{- $td := dig "spire" "trustDomain" "" (.Values.global | default dict) -}}
{{- if not $td -}}
{{- $td = .Values.credentials.spiffe.trustDomain -}}
{{- end -}}
{{- $td -}}
{{- end -}}

{{/*
setec.clientSPIFFEID is the SPIFFE ID of one enrolled frontend client
(setec#233). Call it with (dict "client" <entry> "root" $).

An entry names its ID in one of two ways:

  - spiffeID: the full ID, for a client whose trust domain this chart
    cannot know, such as a cluster in a federated domain.
  - spiffePath: the path only. The chart builds
    spiffe://<trust domain>/<path>. The trust domain is
    global.spire.trustDomain when a parent chart sets it, else
    credentials.spiffe.trustDomain. So a parent chart gives a client that
    follows the trust domain of its install, with no literal (ADR-0164).

An entry with both, or with neither, fails the render.
*/}}
{{- define "setec.clientSPIFFEID" -}}
{{- $c := .client -}}
{{- $root := .root -}}
{{- if and $c.spiffeID $c.spiffePath -}}
{{- fail (printf "frontend.clients entry %q: set spiffeID or spiffePath, not both" $c.name) -}}
{{- else if $c.spiffeID -}}
{{- if not (hasPrefix "spiffe://" $c.spiffeID) -}}
{{- fail (printf "frontend.clients entry %q: spiffeID must start with spiffe://, got %q" $c.name $c.spiffeID) -}}
{{- end -}}
{{- $c.spiffeID -}}
{{- else if $c.spiffePath -}}
{{- $path := trimPrefix "/" $c.spiffePath -}}
{{- if or (not $path) (contains "://" $path) (regexMatch "\\s" $path) -}}
{{- fail (printf "frontend.clients entry %q: spiffePath must be a path such as platform/daemon, got %q" $c.name $c.spiffePath) -}}
{{- end -}}
{{- $td := include "setec.trustDomain" $root -}}
{{- if not $td -}}
{{- fail (printf "frontend.clients entry %q: spiffePath needs a trust domain: set global.spire.trustDomain or credentials.spiffe.trustDomain" $c.name) -}}
{{- end -}}
{{- if or (contains "/" $td) (contains ":" $td) -}}
{{- fail (printf "frontend.clients entry %q: the trust domain must be a bare name such as example.org, got %q" $c.name $td) -}}
{{- end -}}
{{- printf "spiffe://%s/%s" $td $path -}}
{{- else -}}
{{- fail (printf "frontend.clients entry %q: set spiffeID or spiffePath" $c.name) -}}
{{- end -}}
{{- end -}}

{{/*
setec.devicePluginHostDir returns the directory on the node that holds the
socket of the kubelet and the sockets of the device plugins. It is
devicePlugin.kubeletDir and /device-plugins. The kubelet root directory is
/var/lib/kubelet by default. A distribution may move it. The path inside the
container stays /var/lib/kubelet/device-plugins, so the plugin needs no flag.
*/}}
{{- define "setec.devicePluginHostDir" -}}
{{- $dir := (.Values.devicePlugin.kubeletDir | default "/var/lib/kubelet") | toString | trimSuffix "/" -}}
{{- if not (regexMatch "^/[^[:space:]]+$" $dir) -}}
{{- fail (printf "devicePlugin.kubeletDir=%q is not valid. Give the absolute path of the kubelet root directory on the node, such as /var/lib/kubelet. The chart adds /device-plugins." $dir) -}}
{{- end -}}
{{- printf "%s/device-plugins" $dir -}}
{{- end -}}
