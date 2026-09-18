#!/usr/bin/env bash
# Fixture for scripts/check-silent-revert.sh (setec#13).
#
# Builds a throwaway repository with a main history, then branches that
# revert one of its commits with and without saying so. Every REQUIRED
# RED case is the guard's reason to exist. The pass cases prove it does
# not fire on ordinary work.
#
# Run by the `no-silent-revert` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/check-silent-revert.test.sh`.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUARD="${HERE}/check-silent-revert.sh"

PASS=0
FAIL=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

# commit <repo> <message> : commit the working tree as-is.
commit() { git -C "$1" add -A >/dev/null && git -C "$1" commit -q -m "$2"; }

# fresh <name> : a repo whose main has four commits. Prints its path.
fresh() {
  local r="$WORK/$1"
  git init -q -b main "$r"
  printf 'package main\n\nfunc main() {}\n' > "$r/main.go"
  printf 'name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
  commit "$r" "chore: initial tree"
  # C1 adds a whole file.
  printf 'set -eu\necho gate\nexit 0\n' > "$r/gate.sh"
  commit "$r" "ci: add the gate script"
  # C2 adds a hunk to an existing file.
  printf 'name: ci\non: [push]\nschedule:\n  - cron: "0 4 * * *"\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
  commit "$r" "ci: run nightly"
  # C3 changes one line.
  printf 'package main\n\nfunc main() { run() }\n' > "$r/main.go"
  commit "$r" "feat: call run"
  # origin/main is what the guard compares against.
  git -C "$r" update-ref refs/remotes/origin/main HEAD
  printf '%s' "$r"
}

sha_of() { git -C "$1" log --format=%H --grep="$2" -n 1; }

# assert <desc> <expect pass|fail> <repo>
assert() {
  local desc="$1" expect="$2" r="$3" result out
  out="$(cd "$r" && bash "$GUARD" 2>&1)" && result=pass || result=fail
  if [ "$result" = "$expect" ]; then
    echo "✅ ${desc} → ${result} (expected ${expect})"
    PASS=$((PASS + 1))
  else
    echo "❌ ${desc} → ${result} (expected ${expect})"
    sed 's/^/      /' <<<"$out"
    FAIL=$((FAIL + 1))
  fi
}

echo "--- required red: undeclared reverts ---"

r="$(fresh silent-file-delete)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
commit "$r" "ci: tidy the tree"
assert "MUTATION deleting a file a main commit added, undeclared" fail "$r"

r="$(fresh silent-hunk-revert)"
git -C "$r" checkout -q -b pr
printf 'name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
commit "$r" "ci: simplify the workflow"
assert "MUTATION reverting a workflow hunk a main commit added, undeclared" fail "$r"

r="$(fresh silent-line-revert)"
git -C "$r" checkout -q -b pr
printf 'package main\n\nfunc main() {}\n' > "$r/main.go"
commit "$r" "fix: drop the call"
assert "MUTATION reverting a one-line change, undeclared" fail "$r"

r="$(fresh silent-many)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
printf 'name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
printf 'package main\n\nfunc main() {}\n' > "$r/main.go"
commit "$r" "ci: drop the vacuous test from the run set"
assert "MUTATION one commit reverting three main commits, undeclared" fail "$r"

r="$(fresh half-declared)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
printf 'package main\n\nfunc main() {}\n' > "$r/main.go"
commit "$r" "$(printf 'revert: drop the gate\n\nReverts: %s\n' "$(sha_of "$r" 'add the gate script')")"
assert "MUTATION two reverts, only one declared" fail "$r"

r="$(fresh wrong-sha)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
commit "$r" "$(printf 'revert: drop the gate\n\nReverts: %s\n' "$(sha_of "$r" 'run nightly')")"
assert "MUTATION revert declared against the wrong commit" fail "$r"

echo "--- pass: declared reverts ---"

r="$(fresh git-revert)"
git -C "$r" checkout -q -b pr
git -C "$r" revert --no-edit "$(sha_of "$r" 'add the gate script')" >/dev/null
assert "git revert (This reverts commit <sha>)" pass "$r"

r="$(fresh trailer)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
printf 'package main\n\nfunc main() {}\n' > "$r/main.go"
commit "$r" "$(printf 'revert: back out the gate and the call\n\nReverts: %s, %s\n' \
  "$(sha_of "$r" 'add the gate script' | cut -c1-12)" "$(sha_of "$r" 'call run')")"
assert "Reverts: trailer naming both commits, short sha allowed" pass "$r"

echo "--- pass: ordinary work ---"

r="$(fresh unrelated)"
git -C "$r" checkout -q -b pr
printf 'package main\n\nfunc main() { run(); stop() }\n' > "$r/main.go"
printf 'set -eu\necho gate\necho more\nexit 0\n' > "$r/gate.sh"
commit "$r" "feat: call stop and say more"
assert "editing lines recent commits added, to new content" pass "$r"

r="$(fresh move)"
git -C "$r" checkout -q -b pr
printf 'name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\nschedule:\n  - cron: "0 4 * * *"\n' > "$r/ci.yml"
commit "$r" "ci: move the schedule block"
assert "moving a hunk inside its file" pass "$r"

# A whole hunk that leaves its file for another file is a move, not a
# revert: what the commit added is still in the tree.
r="$(fresh cross-file-move)"
git -C "$r" checkout -q -b pr
printf 'name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
printf 'schedule:\n  - cron: "0 4 * * *"\n' > "$r/nightly.yml"
commit "$r" "ci: split the schedule into its own workflow"
assert "moving a hunk to another file" pass "$r"

# A renamed file is the same content at a new path.
r="$(fresh rename)"
git -C "$r" checkout -q -b pr
git -C "$r" mv gate.sh scripts-gate.sh
commit "$r" "ci: rename the gate script"
assert "renaming a file a recent commit added" pass "$r"

# A rename that also drops the content is still a revert.
r="$(fresh rename-and-gut)"
git -C "$r" checkout -q -b pr
git -C "$r" mv gate.sh scripts-gate.sh
printf 'exit 0\n' > "$r/scripts-gate.sh"
commit "$r" "ci: rename and simplify the gate script"
assert "MUTATION renaming a file and dropping what the commit added" fail "$r"

r="$(fresh partial)"
git -C "$r" checkout -q -b pr
printf 'name: ci\non: [push]\nschedule:\n  - cron: "0 5 * * *"\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make build\n' > "$r/ci.yml"
commit "$r" "ci: run an hour later"
assert "changing one line of a recent hunk" pass "$r"

r="$(fresh no-change)"
git -C "$r" checkout -q -b pr
git -C "$r" commit -q --allow-empty -m "chore: nothing"
assert "empty diff" pass "$r"

# The window is the last WINDOW_MAX main commits. With a window of one
# commit only C3 is checked, and this branch reverts C1.
r="$(fresh old-commit)"
git -C "$r" checkout -q -b pr
rm "$r/gate.sh"
commit "$r" "ci: tidy the tree"
WINDOW_MAX=1 assert "revert of a commit outside the window" pass "$r"

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "$FAIL" -eq 0 ]
