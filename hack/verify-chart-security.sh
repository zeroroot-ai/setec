#!/usr/bin/env bash
# Copyright 2026 The Setec Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Renders the chart and asserts that the containment controls it is
# supposed to ship are actually present in the output.
#
# `helm lint` and a bare `helm template >/dev/null` only prove the
# templates parse. Neither notices a control that silently stopped
# rendering — an `if` whose value was renamed, a template file deleted, a
# binding that lost its namespace selector. Those are exactly the
# regressions that matter here, because the failure mode is a cluster that
# installs cleanly and enforces nothing.
#
# Usage: hack/verify-chart-security.sh [chart-dir]

set -euo pipefail

CHART_DIR="${1:-charts/setec}"
HELM="${HELM:-helm}"
# The default systemPolicy.engine is cilium, and the chart refuses to render
# Cilium objects when the cluster has no Cilium CRDs. helm template reads no
# cluster, so it gets the kinds from these flags.
CILIUM_API=(--api-versions cilium.io/v2/CiliumNetworkPolicy
	--api-versions cilium.io/v2/CiliumClusterwideNetworkPolicy)
# The launcher values that the chart requires (hack/chart-launcher-values.yaml).
LAUNCHER_VALUES="$(dirname "$0")/chart-launcher-values.yaml"
NS_A="sandbox-workloads"
NS_B="sandbox-tenants"

fail_count=0

note() { printf '  %s\n' "$*"; }
pass() { printf '  ok   %s\n' "$*"; }
fail() {
	printf '  FAIL %s\n' "$*" >&2
	fail_count=$((fail_count + 1))
}

# assert_contains <haystack-file> <description> <fixed-string...>
# Every fixed string must appear in the rendered output.
assert_contains() {
	local file="$1" desc="$2"
	shift 2
	local needle
	for needle in "$@"; do
		if ! grep -qF -- "$needle" "$file"; then
			fail "$desc — missing: $needle"
			return
		fi
	done
	pass "$desc"
}

# assert_absent <haystack-file> <description> <fixed-string>
assert_absent() {
	local file="$1" desc="$2" needle="$3"
	if grep -qF -- "$needle" "$file"; then
		fail "$desc — unexpectedly present: $needle"
		return
	fi
	pass "$desc"
}

render() {
	local out="$1"
	shift
	"$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
		--set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={${NS_A},${NS_B}}" \
		"$@" >"$out"
}

# strip_comments <in> <out> — drop whole-line YAML comments.
#
# A template's own rationale is rendered into the output, so a fixed-string
# assertion can be satisfied by the comment that EXPLAINS a rule rather than
# the rule itself. That is not hypothetical: asserting "leases" against the
# leader-election Role passed even with the rule changed to `configmaps`,
# because the template comment quotes "leases.coordination.k8s.io is
# forbidden". Assert structural facts against the stripped copy.
strip_comments() {
	sed 's/[[:space:]]*#.*$//' "$1" >"$2"
}

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

printf 'verify-chart-security: rendering %s\n' "$CHART_DIR"

# ---------------------------------------------------------------------------
# Sandbox-namespace host guard (setec#159).
#
# The namespace default-deny NetworkPolicy does not reach a hostNetwork
# Pod. This policy is what closes that, so its absence is a silent loss of
# containment, not a cosmetic diff.
# ---------------------------------------------------------------------------
render "$workdir/default.yaml"
# --show-only isolates the guard's own documents, so an assertion cannot be
# satisfied by an identical string somewhere else in the release (the
# webhook configuration also carries failurePolicy: Fail, for instance).
render "$workdir/guard.yaml" --show-only templates/sandbox-namespace-host-guard.yaml

note "sandbox host guard (setec#159)"
assert_contains "$workdir/guard.yaml" "ValidatingAdmissionPolicy is rendered" \
	"kind: ValidatingAdmissionPolicy" \
	"name: setec-sandbox-host-guard"
assert_contains "$workdir/guard.yaml" "binding denies rather than audits" \
	"kind: ValidatingAdmissionPolicyBinding" \
	"policyName: setec-sandbox-host-guard" \
	"- Deny"
assert_contains "$workdir/guard.yaml" "policy fails closed" \
	"failurePolicy: Fail"
assert_contains "$workdir/guard.yaml" "every host-access field is denied" \
	"object.spec.hostNetwork == false" \
	"object.spec.hostPID == false" \
	"object.spec.hostIPC == false" \
	"!has(v.hostPath)" \
	"p.hostPort == 0" \
	"c.securityContext.privileged == false"
assert_contains "$workdir/guard.yaml" "binding is scoped to the sandbox namespaces" \
	"key: kubernetes.io/metadata.name" \
	"- \"${NS_A}\"" \
	"- \"${NS_B}\""

# The guard is a values toggle, so prove the toggle is wired to the object
# and not to a stale key that renders it unconditionally.
render "$workdir/guard-off.yaml" --set sandboxHostGuard.enabled=false
assert_absent "$workdir/guard-off.yaml" "guard is omitted when disabled" \
	"setec-sandbox-host-guard"

# ---------------------------------------------------------------------------
# Namespace baseline default-deny NetworkPolicy (setec#157).
#
# Already shipped; asserted here so the host guard and the policy it
# backstops are covered by one gate.
# ---------------------------------------------------------------------------
note "namespace baseline default-deny (setec#157)"
assert_contains "$workdir/default.yaml" "baseline policy selects every Pod" \
	"name: setec-sandbox-baseline-deny" \
	"podSelector: {}"

# ---------------------------------------------------------------------------
# Operator event RBAC.
#
# controller-runtime records events through the events.k8s.io API group. A
# ClusterRole that grants only the core-group `events` resource installs
# cleanly and then rejects every event the operator emits, which is how the
# chain-6 exit test lost its diagnostics for two days. Assert the rule on the
# comment-stripped copy: the template's own comment names the API group.
# ---------------------------------------------------------------------------
render "$workdir/manager-rbac.yaml" --show-only templates/clusterrole.yaml
strip_comments "$workdir/manager-rbac.yaml" "$workdir/manager-rbac.stripped.yaml"
note "operator can record events (events.k8s.io)"
assert_contains "$workdir/manager-rbac.stripped.yaml" "manager ClusterRole grants events.k8s.io events" \
	"- events.k8s.io"

# ---------------------------------------------------------------------------
# KVM device plugin (setec#187). It is a named exception to the secure pod
# rule: root and two host paths. It must never be privileged, never hold a
# capability or a ServiceAccount token, and never mount more of the host.
# ---------------------------------------------------------------------------
render "$workdir/devplugin.yaml" --show-only templates/device-plugin-daemonset.yaml
strip_comments "$workdir/devplugin.yaml" "$workdir/devplugin.stripped.yaml"
note "KVM device plugin (setec#187)"
assert_contains "$workdir/devplugin.stripped.yaml" "the device plugin is rendered and offers both devices to the kubelet" \
	"app.kubernetes.io/component: device-plugin" \
	"path: /var/lib/kubelet/device-plugins" \
	"automountServiceAccountToken: false" \
	"allowPrivilegeEscalation: false" \
	'drop: ["ALL"]'
assert_absent "$workdir/devplugin.stripped.yaml" "the device plugin is not privileged" "privileged: true"
assert_absent "$workdir/devplugin.stripped.yaml" "the device plugin adds no capability" "add:"
if [ "$(grep -c 'hostPath:' "$workdir/devplugin.stripped.yaml")" -eq 2 ]; then
	pass "the device plugin mounts exactly two host paths"
else
	fail "the device plugin must mount exactly two host paths (the kubelet plugin directory and /dev)"
fi
assert_contains "$workdir/devplugin.stripped.yaml" "the default priority class is system-node-critical" \
	"priorityClassName: system-node-critical"
assert_contains "$workdir/devplugin.stripped.yaml" "the plugin directory is mounted at the path that the plugin expects" \
	"mountPath: /var/lib/kubelet/device-plugins"
# A moved kubelet root directory changes the host path, and keeps the path in
# the container (setec#391).
render "$workdir/devplugin-moved.yaml" --show-only templates/device-plugin-daemonset.yaml \
	--set devicePlugin.kubeletDir=/var/snap/k8s/common/var/lib/kubelet/
strip_comments "$workdir/devplugin-moved.yaml" "$workdir/devplugin-moved.stripped.yaml"
assert_contains "$workdir/devplugin-moved.stripped.yaml" "a moved kubelet directory sets the host path of the plugin socket directory" \
	"path: /var/snap/k8s/common/var/lib/kubelet/device-plugins" \
	"mountPath: /var/lib/kubelet/device-plugins"
assert_absent "$workdir/devplugin-moved.stripped.yaml" "a moved kubelet directory leaves no default host path" "path: /var/lib/kubelet/device-plugins"
# An empty class renders none, and another class renders as named.
render "$workdir/devplugin-noclass.yaml" --show-only templates/device-plugin-daemonset.yaml --set devicePlugin.priorityClassName=
strip_comments "$workdir/devplugin-noclass.yaml" "$workdir/devplugin-noclass.stripped.yaml"
assert_absent "$workdir/devplugin-noclass.stripped.yaml" "an empty priority class renders none" "priorityClassName"
render "$workdir/devplugin-class.yaml" --show-only templates/device-plugin-daemonset.yaml --set devicePlugin.priorityClassName=node-plugins
assert_contains "$workdir/devplugin-class.yaml" "a named priority class renders as written" "priorityClassName: node-plugins"
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set devicePlugin.kubeletDir=var/lib/kubelet >/dev/null 2>"$workdir/devplugin-badpath.err"; then
	fail "a relative devicePlugin.kubeletDir must fail the render"
elif grep -qF "devicePlugin.kubeletDir" "$workdir/devplugin-badpath.err"; then
	pass "a relative devicePlugin.kubeletDir fails the render"
else
	fail "a relative kubelet directory failed for another reason: $(tail -n1 "$workdir/devplugin-badpath.err")"
fi

# Placement values (setec#382). The fixed arch and OS keys stay, the keys of
# an install come on top, and a fixed key wins over the same key of an install.
render "$workdir/devplugin-placed.yaml" --show-only templates/device-plugin-daemonset.yaml \
	--set 'devicePlugin.nodeSelector.setec\.zeroroot\.ai/pool=sandbox' \
	--set 'devicePlugin.nodeSelector.kubernetes\.io/arch=arm64' \
	--set 'devicePlugin.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=node.kubernetes.io/instance-type' \
	--set 'devicePlugin.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists'
assert_contains "$workdir/devplugin-placed.yaml" "devicePlugin.nodeSelector joins the fixed selector and affinity renders" \
	"kubernetes.io/arch: amd64" \
	"kubernetes.io/os: linux" \
	"setec.zeroroot.ai/pool: sandbox" \
	"node.kubernetes.io/instance-type"
assert_absent "$workdir/devplugin-placed.yaml" "a fixed selector key wins over the value of an install" \
	"kubernetes.io/arch: arm64"
assert_absent "$workdir/devplugin.stripped.yaml" "the default device plugin has no affinity" "affinity:"
assert_absent "$workdir/devplugin.stripped.yaml" "the default device plugin has no extra selector key" "setec.zeroroot.ai/pool"

