#!/usr/bin/env bash
# go-retry.sh — a go command, retried past a transient module-network blip.
#
# `go build`, `go install` and `go mod download` verify every module against the
# public Go checksum database. sum.golang.org and proxy.golang.org occasionally
# reset an in-flight HTTP/2 stream, and Go reports that as a build failure
# rather than as the retryable network error it is:
#
#   google.golang.org/genproto/googleapis/rpc@v0.0.0-20241104194629-dd2ea8efbc28:
#   verifying module: reading https://sum.golang.org/tile/8/0/x122/632:
#   stream error: stream ID 629; INTERNAL_ERROR; received from peer
#
#   go: k8s.io/api@v0.37.0: read
#   "https://proxy.golang.org/k8s.io/api/@v/v0.37.0.zip":
#   stream error: stream ID 873; INTERNAL_ERROR; received from peer
#
# So any PR could hit a red `Proto up-to-date`, or a red image build, for a
# reason outside its diff, and the only remedy was noticing the host in the log
# and rerunning the job (setec#98). The first form was seen on PR #95, run
# 36421909653, while building bin/buf; the second on release PR #80, in
# `go mod download` inside the setec-guest-agent image build.
#
# It wraps a WHOLE go command, not just `go install`, because those are the two
# call sites that hit this: `go install` from the Makefile and `go mod download`
# from the two Dockerfiles. One wrapper covers both.
#
# This retries the command, with GO_RETRY_BACKOFF seconds between attempts, up
# to GO_RETRY_ATTEMPTS times. Both are read per call, so a caller can override
# either one.
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
#   go-retry.sh go install <package@version>
#   go-retry.sh go mod download
#   go-retry.sh --selftest
set -uo pipefail

# A verification mismatch, as Go words it. Never retried.
MISMATCH_RX='SECURITY ERROR|checksum mismatch|does not match|verifying.*: invalid'

attempt_once() { # <log> <command...>
  local log="$1"; shift
  "$@" >"$log" 2>&1
}

