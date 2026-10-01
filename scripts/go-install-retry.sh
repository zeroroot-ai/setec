#!/usr/bin/env bash
# go-install-retry.sh — `go install`, retried past a transient checksum-database blip.
#
# `go build` and `go install` verify every module against the public Go
# checksum database. sum.golang.org occasionally resets an in-flight HTTP/2
# stream, and Go reports that as a build failure rather than as the retryable
# network error it is:
#
#   google.golang.org/genproto/googleapis/rpc@v0.0.0-20241104194629-dd2ea8efbc28:
#   verifying module: reading https://sum.golang.org/tile/8/0/x122/632:
#   stream error: stream ID 629; INTERNAL_ERROR; received from peer
#
# So any PR could hit a red `Proto up-to-date` for a reason outside its diff,
# and the only remedy was noticing sum.golang.org in the log and rerunning the
# job (setec#98). Seen on PR #95, run 36421909653, while building bin/buf.
#
# This retries the install, with backoff, up to GO_INSTALL_ATTEMPTS times.
#
# It does NOT disable verification. GOFLAGS, GONOSUMDB and GOSUMDB=off would
# all make this error go away by not checking the checksum, which trades a
# flaky job for an unverified supply chain. The check stays on; only the
# transport gets another go.
#
# A real checksum MISMATCH is never retried. That is a security signal, and
# repeating it three times would delay it and bury it under two more copies.
# It fails on the first attempt, loudly.
#
#   go-install-retry.sh <package@version>
#   go-install-retry.sh --selftest
set -uo pipefail

ATTEMPTS="${GO_INSTALL_ATTEMPTS:-3}"
BACKOFF="${GO_INSTALL_BACKOFF:-5}"

# A verification mismatch, as Go words it. Never retried.
MISMATCH_RX='SECURITY ERROR|checksum mismatch|does not match|verifying.*: invalid'

install_once() { # <package@version> <log>
  go install "$1" >"$2" 2>&1
}

run() { # <package@version>
  local pkg="$1" log rc attempt=1
  log="$(mktemp)"
  # shellcheck disable=SC2064
  trap "rm -f '$log'" RETURN

  while :; do
    install_once "$pkg" "$log"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      [ "$attempt" -gt 1 ] && echo "go install $pkg: succeeded on attempt $attempt"
      cat "$log"
      return 0
    fi

    if grep -qE "$MISMATCH_RX" "$log"; then
      echo "::error::go install $pkg: module verification FAILED — this is not a transient error and is not retried."
      cat "$log" >&2
      return 1
    fi

    if [ "$attempt" -ge "$ATTEMPTS" ]; then
      echo "::error::go install $pkg: failed $ATTEMPTS time(s)."
      cat "$log" >&2
      return "$rc"
    fi

    echo "go install $pkg: attempt $attempt failed, retrying in ${BACKOFF}s" >&2
    tail -3 "$log" >&2
    sleep "$BACKOFF"
    attempt=$((attempt + 1))
  done
}

selftest() {
  local work pass=0 fail=0
  work="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$work'" RETURN
  ok()  { echo "PASS: $1"; pass=$((pass + 1)); }
  bad() { echo "FAIL: $1"; fail=$((fail + 1)); }

  # A fake `install_once` stands in for the network, so the selftest is
  # hermetic and fast. ATTEMPT_LOG counts how many times it ran.
  local counter="$work/attempts"

  # 1. A transient failure that clears is retried and succeeds.
  : >"$counter"
  install_once() {
    echo x >>"$counter"
    if [ "$(wc -l <"$counter")" -lt 3 ]; then
      echo "verifying module: reading https://sum.golang.org/tile/8/0/x122/632: stream error: stream ID 629; INTERNAL_ERROR" >"$2"
      return 1
    fi
    echo "ok" >"$2"; return 0
  }
  if GO_INSTALL_BACKOFF=0 run fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "3" ]; then
    ok "a transient sum.golang.org blip is retried and succeeds"
  else
    bad "a transient blip did not recover (attempts: $(wc -l <"$counter"))"
  fi

  # 2. A persistent failure still fails, after exactly ATTEMPTS tries.
  : >"$counter"
  install_once() { echo x >>"$counter"; echo "stream error: INTERNAL_ERROR" >"$2"; return 1; }
  if ! GO_INSTALL_BACKOFF=0 GO_INSTALL_ATTEMPTS=3 run fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "3" ]; then
    ok "a persistent failure still fails, after exactly 3 attempts"
  else
    bad "a persistent failure did not fail after 3 attempts (attempts: $(wc -l <"$counter"))"
  fi

  # 3. A checksum mismatch fails on the FIRST attempt. Never retried.
  for msg in \
    "SECURITY ERROR: checksum for module does not match" \
    "verifying module: checksum mismatch"
  do
    : >"$counter"
    install_once() { echo x >>"$counter"; echo "$MSG" >"$2"; return 1; }
    MSG="$msg"
    if ! MSG="$msg" GO_INSTALL_BACKOFF=0 run fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "1" ]; then
      ok "a verification mismatch fails immediately: ${msg:0:34}…"
    else
      bad "a verification mismatch was retried (attempts: $(wc -l <"$counter"))"
    fi
  done

  # 4. A first-attempt success runs exactly once.
  : >"$counter"
  install_once() { echo x >>"$counter"; echo ok >"$2"; return 0; }
  if GO_INSTALL_BACKOFF=0 run fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "1" ]; then
    ok "a clean install runs exactly once"
  else
    bad "a clean install did not run exactly once (attempts: $(wc -l <"$counter"))"
  fi

  echo
  echo "$pass passed, $fail failed"
  [ "$fail" -eq 0 ]
}

main() {
  case "${1:-}" in
    --selftest) selftest ;;
    "" | -h | --help)
      echo "usage: $0 <package@version> | $0 --selftest" >&2; exit 2 ;;
    *) run "$1" ;;
  esac
}

main "$@"
