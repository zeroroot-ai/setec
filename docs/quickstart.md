# Quickstart

This guide takes a prepared user from an empty cluster to a running
`Sandbox` in under 15 minutes. Each `Sandbox` is one Firecracker microVM
in one launcher Pod.

You need working familiarity with `kubectl` and `helm`.

## 1. Prerequisites

Before you start, verify all of the following on your workstation:

- [ ] A Kubernetes **1.35+** cluster you can reach with `kubectl`. Each
      launcher Pod mounts the signed disk of its image as an image volume,
      which needs 1.35.
- [ ] At least one amd64 worker Node with `/dev/kvm` (bare metal, or a VM
      with nested virtualization).
- [ ] An OCI registry that the cluster can push to and pull from, for the
      signed image disks.
- [ ] `kubectl` configured for the target cluster (`kubectl cluster-info`
      succeeds).
- [ ] `helm` 3.8 or later (`helm version`), and `openssl`.
- [ ] Cluster-admin permission in the target cluster for the duration of
      the install (needed to register the CRD and ClusterRole).

[docs/prerequisites.md](prerequisites.md) explains each requirement.

## 2. Make the disk signing key

The disk builder signs each image disk, and each launcher refuses a disk
that your key did not sign:

```bash
openssl genpkey -algorithm ed25519 -out disk-signing.pem
kubectl create namespace setec-system
kubectl -n setec-system create secret generic setec-disk-signing \
  --from-literal=seed="$(openssl pkey -in disk-signing.pem -outform DER | tail -c 32 | base64 -w0)"
DISK_PUB="$(openssl pkey -in disk-signing.pem -pubout -outform DER | tail -c 32 | base64 -w0)"
```

## 3. Install Setec

Install from the OCI chart registry. Replace the registry with yours:

```bash
helm install setec oci://ghcr.io/zeroroot-ai/charts/setec \
  --namespace setec-system \
  --set launcher.diskRepo=registry.example.com/setec-disks \
  --set launcher.diskBuilder.signingSecret=setec-disk-signing \
  --set "launcher.diskBuilder.publicKeys={${DISK_PUB}}" \
  --set 'sandboxNamespaces={default}'
```

Or, if you are installing from a checked-out source tree, use
`./charts/setec` in place of the OCI reference.

Verify the operator is running:

```bash
kubectl get deploy -n setec-system
kubectl get pods -n setec-system
```

Expected: one Deployment named `setec` with one ready replica, and one
device plugin Pod on each amd64 Node.

Check that at least one Node offers the KVM device:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,KVM:.status.allocatable.setec\.zeroroot\.ai/kvm'
```

If the column is empty, no Node exposes `/dev/kvm`. Setec starts anyway,
but each `Sandbox` you apply sits in `Pending` until a Node offers the
device.

## 4. Apply your first Sandbox

Save the following as `hello.yaml`:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: hello
  namespace: default
spec:
  # python:3.12-slim, by digest: the disk of the machine belongs to one
  # digest.
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

Apply it:

```bash
kubectl apply -f hello.yaml
```

## 5. Observe the lifecycle

Watch the Sandbox transition through its phases:

```bash
kubectl get sandbox -w
```

Expected phase sequence:

```
NAME    PHASE      IMAGE                               AGE
hello   Pending    docker.io/library/python@sha256:0210…   2s
hello   Running    docker.io/library/python@sha256:0210…   8s
hello   Completed  docker.io/library/python@sha256:0210…   12s
```

`Pending` → `Running` is the microVM cold start: the disk build for the
first Sandbox of a digest, the disk pull, and the Firecracker boot. `Running` → `Completed` tracks the workload executing and exiting.

Inspect the event stream and status detail:

```bash
kubectl describe sandbox hello
```

## 6. Read the workload output

The Sandbox spawns a Pod named `<sandbox-name>-vm`. Read its logs like any
other Pod:

```bash
kubectl logs hello-vm
```

The log is the console of the machine. Among the boot lines you should
see:

```
hello from a Firecracker microVM
```

## 7. Cleanup

Delete the Sandbox — the backing Pod is garbage-collected via its
OwnerReference, which terminates the microVM:

```bash
kubectl delete sandbox hello
```

Uninstall the operator (preserves any remaining `Sandbox` resources and
the CRD):

```bash
helm uninstall setec --namespace setec-system
```

Remove the CRD (this also deletes every `Sandbox` in the cluster because
the CRD owns them):

```bash
kubectl delete crd sandboxes.setec.zeroroot.ai
```

## Next steps

- [docs/crd-reference.md](crd-reference.md) — full field reference for the
  `Sandbox` CRD.
- [docs/prerequisites.md](prerequisites.md) — deeper explanation of KVM,
  nested virtualization, and the KVM device plugin.
- [charts/setec/README.md](../charts/setec/README.md) — Helm values,
  upgrade, and uninstall.
- [.github/workflows/e2e.yml](../.github/workflows/e2e.yml) — the nightly
  end-to-end run on real Firecracker machines.
