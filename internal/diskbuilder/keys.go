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

// ReadPublicKeys reads one base64 ed25519 public key on each line of path.
// More than one key lets the install rotate the signing key.
func ReadPublicKeys(path string) ([]ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a mounted ConfigMap
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("diskbuilder: %s holds a line that is not a base64 ed25519 public key", path)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("diskbuilder: %s holds no public key", path)
	}
	return keys, nil
}
