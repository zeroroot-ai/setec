#!/usr/bin/env bash
# Tear down the local k3s + Setec dev environment. Idempotent — silent on
# missing components.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKI="${ROOT}/pki"
KUBECONFIG_PATH="${ROOT}/kubeconfig"
export KUBECONFIG="${KUBECONFIG_PATH}"

yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }

# Best-effort helm uninstall only when the cluster has at least one Ready
# node. Without this guard, helm waits indefinitely on API calls to NotReady
# nodes. Even when skipped, k3s-uninstall.sh below removes everything.
if [[ -f "${KUBECONFIG_PATH}" ]] && \
   kubectl get nodes --no-headers 2>/dev/null | grep -qE '\sReady\s'; then
    yellow "helm uninstall setec (best-effort, 30s timeout)"
    timeout 30 helm uninstall setec -n setec-system --no-hooks --wait=false 2>/dev/null || true
    yellow "helm uninstall kata-deploy (best-effort, 30s timeout)"
    timeout 30 helm uninstall kata-deploy -n kube-system --no-hooks --wait=false 2>/dev/null || true
else
    yellow "Skipping helm uninstalls — no Ready node in cluster (or kubeconfig absent)"
    yellow "k3s-uninstall.sh below will wipe cluster state regardless"
fi

# Official k3s uninstaller (silent if not installed)
if [[ -x /usr/local/bin/k3s-uninstall.sh ]]; then
    yellow "Running /usr/local/bin/k3s-uninstall.sh"
    sudo /usr/local/bin/k3s-uninstall.sh
fi

# Working-tree cleanup
rm -rf "${PKI}" "${KUBECONFIG_PATH}"
rm -f "${ROOT}"/manifests/gibson-kind/*.generated.yaml 2>/dev/null || true
yellow "Removed pki/, kubeconfig, generated manifests."
