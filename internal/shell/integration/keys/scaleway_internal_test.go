package keys

// White-box pins for the Scaleway Key Manager client invariants that a
// loopback server cannot observe: the exact default origin, the response
// reader's hard bound (the body is cut off, not merely refused after
// buffering) and the key-reference grammar independently of URL escaping.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// TestNewScalewayKMSDefaultOriginIsPinned pins the literal default origin. A
// moved default (another host or a typo such as api.scaleway.org) would send
// the credential to the wrong endpoint; the constructor succeeding is not
// evidence that the origin is right.
func TestNewScalewayKMSDefaultOriginIsPinned(t *testing.T) {
	client, err := NewScalewayKMS(ScalewayKMSConfig{SecretKey: "k"})
	if err != nil {
		t.Fatalf("NewScalewayKMS: %v", err)
	}
	if client.baseURL != "https://api.scaleway.com" {
		t.Fatalf("default base URL = %q; want %q", client.baseURL, "https://api.scaleway.com")
	}
}

// scalewayEndlessReader serves 'x' forever up to a hard ceiling. The ceiling
// exists only so a mutant that removes the response limit still terminates;
// a compliant reader stops after the bound and never reaches it.
type scalewayEndlessReader struct {
	served  int64
	ceiling int64
}

var errScalewayEndlessCeiling = errors.New("endless reader ceiling reached")

func (reader *scalewayEndlessReader) Read(buffer []byte) (int, error) {
	if reader.served >= reader.ceiling {
		return 0, errScalewayEndlessCeiling
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

// TestReadScalewayResponseStopsAtTheBound proves readScalewayResponse reads at
// most one byte over the 64 KiB bound from an endless body. Without the
// io.LimitReader the whole body (up to the ceiling here) would be buffered
// before the size check refused it.
func TestReadScalewayResponseStopsAtTheBound(t *testing.T) {
	reader := &scalewayEndlessReader{ceiling: scalewayMaxResponse * 4}
	payload, err := readScalewayResponse(reader)
	if err == nil {
		t.Fatalf("endless body was accepted and returned %d bytes", len(payload))
	}
	if payload != nil {
		t.Fatalf("refusal returned %d payload bytes", len(payload))
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.DependencyUnavailable)
	}
	if reader.served > scalewayMaxResponse+1 {
		t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, scalewayMaxResponse+1)
	}
}

// TestReadScalewayResponseExactBound pins the boundary itself: exactly the
// bound is accepted and one byte over is refused.
func TestReadScalewayResponseExactBound(t *testing.T) {
	exact := bytes.Repeat([]byte{'x'}, int(scalewayMaxResponse))
	payload, err := readScalewayResponse(bytes.NewReader(exact))
	if err != nil {
		t.Fatalf("exactly the bound was refused: %v", err)
	}
	if !bytes.Equal(payload, exact) {
		t.Fatalf("payload has length %d; want the exact body", len(payload))
	}

	over := bytes.Repeat([]byte{'x'}, int(scalewayMaxResponse)+1)
	payload, err = readScalewayResponse(bytes.NewReader(over))
	if err == nil {
		t.Fatalf("one byte over the bound was accepted with %d bytes", len(payload))
	}
	if payload != nil {
		t.Fatalf("refusal returned %d payload bytes", len(payload))
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.DependencyUnavailable)
	}
}

// TestParseScalewayKeyRefRefusesPercentEncodedBytes pins the grammar on its own:
// the escaped traversal, newline and query spellings are refused before any URL
// escaping runs, so a mutant that disables escaping still cannot smuggle them.
func TestParseScalewayKeyRefRefusesPercentEncodedBytes(t *testing.T) {
	for _, reference := range []string{
		"scw-kms:fr-par/%2e%2e",
		"scw-kms:%2e%2e/kid",
		"scw-kms:fr-par/%0a",
		"scw-kms:fr%0apar/kid",
		"scw-kms:fr-par/%3f",
	} {
		if region, keyID, err := parseScalewayKeyRef(secret.KeyRef(reference)); err == nil {
			t.Fatalf("reference %q parsed as region %q key %q; want a refusal", reference, region, keyID)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("reference %q fault class = %q (fault %v); want %q", reference, class, ok, fault.InvalidInput)
		}
	}
}

// scalewayStubTransport returns one prepared response. Test-only: it lets a test
// hand the client a 204, 304 or 3xx response that still carries a body, which
// net/http withholds for a real 204/304.
type scalewayStubTransport struct {
	status int
	body   string
}

func (transport scalewayStubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: transport.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Request:    request,
	}, nil
}

// scalewayBlockingBody blocks in Read until the request context is cancelled,
// then returns the context error. A loopback server cannot signal the client
// that the body read has started, so this stub makes a cancellation that lands
// squarely inside readScalewayResponse deterministic.
type scalewayBlockingBody struct {
	ctx context.Context
}

func (body *scalewayBlockingBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*scalewayBlockingBody) Close() error { return nil }

// scalewayBodyCancelTransport returns one accepted response whose body blocks
// inside Read until the request context is cancelled.
type scalewayBodyCancelTransport struct {
	started chan<- context.Context
}

func (transport scalewayBodyCancelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.started <- request.Context()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       &scalewayBlockingBody{ctx: request.Context()},
		Request:    request,
	}, nil
}

// TestScalewayKMSContextCancellationDuringBodyRead proves a cancellation that
// arrives while the accepted response body is being read surfaces as the bare
// context error: errors.Is(err, context.Canceled) holds and the error is not a
// dependency_unavailable fault. Without the re-read of the context after
// readScalewayResponse, the wrapped read failure would be classified as an
// unavailable dependency.
func TestScalewayKMSContextCancellationDuringBodyRead(t *testing.T) {
	client, err := NewScalewayKMS(ScalewayKMSConfig{SecretKey: "k"})
	if err != nil {
		t.Fatalf("NewScalewayKMS: %v", err)
	}
	started := make(chan context.Context, 1)
	client.client.Transport = scalewayBodyCancelTransport{started: started}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Decrypt(ctx, secret.KeyRef("scw-kms:fr-par/kid"), []byte("blob"))
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
}

// TestScalewayKMSRefusesNonOKStatusWithAStubBody proves the status line decides
// the outcome even when a well-formed body is delivered: 201/202/204/302/304/307
// never yield plaintext. The stub is needed for 204/304 because a real HTTP
// response for those statuses has no body, which would make a narrow
// "accept 204/304" mutant look equivalent to a refusal.
func TestScalewayKMSRefusesNonOKStatusWithAStubBody(t *testing.T) {
	reply, err := json.Marshal(struct {
		Plaintext string `json:"plaintext"`
	}{base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, secret.DataKeyBytes))})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	for _, status := range []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusFound,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
	} {
		client, err := NewScalewayKMS(ScalewayKMSConfig{SecretKey: "k"})
		if err != nil {
			t.Fatalf("NewScalewayKMS: %v", err)
		}
		client.client.Transport = scalewayStubTransport{status: status, body: string(reply)}
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef("scw-kms:fr-par/kid"), []byte("blob"))
		if err == nil {
			t.Fatalf("status %d with a valid stub body returned %d plaintext bytes", status, len(plaintext))
		}
		if plaintext != nil {
			t.Fatalf("status %d returned %d plaintext bytes", status, len(plaintext))
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("status %d fault class = %q (fault %v); want %q", status, class, ok, fault.DependencyUnavailable)
		}
	}
}
