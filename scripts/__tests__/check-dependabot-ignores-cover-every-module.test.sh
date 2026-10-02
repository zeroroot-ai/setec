#!/usr/bin/env bash
#
# Fixture for check-dependabot-ignores-cover-every-module.sh.
#
# Every red case asserts a substring of the message, so a rule that starts
# failing for a different reason is a test failure rather than a pass. Offline:
# crafted dependabot.yml and go.mod trees only, no network, no Dependabot.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE=scripts/check-dependabot-ignores-cover-every-module.sh
PASS=0 FAIL=0
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# mkmod <reldir> <require-line>...
mkmod() {
  local d="$tmp/$1"; shift
  mkdir -p "$d"
  { echo "module example.com/$1"; echo; echo "go 1.27"; echo; echo "require ("; 
    for line in "$@"; do echo -e "\t$line"; done
    echo ")"; } > "$d/go.mod"
}

# cfg <yaml>
cfg() { printf '%s\n' "$1" > "$tmp/dependabot.yml"; }

run() { CONFIG="$tmp/dependabot.yml" ROOT="$tmp" bash "$GATE" 2>&1; }

red() {
  local name="$1" want="$2" out
  out="$(run)"
  if [ $? -eq 0 ]; then FAIL=$((FAIL+1)); echo "  FAIL: $name was allowed"; return; fi
  case "$out" in
    *"$want"*) PASS=$((PASS+1)) ;;
    *) FAIL=$((FAIL+1)); echo "  FAIL: $name failed for the wrong reason; wanted '$want', got: $out" ;;
  esac
}
green() {
  local name="$1" out
  out="$(run)"
  if [ $? -ne 0 ]; then FAIL=$((FAIL+1)); echo "  FAIL: $name was blocked: $out"; else PASS=$((PASS+1)); fi
}

mkdir -p "$tmp/a" "$tmp/b"
mkmod a a "google.golang.org/grpc v1.83.2" "example.com/other v1.0.0"
mkmod b b "google.golang.org/grpc v1.83.2"

BOTH='version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /b
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]'

# 1. THE CASE THIS EXISTS FOR. setec#125: the ignore covered one of two modules.
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /b'
red "an ignore that covers one of two modules" "has no ignore for it"

# 2. Both covered: green.
cfg "$BOTH"
green "an ignore that covers every module requiring the dependency"

# 3. One advisory, one version list. Two modules ignoring different versions of
#    the same dependency means one of them is still being offered a bad release.
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /b
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.1"]'
red "two modules ignoring different versions" "different versions per module"

# 4. A module that does NOT require the dependency needs no ignore. Requiring one
#    there would be dead config that reads as protection.
mkmod b b "example.com/unrelated v1.0.0"
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /b'
green "a module that does not require the dependency needs no ignore"

# 5. An INDIRECT require is not offered a bump, so it needs no ignore. This is the
#    same exclusion check-direct-requires-have-no-advisory.sh makes.
mkmod b b "google.golang.org/grpc v1.83.2 // indirect"
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /b'
green "an indirect require needs no ignore"

# 6. A gomod entry naming a directory with no go.mod cannot be checked, and must
#    say so rather than quietly contributing nothing.
mkmod b b "google.golang.org/grpc v1.83.2"
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]
  - package-ecosystem: gomod
    directory: /nope'
red "a gomod directory with no go.mod" "has no go.mod"

# 7. No gomod updates at all: nothing to compare, and reporting ok would be a
#    guard that cannot fail.
cfg 'version: 2
updates:
  - package-ecosystem: docker
    directory: /'
red "a config with no gomod update" "declares no gomod update"

# 8. THE FLOOR. One module parsed is not a comparison.
rm -rf "$tmp/b"
cfg 'version: 2
updates:
  - package-ecosystem: gomod
    directory: /a
    ignore:
      - dependency-name: google.golang.org/grpc
        versions: ["1.84.0"]'
red "a run that parsed a single module" "go.mod discovery is not working"

echo
echo "passed=$PASS failed=$FAIL"
if [ "$PASS" -lt 8 ] && [ "$FAIL" -eq 0 ]; then
  echo "FAIL: only $PASS case(s) ran; cases were added below the summary block" >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
