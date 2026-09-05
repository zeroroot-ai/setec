#!/usr/bin/env bash
# Bootstrap namespaces + dev TLS Secrets + helm install Setec on the local
# k3s cluster. Idempotent (helm upgrade --install).
#
# After this script:
#   - setec-system namespace running the operator + frontend
#   - gibson-dev namespace labelled setec.zeroroot.ai/tenant=gibson-dev
#   - frontend reachable via:
#       in-cluster:   setec-frontend.setec-system.svc:50051
#       external:     <host-lan-ip>:30051 (NodePort wrapper)

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETEC_REPO_ROOT="$(cd "${ROOT}/../.." && pwd)"  # opensource/setec/
PKI="${ROOT}/pki"
export KUBECONFIG="${ROOT}/kubeconfig"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }

[[ -f "${PKI}/server.crt" && -f "${PKI}/client.crt" ]] || {
    echo "FAIL: PKI missing — run scripts/30-generate-pki.sh first" >&2
    exit 1
}

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
    setec.zeroroot.ai/tenant: gibson-dev
EOF

# Server TLS secret (chart consumes setec-frontend-tls)
green "Materialising server TLS Secret (setec-frontend-tls)"
kubectl -n setec-system create secret tls setec-frontend-tls \
    --cert="${PKI}/server.crt" --key="${PKI}/server.key" \
    --dry-run=client -o yaml | kubectl apply -f -

# CA secret for client cert verification (chart consumes setec-frontend-ca)
green "Materialising client CA Secret (setec-frontend-ca)"
kubectl -n setec-system create secret generic setec-frontend-ca \
    --from-file=ca.crt="${PKI}/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f -

# Helm install
green "helm upgrade --install setec"
helm upgrade --install setec "${SETEC_REPO_ROOT}/charts/setec" \
    --namespace setec-system \
    -f "${ROOT}/values-local.yaml" \
    --wait --timeout=5m

# Bolt-on NodePort Service (chart does not template this)
green "Applying NodePort wrapper Service (setec-frontend-nodeport)"
kubectl apply -f "${ROOT}/manifests/setec-nodeport.yaml"

green "Setec installed. Pods:"
kubectl -n setec-system get pods
