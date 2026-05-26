<!-- SPDX-License-Identifier: Apache-2.0 -->
# Show HN: Setec

Draft for Hacker News. Three title candidates, picked one.

## Title candidates

1. **Show HN: Setec - Firecracker microVMs on demand, via a single Kubernetes CRD** (preferred)
2. Show HN: Setec - microVM isolation as a Kubernetes primitive
3. Show HN: Setec - self-hostable sandboxing for AI code execution, CI, and fuzzing

Candidate 1 leads with the concrete technical artefact (CRD + Firecracker), which tends to perform better than abstract category framing. Candidate 2 is shorter but slightly vaguer. Candidate 3 foregrounds the use cases, which is useful for non-Kubernetes readers but buries the architectural story.

## Body

Setec is a Kubernetes operator that turns `kubectl apply -f sandbox.yaml` into a Firecracker microVM. It is open source (Apache 2.0), cloud-agnostic, and self-hostable on any cluster with KVM-capable nodes.

The core idea: isolation-as-primitive. If you already speak Kubernetes, you should not need a separate control plane, CLI, or SaaS to get hardware-level isolation for an individual workload. We ship three custom resources (`Sandbox`, `SandboxClass`, `Snapshot`), one operator, one node-agent DaemonSet, and optionally one gRPC frontend for programmatic consumers. Everything else is the stack you already have.

What makes the cold-start story workable is the pre-warm pool. The node-agent keeps a configurable number of paused Firecracker VMs ready on each node. When a Sandbox that matches an entry lands, Kubernetes restores from the paused state instead of booting from scratch. On a prepared bare-metal host we see sub-100ms P50 cold starts for pool-claimed sandboxes. If your workload can't use the pool, you pay a normal Firecracker boot, which is still comfortably under a second for small images.

Three reference patterns that ship as working examples:

- `examples/ai-code-exec/` runs LLM-generated Python inside a microVM. The pattern is "agent writes code, program pipes it via stdin to a Sandbox".
- `examples/ci-sandbox/` packages a local project as a tarball, launches a sandbox running `npm test`, streams logs, returns the exit code. The pattern is "run untrusted CI job safely".
- `examples/sec-research/` runs AFL++ against a target binary with `network.mode: none`, a 1-hour lifecycle cap, and constrained resources. The pattern is "run potentially-hostile tool without hurting the host".

Each example is a standalone Go module under 200 lines of client code. They all talk to Setec over its gRPC frontend with mTLS.

Positioning against the neighbours:

- gVisor gives you syscall filtering, which is a different threat model than hardware isolation.
- Kata itself gives you the runtime, but not the Kubernetes-shaped API, not the snapshot lifecycle, not the frontend.
- Managed sandbox products do the same shape of work but are SaaS. Setec runs on your cluster.

Setec is pre-1.0 (`v0.1.0` today). The known limitations are listed in the release notes, including that the pre-warm pool is node-local and the API is `v1alpha1`. We would rather ship honest limitations than hide them.

Repo: https://github.com/zeroroot-ai/setec

Quickstart: https://github.com/zeroroot-ai/setec/blob/main/docs/quickstart.md

A 15-minute narrative walkthrough: https://github.com/zeroroot-ai/setec/blob/main/docs/getting-started.md

The name is a 1990s-movie reference. The goal is not to make that cute; the goal is to make hardware-isolated workloads boring infrastructure.

## Notes for the maintainer

- Post on a weekday morning US-east to maximise visibility.
- If the cold-start numbers in the final smoke test come in higher than 100ms P50, update the "sub-100ms P50" phrase to match what we measured. The HN audience is allergic to aspirational latency claims.
- Be ready to answer: "why not gVisor + pod", "why not Modal/E2B", "what stops cross-VM breakout on a shared host". Honest short answers beat pitch-deck phrasing every time.
