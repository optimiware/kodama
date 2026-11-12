package keys

// White-box pins for the Cloud KMS client invariants that a loopback server
// cannot observe: the exact default origins, the response reader's hard bound
// (the body is cut off, not merely refused after buffering), a cancellation
// that lands inside either body read, statuses whose real responses carry no
// body, the checksum polynomial and the metadata request timeout.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	gcpInternalRef   = "gcp-kms:projects/kodama-prod/locations/global/keyRings/ring/cryptoKeys/kek"
	gcpInternalToken = `{"access_token":"gcp-internal-token","expires_in":3600,"token_type":"Bearer"}`
	// gcpInternalReply decodes to "123456789" with the published CRC-32C
	// check value.
	gcpInternalReply = `{"plaintext":"MTIzNDU2Nzg5","plaintextCrc32c":"3808858755"}`
)

// gcpSeedToken makes the client hold a cached token, so a test of the decrypt
// request needs no metadata server.
func gcpSeedToken(client *GCPKMS) {
	client.tokens.cached = "gcp-seeded-token"
	client.tokens.refresh = client.tokens.now().Add(time.Hour)
}

func gcpRequireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if class, ok := fault.ClassOf(err); !ok || class != want {
		t.Fatalf("error class = %q (fault %v, error %v); want %q", class, ok, err, want)
	}
}

// TestNewGCPKMSDefaultOriginsArePinned pins both literal default origins. A
// moved Cloud KMS default would send the bearer token to the wrong endpoint,
// and a moved metadata default would take the token from the wrong source; the
// constructor succeeding is not evidence that either is right.
func TestNewGCPKMSDefaultOriginsArePinned(t *testing.T) {
	client, err := NewGCPKMS(GCPKMSConfig{})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	if client.endpoint != "https://cloudkms.googleapis.com" {
		t.Fatalf("default Cloud KMS origin = %q; want %q", client.endpoint, "https://cloudkms.googleapis.com")
	}
	const tokenURL = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"
	if client.tokens.endpoint != tokenURL {
		t.Fatalf("default token URL = %q; want %q", client.tokens.endpoint, tokenURL)
	}
	if client.tokens.cached != "" {
		t.Fatal("a new client already holds a token")
	}
}

// TestGCPCRC32CCheckValue pins the Castagnoli polynomial with the published
// check value; the IEEE polynomial gives 0xCBF43926 for the same input.
func TestGCPCRC32CCheckValue(t *testing.T) {
	if got := gcpCRC32C([]byte("123456789")); got != 0xE3069283 {
		t.Fatalf("CRC-32C check value = %#x; want 0xe3069283", got)
	}
	if got := gcpCRC32C(nil); got != 0 {
		t.Fatalf("CRC-32C of no bytes = %#x; want 0", got)
	}
}

// TestGCPTokenExpiryMarginIsPinned pins the literal margin and lifetime cap.
func TestGCPTokenExpiryMarginIsPinned(t *testing.T) {
	if gcpTokenExpiryMargin != 225*time.Second {
		t.Fatalf("token expiry margin = %v; want the literal 3m45s", gcpTokenExpiryMargin)
	}
	if gcpMaxTokenLifetime != 3600*time.Second {
		t.Fatalf("token lifetime cap = %v; want the literal 1h", gcpMaxTokenLifetime)
	}
}

