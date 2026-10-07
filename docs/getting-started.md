<!-- SPDX-License-Identifier: Apache-2.0 -->
# Getting Started with Setec

This is a narrative walk-through that takes roughly fifteen minutes and ends with a workload running inside an isolated sandbox under Kubernetes control. Where the [quickstart](./quickstart.md) says "run this command", this page says "run this command; you should see X; this is happening because Y". If you have done this before, the quickstart is shorter.

Everything here runs against your own cluster. No cloud account, no login, no telemetry.

## How a Sandbox runs

Each `Sandbox` is one Firecracker microVM in one launcher Pod. The node needs `/dev/kvm`: bare metal, or a VM with nested virtualization. [docs/design/runtime.md](./design/runtime.md) describes the parts.

## Before You Start

You will need:

1. A Kubernetes cluster (1.35 or later) with cluster-admin credentials. Each launcher Pod mounts the signed disk of its image as an image volume, which needs 1.35. A single-node development cluster works, provided the node has `/dev/kvm`.
2. At least one amd64 worker node with `/dev/kvm`.
3. An OCI registry that the cluster can push to and pull from, for the signed image disks.
4. `kubectl`, `helm` 3.8 or later, and `openssl` on your workstation.
5. About fifteen minutes of unhurried time.

The full prerequisite check-list is in [`docs/prerequisites.md`](./prerequisites.md).

## Step 1: Verify KVM

Setec needs `/dev/kvm` on the worker node. The operator is happy to install without it, but every `Sandbox` you launch will stay `Pending` forever. Saving yourself that frustration takes one command on any worker node:

```bash
ls -l /dev/kvm
```

You should see a character device owned by `root:kvm` (or similar). If you see "No such file or directory", the node is not running on bare metal or nested virtualization is disabled. Fix that first.

## Step 2: Make the disk signing key

The disk builder turns each image into a squashfs disk and signs it. Each launcher refuses a disk that your key did not sign. Make an ed25519 key and store its seed in the operator namespace:

```bash
openssl genpkey -algorithm ed25519 -out disk-signing.pem
kubectl create namespace setec-system
kubectl -n setec-system create secret generic setec-disk-signing \
  --from-literal=seed="$(openssl pkey -in disk-signing.pem -outform DER | tail -c 32 | base64 -w0)"
DISK_PUB="$(openssl pkey -in disk-signing.pem -pubout -outform DER | tail -c 32 | base64 -w0)"
```

The raw seed and the raw public key are the last 32 bytes of the DER forms. Keep `disk-signing.pem` somewhere safe: it is the key that decides which disks a machine boots.

## Step 3: Install Setec

The Setec install is one helm command. Replace the registry with yours:

```bash
helm install setec ./charts/setec \
  --namespace setec-system \
  --set launcher.diskRepo=registry.example.com/setec-disks \
  --set launcher.diskBuilder.signingSecret=setec-disk-signing \
  --set "launcher.diskBuilder.publicKeys={${DISK_PUB}}" \
  --set 'sandboxNamespaces={default}'
```

Helm prints a summary showing the release name, namespace, and the resources it created. There is a `Deployment` for the operator, a `DaemonSet` for the KVM device plugin (it offers `/dev/kvm` and `/dev/net/tun` of each node as the resources `setec.zeroroot.ai/kvm` and `setec.zeroroot.ai/tun`), a `ClusterRole`, a `ClusterRoleBinding`, a few `ServiceAccounts`, and the `Sandbox`, `SandboxClass`, and `Snapshot` `CustomResourceDefinitions`.

Check the operator is healthy:

```bash
kubectl get deploy -n setec-system
kubectl get pods -n setec-system
```

The operator pod and the device plugin pods should be `Running`. Read a few lines of the operator log:

```bash
kubectl -n setec-system logs deployment/setec | head -40
```

Check that at least one node offers the KVM device:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,KVM:.status.allocatable.setec\.zeroroot\.ai/kvm'
```

If the column is empty, go back to step 1; Setec will accept your Sandboxes but nothing will schedule.

### What you just did

You installed a Kubernetes operator that watches a set of custom resources, a device plugin that offers the KVM device of each node to the launcher Pods, and the CRDs that together form Setec's external contract. Nothing launched yet; the cluster is idling in a steady state.

## Step 4: Launch Your First Sandbox

Save this manifest as `hello.yaml`:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: hello
  namespace: default
spec:
  # python:3.12-slim. A Sandbox names its image by digest: the disk of the
  # machine belongs to one digest.
  image: docker.io/library/python@sha256:02108f5d322dd89f1c9e552442c25acb0543dfdbc455693a5599624f20d9155d
  command:
    - python
    - -c
    - "print('hello from a Firecracker microVM')"
  resources:
    vcpu: 1
    memory: 512Mi
  lifecycle:
    timeout: 5m
```

