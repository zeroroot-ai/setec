#!/usr/bin/env bash
# Refuses British spelling in a Go comment (setec#174).
#
# The org spelling guard reads Markdown and nothing else. This guard reads the
# comments of each tracked Go file, with the same word list. Keep the list
# equal to `actions/link-check/check-spelling.sh` in zeroroot-ai/.github.
#
# What it reads: the text after `//` on each line of a tracked `.go` file.
# It does not read generated files (`*.pb.go`, `zz_generated*`). It removes
# code spans in backticks and URLs first, because an identifier is not prose.
# A word in capital letters never matches.
#
# Exemption, by content: the marker `spelling-guard-exempt:` on the same line.
#
#   check-go-comment-spelling.sh             check the tree
#   check-go-comment-spelling.sh --selftest  prove the guard can fail
set -euo pipefail

GUARD_NAME="go-comment-spelling"
EXEMPT_MARKER="spelling-guard-exempt:"

# British stem=American stem. A stem matches the word plus its endings.
SPELLINGS=(
  "analyse=analyze" "analysing=analyzing" "artefact=artifact" "authoris=authoriz"
  "behaviour=behavior" "cancelled=canceled" "catalogue=catalog" "centred=centered"
  "colour=color" "customis=customiz" "defence=defense" "favour=favor"
  "fulfil=fulfill" "grey=gray" "honour=honor" "initialis=initializ"
  "labelled=labeled" "licence=license" "minimis=minimiz" "modelled=modeled"
  "normalis=normaliz" "optimis=optimiz" "organis=organiz" "prioritis=prioritiz"
  "realis=realiz" "recognis=recogniz" "sanitis=sanitiz" "serialis=serializ"
  "standardis=standardiz" "summaris=summariz" "synchronis=synchroniz"
  "utilis=utiliz" "visualis=visualiz" "whilst=while" "amongst=among"
)

pattern() {
  local alts=() entry stem first rest
  for entry in "${SPELLINGS[@]}"; do
    stem="${entry%%=*}"; first="${stem:0:1}"; rest="${stem:1}"
    alts+=("[${first^^}${first}]${rest}")
  done
  local IFS='|'
  echo '\b('"${alts[*]}"')(e|es|ed|er|ers|ing|ation|ations|s|d|r|rs|ion|ions|ment|ments|l|ls|led|ling)?(?![a-z])'
}

# comments <file> prints the comment text of each line, with the line numbers kept.
comments() {
  sed -E 's#https?://[^ )>"]+##g; s/`[^`]*`//g' "$1" | awk '{ i = index($0, "//"); print (i ? substr($0, i + 2) : "") }'
}

# scan <root> prints one "file:line: word" for each violation and returns 1
# when it printed one. It returns 2 when it read no Go file, because a guard
# that looked at nothing must not report a clean tree.
scan() {
  local root="$1" file hit line re found=0 files=0
  re="$(pattern)"
  while IFS= read -r file; do
    case "$file" in *.pb.go|*zz_generated*) continue ;; esac
    [ -f "${root}/${file}" ] || continue
    files=$((files + 1))
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      line="${hit%%:*}"
      sed -n "${line}p" "${root}/${file}" | grep -qF "$EXEMPT_MARKER" && continue
      echo "${file}:${line}: ${hit#*:}"
      found=1
    done < <(comments "${root}/${file}" | grep -noP "$re" || true)
  done < <(git -C "$root" ls-files '*.go')
  if [ "$files" -eq 0 ]; then
    echo "::error::${GUARD_NAME}: read no tracked Go file under ${root}" >&2
    return 2
  fi
  return "$found"
}

selftest() {
  local tmp out rc failures=0
  tmp="$(mktemp -d)"
  git init --quiet -b main "$tmp"
  cat >"$tmp/clean.go" <<'GO'
package p

// Canceled work has the behavior that the license text names. An optimistic lock.
// The value `cancelled` is code. See https://example.com/colour for the theme.
// A MISSION_CANCELLED event. The colour stays here: spelling-guard-exempt: a quotation.
var cancelled = "the behaviour of a string is not a comment"
GO
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a clean tree should pass, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  printf 'package p\n\n// The behaviour is wrong.\nvar x = 1 // Normalises the input.\n' >"$tmp/dirty.go"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"dirty.go:3: behaviour"* ]] || [[ "$out" != *"dirty.go:4: Normalises"* ]]; then
    echo "selftest FAILED: two British words should fail with file and line, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi
  git -C "$tmp" rm --quiet -f dirty.go

  printf 'package p\n\n// The behaviour of generated code is not checked.\n' >"$tmp/zz_generated.deepcopy.go"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a generated file should not be read, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  local empty; empty="$(mktemp -d)"; git init --quiet -b main "$empty"
  set +e; out="$(scan "$empty" 2>&1)"; rc=$?; set -e
  rm -rf "$empty"
  if [ "$rc" -ne 2 ]; then
    echo "selftest FAILED: a tree with no Go file should fail with rc=2, got rc=${rc}"; failures=$((failures + 1))
  fi

  rm -rf "$tmp"
  if [ "$failures" -ne 0 ]; then
    echo "::error::${GUARD_NAME}: selftest FAILED (${failures})" >&2
    return 1
  fi
  echo "PASS: ${GUARD_NAME} selftest (4 cases)"
}

main() {
  case "${1:-}" in
    --selftest) selftest; return ;;
    "") ;;
    *) echo "::error::${GUARD_NAME}: unknown argument: $1" >&2; return 2 ;;
  esac
  local root out rc
  root="$(git rev-parse --show-toplevel)"
  set +e; out="$(scan "$root")"; rc=$?; set -e
  if [ "$rc" -eq 1 ]; then
    echo "::error::${GUARD_NAME}: a Go comment has British spelling:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    echo "Write the American form. For a quotation, put ${EXEMPT_MARKER} and the reason on the line." >&2
    return 1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "PASS: ${GUARD_NAME}: no British spelling in a Go comment"
}

main "$@"
