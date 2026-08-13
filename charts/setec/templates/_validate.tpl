{{/*
Render-time validation hook. Calling the template emits no output but
triggers fail() if the values are inconsistent. Invoked from at least one
real manifest (runtimes-configmap.yaml) so render failures surface during
`helm install`, `helm upgrade`, and `helm template`.
*/}}
{{- define "setec.validate" -}}
{{- include "setec.validateRuntimes" . -}}
{{- include "setec.validateEntropyReseed" . -}}
{{- include "setec.validateRestoreUniquify" . -}}
{{- include "setec.validateCredentials" . -}}
{{- end -}}

{{/*
Credential-mode validation (setec#183). The mode is install-wide: it
covers the frontend server, the node-agent server, and the operator's
node-agent dialer together, so a mixed file/SPIFFE posture is not
reachable from a values file. In spiffe mode an empty authorized-ID
list fails the render rather than deferring to the binary's startup
error, so the mistake is caught before it reaches a cluster.
*/}}
{{- define "setec.validateCredentials" -}}
{{- $c := .Values.credentials -}}
{{- if not (has $c.mode (list "file" "spiffe")) -}}
{{- fail (printf "credentials.mode must be \"file\" or \"spiffe\", got %q" $c.mode) -}}
{{- end -}}
{{- if eq $c.mode "spiffe" -}}
{{- $s := $c.spiffe -}}
{{- if not $s.socketPath -}}
{{- fail "credentials.spiffe.socketPath is required in spiffe mode" -}}
{{- end -}}
{{- if not (hasPrefix "/" $s.socketPath) -}}
{{- fail (printf "credentials.spiffe.socketPath must be a bare absolute filesystem path (no unix:// prefix); got %q" $s.socketPath) -}}
{{- end -}}
{{- if and .Values.frontend.enabled (not $s.authorizedIDs.frontendClients) -}}
{{- fail "credentials.spiffe.authorizedIDs.frontendClients must not be empty in spiffe mode with frontend.enabled=true: an empty allow-list would be a startup error, and \"accept everyone\" is deliberately unreachable" -}}
{{- end -}}
{{- if and .Values.nodeAgent.enabled .Values.snapshots.enabled (not $s.authorizedIDs.nodeAgentClients) -}}
{{- fail "credentials.spiffe.authorizedIDs.nodeAgentClients must not be empty in spiffe mode with nodeAgent.enabled=true and snapshots.enabled=true: the node-agent refuses an empty allow-list" -}}
{{- end -}}
{{- if and .Values.snapshots.enabled (not $s.authorizedIDs.nodeAgentServers) -}}
{{- fail "credentials.spiffe.authorizedIDs.nodeAgentServers must not be empty in spiffe mode with snapshots.enabled=true: the operator's node-agent dialer refuses an empty allow-list" -}}
{{- end -}}
{{- if and .Values.snapshots.enabled .Values.snapshots.mTLS.certManager.enabled -}}
{{- fail "snapshots.mTLS.certManager.enabled has no effect in credentials.mode=spiffe (identities come from the Workload API, not cert-manager); disable it rather than carrying unused Certificate objects" -}}
{{- end -}}
{{- end -}}
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
