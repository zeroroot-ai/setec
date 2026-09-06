#!/usr/bin/env bash
# Verify a Pod scheduled with runtimeClassName: kata-qemu actually boots in
# a qemu/kvm microVM (kernel string differs from the host). Hard-fail if it
# silently falls back to runc.
#
# Uses kata-qemu (not kata-fc) because kata-fc requires the containerd
# `devmapper` snapshotter, which in turn needs a thin-pool block device.
# Setting that up is Setec's node-agent territory — out of scope for a
# phase-0 smoke test. kata-qemu works with the default overlayfs
# snapshotter and still provides full microVM hardware isolation.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${ROOT}/kubeconfig"

POD=kata-smoke-$RANDOM

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

cleanup() { kubectl delete pod "${POD}" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true; }
trap cleanup EXIT

green "Launching Pod ${POD} with runtimeClassName: kata-qemu"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
spec:
  runtimeClassName: kata-qemu
  restartPolicy: Never
  containers:
    - name: smoke
      image: alpine:3
      command: ["uname", "-r"]
EOF

# Wait completion (up to 5m: image pull + microVM cold boot)
kubectl wait --for=condition=Ready=false --timeout=5m pod/"${POD}" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + 300 ))
while :; do
    phase="$(kubectl get pod "${POD}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]] && break
    [[ $(date +%s) -gt $deadline ]] && { red "FAIL: Pod did not complete within 5m (phase=${phase})"; kubectl describe pod "${POD}"; exit 1; }
    sleep 3
done

vm_kernel="$(kubectl logs "${POD}" 2>/dev/null | head -1)"
host_kernel="$(uname -r)"

green "VM   kernel: ${vm_kernel}"
green "Host kernel: ${host_kernel}"

if [[ -z "${vm_kernel}" ]]; then
    red "FAIL: empty Pod logs"
    kubectl describe pod "${POD}"
    exit 1
fi
if [[ "${vm_kernel}" == "${host_kernel}" ]]; then
    red "FAIL: VM kernel matches host — silently fell back to runc, kata not in use"
    exit 1
fi
green "PASS: kata-qemu boots a real microVM"
