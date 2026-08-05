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
//
// The package holds one invariant: for a non-nil Sandbox, Generate always
// returns a policy. There is no input — no mode, no absent network block,
// no absent class — that yields "no policy". A Sandbox the operator cannot
// describe a posture for gets deny-all, not unrestricted egress.
package netpol

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// NetworkPolicySuffix is appended to the Sandbox name to derive the
// NetworkPolicy name (e.g. Sandbox "foo" → NetworkPolicy "foo-netpol").
const NetworkPolicySuffix = "-netpol"

// AllCIDR is the IP block covering all IPv4 addresses. It is the base of
// every permissive egress rule; the operator's reserved ranges are then
// subtracted from it via ipBlock.except.
const AllCIDR = "0.0.0.0/0"

// annotationPrefixAllow prefixes the per-port annotation recording the
// host a caller declared. Kubernetes NetworkPolicy cannot match a DNS
// name, so the declared intent is preserved for audit only.
const annotationPrefixAllow = "setec.zeroroot.ai/allow-"

// AnnotationSuppressed records allow-list entries that were dropped
// because the operator's reserved ranges cover the requested block
// entirely. Surfacing this is what stops a silently-empty policy from
// looking like a working one.
const AnnotationSuppressed = "setec.zeroroot.ai/suppressed-allow"

// Errors returned by Generate for inputs the pure function cannot handle.
var (
	// ErrNilSandbox is returned when Generate is invoked with a nil
	// Sandbox pointer.
	ErrNilSandbox = errors.New("netpol: sandbox is nil")

	// ErrUnknownMode is returned when Sandbox.spec.network.mode carries
	// an unexpected value. The CRD enum keeps this unreachable from
	// the API server but the function double-checks.
	ErrUnknownMode = errors.New("netpol: sandbox.spec.network.mode is not a known value")

	// ErrNoReservedCIDRs is returned by Config.Validate when the
	// reserved list is empty. An empty reserved list means every
	// permissive rule resolves to a bare 0.0.0.0/0 — the unrestricted
	// posture this package exists to prevent — so it is rejected at
	// startup rather than silently honoured.
	ErrNoReservedCIDRs = errors.New("netpol: at least one reserved CIDR is required")

	// ErrNoResolvers is returned by Config.Validate when no DNS
	// resolver addresses are configured. Sandboxes resolve names
	// through explicitly configured resolvers, never through
	// cluster DNS, so the list cannot be empty.
	ErrNoResolvers = errors.New("netpol: at least one sandbox DNS resolver is required")

	// ErrInvalidCIDR is returned when a configured or declared address
	// block does not parse.
	ErrInvalidCIDR = errors.New("netpol: invalid CIDR")

	// ErrInvalidResolver is returned when a configured resolver is not
	// a bare IP address.
	ErrInvalidResolver = errors.New("netpol: invalid resolver address")
)

// Config carries the cluster-wide egress posture the operator was started
// with. It is injected by the controller so this package stays pure and so
// the policy and the Pod's resolver configuration are guaranteed to come
// from the same source.
type Config struct {
	// ReservedCIDRs are the address ranges no Sandbox may reach. They
	// are subtracted from every permissive egress rule via
	// ipBlock.except. The operator default covers private, link-local,
	// carrier-grade-NAT, loopback and multicast space; a cluster
	// operator adds the Service and Pod CIDRs of their own cluster.
	ReservedCIDRs []string

	// ResolverIPs are the DNS servers Sandboxes are permitted to query.
	// The same list is written into the Pod's dnsConfig, so a Sandbox
	// resolves names exclusively through these addresses and never
	// enumerates in-cluster Services through cluster DNS.
	ResolverIPs []string
}

// Validate reports whether the Config expresses a usable posture. It is
// called once at operator startup: a Config that would degrade to
// unrestricted egress must fail the process, not a single reconcile.
func (c Config) Validate() error {
	if len(c.ReservedCIDRs) == 0 {
		return ErrNoReservedCIDRs
	}
	for _, cidr := range c.ReservedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("%w: reserved %q: %w", ErrInvalidCIDR, cidr, err)
		}
	}
	if len(c.ResolverIPs) == 0 {
		return ErrNoResolvers
	}
	for _, ip := range c.ResolverIPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInvalidResolver, ip, err)
		}
	}
	return nil
}

