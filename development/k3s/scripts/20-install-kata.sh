#!/usr/bin/env bash
# Install Kata Containers via kata-deploy and verify the kata-fc RuntimeClass
# appears. The kata-containers project now ships an official Helm chart at
#   github.com/kata-containers/kata-containers/tools/packaging/kata-deploy/helm-chart/kata-deploy
# (added in 3.x), which natively supports k3s via k8sDistribution=k3s.
#
# Helm can't install directly from a subdirectory of a remote git repo, so
# the script shallow-clones kata-containers into a local cache and helm
# installs from that path.
#
# Hard-fail if RuntimeClass kata-fc does not appear — never silently fall
# back to runc.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${ROOT}/kubeconfig"

KATA_VERSION="${KATA_VERSION:-3.28.0}"
KATA_CACHE="${KATA_CACHE:-${ROOT}/.cache/kata-containers-${KATA_VERSION}}"
CHART_PATH="${KATA_CACHE}/tools/packaging/kata-deploy/helm-chart/kata-deploy"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

# Shallow-clone the tagged release (idempotent).
if [[ ! -d "${CHART_PATH}" ]]; then
    green "Fetching kata-containers ${KATA_VERSION} into ${KATA_CACHE}"
    mkdir -p "$(dirname "${KATA_CACHE}")"
    git clone --depth=1 --branch "${KATA_VERSION}" \
        https://github.com/kata-containers/kata-containers \
        "${KATA_CACHE}" 2>&1 | tail -3
else
    yellow "Reusing cached kata-containers at ${KATA_CACHE}"
fi

[[ -f "${CHART_PATH}/Chart.yaml" ]] || {
    red "FAIL: expected chart at ${CHART_PATH} — layout changed upstream?"
    exit 1
}

# ─────────────────────────────────────────────────────────────────────────
# Containerd template setup — two-phase.
#
# Phase A (this step): write a template with an `imports = [...]` line so
# kata-deploy's pre-install check passes. config.toml.d/ is empty at this
# point so imports load nothing; containerd happily serves CRI.
#
# Phase B (end of this script, after helm install kata-deploy): snapshot
# the drop-in content that kata-deploy wrote, remove the drop-in from
# disk, REWRITE the template with the runtime registrations inlined and
# the imports line removed. This sidesteps a containerd table-merge bug
# that wipes the base CRI plugin when a drop-in declares nested
# [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.*] tables via
# imports. (See commit history: three recovery iterations confirmed this.)
#
# Idempotent: if the template already has the Phase B shape (contains kata
# runtime registrations AND no imports line), skip both phases.
# ─────────────────────────────────────────────────────────────────────────
K3S_CONTAINERD_DIR=/var/lib/rancher/k3s/agent/etc/containerd
K3S_CONFIG="${K3S_CONTAINERD_DIR}/config.toml"
KATA_DROPIN="${K3S_CONTAINERD_DIR}/config.toml.d/kata-deploy.toml"
IMPORTS_LINE='imports = ["/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/*.toml"]'

# k3s renders containerd's config.toml from a template whose FILENAME depends
# on the containerd config version. k3s shipping containerd v2 (config
# version 3 — k3s >= ~v1.31) renders from `config-v3.toml.tmpl`; older k3s
# used `config.toml.tmpl`. Writing the wrong filename is silently ignored:
# k3s regenerates a DEFAULT config.toml with no `imports` line, and
# kata-deploy's pre-install check hard-fails ("does not import the drop-in
# dir 'config.toml.d'"). So pick the name k3s actually consumes from the
# version of the config it generates.
TMPL_V3="${K3S_CONTAINERD_DIR}/config-v3.toml.tmpl"
TMPL_V2="${K3S_CONTAINERD_DIR}/config.toml.tmpl"
pick_tmpl_for_version() {
    # echo the template path k3s renders from, given a containerd config version.
    [[ "${1:-2}" -ge 3 ]] && echo "${TMPL_V3}" || echo "${TMPL_V2}"
}
detect_cfg_version() {
    # containerd config version from the rendered config.toml (default 2).
    local v
    v=$(sudo grep -oE '^version[[:space:]]*=[[:space:]]*[0-9]+' "${K3S_CONFIG}" 2>/dev/null \
        | grep -oE '[0-9]+$' | head -1)
    echo "${v:-2}"
}

