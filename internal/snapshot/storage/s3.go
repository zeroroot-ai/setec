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

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config carries the connection parameters for an S3-compatible
// object store (real S3 on EKS, MinIO or any S3-compatible endpoint
// when self-hosted — ADR-0007).
type S3Config struct {
	// Endpoint is the base URL of the S3-compatible service (e.g.
	// "http://minio.minio.svc:9000"). Empty selects the AWS default
	// endpoint resolution for Region.
	Endpoint string

	// Bucket is the bucket all checkpoint objects live in. Required.
	Bucket string

	// Region is the signing region. MinIO accepts any non-empty
	// value; real S3 must match the bucket's region.
	Region string

	// Prefix is an optional key prefix (a pseudo-directory) applied
	// to every object key.
	Prefix string

	// UsePathStyle forces path-style addressing
	// (endpoint/bucket/key). Required for MinIO and most
	// self-hosted S3-compatibles.
	UsePathStyle bool

	// AccessKeyID / SecretAccessKey optionally set static
	// credentials. When both are empty the AWS default credential
	// chain applies (env vars, IRSA web-identity, shared config) —
	// the production path on EKS and the path the chart's
	// credentialsSecret env injection feeds.
	AccessKeyID     string
	SecretAccessKey string
}

// S3Backend implements StorageBackend on an S3-compatible object
// store. Layout mirrors LocalDiskBackend: each snapshot occupies
// <prefix><snapshotID>/state.bin with a state.bin.sha256 hex-digest
// sidecar for integrity verification. The storageRef is the
// snapshotID, same as the local-disk backend.
//
// Save streams via the multipart upload manager so multi-GB memory
// checkpoints never buffer fully in node-agent memory. Open verifies
// the SHA256 while streaming and surfaces a mismatch as ErrCorrupted
// at read time. Delete removes the object pair; an object store
// cannot honour the interface's overwrite-before-unlink guidance, so
// callers MUST front this backend with EncryptedBackend — destroying
// the sealed DEK is what actually erases an S3 checkpoint
// (crypto-erase, ADR-0005 invariant 5). That destroy removes every
// version of the sealed DEK, not only the current one; on a versioned
// bucket a plain delete would leave the key material alive as a
// noncurrent version and the erasure would be nominal (#297).
type S3Backend struct {
	// Client is the S3 API client. Required.
	Client *s3.Client

	// Bucket is the target bucket. Required.
	Bucket string

	// Prefix is the optional key prefix.
	Prefix string
}

// NewS3Backend constructs an S3Backend (plus its client) from config.
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: s3 bucket is required")
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("storage: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &S3Backend{Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix}, nil
}

// stateKey renders the object key for a snapshot's payload.
func (b *S3Backend) stateKey(snapshotID string) string {
	return b.Prefix + snapshotID + "/state.bin"
}

// shaKey renders the object key for the sha256 sidecar.
func (b *S3Backend) shaKey(snapshotID string) string {
	return b.stateKey(snapshotID) + ".sha256"
}

// dekKey renders the object key for the sealed-DEK sidecar written by
// S3DEKStore.
func (b *S3Backend) dekKey(snapshotID string) string {
	return b.stateKey(snapshotID) + ".dek"
}

// Save streams state into <prefix><snapshotID>/state.bin, hashing it
// on the way through, then writes the sha256 sidecar. A snapshot that
// already exists returns ErrAlreadyExists without touching the
// object.
func (b *S3Backend) Save(ctx context.Context, snapshotID string, state io.Reader) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := validateSnapshotID(snapshotID); err != nil {
		return 0, "", err
	}
	exists, err := b.exists(ctx, b.stateKey(snapshotID))
	if err != nil {
		return 0, "", err
	}
	if exists {
		return 0, "", ErrAlreadyExists
	}

	h := sha256.New()
	counter := &countingReader{inner: io.TeeReader(state, h)}

	uploader := manager.NewUploader(b.Client)
	if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(b.stateKey(snapshotID)),
		Body:   counter,
	}); err != nil {
		return 0, "", fmt.Errorf("storage: s3 upload %q: %w", snapshotID, err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if _, err := b.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(b.shaKey(snapshotID)),
		Body:   bytes.NewReader([]byte(digest)),
	}); err != nil {
		// The payload landed but its integrity record did not; remove
		// the orphan payload so the snapshot never looks half-created.
		// All versions: on a versioned bucket a plain delete would leave the
		// half-written payload behind as a noncurrent version, still billed
		// and still readable to anyone with the KEK.
		_ = b.deleteObjectAllVersions(ctx, b.stateKey(snapshotID))
		return 0, "", fmt.Errorf("storage: s3 write sha sidecar %q: %w", snapshotID, err)
	}
	return counter.n, snapshotID, nil
}

