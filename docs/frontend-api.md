# gRPC frontend

The Setec frontend is an optional Deployment that exposes the
`setec.v1.SandboxService` gRPC API. Clients that cannot (or prefer
not to) speak Kubernetes directly use the frontend to launch sandboxes,
wait for completion, and tear them down. Every RPC remains subject to
cluster-side policy: SandboxClass constraints, ResourceQuota, and
NetworkPolicy enforcement all apply identically to CR consumers and
frontend clients.

## Service definition

```protobuf
service SandboxService {
  rpc Launch(LaunchRequest) returns (LaunchResponse);
  rpc StreamLogs(StreamLogsRequest) returns (stream LogChunk);
  rpc Wait(WaitRequest) returns (WaitResponse);
  rpc Kill(KillRequest) returns (KillResponse);
}
```

See `api/grpc/v1/sandbox.proto` for the full message schema.

## Authentication

mTLS is mandatory, TLS 1.3 is the floor, and every client must present a
certificate. What that certificate has to prove depends on the
credential mode.

The frontend runs in exactly one mode. Configuring both or neither is a
startup error naming the cause, and there is no fallback between them:
a SPIFFE frontend that cannot reach its Workload API fails to boot
rather than quietly reverting to files.

### File mode (default)

- `--tls-cert=/etc/setec/tls/tls.crt` and `--tls-key=/etc/setec/tls/tls.key`
  (server cert + key).
- `--tls-client-ca=/etc/setec/tls-ca/ca.crt` (client-cert CA bundle).

All three are required; the process refuses to start if any one is
missing. **A client is accepted if the configured CA issued its
certificate — any client, not a particular one.** Narrowing that is what
SPIFFE mode is for.

The Helm chart refuses to render the frontend Deployment when either
`frontend.tlsCertSecretName` or `frontend.tlsClientCASecretName` is
unset. There is no insecure fallback.

### SPIFFE mode

- `--spiffe-socket=unix:///run/spire/agent-sockets/api.sock` — the SPIFFE
  Workload API endpoint. A bare filesystem path is also accepted and
  read as `unix://<path>`. The `SPIFFE_ENDPOINT_SOCKET` environment
  variable is deliberately not consulted.
- `--spiffe-authorized-id=spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon` —
  repeat once per caller. **Required**: an empty allow-list is a startup
  error, so "accept everyone" cannot be reached by omitting
  configuration.

The frontend's own X509-SVID and the trust bundle come from the socket,
and both are re-read for every handshake, so a rotated SVID is on the
wire without a restart. A client is accepted only if its chain verifies
against the bundle **and** its SPIFFE ID is on the allow-list. Entries
are full SPIFFE IDs: the trust domain is matched as well as the path, so
the same path under a foreign trust domain is refused.

Losing the Workload API is reported immediately rather than becoming
visible when the last SVID expires.

