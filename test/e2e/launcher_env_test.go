// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

// The launcher backend of the suite (setec#197). SETEC_E2E_BACKEND=launcher
// installs the release with the launcher runtime as the one backend: every
// Sandbox is a Firecracker machine in a launcher Pod. The scenarios of
// today for a sandbox, a session, exec and egress then run on the launcher
// unchanged, and launcher_test.go adds the abilities that only the
// launcher has.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// backendLauncher is the name of the launcher backend in a SandboxClass.
const backendLauncher = "launcher"

// launcherContainer is the one container of a launcher Pod. Its log is the
// console of the workload.
const launcherContainer = "launcher"

// launcherKVMResource is the device resource of a launcher Pod.
const launcherKVMResource corev1.ResourceName = "setec.zeroroot.ai/kvm"

// sandboxBackend is the backend of every Sandbox of the suite:
// SETEC_E2E_BACKEND, kata-fc by default.
var sandboxBackend string

// onLauncher reports whether the suite runs on the launcher backend.
func onLauncher() bool { return sandboxBackend == backendLauncher }

// launcherConfig is the launcher part of the install.
type launcherConfig struct {
	// imageRepo, diskBuilderImageRepo and devicePluginImageRepo name the
	// images of this build. Each takes the tag of the suite.
	imageRepo             string
	diskBuilderImageRepo  string
	devicePluginImageRepo string
	// diskRepo is the registry repository of the signed disks. The disk
	// builder pushes to it and the kubelet pulls from it.
	diskRepo string
	// warmPoolNamespace holds the warm pool bases. The suite creates it
	// and lists it in sandboxNamespaces.
	warmPoolNamespace string
}

// launcherCfg is the launcher part of the install, read in TestMain.
var launcherCfg launcherConfig

// diskSigningSecret is the Secret of the disk signing seed. The suite makes
// a new key for each run.
const diskSigningSecret = "setec-e2e-disk-seed"

// loadLauncherConfig reads the SETEC_E2E_LAUNCHER_* environment. Each value
// is required on the launcher backend.
func loadLauncherConfig() (launcherConfig, error) {
	c := launcherConfig{
		imageRepo:             os.Getenv("SETEC_E2E_LAUNCHER_IMAGE_REPO"),
		diskBuilderImageRepo:  os.Getenv("SETEC_E2E_DISK_BUILDER_IMAGE_REPO"),
		devicePluginImageRepo: os.Getenv("SETEC_E2E_DEVICE_PLUGIN_IMAGE_REPO"),
		diskRepo:              os.Getenv("SETEC_E2E_DISK_REPO"),
		warmPoolNamespace:     envOr("SETEC_E2E_WARM_POOL_NAMESPACE", testNamespace+"-pool"),
	}
	for name, v := range map[string]string{
		"SETEC_E2E_LAUNCHER_IMAGE_REPO":      c.imageRepo,
		"SETEC_E2E_DISK_BUILDER_IMAGE_REPO":  c.diskBuilderImageRepo,
		"SETEC_E2E_DEVICE_PLUGIN_IMAGE_REPO": c.devicePluginImageRepo,
		"SETEC_E2E_DISK_REPO":                c.diskRepo,
	} {
		if v == "" {
			return c, fmt.Errorf("%s is required with SETEC_E2E_BACKEND=%s", name, backendLauncher)
		}
	}
	return c, nil
}

