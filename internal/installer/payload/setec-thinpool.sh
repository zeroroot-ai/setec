#!/bin/sh
# setec-thinpool.sh — provision the containerd devmapper thin-pool at boot.
#
# Installed by the setec installer DaemonSet (internal/installer) to
# /usr/local/sbin/setec-thinpool.sh and run by setec-thinpool.service,
# ordered Before= the container runtime so containerd never starts against
# a missing pool. Configuration comes from /etc/setec/thinpool.env
# (written by the installer from Helm values).
#
# Idempotence contract:
#   - pool device already active           -> exit 0 (no-op)
#   - loop mode, backing files present     -> reattach loops, recreate the
#                                             pool device (pool data lives
#                                             in the files and survives)
#   - loop mode, backing files missing     -> create fresh sparse files,
#                                             build the pool, and WIPE the
#                                             stale devmapper snapshotter
#                                             state that references the
#                                             vanished pool
#   - device mode                          -> (first run: zero the
#                                             metadata superblock) build
#                                             the pool from the two
#                                             dedicated block devices
#
# POSIX sh on purpose: this runs on the HOST, whose shell may be dash.
# Host dependencies: dmsetup, blockdev, and (loop mode) losetup, truncate.
# The installer preflights all of them before writing this script.
set -eu

# Defaults; /etc/setec/thinpool.env overrides.
SETEC_THINPOOL_MODE="${SETEC_THINPOOL_MODE:-loop}"
SETEC_THINPOOL_NAME="${SETEC_THINPOOL_NAME:-setec-thinpool}"
SETEC_THINPOOL_DIR="${SETEC_THINPOOL_DIR:-/var/lib/setec/thinpool}"
SETEC_THINPOOL_DATA_GB="${SETEC_THINPOOL_DATA_GB:-50}"
SETEC_THINPOOL_META_GB="${SETEC_THINPOOL_META_GB:-2}"
SETEC_THINPOOL_DATA_DEVICE="${SETEC_THINPOOL_DATA_DEVICE:-}"
SETEC_THINPOOL_META_DEVICE="${SETEC_THINPOOL_META_DEVICE:-}"
SETEC_DEVMAPPER_ROOT="${SETEC_DEVMAPPER_ROOT:-/var/lib/containerd/io.containerd.snapshotter.v1.devmapper}"

log() { echo "setec-thinpool: $*"; }
die() { echo "setec-thinpool: FATAL: $*" >&2; exit 1; }

# 1. Pool already active — nothing to do.
if dmsetup info "${SETEC_THINPOOL_NAME}" >/dev/null 2>&1; then
    log "thin-pool ${SETEC_THINPOOL_NAME} already active"
    exit 0
fi

clear_stale_snapshotter_state() {
    # The pool backing is gone; containerd's devmapper metadata would
    # reference nonexistent thin devices and wedge the snapshotter.
    if [ -d "${SETEC_DEVMAPPER_ROOT}" ] && [ -n "$(ls -A "${SETEC_DEVMAPPER_ROOT}" 2>/dev/null)" ]; then
        log "clearing stale devmapper snapshotter state under ${SETEC_DEVMAPPER_ROOT}"
        rm -rf "${SETEC_DEVMAPPER_ROOT:?}"/*
    fi
    mkdir -p "${SETEC_DEVMAPPER_ROOT}"
}

# create_pool <meta-dev> <data-dev>
create_pool() {
    meta_dev="$1"
    data_dev="$2"
    data_sectors="$(blockdev --getsz "${data_dev}")"
    # 128 sectors (64K) block size, low-water-mark 32768, skip block
    # zeroing: kata rootfs blocks are fully written before use and
    # zeroing tanks first-write latency (same table as the EKS AMI and
    # the containerd devmapper docs).
    dmsetup create "${SETEC_THINPOOL_NAME}" \
        --table "0 ${data_sectors} thin-pool ${meta_dev} ${data_dev} 128 32768 1 skip_block_zeroing"
    dmsetup info "${SETEC_THINPOOL_NAME}" >/dev/null
    log "thin-pool ${SETEC_THINPOOL_NAME} created (${data_sectors} sectors on ${data_dev})"
}

# attach_loop <backing-file> -> echoes the loop device
attach_loop() {
    backing="$1"
    existing="$(losetup -j "${backing}" 2>/dev/null | head -1 | cut -d: -f1)"
    if [ -n "${existing}" ]; then
        echo "${existing}"
        return 0
    fi
    losetup --find --show "${backing}"
}

case "${SETEC_THINPOOL_MODE}" in
loop)
    mkdir -p "${SETEC_THINPOOL_DIR}"
    data_file="${SETEC_THINPOOL_DIR}/data.img"
    meta_file="${SETEC_THINPOOL_DIR}/meta.img"
    fresh=0
    if [ ! -f "${data_file}" ] || [ ! -f "${meta_file}" ]; then
        fresh=1
        log "creating sparse backing files (${SETEC_THINPOOL_DATA_GB}G data, ${SETEC_THINPOOL_META_GB}G meta) under ${SETEC_THINPOOL_DIR}"
        rm -f "${data_file}" "${meta_file}"
        truncate -s "${SETEC_THINPOOL_DATA_GB}G" "${data_file}"
        truncate -s "${SETEC_THINPOOL_META_GB}G" "${meta_file}"
    fi
    data_loop="$(attach_loop "${data_file}")"
    meta_loop="$(attach_loop "${meta_file}")"
    [ -b "${data_loop}" ] || die "no loop device for ${data_file}"
    [ -b "${meta_loop}" ] || die "no loop device for ${meta_file}"
    if [ "${fresh}" = "1" ]; then
        clear_stale_snapshotter_state
    fi
    create_pool "${meta_loop}" "${data_loop}"
    ;;
device)
    [ -n "${SETEC_THINPOOL_DATA_DEVICE}" ] || die "device mode requires SETEC_THINPOOL_DATA_DEVICE"
    [ -n "${SETEC_THINPOOL_META_DEVICE}" ] || die "device mode requires SETEC_THINPOOL_META_DEVICE"
    [ -b "${SETEC_THINPOOL_DATA_DEVICE}" ] || die "${SETEC_THINPOOL_DATA_DEVICE} is not a block device"
    [ -b "${SETEC_THINPOOL_META_DEVICE}" ] || die "${SETEC_THINPOOL_META_DEVICE} is not a block device"
    marker_dir="/var/lib/setec/thinpool"
    marker="${marker_dir}/.device-initialized"
    if [ ! -f "${marker}" ]; then
        # First use of the declared devices: dm-thin requires a zeroed
        # metadata superblock. The devices were dedicated to setec by the
        # cluster administrator via Helm values — zeroing the first 4KiB
        # of the metadata device is part of that contract.
        log "first use: zeroing metadata superblock on ${SETEC_THINPOOL_META_DEVICE}"
        dd if=/dev/zero of="${SETEC_THINPOOL_META_DEVICE}" bs=4096 count=1 conv=fsync 2>/dev/null
        clear_stale_snapshotter_state
        mkdir -p "${marker_dir}"
        : > "${marker}"
    fi
    create_pool "${SETEC_THINPOOL_META_DEVICE}" "${SETEC_THINPOOL_DATA_DEVICE}"
    ;;
*)
    die "unknown SETEC_THINPOOL_MODE '${SETEC_THINPOOL_MODE}' (want loop or device)"
    ;;
esac
