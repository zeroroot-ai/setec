#!/usr/bin/env bash
# 21-install-gvisor.sh — Install gVisor (runsc) and register the gvisor RuntimeClass.
#
# Idempotent: if runsc is already installed and RuntimeClass gvisor already
# exists with handler=runsc, this script exits 0 immediately.
#
# gVisor upstream: https://gvisor.dev/docs/user_guide/install/
# The binaries come from the official gVisor release bucket, as one tarball
# per release. The loose `runsc` and `containerd-shim-runsc-v1` objects the
# gVisor docs still name were removed from the bucket and return 404
# (setec#90). The tarball carries runsc, containerd-shim-runsc-v1 and a
# gvisor-bin/ directory that runsc reads beside itself, so all three have to
# land in the same place. The e2e `suites` job installs it the same way.
#
# WARNING: This script requires root (sudo) and a running k3s cluster.
# It is intended for development use only.

set -euo pipefail

# ── Release channel ───────────────────────────────────────────────────────────
# gVisor prunes old dated releases from its bucket, so a hardcoded date
# eventually 404s. Track the `latest` stable channel by default (override with
# GVISOR_VERSION=release-YYYYMMDD.0 to pin a specific build). The downloaded
# runsc binary is still integrity-checked against upstream's runsc.sha512.
GVISOR_VERSION="${GVISOR_VERSION:-latest}"
# Optional extra pin: if set, the downloaded tarball must also match this
# sha512, on top of the upstream checksum that is always enforced.
GVISOR_SHA512="${GVISOR_SHA512:-}"

# ── Paths ─────────────────────────────────────────────────────────────────────
RUNSC_BIN=/usr/local/bin/runsc
# containerd resolves the gVisor runtime "io.containerd.runsc.v1" to the shim
# binary "containerd-shim-runsc-v1" on its PATH. runsc alone is NOT enough — a
# missing shim makes every gvisor Pod fail RunPodSandbox with
# 'failed to resolve runtime path: ... binary not installed
# "containerd-shim-runsc-v1": file does not exist'. The shim ships in the same
# gVisor release bucket alongside runsc.
SHIM_BIN=/usr/local/bin/containerd-shim-runsc-v1
GVISOR_RELEASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION#release-}/x86_64"
K3S_CONTAINERD_DIR=/var/lib/rancher/k3s/agent/etc/containerd
K3S_CONFIG="${K3S_CONTAINERD_DIR}/config.toml"
# k3s renders containerd config from a version-specific template filename
# (config-v3.toml.tmpl for containerd v2 / config-v3, config.toml.tmpl for
# older). 20-install-kata.sh inlined the kata runtimes into the correct one;
# K3S_TMPL is resolved to that same file in the registration section below.
TMPL_V3="${K3S_CONTAINERD_DIR}/config-v3.toml.tmpl"
TMPL_V2="${K3S_CONTAINERD_DIR}/config.toml.tmpl"

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
shim_ok=0
runtimeclass_ok=0

containerd_ok=0

if command -v runsc >/dev/null 2>&1 || [[ -x "${RUNSC_BIN}" ]]; then
    runsc_ok=1
fi
if command -v containerd-shim-runsc-v1 >/dev/null 2>&1 || [[ -x "${SHIM_BIN}" ]]; then
    shim_ok=1
fi
if kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    runtimeclass_ok=1
fi
# Also require runsc to be registered in the RENDERED containerd config. The
# binary + RuntimeClass can both exist while runsc is absent from containerd —
# notably after 20-install-kata.sh's Phase A wipes & rewrites the containerd
# template. Without this check the early exit skips re-registration and runsc
# silently vanishes from containerd on the next bring-up.
if sudo grep -q 'containerd\.runtimes\.runsc' "${K3S_CONFIG}" 2>/dev/null; then
    containerd_ok=1
fi

if [[ ${runsc_ok} -eq 1 && ${shim_ok} -eq 1 && ${runtimeclass_ok} -eq 1 && ${containerd_ok} -eq 1 ]]; then
    yellow "gVisor already installed — runsc binary, containerd-shim-runsc-v1, RuntimeClass, and containerd registration all present. Skipping."
    exit 0
fi

