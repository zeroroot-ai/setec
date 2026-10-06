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
  rpc Suspend(SuspendRequest) returns (SuspendResponse);
  rpc Resume(ResumeRequest) returns (ResumeResponse);
  rpc Fork(ForkRequest) returns (ForkResponse);
  rpc Keep(KeepRequest) returns (KeepResponse);
  rpc Pin(PinRequest) returns (PinResponse);
  rpc Attach(AttachRequest) returns (AttachResponse);
  rpc Exec(stream SandboxServiceExecRequest) returns (stream SandboxServiceExecResponse);
}
```

See `api/grpc/v1/sandbox.proto` for the full message schema.

## Session boot command

`LaunchRequest.command` is required for the ephemeral lifecycle and
optional for `lifecycle.mode = "session"`. A session that omits it
boots the setec keepalive: a static binary the operator installs into
the Pod, which reaps orphans and exits only on teardown. The session
then outlives every command sent through `Exec`. A session that sets a
command boots that command instead, and ends when it exits.

## Resolved class and runtime reporting

A caller that names `LaunchRequest.sandbox_class` selects a class for
its isolation properties, and the responses report what the Sandbox
actually got so the caller can verify rather than trust — without
holding any Kubernetes credentials:

- `LaunchResponse.sandbox_class` — the SandboxClass the created
  Sandbox was bound to, read back from the created object after
  admission (so admission-time defaulting is reflected, not the
  request value).
- `WaitResponse.runtime` — the backend actually selected after
  evaluating the class's primary backend and any `fallback` chain
  (`status.runtime.chosen`): one of `kata-fc`, `kata-qemu`, `gvisor`,
  `runc`. `Wait` returns only after the Sandbox is terminal, so this
  value is authoritative.
- `AttachResponse.sandbox_class` / `AttachResponse.runtime` — the same
  two values for a reattaching caller, which never saw the
  `LaunchResponse`.

**An empty value means "not yet resolved", never "resolved but
unreported".** `LaunchResponse.sandbox_class` is empty only when the
request named no class and cluster-default resolution happens at
schedule time; `AttachResponse.runtime` is empty while the Sandbox is
still Pending; `WaitResponse.runtime` is empty only when the Sandbox
reached a terminal phase before a backend was ever selected (for
example `ClassNotFound` or `RuntimeUnavailable`). A client can
therefore distinguish "the frontend did not report" from "the operator
reported X" and decide for itself how to treat an unresolved value.

## Session reattach (`Attach`)

A **session** Sandbox (`lifecycle.mode: session`, docs/design/lifecycles.md) outlives any
one connection. The `sandbox_id` returned by `Launch`
(`<namespace>/<name>/<uid>`) is the **session handle**: a caller that
disconnected calls `Attach` with the handle and continues with
`StreamLogs`/`Wait` against the same running microVM. Resolution is
stateless — the frontend keeps no session table, the handle resolves
from cluster state alone — so reattach works identically after a
frontend restart or against a different frontend replica. The UID in
the handle pins it to one session: a later Sandbox with the same name
is a different session and is not reachable through the old handle.

Failure shapes, each carrying a typed `AttachFailure` detail in the
gRPC status:

| Condition | Code | `AttachFailure.reason` |
|---|---|---|
| Handle resolves to no live Sandbox (unknown, deleted, or stale UID) | `NOT_FOUND` | `REASON_SESSION_NOT_FOUND` |
| Session over (terminal phase) or teardown in progress | `FAILED_PRECONDITION` | `REASON_SESSION_ENDED` (with the phase) |
| Sandbox is ephemeral | `FAILED_PRECONDITION` | `REASON_NOT_A_SESSION` |

`Attach` also registers caller activity: it stamps the Sandbox's
`setec.zeroroot.ai/last-activity` annotation, and `StreamLogs` on a
session heartbeats the same annotation once a minute while the stream
is open (plus a final stamp at disconnect). The operator's idle
eviction (`SandboxClass.spec.sessionIdleTimeout`) reads that
annotation, so an attached session is never idle-reaped; the idle clock
starts when the last client disconnects.

## Session exec (`Exec`)

`SandboxService.Exec` runs a command **inside** an already-running
session Sandbox and streams its stdio (docs/design/lifecycles.md). It is what makes a
session more than an observable one-shot: successive commands enter the
same live microVM and see each other's effects on the durable
`/workspace` volume.

It is not `LeaseService.Exec`, which launches a fresh throwaway Sandbox
per call and shares nothing between calls.

### Wire protocol

The client sends exactly one `SessionExecStart` as the **first** message,
then zero or more `stdin` chunks, then optionally `stdin_eof`:

```protobuf
message SessionExecStart {
  string sandbox_id = 1;         // the session handle from Launch
  repeated string command = 2;   // argv, executed directly (no shell)
}
```

The server streams `SessionExecOutput` chunks (`stream` is `"stdout"` or
`"stderr"`, never merged) and terminates with **exactly one**
`SessionExecExit`.

`Exec` deliberately offers no per-exec environment or working-directory
override. The container runtime's exec primitive accepts neither, and
synthesising them would mean wrapping `argv` in a shell and silently
changing its meaning. The command inherits the session container's
environment, and its working directory is `/workspace` — session Pods
are built rooted there for exactly this reason. A caller that wants
either should run its own shell explicitly.

### Exit semantics (read this before writing a client)

An exit code cannot be synthesised. A caller handed a bare stream close,
or a zero-valued `int32`, cannot tell a clean success from a microVM
that vanished mid-build. So `SessionExecExit.status` — not the stream
ending — is the discriminator:

| `status` | Meaning | `exit_code` |
|---|---|---|
| `STATUS_EXITED` | The command ran to completion; the runtime reported its wait status. | **authoritative** |
| `STATUS_SANDBOX_GONE` | The session's microVM stopped existing mid-command (evicted, node lost, torn down). Outcome unknown and unknowable. | meaningless (always 0) |
| `STATUS_TRANSPORT_FAILED` | The exec channel broke before a wait status was read. Outcome unknown; the session may still be healthy, so a retry may succeed. | meaningless (always 0) |
| `STATUS_CANCELED` | The caller canceled the RPC or its deadline elapsed; the command was torn down with it. Sent best-effort. | meaningless (always 0) |
| `STATUS_UNSPECIFIED` | Never sent by a Setec frontend. | meaningless |

Two rules follow, and a correct client implements both:

1. **`exit_code` is meaningful only for `STATUS_EXITED`.** Zero anywhere
   else means "no code was ever reported", never "success".
2. **A stream that ends with no `SessionExecExit` at all is an abnormal
   termination.** The command's outcome is unknown. It is never success
   and never a specific code.

### Session state and activity

An in-flight `Exec` registers as session activity for its whole run
(the same `last-activity` annotation `Attach` stamps), so a long build
cannot be idle-evicted underneath the caller.

An `Exec` against a paused or suspended session flips
`spec.desiredState` to `Running` and waits for the microVM to come back
before running the command; the caller sees only the added latency. If
it does not come back inside the frontend's readiness budget the RPC
fails with `FAILED_PRECONDITION` +
`AttachFailure.REASON_SESSION_NOT_RUNNING` — and because no command
ran, no `SessionExecExit` is sent.

### Failure shapes

Handle resolution reuses `Attach`'s typed `AttachFailure` detail, so a
client keeps one switch for both verbs:

| Condition | Code | `AttachFailure.reason` |
|---|---|---|
| Handle resolves to no live Sandbox | `NOT_FOUND` | `REASON_SESSION_NOT_FOUND` |
| Session over or teardown in progress | `FAILED_PRECONDITION` | `REASON_SESSION_ENDED` |
| Sandbox is ephemeral | `FAILED_PRECONDITION` | `REASON_NOT_A_SESSION` |
| microVM did not reach Running in time | `FAILED_PRECONDITION` | `REASON_SESSION_NOT_RUNNING` |

Every one of these is raised **before** the command starts, so no
`SessionExecExit` is sent and nothing ran. Once the stream is
established, every outcome is reported as a `SessionExecExit` instead.

### Isolation

The exec'd process runs in the workload container's namespaces, as the
same unprivileged user, with the same dropped capabilities and seccomp
profile, inside the same microVM. `Exec` adds a second process to an
existing isolation boundary; it does not widen one.

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
unset in file mode. There is no insecure fallback.

### SPIFFE mode

- `--spiffe-socket=unix:///run/spire/agent-sockets/api.sock` — the SPIFFE
  Workload API endpoint. A bare filesystem path is also accepted and
  read as `unix://<path>`. The `SPIFFE_ENDPOINT_SOCKET` environment
  variable is deliberately not consulted.
