// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"strings"
	"testing"
)

// The operator's client hop to the node-agents is what carries a
// snapshot instruction to a node. These tests pin that its flags reach
// the one credential source intact, and that an incomplete source is a
// startup error that names the missing part.

const nodeAgentID = "spiffe://example.org/ns/setec/sa/setec-node-agent"

func TestNodeAgentCredentialFlags_ReachTheSourceIntact(t *testing.T) {
	t.Parallel()
	other := "spiffe://example.org/ns/setec/sa/setec-node-agent-canary"
	flags := nodeAgentCredentialFlags{
		spiffeSocket:        "unix:///run/spire/agent-sockets/api.sock",
		spiffeAuthorizedIDs: []string{nodeAgentID, other},
	}
	src := flags.source()
	if src.SocketPath != flags.spiffeSocket {
		t.Fatalf("socket = %q, want %q", src.SocketPath, flags.spiffeSocket)
	}
	// Keeping only the last entry would silently narrow the operator to
	// one node-agent, which presents as unreachable nodes rather than
	// as a flag-parsing bug.
	if len(src.AuthorizedIDs) != 2 {
		t.Fatalf("allow-list = %v, want both entries", src.AuthorizedIDs)
	}
}

func TestNodeAgentClientCredentials_RefusesAnIncompleteSource(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flags   nodeAgentCredentialFlags
		wantErr string
	}{
		"no socket":     {nodeAgentCredentialFlags{spiffeAuthorizedIDs: []string{nodeAgentID}}, "socket path is empty"},
		"no allow-list": {nodeAgentCredentialFlags{spiffeSocket: "unix:///run/spire/api.sock"}, "allow-list is empty"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := nodeAgentClientCredentials(t.Context(), tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("nodeAgentClientCredentials = %v, want an error that mentions %q", err, tc.wantErr)
			}
		})
	}
}