// TestGCPTokenCacheHoldsOnlyAUsableToken pins what the token source keeps in
// memory, which no server can observe: a token whose lifetime is inside the
// expiry margin serves its call and is not stored, and a token past its margin
// is forgotten even when the read that should replace it fails.
func TestGCPTokenCacheHoldsOnlyAUsableToken(t *testing.T) {
	read := func(t *testing.T, client *GCPKMS, status int, body string) error {
		t.Helper()
		client.tokens.client.Transport = gcpStubTransport{status: status, body: body}
		client.client.Transport = gcpStubTransport{status: http.StatusOK, body: gcpInternalReply}
		_, err := client.Decrypt(context.Background(), secret.KeyRef(gcpInternalRef), []byte("blob"))
		return err
	}
	lifetimes := []struct {
		expiresIn string
		cached    bool
	}{
		{expiresIn: "1", cached: false},
		{expiresIn: "224", cached: false},
		{expiresIn: "225", cached: false},
		{expiresIn: "226", cached: true},
		{expiresIn: "3600", cached: true},
	}
	for _, lifetime := range lifetimes {
		client, err := NewGCPKMS(GCPKMSConfig{})
		if err != nil {
			t.Fatalf("NewGCPKMS: %v", err)
		}
		body := `{"access_token":"gcp-internal-token","expires_in":` + lifetime.expiresIn + `,"token_type":"Bearer"}`
		if err := read(t, client, http.StatusOK, body); err != nil {
			t.Fatalf("Decrypt with a %ss token: %v", lifetime.expiresIn, err)
		}
		if got := client.tokens.cached != ""; got != lifetime.cached {
			t.Fatalf("a %ss token is cached = %v; want %v", lifetime.expiresIn, got, lifetime.cached)
		}
	}

	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusOK} {
		client, err := NewGCPKMS(GCPKMSConfig{})
		if err != nil {
			t.Fatalf("NewGCPKMS: %v", err)
		}
		client.tokens.cached = "gcp-expired-token"
		client.tokens.refresh = client.tokens.now().Add(-time.Second)
		// The 200 reply is unusable, so every read here fails.
		if err := read(t, client, status, `{"access_token":""}`); err == nil {
			t.Fatalf("failed token read with status %d returned no error", status)
		}
		if client.tokens.cached != "" {
			t.Fatalf("a failed token read with status %d left a token cached", status)
		}
	}
}

// gcpEndlessReader serves 'x' forever up to a hard ceiling. The ceiling exists
// only so a mutant that removes the response limit still terminates; a
// compliant reader stops after the bound and never reaches it.
type gcpEndlessReader struct {
	served  int64
	ceiling int64
}

var errGCPEndlessCeiling = errors.New("endless reader ceiling reached")

func (reader *gcpEndlessReader) Read(buffer []byte) (int, error) {
	if reader.served >= reader.ceiling {
		return 0, errGCPEndlessCeiling
	}
	if remaining := reader.ceiling - reader.served; int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	for index := range buffer {
		buffer[index] = 'x'
	}
	reader.served += int64(len(buffer))
	return len(buffer), nil
}

// TestGCPReadResponseStopsAtTheBound proves gcpReadResponse reads at most one
// byte over each bound from an endless body, and pins both bounds as literals.
// Without the io.LimitReader the whole body (up to the ceiling here) would be
// buffered before the size check refused it.
func TestGCPReadResponseStopsAtTheBound(t *testing.T) {
	if gcpKMSMaxResponse != 64<<10 || gcpMetadataMaxResponse != 16<<10 {
		t.Fatalf("response bounds = %d and %d; want the literals 64 KiB and 16 KiB", gcpKMSMaxResponse, gcpMetadataMaxResponse)
	}
	for _, bound := range []int64{gcpKMSMaxResponse, gcpMetadataMaxResponse} {
		reader := &gcpEndlessReader{ceiling: bound * 4}
		payload, err := gcpReadResponse(reader, bound, "gcp test")
		if err == nil {
			t.Fatalf("endless body was accepted and returned %d bytes", len(payload))
		}
		if payload != nil {
			t.Fatalf("refusal returned %d payload bytes", len(payload))
		}
		gcpRequireClass(t, err, fault.DependencyUnavailable)
		if reader.served > bound+1 {
			t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, bound+1)
		}

		exact := bytes.Repeat([]byte{'x'}, int(bound))
		payload, err = gcpReadResponse(bytes.NewReader(exact), bound, "gcp test")
		if err != nil {
			t.Fatalf("exactly the bound was refused: %v", err)
		}
		if !bytes.Equal(payload, exact) {
			t.Fatalf("payload has length %d; want the exact body", len(payload))
		}
	}
}

