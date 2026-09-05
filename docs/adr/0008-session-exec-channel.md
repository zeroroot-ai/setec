# 0008 — Session exec rides the CRI exec path, not a bespoke vsock channel

## Status

Accepted (2026-08-15)

## Context

ADR-0006 gave a Sandbox two lifecycles. The **session** lifecycle shipped
(setec#197, setec#203): lifecycle mode, a durable `/workspace` PVC, reattach by
handle (`Attach`), suspend/resume, and idle eviction that spares a session in
active use.

What did not ship is the ability to *do* anything with a session after it
boots. `SandboxService` was exactly `Launch` / `StreamLogs` / `Wait` / `Kill` /
`Attach` — every one of them an **observation** verb. A session VM could be
watched, waited on, and killed; a second command could not be run in it. The
durable worktree survived across turns while nothing could work on it across
turns, which is the whole point of a session (setec#239).

`Attach` does not close this gap and was never meant to. It is a unary
handle-resolution RPC: it proves a handle names a live session, reports the
session's phase/class/runtime, and stamps the last-activity annotation. It
carries no command, no stdio, and no exit code.

The obvious implementation — the one setec#150's original plan named — was to
add a verb to `NodeAgentService` and teach the in-guest `setec-guest-agent` to
spawn processes over vsock, alongside the entropy-reseed and uniquification
protocols it already serves. Two facts make that the wrong build:

1. **`setec-guest-agent` lives in the wrong namespace.** It runs in the microVM
   guest's init namespace, not inside the workload container. A process it
   spawned would not see the workload's rootfs or its `/workspace` mount.
   Making it useful would mean entering the container's namespaces — which is
   precisely what `kata-agent` already does, so the work would be a
   reimplementation of the Kata guest agent living next to the Kata guest agent.
2. **`setec-guest-agent` is optional and kata-only.** It must be baked into a
   microVM rootfs image, node-agent enforcement can be turned off for images
   that lack it, and it does not exist at all under `gvisor` or `runc`. An exec
   channel built on it would work on a subset of the backends setec supports.

Meanwhile an in-VM exec channel already exists on every backend: the CRI exec
path. `kubelet` → CRI runtime → (for `kata-fc`) the Kata shim → `kata-agent`'s
`ExecProcess` inside the workload container's namespaces. It is the same path
`kubectl exec` takes, it is maintained by the runtimes rather than by setec, and
it lands where `/workspace` is visible.

## Decision

**Add `rpc Exec(stream SandboxServiceExecRequest) returns (stream
SandboxServiceExecResponse)` to `SandboxService`, implemented over the
Kubernetes `pods/exec` subresource.**

(The envelope messages carry the service prefix because buf's
`RPC_REQUEST_STANDARD_NAME` demands a collision-safe name: `ExecRequest` is
already `LeaseService.Exec`'s. The awkwardness is a feature — the two verbs
must never be mistaken for each other.)

- The frontend resolves the session handle with the same validation `Attach`
  uses (shared code, shared typed `AttachFailure` detail), then opens a
  `pods/exec` stream against the session Pod's `workload` container.
- **Session Pods are rooted at `/workspace`.** The CRI exec primitive takes no
  working directory, so an exec'd command inherits the container's. Setting
  `WorkingDir` on session containers is what makes "every turn starts where the
  last one left off" true, rather than something the ABI pretends to offer.
- **No per-exec `env` or `cwd` in the ABI.** The CRI exec primitive accepts
  neither. Offering them would mean wrapping the caller's `argv` in a shell and
  silently changing its meaning (quoting, globbing, redirection). A caller that
  wants a shell asks for one.
- **`LeaseService.Exec` is untouched.** Leases are a warm-cold-start mechanism
  for one throwaway command; sessions are a state mechanism. They share a verb
  name and nothing else.
- The frontend's ClusterRole gains `create` on `pods/exec`. This grants no reach
  it did not already have: the exec'd process runs as the same unprivileged
  user, with the same dropped capabilities and seccomp profile, inside the same
  microVM as the workload the frontend already launches.

### Exit reporting is typed, because an exit code cannot be synthesised

This is the part of the decision that matters most to consumers, and the reason
the response is not simply `(bytes, int32)`.

A stream that ends carries no information about *why* it ended. A consumer
handed a bare close, or a zero-valued `int32`, cannot tell a clean success from
a microVM evicted mid-build — and a Devbox reaped mid-build is otherwise
indistinguishable from a build that genuinely failed.

So `SessionExecExit.status` is the discriminator, and `exit_code` is meaningful
on exactly one branch:

| `status` | `exit_code` |
|---|---|
| `STATUS_EXITED` — the runtime reported a wait status | **authoritative** |
| `STATUS_SANDBOX_GONE` — the microVM stopped existing mid-command | meaningless |
| `STATUS_TRANSPORT_FAILED` — the channel broke before a wait status | meaningless |
| `STATUS_CANCELED` — the caller went away | meaningless |
| `STATUS_UNSPECIFIED` — never sent | meaningless |

Two invariants follow:

- **Every established exec ends with exactly one `SessionExecExit`.** When the
  outcome is unknowable, setec says so with a non-`EXITED` status rather than
  closing the stream bare or inventing a code.
- **A stream ending with no `SessionExecExit` is an abnormal termination.** The
  command's outcome is unknown. It is never success, and never a specific code.

Errors raised *before* the command starts (bad handle, ephemeral Sandbox, ended
session, VM not running in time) are RPC-level errors with no exit message at
all — nothing ran, so nothing is owed a verdict.

`STATUS_SANDBOX_GONE` and `STATUS_TRANSPORT_FAILED` are kept apart on purpose:
the first means the session is gone and the work must be restarted elsewhere,
the second means a retry against the same live session may well succeed.

### Session state

An in-flight exec registers as session activity for its whole run, reusing the
`Attach` heartbeat, so a long build is never idle-reaped underneath its caller
(ADR-0006). An exec against a paused or suspended session flips
`spec.desiredState` to `Running` and waits — the controllers own the resume;
the frontend only expresses the intent a client could express itself.

## Consequences

- setec depends on the CRI exec path being available. On `kata-fc` that is
  `kata-agent`; the guest image needs no setec-specific component for exec, and
  `gvisor`/`runc` get the same verb for free.
- The frontend needs a `*rest.Config`, not just a typed clientset: `pods/exec`
  is an HTTP upgrade. WebSocket is negotiated first with SPDY fallback, matching
  `kubectl exec`.
- Session Pods differ from ephemeral ones by one more field (`WorkingDir`).
  Ephemeral Pod specs are unchanged.
- `setec-guest-agent` keeps its narrow, auditable surface: entropy reseed and
  restore uniquification, both fail-closed security mechanisms. It does not
  become a general-purpose command executor, which would have been a far larger
  attack surface inside the guest for no capability gain.
- A session's `spec.command` still governs the VM's lifetime: if it exits, the
  Pod terminates and there is nothing left to exec into. Callers must boot a
  session with a long-lived command. Making setec default that for session-mode
  Sandboxes is deliberately left to a follow-up so it can carry its own
  defaulting/webhook change.

## References

- setec#239 (this ABI), setec#150 (session design), setec#197 / setec#203 (what shipped)
- ADR-0005 (snapshot isolation invariants), ADR-0006 (Sandbox lifecycles)
- `api/grpc/v1/sandbox.proto`, `internal/frontend/exec.go`, `docs/frontend-api.md`
