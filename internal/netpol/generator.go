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

// Package netpol contains the pure translator that turns a Sandbox's
// network policy intent into a networkingv1.NetworkPolicy resource. Like
// internal/podspec, the function is side-effect free: the controller owns
// all Kubernetes I/O and ownership references; this package only computes
// shape.
package netpol

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	setecv1alpha1 "github.com/zero-day-ai/setec/api/v1alpha1"
	"github.com/zero-day-ai/setec/internal/podspec"
)

// NetworkPolicySuffix is appended to the Sandbox name to derive the
// NetworkPolicy name (e.g. Sandbox "foo" → NetworkPolicy "foo-netpol").
const NetworkPolicySuffix = "-netpol"

// Errors returned by Generate for inputs the pure function cannot handle.
var (
	// ErrNilSandbox is returned when Generate is invoked with a nil
	// Sandbox pointer.
	ErrNilSandbox = errors.New("netpol: sandbox is nil")

	// ErrUnknownMode is returned when Sandbox.spec.network.mode carries
	// an unexpected value. The CRD enum keeps this unreachable from
	// the API server but the function double-checks.
	ErrUnknownMode = errors.New("netpol: sandbox.spec.network.mode is not a known value")
)

// allCIDR is the IP block covering all IPv4 addresses. Kubernetes
// NetworkPolicy does not express a "match-any-IP" shortcut directly, so
// egress-allow-list rules use this block scoped by port. Operators that
// need stricter controls can supply per-host CIDRs via a future extension.
const allCIDR = "0.0.0.0/0"

// Generate translates a Sandbox's declared network policy into a
// networkingv1.NetworkPolicy. Behavior follows Requirement 4.1/4.2:
//
//   - mode=full (or Network unset): returns (nil, nil). No policy is needed;
//     the namespace's default egress applies.
//   - mode=none: returns a policy selecting the Sandbox Pod with
//     policyTypes=[Ingress, Egress] and empty rules — denying all traffic
//     both ways.
//   - mode=egress-allow-list: returns a policy selecting the Sandbox Pod
//     with policyTypes=[Ingress, Egress], empty ingress (deny-all ingress),
//     and one egress rule per Allow entry. Each allow entry is rendered as
//     an egress rule permitting the entry's port TCP to 0.0.0.0/0; callers
//     operating against a CNI that supports CIDR-based allow lists get
//     filtered egress, and callers operating against a CNI without
//     NetworkPolicy enforcement get advisory-only policies (which is fine).
//
// The returned NetworkPolicy is cluster-ready apart from its
// OwnerReferences, which the controller stamps via
// controllerutil.SetControllerReference. The controller also decides
// whether to Create or Patch; this function never mutates anything.
//
// Generate returns (nil, nil) rather than erroring on mode=full because a
// nil policy is the documented "no policy required" signal the controller
// acts on.
func Generate(sb *setecv1alpha1.Sandbox) (*networkingv1.NetworkPolicy, error) {
	if sb == nil {
		return nil, ErrNilSandbox
	}

	// An absent network block is equivalent to mode=full for generator
	// purposes: the Sandbox accepts unrestricted egress.
	if sb.Spec.Network == nil || sb.Spec.Network.Mode == setecv1alpha1.NetworkModeFull {
		return nil, nil
	}

	switch sb.Spec.Network.Mode {
	case setecv1alpha1.NetworkModeNone:
		return denyAll(sb), nil
	case setecv1alpha1.NetworkModeEgressAllowList:
		return egressAllowList(sb), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, sb.Spec.Network.Mode)
	}
}

// policyFor returns a NetworkPolicy skeleton with the standard metadata
// the operator stamps on every generated policy, plus the pod selector
// that ties the policy to the Sandbox's backing Pod.
func policyFor(sb *setecv1alpha1.Sandbox, policyTypes []networkingv1.PolicyType) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name + NetworkPolicySuffix,
			Namespace: sb.Namespace,
			Labels: map[string]string{
				podspec.SandboxLabelKey: sb.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					podspec.SandboxLabelKey: sb.Name,
				},
			},
			PolicyTypes: policyTypes,
		},
	}
}

// denyAll returns a NetworkPolicy that denies all ingress and egress for
// the selected Pod by setting both policy types and leaving their rule
// lists empty — the K8s NetworkPolicy semantics for "no rules" are
// "deny all" when the corresponding PolicyType is listed.
func denyAll(sb *setecv1alpha1.Sandbox) *networkingv1.NetworkPolicy {
	return policyFor(sb, []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	})
}

// egressAllowList returns a NetworkPolicy that denies all ingress and
// permits egress only on the ports declared in the Sandbox's Allow list.
// Host-based CIDR resolution is deliberately out of scope: operators that
// need hostname-based filtering should layer a CNI with DNS-aware policy
// or pre-resolve hostnames into CIDRs at the Sandbox level.
func egressAllowList(sb *setecv1alpha1.Sandbox) *networkingv1.NetworkPolicy {
	np := policyFor(sb, []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	})

	// Allow DNS resolution so workloads can even attempt outbound
	// traffic. Without this rule, DNS lookups to kube-dns would be
	// blocked and every allow-list becomes functionally a deny-all.
	// The rule matches TCP and UDP 53 egress to any address.
	dnsUDP := networkingv1.NetworkPolicyPort{
		Protocol: ptr.To(corev1.ProtocolUDP),
		Port:     ptr.To(intstr.FromInt32(53)),
	}
	dnsTCP := networkingv1.NetworkPolicyPort{
		Protocol: ptr.To(corev1.ProtocolTCP),
		Port:     ptr.To(intstr.FromInt32(53)),
	}
	dnsRule := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: allCIDR},
		}},
		Ports: []networkingv1.NetworkPolicyPort{dnsUDP, dnsTCP},
	}

	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, 1+len(sb.Spec.Network.Allow))
	egress = append(egress, dnsRule)

	// One egress rule per Allow entry. Port is pulled from the entry;
	// Protocol defaults to TCP because Sandbox.spec.network.allow
	// only carries a port integer today. Future extension can add a
	// Protocol field to NetworkAllow.
	for _, a := range sb.Spec.Network.Allow {
		port := intstr.FromInt32(a.Port)
		rule := networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: allCIDR},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{
				Protocol: ptr.To(corev1.ProtocolTCP),
				Port:     ptr.To(port),
			}},
		}
		// Annotate rule with host for operator-facing clarity. K8s
		// NetworkPolicy does not support hostname-based rules; we
		// record the intent in an annotation so operators can audit
		// what the user requested.
		np.Annotations = appendAnnotation(np.Annotations,
			fmt.Sprintf("setec.zero-day.ai/allow-%d", a.Port),
			a.Host)
		egress = append(egress, rule)
	}

	np.Spec.Egress = egress
	// Ingress is deliberately nil — empty slice means "no ingress
	// rules" which combined with PolicyTypeIngress denies all ingress.
	return np
}

// appendAnnotation returns annotations with the key/value added, creating
// the map if needed. Centralized so the egress-allow-list code path stays
// focused on rule construction.
func appendAnnotation(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

