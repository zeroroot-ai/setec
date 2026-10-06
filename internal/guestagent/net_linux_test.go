// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestLinkConfigurer_TakesThePodIdentity runs in a private network
// namespace: unshare -rn, with SETEC_NETNS_TEST=1. It applies one identity,
// then a second one, as after a snapshot load in a new Pod.
func TestLinkConfigurer_TakesThePodIdentity(t *testing.T) {
	if os.Getenv("SETEC_NETNS_TEST") != "1" {
		t.Skip("needs a private network namespace; set SETEC_NETNS_TEST=1 under unshare -rn")
	}
	if err := netlink.LinkAdd(&netlink.Dummy{Name: "eth0"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	lc := LinkConfigurer{Root: root}
	for _, id := range []Request{
		{MAC: "d6:4f:be:2e:3d:14", MTU: 9001, Address: "10.42.0.138/32", Gateway: "10.42.0.197", DNS: []byte("nameserver 10.43.0.10\n"),
			Hostname: "work-vm"},
		{MAC: "aa:bb:cc:00:11:22", MTU: 1450, Address: "10.42.1.39/32", Gateway: "10.42.1.1"},
	} {
		if err := lc.Configure(id); err != nil {
			t.Fatalf("Configure(%+v): %v", id, err)
		}
		link, _ := netlink.LinkByName("eth0")
		if link.Attrs().HardwareAddr.String() != id.MAC || link.Attrs().MTU != id.MTU {
			t.Fatalf("eth0 mac=%s mtu=%d", link.Attrs().HardwareAddr, link.Attrs().MTU)
		}
		addrs, _ := netlink.AddrList(link, netlink.FAMILY_V4)
		if len(addrs) != 1 || addrs[0].IPNet.String() != id.Address {
			t.Fatalf("addresses = %v, want only %s", addrs, id.Address)
		}
		routes, _ := netlink.RouteList(link, netlink.FAMILY_V4)
		found := false
		for _, r := range routes {
			if (r.Dst == nil || r.Dst.IP.IsUnspecified()) && r.Gw.String() == id.Gateway {
				found = true
			}
		}
		if !found {
			t.Fatalf("no default route via %s in %v", id.Gateway, routes)
		}
	}
	if host, _ := os.ReadFile(filepath.Join(root, "etc/hostname")); string(host) != "work-vm\n" {
		t.Fatalf("/etc/hostname = %q", host)
	}
	raw, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if err != nil || string(raw) != "nameserver 10.43.0.10\n" {
		t.Fatalf("resolv.conf = %q, %v", raw, err)
	}
}
