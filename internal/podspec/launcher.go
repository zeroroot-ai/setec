// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder"
)

// The launcher Pod (docs/design/runtime.md). One setec container starts a
// Firecracker machine inside the Pod. The device plugin
// (cmd/setec-device-plugin) hands it /dev/kvm and /dev/net/tun, so the Pod
// is not privileged and mounts nothing from the host.
const (
	// LauncherContainerName is the one container of a launcher Pod.
	LauncherContainerName = "launcher"

	// KVMResource and TunResource are the extended resources of the device
	// plugin.
	KVMResource corev1.ResourceName = "setec.zeroroot.ai/kvm"
	TunResource corev1.ResourceName = "setec.zeroroot.ai/tun"

	// LauncherWorkVolume holds the machine files: the API socket, the
	// writable layer and snapshot files. The node agent finds it on the
	// node by this name.
	LauncherWorkVolume    = "work"
	LauncherWorkMountPath = "/work"
	// LauncherVMDir is the work directory of Firecracker inside the Pod.
	// The node agent reaches the API socket and the vsock socket here.
	LauncherVMDir = LauncherWorkMountPath + "/vm"
	// LauncherRestoreDir holds the snapshot files that the node agent
	// stages for a restore.
	LauncherRestoreDir = LauncherVMDir + "/restore"
)

// The files of a launcher Pod that the node agent and the launcher share,
// as the Pod sees them. internal/launcher holds the same socket names, and
// a test keeps the two equal.
const (
	LauncherAPISocket   = "api.sock"
	LauncherVsockSocket = "v.sock"
	// LauncherWritableDisk is the writable layer of the machine. A
	// snapshot holds it with the memory.
	LauncherWritableDisk = LauncherWorkMountPath + "/writable.ext4"
	// A restore: the node agent writes the state, the memory and the
	// writable layer, then the staged marker. The launcher loads them,
	// confirms the guest, and writes the evidence file, which the node
	// agent reads.
	LauncherRestoreState    = LauncherRestoreDir + "/state.bin"
	LauncherRestoreMemory   = LauncherRestoreDir + "/memory.bin"
	LauncherRestoreStaged   = LauncherRestoreDir + "/staged"
	LauncherRestoreEvidence = LauncherRestoreDir + "/evidence.json"
)

const (

	// launcherDiskVolume is the image volume of the signed disk of the
	// image digest. The kubelet pulls it on the node, so the launcher
	// reaches no registry through the Pod network.
	launcherDiskVolume    = "disk"
	launcherDiskMountPath = "/disk"
)

// LauncherCapability is the one capability of a launcher Pod. The launcher
// makes a tap device and two tc filters that join the machine to the Pod
// network interface. Proven in setec#181: the Pod needs nothing else.
const LauncherCapability corev1.Capability = "NET_ADMIN"

// dropAllCapabilities is the capability name that drops every capability.
const dropAllCapabilities corev1.Capability = "ALL"

// LauncherOptions carries what a launcher Pod needs beyond the Sandbox.
type LauncherOptions struct {
	// Image is the launcher image. Required.
	Image string
	// Scratch is the size limit of the writable layer of the machine.
	// Zero takes the default scratch size of the Sandbox.
	Scratch resource.Quantity
	// DiskRepo is the repository of the signed image disks
	// (setec-disk-builder). Required.
	DiskRepo string
	// DiskKeys are the base64 ed25519 public keys that may sign a disk.
	// Required: the launcher refuses a disk that no key signed.
	DiskKeys []string
	// ResolverIPs are the DNS servers of the Pod. The launcher gives the
	// resolv.conf of the Pod to the machine.
	ResolverIPs []string
	// Restore makes a Pod whose launcher loads a snapshot that the node
	// agent stages in the work volume, instead of a boot (setec#105).
	Restore bool
	// Base makes the Pod of a warm pool base: a boot with no workload
	// that the node agent snapshots (setec#103). BuildLauncherBase sets it.
	Base bool
	// FromBase makes a Pod whose launcher loads a warm pool base that the
	// node agent stages, and then starts the workload of the Sandbox
	// (setec#103). It implies the snapshot source of Restore.
	FromBase bool
	// NodeName, when set, pins the Pod to that node: the node that holds a
	// snapshot on its local disk.
	NodeName string
	// InstanceType, when set, keeps the Pod on nodes of that
	// node.kubernetes.io/instance-type: a restore with no CPU template
	// needs the CPU of its source.
	InstanceType string
	// CPUTemplate names the Firecracker custom CPU template of the class,
	// a file of the launcher image. Empty shows the guest the CPU of the
	// node.
	CPUTemplate string
}

// LauncherWorkspaceDevice is the raw block device of the session workspace
// in a launcher Pod.
const LauncherWorkspaceDevice = "/dev/setec-workspace"

// LauncherCPUTemplateDir holds the custom CPU templates in the launcher
// image, one <name>.json each.
const LauncherCPUTemplateDir = "/opt/setec/cpu-templates"

// LauncherSpecEnv is the environment variable that carries the launcher
// spec (internal/launcher.Spec) as JSON.
const LauncherSpecEnv = "SETEC_LAUNCHER_SPEC"

