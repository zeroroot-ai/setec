{{/*
Render-time validation hook. Calling the template emits no output but
triggers fail() if the values are inconsistent. Invoked from at least one
real manifest (runtimes-configmap.yaml) so render failures surface during
`helm install`, `helm upgrade`, and `helm template`.
*/}}
{{- define "setec.validate" -}}
{{- include "setec.validateRuntimes" . -}}
{{- include "setec.validateEntropyReseed" . -}}
{{- end -}}

{{/*
Installer values validation (ADR-0003). Fails the render when the
thin-pool configuration is inconsistent, so a misconfigured install
fails at helm time instead of leaving every node's installer NotReady.
*/}}
{{- define "setec.validateInstaller" -}}
{{- $tp := .Values.installer.thinpool -}}
{{- if not (has $tp.mode (list "loop" "device")) -}}
{{- fail (printf "installer.thinpool.mode must be \"loop\" or \"device\", got %q" $tp.mode) -}}
{{- end -}}
{{- if eq $tp.mode "device" -}}
{{- if or (not $tp.dataDevice) (not $tp.metadataDevice) -}}
{{- fail "installer.thinpool.mode=device requires both installer.thinpool.dataDevice and installer.thinpool.metadataDevice" -}}
{{- end -}}
{{- end -}}
{{- if eq $tp.mode "loop" -}}
{{- if or (le (int $tp.loopDataSizeGB) 0) (le (int $tp.loopMetaSizeGB) 0) -}}
{{- fail "installer.thinpool.loopDataSizeGB and loopMetaSizeGB must be positive" -}}
{{- end -}}
{{- end -}}
{{- end -}}
