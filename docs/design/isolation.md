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

## Sandbox identity

Each launcher Sandbox has an identity that a verifier outside the Sandbox can check (setec#235). A hostname or a header that a process writes is not an identity: a fork has the memory of its source and can write any value.

- **The key.** The operator makes an ed25519 key for each Sandbox, so for each fork, in the Secret `<name>-identity`. Only the launcher container mounts it. No process in the machine can read it. The status holds the public key: `status.identity.publicKey`. `internal/controller/identity.go`, `internal/podspec/launcher.go`.
- **The token.** A process gets a token with `GET /v1/token?audience=<verifier>` on the Unix socket that `SETEC_IDENTITY_SOCKET` names (`/run/setec/identity.sock`). The guest agent asks the launcher over vsock, and the launcher signs the token. The token is a compact JWS with the EdDSA algorithm. It names the sandbox id (`sub`, `<namespace>/<name>/<uid>`), the audience, the owner pair, the identity generation, a `jti`, and a lifetime of 5 minutes. A process asks for a new token for each call, so no rotation is necessary. `internal/sandboxid/`, `internal/launcher/identity.go`, `internal/guestagent/identity.go`.
- **The generation.** Each snapshot of a Sandbox raises its identity generation (`status.identity.generation`). The node agent gives the new generation to the launcher while the machine is paused, so each token after the snapshot carries it, and no token in the snapshot does. The Snapshot is Ready only after the status holds the new generation. So a fork or a restore that finds a token of its source in its copy of the memory cannot use it. `internal/snapshot/coordinator.go`, `internal/nodeagent/grpcserver/server.go`.
- **The check.** `SandboxService.VerifySandboxIdentity` checks a token for the owner of the Sandbox: the signature with the key of the Sandbox that the token names, the uid of that Sandbox, the audience, the lifetime and the current generation. A fork signs with its own key, so its token never verifies as its source. A verifier can also refuse a second use of one `jti`. `internal/frontend/identity.go`.

