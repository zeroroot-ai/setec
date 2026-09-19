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
	"$HELM" template setec "$CHART_DIR" \
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
# runtime-agent least privilege (GHSA-p8f8-3qpw-7h93).
#
# The agent's `nodes: patch` grant is only tolerable because admission
# narrows it. If the policy stopped rendering, or nodes/status came back,
# the chart would still install and the narrowing would be gone.
# ---------------------------------------------------------------------------
render "$workdir/agent-rbac.yaml" --show-only templates/runtime-agent-rbac.yaml
render "$workdir/agent-ds.yaml" --show-only templates/runtime-agent-daemonset.yaml
render "$workdir/agent-guard.yaml" --show-only templates/runtime-agent-node-guard.yaml

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

note "runtime-agent least privilege (GHSA-p8f8-3qpw-7h93)"
# The rule, not the prose: the ClusterRole comment explains why the grant
# was dropped, so a bare substring match would fail on its own rationale.
assert_absent "$workdir/agent-rbac.yaml" "agent holds no nodes/status grant" \
	'resources: ["nodes/status"]'
assert_contains "$workdir/agent-ds.yaml" "agent runs as a verified non-root user" \
	"runAsNonRoot: true" \
	"runAsUser: 65532" \
	"type: RuntimeDefault"
assert_absent "$workdir/agent-ds.yaml" "agent is not permitted to run as root" \
	"runAsNonRoot: false"
assert_contains "$workdir/agent-guard.yaml" "node-write guard is rendered and denies" \
	"kind: ValidatingAdmissionPolicy" \
	"name: setec-runtime-agent-node-guard" \
	"- Deny"
assert_contains "$workdir/agent-guard.yaml" "guard is scoped to the agent ServiceAccount" \
	"system:serviceaccount:setec-system:setec-runtime-agent"
assert_contains "$workdir/agent-guard.yaml" "guard pins the writable key set" \
	"'setec.zeroroot.ai/runtime.'" \
	"'setec.zeroroot.ai/runtime-probe'" \
	"object.spec == oldObject.spec"
assert_contains "$workdir/agent-guard.yaml" "guard checks node identity when the cluster supplies it" \
	"authentication.kubernetes.io/node-name"

# requireNodeIdentity must flip the expression from opportunistic to
# mandatory. A toggle that renders the same policy either way is worse
# than no toggle: it reads as a control and is not one.
render "$workdir/agent-guard-strict.yaml" \
	--set runtimeAgent.nodeGuard.requireNodeIdentity=true \
	--show-only templates/runtime-agent-node-guard.yaml
assert_absent "$workdir/agent-guard-strict.yaml" "requireNodeIdentity removes the absent-claim escape" \
	"!has(request.userInfo.extra)"

render "$workdir/guard-off-agent.yaml" --set runtimeAgent.nodeGuard.enabled=false
assert_absent "$workdir/guard-off-agent.yaml" "node guard is omitted when disabled" \
	"setec-runtime-agent-node-guard"

# ---------------------------------------------------------------------------
# Portable node installer (ADR-0003, setec#187).
#
# The installer is privileged by design (it writes host files and
# restarts containerd — that is the product). What bounds its blast
# radius is that it carries NO Kubernetes credentials: a compromised
# installer pod is one node, never the cluster API. These assertions
# pin that property, plus the containment of hostNetwork.
# ---------------------------------------------------------------------------
render "$workdir/installer.yaml" --show-only templates/installer-daemonset.yaml

note "portable node installer (ADR-0003, setec#187)"
assert_contains "$workdir/installer.yaml" "installer DaemonSet is rendered by default" \
	"kind: DaemonSet" \
	"app.kubernetes.io/component: installer"
assert_contains "$workdir/installer.yaml" "installer mounts no ServiceAccount token" \
	"automountServiceAccountToken: false"
assert_absent "$workdir/installer.yaml" "installer names no ServiceAccount" \
	"serviceAccountName:"
assert_contains "$workdir/installer.yaml" "installer stays off the host network" \
	"hostNetwork: false"
assert_contains "$workdir/installer.yaml" "installer targets x86 Linux nodes only" \
	"kubernetes.io/arch: amd64" \
	"kubernetes.io/os: linux"

# The installer's ServiceAccount-less-ness only matters if no RBAC
# object sneaks in for it either.
assert_absent "$workdir/default.yaml" "no RBAC object exists for the installer" \
	"setec-installer-role"

render "$workdir/installer-off.yaml" --set installer.enabled=false
assert_absent "$workdir/installer-off.yaml" "installer is omitted when disabled" \
	"app.kubernetes.io/component: installer"

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
# Frontend tenant → namespace routing (setec#158).
#
# The frontend has two mutually exclusive strategies: a per-tenant
# namespace resolved by label, or one fixed shared Sandbox namespace.
# The chart must be able to render either (the flag existing on the
# binary is worthless if no value reaches it), must refuse both at
# once, and must refuse a fixed namespace the operator holds no
# Pod-write RBAC in — that last one renders cleanly and then no Sandbox
# ever starts, which is exactly the silent failure shape this script
# exists to catch.
# ---------------------------------------------------------------------------
note "frontend tenant routing (setec#158)"
FE_TLS=(--set frontend.enabled=true
	--set frontend.tlsCertSecretName=fe-tls
	--set frontend.tlsClientCASecretName=fe-ca)

