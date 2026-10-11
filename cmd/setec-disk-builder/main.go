// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-disk-builder makes the read-only disk of an image digest
// once and stores it in the registry (docs/design/storage.md). It never
// holds the signing seed: the operator signs the disk between the two steps.
//
//	setec-disk-builder --disk-repo REPO --key-id ID build IMAGE@sha256:...
//	setec-disk-builder --disk-repo REPO --key-id ID publish IMAGE@sha256:...
//
// build pulls the image, makes the disk, pushes its layer, and writes a
// diskbuilder.BuildResult as JSON to stdout and to --result-file (the
// termination message of the Pod, which the operator reads). publish reads
// that result from SETEC_DISK_BUILD and the signature of the operator from
// SETEC_DISK_SIGNATURE, and pushes the signed disk under its tag.
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
	repo := flag.String("disk-repo", "", "the repository of the disks, e.g. ghcr.io/org/setec-disks")
	keyID := flag.String("key-id", "", "build, publish: the id of the key that signs the disk. It names the disk tag")
	resultFile := flag.String("result-file", "/dev/termination-log",
		"build: where the build result goes, besides stdout. The operator reads the termination message of the Pod")
	tmp := flag.String("temp-dir", os.TempDir(), "where a disk is built")
	issuer := flag.String("issuer", "", "verify: the OIDC issuer of a keyless signature")
	identity := flag.String("identity", "", "verify: the certificate identity of a keyless signature")
	trustedRoot := flag.String("trusted-root", "",
		"verify: a Sigstore trusted root JSON file. Empty fetches the public-good root through TUF")
	flag.Parse()
	if flag.NArg() == 2 && flag.Arg(0) == "verify" {
		// The public key of a key signature comes from the environment, so
		// the PEM text needs no file.
		policy := diskbuilder.SignaturePolicy{
			Issuer: *issuer, Identity: *identity, PublicKey: []byte(os.Getenv("SETEC_VERIFY_PUBLIC_KEY")),
		}
		return runVerify(flag.Arg(1), policy, *trustedRoot, *tmp)
	}
	if flag.NArg() != 2 || *repo == "" || *keyID == "" || (flag.Arg(0) != "build" && flag.Arg(0) != "publish") {
		fmt.Fprintln(os.Stderr,
			"usage: setec-disk-builder --disk-repo REPO --key-id ID build IMAGE@sha256:DIGEST")
		fmt.Fprintln(os.Stderr,
			"       setec-disk-builder --disk-repo REPO --key-id ID publish IMAGE@sha256:DIGEST "+
				"(SETEC_DISK_BUILD, SETEC_DISK_SIGNATURE)")
		fmt.Fprintln(os.Stderr,
			"       setec-disk-builder [--issuer I --identity ID | (SETEC_VERIFY_PUBLIC_KEY)] verify IMAGE@sha256:DIGEST")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	if flag.Arg(0) == "publish" {
		return runPublish(ctx, &diskbuilder.Publisher{DiskRepo: *repo, Options: opts, TempDir: *tmp}, *keyID, flag.Arg(1))
	}
	b := &diskbuilder.Builder{DiskRepo: *repo, KeyID: *keyID, TempDir: *tmp, Options: opts}
	res, err := b.Build(ctx, flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 1
	}
	out, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 1
	}
	fmt.Println(string(out))
	if *resultFile != "" {
		if err := os.WriteFile(*resultFile, out, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "setec-disk-builder: write the build result:", err)
			return 1
		}
	}
	return 0
}

// runPublish pushes the signed disk of image. The build result and the
// signature come from the environment; the operator sets both.
func runPublish(ctx context.Context, p *diskbuilder.Publisher, keyID, image string) int {
	res, err := diskbuilder.ParseBuildResult([]byte(os.Getenv("SETEC_DISK_BUILD")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder: SETEC_DISK_BUILD:", err)
		return 2
	}
	var sig diskbuilder.Signature
	if err := json.Unmarshal([]byte(os.Getenv("SETEC_DISK_SIGNATURE")), &sig); err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder: SETEC_DISK_SIGNATURE:", err)
		return 2
	}
	tag, err := p.Publish(ctx, image, keyID, res, sig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 1
	}
	fmt.Println(tag.String())
	return 0
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
