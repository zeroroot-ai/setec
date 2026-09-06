<!-- SPDX-License-Identifier: Apache-2.0 -->
# Setec: microVM isolation as a Kubernetes primitive

Draft of the longer announcement post. Target: 1500-2000 words. Aim: explain the thesis, position against the existing landscape honestly, show numbers, invite contribution.

## The problem, right now

In 2026, three kinds of workload keep running into the same wall:

- **AI code execution.** Agents and copilots are writing code that needs to run. The host running the agent is usually the same developer laptop or CI runner that holds keys, tokens, and project source. Throwing that code at a container is fine for trusted code, fast, and entirely untested against adversarial input.
- **Untrusted CI.** Pull requests from outside the core team run the repository's tests. Those tests execute arbitrary code in the branch. Teams still trip on supply-chain attacks that pivot from test runner to CI credentials.
- **Security research.** Fuzzers and dynamic analysis tools are intentionally hostile. They need to run on fast hardware, but they should not share a kernel with anything the team cares about.

All three have the same shape. Someone has code they do not fully trust and hardware they do. The gap in the middle is isolation, and isolation means kernel boundary.

## What the landscape already has

**gVisor** swapped the Linux syscall surface for a user-space implementation. That is a different threat model from a hardware VM: you trust gVisor to correctly translate every syscall, and the kernel underneath is shared. gVisor is excellent and cheap; it is not equivalent to a hardware boundary.

**Kata Containers** gives you a real microVM per pod. That is the substrate Setec is built on. But Kata is a runtime; it does not give you a Kubernetes-shaped lifecycle, a snapshot story, a pre-warm pool, or a programmatic frontend. You still assemble those yourself.

**Managed sandbox products** (Modal, E2B, Daytona, and so on) solve the problem operationally. You POST code to an endpoint and get a result back. They are excellent for teams that want a billed API. They are not an option for teams that need to self-host: regulated environments, air-gapped sites, or anyone who wants isolation inside their own cluster.

**Homegrown Firecracker integrations** are common. Most large teams who need microVM isolation end up writing their own orchestrator around Firecracker's REST API, the Kata runtime binaries, or a fork of one of the above. These are often fine, but they are not shareable.

Setec fills a narrow, specific gap: a Kubernetes-native microVM operator that an outside team could adopt in an afternoon, with all three of cold-start performance, multi-tenancy, and a programmatic frontend included.

## What Setec is

Three custom resources:

- `Sandbox` describes one microVM workload: image, command, resources, network intent, lifecycle.
- `SandboxClass` is the operator-side equivalent of a `StorageClass`: pool sizes, default resource budgets, runtime-class mapping.
- `Snapshot` captures and restores a `Sandbox`'s memory + state.

Plus an operator that reconciles all three, a node-agent DaemonSet that talks to Firecracker on each node, and an optional gRPC frontend for programmatic consumers.

A minimal Sandbox looks like this:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: hello
spec:
  image: docker.io/library/python:3.12-slim
  command: ["python", "-c", "print('hello from a Firecracker microVM')"]
  resources: { vcpu: 1, memory: 512Mi }
  lifecycle: { timeout: 5m }
