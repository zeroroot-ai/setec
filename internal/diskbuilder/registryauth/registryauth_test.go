// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
)

type fakeECR struct {
	calls   int
	regions []string
	expires time.Time
	err     error
}

func (f *fakeECR) GetAuthorizationToken(_ context.Context, _ *ecr.GetAuthorizationTokenInput, _ ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:secret-" + time.Duration(f.calls).String()))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []ecrtypes.AuthorizationData{{
		AuthorizationToken: aws.String(tok), ExpiresAt: aws.Time(f.expires),
	}}}, nil
}

func ecrWith(f *fakeECR, now *time.Time) *ECR {
	return &ECR{
		Client: func(_ context.Context, region string) (ecrAPI, error) {
			f.regions = append(f.regions, region)
			return f, nil
		},
		Now: func() time.Time { return *now },
	}
}

func TestECR_Serves(t *testing.T) {
	t.Parallel()
	e := NewECR()
	for host, want := range map[string]bool{
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com":          true,
		"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com": true,
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":      true,
		"ghcr.io":                             false,
		"public.ecr.aws":                      false,
		"123.dkr.ecr.eu-west-1.amazonaws.com": false,
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com.evil.example": false,
		"evil.example/123456789012.dkr.ecr.eu-west-1.amazonaws.com": false,
	} {
		if got := e.Serves(host); got != want {
			t.Errorf("Serves(%q) = %t, want %t", host, got, want)
		}
	}
}

func TestECR_TokenInTheRegionOfTheHost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeECR{expires: now.Add(12 * time.Hour)}
	e := ecrWith(f, &now)
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	a, err := e.Authenticator(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Authorization()
	if err != nil || cfg.Username != "AWS" || cfg.Password == "" {
		t.Fatalf("authorization = %+v, %v; want user AWS and a password", cfg, err)
	}
	if len(f.regions) != 1 || f.regions[0] != "eu-west-1" {
		t.Fatalf("regions %v; want the region of the host, eu-west-1", f.regions)
	}
}

func TestECR_ReusesTheTokenUntilItIsNearItsEnd(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeECR{expires: now.Add(time.Hour)}
	e := ecrWith(f, &now)
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	for range 3 {
		if _, err := e.Authenticator(context.Background(), host); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1: the token is reused", f.calls)
	}
	now = now.Add(56 * time.Minute) // less than five minutes left
	f.expires = now.Add(time.Hour)
	if _, err := e.Authenticator(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("calls = %d, want 2: a token near its end is replaced", f.calls)
	}
}

func TestECR_ErrorsNameTheCall(t *testing.T) {
	t.Parallel()
	now := time.Now()
	f := &fakeECR{err: errors.New("no credentials")}
	e := ecrWith(f, &now)
	_, err := e.Authenticator(context.Background(), "123456789012.dkr.ecr.eu-west-1.amazonaws.com")
	if err == nil || !errors.Is(err, f.err) {
		t.Fatalf("err = %v; want the cause wrapped", err)
	}
	if _, err := e.Authenticator(context.Background(), "ghcr.io"); err == nil {
		t.Fatal("a host that is not ECR was accepted")
	}
}

type staticKeychain struct{ auth authn.Authenticator }

func (s staticKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return s.auth, nil }

// The keychain asks the static credential first, then the providers. A
// registry that nobody knows is anonymous.
func TestKeychain_Order(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeECR{expires: now.Add(12 * time.Hour)}
	ecrHostName := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	resolve := func(k authn.Keychain, host string) authn.AuthConfig {
		t.Helper()
		reg, err := name.NewRegistry(host)
		if err != nil {
			t.Fatal(err)
		}
		a, err := k.Resolve(reg)
		if err != nil {
			t.Fatal(err)
		}
		c, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		return *c
	}

	cloud := Keychain(staticKeychain{authn.Anonymous}, ecrWith(f, &now))
	if got := resolve(cloud, ecrHostName); got.Username != "AWS" {
		t.Fatalf("an ECR host with no static entry = %+v; want the ECR token", got)
	}
	if got := resolve(cloud, "ghcr.io"); got != (authn.AuthConfig{}) {
		t.Fatalf("a host that no source knows = %+v; want anonymous", got)
	}
	calls := f.calls
	pull := authn.FromConfig(authn.AuthConfig{Username: "pull", Password: "static"})
	both := Keychain(staticKeychain{pull}, ecrWith(f, &now))
	if got := resolve(both, ecrHostName); got.Username != "pull" || f.calls != calls {
		t.Fatalf("with a pull Secret = %+v, calls %d; want the static credential and no ECR call", got, f.calls-calls)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := New("ecr"); err != nil {
		t.Fatal(err)
	}
	if _, err := New("gcr"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err = %v; want ErrUnknownProvider", err)
	}
	// The operator checks a name against diskbuilder.PullProviders, so the
	// two lists name the same providers.
	if got, want := Names(), slices.Sorted(slices.Values(diskbuilder.PullProviders)); !slices.Equal(got, want) {
		t.Fatalf("names = %v, diskbuilder.PullProviders = %v; they must be equal", got, want)
	}
	// Each push provider is a provider that the builder knows.
	for _, n := range diskbuilder.PushProviders {
		if !slices.Contains(Names(), n) {
			t.Fatalf("diskbuilder.PushProviders names %q, which the builder does not know (%v)", n, Names())
		}
	}
}
