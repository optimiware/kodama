// certs generates a per-scope development CA and Git server certificate.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: certs DIRECTORY DNS_NAME")
	}
	dir, host := os.Args[1], os.Args[2]
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caSerial, err := serial()
	if err != nil {
		return err
	}
	leafSerial, err := serial()
	if err != nil {
		return err
	}
	// A development scope can live for months; a year avoids silent expiry.
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour)
	template := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Kodama development CA"}, NotBefore: notBefore, NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: leafSerial, DNSNames: []string{host}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The scope directory is private on the host and mounted read-only into
	// fixture containers that run as another UID, so files must be readable.
	for name, block := range map[string]*pem.Block{"ca.crt": {Type: "CERTIFICATE", Bytes: caDER}, "tls.crt": {Type: "CERTIFICATE", Bytes: der}, "tls.key": {Type: "PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			return err
		}
	}
	return nil
}
