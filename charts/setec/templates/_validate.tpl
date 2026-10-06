{{/*
Render-time validation hook. Calling the template emits no output but
triggers fail() if the values are inconsistent. Invoked from at least one
real manifest (validate.yaml) so render failures surface during
`helm install`, `helm upgrade`, and `helm template`.
*/}}
{{- define "setec.validate" -}}
{{- include "setec.validateEntropyReseed" . -}}
{{- include "setec.validateCredentials" . -}}
{{- include "setec.validateSnapshotS3" . -}}
{{- include "setec.validateSandboxClasses" . -}}
{{- end -}}

{{/*
SandboxClass egress-allowance validation (setec#76). Every
sandboxClasses.classes[].spec.egressAllowSelectors entry renders verbatim
into a cluster-scoped SandboxClass, and a malformed entry is refused by
the admission webhook at install time, when a post-install hook is the
only thing that sees the error. Check the shape at render time instead:
a peer names at least one selector, and lists at least one port, each
with a port value.
*/}}
{{- define "setec.validateSandboxClasses" -}}
{{- if .Values.sandboxClasses.enabled -}}
{{- range $class := .Values.sandboxClasses.classes -}}
{{- range $i, $a := ($class.spec.egressAllowSelectors | default list) -}}
{{- if and (not $a.namespaceSelector) (not $a.podSelector) -}}
{{- fail (printf "sandboxClasses.classes[%s].spec.egressAllowSelectors[%d] sets neither namespaceSelector nor podSelector; a peer with no selector selects nothing" $class.name $i) -}}
{{- end -}}
{{- if not $a.ports -}}
{{- fail (printf "sandboxClasses.classes[%s].spec.egressAllowSelectors[%d].ports is empty; an allowance never opens every port, list the ports it grants" $class.name $i) -}}
{{- end -}}
{{- range $j, $p := $a.ports -}}
{{- if not (hasKey $p "port") -}}
{{- fail (printf "sandboxClasses.classes[%s].spec.egressAllowSelectors[%d].ports[%d] has no port; give a number or a container port name" $class.name $i $j) -}}
{{- end -}}
{{- if and $p.protocol (not (has $p.protocol (list "TCP" "UDP" "SCTP"))) -}}
{{- fail (printf "sandboxClasses.classes[%s].spec.egressAllowSelectors[%d].ports[%d].protocol must be TCP, UDP or SCTP, got %q" $class.name $i $j $p.protocol) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Session-checkpoint S3 validation (setec#194, docs/design/storage.md). Every
snapshots.s3.* value is consumed by exactly one place — the node-agent
DaemonSet's argv, inside the snapshots.enabled guard. Turning s3 on
without those two switches therefore renders NOTHING and the operator
gets a FailedPrecondition ("s3 session-checkpoint backend is not
configured on this node") at the first suspend, hours later. Fail the
render instead.
*/}}
{{- define "setec.validateSnapshotS3" -}}
{{- $s3 := .Values.snapshots.s3 -}}
{{- if $s3.enabled -}}
{{- if not .Values.snapshots.enabled -}}
{{- fail "snapshots.s3.enabled=true requires snapshots.enabled=true: the s3 flags are only rendered inside the snapshots guard, so this combination would silently produce a node-agent with no checkpoint backend" -}}
{{- end -}}
{{- if not .Values.nodeAgent.enabled -}}
{{- fail "snapshots.s3.enabled=true requires nodeAgent.enabled=true: the s3 checkpoint backend lives in the node-agent DaemonSet, which is not rendered at all when nodeAgent.enabled=false" -}}
{{- end -}}
{{- if not $s3.bucket -}}
{{- fail "snapshots.s3.bucket is required when snapshots.s3.enabled=true" -}}
{{- end -}}
{{- if and (not $s3.endpoint) $s3.pathStyle -}}
{{- fail "snapshots.s3.pathStyle=true with an empty snapshots.s3.endpoint targets real AWS S3 with path-style addressing, which AWS has deprecated; set pathStyle=false for real S3 (keep it true only for MinIO and other self-hosted endpoints)" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Credential validation (setec#175). setec has one credential source: the
SPIFFE Workload API. It covers the frontend server, the node-agent
server, and the operator's node-agent dialer. An empty authorized-ID
list fails the render rather than deferring to the binary's startup
error, so the mistake is caught before it reaches a cluster.
*/}}
{{- define "setec.validateCredentials" -}}
{{- $s := .Values.credentials.spiffe -}}
{{- if not $s.socketPath -}}
{{- fail "credentials.spiffe.socketPath is required" -}}
{{- end -}}
{{- if not (hasPrefix "/" $s.socketPath) -}}
{{- fail (printf "credentials.spiffe.socketPath must be a bare absolute filesystem path (no unix:// prefix); got %q" $s.socketPath) -}}
{{- end -}}
{{- if and .Values.nodeAgent.enabled .Values.snapshots.enabled (not $s.authorizedIDs.nodeAgentClients) -}}
{{- fail "credentials.spiffe.authorizedIDs.nodeAgentClients must not be empty with nodeAgent.enabled=true and snapshots.enabled=true: the node-agent refuses an empty allow-list" -}}
{{- end -}}
{{- if and .Values.snapshots.enabled (not $s.authorizedIDs.nodeAgentServers) -}}
{{- fail "credentials.spiffe.authorizedIDs.nodeAgentServers must not be empty with snapshots.enabled=true: the operator's node-agent dialer refuses an empty allow-list" -}}
{{- end -}}
{{- end -}}
