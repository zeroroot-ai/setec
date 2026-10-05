#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Zero Root AI
#
# Print the sandbox-runtime pins as KEY=VALUE lines, one per line, for every
# consumer that builds Dockerfile.installer.
#
# The payload stage refuses an empty pin, so every caller must pass all of
# them. Each caller reads this script, not the pin files, so a change to the
# list is one edit. The installer image carries kata only: gVisor left it by
# owner decision D70, and gvisor.env now pins the gVisor that the e2e workflow
# lays on its test node.
set -euo pipefail

cd "$(dirname "$0")/.."

pin_files=(kata.env)

for f in "${pin_files[@]}"; do
  if [ ! -f "$f" ]; then
    echo "pin file $f is missing" >&2
    exit 1
  fi
done

# Strip comments and blank lines. Every remaining line is a build arg.
args="$(grep -vhE '^[[:space:]]*(#|$)' "${pin_files[@]}")"

if [ -z "$args" ]; then
  echo "no pins found in ${pin_files[*]}" >&2
  exit 1
fi

printf '%s\n' "$args"
