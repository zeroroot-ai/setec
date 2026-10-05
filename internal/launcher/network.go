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
	// Join reads the identity of the Pod interface, makes the tap device
	// and the two filters between them, and returns the identity.
	Join() (PodNet, error)
	// Leave removes the tap device and the filters. It is safe to call
	// after a failed or partial Join.
	Leave() error
}
