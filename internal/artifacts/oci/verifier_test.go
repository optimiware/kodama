package oci_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/support/layer"
)

var (
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	prov  = artifacts.Provenance{RepositoryURL: "https://git.example.test/acme/app.git", Commit: strings.Repeat("a", 40)}
)

// policy is a programmable Policy that records what it was asked.
type policy struct {
	mu       sync.Mutex
	endpoint func(ref artifacts.RegistryRef) (oci.Endpoint, error)
	asked    []string
}

func (p *policy) Authorize(_ context.Context, sc domain.Scope, attempt domain.AttemptID, ref artifacts.RegistryRef) (oci.Endpoint, error) {
	p.mu.Lock()
	p.asked = append(p.asked, fmt.Sprintf("%s|%s|%s", sc, attempt, ref))
	f := p.endpoint
	p.mu.Unlock()
	return f(ref)
}

type provenance struct {
	prov artifacts.Provenance
	err  error
	seen []oci.ProvenanceQuery
}

func (p *provenance) Resolve(_ context.Context, q oci.ProvenanceQuery) (artifacts.Provenance, error) {
	p.seen = append(p.seen, q)
	return p.prov, p.err
}

func verifier(t *testing.T, base string, insecure bool, auth string, pv *provenance, mutate func(*oci.Config)) (*oci.Verifier, *policy) {
	t.Helper()
	pol := &policy{endpoint: func(artifacts.RegistryRef) (oci.Endpoint, error) {
		return oci.Endpoint{BaseURL: base, Insecure: insecure, Authorization: auth}, nil
	}}
	cfg := oci.Config{Policy: pol, Provenance: pv, Timeout: 5 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := oci.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v, pol
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

const ociManifest = "application/vnd.oci.image.manifest.v1+json"

func manifestJSON(extra string) []byte {
	cfg := "sha256:" + strings.Repeat("c", 64)
	layer := "sha256:" + strings.Repeat("d", 64)
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":4}]%s}`,
		ociManifest, cfg, layer, extra))
}

func digestOf(b []byte) artifacts.Digest { return artifacts.DigestOf(b) }

// fake serves one manifest for a repository and counts requests.
type fake struct {
	srv      *httptest.Server
	hits     atomic.Int32
	mu       sync.Mutex
	headers  []http.Header
	handler  func(w http.ResponseWriter, r *http.Request)
	manifest []byte
}