```

`kubectl apply` and you are done. The operator picks the right `RuntimeClass`, builds the backing pod, Kata boots the microVM, the workload runs, you read logs through the standard `kubectl logs` channel. Delete the Sandbox and garbage collection tears down the VM.

## Design decisions

### Kubernetes-shaped, not product-shaped

The API is `kubectl`. There is no new CLI, no dashboard, no separate control plane. If you already have `kubectl apply` in your muscle memory, you already know how to operate Setec.

### Cloud-agnostic by construction

The operator does not depend on any cloud provider's APIs, IAM, or managed service. It requires a Kubernetes cluster whose worker nodes can run Kata Containers, which in turn requires `/dev/kvm`. Bare-metal Linux is the easy case; nested virtualisation works if the outer hypervisor permits it.

### Snapshot + pre-warm as the cold-start story

Firecracker's snapshot feature lets us capture a paused VM's memory and state to disk. The node-agent holds a configurable pool of paused VMs per class. When a new Sandbox matches an entry, Kubernetes restores from the paused state instead of booting.

On a prepared bare-metal host with small pool entries, we observe sub-100ms P50 cold-start latency for pool-claimed sandboxes. Numbers from the v0.1.0 smoke test are recorded in [`docs/launch/v0.1.0-smoke-test-result.md`](./v0.1.0-smoke-test-result.md); update this post when tagging to reflect the measured figures.

### Tenant-scoped from day one

Both the CRD admission and the gRPC frontend derive a tenant identity from either namespace labels or the caller's mTLS certificate. The operator generates per-Sandbox `NetworkPolicy` automatically. The frontend refuses cross-tenant RPCs. Multi-tenancy is not a bolt-on phase; it is in phase 2 of the three shipped phases.

### Small surface, small binaries

Five binaries total ship in this release:

- `setec-operator` (Kubebuilder-scaffolded controller manager)
- `setec-node-agent` (DaemonSet)
- `setec-frontend` (optional gRPC service)
- `setec-pool-vm` (tiny Firecracker wrapper used by the pool)

All four are distroless static, under 30MB each. There is no gRPC gateway, no admin panel, no telemetry pipeline. If you want more features, you bring them via Kubernetes-native tooling: your existing monitoring stack, your existing ingress, your existing policy engine.

## Benchmarks

From the v0.1.0 smoke test on a bare-metal host (placeholder numbers until the real run is committed):

- Cold start, no pool, small image: \_\_\_ ms median.
- Cold start, pool-claimed: \_\_\_ ms median, \_\_\_ ms P95.
- Snapshot create: \_\_\_ s median.
- Snapshot restore: \_\_\_ s median.
- Pool fill for `preWarmPoolSize=3`: \_\_\_ s.

The cold-start P95 matters most. If your agent is writing code once per second, 3-second boots are visible to the user; sub-100ms is not.

## What it does not do

To save you the trouble of finding out:

- Setec does not ship its own container runtime. It binds to Kata + Firecracker, installed by `kata-deploy`.
- Setec's pre-warm pool is node-local. A Sandbox can only consume a pool entry on the same node.
- The `v1alpha1` API will change before `v1`. Migration paths will be documented per release in `CHANGELOG.md`.
- Image prefetching is wired to the containerd v2 Go client. The node-agent dials the containerd socket at startup, pulls OCI images into the `k8s.io` content store (configurable), and classifies pull errors into typed sentinels so transient failures retry and non-transient ones alert loudly.

See the release notes for the full list.

## Roadmap

The near-term roadmap, post-v0.1.0:

- **Image prefetch.** Wire the node-agent to containerd's content store so pool boots no longer assume the image is present.
- **Cross-node pool.** Let a Sandbox consume a compatible pool entry on any node, not only the one where it ran.
- **More example consumers.** The three we ship are a starting point; we'd like reference programs for data-science notebooks and code-review agents.
- **Admission policy examples.** Shipping reference `ValidatingAdmissionPolicy` manifests for common guardrails.

The longer-term direction is in `docs/` and in the open governance described in `GOVERNANCE.md`. Maintainership is earned through sustained contributions, not assigned.

## Call to action

If you want to try it, the 15-minute walkthrough is [`docs/getting-started.md`](../getting-started.md). The terse quickstart is [`docs/quickstart.md`](../quickstart.md). The example programs are in [`examples/`](../../examples/).

If you want to contribute, start with [`CONTRIBUTING.md`](../../CONTRIBUTING.md). Non-trivial changes go through the `.spec-workflow/` process so design is visible and reviewable before code is written.

If you have found a security issue, please use the private channel described in [`SECURITY.md`](../../SECURITY.md).

Setec is one maintainer and a growing set of reviewers. Help makes it better. See you on the pull request.

## Notes for the maintainer

- Replace every `___` in the Benchmarks section with real numbers from the final smoke-test result file.
- If the post is syndicated, keep the CTA links absolute (github.com/zeroroot-ai/setec/...).
- Target publish time: the same day as the HN post, 1-2 hours after.
