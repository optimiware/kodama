// Package up implements the cross-platform single-origin local stack helpers:
// runtime socket auto-detection, virtiofs-safe state directory validation,
// strict RFC 5280 local TLS generation, built-in ES256 OIDC dev/local issuer,
// single-origin HTTPS reverse-proxy gateway, and container stack lifecycle.
package up

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// caValidity stays within the 825-day maximum enforced by macOS/Apple TLS
	// trust evaluators while remaining long-lived for local development.
	caValidity = 825 * 24 * time.Hour
	// leafValidity stays within the 398-day maximum enforced by modern TLS
	// verifiers for server leaf certificates.
	leafValidity = 365 * 24 * time.Hour
	// minRemainingValidity triggers leaf/CA rotation if an existing cert is
	// within 24 hours of expiry.
	minRemainingValidity = 24 * time.Hour
)

// TLSConfig configures local CA and server leaf certificate generation.
type TLSConfig struct {
	// Dir is the directory holding ca.crt, ca.key, tls.crt and tls.key.
	Dir string
	// Hosts are additional hostnames or IP literals to include in the server
	// leaf Subject Alternative Names alongside localhost, 127.0.0.1, ::1,
	// host.containers.internal and host.docker.internal.
	Hosts []string
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// TLSBundle holds the paths and parsed material of a local TLS directory.
type TLSBundle struct {
	CACertPath   string
	CAKeyPath    string
	LeafCertPath string
	LeafKeyPath  string
	CACertPEM    []byte
	ReusedCA     bool
	ReusedLeaf   bool
}

// CertPool returns an x509.CertPool trusting only this bundle's local CA.
func (bundle TLSBundle) CertPool() (*x509.CertPool, error) {
	if len(bundle.CACertPEM) == 0 {
		data, err := os.ReadFile(bundle.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read local CA certificate: %w", err)
		}
		bundle.CACertPEM = data
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle.CACertPEM) {
		return nil, errors.New("local CA certificate PEM is invalid")
	}
	return pool, nil
}

// ServerTLSConfig loads the leaf keypair and returns a TLS 1.2+ server config.
func (bundle TLSBundle) ServerTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(bundle.LeafCertPath, bundle.LeafKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load server TLS keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// EnsureTLS creates or reuses a strict RFC 5280 self-signed ECDSA P-256 CA and
// server leaf certificate in cfg.Dir. Private keys are written atomically with
// mode 0600; certificates are written atomically with mode 0644.
func EnsureTLS(cfg TLSConfig) (TLSBundle, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return TLSBundle{}, errors.New("TLS directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return TLSBundle{}, fmt.Errorf("create TLS directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	caCertPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")
	leafCertPath := filepath.Join(dir, "tls.crt")
	leafKeyPath := filepath.Join(dir, "tls.key")

	caCert, caKey, caPEM, reusedCA, err := loadOrCreateCA(caCertPath, caKeyPath, now)
	if err != nil {
		return TLSBundle{}, err
	}

	dnsNames, ips := collectSANs(cfg.Hosts)
	reusedLeaf := false
	if reusedCA {
		if existingLeafValid(leafCertPath, leafKeyPath, caCert, dnsNames, ips, now) {
			reusedLeaf = true
		}
	}
	if !reusedLeaf {
		if err := issueLeafCertificate(leafCertPath, leafKeyPath, caCert, caKey, dnsNames, ips, now); err != nil {
			return TLSBundle{}, err
		}
	}

	return TLSBundle{
		CACertPath:   caCertPath,
		CAKeyPath:    caKeyPath,
		LeafCertPath: leafCertPath,
		LeafKeyPath:  leafKeyPath,
		CACertPEM:    caPEM,
		ReusedCA:     reusedCA,
		ReusedLeaf:   reusedLeaf,
	}, nil
}

func loadOrCreateCA(certPath, keyPath string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, []byte, bool, error) {
	if cert, key, certPEM, ok := tryLoadCA(certPath, keyPath, now); ok {
		_ = os.Chmod(keyPath, 0o600)
		_ = os.Chmod(certPath, 0o644)
		return cert, key, certPEM, true, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, false, err
	}
	ski, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return nil, nil, nil, false, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Kodama Local CA",
			Organization: []string{"Kodama"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          ski,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("create CA certificate: %w", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("parse CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, nil, false, fmt.Errorf("write CA key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, nil, nil, false, fmt.Errorf("write CA certificate: %w", err)
	}
	return parsed, key, certPEM, false, nil
}

func tryLoadCA(certPath, keyPath string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, []byte, bool) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, nil, false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, false
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, nil, nil, false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, nil, false
	}
	ecKey, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, nil, false
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, nil, nil, false
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, nil, false
	}
	if now.Before(cert.NotBefore) || !now.Add(minRemainingValidity).Before(cert.NotAfter) {
		return nil, nil, nil, false
	}
	return cert, ecKey, certPEM, true
}

func existingLeafValid(certPath, keyPath string, caCert *x509.Certificate, dnsNames []string, ips []net.IP, now time.Time) bool {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return false
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.IsCA {
		return false
	}
	if now.Before(leaf.NotBefore) || !now.Add(minRemainingValidity).Before(leaf.NotAfter) {
		return false
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return false
	}
	for _, wantDNS := range dnsNames {
		if err := leaf.VerifyHostname(wantDNS); err != nil {
			return false
		}
	}
	for _, wantIP := range ips {
		if err := leaf.VerifyHostname(wantIP.String()); err != nil {
			return false
		}
	}
	_ = os.Chmod(keyPath, 0o600)
	_ = os.Chmod(certPath, 0o644)
	return true
}

func issueLeafCertificate(certPath, keyPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, dnsNames []string, ips []net.IP, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server TLS key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	ski, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return err
	}
	commonName := "localhost"
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Kodama"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          ski,
		AuthorityKeyId:        caCert.SubjectKeyId,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create server TLS certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal server TLS key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write server TLS key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write server TLS certificate: %w", err)
	}
	return nil
}

func collectSANs(extraHosts []string) ([]string, []net.IP) {
	dnsSet := []string{
		"localhost",
		"host.containers.internal",
		"host.docker.internal",
	}
	ipList := []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
	}
	addIP := func(candidate net.IP) {
		if candidate == nil || candidate.IsUnspecified() {
			return
		}
		if v4 := candidate.To4(); v4 != nil {
			candidate = v4
		}
		for _, existing := range ipList {
			if existing.Equal(candidate) {
				return
			}
		}
		ipList = append(ipList, candidate)
	}
	addDNS := func(name string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return
		}
		if !slices.Contains(dnsSet, name) {
			dnsSet = append(dnsSet, name)
		}
	}
	for _, raw := range extraHosts {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(trimmed); err == nil {
			trimmed = host
		}
		trimmed = strings.Trim(trimmed, "[]")
		if ip := net.ParseIP(trimmed); ip != nil {
			addIP(ip)
			continue
		}
		addDNS(trimmed)
	}
	return dnsSet, ipList
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	if serial.Sign() <= 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return bytes.Clone(sum[:20]), nil
}

// writeFileAtomic writes content to a sibling temporary file with mode and
// renames it into place so observers never read a truncated key or certificate.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Chmod(path, mode)
}