// gcpStubTransport returns one prepared response. Test-only: it lets a test
// hand a client a 204, 304 or 3xx response that still carries a body, which
// net/http withholds for a real 204/304.
type gcpStubTransport struct {
	status int
	body   string
}

func (transport gcpStubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	header := make(http.Header)
	header.Set("Metadata-Flavor", "Google")
	return &http.Response{
		StatusCode: transport.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Request:    request,
	}, nil
}

// TestGCPKMSRefusesNonOKStatusWithAStubBody proves the status line decides the
// outcome of both requests even when a well-formed body is delivered:
// 201/202/204/302/304/307 never yield plaintext or a token. The stub is needed
// for 204/304 because a real HTTP response for those statuses has no body,
// which would make a narrow "accept 204/304" mutant look equivalent to a
// refusal.
func TestGCPKMSRefusesNonOKStatusWithAStubBody(t *testing.T) {
	statuses := []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusFound,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
	}
	for _, status := range statuses {
		client, err := NewGCPKMS(GCPKMSConfig{})
		if err != nil {
			t.Fatalf("NewGCPKMS: %v", err)
		}
		gcpSeedToken(client)
		client.client.Transport = gcpStubTransport{status: status, body: gcpInternalReply}
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpInternalRef), []byte("blob"))
		if err == nil {
			t.Fatalf("decrypt status %d with a valid stub body returned %d plaintext bytes", status, len(plaintext))
		}
		if plaintext != nil {
			t.Fatalf("decrypt status %d returned %d plaintext bytes", status, len(plaintext))
		}
		gcpRequireClass(t, err, fault.DependencyUnavailable)
	}
	for _, status := range statuses {
		client, err := NewGCPKMS(GCPKMSConfig{})
		if err != nil {
			t.Fatalf("NewGCPKMS: %v", err)
		}
		client.tokens.client.Transport = gcpStubTransport{status: status, body: gcpInternalToken}
		// Cloud KMS would accept whatever token arrives.
		client.client.Transport = gcpStubTransport{status: http.StatusOK, body: gcpInternalReply}
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpInternalRef), []byte("blob"))
		if err == nil {
			t.Fatalf("metadata status %d with a valid stub body returned %d plaintext bytes", status, len(plaintext))
		}
		gcpRequireClass(t, err, fault.DependencyUnavailable)
		if client.tokens.cached != "" {
			t.Fatalf("metadata status %d cached a token", status)
		}
	}

	// The stubs themselves are sound: with 200 on both sides the call succeeds.
	client, err := NewGCPKMS(GCPKMSConfig{})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	client.tokens.client.Transport = gcpStubTransport{status: http.StatusOK, body: gcpInternalToken}
	client.client.Transport = gcpStubTransport{status: http.StatusOK, body: gcpInternalReply}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpInternalRef), []byte("blob"))
	if err != nil || string(plaintext) != "123456789" {
		t.Fatalf("stubbed Decrypt = %q, %v; want the stub plaintext", plaintext, err)
	}
	if client.tokens.cached != "gcp-internal-token" {
		t.Fatal("a usable token was not cached")
	}
}

// gcpBlockingBody blocks in Read until the request context is cancelled, then
// returns the context error. A loopback server cannot signal the client that
// the body read has started, so this stub makes a cancellation that lands
// squarely inside gcpReadResponse deterministic.
type gcpBlockingBody struct {
	ctx context.Context
}

func (body *gcpBlockingBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*gcpBlockingBody) Close() error { return nil }

// gcpBodyCancelTransport returns one accepted response whose body blocks
// inside Read until the request context is cancelled.
type gcpBodyCancelTransport struct {
	started chan<- struct{}
}

func (transport gcpBodyCancelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.started <- struct{}{}
	header := make(http.Header)
	header.Set("Metadata-Flavor", "Google")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       &gcpBlockingBody{ctx: request.Context()},
		Request:    request,
	}, nil
}

