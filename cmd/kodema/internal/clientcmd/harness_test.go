package clientcmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCmdHarnessHelperProcess(t *testing.T) {
	if os.Getenv("KODEMA_CLIENTCMD_HARNESS_HELPER") != "1" {
		return
	}
	base := os.Getenv("OPENAI_BASE_URL")
	key := os.Getenv("OPENAI_API_KEY")
	req, _ := http.NewRequest(http.MethodPost, base+"/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper request failed: %v\n", err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "helper status=%d body=%s\n", resp.StatusCode, string(body))
		os.Exit(2)
	}
	fmt.Println(strings.TrimSpace(string(body)))
	os.Exit(0)
}

func TestClientCmdRun_HarnessExecUsesIsolatedTransportAndTLSCA(t *testing.T) {
	var poisonHits atomic.Int32
	poisonProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		poisonHits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer poisonProxy.Close()

	const execToken = "kodex.exec-clientcmd-1.secret"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+execToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"via":"clientcmd-e2e-ok"}`))
	})

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ClientCmd Harness Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	srv := httptest.NewUnstartedServer(upstream)
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
		}},
	}
	srv.StartTLS()
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "KODEMA_TOKEN")
	if err := os.WriteFile(tokenFile, []byte(execToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	// Set KODEMA_CA_FILE (not --ca-file on harness exec) AND poison HTTP_PROXY/HTTPS_PROXY
	// so this exercises the real clientcmd.Run path where deps.HTTP is populated by newAPITransport!
	t.Setenv("KODEMA_URL", srv.URL)
	t.Setenv("KODEMA_CA_FILE", caFile)
	t.Setenv("HTTP_PROXY", poisonProxy.URL)
	t.Setenv("HTTPS_PROXY", poisonProxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("KODEMA_CLIENTCMD_HARNESS_HELPER", "1")

	var stdout, stderr bytes.Buffer
	code := Run(
		context.Background(),
		[]string{
			"harness", "exec",
			"--token-file", tokenFile,
			"--",
			os.Args[0], "-test.run=^TestClientCmdHarnessHelperProcess$",
		},
		&stdout,
		&stderr,
		nil,
	)
	if code != 0 {
		t.Fatalf("clientcmd.Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"via":"clientcmd-e2e-ok"`) {
		t.Fatalf("unexpected stdout: %q (stderr=%q)", stdout.String(), stderr.String())
	}
	if got := poisonHits.Load(); got != 0 {
		t.Fatalf("poison HTTP_PROXY received %d requests via clientcmd.Run; want 0", got)
	}
}
