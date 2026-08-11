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

package credentials

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// workloadAPITimeout bounds the first fetch from the Workload API when
// the caller's context carries no deadline of its own.
//
// The go-spiffe client treats an absent socket as a transient condition
// and retries behind a backoff indefinitely. That is right for a
// running process and wrong for a starting one: a missing SPIRE agent
// has to be a boot failure, and a boot that never returns is a failure
// nobody can see. A caller that wants a different bound passes a
// context carrying its own deadline.
const workloadAPITimeout = 30 * time.Second

// SPIFFESource obtains the component's identity and trust anchors from
// the SPIFFE Workload API, and authorizes the peer by SPIFFE ID.
//
// It differs from FileSource in two ways that matter. The identity is
// attested rather than possessed — it is issued to this workload by the
// local SPIRE agent instead of being read from a file anything in the
// container could read — and it rotates in-process, so a handshake uses
// the SVID as it stands at that moment rather than the one that existed
// at boot.
//
// The second difference is authorization. File mode accepts any peer
// the configured CA issued a certificate to; SPIFFE mode additionally
// requires the peer's SPIFFE ID to appear in AuthorizedIDs. That is why
// the allow-list is mandatory: a SPIFFE mode without it would prove
// exactly what file mode proves while reading like an upgrade.
type SPIFFESource struct {
	// SocketPath is the Workload API endpoint, either a filesystem
	// path to the agent's socket ("/run/spire/agent-sockets/api.sock")
	// or a full endpoint address ("unix:///run/spire/agent-sockets/api.sock").
	//
	// It is required. The SPIFFE_ENDPOINT_SOCKET environment variable
	// is deliberately not consulted: which socket a component talks to
	// is part of its configuration, not of its ambient environment.
	SocketPath string

	// AuthorizedIDs is the allow-list of full SPIFFE IDs this
	// component will complete a handshake with, for example
	// "spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon".
	//
	// Entries are full IDs, not paths: an ID is matched on its trust
	// domain as well as its path, so the same path under a foreign
	// trust domain is a different principal and is refused. An empty
	// list is a configuration error rather than "accept everyone", so
	// that widening the list is always a reviewable change and never
	// an omission.
	AuthorizedIDs []string

	// OnRotationError observes failures to maintain the SVID and trust
	// bundle after startup. It is called from the Workload API watcher
	// goroutine and may be called repeatedly while the failure lasts.
	//
	// When it is nil the failure is written to stderr. Something must
	// happen: the go-spiffe client keeps serving the last SVID it
	// received while it retries, so an unreported watch failure would
	// first become visible as connections failing when that SVID
	// expires — minutes or hours later, and looking like a certificate
	// problem rather than a lost agent.
	OnRotationError func(error)
}

// spiffeSource is the Workload API credential source.
//
// The connection to the Workload API is established on the first
// credentials request rather than in New, which is what lets New stay
// free of I/O. Components call for credentials once at startup, so an
// absent agent is still a boot failure.
//
// It watches the Workload API itself rather than using
// workloadapi.X509Source, which reads its cached bundle set without
// holding the lock its watcher writes it under
// (X509Source.GetX509BundleForTrustDomain versus setX509Context in
// go-spiffe v2.8.1). Reading the bundle on every handshake, which is
// what makes rotation take effect here, races that write. Holding the
// material behind this type's own lock also means the watch error
// reaches OnRotationError as an error rather than as a formatted log
// line. Tracked for upstreaming in setec#181.
type spiffeSource struct {
	addr         string
	authorized   []spiffeid.ID
	trustDomains []spiffeid.TrustDomain
	reportError  func(error)

	once      sync.Once
	connErr   error
	firstSVID chan struct{}

	mu      sync.RWMutex
	svid    *x509svid.SVID
	bundles *x509bundle.Set
	lastErr error
}

