package registry_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// TestConcurrentResolves pins that one Resolver serves concurrent admission
// reads without sharing mutable state: every caller gets its own digest and no
// resolution is silently coalesced into a cache.
func TestConcurrentResolves(t *testing.T) {
	cases := []struct {
		reference  string
		repository string
		body       string
		path       string
	}{
		{
			reference:  registryOrigin + "/team/alpha:1",
			repository: registryOrigin + "/team/alpha",
			body:       singleManifestBody,
			path:       "/v2/team/alpha/manifests/1",
		},
		{
			reference:  registryOrigin + "/team/beta:1",
			repository: registryOrigin + "/team/beta",
			body:       secondManifestBody,
			path:       "/v2/team/beta/manifests/1",
		},
	}
	bodies := make(map[string]string, len(cases))
	for _, testCase := range cases {
		bodies[testCase.path] = testCase.body
	}

	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		body, ok := bodies[request.URL.Path]
		if !ok {
			return registryResponse(request, http.StatusNotFound, "", "", ""), nil
		}
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	const workers = 24
	failures := make(chan error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			testCase := cases[index%len(cases)]
			image, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: testCase.reference})
			if err != nil {
				failures <- fmt.Errorf("%s: %w", testCase.reference, err)
				return
			}
			if image.Reference != testCase.repository {
				failures <- fmt.Errorf("%s: reference = %q", testCase.reference, image.Reference)
				return
			}
			if image.Digest != digestOf(testCase.body) {
				failures <- fmt.Errorf("%s: digest = %q", testCase.reference, image.Digest)
				return
			}
			if err := image.Validate(); err != nil {
				failures <- fmt.Errorf("%s: %w", testCase.reference, err)
			}
		}(index)
	}
	wait.Wait()
	close(failures)

	for failure := range failures {
		t.Error(failure)
	}
	if requests := transport.captured(); len(requests) != workers {
		t.Fatalf("concurrent resolves issued %d requests, want %d (no coalescing)", len(requests), workers)
	}
}
