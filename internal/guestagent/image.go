// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// imageConfigPath is where the disk builder writes the image config
// (diskbuilder.ImageConfigPath).
const imageConfigPath = ".setec/image.json"

// imageConfig mirrors diskbuilder.ImageConfig. The guest agent does not
// import the builder, which pulls registry code into the machine.
type imageConfig struct {
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env"`
	User       string   `json:"user"`
	WorkingDir string   `json:"workingDir"`
}

// WithImageDefaults fills p from the image config of the disk under root,
// as a container runtime does: no argv runs the entry point and command of
// the image, the environment of p is laid over the image environment, and
// an empty user or directory takes the image value.
func WithImageDefaults(root string, p Process) Process {
	raw, err := os.ReadFile(filepath.Join(root, imageConfigPath)) //nolint:gosec // a fixed path in the root
	if err != nil {
		return p
	}
	var ic imageConfig
	if json.Unmarshal(raw, &ic) != nil {
		return p
	}
	if len(p.Argv) == 0 {
		p.Argv = append(append([]string{}, ic.Entrypoint...), ic.Cmd...)
	}
	if p.User == "" {
		p.User = ic.User
	}
	if p.Dir == "" {
		p.Dir = ic.WorkingDir
	}
	env := append([]string{}, ic.Env...)
	for _, e := range p.Env {
		key, _, _ := strings.Cut(e, "=")
		replaced := false
		for i, base := range env {
			if strings.HasPrefix(base, key+"=") {
				env[i], replaced = e, true
			}
		}
		if !replaced {
			env = append(env, e)
		}
	}
	p.Env = env
	return p
}
