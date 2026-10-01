#!/usr/bin/env bash
# Fixture for scripts/go-retry.sh (setec#98).
#
# Every REQUIRED RED case is the wrapper's reason to exist, and the
# must-not-retry cases are the reason it is bounded rather than blind:
# a checksum mismatch is a security signal, not a blip.
#
# Both transport forms are covered, because both have cost a red job: the
# sum.golang.org form on PR #95 building bin/buf, and the proxy.golang.org
# form in `go mod download` on release PR #80.
#
# Run by the `ci-required-selftest` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/go-retry.test.sh`.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec bash "${HERE}/go-retry.sh" --selftest
