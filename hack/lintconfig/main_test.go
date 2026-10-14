// Copyright 2026 The Setec Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// schemaFile is the vendored schema that make lint-config passes.
const schemaFile = "golangci.v2.14.jsonschema.json"

// TestVerify_RefusesABrokenConfig plants each defect the guard exists to
// catch, and requires verify to refuse it (gibson#807 condition 7). The
// first case is the repository config, which must pass.
func TestVerify_RefusesABrokenConfig(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{name: "a valid config", config: "version: \"2\"\nlinters:\n  default: none\n  enable: [govet]\n"},
		{name: "an unknown top-level key", config: "version: \"2\"\nlinterz:\n  default: none\n", wantErr: true},
		{name: "an unknown key under linters", config: "version: \"2\"\nlinters:\n  enabel: [govet]\n", wantErr: true},
		{name: "a wrong version", config: "version: \"1\"\n", wantErr: true},
		{name: "no version", config: "linters:\n  default: none\n", wantErr: true},
		{name: "an empty file", config: "\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".golangci.yml")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatalf("write the config: %v", err)
			}
			err := verify(schemaFile, path, "2")
			if tc.wantErr && err == nil {
				t.Fatalf("verify passed %s; it must refuse it", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verify refused %s: %v", tc.name, err)
			}
		})
	}
}

// TestVerify_TheRepositoryConfigPasses keeps the fixture honest: the real
// config passes the same call that refuses each planted defect.
func TestVerify_TheRepositoryConfigPasses(t *testing.T) {
	if err := verify(schemaFile, filepath.Join("..", "..", ".golangci.yml"), "2"); err != nil {
		t.Fatalf("the repository config fails: %v", err)
	}
}
