// Copyright (c) 2026 ZeroRoot
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

// Command lintconfig validates a golangci-lint YAML configuration against a
// vendored copy of the golangci-lint JSON schema.
//
// It replaces `golangci-lint config verify`, which downloads the schema from
// golangci-lint.run on every invocation — a network dependency that fails the
// required lint gate whenever the site is slow or unreachable (setec#260).
// The validation library is the same one golangci-lint itself uses, so the
// verdict is identical; only the transport changed.
//
// This is a module of its own so the lint guard's dependencies stay out of
// github.com/zeroroot-ai/setec.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

// schemaResourceID is the identity the schema document is registered under.
// The document is self-contained (no external $ref), so nothing is ever
// resolved over the network.
const schemaResourceID = "https://json.schemastore.org/golangci-lint.json"

func main() {
	schemaPath := flag.String("schema", "", "path to the vendored golangci-lint JSON schema")
	configPath := flag.String("config", "", "path to the golangci-lint YAML config to verify")
	wantVersion := flag.String("expect-version", "", "config format version the pinned golangci-lint accepts, e.g. 2")
	flag.Parse()

	if *schemaPath == "" || *configPath == "" || *wantVersion == "" {
		fmt.Fprintln(os.Stderr, "usage: lintconfig -schema <schema.json> -config <.golangci.yml> -expect-version <major>")
		os.Exit(2)
	}

	if err := verify(*schemaPath, *configPath, *wantVersion); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %s is invalid:\n%v\n", *configPath, err)
		os.Exit(1)
	}

	fmt.Printf("✅ %s validates against %s\n", *configPath, *schemaPath)
}

func verify(schemaPath, configPath, wantVersion string) error {
	schema, err := loadSchema(schemaPath)
	if err != nil {
		return err
	}

	instance, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	// The schema does not constrain `version` — upstream rejects a wrong one
	// in the config loader instead. Assert it here so this guard is not
	// weaker than the `golangci-lint config verify` it replaces.
	if err := checkVersion(instance, wantVersion); err != nil {
		return err
	}

	return schema.Validate(instance)
}

func checkVersion(instance any, want string) error {
	doc, ok := instance.(map[string]any)
	if !ok {
		return fmt.Errorf("config root is %T, want a mapping", instance)
	}

	got, ok := doc["version"]
	if !ok {
		return fmt.Errorf("config has no `version` key; the pinned golangci-lint needs version %q", want)
	}

	if fmt.Sprintf("%v", got) != want {
		return fmt.Errorf("config version is %v, but the pinned golangci-lint accepts %q", got, want)
	}

	return nil
}

func loadSchema(path string) (*jsonschema.Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", path, err)
	}

	compiler := jsonschema.NewCompiler()
	// No URL loader is registered: a $ref that points off this document is a
	// compile error rather than a silent network fetch.
	if err := compiler.AddResource(schemaResourceID, doc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}

	schema, err := compiler.Compile(schemaResourceID)
	if err != nil {
		return nil, fmt.Errorf("compile schema %s: %w", path, err)
	}

	return schema, nil
}

func loadConfig(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("config is empty")
	}

	asJSON, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	return instance, nil
}
