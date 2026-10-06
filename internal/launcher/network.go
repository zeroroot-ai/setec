// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import "net/netip"

// PodNet is the network identity of the Pod. The machine takes all of it:
// the guest agent sets the address, the MAC, the MTU and the route, so
// the traffic of the machine is the traffic of the Pod endpoint, and the
// network policy of the Pod applies to it (proof 3, setec#182).
type PodNet struct {
	Address netip.Prefix `json:"address"`
	MAC     string       `json:"mac"`
	MTU     int          `json:"mtu"`
	Gateway netip.Addr   `json:"gateway"`
}

// Network joins the machine to the Pod interface and removes the join.
type Network interface {
	// Join reads the identity of the Pod interface, makes the tap device,
	// and returns the identity. The machine has no network yet.
	Join() (PodNet, error)
	// Connect adds the two filters between the Pod interface and the tap
	// device. From then on each frame of the Pod goes to the machine.
	Connect() error
	// Leave removes the tap device and the filters. It is safe to call
	// after a failed or partial Join.
	Leave() error
}