# ---------------------------------------------------------------------------
# Pull access of the disk Jobs (setec#379). The build container reads a
# private registry with a pull Secret, a web identity or a cloud provider. The
# push Secret and the signing seed stay out of it. The Job is made by the
# operator, so the chart renders its flags and its ServiceAccount, and the
# unit tests of internal/controller check the Job.
# ---------------------------------------------------------------------------
note "disk Job pull access (setec#379)"
render "$workdir/pull-default.yaml" --show-only templates/deployment.yaml
assert_absent "$workdir/pull-default.yaml" "no pull flag by default" "--disk-pull-"
assert_contains "$workdir/pull-default.yaml" "the disk Jobs have their own ServiceAccount by default" \
	"--disk-service-account=setec-disk-builder"
render "$workdir/pull-set.yaml" --show-only templates/deployment.yaml \
	--set launcher.diskBuilder.pull.secret=image-pull \
	--set 'launcher.diskBuilder.pull.providers={ecr}' \
	--set launcher.diskBuilder.pull.aws.roleArn=arn:aws:iam::123456789012:role/setec-pull
assert_contains "$workdir/pull-set.yaml" "the operator gets the pull Secret, the provider and the role" \
	"--disk-pull-secret=image-pull" \
	"--disk-pull-provider=ecr" \
	"--disk-pull-aws-role-arn=arn:aws:iam::123456789012:role/setec-pull"
assert_absent "$workdir/pull-set.yaml" "the pull Secret is not the push Secret" "--disk-registry-secret=image-pull"
render "$workdir/pull-sa.yaml" --show-only templates/disk-builder-rbac.yaml \
	--set 'launcher.diskBuilder.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::123456789012:role/x'
assert_contains "$workdir/pull-sa.yaml" "the disk Job ServiceAccount is rendered, with its annotation and no token" \
	"name: setec-disk-builder" "kind: ServiceAccount" "automountServiceAccountToken: false" \
	"eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/x"
render "$workdir/pull-nosa.yaml" --set launcher.diskBuilder.serviceAccount.create=false
assert_absent "$workdir/pull-nosa.yaml" "no ServiceAccount flag when the chart makes none and the install names none" \
	"--disk-service-account="
disk_pull_refused() {
	local desc="$1" want="$2"
	shift 2
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={${NS_A}}" "$@" >/dev/null 2>"$workdir/pull-refused.err"; then
		fail "$desc must fail the render"
	elif grep -qF -- "$want" "$workdir/pull-refused.err"; then
		pass "$desc fails the render"
	else
		fail "$desc failed with another message: $(tail -n1 "$workdir/pull-refused.err")"
	fi
}
# An address of the cloud that has no name (the agent of EKS Pod Identity) goes
# to both disk Jobs in both engines, and to no other Pod.
CRED=(--set 'systemPolicy.credentialEndpoints[0].cidr=169.254.170.23/32'
	--set 'systemPolicy.credentialEndpoints[0].ports={80}')
render "$workdir/cred-cilium.yaml" "${CRED[@]}"
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A}}" --set systemPolicy.engine=networkpolicy "${CRED[@]}" \
	--set 'systemPolicy.networkPolicy.apiServer.cidrs={192.0.2.10/32}' \
	--set 'systemPolicy.networkPolicy.egress.imageSources.cidrs={198.51.100.0/24}' \
	--set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs={203.0.113.5/32}' >"$workdir/cred-np.yaml"
for engine in cilium np; do
	if python3 - "$workdir/cred-$engine.yaml" >"$workdir/cred-$engine.out" 2>&1 <<'PY'
import sys, yaml
want = {"-disk-builder", "-image-verify"}
got = set()
for d in yaml.safe_load_all(open(sys.argv[1])):
    if not isinstance(d, dict) or d.get("kind") not in ("CiliumNetworkPolicy", "NetworkPolicy"):
        continue
    text = yaml.safe_dump(d["spec"])
    if "169.254.170.23/32" in text:
        got.add(next(s for s in ("-disk-builder", "-image-verify", "-operator", "-frontend", "-node-agent", "-default-deny") if d["metadata"]["name"].endswith(s)))
if got != want:
    sys.exit(f"the credential endpoint reaches {sorted(got)}, want {sorted(want)}")
PY
	then
		pass "a credential endpoint reaches the two disk Jobs only, with the $engine engine"
	else
		fail "credential endpoint with the $engine engine: $(cat "$workdir/cred-$engine.out")"
	fi
done
disk_pull_refused "a credential endpoint that is not a CIDR" "is not a CIDR" \
	--set 'systemPolicy.credentialEndpoints[0].cidr=169.254.170.23' --set 'systemPolicy.credentialEndpoints[0].ports={80}'
disk_pull_refused "a credential endpoint with no port" "must list at least one port" \
	--set 'systemPolicy.credentialEndpoints[0].cidr=169.254.170.23/32'
disk_pull_refused "an unknown pull provider" "is not a known provider" \
	--set 'launcher.diskBuilder.pull.providers={gcr}'
disk_pull_refused "a pull role with no provider" "needs launcher.diskBuilder.pull.providers" \
	--set launcher.diskBuilder.pull.aws.roleArn=arn:aws:iam::123456789012:role/x
disk_pull_refused "a pull role that is not an ARN" "must be an IAM role ARN" \
	--set 'launcher.diskBuilder.pull.providers={ecr}' --set launcher.diskBuilder.pull.aws.roleArn=setec-pull
disk_pull_refused "a pull role with no named ServiceAccount" "needs a named ServiceAccount" \
	--set 'launcher.diskBuilder.pull.providers={ecr}' --set launcher.diskBuilder.pull.aws.roleArn=arn:aws:iam::123456789012:role/x \
	--set launcher.diskBuilder.serviceAccount.create=false

# ---------------------------------------------------------------------------
# The node agent has no Kubernetes credentials (setec#198).
#
# The agent is root and privileged: it reads and writes the work volumes of
# the launcher Pods on the host. What bounds it is that it calls no
# Kubernetes API, so it has no token and no RBAC, and it mounts no more of
# the host than the kubelet Pod directory and its own snapshot directories.
# ---------------------------------------------------------------------------
render "$workdir/nodeagent.yaml" --set nodeAgent.enabled=true --set snapshots.enabled=true \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}' \
	--show-only templates/daemonset.yaml
strip_comments "$workdir/nodeagent.yaml" "$workdir/nodeagent.stripped.yaml"
note "node agent has no Kubernetes credentials (setec#198)"
assert_contains "$workdir/nodeagent.stripped.yaml" "the node agent mounts no ServiceAccount token" \
	"automountServiceAccountToken: false" \
	"hostNetwork: false"
assert_absent "$workdir/nodeagent.stripped.yaml" "the node agent has no ClusterRole" "kind: ClusterRole"
assert_absent "$workdir/nodeagent.stripped.yaml" "the node agent does not mount the host /dev" "path: /dev"
# The Workload API socket directory under /run/spire is the one mount of
# /run, so the check matches the whole path, not a prefix.
if grep -qE '^[[:space:]]*path: /run/?$' "$workdir/nodeagent.stripped.yaml"; then
	fail "the node agent does not mount the host /run — unexpectedly present: path: /run"
else
	pass "the node agent does not mount the host /run"
fi

# ---------------------------------------------------------------------------
# The launcher is the only runtime (setec#198).
#
# The Kata, gVisor and runc backends and the node installer were removed.
# None of their objects may come back in a render: a RuntimeClass, an
# installer or runtime-agent DaemonSet, or a runtimes ConfigMap.
# ---------------------------------------------------------------------------
note "the launcher is the only runtime (setec#198)"
render "$workdir/all-on.yaml" --set nodeAgent.enabled=true --set snapshots.enabled=true \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}' \
	--set webhook.enabled=true
for needle in "kind: RuntimeClass" "app.kubernetes.io/component: installer" \
	"app.kubernetes.io/component: runtime-agent" "setec-runtimes" "--runtimes-config"; do
	assert_absent "$workdir/all-on.yaml" "a render has no object of a removed backend" "$needle"
done
assert_contains "$workdir/all-on.yaml" "the operator gets the launcher flags" \
	"--launcher-image=" \
	"--disk-repo=" \
	"--disk-public-key="
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	>/dev/null 2>&1; then
	fail "a render without the launcher values must fail: each launcher needs a disk repository and its keys"
else
	pass "a render without the launcher values fails"
fi

# ---------------------------------------------------------------------------
# Leader-election RBAC (setec#217, granted by #219).
#
# `leaderElect` defaults to true and renders --leader-elect=true, but the
# chart granted no coordination.k8s.io/leases, so controller-runtime could
# not retrieve the lock and the manager shut down: CrashLoopBackOff on a
# default install, and no sandbox execution plane. It installed cleanly and
# ran nothing, which is the failure class this script exists for.
#
# Asserted in BOTH directions so the flag and the permission cannot drift.
# ---------------------------------------------------------------------------
note "leader-election RBAC (setec#217)"
render "$workdir/le.yaml" --show-only templates/leader-election-rbac.yaml
# Comment-stripped: the template quotes the forbidden-leases error in its own
# rationale, so an assertion against the raw render passes even when the rule
# itself has been changed to another resource.
strip_comments "$workdir/le.yaml" "$workdir/le-rules.yaml"
assert_contains "$workdir/le-rules.yaml" "leader election can hold its Lease when enabled" \
	"kind: Role" \
	"- coordination.k8s.io" \
	"- leases" \
	"- create" \
	"- update"
assert_contains "$workdir/le-rules.yaml" "the Lease Role is bound to the operator ServiceAccount" \
	"kind: RoleBinding" \
	"kind: ServiceAccount"
assert_contains "$workdir/default.yaml" "the flag that needs the Lease is actually set" \
	"--leader-elect=true"

# THE NAMESPACE, NOT JUST THE RULE (setec#217 reopened).
#
# The grant must land in the namespace the operator RUNS in. The first cut of
# this template used .Release.Namespace while every other template uses
# .Values.namespace; those coincide in a standalone `helm template
# --namespace setec-system` and DIVERGE in the deployed shape, where gibson
# vendors this chart as a subchart of an Argo Application whose destination is
# its own namespace. The Role went to `gibson`, the operator stayed in
# `setec-system`, and the lease was still forbidden — the fix rendered and did
# nothing. So render with a release namespace that deliberately differs, and
# require the RBAC to follow the Deployment rather than the release.
render "$workdir/ns-skew.yaml" --namespace release-ns-not-operator-ns
operator_ns="$(awk '/^kind: Deployment$/{d=1} d&&/^  namespace: /{print $2; exit}' \
	<(sed -n '/# Source: setec\/templates\/deployment.yaml/,$p' "$workdir/ns-skew.yaml"))"
le_ns="$(awk '/^kind: Role$/{d=1} d&&/^  namespace: /{print $2; exit}' \
	<(sed -n '/# Source: setec\/templates\/leader-election-rbac.yaml/,$p' "$workdir/ns-skew.yaml"))"
if [ -z "$operator_ns" ] || [ -z "$le_ns" ]; then
	fail "leader-election namespace check could not read both namespaces (operator='$operator_ns' role='$le_ns')"
elif [ "$operator_ns" != "$le_ns" ]; then
	fail "the Lease grant lands in '$le_ns' but the operator runs in '$operator_ns' — a lease grant in the wrong namespace is not a grant"
else
	pass "the Lease grant follows the operator's namespace, not the release's"
fi

