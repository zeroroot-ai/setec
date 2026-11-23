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
{{- include "setec.validateOperatorSelector" . -}}
{{- $_ := include "setec.devicePluginHostDir" . -}}
{{- include "setec.validateSystemPolicy" . -}}
{{- include "setec.validateDiskBuilderPull" . -}}
{{- include "setec.validateDiskBuilderPush" . -}}
{{- end -}}

{{/*
Operator Deployment selector (setec#383). Setec 0.130 and older had a selector with
the name and instance labels only, so it also matched the Pods of the device
plugin and the node agent. The selector now holds the component label too.
A Deployment selector is immutable, so an upgrade from such a release stops
with an API error that does not say what to do. When the live Deployment
still has the old selector, stop the render and give the step. lookup reads
the cluster during helm install and helm upgrade. It returns nothing during
helm template, so a template render is not affected.
*/}}
{{- define "setec.validateOperatorSelector" -}}
{{- $live := lookup "apps/v1" "Deployment" .Values.namespace (include "setec.fullname" .) -}}
{{- if $live -}}
{{- $sel := (($live.spec).selector).matchLabels | default dict -}}
{{- if not (hasKey $sel "app.kubernetes.io/component") -}}
{{- fail (printf "the Deployment %s/%s has the selector of Setec 0.130 or older, which also matches the device plugin and node agent Pods. A selector cannot change, so delete the Deployment without its Pods, then run this upgrade again: kubectl -n %s delete deployment %s --cascade=orphan. The operator Pods keep running, and the new Deployment adopts them. See the section \"Upgrading from 0.130\" of the chart README." .Values.namespace (include "setec.fullname" .) .Values.namespace (include "setec.fullname" .)) -}}
{{- end -}}
{{- end -}}
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

{{/*
Disk builder pull access (setec#379). A provider name must be one that the
builder knows, and a role needs the provider that uses it, so a typo fails the
render and not the first disk build.
*/}}
{{- define "setec.validateDiskBuilderPull" -}}
{{- $pull := .Values.launcher.diskBuilder.pull -}}
{{- range $p := $pull.providers -}}
{{- if not (has $p (list "ecr")) -}}
{{- fail (printf "launcher.diskBuilder.pull.providers: %q is not a known provider. Use \"ecr\"." $p) -}}
{{- end -}}
{{- end -}}
{{- if and $pull.aws.roleArn (not (has "ecr" $pull.providers)) -}}
{{- fail "launcher.diskBuilder.pull.aws.roleArn needs launcher.diskBuilder.pull.providers to hold \"ecr\": the provider uses the role." -}}
{{- end -}}
{{- if and $pull.aws.roleArn (not (hasPrefix "arn:" $pull.aws.roleArn)) -}}
{{- fail (printf "launcher.diskBuilder.pull.aws.roleArn must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q" $pull.aws.roleArn) -}}
{{- end -}}
{{- if and $pull.aws.roleArn (not (include "setec.diskBuilderServiceAccountName" .)) -}}
{{- fail "launcher.diskBuilder.pull.aws.roleArn needs a named ServiceAccount for the disk Jobs: the token that the role trusts belongs to it. Set launcher.diskBuilder.serviceAccount.create=true or give it a name." -}}
{{- end -}}
{{- end -}}

{{/*
Disk builder push access (setec#393). The same rules as the flags of the
operator (controller.ValidatePushIdentity), so a mistake fails the render and
not the first disk build. The provider ecr needs a web identity role, because
the identity of the Pod or of the node reaches the build container too. The
audience of the push token differs from the audience of the pull token,
because both tokens belong to one ServiceAccount.
*/}}
{{- define "setec.validateDiskBuilderPush" -}}
{{- $push := .Values.launcher.diskBuilder.push -}}
{{- range $p := $push.providers -}}
{{- if not (has $p (list "ecr")) -}}
{{- fail (printf "launcher.diskBuilder.push.providers: %q is not a known provider. Use \"ecr\"." $p) -}}
{{- end -}}
{{- end -}}
{{- if and $push.aws.roleArn (not (has "ecr" $push.providers)) -}}
{{- fail "launcher.diskBuilder.push.aws.roleArn needs launcher.diskBuilder.push.providers to hold \"ecr\": the provider uses the role." -}}
{{- end -}}
{{- if and (has "ecr" $push.providers) (not $push.aws.roleArn) -}}
{{- fail "launcher.diskBuilder.push.providers holds \"ecr\", so launcher.diskBuilder.push.aws.roleArn is required. The identity of the Pod or of the node reaches the build container too. Only a web identity token, which only the check and publish containers hold, keeps the push right out of the build container." -}}
{{- end -}}
{{- if and $push.aws.roleArn (not (hasPrefix "arn:" $push.aws.roleArn)) -}}
{{- fail (printf "launcher.diskBuilder.push.aws.roleArn must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q" $push.aws.roleArn) -}}
{{- end -}}
{{- if and $push.aws.roleArn (not (include "setec.diskBuilderServiceAccountName" .)) -}}
{{- fail "launcher.diskBuilder.push.aws.roleArn needs a named ServiceAccount for the disk Jobs: the token that the role trusts belongs to it. Set launcher.diskBuilder.serviceAccount.create=true or give it a name." -}}
{{- end -}}
{{- if and $push.aws.roleArn (or (not $push.aws.audience) (eq $push.aws.audience "sts.amazonaws.com")) -}}
{{- fail "launcher.diskBuilder.push.aws.audience must name an audience other than sts.amazonaws.com, the audience of the pull token. With the same audience the build container could assume the push role." -}}
{{- end -}}
{{- if and (has "ecr" $push.providers) (not .Values.launcher.diskRepo) -}}
{{- fail "launcher.diskBuilder.push.providers needs launcher.diskRepo: it names the repository that the Job pushes to." -}}
{{- end -}}
{{- end -}}

{{/*
setec.pushHosts returns, as a JSON list, the hosts that the disk Job reaches to
push, besides the disk registry: launcher.diskBuilder.push.hosts, and with the
provider ecr and a disk repository in an ECR registry, the ECR API and the STS
endpoint of its region. The cilium engine allows them.
*/}}
{{- define "setec.pushHosts" -}}
{{- $push := .Values.launcher.diskBuilder.push -}}
{{- $hosts := $push.hosts | default list -}}
{{- if and (has "ecr" $push.providers) .Values.launcher.diskRepo -}}
{{- $registry := (include "setec.registryAddress" .Values.launcher.diskRepo | fromJson).host -}}
{{- $re := "^[0-9]{12}\\.dkr\\.ecr\\.([a-z0-9-]+)\\.(amazonaws\\.com(?:\\.cn)?)$" -}}
{{- if regexMatch $re $registry -}}
{{- $region := regexReplaceAll $re $registry "${1}" -}}
{{- $domain := regexReplaceAll $re $registry "${2}" -}}
{{- $hosts = concat $hosts (list (printf "api.ecr.%s.%s" $region $domain) (printf "sts.%s.%s" $region $domain)) -}}
{{- end -}}
{{- end -}}
{{- toJson (uniq $hosts) -}}
{{- end -}}
