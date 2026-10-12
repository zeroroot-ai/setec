// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ParsePrivateKey reads an ed25519 seed (32 bytes, base64).
func ParsePrivateKey(raw []byte) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("diskbuilder: the value is not a base64 ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// keyIDLen is the length of a key id: 16 hex characters of the SHA-256 of
// the public key.
const keyIDLen = 16

// KeyID names a signing key in the tag of each disk that it signs (setec#272).
// It is the first 16 hex characters of the SHA-256 of the public key. A new
// seed has a new id, so the disk builder signs a new disk beside the disk of
// the previous seed, and an immutable tag never blocks it.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:keyIDLen]
}

// EncodePublicKey is the base64 form of pub, as --disk-public-key takes it.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// validKeyID reports whether id has the form of KeyID.
func validKeyID(id string) bool {
	if len(id) != keyIDLen {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
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
		return nil, errors.New("diskbuilder: no public key to check a disk with")
	}
	return keys, nil
}
