// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package podspec contains the pure translator that turns a Sandbox custom
// resource into the corev1.Pod the controller will create. The translator is
// deliberately side-effect free so that every mapping rule can be verified via
// table-driven unit tests without a running Kubernetes API server.
package podspec

import (
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

const (
	// PodNameSuffix is appended to the Sandbox name to derive the Pod name
	// (e.g. Sandbox "foo" → Pod "foo-vm").
	PodNameSuffix = "-vm"

	// SandboxLabelKey is the label applied to the owned Pod whose value is
	// the owning Sandbox's name. Callers (e.g. the controller) use this label
	// for owner-ref indexing and to filter events.
	SandboxLabelKey = "setec.zeroroot.ai/sandbox"

	// ContainerName is the name of the single workload container inside the
	// Pod. Kept as a constant so tests and the status reconciler can agree.
	ContainerName = "workload"

	// sandboxKind is the literal kind used in the generated OwnerReference.
	// The v1alpha1 types intentionally do not register a String()-like
	// helper, so we centralize the literal here.
	sandboxKind = "Sandbox"
)

// Errors returned by Build for structural problems the OpenAPI schema cannot
// express (e.g. a caller hand-constructs a Sandbox in Go and skips the API
// server's validation entirely).
var (
	// ErrNilSandbox is returned when Build is invoked with a nil Sandbox.
	ErrNilSandbox = errors.New("podspec: sandbox is nil")

	// ErrMissingName is returned when Sandbox.metadata.name is empty.
	ErrMissingName = errors.New("podspec: sandbox.metadata.name is required")

	// ErrMissingImage is returned when Sandbox.spec.image is empty.
	ErrMissingImage = errors.New("podspec: sandbox.spec.image is required")

	// ErrMissingCommand is returned when an ephemeral Sandbox's
	// spec.command is empty. A session may omit it (see
	// BuildOptions.KeepaliveImage).
	ErrMissingCommand = errors.New("podspec: sandbox.spec.command is required for an ephemeral sandbox and must have at least one entry")

	// ErrNoKeepaliveImage is returned when a session Sandbox omits
	// spec.command and no keepalive image is configured to boot in its
	// place. The operator refuses the Pod rather than booting a command
	// it does not have.
	ErrNoKeepaliveImage = errors.New("podspec: session sandbox has no spec.command and no keepalive image is configured")

	// ErrNoWorkspaceFormatImage is returned when a kata-fc session
	// Sandbox needs its workspace formatted and mounted (Firecracker has
	// no virtio-fs, so the workspace PVC is Block-mode and reaches the
	// guest as a raw device, setec#91) but no keepalive image is
	// configured. The same static binary that installs the keepalive
	// command also formats and mounts the workspace block device, so
	// this reuses BuildOptions.KeepaliveImage rather than adding a
	// second image knob.
	ErrNoWorkspaceFormatImage = errors.New("podspec: kata-fc session sandbox needs its workspace formatted and no keepalive image is configured")

	// ErrInvalidVCPU is returned when Sandbox.spec.resources.vcpu is less
	// than 1. The CRD validation caps the upper bound; we only double-check
	// the structural floor here.
	ErrInvalidVCPU = errors.New("podspec: sandbox.spec.resources.vcpu must be >= 1")

	// ErrInvalidMemory is returned when Sandbox.spec.resources.memory is
	// zero or negative.
	ErrInvalidMemory = errors.New("podspec: sandbox.spec.resources.memory must be > 0")

	// ErrMissingRuntimeClass is returned when Build is invoked with an empty
	// runtimeClassName. A Sandbox Pod without a runtime class would fall
	// through to the default container runtime, defeating the whole point
	// of Setec.
	ErrMissingRuntimeClass = errors.New("podspec: runtimeClassName is required")
)

// BuildOptions carries optional build-time knobs that are additive to
// the Phase 1 Build signature. Nil / zero-valued fields preserve
// Phase 1/2 behaviour.
type BuildOptions struct {
	// NodeName, when non-empty, is written into Pod.Spec.NodeName so
	// the scheduler pins the Pod to a specific node. Used by the
	// snapshot-restore flow which must land on the node holding the
	// snapshot state files.
	NodeName string

	// RuntimeSelection, when non-nil, overrides the runtimeClassName
	// argument and additionally injects NodeAffinity, Overhead, and any
	// dispatcher-specific pod mutations.  Applied as the last step in
	// BuildWithOptions so dispatchers see the fully-assembled pod.
	RuntimeSelection *runtimepkg.Selection

	// ResolverIPs are the DNS servers the Sandbox Pod is configured to
	// use. They are written into spec.dnsConfig.nameservers with
	// dnsPolicy None, so the workload resolves names through these
	// addresses instead of cluster DNS and cannot look up in-cluster
	// Service names. The controller passes the same list it gives
	// netpol.Config, so the Pod's resolver and the NetworkPolicy's DNS
	// rule can never disagree.
	//
	// Empty leaves the Pod on the cluster default resolver, which is
	// only appropriate for tests: the operator refuses to start without
	// a resolver list.
	ResolverIPs []string

	// Requests, when non-nil, is the SandboxClass's scheduler
	// reservation (SandboxClassSpec.Requests). Each set field replaces
	// the matching request on the workload container, bounded by the
	// Sandbox's limit for that resource. Nil keeps requests equal to
	// limits.
	Requests *setecv1alpha1.ResourceRequests

	// KeepaliveImage is the image that carries the static setec-keepalive
	// binary (cmd/setec-keepalive). A session Sandbox with no
	// spec.command boots that binary instead (setec#7): an init container
	// from this image installs it into a shared volume, and the workload
	// container runs it as its command. Empty is an error for such a
	// Sandbox and is ignored for every other one.
	//
	// The same binary also formats and mounts a kata-fc session's
	// workspace block device (setec#91): every kata-fc session Sandbox,
	// regardless of spec.command, boots this binary first to prepare
	// /workspace, then either execs its own command or falls into the
	// reap loop above. Empty is an error for a kata-fc session.
	KeepaliveImage string
}

// Keepalive injection for a session Sandbox with no spec.command.
const (
	// KeepaliveInitContainerName is the init container that installs the
	// keepalive binary into the shared volume.
	KeepaliveInitContainerName = "setec-keepalive"

	// keepaliveVolumeName is the emptyDir the init container writes and
	// the workload container reads.
	keepaliveVolumeName = "setec-keepalive"

	// keepaliveMountPath is where both containers see that volume.
	keepaliveMountPath = "/setec"

	// KeepalivePath is the workload command a session boots when it
	// declares none. The file name matches cmd/setec-keepalive.
	KeepalivePath = keepaliveMountPath + "/setec-keepalive"
)

// Hardening constants applied to every Sandbox Pod.
const (
	// sandboxUID and sandboxGID are the unprivileged user and group the
	// workload container runs as. 65532 is the conventional
	// "nonroot" ID used by distroless base images.
	sandboxUID int64 = 65532
	sandboxGID int64 = 65532

	// scratchVolumeName is the writable scratch mount that makes a
	// read-only root filesystem usable. Tools that expect to write
	// temporary files get this instead of a writable root.
	scratchVolumeName = "scratch"

	// scratchMountPath is where the scratch volume is mounted.
	scratchMountPath = "/tmp"

	// WorkspaceVolumeName is the Pod volume name for the durable
	// per-session workspace PVC (session lifecycle only, ADR-0006/0007).
	WorkspaceVolumeName = "workspace"

	// WorkspaceMountPath is where the session workspace is mounted
	// inside the microVM. The workload's durable state (worktree,
	// corpus, findings) lives here and survives VM restart and node
	// loss because the backing PVC re-attaches.
	WorkspaceMountPath = "/workspace"

	// WorkspacePVCSuffix is appended to the Sandbox name to derive the
	// workspace PVC name (e.g. Sandbox "foo" → PVC "foo-workspace").
	WorkspacePVCSuffix = "-workspace"

	// workspaceMountVolumeName is the emptyDir a kata-fc session's
	// workload container mounts at /workspace and then, from inside its
	// own command (the keepalive binary), mounts the formatted workspace
	// block device onto (setec#91). Every other backend mounts the
	// workspace PVC directly and never uses this volume.
	workspaceMountVolumeName = "workspace-mount"

	// kataFCWorkspaceDevicePath is where the workspace PVC's raw block
	// device appears inside the workload container, via Kubernetes
	// VolumeDevices. It is a Pod-internal convention: no host or CSI
	// driver has to agree on this path.
	kataFCWorkspaceDevicePath = "/dev/setec-workspace"
)

// WorkspacePVCName derives the deterministic name of the workspace PVC
// owned by the named session Sandbox. Centralised so the controller
// (which creates and deletes the claim) and the builder (which mounts
// it) can never disagree.
func WorkspacePVCName(sandboxName string) string {
	return sandboxName + WorkspacePVCSuffix
}

// sandboxCapabilities are the Linux capabilities added back after
// dropping ALL.
//
// NET_RAW and NET_ADMIN are required by raw-socket network tooling: with
// them dropped, half-open port scanning and packet crafting stop working
// and the product cannot do its job. Per ADR-0052 the containment
// boundary for untrusted execution is the microVM, not the container
// capability set, so re-adding these two costs nothing that the guest
// boundary was not already carrying. Everything else stays dropped.
var sandboxCapabilities = []corev1.Capability{"NET_RAW", "NET_ADMIN"}

// Build transforms a Sandbox custom resource into the corev1.Pod the
// controller must create. The function is pure: it performs no I/O, makes no
// Kubernetes API calls, and does not read or mutate any global state.
//
// runtimeClassName is passed as an argument rather than hard-coded so that a
// cluster operator can rename the RuntimeClass (e.g. "kata-fc" → "kata-qemu")
// without a code change.
//
// The returned Pod has:
//   - metadata.name = "<sandbox-name>-vm"
//   - metadata.namespace mirrors the Sandbox namespace
//   - metadata.labels includes setec.zeroroot.ai/sandbox=<sandbox-name>
//   - metadata.ownerReferences contains a single controller-owning reference
//     back to the Sandbox with BlockOwnerDeletion=true
//   - spec.runtimeClassName = runtimeClassName
//   - spec.restartPolicy = Never (Sandboxes are single-shot)
//   - spec.containers has exactly one entry named "workload" whose image,
//     command, env, and resources mirror the Sandbox spec
//
// Build returns a wrapped error if the Sandbox is structurally invalid in
// ways the OpenAPI schema cannot express. Callers should propagate the error;
// the controller records it as an Event and requeues.
//
// Build is preserved with its Phase 1 signature for back-compat.
// Phase 3 callers that need node pinning go through BuildWithOptions.
func Build(sb *setecv1alpha1.Sandbox, runtimeClassName string) (*corev1.Pod, error) {
	return BuildWithOptions(sb, runtimeClassName, BuildOptions{})
}

// BuildWithOptions is the extended Phase 3 entry point. Build is a
// thin wrapper that passes the zero-value options, so existing
// Phase 1/2 callers are unaffected.
//
// When opts.RuntimeSelection is set it is applied LAST so the dispatcher's
// MutatePod sees the fully-constructed pod. The runtimeClassName argument is
// used as the initial runtime class; RuntimeSelection.Dispatcher.RuntimeClassName()
// overrides it when non-empty.
func BuildWithOptions(sb *setecv1alpha1.Sandbox, runtimeClassName string, opts BuildOptions) (*corev1.Pod, error) {
	// When a RuntimeSelection is provided and its Dispatcher returns a non-empty
	// RuntimeClassName, that value takes precedence over the runtimeClassName arg.
	effectiveRCName := runtimeClassName
	if opts.RuntimeSelection != nil {
		if rcn := opts.RuntimeSelection.Dispatcher.RuntimeClassName(); rcn != "" {
			effectiveRCName = rcn
		}
	}

	if err := validate(sb, effectiveRCName); err != nil {
		return nil, err
	}

	podName := sb.Name + PodNameSuffix

	labels := map[string]string{
		SandboxLabelKey: sb.Name,
	}

	ctrl, bod := true, true
	ownerRef := metav1.OwnerReference{
		APIVersion:         setecv1alpha1.GroupVersion.String(),
		Kind:               sandboxKind,
		Name:               sb.Name,
		UID:                sb.UID,
		Controller:         &ctrl,
		BlockOwnerDeletion: &bod,
	}

	// A session with no command boots the keepalive (setec#7). The
	// decision is made once here so the command, the mount and the init
	// container cannot disagree.
	usesKeepalive := sb.Spec.IsSession() && len(sb.Spec.Command) == 0
	if usesKeepalive && opts.KeepaliveImage == "" {
		return nil, ErrNoKeepaliveImage
	}

	// kata-fc has no virtio-fs (setec#91): the workspace PVC reaches the
	// guest as a raw block device instead of a mounted filesystem, so a
	// session on this backend needs to format and mount it itself before
	// running the caller's actual command. Decided once here, alongside
	// usesKeepalive, so every place that touches the workspace volume
	// agrees on which shape it takes.
	usesBlockWorkspace := sb.Spec.IsSession() &&
		opts.RuntimeSelection != nil && opts.RuntimeSelection.Backend == runtimepkg.BackendKataFC
	if usesBlockWorkspace && opts.KeepaliveImage == "" {
		return nil, ErrNoWorkspaceFormatImage
	}
	// Both usesKeepalive and usesBlockWorkspace boot the same static
	// setec-keepalive binary (as the whole command, or as a wrapper that
	// execs into the Sandbox's own command after preparing the
	// workspace), so both need it installed into the shared volume by
	// the same init container.
	needsKeepaliveBinary := usesKeepalive || usesBlockWorkspace

	container := corev1.Container{
		Name:      ContainerName,
		Image:     sb.Spec.Image,
		Command:   append([]string(nil), sb.Spec.Command...),
		Env:       append([]corev1.EnvVar(nil), sb.Spec.Env...),
		Resources: buildResourceRequirements(sb.Spec.Resources, opts.Requests),
		// A read-only root filesystem needs somewhere to write, or every
		// tool that touches a temporary file fails.
		VolumeMounts: []corev1.VolumeMount{{
			Name:      scratchVolumeName,
			MountPath: scratchMountPath,
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			Privileged:               new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  append([]corev1.Capability(nil), sandboxCapabilities...),
			},
		},
	}

	if usesKeepalive {
		container.Command = []string{KeepalivePath}
	}
	if needsKeepaliveBinary {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      keepaliveVolumeName,
			MountPath: keepaliveMountPath,
			ReadOnly:  true,
		})
	}
	if usesBlockWorkspace {
		// mount(2) needs CAP_SYS_ADMIN, and opening the raw block device
		// needs CAP_DAC_OVERRIDE: the workload runs as the unprivileged
		// sandbox UID/GID (podspec keeps every Sandbox non-root), and a
		// freshly attached Block-mode device node is not guaranteed to
		// be group-writable by that GID on every volume plugin (a real
		// run against a kind e2e cluster's static local PVs hit "open
		// /dev/setec-workspace: permission denied" without it; the
		// device's ownership there never reflects the pod's fsGroup the
		// way a CSI driver's would). These are the two extra
		// capabilities a kata-fc session workload carries beyond every
		// other Sandbox (NET_RAW/NET_ADMIN, above): added, never full
		// `privileged`, because the operator's own admission policy
		// (charts/setec/templates/sandbox-namespace-host-guard.yaml,
		// setec#159) refuses any privileged container in a Sandbox
		// namespace outright — a stricter, non-negotiable line than
		// ADR-0052's "the microVM is the boundary" reasoning, which
		// covers individual capabilities, not full node access.
		//
		// Neither capability reaches the Sandbox's own command: the
		// keepalive wrapper (cmd/setec-keepalive) formats and mounts the
		// workspace, then execs the Sandbox's command via syscall.Exec.
		// Per the Linux capability model, an exec'd binary with no file
		// capabilities of its own starts with an empty effective/
		// permitted set regardless of what the exec'ing process held,
		// unless the process populated its ambient set — which nothing
		// here does. So these capabilities are held only for the
		// format+mount step, never by the workload the Sandbox actually
		// runs.
		container.SecurityContext.Capabilities.Add =
			append(container.SecurityContext.Capabilities.Add, "SYS_ADMIN", "DAC_OVERRIDE")
	}

	rcName := effectiveRCName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       sb.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: &rcName,
			RestartPolicy:    corev1.RestartPolicyNever,
			Containers:       []corev1.Container{container},

			// A Sandbox never calls the Kubernetes API. Mounting the
			// namespace's default ServiceAccount token would hand the
			// workload a cluster credential it has no use for, so the
			// projection is switched off rather than merely unused.
			AutomountServiceAccountToken: new(false),

			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      new(sandboxUID),
				RunAsGroup:     new(sandboxGID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},

			Volumes: []corev1.Volume{{
				Name:         scratchVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}

	if needsKeepaliveBinary {
		pod.Spec.InitContainers = []corev1.Container{keepaliveInstaller(opts.KeepaliveImage)}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: keepaliveVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resource.NewQuantity(16<<20, resource.BinarySI),
			}},
		})
	}

	// Session lifecycle: mount the durable workspace PVC at /workspace.
	// The controller creates the claim before the Pod, so the mount can
	// reference it by its deterministic name. Ephemeral Sandboxes get no
	// workspace volume — their Pod spec is byte-for-byte what it was
	// before the lifecycle field existed.
	if sb.Spec.IsSession() {
		// A freshly provisioned PVC is root-owned; the workload runs as
		// the unprivileged sandbox user. FSGroup makes the kubelet chown
		// the volume on attach so /workspace is writable. Set only for
		// sessions to keep the ephemeral Pod spec unchanged.
		pod.Spec.SecurityContext.FSGroup = new(sandboxGID)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: WorkspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: WorkspacePVCName(sb.Name),
				},
			},
		})
		c := &pod.Spec.Containers[0]
		if usesBlockWorkspace {
			// Firecracker has no virtio-fs (setec#91), so kata cannot
			// share a host directory with the guest: the controller
			// provisions this backend's workspace PVC with
			// volumeMode: Block, and a Block-mode PVC must be consumed
			// via VolumeDevices, never VolumeMounts. The workload
			// container gets the raw device on kataFCWorkspaceDevicePath
			// and an ordinary emptyDir at /workspace; its command is the
			// keepalive binary (see below), which formats and mounts the
			// device onto that emptyDir — all inside this one
			// container's own mount namespace, so no cross-container
			// mount propagation (and the `privileged: true` Kubernetes
			// requires for it) is ever needed.
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name:         workspaceMountVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
			c.VolumeDevices = append(c.VolumeDevices, corev1.VolumeDevice{
				Name:       WorkspaceVolumeName,
				DevicePath: kataFCWorkspaceDevicePath,
			})
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      workspaceMountVolumeName,
				MountPath: WorkspaceMountPath,
			})
			// The keepalive binary formats (once — never reformatting an
			// existing filesystem, internal/workspace.FormatOnce) and
			// mounts the workspace, then either falls into its own
			// reap loop (usesKeepalive: no Sandbox command to hand off
			// to) or execs the Sandbox's own command (setec#91).
			formatArgs := []string{
				KeepalivePath,
				"--format-workspace-device", kataFCWorkspaceDevicePath,
				"--format-workspace-target", WorkspaceMountPath,
				"--format-workspace-uid", strconv.Itoa(int(sandboxUID)),
				"--format-workspace-gid", strconv.Itoa(int(sandboxGID)),
			}
			if !usesKeepalive {
				formatArgs = append(formatArgs, "--")
				formatArgs = append(formatArgs, sb.Spec.Command...)
			}
			c.Command = formatArgs
		} else {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      WorkspaceVolumeName,
				MountPath: WorkspaceMountPath,
			})
		}
		// Root the session in its durable workspace. This is what makes
		// SandboxService.Exec land there: the container runtime's exec
		// primitive takes no working directory, so an exec'd command
		// inherits the container's — and a session's turns must all
		// start in the same place their predecessors left work behind.
		// Ephemeral Sandboxes keep the image's own workdir.
		c.WorkingDir = WorkspaceMountPath
	}

	// Resolve names through the operator-configured resolvers rather than
	// cluster DNS. This is half of the containment pair: the matching
	// NetworkPolicy permits port 53 only to these same addresses, so the
	// workload can neither query nor reach cluster DNS and cannot
	// enumerate in-cluster Services by name.
	if len(opts.ResolverIPs) > 0 {
		pod.Spec.DNSPolicy = corev1.DNSNone
		pod.Spec.DNSConfig = &corev1.PodDNSConfig{
			Nameservers: append([]string(nil), opts.ResolverIPs...),
		}
	}

	if opts.NodeName != "" {
		pod.Spec.NodeName = opts.NodeName
	}

	// Apply the RuntimeSelection LAST so the dispatcher's MutatePod sees the
	// fully-assembled pod (per task-12 requirement: option applied last in pipeline).
	if opts.RuntimeSelection != nil {
		if err := applyRuntimeSelection(pod, opts.RuntimeSelection); err != nil {
			return nil, fmt.Errorf("podspec: apply runtime selection: %w", err)
		}
	}

	return pod, nil
}

