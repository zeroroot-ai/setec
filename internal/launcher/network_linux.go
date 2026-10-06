// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// PodInterface is the network interface that the CNI gives the Pod.
const PodInterface = "eth0"

// TCNetwork joins the machine with a tap device and two tc filters: each
// frame that arrives on eth0 goes to tap0, and each frame from tap0 goes
// out of eth0. No NAT and no bridge exist. It needs CAP_NET_ADMIN and
// /dev/net/tun, and the node needs the kernel modules sch_ingress,
// cls_matchall and act_mirred.
type TCNetwork struct{}

// Join implements Network.
func (TCNetwork) Join() (PodNet, error) {
	eth, err := netlink.LinkByName(PodInterface)
	if err != nil {
		return PodNet{}, fmt.Errorf("the Pod interface %s: %w", PodInterface, err)
	}
	pn := PodNet{MAC: eth.Attrs().HardwareAddr.String(), MTU: eth.Attrs().MTU}
	addrs, err := netlink.AddrList(eth, unix.AF_INET)
	if err != nil || len(addrs) == 0 {
		return PodNet{}, fmt.Errorf("the IPv4 address of %s: %v", PodInterface, err)
	}
	ip, _ := netip.AddrFromSlice(addrs[0].IP.To4())
	ones, _ := addrs[0].Mask.Size()
	pn.Address = netip.PrefixFrom(ip, ones)
	routes, err := netlink.RouteList(eth, unix.AF_INET)
	if err != nil {
		return PodNet{}, fmt.Errorf("the routes of %s: %w", PodInterface, err)
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			pn.Gateway, _ = netip.AddrFromSlice(r.Gw.To4())
		}
	}
	if !pn.Gateway.IsValid() {
		return PodNet{}, fmt.Errorf("%s has no default route", PodInterface)
	}

	tap := &netlink.Tuntap{
		Name:  TapDevice,
		MTU:   pn.MTU,
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_DEFAULTS | netlink.TUNTAP_NO_PI,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return PodNet{}, fmt.Errorf("make %s: %w", TapDevice, err)
	}
	tapLink, err := netlink.LinkByName(TapDevice)
	if err != nil {
		return PodNet{}, err
	}
	// A tuntap device ignores the MTU of LinkAdd, so it is set here. The
	// machine and the Pod must agree on it, or large frames are lost.
	if err := netlink.LinkSetMTU(tapLink, pn.MTU); err != nil {
		return PodNet{}, fmt.Errorf("set the MTU of %s: %w", TapDevice, err)
	}
	if err := netlink.LinkSetUp(tapLink); err != nil {
		return PodNet{}, fmt.Errorf("set %s up: %w", TapDevice, err)
	}
	return pn, nil
}

// Connect implements Network.
func (TCNetwork) Connect() error {
	eth, err := netlink.LinkByName(PodInterface)
	if err != nil {
		return fmt.Errorf("the Pod interface %s: %w", PodInterface, err)
	}
	tapLink, err := netlink.LinkByName(TapDevice)
	if err != nil {
		return fmt.Errorf("the tap device %s: %w", TapDevice, err)
	}
	if err := redirect(eth, tapLink); err != nil {
		return err
	}
	return redirect(tapLink, eth)
}

// redirect sends each frame that arrives on from out of to.
func redirect(from, to netlink.Link) error {
	q := &netlink.Ingress{
		LinkIndex: from.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_INGRESS,
	}
	if err := netlink.QdiscAdd(q); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("ingress qdisc on %s: %w", from.Attrs().Name, err)
	}
	f := &netlink.MatchAll{
		LinkIndex: from.Attrs().Index, Parent: netlink.MakeHandle(0xffff, 0), Protocol: unix.ETH_P_ALL,
		Actions: []netlink.Action{netlink.NewMirredAction(to.Attrs().Index)},
	}
	if err := netlink.FilterAdd(f); err != nil {
		return fmt.Errorf("redirect %s to %s: %w", from.Attrs().Name, to.Attrs().Name, err)
	}
	return nil
}

// Leave implements Network. Removing the tap device removes its qdisc and
// filter. The ingress qdisc of eth0 goes too, so eth0 is as the CNI left it.
func (TCNetwork) Leave() error {
	var errs []error
	if tap, err := netlink.LinkByName(TapDevice); err == nil {
		errs = append(errs, netlink.LinkDel(tap))
	}
	if eth, err := netlink.LinkByName(PodInterface); err == nil {
		qs, _ := netlink.QdiscList(eth)
		for _, q := range qs {
			if q.Type() == "ingress" {
				errs = append(errs, netlink.QdiscDel(q))
			}
		}
	}
	return errors.Join(errs...)
}
