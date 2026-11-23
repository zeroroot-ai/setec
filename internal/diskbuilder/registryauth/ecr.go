// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
)

// ecrHost matches the registry host of Amazon ECR: <account>.dkr.ecr.<region>.amazonaws.com,
// the FIPS form, and the China partition.
var ecrHost = regexp.MustCompile(`^([0-9]{12})\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)

// refreshBefore is how long before its end a token is no longer reused.
const refreshBefore = 5 * time.Minute

// ecrAPI is the part of the ECR client that the provider calls.
type ecrAPI interface {
	GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, opts ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// ECR is the Provider of Amazon ECR. It uses the default credential chain of
// the AWS SDK, so it works with each way that a Pod gets an AWS identity: a
// web identity token (IAM roles for service accounts, AWS_WEB_IDENTITY_TOKEN_FILE),
// the container credentials of EKS Pod Identity, and the role of the node.
// The identity needs ecr:GetAuthorizationToken and the pull rights on the
// repository: ecr:BatchGetImage, ecr:GetDownloadUrlForLayer and
// ecr:BatchCheckLayerAvailability.
type ECR struct {
	// Client returns the ECR client of a region. Nil builds one from the
	// default credential chain.
	Client func(ctx context.Context, region string) (ecrAPI, error)
	// Now is the clock of the token cache. Nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	auth    authn.Authenticator
	expires time.Time
}

// NewECR returns the ECR provider.
func NewECR() *ECR { return &ECR{} }

// Serves implements Provider.
func (e *ECR) Serves(registry string) bool { return ecrHost.MatchString(registry) }

func (e *ECR) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func defaultECRClient(ctx context.Context, region string) (ecrAPI, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load the AWS configuration: %w", err)
	}
	return ecr.NewFromConfig(cfg), nil
}

// Authenticator implements Provider. It asks ECR for a token in the region of
// the registry host, and reuses it until shortly before its end. The token
// of a principal opens each ECR registry that the principal may reach, so the
// cache holds one token for each region.
func (e *ECR) Authenticator(ctx context.Context, registry string) (authn.Authenticator, error) {
	m := ecrHost.FindStringSubmatch(registry)
	if m == nil {
		return nil, fmt.Errorf("%q is not an Amazon ECR registry", registry)
	}
	account, region := m[1], m[2]
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.cache[region]; ok && e.now().Add(refreshBefore).Before(t.expires) {
		return t.auth, nil
	}
	mk := e.Client
	if mk == nil {
		mk = defaultECRClient
	}
	api, err := mk(ctx, region)
	if err != nil {
		return nil, err
	}
	out, err := api.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("ecr:GetAuthorizationToken for the registry of account %s in %s: %w", account, region, err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return nil, errors.New("ECR returned no authorization data")
	}
	data := out.AuthorizationData[0]
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(data.AuthorizationToken))
	if err != nil {
		return nil, fmt.Errorf("decode the ECR token: %w", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user == "" || pass == "" {
		return nil, errors.New("the ECR token is not user:password")
	}
	auth := authn.FromConfig(authn.AuthConfig{Username: user, Password: pass})
	if e.cache == nil {
		e.cache = map[string]cachedToken{}
	}
	e.cache[region] = cachedToken{auth: auth, expires: aws.ToTime(data.ExpiresAt)}
	return auth, nil
}