// applyRuntimeSelection applies the dispatcher-derived fields to pod:
//  1. RuntimeClassName is already set above (before MutatePod needs it).
//  2. NodeAffinity terms from the dispatcher are MERGED into any existing
//     required affinity terms — not replaced — so caller-provided affinity
//     is preserved.
//  3. Overhead from the dispatcher is set when non-empty.
//  4. MutatePod is called last so dispatchers can see (and depend on) any
//     of the above values.
//
// The params map is extracted from sandbox.Spec if SandboxClass runtime.Params
// were set; since the builder does not have access to the SandboxClass we
// pass nil here — callers that need param propagation should invoke
// sel.Dispatcher.MutatePod directly after BuildWithOptions.
func applyRuntimeSelection(pod *corev1.Pod, sel *runtimepkg.Selection) error {
	// Merge NodeAffinity required terms.
	dispatcherAffinity := sel.Dispatcher.NodeAffinity()
	if dispatcherAffinity != nil &&
		dispatcherAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		if pod.Spec.Affinity == nil {
			pod.Spec.Affinity = &corev1.Affinity{}
		}
		if pod.Spec.Affinity.NodeAffinity == nil {
			pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		}
		// Merge: append dispatcher terms to any existing terms (do not replace).
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
			dispatcherAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms...,
		)
	}

	// Set Overhead when the dispatcher provides it.  Note: Kubernetes validates
	// that Pod.Spec.Overhead exactly matches the RuntimeClass's overhead field.
	// The RuntimeClass must therefore already declare the same overhead values.
	// In clusters where the RuntimeClass does not define overhead (e.g. dev
	// envtest environments), callers should pass an empty BackendConfig.DefaultOverhead
	// so the dispatcher returns nil here.
	if overhead := sel.Dispatcher.Overhead(); len(overhead) > 0 {
		pod.Spec.Overhead = overhead.DeepCopy()
	}

	// MutatePod is called last. The params map is nil here because the builder
	// does not carry SandboxClass.Spec.Runtime.Params; callers needing param
	// propagation should set them via a post-build MutatePod call or by passing
	// them through a future BuildOptions extension.
	if err := sel.Dispatcher.MutatePod(pod, nil); err != nil {
		return fmt.Errorf("dispatcher %q MutatePod: %w", sel.Backend, err)
	}

	return nil
}

