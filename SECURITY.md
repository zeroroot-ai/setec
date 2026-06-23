<!-- SPDX-License-Identifier: Apache-2.0 -->
# Security Policy

Setec runs untrusted workloads inside microVMs. A report in this project can affect downstream users who rely on that isolation, so we take disclosure seriously and try to make it easy to reach us privately.

## Supported Versions

Setec is pre-1.0. Only the most recent minor release line receives security fixes.

| Version  | Supported |
|----------|-----------|
| v0.1.x   | Yes       |
| <= 0.0.x | No        |

As the project matures, this policy will be updated to cover multiple minor versions with a documented maintenance window. Until then, please track the latest v0.1.x release.

## Reporting a Vulnerability

Please do **not** open a public GitHub issue or pull request for security problems. Use one of the two private channels below:

1. **GitHub private vulnerability reporting** (preferred): open the repository's `Security` tab and select `Report a vulnerability`. This creates a draft advisory that only the maintainers and invited collaborators can see.
2. **Email:** `security@setec.zeroroot.ai`. If you need to encrypt the message, request a PGP key in the first email and we will reply with a current fingerprint.

Useful details to include:

- A description of the issue and why you consider it a security problem.
- Affected versions (Setec, Kata Containers, Firecracker, Kubernetes).
- A minimal reproducer or proof-of-concept if you have one.
- Any relevant logs, manifests, or traces.
- Your preferred name and contact for credit in the advisory.

## Response Timeline

- **Acknowledgement:** within 72 hours of receipt, we will confirm we received the report and assign a maintainer to triage it.
- **Initial assessment:** within 10 business days we aim to share our reading of severity and a rough remediation plan.
- **Fix target:** for issues classified as Critical or High we target a patched release within 30 days of the initial assessment. Medium and Low issues are batched into the next regular release.
- **Public disclosure:** we coordinate timing with the reporter. The default window is 90 days from the original report, or sooner if a fix ships and is broadly available. We will request a short extension only when active exploitation or a deeply invasive fix makes it necessary, and we will explain why.

## Snapshot & sandbox security invariants

Setec warms pools by restoring microVMs from shared Snapshots. A Snapshot is
restored across every warm-pool claim of a SandboxClass, which creates three
distinct risks. The invariants below are enforced in code (ADR-0052).

### No secrets in a Snapshot

A Snapshot is shared across every warm-pool claim, so any secret baked into
snapshot state would leak to every future tenant that restores it. The rule
is therefore: **secrets are injected per-lease POST-restore over the control
plane, never present at snapshot time.**

- Pre-warm pool entries (the VMs that get snapshotted) are booted purely from
  kernel/rootfs/image. No environment variables, credentials, or secret
  material enter the pool launch path (`internal/nodeagent/pool`). A
  regression test (`TestLaunchOptions_CarriesNoSecretMaterial`) fails the
  build if a secret-shaped field is ever added to the launch options.
- Per-Sandbox secrets live only on the per-lease Pod's `env`, applied after a
  pool entry is claimed — never on the snapshotted VM.
- A CI **scan-gate** (`no-secrets-in-snapshot` workflow, backed by
  `internal/snapshot/secretscan` and the `setec-snapshot-scan` CLI) fails the
  build if a snapshot artifact contains secret-shaped material (PEM private
  keys, provider key prefixes, JWTs, secret-shaped env assignments). The gate
  self-tests that it rejects a known-leaky fixture, so it cannot silently
  pass.

### Default-deny egress per SandboxClass

Network egress is default-deny, opened only per SandboxClass policy. A
SandboxClass declares `spec.defaultNetworkMode` (`none` or
`egress-allow-list`); a Sandbox in that class that does not declare its own
`spec.network` inherits the closed posture rather than unrestricted egress
(`internal/netpol.GenerateForClass`). An optional class-level
`spec.defaultEgressAllow` opens a small, audited destination set for the whole
class while keeping everything else denied. A Sandbox that explicitly declares
its own network is constrained to the class's `allowedNetworkModes` by the
admission webhook.

The control plane between the operator and the node-agent is vsock-only /
mTLS-only and is never reachable from the sandboxed workload's egress path.

### Entropy reseed on restore

A microVM restored from a snapshot must re-seed its CSPRNG so two VMs restored
from the same snapshot do not share RNG state (catastrophic for keys and
nonces).

Every microVM is launched with a **virtio-rng (entropy) device** attached
before `InstanceStart` (`cmd/setec-pool-vm` `configureAndBoot`; regression test
`TestConfigureAndBoot_AttachesEntropyBeforeStart`). The device is part of the VM
configuration, so it is captured in the snapshot and re-established on restore.
A restored guest therefore has a continuous **host-backed** entropy source: the
Linux `virtio-rng` driver feeds fresh host entropy into the kernel via
`add_hwgenerator_randomness`, which reseeds the CRNG after resume rather than
leaving it frozen at the snapshot's captured pool state. This breaks the
shared-RNG-across-clones property without any in-guest agent.

A complementary **active** reseed — pushing fresh entropy into the guest over
the vsock control plane at the *instant* of restore (closing the brief window
before the guest next pulls from virtio-rng) — requires an in-guest agent that
the current setec runtime does not yet expose; it is a scoped follow-up gated
on the runtime agent (open-core E5). Until it lands, workloads that mint
keys/nonces immediately on resume should also seed from the per-lease secret
injected post-restore. Setec does not ship a stub that falsely claims to
actively reseed.

## Scope

In scope for coordinated disclosure:

- The operator (`setec-operator` / `bin/manager`) and its admission webhook.
- The node-agent and the `setec-pool-vm` launcher.
- The gRPC frontend.
- The Helm chart shipped from this repository.
- The generated Custom Resource Definitions.

Out of scope here (report to the upstream project):

- Vulnerabilities in Kata Containers, Firecracker, containerd, runc, the Linux kernel, or Kubernetes themselves. Please report these to the corresponding maintainers; we are happy to help you find the right contact.
- Issues that require pre-existing cluster-admin or node-root access to exploit. These are hardening suggestions rather than vulnerabilities and are best filed as normal issues or pull requests.
- Denial of service that only affects a workload the reporter launched against their own sandbox.

If you are unsure whether something is in scope, err on the side of a private report and we will redirect you if needed.

## Safe Harbor

We will not pursue or support legal action against good-faith security research that:

- Respects user privacy, data, and availability.
- Gives us a reasonable window to investigate and fix before public disclosure.
- Does not exfiltrate data beyond what is necessary to demonstrate the issue.
- Does not target systems that do not belong to the reporter.

## Acknowledgements

Unless the reporter opts out, we credit the finder of each fixed vulnerability in the release notes and in any published advisory. If you want to remain anonymous, say so in the initial report and we will honour it.
