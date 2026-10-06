// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder/signtest"
)

// signedPoolImage copies the linux/amd64 image of src into the disk
// registry of the suite and signs it with a new key, as cosign does. The
// operator builds no base from a pool image whose signature it cannot
// check, and the suite cannot sign an image on Docker Hub. It returns the
// copy by digest and the signer to put on the class.
func signedPoolImage(ctx context.Context, t *testing.T, src string) (string, *setecv1alpha1.ImageSignature) {
	t.Helper()
	if launcherCfg.diskRepo == "" {
		t.Fatal("a signed pool image needs SETEC_E2E_DISK_REPO, the registry of the suite")
	}
	srcRef, err := name.ParseReference(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	img, err := remote.Image(srcRef, remote.WithContext(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	reg, _, _ := strings.Cut(launcherCfg.diskRepo, "/")
	dst, err := name.NewTag(reg + "/setec-pool/" + testNamespace + ":e2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(dst, img, remote.WithContext(ctx)); err != nil {
		t.Fatalf("copy %s to %s: %v", src, dst, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref := dst.Context().Digest(d.String())
	pub, err := signtest.SignWithNewKey(ctx, ref)
	if err != nil {
		t.Fatalf("sign %s: %v", ref, err)
	}
	return ref.String(), &setecv1alpha1.ImageSignature{PublicKey: pub}
}
