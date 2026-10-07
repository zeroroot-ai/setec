#!/usr/bin/env bash
# Start a local OCI registry for the signed image disks, and let k3s pull
# from it over HTTP.
#
# The disk builder pushes each signed disk to launcher.diskRepo, and the
# kubelet pulls it as an image volume. A dev box has no registry of its own,
# so this script runs one on the host LAN IP, port 5000. The disk builder
# uses HTTP for an RFC 1918 address. k3s gets a registries.yaml entry so
# containerd also uses HTTP for it.
#
# Idempotent: a running registry is kept, and k3s restarts only when the
# registries.yaml entry changes.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REGISTRY_IMAGE="docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
REGISTRY_NAME="setec-dev-disks"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }

host_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
[[ -n "${host_ip}" ]] || { echo "FAIL: could not detect host LAN IP" >&2; exit 1; }
reg="${host_ip}:5000"

if ! docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null | grep -q true; then
    green "Starting the disk registry at ${reg}"
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
    docker run -d --restart=unless-stopped --name "${REGISTRY_NAME}" -p "${host_ip}:5000:5000" "${REGISTRY_IMAGE}" >/dev/null
fi
for _ in $(seq 1 30); do
    curl -fsS "http://${reg}/v2/" >/dev/null && break
    sleep 1
done
curl -fsS "http://${reg}/v2/" >/dev/null || { echo "FAIL: the disk registry at ${reg} does not answer" >&2; exit 1; }

want="$(printf 'mirrors:\n  "%s":\n    endpoint:\n      - "http://%s"\n' "${reg}" "${reg}")"
if [[ "$(sudo cat /etc/rancher/k3s/registries.yaml 2>/dev/null)" != "${want}" ]]; then
    green "Writing /etc/rancher/k3s/registries.yaml and restarting k3s"
    printf '%s\n' "${want}" | sudo tee /etc/rancher/k3s/registries.yaml >/dev/null
    sudo systemctl restart k3s
fi
echo "${reg}/setec-disks" > "${ROOT}/disk-repo"
green "Disk registry ready: ${reg}/setec-disks"
