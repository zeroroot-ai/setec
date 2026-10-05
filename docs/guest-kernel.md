<!-- SPDX-License-Identifier: Apache-2.0 -->
# Guest kernel

A launcher machine boots one kernel that `setec` builds. This page states what it has, and how it is pinned, built and kept current.

## What it has

The kernel loads no modules: each driver is built in. `kernel/x86_64.defconfig` is the full list. The parts that a launcher machine uses:

- The Firecracker devices: virtio over MMIO for the disks (`VIRTIO_BLK`), the network (`VIRTIO_NET`), vsock to the launcher (`VIRTIO_VSOCKETS`) and the balloon, and the serial console (`SERIAL_8250_CONSOLE`).
- The file systems of the root: squashfs with zstd for the image disk, ext4 for the writable layer and the workspace, overlayfs to join them, and devtmpfs, proc, sysfs and tmpfs.
- New randomness and a correct clock after a snapshot load: the VM generation ID (`VMGENID`) and the KVM PTP clock (`PTP_1588_CLOCK_KVM`).
- No vsock loopback (`VSOCKETS_LOOPBACK` is off), so a process in the machine cannot reach the guest agent over vsock.

## Where it is pinned

- `kernel/kernel.env` holds `KERNEL_VERSION` and `KERNEL_SHA256`. No other file names a version. `scripts/check-runtime-pins.sh` fails CI when one does.
- `kernel/x86_64.defconfig` is the config in its minimal form. `scripts/build-guest-kernel.sh --check` fails when the config is not the fixed point of `make savedefconfig` for the pinned source, or when it turns on modules.

## How it is built

The workflow `guest-kernel.yml` builds `vmlinux` on each release tag, signs it with cosign (keyless, the identity of the workflow), and attaches `vmlinux`, `vmlinux.config`, `SHA256SUMS` and the signature bundle to the release.

## The security update routine

1. The scheduled workflow `guest-kernel-currency.yml` fails when the series of the pin has a later release.
2. The failure opens the work. The maintainer on duty for `setec` takes it the same day.
3. Copy the sha256 of `linux-<version>.tar.xz` from `https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc`.
4. Change both lines of `kernel/kernel.env` in one pull request.
5. If the config check of the pull request fails, run `scripts/build-guest-kernel.sh --check` on the new source and commit the config it asks for.
6. Merge. The next release carries the new kernel.

A new kernel changes the kernel version that a base snapshot records, so the warm pool builds new base snapshots after a kernel update.
