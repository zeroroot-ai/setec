// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package tracing

import (
	"strings"
	"testing"
)

// The OTLP hop can be one-way TLS or mTLS, and never both. These
// assert the selection, not the cryptography — internal/credentials
// owns that and tests it against real handshakes.

func TestExporterCredentials_RefusesBothModes(t *testing.T) {
	t.Parallel()
	_, err := exporterCredentials(Config{
		Endpoint:      "collector:4317",
		CAFile:        "/etc/setec/otel-ca.pem",
		SPIFFESocket:  "unix:///run/spire/agent-sockets/api.sock",
		SPIFFEServers: []string{"spiffe://zeroroot.ai/ns/observability/sa/collector"},
	})
	if err == nil {
		t.Fatal("both --otel-ca-file and --otel-spiffe-socket: want a startup error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error = %q, want it to name the conflict", err)
	}
}

// TestExporterCredentials_RefusesSPIFFEWithoutAnAllowList pins that
// "accept any collector" is unreachable by omission, the same way it is
// on every other SPIFFE surface.
func TestExporterCredentials_RefusesSPIFFEWithoutAnAllowList(t *testing.T) {
	t.Parallel()
	_, err := exporterCredentials(Config{
		Endpoint:     "collector:4317",
		SPIFFESocket: "unix:///run/spire/agent-sockets/api.sock",
	})
	if err == nil {
		t.Fatal("SPIFFE mode with no expected collector: want a startup error, got nil")
	}
	if !strings.Contains(err.Error(), "allow-list is empty") {
		t.Fatalf("error = %q, want it to name the empty allow-list", err)
	}
}

// TestExporterCredentials_RefusesSPIFFEWithoutASocket is the other
// mistyped-flag case: an allow-list with nothing to fetch an SVID from.
func TestExporterCredentials_RefusesSPIFFEWithoutASocket(t *testing.T) {
	t.Parallel()
	_, err := exporterCredentials(Config{
		Endpoint:      "collector:4317",
		SPIFFEServers: []string{"spiffe://zeroroot.ai/ns/observability/sa/collector"},
	})
	if err == nil {
		t.Fatal("SPIFFE mode with no Workload API socket: want a startup error, got nil")
	}
	if !strings.Contains(err.Error(), "socket path is empty") {
		t.Fatalf("error = %q, want it to name the missing socket", err)
	}
}

// TestExporterCredentials_DefaultsToOneWayTLS is the acceptance case
// the three refusals are measured against, and pins that the default
// posture did not quietly become "requires SPIRE".
func TestExporterCredentials_DefaultsToOneWayTLS(t *testing.T) {
	t.Parallel()
	creds, err := exporterCredentials(Config{Endpoint: "collector:4317"})
	if err != nil {
		t.Fatalf("default OTLP credentials: %v", err)
	}
	if creds == nil {
		t.Fatal("default OTLP credentials are nil")
	}
}