- The allow-list is the SPIFFE ID of each `--client` entry. **Required**:
  an empty list is a startup error, so "accept everyone" cannot be
  reached by omitting configuration.

The frontend's own X509-SVID and the trust bundle come from the socket,
and both are re-read for every handshake, so a rotated SVID is on the
wire without a restart. A client is accepted only if its chain verifies
against the bundle **and** its SPIFFE ID is on the allow-list. Entries
are full SPIFFE IDs: the trust domain is matched as well as the path, so
the same path under a foreign trust domain is refused.

An enrolled client can be in a different trust domain than the fleet. Its
bundle comes from SPIFFE federation, through the same Workload API. When
the bundle of one client domain is missing, the frontend refuses the
clients of that domain, reports the failure, and keeps serving every
other domain.

Losing the Workload API is reported immediately rather than becoming
visible when the last SVID expires.

From the Helm chart, SPIFFE mode is selected install-wide with
`credentials.mode=spiffe` — the switch covers the frontend, the
node-agent server, and the operator's node-agent dialer together, so a
mixed file/SPIFFE posture is not reachable from a values file. The
chart renders `--spiffe-socket` from `credentials.spiffe.socketPath`
(default `/run/spire/agent-sockets/api.sock`, hostPath-mounted
read-only by directory) and one `--client` per entry in
`frontend.clients`; an empty list fails the render rather than deferring
to the startup error, and a node
without a Workload API socket directory fails Pod creation rather than
booting a frontend that can never fetch an SVID. See the chart README
"Credential modes".