func newFake(t *testing.T, manifest []byte) *fake {
	t.Helper()
	f := &fake{manifest: manifest}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		h := f.handler
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		serveManifest(w, f.manifest, ociManifest, string(digestOf(f.manifest)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func serveManifest(w http.ResponseWriter, body []byte, contentType, digestHeader string) {
	w.Header().Set("Content-Type", contentType)
	if digestHeader != "" {
		w.Header().Set("Docker-Content-Digest", digestHeader)
	}
	_, _ = w.Write(body)
}

func (f *fake) set(h func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func req(host string, d artifacts.Digest) artifacts.OCIRequest {
	return artifacts.OCIRequest{Scope: scope, Attempt: "att_1", Run: "run_1", Registry: host + "/team/app", Digest: d}
}

// TestOCIVerification: authorized registration, digest mismatch, foreign
// repository, missing provenance, registry outage, unsafe redirects and tag
// movement. The registry fixture is real; hostile responses use controlled
// HTTP servers.
func TestOCIVerification(t *testing.T) {
	ctx := context.Background()

	t.Run("a verified registration binds digest, repository and provenance", func(t *testing.T) {
		m := manifestJSON("")
		f := newFake(t, m)
		pv := &provenance{prov: prov}
		v, pol := verifier(t, f.srv.URL, true, "Bearer registry-token", pv, nil)
		host := hostOf(t, f.srv.URL)
		got, err := v.Verify(ctx, req(host+"", digestOf(m)))
		if err != nil {
			t.Fatal(err)
		}
		if got.Repository != host+"/team/app" || got.Digest != digestOf(m) || got.ManifestSize != int64(len(m)) || got.ManifestMediaType != ociManifest || got.Provenance != prov {
			t.Fatalf("verification: %+v", got)
		}
		if len(pol.asked) != 1 || pol.asked[0] != "t_acme/p_inventory|att_1|"+host+"/team/app" {
			t.Fatalf("the policy must be asked with the trusted scope and producer: %v", pol.asked)
		}
		if len(pv.seen) != 1 || pv.seen[0].Scope != scope || pv.seen[0].Attempt != "att_1" || pv.seen[0].Run != "run_1" || pv.seen[0].Digest != digestOf(m) {
			t.Fatalf("provenance query: %+v", pv.seen)
		}
		if f.headers[0].Get("Authorization") != "Bearer registry-token" {
			t.Fatal("the credential is sent to the authorized registry")
		}
		if !strings.Contains(f.headers[0].Get("Accept"), ociManifest) {
			t.Fatal("manifest types must be requested")
		}
	})

	t.Run("a tag in the caller's spelling is ignored and a moved tag cannot change what was verified", func(t *testing.T) {
		old, moved := manifestJSON(""), manifestJSON(`,"annotations":{"moved":"yes"}`)
		f := newFake(t, nil)
		var paths []string
		var mu sync.Mutex
		f.set(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			switch {
			case strings.HasSuffix(r.URL.Path, "/manifests/dev"):
				serveManifest(w, moved, ociManifest, string(digestOf(moved))) // the tag now points elsewhere
			case strings.HasSuffix(r.URL.Path, "/manifests/"+string(digestOf(old))):
				serveManifest(w, old, ociManifest, string(digestOf(old)))
			default:
				http.NotFound(w, r)
			}
		})
		v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
		r := req(hostOf(t, f.srv.URL), digestOf(old))
		r.Registry += ":dev"
		got, err := v.Verify(ctx, r)
		if err != nil || got.Digest != digestOf(old) || got.ManifestSize != int64(len(old)) {
			t.Fatalf("verification by digest: %+v %v", got, err)
		}
		for _, p := range paths {
			if strings.HasSuffix(p, "/dev") {
				t.Fatalf("a tag was resolved: %s", p)
			}
		}
		// Asking for the digest of the moved content is a separate, also valid, identity.
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(moved))); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("content absent by digest: %v", err)
		}
	})

	t.Run("bytes that do not hash to the digest never verify", func(t *testing.T) {
		good, other := manifestJSON(""), manifestJSON(`,"annotations":{"x":"y"}`)
		for name, serve := range map[string]func(w http.ResponseWriter){
			"other content":            func(w http.ResponseWriter) { serveManifest(w, other, ociManifest, string(digestOf(other))) },
			"lying digest header":      func(w http.ResponseWriter) { serveManifest(w, other, ociManifest, string(digestOf(good))) },
			"header contradicts bytes": func(w http.ResponseWriter) { serveManifest(w, good, ociManifest, string(digestOf(other))) },
			"missing header":           func(w http.ResponseWriter) { serveManifest(w, other, ociManifest, "") },
		} {
			f := newFake(t, nil)
			f.set(func(w http.ResponseWriter, r *http.Request) { serve(w) })
			v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(good))); !errors.Is(err, artifacts.ErrRegistryContent) || !errors.Is(err, artifacts.ErrOutputInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("invalid manifest representations are rejected", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body []byte
			ct   string
		}{
			"not json":        {[]byte("not json"), ociManifest},
			"wrong type":      {manifestJSON(""), "text/html"},
			"no type":         {manifestJSON(""), ""},
			"schema 1":        {bytes.Replace(manifestJSON(""), []byte(`"schemaVersion":2`), []byte(`"schemaVersion":1`), 1), ociManifest},
			"type mismatch":   {manifestJSON(""), "application/vnd.docker.distribution.manifest.v2+json"},
			"no config":       {[]byte(`{"schemaVersion":2,"layers":[]}`), ociManifest},
			"bad layer":       {[]byte(`{"schemaVersion":2,"config":{"digest":"sha256:` + strings.Repeat("c", 64) + `","size":1},"layers":[{"digest":"x","size":1}]}`), ociManifest},
			"empty index":     {[]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`), "application/vnd.oci.image.index.v1+json"},
			"trailing data":   {append(manifestJSON(""), []byte(`{}`)...), ociManifest},
			"size is missing": {[]byte(`{"schemaVersion":2,"config":{"digest":"sha256:` + strings.Repeat("c", 64) + `"},"layers":[]}`), ociManifest},
		} {
			f := newFake(t, nil)
			body, ct := tc.body, tc.ct
			f.set(func(w http.ResponseWriter, r *http.Request) {
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.Header().Set("Docker-Content-Digest", string(digestOf(body)))
				_, _ = w.Write(body)
			})
			v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(body))); !errors.Is(err, artifacts.ErrRegistryContent) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A valid index verifies.
		index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"` + ociManifest + `","digest":"sha256:` + strings.Repeat("e", 64) + `","size":10}]}`)
		f := newFake(t, nil)
		f.set(func(w http.ResponseWriter, r *http.Request) {
			serveManifest(w, index, "application/vnd.oci.image.index.v1+json", string(digestOf(index)))
		})
		v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(index))); err != nil {
			t.Errorf("a valid index: %v", err)
		}
	})

	t.Run("oversized manifests are refused", func(t *testing.T) {
		m := manifestJSON("")
		f := newFake(t, m)
		v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, func(c *oci.Config) { c.MaxManifestBytes = 50 })
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a foreign repository is denied before any network access", func(t *testing.T) {
		m := manifestJSON("")
		f := newFake(t, m)
		pv := &provenance{prov: prov}
		pol := &policy{endpoint: func(artifacts.RegistryRef) (oci.Endpoint, error) { return oci.Endpoint{}, artifacts.ErrRegistryDenied }}
		v, err := oci.NewVerifier(oci.Config{Policy: pol, Provenance: pv})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryDenied) || !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Fatalf("%v", err)
		}
		if f.hits.Load() != 0 || len(pv.seen) != 0 {
			t.Fatal("a denied registration reached the registry or the provenance resolver")
		}
		// Malformed references are denied without consulting anything.
		for _, bad := range []string{"https://x/y", "u:p@h/r", "h/../r", "", "h/R"} {
			r := req("h", digestOf(m))
			r.Registry = bad
			if _, err := v.Verify(ctx, r); !errors.Is(err, artifacts.ErrRegistryDenied) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if len(pol.asked) != 1 {
			t.Fatalf("malformed references reached the policy: %v", pol.asked)
		}
		// A policy outage is retryable, not a denial and not success.
		pol2 := &policy{endpoint: func(artifacts.RegistryRef) (oci.Endpoint, error) { return oci.Endpoint{}, errors.New("directory down") }}
		v2, _ := oci.NewVerifier(oci.Config{Policy: pol2, Provenance: pv})
		if _, err := v2.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Fatalf("policy outage: %v", err)
		}
	})

	t.Run("the authorized origin must be the registry the repository names", func(t *testing.T) {
		m := manifestJSON("")
		f := newFake(t, m)
		other := newFake(t, m)
		for name, base := range map[string]string{
			"another host":      other.srv.URL,
			"userinfo":          "http://user:pw@" + hostOf(t, f.srv.URL),
			"with a path":       f.srv.URL + "/v2",
			"with a query":      f.srv.URL + "?x=1",
			"not a url":         "::::",
			"empty":             "",
			"unsupported":       "ftp://" + hostOf(t, f.srv.URL),
			"plaintext in prod": f.srv.URL, // Insecure is false below
		} {
			pol := &policy{endpoint: func(artifacts.RegistryRef) (oci.Endpoint, error) { return oci.Endpoint{BaseURL: base}, nil }}
			if name != "plaintext in prod" {
				pol.endpoint = func(artifacts.RegistryRef) (oci.Endpoint, error) {
					return oci.Endpoint{BaseURL: base, Insecure: true}, nil
				}
			}
			v, _ := oci.NewVerifier(oci.Config{Policy: pol, Provenance: &provenance{prov: prov}})
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryDenied) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if f.hits.Load() != 0 || other.hits.Load() != 0 {
			t.Fatalf("a denied origin was contacted (%d, %d)", f.hits.Load(), other.hits.Load())
		}
	})

	t.Run("missing provenance is not verifiable", func(t *testing.T) {
		m := manifestJSON("")
		f := newFake(t, m)
		for name, pv := range map[string]*provenance{
			"missing":  {err: artifacts.ErrProvenanceMissing},
			"zero":     {},
			"invalid":  {prov: artifacts.Provenance{RepositoryURL: "https://u:p@git.example.test/x.git", Commit: "main"}},
			"annotate": {prov: artifacts.Provenance{RepositoryURL: "git@example.test:x.git", Commit: strings.Repeat("a", 40)}},
		} {
			v, _ := verifier(t, f.srv.URL, true, "", pv, nil)
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrProvenanceMissing) || !errors.Is(err, artifacts.ErrOutputInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A provenance outage is retryable and verifies nothing.
		v, _ := verifier(t, f.srv.URL, true, "", &provenance{err: errors.New("execution store down")}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Errorf("provenance outage: %v", err)
		}
	})

	t.Run("registry outages are retryable and never success", func(t *testing.T) {
		m := manifestJSON("")
		for name, status := range map[string]int{"unavailable": 503, "bad gateway": 502, "throttled": 429, "internal": 500, "timeout status": 408} {
			f := newFake(t, m)
			status := status
			f.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) || !errors.Is(err, artifacts.ErrUnavailable) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Connection refused.
		f := newFake(t, m)
		base, host := f.srv.URL, hostOf(t, f.srv.URL)
		f.srv.Close()
		v, _ := verifier(t, base, true, "", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(host, digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Errorf("refused: %v", err)
		}
		// Slow registry beyond the request timeout.
		slow := newFake(t, m)
		slow.set(func(w http.ResponseWriter, r *http.Request) { time.Sleep(600 * time.Millisecond) })
		v, _ = verifier(t, slow.srv.URL, true, "", &provenance{prov: prov}, func(c *oci.Config) { c.Timeout = 150 * time.Millisecond })
		if _, err := v.Verify(ctx, req(hostOf(t, slow.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Errorf("timeout: %v", err)
		}
		// Cancellation.
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		v, _ = verifier(t, slow.srv.URL, true, "", &provenance{prov: prov}, nil)
		if _, err := v.Verify(cctx, req(hostOf(t, slow.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryUnavailable) {
			t.Errorf("cancelled: %v", err)
		}
	})

	t.Run("registry refusals and missing content are not outages", func(t *testing.T) {
		m := manifestJSON("")
		for status, want := range map[int]error{401: artifacts.ErrRegistryDenied, 403: artifacts.ErrRegistryDenied, 404: artifacts.ErrRegistryContent, 400: artifacts.ErrRegistryContent} {
			f := newFake(t, m)
			status := status
			f.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			v, _ := verifier(t, f.srv.URL, true, "", &provenance{prov: prov}, nil)
			if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, want) {
				t.Errorf("%d: %v", status, err)
			}
		}
	})

	t.Run("unsafe redirects are rejected and credentials never leave the origin", func(t *testing.T) {
		m := manifestJSON("")
		elsewhere := newFake(t, m)
		f := newFake(t, m)
		f.set(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
		})
		v, _ := verifier(t, f.srv.URL, true, "Bearer secret-registry-token", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, f.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("cross-origin redirect: %v", err)
		}
		if elsewhere.hits.Load() != 0 {
			t.Fatal("a redirect to another origin was followed")
		}
		// Same-origin redirects are allowed within the bound, keep the credential and are re-verified by digest.
		var n atomic.Int32
		g := newFake(t, m)
		g.set(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/v2/") && n.Add(1) <= 2 {
				http.Redirect(w, r, "/v2/team/app/manifests/"+string(digestOf(m)), http.StatusTemporaryRedirect)
				return
			}
			serveManifest(w, m, ociManifest, string(digestOf(m)))
		})
		v, _ = verifier(t, g.srv.URL, true, "Bearer t", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, g.srv.URL), digestOf(m))); err != nil {
			t.Fatalf("same-origin redirects: %v", err)
		}
		// A redirect loop is bounded.
		loop := newFake(t, m)
		loop.set(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path, http.StatusFound) })
		v, _ = verifier(t, loop.srv.URL, true, "", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, loop.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("redirect loop: %v", err)
		}
		if loop.hits.Load() > 6 {
			t.Fatalf("redirect loop made %d requests", loop.hits.Load())
		}
		// Redirecting content still has to hash to the digest.
		bad := newFake(t, m)
		bad.set(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/swap" {
				serveManifest(w, manifestJSON(`,"annotations":{"a":"b"}`), ociManifest, "")
				return
			}
			http.Redirect(w, r, "/swap", http.StatusFound)
		})
		v, _ = verifier(t, bad.srv.URL, true, "", &provenance{prov: prov}, nil)
		if _, err := v.Verify(ctx, req(hostOf(t, bad.srv.URL), digestOf(m))); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("redirected content: %v", err)
		}
	})

	t.Run("construction requires the injected authorities", func(t *testing.T) {
		if _, err := oci.NewVerifier(oci.Config{Provenance: &provenance{}}); err == nil {
			t.Error("constructed without a policy")
		}
		if _, err := oci.NewVerifier(oci.Config{Policy: &policy{}}); err == nil {
			t.Error("constructed without a provenance resolver")
		}
	})
}

