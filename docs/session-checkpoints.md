# Session checkpoints on real infrastructure

How to run the session-lifecycle e2e (setec#192 / #193 / #194) against a
real cluster and a real object store, and what each scenario costs.

The suite is `test/e2e/session_reattach_test.go` and
`test/e2e/session_checkpoint_test.go`. CI runs it from the
`session-checkpoint` job in `.github/workflows/e2e.yml`.

## What the scenarios need

| Scenario | Object store | Sandbox-capable nodes |
|---|---|---|
| `TestSession_ReattachByHandle` | no | 1 |
| `TestSessionCheckpoint_SuspendIdleResume` | yes | 1 |
| `TestSessionCheckpoint_DrainResumeOnOtherNode` | yes | **2** |

All three need the `kata-fc` backend. Memory checkpointing drives the
Firecracker API socket directly, so `kata-qemu` cannot serve these
scenarios however healthy the node looks.

Every scenario that cannot run **skips loudly**: it prints a banner naming
what is missing and what would satisfy it, and the CI job repeats the
skips in the run summary. A green run that contains a skip banner has not
verified that scenario.

## Object store

Set `SETEC_E2E_S3=1` plus:

| Variable | Meaning |
|---|---|
| `SETEC_E2E_S3_BUCKET` | checkpoint bucket (required) |
| `SETEC_E2E_S3_REGION` | signing region, default `us-east-1` |
| `SETEC_E2E_S3_PREFIX` | key prefix; must match the prefix the IAM policy is scoped to |
| `SETEC_E2E_S3_ENDPOINT` | set for MinIO and other self-hosted stores, empty for real S3 |
| `SETEC_E2E_S3_ROLE_ARN` | IRSA role for the node-agent ServiceAccount (EKS) |
| `SETEC_E2E_S3_CREDENTIALS_SECRET` | pre-existing Secret of static keys, for non-IRSA environments |

`SETEC_E2E_S3=1` with no bucket, or with neither a role ARN nor a
credentials Secret against real S3, fails the suite at startup rather than
installing a node-agent that would fail closed at the first suspend.

On staging the bucket and role come from Terraform in the `deploy` repo,
`eks/gibson`: `module.s3` key `setec_checkpoints` and `module.iam_irsa`
key `setec_node_agent`. The role's trust policy pins exact
`system:serviceaccount:<ns>:<sa>` subjects, which is why the CI job uses a
**fixed** namespace and release name (`setec-e2e-session`) instead of the
suite's default per-run stamp — a wildcard subject would let anyone able
to create a namespace in staging assume a role with write access to the
bucket.

## The two-node scenario, and what it costs

`TestSessionCheckpoint_DrainResumeOnOtherNode` checkpoints a session on
node A and resumes it on node B. It therefore needs two sandbox-capable
nodes.

Staging runs exactly **one**, by design. The `setec-metal` Karpenter
NodePool (deploy `eks/gibson/karpenter.tf`) carries `limits: {cpu: 48}`,
which is one `m5zn.metal`, and consolidates it away 120 seconds after it
goes idle. The standing instruction is cheapest-possible with no warm
nodes, so **the ceiling is not raised permanently**.

A second `m5zn.metal` costs roughly **$4/hour on-demand in us-east-1**
(48 vCPU, 192 GiB, bare metal), billed from the moment Karpenter
provisions it until consolidation reclaims it. A drain run occupies it for
a few minutes, but node provisioning and kata-deploy installation add
10–20 minutes on top, so budget on the order of **$1–2 per run** and treat
a forgotten ceiling as roughly **$100/day**.

The scenario is therefore an explicit, temporary opt-in rather than an
automatic capability probe:

1. Raise the ceiling. In `deploy`, `eks/gibson/karpenter.tf`, set the
   `setec-metal` NodePool `limits.cpu` to `96`, open a PR, merge, and let
   the apply run. This is a Terraform change, not a `kubectl patch` — the
   cluster is GitOps-driven and a hand-patched NodePool is reverted by the
   next reconcile.
2. Wait for the second node to join and to carry
   `setec.zeroroot.ai/runtime.kata-fc=true`. Both the label AND
   `katacontainers.io/kata-runtime=true` matter: the capability label
   alone can appear before kata is actually installed (setec#243).
3. Run with `SETEC_E2E_SESSION_DRAIN=1`, or set the repository variable
   `STAGING_SESSION_DRAIN_CAPACITY` and dispatch the `e2e` workflow.
4. **Revert the ceiling PR.** Consolidation reclaims the node once it is
   idle and back under the limit.

With the opt-in set but fewer than two schedulable capable nodes, the test
**fails** rather than skipping. The opt-in is an assertion that capacity
was paid for; a silent skip there would mean full cost and zero coverage.

## Diagnosing a failure

Attribute the failure before filing it against the session path.

- **Sandbox stuck Pending, no node has `runtime.kata-fc=true`** — the
  runtime-agent's node probe, not the session code. setec#281 is the
  known instance (the probe did not follow containerd's `imports` array).
  The CI job checks this in preflight and fails with that pointer.
- **`AccessDenied` in the node-agent log** — IAM or KMS, not the
  checkpoint code. The bucket's default encryption is SSE-KMS, so the role
  needs a KMS grant as well as the S3 statement; without it every
  `PutObject` returns `KMS.AccessDeniedException` while the S3 policy
  looks correct. A `HeadObject` on a key that does not exist returns 403
  instead of 404 unless the role holds `s3:ListBucket` on the bucket, and
  the node-agent HEADs before every save.
- **`s3 session-checkpoint backend is not configured on this node`** —
  the node-agent is running without `--s3-bucket`. The chart renders the
  S3 flags only inside the `snapshots.enabled` guard, inside the
  node-agent DaemonSet; `snapshots.s3.enabled=true` with either switch off
  now fails the render rather than deploying a node-agent that cannot
  checkpoint.
- **The operator reports a checkpoint failure with no detail** — read the
  node-agent log, not the operator log. The operator never touches the
  bucket; it forwards the per-session KEK and records the outcome.
