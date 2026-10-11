// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package e2e

// This file has no build tag: the unit test of the -run guard of installed
// mode runs with the unit tests of the module, with no cluster.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// installedNamespaceEnv names the namespace of a setec release that is
// already on the cluster. When it is set, the suite installs nothing: it runs
// the scenarios that installedScenarios names against that release, and it
// creates and deletes only the objects of those scenarios.
//
// This mode is for a cluster that the suite does not own, such as a staging
// cluster. A second release there would run a second KVM device plugin on
// each node, and the kubelet keeps one plugin for each resource name, so the
// uninstall of the second release would take the device away from the
// release that owns the cluster.
const installedNamespaceEnv = "SETEC_E2E_INSTALLED_NAMESPACE"

// installedScenarios are the scenarios that may run against an installed
// release. Each one makes its own SandboxClass and Sandboxes, and deletes
// them. A scenario that restarts the operator, changes the release or needs a
// suite-owned namespace is not in this list.
var installedScenarios = map[string]bool{
	"TestLauncher_CPUTemplateSnapshotLoads": true,
}

// installedRunPattern is the one -run shape that installed mode accepts:
// ^Name$ or ^(Name|Name)$.
var installedRunPattern = regexp.MustCompile(`^\^\(?([A-Za-z0-9_|]+)\)?\$$`)

// checkInstalledRun refuses a -run pattern that could select a scenario that
// is not in installedScenarios. An empty pattern runs every scenario, so it
// is refused too.
func checkInstalledRun(run string) error {
	allowed := make([]string, 0, len(installedScenarios))
	for name := range installedScenarios {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	usage := fmt.Sprintf("%s runs only these scenarios, named exactly, for example -run '^%s$': %s",
		installedNamespaceEnv, allowed[0], strings.Join(allowed, ", "))

	match := installedRunPattern.FindStringSubmatch(run)
	if match == nil {
		return fmt.Errorf("refusing -run %q: %s", run, usage)
	}
	for name := range strings.SplitSeq(match[1], "|") {
		if !installedScenarios[name] {
			return fmt.Errorf("refusing -run %q: %s is not safe against an installed release. %s", run, name, usage)
		}
	}
	return nil
}

func TestCheckInstalledRun(t *testing.T) {
	for _, tc := range []struct {
		run string
		ok  bool
	}{
		{"^TestLauncher_CPUTemplateSnapshotLoads$", true},
		{"^(TestLauncher_CPUTemplateSnapshotLoads)$", true},
		{"", false},
		{".", false},
		{"TestLauncher_CPUTemplateSnapshotLoads", false},
		{"^TestLauncher_.*$", false},
		{"^TestSandbox_OperatorRestartMidRun$", false},
		{"^(TestLauncher_CPUTemplateSnapshotLoads|TestPhase3_UpgradeFromPhase2)$", false},
	} {
		err := checkInstalledRun(tc.run)
		if (err == nil) != tc.ok {
			t.Errorf("checkInstalledRun(%q) = %v, want ok=%v", tc.run, err, tc.ok)
		}
	}
}