# Best-effort initial guess from whatever config exists now; refined in Phase A
# once k3s has regenerated a fresh config.
K3S_TMPL="$(pick_tmpl_for_version "$(detect_cfg_version)")"

# Has Phase B already run (a template contains kata-fc runtime inline + no
# imports line)? Check BOTH possible template filenames.
ALREADY_INLINED=0
for _t in "${TMPL_V3}" "${TMPL_V2}"; do
    if sudo test -f "${_t}" && \
       sudo grep -Fq 'containerd.runtimes.kata-fc' "${_t}" && \
       ! sudo grep -q '^imports = ' "${_t}"; then
        ALREADY_INLINED=1
        K3S_TMPL="${_t}"
        break
    fi
done

if [[ ${ALREADY_INLINED} -eq 1 ]]; then
    yellow "Containerd template already contains inlined kata runtimes — skipping template writes"
else
    green "Phase A: write imports-enabled template (required for kata-deploy pre-install check)"
    # Wipe any prior state so we start from a clean default-generated config.
    # Remove BOTH template variants — we don't yet know which one k3s renders.
    sudo rm -f "${TMPL_V3}" "${TMPL_V2}" "${K3S_CONFIG}"
    sudo rm -rf "${K3S_CONTAINERD_DIR}/config.toml.d/"
    sudo mkdir -p "${K3S_CONTAINERD_DIR}/config.toml.d/"
    sudo systemctl restart k3s

    green "        waiting for node Ready after default-config regen..."
    deadline=$(( $(date +%s) + 120 ))
    while ! kubectl get nodes 2>/dev/null | grep -q ' Ready '; do
        [[ $(date +%s) -gt $deadline ]] && { red "FAIL: node not Ready within 2m"; exit 1; }
        sleep 3
    done

    # The node can report Ready (old k3s API answers during the restart window)
    # before containerd has regenerated config.toml. Poll for the regenerated
    # config so version-detection reads a real file and the wrap below doesn't
    # `cat` a missing config.toml.
    green "        waiting for containerd to regenerate ${K3S_CONFIG}..."
    deadline=$(( $(date +%s) + 120 ))
    until sudo test -s "${K3S_CONFIG}"; do
        [[ $(date +%s) -gt $deadline ]] && { red "FAIL: k3s did not regenerate ${K3S_CONFIG} within 2m"; exit 1; }
        sleep 2
    done

    # k3s has now regenerated a default config.toml. Pick the template FILENAME
    # it renders from based on that config's version (config.toml.tmpl for
    # containerd v1/config-v2, config-v3.toml.tmpl for containerd v2/config-v3).
    CFG_VERSION="$(detect_cfg_version)"
    K3S_TMPL="$(pick_tmpl_for_version "${CFG_VERSION}")"
    green "        containerd config version ${CFG_VERSION} → rendering template ${K3S_TMPL}"

    # Now wrap the generated config.toml with the imports line prepended.
    sudo sh -c "
        {
            printf '# Managed by opensource/setec/development/k3s/scripts/20-install-kata.sh (Phase A).\n'
            printf '# imports enabled so kata-deploy pre-install check passes; after kata-deploy\n'
            printf '# writes its drop-in, this template is rewritten by Phase B at end of script.\n'
            printf '%s\n\n' '${IMPORTS_LINE}'
            cat '${K3S_CONFIG}'
        } > '${K3S_TMPL}'
    "
    sudo systemctl restart k3s

    green "        waiting for node Ready after imports-enabled template..."
    deadline=$(( $(date +%s) + 120 ))
    while ! kubectl get nodes 2>/dev/null | grep -q ' Ready '; do
        [[ $(date +%s) -gt $deadline ]] && { red "FAIL: node not Ready within 2m"; exit 1; }
        sleep 3
    done

    # Force any stuck kata-deploy pods from a prior run to re-roll.
    kubectl -n kube-system delete pods -l name=kata-deploy --ignore-not-found=true --wait=false 2>/dev/null || true
fi

# The kata-deploy chart depends on node-feature-discovery. helm dependency
# build requires the subchart's source repo to be registered first.
if ! helm repo list 2>/dev/null | awk '{print $2}' | grep -q '^https://kubernetes-sigs.github.io/node-feature-discovery/charts$'; then
    green "Registering node-feature-discovery helm repo"
    helm repo add nfd https://kubernetes-sigs.github.io/node-feature-discovery/charts
