// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// ReadPrivateKey reads an ed25519 seed (32 bytes, base64) from path. The
// install keeps it in a Secret of the disk builder.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a mounted Secret
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("diskbuilder: %s is not a base64 ed25519 seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKeys reads base64 ed25519 public keys. More than one key lets
// the install rotate the signing key. An empty list is refused: a node that
// trusts no key must not start a machine.
func ParsePublicKeys(encoded []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, e := range encoded {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e))
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("diskbuilder: %q is not a base64 ed25519 public key", e)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("diskbuilder: no public key to check a disk with")
	}
	return keys, nil
}
