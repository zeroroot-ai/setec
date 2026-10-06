<!-- SPDX-License-Identifier: Apache-2.0 -->
# Lifecycles

A Sandbox follows one of two lifecycles. `spec.lifecycle.mode` selects it, and it cannot change after creation (`api/v1alpha1/sandbox_types.go`, `internal/webhook/sandbox_webhook.go`).

## Ephemeral

The default. The Sandbox runs one command to completion and keeps no state. `spec.lifecycle.timeout` bounds its run time: past it, the operator stops the Pod and marks the Sandbox `Failed` with reason `Timeout` (`internal/controller/sandbox_controller.go`).

## Session

A session lives across many calls.

- It has a workspace volume at `/workspace` that outlives its Pod ([storage](storage.md)).
- A session that names no command runs the entry point of its image, and work arrives through `Exec` (`internal/podspec/launcher.go`).
- A client attaches again with the sandbox id (`Attach`). `internal/frontend/attach.go`.
- `Exec` runs a command in the running session and returns a typed exit. `internal/frontend/exec.go`.
- `Attach` and an open log stream record activity on the Sandbox (`setec.zeroroot.ai/last-activity`), so a session in use is never idle-evicted. `internal/frontend/attach.go`, `internal/frontend/service.go`.

### Idle eviction, pause and suspend

The class sets the policy (`api/v1alpha1/sandboxclass_types.go`):

- `sessionIdleTimeout`: a session with no activity for this long is evicted (`Failed`, reason `IdleTimeout`).
- `maxPauseDuration`: a paused Sandbox past this cap is stopped (`Failed`, reason `PauseTimeoutExceeded`).
- `sessionCheckpoint`: with it, a session takes periodic memory checkpoints, takes one when its node drains, and suspends instead of being evicted. A suspended session holds no microVM and resumes on the next attach, on any node. See [session checkpoints](../session-checkpoints.md).

## Limits

- vCPU is 1 to 32, and memory is at most 64 GiB, in the CRD schema (`api/v1alpha1/sandbox_types.go`).
- A class lowers the ceilings with `maxResources`, and `internal/class/validator.go` refuses a Sandbox above them.
- A `ResourceQuota` of the tenant namespace is the second control ([multi-tenancy](../multitenancy.md)).

## The warm pool and leases

The lease service keeps a pool of started Sandboxes for each tenant namespace and class (`internal/leasepool/pool.go`). `Lease` claims one, `Exec` runs one command in a new Sandbox of the same class, and `Release` destroys the leased Sandbox and fills the pool again. A used Sandbox is never handed to a second caller (`internal/frontend/leaseservice.go`).
