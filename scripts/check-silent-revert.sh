#!/usr/bin/env bash
# check-silent-revert.sh — a PR that undoes a merged commit must say so.
#
# Why (setec#13)
# --------------
# A PR whose parent was the main tip once carried one commit that reverted
# five merged PRs (1,942 deletions under a one-line `ci(...)` title). Every
# check was green: nothing in CI compared the PR's diff against the history
# it undid. This guard does that comparison, keyed by content.
#
# What it does
# ------------
# 1. base = merge-base(origin/main, HEAD). The PR diff is base..HEAD. On a
#    pull_request run HEAD is the PR merge ref; on a merge_group run it is
#    the queue's merge commit. Both give the diff that would land on main.
# 2. For every non-merge commit on main in the window (WINDOW_DAYS back
#    from base, at most WINDOW_MAX commits), and for every file that commit
#    touched, the commit's change to that file is REVERTED when the PR diff
#    removes every line the commit added there and adds back every line the
#    commit removed there, hunk by hunk, in order, and does not also re-add
#    what it removed anywhere in the diff (a move, within a file or to
#    another file, is not a revert). Renames are followed, so a renamed
#    file is not a deleted one.
# 3. A revert is DECLARED when a commit message in base..HEAD names the
#    reverted commit: `This reverts commit <sha>` (what `git revert`
#    writes) or a `Reverts: <sha>[, <sha>...]` trailer. Seven or more hex
#    characters of the sha are enough.
# 4. Every undeclared revert is an error. Declared reverts are printed and
#    pass.
#
# Only exact content reversal fires. A PR that changes a line a recent
# commit added to something new is not a revert of that commit.
#
# Usage: check-silent-revert.sh [--base <ref>] [--head <ref>] [--upstream <ref>]
#        exit 1 on an undeclared revert
#   WINDOW_DAYS  how far back on main to look (default 30)
#   WINDOW_MAX   at most this many main commits (default 200)
set -uo pipefail

UPSTREAM="origin/main"
HEAD_REF="HEAD"
BASE_REF=""
WINDOW_DAYS="${WINDOW_DAYS:-30}"
WINDOW_MAX="${WINDOW_MAX:-200}"

while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE_REF="$2"; shift 2 ;;
    --head) HEAD_REF="$2"; shift 2 ;;
    --upstream) UPSTREAM="$2"; shift 2 ;;
    *) echo "::error::unknown argument: $1"; exit 2 ;;
  esac
done

if [ -z "$BASE_REF" ]; then
  BASE_REF="$(git merge-base "$UPSTREAM" "$HEAD_REF")" || {
    echo "::error::cannot compute merge-base of ${UPSTREAM} and ${HEAD_REF}; fetch the full history (fetch-depth: 0)"
    exit 2
  }
fi
BASE="$(git rev-parse --verify "${BASE_REF}^{commit}")" || exit 2
HEAD_SHA="$(git rev-parse --verify "${HEAD_REF}^{commit}")" || exit 2

# Every declared sha prefix in the PR's own commit messages.
declared="$(git log --no-merges --format=%B "${BASE}..${HEAD_SHA}" \
  | grep -oiE '(this reverts commit [0-9a-f]{7,40}|^reverts:[[:space:]]*[0-9a-f]{7,40}([[:space:]]*,[[:space:]]*[0-9a-f]{7,40})*)' \
  | grep -oE '[0-9a-f]{7,40}' | tr 'A-F' 'a-f' | sort -u)"

is_declared() {
  local sha="$1" d
  for d in $declared; do
    case "$sha" in "$d"*) return 0 ;; esac
  done
  return 1
}

# lines_of <from> <to>: "<sign>\t<file>\t<hunk>\t<line>" for each added (+)
# or removed (-) line of the diff, no context. Renames are detected, so a
# renamed file contributes only the lines that changed inside it, keyed by
# its new path. Hunks are numbered per file.
lines_of() {
  git diff --no-color --unified=0 --find-renames --no-ext-diff "$1" "$2" \
    | awk '
      /^diff --git / { file = substr($0, index($0, " b/") + 3); hunk = 0; next }
      /^(\+\+\+|---) /  { next }
      /^@@ /            { hunk++; next }
      /^Binary files /  { next }
      /^\+/             { printf "+\t%s\t%d\t%s\n", file, hunk, substr($0, 2); next }
      /^-/              { printf "-\t%s\t%d\t%s\n", file, hunk, substr($0, 2); next }
    '
}

pr_lines="$(lines_of "$BASE" "$HEAD_SHA")"
if [ -z "$pr_lines" ]; then
  echo "no textual change between ${BASE:0:12} and ${HEAD_SHA:0:12}; nothing to check"
  exit 0
