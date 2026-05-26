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