SPIFFE mode is server-side only today. The snapshot dialer and tracing
exporter still use file credentials, and asking for client credentials
from a SPIFFE-configured frontend is an error rather than a silent
downgrade.

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

## Enrolled clients and tenant resolution

Each Gibson cluster that calls the frontend is an enrolled client. The
frontend takes one `--client=<name>=<spiffe-id>` flag for each client.
The chart renders them from `frontend.clients`. A name is a DNS label.
An empty list is a startup error. An entry gives the full ID in
`spiffeID`, or the path only in `spiffePath`. For a path, the chart
builds `spiffe://<trust domain>/<path>` from `global.spire.trustDomain`,
else from `credentials.spiffe.trustDomain`.

The frontend reads the SPIFFE ID from the URI SAN of the verified client
certificate and finds the enrolled name. A caller with no SPIFFE ID, or
with an ID that is not enrolled, gets `PERMISSION_DENIED`. The
certificate never gives the tenant: a certificate names a workload, not
a tenant.

Each request carries the `tenant` field. An empty tenant, or a tenant that
is not a DNS label, gets `INVALID_ARGUMENT`. The pair of the client name
and the tenant has one namespace. Its name is `sbx-` and 20 hex digits
of a hash of the pair, so a pair never gets two. The frontend makes it on
the first call of the pair, with the labels
`setec.zeroroot.ai/client=<name>`, `setec.zeroroot.ai/tenant=<tenant>` and
`setec.zeroroot.ai/sandbox-namespace=true`, and with two RoleBindings: Pod
writes for the operator and exec for the frontend. The operator writes
the default-deny policy of the namespace before its first Pod, and the
host guard binds to the label. A namespace with that name and different
labels gets `PERMISSION_DENIED`. The admission policy `-frontend-scope`
refuses any other namespace or binding that the frontend tries to write.

