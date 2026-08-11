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

A complementary **active** reseed is **enforced fail-closed on the restore
path** (setec#72). Every pool microVM is launched with a vsock device attached
before `InstanceStart` (`cmd/setec-pool-vm`; regression test
`TestConfigureAndBoot_AttachesVsockBeforeStart`), and the in-guest
**`setec-guest-agent`** listens on an AF_VSOCK port inside the microVM. After
every `LoadSnapshot`, the node-agent connects through the device's host-side
Unix socket, pushes fresh `crypto/rand` entropy, and requires the guest agent
to credit it into the kernel CRNG (`RNDADDENTROPY`) and acknowledge with the
payload's SHA-256. The restore RPC only reports success once that
digest-verified ack arrives; on any failure the restored VM is paused and the
restore fails, so a sandbox is never handed over with unconfirmed CSPRNG state
(regression test `TestRestoreSandbox_ReseedFailureFailsClosed`). Reseed
outcomes are observable via the `setec_node_entropy_reseed_total{outcome}`
metric and an `EntropyReseeded` Event on the Sandbox.

Residual risk and scope limits, stated precisely:

- Enforcement requires the guest image to **bundle `setec-guest-agent`**
  (published as `ghcr.io/zeroroot-ai/setec-guest-agent`; `make
  build-guest-agent`). Setec does not build guest rootfs images, so an
  operator whose images lack the agent must either add it or explicitly opt
  out with `snapshots.entropyReseed: off` (`--entropy-reseed=off`). The
  opt-out downgrades restored clones to the passive virtio-rng mechanism
  only — until virtio-rng's next reseed, workloads minting keys/nonces
  immediately on resume may share RNG state across clones. The opt-out is a
  deliberate, auditable flag; there is no silent fallback, and setec still
  ships no stub that falsely claims to reseed.
- The reseed covers the node-agent `RestoreSandbox` path (the only
  snapshot-load path in the runtime). Pause/resume of the *same* VM does not
  clone CSPRNG state and needs no reseed.

### What mTLS proves, per credential mode

Every setec control-plane hop is mTLS. mTLS on its own is an
authentication control, not an authorization one, and the two modes
prove different things. Reading "mTLS" and inferring "only the intended
caller can reach this" is wrong in one of them.

#### File mode — the default

Selected by the `--tls-*` flags: `--tls-cert` / `--tls-key` /
`--tls-client-ca` on the frontend and the node-agent, and
`--nodeagent-tls-cert` / `--nodeagent-tls-key` / `--nodeagent-ca` on the
operator's client to the node-agent. This is the default, and it was
every setec hop's only mode before SPIFFE mode landed.

- Proves the peer holds a certificate issued by the configured CA, and
  that the connection is TLS 1.3 with a client certificate present.
- **It authenticates CA issuance. It does not authorize which peer is
  calling.** Any holder of any certificate from that CA is accepted. If
  the CA issues to more workloads than the ones meant to call setec, all
  of them can call setec. Narrowing that is what SPIFFE mode is for; in
  file mode it has to be done outside setec, by keeping the CA's
  issuance scope as narrow as the caller set.
- The credential is a file. Anything that can read the mounted Secret is
  the workload. Rotation is the delivery pipeline's job, and a pipeline
  that stops delivering shows up as an expiry.

#### SPIFFE mode

Selected by `--spiffe-socket` plus at least one `--spiffe-authorized-id`
on the frontend and the node-agent, and by `--nodeagent-spiffe-socket`
plus `--nodeagent-spiffe-authorized-id` on the operator's client to the
node-agent. It proves everything file mode proves, and then:

- **It authorizes the peer.** The peer's SPIFFE ID must appear on an
  explicit allow-list of full SPIFFE IDs. A certificate validly signed
  by the trusted authority but carrying an unlisted identity is refused.
  This is the difference between authentication and authorization, and
  it is the whole point of the mode.
- **The trust domain is matched, not just the path.** Allow-list entries
  are full IDs such as
  `spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon`, so an identical
  path under a foreign trust domain is a different principal and is
  refused.
- **The identity is attested, not possessed.** It is issued to this
  workload by the local SPIRE agent rather than read from a file
  anything in the container could read, and it rotates in-process, so a
  handshake uses the SVID as it stands at that moment. A watch failure
  is reported when it happens rather than surfacing later as the last
  SVID expiring.
- **An empty allow-list is a startup error.** There is no
  accept-everyone setting, so "authorize everyone" cannot be reached by
  omitting configuration.

#### The Workload API socket, and what happens without it

`--spiffe-socket` (and its `--nodeagent-` and `--otel-` siblings) takes
either a filesystem path to the SPIRE agent's socket or a full endpoint
address. The conventional path, and the one setec's flag help quotes, is:

```
unix:///run/spire/agent-sockets/api.sock
```

Mount the SPIRE agent's socket directory into the setec container at
that path — usually as a `hostPath` or via the SPIFFE CSI driver — and
pass it as the flag value. setec deliberately does **not** consult the
`SPIFFE_ENDPOINT_SOCKET` environment variable: which socket a component
talks to is part of its configuration, not of its ambient environment.

If the socket is absent or unreachable, the component **fails to
start**. The first fetch is bounded (30 seconds when the caller's
context carries no deadline of its own) rather than retried behind an
indefinite backoff, so a missing SPIRE agent is a visible boot failure
and not a process that hangs.

**There is no fallback from SPIFFE mode to file mode.** This is
deliberate: a silent downgrade to a weaker credential source, at the
moment the stronger one breaks, is precisely the failure this design
exists to prevent. Configuring both modes on one component is a startup
error too, and so is configuring neither, so an operator never has to
work out which one won.

#### Which hops are covered

An operator should be able to state their posture from this table
without reading source. The default on every row is the non-SPIFFE
option — file mTLS on the three control-plane hops, one-way TLS on the
tracing hop. The Helm chart selects those defaults and does not yet
expose the SPIFFE flags, so SPIFFE mode is reached today by setting the
flags directly.

| Hop | Credential modes | Flags |
|---|---|---|
| caller → frontend server (`cmd/frontend`) | file mTLS or SPIFFE mTLS | `--tls-cert` / `--tls-key` / `--tls-client-ca`, or `--spiffe-socket` / `--spiffe-authorized-id` |
| operator → node-agent server (`cmd/node-agent`) | file mTLS or SPIFFE mTLS | `--tls-cert` / `--tls-key` / `--tls-client-ca`, or `--spiffe-socket` / `--spiffe-authorized-id` |
| operator → node-agent client (snapshots) | file mTLS or SPIFFE mTLS | `--nodeagent-tls-cert` / `--nodeagent-tls-key` / `--nodeagent-ca`, or `--nodeagent-spiffe-socket` / `--nodeagent-spiffe-authorized-id` |
| operator → OTLP collector (tracing) | **one-way TLS by default**, SPIFFE mTLS on request | `--otel-ca-file`, or `--otel-spiffe-socket` / `--otel-spiffe-server-id` |

Hops that are **not** setec mTLS surfaces, stated so their absence is
not read as coverage:

- **The admission webhook's serving certificate.** It is served by
  controller-runtime from `--webhook-cert-dir` (`tls.crt` / `tls.key`,
  cert-manager-issued in the chart). The apiserver verifies the webhook;
  the webhook does not authorize the apiserver.
- **The metrics endpoints** on the operator (`--metrics-bind-address`),
  the frontend and the node-agent (`--metrics-addr`). Plain HTTP,
  intended to be reachable only from the cluster's scrape path.
- **The node-agent → guest agent control channel.** AF_VSOCK, not TCP,
  and not reachable from the sandboxed workload's egress path at all.

#### The tracing exporter is not an mTLS surface by default

The OTLP exporter is the one hop where "TLS" does not mean "mTLS", and
it is worth being exact about it.

By default the exporter uses one-way TLS: setec verifies the collector
against the host root store, or against a bundle given with
`--otel-ca-file`, and **presents no identity of its own**. A collector
cannot use this channel to establish who is talking to it. The floor on
this hop is **TLS 1.2**, one notch below the TLS 1.3 floor every mTLS
hop holds — a deliberate, narrow concession, because the peer is a
third-party endpoint outside setec's control (frequently a vendor
gateway or a TLS-terminating proxy) and this channel carries spans
rather than the authority to run a microVM. The mTLS floor is not
negotiable and is unaffected.

