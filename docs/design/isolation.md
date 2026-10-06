<!-- SPDX-License-Identifier: Apache-2.0 -->
# Isolation

A Sandbox runs code that `setec` does not trust. This page lists each control that keeps that code inside its Sandbox. The controls that depend on the backend are on the [runtime](runtime.md) and [threat model](threat-model.md) pages.

## The Pod

`internal/podspec/builder.go` builds every Sandbox Pod with these settings:

- The workload runs as user and group 65532, with `runAsNonRoot`.
- The root filesystem is read-only. The only writable path is the scratch volume at `/tmp` ([storage](storage.md)).
- `allowPrivilegeEscalation` is false. The container drops every capability and adds `NET_RAW` and `NET_ADMIN` only.
- The seccomp profile is `RuntimeDefault`.
- No ServiceAccount token is mounted (`automountServiceAccountToken: false`).
- The Pod uses the resolvers that the operator is configured with (`dnsPolicy: None`), so it cannot look up in-cluster Service names.

## The network

`internal/netpol/generator.go` writes one NetworkPolicy for each Sandbox. The mode comes from the Sandbox, else from its class, else it is `none` (`api/v1alpha1/sandbox_types.go`, `NetworkMode`):

- `none`: no ingress and no egress, DNS included.
- `external-only`: egress to the internet on every port, with the reserved ranges removed. No ingress. DNS only to the configured resolvers.
- `egress-allow-list`: egress only to the entries of `network.allow`, each on its own ports, with the same reserved ranges removed.

The reserved ranges cover private, link-local and metadata addresses in IPv4 and IPv6 (`charts/setec/values.yaml`, `netpol.reservedCIDRs`). A class can exempt a range with `spec.egressExemptCIDRs`.

## The namespace

- Each Sandbox namespace gets a default-deny NetworkPolicy for every Pod, so a Pod that `setec` did not build is not open by default. `internal/netpol/baseline.go`, `charts/setec/templates/sandbox-namespace-baseline-netpol.yaml`.
- A ValidatingAdmissionPolicy refuses host network, host PID, host IPC, host ports, `hostPath` volumes and privileged containers in a Sandbox namespace. `charts/setec/templates/sandbox-namespace-host-guard.yaml`.
- The operator can write Pods only in the Sandbox namespaces that the install lists. `charts/setec/templates/sandbox-namespace-rbac.yaml`.
- The frontend serves the enrolled client clusters of one owner. Each pair of client cluster and tenant has its own namespace. A call from another pair on a Sandbox gets `PERMISSION_DENIED`. `internal/frontend/auth.go`, `internal/tenancy/identity.go`.

## Restored Sandboxes

A Sandbox that starts from a snapshot is served only when five checks have positive evidence. A missing signal counts as a failure. `internal/snapshot/gate/gate.go` holds the one decision:

1. **Clean base**: the snapshot holds no secret. `internal/snapshot/secretscan/`.
2. **New identity for each restore**: the guest reseeds its random number generator and confirms it, and the node agent makes the restored identity unique. `internal/entropy/`, `internal/uniquify/`, `cmd/setec-guest-agent/`.
3. **One session**: one restored state serves one session and is then destroyed. `internal/snapshot/coordinator.go`.
4. **Provenance**: a pool template comes from its class image. `internal/snapshot/gate/gate.go`.
5. **Encrypted at rest**: each artifact has its own key, sealed by a node key. `internal/snapshot/atrest/atrest.go`.

A dev cluster can turn the checks off only with two signals: a label on the gate namespace and an annotation on the class. The class then carries a condition that says so, and each restore records a warning event.
