#!/usr/bin/env bash
# End-to-end integration smoke: launches the gibson-tool-runner:hello-dev image
# via Setec using the tool-runner ABI (GIBSON_TOOL_INPUT_B64 env in, marker-
# framed base64 protojson on stdout out) — the same ABI the daemon's
# SandboxedToolExecutor (core/gibson/internal/harness/sandboxed) speaks.
#
# Passing here confirms every hop in Kind → Setec → kata-fc microVM → back is
# wired: cross-cluster mTLS, CRD admission, image pull from the local k3s
# containerd, tool-runner startup, stdout capture, and proto round-trip.
#
# Prerequisites:
#   - `make up` completed (k3s + kata + devmapper + Setec running)
#   - Kind cluster 'gibson' is up with LAN access to the host (default for Kind)
#   - Dev mTLS PKI produced by `make pki` (scripts/30-gen-dev-pki.sh)
#
# This script is non-destructive: it rebuilds the tool-runner image, loads it
# into k3s containerd, applies the smoke Job, waits for completion, and prints
# the microVM's stdout + the PASS line.
#
# sudo is required for `k3s ctr images import` — the script will prompt.
set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ZERODAY_ROOT="$(cd "${ROOT}/../../.." && pwd)"
KIND_CONTEXT="${KIND_CONTEXT:-kind-gibson}"
KUBECONFIG_K3S="${ROOT}/kubeconfig"

# SETEC_ADDR discovery: prefer explicit override, else derive from the host LAN
# IP (the NodePort the dev k3s cluster exposes at :30051, reachable from Kind
# Pods without any Kind cluster config changes).
if [[ -z "${SETEC_ADDR:-}" ]]; then
    LAN_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++)if($i=="src"){print $(i+1);exit}}')"
    SETEC_ADDR="${LAN_IP:-127.0.0.1}:30051"
fi

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

# 1. Verify both clusters are reachable.
green "Verifying k3s cluster (Setec side)"
KUBECONFIG="${KUBECONFIG_K3S}" kubectl get nodes | head -3

green "Verifying Kind cluster ${KIND_CONTEXT} (Gibson side)"
kubectl --context="${KIND_CONTEXT}" get nodes | head -3

# 2. Build the hello-dev tool-runner image.
green "Building hello-dev tool-runner image"
make -C "${ZERODAY_ROOT}/core/sdk" tool-runner-image TOOL=hello

# 3. Load the image into k3s containerd (requires sudo).
green "Loading image into k3s containerd (sudo required)"
docker save ghcr.io/zeroroot-ai/gibson-tool-runner:hello-dev | \
    sudo KUBECONFIG="${KUBECONFIG_K3S}" k3s ctr images import -

# 4. Regenerate + apply mTLS client Secret to the Kind cluster (idempotent).
green "Applying dev mTLS client Secret to ${KIND_CONTEXT}/gibson"
PKI="${ROOT}/pki"
[[ -f "${PKI}/ca.crt" && -f "${PKI}/client.crt" && -f "${PKI}/client.key" ]] || {
    red "FAIL: dev PKI missing under ${PKI} — run scripts/30-generate-pki.sh first"
    exit 1
}
GEN="${ROOT}/manifests/gibson-kind/setec-client-tls.generated.yaml"
sed \
    -e "s|__CA_B64__|$(base64 -w0 < "${PKI}/ca.crt")|" \
    -e "s|__CLIENT_CRT_B64__|$(base64 -w0 < "${PKI}/client.crt")|" \
    -e "s|__CLIENT_KEY_B64__|$(base64 -w0 < "${PKI}/client.key")|" \
    "${ROOT}/manifests/gibson-kind/setec-client-tls.yaml.tpl" > "${GEN}"
kubectl --context="${KIND_CONTEXT}" apply -f "${GEN}"

# 5. Apply the smoke Job (tears down prior run first).
MANIFEST="${ROOT}/manifests/gibson-kind/hello-toolrunner-smoke.yaml"
green "Applying smoke Job (Setec target: ${SETEC_ADDR})"
kubectl --context="${KIND_CONTEXT}" -n gibson delete job hello-toolrunner-smoke --ignore-not-found >/dev/null 2>&1
kubectl --context="${KIND_CONTEXT}" -n gibson delete cm hello-toolrunner-smoke-source --ignore-not-found >/dev/null 2>&1
sed "s|__SETEC_ADDR__|${SETEC_ADDR}|" "${MANIFEST}" | \
    kubectl --context="${KIND_CONTEXT}" apply -f -

# 6. Wait for completion.
green "Waiting for Job to complete (up to 5m)..."
kubectl --context="${KIND_CONTEXT}" -n gibson wait \
    --for=condition=complete --timeout=300s \
    job/hello-toolrunner-smoke

# 7. Print the microVM stdout + PASS line.
green "Smoke Job output:"
kubectl --context="${KIND_CONTEXT}" -n gibson logs job/hello-toolrunner-smoke

# 8. Confirm a Sandbox CR appeared in k3s/gibson-dev (proves it reached Setec).
green "Sandbox CRs observed in k3s/gibson-dev (most recent 5):"
KUBECONFIG="${KUBECONFIG_K3S}" kubectl -n gibson-dev get sandboxes.setec.zeroroot.ai \
    --sort-by=.metadata.creationTimestamp 2>/dev/null | tail -5 || {
    red "No Sandbox CRs observed. Either the smoke did not fire or Setec did not receive it."
    exit 1
}

green "Integration smoke complete — Gibson→Setec tool-runner ABI verified end-to-end."
green "Trace: kubectl --context=${KIND_CONTEXT} -n gibson port-forward svc/gibson-jaeger 16686:16686"
