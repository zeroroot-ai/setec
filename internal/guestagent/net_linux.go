// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LinkConfigurer gives eth0 of the machine the identity of the Pod: MAC,
// MTU, address and default route, and writes the resolv.conf of the Pod
// into the root of the image. After a snapshot load in a new Pod it runs
// again with the new identity (proof 4, setec#183).
type LinkConfigurer struct {
	Root string
}

// Configure implements NetConfigurer.
func (l LinkConfigurer) Configure(req Request) error {
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("eth0: %w", err)
	}
	mac, err := net.ParseMAC(req.MAC)
	if err != nil {
		return fmt.Errorf("mac: %w", err)
	}
	addr, err := netlink.ParseAddr(req.Address)
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}
	gw := net.ParseIP(req.Gateway)
	if gw == nil {
		return fmt.Errorf("gateway %q is not an address", req.Gateway)
	}
	_ = netlink.LinkSetDown(link)
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return fmt.Errorf("set the mac: %w", err)
	}
	if req.MTU > 0 {
		if err := netlink.LinkSetMTU(link, req.MTU); err != nil {
			return fmt.Errorf("set the mtu: %w", err)
		}
	}
	old, _ := netlink.AddrList(link, netlink.FAMILY_V4)
	for i := range old {
		_ = netlink.AddrDel(link, &old[i])
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("add the address: %w", err)
	}
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}, Scope: netlink.SCOPE_LINK,
	}); err != nil {
		return fmt.Errorf("route to the gateway: %w", err)
	}
	if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
		return fmt.Errorf("default route: %w", err)
	}
	if req.Hostname != "" {
		if err := l.setHostname(req.Hostname); err != nil {
			return err
		}
	}
	if len(req.DNS) > 0 {
		etc := filepath.Join(l.Root, "etc")
		if err := os.MkdirAll(etc, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), req.DNS, 0o644); err != nil { //nolint:gosec // world-readable, as resolv.conf is
			return err
		}
	}
	return nil
}

// setHostname gives the machine the name of the Pod: the kernel name, when
// the configurer runs in the machine, and /etc/hostname.
func (l LinkConfigurer) setHostname(name string) error {
	if l.Root == "" || l.Root == "/" {
		if err := unix.Sethostname([]byte(name)); err != nil {
			return fmt.Errorf("set the hostname: %w", err)
		}
	}
	etc := filepath.Join(l.Root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(etc, "hostname"), []byte(name+"\n"), 0o644) //nolint:gosec // world-readable, as /etc/hostname is
}
