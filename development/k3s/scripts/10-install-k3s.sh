#!/usr/bin/env bash
# Install k3s as a single-node systemd unit (Traefik disabled), export a
# kubeconfig with the API server URL rewritten to the host LAN IP so the
# kubeconfig is usable from inside the Kind 'gibson' cluster's network.
#
# Idempotent: re-running converges; if k3s is already active we skip install
# but still re-export the kubeconfig.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUBECONFIG_OUT="${ROOT}/kubeconfig"

K3S_VERSION="${K3S_VERSION:-v1.31.4+k3s1}"

# The k3s installer is fetched from the pinned release commit and verified
# against a recorded digest, never piped straight from the mutable
# https://get.k3s.io redirect (setec#280). get.k3s.io serves whatever is on
# k3s master at the moment of the curl, so an unpinned `curl | sh` executes
# unreviewed root-privileged code that can change under us between two
# bring-ups of the same K3S_VERSION. To bump: change K3S_INSTALLER_COMMIT to
# the commit the new K3S_VERSION tag points at, re-download, and record the
# new sha256 below.
K3S_INSTALLER_COMMIT="${K3S_INSTALLER_COMMIT:-a562d090b05cf8d55b6a8b57556787c24c8ce21a}"
K3S_INSTALLER_SHA256="${K3S_INSTALLER_SHA256:-f60c3d8940dfc896f7d83aaf57726c91cf21afc4bca40036472df108d9700b4b}"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }

# Detect host LAN IP — the address the Kind containers will dial.
host_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
if [[ -z "${host_ip}" ]]; then
    echo "FAIL: could not detect host LAN IP via 'ip route get 1.1.1.1'" >&2
    exit 1
fi
green "Detected host LAN IP: ${host_ip}"

if systemctl is-active --quiet k3s 2>/dev/null; then
    yellow "k3s service is already active — skipping install, re-exporting kubeconfig"
else
    green "Installing k3s ${K3S_VERSION} (Traefik disabled)"
    installer="$(mktemp -t k3s-install.XXXXXX.sh)"
    trap 'rm -f "${installer}"' EXIT
    curl -sfL --retry 3 -o "${installer}" \
        "https://raw.githubusercontent.com/k3s-io/k3s/${K3S_INSTALLER_COMMIT}/install.sh"
    echo "${K3S_INSTALLER_SHA256}  ${installer}" | sha256sum -c - >/dev/null || {
        echo "FAIL: k3s install.sh digest mismatch at commit ${K3S_INSTALLER_COMMIT}." >&2
        echo "      Expected ${K3S_INSTALLER_SHA256}, got $(sha256sum "${installer}" | cut -d' ' -f1)." >&2
        echo "      Refusing to run an unverified root-privileged installer." >&2
        exit 1
    }
    INSTALL_K3S_VERSION="${K3S_VERSION}" \
        INSTALL_K3S_EXEC="server --disable=traefik --write-kubeconfig-mode=0644 --node-name=setec-dev" \
        sh "${installer}"
fi

# Wait for node Ready. If k3s is already active but the node is stuck
# NotReady — e.g. a prior bring-up's kata step left containerd's config
# half-written and CNI never initialised ("cni plugin not initialized") —
# restart k3s once to recover: with no custom config.toml.tmpl present k3s
# regenerates a complete default containerd config (CNI included) and the node
# comes back. This makes the bring-up self-healing instead of wedging.
wait_ready() {
    local deadline; deadline=$(( $(date +%s) + $1 ))
    while ! sudo k3s kubectl get nodes 2>/dev/null | grep -q ' Ready '; do
        [[ $(date +%s) -gt $deadline ]] && return 1
        sleep 2
    done
    return 0
}

if ! wait_ready 60; then
    yellow "node not Ready after 60s — restarting k3s to recover (regenerates containerd config + CNI)"
    sudo systemctl restart k3s
    if ! wait_ready 150; then
        echo "FAIL: node did not reach Ready within 150s even after a k3s restart" >&2
        sudo k3s kubectl get nodes 2>&1 || true
        sudo k3s kubectl describe node setec-dev 2>&1 | grep -iE 'Ready|cni|NetworkReady' | head -10 || true
        exit 1
    fi
fi
green "Node Ready"

# Export kubeconfig with rewritten server URL
sudo cp /etc/rancher/k3s/k3s.yaml "${KUBECONFIG_OUT}"
sudo chown "$(id -u):$(id -g)" "${KUBECONFIG_OUT}"
sed -i "s|https://127\.0\.0\.1:6443|https://${host_ip}:6443|g" "${KUBECONFIG_OUT}"
chmod 0644 "${KUBECONFIG_OUT}"
green "kubeconfig exported to ${KUBECONFIG_OUT} (server: https://${host_ip}:6443)"
