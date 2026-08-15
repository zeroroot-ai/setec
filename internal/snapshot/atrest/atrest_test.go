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

package atrest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func mustDEK(t *testing.T) []byte {
	t.Helper()
	dek, err := NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	return dek
}

func encryptBytes(t *testing.T, pt, dek []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	n, err := Encrypt(&buf, bytes.NewReader(pt), dek)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("Encrypt reported %d bytes, wrote %d", n, buf.Len())
	}
	return buf.Bytes()
}

func decryptBytes(ct, dek []byte) ([]byte, error) {
	dr, err := NewDecryptingReader(bytes.NewReader(ct), dek)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(dr)
}

func TestStreamRoundtrip(t *testing.T) {
	sizes := []int{0, 1, 100, chunkSize - 1, chunkSize, chunkSize + 1, 2*chunkSize + 5000}
	dek := mustDEK(t)
	for _, size := range sizes {
		pt := make([]byte, size)
		if _, err := rand.Read(pt); err != nil {
			t.Fatal(err)
		}
		ct := encryptBytes(t, pt, dek)
		// Containment is only evidence of a leak when the needle is long
		// enough that a chance match is impossible in practice. Ciphertext
		// is effectively uniform random, so a 1-byte needle appears in a
		// ~29-byte frame about 11% of the time (1 - (255/256)^29) — this
		// assertion used to fail roughly one run in ten on size 1 and had
		// nothing to do with encryption (setec#292). At 32 bytes the
		// chance match is ~2^-256.
		//
		// The roundtrip assertion below still covers EVERY size, including
		// 0 and 1, which is what actually exercises the short-input edges.
		const minLeakNeedle = 32
		if size >= minLeakNeedle && bytes.Contains(ct, pt[:minLeakNeedle]) {
			t.Fatalf("size %d: ciphertext contains plaintext prefix", size)
		}
		got, err := decryptBytes(ct, dek)
		if err != nil {
			t.Fatalf("size %d: decrypt: %v", size, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("size %d: roundtrip mismatch", size)
		}
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	ct := encryptBytes(t, []byte("attack at dawn"), mustDEK(t))
	if _, err := decryptBytes(ct, mustDEK(t)); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong key must fail with ErrDecrypt, got %v", err)
	}
}

func TestDecrypt_TamperFails(t *testing.T) {
	dek := mustDEK(t)
	ct := encryptBytes(t, bytes.Repeat([]byte("m"), 4096), dek)
	// Flip one ciphertext byte past the header.
	ct[len(ct)-10] ^= 0x40
	if _, err := decryptBytes(ct, dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext must fail with ErrDecrypt, got %v", err)
	}
}

func TestDecrypt_TruncationFails(t *testing.T) {
	dek := mustDEK(t)
	pt := make([]byte, 2*chunkSize) // two full chunks
	ct := encryptBytes(t, pt, dek)

	// Cut the stream at the first chunk boundary: header + lenPrefix +
	// (chunk + tag). The remaining first chunk verifies under the
	// non-final nonce only if more data follows — dropping the tail
	// must be detected.
	cut := len(streamMagic) + noncePrefixLen + 4 + chunkSize + gcmTagSize
	if _, err := decryptBytes(ct[:cut], dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated stream must fail with ErrDecrypt, got %v", err)
	}
}

func TestDecrypt_ReorderFails(t *testing.T) {
	dek := mustDEK(t)
	pt := make([]byte, 3*chunkSize)
	if _, err := rand.Read(pt); err != nil {
		t.Fatal(err)
	}
	ct := encryptBytes(t, pt, dek)

	header := len(streamMagic) + noncePrefixLen
	chunkLen := 4 + chunkSize + gcmTagSize
	// Swap chunk 0 and chunk 1.
	swapped := append([]byte{}, ct[:header]...)
	swapped = append(swapped, ct[header+chunkLen:header+2*chunkLen]...)
	swapped = append(swapped, ct[header:header+chunkLen]...)
	swapped = append(swapped, ct[header+2*chunkLen:]...)
	if _, err := decryptBytes(swapped, dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("reordered chunks must fail with ErrDecrypt, got %v", err)
	}
}

func TestDecrypt_BadMagicFails(t *testing.T) {
	dek := mustDEK(t)
	if _, err := decryptBytes([]byte("definitely-not-an-encrypted-stream"), dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("foreign input must fail with ErrDecrypt, got %v", err)
	}
}

func TestDecrypt_OversizedChunkLengthFails(t *testing.T) {
	dek := mustDEK(t)
	ct := encryptBytes(t, []byte("x"), dek)
	// Corrupt the length prefix to a huge value; the reader must
	// refuse rather than allocate.
	binary.BigEndian.PutUint32(ct[len(streamMagic)+noncePrefixLen:], 1<<31)
	if _, err := decryptBytes(ct, dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("oversized chunk length must fail with ErrDecrypt, got %v", err)
	}
}

func TestSealOpenDEK_RoundtripAndAADBinding(t *testing.T) {
	kek := mustDEK(t)
	dek := mustDEK(t)
	sealed, err := SealDEK(kek, dek, "artifact-a")
	if err != nil {
		t.Fatalf("SealDEK: %v", err)
	}
	got, err := OpenDEK(kek, sealed, "artifact-a")
	if err != nil {
		t.Fatalf("OpenDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unsealed DEK differs")
	}
	// A sealed DEK is bound to its artifact identity: opening under a
	// different identity must fail.
	if _, err := OpenDEK(kek, sealed, "artifact-b"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("AAD mismatch must fail with ErrDecrypt, got %v", err)
	}
	// And a different KEK must fail.
	if _, err := OpenDEK(mustDEK(t), sealed, "artifact-a"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong KEK must fail with ErrDecrypt, got %v", err)
	}
}

func TestLoadOrCreateKEK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "node.key")
	k1, err := LoadOrCreateKEK(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1) != KeySize {
		t.Fatalf("KEK length %d", len(k1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("keyfile mode %04o, want 0600", info.Mode().Perm())
	}
	// Second load returns the same key.
	k2, err := LoadOrCreateKEK(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("KEK not stable across loads")
	}
}

func TestLoadOrCreateKEK_RejectsLooseModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.key")
	if err := os.WriteFile(path, make([]byte, KeySize), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKEK(path); err == nil {
		t.Fatal("world-readable keyfile must be rejected")
	}
}

func TestEncryptFile_ReplacesPlaintextInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bin")
	marker := bytes.Repeat([]byte("TOPSECRET-GUEST-MEMORY-"), 100)
	if err := os.WriteFile(path, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	dek := mustDEK(t)
	if err := EncryptFile(path, dek); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("TOPSECRET")) {
		t.Fatal("encrypted file still contains plaintext marker")
	}
	if !bytes.HasPrefix(raw, []byte(streamMagic)) {
		t.Fatal("encrypted file missing stream magic")
	}
	// No stray temp files.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file in dir, got %d", len(entries))
	}
	// Roundtrip through DecryptFile, which also reports the plaintext
	// digest for verdict checks (ADR-0005 invariant 1).
	out := filepath.Join(dir, "plain.bin")
	digest, err := DecryptFile(path, out, dek)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, marker) {
		t.Fatal("DecryptFile roundtrip mismatch")
	}
	want := sha256.Sum256(marker)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("DecryptFile digest = %s, want SHA-256 of the plaintext", digest)
	}
}

func TestShred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(path, []byte("sealed key material"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Shred(path); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shredded file still present: %v", err)
	}
	// Idempotency contract: missing file returns os.ErrNotExist.
	if err := Shred(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second Shred should return os.ErrNotExist, got %v", err)
	}
}
