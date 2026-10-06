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
{{- $td := dig "spire" "trustDomain" "" ($root.Values.global | default dict) -}}
{{- if not $td -}}
{{- $td = $root.Values.credentials.spiffe.trustDomain -}}
{{- end -}}
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
