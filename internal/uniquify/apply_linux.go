//go:build linux

/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package uniquify

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// LinuxIdentity is the production IdentityApplier: it rewrites the
// guest's machine identity in place after a snapshot restore.
//
//   - machine-id: /etc/machine-id is overwritten with the directed
//     32-hex value.
//   - boot-id: the kernel's /proc/sys/kernel/random/boot_id is
//     read-only and cloned by the restore; the directed value is
//     written to RunPath and bind-mounted over the proc file — the
//     same mechanism container runtimes use to give containers a
//     private boot id. Any previous bind from an earlier restore is
//     detached first.
//   - hostname: sethostname(2) plus /etc/hostname.
//
// All paths are fields so tests can point them into a tempdir.
type LinuxIdentity struct {
	// MachineIDPath defaults to /etc/machine-id.
	MachineIDPath string
	// HostnamePath defaults to /etc/hostname.
	HostnamePath string
	// BootIDProcPath defaults to /proc/sys/kernel/random/boot_id.
	BootIDProcPath string
	// RunPath is where the fresh boot-id file is materialised before
	// the bind mount. Defaults to /run/setec/boot-id.
	RunPath string
	// Sethostname defaults to unix.Sethostname; injectable for tests.
	Sethostname func([]byte) error
	// BindMount defaults to a MS_BIND mount (with a prior lazy
	// unmount of the target); injectable for tests.
	BindMount func(source, target string) error
	// ReadHostname defaults to os.Hostname; injectable for tests.
	ReadHostname func() (string, error)
}

// NewLinuxIdentity returns a LinuxIdentity with production defaults.
func NewLinuxIdentity() *LinuxIdentity {
	return &LinuxIdentity{
		MachineIDPath:  "/etc/machine-id",
		HostnamePath:   "/etc/hostname",
		BootIDProcPath: "/proc/sys/kernel/random/boot_id",
		RunPath:        "/run/setec/boot-id",
		Sethostname:    unix.Sethostname,
		BindMount:      bindMountOver,
		ReadHostname:   os.Hostname,
	}
}

// bindMountOver bind-mounts source over target, detaching any earlier
// bind on target first so repeated restores stack cleanly.
func bindMountOver(source, target string) error {
	// Best-effort detach of a previous bind; ENOENT/EINVAL mean no
	// prior mount, which is fine.
	_ = unix.Unmount(target, unix.MNT_DETACH)
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s over %s: %w", source, target, err)
	}
	return nil
}

// ApplyMachineID writes the directed machine-id.
func (l *LinuxIdentity) ApplyMachineID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("machine-id must be 32 hex chars, got %d", len(id))
	}
	return os.WriteFile(l.MachineIDPath, []byte(id+"\n"), 0o444)
}