// TestGCPKMSContextCancellationDuringBodyRead proves a cancellation that
// arrives while either accepted response body is being read surfaces as the
// bare context error: errors.Is(err, context.Canceled) holds and the error is
// not a dependency_unavailable fault. Without the re-read of the context after
// gcpReadResponse, the wrapped read failure would be classified as an
// unavailable dependency.
func TestGCPKMSContextCancellationDuringBodyRead(t *testing.T) {
	cases := []struct {
		name    string
		install func(*GCPKMS, chan<- struct{})
	}{
		{name: "token reply", install: func(client *GCPKMS, started chan<- struct{}) {
			client.tokens.client.Transport = gcpBodyCancelTransport{started: started}
		}},
		{name: "decrypt reply", install: func(client *GCPKMS, started chan<- struct{}) {
			gcpSeedToken(client)
			client.client.Transport = gcpBodyCancelTransport{started: started}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, err := NewGCPKMS(GCPKMSConfig{})
			if err != nil {
				t.Fatalf("NewGCPKMS: %v", err)
			}
			started := make(chan struct{}, 1)
			testCase.install(client, started)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.Decrypt(ctx, secret.KeyRef(gcpInternalRef), []byte("blob"))
				result <- err
			}()

			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("client did not start the request")
			}
			cancel()

			select {
			case err := <-result:
				if err == nil {
					t.Fatal("cancelled body read returned no error")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled body read error = %v; want context.Canceled", err)
				}
				if class, ok := fault.ClassOf(err); ok {
					t.Fatalf("cancelled body read carried fault class %q; want the bare context error", class)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled body read did not return")
			}
		})
	}
}

// TestGCPTokenWaitHonoursCancellation proves a caller that was cancelled while
// it waited for another decrypt's token read gets its context error, not the
// token: the wait for the lock is not interruptible, so the context is
// re-read once the lock is held.
func TestGCPTokenWaitHonoursCancellation(t *testing.T) {
	client, err := NewGCPKMS(GCPKMSConfig{})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	gcpSeedToken(client)
	ctx, cancel := context.WithCancel(context.Background())

	client.tokens.mu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := client.tokens.token(ctx)
		result <- err
	}()
	cancel()
	client.tokens.mu.Unlock()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait for the token returned %v; want context.Canceled", err)
	} else if class, ok := fault.ClassOf(err); ok {
		t.Fatalf("cancelled wait carried fault class %q; want the bare context error", class)
	}
}

// TestGCPTokenDropKeepsANewerToken proves a rejected token is only forgotten
// while it is still the cached one: a decrypt that was rejected with an older
// token must not discard the token a concurrent decrypt has already read.
func TestGCPTokenDropKeepsANewerToken(t *testing.T) {
	client, err := NewGCPKMS(GCPKMSConfig{})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	gcpSeedToken(client)
	client.tokens.drop("an-older-token")
	if client.tokens.cached != "gcp-seeded-token" {
		t.Fatal("dropping an older token discarded the newer cached one")
	}
	client.tokens.drop("gcp-seeded-token")
	if client.tokens.cached != "" {
		t.Fatal("dropping the cached token kept it")
	}
}

// TestGCPMetadataTimeoutIsAnUnavailableDependency proves a metadata server
// that accepts the request and never answers is cut off by the client's own
// timeout and reported as an unavailable dependency: the caller's context is
// still live, so this is not a cancellation.
func TestGCPMetadataTimeoutIsAnUnavailableDependency(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewGCPKMS(GCPKMSConfig{MetadataEndpoint: server.URL})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	client.tokens.client.Timeout = 50 * time.Millisecond
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpInternalRef), []byte("blob"))
	if err == nil {
		t.Fatalf("silent metadata server returned %d plaintext bytes", len(plaintext))
	}
	gcpRequireClass(t, err, fault.DependencyUnavailable)
	if errors.Is(err, context.Canceled) {
		t.Fatalf("metadata timeout was reported as a cancellation: %v", err)
	}
	if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("metadata timeout disclosed the endpoint: %q", err.Error())
	}
}