// TestOCIVerificationRealRegistry pushes a real image to the registry fixture
// and verifies it by digest. It skips unless KODAMA_REGISTRY_URL is set
// (KODAMA_REQUIRE_REGISTRY=1 makes absence a failure).
func TestOCIVerificationRealRegistry(t *testing.T) {
	layer.SkipService(t, "registry")
	base := os.Getenv("KODAMA_REGISTRY_URL")
	if base == "" {
		if os.Getenv("KODAMA_REQUIRE_REGISTRY") == "1" {
			t.Fatal("KODAMA_REGISTRY_URL is unset but KODAMA_REQUIRE_REGISTRY=1")
		}
		t.Skip("registry integration skipped: set KODAMA_REGISTRY_URL (or KODAMA_REQUIRE_REGISTRY=1 to require it)")
	}
	ctx := context.Background()
	prefix := os.Getenv("KODAMA_RESOURCE_PREFIX")
	if prefix == "" {
		prefix = "t015"
	}
	repo := prefix + "/artifacts-" + randHex(t)
	host := hostOf(t, base)
	insecure := strings.HasPrefix(base, "http://")

	m1, d1 := pushImage(t, base, repo, "dev", "one")
	pv := &provenance{prov: prov}
	v, _ := verifier(t, base, insecure, "", pv, nil)
	r := artifacts.OCIRequest{Scope: scope, Attempt: "att_1", Run: "run_1", Registry: host + "/" + repo + ":dev", Digest: d1}
	got, err := v.Verify(ctx, r)
	if err != nil {
		t.Fatalf("verifying a pushed image: %v", err)
	}
	if got.Digest != d1 || got.ManifestSize != int64(len(m1)) || got.Repository != host+"/"+repo || got.ManifestMediaType != ociManifest {
		t.Fatalf("verification: %+v", got)
	}

	// Tag movement: push different content under the same tag; the original digest still verifies and the new content is a different identity.
	m2, d2 := pushImage(t, base, repo, "dev", "two")
	if d1 == d2 {
		t.Fatal("test images must differ")
	}
	got, err = v.Verify(ctx, r)
	if err != nil || got.Digest != d1 || got.ManifestSize != int64(len(m1)) {
		t.Fatalf("after the tag moved: %+v %v", got, err)
	}
	r2 := r
	r2.Digest = d2
	if got, err := v.Verify(ctx, r2); err != nil || got.ManifestSize != int64(len(m2)) {
		t.Fatalf("the new digest: %+v %v", got, err)
	}

	// Unknown digest, unknown repository: content errors, not outages.
	r3 := r
	r3.Digest = artifacts.DigestOf([]byte("never pushed"))
	if _, err := v.Verify(ctx, r3); !errors.Is(err, artifacts.ErrRegistryContent) {
		t.Fatalf("unknown digest: %v", err)
	}
	r4 := r
	r4.Registry = host + "/" + prefix + "/does-not-exist"
	if _, err := v.Verify(ctx, r4); !errors.Is(err, artifacts.ErrRegistryContent) {
		t.Fatalf("unknown repository: %v", err)
	}
}

