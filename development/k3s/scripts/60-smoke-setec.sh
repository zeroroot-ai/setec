#!/usr/bin/env bash
# Exercise the Setec example client (examples/ai-code-exec) end-to-end against
# the local k3s install, dialling the NodePort with the dev mTLS client cert.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETEC_REPO_ROOT="$(cd "${ROOT}/../.." && pwd)"
PKI="${ROOT}/pki"
export KUBECONFIG="${ROOT}/kubeconfig"

NODEPORT="${SETEC_NODEPORT:-30051}"
host_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
ADDR="${host_ip}:${NODEPORT}"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

dump_diagnostics() {
    red "FAIL: Setec smoke. Diagnostics follow."
    echo '--- frontend logs ---'
    kubectl -n setec-system logs deploy/setec-frontend --tail=80 2>/dev/null || true
    echo '--- recent events (setec-system) ---'
    kubectl -n setec-system get events --sort-by=.lastTimestamp | tail -30 2>/dev/null || true
    echo '--- sandboxes (gibson-dev) ---'
    kubectl -n gibson-dev get sandboxes -o yaml 2>/dev/null || true
}
trap dump_diagnostics ERR

green "Dialling Setec at ${ADDR} as tenant gibson-dev (CN of client cert)"
LOG=$(mktemp)
# ai-code-exec reads the Python source to execute from stdin (runs it as
# `python3 -c <stdin>`). Pipe the test snippet in; no --command flag exists.
( cd "${SETEC_REPO_ROOT}/examples/ai-code-exec" && \
  echo 'print("hello from microvm")' | go run . \
    --addr="${ADDR}" \
    --client-cert="${PKI}/client.crt" \
    --client-key="${PKI}/client.key" \
    --ca="${PKI}/ca.crt" \
    --image=docker.io/library/python:3.12-slim ) | tee "${LOG}"

if grep -q 'hello from microvm' "${LOG}"; then
    green "PASS: Setec smoke — sandbox printed expected output"
    rm -f "${LOG}"
    trap - ERR
else
    red "FAIL: expected 'hello from microvm' not found in client output"
    cat "${LOG}"
    exit 1
fi
