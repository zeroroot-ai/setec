#!/usr/bin/env bash
# Fixture for the build-file rule of hack/verify-x86-substrate.sh (setec#160).
#
# X86_BUILD_FILES points the rule at crafted files. The rest of the guard runs
# against the real workflows and the real chart, so each case here differs from
# the real tree in the build files only. Each red case asserts its reason.
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

echo
echo "passed=$PASS failed=$FAIL"
if [ "$FAIL" -eq 0 ] && [ "$PASS" -lt 9 ]; then echo "FAIL: only $PASS case(s) ran, want 9"; exit 1; fi
[ "$FAIL" -eq 0 ]
