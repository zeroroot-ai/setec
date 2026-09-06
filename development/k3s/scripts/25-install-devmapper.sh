#!/usr/bin/env bash
# install-devmapper-snapshotter.sh — set up the containerd devmapper
# snapshotter on this host so kata-fc can boot Firecracker microVMs.
#
# Firecracker requires a block device for the rootfs (no overlayfs). The
# containerd devmapper snapshotter fulfils this by carving out thinly-
# provisioned block volumes from a thin-pool. This script creates a
# loopback-backed thin-pool sized for dev use.
#
# What it does:
#   1. Install dmsetup + thin-provisioning-tools if missing (apt)
#   2. Create 50G data + 2G metadata backing files under /var/lib/containerd
#   3. Attach loop devices (not persistent across reboots — see end of
#      script for systemd unit recipe to harden)
#   4. Create the thin-pool via dmsetup
#   5. Extend the k3s containerd template with the devmapper snapshotter
#      registration block
#   6. Restart k3s; evict any stuck sandbox Pods so they re-roll
#   7. Verify snapshotter is registered with containerd
#
# Re-runnable: skips each step when its artifact is already in place.

set -eo pipefail

TMPL_DIR=/var/lib/rancher/k3s/agent/etc/containerd
CONFIG=${TMPL_DIR}/config.toml
# k3s renders containerd config from a version-specific template FILENAME:
# config-v3.toml.tmpl for containerd v2 (config version 3, k3s >= ~v1.31) or
# config.toml.tmpl for older. Earlier scripts (kata, gvisor) already wrote the
# correct one; resolve to whichever exists, falling back to the version of the
# rendered config. Appending to the wrong filename is silently ignored by k3s
# and the snapshotter never registers.
TMPL_V3=${TMPL_DIR}/config-v3.toml.tmpl
TMPL_V2=${TMPL_DIR}/config.toml.tmpl
TMPL=""
for _t in "${TMPL_V3}" "${TMPL_V2}"; do
    sudo test -f "${_t}" && { TMPL="${_t}"; break; }
done
if [[ -z "${TMPL}" ]]; then
    _v=$(sudo grep -oE '^version[[:space:]]*=[[:space:]]*[0-9]+' "${CONFIG}" 2>/dev/null \
        | grep -oE '[0-9]+$' | head -1)
    [[ "${_v:-2}" -ge 3 ]] && TMPL="${TMPL_V3}" || TMPL="${TMPL_V2}"
fi

# Sizing — 50G data + 2G metadata is enough for ~40 concurrent Firecracker
# sandboxes at an 8G base-image size each. Override via env vars if needed.
DATA_SIZE=${DEVMAPPER_DATA_SIZE:-50G}
META_SIZE=${DEVMAPPER_META_SIZE:-2G}
POOL_NAME=${DEVMAPPER_POOL_NAME:-containerd-pool}
BACKING_DIR=${DEVMAPPER_BACKING_DIR:-/var/lib/containerd}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KC=${ROOT}/kubeconfig

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

# ───────────────────────────────────────────────────────────────────────────
# 1. Install tooling.
# ───────────────────────────────────────────────────────────────────────────
green "Step 1/7: ensure dmsetup + thin-provisioning-tools are installed"
if ! command -v dmsetup >/dev/null 2>&1 || ! command -v thin_check >/dev/null 2>&1; then
    sudo apt-get update -qq
    sudo apt-get install -y dmsetup thin-provisioning-tools
else
    yellow "        already installed"
fi

# ───────────────────────────────────────────────────────────────────────────
# 2. Create backing files.
# ───────────────────────────────────────────────────────────────────────────
green "Step 2/7: create ${DATA_SIZE} data + ${META_SIZE} metadata backing files under ${BACKING_DIR}"
sudo mkdir -p "${BACKING_DIR}"
if ! sudo test -f "${BACKING_DIR}/thinpool-data"; then
    sudo truncate -s "${DATA_SIZE}" "${BACKING_DIR}/thinpool-data"
else
    yellow "        ${BACKING_DIR}/thinpool-data already exists"
fi
if ! sudo test -f "${BACKING_DIR}/thinpool-meta"; then
    sudo truncate -s "${META_SIZE}" "${BACKING_DIR}/thinpool-meta"
else
    yellow "        ${BACKING_DIR}/thinpool-meta already exists"
fi

# ───────────────────────────────────────────────────────────────────────────
# 3. Attach loop devices.
# ───────────────────────────────────────────────────────────────────────────
green "Step 3/7: attach loop devices"
DATA_LOOP=$(sudo losetup -j "${BACKING_DIR}/thinpool-data" --output NAME --noheadings 2>/dev/null | head -1)
META_LOOP=$(sudo losetup -j "${BACKING_DIR}/thinpool-meta" --output NAME --noheadings 2>/dev/null | head -1)
if [[ -z "${DATA_LOOP}" ]]; then
    DATA_LOOP=$(sudo losetup --find --show "${BACKING_DIR}/thinpool-data")
    green "        attached data loop: ${DATA_LOOP}"
else
    yellow "        data loop already attached: ${DATA_LOOP}"
fi
if [[ -z "${META_LOOP}" ]]; then
    META_LOOP=$(sudo losetup --find --show "${BACKING_DIR}/thinpool-meta")
    green "        attached meta loop: ${META_LOOP}"
else
    yellow "        meta loop already attached: ${META_LOOP}"
fi

