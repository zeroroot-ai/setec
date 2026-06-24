//go:build e2e

/*
Webhook serving-cert generation for the E2E suite.

The SandboxClass/Sandbox admission webhook is served by the operator's
controller-runtime webhook server, which reads tls.crt/tls.key from
/tmp/k8s-webhook-server/serving-certs and does NOT self-generate. To exercise
TestPhase2_WebhookRejects without cert-manager, the suite generates a
self-signed CA + a server cert for the webhook Service DNS, publishes the
server cert as the chart's webhook.certSecret, and passes the CA to the
ValidatingWebhookConfiguration via webhook.caBundle so the API server trusts
the webhook's TLS.
*/

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// webhookCert holds a generated serving cert + the CA that signed it.
type webhookCert struct {
	certPEM     []byte // server cert (tls.crt)
	keyPEM      []byte // server key (tls.key)
	caBundleB64 string // base64(CA cert PEM) for webhook.caBundle
}

// generateWebhookCert mints a self-signed CA and a server cert valid for the
// webhook Service DNS names (<svc>.<ns>.svc[.cluster.local]).
func generateWebhookCert(serviceName, namespace string) (*webhookCert, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "setec-e2e-webhook-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}
	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse ca: %w", err)
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}
	// PKCS#8 ("PRIVATE KEY"), not PKCS#1 ("RSA PRIVATE KEY"): controller-
	// runtime's webhook server loads the PKCS#1 form without error but then
	// fails the TLS handshake (the API server sees "bad certificate"); the
	// PKCS#8 form serves correctly.
	srvKeyDER, err := x509.MarshalPKCS8PrivateKey(srvKey)
	if err != nil {
		return nil, fmt.Errorf("marshal server key: %w", err)
	}

	return &webhookCert{
		certPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		keyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDER}),
		caBundleB64: base64.StdEncoding.EncodeToString(caPEM),
	}, nil
}

// createWebhookCertSecret generates a serving cert and writes it as the
// kubernetes.io/tls Secret the chart mounts into the operator. It returns the
// base64 CA bundle for webhook.caBundle. The Secret must exist before the
// operator Pod starts (helm --wait), so installChart calls this pre-install.
func createWebhookCertSecret(ctx context.Context, secretName, serviceName, namespace string) (string, error) {
	wc, err := generateWebhookCert(serviceName, namespace)
	if err != nil {
		return "", err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": wc.certPEM, "tls.key": wc.keyPEM},
	}
	if err := k8sClient.Create(ctx, sec); err != nil && !apierrorsIsAlreadyExists(err) {
		return "", fmt.Errorf("create webhook cert secret %q: %w", secretName, err)
	}
	return wc.caBundleB64, nil
}

// apierrorsIsAlreadyExists avoids an extra import in suite_test.go; it mirrors
// apierrors.IsAlreadyExists for the one call site here.
func apierrorsIsAlreadyExists(err error) bool {
	return err != nil && client.IgnoreAlreadyExists(err) == nil
}
