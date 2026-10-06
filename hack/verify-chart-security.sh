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
	"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
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

# ---------------------------------------------------------------------------
# The node agent has no Kubernetes credentials (setec#198).
#
# The agent is root and privileged: it reads and writes the work volumes of
# the launcher Pods on the host. What bounds it is that it calls no
# Kubernetes API, so it has no token and no RBAC, and it mounts no more of
# the host than the kubelet Pod directory and its own snapshot directories.
# ---------------------------------------------------------------------------
render "$workdir/nodeagent.yaml" --set nodeAgent.enabled=true --set snapshots.enabled=true \
	--set snapshots.mTLS.caProvided=true --show-only templates/daemonset.yaml
strip_comments "$workdir/nodeagent.yaml" "$workdir/nodeagent.stripped.yaml"
note "node agent has no Kubernetes credentials (setec#198)"
assert_contains "$workdir/nodeagent.stripped.yaml" "the node agent mounts no ServiceAccount token" \
	"automountServiceAccountToken: false" \
	"hostNetwork: false"
assert_absent "$workdir/nodeagent.stripped.yaml" "the node agent has no ClusterRole" "kind: ClusterRole"
assert_absent "$workdir/nodeagent.stripped.yaml" "the node agent does not mount the host /dev" "path: /dev"
assert_absent "$workdir/nodeagent.stripped.yaml" "the node agent does not mount the host /run" "path: /run"

# ---------------------------------------------------------------------------
# The launcher is the only runtime (setec#198).
#
# The Kata, gVisor and runc backends and the node installer were removed.
# None of their objects may come back in a render: a RuntimeClass, an
# installer or runtime-agent DaemonSet, or a runtimes ConfigMap.
# ---------------------------------------------------------------------------
note "the launcher is the only runtime (setec#198)"
render "$workdir/all-on.yaml" --set nodeAgent.enabled=true --set snapshots.enabled=true \
	--set snapshots.mTLS.caProvided=true --set webhook.enabled=true
for needle in "kind: RuntimeClass" "app.kubernetes.io/component: installer" \
	"app.kubernetes.io/component: runtime-agent" "setec-runtimes" "--runtimes-config"; do
	assert_absent "$workdir/all-on.yaml" "a render has no object of a removed backend" "$needle"
done
assert_contains "$workdir/all-on.yaml" "the operator gets the launcher flags" \
	"--launcher-image=" \
	"--disk-repo=" \
	"--disk-public-key="
if "$HELM" template setec "$CHART_DIR" \
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
FE_TLS=(--set frontend.enabled=true
	--set frontend.tlsCertSecretName=fe-tls
	--set frontend.tlsClientCASecretName=fe-ca
	--set 'frontend.clients[0].name=saas'
	--set 'frontend.clients[0].spiffeID=spiffe://example.org/ns/gibson/sa/gibson-daemon'
	--set 'frontend.clients[1].name=onprem'
	--set 'frontend.clients[1].spiffeID=spiffe://onprem.example/ns/gibson/sa/gibson-daemon')

render "$workdir/fe-default.yaml" "${FE_TLS[@]}" \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-default.yaml" "$workdir/fe-default.stripped.yaml"
assert_contains "$workdir/fe-default.stripped.yaml" "each enrolled client reaches the frontend" \
	"--client=saas=spiffe://example.org/ns/gibson/sa/gibson-daemon" \
	"--client=onprem=spiffe://onprem.example/ns/gibson/sa/gibson-daemon"
assert_absent "$workdir/fe-default.stripped.yaml" "no fixed shared namespace" \
	"--sandbox-namespace"
assert_absent "$workdir/fe-default.stripped.yaml" "no tenant label override" \
	"--tenant-namespace-label"

render "$workdir/fe-scope.yaml" "${FE_TLS[@]}"
strip_comments "$workdir/fe-scope.yaml" "$workdir/fe-scope.stripped.yaml"
assert_contains "$workdir/fe-scope.stripped.yaml" "the frontend gets the pair grants and the scope policy (setec#207)" \
	"--pair-namespace-grant=setec-sandbox-namespace=" \
	"--pair-namespace-grant=setec-frontend-exec=" \
	"name: setec-frontend-scope" \
	"name: setec-sandbox-host-guard-pairs" \
	"setec.zeroroot.ai/sandbox-namespace: \"true\""
if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" --set webhook.certManager.enabled=true "${FE_TLS[@]}" >/dev/null 2>&1; then
	pass "a frontend install renders with no static sandboxNamespaces"
else
	fail "a frontend install must render with no static sandboxNamespaces: the frontend makes the pair namespaces"
fi

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set frontend.enabled=true \
	--set frontend.tlsCertSecretName=fe-tls \
	--set frontend.tlsClientCASecretName=fe-ca \
	>/dev/null 2>&1; then
	fail "a frontend with no enrolled client must fail the render"
else
	pass "a frontend with no enrolled client fails the render"
fi

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set frontend.enabled=true \
	--set frontend.tlsCertSecretName=fe-tls \
	--set frontend.tlsClientCASecretName=fe-ca \
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

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set 'netpol.reservedCIDRs={10.0.0.0/8,169.254.0.0/16}' \
	>/dev/null 2>&1; then
	fail "an IPv4-only reserved list must fail the render"
else
	pass "an IPv4-only reserved list fails the render"
fi

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
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