// Generate translates a Sandbox's declared network policy into a
// networkingv1.NetworkPolicy. Behaviour:
//
//   - Network unset: treated as mode=none. Callers that want a class
//     default applied must go through GenerateForClass.
//   - mode=none: a policy selecting the Sandbox Pod with
//     policyTypes=[Ingress, Egress] and empty rules — denying all traffic
//     both ways.
//   - mode=external-only: deny-all ingress; egress to 0.0.0.0/0 on every
//     port with the operator's reserved ranges subtracted, plus DNS to
//     the configured resolvers. This is the posture for workloads that
//     must reach arbitrary external endpoints.
//   - mode=egress-allow-list: deny-all ingress; one port-scoped egress
//     rule per Allow entry, each carrying the same reserved-range
//     subtraction, plus DNS to the configured resolvers.
//
// The returned NetworkPolicy is cluster-ready apart from its
// OwnerReferences, which the controller stamps via
// controllerutil.SetControllerReference. The controller also decides
// whether to Create or Patch; this function never mutates anything.
//
// Generate never returns a nil policy together with a nil error.
func (c Config) Generate(sb *setecv1alpha1.Sandbox) (*networkingv1.NetworkPolicy, error) {
	return c.generate(sb, nil)
}

// GenerateForClass is the class-aware entry point (ADR-0052, setec#66). It
// resolves the Sandbox's effective network posture, applying the
// SandboxClass default when the Sandbox does not declare its own
// spec.network, then produces the policy.
//
// A Sandbox that DOES declare spec.network always wins — the class default
// only fills the gap, it never overrides an explicit choice (which the
// admission webhook separately constrains to the class's
// AllowedNetworkModes). A nil class, or a class with no declared default,
// resolves to mode=none: the absence of a stated posture is not a licence
// to skip the policy.
//
// GenerateForClass never mutates sb or class.
func (c Config) GenerateForClass(sb *setecv1alpha1.Sandbox, class *setecv1alpha1.SandboxClass) (*networkingv1.NetworkPolicy, error) {
	return c.generate(sb, class)
}

// generate is the single implementation behind both entry points. It
// resolves the effective posture, then dispatches on mode.
func (c Config) generate(sb *setecv1alpha1.Sandbox, class *setecv1alpha1.SandboxClass) (*networkingv1.NetworkPolicy, error) {
	if sb == nil {
		return nil, ErrNilSandbox
	}

	mode, allow := effectivePosture(sb, class)

	// A class may exempt specific ranges from the cluster-wide reserved
	// list. Exemptions apply only to that class's Sandboxes.
	var exempt []string
	if class != nil {
		exempt = class.Spec.EgressExemptCIDRs
	}
	reserved, err := subtractExempt(c.ReservedCIDRs, exempt)
	if err != nil {
		return nil, err
	}

	switch mode {
	case setecv1alpha1.NetworkModeNone:
		return denyAll(sb), nil
	case setecv1alpha1.NetworkModeExternalOnly:
		return c.externalOnly(sb, reserved)
	case setecv1alpha1.NetworkModeEgressAllowList:
		return c.egressAllowList(sb, allow, reserved)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, mode)
	}
}

// effectivePosture resolves the mode and allow-list actually in force for
// a Sandbox. An explicit spec.network wins; otherwise the class default
// applies; otherwise the posture is none.
func effectivePosture(
	sb *setecv1alpha1.Sandbox,
	class *setecv1alpha1.SandboxClass,
) (setecv1alpha1.NetworkMode, []setecv1alpha1.NetworkAllow) {
	if sb.Spec.Network != nil {
		return sb.Spec.Network.Mode, sb.Spec.Network.Allow
	}
	if class != nil && class.Spec.DefaultNetworkMode != "" {
		return class.Spec.DefaultNetworkMode, class.Spec.DefaultEgressAllow
	}
	return setecv1alpha1.NetworkModeNone, nil
}