Setting `--otel-spiffe-socket` together with at least one
`--otel-spiffe-server-id` opts this hop into mutual TLS: the operator
presents its X509-SVID and authorizes the collector's SPIFFE ID, with
the same allow-list-and-trust-domain rules as every other SPIFFE
surface. `--otel-ca-file` and the `--otel-spiffe-*` flags are mutually
exclusive; configuring both is a startup error.

`--otel-insecure` exports spans in **plaintext**. It is a dev-cluster
setting, the chart leaves it off, and the operator logs a loud warning
at startup when it is on.

#### One module owns this, and a guard keeps it that way

Every credential above is built in one place, `internal/credentials`,
behind a narrow interface: configuration in, transport credentials out.
The TLS floor, the mandatory client certificate and the peer
authorization hook are properties of that module, so they hold on every
hop rather than on whichever call site remembered them.

That is enforced, not merely intended. `internal/credguard` fails the
build when a TLS credential is assembled anywhere else — a hand-built
`tls.Config`, a hand-assembled trust pool, a gRPC TLS-credential
constructor, or a `go-spiffe` import. It runs in `make check` and
`make test` (and alone as `make guard-credentials`), it walks the whole
tree including the separate Go modules under `examples/`, test files
included, and it fails rather than passes when its scan root is empty
or missing. Its allow-list lives in `internal/credguard/exemptions.go`;
every entry names one file or one directory and carries the reason it
is there, and an entry that stops being needed fails the build too.

#### One caveat an operator should hear rather than infer

In a cluster where any principal can create a Pod with an arbitrary
`serviceAccountName`, SVID identity reduces to workload-create RBAC,
because Kubernetes has no `serviceaccounts/use` verb. Anyone who can
create a workload in a namespace can create it with the identity setec
authorizes, and the SPIFFE ID then proves only that the caller could
schedule a Pod under that service account.

That is a property of the *consuming* cluster, not of setec, and
addressing it is the cluster operator's work: narrow workload-create
grants, and an admission policy keyed on the requesting user. setec
implements SPIFFE correctly regardless — but no operator should be told
that turning on SPIFFE mode bought them a boundary their cluster does
not enforce.

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