// WithRuntimeSelection returns a BuildOptions with RuntimeSelection set to sel.
// It is a convenience constructor for callers that use the functional-option
// style; callers that already have a BuildOptions struct may set the field directly.
func WithRuntimeSelection(sel *runtimepkg.Selection) BuildOptions {
	return BuildOptions{RuntimeSelection: sel}
}

// validate performs the structural checks that the OpenAPI schema cannot
// express. Returning a structured error keeps Build side-effect free.
func validate(sb *setecv1alpha1.Sandbox, runtimeClassName string) error {
	if sb == nil {
		return ErrNilSandbox
	}
	if sb.Name == "" {
		return ErrMissingName
	}
	if runtimeClassName == "" {
		return ErrMissingRuntimeClass
	}
	if sb.Spec.Image == "" {
		return ErrMissingImage
	}
	if len(sb.Spec.Command) == 0 && !sb.Spec.IsSession() {
		return ErrMissingCommand
	}
	if sb.Spec.Resources.VCPU < 1 {
		return fmt.Errorf("%w: got %d", ErrInvalidVCPU, sb.Spec.Resources.VCPU)
	}
	if sb.Spec.Resources.Memory.Sign() <= 0 {
		return fmt.Errorf("%w: got %q", ErrInvalidMemory, sb.Spec.Resources.Memory.String())
	}
	return nil
}

