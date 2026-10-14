#!/usr/bin/env bash
# Copyright 2026 The Setec Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Text that must not appear in the tree. Each rule names a pattern and the
# files it reads:
#
#   markers        TODO, FIXME, XXX or HACK in a Go or Markdown file
#   insecure-flag  auth-disabled or insecure-grpc in charts/setec (mTLS only)
#   unimplemented  codes.Unimplemented in internal/frontend
#
# A rule fails when it finds the pattern, and also when it reads no file.
# A bare `grep -r` on a moved directory exits 2, and an `if grep` reads that
# as "no match", so the old inline checks passed on a path that was gone.
#
# Usage: hack/check-forbidden-text.sh <rule>   [ROOT=dir]
#        hack/check-forbidden-text.sh --selftest
set -euo pipefail

ROOT="${ROOT:-.}"

# scan <rule>: print each match, exit 1 on a match or on nothing read.
scan() {
	local rule="$1" pattern files
	case "$rule" in
	markers)
		pattern='TODO|FIXME|XXX|HACK'
		files="$(find "$ROOT" \( -name vendor -o -name bin -o -name .git -o -name .worktrees \) -prune \
			-o -type f \( -name '*.go' -o -name '*.md' \) -print)"
		;;
	insecure-flag)
		pattern='auth-disabled|insecure-grpc'
		files="$(find "$ROOT/charts/setec" -type f -print 2>/dev/null || true)"
		;;
	unimplemented)
		pattern='codes\.Unimplemented'
		files="$(find "$ROOT/internal/frontend" -type f -name '*.go' -print 2>/dev/null || true)"
		;;
	*)
		echo "check-forbidden-text: unknown rule '$rule'" >&2
		return 2
		;;
	esac
	if [ -z "$files" ]; then
		echo "::error::check-forbidden-text $rule: no file to read under $ROOT; the path moved and the rule reads nothing" >&2
		return 1
	fi
	local found
	found="$(printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 grep -nE -- "$pattern" || true)"
	if [ -n "$found" ]; then
		printf '%s\n' "$found"
		echo "::error::check-forbidden-text $rule: the pattern '$pattern' is in the files above" >&2
		return 1
	fi
	echo "check-forbidden-text $rule: $(printf '%s\n' "$files" | wc -l) file(s), no match"
}

if [ "${1:-}" != "--selftest" ]; then
	scan "${1:?usage: check-forbidden-text.sh <markers|insecure-flag|unimplemented>}"
	exit
fi

# --selftest: each rule must fail on a planted match and on an empty tree,
# and pass on a clean tree (gibson#807 condition 7).
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fails=0
expect() { # expect <pass|fail> <desc> <rule> <root>
	local got=pass
	ROOT="$4" scan "$3" >/dev/null 2>&1 || got=fail
	if [ "$got" = "$1" ]; then
		echo "  ok    $2 -> $got"
	else
		echo "  FAIL  $2 -> $got, want $1"
		fails=$((fails + 1))
	fi
}
mk() { # mk <tree> <file> <content>
	mkdir -p "$(dirname "$work/$1/$2")"
	printf '%s\n' "$3" >"$work/$1/$2"
}
mk clean main.go 'package main'
mk clean README.md 'Setec.'
mk clean charts/setec/values.yaml 'mtls: true'
mk clean internal/frontend/server.go 'return status.Error(codes.NotFound, "x")'
cp -r "$work/clean" "$work/dirty"
mk dirty main.go '// TODO: remove'
mk dirty README.md 'FIXME later'
mk dirty charts/setec/values.yaml 'args: [--insecure-grpc]'
mk dirty internal/frontend/server.go 'return status.Error(codes.Unimplemented, "x")'
mkdir -p "$work/empty"
for rule in markers insecure-flag unimplemented; do
	expect pass "$rule: a clean tree" "$rule" "$work/clean"
	expect fail "$rule: MUTATION a planted match" "$rule" "$work/dirty"
	expect fail "$rule: MUTATION a tree with no file to read" "$rule" "$work/empty"
done
echo "check-forbidden-text --selftest: $fails failure(s)"
exit $((fails > 0))
