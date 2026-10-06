// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The identity socket (setec#235). A process in the machine gets an
// identity token of its Sandbox with GET /v1/token?audience=<verifier> on
// this Unix socket. The guest agent asks the launcher for each token: the
// launcher signs it with a key that no process in the machine can read.
const (
	// IdentitySocket is the path of the socket, as the workload sees it.
	IdentitySocket = "/run/setec/identity.sock"
	// IdentitySocketEnv names the socket in the environment of each
	// process that the guest agent starts.
	IdentitySocketEnv = "SETEC_IDENTITY_SOCKET"
	// IdentityTokenPath is the HTTP path of a token.
	IdentityTokenPath = "/v1/token"
)

// TokenResponse is the answer on the identity socket. Expires is the end
// of the lifetime in Unix seconds.
type TokenResponse struct {
	Token   string `json:"token,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	Error   string `json:"error,omitempty"`
}

// IdentityProxy answers the identity socket. Dial opens a connection to
// the identity port of the launcher.
type IdentityProxy struct {
	Dial func() (net.Conn, error)
}

// ServeHTTP gets one token from the launcher for each request.
func (p *IdentityProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || r.URL.Path != IdentityTokenPath {
		writeToken(w, http.StatusNotFound, TokenResponse{Error: "GET " + IdentityTokenPath + "?audience=<verifier>"})
		return
	}
	resp, err := p.token(r.URL.Query().Get("audience"))
	switch {
	case err != nil:
		writeToken(w, http.StatusBadGateway, TokenResponse{Error: err.Error()})
	case resp.Error != "":
		writeToken(w, http.StatusBadRequest, resp)
	default:
		writeToken(w, http.StatusOK, resp)
	}
}

func (p *IdentityProxy) token(audience string) (TokenResponse, error) {
	c, err := p.Dial()
	if err != nil {
		return TokenResponse{}, fmt.Errorf("reach the launcher: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req, err := json.Marshal(struct {
		Audience string `json:"audience"`
	}{audience})
	if err != nil {
		return TokenResponse{}, err
	}
	if _, err := c.Write(append(req, '\n')); err != nil {
		return TokenResponse{}, fmt.Errorf("ask the launcher: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(c, 16<<10)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return TokenResponse{}, fmt.Errorf("read the launcher: %w", err)
	}
	var resp TokenResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return TokenResponse{}, fmt.Errorf("the launcher answered no JSON: %w", err)
	}
	return resp, nil
}

func writeToken(w http.ResponseWriter, code int, resp TokenResponse) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// ServeIdentity serves the identity socket under root until ctx ends.
// Each user of the machine may connect: the identity is the one of the
// Sandbox, not of a process.
func ServeIdentity(ctx context.Context, root string, proxy *IdentityProxy) error {
	path := filepath.Join(root, IdentitySocket)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the workload user reads it
		return fmt.Errorf("make the identity socket directory: %w", err)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on the identity socket: %w", err)
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // each user of the machine may ask
		_ = l.Close()
		return fmt.Errorf("open the identity socket to the workload: %w", err)
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