// buildResourceRequirements maps the Sandbox resources block to the
// workload container's limits, and derives its requests. With no class
// reservation the requests equal the limits, so the kubelet guarantees
// the microVM exactly what was asked for. A class reservation replaces
// the request for each resource it names, bounded by that resource's
// limit: a request above its limit is an invalid Pod, so the
// reservation can only ever lower what the scheduler sets aside.
func buildResourceRequirements(r setecv1alpha1.Resources, req *setecv1alpha1.ResourceRequests) corev1.ResourceRequirements {
	cpu := *resource.NewQuantity(int64(r.VCPU), resource.DecimalSI)
	mem := r.Memory.DeepCopy()

	limits := corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: mem,
	}
	requests := limits.DeepCopy()
	if req != nil {
		if req.CPU != nil {
			requests[corev1.ResourceCPU] = minQuantity(*req.CPU, cpu)
		}
		if req.Memory != nil {
			requests[corev1.ResourceMemory] = minQuantity(*req.Memory, mem)
		}
	}

	return corev1.ResourceRequirements{
		Requests: requests,
		Limits:   limits,
	}
}

// keepaliveInstaller is the init container that copies the static
// keepalive binary into the shared volume. It carries the same hardening
// as the workload container and no capabilities: it reads its own
// executable and writes one file. Requests equal limits, so a class with
// no reservation keeps the Pod's Guaranteed QoS.
func keepaliveInstaller(image string) corev1.Container {
	res := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("32Mi"),
	}
	return corev1.Container{
		Name:  KeepaliveInitContainerName,
		Image: image,
		Args:  []string{"--install", keepaliveMountPath},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      keepaliveVolumeName,
			MountPath: keepaliveMountPath,
		}},
		Resources: corev1.ResourceRequirements{Requests: res, Limits: res.DeepCopy()},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			Privileged:               new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// minQuantity returns a copy of the smaller of a and b.
func minQuantity(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) > 0 {
		return b.DeepCopy()
	}
	return a.DeepCopy()
}
