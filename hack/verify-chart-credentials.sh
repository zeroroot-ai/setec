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
# Renders the chart in both credential modes and asserts the install-wide
# switch does what setec#183 promises:
#
#   - file mode (the default) renders exactly the Secret-mounted
#     certificate posture the chart rendered before the switch existed —
#     no SPIFFE flag, socket mount, or value leaks in;
#   - spiffe mode renders the Workload API socket mount and the
#     --spiffe-socket / --spiffe-authorized-id flags on the frontend, the
#     node-agent, and the operator's node-agent dialer — all three, so a
#     mixed posture is not reachable;
#   - an empty authorized-ID list in spiffe mode fails the render rather
#     than deferring to the binary's startup error.
#
# Assertions run against a comment-stripped copy of the render: template
# comments explaining a rule must not satisfy an assertion about the rule.
#
# Usage: hack/verify-chart-credentials.sh [chart-dir]

set -euo pipefail

CHART_DIR="${1:-charts/setec}"
HELM="${HELM:-helm}"

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
	if "$HELM" template setec "$CHART_DIR" "$@" >/dev/null 2>"$err"; then
		fail "$desc — render unexpectedly succeeded"
		return
	fi
	if ! grep -qF -- "$want" "$err"; then
		fail "$desc — render failed but without the expected message: $want"
		return
	fi
	pass "$desc"
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
	--set nodeAgent.enabled=true
	--set snapshots.enabled=true
)
FILE_CREDS=(
	--set frontend.tlsCertSecretName=fe-tls
	--set frontend.tlsClientCASecretName=fe-ca
)
SPIFFE=(
	--set credentials.mode=spiffe
	--set 'credentials.spiffe.authorizedIDs.frontendClients={spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://zeroroot.ai/ns/setec/sa/setec}'
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent}'
)

# ---------------------------------------------------------------------------
# File mode (default): today's posture, nothing SPIFFE leaks in.
# ---------------------------------------------------------------------------
note "file mode (default)"
"$HELM" template setec "$CHART_DIR" "${BASE[@]}" "${FILE_CREDS[@]}" >"$workdir/file.yaml"
strip_comments "$workdir/file.yaml" "$workdir/file.stripped.yaml"

assert_contains "$workdir/file.stripped.yaml" "frontend keeps the file-mode flags" \
	"--tls-cert=/etc/setec/tls/tls.crt" \
	"--tls-key=/etc/setec/tls/tls.key" \
	"--tls-client-ca=/etc/setec/tls-ca/ca.crt"
assert_contains "$workdir/file.stripped.yaml" "node-agent keeps the file-mode flags" \
	"--tls-cert=/etc/setec/nodeagent-tls/tls.crt"
assert_contains "$workdir/file.stripped.yaml" "operator dialer keeps the file-mode flags" \
	"--nodeagent-tls-cert=/etc/setec/nodeagent-tls/tls.crt" \
	"--nodeagent-ca=/etc/setec/nodeagent-ca/ca.crt"
assert_contains "$workdir/file.stripped.yaml" "file-mode Secret volumes render" \
	"secretName: fe-tls" \
	"secretName: fe-ca" \
	"secretName: setec-nodeagent-server-tls" \
	"secretName: setec-nodeagent-client-tls"
assert_absent "$workdir/file.stripped.yaml" "no SPIFFE flag in file mode" "--spiffe-socket"
assert_absent "$workdir/file.stripped.yaml" "no SPIFFE dialer flag in file mode" "--nodeagent-spiffe-socket"
assert_absent "$workdir/file.stripped.yaml" "no Workload API mount in file mode" "spiffe-workload-api"

# ---------------------------------------------------------------------------
# SPIFFE mode: all three surfaces flip together (setec#183 "both or
# neither" — the operator's dialer is the third surface).
# ---------------------------------------------------------------------------
note "spiffe mode"
"$HELM" template setec "$CHART_DIR" "${BASE[@]}" "${SPIFFE[@]}" >"$workdir/spiffe.yaml"
strip_comments "$workdir/spiffe.yaml" "$workdir/spiffe.stripped.yaml"

# --show-only isolates each component's document so an assertion cannot be
# satisfied by the same flag on a different component.
"$HELM" template setec "$CHART_DIR" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/frontend.yaml >"$workdir/spiffe-frontend.yaml"
strip_comments "$workdir/spiffe-frontend.yaml" "$workdir/spiffe-frontend.stripped.yaml"
"$HELM" template setec "$CHART_DIR" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/daemonset.yaml >"$workdir/spiffe-nodeagent.yaml"
strip_comments "$workdir/spiffe-nodeagent.yaml" "$workdir/spiffe-nodeagent.stripped.yaml"
"$HELM" template setec "$CHART_DIR" "${BASE[@]}" "${SPIFFE[@]}" \
	--show-only templates/deployment.yaml >"$workdir/spiffe-operator.yaml"