# Disabled: no flag, so no lease grant either. `helm template --show-only` on a
# template that renders nothing exits non-zero, so assert over the full render.
render "$workdir/le-off.yaml" --set leaderElect=false
assert_absent "$workdir/le-off.yaml" "no Lease grant when leader election is off" \
	"-leader-election"
assert_absent "$workdir/le-off.yaml" "no leader-elect flag when disabled" \
	"--leader-elect=true"

# ---------------------------------------------------------------------------
# Frontend enrollment (setec#168, docs/design/isolation.md).
#
# The frontend refuses a caller that is not an enrolled client, so an
# install with no client serves nobody. The chart must render one --client
# for each entry, refuse an empty list, and refuse an entry that is not a
# SPIFFE ID. The fixed shared namespace and the tenant label override are
# gone: two pairs of client and tenant must never share a namespace.
# ---------------------------------------------------------------------------
note "frontend enrollment (setec#168)"
FE=(--set frontend.enabled=true
	--set credentials.spiffe.trustDomain=example.org
	--set 'frontend.clients[0].name=saas'
	--set 'frontend.clients[0].spiffeID=spiffe://example.org/ns/gibson/sa/gibson-daemon'
	--set 'frontend.clients[1].name=onprem'
	--set 'frontend.clients[1].spiffeID=spiffe://onprem.example/ns/gibson/sa/gibson-daemon'
	--set 'frontend.clients[1].federation.bundleEndpointURL=https://spire.onprem.example:8443'
	--set 'frontend.clients[1].federation.endpointSPIFFEID=spiffe://onprem.example/spire/server'
	--set 'systemPolicy.frontendCallers[0].namespace=gibson'
	--set 'systemPolicy.frontendCallers[0].podLabels.app\.kubernetes\.io/component=daemon'
	--set 'systemPolicy.frontendCallers[1].cidr=203.0.113.0/24')

render "$workdir/fe-default.yaml" "${FE[@]}" \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-default.yaml" "$workdir/fe-default.stripped.yaml"
assert_contains "$workdir/fe-default.stripped.yaml" "each enrolled client reaches the frontend" \
	"--client=saas=spiffe://example.org/ns/gibson/sa/gibson-daemon" \
	"--client=onprem=spiffe://onprem.example/ns/gibson/sa/gibson-daemon"
assert_absent "$workdir/fe-default.stripped.yaml" "no fixed shared namespace" \
	"--sandbox-namespace"
assert_absent "$workdir/fe-default.stripped.yaml" "no tenant label override" \
	"--tenant-namespace-label"

render "$workdir/fe-scope.yaml" "${FE[@]}"
strip_comments "$workdir/fe-scope.yaml" "$workdir/fe-scope.stripped.yaml"
assert_contains "$workdir/fe-scope.stripped.yaml" "the frontend gets the pair grants and the scope policy (setec#207)" \
	"--pair-namespace-grant=setec-sandbox-namespace=" \
	"--pair-namespace-grant=setec-frontend-exec=" \
	"name: setec-frontend-scope" \
	"name: setec-sandbox-host-guard-pairs" \
	"setec.zeroroot.ai/sandbox-namespace: \"true\""
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true "${FE[@]}" >/dev/null 2>&1; then
	pass "a frontend install renders with no static sandboxNamespaces"
else
	fail "a frontend install must render with no static sandboxNamespaces: the frontend makes the pair namespaces"
fi

if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set frontend.enabled=true \
	--set credentials.spiffe.trustDomain=example.org \
	>/dev/null 2>&1; then
	fail "a frontend with no enrolled client must fail the render"
else
	pass "a frontend with no enrolled client fails the render"
fi

if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set frontend.enabled=true \
	--set credentials.spiffe.trustDomain=example.org \
	--set 'frontend.clients[0].name=saas' \
	--set 'frontend.clients[0].spiffeID=https://zeroroot.ai/gibson' \
	>/dev/null 2>&1; then
	fail "a client whose ID is not a SPIFFE ID must fail the render"
else
	pass "a client whose ID is not a SPIFFE ID fails the render"
fi

# ---------------------------------------------------------------------------
# Reserved egress ranges cover both address families (GHSA-qwgf-q723-rpjf).
#
# A reserved prefix only subtracts from an egress block of its own family.
# The shipped list used to be IPv4 only, so an allow-list entry naming an
# IPv6 address such as the AWS instance-metadata address fd00:ec2::254 was
# granted outright. The default render must carry the IPv6 entries, and a
# list with either family missing must fail the render rather than start
# an operator that grants one family unrestricted.
# ---------------------------------------------------------------------------
note "reserved egress ranges in both address families (GHSA-qwgf-q723-rpjf)"
render "$workdir/operator.yaml" --show-only templates/deployment.yaml
strip_comments "$workdir/operator.yaml" "$workdir/operator.stripped.yaml"
assert_contains "$workdir/operator.stripped.yaml" "default reserved list reaches the operator with IPv6 entries" \
	"--reserved-cidrs=" \
	"::1/128" \
	"fc00::/7" \
	"fe80::/10" \
	"ff00::/8" \
	"169.254.0.0/16"

if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set 'netpol.reservedCIDRs={10.0.0.0/8,169.254.0.0/16}' \
	>/dev/null 2>&1; then
	fail "an IPv4-only reserved list must fail the render"
else
	pass "an IPv4-only reserved list fails the render"
fi

if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set 'netpol.reservedCIDRs={fc00::/7,fe80::/10}' \
	>/dev/null 2>&1; then
	fail "an IPv6-only reserved list must fail the render"
else
	pass "an IPv6-only reserved list fails the render"
fi

# ---------------------------------------------------------------------------
# Frontend pods/exec is bound per Sandbox namespace, never cluster-wide
# (setec#6).
#
# pods/exec is a pod-entry grant: the holder steps into a process that
# already holds its ServiceAccount token, its Secrets and its SPIFFE SVID.
# No admission policy runs on a subresource, so the binding scope is the
# whole of the control. The grant must reach the frontend only through a
# RoleBinding in each sandboxNamespaces entry, and through a
# ClusterRoleBinding only under the explicit rbac.allowClusterWideSandboxWrite
# toggle. This check is structural: it walks every document and refuses any
# ClusterRoleBinding whose roleRef names a ClusterRole that grants pods/exec.
# ---------------------------------------------------------------------------
note "frontend pods/exec bound per Sandbox namespace (setec#6)"

# rbac_index <stripped-render> — one line per RBAC document:
#   <kind> <name> <namespace-or-> <roleRef-name-or-> <exec|->
rbac_index() {
	awk '
	function flush() {
		if (kind != "") printf "%s %s %s %s %s\n", kind, name, ns, ref, (exec ? "exec" : "-")
		kind = ""; name = ""; ns = "-"; ref = "-"; exec = 0; sect = ""
	}
	/^---/ { flush(); next }
	/^kind:/ { kind = $2; next }
	/^metadata:/ { sect = "meta"; next }
	/^roleRef:/ { sect = "ref"; next }
	/^[a-zA-Z]/ { sect = "" }
	/^  name:/ { if (sect == "meta") name = $2; else if (sect == "ref") ref = $2; next }
	/^  namespace:/ { if (sect == "meta") ns = $2; next }
	/pods\/exec/ { exec = 1 }
	END { flush() }
	' "$1" | grep -E '^(ClusterRole|ClusterRoleBinding|Role|RoleBinding) '
}

# assert_no_clusterwide_exec <stripped-render> <description>
# Fails when any ClusterRoleBinding references a ClusterRole that grants
# pods/exec, whatever either object is called.
assert_no_clusterwide_exec() {
	local file="$1" desc="$2" index exec_roles role
	index="$(rbac_index "$file")"
	exec_roles="$(printf '%s\n' "$index" | awk '$1 == "ClusterRole" && $5 == "exec" { print $2 }')"
	for role in $exec_roles; do
		if printf '%s\n' "$index" | awk -v r="$role" '$1 == "ClusterRoleBinding" && $4 == r { found = 1 } END { exit !found }'; then
			fail "$desc — ClusterRoleBinding grants pods/exec through ClusterRole $role"
			return
		fi
	done
	pass "$desc"
}

render "$workdir/fe-rbac.yaml" "${FE[@]}" --show-only templates/frontend.yaml
strip_comments "$workdir/fe-rbac.yaml" "$workdir/fe-rbac.stripped.yaml"
assert_no_clusterwide_exec "$workdir/fe-rbac.stripped.yaml" "no ClusterRoleBinding grants pods/exec by default"
if rbac_index "$workdir/fe-rbac.stripped.yaml" | grep -q "^ClusterRole setec-frontend-exec - - exec$"; then
	pass "pods/exec lives in its own ClusterRole"
else
	fail "pods/exec must live in ClusterRole setec-frontend-exec and nowhere else"
fi
for ns in "$NS_A" "$NS_B"; do
	if rbac_index "$workdir/fe-rbac.stripped.yaml" | grep -q "^RoleBinding setec-frontend-exec $ns setec-frontend-exec"; then
		pass "pods/exec is RoleBound into $ns"
	else
		fail "pods/exec must be RoleBound into $ns"
	fi
done

# The cluster-wide form must exist only under the explicit toggle, so the
# toggle is proven wired and the default is proven to be the narrow one.
render "$workdir/fe-rbac-wide.yaml" "${FE[@]}" \
	--set rbac.allowClusterWideSandboxWrite=true \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-rbac-wide.yaml" "$workdir/fe-rbac-wide.stripped.yaml"
if rbac_index "$workdir/fe-rbac-wide.stripped.yaml" | grep -q "^ClusterRoleBinding setec-frontend-exec - setec-frontend-exec"; then
	pass "rbac.allowClusterWideSandboxWrite=true binds pods/exec cluster-wide, on the record"
else
	fail "rbac.allowClusterWideSandboxWrite=true must bind pods/exec with a ClusterRoleBinding"
fi

# ---------------------------------------------------------------------------
# SandboxClass egress allowance by selector (setec#76).
#
# The umbrella's test profile grants the agent class kube-dns and the
# platform edge through sandboxClasses.classes[].spec.egressAllowSelectors.
# The value renders verbatim, so the regression to catch is a chart that
# quietly drops or mangles it, and a render-time validator that stops
# refusing a peer with no selector or no port: that entry would install
# cleanly and the admission webhook would refuse the class from inside a
# post-install hook.
# ---------------------------------------------------------------------------
note "SandboxClass egress allowance by selector (setec#76)"
cat >"$workdir/allowance-values.yaml" <<'YAML'
sandboxClasses:
  classes:
    - name: tool
      spec:
        runtime: {backend: launcher}
        defaultNetworkMode: external-only
        default: true
    - name: agent
      spec:
        runtime: {backend: launcher}
        defaultNetworkMode: external-only
        egressAllowSelectors:
          - namespaceSelector:
              matchLabels: {kubernetes.io/metadata.name: kube-system}
            podSelector:
              matchLabels: {k8s-app: kube-dns}
            ports:
              - {protocol: UDP, port: 53}
              - {protocol: TCP, port: 53}
          - namespaceSelector:
              matchLabels: {kubernetes.io/metadata.name: gibson}
            podSelector:
              matchLabels:
                app.kubernetes.io/name: gibson-workloads
                app.kubernetes.io/component: envoy
            ports:
              - {port: https}
YAML
render "$workdir/allowance.yaml" -f "$workdir/allowance-values.yaml" \
	--show-only templates/sandboxclass-default.yaml
