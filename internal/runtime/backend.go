// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package runtime names the one isolation backend of setec: each Sandbox is
// a Firecracker machine in a launcher Pod (docs/design/runtime.md). The
// backends kata-fc, kata-qemu, gvisor and runc left setec in one cutover
// (setec#198). A class that still names one is refused with a clear
// message, never run on another isolation.
package runtime

import "fmt"

// BackendLauncher is the one backend.
const BackendLauncher = "launcher"

// RemovedBackends are the backend names that setec no longer runs.
var RemovedBackends = []string{"kata-fc", "kata-qemu", "gvisor", "runc"}

// ValidateBackend accepts the launcher and an empty name, which means the
// launcher. It refuses each other name, with the reason for a removed one.
func ValidateBackend(name string) error {
	if name == "" || name == BackendLauncher {
		return nil
	}
	for _, removed := range RemovedBackends {
		if name == removed {
			return fmt.Errorf("the backend %q was removed: each sandbox is a Firecracker machine in a "+
				"launcher pod (setec#198); set runtime.backend to %q or leave it empty", name, BackendLauncher)
		}
	}
	return fmt.Errorf("%q is not a backend: the one backend is %q", name, BackendLauncher)
}
