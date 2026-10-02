#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Zero Root AI
#
# Print the sandbox-runtime pins as KEY=VALUE lines, one per line, for every
# consumer that builds Dockerfile.installer.
#
# The payload stage refuses an empty pin, so every caller must pass all of
# them. Before this script each caller named the pin files itself, and adding
# gvisor.env (setec#89) left three of the four behind: the builds failed with
# "GVISOR_VERSION: parameter not set", which reads as a broken guard rather
# than a missing argument. Adding the next runtime means editing this list
# once.
set -euo pipefail

cd "$(dirname "$0")/.."

pin_files=(kata.env gvisor.env)

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
