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

package tracing

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestSetup_EmptyEndpointNoop verifies that an empty OTLP endpoint yields
// a tracer whose spans are discarded, and that the returned shutdown
// function is safely callable as many times as the caller wants.
func TestSetup_EmptyEndpointNoop(t *testing.T) {
	t.Parallel()

	tr, shutdown, err := Setup(Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("Setup(\"\") err: %v", err)
	}
	if tr == nil {
		t.Fatal("Setup(\"\") returned nil tracer")
	}
	if shutdown == nil {
		t.Fatal("Setup(\"\") returned nil ShutdownFunc")
	}
	// A no-op span still satisfies the tracer contract.
	ctx, span := tr.Start(context.Background(), "Reconcile.Sandbox")
	span.End()
	_ = ctx

	// Shutdown must not error; call twice to confirm idempotency.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() second call: %v", err)
	}
}

// TestStartSandboxSpan_InMemoryExporter uses tracetest.NewInMemoryExporter
// to capture spans and asserts the attribute set StartSandboxSpan stamps.
func TestStartSandboxSpan_InMemoryExporter(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})

	tr := tp.Tracer(TracerName)

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-a", Namespace: "tenant-a"},
		Spec:       setecv1alpha1.SandboxSpec{SandboxClassName: "standard"},
	}

	_, span := StartSandboxSpan(context.Background(), tr, sb)
	span.End()

	spans := exp.GetSpans()
	if got, want := len(spans), 1; got != want {
		t.Fatalf("exported %d spans, want %d", got, want)
	}
	got := spans[0]
	if got.Name != "Reconcile.Sandbox" {
		t.Errorf("span name = %q, want Reconcile.Sandbox", got.Name)
	}

	attrs := map[string]string{}
	for _, kv := range got.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["k8s.namespace.name"] != "tenant-a" {
		t.Errorf("k8s.namespace.name = %q, want tenant-a", attrs["k8s.namespace.name"])
	}
	if attrs["k8s.object.name"] != "sb-a" {
		t.Errorf("k8s.object.name = %q, want sb-a", attrs["k8s.object.name"])
	}
	if attrs[TracerSandboxAttrClass] != "standard" {
		t.Errorf("setec.sandbox.class = %q, want standard", attrs[TracerSandboxAttrClass])
	}
}

// TestStartSandboxSpan_NilTracer and nil Sandbox must not panic and must
// return a usable span (no-op is fine).
func TestStartSandboxSpan_NilInputs(t *testing.T) {
	t.Parallel()

	t.Run("nil tracer", func(t *testing.T) {
		ctx, span := StartSandboxSpan(context.Background(), nil, nil)
		if span == nil {
			t.Fatal("StartSandboxSpan(nil, nil) returned nil span")
		}
		// Must not panic on End.
		span.End()
		_ = ctx
	})

	t.Run("nil sandbox with real tracer", func(t *testing.T) {
		exp := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = tp.Shutdown(ctx)
		})
		tr := tp.Tracer(TracerName)
		_, span := StartSandboxSpan(context.Background(), tr, nil)
		span.End()

		spans := exp.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("want 1 span, got %d", len(spans))
		}
	})
}

// TestSetup_WithLocalEndpointInsecure drives Setup through its full
// happy-path with a loopback address and explicit insecure mode. The
// exporter dials lazily, so a non-listening address still allows
// Setup itself to succeed; we immediately shut down to confirm the
// TracerProvider plumbing is wired cleanly.
func TestSetup_WithLocalEndpointInsecure(t *testing.T) {
	t.Parallel()

	tr, shutdown, err := Setup(Config{Endpoint: "127.0.0.1:1", Insecure: true})
	if err != nil {
		t.Fatalf("Setup(): %v", err)
	}
	if tr == nil || shutdown == nil {
		t.Fatal("Setup() returned nil tracer or shutdown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		// Shutdown may surface the underlying exporter error; that
		// is acceptable — the contract is that shutdown does not
		// panic and can be called by the caller's signal handler.
		t.Logf("shutdown returned error (acceptable): %v", err)
	}
}

// TestSetup_WithTLSSystemRoots verifies that the default TLS path
// (empty CAFile, Insecure=false) constructs credentials without
// error.
func TestSetup_WithTLSSystemRoots(t *testing.T) {
	t.Parallel()

	tr, shutdown, err := Setup(Config{Endpoint: "otel-collector.setec.svc:4317"})
	if err != nil {
		t.Fatalf("Setup(): %v", err)
	}
	if tr == nil || shutdown == nil {
		t.Fatal("Setup() returned nil tracer or shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// TestSetup_WithTLSCAFile verifies that a non-empty CAFile is read
// and used to populate the RootCAs on the exporter.
func TestSetup_WithTLSCAFile(t *testing.T) {
	t.Parallel()

	// Generate a self-signed CA inline so the test does not depend on
	// host-system certificates.
	caPEM := testGeneratePEM(t)
	dir := t.TempDir()
	caPath := dir + "/ca.pem"
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}

	tr, shutdown, err := Setup(Config{
		Endpoint: "otel-collector.setec.svc:4317",
		CAFile:   caPath,
	})
	if err != nil {
		t.Fatalf("Setup(): %v", err)
	}
	if tr == nil || shutdown == nil {
		t.Fatal("Setup() returned nil tracer or shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// TestSetup_WithTLSBadCAFile surfaces a construction error when the
// operator points at a file that is not a PEM bundle.
func TestSetup_WithTLSBadCAFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := dir + "/bad.pem"
	if err := os.WriteFile(bad, []byte("not a PEM"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := Setup(Config{Endpoint: "x:1", CAFile: bad}); err == nil {
		t.Fatal("expected error for malformed CA, got nil")
	}
}

// TestSetup_WithTLSMissingCAFile surfaces a construction error when
// the file does not exist.
func TestSetup_WithTLSMissingCAFile(t *testing.T) {
	t.Parallel()

	if _, _, err := Setup(Config{Endpoint: "x:1", CAFile: "/nope/nope/nope.pem"}); err == nil {
		t.Fatal("expected error for missing CA file, got nil")
	}
}

// testGeneratePEM produces a self-signed CA PEM block suitable for
// feeding to x509.CertPool.AppendCertsFromPEM. It uses only the
// standard library so the test stays dependency-free.
func testGeneratePEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "setec test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