// The fixed paths inside a launcher Pod. The launcher image holds the
// kernel and the initrd with the guest agent.
const (
	launcherKernel   = "/opt/setec/vmlinux"
	launcherInitrd   = "/opt/setec/initrd.cpio"
	launcherBootArgs = "console=ttyS0 reboot=k panic=1 pci=off setec.lowerfs=squashfs"
)

// launcherSpec is the JSON of internal/launcher.Spec. It is written here
// rather than imported, so the operator does not link the launcher.
type launcherSpec struct {
	VCPU            int              `json:"vcpu"`
	MemoryMiB       int64            `json:"memoryMiB"`
	ImageRef        string           `json:"imageRef"`
	DiskKeys        []string         `json:"diskKeys"`
	ImageDisk       string           `json:"imageDisk"`
	DiskSignature   string           `json:"diskSignature"`
	CPUTemplate     string           `json:"cpuTemplate,omitempty"`
	WorkspaceDevice string           `json:"workspaceDevice,omitempty"`
	Base            bool             `json:"base,omitempty"`
	WritableDisk    string           `json:"writableDisk"`
	WritableBytes   int64            `json:"writableBytes"`
	WorkDir         string           `json:"workDir"`
	Source          launcherSource   `json:"source"`
	Workload        *launcherProcess `json:"workload,omitempty"`
}

type launcherSource struct {
	Boot     *launcherBoot     `json:"boot,omitempty"`
	Snapshot *launcherSnapshot `json:"snapshot,omitempty"`
}

type launcherSnapshot struct {
	State    string `json:"state"`
	Memory   string `json:"memory"`
	Staged   string `json:"staged"`
	Evidence string `json:"evidence"`
}

type launcherBoot struct {
	Kernel   string `json:"kernel"`
	Initrd   string `json:"initrd"`
	BootArgs string `json:"bootArgs"`
}

type launcherProcess struct {
	Argv []string `json:"argv,omitempty"`
	Env  []string `json:"env,omitempty"`
}

