// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package registryauth gives the build step the credential to read an image
// from a private registry, with no static secret. A Provider asks the cloud
// that runs the cluster for a short-lived registry token, by the identity of
// the Job: a web identity token, a Pod Identity agent, or the node role. The
// build step and the image verify step use the keychain of this package. The
// check and publish steps never do: they hold the push credential, and the
// build step must not (docs/design/runtime.md).
//
// A cloud adds a Provider and a name in Providers. The rest of the chain
// stays the same.
package registryauth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// A Provider returns the credential of the registries that it serves.
type Provider interface {
	// Serves reports whether the provider answers for the registry host.
	Serves(registry string) bool
	// Authenticator returns the credential for the registry host. It is
	// called only for a host that Serves accepted.
	Authenticator(ctx context.Context, registry string) (authn.Authenticator, error)
}

// ErrUnknownProvider is the refusal of a provider name that no cloud has.
var ErrUnknownProvider = errors.New("registryauth: unknown credential provider")

// providers maps a name of --registry-auth to a constructor.
var providers = map[string]func() (Provider, error){
	"ecr": func() (Provider, error) { return NewECR(), nil },
}

// Names returns the names of the providers, sorted.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// New returns the provider of a name.
func New(name string) (Provider, error) {
	mk, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownProvider, name, strings.Join(Names(), ", "))
	}
	return mk()
}

// Keychain returns a keychain that asks static first and then each provider
// that serves the registry. static is the keychain of the docker config of
// the container (authn.DefaultKeychain): a pull Secret answers before the
// cloud does. A registry that no source knows gets the anonymous access, as
// before.
func Keychain(static authn.Keychain, ps ...Provider) authn.Keychain {
	chain := make([]authn.Keychain, 0, 1+len(ps))
	chain = append(chain, static)
	for _, p := range ps {
		chain = append(chain, providerKeychain{p})
	}
	return authn.NewMultiKeychain(chain...)
}

type providerKeychain struct{ p Provider }

// Resolve implements authn.Keychain.
func (k providerKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	host := res.RegistryStr()
	if !k.p.Serves(host) {
		return authn.Anonymous, nil
	}
	a, err := k.p.Authenticator(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("registryauth: the credential for %s: %w", host, err)
	}
	return a, nil
}
