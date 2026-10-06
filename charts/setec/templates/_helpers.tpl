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

