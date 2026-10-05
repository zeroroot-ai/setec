// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	spiffeFlags := credentialFlags{spiffeSocket: "unix:///run/spire/agent-sockets/api.sock"}
	enrolled := []string{daemonID}

	tests := map[string]struct {
		flags    credentialFlags
		enrolled []string
		wantMode string
		wantErr  string
	}{
		"file mode": {
			flags:    fileFlags,
			wantMode: fileMode,
		},
		"spiffe mode": {
			flags:    spiffeFlags,
			enrolled: enrolled,
			wantMode: spiffeMode,
		},
		"both modes": {
			flags: credentialFlags{
				tlsCert: fileFlags.tlsCert, tlsKey: fileFlags.tlsKey, tlsClientCA: fileFlags.tlsClientCA,
				spiffeSocket: spiffeFlags.spiffeSocket,
			},
			enrolled: enrolled,
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
		// The allow-list is the enrolled clients. ParseEnrollment refuses
		// an empty list first, and credentials.New refuses it again.
		"spiffe mode with no enrolled client": {
			flags:    spiffeFlags,
			wantMode: spiffeMode,
			wantErr:  "allow-list is empty",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, mode := tc.flags.config(tc.enrolled)
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

// TestLabelPairResolver_ExactlyOneNamespacePerPair pins the namespace rule of
// ADR-0142: one namespace for each pair, found by both labels. A namespace
// with the tenant label only, or with the labels of a different client, is
// never the namespace of the pair.
func TestLabelPairResolver_ExactlyOneNamespacePerPair(t *testing.T) {
	t.Parallel()
	ns := func(name string, labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	pairA, _ := tenancy.NewPair("cluster-a", "acme")
	pairB, _ := tenancy.NewPair("cluster-b", "acme")
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		ns("sbx-a-acme", pairA.Labels()),
		ns("tenant-only", map[string]string{tenancy.TenantLabelKey: "acme"}),
		ns("sbx-b-acme-1", pairB.Labels()),
		ns("sbx-b-acme-2", pairB.Labels()),
	).Build()
	r := &labelPairResolver{client: c}

	got, err := r.NamespaceFor(t.Context(), pairA)
	if err != nil || got != "sbx-a-acme" {
		t.Fatalf("pair A: namespace = %q, %v; want sbx-a-acme", got, err)
	}
	if _, err := r.NamespaceFor(t.Context(), pairB); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("pair B has two namespaces: error = %v, want a refusal", err)
	}
	pairC, _ := tenancy.NewPair("cluster-c", "acme")
	if _, err := r.NamespaceFor(t.Context(), pairC); err == nil || !strings.Contains(err.Error(), "no namespace") {
		t.Fatalf("pair C has no namespace: error = %v, want a refusal", err)
	}
}