strip_comments "$workdir/spiffe-operator.yaml" "$workdir/spiffe-operator.stripped.yaml"

assert_contains "$workdir/spiffe-frontend.stripped.yaml" "frontend gets socket + allow-list" \
	"--spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--spiffe-authorized-id=spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon"
assert_contains "$workdir/spiffe-frontend.stripped.yaml" "frontend mounts the Workload API socket dir read-only" \
	"name: spiffe-workload-api" \
	"path: /run/spire/agent-sockets" \
	"type: Directory"
assert_absent "$workdir/spiffe-frontend.stripped.yaml" "frontend drops the file-mode flags" "--tls-cert"
assert_absent "$workdir/spiffe-frontend.stripped.yaml" "frontend drops the TLS Secret volumes" "secretName: fe-tls"

assert_contains "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent gets socket + allow-list" \
	"--spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--spiffe-authorized-id=spiffe://zeroroot.ai/ns/setec/sa/setec"
assert_contains "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent mounts the Workload API socket dir" \
	"name: spiffe-workload-api"
assert_absent "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent drops the file-mode flags" "--tls-cert"
assert_absent "$workdir/spiffe-nodeagent.stripped.yaml" "node-agent drops the TLS Secret volumes" "secretName: setec-nodeagent-server-tls"

assert_contains "$workdir/spiffe-operator.stripped.yaml" "operator dialer gets socket + allow-list" \
	"--nodeagent-spiffe-socket=/run/spire/agent-sockets/api.sock" \
	"--nodeagent-spiffe-authorized-id=spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent"
assert_absent "$workdir/spiffe-operator.stripped.yaml" "operator drops the file-mode dialer flags" "--nodeagent-tls-cert"
assert_absent "$workdir/spiffe-operator.stripped.yaml" "operator drops the client-cert Secret volume" "secretName: setec-nodeagent-client-tls"

# ---------------------------------------------------------------------------
# Render-time failures: an empty allow-list must die at helm time, not at
# pod startup; so must a mode typo and a legacy cert-manager block that
# would otherwise render unused Certificates.
# ---------------------------------------------------------------------------
note "render-time failures"
assert_render_fails "empty frontend allow-list fails the render" \
	"credentials.spiffe.authorizedIDs.frontendClients must not be empty" \
	"${BASE[@]}" --set credentials.mode=spiffe \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://zeroroot.ai/ns/setec/sa/setec}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent}'
assert_render_fails "empty node-agent allow-list fails the render" \
	"credentials.spiffe.authorizedIDs.nodeAgentClients must not be empty" \
	"${BASE[@]}" --set credentials.mode=spiffe \
	--set 'credentials.spiffe.authorizedIDs.frontendClients={spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentServers={spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent}'
assert_render_fails "empty dialer allow-list fails the render" \
	"credentials.spiffe.authorizedIDs.nodeAgentServers must not be empty" \
	"${BASE[@]}" --set credentials.mode=spiffe \
	--set 'credentials.spiffe.authorizedIDs.frontendClients={spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon}' \
	--set 'credentials.spiffe.authorizedIDs.nodeAgentClients={spiffe://zeroroot.ai/ns/setec/sa/setec}'
assert_render_fails "unknown mode fails the render" \
	'credentials.mode must be "file" or "spiffe"' \
	"${BASE[@]}" "${FILE_CREDS[@]}" --set credentials.mode=files
assert_render_fails "socketPath with a scheme prefix fails the render" \
	"must be a bare absolute filesystem path" \
	"${BASE[@]}" "${SPIFFE[@]}" \
	--set credentials.spiffe.socketPath=unix:///run/spire/agent-sockets/api.sock
assert_render_fails "cert-manager for the node-agent channel fails in spiffe mode" \
	"snapshots.mTLS.certManager.enabled has no effect" \
	"${BASE[@]}" "${SPIFFE[@]}" --set snapshots.mTLS.certManager.enabled=true
assert_render_fails "file mode still requires the frontend Secrets" \
	"frontend.tlsCertSecretName is required" \
	"${BASE[@]}"

if [ "$fail_count" -gt 0 ]; then
	printf 'verify-chart-credentials: %d failure(s)\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-chart-credentials: all checks passed\n'
