#!/usr/bin/env bash
# Bootstrap namespaces + the disk signing Secret + helm install Setec on the local
# k3s cluster. Idempotent (helm upgrade --install).
#
# After this script:
#   - setec-system namespace running the operator + frontend
#   - gibson-dev namespace labelled setec.zeroroot.ai/client=dev and
#     setec.zeroroot.ai/tenant=gibson-dev (the namespace of the pair dev/gibson-dev)
#   - each Sandbox a Firecracker machine in a launcher Pod, with its image
#     disk signed by the dev key in the Secret setec-disk-signing
#   - frontend reachable via:
#       in-cluster:   setec-frontend.setec-system.svc:50051
#       external:     <host-lan-ip>:30051 (NodePort wrapper)

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETEC_REPO_ROOT="$(cd "${ROOT}/../.." && pwd)"  # the setec repo root
PKI="${ROOT}/pki"
export KUBECONFIG="${ROOT}/kubeconfig"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }

kubectl -n spire-server get statefulset spire-server >/dev/null 2>&1 || {
    echo "FAIL: SPIRE missing — run scripts/30-install-spire.sh first" >&2
    exit 1
}
mkdir -p "${PKI}"
[[ -f "${ROOT}/disk-repo" ]] || {
    echo "FAIL: no disk registry — run scripts/20-install-disk-registry.sh first" >&2
    exit 1
}
disk_repo="$(cat "${ROOT}/disk-repo")"

# Namespaces (idempotent via apply)
green "Creating namespaces"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: setec-system
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/warn: privileged
    pod-security.kubernetes.io/audit: privileged
---
apiVersion: v1
kind: Namespace
metadata:
  name: gibson-dev
  labels:
    setec.zeroroot.ai/client: dev
    setec.zeroroot.ai/tenant: gibson-dev
EOF

# The disk signing key. The disk builder signs each image disk with the
# seed, and each launcher checks the signature with the public key. The dev
# key lives in pki/ beside the dev client SVID.
if [[ ! -f "${PKI}/disk-signing.pem" ]]; then
    green "Generating the dev disk signing key"
    openssl genpkey -algorithm ed25519 -out "${PKI}/disk-signing.pem"
    chmod 0600 "${PKI}/disk-signing.pem"
fi
# The raw ed25519 seed and public key are the last 32 bytes of the DER forms.
disk_seed="$(openssl pkey -in "${PKI}/disk-signing.pem" -outform DER | tail -c 32 | base64 -w0)"
disk_pub="$(openssl pkey -in "${PKI}/disk-signing.pem" -pubout -outform DER | tail -c 32 | base64 -w0)"
green "Materialising the disk signing Secret (setec-disk-signing)"
kubectl -n setec-system create secret generic setec-disk-signing \
    --from-literal=seed="${disk_seed}" \
    --dry-run=client -o yaml | kubectl apply -f -

# Helm install
green "helm upgrade --install setec"
helm upgrade --install setec "${SETEC_REPO_ROOT}/charts/setec" \
    --namespace setec-system \
    -f "${ROOT}/values-local.yaml" \
    --set-string launcher.diskRepo="${disk_repo}" \
    --set-string launcher.diskBuilder.signingSecret=setec-disk-signing \
    --set-string "launcher.diskBuilder.publicKeys[0]=${disk_pub}" \
    --wait --timeout=5m

# Bolt-on NodePort Service (chart does not template this)
green "Applying NodePort wrapper Service (setec-frontend-nodeport)"
kubectl apply -f "${ROOT}/manifests/setec-nodeport.yaml"

green "Setec installed. Pods:"
kubectl -n setec-system get pods