run() { # <command...>
  # Read both knobs per call, not once at load. Binding them at the top of the
  # file makes a `GO_RETRY_BACKOFF=0 run ...` prefix inert, because the variable
  # is already set by the time the prefix exists — which is what made this
  # script's own selftest sleep 25 seconds while claiming a backoff of zero.
  local attempts="${GO_RETRY_ATTEMPTS:-3}"
  local backoff="${GO_RETRY_BACKOFF:-5}"
  local label="$*" log rc attempt=1
  log="$(mktemp)"
  # shellcheck disable=SC2064
  trap "rm -f '$log'" RETURN

  while :; do
    attempt_once "$log" "$@"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      [ "$attempt" -gt 1 ] && echo "$label: succeeded on attempt $attempt"
      cat "$log"
      return 0
    fi

    if grep -qE "$MISMATCH_RX" "$log"; then
      echo "::error::$label: module verification FAILED — this is not a transient error and is not retried."
      cat "$log" >&2
      return 1
    fi

    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$label: failed $attempts time(s)."
      cat "$log" >&2
      return "$rc"
    fi

    echo "$label: attempt $attempt failed, retrying in ${backoff}s" >&2
    tail -3 "$log" >&2
    sleep "$backoff"
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

  # A fake `attempt_once` stands in for the network, so the selftest is
  # hermetic and fast. The counter file records how many times it ran.
  local counter="$work/attempts"

  # 1. A transient failure that clears is retried and succeeds.
  : >"$counter"
  attempt_once() {
    echo x >>"$counter"
    if [ "$(wc -l <"$counter")" -lt 3 ]; then
      echo "verifying module: reading https://sum.golang.org/tile/8/0/x122/632: stream error: stream ID 629; INTERNAL_ERROR" >"$1"
      return 1
    fi
    echo "ok" >"$1"; return 0
  }
  if GO_RETRY_BACKOFF=0 run go install fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "3" ]; then
    ok "a transient sum.golang.org blip is retried and succeeds"
  else
    bad "a transient blip did not recover (attempts: $(wc -l <"$counter"))"
  fi

  # 2. A persistent failure still fails, after exactly GO_RETRY_ATTEMPTS tries.
  : >"$counter"
  attempt_once() { echo x >>"$counter"; echo "stream error: INTERNAL_ERROR" >"$1"; return 1; }
  if ! GO_RETRY_BACKOFF=0 GO_RETRY_ATTEMPTS=3 run go install fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "3" ]; then
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
    MSG="$msg"
    attempt_once() { echo x >>"$counter"; echo "$MSG" >"$1"; return 1; }
    if ! GO_RETRY_BACKOFF=0 run go install fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "1" ]; then
      ok "a verification mismatch fails immediately: ${msg:0:34}…"
    else
      bad "a verification mismatch was retried (attempts: $(wc -l <"$counter"))"
    fi
  done

  # 4. A first-attempt success runs exactly once.
  : >"$counter"
  attempt_once() { echo x >>"$counter"; echo ok >"$1"; return 0; }
  if GO_RETRY_BACKOFF=0 run go install fake/pkg@v1 >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "1" ]; then
    ok "a clean command runs exactly once"
  else
    bad "a clean command did not run exactly once (attempts: $(wc -l <"$counter"))"
  fi

  # 5. The proxy.golang.org form, from `go mod download`, recovers the same way.
  #    This is the case release PR #80 hit, and the reason this is no longer
  #    `go install`-only.
  : >"$counter"
  attempt_once() {
    echo x >>"$counter"
    if [ "$(wc -l <"$counter")" -lt 2 ]; then
      echo 'go: k8s.io/api@v0.37.0: read "https://proxy.golang.org/k8s.io/api/@v/v0.37.0.zip": stream error: stream ID 873; INTERNAL_ERROR; received from peer' >"$1"
      return 1
    fi
    echo ok >"$1"; return 0
  }
  if GO_RETRY_BACKOFF=0 run go mod download >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "2" ]; then
    ok "a transient proxy.golang.org blip in go mod download is retried"
  else
    bad "a transient proxy.golang.org blip did not recover (attempts: $(wc -l <"$counter"))"
  fi

  # 6. A mismatch during `go mod download` is just as unretryable.
  : >"$counter"
  MSG="SECURITY ERROR: checksum for module does not match"
  attempt_once() { echo x >>"$counter"; echo "$MSG" >"$1"; return 1; }
  if ! GO_RETRY_BACKOFF=0 run go mod download >/dev/null 2>&1 && [ "$(wc -l <"$counter")" = "1" ]; then
    ok "a verification mismatch in go mod download fails immediately"
  else
    bad "a verification mismatch in go mod download was retried (attempts: $(wc -l <"$counter"))"
  fi

  # 7. GO_RETRY_BACKOFF is actually honoured. Without this, both knobs can be
  #    bound once at load and every override silently does nothing: every
  #    assertion above still passes, and the only symptom is this selftest
  #    taking 25 seconds instead of a fraction of one. The margin is wide on
  #    purpose — the default sleeps 5s per retry, so two retries at zero
  #    backoff cannot plausibly reach 3s on any runner.
  : >"$counter"
  attempt_once() {
    echo x >>"$counter"
    if [ "$(wc -l <"$counter")" -lt 3 ]; then
      echo "stream error: INTERNAL_ERROR" >"$1"; return 1
    fi
    echo ok >"$1"; return 0
  }
  local t0=$SECONDS
  GO_RETRY_BACKOFF=0 run go mod download >/dev/null 2>&1
  local elapsed=$((SECONDS - t0))
  if [ "$(wc -l <"$counter")" = "3" ] && [ "$elapsed" -lt 3 ]; then
    ok "GO_RETRY_BACKOFF=0 is honoured (2 retries in ${elapsed}s)"
  else
    bad "GO_RETRY_BACKOFF=0 was ignored (2 retries took ${elapsed}s)"
  fi

  echo
  echo "$pass passed, $fail failed"
  [ "$fail" -eq 0 ]
}

main() {
  case "${1:-}" in
    --selftest) selftest ;;
    "" | -h | --help)
      echo "usage: $0 <command...> | $0 --selftest" >&2; exit 2 ;;
    *) run "$@" ;;
  esac
}

main "$@"