// Open returns a reader over the payload that verifies the persisted
// SHA256 as the stream drains; the final Read reports ErrCorrupted on
// a digest mismatch. A missing payload returns ErrNotFound; a payload
// whose sidecar is missing returns ErrCorrupted (the integrity record
// is part of the snapshot).
func (b *S3Backend) Open(ctx context.Context, storageRef string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return nil, err
	}
	shaOut, err := b.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.Bucket), Key: aws.String(b.shaKey(storageRef)),
	})
	if err != nil {
		if isS3NotFound(err) {
			// Distinguish "snapshot gone" from "sidecar lost".
			exists, exErr := b.exists(ctx, b.stateKey(storageRef))
			if exErr == nil && exists {
				return nil, fmt.Errorf("%w: sha256 sidecar missing for %q", ErrCorrupted, storageRef)
			}
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: s3 get sha sidecar %q: %w", storageRef, err)
	}
	shaBytes, err := io.ReadAll(io.LimitReader(shaOut.Body, 128))
	_ = shaOut.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("storage: s3 read sha sidecar %q: %w", storageRef, err)
	}
	wantDigest := strings.TrimSpace(string(shaBytes))

	out, err := b.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.Bucket), Key: aws.String(b.stateKey(storageRef)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: s3 get %q: %w", storageRef, err)
	}
	return &verifyingReadCloser{
		inner: out.Body,
		hash:  sha256.New(),
		want:  wantDigest,
	}, nil
}

// Delete removes the payload, its sha sidecar, and any sealed-DEK
// sidecar. An object store offers no overwrite-before-unlink
// semantics; secure erasure of S3 checkpoints is the EncryptedBackend
// wrapper's DEK destruction (crypto-erase). A snapshot with no
// objects at all returns ErrNotFound to preserve the idempotency
// contract.
func (b *S3Backend) Delete(ctx context.Context, storageRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return err
	}
	exists, err := b.exists(ctx, b.stateKey(storageRef))
	if err != nil {
		return err
	}
	for _, key := range []string{b.stateKey(storageRef), b.shaKey(storageRef), b.dekKey(storageRef)} {
		if err := b.deleteObjectAllVersions(ctx, key); err != nil {
			return err
		}
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// Stat reports the payload's size via HeadObject.
func (b *S3Backend) Stat(ctx context.Context, storageRef string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return 0, false, err
	}
	head, err := b.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.Bucket), Key: aws.String(b.stateKey(storageRef)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return 0, false, nil
		}
		if isS3AccessDenied(err) {
			return 0, false, fmt.Errorf("storage: s3 head %q: %w", storageRef, errHeadForbidden(err))
		}
		return 0, false, fmt.Errorf("storage: s3 head %q: %w", storageRef, err)
	}
	return aws.ToInt64(head.ContentLength), true, nil
}

// exists reports whether an object key is present.
func (b *S3Backend) exists(ctx context.Context, key string) (bool, error) {
	_, err := b.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.Bucket), Key: aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	if isS3AccessDenied(err) {
		return false, fmt.Errorf("storage: s3 head %q: %w", key, errHeadForbidden(err))
	}
	return false, fmt.Errorf("storage: s3 head %q: %w", key, err)
}

// errHeadForbidden wraps a 403 from HeadObject with the remedy, because
// the obvious reading of the error is wrong.
//
// S3 answers HeadObject for a key that DOES NOT EXIST with 403, not 404,
// unless the caller holds s3:ListBucket on the bucket. Save() heads the
// target key before every write and on the happy path that key does not
// exist — so a least-privilege policy scoped to object actions on a prefix,
// which is the obvious way to write one, makes every checkpoint fail on the
// first suspend, hours after the deploy, on a path nobody is watching.
//
// An s3:prefix condition does not rescue it: that condition key is only
// populated for ListObjects* requests, so a conditioned ListBucket is not
// satisfied by a HeadObject and the 403 comes back anyway. The grant has to
// be unconditioned.
func errHeadForbidden(err error) error {
	return fmt.Errorf("%w (HeadObject returns 403 rather than 404 for a key that does not "+
		"exist unless the principal holds s3:ListBucket on the whole bucket, unconditioned — "+
		"an s3:prefix condition does not satisfy a HeadObject)", err)
}

