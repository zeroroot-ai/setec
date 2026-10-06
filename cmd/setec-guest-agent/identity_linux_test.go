// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package main

import "testing"

// TestWorkloadIdentity_GoesToTheRootOfTheImage proves that a launcher
// machine writes the new identity where the workload sees it.
func TestWorkloadIdentity_GoesToTheRootOfTheImage(t *testing.T) {
	id := workloadIdentity(true)
	if id.MachineIDPath != "/newroot/etc/machine-id" || id.HostnamePath != "/newroot/etc/hostname" ||
		id.BootIDProcPath != "/newroot/proc/sys/kernel/random/boot_id" {
		t.Fatalf("identity paths = %+v", id)
	}
	if workloadIdentity(false).MachineIDPath != "/etc/machine-id" {
		t.Fatal("outside a launcher machine the identity moved")
	}
}