// EffectiveMode reports the network mode that will actually be enforced
// for sb under class. The admission webhook uses it so that a Sandbox
// which omits spec.network is still checked against the class's
// AllowedNetworkModes, rather than bypassing the check entirely.
func EffectiveMode(sb *setecv1alpha1.Sandbox, class *setecv1alpha1.SandboxClass) setecv1alpha1.NetworkMode {
	if sb == nil {
		return setecv1alpha1.NetworkModeNone
	}
	mode, _ := effectivePosture(sb, class)
	return mode
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

// bothDirections is the policy-type list every generated policy carries.
// Listing Egress is what makes an empty egress rule set mean deny-all;
// listing Ingress denies all inbound traffic in every mode, because
// nothing ever dials into a Sandbox.
func bothDirections() []networkingv1.PolicyType {
	return []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	}
}

// denyAll returns a NetworkPolicy that denies all ingress and egress for
// the selected Pod by setting both policy types and leaving their rule
// lists empty — the K8s NetworkPolicy semantics for "no rules" are
// "deny all" when the corresponding PolicyType is listed.
func denyAll(sb *setecv1alpha1.Sandbox) *networkingv1.NetworkPolicy {
	return policyFor(sb, bothDirections())
}

// externalOnly returns a policy permitting egress to public address space
// on every port while denying every reserved range, plus DNS to the
// configured resolvers.
//
// The rule deliberately carries no Ports: workloads under this posture
// probe arbitrary destination ports, and constraining them would defeat
// the purpose. Confinement comes from the address space the rule cannot
// reach, not from the port set it may use.
func (c Config) externalOnly(sb *setecv1alpha1.Sandbox, reserved []string) (*networkingv1.NetworkPolicy, error) {
	except, covered, err := exceptFor(AllCIDR, reserved)
	if err != nil {
		return nil, err
	}
	np := policyFor(sb, bothDirections())

	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, 2)
	egress = append(egress, c.dnsRule())
	if !covered {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: AllCIDR, Except: except},
			}},
		})
	}

	np.Spec.Egress = egress
	return np, nil
}

// egressAllowList returns a policy that denies all ingress and permits
// egress only on the declared destinations, each rule scoped to its port
// and stripped of the reserved ranges.
//
// The declared Host is recorded on an annotation rather than in a rule:
// Kubernetes NetworkPolicy has no hostname matcher, and resolving the
// name here would bake a DNS answer into a long-lived object. Callers
// that must narrow a rule to real addresses set NetworkAllow.CIDR.
func (c Config) egressAllowList(
	sb *setecv1alpha1.Sandbox,
	allow []setecv1alpha1.NetworkAllow,
	reserved []string,
) (*networkingv1.NetworkPolicy, error) {
	np := policyFor(sb, bothDirections())

	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, 1+len(allow))
	egress = append(egress, c.dnsRule())

	var suppressed []string
	for _, a := range allow {
		base := a.CIDR
		if base == "" {
			base = AllCIDR
		}
		except, covered, err := exceptFor(base, reserved)
		if err != nil {
			return nil, err
		}

		// Record the declared intent for audit regardless of whether the
		// rule survives, so an operator reading the policy sees what was
		// asked for as well as what was granted.
		np.Annotations = appendAnnotation(np.Annotations,
			fmt.Sprintf("%s%d", annotationPrefixAllow, a.Port), a.Host)

		if covered {
			// The requested block lies entirely inside a reserved range.
			// Emitting the rule would produce an ipBlock whose except
			// equals its cidr; dropping it is the same posture stated
			// honestly.
			suppressed = append(suppressed, fmt.Sprintf("%s:%d", a.Host, a.Port))
			continue
		}

		port := intstr.FromInt32(a.Port)
		proto := corev1.ProtocolTCP
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: base, Except: except},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{
				Protocol: &proto,
				Port:     &port,
			}},
		})
	}

	if len(suppressed) > 0 {
		np.Annotations = appendAnnotation(np.Annotations,
			AnnotationSuppressed, strings.Join(suppressed, ", "))
	}

	np.Spec.Egress = egress
	// Ingress is deliberately nil — no ingress rules combined with
	// PolicyTypeIngress denies all inbound traffic.
	return np, nil
}