strip_comments "$workdir/allowance.yaml" "$workdir/allowance.stripped.yaml"
assert_contains "$workdir/allowance.stripped.yaml" "the allowance reaches the rendered SandboxClass" \
	"egressAllowSelectors:" \
	"kubernetes.io/metadata.name: kube-system" \
	"k8s-app: kube-dns" \
	"protocol: UDP" \
	"port: 53" \
	"app.kubernetes.io/component: envoy" \
	"port: https"

# The default values ship no allowance: production keeps public resolvers
# and no selectors. Scoped to the SandboxClass documents, because the
# admission policies elsewhere in the release carry selectors of their own.
render "$workdir/classes-default.yaml" --show-only templates/sandboxclass-default.yaml
strip_comments "$workdir/classes-default.yaml" "$workdir/classes-default.stripped.yaml"
assert_absent "$workdir/classes-default.stripped.yaml" "the shipped classes grant no selector allowance" \
	"namespaceSelector:"

# Malformed shapes must fail the render, not the post-install hook.
allowance_refused() {
	local desc="$1" entries="$2"
	printf 'sandboxClasses:\n  classes:\n    - name: t\n      spec:\n        defaultNetworkMode: none\n        egressAllowSelectors: %s\n' \
		"$entries" >"$workdir/allowance-bad.yaml"
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" \
		--set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={${NS_A},${NS_B}}" \
		-f "$workdir/allowance-bad.yaml" >/dev/null 2>&1; then
		fail "$desc"
	else
		pass "$desc"
	fi
}
allowance_refused "an allowance with no selector fails the render" \
	'[{"ports":[{"port":53}]}]'
allowance_refused "an allowance with no ports fails the render" \
	'[{"podSelector":{"matchLabels":{"a":"b"}}}]'
allowance_refused "a port with no port value fails the render" \
	'[{"podSelector":{"matchLabels":{"a":"b"}},"ports":[{"protocol":"TCP"}]}]'
allowance_refused "a port with an unknown protocol fails the render" \
	'[{"podSelector":{"matchLabels":{"a":"b"}},"ports":[{"protocol":"ICMP","port":53}]}]'

# ---------------------------------------------------------------------------
# Disjoint selectors (setec#383). The operator Deployment, the device plugin,
# the node agent and the frontend each select their own Pods only, and each
# Service and the PodDisruptionBudget select the Pods of one workload. The
# new Deployment also selects the Pods of a 0.130 Deployment, so the step
# "Upgrading from 0.130" of the chart README adopts them.
# ---------------------------------------------------------------------------
note "disjoint selectors (setec#383)"
SEL_ALL=("${FE[@]}" --set nodeAgent.enabled=true --set snapshots.enabled=true
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}')
render "$workdir/sel-all.yaml" "${SEL_ALL[@]}"
OLD_DEPLOYMENT="$(dirname "$0")/testdata/operator-deployment-0.130.yaml"
if python3 "$(dirname "$0")/check-selectors.py" "$workdir/sel-all.yaml" --adopt "$OLD_DEPLOYMENT" >"$workdir/sel-all.out"; then
	pass "each selector matches its own Pods only, and the Deployment adopts the Pods of 0.130"
else
	fail "the selectors overlap: $(cat "$workdir/sel-all.out")"
fi
python3 - "$workdir/sel-all.yaml" "$workdir" <<'PY'
import copy, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
def save(name, ds):
    yaml.safe_dump_all(ds, open(f"{sys.argv[2]}/sel-{name}.yaml", "w"))
# The selector of 0.130: no component label.
old = copy.deepcopy(docs)
for d in old:
    if d["kind"] == "Deployment" and d["metadata"]["name"] == "setec":
        del d["spec"]["selector"]["matchLabels"]["app.kubernetes.io/component"]
save("old-selector", old)
# A Service that selects the Pods of every component.
wide = copy.deepcopy(docs)
for d in wide:
    if d["kind"] == "Service" and d["metadata"]["name"].endswith("-node-agent"):
        del d["spec"]["selector"]["app.kubernetes.io/component"]
save("wide-service", wide)
# A PodDisruptionBudget that selects the Pods of every component.
pdb = copy.deepcopy(docs)
for d in pdb:
    if d["kind"] == "PodDisruptionBudget":
        del d["spec"]["selector"]["matchLabels"]["app.kubernetes.io/component"]
save("wide-pdb", pdb)
# A Deployment selector that does not select the Pods of 0.130.
noadopt = copy.deepcopy(docs)
for d in noadopt:
    if d["kind"] == "Deployment" and d["metadata"]["name"] == "setec":
        d["spec"]["selector"]["matchLabels"]["app.kubernetes.io/part-of"] = "setec"
        d["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/part-of"] = "setec"
save("no-adoption", noadopt)
PY
for fx in old-selector wide-service wide-pdb; do
	if python3 "$(dirname "$0")/check-selectors.py" "$workdir/sel-$fx.yaml" >/dev/null; then
		fail "fixture $fx: the checker passed overlapping selectors"
	else
		pass "fixture $fx: the checker refuses it"
	fi
done
if python3 "$(dirname "$0")/check-selectors.py" "$workdir/sel-no-adoption.yaml" --adopt "$OLD_DEPLOYMENT" >/dev/null; then
	fail "fixture no-adoption: the checker passed a selector that cannot adopt the Pods of 0.130"
else
	pass "fixture no-adoption: the checker refuses it"
fi

# ---------------------------------------------------------------------------
# The setec namespace has its own network policy (docs/design/network-policy.md):
# a default deny with DNS, and one allow for each component
# (templates/system-namespace-policy.yaml, the cilium engine).
# hack/check-system-policy.py checks the render. Each fixture below is a
# render with one piece removed, and the checker must refuse it.
# ---------------------------------------------------------------------------
note "the setec namespace network policy"
SP_ALL=("${FE[@]}" --set nodeAgent.enabled=true --set snapshots.enabled=true
	--set snapshots.s3.bucket=setec-snapshots
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}'
	--set 'systemPolicy.webhookCallers[0].namespace=gibson'
	--set 'systemPolicy.webhookCallers[0].podLabels.app\.kubernetes\.io/component=webhook-gate')
render "$workdir/sp-all.yaml" "${SP_ALL[@]}"
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/sp-all.yaml" setec-system >"$workdir/sp-all.out"; then
	pass "each component of the setec namespace has an allow, under a default deny with DNS"
else
	fail "the setec namespace policy is incomplete: $(cat "$workdir/sp-all.out")"
fi
render "$workdir/sp-default.yaml"
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/sp-default.yaml" setec-system >"$workdir/sp-default.out"; then
	pass "the default install has the default deny and the operator allow"
else
	fail "the default install policy is incomplete: $(cat "$workdir/sp-default.out")"
fi
python3 - "$workdir/sp-all.yaml" "$workdir" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
def drop(pred, name):
    yaml.safe_dump_all([d for d in docs if not pred(d)], open(f"{sys.argv[2]}/sp-{name}.yaml", "w"))