// createDiskSigningSecret makes a new ed25519 key, stores its seed in the
// release namespace and returns the public key for the chart.
func createDiskSigningSecret(ctx context.Context) (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("disk signing seed: %w", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	sec := &corev1.Secret{StringData: map[string]string{"seed": base64.StdEncoding.EncodeToString(seed)}}
	sec.Name, sec.Namespace = diskSigningSecret, testNamespace
	if err := k8sClient.Create(ctx, sec); err != nil {
		return "", fmt.Errorf("create the disk signing Secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// launcherHelmArgs turns the launcher on as the one backend.
func launcherHelmArgs(publicKey string) []string {
	return []string{
		"--set", "runtimes.kata-fc.enabled=false",
		"--set", "runtimes.launcher.enabled=true",
		"--set", "defaults.runtime.backend=" + backendLauncher,
		"--set-string", "runtimes.launcher.image.repository=" + launcherCfg.imageRepo,
		"--set-string", "runtimes.launcher.image.tag=" + imageTag,
		"--set-string", "runtimes.launcher.diskRepo=" + launcherCfg.diskRepo,
		"--set-string", "runtimes.launcher.diskBuilder.image.repository=" + launcherCfg.diskBuilderImageRepo,
		"--set-string", "runtimes.launcher.diskBuilder.image.tag=" + imageTag,
		"--set-string", "runtimes.launcher.diskBuilder.signingSecret=" + diskSigningSecret,
		"--set-string", "runtimes.launcher.diskBuilder.publicKeys[0]=" + publicKey,
		"--set-string", "runtimes.launcher.warmPool.namespace=" + launcherCfg.warmPoolNamespace,
		"--set-string", "devicePlugin.image.repository=" + launcherCfg.devicePluginImageRepo,
		"--set-string", "devicePlugin.image.tag=" + imageTag,
		"--set", "devicePlugin.image.pullPolicy=" + imagePullPolicy,
	}
}

// launcherImageDigests pins each image of the scenarios by digest. A
// launcher Sandbox needs a digest: its disk belongs to one digest. The
// digest is the one of the image index. The disk builder takes its
// linux/amd64 image.
var launcherImageDigests = map[string]string{
	"busybox:1.36": "docker.io/library/busybox:1.36" +
		"@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
	"docker.io/library/alpine:3.19": "docker.io/library/alpine:3.19" +
		"@sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1",
	"docker.io/library/python:3.12-slim": "docker.io/library/python:3.12-slim" +
		"@sha256:02108f5d322dd89f1c9e552442c25acb0543dfdbc455693a5599624f20d9155d",
}

// testImage returns the image reference a scenario uses: the reference
// itself, or its digest form on the launcher backend.
func testImage(ref string) string {
	if !onLauncher() {
		return ref
	}
	if pinned, ok := launcherImageDigests[ref]; ok {
		return pinned
	}
	return ref
}

// workloadContainer is the container whose log holds the output of the
// workload.
func workloadContainer() string {
	if onLauncher() {
		return launcherContainer
	}
	return "workload"
}

// sandboxCapableNodes returns the schedulable nodes that can run a Sandbox
// of backend. A launcher node offers the KVM device. A node of another
// backend carries the runtime label of the runtime agent.
func sandboxCapableNodes(t *testing.T, backend string) []string {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := k8sClient.List(context.Background(), nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var capable []string
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			continue
		}
		if backend == backendLauncher {
			if q, ok := n.Status.Allocatable[launcherKVMResource]; !ok || q.Cmp(resource.MustParse("1")) < 0 {
				continue
			}
		} else if n.Labels["setec.zeroroot.ai/runtime."+backend] != "true" {
			continue
		}
		capable = append(capable, n.Name)
	}
	return capable
}

// launcherExec runs argv in the guest of a launcher Sandbox and returns its
// output.
func launcherExec(ns, sandbox string, argv ...string) (string, error) {
	args := append([]string{"-n", ns, "exec", sandbox + "-vm", "-c", launcherContainer, "--",
		"/usr/local/bin/setec-launcher", "exec", "--"}, argv...)
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// mustLauncherExec is launcherExec that fails the test on an error.
func mustLauncherExec(t *testing.T, ns, sandbox string, argv ...string) string {
	t.Helper()
	out, err := launcherExec(ns, sandbox, argv...)
	if err != nil {
		t.Fatalf("exec %v in %s/%s: %v (%s)", argv, ns, sandbox, err, out)
	}
	return out
}

// sandboxPod returns the launcher Pod of a Sandbox.
func sandboxPod(t *testing.T, ns, sandbox string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: ns, Name: sandbox + "-vm"}
	if err := k8sClient.Get(context.Background(), key, pod); err != nil {
		t.Fatalf("get the Pod of %s/%s: %v", ns, sandbox, err)
	}
	return pod
}
