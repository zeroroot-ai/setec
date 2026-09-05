#!/usr/bin/env bash
# verify.sh — bake-time gate. Everything a kata-fc node needs must already be
# on disk; fail the build otherwise. (KVM-dependent checks — actually
# booting a microVM, `kata-runtime check` — can only run on the target
# metal instance and are covered by the on-node checklist in README.md.)
#
# Runs as root inside the Packer build instance.
set -euo pipefail

: "${KATA_VERSION:?KATA_VERSION must be set}"

fail=0
check() { # <description> <command...>
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then
        echo "PASS: ${desc}"
    else
        echo "FAIL: ${desc}" >&2
        fail=1
    fi
}

check "kata shim binary present"        test -x /opt/kata/bin/containerd-shim-kata-v2
check "kata-fc shim symlink on PATH"    test -x /usr/local/bin/containerd-shim-kata-fc-v2
check "firecracker binary present"      test -x /opt/kata/bin/firecracker
check "jailer binary present"           test -x /opt/kata/bin/jailer
check "firecracker config present"      test -f /opt/kata/share/defaults/kata-containers/configuration-fc.toml
check "containerd drop-in present"      test -f /etc/containerd/config.d/99-setec-kata-fc.toml
check "thin-pool provisioner present"   test -x /usr/local/sbin/setec-thinpool.sh
check "thin-pool unit enabled"          systemctl is-enabled setec-thinpool.service
check "containerd ordering drop-in"     test -f /etc/systemd/system/containerd.service.d/10-setec-thinpool.conf
check "RuntimeClass manifest baked"     test -f /etc/setec/manifests/runtimeclass-kata-fc.yaml
check "lvm2 installed"                  command -v lvcreate
check "thin_check installed"            command -v thin_check

# Pinned version actually landed.
if /opt/kata/bin/kata-runtime --version | grep -q "${KATA_VERSION}"; then
    echo "PASS: kata-runtime reports ${KATA_VERSION}"
else
    echo "FAIL: kata-runtime version mismatch (want ${KATA_VERSION}):" >&2
    /opt/kata/bin/kata-runtime --version >&2 || true
    fail=1
fi

# The drop-in must parse. Validate it with containerd itself — the real
# consumer — rather than a language TOML library (AL2023 ships python 3.9,
# which predates the stdlib tomllib module). Pointing containerd at the
# drop-in as its config file makes it TOML-decode the file on load;
# `config dump` prints the effective config and exits non-zero on a parse
# error.
DROPIN=/etc/containerd/config.d/99-setec-kata-fc.toml
if containerd -c "${DROPIN}" config dump >/dev/null 2>&1; then
    echo "PASS: containerd accepts the kata-fc drop-in"
else
    echo "FAIL: containerd rejected the kata-fc drop-in:" >&2
    containerd -c "${DROPIN}" config dump >/dev/null || true
    fail=1
fi

if [[ ${fail} -ne 0 ]]; then
    echo "verify.sh: bake verification FAILED" >&2
    exit 1
fi
echo "verify.sh: all bake-time checks passed"
