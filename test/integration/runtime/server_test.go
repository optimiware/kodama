package runtime_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
)

// server is the runtime API the entrypoint talks to: the real runtime
// admission routes behind TLS, and a stand-in for the result route (T-022)
// that authorizes through runtimeauth and commits through execution.
type server struct {
	ln   net.Listener
	srv  *http.Server
	port string
	CA   []byte

	rt     *runtimeauth.Service
	exec   *execution.Service
	admits atomic.Int32
	// Results counts accepted result submissions.
	Results atomic.Int32
}

// hostAliases are the names a workload reaches the host by.
var hostAliases = []string{"localhost", "host.containers.internal"}

func newServer(t *testing.T) *server {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(t, err)
		return k
	}
	caKey, leafKey := key(), key()
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t086 runtime ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	must(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	must(t, err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "t086 runtime"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: hostAliases, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	must(t, err)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return &server{
		ln: ln, port: port, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		srv: &http.Server{
			TLSConfig:         &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}, MinVersion: tls.VersionTLS12},
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// URL is the server's origin as host reaches it.
func (s *server) URL(host string) string { return "https://" + net.JoinHostPort(host, s.port) }

// start serves the routes; the services are bound late because the runtime
// they depend on needs the URL and CA first.
func (s *server) start(t *testing.T, rt *runtimeauth.Service, exec *execution.Service) {
	t.Helper()
	s.rt, s.exec = rt, exec
	admission, err := runtimehttp.NewHandler(runtimehttp.Config{Service: rt})
	must(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runtimev1.BasePath+"/result", s.result)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/admit") {
			s.admits.Add(1)
		}
		admission.ServeHTTP(w, r)
	}))
	s.srv.Handler = mux
	go func() { _ = s.srv.ServeTLS(s.ln, "", "") }()
	t.Cleanup(func() { _ = s.srv.Close() })
}

func (s *server) result(w http.ResponseWriter, r *http.Request) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	req, err := runtimev1.DecodeRequest[runtimev1.ResultRequest](body)
	if err != nil {
		reply(w, domain.CodeInvalidRequest)
		return
	}
	az, err := s.rt.Authorize(r.Context(), runtimeauth.AuthorizeRequest{
		AttemptID: attempt, Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Action: runtimeauth.ActionResultSubmit,
	})
	switch {
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		reply(w, runtimev1.CodeAttemptRevoked)
		return
	case err != nil:
		reply(w, domain.CodeInvalidCredential)
		return
	}
	ack, err := s.exec.CommitResult(r.Context(), execution.ResultCommit{Scope: az.Scope, AttemptID: attempt, Request: req})
	switch {
	case errors.Is(err, execution.ErrAttemptRevoked):
		reply(w, runtimev1.CodeAttemptRevoked)
		return
	case err != nil:
		reply(w, domain.CodeUnavailable)
		return
	}
	s.Results.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runtimev1.ResultResponse{
		AttemptID: attempt, ResultHash: runtimev1.Digest(ack.ResultHash), AttemptStatus: runtimev1.AttemptStatus(ack.AttemptStatus),
		AcceptedAt: runtimev1.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)),
	})
}

func reply(w http.ResponseWriter, code domain.ErrorCode) {
	status, _ := runtimev1.Status(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: "t086 fixture", RequestID: "req_t086fixture", Details: map[string]any{}}})
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
