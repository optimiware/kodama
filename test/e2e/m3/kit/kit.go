// Package kit is the shared fixture of T-044's M3 acceptance: the M2 kit's
// installation (the actual core assembly, KODAMA_ENV=test, over real PostgreSQL,
// JetStream, S3, a Git host over TLS, an OCI registry, the Podman runtime and a
// kind namespace as the stage's deployment target) started with the
// business-loop installation statement (T-075): a ChatOps destination policy and
// canonical review-link origin, and the QA mapping principal.
//
// What is real: the server and every module it wires, the outbox, the broker,
// the notification worker and its outbound client, the work, gate, delivery and
// metrics stores and the identity authority. What is test-owned and
// deterministic: the QA "agent" (a /bin/sh program in the tools image, see
// qa.go), the receiver of the notifications (a local TLS server) and the human
// reviewer, who is an authenticated persona distinct from every workload.
// Nothing here is a browser, live-provider or wrapper qualification (T-069,
// T-085).
package kit

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	oidcfx "github.com/optimiware/kodama/test/support/oidc"
)

// MapperPersona is the identity the QA mapping files defects as: a user bound as
// analyst in the project, distinct from the human reviewer and from every
// workload (a workload identity cannot even ask for the mapping).
const MapperPersona = "qa-mapper"

func init() {
	oidcfx.Personas = append(oidcfx.Personas, oidcfx.Persona{ID: MapperPersona, Roles: []string{"analyst"}})
}

// Options select what a test needs.
type Options struct {
	// Tune adjusts the operational bounds after the kit's ChatOps values.
	Tune func(*app.Tuning)
	// Config adjusts the configuration after the business-loop statement.
	Config func(*app.CoreConfig)
	// NoQA leaves the QA mapping unconfigured (no principal).
	NoQA bool
	// NoChatOps leaves the notification statement out (no origin, no policy).
	NoChatOps bool
	// ReceiverBehavior is the receiver's first behavior (nil accepts).
	ReceiverBehavior chatopstest.Behavior
	// Delivery builds the real deployment-tools image (needed to stage).
	Delivery bool
	// ForeignReceiver also allows tenant B's destinations to reach the receiver
	// (the policy is per tenant, so a tenant B destination can exist).
	ForeignReceiver bool
}

// Env is one installation with the business loop selected.
type Env struct {
	*m2.Env

	// Receiver is the ChatOps webhook receiver, a local TLS server whose
	// behavior the test changes.
	Receiver *chatopstest.Receiver
	// Links is the canonical origin review links are built from: a TLS front of
	// the public API. A person following a link reaches the API through it.
	Links *LinkFront
	// CAFile is the PEM the notification client trusts (the receiver's).
	CAFile string
	// MapperID is the QA mapping principal's identity.
	MapperID string

	st *state
}

type state struct{ mapper string }

// LinkFront is the TLS origin of the public API as people reach it.
type LinkFront struct {
	// Origin is the canonical https origin ("https://127.0.0.1:port").
	Origin string
	srv    *httptest.Server
	client *http.Client
}

