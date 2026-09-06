#!/usr/bin/env bash
# Recovery helper: clean stuck helm state, restart k3s, verify kata-fc is
# still functional, re-run setec install. Use when a prior `make up` left
# the node NotReady or helm in pending-install / pending-upgrade state.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${ROOT}/kubeconfig"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

# 1. Wipe any in-flight helm releases that would block upgrade.
green "Cleaning stuck helm releases in setec-system"
for rel in setec; do
    status=$(helm -n setec-system status "${rel}" 2>/dev/null | awk -F': ' '/^STATUS:/ {print $2}')
    case "${status}" in
        pending-install|pending-upgrade|pending-rollback|failed)
            yellow "  ${rel} is ${status}; uninstalling"
            timeout 30 helm -n setec-system uninstall "${rel}" --no-hooks --wait=false 2>/dev/null || true
            ;;
        deployed)
            green "  ${rel} is deployed; leaving alone"
            ;;
        "")
            yellow "  ${rel} not present"
            ;;
    esac
done

# 2. Restart k3s. Force kills the cluster — kata-deploy's containerd state
#    gets re-read from the drop-in directory on restart, so a clean boot
#    picks up the kata handler properly.
green "Restarting k3s (requires sudo)"
sudo systemctl restart k3s

# 3. Wait for node Ready.
deadline=$(( $(date +%s) + 120 ))
green "Waiting up to 2m for node to return to Ready"
while ! kubectl get nodes --no-headers 2>/dev/null | grep -qE '\sReady\s'; do
    [[ $(date +%s) -gt $deadline ]] && { red "FAIL: node did not reach Ready within 2m"; kubectl get nodes 2>&1; exit 1; }
    sleep 3
done
green "Node Ready"

# 4. Verify kata-fc RuntimeClass is still registered.
if ! kubectl get runtimeclass kata-fc >/dev/null 2>&1; then
    red "FAIL: RuntimeClass kata-fc missing after k3s restart — re-run scripts/20-install-kata.sh"
    exit 1
fi
green "RuntimeClass kata-fc still registered"

# 5. Let the kata-deploy DaemonSet re-stabilise if its pod was evicted.
green "Waiting for kata-deploy DaemonSet to return to Ready (up to 3m)"
kubectl -n kube-system rollout status ds/kata-deploy --timeout=3m 2>&1 | tail -2

# 6. Re-run setec install.
green "Re-running 40-install-setec.sh"
"${ROOT}/scripts/40-install-setec.sh"
