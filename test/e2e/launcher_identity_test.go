// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// identityAudience is the audience of the tokens of the scenario.
const identityAudience = "setec-e2e"

// tokenScript gets an identity token from the identity socket of the
// machine, with the Python of the image: the guest needs no HTTP client.
const tokenScript = `import json, os, socket
s = socket.socket(socket.AF_UNIX)
s.connect(os.environ["SETEC_IDENTITY_SOCKET"])
s.sendall(b"GET /v1/token?audience=` + identityAudience + ` HTTP/1.0\r\n\r\n")
raw = b""
while True:
    b = s.recv(65536)
    if not b:
        break
    raw += b
print(json.loads(raw.split(b"\r\n\r\n", 1)[1])["token"])`

// guestToken gets a token in the guest of a Sandbox.
func guestToken(t *testing.T, name string) string {
	t.Helper()
	tok := mustLauncherExec(t, sandboxNamespace, name, "python3", "-c", tokenScript)
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("%s: the identity socket gave %q", name, tok)
	}
	return tok
}

// verifyToken asks the frontend which Sandbox a token names.
func verifyToken(t *testing.T, tok string) (string, error) {
	t.Helper()
	resp, err := newInProcessFrontend().VerifySandboxIdentity(context.Background(),
		&setecv1grpc.VerifySandboxIdentityRequest{Token: tok, Audience: identityAudience})
	return resp.GetSandboxId(), err
}

func idOf(t *testing.T, name string) string {
	t.Helper()
	sb, err := getSandboxE2E(types.NamespacedName{Namespace: sandboxNamespace, Name: name})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return sandboxNamespace + "/" + name + "/" + string(sb.UID)
}

// TestLauncher_SandboxIdentity proves setec#235 on the launcher: each
// Sandbox gets a token that verifies as itself; a snapshot ends each token
// from before it; two forks of one snapshot get different identities; and
// a fork cannot present the token of its source that it finds in its copy
// of the source.
func TestLauncher_SandboxIdentity(t *testing.T) {
	requireLauncher(t)
	src := launcherSandbox("id-src", "sleep 3600")
	src.Spec.Image = testImage("docker.io/library/python:3.12-slim")
	createAndCleanup(t, src)
	waitRunning(t, src, defaultWait)
	srcID := idOf(t, src.Name)

	before := guestToken(t, src.Name)
	if got, err := verifyToken(t, before); err != nil || got != srcID {
		t.Fatalf("the token of the source = %q, %v; want %q", got, err, srcID)
	}
	// The source keeps the token in its memory and on its disk, so the
	// snapshot holds it.
	mustLauncherExec(t, sandboxNamespace, src.Name, "sh", "-c", "echo "+before+" > /tmp/token")

	takeSnapshot(t, src, setecv1alpha1.SandboxSnapshotSpec{Name: "id-snap", Forkable: true,
		TTL: &metav1.Duration{Duration: 30 * time.Minute}})
	if _, err := verifyToken(t, before); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a token from before the snapshot = %v, want Unauthenticated", err)
	}
	if got, err := verifyToken(t, guestToken(t, src.Name)); err != nil || got != srcID {
		t.Fatalf("a token of the source after the snapshot = %q, %v", got, err)
	}

	forks := []string{"id-fork-1", "id-fork-2"}
	for _, n := range forks {
		restoreFrom(t, n, "id-snap", func(s *setecv1alpha1.SandboxSpec) {
			s.Image = testImage("docker.io/library/python:3.12-slim")
		})
	}
	seen := map[string]string{}
	for _, n := range forks {
		waitForPhase(t, client.ObjectKey{Namespace: sandboxNamespace, Name: n}, launcherRestoreWait,
			setecv1alpha1.SandboxPhaseRunning)
		copied := mustLauncherExec(t, sandboxNamespace, n, "cat", "/tmp/token")
		if copied != before {
			t.Fatalf("%s: the copy of the source token is %q", n, copied)
		}
		if got, err := verifyToken(t, copied); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s presented the token of its source: %q, %v", n, got, err)
		}
		own, err := verifyToken(t, guestToken(t, n))
		if err != nil || own != idOf(t, n) || own == srcID {
			t.Errorf("%s: its own token = %q, %v", n, own, err)
		}
		if other, dup := seen[own]; dup {
			t.Errorf("%s and %s share the identity %q", n, other, own)
		}
		seen[own] = n
	}
}
