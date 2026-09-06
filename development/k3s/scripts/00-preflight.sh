#!/usr/bin/env bash
# Read-only host readiness check. Exits non-zero with actionable diagnostics
# on any unmet prerequisite. Run before any other script.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODEPORT="${SETEC_NODEPORT:-30051}"

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
fail=0

check() {
    local label="$1"; shift
    if "$@" >/dev/null 2>&1; then
        green "PASS: $label"
    else
        red   "FAIL: $label"
        fail=1
    fi
}

# OS family
if grep -qiE 'debian|ubuntu' /etc/os-release; then
    green "PASS: Debian/Ubuntu host detected"
else
    red   "FAIL: not a Debian/Ubuntu host (only tested on those)"
    fail=1
fi

# Virtualization extensions
if grep -qE '(vmx|svm)' /proc/cpuinfo; then
    green "PASS: CPU virtualization extensions present (vmx/svm)"
else
    red   "FAIL: no vmx/svm in /proc/cpuinfo — enable virtualization in BIOS/UEFI"
    fail=1
fi

# /dev/kvm
if [[ -c /dev/kvm ]]; then
    green "PASS: /dev/kvm exists"
    if [[ -r /dev/kvm && -w /dev/kvm ]]; then
        green "PASS: /dev/kvm is readable+writable by current user"
    else
        red   "FAIL: /dev/kvm not r/w by current user — run: sudo usermod -aG kvm \$USER && newgrp kvm"
        fail=1
    fi
else
    red   "FAIL: /dev/kvm missing — install qemu-kvm (sudo apt-get install -y qemu-kvm) or check kernel modules"
    fail=1
fi

# Required CLIs
for tool in docker kubectl helm gh openssl jq curl; do
    if command -v "$tool" >/dev/null 2>&1; then
        green "PASS: $tool on PATH"
    else
        red   "FAIL: $tool not on PATH — install it before continuing"
        fail=1
    fi
done

# Port availability
if ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "(^|:)${NODEPORT}\$"; then
    red "FAIL: TCP port ${NODEPORT} already in use — pick a different SETEC_NODEPORT or free the port"
    fail=1
else
    green "PASS: TCP port ${NODEPORT} is free"
fi

# k3s already installed?
if systemctl is-active --quiet k3s 2>/dev/null; then
    green "INFO: k3s service is already active — install scripts will be idempotent"
fi

if [[ $fail -ne 0 ]]; then
    red "Preflight failed. Fix the items above and re-run."
    exit 1
fi
green "All preflight checks passed."