// New provisions the isolated dependencies, the receiver and the origin, starts
// the application with the business-loop statement and seeds tenant A. The QA
// mapping principal is bound as analyst and the process restarted with it, since
// a user exists only after its first login.
func New(t *testing.T, o Options) *Env {
	t.Helper()
	e := &Env{st: &state{}}

	e.Receiver = chatopstest.NewReceiver(t, o.ReceiverBehavior)
	dir := t.TempDir()
	e.Links = &LinkFront{}
	e.Links.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(e.Stack.Base())
		(&httputil.ReverseProxy{
			Rewrite: func(p *httputil.ProxyRequest) { p.SetURL(u); p.Out.Host = p.In.Host },
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "the server is not serving", http.StatusBadGateway)
			},
		}).ServeHTTP(w, r)
	}))
	t.Cleanup(e.Links.srv.Close)
	e.Links.Origin = e.Links.srv.URL
	pool := x509.NewCertPool()
	pool.AddCert(e.Links.srv.Certificate())
	e.Links.client = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	e.CAFile = filepath.Join(dir, "receiver-ca.pem")
	// httptest serves every TLS server with one built-in certificate, so the
	// receiver's trust root is the origin's too.
	writeCA(t, e.CAFile, e.Links.srv.Certificate())

	e.Env = m2.New(t, m2.Options{
		NoProfile: true,
		Delivery:  o.Delivery,
		Tune: func(tu *app.Tuning) {
			tu.ChatOps = FastChatOps()
			if o.Tune != nil {
				o.Tune(tu)
			}
		},
		Config: func(cfg *app.CoreConfig) {
			cfg.BusinessLoop = e.loopConfig(o)
			if o.Config != nil {
				o.Config(cfg)
			}
		},
	})
	if !o.NoQA {
		e.AddMember(MapperPersona, "analyst")
		e.MapperID = e.UserID(MapperPersona)
		e.st.mapper = e.MapperID
		e.Restart()
	}
	return e
}

func (e *Env) loopConfig(o Options) app.BusinessLoopConfig {
	var c app.BusinessLoopConfig
	if !o.NoChatOps {
		c.ChatOps = &app.ChatOpsConfig{
			Origin:           e.Links.Origin,
			Ports:            []int{int(e.Receiver.AddrPort().Port())},
			PrivateEndpoints: []app.PrivateEndpointConfig{{Tenant: m2.TenantA, Address: e.Receiver.AddrPort().String()}},
			CAFile:           e.CAFile,
		}
		if o.ForeignReceiver {
			c.ChatOps.PrivateEndpoints = append(c.ChatOps.PrivateEndpoints, app.PrivateEndpointConfig{Tenant: m2.TenantB, Address: e.Receiver.AddrPort().String()})
		}
	}
	if !o.NoQA && e.st.mapper != "" {
		c.QA = &app.QAMappingConfig{MappingPrincipal: e.st.mapper, ReportOutput: ReportOutput, DeploymentInput: DeploymentInput}
	}
	return c
}

// FastChatOps are the notification worker's bounds for the tests: a short
// schedule so an outage and its recovery fit in a test, never production values.
func FastChatOps() app.ChatOpsTuning {
	t := app.DefaultChatOpsTuning()
	t.Retry = chatops.RetryPolicy{Initial: 300 * time.Millisecond, Max: 1500 * time.Millisecond, MaxAttempts: 4}
	t.Interval = 150 * time.Millisecond
	t.Lease = 20 * time.Second
	t.AttemptTimeout = 3 * time.Second
	return t
}

// Declared names of the QA mapping (the installation statement) and of the
// pinned run input and declared output they name.
const (
	ReportOutput    = "report"
	DeploymentInput = "deployment"
)

// Follow issues a GET of a review link as the persona through the canonical
// origin, the way a person who received the notification would.
func (e *Env) Follow(persona, link string) (int, []byte) {
	e.T.Helper()
	return e.FollowWith(http.MethodGet, persona, link, nil)
}

// FollowWith is Follow with another method and an optional JSON body: what a
// holder of the link could try besides reading it.
func (e *Env) FollowWith(method, persona, link string, body any) (int, []byte) {
	t := e.T
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, link, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if persona != "" {
		req.Header.Set("Authorization", "Bearer "+e.Token(persona))
	}
	resp, err := e.Links.client.Do(req)
	if err != nil {
		t.Fatalf("follow %s: %v", link, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Loop is the business-loop slice of the running process.
func (e *Env) Loop() *app.BusinessLoopSlice {
	e.T.Helper()
	l := e.Server().Core().BusinessLoop
	if l == nil {
		e.T.Fatal("the process has no business-loop slice")
	}
	return l
}

func writeCA(t testing.TB, path string, cert *x509.Certificate) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tenantA() string { return m2.TenantA }