# ───────────────────────────────────────────────────────────────────────────
# 4. Create thin-pool.
# ───────────────────────────────────────────────────────────────────────────
green "Step 4/7: create thin-pool ${POOL_NAME}"
if sudo dmsetup info "${POOL_NAME}" >/dev/null 2>&1; then
    yellow "        thin-pool ${POOL_NAME} already exists"
else
    DATA_SECTORS=$(sudo blockdev --getsz "${DATA_LOOP}")
    sudo dmsetup create "${POOL_NAME}" \
        --table "0 ${DATA_SECTORS} thin-pool ${META_LOOP} ${DATA_LOOP} 128 32768 1 skip_block_zeroing"
    green "        thin-pool created (${DATA_SECTORS} sectors)"
fi

# ───────────────────────────────────────────────────────────────────────────
# 5. Extend the k3s containerd template with the snapshotter block.
# ───────────────────────────────────────────────────────────────────────────
green "Step 5/7: register devmapper snapshotter in ${TMPL}"
if sudo grep -q '\[plugins."io.containerd.snapshotter.v1.devmapper"\]' "${TMPL}"; then
    yellow "        devmapper plugin block already present in template"
else
    sudo sh -c "cat >> '${TMPL}'" <<EOF

# ── devmapper snapshotter (required by kata-fc) ──────────────────────────
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devmapper"
  pool_name = "${POOL_NAME}"
  base_image_size = "8589934592"
  discard_blocks = true
EOF
    green "        appended devmapper snapshotter config"
fi

# ───────────────────────────────────────────────────────────────────────────
# 6. Restart k3s so containerd picks up the new config; evict stuck Pods.
# ───────────────────────────────────────────────────────────────────────────
green "Step 6/7: restart k3s"
sudo systemctl restart k3s
deadline=$(( $(date +%s) + 180 ))
while ! kubectl --kubeconfig="${KC}" get nodes --no-headers 2>/dev/null | grep -q ' Ready '; do
    [[ $(date +%s) -gt ${deadline} ]] && { red "FAIL: node not Ready within 3m"; exit 1; }
    sleep 3
done
green "        node Ready"

# Evict any sandbox Pods that got stuck on the missing snapshotter so they
# re-roll against the now-working devmapper.
yellow "        evicting stuck sandbox Pods so they re-schedule cleanly"
kubectl --kubeconfig="${KC}" -n gibson-dev delete pods --all --ignore-not-found=true --force --grace-period=0 2>/dev/null || true

# ───────────────────────────────────────────────────────────────────────────
# 7. Verify snapshotter is registered.
# ───────────────────────────────────────────────────────────────────────────
green "Step 7/7: verify devmapper plugin registered with containerd"
# containerd initialises plugins asynchronously after the k3s restart: the
# devmapper snapshotter can report non-ok for a second or two before it opens
# the thin-pool and flips to ok. A single check races that init (observed: the
# check failed while the very next invocation showed the plugin 'ok'), so poll
# instead. Match the row whose ID column is exactly "devmapper" and whose
# status column is "ok" — not a loose substring grep.
deadline=$(( $(date +%s) + 90 ))
devmapper_ok=0
while [[ $(date +%s) -le ${deadline} ]]; do
    if sudo k3s ctr --address=/run/k3s/containerd/containerd.sock plugins ls 2>/dev/null \
         | awk '$2 == "devmapper" && $NF == "ok" {ok=1} END {exit ok?0:1}'; then
        devmapper_ok=1
        break
    fi
    sleep 3
done
if [[ ${devmapper_ok} -eq 1 ]]; then
    green "PASS: devmapper snapshotter is active."
    green "Next: make smoke-setec — the kata-fc sandbox should now launch."
else
    yellow "devmapper plugin status:"
    sudo k3s ctr --address=/run/k3s/containerd/containerd.sock plugins ls 2>&1 | grep -i devmapper || true
    red "devmapper did not register within 90s. Check: sudo journalctl -u k3s | grep -i devmapper | tail"
    exit 1
fi

cat <<'NOTE'

────────────────────────────────────────────────────────────────────────
Reboot persistence (optional, recommended):
────────────────────────────────────────────────────────────────────────

Loopback attachments don't survive reboot. For a long-lived dev cluster
drop this in /etc/systemd/system/containerd-thinpool.service:

  [Unit]
  Description=Attach containerd devmapper thin-pool
  DefaultDependencies=no
  After=local-fs.target
  Before=k3s.service

  [Service]
  Type=oneshot
  RemainAfterExit=yes
  ExecStart=/bin/sh -c 'DATA=$(losetup -j /var/lib/containerd/thinpool-data --output NAME --noheadings || losetup --find --show /var/lib/containerd/thinpool-data) ; \
                        META=$(losetup -j /var/lib/containerd/thinpool-meta --output NAME --noheadings || losetup --find --show /var/lib/containerd/thinpool-meta) ; \
                        dmsetup info containerd-pool >/dev/null 2>&1 || \
                        dmsetup create containerd-pool --table "0 $(blockdev --getsz $DATA) thin-pool $META $DATA 128 32768 1 skip_block_zeroing"'
  ExecStop=/bin/sh -c 'dmsetup remove containerd-pool ; losetup -d $(losetup -j /var/lib/containerd/thinpool-data -O NAME -n) ; losetup -d $(losetup -j /var/lib/containerd/thinpool-meta -O NAME -n)'

  [Install]
  WantedBy=multi-user.target

Then: sudo systemctl daemon-reload && sudo systemctl enable containerd-thinpool
────────────────────────────────────────────────────────────────────────
NOTE