func randHex(t *testing.T) string {
	t.Helper()
	s := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(s[:6])
}

// pushImage pushes a minimal OCI image (empty config and one tiny layer whose
// bytes depend on seed) and returns the manifest bytes and digest.
func pushImage(t *testing.T, base, repo, tag, seed string) ([]byte, artifacts.Digest) {
	t.Helper()
	cfg := []byte("{}")
	layer := []byte("layer-" + seed)
	cd, ld := pushBlob(t, base, repo, cfg), pushBlob(t, base, repo, layer)
	m := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`,
		ociManifest, cd, len(cfg), ld, len(layer)))
	req, err := http.NewRequest(http.MethodPut, base+"/v2/"+repo+"/manifests/"+tag, bytes.NewReader(m))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ociManifest)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push manifest: %d %s", resp.StatusCode, body)
	}
	return m, digestOf(m)
}

func pushBlob(t *testing.T, base, repo string, data []byte) artifacts.Digest {
	t.Helper()
	d := digestOf(data)
	resp, err := http.Post(base+"/v2/"+repo+"/blobs/uploads/", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || loc == "" {
		t.Fatalf("start upload: %d", resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if !u.IsAbs() {
		bu, _ := url.Parse(base)
		u = bu.ResolveReference(u)
	}
	q := u.Query()
	q.Set("digest", string(d))
	u.RawQuery = q.Encode()
	req, _ := http.NewRequest(http.MethodPut, u.String(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("push blob: %d %s", resp.StatusCode, b)
	}
	return d
}
