<!-- SPDX-License-Identifier: Apache-2.0 -->
# Example: AI Code Execution

Run LLM-generated Python code inside a Firecracker microVM, managed by Setec.

## What this demonstrates

- Dial the Setec gRPC frontend with mTLS.
- `Launch` a sandbox running `python3 -c <source>` where the source is read from stdin.
- `StreamLogs` while the code runs; relay stdout/stderr to the host.
- `Wait` for a terminal phase; exit with the same code the sandbox returned.

This is the smallest demonstration of the "LLM wrote some code; run it safely" pattern. An agent can pipe untrusted code through this program without exposing the host to arbitrary execution.

## Prerequisites

- A running Setec cluster with the gRPC frontend enabled (`frontend.enabled=true` in Helm values).
- Client TLS material (cert, key, and the CA that signed the frontend's certificate). See [`docs/frontend-api.md`](../../docs/frontend-api.md).
- Go 1.23 or later to build the client.
- A KVM-capable worker node for the microVM.

## Build

```bash
cd examples/ai-code-exec
go build -o ai-code-exec .
```

## Run

Pipe Python source on stdin:

```bash
echo 'print(sum(range(1000000)))' | ./ai-code-exec \
  --addr=setec-frontend.example.com:8443 \
  --client-cert=./client.crt \
  --client-key=./client.key \
  --ca=./ca.crt
```

Expected output:

```
499999500000
ai-code-exec: launched default/sandbox-xxxxx/<uid>
ai-code-exec: phase=Completed exit=0 reason=""
```

The program exits with the sandbox's exit code, so you can use it directly in a shell pipeline.

## Applying the equivalent manifest

If you prefer `kubectl apply`:

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox ai-code-exec-sample -w
kubectl logs ai-code-exec-sample-vm
kubectl delete sandbox ai-code-exec-sample
```

## Where to look next

- [`docs/frontend-api.md`](../../docs/frontend-api.md) for the full gRPC surface.
- [`docs/snapshots.md`](../../docs/snapshots.md) to cut cold-start time when you launch many sandboxes back-to-back.
