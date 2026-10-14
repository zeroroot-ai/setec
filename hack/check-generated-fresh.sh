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
# Fails when a generator left the tree different from the commit. Run it
# after `make manifests generate` or `make proto`.
#
# `git diff --exit-code` sees only tracked files. A generator that writes a
# NEW file (a new CRD, a new stub) leaves it untracked, and that check
# passes. `git status --porcelain` sees both.
#
# Usage: hack/check-generated-fresh.sh <what to run to fix it>
#        hack/check-generated-fresh.sh --selftest
set -euo pipefail

check() { # check <fix command>, in the current directory
	local out
	out="$(git status --porcelain --untracked-files=all)"
	if [ -n "$out" ]; then
		printf '%s\n' "$out"
		git --no-pager diff --stat
		echo "::error::generated files are out of date: run '$1' and commit every file it writes"
		return 1
	fi
	echo "generated files are fresh"
}

if [ "${1:-}" != "--selftest" ]; then
	check "${1:?usage: check-generated-fresh.sh <fix command>}"
	exit
fi

# --selftest: a changed tracked file and a new untracked file must each
# fail, and a clean tree must pass (gibson#807 condition 7).
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git -C "$work" init -q
printf 'a\n' >"$work/gen.go"
git -C "$work" add gen.go
git -C "$work" -c user.name=t -c user.email=t@t commit -q -m init
fails=0
expect() { # expect <pass|fail> <desc>
	local got=pass
	(cd "$work" && check "make x") >/dev/null 2>&1 || got=fail
	if [ "$got" = "$1" ]; then echo "  ok    $2 -> $got"; else echo "  FAIL  $2 -> $got, want $1"; fails=$((fails + 1)); fi
}
expect pass "a clean tree"
printf 'b\n' >"$work/gen.go"
expect fail "MUTATION a changed generated file"
git -C "$work" checkout -q -- gen.go
printf 'new\n' >"$work/new_crd.yaml"
expect fail "MUTATION a new generated file, untracked"
echo "check-generated-fresh --selftest: $fails failure(s)"
exit $((fails > 0))