// deleteObjectAllVersions removes an object and every non-current version
// and delete marker it has.
//
// A plain DeleteObject on a VERSIONED bucket deletes nothing: it writes a
// delete marker and the previous version survives until a lifecycle rule
// reaps it. Every platform bucket in zeroroot-ai/deploy's eks/gibson is
// versioned, and that is a sane default generally.
//
// For the sealed DEK that is the difference between crypto-erase and the
// appearance of one — S3DEKStore.Destroy is what ADR-0005 invariant 5 leans
// on. As deployed it is not a confidentiality hole, because the sealed DEK is
// useless without the per-session KEK in a Kubernetes Secret that never
// enters the bucket, so deleting that Secret is still a true crypto-erase.
// But the code claimed an erasure it did not perform on its own, and on any
// store where the KEK Secret is not the only copy the reasoning changes with
// nothing in the code to say so. Now the delete is real either way.
//
// Services with no versioning API (MinIO in some configurations, other
// S3-compatibles) answer ListObjectVersions with NotImplemented; that is not
// an error here, it means there are no versions to chase and the plain
// delete below was already sufficient.
func (b *S3Backend) deleteObjectAllVersions(ctx context.Context, key string) error {
	if _, err := b.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.Bucket), Key: aws.String(key),
	}); err != nil && !isS3NotFound(err) {
		return fmt.Errorf("storage: s3 delete %q: %w", key, err)
	}

	paginator := s3.NewListObjectVersionsPaginator(b.Client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(b.Bucket),
		// Prefix, not exact match — the API has no exact-key filter. Our own
		// keys are prefixes of each other (state.bin, state.bin.sha256,
		// state.bin.dek), so every entry MUST be checked against the key we
		// were asked for or deleting the payload would take the sidecars with
		// it.
		Prefix: aws.String(key),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			if isS3NotFound(err) || isS3NotImplemented(err) {
				return nil
			}
			return fmt.Errorf("storage: s3 list versions %q: %w", key, err)
		}
		ids := make([]string, 0, len(page.Versions)+len(page.DeleteMarkers))
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == key {
				ids = append(ids, aws.ToString(v.VersionId))
			}
		}
		for _, m := range page.DeleteMarkers {
			if aws.ToString(m.Key) == key {
				ids = append(ids, aws.ToString(m.VersionId))
			}
		}
		for _, id := range ids {
			if id == "" || id == "null" {
				// An unversioned bucket reports "null"; the plain delete above
				// already handled it.
				continue
			}
			if _, err := b.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket:    aws.String(b.Bucket),
				Key:       aws.String(key),
				VersionId: aws.String(id),
			}); err != nil && !isS3NotFound(err) {
				return fmt.Errorf("storage: s3 delete version %s of %q: %w", id, key, err)
			}
		}
	}
	return nil
}

// AbortStaleMultipartUploads aborts every in-progress multipart upload under
// this backend's prefix that was initiated more than olderThan ago, and
// returns how many it aborted.
//
// Save streams through the multipart uploader. The uploader aborts its own
// upload when Upload returns an error, but it cannot when the node-agent is
// killed mid-suspend — an OOM or a node drain, which is precisely the
// situation session checkpoints exist for. The parts left behind are billed,
// are NOT returned by ListObjects, and no expiration rule touches them. Only
// an abort_incomplete_multipart_upload lifecycle rule or an explicit abort
// reaps them, and a self-hosted MinIO has neither by default.
//
// Call this at node-agent startup. olderThan must comfortably exceed the
// longest suspend this node will run, or a sweep will abort a live upload
// from another agent sharing the prefix.
//
// Needs s3:AbortMultipartUpload and s3:ListBucketMultipartUploads, both easy
// to omit from a Put/Get/Delete least-privilege policy.
func (b *S3Backend) AbortStaleMultipartUploads(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, errors.New("storage: s3 abort stale multipart uploads: olderThan must be positive")
	}
	cutoff := time.Now().Add(-olderThan)

	var aborted int
	var keyMarker, uploadIDMarker *string
	for {
		out, err := b.Client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(b.Bucket),
			Prefix:         aws.String(b.Prefix),
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if err != nil {
			if isS3NotImplemented(err) {
				return aborted, nil
			}
			if isS3AccessDenied(err) {
				return aborted, fmt.Errorf("storage: s3 list multipart uploads: %w "+
					"(the principal needs s3:ListBucketMultipartUploads on the bucket)", err)
			}
			return aborted, fmt.Errorf("storage: s3 list multipart uploads: %w", err)
		}
		for _, u := range out.Uploads {
			if u.Initiated == nil || !u.Initiated.Before(cutoff) {
				continue
			}
			if _, err := b.Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.Bucket),
				Key:      u.Key,
				UploadId: u.UploadId,
			}); err != nil {
				if isS3NotFound(err) {
					continue // someone else got there first
				}
				return aborted, fmt.Errorf("storage: s3 abort multipart upload %q on %q: %w",
					aws.ToString(u.UploadId), aws.ToString(u.Key), err)
			}
			aborted++
		}
		if !aws.ToBool(out.IsTruncated) {
			return aborted, nil
		}
		keyMarker, uploadIDMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// DEKStore returns the SealedDEKStore that keeps sealed DEKs as
