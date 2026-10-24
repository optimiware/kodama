package logs_test

// The Pod-reachable runtime endpoint: one TLS listener on the host serving the
// real runtime routes of the control plane (admission/refresh/context from
// runtimeauth, result from results, artifact reserve/complete from artifacts)
// and a scoped, signed-upload forwarder to the real S3 endpoint. Pods reach it
// through the container provider's host address; the network policy allows only
// that address and port, so the S3 transfer goes through the same listener
// (the artifact store signs for this origin through its TransferClient and the
// forwarder preserves the signed Host). Per-attempt faults are injected here and
// nowhere in production code.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// reqEvent is one request the runtime endpoint served.
type reqEvent struct {
	Op       string // admit, token, context, result, source, reserve, complete, upload, other
	Attempt  domain.AttemptID
	Status   int
	Name     string // artifact name, when known
	Artifact domain.ArtifactID
	Faulted  string // "", "status", "lose"
}

// faultSpec injects one failure into the next matching request(s).
type faultSpec struct {
	Attempt domain.AttemptID
	Op      string // reserve, upload, complete, result
	Name    string // exact artifact name; "" matches every artifact
	Status  int    // answer with this status without processing
	Lose    bool   // process the request, then drop the connection
	Times   int    // remaining matches; < 0 unlimited
}

type runtimeServer struct {
	ln  net.Listener
	srv *http.Server
	URL string // https://<host IP>:<port>, the origin signed upload instructions use
	CA  []byte

	runtime, results, artifacts, secrets http.Handler
	s3                                   *httputil.ReverseProxy
	s3Prefix                             string // bucket-relative path prefix the forwarder accepts ("/<bucket>/<prefix>")

	mu       sync.Mutex
	events   []reqEvent
	tokens   map[domain.AttemptID][]string
	creds    map[domain.AttemptID]string // admission credentials presented (values the attempt holds)
	faults   []*faultSpec
	byURL    map[string]reserved // upload URL path -> artifact
	byID     map[domain.ArtifactID]reserved
	uploaded []string // object paths accepted by the forwarder
}

type reserved struct {
	Attempt domain.AttemptID
	Name    string
	ID      domain.ArtifactID
}

func newRuntimeServer(t testing.TB, hostIP string) *runtimeServer {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t064 runtime ca"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	must(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	must(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ip, err := netip.ParseAddr(hostIP)
	must(t, err)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "t064 runtime"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.IP(ip.AsSlice())},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	must(t, err)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	s := &runtimeServer{ln: ln, tokens: map[domain.AttemptID][]string{}, creds: map[domain.AttemptID]string{}, byURL: map[string]reserved{}, byID: map[domain.ArtifactID]reserved{}}
	s.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	s.URL = "https://" + net.JoinHostPort(hostIP, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	s.srv = &http.Server{
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		// A connection that may be aborted mid-response must be HTTP/1.1.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	s.srv.Handler = http.HandlerFunc(s.serve)
	return s
}

// bind installs the real handlers and the S3 forwarder, then starts serving.
func (s *runtimeServer) bind(t testing.TB, runtime, results, artifacts http.Handler, s3Endpoint, bucketPrefix string) {
	t.Helper()
	s.runtime, s.results, s.artifacts = runtime, results, artifacts
	target, err := url.Parse(s3Endpoint)
	must(t, err)
	s.s3Prefix = bucketPrefix
	s.s3 = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host // the signed host is the one the Pod used
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) },
	}
	go func() { _ = s.srv.ServeTLS(s.ln, "", "") }()
	t.Cleanup(func() { _ = s.srv.Close() })
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *recorder) Write(b []byte) (int, error) {
	if r.body.Len() < 1<<20 {
		r.body.Write(b)
	}
	return r.ResponseWriter.Write(b)
}
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// discard swallows a response so the connection can be dropped afterwards.
type discard struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) WriteHeader(c int)           { d.status = c }
func (d *discard) Write(b []byte) (int, error) { return d.body.Write(b) }

const attemptsPrefix = "/runtime/v1/attempts/"