SPIFFE mode is server-side only today. The snapshot dialer and tracing
exporter still use file credentials, and asking for client credentials
from a SPIFFE-configured frontend is an error rather than a silent
downgrade (setec#174).

### Startup log line

The selected mode is stated once at startup, so a pod's logs say which
posture it is running:

```
frontend: credential mode: spiffe
```

### Tenant identity

Independently of the mode, the server derives the *tenant* from the peer
certificate in precedence order: SPIFFE URI SAN, DNS SAN, Subject CN.
That is a different question from authorization — it answers which
tenant a call is for, not whether the caller may make it.

## Tenant resolution

The frontend reads namespaces carrying the configured tenant label
(default `setec.zeroroot.ai/tenant=<tenant>`) and picks the first match as the
tenant's namespace. Every RPC verifies the requested sandbox id's
namespace matches the caller's resolved namespace; cross-tenant access
returns gRPC `PERMISSION_DENIED`.

## Example client

```go
package main

import (
  "context"
  "crypto/tls"
  "crypto/x509"
  "log"
  "os"

  pb "github.com/zeroroot-ai/setec/api/grpc/v1"
  "google.golang.org/grpc"
  "google.golang.org/grpc/credentials"
)

func main() {
  cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
  if err != nil {
    log.Fatal(err)
  }
  caPEM, err := os.ReadFile("ca.crt")
  if err != nil {
    log.Fatal(err)
  }
  pool := x509.NewCertPool()
  pool.AppendCertsFromPEM(caPEM)

  creds := credentials.NewTLS(&tls.Config{
    Certificates: []tls.Certificate{cert},
    RootCAs:      pool,
    MinVersion:   tls.VersionTLS13,
  })
  conn, err := grpc.NewClient("setec-frontend.setec-system.svc:50051",
    grpc.WithTransportCredentials(creds))
  if err != nil {
    log.Fatal(err)
  }
  defer conn.Close()

  c := pb.NewSandboxServiceClient(conn)

  resp, err := c.Launch(context.Background(), &pb.LaunchRequest{
    SandboxClass: "standard",
    Image:        "docker.io/library/python:3.12-slim",
    Command:      []string{"python", "-c", "print('hello')"},
    Resources:    &pb.Resources{Vcpu: 1, Memory: "256Mi"},
  })
  if err != nil {
    log.Fatal(err)
  }
  log.Println("sandbox_id:", resp.SandboxId)

  wait, err := c.Wait(context.Background(), &pb.WaitRequest{SandboxId: resp.SandboxId})
  if err != nil {
    log.Fatal(err)
  }
  log.Printf("phase=%s exit_code=%d", wait.Phase, wait.ExitCode)
}
```

## Streaming logs

`StreamLogs` opens the kubelet log stream for the Sandbox's workload
container and forwards each line to the gRPC client as a `LogChunk`:

```protobuf
message StreamLogsRequest {
  string sandbox_id = 1;
  bool follow = 2;
}

message LogChunk {
  bytes  data   = 1;
  string stream = 2;  // "stdout"
}
```

Semantics:

- `follow=false` sends every available log byte and closes the stream
  on EOF.
- `follow=true` keeps the stream open until the workload container
  exits or the client cancels. When the Pod has not yet reached a
  loggable phase, the server polls for up to 30 seconds before
  returning `FAILED_PRECONDITION`.
- Tenant scope is enforced: a caller whose resolved namespace does not
  match the sandbox's namespace gets `PERMISSION_DENIED`.
- A missing Sandbox returns `NOT_FOUND`; a Sandbox whose Pod has not
  yet been created returns `FAILED_PRECONDITION`.
- Client-side cancel (e.g. closing the gRPC stream) causes a clean
  server shutdown with no error surfaced to the caller.

Example:

```go
stream, err := c.StreamLogs(ctx, &pb.StreamLogsRequest{
    SandboxId: resp.SandboxId,
    Follow:    true,
})
if err != nil {
    log.Fatal(err)
}
for {
    chunk, err := stream.Recv()
    if err == io.EOF {
        return
    }
    if err != nil {
        log.Fatal(err)
    }
    os.Stdout.Write(chunk.Data)
}
```

## Warm-pool lease layer (`setec.v1.LeaseService`)

`SandboxService.Launch` cold-boots a fresh microVM per call. For latency-
sensitive callers the frontend also serves `setec.v1.LeaseService`, a
warm-pool lease layer over the same isolation ABI. It keeps a pool of
pre-warmed Sandboxes per `SandboxClass` (restored from a `Snapshot` when
the class declares one) so a caller can claim one without paying the
cold-boot cost.

```proto
service LeaseService {
  rpc Lease(LeaseRequest) returns (LeaseResponse);
  rpc Exec(ExecRequest) returns (stream ExecResponse);
  rpc Release(ReleaseRequest) returns (ReleaseResponse);
  rpc PoolStatus(PoolStatusRequest) returns (PoolStatusResponse);
}
```

The contract is **Lease → Exec → Release**:

- **Lease** claims a ready (warm) Sandbox for a `SandboxClass`. The pool is
  keyed by class and sized from the class's `spec.preWarmPoolSize`, booting
  the class's `spec.preWarmImage`. When the pool has a ready entry the call
  is fast (`warm=true`); when empty it cold-launches on demand
  (`warm=false`) unless `fail_if_empty` is set, in which case it returns
  `RESOURCE_EXHAUSTED`. A class with no `preWarmImage` is rejected with
  `FAILED_PRECONDITION`.
- **Exec** runs the caller's command in the leased Sandbox and streams its
  output to a terminal `done` message carrying the exit code. Exactly one
  Exec is permitted per lease.
- **Release** destroys the leased Sandbox — **destroy-on-release**: a dirty
  sandbox is never reused — and replenishes the pool back to its warm
  target. Releasing an unknown (but well-formed) lease token is an
  idempotent no-op so cleanup paths are safe to retry.

Lease tokens are tenant-scoped: a token minted for one tenant's namespace
is rejected (`PERMISSION_DENIED`) on any other tenant's RPCs, mirroring
`SandboxService`'s per-call namespace scoping. Pools are maintained per
resolved tenant namespace and never cross tenant boundaries.

`PoolStatus` reports the `ready` / `target` / `leased` counts for a class.
The frontend also exports `setec_lease_pool_ready{namespace,sandbox_class}`
and `setec_lease_pool_leased{namespace,sandbox_class}` gauges.

> **Note on the runtime model.** Setec Sandboxes are one-shot: a microVM
> runs its immutable `spec.command` then terminates, and the v1 ABI exposes
> no in-VM exec channel. `Exec` therefore launches the caller's command as
> a fresh workload Sandbox in the leased entry's class (snapshot-restored
> from the class snapshot when one is configured, so it inherits the warm
> base) rather than injecting a command into an already-running VM. The
> warm-pool benefit is that image prefetch, scheduling, and (with a
> snapshot) restore are already paid down. A future ADR may add an in-VM
> exec channel to run directly inside the leased microVM.

## Rate limiting and concurrency

The frontend does not itself rate-limit; it applies whatever limits
Kubernetes enforces via `ResourceQuota` and API server throttling. For
public-facing endpoints, put the frontend behind an ingress that
enforces per-tenant request rate limits.

## Current limitations

- JWT auth is not implemented; mTLS is the only supported authentication
  mechanism.
- SPIFFE mode covers the frontend's server surface only. The node-agent
  (setec#173) and the outbound dialers (setec#174) remain file-based.
