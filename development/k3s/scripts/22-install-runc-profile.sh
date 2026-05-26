#!/usr/bin/env bash
# 22-install-runc-profile.sh — Register the runc RuntimeClass for dev clusters.
#
# !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
# WARNING: runc is dev-only — it provides NO isolation beyond a standard Linux
# container.  DO NOT enable this script in production clusters.  The runc
# RuntimeClass carries the label setec.zeroroot.ai/isolation=container-only
# which the Setec admission webhook uses to gate runc to dev namespaces only
# (see internal/admission/sandboxclass_webhook.go).
# !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
#
# runc ships with k3s/containerd — no binary install is required.  This
# script only registers the RuntimeClass object and labels the node so that
# Setec's node-agent capability detection finds it.
#
# Idempotent: if RuntimeClass runc already exists with handler=runc, exits 0.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${ROOT}/kubeconfig"

# ── Colour helpers ─────────────────────────────────────────────────────────────
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }

# ── Idempotence check ─────────────────────────────────────────────────────────
if kubectl get runtimeclass runc >/dev/null 2>&1; then
    # Confirm the handler is correct — if someone installed a bad object, warn.
    HANDLER=$(kubectl get runtimeclass runc -o jsonpath='{.handler}' 2>/dev/null || echo "")
    if [[ "${HANDLER}" == "runc" ]]; then
        yellow "RuntimeClass runc already exists with handler=runc. Skipping."
        exit 0
    else
        yellow "RuntimeClass runc exists but handler='${HANDLER}' (expected 'runc'). Re-applying."
    fi
fi

# ── Apply RuntimeClass runc ───────────────────────────────────────────────────
green "Applying RuntimeClass runc (handler=runc, dev-only)"
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
  labels:
    # Marks this RuntimeClass as providing container-level isolation only.
    # The Setec admission webhook enforces that only namespaces bearing
    # setec.zeroroot.ai/allow-dev-runtimes=true may reference this class.
    setec.zeroroot.ai/isolation: container-only
handler: runc
scheduling:
  nodeSelector:
    setec.zeroroot.ai/runtime.runc: "true"
EOF
green "RuntimeClass runc applied"

# ── Label the current node ────────────────────────────────────────────────────
NODE_NAME=$(kubectl get nodes -o name 2>/dev/null | head -1 | sed 's|node/||')
if [[ -z "${NODE_NAME}" ]]; then
    red "FAIL: could not determine node name from kubectl get nodes"
    exit 1
fi

green "Labelling node ${NODE_NAME} with setec.zeroroot.ai/runtime.runc=true"
kubectl label node "${NODE_NAME}" \
    "setec.zeroroot.ai/runtime.runc=true" \
    --overwrite

green "runc profile complete — RuntimeClass runc registered, node labelled."
yellow "REMINDER: runc is dev-only. Do not enable this script in production clusters."
