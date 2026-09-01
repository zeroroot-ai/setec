#!/usr/bin/env bash
# Copyright 2026 The Setec Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Measures the memory footprint of a Sandbox that is up and idle, and
# fails when the footprint grows.
#
# A Sandbox that holds an always-on agent stays up for weeks. Its
# process spends nearly all of that time waiting. A gVisor Sandbox
# carries a Sentry process per Sandbox, so what has to hold steady is
# the whole Pod's memory, not the workload's own RSS: a Sentry that
# grows while nothing happens is the leak that ends a long-lived
# member.
#
# The script only READS. It samples an existing Sandbox and never
# creates, patches or deletes anything, so it is safe to point at a
# cluster it does not own.
#
# Usage:
#   hack/measure-idle-footprint.sh <namespace> <sandbox> [samples] [interval-seconds] [max-growth-percent]
#   hack/measure-idle-footprint.sh --self-test
#
# Defaults: 12 samples, 300 s apart (one hour), 10 percent growth allowed.
# A soak long enough to answer the 24 h question is 288 samples at 300 s.
#
# Requires: kubectl, and metrics-server serving metrics.k8s.io in the
# cluster (kind: `kubectl apply -f components.yaml` from
# kubernetes-sigs/metrics-server, with --kubelet-insecure-tls).

set -euo pipefail

# to_kib normalizes a Kubernetes quantity to whole KiB. It accepts the
# suffixes the metrics API emits (Ki, Mi, Gi) and a plain byte count.
to_kib() {
  local q="$1"
  case "$q" in
    *Ki) echo "${q%Ki}" ;;
    *Mi) echo $(( ${q%Mi} * 1024 )) ;;
    *Gi) echo $(( ${q%Gi} * 1024 * 1024 )) ;;
    *[!0-9]*) echo "measure-idle-footprint: unsupported quantity ${q}" >&2; return 1 ;;
    *) echo $(( q / 1024 )) ;;
  esac
}

# assess_drift decides whether a series of KiB samples is steady. It
# compares the last sample against the smallest one, because a Sandbox
# settles for a moment after it starts and the floor is the honest
# baseline. Growth beyond max_growth_percent fails.
assess_drift() {
  local max_growth_percent="$1"
  shift
  local -a samples=("$@")

  if [ "${#samples[@]}" -lt 2 ]; then
    echo "measure-idle-footprint: need at least 2 samples, got ${#samples[@]}" >&2
    return 2
  fi

  local floor="${samples[0]}" s
  for s in "${samples[@]}"; do
    [ "$s" -lt "$floor" ] && floor="$s"
  done
  local last="${samples[${#samples[@]}-1]}"

  if [ "$floor" -le 0 ]; then
    echo "measure-idle-footprint: floor sample is ${floor} KiB; the metrics source reported nothing usable" >&2
    return 2
  fi

  local growth=$(( (last - floor) * 100 / floor ))
  echo "floor=${floor}Ki last=${last}Ki growth=${growth}% allowed=${max_growth_percent}%"
  if [ "$growth" -gt "$max_growth_percent" ]; then
    echo "FAIL: the idle footprint grew by ${growth}%, past the ${max_growth_percent}% budget"
    return 1
  fi
  echo "PASS: the idle footprint held steady"
  return 0
}

# sample_pod_kib reads one memory sample for a Pod from metrics.k8s.io,
# summed over its containers.
sample_pod_kib() {
  local ns="$1" pod="$2" raw total=0 c
  raw="$(kubectl get --raw "/apis/metrics.k8s.io/v1beta1/namespaces/${ns}/pods/${pod}")"
  for c in $(echo "$raw" | grep -o '"memory":"[^"]*"' | cut -d'"' -f4); do
    total=$(( total + $(to_kib "$c") ))
  done
  echo "$total"
}

self_test() {
  local failures=0

  # to_kib understands every suffix the metrics API emits.
  [ "$(to_kib 4096Ki)" = "4096" ] || { echo "to_kib Ki wrong"; failures=1; }
  [ "$(to_kib 2Mi)" = "2048" ] || { echo "to_kib Mi wrong"; failures=1; }
  [ "$(to_kib 1Gi)" = "1048576" ] || { echo "to_kib Gi wrong"; failures=1; }
  [ "$(to_kib 2048)" = "2" ] || { echo "to_kib bytes wrong"; failures=1; }
  if to_kib "12Ti" >/dev/null 2>&1; then
    echo "to_kib accepted an unsupported suffix"; failures=1
  fi

  # A steady series passes. Small jitter either way is not growth.
  if ! assess_drift 10 40960 41200 40800 41100 41000 >/dev/null; then
    echo "assess_drift failed a steady series"; failures=1
  fi

  # The failing fixture: a series that climbs past the budget must
  # fail. Without this the guard could not fail at all.
  if assess_drift 10 40960 44000 48000 52000 >/dev/null; then
    echo "assess_drift passed a leaking series"; failures=1
  fi

  # A series exactly at the budget is inside it.
  if ! assess_drift 10 1000 1100 >/dev/null; then
    echo "assess_drift failed a series exactly at the budget"; failures=1
  fi

  # Too few samples is an error, not a pass.
  if assess_drift 10 1000 >/dev/null 2>&1; then
    echo "assess_drift passed a single sample"; failures=1
  fi

  if [ "$failures" -ne 0 ]; then
    echo "measure-idle-footprint: SELF-TEST FAILED"
    return 1
  fi
  echo "measure-idle-footprint: self-test passed"
}

main() {
  if [ "${1:-}" = "--self-test" ]; then
    self_test
    return
  fi

  if [ "$#" -lt 2 ]; then
    grep '^#   hack/measure-idle-footprint.sh' "$0" >&2
    exit 2
  fi

  local ns="$1" sandbox="$2"
  local sample_count="${3:-12}" interval="${4:-300}" max_growth="${5:-10}"
  local pod="${sandbox}-vm"

  echo "sandbox=${ns}/${sandbox} pod=${pod} samples=${sample_count} interval=${interval}s"
  local -a series=()
  local i kib
  for (( i = 1; i <= sample_count; i++ )); do
    kib="$(sample_pod_kib "$ns" "$pod")"
    series+=("$kib")
    printf '%s sample %d/%d %sKi\n' "$(date -u +%FT%TZ)" "$i" "$sample_count" "$kib"
    if [ "$i" -lt "$sample_count" ]; then
      sleep "$interval"
    fi
  done

  assess_drift "$max_growth" "${series[@]}"
}

main "$@"
