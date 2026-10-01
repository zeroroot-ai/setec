#!/usr/bin/env bash
# Fixture for scripts/go-install-retry.sh (setec#98).
#
# Every REQUIRED RED case is the wrapper's reason to exist, and the two
# must-not-retry cases are the reason it is bounded rather than blind:
# a checksum mismatch is a security signal, not a blip.
#
# Run by the `ci-required-selftest` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/go-install-retry.test.sh`.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec bash "${HERE}/go-install-retry.sh" --selftest
