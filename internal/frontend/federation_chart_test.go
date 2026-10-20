// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderFederation renders templates/federation.yaml with the values and
// returns the output and the helm error, if any.
func renderFederation(t *testing.T, sets ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("this test renders the chart and needs helm on PATH: %v", err)
	}
	args := make([]string, 0, 17+2*len(sets))
	args = append(args, "template", "setec", filepath.Join("..", "..", "charts", "setec"),
		"-f", filepath.Join("..", "..", "hack", "chart-launcher-values.yaml"),
		"--set", "webhook.certManager.enabled=true",
		"--set", "frontend.enabled=true",
		"--set", "credentials.spiffe.trustDomain=fleet.test",
		"--set", "systemPolicy.frontendCallers[0].namespace=gibson",
		"--set", `systemPolicy.frontendCallers[0].podLabels.app\.kubernetes\.io/component=daemon`,
		"--show-only", "templates/federation.yaml")
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(helm, args...) //nolint:gosec // fixed arguments
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &renderError{msg: errb.String()}
	}
	return out.String(), nil
}

type renderError struct{ msg string }

func (e *renderError) Error() string { return e.msg }

// TestFederationChart_ForeignClientGetsItsBundle checks setec#251 and
// a client in another trust domain gets one
// ClusterFederatedTrustDomain, with the domain taken from its SPIFFE ID
// and the bundle source from its federation block. A client in the trust
// domain of the fleet gets none.
func TestFederationChart_ForeignClientGetsItsBundle(t *testing.T) {
	t.Parallel()
	out, err := renderFederation(t,
		"frontend.clients[0].name=local",
		"frontend.clients[0].spiffePath=platform/daemon",
		"frontend.clients[1].name=outside",
		"frontend.clients[1].spiffeID=spiffe://install-b.test/ns/gibson/sa/daemon",
		"frontend.clients[1].federation.bundleEndpointURL=https://spire.install-b.test:8443",
		"frontend.clients[1].federation.endpointSPIFFEID=spiffe://install-b.test/spire/server")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: ClusterFederatedTrustDomain",
		`trustDomain: "install-b.test"`,
		`bundleEndpointURL: "https://spire.install-b.test:8443"`,
		"type: https_spiffe",
		`endpointSPIFFEID: "spiffe://install-b.test/spire/server"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "kind: ClusterFederatedTrustDomain"); n != 1 {
		t.Errorf("got %d ClusterFederatedTrustDomain objects, want 1 (the local client needs none)", n)
	}
	if strings.Contains(out, "fleet.test") {
		t.Errorf("the own trust domain is in the federation render:\n%s", out)
	}
}

// TestFederationChart_RefusesAnIncompleteForeignClient is the failing
// fixture: a foreign client with no usable bundle source fails the render.
func TestFederationChart_RefusesAnIncompleteForeignClient(t *testing.T) {
	t.Parallel()
	base := []string{
		"frontend.clients[0].name=outside",
		"frontend.clients[0].spiffeID=spiffe://install-b.test/ns/gibson/sa/daemon",
	}
	for _, tc := range []struct {
		name string
		sets []string
		want string
	}{
		{"no bundle source", nil, "needs federation.bundleEndpointURL"},
		{"bad profile", []string{
			"frontend.clients[0].federation.bundleEndpointURL=https://x.test",
			"frontend.clients[0].federation.bundleEndpointProfile=http",
		}, "must be https_spiffe or https_web"},
		{"https_spiffe with no endpoint ID", []string{
			"frontend.clients[0].federation.bundleEndpointURL=https://x.test",
		}, "endpointSPIFFEID is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := renderFederation(t, append(append([]string{}, base...), tc.sets...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("render error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestFederationChart_GlobalTrustDomainWins checks that the trust domain of
// a parent chart (global.spire.trustDomain) decides which client is foreign,
// before credentials.spiffe.trustDomain does.
func TestFederationChart_GlobalTrustDomainWins(t *testing.T) {
	t.Parallel()
	sets := []string{
		"frontend.clients[0].name=same",
		"frontend.clients[0].spiffeID=spiffe://global.test/ns/gibson/sa/daemon",
		"frontend.clients[1].name=outside",
		"frontend.clients[1].spiffeID=spiffe://install-b.test/ns/gibson/sa/daemon",
		"frontend.clients[1].federation.bundleEndpointURL=https://spire.install-b.test:8443",
		"frontend.clients[1].federation.endpointSPIFFEID=spiffe://install-b.test/spire/server",
	}
	out, err := renderFederation(t, append([]string{"global.spire.trustDomain=global.test"}, sets...)...)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "kind: ClusterFederatedTrustDomain"); n != 1 || !strings.Contains(out, `trustDomain: "install-b.test"`) {
		t.Errorf("want one federation for install-b.test, got:\n%s", out)
	}
	// Without the global value, credentials.spiffe.trustDomain (fleet.test)
	// makes the client of global.test foreign, and it has no bundle source.
	if _, err := renderFederation(t, sets...); err == nil || !strings.Contains(err.Error(), "needs federation.bundleEndpointURL") {
		t.Fatalf("render error = %v, want the missing bundle source of the client in global.test", err)
	}
}