drop(lambda d: d.get("kind") == "CiliumClusterwideNetworkPolicy", "no-deny")
drop(lambda d: d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-node-agent"), "no-node-agent")
drop(lambda d: d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-frontend"), "no-frontend")
drop(lambda d: d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-disk-builder"), "no-disk-builder")
drop(lambda d: d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-image-verify"), "no-image-verify")
# A Job policy that reaches the world, in each form the checker knows
# (setec#279): the entity world, the whole address space, a "*" pattern.
import copy
for fx, rule in {
    "builder-world": {"toEntities": ["world"], "toPorts": [{"ports": [{"port": "443", "protocol": "TCP"}]}]},
    "builder-all-cidr": {"toCIDR": ["0.0.0.0/0"]},
    "verify-any-fqdn": {"toFQDNs": [{"matchPattern": "*"}]},
}.items():
    world = copy.deepcopy(docs)
    target = "-image-verify" if fx.startswith("verify") else "-disk-builder"
    for d in world:
        if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith(target):
            d["spec"]["egress"].append(rule)
    yaml.safe_dump_all(world, open(f"{sys.argv[2]}/sp-{fx}.yaml", "w"))
# A build policy that does not reach the disk registry.
nodisk = copy.deepcopy(docs)
for d in nodisk:
    if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-disk-builder"):
        d["spec"]["egress"] = d["spec"]["egress"][:1]
yaml.safe_dump_all(nodisk, open(f"{sys.argv[2]}/sp-builder-no-disk-registry.yaml", "w"))
# A metrics peer of the operator that pins no namespace (setec#252).
for d in docs:
    if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-operator"):
        for rule in d["spec"].get("ingress") or []:
            for peer in rule.get("fromEndpoints") or []:
                peer.get("matchLabels", {}).pop("k8s:io.kubernetes.pod.namespace", None)
                peer["matchExpressions"] = [{"key": "k8s:io.kubernetes.pod.namespace", "operator": "Exists"}]
yaml.safe_dump_all(docs, open(f"{sys.argv[2]}/sp-any-namespace.yaml", "w"))
PY
for fx in no-deny no-node-agent no-frontend no-disk-builder no-image-verify \
	builder-world builder-all-cidr verify-any-fqdn builder-no-disk-registry any-namespace; do
	if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/sp-$fx.yaml" setec-system >/dev/null; then
		fail "fixture $fx: the checker passed a render with a missing policy"
	else
		pass "fixture $fx: the checker refuses it"
	fi
done
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set frontend.enabled=true --set frontend.tlsCertSecretName=fe-tls --set frontend.tlsClientCASecretName=fe-ca \
	--set 'frontend.clients[0].name=saas' \
	--set 'frontend.clients[0].spiffeID=spiffe://example.org/ns/gibson/sa/gibson-daemon' \
	>/dev/null 2>"$workdir/sp-nocaller.err"; then
	fail "a frontend with no systemPolicy.frontendCallers must fail the render"
elif grep -qF "systemPolicy.frontendCallers" "$workdir/sp-nocaller.err"; then
	pass "a frontend with no systemPolicy.frontendCallers fails the render"
else
	fail "a frontend with no callers failed for another reason: $(tail -n1 "$workdir/sp-nocaller.err")"
fi
grep -q "port: \"50051\"" "$workdir/sp-all.yaml" && grep -q "k8s:io.kubernetes.pod.namespace: gibson" "$workdir/sp-all.yaml" \
	&& grep -q -- "- 203.0.113.0/24" "$workdir/sp-all.yaml" \
	&& pass "the frontend gRPC port admits the named caller and the named range" \
	|| fail "the frontend gRPC port does not admit the named caller and range"
# A webhook caller (setec#281): the operator admits it on 9443, pinned to its
# namespace, and the default install admits nobody but the API server.
if python3 - "$workdir/sp-all.yaml" "$workdir/sp-default.yaml" >"$workdir/sp-webhook.out" 2>&1 <<'PY'
import sys, yaml
def callers(path):
    out = []
    for d in yaml.safe_load_all(open(path)):
        if isinstance(d, dict) and d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-operator"):
            for rule in d["spec"].get("ingress") or []:
                ports = [p.get("port") for tp in rule.get("toPorts") or [] for p in tp.get("ports") or []]
                if "9443" in ports:
                    out += [peer.get("matchLabels") for peer in rule.get("fromEndpoints") or []]
    return out
want = {"k8s:io.kubernetes.pod.namespace": "gibson", "app.kubernetes.io/component": "webhook-gate"}
got_all, got_default = callers(sys.argv[1]), callers(sys.argv[2])
if got_all != [want]:
    sys.exit(f"the named webhook caller is not admitted on 9443: {got_all}")
if got_default:
    sys.exit(f"the default install admits a webhook caller: {got_default}")
PY
then
	pass "the webhook port admits the named caller only, pinned to its namespace"
else
	fail "webhook callers: $(cat "$workdir/sp-webhook.out")"
fi
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set 'systemPolicy.webhookCallers[0].namespace=gibson' \
	>/dev/null 2>"$workdir/sp-nowhlabels.err"; then
	fail "a webhook caller with no podLabels must fail the render"
elif grep -qF "systemPolicy.webhookCallers" "$workdir/sp-nowhlabels.err"; then
	pass "a webhook caller with no podLabels fails the render"
else
	fail "a webhook caller with no podLabels failed for another reason: $(tail -n1 "$workdir/sp-nowhlabels.err")"
fi
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set 'systemPolicy.metricsScrapers[0].namespace=' \
	--set 'systemPolicy.metricsScrapers[0].podLabels.app\.kubernetes\.io/name=prometheus' \
	>/dev/null 2>"$workdir/sp-noscrapens.err"; then
	fail "a metrics scraper with no namespace must fail the render"
elif grep -qF "systemPolicy.metricsScrapers" "$workdir/sp-noscrapens.err"; then
	pass "a metrics scraper with no namespace fails the render"
else
	fail "a scraper with no namespace failed for another reason: $(tail -n1 "$workdir/sp-noscrapens.err")"
fi

# An image source at an IP address lists no hosts (setec#381). The disk Jobs
# reach that address and port, and no other new destination. A source at a
# name still needs its hosts.
MIRROR=(--set 'launcher.imageSources[0].registry=ghcr.io'
	--set 'launcher.imageSources[0].hosts={ghcr.io}'
	--set 'launcher.imageSources[1].registry=10.43.0.5:5000')
render "$workdir/sp-ipsource.yaml" "${MIRROR[@]}"
render "$workdir/sp-ipsource-base.yaml" --set 'launcher.imageSources[0].registry=ghcr.io' --set 'launcher.imageSources[0].hosts={ghcr.io}'
if python3 - "$workdir/sp-ipsource.yaml" "$workdir/sp-ipsource-base.yaml" >"$workdir/sp-ipsource.out" 2>&1 <<'PY'
import sys, yaml
def dests(path, comp):
    out = set()
    for d in yaml.safe_load_all(open(path)):
        if isinstance(d, dict) and d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith(comp):
            for e in d["spec"].get("egress") or []:
                ports = tuple(p["port"] for tp in e.get("toPorts") or [] for p in tp.get("ports") or [])
                for f in e.get("toFQDNs") or []:
                    out.add((f.get("matchName") or f.get("matchPattern"), ports))
                for c in e.get("toCIDR") or []:
                    out.add((c, ports))
    return out
for comp in ("-disk-builder", "-image-verify"):
    new = dests(sys.argv[1], comp) - dests(sys.argv[2], comp)
    if new != {("10.43.0.5/32", ("5000",))}:
        sys.exit(f"{comp}: the new destinations are {sorted(new)}, want only 10.43.0.5/32 on 5000")
PY
then
	pass "an image source at an IP address with no hosts renders, and the Jobs reach that address and port only"
else
	fail "image source at an IP address: $(cat "$workdir/sp-ipsource.out")"
fi
if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set 'launcher.imageSources[0].registry=registry.example.com' \
	>/dev/null 2>"$workdir/sp-nohosts.err"; then
	fail "an image source at a name with no hosts must fail the render"
elif grep -qF "needs its hosts" "$workdir/sp-nohosts.err"; then
	pass "an image source at a name with no hosts fails the render"
else
	fail "a source with no hosts failed for another reason: $(tail -n1 "$workdir/sp-nohosts.err")"
fi

# ---------------------------------------------------------------------------
# The two policy engines (setec#378). A cluster with no Cilium refuses the
# default engine with a message, and installs with the networkpolicy engine,
# which renders plain NetworkPolicy objects only. These renders pass no
# --api-versions flag, so they see a cluster with no Cilium CRDs.
# ---------------------------------------------------------------------------
note "the cilium engine needs the Cilium CRDs, the networkpolicy engine does not (setec#378)"
if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	>/dev/null 2>"$workdir/eng-nocrd.err"; then
	fail "the cilium engine rendered Cilium objects on a cluster with no Cilium CRDs"
elif grep -qF "has no CRD for CiliumNetworkPolicy and CiliumClusterwideNetworkPolicy" "$workdir/eng-nocrd.err" \
	&& grep -qF "systemPolicy.engine=networkpolicy" "$workdir/eng-nocrd.err"; then
	pass "the cilium engine refuses a cluster with no Cilium CRDs, and names the other engine"
else
	fail "the cilium engine failed with another message: $(tail -n2 "$workdir/eng-nocrd.err")"
fi
if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
	--set systemPolicy.engine=none >/dev/null 2>"$workdir/eng-bad.err"; then
	fail "an unknown systemPolicy.engine must fail the render"
elif grep -qF "systemPolicy.engine=\"none\" is not valid" "$workdir/eng-bad.err"; then
	pass "an unknown systemPolicy.engine fails the render"
else
	fail "an unknown engine failed with another message: $(tail -n1 "$workdir/eng-bad.err")"
fi

# The inputs that the networkpolicy engine needs, for a cluster with flannel.
# The disk registry and the image source are names, so each gets its CIDRs.
NP=(--set systemPolicy.engine=networkpolicy
	--set 'systemPolicy.networkPolicy.apiServer.cidrs={192.0.2.10/32}'
	--set 'systemPolicy.networkPolicy.webhookSourceCIDRs={10.42.0.0/24}'
	--set 'systemPolicy.networkPolicy.egress.imageSources.cidrs={198.51.100.0/24}'
	--set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs={203.0.113.5/32}')
NP_ALL=("${NP[@]}" "${FE[@]}" --set nodeAgent.enabled=true --set snapshots.enabled=true
	--set snapshots.s3.enabled=true --set snapshots.s3.bucket=setec-snapshots --set snapshots.s3.pathStyle=false
	--set 'systemPolicy.networkPolicy.egress.snapshotStore.cidrs={198.18.0.0/24}'
	--set 'systemPolicy.networkPolicy.egress.sigstore.cidrs={198.19.0.0/24}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}'
	--set 'systemPolicy.webhookCallers[0].namespace=gibson'
	--set 'systemPolicy.webhookCallers[0].podLabels.app\.kubernetes\.io/component=webhook-gate')
npender() {
	local out="$1"
	shift
	"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={${NS_A},${NS_B}}" "$@" >"$out"
}
npender "$workdir/np-all.yaml" "${NP_ALL[@]}"
npender "$workdir/np-default.yaml" "${NP[@]}"
assert_absent "$workdir/np-all.yaml" "the networkpolicy engine renders no Cilium object" "cilium.io"
assert_contains "$workdir/np-all.yaml" "the networkpolicy engine renders a namespace deny and a policy for each component" \
	"name: setec-default-deny" "name: setec-operator" "name: setec-frontend" "name: setec-node-agent" \
	"name: setec-disk-builder" "name: setec-image-verify"
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/np-all.yaml" setec-system >"$workdir/np-all.out"; then
	pass "each component has an allow under a default deny with DNS, with the networkpolicy engine"
else
	fail "the networkpolicy engine policy is incomplete: $(cat "$workdir/np-all.out")"
fi
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/np-default.yaml" setec-system >"$workdir/np-default.out"; then
	pass "the default install has the default deny and the operator allow, with the networkpolicy engine"
else
	fail "the default networkpolicy install is incomplete: $(cat "$workdir/np-default.out")"
fi
# The Sandbox namespaces have the same default deny under both engines. It
# is a plain NetworkPolicy, so Cilium is not needed.
assert_contains "$workdir/np-all.yaml" "the Sandbox namespaces keep the baseline default deny with the networkpolicy engine" \
	"name: setec-sandbox-baseline-deny" "podSelector: {}" "namespace: ${NS_A}" "namespace: ${NS_B}"
assert_contains "$workdir/np-all.yaml" "the networkpolicy engine renders the host guard of the Sandbox namespaces" \
	"name: setec-sandbox-host-guard"
python3 - "$workdir/np-all.yaml" "$workdir" <<'PY'
import copy, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
out = sys.argv[2]
def save(name, ds):
    yaml.safe_dump_all(ds, open(f"{out}/np-{name}.yaml", "w"))
def netpol(d, suffix):
    return d.get("kind") == "NetworkPolicy" and d["metadata"]["name"].endswith(suffix)
save("no-deny", [d for d in docs if not netpol(d, "-default-deny")])
for fx, suffix in (("no-node-agent", "-node-agent"), ("no-frontend", "-frontend"),
                   ("no-disk-builder", "-disk-builder"), ("no-image-verify", "-image-verify")):
    save(fx, [d for d in docs if not netpol(d, suffix)])
def edit(name, suffix, fn):
    c = copy.deepcopy(docs)
    for d in c:
        if netpol(d, suffix):
            fn(d)
    save(name, c)
# A Job policy that reaches the world: the whole address space with no except list.
edit("builder-world", "-disk-builder", lambda d: d["spec"]["egress"].append(
    {"to": [{"ipBlock": {"cidr": "0.0.0.0/0"}}], "ports": [{"port": 443, "protocol": "TCP"}]}))
edit("verify-world-v6", "-image-verify", lambda d: d["spec"]["egress"].append(
    {"to": [{"ipBlock": {"cidr": "::/0"}}], "ports": [{"port": 443, "protocol": "TCP"}]}))
# A rule with no port allows every port. A rule with no peer allows every peer.
edit("rule-no-port", "-disk-builder", lambda d: d["spec"]["egress"][0].pop("ports"))
edit("rule-no-peer", "-operator", lambda d: d["spec"]["egress"][0].pop("to"))
# A metrics peer that matches Pods of every namespace.
def widen(d):
    for rule in d["spec"].get("ingress") or []:
        for peer in rule.get("from") or []:
            if "namespaceSelector" in peer:
                peer["namespaceSelector"] = {"matchExpressions": [{"key": "kubernetes.io/metadata.name", "operator": "Exists"}]}
edit("any-namespace", "-operator", widen)
PY
# The disk registry at an IP address needs no CIDR, and a fixture proves the
# checker reads it.
npender "$workdir/np-diskip.yaml" "${NP[@]}" --set 'launcher.diskRepo=10.43.0.9:5000/disks' \
	--set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs=null'
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/np-diskip.yaml" setec-system >"$workdir/np-diskip.out" \
	&& grep -qF 'cidr: "10.43.0.9/32"' "$workdir/np-diskip.yaml"; then
	pass "a disk repository at an IP address is reached with no CIDR value"
else
	fail "a disk repository at an IP address: $(cat "$workdir/np-diskip.out")"
fi
# A build policy that does not reach the disk registry (an IP address here,
# because the checker cannot read a name).
python3 - "$workdir/np-diskip.yaml" "$workdir/np-builder-no-disk-registry.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
for d in docs:
    if d.get("kind") == "NetworkPolicy" and d["metadata"]["name"].endswith("-disk-builder"):
        d["spec"]["egress"] = [e for e in d["spec"]["egress"]
                               if not any((t.get("ipBlock") or {}).get("cidr") == "10.43.0.9/32" for t in e["to"])]
yaml.safe_dump_all(docs, open(sys.argv[2], "w"))
PY
for fx in no-deny no-node-agent no-frontend no-disk-builder no-image-verify builder-world verify-world-v6 \
	rule-no-port rule-no-peer builder-no-disk-registry any-namespace; do
	if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/np-$fx.yaml" setec-system >/dev/null; then
		fail "networkpolicy fixture $fx: the checker passed a render with a defect"
	else
		pass "networkpolicy fixture $fx: the checker refuses it"
	fi
done
# A whole-family CIDR leaves out the reserved ranges, so a Job cannot reach the
# cluster networks through it.
npender "$workdir/np-public.yaml" "${NP[@]}" --set 'systemPolicy.networkPolicy.egress.imageSources.cidrs={0.0.0.0/0}'
if python3 "$(dirname "$0")/check-system-policy.py" "$workdir/np-public.yaml" setec-system >"$workdir/np-public.out" \
	&& grep -qF -- '- "10.0.0.0/8"' "$workdir/np-public.yaml"; then
	pass "a 0.0.0.0/0 destination leaves out the reserved ranges"
else
	fail "a 0.0.0.0/0 destination: $(cat "$workdir/np-public.out")"
fi
# An IP image source under this engine gets its address and port only.
npender "$workdir/np-ipsource.yaml" "${NP[@]}" --set 'launcher.imageSources[0].registry=10.43.0.5:5000'
if grep -qF 'cidr: "10.43.0.5/32"' "$workdir/np-ipsource.yaml" && grep -qF "port: 5000" "$workdir/np-ipsource.yaml"; then
	pass "an image source at an IP address with no hosts renders its address and port, with the networkpolicy engine"
else
	fail "an image source at an IP address renders no allow, with the networkpolicy engine"
fi
# Each input that the engine needs fails the render with a message that names it.
np_refused() {
	local desc="$1" want="$2"
	shift 2
	if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		--set systemPolicy.engine=networkpolicy "$@" >/dev/null 2>"$workdir/np-refused.err"; then
		fail "$desc must fail the render"
	elif grep -qF -- "$want" "$workdir/np-refused.err"; then
		pass "$desc fails the render"
	else
		fail "$desc failed with another message: $(tail -n1 "$workdir/np-refused.err")"
	fi
}
np_refused "no API server CIDR" "systemPolicy.networkPolicy.apiServer.cidrs is required" \
	--set 'systemPolicy.networkPolicy.egress.imageSources.cidrs={198.51.100.0/24}' \
	--set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs={203.0.113.5/32}'
np_refused "an image source at a name with no CIDR" "systemPolicy.networkPolicy.egress.imageSources.cidrs is required" \
	--set 'systemPolicy.networkPolicy.apiServer.cidrs={192.0.2.10/32}' \
	--set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs={203.0.113.5/32}'
np_refused "a disk registry at a name with no CIDR" "systemPolicy.networkPolicy.egress.diskRegistry.cidrs is required" \
	--set 'systemPolicy.networkPolicy.apiServer.cidrs={192.0.2.10/32}' \
	--set 'systemPolicy.networkPolicy.egress.imageSources.cidrs={198.51.100.0/24}'
np_refused "a snapshot store at a name with no CIDR" "systemPolicy.networkPolicy.egress.snapshotStore.cidrs is required" \
	"${NP[@]}" --set nodeAgent.enabled=true --set snapshots.enabled=true \
	--set snapshots.s3.enabled=true --set snapshots.s3.bucket=b --set snapshots.s3.pathStyle=false \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}'
np_refused "a value that is not a CIDR" "is not a CIDR" "${NP[@]}" \
	--set 'systemPolicy.networkPolicy.apiServer.cidrs={192.0.2.10}'
np_refused "an empty port list" "must list at least one port" "${NP[@]}" \
	--set 'systemPolicy.networkPolicy.egress.imageSources.ports=null'

# ---------------------------------------------------------------------------
# The DNS peer of the namespace deny (setec#390). Both engines read
# systemPolicy.dns. The default is kube-dns of kube-system. A changed peer
# replaces it in each engine, and a value that is not a peer fails the render.
# ---------------------------------------------------------------------------
note "the DNS peer of the setec namespace policy (setec#390)"
# dns_peers <render> <engine> prints the DNS peers of the default deny, one for
# each line: "pod <namespace> <labels>" or "cidr <cidr>".
dns_peers() {
	python3 - "$1" "$2" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
if sys.argv[2] == "cilium":
    deny = [d for d in docs if d.get("kind") == "CiliumClusterwideNetworkPolicy"][0]
    for rule in deny["spec"]["egress"]:
        for peer in rule.get("toEndpoints") or []:
            labels = dict(peer["matchLabels"])
            ns = labels.pop("k8s:io.kubernetes.pod.namespace")
            print("pod", ns, ",".join(f"{k.removeprefix('k8s:')}={v}" for k, v in sorted(labels.items())))
        for c in rule.get("toCIDR") or []:
            print("cidr", c)
        if not (rule.get("toEndpoints") or rule.get("toCIDR")):
            sys.exit("a DNS rule has no peer")
        for tp in rule["toPorts"]:
            if {(p["port"], p["protocol"]) for p in tp["ports"]} != {("53", "UDP"), ("53", "TCP")}:
                sys.exit("DNS ports are not 53 UDP and TCP")
else:
    deny = [d for d in docs if d.get("kind") == "NetworkPolicy" and d["metadata"]["name"].endswith("-default-deny")][0]
    for rule in deny["spec"]["egress"]:
        if not rule.get("to"):
            sys.exit("a DNS rule has no peer")
        for peer in rule["to"]:
            if "podSelector" in peer:
                ns = peer["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"]
                print("pod", ns, ",".join(f"{k}={v}" for k, v in sorted(peer["podSelector"]["matchLabels"].items())))
            else:
                print("cidr", peer["ipBlock"]["cidr"])
PY
}
dns_case() {
	local engine="$1" name="$2" want="$3"
	shift 3
	local extra=()
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	render "$workdir/dns-$engine-$name.yaml" "${extra[@]}" "$@"
	local got
	got="$(dns_peers "$workdir/dns-$engine-$name.yaml" "$engine" 2>&1)" || {
		fail "DNS peer, $engine, $name: $got"
		return
	}
	if [ "$got" = "$want" ]; then
		pass "DNS peer, $engine engine, $name"
	else
		fail "DNS peer, $engine engine, $name: got [$got], want [$want]"
	fi
}
for engine in cilium networkpolicy; do
	dns_case "$engine" default "pod kube-system k8s-app=kube-dns"
	dns_case "$engine" other-pods "pod dns-system app=my-dns,tier=dns" \
		--set systemPolicy.dns.namespace=dns-system \
		--set systemPolicy.dns.podLabels.app=my-dns --set systemPolicy.dns.podLabels.tier=dns
	dns_case "$engine" cidrs-only "cidr 169.254.20.10/32" \
		--set 'systemPolicy.dns.cidrs={169.254.20.10/32}'
	dns_case "$engine" pods-and-cidrs "$(printf 'pod dns-system app=my-dns\ncidr 169.254.20.10/32')" \
		--set systemPolicy.dns.namespace=dns-system --set systemPolicy.dns.podLabels.app=my-dns \
		--set 'systemPolicy.dns.cidrs={169.254.20.10/32}'
	# The path of version 0.131 still works, and wins.
	dns_case "$engine" old-path "pod dns-system app=my-dns" \
		--set systemPolicy.networkPolicy.dns.namespace=dns-system \
		--set systemPolicy.networkPolicy.dns.podLabels.app=my-dns
done
dns_refused() {
	local engine="$1" desc="$2" want="$3"
	shift 3
	local extra=(--set "systemPolicy.engine=$engine")
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		"${extra[@]}" "$@" >/dev/null 2>"$workdir/dns-refused.err"; then
		fail "$desc, $engine, must fail the render"
	elif grep -qF -- "$want" "$workdir/dns-refused.err"; then
		pass "$desc fails the render, $engine engine"
	else
		fail "$desc, $engine, failed with another message: $(tail -n1 "$workdir/dns-refused.err")"
	fi
}
for engine in cilium networkpolicy; do
	dns_refused "$engine" "a DNS namespace with no Pod labels" "systemPolicy.dns: set both namespace and podLabels" \
		--set systemPolicy.dns.namespace=dns-system
	dns_refused "$engine" "a DNS value that is not a CIDR" "systemPolicy.dns.cidrs" \
		--set 'systemPolicy.dns.cidrs={169.254.20.10}'
done

# ---------------------------------------------------------------------------
# The trace collector of the operator (setec#392). With tracing on, the
# operator exports to observability.otlpEndpoint, and the policy of the
# namespace must allow that destination and no other new one, under each
# engine. A render with tracing on is compared with the render with tracing
# off: the operator policy may gain exactly one egress rule.
# ---------------------------------------------------------------------------
note "the trace collector of the operator (setec#392)"
# otlp_extra <engine> <base render> <render> prints the egress rules that the
# operator policy gained, one for each line.
otlp_extra() {
	python3 - "$1" "$2" "$3" <<'PY'
import sys, yaml
engine = sys.argv[1]
def rules(path):
    docs = [d for d in yaml.safe_load_all(open(path)) if isinstance(d, dict)]
    if engine == "cilium":
        op = [d for d in docs if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"]["name"].endswith("-operator")][0]
    else:
        op = [d for d in docs if d.get("kind") == "NetworkPolicy" and d["metadata"]["name"].endswith("-operator")][0]
    return op["spec"]["egress"]
def describe(rule):
    if engine == "cilium":
        ports = ",".join(sorted({p["port"] for tp in rule.get("toPorts") or [] for p in tp["ports"]}))
        out = []
        for f in rule.get("toFQDNs") or []:
            out.append(f"fqdn {f['matchName']} {ports}")
        for c in rule.get("toCIDR") or []:
            out.append(f"cidr {c} {ports}")
        for e in rule.get("toEndpoints") or []:
            labels = dict(e["matchLabels"])
            ns = labels.pop("k8s:io.kubernetes.pod.namespace", None)
            out.append("pod %s %s %s" % (ns, ",".join(f"{k.removeprefix('k8s:')}={v}" for k, v in sorted(labels.items())), ports))
        for e in rule.get("toEntities") or []:
            out.append(f"entity {e}")
        return out
    ports = ",".join(sorted(str(p["port"]) for p in rule.get("ports") or []))
    out = []
    for peer in rule.get("to") or []:
        if "ipBlock" in peer:
            out.append(f"cidr {peer['ipBlock']['cidr']} {ports}")
        elif "podSelector" in peer and "namespaceSelector" in peer:
            out.append("pod %s %s %s" % (peer["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"],
                ",".join(f"{k}={v}" for k, v in sorted(peer["podSelector"]["matchLabels"].items())), ports))
        else:
            out.append("peer " + str(peer))
    if not rule.get("to"):
        out.append("EVERY PEER")
    return out
base = [d for r in rules(sys.argv[2]) for d in describe(r)]
new = [d for r in rules(sys.argv[3]) for d in describe(r)]
for b in base:
    if b not in new:
        sys.exit(f"the operator lost a destination: {b}")
extra = [n for n in new if n not in base]
print("\n".join(extra))
PY
}
# otlp_case <engine> <name> <want> <helm args...>
otlp_case() {
	local engine="$1" name="$2" want="$3"
	shift 3
	local extra=()
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	render "$workdir/otlp-$engine-base.yaml" "${extra[@]}"
	render "$workdir/otlp-$engine-$name.yaml" "${extra[@]}" --set observability.enabled=true "$@"
	local got
	got="$(otlp_extra "$engine" "$workdir/otlp-$engine-base.yaml" "$workdir/otlp-$engine-$name.yaml" 2>&1)" || {
		fail "OTLP, $engine, $name: $got"
		return
	}
	if [ "$got" = "$want" ]; then
		pass "OTLP collector, $engine engine, $name: the operator policy gains [${want:-nothing}]"
	else
		fail "OTLP collector, $engine engine, $name: got [$got], want [$want]"
	fi
}
otlp_case cilium host "fqdn otel.example.com 4317" --set observability.otlpEndpoint=otel.example.com:4317
otlp_case cilium host-no-port "fqdn otel.example.com 443" --set observability.otlpEndpoint=https://otel.example.com
otlp_case cilium ip "cidr 10.1.2.3/32 4317" --set observability.otlpEndpoint=10.1.2.3:4317
otlp_case cilium ipv6 "cidr 2001:db8::5/128 4317" --set 'observability.otlpEndpoint=[2001:db8::5]:4317'
otlp_case cilium pods "pod monitoring app=otel 4317" --set observability.otlpEndpoint=otel-collector.monitoring.svc:4317 \
	--set systemPolicy.otlp.namespace=monitoring --set systemPolicy.otlp.podLabels.app=otel
otlp_case networkpolicy host "cidr 198.51.100.0/24 4317" --set observability.otlpEndpoint=otel.example.com:4317 \
	--set 'systemPolicy.networkPolicy.egress.otlp.cidrs={198.51.100.0/24}'
otlp_case networkpolicy host-ports "cidr 198.51.100.0/24 4318" --set observability.otlpEndpoint=otel.example.com:4317 \
	--set 'systemPolicy.networkPolicy.egress.otlp.cidrs={198.51.100.0/24}' --set 'systemPolicy.networkPolicy.egress.otlp.ports={4318}'
otlp_case networkpolicy ip "cidr 10.1.2.3/32 4317" --set observability.otlpEndpoint=10.1.2.3:4317
otlp_case networkpolicy pods "pod monitoring app=otel 4317" --set observability.otlpEndpoint=otel-collector.monitoring.svc:4317 \
	--set systemPolicy.otlp.namespace=monitoring --set systemPolicy.otlp.podLabels.app=otel
# With tracing off, or with no endpoint, the policy gains nothing.
for engine in cilium networkpolicy; do
	otlp_case "$engine" off "" --set observability.enabled=false --set observability.otlpEndpoint=otel.example.com:4317
	otlp_case "$engine" no-endpoint ""
done
otlp_refused() {
	local engine="$1" desc="$2" want="$3"
	shift 3
	local extra=(--set "systemPolicy.engine=$engine")
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		"${extra[@]}" --set observability.enabled=true "$@" >/dev/null 2>"$workdir/otlp-refused.err"; then
		fail "$desc, $engine, must fail the render"
	elif grep -qF -- "$want" "$workdir/otlp-refused.err"; then
		pass "$desc fails the render, $engine engine"
	else
		fail "$desc, $engine, failed with another message: $(tail -n1 "$workdir/otlp-refused.err")"
	fi
}
otlp_refused cilium "a Service name with no collector Pods" "systemPolicy.otlp" \
	--set observability.otlpEndpoint=otel-collector.monitoring.svc:4317
otlp_refused networkpolicy "a host with no CIDR" "systemPolicy.networkPolicy.egress.otlp.cidrs is required" \
	--set observability.otlpEndpoint=otel.example.com:4317
otlp_refused networkpolicy "a CIDR value that is not a CIDR" "systemPolicy.networkPolicy.egress.otlp.cidrs" \
	--set observability.otlpEndpoint=otel.example.com:4317 --set 'systemPolicy.networkPolicy.egress.otlp.cidrs={198.51.100.7}'
for engine in cilium networkpolicy; do
	otlp_refused "$engine" "a collector namespace with no Pod labels" "systemPolicy.otlp: set both namespace and podLabels" \
		--set observability.otlpEndpoint=otel.example.com:4317 --set systemPolicy.otlp.namespace=monitoring
	otlp_refused "$engine" "an endpoint with a bad port" "observability.otlpEndpoint" \
		--set observability.otlpEndpoint=otel.example.com:port
done

# Push access of the disk Jobs (setec#393). The check and publish containers
# push to the disk repository with a cloud identity of their own. The build
# container never holds it. The Job is made by the operator, so the chart
# renders its flags and its egress, and the unit tests of internal/controller
# check the Job shape.
# ---------------------------------------------------------------------------
note "disk Job push access (setec#393)"
PUSH_ROLE=arn:aws:iam::123456789012:role/setec-push
ECR_DISK=(--set launcher.diskRepo=123456789012.dkr.ecr.eu-west-1.amazonaws.com/setec-disks
	--set 'launcher.imageSources[0].registry=ghcr.io' --set 'launcher.imageSources[0].hosts={ghcr.io}')
PUSH=(--set 'launcher.diskBuilder.push.providers={ecr}' --set "launcher.diskBuilder.push.aws.roleArn=$PUSH_ROLE")
render "$workdir/push-default.yaml" "${ECR_DISK[@]}"
assert_absent "$workdir/push-default.yaml" "no push flag by default" "--disk-push-"
assert_absent "$workdir/push-default.yaml" "no ECR host is allowed by default" "api.ecr."
render "$workdir/push-set.yaml" "${ECR_DISK[@]}" "${PUSH[@]}"
assert_contains "$workdir/push-set.yaml" "the operator gets the push provider, the role and the audience" \
	"--disk-push-provider=ecr" \
	"--disk-push-aws-role-arn=$PUSH_ROLE" \
	"--disk-push-aws-audience=setec-disk-push"
assert_absent "$workdir/push-set.yaml" "the push role is not the pull role" "--disk-pull-aws-role-arn"
assert_contains "$workdir/push-set.yaml" "the cilium engine allows the ECR API and the STS endpoint of the region of the disk repository" \
	'matchName: "api.ecr.eu-west-1.amazonaws.com"' \
	'matchName: "sts.eu-west-1.amazonaws.com"'
# The two hosts reach the disk build Job only. No other policy gets them.
if python3 - "$workdir/push-set.yaml" >"$workdir/push-hosts.out" 2>&1 <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
hosts = {"api.ecr.eu-west-1.amazonaws.com", "sts.eu-west-1.amazonaws.com"}
for d in docs:
    if d.get("kind") != "CiliumNetworkPolicy":
        continue
    got = {f["matchName"] for e in d["spec"].get("egress") or [] for f in e.get("toFQDNs") or []}
    name = d["metadata"]["name"]
    if name.endswith("-disk-builder"):
        if not hosts <= got:
            sys.exit(f"{name} lacks {hosts - got}")
    elif hosts & got:
        sys.exit(f"{name} reaches {hosts & got}")
PY
then
	pass "the ECR hosts reach the disk build Job only"
else
	fail "the ECR hosts reach a Pod that has no use for them: $(cat "$workdir/push-hosts.out")"
fi
render "$workdir/push-hosts.yaml" "${ECR_DISK[@]}" "${PUSH[@]}" --set 'launcher.diskBuilder.push.hosts={ecr-fips.example.test,sts.eu-west-1.amazonaws.com}'
assert_contains "$workdir/push-hosts.yaml" "an extra push host is allowed" 'matchName: "ecr-fips.example.test"'
if [ "$(grep -cF 'matchName: "sts.eu-west-1.amazonaws.com"' "$workdir/push-hosts.yaml")" -eq 1 ]; then
	pass "a host that is derived and listed is allowed once"
else
	fail "a host that is derived and listed appears more than once"
fi
render "$workdir/push-np.yaml" "${ECR_DISK[@]}" "${PUSH[@]}" "${NP[@]}" --set 'systemPolicy.networkPolicy.egress.diskRegistry.cidrs={0.0.0.0/0}'
assert_absent "$workdir/push-np.yaml" "the networkpolicy engine renders no Cilium object with a push identity" "cilium.io"
assert_contains "$workdir/push-np.yaml" "the operator gets the push flags under the networkpolicy engine" "--disk-push-provider=ecr"
render "$workdir/push-both.yaml" "${ECR_DISK[@]}" "${PUSH[@]}" --set 'launcher.diskBuilder.pull.providers={ecr}' \
	--set launcher.diskBuilder.pull.aws.roleArn=arn:aws:iam::123456789012:role/setec-pull
assert_contains "$workdir/push-both.yaml" "the pull role and the push role are two flags" \
	"--disk-pull-aws-role-arn=arn:aws:iam::123456789012:role/setec-pull" "--disk-push-aws-role-arn=$PUSH_ROLE"
disk_push_refused() {
	local desc="$1" want="$2"
	shift 2
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={${NS_A}}" "$@" >/dev/null 2>"$workdir/push-refused.err"; then
		fail "$desc must fail the render"
	elif grep -qF -- "$want" "$workdir/push-refused.err"; then
		pass "$desc fails the render"
	else
		fail "$desc failed with another message: $(tail -n1 "$workdir/push-refused.err")"
	fi
}
disk_push_refused "a push provider with no role" "launcher.diskBuilder.push.aws.roleArn is required" \
	--set 'launcher.diskBuilder.push.providers={ecr}'
disk_push_refused "a push role with no provider" "needs launcher.diskBuilder.push.providers" \
	--set "launcher.diskBuilder.push.aws.roleArn=$PUSH_ROLE"
disk_push_refused "an unknown push provider" "is not a known provider" \
	--set 'launcher.diskBuilder.push.providers={gcr}'
disk_push_refused "a push role that is not an ARN" "must be an IAM role ARN" \
	--set 'launcher.diskBuilder.push.providers={ecr}' --set launcher.diskBuilder.push.aws.roleArn=push
disk_push_refused "the pull audience for the push token" "other than sts.amazonaws.com" "${PUSH[@]}" \
	--set launcher.diskBuilder.push.aws.audience=sts.amazonaws.com
disk_push_refused "an empty push audience" "other than sts.amazonaws.com" "${PUSH[@]}" \
	--set launcher.diskBuilder.push.aws.audience=
disk_push_refused "a push role with no ServiceAccount" "needs a named ServiceAccount" "${PUSH[@]}" \
	--set launcher.diskBuilder.serviceAccount.create=false

# ---------------------------------------------------------------------------
# The release name reaches the operator, which puts it on each disk Job, Pod
# and ConfigMap as app.kubernetes.io/instance (the unit tests of
# internal/controller check the labels).
# ---------------------------------------------------------------------------
note "disk Job labels"
render "$workdir/instance.yaml" --show-only templates/deployment.yaml
assert_contains "$workdir/instance.yaml" "the operator gets the release name for the labels of its Jobs" "--instance-name=setec"
# Placement of the disk Job Pods (the operator makes them, so the chart passes
# the placement as one flag, and the unit tests of internal/controller check
# the Pods).
# ---------------------------------------------------------------------------
note "disk Job placement"
render "$workdir/placement-default.yaml" --show-only templates/deployment.yaml
assert_absent "$workdir/placement-default.yaml" "no placement flag by default" "--disk-job-placement"
render "$workdir/placement-set.yaml" --show-only templates/deployment.yaml \
	--set launcher.diskBuilder.nodeSelector.pool=sandbox \
	--set 'launcher.diskBuilder.tolerations[0].key=sandbox' --set 'launcher.diskBuilder.tolerations[0].operator=Exists' \
	--set launcher.diskBuilder.priorityClassName=jobs
python3 - "$workdir/placement-set.yaml" >"$workdir/placement-set.out" 2>&1 <<'PY' && pass "the operator gets the placement of the disk Jobs as JSON" || fail "disk Job placement: $(cat "$workdir/placement-set.out")"
import json, sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if isinstance(d, dict) and d.get("kind") == "Deployment":
        args = [a for c in d["spec"]["template"]["spec"]["containers"] for a in c["args"] if a.startswith("--disk-job-placement=")]
        if len(args) != 1:
            sys.exit(f"flags: {args}")
        got = json.loads(args[0].split("=", 1)[1])
        want = {"nodeSelector": {"pool": "sandbox"}, "tolerations": [{"key": "sandbox", "operator": "Exists"}], "priorityClassName": "jobs"}
        if got != want:
            sys.exit(f"got {got}, want {want}")
PY
# Pull Secrets of the images of the chart. Each Pod that the chart
# runs pulls its image with imagePullSecrets, not the operator alone. The disk
# Jobs get the same names from the operator flag --image-pull-secret.
# ---------------------------------------------------------------------------
note "image pull Secrets reach each workload"
render "$workdir/ips-default.yaml" "${FE[@]}" --set nodeAgent.enabled=true
assert_absent "$workdir/ips-default.yaml" "no pull Secret by default" "imagePullSecrets"
assert_absent "$workdir/ips-default.yaml" "no pull Secret flag by default" "--image-pull-secret"
render "$workdir/ips-set.yaml" "${FE[@]}" --set nodeAgent.enabled=true \
	--set 'imagePullSecrets[0].name=registry-a' --set 'imagePullSecrets[1].name=registry-b'
python3 - "$workdir/ips-set.yaml" >"$workdir/ips-set.out" 2>&1 <<'PY' && pass "the operator, frontend, node agent and device plugin name the pull Secrets" || fail "pull Secrets: $(cat "$workdir/ips-set.out")"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if isinstance(d, dict)]
want = [{"name": "registry-a"}, {"name": "registry-b"}]
seen = set()
for d in docs:
    if d.get("kind") not in ("Deployment", "DaemonSet"):
        continue
    comp = d["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/component"]
    got = d["spec"]["template"]["spec"].get("imagePullSecrets")
    if got != want:
        sys.exit(f"{comp}: imagePullSecrets = {got}")
    seen.add(comp)
need = {"operator", "frontend", "node-agent", "device-plugin"}
if seen != need:
    sys.exit(f"workloads with pull Secrets {sorted(seen)}, want {sorted(need)}")
PY
assert_contains "$workdir/ips-set.yaml" "the operator gets the names for the disk Job Pods" \
	"--image-pull-secret=registry-a" "--image-pull-secret=registry-b"


# ---------------------------------------------------------------------------
# The secret scan of a warm base (setec#412). The node agent takes the patterns
# that an operator states as safe, one flag for each.
# ---------------------------------------------------------------------------
note "secret scan allow list"
render "$workdir/scan-default.yaml" "${SP_ALL[@]}"
assert_absent "$workdir/scan-default.yaml" "no allow flag by default" "--secret-scan-allow"
render "$workdir/scan-set.yaml" "${SP_ALL[@]}" \
	--set 'snapshots.secretScan.allow={secret-shaped-env-assignment=SecretKeyRef:.*,TESTKEY[0-9]+}'
assert_contains "$workdir/scan-set.yaml" "the node agent gets one flag for each pattern" \
	'--secret-scan-allow=secret-shaped-env-assignment=SecretKeyRef:.*' '--secret-scan-allow=TESTKEY[0-9]+'

# ---------------------------------------------------------------------------
# The snapshot store of the node agent (setec#402). The node agent writes to
# snapshots.s3.endpoint, and the policy of the setec namespace must allow that
# destination and no other, under each engine. A store in the cluster is a Pod
# peer, because a policy by host name cannot match a Service. The check reads
# the whole egress of the node agent policy and compares it with the wanted
# destinations.
# ---------------------------------------------------------------------------
note "the snapshot store of the node agent (setec#402)"
STORE_BASE=(--set nodeAgent.enabled=true --set snapshots.enabled=true
	--set snapshots.s3.enabled=true --set snapshots.s3.bucket=setec-snapshots --set snapshots.s3.pathStyle=false
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}')
# store_egress <engine> <render> prints the egress destinations of the node
# agent policy, one for each line.
store_egress() {
	python3 - "$1" "$2" <<'PY'
import sys, yaml
engine, path = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(path)) if isinstance(d, dict)]
kind = "CiliumNetworkPolicy" if engine == "cilium" else "NetworkPolicy"
np = [d for d in docs if d.get("kind") == kind and d["metadata"]["name"].endswith("-node-agent")][0]
out = []
for rule in np["spec"].get("egress") or []:
    if engine == "cilium":
        ports = ",".join(sorted({p["port"] for tp in rule.get("toPorts") or [] for p in tp["ports"]}))
        for f in rule.get("toFQDNs") or []:
            out.append(f"fqdn {f['matchName']} {ports}")
        for c in rule.get("toCIDR") or []:
            out.append(f"cidr {c} {ports}")
        for e in rule.get("toEndpoints") or []:
            labels = dict(e["matchLabels"])
            ns = labels.pop("k8s:io.kubernetes.pod.namespace", None)
            out.append("pod %s %s %s" % (ns, ",".join(f"{k.removeprefix('k8s:')}={v}" for k, v in sorted(labels.items())), ports))
        for e in rule.get("toEntities") or []:
            out.append(f"entity {e}")
        continue
    ports = ",".join(sorted(str(p["port"]) for p in rule.get("ports") or []))
    for peer in rule.get("to") or []:
        if "ipBlock" in peer:
            out.append(f"cidr {peer['ipBlock']['cidr']} {ports}")
        elif "podSelector" in peer and "namespaceSelector" in peer:
            out.append("pod %s %s %s" % (peer["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"],
                ",".join(f"{k}={v}" for k, v in sorted(peer["podSelector"]["matchLabels"].items())), ports))
        else:
            out.append("peer " + str(peer))
    if not rule.get("to"):
        out.append("EVERY PEER")
print("\n".join(sorted(out)))
PY
}
# store_case <engine> <name> <want> <helm args...>
store_case() {
	local engine="$1" name="$2" want="$3"
	shift 3
	local extra=()
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	render "$workdir/store-$engine-$name.yaml" "${extra[@]}" "${STORE_BASE[@]}" "$@"
	local got
	got="$(store_egress "$engine" "$workdir/store-$engine-$name.yaml" 2>&1)" || {
		fail "snapshot store, $engine, $name: $got"
		return
	}
	if [ "$got" = "$want" ]; then
		pass "snapshot store, $engine engine, $name: the node agent reaches exactly [${want//$'\n'/; }]"
	else
		fail "snapshot store, $engine engine, $name: got [${got//$'\n'/; }], want [${want//$'\n'/; }]"
	fi
}
store_case cilium host "fqdn s3.example.com 9000" --set snapshots.s3.endpoint=http://s3.example.com:9000
store_case cilium host-no-port "fqdn s3.example.com 443" --set snapshots.s3.endpoint=https://s3.example.com
store_case cilium host-http-no-port "fqdn s3.example.com 80" --set snapshots.s3.endpoint=http://s3.example.com
store_case cilium ip "cidr 10.1.2.3/32 9000" --set snapshots.s3.endpoint=http://10.1.2.3:9000
store_case cilium ipv6 "cidr 2001:db8::5/128 9000" --set 'snapshots.s3.endpoint=http://[2001:db8::5]:9000'
store_case cilium pods "pod minio app=minio 9000" --set snapshots.s3.endpoint=http://minio.minio.svc:9000 \
	--set systemPolicy.snapshotStore.namespace=minio --set systemPolicy.snapshotStore.podLabels.app=minio
store_case cilium real-s3 "$(printf 'fqdn s3.us-east-1.amazonaws.com 443\nfqdn setec-snapshots.s3.us-east-1.amazonaws.com 443')"
store_case networkpolicy host "cidr 198.51.100.0/24 9000" --set snapshots.s3.endpoint=http://s3.example.com:9000 \
	--set 'systemPolicy.networkPolicy.egress.snapshotStore.cidrs={198.51.100.0/24}'
store_case networkpolicy host-ports "cidr 198.51.100.0/24 9001" --set snapshots.s3.endpoint=http://s3.example.com:9000 \
	--set 'systemPolicy.networkPolicy.egress.snapshotStore.cidrs={198.51.100.0/24}' \
	--set 'systemPolicy.networkPolicy.egress.snapshotStore.ports={9001}'
store_case networkpolicy ip "cidr 10.1.2.3/32 9000" --set snapshots.s3.endpoint=http://10.1.2.3:9000
store_case networkpolicy pods "pod minio app=minio 9000" --set snapshots.s3.endpoint=http://minio.minio.svc:9000 \
	--set systemPolicy.snapshotStore.namespace=minio --set systemPolicy.snapshotStore.podLabels.app=minio
store_refused() {
	local engine="$1" desc="$2" want="$3"
	shift 3
	local extra=(--set "systemPolicy.engine=$engine")
	[ "$engine" = networkpolicy ] && extra=("${NP[@]}")
	if "$HELM" template setec "$CHART_DIR" "${CILIUM_API[@]}" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true \
		"${extra[@]}" "${STORE_BASE[@]}" "$@" >/dev/null 2>"$workdir/store-refused.err"; then
		fail "$desc, $engine, must fail the render"
	elif grep -qF -- "$want" "$workdir/store-refused.err"; then
		pass "$desc fails the render, $engine engine"
	else
		fail "$desc, $engine, failed with another message: $(tail -n1 "$workdir/store-refused.err")"
	fi
}
store_refused cilium "a Service name with no store Pods" "systemPolicy.snapshotStore" \
	--set snapshots.s3.endpoint=http://minio.minio.svc:9000
store_refused cilium "a Service name with the full cluster domain and no store Pods" "systemPolicy.snapshotStore" \
	--set snapshots.s3.endpoint=http://minio.minio.svc.cluster.local:9000
store_refused networkpolicy "a host with no CIDR and no Pods" "systemPolicy.networkPolicy.egress.snapshotStore.cidrs is required" \
	--set snapshots.s3.endpoint=http://s3.example.com:9000
for engine in cilium networkpolicy; do
	store_refused "$engine" "a store namespace with no Pod labels" "systemPolicy.snapshotStore: set both namespace and podLabels" \
		--set snapshots.s3.endpoint=http://s3.example.com:9000 --set systemPolicy.snapshotStore.namespace=minio
	store_refused "$engine" "an endpoint with a bad port" "snapshots.s3.endpoint" \
		--set snapshots.s3.endpoint=http://s3.example.com:port
done

printf '\n'
if [ "$fail_count" -ne 0 ]; then
	printf 'verify-chart-security: %d assertion(s) failed\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-security: all assertions passed\n'
