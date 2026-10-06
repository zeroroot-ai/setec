// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package launcher

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestTCNetwork_JoinAndLeave runs only in a private network namespace with
// CAP_NET_ADMIN, for example: unshare -rn go test -run TCNetwork ./internal/launcher
// with SETEC_NETNS_TEST=1. It makes a fake Pod interface, joins, checks the
// tap device and both redirects, and checks that Leave removes all of it.
func TestTCNetwork_JoinAndLeave(t *testing.T) {
	if os.Getenv("SETEC_NETNS_TEST") != "1" {
		t.Skip("needs a private network namespace; set SETEC_NETNS_TEST=1 under unshare -rn")
	}
	lo, _ := netlink.LinkByName("lo")
	_ = netlink.LinkSetUp(lo)
	eth := &netlink.Dummy{Name: PodInterface, MTU: 1450}
	if err := netlink.LinkAdd(eth); err != nil {
		t.Fatalf("make the fake Pod interface: %v", err)
	}
	link, _ := netlink.LinkByName(PodInterface)
	addr, _ := netlink.ParseAddr("10.42.0.7/32")
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	gw := net.ParseIP("10.42.0.1")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}, Scope: netlink.SCOPE_LINK}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
		t.Fatal(err)
	}

	pn, err := TCNetwork{}.Join()
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if pn.Address.String() != "10.42.0.7/32" || pn.Gateway.String() != "10.42.0.1" || pn.MTU != 1450 ||
		pn.MAC != link.Attrs().HardwareAddr.String() {
		t.Fatalf("PodNet = %+v", pn)
	}
	tap, err := netlink.LinkByName(TapDevice)
	if err != nil || tap.Attrs().MTU != 1450 {
		t.Fatalf("tap: %v %v", tap, err)
	}
	// Before Connect the machine has no network: no filter exists.
	for _, l := range []netlink.Link{link, tap} {
		if fs, _ := netlink.FilterList(l, netlink.MakeHandle(0xffff, 0)); len(fs) != 0 {
			t.Fatalf("filters of %s before Connect = %v; want none", l.Attrs().Name, fs)
		}
	}
	if err := (TCNetwork{}).Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, l := range []netlink.Link{link, tap} {
		fs, err := netlink.FilterList(l, netlink.MakeHandle(0xffff, 0))
		if err != nil || len(fs) != 1 || fs[0].Type() != "matchall" {
			t.Fatalf("filters of %s = %v, %v; want one matchall redirect", l.Attrs().Name, fs, err)
		}
	}

	if err := (TCNetwork{}).Leave(); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if _, err := netlink.LinkByName(TapDevice); err == nil {
		t.Fatal("the tap device is still there after Leave")
	}
	qs, _ := netlink.QdiscList(link)
	for _, q := range qs {
		if q.Type() == "ingress" {
			t.Fatal("the ingress qdisc of the Pod interface is still there after Leave")
		}
	}
}
