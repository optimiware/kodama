package registry_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// TestResolveHTTPFailureClasses pins the status-to-fault mapping. A failed read
// never yields a digest, and a response body is never echoed into the error.
func TestResolveHTTPFailureClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   fault.Class
	}{
		{"not found", http.StatusNotFound, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, fault.NotFound},
		{"unauthorized", http.StatusUnauthorized, `{"errors":[{"code":"UNAUTHORIZED"}]}`, fault.Unauthenticated},
		{"forbidden", http.StatusForbidden, `{"errors":[{"code":"DENIED"}]}`, fault.Forbidden},
		{"rate limited", http.StatusTooManyRequests, "", fault.DependencyUnavailable},
		{"server error", http.StatusInternalServerError, "backend internal detail", fault.DependencyUnavailable},
		{"bad gateway", http.StatusBadGateway, "", fault.DependencyUnavailable},
		{"service unavailable", http.StatusServiceUnavailable, "", fault.DependencyUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				return registryResponse(request, testCase.status, "application/json", testCase.body, ""), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})

			image, err := resolveWith(t, resolver, tagReference)
			requireFault(t, err, testCase.want)
			requireNoDigest(t, image)
			requireNoLeak(t, err, "backend internal detail", "MANIFEST_UNKNOWN")
		})
	}
}

// TestResolveTransportError pins that a transport failure is a retryable
// dependency fault that keeps the private cause reachable.
func TestResolveTransportError(t *testing.T) {
	cause := errors.New("dial registry.example.com: i/o timeout")
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return nil, cause
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, cause) {
		t.Fatalf("dependency fault lost its private cause: %v", err)
	}
	requireNoDigest(t, image)
}

// TestResolveCanceledBeforeRequest pins cancellation propagation without a
// registry call and the deadline_expired public class.
func TestResolveCanceledBeforeRequest(t *testing.T) {
	transport := &fakeTransport{}
	resolver := newResolver(t, registry.Config{Transport: transport})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	image, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
	requireFault(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation fault lost context.Canceled: %v", err)
	}
	requireNoDigest(t, image)
}

// TestResolveDeadlineExceededBeforeRequest pins deadline propagation and keeps
// context.DeadlineExceeded reachable through the fault.
func TestResolveDeadlineExceededBeforeRequest(t *testing.T) {
	transport := &fakeTransport{}
	resolver := newResolver(t, registry.Config{Transport: transport})

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	image, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
	requireFault(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline fault lost context.DeadlineExceeded: %v", err)
	}
	requireNoDigest(t, image)
}

// TestResolveCanceledInFlight pins that a resolve blocked on the registry
// returns promptly when its context is canceled and never produces a digest.
func TestResolveCanceledInFlight(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		image pipeline.ResolvedImage
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		image, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
		result <- outcome{image: image, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve never reached the registry transport")
	}
	cancel()

	select {
	case got := <-result:
		requireFault(t, got.err, fault.DeadlineExpired)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation fault lost context.Canceled: %v", got.err)
		}
		requireNoDigest(t, got.image)
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return after cancellation")
	}
}

// TestResolveFailureAfterSuccessIsNotStale pins the no-cache recovery rule: a
// later failure is an error and never the previously resolved digest.
func TestResolveFailureAfterSuccessIsNotStale(t *testing.T) {
	body := singleManifestBody
	transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
		if call == 0 {
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
		}
		return registryResponse(request, http.StatusServiceUnavailable, "", "", ""), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	first, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("first resolution failed: %v", err)
	}
	requireResolved(t, first, repository, digestOf(body))

	second, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.DependencyUnavailable)
	requireNoDigest(t, second)
	if second.Digest == first.Digest {
		t.Fatalf("failure returned the stale digest %q", first.Digest)
	}
	if requests := transport.captured(); len(requests) != 2 {
		t.Fatalf("resolver issued %d requests for two resolves, want 2", len(requests))
	}
}

// TestResolveChangedTagReResolves pins that a moved tag is looked up again and
// reports the new immutable digest.
func TestResolveChangedTagReResolves(t *testing.T) {
	bodies := []string{singleManifestBody, secondManifestBody}
	transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
		body := bodies[call%len(bodies)]
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	first, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("first resolution failed: %v", err)
	}
	requireResolved(t, first, repository, digestOf(bodies[0]))

	second, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("second resolution failed: %v", err)
	}
	requireResolved(t, second, repository, digestOf(bodies[1]))
	if first.Digest == second.Digest {
		t.Fatalf("changed tag kept the old digest %q", first.Digest)
	}
	if requests := transport.captured(); len(requests) != 2 {
		t.Fatalf("changed tag issued %d requests, want 2", len(requests))
	}
}
