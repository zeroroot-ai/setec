// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

// Fixed names inside Spec.WorkDir and inside the machine.
const (
	// APISocket is the Firecracker API socket.
	APISocket = "api.sock"
	// VsockSocket is the host side of the vsock device. Firecracker
	// connects a guest call to host port P to VsockSocket + "_P".
	VsockSocket = "v.sock"
	// GuestCID is the vsock address of the guest.
	GuestCID = 3
	// ExitPort is the vsock port on which the guest agent reports the exit
	// of the workload.
	ExitPort = 5300
	// TapDevice is the tap device that joins the machine to the Pod.
	TapDevice = "tap0"

	defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

	// The disks appear in WorkDir under fixed names, links to the real
	// files. A snapshot records these names, so a snapshot of one launcher
	// loads in any other launcher (proof 4, setec#183).
	imageLink     = "image.disk"
	writableLink  = "writable.disk"
	workspaceLink = "workspace.disk"
)

// vmConfig is the Firecracker configuration of a boot (--config-file).
type vmConfig struct {
	BootSource        bootSource  `json:"boot-source"`
	Drives            []drive     `json:"drives"`
	MachineConfig     machine     `json:"machine-config"`
	NetworkInterfaces []netIface  `json:"network-interfaces"`
	Vsock             vsockDevice `json:"vsock"`
}

type bootSource struct {
	Kernel     string `json:"kernel_image_path"`
	InitrdPath string `json:"initrd_path,omitempty"`
	BootArgs   string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machine struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type netIface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

type vsockDevice struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// drives returns the disks of s in device order: vda the image, vdb the
// writable layer, vdc the workspace of a session.
func (s *Spec) drives() []drive {
	out := []drive{
		{DriveID: "image", PathOnHost: imageLink, IsReadOnly: true},
		{DriveID: "writable", PathOnHost: writableLink},
	}
	if s.WorkspaceDevice != "" {
		out = append(out, drive{DriveID: "workspace", PathOnHost: workspaceLink})
	}
	return out
}

// bootConfig is the configuration of a boot of s. The machine takes the MAC
// of the Pod interface, so the Pod network sees one endpoint.
func (s *Spec) bootConfig(guestMAC string) vmConfig {
	args := s.Source.Boot.BootArgs
	if args == "" {
		args = defaultBootArgs
	}
	return vmConfig{
		BootSource: bootSource{
			Kernel:     s.Source.Boot.Kernel,
			InitrdPath: s.Source.Boot.Initrd,
			BootArgs:   args,
		},
		Drives:            s.drives(),
		MachineConfig:     machine{VCPUCount: s.VCPU, MemSizeMiB: s.MemoryMiB},
		NetworkInterfaces: []netIface{{IfaceID: "eth0", HostDevName: TapDevice, GuestMAC: guestMAC}},
		Vsock:             vsockDevice{GuestCID: GuestCID, UDSPath: VsockSocket},
	}
}