Every call on an existing Sandbox checks that the namespace in the
sandbox id is the namespace of the pair of the caller. A different
client with the same tenant, or the same client with a different
tenant, gets `PERMISSION_DENIED`. `Launch` and the lease service write
the two labels on each Sandbox that they create.

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
    Tenant:       "acme",
    SandboxClass: "standard",
    Image:        "docker.io/library/python:3.12-slim",
    Command:      []string{"python", "-c", "print('hello')"},
    Resources:    &pb.Resources{Vcpu: 1, Memory: "256Mi"},
  })
  if err != nil {
    log.Fatal(err)
  }
  log.Println("sandbox_id:", resp.SandboxId)

  wait, err := c.Wait(context.Background(), &pb.WaitRequest{SandboxId: resp.SandboxId, Tenant: "acme"})
  if err != nil {
    log.Fatal(err)
  }
  log.Printf("phase=%s exit_code=%d", wait.Phase, wait.ExitCode)
}
```

## Stopping a Sandbox (`Kill`)

`Kill` deletes the Sandbox. Owner-reference GC then removes the Pod and
the NetworkPolicy.

```protobuf
message KillRequest {
  string sandbox_id    = 1;
  int64  grace_seconds = 2;
}
```

- `grace_seconds = 0` (the default) deletes only the Sandbox. The Pod
  goes with it on the Pod's own `terminationGracePeriodSeconds`, which
  is the Kubernetes default of 30 seconds.
- A positive `grace_seconds` deletes the Sandbox Pod with that grace
  period first, then the Sandbox. The kubelet sends `SIGTERM` to the
  workload at once and `SIGKILL` after the window, so a long-lived
  driver gets that long to checkpoint its work. The Pod delete comes
  first because the API server only ever shortens a grace period an
  object already carries: the later ownerless-GC delete cannot cut the
  window short.
- A negative `grace_seconds` is `INVALID_ARGUMENT`.
- A Sandbox that is already gone returns success. `Kill` is idempotent.

## Suspend and resume a session (`Suspend`, `Resume`)

`Suspend` checkpoints a session and releases its microVM. The session
keeps its workspace and its checkpoint, and it uses no compute while it
is suspended. `Resume` starts a new microVM that loads the checkpoint.
The session runs again only after the isolation checks pass.

- Only a session of a class with `sessionCheckpoint` suspends. Any other
  Sandbox gets `FAILED_PRECONDITION`.
- A class with checkpoints also suspends an idle session: by default
  after 10 minutes with no attach, no exec and no client stream
  (`sessionIdleTimeout`). An `Attach` or an `Exec` resumes such a session.
- A session that stays suspended longer than
  `sessionCheckpoint.suspendedTTL` (7 days by default) is recycled: the
  Sandbox goes with its checkpoint, its key and its workspace.
- Both calls are idempotent. Each takes `sandbox_id` and `tenant`, as
  `Kill` does.

## Fork a sandbox (`Fork`)

`Fork` takes a snapshot of a running launcher sandbox and starts `count`
sandboxes from it (1 to 32). Each fork gets a new identity, new
randomness and its own writable layer, so no fork sees a change of
another fork.

- Each fork gets the `network` of the request, or the default of the
  class. It never gets the network of the source.
- Only the owner of the source forks it: the snapshot loads only in the
  namespace of its owner pair.
- The snapshot is deleted `snapshot_ttl_seconds` after the fork (one hour
  by default), once no fork still needs it.
- A session does not fork: its workspace belongs to one sandbox.

## Keep a sandbox for review (`Keep`, `Pin`)

`Keep` takes a snapshot of a running launcher sandbox for a later review.
The snapshot is sealed with the key of the tenant in the S3-compatible
store, so a node that never held it can open it.

- A kept snapshot opens only in a review sandbox: `Launch` with
  `review_snapshot` set to the snapshot name. A review sandbox has no
  network and the machine size of its source. A normal launch from a
  kept snapshot stays Pending with the reason `SnapshotIncompatible`.
- A kept snapshot is deleted after 30 days. `Pin` keeps it past that
  time, and `Pin` with `pinned: false` lets it expire again.
- The pinned snapshots of a tenant count against a storage limit
  (`snapshots.kept.pinnedLimit`, 20 GiB by default). A pin above it is
  `FAILED_PRECONDITION`.
- When the tenant goes, its namespace goes with its kept snapshots and
  its key.

## Verify a sandbox identity (`VerifySandboxIdentity`)

A process in a launcher sandbox gets an identity token from its machine:
`GET /v1/token?audience=<verifier>` on the Unix socket that
`SETEC_IDENTITY_SOCKET` names. It sends the token on its calls. The
verifier sends the token and its audience to `VerifySandboxIdentity`
and gets the `sandbox_id` that Launch and Fork return.

- The check uses the key and the identity generation of the sandbox that
  the token names (`docs/design/isolation.md`, "Sandbox identity"). A
  token of a fork never verifies as its source. A token from before a
  snapshot of the sandbox no longer verifies.
- A token that does not verify is `UNAUTHENTICATED`. Only the owner of
  the sandbox can verify its tokens.
- `token_id` is the `jti` of the token, for a verifier that refuses a
  second use of one token. A token lives 5 minutes.

## Streaming logs

`StreamLogs` opens the kubelet log stream for the Sandbox's workload
container and forwards each line to the gRPC client as a `LogChunk`:

```protobuf
message StreamLogsRequest {
  string sandbox_id = 1;
  bool   follow     = 2;
  int64  tail_lines = 3;
}

