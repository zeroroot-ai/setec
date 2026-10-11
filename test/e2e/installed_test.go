// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zeroroot-ai/setec/internal/chartname"
)

// runInstalled runs the suite against the release in namespace and returns
// the exit code of the run.
func runInstalled(m *testing.M, namespace string) int {
	testNamespace = namespace
	helmReleaseName = os.Getenv("SETEC_E2E_RELEASE")
	if helmReleaseName == "" {
		fmt.Fprintf(os.Stderr, "e2e: %s needs SETEC_E2E_RELEASE, the Helm release of the installed setec\n",
			installedNamespaceEnv)
		return 1
	}
	chartFullname = chartname.Fullname(helmReleaseName)
	sandboxNamespace = os.Getenv("SETEC_E2E_SANDBOX_NAMESPACE")
	if sandboxNamespace == "" || sandboxNamespace == testNamespace {
		fmt.Fprintf(os.Stderr,
			"e2e: %s needs SETEC_E2E_SANDBOX_NAMESPACE, a Sandbox namespace that the installed release lists\n",
			installedNamespaceEnv)
		return 1
	}

	flag.Parse()
	if err := checkInstalledRun(flag.Lookup("test.run").Value.String()); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := buildClient(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to build Kubernetes client: %v\n", err)
		return 1
	}
	if err := requireClusterConsent(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := checkInstalledNamespaces(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr,
		"e2e: %s=%s: the suite installs nothing and runs against the release in %s, with Sandboxes in %s\n",
		installedNamespaceEnv, namespace, testNamespace, sandboxNamespace)
	return m.Run()
}

// checkInstalledNamespaces proves that the release namespace and the Sandbox
// namespace exist. Installed mode never creates or deletes a namespace.
func checkInstalledNamespaces() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{testNamespace, sandboxNamespace} {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
			return fmt.Errorf("installed mode needs the namespace %q on the cluster: %w", name, err)
		}
	}
	return nil
}
