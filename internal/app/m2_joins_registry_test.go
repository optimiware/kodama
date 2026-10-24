package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// registryManifest is a minimal valid OCI image manifest; its digest is what
// the producing attempt registers.
var registryManifest = []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":2},"layers":[]}`)

// fakeRegistry serves the digest-addressed manifest of one repository and
// records what it was asked.
type fakeRegistry struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	auths  []string
	status int
	body   []byte
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{status: http.StatusOK, body: registryManifest}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths, f.auths = append(f.paths, r.URL.Path), append(f.auths, r.Header.Get("Authorization"))
		status, body := f.status, f.body
		f.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }
func (f *fakeRegistry) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

// runReads is the execution store's run read: one run with the given attempts.
type runReads struct {
	agg *execution.Aggregate
	err error
}

func (r runReads) Get(_ context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.agg == nil || r.agg.Run.Scope != scope || r.agg.Run.ID != id {
		return nil, execution.ErrNotFound
	}
	return r.agg, nil
}

func producerRun(t *testing.T, spec execution.AttemptSpec) runReads {
	t.Helper()
	at := &execution.Attempt{Scope: spec.Scope, ID: spec.AttemptID, RunID: spec.RunID, StepID: spec.StepID, Context: spec.Context, ContextDigest: spec.ContextDigest}
	return runReads{agg: &execution.Aggregate{Run: &execution.Run{Scope: spec.Scope, ID: spec.RunID}, Attempts: []*execution.Attempt{at}}}
}