render "$workdir/fe-rbac.yaml" "${FE_TLS[@]}" --show-only templates/frontend.yaml
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
render "$workdir/fe-rbac-wide.yaml" "${FE_TLS[@]}" \
	--set rbac.allowClusterWideSandboxWrite=true \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-rbac-wide.yaml" "$workdir/fe-rbac-wide.stripped.yaml"
if rbac_index "$workdir/fe-rbac-wide.stripped.yaml" | grep -q "^ClusterRoleBinding setec-frontend-exec - setec-frontend-exec"; then
	pass "rbac.allowClusterWideSandboxWrite=true binds pods/exec cluster-wide, on the record"
else
	fail "rbac.allowClusterWideSandboxWrite=true must bind pods/exec with a ClusterRoleBinding"
fi

# ---------------------------------------------------------------------------
# Operator<->node-agent mTLS channel is issued from one trust root (setec#14).
#
# snapshots.enabled=true mounts three Secrets across two workloads, none of
# them optional. The chart used to issue only the two leaves, each its own
# root, and never the CA both workloads mount, so no values combination could
# install. cert-manager mode must now issue a CA Certificate into caSecret, a
# namespaced Issuer over it, and both leaves from that Issuer. The workloads
# must mount only ca.crt, because the CA Secret also holds the CA key.
# ---------------------------------------------------------------------------
note "node-agent mTLS channel from one CA (setec#14)"
SNAP_CM=(--set snapshots.enabled=true
	--set nodeAgent.enabled=true
	--set snapshots.mTLS.certManager.enabled=true)

render "$workdir/na-certs.yaml" "${SNAP_CM[@]}" --show-only templates/nodeagent-certificates.yaml
strip_comments "$workdir/na-certs.yaml" "$workdir/na-certs.stripped.yaml"
assert_contains "$workdir/na-certs.stripped.yaml" "cert-manager mode issues the CA into caSecret" \
	"kind: Certificate" \
	"name: setec-nodeagent-ca" \
	"isCA: true" \
	"secretName: setec-nodeagent-ca"
assert_contains "$workdir/na-certs.stripped.yaml" "a namespaced CA Issuer reads caSecret" \
	"kind: Issuer" \
	"    secretName: setec-nodeagent-ca"
# Both leaves must chain to the CA Issuer, never to the bootstrap issuer.
leaf_refs="$(awk '/^kind: Certificate/ {c=1} /^---/ {c=0} c && /^  issuerRef:/ {r=1; next} r && /^    (kind|name):/ {sub(/^ +/, ""); print; next} r && !/^    / {r=0}' "$workdir/na-certs.stripped.yaml" | sort | uniq -c | sed 's/^ *//')"
if [ "$leaf_refs" = "$(printf '1 kind: ClusterIssuer\n2 kind: Issuer\n1 name: selfsigned\n2 name: setec-nodeagent-ca')" ]; then
	pass "both leaves chain to the CA Issuer and only the CA uses the bootstrap issuer"
else
	fail "leaf issuerRefs are wrong (want two Issuer/setec-nodeagent-ca, one bootstrap for the CA); got: $(printf '%s' "$leaf_refs" | tr '\n' ';')"
fi

# Every Secret a workload mounts for the channel must be one the chart issues.
render "$workdir/na-operator.yaml" "${SNAP_CM[@]}" --show-only templates/deployment.yaml
render "$workdir/na-agent.yaml" "${SNAP_CM[@]}" --show-only templates/daemonset.yaml
issued="$(sed 's/[[:space:]]*#.*$//' "$workdir/na-certs.yaml" | awk '/^  secretName:/ {print $2}' | sort -u)"
for f in na-operator na-agent; do
	strip_comments "$workdir/$f.yaml" "$workdir/$f.stripped.yaml"
	mounted="$(awk '/^        - name: nodeagent-(tls|ca)$/ {m=1; next} m && /secretName:/ {print $2; m=0}' "$workdir/$f.stripped.yaml" | sort -u)"
	missing=""
	for s in $mounted; do
		printf '%s\n' "$issued" | grep -qx "$s" || missing="$missing $s"
	done
	if [ -n "$mounted" ] && [ -z "$missing" ]; then
		pass "$f mounts only Secrets the chart issues ($(printf '%s' "$mounted" | tr '\n' ' '))"
	else
		fail "$f mounts a Secret the chart does not issue:${missing:- (no nodeagent mounts found)}"
	fi
	if awk '/^        - name: nodeagent-ca$/ {m=1; next} m && /^        - name:/ {exit 1} m && /key: ca.crt/ {found=1} END {exit !found}' "$workdir/$f.stripped.yaml"; then
		pass "$f mounts only ca.crt from the CA Secret"
	else
		fail "$f must mount only the ca.crt item of the CA Secret (it also holds the CA key)"
	fi
done

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	"${SNAP_CM[@]}" --set snapshots.mTLS.caProvided=true \
	>/dev/null 2>&1; then
	fail "caProvided=true with certManager.enabled=true must fail the render (the knob would be ignored)"
else
	pass "caProvided=true with certManager.enabled=true fails the render"
fi

if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set snapshots.enabled=true --set nodeAgent.enabled=true \
	>/dev/null 2>&1; then
	fail "snapshots without cert-manager and without caProvided must fail the render"
else
	pass "snapshots without cert-manager and without caProvided fails the render"
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
        runtime: {backend: kata-fc}
        defaultNetworkMode: external-only
        default: true
    - name: agent
      spec:
        runtime: {backend: kata-fc}
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
	if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
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

printf '\n'
if [ "$fail_count" -ne 0 ]; then
	printf 'verify-chart-security: %d assertion(s) failed\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-security: all assertions passed\n'