fi
helm repo update nfd >/dev/null 2>&1 || true

green "helm dependency build (fetches node-feature-discovery subchart)"
helm dependency build "${CHART_PATH}"

green "helm upgrade --install kata-deploy (k3s distribution)"
helm upgrade --install kata-deploy "${CHART_PATH}" \
    --namespace kube-system \
    --set k8sDistribution=k3s \
    --wait --timeout=10m

green "Waiting for kata-deploy DaemonSet to report Ready"
kubectl -n kube-system rollout status ds/kata-deploy --timeout=10m

# The chart installs RuntimeClass objects for kata-qemu / kata-clh / kata-fc
# / kata-dragonball via a post-install Job. Wait for them to appear.
deadline=$(( $(date +%s) + 180 ))
while ! kubectl get runtimeclass kata-fc >/dev/null 2>&1; do
    [[ $(date +%s) -gt $deadline ]] && {
        red "FAIL: RuntimeClass kata-fc did not appear within 3m"
        echo '--- installed runtimeclasses ---'
        kubectl get runtimeclass 2>&1 || true
        echo '--- kata-deploy pod status ---'
        kubectl -n kube-system get pods -l name=kata-deploy 2>&1 || true
        exit 1
    }
    sleep 3
done
green "RuntimeClass kata-fc present"

# ─────────────────────────────────────────────────────────────────────────
# Phase B: rewrite the template with kata runtimes INLINED and the
# imports line REMOVED. Prevents the containerd merge bug from wiping
# CRI on the next restart.
# ─────────────────────────────────────────────────────────────────────────
if [[ ${ALREADY_INLINED} -eq 0 ]]; then
    green "Phase B: inline kata runtimes into template, remove drop-in + imports"
    # kata-deploy writes config.toml.d/kata-deploy.toml ASYNCHRONOUSLY: the
    # daemonset reports Ready and the post-install Job registers the kata-fc
    # RuntimeClass BEFORE the in-container installer finishes copying the
    # drop-in to the host. A single `test -f` here races that write and aborts
    # (observed with kata-deploy 3.28: drop-in landed ~1s after the check).
    # Poll for it instead. Until Phase B runs, the drop-in is loaded via the
    # imports line, which trips the containerd table-merge bug that wipes the
    # base CRI plugin (node goes NotReady) — so getting here promptly matters.
    green "        waiting for kata-deploy to POPULATE the drop-in (${KATA_DROPIN})..."
    # Poll for the drop-in to exist *and contain the kata runtime tables*, not
    # merely exist. kata-deploy creates the file and writes its content in
    # separate steps; a test-f-only poll races the content write and captures
    # an EMPTY drop-in, inlining nothing and leaving containerd with no kata
    # runtime ("no runtime for kata-qemu is configured").
    deadline=$(( $(date +%s) + 180 ))
    until sudo test -f "${KATA_DROPIN}" && \
          sudo grep -q 'containerd\.runtimes\.kata' "${KATA_DROPIN}"; do
        if [[ $(date +%s) -gt $deadline ]]; then
            red "FAIL: kata-deploy did not populate ${KATA_DROPIN} with kata runtimes within 3m."
            echo '--- drop-in contents (if any) ---'; sudo cat "${KATA_DROPIN}" 2>&1 | head -20 || true
            echo '--- kata-deploy pods ---'; kubectl -n kube-system get pods -l name=kata-deploy 2>&1 || true
            exit 1
        fi
        sleep 3
    done
    green "        drop-in populated with kata runtimes — proceeding to inline."

    # Snapshot drop-in content, then remove the drop-in file so containerd
    # never loads it via imports again. Assert the capture is non-empty so we
    # never silently inline an empty config.
    sudo cp "${KATA_DROPIN}" /tmp/kata-runtimes-captured.toml
    if ! grep -q 'containerd\.runtimes\.kata' /tmp/kata-runtimes-captured.toml; then
        red "FAIL: captured drop-in has no kata runtime tables — refusing to inline."
        exit 1
    fi
    sudo rm -f "${KATA_DROPIN}"

    # Rewrite template: the current config.toml without the imports line,
    # then the kata runtime registrations appended inline.
    sudo sh -c "
        {
            printf '# Managed by opensource/setec/development/k3s/scripts/20-install-kata.sh (Phase B).\n'
            printf '# Kata runtimes inlined — imports mechanism wipes CRI via containerd merge bug.\n'
            grep -v '^imports = ' '${K3S_CONFIG}'
            printf '\n# ── kata runtime registrations (inlined from kata-deploy.toml) ──\n'
            cat /tmp/kata-runtimes-captured.toml
        } > '${K3S_TMPL}'
    "

    # Assert kata runtimes actually landed in the template before restarting
    # k3s onto it — guards against a silently-empty inline.
    if ! sudo grep -q 'containerd\.runtimes\.kata-fc' "${K3S_TMPL}"; then
        red "FAIL: ${K3S_TMPL} has no kata-fc runtime after inline — aborting."
        exit 1
    fi

    sudo systemctl restart k3s
    green "        waiting for node Ready after inline-template restart..."
    deadline=$(( $(date +%s) + 120 ))
    while ! kubectl get nodes 2>/dev/null | grep -q ' Ready '; do
        [[ $(date +%s) -gt $deadline ]] && { red "FAIL: node not Ready within 2m after Phase B"; exit 1; }
        sleep 3
    done
    green "Phase B complete — kata runtimes registered via inline template (no drop-in, no imports)"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Register RuntimeClass kata-qemu