// Test_app_registry_verifier_bound: an `ociArtifact` registration verifies
// against the configured registry, with the producing attempt's own source as
// provenance, and is refused without one.
func Test_app_registry_verifier_bound(t *testing.T) {
	reg := newFakeRegistry(t)
	spec := checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo)
	run := producerRun(t, spec)
	digest := artifacts.DigestOf(registryManifest)
	cfg := RegistryConfig{Endpoints: []RegistryEndpoint{{
		Host: reg.host(), URL: reg.srv.URL, AllowInsecureHTTP: true, Tenants: []string{string(spec.Scope.TenantID)},
		Repositories: []string{"team/app"},
	}}}
	request := func(repository string, mutate func(*artifacts.OCIRequest)) artifacts.OCIRequest {
		r := artifacts.OCIRequest{Scope: spec.Scope, Attempt: spec.AttemptID, Run: spec.RunID, Registry: reg.host() + "/" + repository, Digest: digest}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}
	verifier := func(t *testing.T, c RegistryConfig, runs RunReader) artifacts.RegistryVerifier {
		t.Helper()
		v, configured, err := NewRegistryVerifier(c, Test, runs)
		if err != nil || !configured {
			t.Fatalf("verifier %v configured=%v", err, configured)
		}
		return v
	}

	t.Run("a registration verifies against the configured registry", func(t *testing.T) {
		before := reg.requests()
		v := verifier(t, cfg, run)
		got, err := v.Verify(context.Background(), request("team/app", nil))
		if err != nil {
			t.Fatal(err)
		}
		want := artifacts.Provenance{RepositoryURL: checkoutRepo, Commit: checkoutCommit}
		if got.Repository != reg.host()+"/team/app" || got.Digest != digest || got.Provenance != want || got.ManifestSize != int64(len(registryManifest)) {
			t.Fatalf("verification %+v", got)
		}
		reg.mu.Lock()
		defer reg.mu.Unlock()
		if len(reg.paths) != before+1 || reg.paths[before] != "/v2/team/app/manifests/"+string(digest) {
			t.Fatalf("the registry was asked %v", reg.paths[before:])
		}
	})

	t.Run("the credential goes to the configured origin only", func(t *testing.T) {
		c := cfg
		c.Endpoints = []RegistryEndpoint{cfg.Endpoints[0]}
		c.Endpoints[0].Authorization = "Basic dDEwMTpzZWNyZXQ="
		c.Endpoints[0].AllowInsecureHTTP = true
		if err := c.Validate(Test); err == nil {
			t.Fatal("an authorization over plain http was accepted")
		}
		// Over https the header is sent (the TLS client is the test server's).
		tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Basic dDEwMTpzZWNyZXQ=" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(registryManifest)
		}))
		defer tls.Close()
		host := strings.TrimPrefix(tls.URL, "https://")
		sc := RegistryConfig{HTTPClient: tls.Client(), Endpoints: []RegistryEndpoint{{
			Host: host, URL: tls.URL, Authorization: "Basic dDEwMTpzZWNyZXQ=", Tenants: []string{"*"}, Repositories: []string{"team"},
		}}}
		v := verifier(t, sc, run)
		if _, err := v.Verify(context.Background(), artifacts.OCIRequest{Scope: spec.Scope, Attempt: spec.AttemptID, Run: spec.RunID, Registry: host + "/team/app", Digest: digest}); err != nil {
			t.Fatalf("an authorized registration: %v", err)
		}
		sc.Endpoints[0].Authorization = "Basic d3Jvbmc="
		v = verifier(t, sc, run)
		if _, err := v.Verify(context.Background(), artifacts.OCIRequest{Scope: spec.Scope, Attempt: spec.AttemptID, Run: spec.RunID, Registry: host + "/team/app", Digest: digest}); !errors.Is(err, artifacts.ErrRegistryDenied) {
			t.Fatalf("a rejected credential: %v", err)
		}
	})

	t.Run("a registration outside the operator's statement is refused before any request", func(t *testing.T) {
		v := verifier(t, cfg, run)
		before := reg.requests()
		for name, req := range map[string]artifacts.OCIRequest{
			"a repository outside the prefixes": request("team/application", nil),
			"a sibling prefix":                  request("other/app", nil),
			"another registry":                  request("team/app", func(r *artifacts.OCIRequest) { r.Registry = "registry.example.test/team/app" }),
			"another tenant": request("team/app", func(r *artifacts.OCIRequest) {
				r.Scope = domain.Scope{TenantID: "t_other", ProjectID: r.Scope.ProjectID}
			}),
			"a scheme": request("team/app", func(r *artifacts.OCIRequest) { r.Registry = "http://" + r.Registry }),
		} {
			if _, err := v.Verify(context.Background(), req); err == nil {
				t.Errorf("%s: the registration verified", name)
			}
		}
		if reg.requests() != before {
			t.Fatalf("the registry was contacted %d times for refused registrations", reg.requests()-before)
		}
	})

	t.Run("the content is checked by digest", func(t *testing.T) {
		v := verifier(t, cfg, run)
		reg.mu.Lock()
		reg.body = append([]byte(nil), registryManifest...)
		reg.body[len(reg.body)-3] = ' ' // not the bytes the digest names
		reg.mu.Unlock()
		_, err := v.Verify(context.Background(), request("team/app", nil))
		reg.mu.Lock()
		reg.body = registryManifest
		reg.mu.Unlock()
		if !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("altered content: %v", err)
		}
		reg.mu.Lock()
		reg.status = http.StatusServiceUnavailable
		reg.mu.Unlock()
		_, err = v.Verify(context.Background(), request("team/app", nil))
		reg.mu.Lock()
		reg.status = http.StatusOK
		reg.mu.Unlock()
		if !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Fatalf("a registry outage: %v", err)
		}
	})

	t.Run("provenance comes from the producing attempt's pinned source", func(t *testing.T) {
		none := checkoutAttempt(t, runtimev1.AccessNone, "")
		tampered := spec
		tampered.ContextDigest = "sha256:" + strings.Repeat("9", 64)
		for name, tc := range map[string]struct {
			spec execution.AttemptSpec
			runs runReads
			want error
		}{
			"an attempt with no source":      {none, producerRun(t, none), artifacts.ErrProvenanceMissing},
			"a context that lost its digest": {tampered, producerRun(t, tampered), artifacts.ErrProvenanceMissing},
			"an unknown run":                 {spec, runReads{}, artifacts.ErrProvenanceMissing},
			"a run read outage":              {spec, runReads{err: errors.New("db down")}, artifacts.ErrRegistryUnavailable},
			"an attempt the run does not have": {spec, producerRun(t, checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo)),
				artifacts.ErrProvenanceMissing},
		} {
			v := verifier(t, cfg, tc.runs)
			req := request("team/app", func(r *artifacts.OCIRequest) {
				r.Scope, r.Attempt, r.Run = tc.spec.Scope, tc.spec.AttemptID, tc.spec.RunID
			})
			if _, err := v.Verify(context.Background(), req); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
	})

	t.Run("without a registry every registration is refused and the capability is reported", func(t *testing.T) {
		v, configured, err := NewRegistryVerifier(RegistryConfig{}, Test, run)
		if err != nil || configured {
			t.Fatalf("configured=%v, %v", configured, err)
		}
		before := reg.requests()
		if _, err := v.Verify(context.Background(), request("team/app", nil)); !errors.Is(err, artifacts.ErrRegistryDenied) {
			t.Fatalf("an unconfigured installation verified a registration: %v", err)
		}
		if reg.requests() != before {
			t.Fatal("an unconfigured installation contacted a registry")
		}
		names, reasons := m2JoinReport(nil, JoinState{})
		if !contains(names, capRegistry) || reasons[capRegistry] == "" {
			t.Fatalf("not reported: %v %v", names, reasons)
		}
		_, reasons = m2JoinReport(nil, JoinState{Registry: true})
		if reasons[capRegistry] != "" {
			t.Fatalf("a configured registry is reported disabled: %v", reasons)
		}
		// The run reader of the provenance is mandatory once a registry is named.
		if _, _, err := NewRegistryVerifier(cfg, Test, nil); err == nil {
			t.Fatal("a verifier was built without a provenance source")
		}
	})
}

