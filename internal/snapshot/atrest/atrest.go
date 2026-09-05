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

// Package atrest implements encryption at rest for snapshot artifacts
// (ADR-0005 invariant 5). The design is deliberately boring:
//
//   - Every artifact (a Snapshot's framed state stream, a pool entry's
//     state/memory pair) is encrypted with its own fresh 256-bit data
//     encryption key (DEK) under chunked AES-256-GCM. Keys are
//     per-artifact — per-pool-entry and per-session-snapshot — never
//     cluster-global.
//   - Each DEK is sealed (AES-256-GCM key wrap) with a node-local key
//     encryption key (KEK) held in a 0600 root-owned keyfile. The AAD
//     of the wrap binds the DEK to the artifact's identity so a sealed
//     DEK cannot be swapped between artifacts.
//   - Destroying an artifact destroys its key: the sealed DEK is
//     zero-overwritten, fsynced, and unlinked BEFORE the ciphertext is
//     reclaimed. Even where filesystem copy-on-write defeats the
//     ciphertext overwrite, the artifact is cryptographically erased
//     because its only key is gone.
//
// There is deliberately no external KMS dependency (ADR-0003/0007
// portability): the KEK is a node-local file. The accepted residual is
// that an attacker with simultaneous access to a node's keyfile AND its
// artifact tree can decrypt that node's artifacts; the encryption
// defends copies of the artifact tree (backups, StorageBackend copies,
// reclaimed block devices) and provides provable crypto-erase on
// teardown.
//
// Stream format (version 1):
//
//	magic "SETECAS1" (8 bytes)
//	nonce prefix    (7 random bytes)
//	chunk*          each: be_uint32(len(ct)) || ct
//
// Chunk i is sealed with nonce = prefix || be_uint32(i) || finalFlag,
// where finalFlag is 0x01 on the last chunk and 0x00 otherwise (the
// STREAM construction). The final flag defeats truncation; the counter
// defeats reordering; GCM defeats modification.
package atrest

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// streamMagic identifies a version-1 encrypted artifact stream.
	streamMagic = "SETECAS1"

	// sealMagic identifies a version-1 sealed-DEK blob.
	sealMagic = "SETECDK1"

	// KeySize is the AES-256 key length used for both KEKs and DEKs.
	KeySize = 32

	// chunkSize is the plaintext chunk length. 1 MiB keeps memory use
	// bounded while amortising the 16-byte GCM tag per chunk.
	chunkSize = 1 << 20

	// noncePrefixLen + 4-byte counter + 1-byte final flag = the 12-byte
	// GCM nonce.
	noncePrefixLen = 7

	gcmTagSize   = 16
	gcmNonceSize = 12

	// maxCiphertextChunk bounds a chunk read so a corrupted length
	// prefix cannot drive an unbounded allocation.
	maxCiphertextChunk = chunkSize + gcmTagSize
)

// ErrDecrypt is returned (wrapped) for every decryption failure —
// wrong key, tampered ciphertext, truncation, reordering, bad framing.
// The failure modes are deliberately not distinguished to the caller.
var ErrDecrypt = errors.New("atrest: decryption failed")

// NewDEK returns a fresh random 256-bit data encryption key.
func NewDEK() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("atrest: generate DEK: %w", err)
	}
	return k, nil
}

// LoadOrCreateKEK returns the node key-encryption key stored at path,
// creating it (0600, parent dirs 0700) with fresh random bytes when it
// does not exist. A keyfile that is group- or world-accessible is
// rejected rather than silently used.
func LoadOrCreateKEK(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != KeySize {
			return nil, fmt.Errorf("atrest: keyfile %q has %d bytes, want %d", path, len(b), KeySize)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("atrest: stat keyfile: %w", statErr)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("atrest: keyfile %q mode %04o is group/world accessible; want 0600", path, info.Mode().Perm())
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("atrest: read keyfile %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("atrest: mkdir key dir: %w", err)
	}
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("atrest: generate KEK: %w", err)
	}
	// O_EXCL: if two processes race the create, exactly one wins and
	// the loser re-reads the winner's key.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateKEK(path)
		}
		return nil, fmt.Errorf("atrest: create keyfile %q: %w", path, err)
	}
	_, werr := f.Write(k)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("atrest: write keyfile %q: %w", path, werr)
	}
	return k, nil
}

// SealDEK wraps dek with kek under AES-256-GCM. aad binds the sealed
// blob to the artifact's identity: OpenDEK with a different aad fails,
// so a sealed DEK cannot be moved between artifacts.
func SealDEK(kek, dek []byte, aad string) ([]byte, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("atrest: seal nonce: %w", err)
	}
	out := make([]byte, 0, len(sealMagic)+gcmNonceSize+len(dek)+gcmTagSize)
	out = append(out, sealMagic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, dek, []byte(aad)), nil
}