// newSPIFFESource validates a SPIFFE source configuration.
//
// Everything checkable without touching the network is checked here, so
// that a mistyped socket address or a malformed allow-list entry is a
// configuration error at construction rather than a refusal at
// handshake time.
func newSPIFFESource(cfg SPIFFESource) (*spiffeSource, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("SPIFFE credential source: Workload API socket path is empty")
	}
	addr := cfg.SocketPath
	if !strings.Contains(addr, "://") {
		addr = "unix://" + addr
	}
	if err := workloadapi.ValidateAddress(addr); err != nil {
		return nil, fmt.Errorf("SPIFFE credential source: Workload API address %q: %w", cfg.SocketPath, err)
	}

	if len(cfg.AuthorizedIDs) == 0 {
		return nil, errors.New(
			"SPIFFE credential source: the authorized SPIFFE ID allow-list is empty; " +
				"SPIFFE mode authorizes peers by identity and has no accept-everyone setting")
	}
	authorized := make([]spiffeid.ID, 0, len(cfg.AuthorizedIDs))
	seen := make(map[spiffeid.TrustDomain]struct{}, len(cfg.AuthorizedIDs))
	trustDomains := make([]spiffeid.TrustDomain, 0, len(cfg.AuthorizedIDs))
	for _, raw := range cfg.AuthorizedIDs {
		id, err := spiffeid.FromString(raw)
		if err != nil {
			return nil, fmt.Errorf("SPIFFE credential source: authorized ID %q: %w", raw, err)
		}
		if id.Path() == "" {
			return nil, fmt.Errorf(
				"SPIFFE credential source: authorized ID %q names a trust domain and no workload; "+
					"entries are full SPIFFE IDs", raw)
		}
		authorized = append(authorized, id)
		if _, ok := seen[id.TrustDomain()]; !ok {
			seen[id.TrustDomain()] = struct{}{}
			trustDomains = append(trustDomains, id.TrustDomain())
		}
	}

	report := cfg.OnRotationError
	if report == nil {
		report = func(err error) {
			fmt.Fprintf(os.Stderr, "credentials: SPIFFE Workload API failure: %v\n", err)
		}
	}

	return &spiffeSource{
		addr:         addr,
		authorized:   authorized,
		trustDomains: trustDomains,
		reportError:  report,
		firstSVID:    make(chan struct{}),
	}, nil
}

// connect starts the Workload API watch once and waits for a first
// SVID. The watch then runs for the process lifetime, feeding
// OnX509ContextUpdate.
//
// The failure is latched along with the connection. A component that
// cannot reach its agent at startup is expected to exit rather than
// serve, and re-attempting on a later handshake would turn a boot
// failure into an intermittent one.
func (s *spiffeSource) connect(ctx context.Context) error {
	s.once.Do(func() { s.connErr = s.start(ctx) })
	return s.connErr
}

func (s *spiffeSource) start(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, workloadAPITimeout)
		defer cancel()
	}
	// The client outlives the startup context: that context bounds how
	// long boot waits, not how long the watch runs.
	client, err := workloadapi.New(context.Background(), workloadapi.WithAddr(s.addr))
	if err != nil {
		return fmt.Errorf("SPIFFE credential source: Workload API client for %s: %w", s.addr, err)
	}
	go func() { _ = client.WatchX509Context(context.Background(), s) }()

	select {
	case <-s.firstSVID:
		return nil
	case <-ctx.Done():
		// ctx.Err() alone would say "deadline exceeded" and leave the
		// operator to guess. The watcher's own last error names the
		// missing socket or the refused connection.
		return fmt.Errorf("SPIFFE credential source: no SVID from the Workload API at %s: %w%s",
			s.addr, ctx.Err(), s.lastErrSuffix())
	}
}

// lastErrSuffix renders the most recent watch failure, if any.
func (s *spiffeSource) lastErrSuffix() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastErr == nil {
		return ""
	}
	return fmt.Sprintf(" (last Workload API error: %v)", s.lastErr)
}

// OnX509ContextUpdate stores the material the Workload API just
// delivered. It implements workloadapi.X509ContextWatcher.
func (s *spiffeSource) OnX509ContextUpdate(x509Context *workloadapi.X509Context) {
	if len(x509Context.SVIDs) == 0 {
		s.reportError(errors.New("Workload API delivered no X509-SVID"))
		return
	}
	s.mu.Lock()
	s.svid = x509Context.DefaultSVID()
	s.bundles = x509Context.Bundles
	s.mu.Unlock()

	select {
	case <-s.firstSVID:
	default:
		close(s.firstSVID)
	}
}