// Test_app_registry_statement: the statement is strict and fails closed.
func Test_app_registry_statement(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry-auth"), []byte("Bearer t101-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := func(endpoint string) string { return `{"endpoints": [` + endpoint + `]}` }
	load := func(content string) (RegistryConfig, error) {
		p := filepath.Join(dir, "registry.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadRegistry(p, dir)
	}
	const good = `{"host": "registry.example.test", "authorizationFile": "registry-auth", "tenants": ["t_acme"], "repositories": ["team/app"]}`
	c, err := load(doc(good))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Endpoints[0].Authorization; got != "Bearer t101-token" {
		t.Fatalf("authorization %q", got)
	}
	if err := c.Validate(Production); err != nil {
		t.Fatalf("a valid https statement: %v", err)
	}
	for name, tc := range map[string]struct {
		doc string
		env Env
	}{
		"an unknown member":         {doc(`{"host": "registry.example.test", "tenants": ["*"], "repositories": ["a"], "extra": 1}`), Test},
		"no tenants":                {doc(`{"host": "registry.example.test", "repositories": ["a"]}`), Test},
		"no repositories":           {doc(`{"host": "registry.example.test", "tenants": ["*"]}`), Test},
		"a path prefix with a tag":  {doc(`{"host": "registry.example.test", "tenants": ["*"], "repositories": ["a:latest"]}`), Test},
		"an uppercase host":         {doc(`{"host": "Registry.Example.test", "tenants": ["*"], "repositories": ["a"]}`), Test},
		"a url of another host":     {doc(`{"host": "registry.example.test", "url": "https://evil.example.test", "tenants": ["*"], "repositories": ["a"]}`), Test},
		"a url with a path":         {doc(`{"host": "registry.example.test", "url": "https://registry.example.test/v2", "tenants": ["*"], "repositories": ["a"]}`), Test},
		"plain http in production":  {doc(`{"host": "registry.example.test", "url": "http://registry.example.test", "allowInsecureHttp": true, "tenants": ["*"], "repositories": ["a"]}`), Production},
		"plain http without opt-in": {doc(`{"host": "registry.example.test", "url": "http://registry.example.test", "tenants": ["*"], "repositories": ["a"]}`), Test},
		"plain http with a secret":  {doc(`{"host": "registry.example.test", "url": "http://registry.example.test", "allowInsecureHttp": true, "authorizationFile": "registry-auth", "tenants": ["*"], "repositories": ["a"]}`), Test},
	} {
		c, err := load(tc.doc)
		if err == nil {
			err = c.Validate(tc.env)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := load(doc(`{"host": "registry.example.test", "authorizationFile": "missing", "tenants": ["*"], "repositories": ["a"]}`)); err == nil {
		t.Error("a missing authorization file was accepted")
	}
}
