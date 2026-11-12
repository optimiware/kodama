//go:build e2e

package harness

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
)

// LiveBrokerTLS builds the verified-TLS configuration a live subscription probe
// uses. A plain nats:// broker with no CA file keeps its unchanged, certificate
// verified default by returning nil; a tls:// broker (or an explicitly
// configured CA) gets the bundle at Q01_NATS_CA_FILE, the configured server
// name or the URL host, and TLS 1.2 as the floor.
func (config Config) LiveBrokerTLS() (*tls.Config, error) {
	parsed, err := url.Parse(config.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("%s: is not a valid URL: %v", InputNATSURL, err)
	}
	if parsed.Scheme != "tls" && config.NATSCAFile == "" {
		return nil, nil
	}
	if config.NATSCAFile == "" {
		return nil, fmt.Errorf("%s: a tls:// broker requires the broker CA bundle", InputNATSCAFile)
	}
	data, err := os.ReadFile(config.NATSCAFile)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the CA file", InputNATSCAFile)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s: the CA file contains no certificate", InputNATSCAFile)
	}
	serverName := config.NATSTLSServerName
	if serverName == "" {
		serverName = parsed.Hostname()
	}
	if serverName == "" {
		return nil, fmt.Errorf("%s: cannot derive the broker TLS server name", InputNATSURL)
	}
	tlsConfig := &tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
	// The stage broker verifies client certificates (mTLS) in addition to the
	// per-credential authorization callout, so it also needs this client's
	// certificate and key when the installation supplies them.
	if config.NATSClientCertFile != "" || config.NATSClientKeyFile != "" {
		certificate, err := tls.LoadX509KeyPair(config.NATSClientCertFile, config.NATSClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot load the broker client certificate and key", InputNATSClientCertFile)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil

}

// LiveBrokerToken reads the bearer token a live probe presents as the broker
// auth token. The explicit Q01_NATS_TOKEN_FILE wins; otherwise the existing
// admin token file is accepted rather than a raw environment token.
func (config Config) LiveBrokerToken() (Secret, error) {
	path := config.brokerTokenFile()
	if path == "" {
		return "", fmt.Errorf("%s: no broker token file is configured", InputNATSTokenFile)
	}
	token, err := loadNATSTokenFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", InputNATSTokenFile, err)
	}
	return token, nil
}
