#!/usr/bin/env bash
# Fixture for scripts/check-direct-requires-have-no-advisory.sh (setec#97).
#
# Builds throwaway module trees and serves canned OSV responses from a local
# stub, so the parser and the result mapping are tested without the network.
# One case then queries the REAL OSV endpoint for grpc v1.84.0, the version
# this guard was written for, so a change in the API shape fails here instead
# of passing silently in CI.
#
# Every REQUIRED RED case is the guard's reason to exist.
#
# Run by the `direct-require-advisories` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/check-direct-requires-have-no-advisory.test.sh`.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUARD="${HERE}/check-direct-requires-have-no-advisory.sh"

PASS=0
FAIL=0
WORK="$(mktemp -d)"
STUB_PID=""
cleanup() { [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

command -v go >/dev/null || { echo "SKIP-IS-FAILURE: go not on PATH"; exit 1; }
command -v jq >/dev/null || { echo "SKIP-IS-FAILURE: jq not on PATH"; exit 1; }
command -v python3 >/dev/null || { echo "SKIP-IS-FAILURE: python3 not on PATH"; exit 1; }

# ---------------------------------------------------------------------------
# A local OSV stub. It answers querybatch from a file of per-query verdicts:
# one line per query, either empty (clean) or a comma-separated id list. The
# stub echoes one results[] entry per query it was SENT, so a case can also
# make the counts disagree on purpose.
# ---------------------------------------------------------------------------
cat > "$WORK/stub.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        queries = body.get('queries', [])
        # Verdicts are keyed by module path, not by position: the guard sends
        # the rows in `git ls-files` order, and a positional fixture silently
        # asserts against whichever row happened to be first.
        verdicts = {}
        for ln in open(os.environ['STUB_VERDICTS']):
            if '\t' not in ln:
                continue
            name, ids = ln.rstrip('\n').split('\t', 1)
            verdicts[name] = ids
        try:
            mode = open(os.environ['STUB_MODE_FILE']).read().strip() or 'ok'
        except OSError:
            mode = 'ok'
        if mode == 'error':
            self.send_response(500); self.end_headers(); self.wfile.write(b'nope'); return
        if mode == 'short':
            queries = queries[:-1]
        out = []
        for q in queries:
            ids = verdicts.get(q['package']['name'], '')
            out.append({} if not ids else {'vulns': [{'id': x} for x in ids.split(',')]})
        payload = json.dumps({'results': out}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers(); self.wfile.write(payload)
    def log_message(self, *a): pass

srv = HTTPServer(('127.0.0.1', 0), H)
print(srv.server_address[1], flush=True)
srv.serve_forever()
PY

export STUB_VERDICTS="$WORK/verdicts"
: > "$STUB_VERDICTS"
export STUB_MODE_FILE="$WORK/mode"
echo ok > "$STUB_MODE_FILE"
python3 -u "$WORK/stub.py" > "$WORK/port" 2>/dev/null &
STUB_PID=$!
for _ in $(seq 1 50); do [ -s "$WORK/port" ] && break; sleep 0.1; done
PORT="$(head -1 "$WORK/port")"
[ -n "$PORT" ] || { echo "the OSV stub never reported a port"; exit 1; }
STUB="http://127.0.0.1:${PORT}/v1/querybatch"

# mod <dir> <module> <require-lines...> : write a go.mod, no network needed.
# `line` is declared local on purpose: an undeclared loop variable here once
# clobbered the caller's `$r` (the repo path), and every case then cd'd into a
# module path instead of a directory.
mod() {
  local d="$1" name="$2" line; shift 2
  sandbox "$d"
  mkdir -p "$d"
  { echo "module $name"; echo; echo "go 1.27.1"; echo; echo "require ("
    for line in "$@"; do echo "	$line"; done
    echo ")"; } > "$d/go.mod"
}

# sandbox <path> : refuse any git or rm outside $WORK.
#
# `mod` once clobbered the caller's repo-path variable, so `git -C "$r" add -A`
# ran in THIS repository: it staged throwaway module trees as real paths named
# `google.golang.org/grpc v1.83.2/` and wrote three `init` commits onto the
# working branch. Every git call below goes through this check first, so the
# next variable mistake fails the fixture instead of committing to the repo.
sandbox() {
  case "$1" in
    "$WORK"/*) return 0 ;;
    *) echo "FIXTURE BUG: refusing to touch '$1', which is outside $WORK" >&2; exit 1 ;;
  esac
}

# gitx <repo> <args...> : git inside a sandboxed repo.
gitx() { sandbox "$1"; local r="$1"; shift; git -C "$r" "$@"; }

# mktree <name> : a repo with a root module and one example module.
mktree() {
  local r="$WORK/$1"
  sandbox "$r"
  mkdir -p "$r"
  gitx "$r" init -q 2>/dev/null
  mod "$r" example.test/root "github.com/spf13/cobra v1.8.1"
  mod "$r/examples/one" example.test/one "google.golang.org/grpc v1.83.2"
  gitx "$r" add -A >/dev/null 2>&1
  gitx "$r" -c user.email=f@invalid -c user.name=f commit -qm init >/dev/null 2>&1
  echo "$r"
}

# run_case <name> <expect rc> <mode> ["<module>\t<ids>"...]
# A module with no line is clean.
run_case() {
  local name="$1" want="$2" mode="$3"; shift 3
  printf '%s\n' "$@" > "$STUB_VERDICTS"
  printf '%s\n' "$mode" > "$STUB_MODE_FILE"
  local repo; repo="$(mktree "r$RANDOM$RANDOM")"
  local out rc
  out="$(OSV_ENDPOINT="$STUB" bash "$GUARD" --root "$repo" 2>&1)"; rc=$?
  echo ok > "$STUB_MODE_FILE"
  if [ "$rc" = "$want" ]; then
    PASS=$((PASS+1)); printf '  ok   %s (rc=%s)\n' "$name" "$rc"
  else
    FAIL=$((FAIL+1)); printf '  FAIL %s: want rc=%s got rc=%s\n' "$name" "$want" "$rc"
    printf '%s\n' "$out" | sed 's/^/       /'
  fi
  LAST_OUT="$out"
}

echo "== stubbed cases"

# PASS: both direct requires clean. No verdict line means clean.
run_case "clean tree passes" 0 ok

# REQUIRED RED: an affected direct require in the example module.
run_case "affected example require fails" 1 ok "$(printf 'google.golang.org/grpc\tGO-2026-6443')"
case "$LAST_OUT" in
  *"examples/one/go.mod"*"google.golang.org/grpc v1.83.2"*"GO-2026-6443"*)
    PASS=$((PASS+1)); echo "  ok   the message names the module, the dep, the version and the id" ;;
  *) FAIL=$((FAIL+1)); echo "  FAIL the message does not name all four"; printf '%s\n' "$LAST_OUT" | sed 's/^/       /' ;;
esac

# REQUIRED RED: an affected direct require in the root module.
run_case "affected root require fails" 1 ok "$(printf 'github.com/spf13/cobra\tGO-0000-0001')"

# REQUIRED RED: a failed query must not read as "no advisories".
run_case "a 500 from OSV fails the check" 1 error
case "$LAST_OUT" in
  *"OSV query failed"*) PASS=$((PASS+1)); echo "  ok   it says the query failed" ;;
  *) FAIL=$((FAIL+1)); echo "  FAIL a failed query did not say so" ;;
esac

# REQUIRED RED: results[] shorter than the queries would misalign the mapping.
run_case "a short results[] fails instead of mapping" 1 short
case "$LAST_OUT" in
  *"refusing to map"*) PASS=$((PASS+1)); echo "  ok   it refuses to map a short response" ;;
  *) FAIL=$((FAIL+1)); echo "  FAIL a short response was mapped anyway" ;;
esac

# REQUIRED RED: an indirect-only tree parses zero direct requires, which is a
# broken parser rather than a clean tree.
repo="$(mktree "ronly$RANDOM")"
mod "$repo" example.test/root "github.com/spf13/cobra v1.8.1 // indirect"
mod "$repo/examples/one" example.test/one "google.golang.org/grpc v1.83.2 // indirect"
gitx "$repo" add -A >/dev/null 2>&1
out="$(OSV_ENDPOINT="$STUB" bash "$GUARD" --root "$repo" 2>&1)"; rc=$?
if [ "$rc" = "1" ] && [ "${out#*parsed 0 direct requires}" != "$out" ]; then
  PASS=$((PASS+1)); echo "  ok   zero direct requires fails as a broken parser"
else
  FAIL=$((FAIL+1)); echo "  FAIL zero direct requires did not fail loudly (rc=$rc)"
  printf '%s\n' "$out" | sed 's/^/       /'
fi

# An indirect require that IS affected must not fire: reachability is
# govulncheck's job and the full graph would red-wall this gate.
repo="$(mktree "rind$RANDOM")"
mod "$repo" example.test/root "github.com/spf13/cobra v1.8.1" "google.golang.org/grpc v1.84.0 // indirect"
gitx "$repo" add -A >/dev/null 2>&1
: > "$STUB_VERDICTS"
out="$(OSV_ENDPOINT="$STUB" bash "$GUARD" --root "$repo" 2>&1)"; rc=$?
if [ "$rc" = "0" ] && [ "${out#*2 direct require}" != "$out" ]; then
  PASS=$((PASS+1)); echo "  ok   an affected indirect require is not queried"
else
  FAIL=$((FAIL+1)); echo "  FAIL indirect requires reached the query (rc=$rc)"
  printf '%s\n' "$out" | sed 's/^/       /'
fi

echo "== live OSV case (the API shape this guard depends on)"

# The real endpoint, the real advisory, the exact version #81 introduced. This
# is not a nicety: if OSV renames `results[]` or `vulns[]`, every stubbed case
# above still passes and the gate in CI silently stops finding anything.
repo="$(mktree "rlive$RANDOM")"
mod "$repo/examples/one" example.test/one "google.golang.org/grpc v1.84.0"
gitx "$repo" add -A >/dev/null 2>&1
out="$(bash "$GUARD" --root "$repo" 2>&1)"; rc=$?
if [ "$rc" = "1" ] && [ "${out#*GO-2026-6443}" != "$out" ]; then
  PASS=$((PASS+1)); echo "  ok   live OSV reports grpc v1.84.0 as affected by GO-2026-6443"
else
  FAIL=$((FAIL+1)); echo "  FAIL live OSV did not report grpc v1.84.0 (rc=$rc)"
  printf '%s\n' "$out" | sed 's/^/       /'
fi

# And the version this PR moves to must come back clean, so the fix is the fix.
repo="$(mktree "rlive2$RANDOM")"
mod "$repo/examples/one" example.test/one "google.golang.org/grpc v1.83.2"
gitx "$repo" add -A >/dev/null 2>&1
out="$(bash "$GUARD" --root "$repo" 2>&1)"; rc=$?
if [ "$rc" = "0" ]; then
  PASS=$((PASS+1)); echo "  ok   live OSV reports grpc v1.83.2 as clean"
else
  FAIL=$((FAIL+1)); echo "  FAIL live OSV flagged v1.83.2 (rc=$rc)"
  printf '%s\n' "$out" | sed 's/^/       /'
fi

echo
echo "passed ${PASS}, failed ${FAIL}"
[ "$FAIL" = "0" ]
