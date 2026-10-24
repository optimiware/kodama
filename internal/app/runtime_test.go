package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
)

type fxRepos struct {
	cfg workspace.RepositoryConfig
	err error
}

func (r fxRepos) Repository(context.Context, domain.Scope) (workspace.RepositoryConfig, error) {
	return r.cfg, r.err
}

type fxCreds struct {
	cred gitad.Credential
	err  error
}

func (c fxCreds) Resolve(context.Context, domain.Scope, string) (gitad.Credential, error) {
	return c.cred, c.err
}

func TestCheckoutProviderIssuesOnlyTheAttemptsOwnRepository(t *testing.T) {
	ca := kubetest.CA(t)
	req := kube.CheckoutRequest{
		Scope: fxScope, WorkspaceID: "ws_1", RepositoryURL: "https://git.example/acme/repo.git", Access: execution.AccessRead,
	}
	ok := checkoutProvider{
		repos: fxRepos{cfg: workspace.RepositoryConfig{URL: req.RepositoryURL, CheckoutSecretRef: "secret://installation/checkout"}},
		creds: fxCreds{cred: gitad.Credential{Username: "kodama", Password: "s3cret"}}, gitCA: ca,
	}
	m, err := ok.Checkout(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Credential) != "kodama:s3cret" || m.Scope != fxScope || m.WorkspaceID != "ws_1" || m.RepositoryURL != req.RepositoryURL || string(m.GitCA) != string(ca) {
		t.Fatalf("material %+v", m)
	}
	if strings.Contains(m.String(), "s3cret") {
		t.Fatal("the credential is printable")
	}

	denied := map[string]checkoutProvider{
		"no repository": {repos: fxRepos{err: workspace.ErrNotFound}, creds: ok.creds, gitCA: ca},
		"another repository": {
			repos: fxRepos{cfg: workspace.RepositoryConfig{URL: "https://git.example/acme/other.git", CheckoutSecretRef: "secret://installation/checkout"}}, creds: ok.creds, gitCA: ca,
		},
		"no credential":     {repos: ok.repos, creds: fxCreds{err: errors.New("none")}, gitCA: ca},
		"colon in the user": {repos: ok.repos, creds: fxCreds{cred: gitad.Credential{Username: "a:b", Password: "p"}}, gitCA: ca},
		"empty password":    {repos: ok.repos, creds: fxCreds{cred: gitad.Credential{Username: "a"}}, gitCA: ca},
		"no git CA":         {repos: ok.repos, creds: ok.creds},
	}
	for name, p := range denied {
		if _, err := p.Checkout(context.Background(), req); !errors.Is(err, kube.ErrCheckoutDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An outage of the repository lookup is not a refusal: the dispatch is redriven.
	outage := errors.New("the database is down")
	if _, err := (checkoutProvider{repos: fxRepos{err: outage}, creds: ok.creds, gitCA: ca}).Checkout(context.Background(), req); !errors.Is(err, outage) || errors.Is(err, kube.ErrCheckoutDenied) {
		t.Fatalf("an outage must stay an outage: %v", err)
	}
}

func TestKubernetesPolicyIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := fxKubeConfig(t)
	valid, err := os.ReadFile(good.PolicyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadKubePolicy(good.PolicyFile); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"unknown field": strings.Replace(string(valid), `{`, `{"Typo":1,`, 1),
		"trailing data": string(valid) + `{}`,
		"not json":      `policy: yes`,
	} {
		if _, err := loadKubePolicy(write(name, content)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := loadKubePolicy(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing policy was accepted")
	}
}

func TestKubernetesClientAuthenticatesEveryRequestWithTheCurrentToken(t *testing.T) {
	kc := fxKubeConfig(t)
	client, err := newKubeHTTPClient(kc)
	if err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect == nil {
		t.Fatal("redirects would forward the token")
	}
	var seen string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("Authorization")
		return httptest.NewRecorder().Result(), nil
	})
	tr := &bearerTransport{base: base, tokenFile: kc.TokenFile}
	req := httptest.NewRequest("GET", "https://192.0.2.1/api", nil)
	if _, err := tr.RoundTrip(req); err != nil || seen != "Bearer service-account-token" {
		t.Fatalf("%q %v", seen, err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("the caller's request was modified")
	}
	// The kubelet rotates the token: the next request carries the new one.
	if err := os.WriteFile(kc.TokenFile, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err != nil || seen != "Bearer rotated" {
		t.Fatalf("%q %v", seen, err)
	}
	if err := os.WriteFile(kc.TokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("a request was sent without a token")
	}
	for name, mutate := range map[string]func(*KubernetesConfig){
		"no CA file":  func(k *KubernetesConfig) { k.CAFile = filepath.Join(t.TempDir(), "absent") },
		"empty token": func(k *KubernetesConfig) { k.TokenFile = kc.TokenFile }, // emptied above
		"CA without certificates": func(k *KubernetesConfig) {
			p := filepath.Join(t.TempDir(), "ca")
			_ = os.WriteFile(p, []byte("nothing"), 0o600)
			k.CAFile = p
		},
	} {
		k := kc
		mutate(&k)
		if _, err := newKubeHTTPClient(k); err == nil {
			t.Errorf("%s: a client was built", name)
		}
	}
	block, _ := pem.Decode(kubetest.CA(t))
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLateRegistrarRefusesUntilBound(t *testing.T) {
	var late LateRegistrar
	cmd := runtimeauth.RegisterCommand{}
	if _, err := late.Register(context.Background(), cmd); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("an unbound registrar must refuse: %v", err)
	}
	var calls atomic.Int32
	late.Bind(registrarFunc(func(context.Context, runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
		calls.Add(1)
		return runtimeauth.Registration{}, nil
	}))
	if _, err := late.Register(context.Background(), cmd); err != nil || calls.Load() != 1 {
		t.Fatalf("%v %d", err, calls.Load())
	}
}

type registrarFunc func(context.Context, runtimeauth.RegisterCommand) (runtimeauth.Registration, error)

func (f registrarFunc) Register(ctx context.Context, c runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	return f(ctx, c)
}

type verifierFunc func(context.Context, string) (runtimeauth.TokenClaims, error)

func (f verifierFunc) Verify(ctx context.Context, raw string) (runtimeauth.TokenClaims, error) {
	return f(ctx, raw)
}

func TestRuntimeRouterAuthenticatesRuntimeTokensOnly(t *testing.T) {
	outage := errors.New("keys unavailable")
	a := runtimeAuthenticator{verifier: verifierFunc(func(_ context.Context, raw string) (runtimeauth.TokenClaims, error) {
		switch raw {
		case "good":
			return runtimeauth.TokenClaims{Tenant: "t_acme", Session: "ses_abc"}, nil
		case "outage":
			return runtimeauth.TokenClaims{}, outage
		}
		return runtimeauth.TokenClaims{}, runtimeauth.ErrInvalidCredential
	})}
	call := func(headers ...string) (httpapi.Principal, error) {
		r := httptest.NewRequest("POST", "/runtime/v1/attempts/att_1/artifacts", nil)
		for _, h := range headers {
			r.Header.Add("Authorization", h)
		}
		return a.Authenticate(context.Background(), r)
	}
	p, err := call("Bearer good")
	if err != nil || p.ID != "runtime:t_acme:ses_abc" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, hs := range map[string][]string{
		"no header": nil, "two headers": {"Bearer good", "Bearer good"}, "basic": {"Basic good"}, "empty token": {"Bearer "},
		"spaces": {"Bearer good extra"}, "invalid": {"Bearer forged"},
	} {
		if _, err := call(hs...); !errors.Is(err, httpapi.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := call("Bearer outage"); !errors.Is(err, outage) || errors.Is(err, httpapi.ErrUnauthenticated) {
		t.Errorf("an outage is not a bad credential: %v", err)
	}

	z := runtimeAuthorizer{}
	target := httpapi.Target{Resource: httpapi.Reference{Kind: "attempt", ID: "att_1"}}
	if err := z.Authorize(context.Background(), p, "artifact.reserve", target); err != nil {
		t.Fatal(err)
	}
	if err := z.Authorize(context.Background(), p, "artifact.complete", target); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		p      httpapi.Principal
		action httpapi.Action
		t      httpapi.Target
	}{
		"a user principal":    {httpapi.Principal{ID: "u_1"}, "artifact.reserve", target},
		"another action":      {p, "run.create", target},
		"another resource":    {p, "artifact.reserve", httpapi.Target{Resource: httpapi.Reference{Kind: "run", ID: "att_1"}}},
		"a malformed attempt": {p, "artifact.reserve", httpapi.Target{Resource: httpapi.Reference{Kind: "attempt", ID: "att/../1"}}},
		"no resource":         {p, "artifact.reserve", httpapi.Target{}},
	} {
		if err := z.Authorize(context.Background(), tc.p, tc.action, tc.t); !errors.Is(err, httpapi.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRuntimeSigningKeyIsLoadedAtItsGeneration(t *testing.T) {
	dir := t.TempDir()
	key, err := rtjwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := loadRuntimeKeys(RuntimeConfig{SigningKeyFile: path, SigningGeneration: 7})
	if err != nil || ring.Generation() != 7 {
		t.Fatalf("%v", err)
	}
	for name, c := range map[string]RuntimeConfig{
		"missing file":       {SigningKeyFile: filepath.Join(dir, "absent"), SigningGeneration: 1},
		"generation zero":    {SigningKeyFile: path},
		"not a PEM key file": {SigningKeyFile: func() string { p := filepath.Join(dir, "bad"); _ = os.WriteFile(p, []byte("x"), 0o600); return p }(), SigningGeneration: 1},
	} {
		if _, err := loadRuntimeKeys(c); err == nil {
			t.Errorf("%s: a key ring was built", name)
		}
	}
}

func TestLoopRunsNowRepeatsAndStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var passes atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- loop(ctx, time.Millisecond, func(context.Context) error {
			if passes.Add(1) == 1 {
				return errors.New("the first pass fails and is repeated")
			}
			return nil
		}, nil, "test")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for passes.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the loop did not repeat")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if err := loop(context.Background(), 0, func(context.Context) error { return nil }, nil, "test"); err == nil {
		t.Fatal("a loop without an interval ran")
	}
}