// OpenDEK reverses SealDEK. Any failure — wrong KEK, wrong aad,
// tampered blob — returns an error wrapping ErrDecrypt.
func OpenDEK(kek, blob []byte, aad string) ([]byte, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	if len(blob) < len(sealMagic)+gcmNonceSize+gcmTagSize || string(blob[:len(sealMagic)]) != sealMagic {
		return nil, fmt.Errorf("%w: sealed DEK framing", ErrDecrypt)
	}
	nonce := blob[len(sealMagic) : len(sealMagic)+gcmNonceSize]
	dek, err := aead.Open(nil, nonce, blob[len(sealMagic)+gcmNonceSize:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: open sealed DEK", ErrDecrypt)
	}
	return dek, nil
}

// Encrypt streams src through chunked AES-256-GCM into dst and returns
// the number of ciphertext bytes written (including framing).
func Encrypt(dst io.Writer, src io.Reader, dek []byte) (int64, error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return 0, err
	}
	prefix := make([]byte, noncePrefixLen)
	if _, err := rand.Read(prefix); err != nil {
		return 0, fmt.Errorf("atrest: nonce prefix: %w", err)
	}

	var written int64
	n, err := dst.Write([]byte(streamMagic))
	written += int64(n)
	if err != nil {
		return written, err
	}
	n, err = dst.Write(prefix)
	written += int64(n)
	if err != nil {
		return written, err
	}

	br := bufio.NewReaderSize(src, chunkSize)
	buf := make([]byte, chunkSize)
	lenBuf := make([]byte, 4)
	var counter uint32
	for {
		pt, readErr := readFull(br, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return written, fmt.Errorf("atrest: read plaintext: %w", readErr)
		}
		// A chunk is final when the reader is exhausted after it.
		final := false
		if errors.Is(readErr, io.EOF) {
			final = true
		} else if _, peekErr := br.Peek(1); peekErr != nil {
			if !errors.Is(peekErr, io.EOF) {
				return written, fmt.Errorf("atrest: peek plaintext: %w", peekErr)
			}
			final = true
		}

		ct := aead.Seal(nil, chunkNonce(prefix, counter, final), pt, nil)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(ct)))
		n, err = dst.Write(lenBuf)
		written += int64(n)
		if err != nil {
			return written, err
		}
		n, err = dst.Write(ct)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if final {
			return written, nil
		}
		if counter == ^uint32(0) {
			return written, errors.New("atrest: chunk counter exhausted")
		}
		counter++
	}
}

// NewDecryptingReader returns a reader that yields the plaintext of an
// Encrypt-produced stream, verifying integrity chunk by chunk. The
// header is validated eagerly so callers fail fast on non-encrypted or
// foreign input. Truncation (missing final chunk), reordering, and
// modification all surface as read errors wrapping ErrDecrypt.
func NewDecryptingReader(src io.Reader, dek []byte) (io.Reader, error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(src, maxCiphertextChunk)
	header := make([]byte, len(streamMagic)+noncePrefixLen)
	if _, err := io.ReadFull(br, header); err != nil {
		return nil, fmt.Errorf("%w: stream header", ErrDecrypt)
	}
	if string(header[:len(streamMagic)]) != streamMagic {
		return nil, fmt.Errorf("%w: bad stream magic", ErrDecrypt)
	}
	return &decryptingReader{
		aead:   aead,
		src:    br,
		prefix: header[len(streamMagic):],
	}, nil
}

type decryptingReader struct {
	aead    cipher.AEAD
	src     *bufio.Reader
	prefix  []byte
	counter uint32
	buf     []byte // decrypted bytes not yet consumed
	done    bool
	err     error
}

func (r *decryptingReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.nextChunk(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// nextChunk reads and authenticates one chunk into r.buf.
func (r *decryptingReader) nextChunk() error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r.src, lenBuf); err != nil {
		// Running out of input before a final-flagged chunk is a
		// truncation attack, not a clean EOF.
		return fmt.Errorf("%w: truncated stream", ErrDecrypt)
	}
	ctLen := binary.BigEndian.Uint32(lenBuf)
	if ctLen < gcmTagSize || ctLen > maxCiphertextChunk {
		return fmt.Errorf("%w: chunk length out of range", ErrDecrypt)
	}
	ct := make([]byte, ctLen)
	if _, err := io.ReadFull(r.src, ct); err != nil {
		return fmt.Errorf("%w: truncated chunk", ErrDecrypt)
	}

	// Determine finality by lookahead: the last chunk of the stream
	// must verify under the final-flagged nonce, all others under the
	// non-final nonce.
	final := false
	if _, err := r.src.Peek(1); err != nil {
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("atrest: peek ciphertext: %w", err)
		}
		final = true
	}
	pt, err := r.aead.Open(nil, chunkNonce(r.prefix, r.counter, final), ct, nil)
	if err != nil {
		return fmt.Errorf("%w: chunk %d", ErrDecrypt, r.counter)
	}
	r.counter++
	r.buf = pt
	r.done = final
	return nil
}

