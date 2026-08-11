/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"strings"
	"testing"

	"github.com/zeroroot-ai/setec/internal/credentials"
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
