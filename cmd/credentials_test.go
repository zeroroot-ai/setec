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

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The operator's client hop to the node-agents is what carries a
// snapshot instruction to a node. Its flags had no test, and a
// half-configured credential used to surface as an "open :" error from
// deep inside crypto/tls. What is asserted here is that the operator
// refuses to start, and says which flags it wanted.

func TestNodeAgentCredentialFlags_RejectsIncompleteFlags(t *testing.T) {
	t.Parallel()
	tests := map[string]nodeAgentCredentialFlags{
		"no cert":     {keyPath: "k.pem", caPath: "ca.pem"},
		"no key":      {certPath: "c.pem", caPath: "ca.pem"},
		"no ca":       {certPath: "c.pem", keyPath: "k.pem"},
		"none at all": {},
	}
	for name, flags := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := flags.credentialConfig()
			if err == nil {
				t.Fatalf("credentialConfig(%+v): want error, got nil", flags)
			}
			for _, want := range []string{
				"--nodeagent-tls-cert", "--nodeagent-tls-key", "--nodeagent-ca", "mTLS is mandatory",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestNodeAgentCredentialFlags_AcceptsCompleteFlags(t *testing.T) {
	t.Parallel()
	flags := nodeAgentCredentialFlags{certPath: "c.pem", keyPath: "k.pem", caPath: "ca.pem"}
	cfg, err := flags.credentialConfig()
	if err != nil {
		t.Fatalf("credentialConfig(%+v): %v", flags, err)
	}
	if cfg.Files == nil {
		t.Fatal("complete flags produced no file credential source")
	}
	if cfg.Files.CertFile != "c.pem" || cfg.Files.KeyFile != "k.pem" || cfg.Files.CAFile != "ca.pem" {
		t.Fatalf("flags reached the credential source scrambled: %+v", *cfg.Files)
	}
}

func TestNodeAgentClientCredentials_MissingKeypairNamesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := nodeAgentClientCredentials(t.Context(), nodeAgentCredentialFlags{
		certPath: filepath.Join(dir, "absent.crt"),
		keyPath:  filepath.Join(dir, "absent.key"),
		caPath:   filepath.Join(dir, "absent-ca.pem"),
	})
	if err == nil {
		t.Fatal("want error for an absent keypair, got nil")
	}
	if !strings.Contains(err.Error(), "absent.crt") {
		t.Fatalf("error = %q, want it to name the missing certificate file", err)
	}
}
