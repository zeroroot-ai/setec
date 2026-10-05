// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package tenancy

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFromNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ns        *corev1.Namespace
		labelKey  string
		want      TenantID
		wantErrIs error
	}{
		{
			name: "label set",
			ns: &corev1.Namespace{
				Name:   "tenant-a",
				Labels: map[string]string{"setec.zeroroot.ai/tenant": "tenant-a"},
			},
			labelKey: "setec.zeroroot.ai/tenant",
			want:     "tenant-a",
		},
		{
			name: "label missing",
			ns: &corev1.Namespace{
				Name:   "nolabel",
				Labels: map[string]string{"other": "value"},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "label present but empty",
			ns: &corev1.Namespace{
				Name:   "empty",
				Labels: map[string]string{"setec.zeroroot.ai/tenant": ""},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "label value not a DNS label",
			ns: &corev1.Namespace{
				Name:   "bad",
				Labels: map[string]string{"setec.zeroroot.ai/tenant": "NOT_VALID"},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantInvalid,
		},
		{
			name:      "nil namespace",
			ns:        nil,
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "empty label key",
			ns: &corev1.Namespace{
				Name:   "ns",
				Labels: map[string]string{"setec.zeroroot.ai/tenant": "tenant-a"},
			},
			labelKey:  "",
			wantErrIs: ErrTenantLabelMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromNamespace(tc.ns, tc.labelKey)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tc.wantErrIs)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewPair(t *testing.T) {
	p, err := NewPair("cluster-a", "acme")
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	if p.String() != "cluster-a/acme" {
		t.Errorf("String = %q", p.String())
	}
	if got := p.Labels(); got[ClientLabelKey] != "cluster-a" || got[TenantLabelKey] != "acme" {
		t.Errorf("Labels = %v", got)
	}
	for _, tc := range []struct{ name, client, tenant string }{
		{"empty client", "", "acme"},
		{"empty tenant", "cluster-a", ""},
		{"tenant with a slash", "cluster-a", "acme/other"},
		{"client in capitals", "Cluster-A", "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPair(tc.client, tc.tenant); !errors.Is(err, ErrTenantInvalid) {
				t.Errorf("NewPair(%q, %q) error = %v, want ErrTenantInvalid", tc.client, tc.tenant, err)
			}
		})
	}
}
