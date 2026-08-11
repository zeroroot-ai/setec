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

// Package credentials owns mTLS credential acquisition for every setec
// component. It is the single answer to the question "what credentials
// should this process present, and whom should it accept".
//
// The contract is deliberately narrow: configuration in, transport
// credentials out, or an error. A caller says which surface it is — a
// server that must verify its clients, or a client that must verify the
// server it dials — and receives credentials. Where the key material
// came from is not the caller's concern, and no caller should ever
// reach for crypto/tls itself. That is the whole point: a new mTLS
// surface added later cannot reintroduce direct file loading without
// deliberately going around this package.
//
// There are two credential sources: PEM files on disk, which is what
// every setec component has always used and remains the default, and
// the SPIFFE Workload API. They plug in behind the same Provider
// methods as fields on Config; callers do not change and no signature
// moves. There is deliberately no "which source am I using" accessor
// and no boolean mode switch in the API — the source is chosen once, at
// construction, from configuration, and exactly one source is a
// configuration this package will build.
//
// A Provider performs no I/O until a credentials method is called, and
// components are expected to call it once at startup so that a bad
// credential configuration is a boot failure rather than a per-request
// one.
package credentials

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	grpccreds "google.golang.org/grpc/credentials"
)

// minTLSVersion is the floor for every setec mTLS hop. It is not
// configurable: an operator who wants a weaker floor wants a different
// security posture than setec offers.
const minTLSVersion = tls.VersionTLS13

// Config selects a credential source and carries its settings.
//
// Exactly one source must be set. Zero configured sources is an error
// rather than a silent plaintext or system-roots default, and more than
// one is an error too, so an operator never has to reason about which
// source won. There is no fallback between them: a SPIFFE
// configuration whose Workload API cannot be reached fails, rather than
// quietly reverting to files and running weaker credentials than the
// operator asked for.
type Config struct {
	// Files configures PEM files on disk as the credential source.
	// This is the default posture and the one a standalone setec
	// install with no SPIRE deployment uses.
	Files *FileSource

	// SPIFFE configures the SPIFFE Workload API as the credential
	// source, with peer authorization by SPIFFE ID.
	SPIFFE *SPIFFESource
}

// FileSource reads the component's identity and its trust anchors from
// PEM files on disk. All three paths are required: setec's mTLS hops
// authenticate in both directions, so a source that can present an
// identity but not verify a peer is a misconfiguration.
type FileSource struct {
	// CertFile is the PEM certificate this component presents.
	CertFile string
	// KeyFile is the PEM private key matching CertFile.
	KeyFile string
	// CAFile is the PEM bundle of trust anchors used to verify the
	// peer — the client CA when serving, the server CA when dialing.
	CAFile string
}

// Provider hands out the transport credentials for one component's mTLS
// surfaces. Construct it with New and keep it for the process lifetime.
type Provider struct {
	source source
}

// source is the internal seam every credential source implements.
// Keeping it unexported means the set of sources is closed to this
// package: a caller cannot supply its own and thereby bypass the
// guarantees the exported methods make about TLS version and peer
// verification.
type source interface {
	// identity returns the certificate chain this component presents.
	identity(ctx context.Context) (tls.Certificate, error)
	// trustAnchors returns the pool used to verify the peer.
	trustAnchors(ctx context.Context) (*x509.CertPool, error)
	// authorizePeer decides whether a peer whose chain has already
	// been verified against trustAnchors is one this component talks
	// to. Chain verification proves a trusted authority issued the
	// certificate; this is the separate question of who is holding it.
	authorizePeer(verified [][]*x509.Certificate) error
	// rotates reports whether the source maintains its material
	// in-process. A source that says yes is re-asked for every
	// handshake, so a rotated certificate or an updated trust bundle
	// takes effect without a restart. A source that says no is read
	// once, at startup, which is what file mode has always done.
	rotates() bool
	// dialing reports whether the source can furnish credentials for
	// dialing a peer, and names what is missing when it cannot.
	dialing() error
}

// New validates cfg and returns the Provider for it.
//
// It reports an error when no credential source is configured, or when
// the configured source is incomplete. It performs no I/O — a source
// that reads files does so when credentials are requested — so a
// successful New means the configuration is coherent, not that the
// credentials exist.
func New(cfg Config) (*Provider, error) {
	switch {
	case cfg.Files != nil && cfg.SPIFFE != nil:
		return nil, errors.New(
			"both the file and SPIFFE credential sources are configured; exactly one must be")
	case cfg.Files != nil:
		src, err := newFileSource(*cfg.Files)
		if err != nil {
			return nil, err
		}
		return &Provider{source: src}, nil
	case cfg.SPIFFE != nil:
		src, err := newSPIFFESource(*cfg.SPIFFE)
		if err != nil {
			return nil, err
		}
		return &Provider{source: src}, nil
	default:
		return nil, errors.New("no credential source configured")
	}
}

