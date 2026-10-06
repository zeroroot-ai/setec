// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/sandboxid"
)

func identityFixture(t *testing.T) (*IdentitySpec, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	seed := make([]byte, ed25519.SeedSize)
	seed[3] = 9
	keyFile := filepath.Join(dir, "seed")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(seed)), 0o400); err != nil {
		t.Fatal(err)
	}
	spec := &IdentitySpec{SandboxID: "ns/sb/uid-1", Tenant: "acme", KeyFile: keyFile, Generation: 1,
		GenerationFile: filepath.Join(dir, "identity-generation")}
	return spec, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

// TestIdentity_SignsAVerifiableTokenOverTheSocket proves the guest path: a
// request on the identity socket gets a token that verifies with the
// public key of the Sandbox.
func TestIdentity_SignsAVerifiableTokenOverTheSocket(t *testing.T) {
	t.Parallel()
	spec, pub := identityFixture(t)
	signer, err := newIdentitySigner(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "id")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := listenIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	go serveIdentity(t.Context(), l, signer)
	c, err := net.Dial("unix", filepath.Join(dir, VsockSocket+"_"+strconv.Itoa(IdentityPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte(`{"audience":"gibson"}` + "\n"))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp IdentityResponse
	if err := json.Unmarshal(line, &resp); err != nil || resp.Token == "" {
		t.Fatalf("response %s: %v", line, err)
	}
	got, err := sandboxid.Verify(resp.Token, sandboxid.Expect{Key: pub, Subject: "ns/sb/uid-1",
		Audience: "gibson", Generation: 1, Now: time.Now()})
	if err != nil || got.Tenant != "acme" {
		t.Fatalf("Verify = %+v, %v", got, err)
	}
}

// TestIdentity_TakesTheGenerationOfTheSnapshot proves that a token after a
// snapshot carries the generation that the node agent wrote, so a token
// from before the snapshot no longer verifies.
func TestIdentity_TakesTheGenerationOfTheSnapshot(t *testing.T) {
	t.Parallel()
	spec, pub := identityFixture(t)
	signer, err := newIdentitySigner(spec)
	if err != nil {
		t.Fatal(err)
	}
	before := signer.sign("gibson")
	if err := os.WriteFile(spec.GenerationFile, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := signer.sign("gibson")
	want := sandboxid.Expect{Key: pub, Subject: "ns/sb/uid-1", Audience: "gibson", Generation: 2, Now: time.Now()}
	if _, err := sandboxid.Verify(after.Token, want); err != nil {
		t.Fatalf("the token after the snapshot: %v", err)
	}
	if _, err := sandboxid.Verify(before.Token, want); !errors.Is(err, sandboxid.ErrGeneration) {
		t.Fatalf("the token from before the snapshot: %v, want ErrGeneration", err)
	}
	if bad := signer.sign(""); bad.Error == "" || bad.Token != "" {
		t.Fatalf("an empty audience got %+v", bad)
	}
}

// TestIdentity_RefusesAMissingKey proves that a launcher with no key does
// not start a machine with an identity it cannot sign.
func TestIdentity_RefusesAMissingKey(t *testing.T) {
	t.Parallel()
	if _, err := newIdentitySigner(&IdentitySpec{SandboxID: "a/b/c", KeyFile: "/nonexistent/seed"}); err == nil {
		t.Fatal("a missing key file was accepted")
	}
}
