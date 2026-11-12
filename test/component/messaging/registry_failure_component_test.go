//go:build component

package messaging_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

type fakeRegistryOptions struct {
	credential   registry.Credential
	maxBytes     int64
	maxRedirects int
}

// newFakeRegistry builds a resolver over one in-test HTTP origin. It is not a
// container: these cases pin the adapter's status, redirect and bound
// classification, while the real pinned registry covers the wire path.
func newFakeRegistry(t *testing.T, handler http.HandlerFunc, options fakeRegistryOptions) (*httptest.Server, *registry.Resolver, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	credential := options.credential
	if credential.Scheme == "" {
		credential = registry.Credential{Scheme: registry.CredentialBasic, Username: "user", Password: "password"}
	}
	resolver, err := registry.New(registry.Config{
		Transport:         http.DefaultTransport,
		Credentials:       support.StaticCredential{Value: credential},
		AllowInsecureHTTP: true,
		MaxResponseBytes:  options.maxBytes,
		MaxRedirects:      options.maxRedirects,
	})
	if err != nil {
		t.Fatalf("build fake registry resolver: %v", err)
	}
	return server, resolver, host + "/repo:tag"
}

func statusHandler(status int, counter *atomic.Int64) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if counter != nil {
			counter.Add(1)
		}
		writer.WriteHeader(status)
	}
}

func manifestHandler(body []byte, mediaType string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", mediaType)
		_, _ = writer.Write(body)
	}
}

// TestRegistryStatusClassification pins the adapter's fault classes and the
// no-retry, no-cache read.
func TestRegistryStatusClassification(t *testing.T) {
	support.TrackTest(t)
	cases := []struct {
		status int
		want   fault.Class
	}{
		{http.StatusUnauthorized, fault.Unauthenticated},
		{http.StatusForbidden, fault.Forbidden},
		{http.StatusNotFound, fault.NotFound},
		{http.StatusTooManyRequests, fault.DependencyUnavailable},
		{http.StatusInternalServerError, fault.DependencyUnavailable},
		{http.StatusBadGateway, fault.DependencyUnavailable},
		{http.StatusServiceUnavailable, fault.DependencyUnavailable},
		{http.StatusBadRequest, fault.InvalidInput},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var counter atomic.Int64
			_, resolver, reference := newFakeRegistry(t, statusHandler(tc.status, &counter), fakeRegistryOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: reference}); err == nil {
				t.Fatalf("status %d resolved; want %s", tc.status, tc.want)
			} else {
				requireClass(t, err, tc.want)
			}
			if got := counter.Load(); got != 1 {
				t.Fatalf("status %d made %d requests; want exactly 1 (the adapter does not retry)", tc.status, got)
			}
		})
	}
}