// chunkNonce renders the 12-byte GCM nonce for chunk i:
// prefix(7) || be_uint32(i) || finalFlag(1).
func chunkNonce(prefix []byte, counter uint32, final bool) []byte {
	nonce := make([]byte, gcmNonceSize)
	copy(nonce, prefix)
	binary.BigEndian.PutUint32(nonce[noncePrefixLen:], counter)
	if final {
		nonce[gcmNonceSize-1] = 1
	}
	return nonce
}

// EncryptFile replaces the plaintext file at path with its encrypted
// form: the ciphertext is written to a sibling temp file, fsynced, the
// plaintext is zero-overwritten and unlinked, and the temp file is
// renamed into place. On any error the plaintext file is left intact.
func EncryptFile(path string, dek []byte) error {
	src, err := os.Open(path) //nolint:gosec // path is node-agent controlled, not attacker input
	if err != nil {
		return fmt.Errorf("atrest: open %q: %w", path, err)
	}
	tmp := path + ".enc.tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // sibling of node-agent controlled path
	if err != nil {
		_ = src.Close()
		return fmt.Errorf("atrest: create %q: %w", tmp, err)
	}
	_, encErr := Encrypt(dst, src, dek)
	if serr := dst.Sync(); encErr == nil {
		encErr = serr
	}
	if cerr := dst.Close(); encErr == nil {
		encErr = cerr
	}
	_ = src.Close()
	if encErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atrest: encrypt %q: %w", path, encErr)
	}
	// Plaintext existed on disk; zero it before unlink so residual
	// bytes are not trivially recoverable, then move the ciphertext in.
	if err := Shred(path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atrest: shred plaintext %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atrest: rename %q: %w", tmp, err)
	}
	return nil
}

// DecryptFile streams the encrypted file at src into a plaintext file
// at dst (0600) and returns the lowercase-hex SHA-256 of the plaintext,
// computed in the same pass. Used by restore paths that must hand
// Firecracker a plaintext state file; the digest lets them check the
// recovered bytes against a recorded verdict (e.g. the pool entry's
// secret-scan record, ADR-0005 invariant 1) without a second read.
func DecryptFile(src, dst string, dek []byte) (string, error) {
	in, err := os.Open(src) //nolint:gosec // node-agent controlled path
	if err != nil {
		return "", fmt.Errorf("atrest: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	dr, err := NewDecryptingReader(in, dek)
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // node-agent controlled path
	if err != nil {
		return "", fmt.Errorf("atrest: create %q: %w", dst, err)
	}
	h := sha256.New()
	_, cpErr := io.Copy(io.MultiWriter(out, h), dr)
	if cerr := out.Close(); cpErr == nil {
		cpErr = cerr
	}
	if cpErr != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("atrest: decrypt %q: %w", src, cpErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Shred zero-overwrites the file at path (single pass), fsyncs, and
// unlinks it. Missing files return os.ErrNotExist so callers can treat
// "already gone" as idempotent success. Like the storage backend's
// secure erase this is pragmatic, not cryptographic, erasure — but for
// sealed-DEK files it IS the cryptographic erasure of the artifact the
// key protected, which is why key destruction always happens first.
func Shred(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if size := info.Size(); size > 0 {
		f, err := os.OpenFile(path, os.O_WRONLY, 0o600) //nolint:gosec // node-agent controlled path
		if err != nil {
			return err
		}
		buf := make([]byte, 64*1024)
		var written int64
		for written < size {
			chunk := min(size-written, int64(len(buf)))
			n, werr := f.Write(buf[:chunk])
			if werr != nil {
				_ = f.Close()
				return werr
			}
			written += int64(n)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// readFull reads as much of buf as possible, returning io.EOF only
// when zero bytes remain. Unlike io.ReadFull it treats a short read at
// stream end as success with the short slice.
func readFull(r io.Reader, buf []byte) ([]byte, error) {
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return buf[:n], nil
	}
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return buf[:n], err
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("atrest: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("atrest: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("atrest: gcm: %w", err)
	}
	return aead, nil
}
