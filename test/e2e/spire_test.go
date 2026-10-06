// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// setec has one credential source: the SPIFFE Workload API (setec#175).
// The operator dials each node-agent with its SVID, and the node-agent
// authorizes the operator by SPIFFE ID. So a suite with snapshots on
// installs SPIRE first, from the hardened charts that the platform pins,
// and gives the chart the socket and the two allow-lists.
const (
	spireRepo         = "https://spiffe.github.io/helm-charts-hardened"
	spireChartVersion = "0.28.4"
	spireCRDsVersion  = "0.5.0"
	spireNamespace    = "spire-mgmt"
	// e2eTrustDomain is the trust domain of the suite's SPIRE.
	e2eTrustDomain = "example.org"
	// spireAgentSocket is the Workload API socket of the hardened chart's
	// agent on each node.
	spireAgentSocket = "/run/spire/agent-sockets/spire-agent.sock"
)

// installSPIRE installs the SPIRE CRDs, server, agent and controller
// manager, and waits for them. It is idempotent, so a second call (the
// Phase 3 upgrade test) is cheap. The default ClusterSPIFFEID of the
// chart gives each Pod spiffe://<trust domain>/ns/<namespace>/sa/<sa>.
func installSPIRE(ctx context.Context) error {
	steps := [][]string{
		{"upgrade", "--install", "spire-crds", "spire-crds", "--repo", spireRepo,
			"--version", spireCRDsVersion, "--namespace", spireNamespace, "--create-namespace", "--wait"},
		{"upgrade", "--install", "spire", "spire", "--repo", spireRepo,
			"--version", spireChartVersion, "--namespace", spireNamespace,
			"--set", "global.spire.trustDomain=" + e2eTrustDomain,
			"--set", "global.spire.clusterName=setec-e2e",
			"--set", "global.spire.namespaces.create=true",
			"--wait", "--timeout", "10m"},
	}
	for _, args := range steps {
		out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("helm %s: %w\n%s", strings.Join(args[:3], " "), err, out)
		}
	}
	return nil
}

// spireHelmArgs are the chart values of the SPIFFE credential source for
// the release fullname in namespace: the socket of the agent, the trust
// domain, and the SPIFFE IDs of the operator and the node-agent.
func spireHelmArgs(fullname, namespace string) []string {
	id := func(sa string) string {
		return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", e2eTrustDomain, namespace, sa)
	}
	return []string{
		"--set", "credentials.spiffe.socketPath=" + spireAgentSocket,
		"--set", "credentials.spiffe.trustDomain=" + e2eTrustDomain,
		"--set", "credentials.spiffe.authorizedIDs.nodeAgentClients={" + id(fullname) + "}",
		"--set", "credentials.spiffe.authorizedIDs.nodeAgentServers={" + id(fullname+"-node-agent") + "}",
	}
}
