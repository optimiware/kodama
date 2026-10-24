package harnessopencode_test

// The network surface Pods reach, all signed by one test CA that the Job's
// bootstrap Secret carries: the platform origin (runtime API, model gateway and
// MCP proxy on one TLS listener, as one server would serve them), a TLS front
// for the S3 endpoint (artifact upload instructions must name an address Pods
// can reach) and a TLS front for the Git remote. The Job now delivers the
// read-only Git credential and a separate Git CA (T-079); these fronts carry no
// authority of their own.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	PEM  []byte
	File string
}

func newCA(t testing.TB) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t031 platform ca"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &testCA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	ca.File = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca.File, ca.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return ca
}

// leaf signs a server certificate valid for the IPs.
func (ca *testCA) leaf(t testing.TB, serial int64, ips ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "t031 server"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range ips {
		tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// listener is one TLS server on every interface, addressable from Pods as
// https://<hostIP>:<port>.
type listener struct {
	URL  string
	Port int
	srv  *http.Server
}

func serveTLS(t testing.TB, ca *testCA, hostIP string, serial int64, h http.Handler) *listener {
	t.Helper()
	return serveTLSOn(t, ca, "0.0.0.0", hostIP, serial, h)
}

// serveTLSLoopback is a listener only the test host can reach.
func serveTLSLoopback(t testing.TB, ca *testCA, serial int64, h http.Handler) *listener {
	t.Helper()
	return serveTLSOn(t, ca, "127.0.0.1", "127.0.0.1", serial, h)
}

func serveTLSOn(t testing.TB, ca *testCA, bind, hostIP string, serial int64, h http.Handler) *listener {
	t.Helper()
	ln, err := net.Listen("tcp", bind+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	l := &listener{Port: port, URL: "https://" + net.JoinHostPort(hostIP, fmt.Sprint(port))}
	l.srv = &http.Server{
		Handler:           h,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{ca.leaf(t, serial, hostIP, "127.0.0.1")}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = l.srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = l.srv.Close() })
	return l
}

// reverse is a TLS-terminating reverse proxy to target. keepHost forwards the
// client's Host header (a presigned S3 URL signs it); inject, when non-nil,
// adds an Authorization header to requests that carry none.
func reverse(target *url.URL, upstreamCA []byte, keepHost bool, inject string) http.Handler {
	pool := x509.NewCertPool()
	if len(upstreamCA) > 0 {
		pool.AppendCertsFromPEM(upstreamCA)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if keepHost {
				pr.Out.Host = pr.In.Host
			}
			if inject != "" && pr.Out.Header.Get("Authorization") == "" {
				pr.Out.Header.Set("Authorization", inject)
			}
		},
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DisableCompression: true,
			ResponseHeaderTimeout: 2 * time.Minute,
		},
	}
	return rp
}

// rtAuth is the runtime-token authenticator the artifact router needs (T-027
// supplies it in production): it verifies the bearer and emits the runtime
// principal; every other fact is read from the durable grant.
type rtAuth struct{ v *jwt.Verifier }

func (a rtAuth) Authenticate(ctx context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	claims, err := a.v.Verify(ctx, tok)
	if err != nil {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: resultsadapters.RuntimePrincipal(claims.Tenant, claims.Session)}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

// accessLog records which paths a listener served, for assertions about what
// did and did not reach it.
type accessLog struct {
	mu   sync.Mutex
	rows []string
}

func (l *accessLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.rows = append(l.rows, r.Method+" "+r.URL.Path)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (l *accessLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

// hostTransport is an HTTP transport for the test host: the container
// provider's host alias (the address Pods use) is not routable from the host
// itself, so connections to it are made to loopback, where the same listeners
// are bound. TLS verification still uses the alias (the certificate carries it).
func hostTransport(ca *testCA, alias string) *http.Transport {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if h, p, err := net.SplitHostPort(addr); err == nil && h == alias {
				addr = net.JoinHostPort("127.0.0.1", p)
			}
			return d.DialContext(ctx, network, addr)
		},
	}
}
