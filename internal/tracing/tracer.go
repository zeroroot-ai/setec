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

// Package tracing owns the OpenTelemetry tracer setup the operator uses
// to emit spans for the Sandbox lifecycle. An empty OTLP endpoint must
// yield a true no-op tracer so Phase 1 deployments (and any operator
// instance with tracing disabled) pay zero runtime cost.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	grpccreds "google.golang.org/grpc/credentials"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/credentials"
)

// ServiceName is the service.name resource attribute stamped on every
// emitted span. Consumers can use this to scope queries in any OTEL
// backend that supports the semantic-conventions keyword.
const ServiceName = "setec-operator"

// TracerName is the instrumentation-library identifier used when obtaining
// a tracer from the TracerProvider. Keeping it constant across call sites
// means span queries can filter on otel.library.name = setec-operator.
const TracerName = "github.com/zeroroot-ai/setec"

// TracerSandboxAttrClass is the span attribute key carrying the resolved
// SandboxClass name.
const TracerSandboxAttrClass = "setec.sandbox.class"

// ShutdownFunc flushes any buffered spans and releases the OTLP exporter
// resources. Callers must invoke it during graceful shutdown so in-flight
// spans reach the configured backend.
type ShutdownFunc func(context.Context) error

// noopShutdown is returned for the empty-endpoint path so callers can use
// the same defer pattern regardless of whether tracing is enabled.
func noopShutdown(context.Context) error { return nil }

// Config bundles the knobs Setup consumes. Zero values produce the
// secure default (TLS with system root CAs).
type Config struct {
	// Endpoint is the OTLP/gRPC collector address. Empty → no-op tracer.
	Endpoint string
	// Insecure disables TLS on the OTLP exporter. Must only be set for
	// dev clusters; Setup emits a loud warning when true.
	Insecure bool
	// CAFile optionally overrides the system root CAs with a PEM
	// bundle the operator has mounted. Ignored when Insecure is true.
	//
	// Note what this hop is: the collector is authenticated, setec is
	// not. The exporter presents no client certificate, so a collector
	// cannot use this channel to establish who is talking to it. Set
	// SPIFFESocket instead to make it mutual.
	CAFile string

	// SPIFFESocket is the SPIFFE Workload API endpoint. Setting it
	// makes this hop mutual TLS: the operator presents its X509-SVID
	// and authorizes the collector's SPIFFE ID, instead of merely
	// verifying that the collector's certificate chains.
	SPIFFESocket string

	// SPIFFEServers is the allow-list of full SPIFFE IDs the collector
	// may present. Required with SPIFFESocket; an empty list is a
	// configuration error rather than "accept any collector".
	SPIFFEServers []string
}

// Setup constructs an OTEL tracer backed by an OTLP/gRPC exporter. When
// cfg.Endpoint is empty, Setup returns a no-op tracer and a no-op
// shutdown function — this is the default path for Phase 1 deployments
// and must stay allocation-free on the hot path.
//
// TLS is the default. cfg.Insecure=true is an explicit operator
// choice and causes Setup to emit a loud WARN log noting plaintext
// export; the Helm chart disables this path by default.
func Setup(cfg Config) (trace.Tracer, ShutdownFunc, error) {
	if cfg.Endpoint == "" {
		// noop.NewTracerProvider yields a tracer whose spans are
		// discarded without ever reaching an exporter, so the
		// disabled path pays only method dispatch cost.
		tp := tracenoop.NewTracerProvider()
		return tp.Tracer(TracerName), noopShutdown, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		// Stderr keeps the warning observable in controller-runtime
		// log pipelines even when the structured logger has not yet
		// been installed. Matches cmd/main.go's startup style.
		fmt.Fprintln(os.Stderr,
			"tracing: WARNING --otel-insecure set; OTLP traces will be exported in plaintext")
		opts = append(opts, otlptracegrpc.WithInsecure())
	} else {
		tlsCreds, err := exporterCredentials(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("tracing: build OTLP TLS credentials: %w", err)
		}
		opts = append(opts, otlptracegrpc.WithTLSCredentials(tlsCreds))
	}

	ctx := context.Background()
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: construct OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(ServiceName),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: merge resource attributes: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	// Installing as the global provider lets any other library in the
	// process that calls otel.Tracer(...) participate in the same
	// exporter pipeline. Consumers that want strict isolation should
	// construct their own provider with their own resource attributes.
	otel.SetTracerProvider(tp)

	shutdown := func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("tracing: tracer provider shutdown: %w", err)
		}
		return nil
	}

	return tp.Tracer(TracerName), shutdown, nil
}

// exporterCredentials resolves the transport credentials for the OTLP
// channel. Nothing in this package touches crypto/tls; the two shapes
// this hop can take are both named by the credential module.
//
// Which shape applies is the operator's choice and is exclusive:
//
//   - Default — one-way TLS. The collector is authenticated against the
//     host root store or an explicitly mounted bundle, and setec
//     presents no identity. This is not mTLS, and the credential module
//     says so in its type name rather than letting a reader infer
//     otherwise from the word "TLS".
//   - SPIFFESocket set — mutual TLS. The operator presents its
//     X509-SVID and authorizes the collector's SPIFFE ID, which is a
//     stronger statement than "the certificate chains".
//
// Configuring both is a startup error rather than a silent precedence
// rule, for the same reason the credential modes are exclusive
// everywhere else in setec: an operator must never have to work out
// which one won.
func exporterCredentials(cfg Config) (grpccreds.TransportCredentials, error) {
	if cfg.SPIFFESocket == "" && len(cfg.SPIFFEServers) == 0 {
		return credentials.TrustOnlyCredentials(credentials.TrustOnly{CAFile: cfg.CAFile})
	}
	if cfg.CAFile != "" {
		return nil, errors.New(
			"both --otel-ca-file and the --otel-spiffe-* flags are configured; exactly one must be")
	}
	provider, err := credentials.New(credentials.Config{SPIFFE: &credentials.SPIFFESource{
		SocketPath:    cfg.SPIFFESocket,
		AuthorizedIDs: cfg.SPIFFEServers,
	}})
	if err != nil {
		return nil, err
	}
	return provider.ClientCredentials(context.Background())
}

// StartSandboxSpan starts a root span for a Sandbox reconciliation or any
// Sandbox-lifecycle event. Callers End() the span themselves; the helper
// exists so the standard attributes (namespace, name, class) stay
// consistent across every call site.
//
// When tr is nil the function returns the input context and a no-op span
// so callers never need to nil-check before recording attributes. This
// mirrors the trace.Tracer contract: a no-op tracer returns no-op spans.
func StartSandboxSpan(ctx context.Context, tr trace.Tracer, sb *setecv1alpha1.Sandbox) (context.Context, trace.Span) {
	if tr == nil {
		return ctx, tracenoop.Span{}
	}
	if sb == nil {
		ctx, span := tr.Start(ctx, "Reconcile.Sandbox")
		return ctx, span
	}

	attrs := []attribute.KeyValue{
		semconv.K8SNamespaceName(sb.Namespace),
		attribute.String("k8s.object.name", sb.Name),
		attribute.String(TracerSandboxAttrClass, sb.Spec.SandboxClassName),
	}
	return tr.Start(ctx, "Reconcile.Sandbox", trace.WithAttributes(attrs...))
}
