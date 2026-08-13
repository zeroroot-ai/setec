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

printf '\n'
if [ "$fail_count" -ne 0 ]; then
	printf 'verify-chart-security: %d assertion(s) failed\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-security: all assertions passed\n'