// ServerCredentials returns the credentials for a gRPC server that
// requires and verifies a client certificate. Client authentication is
// mandatory and not configurable; every setec server surface is mTLS.
//
// It acquires credentials once before returning, so a source that
// cannot produce them is a startup failure rather than a listener that
// refuses every connection.
func (p *Provider) ServerCredentials(ctx context.Context) (grpccreds.TransportCredentials, error) {
	cfg, err := p.serverConfig(ctx)
	if err != nil {
		return nil, err
	}
	if p.source.rotates() {
		// Rebuild from the source for every connection so a rotated
		// certificate or an updated trust bundle is on the wire
		// immediately. The nested configuration comes from the same
		// function, so the guarantees it sets hold on every handshake
		// and not only the first; it carries no GetConfigForClient of
		// its own, so this does not recurse.
		cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			return p.serverConfig(hello.Context())
		}
	}
	return grpccreds.NewTLS(cfg), nil
}

// serverConfig assembles one server TLS configuration from the source.
// The TLS floor, the mandatory client certificate and the peer
// authorization hook are set here rather than by the source, so no
// source can weaken them.
func (p *Provider) serverConfig(ctx context.Context) (*tls.Config, error) {
	cert, pool, err := p.materialise(ctx)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            minTLSVersion,
		ClientCAs:             pool,
		ClientAuth:            tls.RequireAndVerifyClientCert,
		VerifyPeerCertificate: p.authorizePeer,
	}, nil
}

// authorizePeer runs after the standard chain verification and asks the
// source whether the authenticated peer is one this component accepts.
func (p *Provider) authorizePeer(_ [][]byte, verified [][]*x509.Certificate) error {
	return p.source.authorizePeer(verified)
}

// ClientCredentials returns the credentials for dialing a peer,
// presenting this component's certificate and verifying the peer
// against the configured trust anchors.
func (p *Provider) ClientCredentials(ctx context.Context) (grpccreds.TransportCredentials, error) {
	if err := p.source.dialing(); err != nil {
		return nil, err
	}
	cert, pool, err := p.materialise(ctx)
	if err != nil {
		return nil, err
	}
	return grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minTLSVersion,
		RootCAs:      pool,
	}), nil
}

// materialise acquires both halves of the credential from the source.
func (p *Provider) materialise(ctx context.Context) (tls.Certificate, *x509.CertPool, error) {
	cert, err := p.source.identity(ctx)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool, err := p.source.trustAnchors(ctx)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return cert, pool, nil
}

// fileSource is the PEM-files-on-disk credential source.
type fileSource struct {
	certFile string
	keyFile  string
	caFile   string
}

// newFileSource validates that every path a file source needs is set.
func newFileSource(fs FileSource) (*fileSource, error) {
	switch {
	case fs.CertFile == "":
		return nil, errors.New("file credential source: certificate path is empty")
	case fs.KeyFile == "":
		return nil, errors.New("file credential source: key path is empty")
	case fs.CAFile == "":
		return nil, errors.New("file credential source: CA path is empty")
	}
	return &fileSource{certFile: fs.CertFile, keyFile: fs.KeyFile, caFile: fs.CAFile}, nil
}

// identity loads the keypair from disk. The error names the files so an
// operator can fix a bad mount without reading source.
func (s *fileSource) identity(context.Context) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load keypair %q/%q: %w", s.certFile, s.keyFile, err)
	}
	return cert, nil
}

// trustAnchors reads the CA bundle from disk. A file that exists but
// carries no parseable certificate is an error rather than an empty
// pool: an empty pool would reject every peer at handshake time, which
// is a far harder failure to diagnose than a startup error.
func (s *fileSource) trustAnchors(context.Context) (*x509.CertPool, error) {
	pem, err := os.ReadFile(s.caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", s.caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %q contains no usable certificates", s.caFile)
	}
	return pool, nil
}

// authorizePeer accepts any peer the configured CA issued a certificate
// to. That is the whole of what file mode proves, and stating it here
// as an explicit answer rather than an absent check is the point: a
// reader of this package can see that file mode authenticates the peer
// and does not authorize it. Narrowing that is what SPIFFE mode is for.
func (s *fileSource) authorizePeer([][]*x509.Certificate) error { return nil }

// rotates reports that files are read once, at startup — the behaviour
// file mode has always had. Certificates delivered by cert-manager or
// ESO are rotated by restarting the pod.
func (s *fileSource) rotates() bool { return false }

// dialing reports that a file source can furnish client credentials.
func (s *fileSource) dialing() error { return nil }
