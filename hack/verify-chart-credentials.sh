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
# Renders the chart and asserts that it has one credential source, the
# SPIFFE Workload API (setec#175):
#
#   - the frontend, the node-agent and the operator's node-agent dialer
#     each get the Workload API socket mount and their SPIFFE flags;
#   - no PEM file credential flag and no certificate Secret volume renders
#     anywhere. The PEM check has a failing fixture,
#     hack/testdata/pem-credential-render.yaml, so it cannot rot into a
#     check that passes on everything;
#   - an empty authorized-ID list fails the render rather than deferring
#     to the binary's startup error.
#
# Assertions run against a comment-stripped copy of the render: template
# comments explaining a rule must not satisfy an assertion about the rule.
#
# Usage: hack/verify-chart-credentials.sh [chart-dir]

set -euo pipefail

CHART_DIR="${1:-charts/setec}"
HELM="${HELM:-helm}"
# The launcher values that the chart requires (hack/chart-launcher-values.yaml).
LAUNCHER_VALUES="$(dirname "$0")/chart-launcher-values.yaml"

fail_count=0

note() { printf '  %s\n' "$*"; }
pass() { printf '  ok   %s\n' "$*"; }
fail() {
	printf '  FAIL %s\n' "$*" >&2
	fail_count=$((fail_count + 1))
}

# assert_contains <haystack-file> <description> <fixed-string...>
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

# assert_render_fails <description> <error-substring> <helm-args...>
assert_render_fails() {
	local desc="$1" want="$2"
	shift 2
	local err="$workdir/err.txt"
	if "$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "$@" >/dev/null 2>"$err"; then
		fail "$desc — render unexpectedly succeeded"
		return
	fi
	if ! grep -qF -- "$want" "$err"; then
		fail "$desc — render failed but without the expected message: $want"
		return
	fi
	pass "$desc"
}

# PEM_FLAGS are the flags and volumes of the PEM file credential source,
# which no longer exists. None may render.
PEM_FLAGS=(
	"--tls-cert"
	"--tls-key"
	"--tls-client-ca"
	"--nodeagent-tls-"
	"--nodeagent-ca"
	"secretName: setec-nodeagent-"
)

# has_pem <file> prints each PEM flag in file and returns 0 when it found
# one.
has_pem() {
	local file="$1" found=1 flag
	for flag in "${PEM_FLAGS[@]}"; do
		if grep -nF -- "$flag" "$file"; then found=0; fi
	done
	return "$found"
}

# strip_comments <in> <out> — drop whole-line and trailing YAML comments so
# an assertion cannot be satisfied by the prose that explains a rule.
strip_comments() {
	sed 's/[[:space:]]*#.*$//' "$1" >"$2"
}

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

printf 'verify-chart-credentials: rendering %s\n' "$CHART_DIR"

# Every credential surface enabled: frontend server, node-agent server
# (snapshots gRPC), operator node-agent dialer.
BASE=(
	--set webhook.certManager.enabled=true
	--set 'sandboxNamespaces={sandbox-workloads}'
	--set frontend.enabled=true
	--set 'frontend.clients[0].name=saas'
	--set 'frontend.clients[0].spiffeID=spiffe://example.org/ns/gibson/sa/gibson-daemon'
	--set 'systemPolicy.frontendCallers[0].namespace=gibson'
	--set 'systemPolicy.frontendCallers[0].podLabels.app\.kubernetes\.io/component=daemon'
	--set nodeAgent.enabled=true
	--set snapshots.enabled=true
)
SPIFFE=(
	--set credentials.spiffe.trustDomain=example.org
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}'
)

# ---------------------------------------------------------------------------
# The PEM check can fail: the fixture holds the PEM flags.
# ---------------------------------------------------------------------------
note "PEM check fixture"
FIXTURE="$(dirname "$0")/testdata/pem-credential-render.yaml"
if has_pem "$FIXTURE" >/dev/null; then
	pass "the PEM check refuses its fixture"
else
	fail "the PEM check accepts $FIXTURE, which holds PEM flags"
fi

# ---------------------------------------------------------------------------
# The one source: all three surfaces use the Workload API.
# ---------------------------------------------------------------------------
note "the SPIFFE Workload API"
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "${BASE[@]}" "${SPIFFE[@]}" >"$workdir/spiffe.yaml"
strip_comments "$workdir/spiffe.yaml" "$workdir/spiffe.stripped.yaml"
if has_pem "$workdir/spiffe.stripped.yaml" >"$workdir/pem.txt"; then
	fail "a PEM credential flag or Secret volume renders: $(tr '\n' ' ' <"$workdir/pem.txt")"
else
	pass "no PEM credential flag or Secret volume renders"
fi

# --show-only isolates each component's document so an assertion cannot be
# satisfied by the same flag on a different component.
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/frontend.yaml >"$workdir/spiffe-frontend.yaml"
strip_comments "$workdir/spiffe-frontend.yaml" "$workdir/spiffe-frontend.stripped.yaml"
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/daemonset.yaml >"$workdir/spiffe-nodeagent.yaml"
strip_comments "$workdir/spiffe-nodeagent.yaml" "$workdir/spiffe-nodeagent.stripped.yaml"
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/deployment.yaml >"$workdir/spiffe-operator.yaml"
strip_comments "$workdir/spiffe-operator.yaml" "$workdir/spiffe-operator.stripped.yaml"

