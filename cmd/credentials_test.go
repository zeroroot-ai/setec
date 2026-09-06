// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroroot-ai/setec/internal/credentials"
)

// The operator's client hop to the node-agents is what carries a
// snapshot instruction to a node. Its flags had no test, and a
// half-configured credential used to surface as an "open :" error from
// deep inside crypto/tls.
//
// The table below is deliberately the same table cmd/frontend and
// cmd/node-agent assert against. "Failure semantics match the server
// slices" is the acceptance criterion; asserting the same cases is how
// it stays true.

const nodeAgentID = "spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent"

func TestNodeAgentCredentialFlags_SelectsAMode(t *testing.T) {
	t.Parallel()
	fileFlags := nodeAgentCredentialFlags{certPath: "c.pem", keyPath: "k.pem", caPath: "ca.pem"}
	spiffeFlags := nodeAgentCredentialFlags{
		spiffeSocket:        "unix:///run/spire/agent-sockets/api.sock",
		spiffeAuthorizedIDs: []string{nodeAgentID},
	}

	tests := map[string]struct {
		flags    nodeAgentCredentialFlags
		wantMode string
		wantErr  string
	}{
		"file mode": {
			flags:    fileFlags,
			wantMode: fileMode,
		},
		"spiffe mode": {
			flags:    spiffeFlags,
			wantMode: spiffeMode,
		},
		"both modes": {
			flags: nodeAgentCredentialFlags{
				certPath: fileFlags.certPath, keyPath: fileFlags.keyPath, caPath: fileFlags.caPath,
				spiffeSocket:        spiffeFlags.spiffeSocket,
				spiffeAuthorizedIDs: spiffeFlags.spiffeAuthorizedIDs,
			},
			wantMode: conflictingMode,
			wantErr:  "exactly one",
		},
		"no mode": {
			flags:    nodeAgentCredentialFlags{},
			wantMode: unsetMode,
			wantErr:  "no credential source",
		},
		"file mode with a mistyped CA flag": {
			flags:    nodeAgentCredentialFlags{certPath: "c.pem", keyPath: "k.pem"},
			wantMode: fileMode,
			wantErr:  "CA path is empty",
		},
		"spiffe mode with a mistyped allow-list flag": {
			flags:    nodeAgentCredentialFlags{spiffeSocket: spiffeFlags.spiffeSocket},
			wantMode: spiffeMode,
			wantErr:  "allow-list is empty",
		},
		"spiffe mode with a mistyped socket flag": {
			flags:    nodeAgentCredentialFlags{spiffeAuthorizedIDs: []string{nodeAgentID}},
			wantMode: spiffeMode,
			wantErr:  "socket path is empty",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, mode := tc.flags.config()
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			_, err := credentials.New(cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("credentials.New: %v", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("credentials.New: want an error mentioning %q, got nil", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestNodeAgentCredentialFlags_FileFlagsReachTheSourceIntact(t *testing.T) {
	t.Parallel()
	flags := nodeAgentCredentialFlags{certPath: "c.pem", keyPath: "k.pem", caPath: "ca.pem"}
	cfg, mode := flags.config()
	if mode != fileMode {
		t.Fatalf("mode = %q, want %q", mode, fileMode)
	}
	if cfg.Files == nil {
		t.Fatal("complete file flags produced no file credential source")
	}
	if cfg.Files.CertFile != "c.pem" || cfg.Files.KeyFile != "k.pem" || cfg.Files.CAFile != "ca.pem" {
		t.Fatalf("flags reached the credential source scrambled: %+v", *cfg.Files)
	}
}

func TestNodeAgentCredentialFlags_SPIFFEFlagsReachTheSourceIntact(t *testing.T) {
	t.Parallel()
	other := "spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent-canary"
	flags := nodeAgentCredentialFlags{
		spiffeSocket:        "unix:///run/spire/agent-sockets/api.sock",
		spiffeAuthorizedIDs: []string{nodeAgentID, other},
	}
	cfg, mode := flags.config()
	if mode != spiffeMode {
		t.Fatalf("mode = %q, want %q", mode, spiffeMode)
	}
	if cfg.SPIFFE == nil {
		t.Fatal("SPIFFE flags produced no SPIFFE credential source")
	}
	if cfg.SPIFFE.SocketPath != flags.spiffeSocket {
		t.Fatalf("socket = %q, want %q", cfg.SPIFFE.SocketPath, flags.spiffeSocket)
	}
	// Keeping only the last entry would silently narrow the operator to
	// one node-agent, which presents as unreachable nodes rather than
	// as a flag-parsing bug.
	if len(cfg.SPIFFE.AuthorizedIDs) != 2 {
		t.Fatalf("allow-list = %v, want both entries", cfg.SPIFFE.AuthorizedIDs)
	}
}

func TestNodeAgentClientCredentials_MissingKeypairNamesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, mode, err := nodeAgentClientCredentials(t.Context(), nodeAgentCredentialFlags{
		certPath: filepath.Join(dir, "absent.crt"),
		keyPath:  filepath.Join(dir, "absent.key"),
		caPath:   filepath.Join(dir, "absent-ca.pem"),
	})
	if err == nil {
		t.Fatal("want error for an absent keypair, got nil")
	}
	if mode != fileMode {
		t.Fatalf("mode = %q, want %q", mode, fileMode)
	}
	if !strings.Contains(err.Error(), "absent.crt") {
		t.Fatalf("error = %q, want it to name the missing certificate file", err)
	}
}