func classify(path string) (attempt domain.AttemptID, op string) {
	rest, ok := strings.CutPrefix(path, attemptsPrefix)
	if !ok {
		return "", "other"
	}
	id, tail, _ := strings.Cut(rest, "/")
	attempt = domain.AttemptID(id)
	switch {
	case tail == "admit":
		return attempt, "admit"
	case tail == "token":
		return attempt, "token"
	case tail == "context":
		return attempt, "context"
	case tail == "result":
		return attempt, "result"
	case tail == "source":
		return attempt, "source"
	case tail == "secrets/resolve":
		return attempt, "secret"
	case tail == "artifacts":
		return attempt, "reserve"
	case strings.HasPrefix(tail, "artifacts/") && strings.HasSuffix(tail, "/complete"):
		return attempt, "complete"
	}
	return attempt, "other"
}

func (s *runtimeServer) serve(w http.ResponseWriter, r *http.Request) {
	attempt, op := classify(r.URL.Path)
	if !strings.HasPrefix(r.URL.Path, attemptsPrefix) {
		s.forward(w, r)
		return
	}
	var name string
	var artifact domain.ArtifactID
	body := []byte(nil)
	if r.Body != nil && (op == "reserve" || op == "complete" || op == "admit") {
		body, _ = readAllLimited(r)
		r.Body = readCloser(body)
	}
	switch op {
	case "admit":
		var req runtimev1.AdmitRequest
		if json.Unmarshal(body, &req) == nil && req.AdmissionCredential != "" {
			s.mu.Lock()
			s.creds[attempt] = req.AdmissionCredential
			s.mu.Unlock()
		}
	case "reserve":
		var req runtimev1.ArtifactReserveRequest
		if json.Unmarshal(body, &req) == nil {
			name = req.Name
		}
	case "complete":
		artifact = domain.ArtifactID(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, attemptsPrefix+string(attempt)+"/artifacts/"), "/"), "/complete"))
		s.mu.Lock()
		name = s.byID[artifact].Name
		s.mu.Unlock()
	}
	if f := s.match(attempt, op, name); f != nil {
		if f.Status != 0 {
			s.record(reqEvent{Op: op, Attempt: attempt, Status: f.Status, Name: name, Artifact: artifact, Faulted: "status"})
			contractFault(w, f.Status)
			return
		}
	}
	rec := &recorder{ResponseWriter: w, status: 200}
	var dw http.ResponseWriter = rec
	f := s.matchLose(attempt, op, name)
	var d *discard
	if f {
		d = &discard{h: http.Header{}, status: 200}
		dw = d
	}
	switch op {
	case "admit", "token", "context":
		s.runtime.ServeHTTP(dw, r)
	case "result", "source":
		s.results.ServeHTTP(dw, r)
	case "reserve", "complete":
		s.artifacts.ServeHTTP(dw, r)
	case "secret":
		if s.secrets != nil {
			s.secrets.ServeHTTP(dw, r)
		} else {
			contractFault(dw, http.StatusNotFound)
		}
	default:
		s.runtime.ServeHTTP(dw, r)
	}
	switch {
	case d != nil:
		s.record(reqEvent{Op: op, Attempt: attempt, Status: d.status, Name: name, Artifact: artifact, Faulted: "lose"})
		panic(http.ErrAbortHandler) // processed, answer lost
	default:
		s.afterServe(op, attempt, name, artifact, rec)
	}
}

func (s *runtimeServer) afterServe(op string, attempt domain.AttemptID, name string, artifact domain.ArtifactID, rec *recorder) {
	switch op {
	case "admit":
		var resp runtimev1.AdmitResponse
		if rec.status == 200 && json.Unmarshal(rec.body.Bytes(), &resp) == nil {
			s.addToken(attempt, resp.Token.Token)
		}
	case "token":
		var resp runtimev1.TokenResponse
		if rec.status == 200 && json.Unmarshal(rec.body.Bytes(), &resp) == nil {
			s.addToken(attempt, resp.Token.Token)
		}
	case "reserve":
		var resp runtimev1.ArtifactReserveResponse
		if (rec.status == 200 || rec.status == 201) && json.Unmarshal(rec.body.Bytes(), &resp) == nil {
			s.mu.Lock()
			s.byID[resp.ArtifactID] = reserved{Attempt: attempt, Name: name, ID: resp.ArtifactID}
			if resp.Upload != nil {
				if u, err := url.Parse(resp.Upload.URL); err == nil {
					s.byURL[u.Path] = reserved{Attempt: attempt, Name: name, ID: resp.ArtifactID}
				}
			}
			s.mu.Unlock()
			artifact = resp.ArtifactID
		}
	}
	s.record(reqEvent{Op: op, Attempt: attempt, Status: rec.status, Name: name, Artifact: artifact})
}

