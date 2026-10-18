// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package e2e

// This file has no build tag: the unit test of the -run guard of installed
// mode runs with the unit tests of the module, with no cluster.

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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

// installedScenarioImages is the image of the Sandboxes of each installed
// scenario. An installed release takes a launcher image only from the
// registries of its launcher.imageSources, and staging and prod list only
// ghcr.io. So the image is a mirror of the platform on ghcr.io, pinned by
// the digest of its image index with no tag: the disk builder refuses a
// reference that names both (setec#239).
var installedScenarioImages = map[string]string{
	// The mirror of alpine 3.21 that the platform chart also uses.
	"TestLauncher_CPUTemplateSnapshotLoads": "ghcr.io/zeroroot-ai/mirror/alpine" +
		"@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d",
}

// installedImageRegistry is the one registry that every installed release
// allows (the default of launcher.imageSources in the setec chart).
const installedImageRegistry = "ghcr.io"

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

// TestInstalledScenarioImagesAreAllowed: each installed scenario has an
// image, and the image comes from the registry that each installed release
// allows, by digest with no tag. A Docker Hub image failed the disk build of
// the hosted exit test with "the registry of the image is not in
// --image-registry: index.docker.io" (setec#239).
func TestInstalledScenarioImagesAreAllowed(t *testing.T) {
	for name := range installedScenarios {
		image, ok := installedScenarioImages[name]
		if !ok {
			t.Errorf("%s has no image in installedScenarioImages", name)
			continue
		}
		repo, digest, found := strings.Cut(image, "@")
		if !found || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			t.Errorf("%s: image %q is not pinned by a sha256 digest", name, image)
		}
		if strings.Contains(repo[strings.LastIndex(repo, "/")+1:], ":") {
			t.Errorf("%s: image %q names a tag and a digest, and the disk builder refuses that", name, image)
		}
		if registry, _, _ := strings.Cut(repo, "/"); registry != installedImageRegistry {
			t.Errorf("%s: image %q comes from %q, and an installed release allows only %q",
				name, image, registry, installedImageRegistry)
		}
	}
}

// TestInstalledImageRegistryIsTheChartDefault: installedImageRegistry is a
// registry that the setec chart allows by default, so an install with the
// default launcher.imageSources can run the installed scenarios.
func TestInstalledImageRegistryIsTheChartDefault(t *testing.T) {
	raw, err := os.ReadFile("../../charts/setec/values.yaml")
	if err != nil {
		t.Fatalf("read the values of the chart: %v", err)
	}
	var values struct {
		Launcher struct {
			ImageSources []struct {
				Registry string `json:"registry"`
			} `json:"imageSources"`
		} `json:"launcher"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse the values of the chart: %v", err)
	}
	registries := make([]string, 0, len(values.Launcher.ImageSources))
	for _, src := range values.Launcher.ImageSources {
		registries = append(registries, src.Registry)
	}
	if !slices.Contains(registries, installedImageRegistry) {
		t.Fatalf("the chart allows %v by default, not %q", registries, installedImageRegistry)
	}
}
