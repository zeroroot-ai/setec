#!/usr/bin/env bash
# Fixture for hack/verify-x86-substrate.sh (setec#160, gibson#807 condition 7).
#
# X86_BUILD_FILES points the build-file rule at crafted files, X86_WORKFLOW_DIR
# points the platforms rule at crafted workflows, and the chart argument points
# the nodeSelector rule at a crafted chart. Each case differs from the real tree
# in one input only. Each red case asserts its reason.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
PASS=0 FAIL=0
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

ok()  { PASS=$((PASS+1)); echo "ok    $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL  $1"; }

guard() { X86_BUILD_FILES="$1" ./hack/verify-x86-substrate.sh charts/setec 2>&1; }

red() { # red <name> <build files> <expected substring>
  out="$(guard "$2")"; rc=$?
  if [ "$rc" -ne 0 ] && [[ "$out" == *"$3"* ]]; then ok "$1"; else bad "$1 (rc=$rc): $(printf '%s' "$out" | grep -E 'FAIL|build file' | head -3)"; fi
}

# 1. The real tree passes.
out="$(./hack/verify-x86-substrate.sh charts/setec 2>&1)"; rc=$?
if [ "$rc" -eq 0 ]; then ok "the real build files pass"; else bad "the real tree was blocked: $(printf '%s' "$out" | grep FAIL)"; fi

# 2. THE CASE setec#160 IS ABOUT. The kubebuilder default.
printf 'PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le\n' > "$tmp/multi.mk"
red "a multi-arch PLATFORMS default is refused" "$tmp/multi.mk" "names a platform other than linux/amd64"
out="$(guard "$tmp/multi.mk")"
for arch in arm64 s390x ppc64le; do
  if [[ "$out" == *"linux/$arch"* ]]; then ok "the message names linux/$arch"; else bad "the message does not name linux/$arch"; fi
done

# 3. One other platform on a build command line.
printf 'docker-build:\n\tdocker build --platform linux/arm64 -t x .\n' > "$tmp/flag.mk"
red "a --platform flag for arm64 is refused" "$tmp/flag.mk" "2:linux/arm64"

# 4. A comment that describes an arm64 build is refused too.
printf '# multi-arch builds (linux/amd64,linux/arm64) never emulate\nFROM scratch\n' > "$tmp/Dockerfile"
red "a comment that describes an arm64 build is refused" "$tmp/Dockerfile" "1:linux/arm64"

# 5. linux/amd64 alone passes.
printf 'PLATFORMS ?= linux/amd64\nbuild:\n\tdocker build --platform linux/amd64 .\n' > "$tmp/amd64.mk"
out="$(guard "$tmp/amd64.mk")"; rc=$?
if [ "$rc" -eq 0 ]; then ok "linux/amd64 alone passes"; else bad "linux/amd64 was blocked: $(printf '%s' "$out" | grep FAIL)"; fi

# 6. A build file that is missing was not checked, and that is a failure.
red "a missing build file is not a pass" "$tmp/absent.mk" "is missing or empty"

# 7. A multi-arch platforms input in a workflow is refused.
mkdir -p "$tmp/wf-multi" "$tmp/wf-none"
printf 'jobs:\n  images:\n    with:\n      platforms: linux/amd64,linux/arm64\n' > "$tmp/wf-multi/images.yml"
out="$(X86_WORKFLOW_DIR="$tmp/wf-multi" ./hack/verify-x86-substrate.sh charts/setec 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [[ "$out" == *"non-amd64-only platforms input"* ]]; then ok "a multi-arch platforms input is refused"; else bad "a multi-arch platforms input passed (rc=$rc)"; fi

# 8. A workflow tree with no platforms input read nothing, and that is a failure.
printf 'jobs: {}\n' > "$tmp/wf-none/ci.yml"
out="$(X86_WORKFLOW_DIR="$tmp/wf-none" ./hack/verify-x86-substrate.sh charts/setec 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [[ "$out" == *"no 'platforms:' inputs found"* ]]; then ok "no platforms input is not a pass"; else bad "no platforms input passed (rc=$rc)"; fi

# 9. A device plugin DaemonSet that lost its arch selector is refused.
cp -r charts/setec "$tmp/chart"
sed -i '/kubernetes.io\/arch: amd64/d' "$tmp/chart/templates/device-plugin-daemonset.yaml"
out="$(./hack/verify-x86-substrate.sh "$tmp/chart" 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [[ "$out" == *"device plugin pins amd64"* ]]; then ok "a device plugin with no arch selector is refused"; else bad "a device plugin with no arch selector passed (rc=$rc)"; fi

echo
echo "passed=$PASS failed=$FAIL"
if [ "$FAIL" -eq 0 ] && [ "$PASS" -lt 12 ]; then echo "FAIL: only $PASS case(s) ran, want 12"; exit 1; fi
[ "$FAIL" -eq 0 ]