#
# kata-deploy ships both kata-fc and kata-qemu handlers. The chart creates a
# RuntimeClass for each via a post-install Job; wait for it the same way we
# waited for kata-fc above. Then ensure Setec's own label is present.
#
# Handler name: kata-qemu (as created by the kata-deploy Job — see
#   https://github.com/kata-containers/kata-containers/blob/main/tools/packaging/kata-deploy/scripts/kata-deploy.sh)
# ─────────────────────────────────────────────────────────────────────────────
green "Verifying / registering RuntimeClass kata-qemu"

if kubectl get runtimeclass kata-qemu >/dev/null 2>&1; then
    yellow "RuntimeClass kata-qemu already exists (created by kata-deploy Job)"
else
    # kata-deploy should have created it; if not, apply a fallback definition.
    yellow "RuntimeClass kata-qemu not found — applying fallback definition"
    kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-qemu
handler: kata-qemu
scheduling:
  nodeSelector:
    setec.zeroroot.ai/runtime.kata-qemu: "true"
EOF
    green "RuntimeClass kata-qemu applied"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Node labels — capability detection for the multi-runtime operator.
#
# Label schema: setec.zeroroot.ai/runtime.<backend>=true
#
# The legacy katacontainers.io/kata-runtime label is preserved for compat
# (pre-existing Setec SandboxClasses reference it until the operator upgrade
# completes the defaulting webhook migration).
# ─────────────────────────────────────────────────────────────────────────────
NODE_NAME=$(kubectl get nodes -o name 2>/dev/null | head -1 | sed 's|node/||')
if [[ -z "${NODE_NAME}" ]]; then
    red "FAIL: could not determine node name from kubectl get nodes"
    exit 1
fi
green "Node: ${NODE_NAME}"

# kata-fc label (set whether or not this node has KVM — kata-fc requires KVM
# but the RuntimeClass must still be reachable; the node-agent will set the
# label to false on KVM-absent nodes during capability probing).
green "Labelling node with setec.zeroroot.ai/runtime.kata-fc=true"
kubectl label node "${NODE_NAME}" \
    "setec.zeroroot.ai/runtime.kata-fc=true" \
    --overwrite

# kata-qemu label.  kata-qemu supports TCG fallback when KVM is absent, so
# the label is always set true. Print a prominent warning when KVM is absent
# so operators know hardware acceleration is unavailable.
if test -c /dev/kvm; then
    green "KVM device present — kata-qemu will use hardware acceleration"
else
    yellow "WARNING: /dev/kvm not found on this node."
    yellow "         kata-qemu will fall back to TCG (software emulation)."
    yellow "         Performance will be significantly degraded."
    yellow "         For production use, ensure KVM is available."
fi
green "Labelling node with setec.zeroroot.ai/runtime.kata-qemu=true"
kubectl label node "${NODE_NAME}" \
    "setec.zeroroot.ai/runtime.kata-qemu=true" \
    --overwrite

green "kata install complete — RuntimeClasses kata-fc + kata-qemu registered, node labelled."
