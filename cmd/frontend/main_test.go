// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"strings"
	"testing"

	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

const daemonID = "spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon"

// TestCredentialFlags_SelectsAMode covers what an operator can type.
// The file flags keep their meaning and remain the default posture, the
// SPIFFE flags are additive, and the two combinations that must never
// produce a listener — both modes and neither — are refused with a
// message naming the cause.
func TestCredentialFlags_SelectsAMode(t *testing.T) {
	t.Parallel()
	fileFlags := credentialFlags{tlsCert: "c.pem", tlsKey: "k.pem", tlsClientCA: "ca.pem"}
	spiffeFlags := credentialFlags{
		spiffeSocket:        "unix:///run/spire/agent-sockets/api.sock",
		spiffeAuthorizedIDs: repeatedString{daemonID},
	}

	tests := map[string]struct {
		flags    credentialFlags
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
			flags: credentialFlags{
				tlsCert: fileFlags.tlsCert, tlsKey: fileFlags.tlsKey, tlsClientCA: fileFlags.tlsClientCA,
				spiffeSocket:        spiffeFlags.spiffeSocket,
				spiffeAuthorizedIDs: spiffeFlags.spiffeAuthorizedIDs,
			},
			wantMode: conflictingMode,
			wantErr:  "exactly one",
		},
		"no mode": {
			flags:    credentialFlags{},
			wantMode: unsetMode,
			wantErr:  "no credential source",
		},
		// A mistyped flag name leaves its value empty. That must name
		// the missing piece rather than silently selecting the other
		// mode or producing a listener with unintended credentials.
		"file mode with a mistyped client-CA flag": {
			flags:    credentialFlags{tlsCert: "c.pem", tlsKey: "k.pem"},
			wantMode: fileMode,
			wantErr:  "CA path is empty",
		},
		"spiffe mode with a mistyped allow-list flag": {
			flags:    credentialFlags{spiffeSocket: spiffeFlags.spiffeSocket},
			wantMode: spiffeMode,
			wantErr:  "allow-list is empty",
		},
		"spiffe mode with a mistyped socket flag": {
			flags:    credentialFlags{spiffeAuthorizedIDs: repeatedString{daemonID}},
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

// TestRepeatedString_CollectsEveryOccurrence pins the allow-list flag
// being repeatable. Keeping only the last occurrence would silently
// narrow the allow-list to one caller.
func TestRepeatedString_CollectsEveryOccurrence(t *testing.T) {
	t.Parallel()
	var ids repeatedString
	for _, id := range []string{daemonID, "spiffe://zeroroot.ai/ns/gibson/sa/gibson-executor"} {
		if err := ids.Set(id); err != nil {
			t.Fatalf("Set(%q): %v", id, err)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("collected %d IDs (%v), want 2", len(ids), ids)
	}
	if got := ids.String(); !strings.Contains(got, "gibson-daemon") ||
		!strings.Contains(got, "gibson-executor") {
		t.Fatalf("String() = %q, want both IDs", got)
	}
}

// TestSelectResolver_ExactlyOneStrategy pins the tenant → namespace
// strategy selection (setec#158). A fixed shared namespace and the
// label lookup are mutually exclusive; asking for both is refused with
// a message naming the cause rather than silently preferring one, and
// the label strategy stays the default so an install that configures
// nothing keeps today's behaviour.
func TestSelectResolver_ExactlyOneStrategy(t *testing.T) {
	t.Parallel()

	t.Run("default is the label resolver", func(t *testing.T) {
		t.Parallel()
		r, desc, err := selectResolver(nil, "", "setec.zeroroot.ai/tenant", false)
		if err != nil {
			t.Fatalf("selectResolver: %v", err)
		}
		if _, ok := r.(*labelTenantResolver); !ok {
			t.Fatalf("resolver = %T, want *labelTenantResolver", r)
		}
		if !strings.Contains(desc, "setec.zeroroot.ai/tenant") {
			t.Fatalf("desc = %q, want it to name the label key", desc)
		}
	})

	t.Run("sandbox-namespace selects the fixed resolver", func(t *testing.T) {
		t.Parallel()
		r, desc, err := selectResolver(nil, "setec-sandboxes", "setec.zeroroot.ai/tenant", false)
		if err != nil {
			t.Fatalf("selectResolver: %v", err)
		}
		if _, ok := r.(fixedNamespaceResolver); !ok {
			t.Fatalf("resolver = %T, want fixedNamespaceResolver", r)
		}
		if !strings.Contains(desc, "setec-sandboxes") {
			t.Fatalf("desc = %q, want it to name the namespace", desc)
		}
	})

	t.Run("both strategies are refused", func(t *testing.T) {
		t.Parallel()
		_, _, err := selectResolver(nil, "setec-sandboxes", "gibson.zeroroot.ai/tenant", true)
		if err == nil {
			t.Fatal("selectResolver: want an error, got nil")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("error = %q, want it to say the flags are mutually exclusive", err)
		}
	})
}

// TestFixedNamespaceResolver_SameNamespaceForEveryTenant pins the fixed
// resolver returning the configured namespace regardless of tenant, so
// the per-namespace ownership check resolves consistently for every
// authorized caller.
func TestFixedNamespaceResolver_SameNamespaceForEveryTenant(t *testing.T) {
	t.Parallel()
	r := fixedNamespaceResolver("setec-sandboxes")
	for _, tenant := range []string{"team-a", "team-b", ""} {
		ns, err := r.NamespaceFor(t.Context(), tenancy.TenantID(tenant))
		if err != nil {
			t.Fatalf("NamespaceFor(%q): %v", tenant, err)
		}
		if ns != "setec-sandboxes" {
			t.Fatalf("NamespaceFor(%q) = %q, want %q", tenant, ns, "setec-sandboxes")
		}
	}
}