// ApplyBootID materialises the directed boot-id and bind-mounts it
// over the kernel's boot_id proc file.
func (l *LinuxIdentity) ApplyBootID(id string) error {
	if id == "" {
		return errors.New("empty boot-id")
	}
	if err := os.MkdirAll(filepath.Dir(l.RunPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(l.RunPath, []byte(id+"\n"), 0o444); err != nil {
		return err
	}
	return l.BindMount(l.RunPath, l.BootIDProcPath)
}

// ApplyHostname sets the kernel hostname and persists /etc/hostname.
func (l *LinuxIdentity) ApplyHostname(name string) error {
	if name == "" {
		return errors.New("empty hostname")
	}
	if err := l.Sethostname([]byte(name)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	return os.WriteFile(l.HostnamePath, []byte(name+"\n"), 0o644)
}

// Read returns the identity currently observable in the guest. The
// boot-id is read through the proc path so the report proves the bind
// mount actually took effect.
func (l *LinuxIdentity) Read() (machineID, bootID, hostname string, err error) {
	mid, err := os.ReadFile(l.MachineIDPath)
	if err != nil {
		return "", "", "", fmt.Errorf("read machine-id: %w", err)
	}
	bid, err := os.ReadFile(l.BootIDProcPath)
	if err != nil {
		return "", "", "", fmt.Errorf("read boot-id: %w", err)
	}
	host, err := l.ReadHostname()
	if err != nil {
		return "", "", "", fmt.Errorf("read hostname: %w", err)
	}
	return strings.TrimSpace(string(mid)), strings.TrimSpace(string(bid)), host, nil
}

// LinuxNetwork is the production NetworkReconciler. Snapshot restores
// are pinned to the snapshot's node (the Coordinator enforces this),
// so the node-scoped routes and gateway captured in the snapshot stay
// valid — the one piece of network identity that changes per restore
// is the CNI-assigned Pod IP. Reconcile therefore replaces the
// primary interface's IPv4 address with the expected one (via the
// classic SIOCSIFADDR ioctl, which atomically swaps the primary
// address) and reports every global unicast address observed
// afterwards.
type LinuxNetwork struct {
	// Interfaces defaults to net.Interfaces; injectable for tests.
	Interfaces func() ([]net.Interface, error)
	// SetAddr applies ip to the named interface, preserving the
	// interface's existing prefix length. Injectable for tests;
	// defaults to the SIOCSIFADDR/SIOCSIFNETMASK ioctl pair.
	SetAddr func(iface string, ip net.IP, mask net.IPMask) error
}

// NewLinuxNetwork returns a LinuxNetwork with production defaults.
func NewLinuxNetwork() *LinuxNetwork {
	return &LinuxNetwork{
		Interfaces: net.Interfaces,
		SetAddr:    ioctlSetAddr,
	}
}

// Reconcile ensures expectedIP is configured on the guest's primary
// non-loopback interface, replacing a stale snapshot-time address if
// necessary, and returns the global unicast addresses observed after
// the change. An empty expectedIP only reports.
func (l *LinuxNetwork) Reconcile(expectedIP string) ([]string, error) {
	ifaces, err := l.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	if expectedIP != "" {
		want := net.ParseIP(expectedIP)
		if want == nil || want.To4() == nil {
			return nil, fmt.Errorf("expected IP %q is not a valid IPv4 address", expectedIP)
		}
		iface, mask, present, err := primaryInterface(ifaces, want)
		if err != nil {
			return nil, err
		}
		if !present {
			if err := l.SetAddr(iface, want.To4(), mask); err != nil {
				return nil, fmt.Errorf("set %s on %s: %w", expectedIP, iface, err)
			}
			// Re-read so the report reflects the post-change state.
			if ifaces, err = l.Interfaces(); err != nil {
				return nil, fmt.Errorf("re-list interfaces: %w", err)
			}
		}
	}

	return observedGlobalIPs(ifaces), nil
}

// primaryInterface picks the first up, non-loopback interface,
// returning its name, the prefix mask to reuse for the new address
// (the existing primary address's mask, defaulting to /24), and
// whether want is already configured anywhere.
func primaryInterface(ifaces []net.Interface, want net.IP) (name string, mask net.IPMask, present bool, err error) {
	mask = net.CIDRMask(24, 32)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		if name == "" {
			name = iface.Name
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					mask = ipn.Mask
					break
				}
			}
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(want) {
				present = true
			}
		}
	}
	if name == "" {
		return "", nil, false, errors.New("no up non-loopback interface found")
	}
	return name, mask, present, nil
}

// observedGlobalIPs flattens the global unicast addresses across all
// interfaces.
func observedGlobalIPs(ifaces []net.Interface) []string {
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !ipn.IP.IsGlobalUnicast() {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// ioctlSetAddr applies ip/mask as the primary IPv4 address of iface
// using the classic SIOCSIFADDR + SIOCSIFNETMASK ioctls. SIOCSIFADDR
// atomically replaces the interface's primary address, which also
// removes the stale snapshot-time address.
func ioctlSetAddr(iface string, ip net.IP, mask net.IPMask) error {
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("%s is not IPv4", ip)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", iface, err)
	}
	if err := ifr.SetInet4Addr(v4); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, ifr); err != nil {
		return fmt.Errorf("SIOCSIFADDR: %w", err)
	}

	if len(mask) == 4 {
		mfr, err := unix.NewIfreq(iface)
		if err != nil {
			return err
		}
		if err := mfr.SetInet4Addr(net.IP(mask).To4()); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, mfr); err != nil {
			return fmt.Errorf("SIOCSIFNETMASK: %w", err)
		}
	}
	return nil
}

// VsockCID is the production CIDReporter, backed by the
// IOCTL_VM_SOCKETS_GET_LOCAL_CID ioctl on /dev/vsock.
type VsockCID struct{}

// LocalCID returns the guest's local vsock context id.
func (VsockCID) LocalCID() (uint32, error) {
	cid, err := vsock.ContextID()
	if err != nil {
		return 0, fmt.Errorf("vsock context id: %w", err)
	}
	return cid, nil
}
