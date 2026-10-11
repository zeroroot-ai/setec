// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-disk-builder makes the read-only disk of an image digest
// once and stores it in the registry (docs/design/runtime.md). The operator
// runs it as one Job with three containers, and none holds the signing seed:
//
//	setec-disk-builder --disk-repo REPO --key-id ID check IMAGE@sha256:...
//	setec-disk-builder --work DIR build IMAGE@sha256:...
//	setec-disk-builder --disk-repo REPO --key-id ID --work DIR publish IMAGE@sha256:...
//
// check looks up the disk with the push credential and leaves the marker
// file "exists" in the work directory when the registry holds it. build
// pulls the image and makes the disk in the work directory; it has no
// credential. publish waits for the signature of the operator at
// --signature-file and pushes the disk and the signature. Each step writes
// its JSON report to stdout and to --result-file, the termination message
// that the operator reads.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/diskbuilder/imagesig"
)

func main() {
	os.Exit(runMain())
}

// runMain is the body of main. It returns the exit code, so that each
// deferred call runs before the process exits.
func runMain() int {
	repo := flag.String("disk-repo", "", "check, publish: the repository of the disks, e.g. ghcr.io/org/setec-disks")
	keyID := flag.String("key-id", "", "check, publish: the id of the key that signs the disk. It names the disk tag")
	work := flag.String("work", "/work", "build, publish: the work directory that the containers of the Job share")
	resultFile := flag.String("result-file", "/dev/termination-log",
		"check, build: where the report goes, besides stdout. The operator reads the termination message")
	sigFile := flag.String("signature-file", "/etc/setec/disk-signature/signature.json",
		"publish: the signature of the operator, from a ConfigMap volume that appears after the build")
	wait := flag.Duration("wait", 20*time.Minute, "publish: how long to wait for the signature")
	tmp := flag.String("temp-dir", os.TempDir(), "verify, publish: a directory for temporary files")
	issuer := flag.String("issuer", "", "verify: the OIDC issuer of a keyless signature")
	identity := flag.String("identity", "", "verify: the certificate identity of a keyless signature")
	trustedRoot := flag.String("trusted-root", "",
		"verify: a Sigstore trusted root JSON file. Empty fetches the public-good root through TUF")
	flag.Parse()
	if flag.NArg() != 2 {
		usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	image := flag.Arg(1)
	marker := filepath.Join(*work, "exists")
	disk := filepath.Join(*work, diskbuilder.DiskFile)
	switch flag.Arg(0) {
	case "verify":
		// The public key of a key signature comes from the environment, so
		// the PEM text needs no file.
		policy := diskbuilder.SignaturePolicy{
			Issuer: *issuer, Identity: *identity, PublicKey: []byte(os.Getenv("SETEC_VERIFY_PUBLIC_KEY")),
		}
		return runVerify(image, policy, *trustedRoot, *tmp)
	case "check":
		if *repo == "" || *keyID == "" {
			usage()
			return 2
		}
		ok, err := diskbuilder.Exists(ctx, *repo, image, *keyID, remote.WithAuthFromKeychain(authn.DefaultKeychain))
		if err != nil {
			return fail(err)
		}
		if ok {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				return fail(err)
			}
		}
		return report(*resultFile, diskbuilder.CheckResult{Exists: ok})
	case "build":
		if _, err := os.Stat(marker); err == nil {
			fmt.Println("the registry holds the disk; nothing to build")
			return 0
		}
		// No credential: the image registries of the class serve the
		// image anonymously, and this container holds no secret.
		res, err := (&diskbuilder.Builder{}).Build(ctx, image, disk)
		if err != nil {
			return fail(err)
		}
		return report(*resultFile, res)
	case "publish":
		if *repo == "" || *keyID == "" {
			usage()
			return 2
		}
		if _, err := os.Stat(marker); err == nil {
			fmt.Println("the registry holds the disk; nothing to publish")
			return 0
		}
		sig, err := waitForSignature(ctx, *sigFile, *wait)
		if err != nil {
			return fail(err)
		}
		p := &diskbuilder.Publisher{
			DiskRepo: *repo, TempDir: *tmp,
			Options: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)},
		}
		tag, err := p.Publish(ctx, image, *keyID, disk, sig)
		if err != nil {
			return fail(err)
		}
		fmt.Println(tag.String())
		return 0
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: setec-disk-builder --disk-repo REPO --key-id ID check IMAGE@sha256:DIGEST")
	fmt.Fprintln(os.Stderr, "       setec-disk-builder [--work DIR] build IMAGE@sha256:DIGEST")
	fmt.Fprintln(os.Stderr, "       setec-disk-builder --disk-repo REPO --key-id ID [--work DIR] publish IMAGE@sha256:DIGEST")
	fmt.Fprintln(os.Stderr,
		"       setec-disk-builder [--issuer I --identity ID | (SETEC_VERIFY_PUBLIC_KEY)] verify IMAGE@sha256:DIGEST")
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
	return 1
}

// report writes v as JSON to stdout and to path.
func report(path string, v any) int {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err)
	}
	fmt.Println(string(out))
	if path != "" {
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return fail(fmt.Errorf("write the report: %w", err))
		}
	}
	return 0
}

// waitForSignature reads the signature at path. The kubelet writes the file
// when the operator creates the ConfigMap, after the build container ends.
func waitForSignature(ctx context.Context, path string, wait time.Duration) (diskbuilder.Signature, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		raw, err := os.ReadFile(path) //nolint:gosec // the path of the signature volume
		if err == nil && len(raw) > 0 {
			var sig diskbuilder.Signature
			if err := json.Unmarshal(raw, &sig); err != nil {
				return diskbuilder.Signature{}, fmt.Errorf("the signature at %s: %w", path, err)
			}
			return sig, nil
		}
		select {
		case <-ctx.Done():
			return diskbuilder.Signature{}, fmt.Errorf("no signature at %s after %s", path, wait)
		case <-t.C:
		}
	}
}

// runVerify checks the cosign signature of image against policy. It exits
// 0 when the image verifies, 1 when it does not, and 2 on a bad policy.
func runVerify(image string, policy diskbuilder.SignaturePolicy, trustedRoot, tmp string) int {
	if err := policy.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var trusted root.TrustedMaterial
	if len(policy.PublicKey) == 0 {
		var err error
		if trustedRoot != "" {
			trusted, err = root.NewTrustedRootFromPath(trustedRoot)
		} else {
			trusted, err = root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithCachePath(filepath.Join(tmp, "tuf")))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "setec-disk-builder: the Sigstore trusted root:", err)
			return 1
		}
	}
	err := imagesig.VerifyImage(ctx, image, policy, trusted, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 1
	}
	fmt.Println("verified", image)
	return 0
}
