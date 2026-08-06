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
	"kind: ValidatingAdmissionPolicy"

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

printf '\n'
if [ "$fail_count" -ne 0 ]; then
	printf 'verify-chart-security: %d assertion(s) failed\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-security: all assertions passed\n'