func (s *runtimeServer) addToken(attempt domain.AttemptID, tok string) {
	if tok == "" {
		return
	}
	s.mu.Lock()
	s.tokens[attempt] = append(s.tokens[attempt], tok)
	s.mu.Unlock()
}

func (s *runtimeServer) record(e reqEvent) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

// forward is the scoped signed-upload forwarder: only PUT of an object under the
// fixture's own bucket prefix is relayed, and only when an artifact reservation
// issued this exact URL; everything else is refused before S3 is contacted.
func (s *runtimeServer) forward(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	res, known := s.byURL[r.URL.Path]
	s.mu.Unlock()
	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, s.s3Prefix) || !known {
		s.record(reqEvent{Op: "upload", Status: http.StatusForbidden, Faulted: "refused"})
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f := s.match(res.Attempt, "upload", res.Name); f != nil && f.Status != 0 {
		s.record(reqEvent{Op: "upload", Attempt: res.Attempt, Status: f.Status, Name: res.Name, Artifact: res.ID, Faulted: "status"})
		_, _ = readAllLimited(r)
		w.WriteHeader(f.Status)
		return
	}
	rec := &recorder{ResponseWriter: w, status: 200}
	lose := s.matchLose(res.Attempt, "upload", res.Name)
	if lose {
		d := &discard{h: http.Header{}, status: 200}
		s.s3.ServeHTTP(d, r)
		s.record(reqEvent{Op: "upload", Attempt: res.Attempt, Status: d.status, Name: res.Name, Artifact: res.ID, Faulted: "lose"})
		panic(http.ErrAbortHandler)
	}
	s.s3.ServeHTTP(rec, r)
	s.mu.Lock()
	if rec.status/100 == 2 {
		s.uploaded = append(s.uploaded, r.URL.Path)
	}
	s.mu.Unlock()
	s.record(reqEvent{Op: "upload", Attempt: res.Attempt, Status: rec.status, Name: res.Name, Artifact: res.ID})
}

func (s *runtimeServer) match(attempt domain.AttemptID, op, name string) *faultSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.Times == 0 || f.Attempt != attempt || f.Op != op || f.Lose || (f.Name != "" && f.Name != name) {
			continue
		}
		if f.Times > 0 {
			f.Times--
		}
		return f
	}
	return nil
}

func (s *runtimeServer) matchLose(attempt domain.AttemptID, op, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.Times == 0 || f.Attempt != attempt || f.Op != op || !f.Lose || (f.Name != "" && f.Name != name) {
			continue
		}
		if f.Times > 0 {
			f.Times--
		}
		return true
	}
	return false
}

// inject arms a fault for one attempt. The attempt is known before its Job is
// dispatched, so the fault is in place before the first request.
func (s *runtimeServer) inject(f faultSpec) {
	if f.Times == 0 {
		f.Times = -1
	}
	s.mu.Lock()
	s.faults = append(s.faults, &f)
	s.mu.Unlock()
}

func (s *runtimeServer) clearFaults() {
	s.mu.Lock()
	s.faults = nil
	s.mu.Unlock()
}

func (s *runtimeServer) eventsOf(attempt domain.AttemptID, op string) (out []reqEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Attempt == attempt && (op == "" || e.Op == op) {
			out = append(out, e)
		}
	}
	return
}

// successfulUploads lists the artifact names whose transfer reached S3.
func (s *runtimeServer) successfulUploads(attempt domain.AttemptID) (names []string) {
	for _, e := range s.eventsOf(attempt, "upload") {
		if e.Status/100 == 2 && e.Faulted == "" {
			names = append(names, e.Name)
		}
	}
	return
}

func (s *runtimeServer) tokensOf(attempt domain.AttemptID) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens[attempt]...)
}

// credentialOf is the admission credential the attempt presented ("" before).
func (s *runtimeServer) credentialOf(attempt domain.AttemptID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds[attempt]
}

func (s *runtimeServer) refused() (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Faulted == "refused" {
			n++
		}
	}
	return
}

func contractFault(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	code := domain.CodeUnavailable
	switch status {
	case http.StatusForbidden:
		code = domain.CodeForbidden
	case http.StatusNotFound:
		code = domain.CodeNotFound
	}
	_ = json.NewEncoder(w).Encode(runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: "t064 injected fault", RequestID: "req_t064fault", Details: map[string]any{}}})
}

func readAllLimited(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 8<<20))
}

func readCloser(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
