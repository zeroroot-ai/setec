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

	// launcherWorkVolume holds the machine files: the API socket, the
	// writable layer and snapshot files.
	launcherWorkVolume    = "work"
	launcherWorkMountPath = "/work"
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
	// ResolverIPs are the DNS servers of the Pod. The launcher gives the
	// resolv.conf of the Pod to the machine.
	ResolverIPs []string
}

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
	VCPU          int             `json:"vcpu"`
	MemoryMiB     int64           `json:"memoryMiB"`
	ImageRef      string          `json:"imageRef"`
	DiskRepo      string          `json:"diskRepo"`
	ImageDisk     string          `json:"imageDisk"`
	WritableDisk  string          `json:"writableDisk"`
	WritableBytes int64           `json:"writableBytes"`
	WorkDir       string          `json:"workDir"`
	Source        launcherSource  `json:"source"`
	Workload      launcherProcess `json:"workload"`
}

type launcherSource struct {
	Boot launcherBoot `json:"boot"`
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
	if opts.DiskRepo == "" {
		return nil, fmt.Errorf("podspec: the disk repository is empty")
	}
	if !strings.Contains(sb.Spec.Image, "@sha256:") {
		return nil, fmt.Errorf("podspec: a launcher Sandbox needs an image with a digest, got %q", sb.Spec.Image)
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
	// The writable layer takes the scratch size; the emptyDir holds it, the
	// downloaded image disk and the machine files, so it gets 2 GiB more.
	workDir := work.DeepCopy()
	workDir.Add(resource.MustParse("2Gi"))
	spec := launcherSpec{
		VCPU:          int(sb.Spec.Resources.VCPU),
		MemoryMiB:     sb.Spec.Resources.Memory.Value() >> 20,
		ImageRef:      sb.Spec.Image,
		DiskRepo:      opts.DiskRepo,
		ImageDisk:     launcherWorkMountPath + "/image.sqfs",
		WritableDisk:  launcherWorkMountPath + "/writable.ext4",
		WritableBytes: work.Value(),
		WorkDir:       launcherWorkMountPath + "/vm",
		Source:        launcherSource{Boot: launcherBoot{Kernel: launcherKernel, Initrd: launcherInitrd, BootArgs: launcherBootArgs}},
		Workload:      launcherProcess{Argv: append([]string(nil), sb.Spec.Command...)},
	}
	for _, e := range sb.Spec.Env {
		if e.ValueFrom == nil {
			spec.Workload.Env = append(spec.Workload.Env, e.Name+"="+e.Value)
		}
	}
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
				VolumeMounts: []corev1.VolumeMount{{Name: launcherWorkVolume, MountPath: launcherWorkMountPath}},
			}},
			Volumes: []corev1.Volume{{
				Name:     launcherWorkVolume,
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workDir},
			}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
					}}}},
				},
			}},
		},
	}
	if len(opts.ResolverIPs) > 0 {
		pod.Spec.DNSPolicy = corev1.DNSNone
		pod.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: append([]string(nil), opts.ResolverIPs...)}
	}
	return pod, nil
}
