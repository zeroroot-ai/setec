// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLauncher answers one identity request with the audience in the token.
func fakeLauncher(t *testing.T) func() (net.Conn, error) {
	t.Helper()
	return func() (net.Conn, error) {
		guest, host := net.Pipe()
		go func() {
			defer func() { _ = host.Close() }()
			line, _ := bufio.NewReader(host).ReadBytes('\n')
			var req struct{ Audience string }
			_ = json.Unmarshal(line, &req)
			resp := TokenResponse{Token: "token-for-" + req.Audience, Expires: 42}
			if req.Audience == "" {
				resp = TokenResponse{Error: "no audience"}
			}
			raw, _ := json.Marshal(resp)
			_, _ = host.Write(append(raw, '\n'))
		}()
		return guest, nil
	}
}

// TestServeIdentity_GivesEachUserAToken proves the workload path: the
// socket under the image root is open to each user, and a GET returns the
// token of the launcher.
func TestServeIdentity_GivesEachUserAToken(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("", "idroot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = ServeIdentity(ctx, root, &IdentityProxy{Dial: fakeLauncher(t)}) }()
	sock := filepath.Join(root, IdentitySocket)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil {
			if fi.Mode().Perm() != 0o666 {
				t.Fatalf("socket mode = %v, want 0666", fi.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the identity socket did not appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	get := func(q string) (int, TokenResponse) {
		t.Helper()
		resp, err := hc.Get("http://identity" + IdentityTokenPath + q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var tr TokenResponse
		_ = json.NewDecoder(resp.Body).Decode(&tr)
		return resp.StatusCode, tr
	}
	if code, tr := get("?audience=gibson"); code != http.StatusOK || tr.Token != "token-for-gibson" {
		t.Fatalf("token = %d %+v", code, tr)
	}
	if code, tr := get(""); code != http.StatusBadRequest || !strings.Contains(tr.Error, "audience") {
		t.Fatalf("no audience = %d %+v", code, tr)
	}
}

// TestSupervisor_EnvAddsTheIdentitySocket proves that a started process
// learns the path of the socket, and that a process that sets the key
// keeps its own value.
func TestSupervisor_EnvAddsTheIdentitySocket(t *testing.T) {
	s := testSupervisor(t)
	s.Env = []string{IdentitySocketEnv + "=" + IdentitySocket}
	out := filepath.Join(t.TempDir(), "env")
	null, _ := os.Open(os.DevNull)
	defer func() { _ = null.Close() }()
	for _, p := range []Process{
		{Argv: []string{"sh", "-c", "echo $" + IdentitySocketEnv + " >> " + out}},
		{Argv: []string{"sh", "-c", "echo $" + IdentitySocketEnv + " >> " + out}, Env: []string{IdentitySocketEnv + "=/own"}},
	} {
		run, err := s.Start(p, null, null, null)
		if err != nil {
			t.Fatal(err)
		}
		if code := run.Wait(); code != 0 {
			t.Fatalf("exit = %d", code)
		}
	}
	raw, _ := os.ReadFile(out)
	if got := strings.Fields(string(raw)); len(got) != 2 || got[0] != IdentitySocket || got[1] != "/own" {
		t.Fatalf("the processes saw %q", got)
	}
}
