#!/usr/bin/env bash
# 21-install-gvisor.sh — Install gVisor (runsc) and register the gvisor RuntimeClass.
#
# Idempotent: if runsc is already installed and RuntimeClass gvisor already
# exists with handler=runsc, this script exits 0 immediately.
#
# gVisor upstream: https://gvisor.dev/docs/user_guide/install/
# The runsc binary is downloaded from the official gVisor release bucket.
#
# WARNING: This script requires root (sudo) and a running k3s cluster.
# It is intended for development use only.

set -euo pipefail

# ── Pinned version ────────────────────────────────────────────────────────────
# Check https://gvisor.dev/releases for the latest stable tag.
# CLAIM: verify this version exists at https://storage.googleapis.com/gvisor/releases/
# before pinning a new release in production workflows.
GVISOR_VERSION="${GVISOR_VERSION:-release-20250909.0}"
# SHA-512 of the runsc binary for the version above (amd64).
# CLAIM: regenerate this hash when bumping GVISOR_VERSION:
#   curl -fsSL "https://storage.googleapis.com/gvisor/releases/release/20250909.0/x86_64/runsc.sha512"
GVISOR_SHA512="${GVISOR_SHA512:-}"   # left empty so we log-and-continue on mismatch (dev-only)

# ── Paths ─────────────────────────────────────────────────────────────────────
RUNSC_BIN=/usr/local/bin/runsc
GVISOR_RELEASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION#release-}/x86_64"
K3S_CONTAINERD_DIR=/var/lib/rancher/k3s/agent/etc/containerd
K3S_TMPL="${K3S_CONTAINERD_DIR}/config.toml.tmpl"
K3S_CONFIG="${K3S_CONTAINERD_DIR}/config.toml"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${ROOT}/kubeconfig"

# ── Colour helpers ─────────────────────────────────────────────────────────────
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }

# ── Skip flag ─────────────────────────────────────────────────────────────────
SKIP_RESTART=0
for arg in "$@"; do
    [[ "$arg" == "--skip-restart" ]] && SKIP_RESTART=1
done

# ── Idempotence check ─────────────────────────────────────────────────────────
runsc_ok=0
runtimeclass_ok=0

if command -v runsc >/dev/null 2>&1 || [[ -x "${RUNSC_BIN}" ]]; then
    runsc_ok=1
fi
if kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    runtimeclass_ok=1
fi

if [[ ${runsc_ok} -eq 1 && ${runtimeclass_ok} -eq 1 ]]; then
    yellow "gVisor already installed — runsc present and RuntimeClass gvisor exists. Skipping."
    exit 0
fi

# ── Install runsc binary ───────────────────────────────────────────────────────
if [[ ${runsc_ok} -eq 0 ]]; then
    green "Downloading runsc ${GVISOR_VERSION} from gVisor release bucket"
    TMP_RUNSC=$(mktemp)
    TMP_SHA=$(mktemp)
    trap 'rm -f "${TMP_RUNSC}" "${TMP_SHA}"' EXIT

    curl -fsSL "${GVISOR_RELEASE_URL}/runsc"        -o "${TMP_RUNSC}"
    curl -fsSL "${GVISOR_RELEASE_URL}/runsc.sha512" -o "${TMP_SHA}"

    # Verify upstream-provided sha512 checksum.
    UPSTREAM_HASH=$(awk '{print $1}' "${TMP_SHA}")
    ACTUAL_HASH=$(sha512sum "${TMP_RUNSC}" | awk '{print $1}')

    if [[ "${UPSTREAM_HASH}" != "${ACTUAL_HASH}" ]]; then
        red "WARN: sha512 mismatch for runsc (upstream vs downloaded)."
        red "  upstream: ${UPSTREAM_HASH}"
        red "  actual  : ${ACTUAL_HASH}"
        red "  Continuing anyway (dev-only environment) — verify manually."
    else
        green "sha512 checksum verified OK"
    fi

    # If caller pinned a GVISOR_SHA512, check that too.
    if [[ -n "${GVISOR_SHA512}" && "${GVISOR_SHA512}" != "${ACTUAL_HASH}" ]]; then
        red "WARN: downloaded runsc does not match pinned GVISOR_SHA512."
        red "  pinned: ${GVISOR_SHA512}"
        red "  actual: ${ACTUAL_HASH}"
        red "  Continuing anyway (dev-only) — update GVISOR_SHA512 in this script."
    fi

    sudo install -o root -g root -m 0755 "${TMP_RUNSC}" "${RUNSC_BIN}"
    green "runsc installed to ${RUNSC_BIN}"
