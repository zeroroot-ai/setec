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

// Package runtime provides the RuntimeDispatcher abstraction, registry, and
// operator-wide configuration loader used by the Setec operator and admission
// webhook to select, validate, and execute isolation backends.
package runtime

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Known backend name constants.  These are the only values permitted in
// BackendConfig keys, SandboxClassRuntime.Backend, and fallback lists.
const (
	BackendKataFC   = "kata-fc"
	BackendKataQEMU = "kata-qemu"
	BackendGVisor   = "gvisor"
	BackendRunc     = "runc"
)

// AllKnownBackends is the full set of backends Setec understands, sorted
// alphabetically.  Callers may use this slice for validation without hard-
// coding strings; it is not a registry of enabled backends.
var AllKnownBackends = []string{
	BackendGVisor,
	BackendKataFC,
	BackendKataQEMU,
	BackendRunc,
}

// RuntimeConfig is the operator-internal configuration loaded from the file
// passed via --runtimes-config.  It mirrors the Helm values block under
// "runtimes" and "defaults".
type RuntimeConfig struct {
	Runtimes map[string]BackendConfig `yaml:"runtimes" json:"runtimes"`
	Defaults DefaultsConfig           `yaml:"defaults" json:"defaults"`
}

// BackendConfig holds the per-backend operator configuration sourced from
// Helm values.
type BackendConfig struct {
	// Enabled controls whether this backend is available for use.  When false
	// the admission webhook rejects SandboxClass specs that reference it.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// RuntimeClassName is the Kubernetes RuntimeClass name the Pod spec will
	// reference for this backend (e.g. "kata-fc", "gvisor").
	RuntimeClassName string `yaml:"runtimeClassName" json:"runtimeClassName"`

	// DevOnly gates the backend so it is only usable from namespaces carrying
	// the label setec.zeroroot.ai/allow-dev-runtimes=true.  Intended for runc.
	DevOnly bool `yaml:"devOnly,omitempty" json:"devOnly,omitempty"`

	// DefaultOverhead is the resource overhead the backend's Dispatcher returns
	// when no per-SandboxClass overhead is set.  Matches the corev1.Pod
	// Overhead field format (resource name → quantity).
	DefaultOverhead corev1.ResourceList `yaml:"defaultOverhead,omitempty" json:"defaultOverhead,omitempty"`
}

// DefaultsConfig holds operator-wide runtime defaults.
type DefaultsConfig struct {
	Runtime RuntimeDefaults `yaml:"runtime" json:"runtime"`
}

// RuntimeDefaults captures cluster-wide fallback and probing settings.
type RuntimeDefaults struct {
	// Backend is the cluster-default isolation backend applied when a Sandbox
	// does not reference a SandboxClass or when a SandboxClass's runtime.backend
	// is unset.
	Backend string `yaml:"backend" json:"backend"`

	// Fallback is the ordered list of backends to try when the primary backend
	// has no capable node.  Each entry must be enabled.
	Fallback []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`

	// ProbeInterval is the period between node-agent capability re-probes.
	// Zero (default) means the node-agent uses its built-in default of 5m.
	ProbeInterval time.Duration `yaml:"probeInterval,omitempty" json:"probeInterval,omitempty"`

	// NodeCapabilitiesMode is either "probe" (default, node-agent DaemonSet
	// probes each node) or "static" (operator trusts StaticCapabilities and the
	// DaemonSet exits immediately).
	NodeCapabilitiesMode string `yaml:"nodeCapabilitiesMode,omitempty" json:"nodeCapabilitiesMode,omitempty"`

	// StaticCapabilities maps node name to the list of backends it supports.
	// Used only when NodeCapabilitiesMode is "static".
	StaticCapabilities map[string][]string `yaml:"staticCapabilities,omitempty" json:"staticCapabilities,omitempty"`
}

// ConfigValidationError is a typed error that points at the YAML key whose
// value failed validation.  Multiple ConfigValidationErrors may be joined with
// errors.Join so callers can report all problems in one message.
type ConfigValidationError struct {
	// Field is the dotted YAML key path (e.g. "runtimes.kata-fc.enabled").
	Field string
	// Detail describes the problem in one sentence.
	Detail string
}

func (e *ConfigValidationError) Error() string {
	return fmt.Sprintf("config validation error at %q: %s", e.Field, e.Detail)
}

// LoadFromFile reads and YAML-unmarshals the file at path into a RuntimeConfig
// and then calls Validate on the result.  It returns a non-nil error for any
// I/O, parse, or validation failure.
func LoadFromFile(path string) (*RuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading runtimes config %q: %w", path, err)
	}
	var cfg RuntimeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing runtimes config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks that the RuntimeConfig satisfies all invariants required
// before the operator accepts it.  All violations are collected and returned
// as a single joined error so operators see every problem at once.
//
// Rules:
//  1. At least one backend must have enabled=true.
//  2. defaults.runtime.backend must name a backend in the enabled set.
//  3. Every entry in defaults.runtime.fallback must be enabled.
//  4. defaults.runtime.nodeCapabilitiesMode must be "", "probe", or "static".
func (c *RuntimeConfig) Validate() error {
	var errs []error

	enabled := c.enabledSet()

	if len(enabled) == 0 {
		errs = append(errs, &ConfigValidationError{
			Field:  "runtimes",
			Detail: "at least one runtime must have enabled=true (REQ-4.5)",
		})
		// The remaining checks depend on the enabled set being non-empty; bail
		// early so we don't emit misleading secondary errors.
		return errors.Join(errs...)
	}

	defaultBackend := c.Defaults.Runtime.Backend
	if defaultBackend == "" {
		errs = append(errs, &ConfigValidationError{
			Field:  "defaults.runtime.backend",
			Detail: "must be non-empty and name an enabled backend",
		})
	} else if !enabled[defaultBackend] {
		errs = append(errs, &ConfigValidationError{
			Field:  "defaults.runtime.backend",
			Detail: fmt.Sprintf("backend %q is not enabled; enable it via runtimes.%s.enabled=true", defaultBackend, defaultBackend),
		})
	}

	for i, fb := range c.Defaults.Runtime.Fallback {
		if !enabled[fb] {
			errs = append(errs, &ConfigValidationError{
				Field:  fmt.Sprintf("defaults.runtime.fallback[%d]", i),
				Detail: fmt.Sprintf("backend %q is not enabled; enable it or remove it from the fallback list", fb),
			})
		}
	}

	mode := c.Defaults.Runtime.NodeCapabilitiesMode
	switch mode {
	case "", "probe", "static":
		// valid
	default:
		errs = append(errs, &ConfigValidationError{
			Field:  "defaults.runtime.nodeCapabilitiesMode",
			Detail: fmt.Sprintf("invalid value %q; must be one of: probe, static (or omit for probe default)", mode),
		})
	}

	return errors.Join(errs...)
}

// EnabledBackends returns the sorted list of backend names whose enabled flag
// is true.  The order is deterministic (alphabetical) regardless of map
// iteration.
func (c *RuntimeConfig) EnabledBackends() []string {
	enabled := make([]string, 0, len(c.Runtimes))
	for name, bc := range c.Runtimes {
		if bc.Enabled {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	return enabled
}

// enabledSet returns the enabled backends as a set (map to bool) for O(1)
// lookup during validation.
func (c *RuntimeConfig) enabledSet() map[string]bool {
	set := make(map[string]bool, len(c.Runtimes))
	for name, bc := range c.Runtimes {
		if bc.Enabled {
			set[name] = true
		}
	}
	return set
}
