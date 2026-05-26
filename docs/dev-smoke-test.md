# Release Smoke Test (Manual)

This is the **manual pre-release verification checklist** the Setec
maintainer runs on a real KVM-capable Linux box before tagging a release.
It is explicitly a human checklist — not an automated test — because its
purpose is to catch real-runtime issues (VM boot latency, image-pull
behavior, Kata/Firecracker surprises) that `envtest` and the gated E2E
suite can miss.

This document builds on [docs/quickstart.md](quickstart.md); the install
steps are the same but with extra observation points and a structured
report at the end. Allow roughly one hour end-to-end.

## Pre-conditions

Before you begin:

- [ ] A **KVM-capable Linux box** with `/dev/kvm` present (`ls -l /dev/kvm`).
- [ ] A **Kubernetes 1.28+** cluster running on that box (single-node or
      multi-node, either works).
- [ ] `kubectl` configured for the target cluster (`kubectl cluster-info`
      succeeds) with cluster-admin privileges.
- [ ] `helm` 3.8 or later on your PATH.
- [ ] Local checkout of the Setec repository at the revision you intend to
      tag.
- [ ] The workstation's clock is correct (used for wall-clock measurements).
- [ ] A blank text file open for recording timings (or a copy of the
      [report template](#report-template) at the bottom of this doc).

Record the following up front:

- Host OS / kernel version (`uname -a`).
- Kubernetes server version (`kubectl version --short`).
- Setec commit SHA to be released (`git rev-parse --short HEAD`).

## 1. Install Kata Containers

If Kata is already installed cluster-side, skip to step 2. Otherwise:

```bash
kubectl apply -k "github.com/kata-containers/kata-containers/tools/packaging/kata-deploy/kata-deploy/base?ref=main"
kubectl rollout status -n kube-system ds/kata-deploy --timeout=5m
kubectl get runtimeclass kata-fc
```

- [ ] `kata-fc` RuntimeClass is present.
- [ ] At least one Node is labeled `katacontainers.io/kata-runtime=true`
      (`kubectl get nodes -l katacontainers.io/kata-runtime`).

Record: Kata version (from the `kata-deploy` image tag), time elapsed.

## 2. Install Setec from the local chart

```bash
helm install setec ./charts/setec \
  --namespace setec-system \
  --create-namespace
kubectl get deploy -n setec-system
kubectl -n setec-system logs deployment/setec | head -40
```

- [ ] Operator Deployment shows 1/1 ready within **30 seconds**.
- [ ] Startup log reports `kata_runtime_available: true` and
      `kata_capable_nodes > 0`.
- [ ] `/readyz` returns HTTP 200 with a body containing
      `kata_runtime_available: true` (port-forward to `:8081` and curl
      `/readyz`).

Record: operator image digest, ready time, /readyz response body.

## 3. Walk through the six design.md E2E scenarios

Apply each manifest, observe the expected outcome with `kubectl`, and
record the requested measurements. Between scenarios, delete the Sandbox
and confirm the backing Pod is garbage-collected.

Save the shared setup as `/tmp/ns.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: setec-smoke
```

```bash
kubectl apply -f /tmp/ns.yaml
```

For every scenario below, record:

- **Pod schedule-to-Running** latency: time from first `kubectl get pod
  <pod>` showing a scheduled Pod to `status.phase=Running`. This is the
  microVM cold-start envelope (Firecracker boot + kubelet readiness).
- **Image pull duration**: from `kubectl describe pod <pod>` events
  (`Pulling` → `Pulled`).
- **Sandbox phase-to-Running** latency: time from Pod `Running` event to
  Sandbox `status.phase=Running`. Target: under 5 seconds (Requirement
  3.2).
- **Wall-clock scenario duration**: apply → terminal phase observed.

### Scenario 1: successful run, exit 0

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-success
  namespace: setec-smoke
spec:
  image: docker.io/library/python:3.12-slim
  command: ["python", "-c", "print('ok')"]
  resources:
    vcpu: 1
    memory: 512Mi
```

- [ ] Phase sequence: `Pending` → `Running` → `Completed`.
- [ ] `status.exitCode == 0`.
- [ ] `kubectl logs smoke-success-vm` prints `ok`.

### Scenario 2: failure, exit 1

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-failure
  namespace: setec-smoke
spec:
  image: docker.io/library/busybox:1.36
  command: ["false"]
  resources:
    vcpu: 1
    memory: 256Mi
```

- [ ] Phase sequence: `Pending` → `Running` → `Failed`.
- [ ] `status.exitCode == 1`.
- [ ] `status.reason == "ContainerExitedNonZero"`.

### Scenario 3: timeout

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-timeout
  namespace: setec-smoke
spec:
  image: docker.io/library/busybox:1.36
  command: ["sleep", "300"]
  resources:
    vcpu: 1
    memory: 256Mi
  lifecycle:
    timeout: 15s
```

- [ ] Phase sequence: `Pending` → `Running` → `Failed`.
- [ ] `status.reason == "Timeout"` within **~15 seconds ± 5s** of entering
      `Running`.
- [ ] Backing Pod is deleted; `kubectl get pod smoke-timeout-vm` returns
      `NotFound`.

### Scenario 4: deletion mid-run

Reuse the `smoke-timeout` manifest with a longer sleep:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-deletion
  namespace: setec-smoke
spec:
  image: docker.io/library/busybox:1.36
  command: ["sleep", "600"]
  resources:
    vcpu: 1
    memory: 256Mi
```

Apply, wait for `Running`, then:

```bash
kubectl -n setec-smoke delete sandbox smoke-deletion
kubectl -n setec-smoke get pod smoke-deletion-vm
```

- [ ] Sandbox is removed within a few seconds.
- [ ] Backing Pod is garbage-collected (returns `NotFound` within ~10s).
- [ ] No stuck finalizers or orphaned Pods.

### Scenario 5: no RuntimeClass present

Temporarily remove `kata-fc` to exercise the `RuntimeUnavailable` path.
**Important:** back up the RuntimeClass first and restore it after the
scenario — other scenarios depend on it.

```bash
kubectl get runtimeclass kata-fc -o yaml > /tmp/kata-fc.yaml
kubectl delete runtimeclass kata-fc
```

Apply:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-no-runtime
  namespace: setec-smoke
spec:
  image: docker.io/library/busybox:1.36
  command: ["true"]
  resources:
    vcpu: 1
    memory: 256Mi
```

- [ ] Sandbox phase stays `Pending` for at least 60 seconds.
- [ ] `kubectl describe sandbox smoke-no-runtime` shows a
      `Warning RuntimeUnavailable` event with remediation guidance.
- [ ] Operator `/readyz` body now reports `kata_runtime_available: false`.

Restore the RuntimeClass:

```bash
kubectl apply -f /tmp/kata-fc.yaml
```

- [ ] Within 60 seconds of restoration the Sandbox transitions to
      `Running` / `Completed` without manual intervention.

### Scenario 6: operator restart mid-Sandbox

Apply a long-running Sandbox and restart the operator while it is
`Running`:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: smoke-restart
  namespace: setec-smoke
spec:
  image: docker.io/library/busybox:1.36
  command: ["sleep", "120"]
  resources:
    vcpu: 1
    memory: 256Mi
```

Once the Sandbox is `Running`:

```bash
kubectl -n setec-system rollout restart deployment/setec
kubectl -n setec-system rollout status deployment/setec --timeout=1m
```

- [ ] Operator Deployment rolls over cleanly.
- [ ] Sandbox phase remains `Running` throughout; no flap back to
      `Pending`.
- [ ] Sandbox eventually transitions to `Completed` with
      `status.exitCode == 0`.

## 4. Cleanup

```bash
kubectl delete namespace setec-smoke
helm uninstall setec --namespace setec-system
kubectl delete crd sandboxes.setec.zeroroot.ai
kubectl delete namespace setec-system
```

- [ ] All `Sandbox` resources removed.
- [ ] CRD removed.
- [ ] Operator namespace removed.
- [ ] No orphaned Pods in any namespace (`kubectl get pods -A | grep -i
      vm` returns nothing Setec-related).

## 5. Produce the report

Fill out the [report template](#report-template) below and attach it to
the release pull request or tag as `smoke-test-<version>.md`. If any
checklist item failed, the release is **not** green — file an issue,
block the tag, and triage before retrying.

---

## Report template

```markdown
# Setec Release Smoke Test — vX.Y.Z

- **Date:** YYYY-MM-DD
- **Tester:** <name>
- **Setec commit:** <short-sha>
- **Operator image digest:** sha256:...

## Environment
- Host OS / kernel: <uname -a>
- K8s server version: <kubectl version --short>
- Kata version (kata-deploy image): <tag>
- Node count / KVM-capable Node count: <n/n>

## Install
- kata-deploy rollout time: __s
- Setec Deployment ready time: __s
- /readyz response: {"kata_runtime_available": true, "kata_capable_nodes": <n>}

## Observed latencies (median across the six scenarios)
- Image pull duration: __s (python:3.12-slim), __s (busybox:1.36)
- microVM cold start (Pod schedule → Pod Running): __ms
- Sandbox phase-to-Running (Pod Running → Sandbox Running): __ms

## Scenario results
| # | Scenario | Expected | Observed | Pass |
|---|----------|----------|----------|------|
| 1 | success, exit 0 | Completed, exit=0 | | [ ] |
| 2 | failure, exit 1 | Failed, exit=1, reason=ContainerExitedNonZero | | [ ] |
| 3 | timeout 15s | Failed, reason=Timeout within ~15s | | [ ] |
| 4 | delete mid-run | Sandbox + Pod gone within ~10s | | [ ] |
| 5 | no RuntimeClass | Pending + RuntimeUnavailable event; recovers on restore | | [ ] |
| 6 | operator restart | Sandbox phase stable through rollout; Completed | | [ ] |

## Anomalies / notes
- <free-form list of unexpected behaviors, warnings, or performance observations>

## Conclusion
- [ ] All six scenarios passed.
- [ ] Cleanup completed with no orphans.
- [ ] Release vX.Y.Z is cleared to tag.
```

---

# Phase 2 smoke test

These scenarios exercise the Phase 2 features — multi-tenancy,
NetworkPolicy, webhook rejection, node-agent thin-pool, frontend
roundtrip, Phase 1 → Phase 2 upgrade. Run AFTER the Phase 1 six
scenarios pass. Allow roughly 90 minutes end-to-end on a prepared
bare-metal host.

## Prerequisites (Phase 2)

In addition to the Phase 1 prerequisites:

- A CNI that enforces NetworkPolicy (Calico or Cilium recommended).
- Two unused block devices per KVM node for the devmapper thin-pool.
- cert-manager optionally installed (required only if you want the
  chart to auto-issue the webhook TLS cert).

## Install

```bash
helm upgrade --install setec charts/setec \
  --set multiTenancy.enabled=true \
  --set nodeAgent.enabled=true \
  --set nodeAgent.thinpoolDataDevice=/dev/vdb \
  --set nodeAgent.thinpoolMetadataDevice=/dev/vdc \
  --set webhook.enabled=true \
  --set webhook.certManager.enabled=true \
  --set frontend.enabled=true \
  --set observability.enabled=true \
  --set defaultClass.enabled=true
```

## Scenario P1: Multi-tenant ResourceQuota

1. `kubectl create ns tenant-a tenant-b && kubectl label ns tenant-a
   setec.zeroroot.ai/tenant=tenant-a && kubectl label ns tenant-b
   setec.zeroroot.ai/tenant=tenant-b`.
2. Apply a tight ResourceQuota (1 cpu / 1Gi) to tenant-a.
3. Apply two Sandboxes in tenant-a each requesting 1 cpu / 512Mi.
4. Apply one Sandbox in tenant-b.
5. Expected: at least one tenant-a Sandbox stays Pending; tenant-b's
   Sandbox reaches Running within normal cold-start time.

Observed wall-clock: __s. Pass: [ ]

## Scenario P2: NetworkPolicy enforcement

1. In tenant-a, apply a Sandbox with `network.mode=egress-allow-list`
   allowing only `api.example.com:443`.
2. Exec a debug container in the Pod that tries to reach two hosts.
3. Expected: the allowed host responds; any other host times out
   within 2s.

Observed: __. Pass: [ ]

## Scenario P3: Node-agent thin-pool

1. On a fresh node, verify `dmsetup ls` does not list `setec-thinpool`.
2. Deploy the node-agent DaemonSet with correct thinpoolDataDevice /
   thinpoolMetadataDevice.
3. Wait for the Pod to Ready.
4. Expected: `dmsetup ls` now lists `setec-thinpool`; `setec_node_
   thinpool_total_bytes` reports the configured device size.

Observed: __. Pass: [ ]

## Scenario P4: Webhook rejection at apply time

1. Apply a SandboxClass with `maxResources.vcpu=2`.
2. Apply a Sandbox referencing that class with `resources.vcpu=8`.
3. Expected: `kubectl apply` fails immediately with an error that
   contains the substring "vcpu".

Observed: __. Pass: [ ]

## Scenario P5: Frontend gRPC roundtrip

1. Create a client mTLS cert signed by the chart's client CA.
2. Using the example client in `docs/frontend-api.md`, issue
   Launch → Wait → Kill against `setec-frontend:50051`.
3. Expected: Launch returns a sandbox_id; Wait blocks until the
   Pod exits and returns the exit code; Kill returns cleanly; the
   Sandbox CR is deleted within 10s.

Observed: __. Pass: [ ]

## Scenario P6: Phase 1 to Phase 2 upgrade

1. Install the Phase 1 chart at tag v0.1.0.
2. Create a simple Sandbox and let it reach Running.
3. Run `helm upgrade` to the Phase 2 chart with every new toggle
   default-off.
4. Expected: the running Sandbox's Pod does not restart; the CR's
   .status.phase stays Running through the rollout; new Sandboxes
   applied after the upgrade continue to work.

Observed: __. Pass: [ ]

## Phase 2 report

- [ ] All six Phase 2 scenarios passed.
- [ ] No regressions observed in Phase 1 scenarios 1–6.
- [ ] Cluster cleanup completed with no orphaned Sandboxes, Pods,
      NetworkPolicies, or SandboxClasses outside release resources.
- [ ] Release v0.2.0 is cleared to tag.

## Phase 3 scenarios (snapshot, restore, pause/resume, pool)

Phase 3 scenarios require a cluster where the chart is installed
with `snapshots.enabled=true` AND `nodeAgent.enabled=true` AND a
runtime that actually supports Firecracker snapshots (see
docs/kata-firecracker-integration.md). Skip sections where a
prerequisite is missing and note it in the report.

The corresponding Go E2E scenarios (`TestPhase3_PoolColdStart`,
`TestPhase3_StorageFillProtection`, `TestPhase3_UpgradeFromPhase2`)
are expected to pass on the bare-metal runner once Phase 4 has
landed the `setec-pool-vm` launcher and pool reconcile tick. Run
them via `make e2e` on that host and record results in the
v0.1.0 smoke-test report (`docs/launch/v0.1.0-smoke-test-result.md`).

## Scenario P7: Snapshot create and restore roundtrip

1. Create a Sandbox that writes a marker file and sleeps long
   enough for the snapshot to capture:
   ```yaml
   spec:
     image: docker.io/library/alpine:3.19
     command: ["sh", "-c", "echo hello > /tmp/marker && sleep 120"]
     snapshot:
       create: true
       name: smoke-state
   ```
2. Wait for `kubectl get snapshot smoke-state` to reach
   `PHASE=Ready` (typically under 30s).
3. Launch a new Sandbox with `spec.snapshotRef.name: smoke-state`
   and a command that reads the marker.
4. Expected: the new Sandbox reaches phase=Completed and its logs
   contain "hello". The cold-start metric histogram records a
   sample under 500ms.

Observed: __. Pass: [ ]

## Scenario P8: Pause and Resume

1. Launch a Sandbox that burns CPU
   (`command: ["sh", "-c", "while :; do :; done"]`).
2. `kubectl top pod` (with metrics-server installed) shows CPU
   above 0.5.
3. Patch `spec.desiredState=Paused`; expect
   `status.phase=Paused` within a few reconciles.
4. Observe CPU usage drops to near zero.
5. Patch `spec.desiredState=Running`; phase returns to Running
   within ~1s.

Observed: __. Pass: [ ]

## Scenario P9: Pre-warmed pool cold-start

1. Create a SandboxClass with `preWarmPoolSize=3` and
   `preWarmImage` matching the workload.
2. Wait for `setec_prewarm_pool_entries{class="fast"}` to report 3
   (via Prometheus).
3. Launch a Sandbox referencing the class and image.
4. Expected: `setec_sandbox_cold_start_seconds` records a sample
   under 0.1s.

Observed: __. Pass: [ ]

## Scenario P10: Disk fill protection

1. Fill the snapshot-root filesystem above 90% (e.g. `dd
   if=/dev/zero of=/var/lib/setec/snapshots/pad bs=1M count=...`).
2. Attempt a snapshot create.
3. Expected: the Sandbox stays Running; an Event
   `reason=InsufficientStorage` is emitted; no partial snapshot is
   persisted.
4. Clean up the padding file; subsequent snapshot requests
   succeed.

Observed: __. Pass: [ ]

## Scenario P11: Phase 2 to Phase 3 upgrade

1. Install the Phase 2 chart at tag v0.2.0.
2. Create a simple Sandbox without any snapshot fields; let it
   reach Running.
3. `helm upgrade` to Phase 3 with `snapshots.enabled=true`.
4. Expected: the running Sandbox remains Running; no Pod restart.
5. Create a new Sandbox with `snapshot.create=true`; observe a
   Ready Snapshot CR.

Observed: __. Pass: [ ]

## Phase 3 report

- [ ] All Phase 3 scenarios above passed (or were skipped with a
      documented reason).
- [ ] No regressions observed in Phase 1 and Phase 2 scenarios.
- [ ] Snapshot storage directory is fully reclaimed after
      `kubectl delete snapshot --all -A`.

## Pre-tag documentation review

Before tagging a release, walk every user-facing doc and confirm its
claims match the post-hardening code. This is the release gate for
Requirement 10.5 and catches stale wording introduced by recent
merges.

1. `README.md` — Highlights and Quick Install sections must describe
   only features the current code ships. No bullet mentions a feature
   that is still stubbed or behind a feature flag not documented in
   the surrounding text.
2. `charts/setec/README.md` — The Observability subsection must
   describe `observability.otelTLS` knobs correctly. The Frontend
   bullet must state that mTLS is mandatory (both Secret names
   required). No reference to `auth-disabled` or `insecure-grpc`
   anywhere in the file.
3. `docs/frontend-api.md` — Every RPC in the service definition has
   a usage example (Launch, StreamLogs, Wait, Kill). The
   Authentication section states mTLS is mandatory. No section says
   any RPC is "unimplemented" or "coming soon".
4. `docs/snapshots.md` — The Pre-warmed pool section describes the
   real containerd prefetch (with the `setec_node_image_prefetch_errors_total`
   counter). The mTLS bullet states no fallback exists.
5. `docs/crd-reference.md` — `spec.network` describes all three
   modes (`full`, `egress-allow-list`, `none`) as actively enforced
   via NetworkPolicy.
6. `docs/quickstart.md` and `docs/getting-started.md` — Every
   `kubectl` / `helm` invocation in the walkthrough matches the
   current chart's required values and flags.

For each doc, either tick the box or record the file path plus the
exact line that needs an update. Do not tag v0.1.0 with any box
unchecked.
- [ ] Release v0.3.0 is cleared to tag.