message LogChunk {
  bytes  data   = 1;
  string stream = 2;  // "stdout"
}
```

Semantics:

- `follow=false` sends every available log byte and closes the stream
  on EOF.
- `tail_lines`, when positive, starts the stream that many lines from
  the end. A client that lost its stream to a Sandbox running for weeks
  reconnects with it and gets a bounded tail plus every new line,
  instead of the whole history. Zero sends the whole log the kubelet
  still holds. Negative is `INVALID_ARGUMENT`.
- `follow=true` keeps the stream open until the workload container
  exits or the client cancels. When the Pod has not yet reached a
  loggable phase, the server polls for up to 30 seconds before
  returning `FAILED_PRECONDITION`.
- A finished workload still yields its output. A Sandbox that runs to
  completion faster than the caller can attach has nothing left to
  follow, so `follow=true` is served as a completed-log read of the
  terminated container. If the container exits between the status read
  and the attach — the attach is then refused — the server falls back
  to the same completed-log read instead of failing the RPC. A follow
  stream that breaks mid-flight is resumed from the instant of the last
  line the caller received, so partial output is neither lost nor
  duplicated, and the resumed read costs the gap rather than the whole
  run.
- The server reads one line at a time, capped at 1 MiB per line, and
  forwards it at once. Nothing accumulates, so the memory one stream
  costs does not grow with how long the Sandbox has been running. The
  server asks the kubelet for timestamps to anchor a resumed read and
  strips them before the bytes reach the caller.
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

> **Not available yet.** The pre-warm pool is being redesigned ([setec#103](https://github.com/zeroroot-ai/setec/issues/103)).

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

> **Note on the runtime model.** A leased Sandbox is *ephemeral*: the
> microVM runs its immutable `spec.command` then terminates.
> `LeaseService.Exec` therefore launches the caller's command as a fresh
> workload Sandbox in the leased entry's class (snapshot-restored from the
> class snapshot when one is configured, so it inherits the warm base)
> rather than injecting a command into an already-running VM. The
> warm-pool benefit is that image prefetch, scheduling, and (with a
> snapshot) restore are already paid down.
>
> To run successive commands *inside one live microVM* — sharing a durable
> `/workspace` across turns — use `SandboxService.Exec` against a session
> Sandbox instead (see [Session exec](#session-exec-exec) above and docs/design/lifecycles.md). Leases are a
> fast-start mechanism; sessions are a state mechanism. The two verbs
> share a name and nothing else.

## Rate limiting and concurrency

The frontend does not itself rate-limit; it applies whatever limits
Kubernetes enforces via `ResourceQuota` and API server throttling. For
public-facing endpoints, put the frontend behind an ingress that
enforces per-tenant request rate limits.

## Current limitations

- JWT auth is not implemented; mTLS is the only supported authentication
  mechanism.
- SPIFFE mode covers the frontend's server surface only. The node-agent
  and the outbound dialers remain file-based.