# ── Install runsc and the containerd shim ─────────────────────────────────────
# containerd resolves "io.containerd.runsc.v1" to containerd-shim-runsc-v1 on
# its PATH. runsc alone is NOT enough — a missing shim makes every gvisor Pod
# fail RunPodSandbox with 'failed to resolve runtime path: ... binary not
# installed "containerd-shim-runsc-v1": file does not exist'. Both ship in the
# one tarball, so both are installed together or neither is.
if [[ ${runsc_ok} -eq 0 || ${shim_ok} -eq 0 ]]; then
    green "Downloading gVisor ${GVISOR_VERSION} from the release bucket"
    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "${TMP_DIR}"' EXIT

    curl -fsSL "${GVISOR_RELEASE_URL}/gvisor.tar.bz2"        -o "${TMP_DIR}/gvisor.tar.bz2"
    curl -fsSL "${GVISOR_RELEASE_URL}/gvisor.tar.bz2.sha512" -o "${TMP_DIR}/gvisor.tar.bz2.sha512"

    # Fail closed. The previous script printed a warning on a checksum
    # mismatch and installed the binary anyway, which is a check that cannot
    # fail. This is the sandbox boundary: a tarball that does not match its
    # published checksum is not installed.
    if ! (cd "${TMP_DIR}" && sha512sum -c gvisor.tar.bz2.sha512); then
        red "FAIL: gvisor.tar.bz2 does not match its published sha512 — refusing to install."
        exit 1
    fi
    green "sha512 checksum verified OK"

    if [[ -n "${GVISOR_SHA512}" ]]; then
        ACTUAL_HASH=$(sha512sum "${TMP_DIR}/gvisor.tar.bz2" | awk '{print $1}')
        if [[ "${GVISOR_SHA512}" != "${ACTUAL_HASH}" ]]; then
            red "FAIL: downloaded gVisor does not match the pinned GVISOR_SHA512."
            red "  pinned: ${GVISOR_SHA512}"
            red "  actual: ${ACTUAL_HASH}"
            exit 1
        fi
        green "pinned GVISOR_SHA512 verified OK"
    fi

    mkdir "${TMP_DIR}/bin"
    tar -xjf "${TMP_DIR}/gvisor.tar.bz2" -C "${TMP_DIR}/bin"
    for required in runsc containerd-shim-runsc-v1; do
        [[ -f "${TMP_DIR}/bin/${required}" ]] || {
            red "FAIL: the gVisor tarball holds no ${required} — upstream changed its layout."
            exit 1
        }
    done

    # gvisor-bin/ lands beside runsc, because runsc reads it from there.
    sudo cp -a "${TMP_DIR}/bin/." /usr/local/bin/
    sudo chown -R root:root "${RUNSC_BIN}" "${SHIM_BIN}"
    sudo chmod 0755 "${RUNSC_BIN}" "${SHIM_BIN}"
    green "runsc installed to ${RUNSC_BIN}"
    green "containerd-shim-runsc-v1 installed to ${SHIM_BIN}"
    "${RUNSC_BIN}" --version
else
    yellow "runsc and containerd-shim-runsc-v1 already present — skipping download"
fi

# ── Register runsc with k3s containerd ────────────────────────────────────────
# Append a runsc runtime stanza to the SAME versioned template kata inlined
# into, using the SAME CRI runtimes table path kata used. That path differs
# between containerd config v2 (io.containerd.grpc.v1.cri) and v3
# (io.containerd.cri.v1.runtime), so we DERIVE it from an existing kata runtime
# registration rather than hardcoding it (a wrong path is silently ignored by
# containerd and runsc would never register).

detect_cfg_version() {
    local v
    v=$(sudo grep -oE '^version[[:space:]]*=[[:space:]]*[0-9]+' "${K3S_CONFIG}" 2>/dev/null \
        | grep -oE '[0-9]+$' | head -1)
    echo "${v:-2}"
}

# Resolve the active template: prefer an existing one (kata wrote it), else
# pick by the rendered config's version.
K3S_TMPL=""
for _t in "${TMPL_V3}" "${TMPL_V2}"; do
    sudo test -f "${_t}" && { K3S_TMPL="${_t}"; break; }
done
if [[ -z "${K3S_TMPL}" ]]; then
    [[ "$(detect_cfg_version)" -ge 3 ]] && K3S_TMPL="${TMPL_V3}" || K3S_TMPL="${TMPL_V2}"
fi

# Derive the CRI runtimes table prefix from an existing runtime registration
# (e.g. kata-fc); fall back to the version-appropriate default if none found.
RUNTIMES_PREFIX=""
if sudo test -f "${K3S_TMPL}"; then
    RUNTIMES_PREFIX=$(sudo grep -oE '\[plugins\.[^]]*containerd\.runtimes\.' "${K3S_TMPL}" 2>/dev/null | head -1)
fi
if [[ -z "${RUNTIMES_PREFIX}" ]]; then
    if [[ "$(detect_cfg_version)" -ge 3 ]]; then
        RUNTIMES_PREFIX="[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes."
    else
        RUNTIMES_PREFIX='[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.'
    fi
fi
green "Registering runsc under CRI runtimes path: ${RUNTIMES_PREFIX}runsc]"

RUNSC_STANZA="
# ── gVisor / runsc runtime (added by 21-install-gvisor.sh) ──
${RUNTIMES_PREFIX}runsc]
  runtime_type = \"io.containerd.runsc.v1\"
"

needs_containerd_update=0

# Check the resolved template (fall back to the live config) for an existing
# runsc registration.
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
                printf '# Managed by zeroroot-ai/setec development/k3s/scripts/21-install-gvisor.sh.\n'
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
