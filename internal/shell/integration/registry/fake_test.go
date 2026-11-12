package registry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

const (
	registryOrigin   = "registry.example.com"
	registryPortHost = "registry.example.com:5000"
	repositoryPath   = "team/image"
	repository       = registryOrigin + "/" + repositoryPath
	imageTag         = "1.2"
	tagReference     = repository + ":" + imageTag

	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexMediaType    = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	ociIndexMediaType       = "application/vnd.oci.image.index.v1+json"

	childDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// Manifest and index fixtures are exact bytes: the resolver must hash the bytes
// it received, not a re-encoded or normalized document.
const (
	singleManifestBody = `{"schemaVersion":2,"mediaType":"` + dockerManifestMediaType + `","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":1234,"digest":"` + childDigest + `"},"layers":[]}`

	secondManifestBody = `{"schemaVersion":2,"mediaType":"` + dockerManifestMediaType + `","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":4321,"digest":"` + otherDigest + `"},"layers":[]}`

	ociManifestBody = `{"schemaVersion":2,"mediaType":"` + ociManifestMediaType + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":1234,"digest":"` + childDigest + `"},"layers":[]}`

	ociIndexBody = `{"schemaVersion":2,"mediaType":"` + ociIndexMediaType + `","manifests":[{"mediaType":"` + ociManifestMediaType + `","size":7023,"digest":"` + childDigest + `","platform":{"architecture":"amd64","os":"linux"}},{"mediaType":"` + ociManifestMediaType + `","size":7024,"digest":"` + otherDigest + `","platform":{"architecture":"arm64","os":"linux"}}]}`

	dockerIndexBody = `{"schemaVersion":2,"mediaType":"` + dockerIndexMediaType + `","manifests":[{"mediaType":"` + dockerManifestMediaType + `","size":7023,"digest":"` + childDigest + `","platform":{"architecture":"amd64","os":"linux"}}]}`
)

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// padJSON returns valid JSON padded with trailing spaces to exactly size bytes.
func padJSON(body string, size int) string {
	if len(body) >= size {
		return body
	}
	return body + strings.Repeat(" ", size-len(body))
}

func manifestURL(scheme, host, path, reference string) string {
	return scheme + "://" + host + "/v2/" + path + "/manifests/" + reference
}

// capturedRequest is one recorded registry request. Headers are cloned so a
// later mutation by the client cannot rewrite the evidence.
type capturedRequest struct {
	Method string
	URL    string
	Header http.Header
}

// fakeRequestCap bounds one resolution's request count. A resolver that retries
// without a natural stop (for example a mutation that retries every challenge)
// would otherwise loop until the go test timeout; the cap turns that into a
// fast, clear failure.
const fakeRequestCap = 50

// fakeTransport is a deterministic http.RoundTripper. It records every request
// before answering and never touches a socket. After fakeRequestCap requests it
// fails every further request with an explicit error instead of hanging.
type fakeTransport struct {
	mu       sync.Mutex
	requests []capturedRequest
	handle   func(call int, request *http.Request) (*http.Response, error)
}

func (transport *fakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	if len(transport.requests) >= fakeRequestCap {
		transport.mu.Unlock()
		return nil, fmt.Errorf("fake transport request cap of %d exceeded: %s %s", fakeRequestCap, request.Method, request.URL)
	}
	call := len(transport.requests)
	transport.requests = append(transport.requests, capturedRequest{
		Method: request.Method,
		URL:    request.URL.String(),
		Header: request.Header.Clone(),
	})
	handle := transport.handle
	transport.mu.Unlock()

	if handle == nil {
		return nil, fmt.Errorf("unexpected registry request %s %s", request.Method, request.URL)
	}
	return handle(call, request)
}

func (transport *fakeTransport) captured() []capturedRequest {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]capturedRequest(nil), transport.requests...)
}

func registryResponse(request *http.Request, status int, contentType, body, contentDigest string) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if contentDigest != "" {
		header.Set("Docker-Content-Digest", contentDigest)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func redirectResponse(request *http.Request, location string) *http.Response {
	header := make(http.Header)
	header.Set("Location", location)
	return &http.Response{
		StatusCode: http.StatusTemporaryRedirect,
		Status:     fmt.Sprintf("%d %s", http.StatusTemporaryRedirect, http.StatusText(http.StatusTemporaryRedirect)),
		Header:     header,
		Body:       http.NoBody,
		Request:    request,
	}
}

// responseWithBody answers with an arbitrary body stream, so a test can feed a
// reader that never ends.
func responseWithBody(request *http.Request, status int, contentType string, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       body,
		Request:    request,
	}
}