// BuildLauncher returns the launcher Pod of sb. The guest memory and vCPUs
// come from the Sandbox resources. The Pod runs as root, because the one
// capability is effective only for root without ambient capabilities, and
// root holds no other capability here.
func BuildLauncher(sb *setecv1alpha1.Sandbox, opts LauncherOptions) (*corev1.Pod, error) {
	if sb == nil {
		return nil, ErrNilSandbox
	}
	if sb.Name == "" {
		return nil, ErrMissingName
	}
	if opts.Image == "" {
		return nil, fmt.Errorf("podspec: the launcher image is empty")
	}
	if opts.DiskRepo == "" || len(opts.DiskKeys) == 0 {
		return nil, fmt.Errorf("podspec: the disk repository or the disk keys are empty")
	}
	if !strings.Contains(sb.Spec.Image, "@sha256:") {
		return nil, fmt.Errorf("podspec: a launcher Sandbox needs an image with a digest, got %q", sb.Spec.Image)
	}
	diskRef, err := diskbuilder.DiskRef(opts.DiskRepo, sb.Spec.Image)
	if err != nil {
		return nil, fmt.Errorf("podspec: the disk of %q: %w", sb.Spec.Image, err)
	}
	if sb.Spec.Resources.VCPU < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidVCPU, sb.Spec.Resources.VCPU)
	}
	if sb.Spec.Resources.Memory.Sign() <= 0 {
		return nil, fmt.Errorf("%w: got %q", ErrInvalidMemory, sb.Spec.Resources.Memory.String())
	}

	one := resource.MustParse("1")
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(sb.Spec.Resources.VCPU), resource.DecimalSI),
		corev1.ResourceMemory: sb.Spec.Resources.Memory.DeepCopy(),
		KVMResource:           one.DeepCopy(),
		TunResource:           one.DeepCopy(),
	}
	work := opts.Scratch.DeepCopy()
	if work.IsZero() {
		work = resource.MustParse("10Gi")
	}
	// The writable layer takes the scratch size. The emptyDir holds it and
	// the machine files, so it gets 2 GiB more.
	workDir := work.DeepCopy()
	workDir.Add(resource.MustParse("2Gi"))
	spec := launcherSpec{
		VCPU:          int(sb.Spec.Resources.VCPU),
		MemoryMiB:     sb.Spec.Resources.Memory.Value() >> 20,
		ImageRef:      sb.Spec.Image,
		DiskKeys:      opts.DiskKeys,
		ImageDisk:     launcherDiskMountPath + "/" + diskbuilder.DiskFile,
		DiskSignature: launcherDiskMountPath + "/" + diskbuilder.SignatureFile,
		WritableDisk:  LauncherWritableDisk,
		WritableBytes: work.Value(),
		WorkDir:       LauncherVMDir,
	}
	if opts.CPUTemplate != "" {
		spec.CPUTemplate = LauncherCPUTemplateDir + "/" + opts.CPUTemplate + ".json"
	}
	// A session keeps its workspace on its PVC, a raw block device that
	// the machine mounts at /workspace (setec#193).
	if sb.Spec.IsSession() {
		spec.WorkspaceDevice = LauncherWorkspaceDevice
	}
	if opts.Restore || opts.FromBase {
		spec.Source.Snapshot = &launcherSnapshot{
			State:    LauncherRestoreState,
			Memory:   LauncherRestoreMemory,
			Staged:   LauncherRestoreStaged,
			Evidence: LauncherRestoreEvidence,
		}
	} else {
		spec.Source.Boot = &launcherBoot{Kernel: launcherKernel, Initrd: launcherInitrd, BootArgs: launcherBootArgs}
	}
	// A loaded Sandbox snapshot already runs its workload, and a base runs
	// none. A boot and the load of a base start the workload of the Sandbox.
	if !opts.Restore && !opts.Base {
		spec.Workload = &launcherProcess{Argv: append([]string(nil), sb.Spec.Command...)}
		for _, e := range sb.Spec.Env {
			if e.ValueFrom == nil {
				spec.Workload.Env = append(spec.Workload.Env, e.Name+"="+e.Value)
			}
		}
	}
	spec.Base = opts.Base
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		Name:      sb.Name + PodNameSuffix,
		Namespace: sb.Namespace,
		Labels:    map[string]string{SandboxLabelKey: sb.Name},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         setecv1alpha1.GroupVersion.String(),
			Kind:               sandboxKind,
			Name:               sb.Name,
			UID:                sb.UID,
			Controller:         new(true),
			BlockOwnerDeletion: new(true),
		}},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  LauncherContainerName,
				Image: opts.Image,
				Env:   []corev1.EnvVar{{Name: LauncherSpecEnv, Value: string(specJSON)}},
				Resources: corev1.ResourceRequirements{
					Limits:   limits,
					Requests: limits.DeepCopy(),
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                new(int64(0)),
					RunAsNonRoot:             new(false),
					AllowPrivilegeEscalation: new(false),
					ReadOnlyRootFilesystem:   new(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{dropAllCapabilities},
						Add:  []corev1.Capability{LauncherCapability},
					},
				},
				// The Sandbox is Running only once the guest agent answers
				// (internal/status: a Pod that declares readiness stays
				// Pending until it is Ready).
				ReadinessProbe: &corev1.Probe{
					Exec: &corev1.ExecAction{
						Command: []string{"/usr/local/bin/setec-launcher", "ready"},
					},
					PeriodSeconds: 1, TimeoutSeconds: 3, FailureThreshold: 3,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: LauncherWorkVolume, MountPath: LauncherWorkMountPath},
					{Name: launcherDiskVolume, MountPath: launcherDiskMountPath, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name:     LauncherWorkVolume,
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workDir},
				},
				{
					Name: launcherDiskVolume,
					Image: &corev1.ImageVolumeSource{
						Reference: diskRef, PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
					}}}},
				},
			}},
		},
	}
	term := &pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
	if sb.Spec.IsSession() {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: WorkspaceVolumeName,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: WorkspacePVCName(sb.Name),
			},
		})
		pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{{
			Name: WorkspaceVolumeName, DevicePath: LauncherWorkspaceDevice,
		}}
	}
	if opts.NodeName != "" {
		term.MatchFields = []corev1.NodeSelectorRequirement{{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{opts.NodeName},
		}}
	}
	if opts.InstanceType != "" {
		term.MatchExpressions = append(term.MatchExpressions, corev1.NodeSelectorRequirement{
			Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{opts.InstanceType},
		})
	}
	if len(opts.ResolverIPs) > 0 {
		pod.Spec.DNSPolicy = corev1.DNSNone
		pod.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: append([]string(nil), opts.ResolverIPs...)}
	}
	return pod, nil
}

// RestoreEvidence is what the launcher writes to LauncherRestoreEvidence
// after a snapshot load. Each field is true only when the guest agent
// confirmed the step.
type RestoreEvidence struct {
	EntropyReseeded bool   `json:"entropyReseeded"`
	Uniquified      bool   `json:"uniquified"`
	ClockSet        bool   `json:"clockSet"`
	Error           string `json:"error,omitempty"`
}

// BaseLabel marks the launcher Pods and the Snapshots of the warm pool.
const BaseLabel = "setec.zeroroot.ai/base"

// BuildLauncherBase returns the launcher Pod that boots a warm pool base:
// the image and the default resources of a class, no workload, no owner
// Sandbox. The Pod lives in the namespace of the operator.
func BuildLauncherBase(name, namespace, image string, res setecv1alpha1.Resources, opts LauncherOptions) (*corev1.Pod, error) {
	sb := &setecv1alpha1.Sandbox{Name: name, Namespace: namespace}
	sb.Spec.Image = image
	sb.Spec.Resources = res
	opts.Base, opts.Restore, opts.FromBase = true, false, false
	pod, err := BuildLauncher(sb, opts)
	if err != nil {
		return nil, err
	}
	pod.Name = name
	pod.OwnerReferences = nil
	pod.Labels = map[string]string{BaseLabel: "true"}
	return pod, nil
}