fi

# compare <pr-lines> <commit-lines>: prints one "<file>" per file whose
# change in the commit the PR reverts in full.
compare() {
  awk -F'\t' '
    # SEP-wrapped joins: SEP a SEP b SEP. A search for SEP x SEP y SEP then
    # matches only whole lines in sequence, never a substring of a line.
    function join(arr, n,   i, s) { s = SEP; for (i = 1; i <= n; i++) s = s arr[i] SEP; return s }
    function blank(s) { return s ~ /^[[:space:]]*$/ }
    BEGIN { SEP = "\037" }
    FILENAME == ARGV[1] {
      # PR side: per file, the removed lines in order and the added lines in
      # order, plus the same across every file for the move check.
      f = $2
      if ($1 == "+") { npa[f]++; pa[f, npa[f]] = $4; nall_a++; all_a[nall_a] = $4 }
      else            { npr[f]++; pr[f, npr[f]] = $4; nall_r++; all_r[nall_r] = $4 }
      next
    }
    {
      # Commit side: per file and hunk, the added and removed sequences.
      f = $2; h = $3
      key = f SUBSEP h
      if (!(key in seen)) { seen[key] = 1; nh[f]++; hk[f, nh[f]] = h }
      if ($1 == "+") { nca[key]++; ca[key, nca[key]] = $4; if (!blank($4)) real[key] = 1 }
      else            { ncr[key]++; cr[key, ncr[key]] = $4; if (!blank($4)) real[key] = 1 }
      files[f] = 1
    }
    END {
      # Every added and every removed line of the PR, in diff order.
      n = nall_a; for (i = 1; i <= n; i++) tmp[i] = all_a[i]; padd_all = join(tmp, n)
      n = nall_r; for (i = 1; i <= n; i++) tmp[i] = all_r[i]; prem_all = join(tmp, n)
      for (f in files) {
        # Join the PR side once per file.
        n = npr[f]; for (i = 1; i <= n; i++) tmp[i] = pr[f, i]; prem = join(tmp, n)
        n = npa[f]; for (i = 1; i <= n; i++) tmp[i] = pa[f, i]; padd = join(tmp, n)
        reverted = 1; content = 0
        for (j = 1; j <= nh[f]; j++) {
          key = f SUBSEP hk[f, j]
          if (!(key in real)) continue
          content = 1
          n = nca[key]; for (i = 1; i <= n; i++) tmp[i] = ca[key, i]; addseq = (n > 0) ? join(tmp, n) : ""
          n = ncr[key]; for (i = 1; i <= n; i++) tmp[i] = cr[key, i]; remseq = (n > 0) ? join(tmp, n) : ""
          # What the commit added must be gone from this file, and not
          # merely moved somewhere else in the diff. What the commit
          # removed must be back in this file, and not merely moved.
          if (addseq != "" && (index(prem, addseq) == 0 || index(padd_all, addseq) > 0)) { reverted = 0; break }
          if (remseq != "" && (index(padd, remseq) == 0 || index(prem_all, remseq) > 0)) { reverted = 0; break }
        }
        if (content && reverted) print f
      }
    }
  ' "$1" "$2"
}

pr_file="$(mktemp)"; trap 'rm -f "$pr_file" "$c_file"' EXIT
printf '%s\n' "$pr_lines" > "$pr_file"
c_file="$(mktemp)"

rc=0
declared_count=0
window="$(git rev-list --no-merges --max-count="$WINDOW_MAX" --since="${WINDOW_DAYS}.days" "$BASE")"
echo "checking ${BASE:0:12}..${HEAD_SHA:0:12} against $(printf '%s\n' "$window" | grep -c .) main commit(s) from the last ${WINDOW_DAYS} days"

for c in $window; do
  # A root commit has no parent to diff against.
  git rev-parse --verify --quiet "${c}^" >/dev/null || continue
  lines_of "${c}^" "$c" > "$c_file"
  [ -s "$c_file" ] || continue
  files="$(compare "$pr_file" "$c_file" | sort)"
  [ -n "$files" ] || continue
  subject="$(git log -1 --format=%s "$c")"
  if is_declared "$c"; then
    declared_count=$((declared_count + 1))
    echo "declared revert of ${c:0:12} (${subject}):"
    printf '  %s\n' $files
    continue
  fi
  echo "::error::the diff reverts ${c:0:12} (${subject}) in: $(printf '%s ' $files)"
  rc=1
done

if [ "$rc" -ne 0 ]; then
  echo "::error::a PR that undoes a merged commit must say so: add 'Reverts: <sha>' to a commit message, or stop reverting it (setec#13)"
  exit 1
fi
echo "no undeclared revert of a main commit (declared: ${declared_count})"
