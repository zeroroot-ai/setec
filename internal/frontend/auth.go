// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	setcreds "github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// ErrNoPeerCert is returned when a call carries no TLS peer certificate.
var ErrNoPeerCert = errors.New("frontend: no TLS peer certificate")

// Enrollment is the list of client clusters that may call the frontend
// (ADR-0142). Each entry joins a client name to the SPIFFE ID of the daemon
// of that cluster. The frontend refuses a caller whose SPIFFE ID is not in
// the list. The name of the client is half of the owner pair of a Sandbox,
// and the tenant field of the request is the other half.
//
// The certificate gives only the client. It never gives the tenant: a
// certificate names a workload, not a tenant.
type Enrollment struct {
	byID map[string]string
	ids  []string
}

// ParseEnrollment reads entries of the form "<name>=<spiffe-id>". A name is
// a DNS label. Two entries with one name, or with one SPIFFE ID, are refused,
// because each name must select one cluster and each cluster one name. An
// empty list is refused: no setting accepts every caller.
func ParseEnrollment(entries []string) (*Enrollment, error) {
	if len(entries) == 0 {
		return nil, errors.New("enrollment: no client is enrolled; give --client=<name>=<spiffe-id> for each Gibson cluster")
	}
	e := &Enrollment{byID: make(map[string]string, len(entries))}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name, rawID, ok := strings.Cut(entry, "=")
		if !ok || name == "" || rawID == "" {
			return nil, fmt.Errorf("enrollment: entry %q is not <name>=<spiffe-id>", entry)
		}
		if errs := validation.IsDNS1123Label(name); len(errs) != 0 {
			return nil, fmt.Errorf("enrollment: client name %q is not a DNS label", name)
		}
		id, err := setcreds.ParseSPIFFEID(rawID)
		if err != nil {
			return nil, fmt.Errorf("enrollment: client %q: %w", name, err)
		}
		if _, dup := names[name]; dup {
			return nil, fmt.Errorf("enrollment: client name %q is enrolled twice", name)
		}
		if other, dup := e.byID[id]; dup {
			return nil, fmt.Errorf("enrollment: SPIFFE ID %s is enrolled as %q and as %q", id, other, name)
		}
		names[name] = struct{}{}
		e.byID[id] = name
		e.ids = append(e.ids, id)
	}
	sort.Strings(e.ids)
	return e, nil
}

// SPIFFEIDs returns the SPIFFE ID of each enrolled client, sorted. The
// credential layer authorizes exactly these peers.
func (e *Enrollment) SPIFFEIDs() []string {
	return append([]string(nil), e.ids...)
}

// clientFromContext returns the enrolled name of the caller. It reads the
// SPIFFE ID from the URI SAN of the verified peer certificate.
func (e *Enrollment) clientFromContext(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "peer is not TLS-authenticated")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", status.Error(codes.Unauthenticated, "no client certificate presented")
	}
	id, err := setcreds.PeerSPIFFEID(tlsInfo.State.PeerCertificates[0])
	if err != nil {
		return "", status.Error(codes.PermissionDenied, "the client certificate carries no SPIFFE ID")
	}
	name, ok := e.byID[id]
	if !ok {
		return "", status.Errorf(codes.PermissionDenied, "caller %s is not an enrolled client", id)
	}
	return name, nil
}

// PairResolver maps the owner pair of a call to the one namespace of that
// pair. Two pairs never share a namespace.
type PairResolver interface {
	NamespaceFor(ctx context.Context, p tenancy.Pair) (string, error)
}

// resolveCallerNamespace is the scope check of both services. It names the
// enrolled client of the caller, joins it with the tenant of the request,
// and returns the namespace of that pair.
func resolveCallerNamespace(
	ctx context.Context, e *Enrollment, r PairResolver, tenant string,
) (string, tenancy.Pair, error) {
	if e == nil || r == nil {
		return "", tenancy.Pair{}, status.Error(codes.FailedPrecondition,
			"the frontend has no enrollment or no namespace resolver")
	}
	client, err := e.clientFromContext(ctx)
	if err != nil {
		return "", tenancy.Pair{}, err
	}
	if tenant == "" {
		return "", tenancy.Pair{}, status.Error(codes.InvalidArgument, "tenant is required")
	}
	pair, err := tenancy.NewPair(client, tenant)
	if err != nil {
		return "", tenancy.Pair{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	ns, err := r.NamespaceFor(ctx, pair)
	if err != nil {
		return "", tenancy.Pair{}, status.Errorf(codes.PermissionDenied,
			"pair %s has no accessible namespace: %v", pair, err)
	}
	return ns, pair, nil
}
