// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"reflect"
	"testing"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/netpol"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// TestPolicyGateOfTheLauncherPod: the setting of the operator reaches the Pod
// spec, and the gate off leaves the Pod with none.
func TestPolicyGateOfTheLauncherPod(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{}
	sb.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone}
	r := &SandboxReconciler{NetPol: testNetPolConfig}

	if g, err := r.policyGate(sb, nil); err != nil || g != nil {
		t.Fatalf("with the gate off: %+v, %v; want none", g, err)
	}
	r.PolicyGate = netpol.GateConfig{Enabled: true, APIServer: "10.43.0.1:443", Wait: 45 * time.Second}
	got, err := r.policyGate(sb, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &podspec.LauncherPolicyGate{Wait: 45 * time.Second, Blocked: []string{"tcp://10.43.0.1:443"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gate %+v, want %+v", got, want)
	}
}
