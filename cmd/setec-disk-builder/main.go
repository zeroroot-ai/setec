// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-disk-builder makes the signed read-only disk of an image
// digest once and stores it in the registry (docs/design/storage.md).
//
//	setec-disk-builder --disk-repo REPO --key-file KEY build IMAGE@sha256:...
package main

import (
	"context"
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
	keyFile := flag.String("key-file", "/etc/setec/disk-signing/seed", "the base64 ed25519 seed that signs each disk")
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
	if flag.NArg() != 2 || flag.Arg(0) != "build" || *repo == "" {
		fmt.Fprintln(os.Stderr, "usage: setec-disk-builder --disk-repo REPO [--key-file KEY] build IMAGE@sha256:DIGEST")
		fmt.Fprintln(os.Stderr,
			"       setec-disk-builder [--issuer I --identity ID | (SETEC_VERIFY_PUBLIC_KEY)] verify IMAGE@sha256:DIGEST")
		return 2
	}
	key, err := diskbuilder.ReadPrivateKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-disk-builder:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b := &diskbuilder.Builder{
		DiskRepo: *repo, Key: key, TempDir: *tmp,
		Options: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)},
	}
	tag, err := b.Ensure(ctx, flag.Arg(1))
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
