package natsdriver

// Explicit trusted TLS for one broker client.
//
// TLS is additive: a nil Config.TLS keeps the existing plaintext composition.
// A non-nil TLSConfig always requires a readable private CA bundle; there is no
// system-root fallback and no insecure-verify escape hatch. Hostname and
// certificate verification stay on, and the same configuration (trust, client
// certificate and authentication) is reused for every reconnect because it is
// installed in the nats.go options once at connect time.

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// TLSConfig is explicit trusted TLS for one broker client.
//
// CAFile is the PEM certificate-authority bundle the client must trust. It is
// required. CertFile and KeyFile are an optional client certificate pair used
// for mutual TLS; they must be configured together.
type TLSConfig struct {
	// CAFile is the path to a PEM certificate-authority bundle. It is required
	// whenever TLS is configured.
	CAFile string
	// CertFile is the path to a PEM client certificate. It requires KeyFile.
	CertFile string
	// KeyFile is the path to the PEM private key for CertFile. It requires
	// CertFile.
	KeyFile string
}

// clientTLS builds the verified client configuration from the configured CA
// bundle and optional client certificate. A nil TLS configuration returns nil,
// which leaves the plaintext composition unchanged.
//
// Every configuration failure is InvalidInput and never renders file content,
// private-key material or URL credentials: the causes are private and only the
// public message is exposed.
func (config Config) clientTLS() (*tls.Config, error) {
	if config.TLS == nil {
		return nil, nil
	}
	caFile := strings.TrimSpace(config.TLS.CAFile)
	if caFile == "" {
		return nil, fault.New(fault.InvalidInput, "broker TLS has no certificate authority bundle")
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "broker TLS certificate authority is unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fault.New(fault.InvalidInput, "broker TLS certificate authority is not a usable certificate")
	}

	// RootCAs is the configured private bundle only: verification never falls
	// back to the system roots. InsecureSkipVerify stays false.
	tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	certFile := strings.TrimSpace(config.TLS.CertFile)
	keyFile := strings.TrimSpace(config.TLS.KeyFile)
	if (certFile == "") != (keyFile == "") {
		return nil, fault.New(fault.InvalidInput, "broker TLS client certificate is half configured")
	}
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "broker TLS client certificate is unusable")
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}
