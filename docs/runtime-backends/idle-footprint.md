# Idle footprint of a long-lived Sandbox

A Sandbox that holds an always-on agent stays up for weeks and spends
nearly all of that time waiting. What must hold steady is the memory
the Sandbox costs the node while nothing happens. A footprint that
climbs while the workload is idle is a leak, and it ends the member
when the node runs out of memory or the limit evicts the Pod.

On the `gvisor` backend the number to watch is the whole Pod, not the
workload's own RSS. Every gVisor Sandbox carries a Sentry process, the
user-space kernel that serves the workload's syscalls. Sentry memory is
charged to the Pod, so a Sentry that grows shows up in the Pod's
metrics and never in the workload's `/proc/self/status`.

## Method

`hack/measure-idle-footprint.sh` samples a running Sandbox and reports
the drift.

```bash
# 12 samples, 5 minutes apart, 10 percent growth allowed.
hack/measure-idle-footprint.sh sandbox-workloads my-member

# The 24 hour soak: 288 samples, 5 minutes apart.
hack/measure-idle-footprint.sh sandbox-workloads my-member 288 300 10
```

The script only reads. It samples an existing Sandbox and creates,
patches and deletes nothing, so it is safe to point at a cluster it
does not own.

It needs `kubectl` and a metrics-server serving `metrics.k8s.io`. On a
kind cluster, install metrics-server with `--kubelet-insecure-tls`.

## What the numbers mean

Each sample sums the memory of every container in the Sandbox Pod.
The verdict compares the last sample against the lowest one, because a
Sandbox settles for a moment after it starts and that floor is the
honest baseline.

- **PASS** — the last sample is within the growth budget of the floor.
  The Sandbox holds its footprint. This is the expected result for a
  process that sleeps.
- **FAIL** — the footprint grew past the budget. Record the series and
  the backend. A steady climb across the whole window is a leak; a
  single step that then holds flat is usually the workload allocating
  once, and the run should be repeated with the workload idle from the
  first sample.

## Choosing the workload

Measure a process that does nothing, so any growth belongs to the
runtime rather than the workload:

```yaml
image: docker.io/library/busybox:1.37
command: ["sh", "-c", "while :; do sleep 3600; done"]
```

A real member is a fair second measurement, but it is not the one that
answers the question about the runtime.

## Where this runs

The soak is too long for the merge gate. It belongs to the scheduled
exit test that keeps members up (`gibson#1718`), which already runs a
bank of always-on members on a real model. The script's own arithmetic
is checked offline on every PR:

```bash
hack/measure-idle-footprint.sh --self-test
```

The self-test feeds the drift check a steady series and a leaking one,
so a change that makes the check unable to fail is caught in CI.
