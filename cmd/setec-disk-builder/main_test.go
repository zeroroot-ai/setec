// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"strings"
	"testing"
)

// TestCheckRegistryAuthUse: a cloud provider is for the commands that read an
// image. check and publish hold the push credential, so they refuse it.
func TestCheckRegistryAuthUse(t *testing.T) {
	t.Parallel()
	for command, wantErr := range map[string]bool{"build": false, "verify": false, "check": true, "publish": true} {
		err := checkRegistryAuthUse(command, []string{"ecr"})
		if (err != nil) != wantErr {
			t.Errorf("%s with a provider: err = %v, want error %t", command, err, wantErr)
		}
		if err := checkRegistryAuthUse(command, nil); err != nil {
			t.Errorf("%s with no provider: err = %v, want none", command, err)
		}
	}
}

func TestPullKeychain(t *testing.T) {
	t.Parallel()
	if _, err := pullKeychain(nil); err != nil {
		t.Fatalf("no provider: %v", err)
	}
	if _, err := pullKeychain([]string{"ecr"}); err != nil {
		t.Fatalf("ecr: %v", err)
	}
	if _, err := pullKeychain([]string{"nope"}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

// TestCheckPushAuthUse: the push identity is for the commands that push. build
// and verify read the image of a tenant, so they refuse it.
func TestCheckPushAuthUse(t *testing.T) {
	t.Parallel()
	for command, wantErr := range map[string]bool{"build": true, "verify": true, "check": false, "publish": false} {
		err := checkPushAuthUse(command, []string{"ecr"})
		if (err != nil) != wantErr {
			t.Errorf("%s with a push provider: err = %v, want error %t", command, err, wantErr)
		}
		if err := checkPushAuthUse(command, nil); err != nil {
			t.Errorf("%s with no push provider: err = %v, want none", command, err)
		}
	}
}

// TestTheTwoSidesRefuseEachOthersProviders: a command takes the provider flag
// of its own side only. No command takes both.
func TestTheTwoSidesRefuseEachOthersProviders(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"check", "build", "publish", "verify"} {
		pull := checkRegistryAuthUse(command, []string{"ecr"}) == nil
		push := checkPushAuthUse(command, []string{"ecr"}) == nil
		if pull && push {
			t.Errorf("%s takes both a pull and a push provider", command)
		}
		if !pull && !push {
			t.Errorf("%s takes neither provider flag", command)
		}
	}
}

func TestPushKeychain(t *testing.T) {
	t.Parallel()
	if _, err := pushKeychain(nil); err != nil {
		t.Fatalf("no provider: %v", err)
	}
	if _, err := pushKeychain([]string{"ecr"}); err != nil {
		t.Fatalf("ecr: %v", err)
	}
	if _, err := pushKeychain([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "--push-auth") {
		t.Fatalf("an unknown provider: err = %v; want one that names --push-auth", err)
	}
}
