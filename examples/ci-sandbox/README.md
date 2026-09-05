<!-- SPDX-License-Identifier: Apache-2.0 -->
# Example: CI Sandbox

Run an untrusted test suite inside a Firecracker microVM, managed by Setec.

## What this demonstrates

- Walk a local project directory, pack it into an in-memory gzip tarball, and enforce a size limit.
- Dial the Setec gRPC frontend with mTLS.
- `Launch` a sandbox that shell-executes `tar xz` on the payload and runs a user-supplied test command inside the extracted workspace.
- Stream logs back and exit with the sandbox's exit code.

This is the "run untrusted CI job" pattern: a pull-request-driven build system can shell out to this program to execute the project's test command inside a hardware-isolated microVM instead of on the CI runner itself.

## Prerequisites

- A running Setec cluster with the gRPC frontend enabled.
- Client TLS material (cert, key, and the signing CA).
- Go 1.23 or later to build the client.
- A Node.js project with an `npm test` (or equivalent) command; the example defaults to that.

## Build

```bash
cd examples/ci-sandbox
go build -o ci-sandbox .
```

## Run

```bash
./ci-sandbox \
  --addr=setec-frontend.example.com:8443 \
  --client-cert=./client.crt \
  --client-key=./client.key \
  --ca=./ca.crt \
  --project=./my-node-app \
  --command='npm ci && npm test'
```

The program excludes `.git/` and `node_modules/` from the payload to keep the transfer small; `node_modules/` is rebuilt inside the sandbox by `npm ci`. If your repo has other large directories to skip, extend the walker in `main.go` or change `--max-bytes` to hard-cap the transfer size.

## Applying the equivalent manifest

For GitOps-style usage you can ship the sandbox as a `kubectl apply`-able resource. Mount the project by building a purpose-built image or by pointing at a shared workspace volume (see [`docs/crd-reference.md`](../../docs/crd-reference.md)):

```bash
kubectl apply -f sandbox.yaml
kubectl logs ci-sandbox-sample-vm
```

## Limitations

- The workspace is inlined as a base64 blob in the sandbox command. That is simple but bounds the practical project size; `--max-bytes` defaults to 32MiB.
- Artifacts produced by the test run (coverage reports, build output) are not collected back automatically. Extend `main.go` to pipe specific files out via logs, or use a shared PersistentVolume if your operations team provisions one.
