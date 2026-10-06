// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

// operator_helpers_test.go finds the leading operator Pod.

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// operatorLeaseName is the operator's leader-election Lease
// (cmd/main.go LeaderElectionID).
const operatorLeaseName = "setec.zeroroot.ai"

// operatorLeaderPod returns the name of the operator Pod that holds the
// leader-election Lease, the one replica that reconciles.
func operatorLeaderPod(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "get", "lease", operatorLeaseName,
		"-n", testNamespace, "-o", "jsonpath={.spec.holderIdentity}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read the operator leader Lease %s/%s: %v (%s)", testNamespace, operatorLeaseName, err, out)
	}
	// controller-runtime writes the holder as <pod name>_<uuid>.
	holder := strings.TrimSpace(string(out))
	pod, _, found := strings.Cut(holder, "_")
	if !found || pod == "" {
		return "", fmt.Errorf("operator leader Lease %s/%s has holder %q, want <pod>_<uuid>", testNamespace, operatorLeaseName, holder)
	}
	return pod, nil
}