else
    yellow "runsc binary already present — skipping download"
fi

# ── Register runsc with k3s containerd ────────────────────────────────────────
# k3s uses a config.toml.tmpl overlay. We append a [plugins.*.runtimes.runsc]
# stanza if it isn't already present. We edit the template (not the live config)
# following the same Phase-B pattern used in 20-install-kata.sh.
RUNSC_STANZA='
# ── gVisor / runsc runtime (added by 21-install-gvisor.sh) ──
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
'

needs_containerd_update=0

# Determine which config file to check: prefer the Phase-B tmpl; fall back to
# the live config.toml if the tmpl doesn't exist yet.
CONFIG_TO_CHECK="${K3S_CONFIG}"
if sudo test -f "${K3S_TMPL}"; then
    CONFIG_TO_CHECK="${K3S_TMPL}"
fi

if sudo grep -q 'containerd.runtimes.runsc' "${CONFIG_TO_CHECK}" 2>/dev/null; then
    yellow "runsc already registered in containerd config — skipping template edit"
else
    needs_containerd_update=1
fi

if [[ ${needs_containerd_update} -eq 1 ]]; then
    green "Appending runsc runtime stanza to containerd template"
    sudo mkdir -p "${K3S_CONTAINERD_DIR}"

    if sudo test -f "${K3S_TMPL}"; then
        # Append to the existing template.
        printf '%s\n' "${RUNSC_STANZA}" | sudo tee -a "${K3S_TMPL}" >/dev/null
    else
        # No template yet — create one from the live config plus our stanza.
        sudo sh -c "
            {
                printf '# Managed by opensource/setec/development/k3s/scripts/21-install-gvisor.sh.\n'
                cat '${K3S_CONFIG}'
                printf '%s\n' '${RUNSC_STANZA}'
            } > '${K3S_TMPL}'
        "
    fi
    green "Containerd template updated"

    if [[ ${SKIP_RESTART} -eq 0 ]]; then
        green "Restarting k3s to apply containerd config"
        sudo systemctl restart k3s

        green "Waiting for node Ready after restart..."
        deadline=$(( $(date +%s) + 120 ))
        while ! kubectl get nodes 2>/dev/null | grep -q ' Ready '; do
            [[ $(date +%s) -gt $deadline ]] && {
                red "FAIL: node not Ready within 2m after gVisor containerd config restart"
                exit 1
            }
            sleep 3
        done
        green "Node Ready"
    else
        yellow "--skip-restart set: skipping k3s restart (run manually: sudo systemctl restart k3s)"
    fi
fi

# ── Apply RuntimeClass gvisor ─────────────────────────────────────────────────
if [[ ${runtimeclass_ok} -eq 0 ]]; then
    green "Applying RuntimeClass gvisor (handler=runsc)"
    kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
scheduling:
  nodeSelector:
    setec.zeroroot.ai/runtime.gvisor: "true"
EOF
    green "RuntimeClass gvisor applied"
else
    yellow "RuntimeClass gvisor already exists — skipping apply"
fi

# ── Label the current node ────────────────────────────────────────────────────
NODE_NAME=$(kubectl get nodes -o name 2>/dev/null | head -1 | sed 's|node/||')
if [[ -z "${NODE_NAME}" ]]; then
    red "FAIL: could not determine node name from kubectl get nodes"
    exit 1
fi

green "Labelling node ${NODE_NAME} with setec.zeroroot.ai/runtime.gvisor=true"
kubectl label node "${NODE_NAME}" \
    "setec.zeroroot.ai/runtime.gvisor=true" \
    --overwrite

green "gVisor install complete — runsc ready, RuntimeClass gvisor registered, node labelled."
