// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package netpol

import (
	"reflect"
	"testing"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func gateCfg() Config {
	return Config{
		ReservedCIDRs: []string{"10.0.0.0/8", "fc00::/7"},
		ResolverIPs:   []string{"1.1.1.1"},
	}
}

func sandboxWithMode(m setecv1alpha1.NetworkMode) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{}
	sb.Spec.Network = &setecv1alpha1.Network{Mode: m}
	return sb
}

func TestGateFor(t *testing.T) {
	t.Parallel()
	on := GateConfig{Enabled: true, APIServer: "10.43.0.1:443"}
	for name, tc := range map[string]struct {
		mode  setecv1alpha1.NetworkMode
		gate  GateConfig
		class *setecv1alpha1.SandboxClass
		want  *Gate
	}{
		"none blocks the API Service and allows nothing": {
			mode: setecv1alpha1.NetworkModeNone, gate: on,
			want: &Gate{Wait: DefaultGateWait, Blocked: []string{"tcp://10.43.0.1:443"}},
		},
		"external-only blocks a Service in the reserved ranges and allows the resolver": {
			mode: setecv1alpha1.NetworkModeExternalOnly, gate: on,
			want: &Gate{Wait: DefaultGateWait, Blocked: []string{"tcp://10.43.0.1:443"}, Allowed: []string{"dns://1.1.1.1:53"}},
		},
		"a Service outside the reserved ranges is no proof of a block": {
			mode: setecv1alpha1.NetworkModeEgressAllowList,
			gate: GateConfig{Enabled: true, APIServer: "100.64.0.1:443", Wait: time.Minute},
			want: &Gate{Wait: time.Minute, Allowed: []string{"dns://1.1.1.1:53"}},
		},
		"a class that exempts the range of the Service gives no block probe": {
			mode: setecv1alpha1.NetworkModeExternalOnly, gate: on,
			class: &setecv1alpha1.SandboxClass{Spec: setecv1alpha1.SandboxClassSpec{EgressExemptCIDRs: []string{"10.43.0.0/16"}}},
			want:  &Gate{Wait: DefaultGateWait, Allowed: []string{"dns://1.1.1.1:53"}},
		},
		"extra destinations of the operator are added": {
			mode: setecv1alpha1.NetworkModeExternalOnly,
			gate: GateConfig{Enabled: true, ExtraBlocked: []string{"10.1.1.1:22"}, ExtraAllowed: []string{"192.0.2.9:443"}},
			want: &Gate{Wait: DefaultGateWait, Blocked: []string{"tcp://10.1.1.1:22"},
				Allowed: []string{"dns://1.1.1.1:53", "tcp://192.0.2.9:443"}},
		},
		"off gives no gate": {
			mode: setecv1alpha1.NetworkModeNone, gate: GateConfig{APIServer: "10.43.0.1:443"},
			want: nil,
		},
	} {
		got, err := gateCfg().GateFor(tc.gate, sandboxWithMode(tc.mode), tc.class)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got, tc.want)
		}
	}
}

func TestGateConfigValidate(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]GateConfig{
		"a name, not an IP": {ExtraBlocked: []string{"kubernetes.default:443"}},
		"no port":           {ExtraAllowed: []string{"10.0.0.1"}},
		"bad API Service":   {APIServer: "nope"},
		"negative wait":     {Wait: -time.Second},
	} {
		if err := g.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (GateConfig{APIServer: "10.43.0.1:443", ExtraBlocked: []string{"[fd00::1]:22"}}).Validate(); err != nil {
		t.Errorf("a good setting is refused: %v", err)
	}
}