// OnX509ContextWatchError reports a failure to maintain the material.
// It implements workloadapi.X509ContextWatcher.
//
// The watch retries behind a backoff and the last good SVID keeps being
// served in the meantime, so this is the only place the failure is
// visible until that SVID expires.
func (s *spiffeSource) OnX509ContextWatchError(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	s.reportError(err)
}

// current returns the material as it stands right now.
func (s *spiffeSource) current() (*x509svid.SVID, *x509bundle.Set) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.svid, s.bundles
}

// identity returns the current X509-SVID as a TLS certificate. It is
// asked once per handshake, which is how a rotated SVID reaches the
// wire without a restart.
func (s *spiffeSource) identity(ctx context.Context) (tls.Certificate, error) {
	if err := s.connect(ctx); err != nil {
		return tls.Certificate{}, err
	}
	svid, _ := s.current()
	if svid == nil {
		return tls.Certificate{}, errors.New("SPIFFE credential source: no X509-SVID held")
	}
	chain := make([][]byte, 0, len(svid.Certificates))
	for _, cert := range svid.Certificates {
		chain = append(chain, cert.Raw)
	}
	return tls.Certificate{
		Certificate: chain,
		PrivateKey:  svid.PrivateKey,
		Leaf:        svid.Certificates[0],
	}, nil
}

// trustAnchors returns the X.509 authorities of every trust domain an
// authorized peer could come from.
//
// Scoping the pool to the allow-list rather than to every bundle the
// agent happens to hold means a federated trust domain nobody is
// authorized to speak from cannot issue a certificate this component
// will even parse a chain for. A trust domain that is on the allow-list
// but has no bundle is an error, not an empty contribution: silently
// dropping it would refuse that peer at handshake time for a reason no
// log line explains.
func (s *spiffeSource) trustAnchors(ctx context.Context) (*x509.CertPool, error) {
	if err := s.connect(ctx); err != nil {
		return nil, err
	}
	_, bundles := s.current()
	if bundles == nil {
		return nil, errors.New("SPIFFE credential source: no trust bundles held")
	}
	pool := x509.NewCertPool()
	for _, td := range s.trustDomains {
		bundle, err := bundles.GetX509BundleForTrustDomain(td)
		if err != nil {
			return nil, fmt.Errorf(
				"SPIFFE credential source: no trust bundle for %q, which an authorized peer belongs to: %w",
				td, err)
		}
		for _, authority := range bundle.X509Authorities() {
			pool.AddCert(authority)
		}
	}
	return pool, nil
}

// authorizePeer requires the peer's SPIFFE ID to be on the allow-list.
//
// It runs after chain verification, so the peer has already proved a
// trusted authority issued its certificate. That is authentication.
// This is the separate question of whether the authenticated peer is
// one this component talks to — the check that makes possession of any
// certificate from the right CA insufficient.
func (s *spiffeSource) authorizePeer(verified [][]*x509.Certificate) error {
	if len(verified) == 0 || len(verified[0]) == 0 {
		return errors.New("SPIFFE peer authorization: peer presented no verified certificate chain")
	}
	id, err := x509svid.IDFromCert(verified[0][0])
	if err != nil {
		return fmt.Errorf("SPIFFE peer authorization: peer certificate carries no SPIFFE ID: %w", err)
	}
	// spiffeid.ID compares on trust domain and path together, so an
	// identical path under a foreign trust domain does not match.
	if slices.Contains(s.authorized, id) {
		return nil
	}
	return fmt.Errorf("SPIFFE peer authorization: peer %q is not an authorized SPIFFE ID", id)
}

// rotates reports that this source maintains its material in-process.
func (s *spiffeSource) rotates() bool { return true }

// namesPeer reports that an X509-SVID carries no name the standard
// hostname check can use.
//
// An SVID identifies its holder by URI SAN and nothing else: there is
// no DNS SAN for Go to match against the dial target. Saying so here is
// what makes the Provider replace the hostname check with the SPIFFE-ID
// check on the client side, rather than skip verification or fail every
// handshake on a name that was never going to be there.
//
// AuthorizedIDs means the same thing in both directions — the peers
// this component completes a handshake with. On a server that is the
// set of callers; on a client it is the set of servers worth talking
// to, which is the check that makes chaining to the trust bundle
// insufficient.
func (s *spiffeSource) namesPeer() bool { return false }
