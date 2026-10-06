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
	"syscall"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
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
	flag.Parse()
	if flag.NArg() != 2 || flag.Arg(0) != "build" || *repo == "" {
		fmt.Fprintln(os.Stderr, "usage: setec-disk-builder --disk-repo REPO [--key-file KEY] build IMAGE@sha256:DIGEST")
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