// TestRegistryDigestAndMediaType pins digest computation and response-shape
// refusal.
func TestRegistryDigestAndMediaType(t *testing.T) {
	support.TrackTest(t)
	manifest := []byte(`{"schemaVersion":2}`)
	digest := support.DigestOf(manifest)

	t.Run("valid", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler(manifest, support.OCIManifestMediaType), fakeRegistryOptions{})
		resolved, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.Digest != digest {
			t.Fatalf("digest = %q; want %q", resolved.Digest, digest)
		}
	})

	t.Run("header mismatch", func(t *testing.T) {
		handler := func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", support.OCIManifestMediaType)
			writer.Header().Set("Docker-Content-Digest", support.DigestOf([]byte("other")))
			_, _ = writer.Write(manifest)
		}
		_, resolver, reference := newFakeRegistry(t, handler, fakeRegistryOptions{})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("mismatched content digest header accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	})

	t.Run("pinned digest mismatch", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler(manifest, support.OCIManifestMediaType), fakeRegistryOptions{})
		host, _, _ := strings.Cut(reference, "/")
		pinned := host + "/repo@" + support.DigestOf([]byte("another manifest"))
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: pinned}); err == nil {
			t.Fatal("pinned digest mismatch accepted; want changed_idempotency_content")
		} else {
			requireClass(t, err, fault.ChangedIdempotencyContent)
		}
	})

	t.Run("disallowed media type", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler(manifest, "text/plain"), fakeRegistryOptions{})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("disallowed media type accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	})

	t.Run("missing media type", func(t *testing.T) {
		handler := func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(manifest) }
		_, resolver, reference := newFakeRegistry(t, handler, fakeRegistryOptions{})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("response without media type accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler([]byte("not-json"), support.OCIManifestMediaType), fakeRegistryOptions{})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("non-JSON manifest accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	})

	t.Run("response bound", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler(manifest, support.OCIManifestMediaType), fakeRegistryOptions{maxBytes: 4})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("oversized manifest accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	})
}

// TestRegistryRedirectCredentialSafety proves credentials stay on the
// configured origin across a redirect, the redirect cap is enforced, and
// cancellation is classified as a deadline.
func TestRegistryRedirectCredentialSafety(t *testing.T) {
	support.TrackTest(t)

	t.Run("cross-origin redirect does not forward credentials", func(t *testing.T) {
		var destinationAuth atomic.Value
		destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			destinationAuth.Store(request.Header.Get("Authorization"))
			writer.Header().Set("Content-Type", support.OCIManifestMediaType)
			_, _ = writer.Write([]byte(`{"schemaVersion":2}`))
		}))
		t.Cleanup(destination.Close)

		var sourceAuth atomic.Value
		source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			sourceAuth.Store(request.Header.Get("Authorization"))
			http.Redirect(writer, request, destination.URL+"/v2/repo/manifests/tag", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(source.Close)

		host := strings.TrimPrefix(source.URL, "http://")
		resolver, err := registry.New(registry.Config{
			Transport:         http.DefaultTransport,
			Credentials:       support.StaticCredential{Value: registry.Credential{Scheme: registry.CredentialBasic, Username: "user", Password: "password"}},
			AllowInsecureHTTP: true,
		})
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: host + "/repo:tag"}); err != nil {
			t.Fatalf("resolve through cross-origin redirect: %v", err)
		}
		if got, _ := sourceAuth.Load().(string); !strings.HasPrefix(got, "Basic ") {
			t.Fatalf("source origin did not receive the configured credential: %q", got)
		}
		if got, _ := destinationAuth.Load().(string); got != "" {
			t.Fatalf("credential leaked to the redirect destination: %q", got)
		}
	})

	t.Run("same-origin redirect keeps credentials", func(t *testing.T) {
		var finalAuth atomic.Value
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/manifests/tag") {
				http.Redirect(writer, request, "/internal/manifest", http.StatusTemporaryRedirect)
				return
			}
			finalAuth.Store(request.Header.Get("Authorization"))
			writer.Header().Set("Content-Type", support.OCIManifestMediaType)
			_, _ = writer.Write([]byte(`{"schemaVersion":2}`))
		}))
		t.Cleanup(server.Close)
		host := strings.TrimPrefix(server.URL, "http://")
		resolver, err := registry.New(registry.Config{
			Transport:         http.DefaultTransport,
			Credentials:       support.StaticCredential{Value: registry.Credential{Scheme: registry.CredentialBearer, Token: "token-value"}},
			AllowInsecureHTTP: true,
		})
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: host + "/repo:tag"}); err != nil {
			t.Fatalf("same-origin redirect resolve: %v", err)
		}
		if got, _ := finalAuth.Load().(string); got != "Bearer token-value" {
			t.Fatalf("same-origin redirect lost the credential: %q", got)
		}
	})

	t.Run("redirect cap", func(t *testing.T) {
		var counter atomic.Int64
		handler := func(writer http.ResponseWriter, request *http.Request) {
			counter.Add(1)
			http.Redirect(writer, request, "/loop", http.StatusTemporaryRedirect)
		}
		_, resolver, reference := newFakeRegistry(t, handler, fakeRegistryOptions{maxRedirects: 2})
		if _, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("redirect loop accepted; want invalid_input")
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
		if got := counter.Load(); got != 3 {
			t.Fatalf("redirect loop made %d requests; want 3 (initial plus two redirects)", got)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		_, resolver, reference := newFakeRegistry(t, manifestHandler([]byte(`{}`), support.OCIManifestMediaType), fakeRegistryOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("canceled resolve succeeded; want deadline_expired")
		} else {
			requireClass(t, err, fault.DeadlineExpired)
		}
	})

	t.Run("deadline while waiting for the registry", func(t *testing.T) {
		handler := func(writer http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}
		_, resolver, reference := newFakeRegistry(t, handler, fakeRegistryOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if _, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: reference}); err == nil {
			t.Fatal("timed-out resolve succeeded; want deadline_expired")
		} else {
			requireClass(t, err, fault.DeadlineExpired)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		resolver, err := registry.New(registry.Config{
			Transport:         http.DefaultTransport,
			AllowInsecureHTTP: true,
		})
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: "127.0.0.1:1/repo:tag"}); err == nil {
			t.Fatal("connection-refused resolve succeeded; want dependency_unavailable")
		} else {
			requireClass(t, err, fault.DependencyUnavailable)
		}
	})
}
