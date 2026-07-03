#!/usr/bin/env bash
# setec-thinpool.sh — provision the containerd devmapper thin-pool from the
# instance's LOCAL NVMe instance store. Runs at every boot via
# setec-thinpool.service, ordered Before=containerd.service.
#
# Idempotence contract:
#   - pool device already active            -> exit 0 (no-op)
#   - VG survives a plain reboot            -> reactivate the existing pool
#   - fresh instance store (first boot, or  -> build the pool from scratch and
#     stop/start wiped the NVMe)               WIPE the stale devmapper
#                                              snapshotter state that
#                                              references the vanished pool
#
# Target instance types (c6gd.metal / m6gd.metal) expose one or more
# ephemeral "Amazon EC2 NVMe Instance Storage" devices. EBS volumes are
# never touched: selection is strictly by NVMe model string.
set -euo pipefail

VG=setec
LV=thinpool
POOL_DM="${VG}-${LV}"          # dm name for VG "setec" LV "thinpool"
DEVMAPPER_ROOT=/var/lib/containerd/io.containerd.snapshotter.v1.devmapper

log() { echo "setec-thinpool: $*"; }

# 1. Pool already active — nothing to do.
if dmsetup info "${POOL_DM}" >/dev/null 2>&1; then
    log "thin-pool ${POOL_DM} already active"
    exit 0
fi

# 2. VG survived (plain reboot keeps instance-store contents) — reactivate.
if vgs "${VG}" >/dev/null 2>&1; then
    log "volume group ${VG} present — activating ${VG}/${LV}"
    lvchange -ay "${VG}/${LV}"
    dmsetup info "${POOL_DM}" >/dev/null
    log "thin-pool ${POOL_DM} reactivated"
    exit 0
fi

# 3. Fresh build: enumerate unused instance-store NVMe devices.
mapfile -t CANDIDATES < <(
    lsblk -dno NAME,MODEL | awk '/Instance Storage/ {print "/dev/"$1}'
)
DEVICES=()
for dev in "${CANDIDATES[@]:-}"; do
    [[ -b "${dev}" ]] || continue
    # Skip anything that already has partitions, a filesystem, or an FS/RAID
    # signature — never claim a device someone else prepared.
    if [[ -n "$(lsblk -no FSTYPE "${dev}" | tr -d '[:space:]')" ]]; then
        log "skipping ${dev}: existing filesystem/signature"
        continue
    fi
    if [[ "$(lsblk -no TYPE "${dev}" --list | wc -l)" -gt 1 ]]; then
        log "skipping ${dev}: has partitions"
        continue
    fi
    DEVICES+=("${dev}")
done

if [[ ${#DEVICES[@]} -eq 0 ]]; then
    log "FATAL: no unused NVMe instance-store device found." >&2
    log "       kata-fc needs local NVMe for the devmapper thin-pool —" >&2
    log "       launch this AMI on a 'd'-suffixed metal type (c6gd.metal / m6gd.metal)." >&2
    exit 1
fi
log "building thin-pool from: ${DEVICES[*]}"

# The previous pool (if any) is gone with the instance store; containerd's
# devmapper metadata would reference nonexistent thin devices. Start clean.
if [[ -d "${DEVMAPPER_ROOT}" ]]; then
    log "clearing stale devmapper snapshotter state under ${DEVMAPPER_ROOT}"
    rm -rf "${DEVMAPPER_ROOT:?}"/*
fi
mkdir -p "${DEVMAPPER_ROOT}"

pvcreate -y "${DEVICES[@]}"
vgcreate "${VG}" "${DEVICES[@]}"
# 95%VG leaves headroom for the thin-pool's own metadata LV (auto-sized by
# lvm). thin_pool_zero=0 skips block zeroing — kata rootfs blocks are always
# fully written before use, and zeroing tanks first-write latency.
lvcreate \
    --config 'allocation/thin_pool_zero=0' \
    --type thin-pool \
    --name "${LV}" \
    --extents '95%VG' \
    "${VG}"

dmsetup info "${POOL_DM}" >/dev/null
log "thin-pool ${POOL_DM} created"