// dnsRule permits UDP and TCP 53 to exactly the configured resolvers.
//
// Targeting named resolvers rather than any address is what keeps a
// Sandbox off cluster DNS: it cannot resolve in-cluster Service names, so
// it cannot enumerate the control plane by name even before any packet is
// dropped. The Pod's dnsConfig points at the same addresses.
func (c Config) dnsRule() networkingv1.NetworkPolicyEgressRule {
	protoUDP, protoTCP := corev1.ProtocolUDP, corev1.ProtocolTCP
	port53UDP, port53TCP := intstr.FromInt32(53), intstr.FromInt32(53)

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(c.ResolverIPs))
	for _, ip := range c.ResolverIPs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: hostCIDR(ip)},
		})
	}

	return networkingv1.NetworkPolicyEgressRule{
		To: peers,
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &protoUDP, Port: &port53UDP},
			{Protocol: &protoTCP, Port: &port53TCP},
		},
	}
}

// hostCIDR renders a bare IP address as a single-host prefix. An
// unparseable address is returned unchanged so that Config.Validate is
// the single place that reports the problem.
func hostCIDR(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	return netip.PrefixFrom(addr, addr.BitLen()).String()
}

// exceptFor computes the ipBlock.except list for a rule whose base block
// is base. Kubernetes requires every except entry to fall within the
// block it modifies, so reserved ranges that do not nest inside base are
// dropped — they are already unreachable through this rule.
//
// covered reports that base lies entirely inside a reserved range, which
// means the rule grants nothing and the caller should omit it.
func exceptFor(base string, reserved []string) (except []string, covered bool, err error) {
	basePrefix, err := netip.ParsePrefix(base)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %q: %w", ErrInvalidCIDR, base, err)
	}
	basePrefix = basePrefix.Masked()

	for _, r := range reserved {
		rp, perr := netip.ParsePrefix(r)
		if perr != nil {
			return nil, false, fmt.Errorf("%w: reserved %q: %w", ErrInvalidCIDR, r, perr)
		}
		rp = rp.Masked()

		switch {
		case rp.Overlaps(basePrefix) && rp.Bits() <= basePrefix.Bits():
			// The reserved range contains (or equals) the base block:
			// nothing in this rule is reachable.
			return nil, true, nil
		case basePrefix.Overlaps(rp):
			// The reserved range nests inside the base block: subtract it.
			except = append(except, rp.String())
		default:
			// Disjoint — including a different address family — so the
			// range is already outside this rule's reach.
		}
	}
	return except, false, nil
}

// subtractExempt removes a class's exempted ranges from the cluster-wide
// reserved list. An exemption matches only an exact reserved entry or a
// range that contains it, so a class cannot widen its reach beyond what
// the operator wrote down by naming a range that merely overlaps.
func subtractExempt(reserved, exempt []string) ([]string, error) {
	if len(exempt) == 0 {
		return reserved, nil
	}

	exemptPrefixes := make([]netip.Prefix, 0, len(exempt))
	for _, e := range exempt {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("%w: exempt %q: %w", ErrInvalidCIDR, e, err)
		}
		exemptPrefixes = append(exemptPrefixes, p.Masked())
	}

	out := make([]string, 0, len(reserved))
	for _, r := range reserved {
		rp, err := netip.ParsePrefix(r)
		if err != nil {
			return nil, fmt.Errorf("%w: reserved %q: %w", ErrInvalidCIDR, r, err)
		}
		rp = rp.Masked()

		var dropped bool
		for _, e := range exemptPrefixes {
			if e.Overlaps(rp) && e.Bits() <= rp.Bits() {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, r)
		}
	}
	return out, nil
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