// sibling objects of the checkpoints they protect. Unlike the
// node-local DirDEKStore this is safe ONLY because session-checkpoint
// DEKs are sealed under a cluster-scoped per-session KEK held in a
// Kubernetes Secret (never in the bucket): a copy of the bucket still
// carries zero usable key material, and deleting the Secret
// crypto-erases every checkpoint it sealed.
func (b *S3Backend) DEKStore() *S3DEKStore { return &S3DEKStore{Backend: b} }

// S3DEKStore implements SealedDEKStore on the same bucket as its
// S3Backend. See DEKStore for why co-locating sealed DEKs with
// ciphertext is sound here and would NOT be for the node-local KEK.
type S3DEKStore struct {
	Backend *S3Backend
}

// Put writes the sealed DEK, refusing to overwrite an existing one
// (never replace key material — mirrors DirDEKStore's O_EXCL).
func (s *S3DEKStore) Put(ctx context.Context, snapshotID string, sealed []byte) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	exists, err := s.Backend.exists(ctx, s.Backend.dekKey(snapshotID))
	if err != nil {
		return err
	}
	if exists {
		return ErrAlreadyExists
	}
	if _, err := s.Backend.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Backend.Bucket),
		Key:    aws.String(s.Backend.dekKey(snapshotID)),
		Body:   bytes.NewReader(sealed),
	}); err != nil {
		return fmt.Errorf("storage: s3 put sealed DEK %q: %w", snapshotID, err)
	}
	return nil
}

// Get reads the sealed DEK; a missing blob returns ErrNotFound.
func (s *S3DEKStore) Get(ctx context.Context, snapshotID string) ([]byte, error) {
	if err := validateSnapshotID(snapshotID); err != nil {
		return nil, err
	}
	out, err := s.Backend.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Backend.Bucket), Key: aws.String(s.Backend.dekKey(snapshotID)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: s3 get sealed DEK %q: %w", snapshotID, err)
	}
	defer func() { _ = out.Body.Close() }()
	sealed, err := io.ReadAll(io.LimitReader(out.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("storage: s3 read sealed DEK %q: %w", snapshotID, err)
	}
	return sealed, nil
}

// Destroy deletes the sealed DEK. A blob that never existed (or is
// already gone) returns os.ErrNotExist, matching DirDEKStore so the
// EncryptedBackend's idempotent-delete contract holds unchanged.
func (s *S3DEKStore) Destroy(ctx context.Context, snapshotID string) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	exists, err := s.Backend.exists(ctx, s.Backend.dekKey(snapshotID))
	if err != nil {
		return err
	}
	if !exists {
		return os.ErrNotExist
	}
	// Every version, not just the current one. On a versioned bucket a plain
	// DeleteObject writes a delete marker and leaves the sealed DEK alive as
	// a noncurrent version — and this call is what ADR-0005 invariant 5
	// treats as the erasure.
	if err := s.Backend.deleteObjectAllVersions(ctx, s.Backend.dekKey(snapshotID)); err != nil {
		return fmt.Errorf("storage: s3 destroy sealed DEK %q: %w", snapshotID, err)
	}
	return nil
}

// isS3NotFound matches the assorted shapes an S3-compatible service
// uses for "no such key/object".
func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

// isS3AccessDenied matches the shapes an S3-compatible service uses for
// "you may not do that". Kept separate from isS3NotFound on purpose: a 403 is
// NOT a 404, and treating it as one would turn a broken IAM policy into a
// silent "no such checkpoint".
func isS3AccessDenied(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusForbidden {
		return true
	}
	return false
}

// isS3NotImplemented matches an S3-compatible service that does not offer an
// API at all — versioning and multipart listing are both optional in
// practice. Absence of the API means there is nothing for us to clean up, not
// that cleanup failed.
func isS3NotImplemented(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotImplemented", "MethodNotAllowed":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotImplemented {
		return true
	}
	return false
}

// countingReader counts the bytes drained through it.
type countingReader struct {
	inner io.Reader
	n     int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.n += int64(n)
	return n, err
}

// verifyingReadCloser hashes the payload as it streams and compares
// against the recorded digest at EOF.
type verifyingReadCloser struct {
	inner    io.ReadCloser
	hash     hash.Hash
	want     string
	verified bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := v.inner.Read(p)
	if n > 0 {
		_, _ = v.hash.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && !v.verified {
		v.verified = true
		if hex.EncodeToString(v.hash.Sum(nil)) != v.want {
			return n, fmt.Errorf("%w: s3 payload digest mismatch", ErrCorrupted)
		}
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error { return v.inner.Close() }

// Compile-time interface assertions.
var (
	_ StorageBackend = (*S3Backend)(nil)
	_ SealedDEKStore = (*S3DEKStore)(nil)
)
