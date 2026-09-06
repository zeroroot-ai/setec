<!-- SPDX-License-Identifier: Apache-2.0 -->
# Example: Security Research

Fuzz a potentially-hostile target binary inside a Firecracker microVM, managed by Setec.

## What this demonstrates

- Pack a local target binary (and optional seed corpus) into an in-memory gzip tarball.
- Dial the Setec gRPC frontend with mTLS.
- `Launch` a sandbox with constrained resources (2 vCPU, 2GiB memory by default) and a hard 1-hour lifecycle timeout.
- The sandbox runs AFL++ against the target, bounded by an internal `timeout` so the fuzzer exits cleanly before the lifecycle kills it.
- After the fuzzer returns, the harness dumps any crash artefacts to stdout, base64-encoded, so a client script can pick them up from the log stream.

This is the "run a CPU-hungry, potentially-misbehaving tool without hurting anything" pattern.

## Prerequisites

- A running Setec cluster with the gRPC frontend enabled.
- Client TLS material (cert, key, CA).
- Go 1.23 or later to build the client.
- **Hardware:** AFL++ is CPU-intensive. Plan for the fuzzer to saturate the vCPUs assigned to the sandbox for the full run. A laptop-class node can handle one sandbox; a bare-metal server can handle several concurrently.
- An AFL-instrumented target binary. Building one is out of scope here; the upstream [AFL++ docs](https://aflplus.plus/) cover instrumentation.

## Build

```bash
cd examples/sec-research
go build -o sec-research .
```

## Run

```bash
./sec-research \
  --addr=setec-frontend.example.com:8443 \
  --client-cert=./client.crt \
  --client-key=./client.key \
  --ca=./ca.crt \
  --target=./my_target \
  --seed-dir=./seeds \
  --timeout=1h
```

The program launches the sandbox, streams AFL++ output to stdout, and dumps any crash inputs (base64-encoded) at the end of the run. Parse the log tail to extract crashes programmatically.

## Applying the equivalent manifest

The `sandbox.yaml` in this directory shows the in-cluster shape; adapt it if you keep fuzz corpora in a PersistentVolume or deliver the target via an init-container. Note that the manifest deliberately sets `network.mode: none`, which is the right default for fuzz runs.

## Safety notes

- `network.mode: none` prevents the sandbox from reaching the pod network. Remove only if your fuzz harness explicitly needs upstream access.
- The 1-hour lifecycle timeout is a hard ceiling. If AFL++ is still finding new paths at that point, re-run with a longer budget rather than trying to subvert the cap.
- Crashes are reported in the log stream as a courtesy; for serious campaigns, mount a PersistentVolume into the sandbox to persist the full `findings/` tree.
