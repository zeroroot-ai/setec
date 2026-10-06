// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package credentials owns mTLS credential acquisition for every setec
// component. It is the single answer to the question "what credentials
// should this process present, and whom should it accept".
//
// The contract is deliberately narrow: configuration in, transport
// credentials out, or an error. A caller says which surface it is — a
// server that must verify its clients, or a client that must verify the
// server it dials — and receives credentials. No caller should ever
// reach for crypto/tls itself, so a new mTLS surface cannot add its own
// credential loading without deliberately going around this package.
//
// setec has one credential source: the SPIFFE Workload API. A caller is
// authorized by its SPIFFE ID, and no PEM file credential source exists
// (setec#175).
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

	grpccreds "google.golang.org/grpc/credentials"
)

// minTLSVersion is the floor for every setec mTLS hop. It is not
// configurable: an operator who wants a weaker floor wants a different
// security posture than setec offers.
const minTLSVersion = tls.VersionTLS13

// Provider hands out the transport credentials for one component's mTLS
// surfaces. Construct it with New and keep it for the process lifetime.
type Provider struct {
	source *spiffeSource
}

// New validates src and returns the Provider for it.
//
// It reports an error when the source is incomplete. It performs no I/O:
// the Workload API is reached when credentials are requested, so a
// successful New means the configuration is coherent, not that the
// credentials exist.
func New(src SPIFFESource) (*Provider, error) {
	s, err := newSPIFFESource(src)
	if err != nil {
		return nil, err
	}
	return &Provider{source: s}, nil
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
	// Rebuild from the source for every connection so a rotated
	// certificate or an updated trust bundle is on the wire immediately.
	// The nested configuration comes from the same function, so the
	// guarantees it sets hold on every handshake and not only the first.
	// It carries no GetConfigForClient of its own, so this does not
	// recurse.
	//nolint:contextcheck // each handshake has its own context, not the context of setup
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		return p.serverConfig(hello.Context())
	}
	return grpccreds.NewTLS(cfg), nil
}

// serverConfig assembles one server TLS configuration from the source.
// The TLS floor, the mandatory client certificate and the peer
// authorization hook are set here rather than by the source, so no
// source can weaken them.
func (p *Provider) serverConfig(ctx context.Context) (*tls.Config, error) {
	cert, pool, err := p.materialize(ctx)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       minTLSVersion,
		ClientCAs:        pool,
		ClientAuth:       tls.RequireAndVerifyClientCert,
		VerifyConnection: p.authorizeConnection,
	}, nil
}

// authorizeConnection runs after the standard chain verification, on each
// handshake and each resumed session, and asks the source whether the
// authenticated peer is one this component accepts.
func (p *Provider) authorizeConnection(cs tls.ConnectionState) error {
	return p.source.authorizePeer(cs.VerifiedChains)
}

// ClientCredentials returns the credentials for dialing a peer,
// presenting this component's certificate and authorizing the peer it
// reaches.
//
// A client's question is not the server's question turned around. A
// server asks "did a trusted authority issue this, and is the holder
// on my list". A client asks that too, but the standard answer to "is
// this the server I meant" is a hostname check, and an X509-SVID
// carries no hostname. So the SPIFFE-ID check replaces the hostname
// check.
//
// It acquires credentials once before returning, so a source that
// cannot produce them is a startup failure rather than a dial that
// fails later looking like a network problem.
func (p *Provider) ClientCredentials(ctx context.Context) (grpccreds.TransportCredentials, error) {
	cfg, err := p.clientConfig(ctx)
	if err != nil {
		return nil, err
	}
	// Re-ask the source for every handshake so a rotated certificate is
	// on the wire immediately. There is no client-side
	// GetConfigForClient, so the identity is refreshed through this hook
	// and the trust anchors are refreshed inside the verification
	// callback.
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := p.source.identity(ctx)
		if err != nil {
			return nil, err
		}
		return &cert, nil
	}
	return grpccreds.NewTLS(cfg), nil
}

// clientConfig assembles one client TLS configuration from the source.
// The TLS floor and the peer authorization hook are set here rather
// than by the source, so no source can weaken them.
func (p *Provider) clientConfig(ctx context.Context) (*tls.Config, error) {
	cert, pool, err := p.materialize(ctx)
	if err != nil {
		return nil, err
	}
	// An X509-SVID carries no name to check, so Go's standard
	// verification cannot run. It is replaced rather than relaxed:
	// authorizeUnnamedPeer performs the chain verification Go would have
	// performed and then asks the source whether the verified identity
	// is one this component talks to. Skipping verification here without
	// that replacement would accept any certificate at all.
	unnamed := p.authorizeUnnamedPeer(ctx)
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         minTLSVersion,
		RootCAs:            pool,
		InsecureSkipVerify: true, //nolint:gosec // replaced by authorizeUnnamedPeer in VerifyConnection, not dropped
		// VerifyConnection runs on each handshake, a resumed one included.
		VerifyConnection: func(cs tls.ConnectionState) error {
			raw := make([][]byte, 0, len(cs.PeerCertificates))
			for _, c := range cs.PeerCertificates {
				raw = append(raw, c.Raw)
			}
			return unnamed(raw, nil)
		},
	}, nil
}

// authorizeUnnamedPeer verifies and authorizes a peer whose certificate
// carries no name the hostname check could use.
//
// It fetches the trust anchors per handshake rather than closing over
// the pool built at startup, so a rotated bundle takes effect without a
// restart — the client-side equivalent of the server's
// GetConfigForClient.
func (p *Provider) authorizeUnnamedPeer(ctx context.Context) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("peer authorization: peer presented no certificate")
		}
		chain := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("peer authorization: parse peer certificate: %w", err)
			}
			chain = append(chain, cert)
		}
		pool, err := p.source.trustAnchors(ctx)
		if err != nil {
			return fmt.Errorf("peer authorization: trust anchors: %w", err)
		}
		intermediates := x509.NewCertPool()
		for _, cert := range chain[1:] {
			intermediates.AddCert(cert)
		}
		verified, err := chain[0].Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return fmt.Errorf("peer authorization: verify peer chain: %w", err)
		}
		return p.source.authorizePeer(verified)
	}
}

// materialize acquires both halves of the credential from the source.
func (p *Provider) materialize(ctx context.Context) (tls.Certificate, *x509.CertPool, error) {
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