A tour of the fields:

- `spec.image`: the OCI image whose root filesystem becomes the microVM's root filesystem, by digest. Any standard image works. The first Sandbox of a digest waits for the disk builder to make and sign its disk.
- `spec.command`: what to run inside the VM. If omitted, the image's entrypoint is used.
- `spec.resources.vcpu` and `spec.resources.memory`: the microVM's CPU and memory ceiling. These are hard caps enforced by Firecracker, not Kubernetes requests.
- `spec.lifecycle.timeout`: after this duration the operator terminates the workload and records a timeout status. It stops runaway jobs without you watching them.

Apply it:

```bash
kubectl apply -f hello.yaml
```

### What you just did

You told Kubernetes "I want this workload to run in a microVM with these specific limits". The Setec operator turned that intent into a launcher `Pod`. The launcher mounts the signed disk of your image, boots a Firecracker VM, and runs your workload inside it. The Sandbox resource is the long-lived record of the job; the pod is its short-lived implementation detail.

## Step 5: Watch It Run

Watch the Sandbox transition through phases:

```bash
kubectl get sandbox -w
```

You will see three phases in sequence:

```
NAME    PHASE      IMAGE                               AGE
hello   Pending    docker.io/library/python@sha256:0210…  2s
hello   Running    docker.io/library/python@sha256:0210…  8s
hello   Completed  docker.io/library/python@sha256:0210…  12s
```

- `Pending` means the operator has accepted the request and is preparing the pod. The disk build, the disk pull and the Firecracker boot happen during this phase.
- `Running` means the VM is up and the workload is executing.
- `Completed` means the process exited cleanly. You will see `Failed` instead if the process exited non-zero, or `TimedOut` if the lifecycle deadline elapsed first.

Press Ctrl-C to stop the watch, then inspect the detail:

```bash
kubectl describe sandbox hello
```

The `Status` block carries the underlying pod name (`hello-vm`), phase transition timestamps, and any events the operator emitted. The final event usually reports workload exit code.

Read the workload output by reading its pod logs:

```bash
kubectl logs hello-vm
```

The log is the console of the machine. Among the boot lines you should see:

```
hello from a Firecracker microVM
```

If you instead see `error: container not found`, the pod has already been cleaned up because its Sandbox was deleted. That's fine; re-apply the manifest to try again.

### What you just did

You proved end-to-end: Sandbox admitted, pod scheduled, microVM booted, workload ran, exit captured, logs surfaced. Everything you touched used standard Kubernetes verbs and standard pod log retrieval. No Setec-specific CLI.

## Step 6: Clean Up

Delete the Sandbox. Because the backing pod has an `OwnerReference` to the Sandbox, Kubernetes garbage-collects the pod (and therefore tears down the microVM) as soon as the Sandbox is gone:

```bash
kubectl delete sandbox hello
```

If you are done experimenting, uninstall the chart. This leaves the CRD in place, which means any `Sandbox` resources that already exist survive:

```bash
helm uninstall setec --namespace setec-system
```

To remove Setec entirely, including the CRDs (and any remaining `Sandbox` objects with them):

```bash
kubectl delete crd sandboxes.setec.zeroroot.ai sandboxclasses.setec.zeroroot.ai snapshots.setec.zeroroot.ai
```

## What You Just Did

In fifteen minutes you installed a Kubernetes-native microVM runtime, declared a workload as a custom resource, and watched Kubernetes orchestrate a Firecracker VM to run it. The only thing your cluster knew how to do beforehand was schedule containers; now it can also schedule hardware-isolated microVMs, described through the same `kubectl apply` pattern that every Kubernetes operator uses.

The point of Setec is that the interface to microVM isolation is the same interface you already use for everything else. The operator does the translation between Kubernetes intent and the Firecracker machine below it. There is no new dashboard, no new CLI, no cloud account.

## Next Steps

- [Multi-tenancy](./multitenancy.md) &mdash; tenant labels and per-tenant policy.
- [Snapshots](./snapshots.md) &mdash; snapshot capture, restore, and pause/resume.
- [Observability](./observability.md) &mdash; the metrics you should scrape and the alerts we ship.
- [gRPC Frontend API](./frontend-api.md) &mdash; launch Sandboxes programmatically from a client.
- [Examples](../examples/) &mdash; three reference consumer programs (AI code execution, CI sandbox, security research).
- [CRD Reference](./crd-reference.md) &mdash; every field, every default.