assert_contains "$workdir/spiffe-frontend.stripped.yaml" "frontend gets the socket and the enrolled clients" \
	"--spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--client=saas=spiffe://example.org/ns/gibson/sa/gibson-daemon"
assert_absent "$workdir/spiffe-frontend.stripped.yaml" "frontend has no second allow-list" \
	"--spiffe-authorized-id"
assert_contains "$workdir/spiffe-frontend.stripped.yaml" "frontend mounts the Workload API socket dir read-only" \
	"name: spiffe-workload-api" \
	"path: /run/spire/agent-sockets" \
	"type: Directory"

assert_contains "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent gets socket + allow-list" \
	"--spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--spiffe-authorized-id=spiffe://example.org/ns/setec/sa/setec"
assert_contains "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent mounts the Workload API socket dir" \
	"name: spiffe-workload-api"

assert_contains "$workdir/spiffe-operator.stripped.yaml" "operator dialer gets socket + allow-list" \
	"--nodeagent-spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--nodeagent-spiffe-authorized-id=spiffe://example.org/ns/setec/sa/setec-node-agent"
assert_contains "$workdir/spiffe-operator.stripped.yaml" "operator mounts the Workload API socket dir" \
	"name: spiffe-workload-api"

# ---------------------------------------------------------------------------
# Federation (setec#169, ADR-0164): a client in a foreign trust domain gets a
# ClusterFederatedTrustDomain, and a client in the domain of the fleet gets
# none. A foreign client with no bundle source fails the render.
# ---------------------------------------------------------------------------
note "federation with enrolled clients"
FOREIGN=(
	--set 'frontend.clients[1].name=onprem'
	--set 'frontend.clients[1].spiffeID=spiffe://onprem.example/ns/gibson/sa/gibson-daemon'
)
"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" "${BASE[@]}" "${SPIFFE[@]}" "${FOREIGN[@]}" \
	--set 'frontend.clients[1].federation.bundleEndpointURL=https://spire.onprem.example:8443' \
	--set 'frontend.clients[1].federation.endpointSPIFFEID=spiffe://onprem.example/spire/server' \
	--show-only templates/federation.yaml >"$workdir/federation.yaml"
assert_contains "$workdir/federation.yaml" "the foreign client gets a federated trust domain" \
	"kind: ClusterFederatedTrustDomain" \
	'trustDomain: "onprem.example"' \
	'bundleEndpointURL: "https://spire.onprem.example:8443"' \
	'endpointSPIFFEID: "spiffe://onprem.example/spire/server"'
assert_absent "$workdir/federation.yaml" "the client in the domain of the fleet gets none" \
	'trustDomain: "example.org"'
assert_render_fails "a foreign client with no bundle source fails the render" \
	"needs federation.bundleEndpointURL" \
	"${BASE[@]}" "${SPIFFE[@]}" "${FOREIGN[@]}"
assert_render_fails "no fleet trust domain fails the render" \
	"credentials.spiffe.trustDomain is required" \
	"${BASE[@]}" "${SPIFFE[@]}" --set credentials.spiffe.trustDomain=
assert_render_fails "the https_spiffe profile with no endpoint ID fails the render" \
	"federation.endpointSPIFFEID is required" \
	"${BASE[@]}" "${SPIFFE[@]}" "${FOREIGN[@]}" \
	--set 'frontend.clients[1].federation.bundleEndpointURL=https://spire.onprem.example:8443'

note "render-time failures"
assert_render_fails "a frontend with no enrolled client fails the render" \
	"frontend.clients must not be empty" \
	--set webhook.certManager.enabled=true --set 'sandboxNamespaces={sandbox-workloads}' \
	--set frontend.enabled=true --set credentials.spiffe.trustDomain=example.org
assert_render_fails "empty node-agent allow-list fails the render" \
	"credentials.spiffe.authorizedIDs.nodeAgentClients must not be empty" \
	"${BASE[@]}" --set credentials.spiffe.trustDomain=example.org \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://example.org/ns/setec/sa/setec-node-agent}'
assert_render_fails "empty dialer allow-list fails the render" \
	"credentials.spiffe.authorizedIDs.nodeAgentServers must not be empty" \
	"${BASE[@]}" --set credentials.spiffe.trustDomain=example.org \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://example.org/ns/setec/sa/setec}'
assert_render_fails "socketPath with a scheme prefix fails the render" \
	"must be a bare absolute filesystem path" \
	"${BASE[@]}" "${SPIFFE[@]}" \
	--set credentials.spiffe.socketPath=unix:///run/spire/agent-sockets/api.sock

if [ "$fail_count" -gt 0 ]; then
	printf 'verify-chart-credentials: %d failure(s)\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-credentials: all checks passed\n'
