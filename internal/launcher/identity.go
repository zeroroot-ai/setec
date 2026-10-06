// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zeroroot-ai/setec/internal/sandboxid"
)

// IdentityPort is the vsock port on which the guest agent asks the
// launcher for an identity token of the Sandbox (setec#235).
const IdentityPort = 5301

// maxAudience bounds the audience of a token.
const maxAudience = 256

// IdentitySpec is the identity of the Sandbox of this launcher.
type IdentitySpec struct {
	// SandboxID is the <namespace>/<name>/<uid> of the Sandbox.
	SandboxID string `json:"sandboxID"`
	// Client and Tenant are the owner pair of the namespace.
	Client string `json:"client,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	// KeyFile holds the base64 ed25519 seed of the Sandbox. It is outside
	// the machine: no process in the machine can read it.
	KeyFile string `json:"keyFile"`
	// Generation is the identity generation when the Pod started.
	Generation int64 `json:"generation"`
	// GenerationFile, when set, holds a later generation. The node agent
	// writes it during a snapshot, while the machine is paused.
	GenerationFile string `json:"generationFile,omitempty"`
}

// IdentityRequest is what the guest agent sends on IdentityPort.
type IdentityRequest struct {
	Audience string `json:"audience"`
}

// IdentityResponse is the answer of the launcher: a token or an error.
type IdentityResponse struct {
	Token   string `json:"token,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	Error   string `json:"error,omitempty"`
}

// identitySigner signs the tokens of one Sandbox.
type identitySigner struct {
	spec *IdentitySpec
	key  ed25519.PrivateKey
	now  func() time.Time
}

func newIdentitySigner(spec *IdentitySpec) (*identitySigner, error) {
	raw, err := os.ReadFile(spec.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the identity key: %w", err)
	}
	key, err := sandboxid.KeyFromSeed(string(raw))
	if err != nil {
		return nil, err
	}
	return &identitySigner{spec: spec, key: key, now: time.Now}, nil
}

// generation is the larger of the generation of the spec and the one in
// the generation file.
func (s *identitySigner) generation() int64 {
	gen := s.spec.Generation
	if s.spec.GenerationFile == "" {
		return gen
	}
	raw, err := os.ReadFile(s.spec.GenerationFile)
	if err != nil {
		return gen
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && v > gen {
		return v
	}
	return gen
}

func (s *identitySigner) sign(audience string) IdentityResponse {
	if audience == "" || len(audience) > maxAudience || strings.ContainsAny(audience, "\n\r") {
		return IdentityResponse{Error: "the audience must be 1 to 256 characters on one line"}
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return IdentityResponse{Error: "no randomness for the token id"}
	}
	now := s.now()
	c := sandboxid.Claims{
		Issuer: sandboxid.Issuer, Subject: s.spec.SandboxID, Audience: audience,
		Client: s.spec.Client, Tenant: s.spec.Tenant, Generation: s.generation(),
		IssuedAt: now.Unix(), Expires: now.Add(sandboxid.Lifetime).Unix(), ID: hex.EncodeToString(jti),
	}
	tok, err := sandboxid.Sign(s.key, c)
	if err != nil {
		return IdentityResponse{Error: err.Error()}
	}
	return IdentityResponse{Token: tok, Expires: c.Expires}
}

// listenIdentity listens where Firecracker connects a guest call to the
// host on IdentityPort.
func listenIdentity(workDir string) (net.Listener, error) {
	path := filepath.Join(workDir, VsockSocket+"_"+strconv.Itoa(IdentityPort))
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen for identity requests on %s: %w", path, err)
	}
	return l, nil
}

// serveIdentity answers one request on each connection until ctx ends.
func serveIdentity(ctx context.Context, l net.Listener, s *identitySigner) {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}
		go answerIdentity(c, s)
	}
}

func answerIdentity(c net.Conn, s *identitySigner) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req IdentityRequest
	resp := IdentityResponse{Error: "the request is not valid JSON"}
	if json.Unmarshal(line, &req) == nil {
		resp = s.sign(req.Audience)
	}
	raw, _ := json.Marshal(resp)
	_, _ = c.Write(append(raw, '\n'))
}