var errEndlessBodyLimit = fmt.Errorf("test endless body hard limit reached")

// endlessBody streams 'a' bytes forever in principle and counts how many bytes
// it handed to its reader. limit is a test-process safety stop only: it sits far
// above every byte assertion, so a resolver that drains the body without a bound
// still reads more than the assertion allows and fails, without consuming
// unbounded memory or running until the go test timeout.
type endlessBody struct {
	read  int64
	limit int64
}

func newEndlessBody(limit int64) *endlessBody {
	return &endlessBody{limit: limit}
}

func (body *endlessBody) Read(target []byte) (int, error) {
	if body.read >= body.limit {
		return 0, errEndlessBodyLimit
	}
	count := int64(len(target))
	if remaining := body.limit - body.read; count > remaining {
		count = remaining
	}
	for index := int64(0); index < count; index++ {
		target[index] = 'a'
	}
	body.read += count
	return int(count), nil
}

func (body *endlessBody) Close() error { return nil }

// staticCredentialSource is an installation credential bound to one origin.
// Requests for any other origin get an anonymous credential, and every origin
// it was asked about is recorded.
type staticCredentialSource struct {
	origin     string
	credential registry.Credential
	err        error

	mu      sync.Mutex
	origins []string
}

func (source *staticCredentialSource) Credential(_ context.Context, origin string) (registry.Credential, error) {
	source.mu.Lock()
	source.origins = append(source.origins, origin)
	source.mu.Unlock()

	if source.err != nil {
		return registry.Credential{}, source.err
	}
	if source.origin != "" && origin != source.origin {
		return registry.Credential{Scheme: registry.CredentialAnonymous}, nil
	}
	return source.credential, nil
}

func (source *staticCredentialSource) seenOrigins() []string {
	source.mu.Lock()
	defer source.mu.Unlock()
	return append([]string(nil), source.origins...)
}

func newResolver(t *testing.T, config registry.Config) *registry.Resolver {
	t.Helper()
	resolver, err := registry.New(config)
	if err != nil {
		t.Fatalf("registry.New refused the fixture config: %v", err)
	}
	if resolver == nil {
		t.Fatal("registry.New returned a nil resolver")
	}
	return resolver
}

func resolveWith(t *testing.T, resolver *registry.Resolver, reference string) (pipeline.ResolvedImage, error) {
	t.Helper()
	return resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference})
}

func requireResolved(t *testing.T, image pipeline.ResolvedImage, wantReference, wantDigest string) {
	t.Helper()
	if image.Reference != wantReference {
		t.Fatalf("resolved reference = %q, want %q", image.Reference, wantReference)
	}
	if image.Digest != wantDigest {
		t.Fatalf("resolved digest = %q, want %q", image.Digest, wantDigest)
	}
	if err := image.Validate(); err != nil {
		t.Fatalf("resolved image violates the declared contract: %v", err)
	}
}

func requireNoDigest(t *testing.T, image pipeline.ResolvedImage) {
	t.Helper()
	if image.Digest != "" {
		t.Fatalf("refused resolution returned digest %q; a refusal never carries a digest", image.Digest)
	}
}

func requireFault(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault, got nil", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("expected %s fault, got untyped error: %v", want, err)
	}
	if class != want {
		t.Fatalf("expected %s fault, got %s: %v", want, class, err)
	}
}

func requireNoLeak(t *testing.T, err error, secrets ...string) {
	t.Helper()
	message := err.Error()
	if public, ok := fault.MessageOf(err); ok {
		message += "\n" + public
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}

func requireOrigins(t *testing.T, source *staticCredentialSource, want string) {
	t.Helper()
	origins := source.seenOrigins()
	if len(origins) == 0 {
		t.Fatal("credential source was never consulted")
	}
	for _, origin := range origins {
		if origin != want {
			t.Fatalf("credential source consulted for origin %q, want %q", origin, want)
		}
	}
}

func requireNoOrigin(t *testing.T, source *staticCredentialSource, forbidden string) {
	t.Helper()
	for _, origin := range source.seenOrigins() {
		if strings.Contains(origin, forbidden) {
			t.Fatalf("credential source consulted for forbidden origin %q", origin)
		}
	}
}
