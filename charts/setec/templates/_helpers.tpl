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
Compute the effective RuntimeClass name for the kata-fc backend. When the
legacy .Values.runtimeClassName is set and the user hasn't explicitly
overridden .Values.runtimes.kata-fc.runtimeClassName away from the chart
default ("kata-fc"), carry the legacy value through. This preserves the
pre-runtime-backends behaviour of `--set runtimeClassName=foo` as a one-line
override.
*/}}
{{- define "setec.katafc.runtimeClassName" -}}
{{- $legacy := .Values.runtimeClassName | default "" -}}
{{- $new := "" -}}
{{- if and .Values.runtimes (hasKey .Values.runtimes "kata-fc") -}}
  {{- $new = (index .Values.runtimes "kata-fc").runtimeClassName | default "" -}}
{{- end -}}
{{- if and $legacy (or (eq $new "") (eq $new "kata-fc")) -}}
{{- $legacy -}}
{{- else -}}
{{- $new | default "kata-fc" -}}
{{- end -}}
{{- end -}}

{{/*
Validate the runtimes block. Fails pre-render with a clear operator-facing
message when no backend is enabled (REQ-4.5) or when defaults.runtime.backend
is not enabled. Called once from templates/_validate.tpl so render fails
pre-install/upgrade.
*/}}
{{- define "setec.validateRuntimes" -}}
{{- $enabledCount := 0 -}}
{{- $enabledList := list -}}
{{- range $name, $cfg := .Values.runtimes -}}
  {{- if $cfg.enabled -}}
    {{- $enabledCount = add $enabledCount 1 -}}
    {{- $enabledList = append $enabledList $name -}}
  {{- end -}}
{{- end -}}
{{- if eq $enabledCount 0 -}}
{{- fail "runtimes validation (REQ-4.5): At least one runtime must be enabled. Set runtimes.<backend>.enabled=true for at least one of kata-fc, kata-qemu, gvisor, or runc." -}}
{{- end -}}
{{- $defaultBackend := .Values.defaults.runtime.backend -}}
{{- if not (has $defaultBackend $enabledList) -}}
{{- fail (printf "runtimes validation: defaults.runtime.backend=%q is not enabled. Enable runtimes.%s.enabled=true or pick a different default from: %s" $defaultBackend $defaultBackend (join ", " $enabledList)) -}}
{{- end -}}
{{- range $i, $fb := .Values.defaults.runtime.fallback -}}
  {{- if not (has $fb $enabledList) -}}
  {{- fail (printf "runtimes validation: defaults.runtime.fallback[%d]=%q is not enabled. Enable runtimes.%s.enabled=true or remove it from the fallback list." $i $fb $fb) -}}
  {{- end -}}
{{- end -}}
{{- /* devOnly gate on the cluster defaults (GHSA-q7hq-f8hm-wmjr).

The admission webhook gates a devOnly backend behind a namespace consent
label, but it only ever inspects SandboxClass objects — it never sees this
block. The cluster defaults apply to Sandboxes in every namespace, and no
single namespace's label would be the right consent to ask for, so the
rule is absolute: a devOnly backend may not be a cluster default.

The operator enforces the same rule at startup (internal/runtime/config.go
Validate). Repeating it here turns a boot crash-loop into a render error
that names the value, which is the difference between debugging a
CrashLoopBackOff and reading one sentence. */ -}}
{{- if (get .Values.runtimes $defaultBackend).devOnly -}}
{{- fail (printf "runtimes validation: defaults.runtime.backend=%q is marked devOnly and cannot be the cluster default. The cluster default applies to Sandboxes in EVERY namespace, and the devOnly consent gate is a per-namespace label the admission webhook only checks on SandboxClass objects — it never sees defaults.runtime. Pick a backend with stronger isolation, or set runtimes.%s.devOnly=false to state cluster-wide that its isolation is acceptable." $defaultBackend $defaultBackend) -}}
{{- end -}}
{{- range $i, $fb := .Values.defaults.runtime.fallback -}}
  {{- if (get $.Values.runtimes $fb).devOnly -}}
  {{- fail (printf "runtimes validation: defaults.runtime.fallback[%d]=%q is marked devOnly and cannot be a cluster-default fallback. A fallback is reached automatically when the primary backend has no capable node, in any namespace, with no gate in the path. Remove it, or set runtimes.%s.devOnly=false to state cluster-wide that its isolation is acceptable." $i $fb $fb) -}}
  {{- end -}}
{{- end -}}
{{- $mode := .Values.defaults.runtime.nodeCapabilitiesMode | default "probe" -}}
{{- if ne $mode "probe" -}}
{{- fail (printf "runtimes validation: defaults.runtime.nodeCapabilitiesMode=%q is invalid; \"probe\" is the only supported mode (the never-implemented \"static\" mode was removed in #79, and the operator rejects it at config load)." $mode) -}}
{{- end -}}
{{- end -}}

{{/*
Render the cleaned-up runtime configuration consumed by the operator and
node-agent. Matches the Go struct shape in internal/runtime/config.go.
Applies the legacy runtimeClassName -> runtimes.kata-fc.runtimeClassName
translation so `--set runtimeClassName=foo` keeps working for one release.
*/}}
{{- define "setec.runtimesConfig" -}}
{{- $katafcRC := include "setec.katafc.runtimeClassName" . -}}
runtimes:
{{- range $name, $cfg := .Values.runtimes }}
  {{ $name }}:
    enabled: {{ $cfg.enabled | default false }}
    runtimeClassName: {{ if eq $name "kata-fc" }}{{ $katafcRC | quote }}{{ else }}{{ $cfg.runtimeClassName | default $name | quote }}{{ end }}
    {{- /* install defaults true; hasKey so an explicit false is honoured (Helm's `default` treats false as empty). */}}
    install: {{ if hasKey $cfg "install" }}{{ $cfg.install }}{{ else }}true{{ end }}
    {{- if $cfg.devOnly }}
    devOnly: true
    {{- end }}
    {{- if $cfg.defaultOverhead }}
    defaultOverhead:
      {{- toYaml $cfg.defaultOverhead | nindent 6 }}
    {{- end }}
{{- end }}
defaults:
  runtime:
    backend: {{ .Values.defaults.runtime.backend | quote }}
    {{- with .Values.defaults.runtime.fallback }}
    fallback:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    probeInterval: {{ .Values.defaults.runtime.probeInterval | default "5m" | quote }}
    nodeCapabilitiesMode: {{ .Values.defaults.runtime.nodeCapabilitiesMode | default "probe" | quote }}
{{- end -}}

{{/*
Map a backend name to its containerd runtime handler.
*/}}
{{- define "setec.runtimeHandler" -}}
{{- $name := . -}}
{{- if eq $name "kata-fc" -}}kata-fc
{{- else if eq $name "kata-qemu" -}}kata-qemu
{{- else if eq $name "gvisor" -}}runsc
{{- else if eq $name "runc" -}}runc
{{- else -}}{{ $name }}
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
Validate snapshots.restoreUniquify (ADR-0005 invariant 2, setec#189):
only "require" (fail-closed per-restore identity uniquification) and
"off" (explicit opt-out) are meaningful; anything else would silently
change the security posture.
*/}}
{{- define "setec.validateRestoreUniquify" -}}
{{- $mode := .Values.snapshots.restoreUniquify | default "require" -}}
{{- if not (has $mode (list "require" "off")) -}}
{{- fail (printf "snapshots validation: snapshots.restoreUniquify=%q is invalid; use \"require\" (fail-closed per-restore identity uniquification, default) or \"off\" (explicit opt-out)." $mode) -}}
{{- end -}}
{{- end -}}