render "$workdir/fe-default.yaml" "${FE_TLS[@]}" \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-default.yaml" "$workdir/fe-default.stripped.yaml"
assert_absent "$workdir/fe-default.stripped.yaml" "default render adds no routing flag (binary default applies)" \
	"--tenant-namespace-label"
assert_absent "$workdir/fe-default.stripped.yaml" "default render selects no fixed namespace" \
	"--sandbox-namespace"

render "$workdir/fe-label.yaml" "${FE_TLS[@]}" \
	--set frontend.tenantNamespaceLabel=gibson.zeroroot.ai/tenant \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-label.yaml" "$workdir/fe-label.stripped.yaml"
assert_contains "$workdir/fe-label.stripped.yaml" "label key override reaches the frontend" \
	"--tenant-namespace-label=gibson.zeroroot.ai/tenant"

render "$workdir/fe-fixed.yaml" "${FE_TLS[@]}" \
	--set frontend.sandboxNamespace="$NS_A" \
	--show-only templates/frontend.yaml
strip_comments "$workdir/fe-fixed.yaml" "$workdir/fe-fixed.stripped.yaml"
assert_contains "$workdir/fe-fixed.stripped.yaml" "fixed Sandbox namespace reaches the frontend" \
	"--sandbox-namespace=$NS_A"
assert_absent "$workdir/fe-fixed.stripped.yaml" "fixed mode renders no label flag" \
	"--tenant-namespace-label"

if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	"${FE_TLS[@]}" \
	--set frontend.sandboxNamespace="$NS_A" \
	--set frontend.tenantNamespaceLabel=gibson.zeroroot.ai/tenant \
	>/dev/null 2>&1; then
	fail "both routing strategies at once must fail the render"
else
	pass "both routing strategies at once fail the render"
fi

if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	"${FE_TLS[@]}" \
	--set frontend.sandboxNamespace=not-in-the-rbac-list \
	>/dev/null 2>&1; then
	fail "a fixed namespace outside sandboxNamespaces must fail the render (no Pod-write RBAC there)"
else
	pass "a fixed namespace outside sandboxNamespaces fails the render"
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

if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set 'netpol.reservedCIDRs={10.0.0.0/8,169.254.0.0/16}' \
	>/dev/null 2>&1; then
	fail "an IPv4-only reserved list must fail the render"
else
	pass "an IPv4-only reserved list fails the render"
fi

if "$HELM" template setec "$CHART_DIR" \
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

if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	"${SNAP_CM[@]}" --set snapshots.mTLS.caProvided=true \
	>/dev/null 2>&1; then
	fail "caProvided=true with certManager.enabled=true must fail the render (the knob would be ignored)"
else
	pass "caProvided=true with certManager.enabled=true fails the render"
fi

if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set snapshots.enabled=true --set nodeAgent.enabled=true \
	>/dev/null 2>&1; then
	fail "snapshots without cert-manager and without caProvided must fail the render"
else
	pass "snapshots without cert-manager and without caProvided fails the render"
fi

# ---------------------------------------------------------------------------
# Session keepalive image reaches the operator (setec#7).
#
# A session Sandbox with no spec.command boots the setec keepalive, which
# the operator pulls from this image. Without the flag the operator refuses
# every such Sandbox, so a chart that drops the argument turns a documented
# default into a Pod-create failure.
# ---------------------------------------------------------------------------
note "session keepalive image (setec#7)"
render "$workdir/keepalive.yaml" --show-only templates/deployment.yaml
strip_comments "$workdir/keepalive.yaml" "$workdir/keepalive.stripped.yaml"
assert_contains "$workdir/keepalive.stripped.yaml" "operator receives the session keepalive image" \
	"--session-keepalive-image=ghcr.io/zeroroot-ai/setec-keepalive:"
if "$HELM" template setec "$CHART_DIR" \
	--set webhook.certManager.enabled=true \
	--set "sandboxNamespaces={${NS_A},${NS_B}}" \
	--set sessionKeepalive.image.repository="" \
	>/dev/null 2>&1; then
	fail "an empty sessionKeepalive.image.repository must fail the render"
else
	pass "an empty sessionKeepalive.image.repository fails the render"
fi

# --- RuntimeClass scheduling.tolerations ------------------------------------
# The RuntimeClass admission controller injects scheduling.tolerations into
# every Pod naming the class, which is the ONLY path that reaches the per-run
# SandboxClasses an e2e harness creates. Nodes hosting a VMM runtime are
# normally tainted (KVM metal is expensive), so if this block stops rendering
# the nodeSelector still steers Sandbox Pods at nodes they are then forbidden
# to land on — they wait forever, and the chart installs cleanly while
# dispatch is dead. Assert both directions.
RC_TOL=$(mktemp)
trap 'rm -f "$RC_TOL"' EXIT

render "$RC_TOL" \
	--set 'runtimes.kata-fc.scheduling.tolerations[0].key=example.io/dedicated' \
	--set 'runtimes.kata-fc.scheduling.tolerations[0].operator=Equal' \
	--set 'runtimes.kata-fc.scheduling.tolerations[0].value=yes' \
	--set 'runtimes.kata-fc.scheduling.tolerations[0].effect=NoSchedule'
assert_contains "$RC_TOL" "RuntimeClass publishes configured scheduling.tolerations" \
	'key: example.io/dedicated' \
	'effect: NoSchedule'

render "$RC_TOL"
assert_absent "$RC_TOL" "RuntimeClass omits tolerations when none are configured" \
	'example.io/dedicated'

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
	if "$HELM" template setec "$CHART_DIR" \
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
