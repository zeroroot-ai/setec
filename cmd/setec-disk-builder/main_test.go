// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import "testing"

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
