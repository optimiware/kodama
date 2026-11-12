package keys_test

// The Cloud KMS decrypt client is plain net/http: one bounded GET to the GKE
// metadata server for the workload's access token, cached until shortly before
// it expires, and one bounded, authenticated JSON POST to the cryptoKeys
// decrypt method. These tests pin both request shapes, the token cache and its
// single retry, the status-to-fault tables, the checksum, the response bounds,
// the redirect boundary, cancellation, error hygiene, the constructor refusals
// and the interaction with the shared cloud provider.
//
// Cloud KMS is a loopback httptest TLS server and the metadata server a plain
// loopback httptest server; the clock is injected. No test contacts a real
// cloud endpoint or a real metadata server.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	// gcpTestRef is the accepted grammar: the full cryptoKey resource name.
	gcpTestRef = "gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/kodama_KEK"
	// gcpTestPath is the only route the client may call: the resource name
	// with the literal ":decrypt" custom-method suffix.
	gcpTestPath = "/v1/projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/kodama_KEK:decrypt"
	// gcpTestTokenPath is the only metadata entry the client may read.
	gcpTestTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
	// gcpTestToken prefixes every token the stand-in metadata server issues.
	gcpTestToken = "ya29.gcp-test-access-token"
	// gcpCheckInput and gcpCheckCRC32C are the standard CRC-32C check vector
	// (0xE3069283), independent of the implementation under test.
	gcpCheckInput  = "123456789"
	gcpCheckCRC32C = "3808858755"
	// gcpHourToken is the lifetime of a token that is safely cacheable.
	gcpHourToken int64 = 3600
)

// gcpTestKEK is the 32-byte key-encryption key the reply unwraps to.
func gcpTestKEK() []byte {
	return bytes.Repeat([]byte{0x6b}, secret.DataKeyBytes)
}

// gcpChecksum is the decimal CRC-32C a Cloud KMS reply carries for data.
func gcpChecksum(data []byte) string {
	return strconv.FormatUint(uint64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))), 10)
}

// gcpReply encodes one decrypt reply the way Cloud KMS does, including the
// plaintext checksum as an int64 string. It is called before a server starts
// so a handler goroutine never touches testing.T.
func gcpReply(t *testing.T, plaintext []byte) string {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Plaintext       string `json:"plaintext"`
		PlaintextCRC32C string `json:"plaintextCrc32c"`
		UsedPrimary     bool   `json:"usedPrimary"`
		ProtectionLevel string `json:"protectionLevel"`
	}{base64.StdEncoding.EncodeToString(plaintext), gcpChecksum(plaintext), true, "SOFTWARE"})
	if err != nil {
		t.Fatalf("marshal decrypt reply: %v", err)
	}
	return string(encoded)
}

// gcpTokenReply is one metadata token reply.
func gcpTokenReply(token string, expiresIn int64) string {
	return fmt.Sprintf(`{"access_token":"%s","expires_in":%d,"token_type":"Bearer"}`, token, expiresIn)
}

// gcpIssuedToken is the token the stand-in metadata server issues on its nth
// request, so a test can tell a cached token from a newly read one.
func gcpIssuedToken(n int) string {
	return fmt.Sprintf("%s-%d", gcpTestToken, n)
}

// gcpSeen is one request a stand-in server received.
type gcpSeen struct {
	method     string
	path       string
	requestURI string
	rawQuery   string
	header     http.Header
	body       string
}

// gcpRecorder records the requests one stand-in server received.
type gcpRecorder struct {
	mu   sync.Mutex
	seen []gcpSeen
}

// record stores request and returns how many requests have been recorded.
func (recorder *gcpRecorder) record(request *http.Request) int {
	raw, _ := io.ReadAll(request.Body)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.seen = append(recorder.seen, gcpSeen{
		method:     request.Method,
		path:       request.URL.Path,
		requestURI: request.RequestURI,
		rawQuery:   request.URL.RawQuery,
		header:     request.Header.Clone(),
		body:       string(raw),
	})
	return len(recorder.seen)
}

func (recorder *gcpRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.seen)
}

func (recorder *gcpRecorder) requests() []gcpSeen {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]gcpSeen(nil), recorder.seen...)
}

// gcpTokens is a stand-in metadata server: it records each request and issues
// a numbered token with the given lifetime, carrying the Metadata-Flavor
// response header the client requires.
func gcpTokens(recorder *gcpRecorder, expiresIn int64) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		n := recorder.record(request)
		writer.Header().Set("Metadata-Flavor", "Google")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, gcpTokenReply(gcpIssuedToken(n), expiresIn))
	})
}

// gcpTokenBody is a stand-in metadata server that answers every request with
// one fixed status and body, with the Metadata-Flavor response header.
func gcpTokenBody(recorder *gcpRecorder, status int, body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Metadata-Flavor", "Google")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	})
}

// gcpDecrypts is a stand-in Cloud KMS: it records each request and answers
// with one fixed status and body.
func gcpDecrypts(recorder *gcpRecorder, status int, body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	})
}

// gcpRejecting is a stand-in Cloud KMS that rejects the token of its first
// limit requests with 401 and then accepts. The limit exists only so a client
// that retried without bound would end in a wrong success instead of hanging
// the test; a compliant client never reaches it.
func gcpRejecting(recorder *gcpRecorder, limit int, reply string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if recorder.record(request) <= limit {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":{"message":"server-body-sentinel"}}`)
			return
		}
		_, _ = io.WriteString(writer, reply)
	})
}

// gcpClock is the injected clock that decides token expiry.
type gcpClock struct {
	mu  sync.Mutex
	now time.Time
}

func newGCPClock() *gcpClock {
	return &gcpClock{now: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)}
}

func (clock *gcpClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *gcpClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// gcpFixture is one client wired to its two loopback stand-ins.
type gcpFixture struct {
	client   *keys.GCPKMS
	kms      *httptest.Server
	metadata *httptest.Server
}

// gcpStart starts a TLS stand-in for Cloud KMS and a plain-HTTP loopback
// stand-in for the metadata server, and returns a client whose injected
// transport trusts the TLS server's certificate. NewGCPKMS must own the
// transport (clear any proxy, refuse redirects) without losing the test's
// explicit TLS roots.
func gcpStart(t *testing.T, metadata, kms http.Handler, now func() time.Time) gcpFixture {
	t.Helper()
	kmsServer := httptest.NewTLSServer(kms)
	t.Cleanup(kmsServer.Close)
	metadataServer := httptest.NewServer(metadata)
	t.Cleanup(metadataServer.Close)
	client, err := keys.NewGCPKMS(keys.GCPKMSConfig{
		KMSEndpoint:      kmsServer.URL,
		MetadataEndpoint: metadataServer.URL,
		HTTPClient:       kmsServer.Client(),
		Now:              now,
	})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	return gcpFixture{client: client, kms: kmsServer, metadata: metadataServer}
}

func gcpClient(t *testing.T, metadata, kms http.Handler) *keys.GCPKMS {
	t.Helper()
	return gcpStart(t, metadata, kms, nil).client
}

// gcpRoundTripFunc is an opaque RoundTripper used to prove the constructor
// refuses a transport it cannot make proxy-free and redirect-free.
type gcpRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip gcpRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// gcpRequireContextError proves err is the bare context error want: it
// satisfies errors.Is and carries no fault class.
func gcpRequireContextError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v; want %v", err, want)
	}
	if class, ok := fault.ClassOf(err); ok {
		t.Fatalf("error carried fault class %q; want the bare context error", class)
	}
}

// TestGCPKMSDecryptRequestsAndResult pins both exact requests: the metadata
// GET with its Metadata-Flavor header and nothing else of note, and the
// decrypt POST to the one route with the literal ":decrypt" suffix, the bearer
// token, the JSON ciphertext with its CRC-32C as a decimal string, and the
// decoded reply.
func TestGCPKMSDecryptRequestsAndResult(t *testing.T) {
	var metadata, kms gcpRecorder
	plaintext := gcpTestKEK()
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, plaintext)))

	// The blob is the CRC-32C check vector, so the expected checksum below is
	// a published constant rather than a recomputation.
	blob := []byte(gcpCheckInput)
	got, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext = %x; want %x", got, plaintext)
	}

	tokenRequests := metadata.requests()
	if len(tokenRequests) != 1 {
		t.Fatalf("metadata request count = %d; want exactly 1", len(tokenRequests))
	}
	tokenRequest := tokenRequests[0]
	if tokenRequest.method != http.MethodGet {
		t.Fatalf("metadata method = %q; want GET", tokenRequest.method)
	}
	if tokenRequest.path != gcpTestTokenPath || tokenRequest.requestURI != gcpTestTokenPath {
		t.Fatalf("metadata request = %q (URI %q); want exactly %q", tokenRequest.path, tokenRequest.requestURI, gcpTestTokenPath)
	}
	if values := tokenRequest.header.Values("Metadata-Flavor"); len(values) != 1 || values[0] != "Google" {
		t.Fatalf("Metadata-Flavor request values = %q; want Google exactly once", values)
	}
	if tokenRequest.body != "" {
		t.Fatalf("metadata request carried a body: %q", tokenRequest.body)
	}
	// The metadata server refuses any request that looks proxied, and the
	// token request has no credential of its own to present.
	for _, name := range []string{"X-Forwarded-For", "Authorization"} {
		if values := tokenRequest.header.Values(name); len(values) != 0 {
			t.Fatalf("metadata request carried %s: %q", name, values)
		}
	}

	decryptRequests := kms.requests()
	if len(decryptRequests) != 1 {
		t.Fatalf("decrypt request count = %d; want exactly 1", len(decryptRequests))
	}
	decrypt := decryptRequests[0]
	if decrypt.method != http.MethodPost {
		t.Fatalf("method = %q; want POST", decrypt.method)
	}
	if decrypt.path != gcpTestPath {
		t.Fatalf("path = %q; want %q", decrypt.path, gcpTestPath)
	}
	// The request URI is the bytes on the wire: an escaped "%3Adecrypt" would
	// decode to the same path but is not the documented route.
	if decrypt.requestURI != gcpTestPath {
		t.Fatalf("request URI = %q; want the exact path %q", decrypt.requestURI, gcpTestPath)
	}
	if !strings.HasSuffix(decrypt.requestURI, "/cryptoKeys/kodama_KEK:decrypt") {
		t.Fatalf("request URI = %q; want the literal :decrypt suffix", decrypt.requestURI)
	}
	if decrypt.rawQuery != "" {
		t.Fatalf("request URL carried query %q; the token belongs only in Authorization", decrypt.rawQuery)
	}
	wantAuthorization := "Bearer " + gcpIssuedToken(1)
	if values := decrypt.header.Values("Authorization"); len(values) != 1 || values[0] != wantAuthorization {
		t.Fatal("the issued token was not sent exactly once as Authorization: Bearer")
	}
	for name, values := range decrypt.header {
		if http.CanonicalHeaderKey(name) == "Authorization" {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, gcpTestToken) {
				t.Fatalf("header %q repeated the token outside Authorization", name)
			}
		}
	}
	if accept := decrypt.header.Get("Accept"); accept != "application/json" {
		t.Fatalf("Accept = %q; want application/json", accept)
	}
	if contentType := decrypt.header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", contentType)
	}
	if values := decrypt.header.Values("Metadata-Flavor"); len(values) != 0 {
		t.Fatalf("decrypt request carried Metadata-Flavor: %q", values)
	}
	wantBody := `{"ciphertext":"` + base64.StdEncoding.EncodeToString(blob) + `","ciphertextCrc32c":"` + gcpCheckCRC32C + `"}`
	if decrypt.body != wantBody {
		t.Fatalf("body = %s; want %s", decrypt.body, wantBody)
	}
	if strings.Contains(decrypt.body, gcpTestToken) {
		t.Fatal("request body contained the token")
	}
}

// TestGCPKMSReturnsDecodedPlaintextUnchanged proves the client decodes base64
// and does not round, truncate or pad the decoded bytes. Length enforcement
// belongs to the shared cloud provider (proved below).
func TestGCPKMSReturnsDecodedPlaintextUnchanged(t *testing.T) {
	cases := [][]byte{
		gcpTestKEK(),
		bytes.Repeat([]byte{0x01}, secret.DataKeyBytes-1),
		bytes.Repeat([]byte{0x02}, secret.DataKeyBytes+1),
	}
	for _, plaintext := range cases {
		t.Run(fmt.Sprintf("length %d", len(plaintext)), func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, plaintext)))
			got, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("plaintext = %x; want the exact decoded reply %x", got, plaintext)
			}
		})
	}
}

// TestGCPKMSRefusesBadKeyReferencesBeforeAnyIO pins the ordering: the
// reference grammar is checked before a token is read or a request built, so a
// refused reference reaches neither the metadata server nor Cloud KMS.
func TestGCPKMSRefusesBadKeyReferencesBeforeAnyIO(t *testing.T) {
	var metadata, kms gcpRecorder
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))

	bad := []string{
		"",
		"gcp-kms:",
		"gcp-kms:projects/kodama-prod",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key/cryptoKeyVersions/1",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key:decrypt",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/../cryptoKeys/key",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/%2e%2e",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key?x=1",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key#frag",
		"gcp-kms:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key\n",
		"gcp-kms:projects/kodama-prod/locations/europe west1/keyRings/kodama-ring/cryptoKeys/key",
		"gcp-kms:/v1/projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key",
		"GCP-KMS:projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key",
		"aws-kms:alias/kodama",
		"scw-kms:fr-par/kid",
		"projects/kodama-prod/locations/europe-west1/keyRings/kodama-ring/cryptoKeys/key",
	}
	for _, reference := range bad {
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(reference), []byte("blob"))
		if err == nil {
			t.Fatalf("reference %q was accepted and returned %d plaintext bytes", reference, len(plaintext))
		}
		requireFaultClass(t, err, fault.InvalidInput)
	}
	if metadata.count() != 0 {
		t.Fatalf("bad references read %d tokens; the grammar must be checked before the metadata server is asked", metadata.count())
	}
	if kms.count() != 0 {
		t.Fatalf("bad references made %d decrypt requests; the grammar must be checked first", kms.count())
	}

	if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
		t.Fatalf("valid reference was refused: %v", err)
	}
	if metadata.count() != 1 || kms.count() != 1 {
		t.Fatalf("valid reference made %d metadata and %d decrypt requests; want 1 and 1", metadata.count(), kms.count())
	}
}

// TestGCPKMSCachesTheToken proves the token is read once and reused: every
// Decrypt performs its own decrypt request (caching plaintext belongs to the
// shared cloud provider) but shares one metadata request.
func TestGCPKMSCachesTheToken(t *testing.T) {
	var metadata, kms gcpRecorder
	clock := newGCPClock()
	client := gcpStart(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())), clock.Now).client
	for attempt := range 5 {
		if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
	}
	if metadata.count() != 1 {
		t.Fatalf("5 decrypts read %d tokens; want exactly 1", metadata.count())
	}
	requests := kms.requests()
	if len(requests) != 5 {
		t.Fatalf("5 decrypts made %d decrypt requests; want one per call", len(requests))
	}
	for index, request := range requests {
		if request.header.Get("Authorization") != "Bearer "+gcpIssuedToken(1) {
			t.Fatalf("decrypt %d did not carry the cached token", index)
		}
	}
}

// TestGCPKMSRefreshesTheTokenBeforeItExpires pins the fixed margin with
// literals: a one-hour token is reused until 3 minutes 45 seconds before its
// expiry and read again from that instant on.
func TestGCPKMSRefreshesTheTokenBeforeItExpires(t *testing.T) {
	const margin = 3*time.Minute + 45*time.Second // the documented margin, spelled as a literal
	var metadata, kms gcpRecorder
	clock := newGCPClock()
	client := gcpStart(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())), clock.Now).client
	decrypt := func() {
		t.Helper()
		if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
	}

	decrypt()
	clock.advance(time.Hour - margin - time.Nanosecond)
	decrypt()
	if metadata.count() != 1 {
		t.Fatalf("token was read %d times just before the margin; want the cached token", metadata.count())
	}
	clock.advance(time.Nanosecond)
	decrypt()
	if metadata.count() != 2 {
		t.Fatalf("token was read %d times at the margin; want a second read", metadata.count())
	}
	requests := kms.requests()
	if requests[1].header.Get("Authorization") != "Bearer "+gcpIssuedToken(1) {
		t.Fatal("the decrypt before the margin did not carry the first token")
	}
	if requests[2].header.Get("Authorization") != "Bearer "+gcpIssuedToken(2) {
		t.Fatal("the decrypt at the margin did not carry the newly read token")
	}
	// The second token has its own full lifetime.
	clock.advance(time.Hour - margin - time.Nanosecond)
	decrypt()
	if metadata.count() != 2 {
		t.Fatalf("second token was read again after %d reads; want it cached for its own lifetime", metadata.count())
	}
}

// TestGCPKMSTokenLifetimeBoundaries pins which lifetimes are cached. The
// metadata server may hand out a token that is already close to expiry: one
// inside the margin serves the decrypt that read it and is never cached. An
// absurd lifetime is capped at one hour rather than trusted or overflowed.
func TestGCPKMSTokenLifetimeBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		expiresIn int64
		cached    bool
	}{
		{name: "one second", expiresIn: 1, cached: false},
		{name: "exactly the margin", expiresIn: 225, cached: false},
		{name: "one second over the margin", expiresIn: 226, cached: true},
		{name: "one hour", expiresIn: 3600, cached: true},
		{name: "largest int64", expiresIn: math.MaxInt64, cached: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			clock := newGCPClock()
			client := gcpStart(t, gcpTokens(&metadata, testCase.expiresIn), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())), clock.Now).client
			for attempt := range 2 {
				if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
					t.Fatalf("Decrypt %d: %v", attempt, err)
				}
			}
			want := 2
			if testCase.cached {
				want = 1
			}
			if metadata.count() != want {
				t.Fatalf("two decrypts read %d tokens; want %d", metadata.count(), want)
			}
		})
	}

	t.Run("largest int64 is capped at one hour", func(t *testing.T) {
		var metadata, kms gcpRecorder
		clock := newGCPClock()
		client := gcpStart(t, gcpTokens(&metadata, math.MaxInt64), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())), clock.Now).client
		for range 2 {
			if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			clock.advance(time.Hour)
		}
		if metadata.count() != 2 {
			t.Fatalf("token with an absurd lifetime was read %d times across an hour; want 2", metadata.count())
		}
	})
}

// TestGCPKMSRetriesOnceAfterARejectedToken proves a 401 from Cloud KMS drops
// the cached token and retries exactly once with a newly read one.
func TestGCPKMSRetriesOnceAfterARejectedToken(t *testing.T) {
	var metadata, kms gcpRecorder
	reply := gcpReply(t, gcpTestKEK())
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if kms.record(request) == 2 {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(writer, reply)
	}))

	// The first decrypt caches token 1; the second is rejected with it.
	for attempt := range 2 {
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
		if err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
		if !bytes.Equal(plaintext, gcpTestKEK()) {
			t.Fatalf("Decrypt %d plaintext = %x; want the reply plaintext", attempt, plaintext)
		}
	}
	if metadata.count() != 2 {
		t.Fatalf("token was read %d times; want 2 (the rejected token must be dropped)", metadata.count())
	}
	requests := kms.requests()
	if len(requests) != 3 {
		t.Fatalf("decrypt requests = %d; want 3 (one retry after the rejection)", len(requests))
	}
	wantTokens := []int{1, 1, 2}
	for index, request := range requests {
		if request.header.Get("Authorization") != "Bearer "+gcpIssuedToken(wantTokens[index]) {
			t.Fatalf("decrypt request %d did not carry token %d", index, wantTokens[index])
		}
		if request.body != requests[0].body {
			t.Fatalf("decrypt request %d changed the request body", index)
		}
	}
	// The newly read token is cached like any other.
	if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
		t.Fatalf("Decrypt after the retry: %v", err)
	}
	if metadata.count() != 2 {
		t.Fatalf("token was read %d times after the retry; want the retried token cached", metadata.count())
	}
}

// TestGCPKMSNeverLoopsOnRejectedTokens proves the retry is bounded: a Cloud
// KMS that rejects every token gets exactly two requests per Decrypt, and the
// call fails as unauthenticated without caching the rejected token.
func TestGCPKMSNeverLoopsOnRejectedTokens(t *testing.T) {
	var metadata, kms gcpRecorder
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpRejecting(&kms, 16, gcpReply(t, gcpTestKEK())))

	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
	requireFaultClass(t, err, fault.Unauthenticated)
	if plaintext != nil {
		t.Fatalf("rejected decrypt returned %d plaintext bytes", len(plaintext))
	}
	if kms.count() != 2 {
		t.Fatalf("decrypt requests = %d; want exactly 2 (one retry, never a loop)", kms.count())
	}
	if metadata.count() != 2 {
		t.Fatalf("token was read %d times; want 2 (one per attempt)", metadata.count())
	}
	if text := err.Error(); strings.Contains(text, gcpTestToken) || strings.Contains(text, "server-body-sentinel") {
		t.Fatalf("rejection disclosed the token or the response body: %q", text)
	}

	// The second rejected token is not cached either: the next call starts
	// with a newly read token and is again bounded at two requests.
	_, err = client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
	requireFaultClass(t, err, fault.Unauthenticated)
	if kms.count() != 4 || metadata.count() != 4 {
		t.Fatalf("second call made %d decrypt and %d metadata requests in total; want 4 and 4", kms.count(), metadata.count())
	}
}

// TestGCPKMSConcurrentDecryptsReadOneToken proves the token cache is guarded:
// concurrent first decrypts share one metadata request. Run under -race.
func TestGCPKMSConcurrentDecryptsReadOneToken(t *testing.T) {
	const callers = 16
	var metadata, kms gcpRecorder
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))

	start := make(chan struct{})
	failures := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if err == nil && !bytes.Equal(plaintext, gcpTestKEK()) {
				err = errors.New("concurrent decrypt returned the wrong plaintext")
			}
			failures <- err
		}()
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("concurrent Decrypt: %v", err)
		}
	}
	if metadata.count() != 1 {
		t.Fatalf("%d concurrent decrypts read %d tokens; want exactly 1", callers, metadata.count())
	}
	if kms.count() != callers {
		t.Fatalf("%d concurrent decrypts made %d decrypt requests; want one per call", callers, kms.count())
	}
}

// TestGCPKMSDoesNotCacheAFailedTokenRead proves a failed read caches nothing,
// neither a token nor the failure: the next Decrypt asks the metadata server
// again and succeeds.
func TestGCPKMSDoesNotCacheAFailedTokenRead(t *testing.T) {
	failures := []struct {
		name   string
		status int
		flavor string
		body   string
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, flavor: "Google"},
		{name: "no identity", status: http.StatusNotFound, flavor: "Google"},
		{name: "unusable reply", status: http.StatusOK, flavor: "Google", body: `{"access_token":""}`},
		{name: "not the metadata server", status: http.StatusOK, body: gcpTokenReply(gcpTestToken+"-spoofed", gcpHourToken)},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			issue := gcpTokens(&gcpRecorder{}, gcpHourToken)
			client := gcpClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if metadata.record(request) == 1 {
					if failure.flavor != "" {
						writer.Header().Set("Metadata-Flavor", failure.flavor)
					}
					writer.WriteHeader(failure.status)
					_, _ = io.WriteString(writer, failure.body)
					return
				}
				issue.ServeHTTP(writer, request)
			}), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))

			if plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err == nil {
				t.Fatalf("failed token read returned %d plaintext bytes", len(plaintext))
			}
			if kms.count() != 0 {
				t.Fatalf("failed token read still made %d decrypt requests", kms.count())
			}
			if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
				t.Fatalf("Decrypt after a failed token read: %v", err)
			}
			if metadata.count() != 2 {
				t.Fatalf("metadata requests = %d; want 2 (a failed read must not be cached)", metadata.count())
			}
			requests := kms.requests()
			if len(requests) != 1 {
				t.Fatalf("decrypt requests = %d; want 1", len(requests))
			}
			if strings.Contains(requests[0].header.Get("Authorization"), "spoofed") {
				t.Fatal("a token from a refused reply was sent to Cloud KMS")
			}
		})
	}
}

// TestGCPKMSRequiresTheMetadataFlavorResponseHeader pins the documented check
// that the responder really is the metadata server: a well-formed token reply
// without the exact "Metadata-Flavor: Google" response header is refused and
// its token never reaches Cloud KMS.
func TestGCPKMSRequiresTheMetadataFlavorResponseHeader(t *testing.T) {
	cases := []struct {
		name   string
		values []string
	}{
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "another flavor", values: []string{"Amazon"}},
		{name: "lowercase", values: []string{"google"}},
		{name: "suffixed", values: []string{"Google2"}},
		{name: "listed", values: []string{"Google, Amazon"}},
		{name: "another flavor first", values: []string{"Amazon", "Google"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				metadata.record(request)
				for _, value := range testCase.values {
					writer.Header().Add("Metadata-Flavor", value)
				}
				_, _ = io.WriteString(writer, gcpTokenReply(gcpTestToken, gcpHourToken))
			}), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))

			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if plaintext != nil {
				t.Fatalf("refusal returned %d plaintext bytes", len(plaintext))
			}
			if metadata.count() != 1 {
				t.Fatalf("metadata requests = %d; want 1", metadata.count())
			}
			if kms.count() != 0 {
				t.Fatalf("a token from an unidentified responder reached Cloud KMS %d times", kms.count())
			}
			if strings.Contains(err.Error(), gcpTestToken) {
				t.Fatalf("refusal disclosed the token: %q", err.Error())
			}
		})
	}
}

// TestGCPKMSRefusesUnusableTokenReplies pins the token reply grammar: a
// malformed body, an empty or missing token, a token that cannot be a header
// value, a non-Bearer type and a non-positive lifetime are all refused before
// anything is sent to Cloud KMS.
func TestGCPKMSRefusesUnusableTokenReplies(t *testing.T) {
	valid := gcpTokenReply(gcpTestToken, gcpHourToken)
	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "malformed json", body: `{"access_token":`},
		{name: "not an object", body: `"` + gcpTestToken + `"`},
		{name: "trailing garbage", body: valid + "x"},
		{name: "second json object", body: valid + valid},
		{name: "missing token", body: `{"expires_in":3600,"token_type":"Bearer"}`},
		{name: "null token", body: `{"access_token":null,"expires_in":3600,"token_type":"Bearer"}`},
		{name: "empty token", body: `{"access_token":"","expires_in":3600,"token_type":"Bearer"}`},
		{name: "numeric token", body: `{"access_token":12345,"expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a space", body: `{"access_token":"ya29 token","expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a line feed", body: `{"access_token":"ya29\ntoken","expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a carriage return", body: `{"access_token":"ya29\rX-Injected: 1","expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a tab", body: `{"access_token":"ya29\ttoken","expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a nul", body: `{"access_token":"ya29\u0000token","expires_in":3600,"token_type":"Bearer"}`},
		{name: "token with a delete", body: `{"access_token":"ya29\u007ftoken","expires_in":3600,"token_type":"Bearer"}`},
		{name: "non ascii token", body: `{"access_token":"ya29é","expires_in":3600,"token_type":"Bearer"}`},
		{name: "missing token type", body: `{"access_token":"` + gcpTestToken + `","expires_in":3600}`},
		{name: "empty token type", body: `{"access_token":"` + gcpTestToken + `","expires_in":3600,"token_type":""}`},
		{name: "mac token type", body: `{"access_token":"` + gcpTestToken + `","expires_in":3600,"token_type":"MAC"}`},
		{name: "bearer prefix token type", body: `{"access_token":"` + gcpTestToken + `","expires_in":3600,"token_type":"Bearer2"}`},
		{name: "missing lifetime", body: `{"access_token":"` + gcpTestToken + `","token_type":"Bearer"}`},
		{name: "zero lifetime", body: `{"access_token":"` + gcpTestToken + `","expires_in":0,"token_type":"Bearer"}`},
		{name: "negative lifetime", body: `{"access_token":"` + gcpTestToken + `","expires_in":-3600,"token_type":"Bearer"}`},
		{name: "string lifetime", body: `{"access_token":"` + gcpTestToken + `","expires_in":"3600","token_type":"Bearer"}`},
		{name: "overflowing lifetime", body: `{"access_token":"` + gcpTestToken + `","expires_in":9223372036854775808,"token_type":"Bearer"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokenBody(&metadata, http.StatusOK, testCase.body), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if plaintext != nil {
				t.Fatalf("refusal returned %d plaintext bytes", len(plaintext))
			}
			if kms.count() != 0 {
				t.Fatalf("an unusable token reply still made %d decrypt requests", kms.count())
			}
			if text := err.Error(); strings.Contains(text, "ya29") || strings.Contains(text, "access_token") {
				t.Fatalf("refusal echoed the token reply: %q", text)
			}
		})
	}

	// The token type is case-insensitive (RFC 6749 section 5.1).
	t.Run("lowercase bearer accepted", func(t *testing.T) {
		var metadata, kms gcpRecorder
		body := `{"access_token":"` + gcpTestToken + `","expires_in":3600,"token_type":"bearer"}`
		client := gcpClient(t, gcpTokenBody(&metadata, http.StatusOK, body), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
		if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if requests := kms.requests(); len(requests) != 1 || requests[0].header.Get("Authorization") != "Bearer "+gcpTestToken {
			t.Fatal("the token was not sent as Authorization: Bearer")
		}
	})
}

// TestGCPKMSMapsMetadataStatuses pins the metadata status table. A workload
// without a bound service account is a credential fault that says so; a
// throttled or failing metadata server is an unavailable dependency. No
// refusal reads the body, and none reaches Cloud KMS.
func TestGCPKMSMapsMetadataStatuses(t *testing.T) {
	cases := []struct {
		status     int
		class      fault.Class
		noIdentity bool
	}{
		{status: http.StatusUnauthorized, class: fault.Unauthenticated, noIdentity: true},
		{status: http.StatusNotFound, class: fault.Unauthenticated, noIdentity: true},
		{status: http.StatusForbidden, class: fault.Forbidden, noIdentity: true},
		{status: http.StatusTooManyRequests, class: fault.DependencyUnavailable},
		{status: http.StatusInternalServerError, class: fault.DependencyUnavailable},
		{status: http.StatusBadGateway, class: fault.DependencyUnavailable},
		{status: http.StatusServiceUnavailable, class: fault.DependencyUnavailable},
		{status: http.StatusBadRequest, class: fault.DependencyUnavailable},
		{status: http.StatusMethodNotAllowed, class: fault.DependencyUnavailable},
		{status: 499, class: fault.DependencyUnavailable},
		// Any status but 200 is refused even with a valid token body.
		{status: http.StatusCreated, class: fault.DependencyUnavailable},
		{status: http.StatusAccepted, class: fault.DependencyUnavailable},
		{status: http.StatusMovedPermanently, class: fault.DependencyUnavailable},
	}
	// The body is larger than the response bound and repeats a valid token
	// reply: a refusal is decided by the status line alone.
	body := gcpTokenReply(gcpTestToken, gcpHourToken) + strings.Repeat(" ", 32<<10) + "server-body-sentinel"
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("status %d", testCase.status), func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokenBody(&metadata, testCase.status, body), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			requireFaultClass(t, err, testCase.class)
			if plaintext != nil {
				t.Fatalf("status %d returned %d plaintext bytes on failure", testCase.status, len(plaintext))
			}
			if metadata.count() != 1 {
				t.Fatalf("status %d made %d metadata requests; want exactly 1 (no retry)", testCase.status, metadata.count())
			}
			if kms.count() != 0 {
				t.Fatalf("status %d still made %d decrypt requests", testCase.status, kms.count())
			}
			text := err.Error()
			if strings.Contains(text, gcpTestToken) || strings.Contains(text, "server-body-sentinel") {
				t.Fatalf("status %d error disclosed the token or the response body: %q", testCase.status, text)
			}
			if got := strings.Contains(text, "the workload has no usable identity"); got != testCase.noIdentity {
				t.Fatalf("status %d error = %q; names a missing workload identity = %v, want %v", testCase.status, text, got, testCase.noIdentity)
			}
		})
	}
}

// TestGCPKMSMapsStatuses pins the Cloud KMS status-to-fault table: 401/403 are
// credential faults, 404 is not found, 429 and 5xx are an unavailable
// dependency, and every other 4xx (INVALID_ARGUMENT, FAILED_PRECONDITION) is
// invalid input.
func TestGCPKMSMapsStatuses(t *testing.T) {
	cases := []struct {
		status   int
		class    fault.Class
		requests int
	}{
		{http.StatusUnauthorized, fault.Unauthenticated, 2},
		{http.StatusForbidden, fault.Forbidden, 1},
		{http.StatusNotFound, fault.NotFound, 1},
		{http.StatusTooManyRequests, fault.DependencyUnavailable, 1},
		{http.StatusInternalServerError, fault.DependencyUnavailable, 1},
		{http.StatusBadGateway, fault.DependencyUnavailable, 1},
		{http.StatusServiceUnavailable, fault.DependencyUnavailable, 1},
		{http.StatusGatewayTimeout, fault.DependencyUnavailable, 1},
		{http.StatusBadRequest, fault.InvalidInput, 1},
		{http.StatusMethodNotAllowed, fault.InvalidInput, 1},
		{http.StatusConflict, fault.InvalidInput, 1},
		{http.StatusUnprocessableEntity, fault.InvalidInput, 1},
		{http.StatusTeapot, fault.InvalidInput, 1},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("status %d", testCase.status), func(t *testing.T) {
			// The body repeats the token and a server message: a refusal must
			// render neither.
			var metadata, kms gcpRecorder
			handler := gcpDecrypts(&kms, testCase.status, `{"error":{"message":"server-body-sentinel `+gcpIssuedToken(1)+`"}}`)
			if testCase.status == http.StatusUnauthorized {
				handler = gcpRejecting(&kms, 16, gcpReply(t, gcpTestKEK()))
			}
			client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), handler)
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			requireFaultClass(t, err, testCase.class)
			if plaintext != nil {
				t.Fatalf("status %d returned %d plaintext bytes on failure", testCase.status, len(plaintext))
			}
			if kms.count() != testCase.requests {
				t.Fatalf("status %d made %d decrypt requests; want %d", testCase.status, kms.count(), testCase.requests)
			}
			if text := err.Error(); strings.Contains(text, gcpTestToken) || strings.Contains(text, "server-body-sentinel") {
				t.Fatalf("status %d error disclosed the token or the response body: %q", testCase.status, text)
			}
		})
	}
}

// TestGCPKMSRefusesUnexpectedStatusesWithAValidBody proves the status table's
// default arm: an unexpected 2xx or any 3xx is refused even when the body is a
// well-formed, valid decrypt reply.
func TestGCPKMSRefusesUnexpectedStatusesWithAValidBody(t *testing.T) {
	reply := gcpReply(t, gcpTestKEK())
	for _, status := range []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusFound,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
	} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, status, reply))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if err == nil {
				t.Fatalf("status %d with a valid body returned %d plaintext bytes", status, len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("status %d returned %d plaintext bytes on failure", status, len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if kms.count() != 1 {
				t.Fatalf("status %d made %d requests; want exactly 1", status, kms.count())
			}
		})
	}
}

// TestGCPKMSClassifiesStatusBeforeReadingTheBody proves a non-200 status is
// classified from the status line alone: a 403 carrying a body larger than the
// response bound is still a forbidden refusal, not a dependency failure caused
// by the bound.
func TestGCPKMSClassifiesStatusBeforeReadingTheBody(t *testing.T) {
	var metadata, kms gcpRecorder
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusForbidden, strings.Repeat("x", 100<<10)))
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("a forbidden response with an oversized body returned %d plaintext bytes", len(plaintext))
	}
	if plaintext != nil {
		t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.Forbidden)
}

// TestGCPKMSVerifiesThePlaintextChecksum pins the integrity check: a reply
// that carries plaintextCrc32c must match the decoded bytes or the bytes are
// discarded. The accepted vector is the published CRC-32C check value, so a
// wrong polynomial fails here too.
func TestGCPKMSVerifiesThePlaintextChecksum(t *testing.T) {
	plaintext := base64.StdEncoding.EncodeToString([]byte(gcpCheckInput))
	cases := []struct {
		name     string
		checksum string // the raw JSON value; empty omits the field
		accepted bool
	}{
		{name: "matching string", checksum: `"` + gcpCheckCRC32C + `"`, accepted: true},
		{name: "matching number", checksum: gcpCheckCRC32C, accepted: true},
		// proto3 JSON omits a zero value, so an absent field cannot be
		// required; it is the only unverified reply.
		{name: "absent", accepted: true},
		{name: "off by one string", checksum: `"3808858756"`},
		{name: "off by one number", checksum: `3808858754`},
		{name: "zero", checksum: `"0"`},
		{name: "negative", checksum: `"-3808858755"`},
		// The IEEE polynomial's check value for the same input.
		{name: "crc32 ieee", checksum: `"3421780262"`},
		{name: "truncated to 32 bits plus 2^32", checksum: `"8103826051"`},
		{name: "not a number", checksum: `"abc"`},
		{name: "empty string", checksum: `""`},
		{name: "hexadecimal", checksum: `"0xE3069283"`},
		{name: "exponent", checksum: `3.808858755e9`},
		{name: "boolean", checksum: `true`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := `{"plaintext":"` + plaintext + `","usedPrimary":true}`
			if testCase.checksum != "" {
				body = `{"plaintext":"` + plaintext + `","plaintextCrc32c":` + testCase.checksum + `,"usedPrimary":true}`
			}
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, body))
			got, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if testCase.accepted {
				if err != nil {
					t.Fatalf("reply was refused: %v", err)
				}
				if string(got) != gcpCheckInput {
					t.Fatalf("plaintext = %q; want %q", got, gcpCheckInput)
				}
				return
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if got != nil {
				t.Fatalf("a reply that fails its checksum returned %d plaintext bytes", len(got))
			}
			if kms.count() != 1 {
				t.Fatalf("checksum refusal made %d decrypt requests; want exactly 1 (no retry)", kms.count())
			}
			if text := err.Error(); strings.Contains(text, gcpCheckInput) || strings.Contains(text, plaintext) {
				t.Fatalf("checksum refusal disclosed the plaintext: %q", text)
			}
		})
	}
}

// TestGCPKMSRefusesMalformedReplies pins sanitized dependency failures for a
// malformed body, a missing or empty plaintext, a non-base64 plaintext and a
// body over the documented bound.
func TestGCPKMSRefusesMalformedReplies(t *testing.T) {
	oversize := gcpReply(t, bytes.Repeat([]byte{0x2a}, 96<<10))
	// The reply plaintext below encodes with '+' and '/' under StdEncoding, so
	// the standard alphabet is distinguishable from the URL-safe one.
	nonStdPlaintext := bytes.Repeat([]byte{0xfb}, secret.DataKeyBytes)
	validReply := gcpReply(t, gcpTestKEK())
	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "malformed json", body: `{"plaintext":`},
		{name: "missing plaintext", body: `{}`},
		{name: "null plaintext", body: `{"plaintext":null}`},
		{name: "empty plaintext", body: `{"plaintext":""}`},
		{name: "not base64", body: `{"plaintext":"not base64 !!!"}`},
		{name: "url-safe base64", body: `{"plaintext":"` + base64.URLEncoding.EncodeToString(nonStdPlaintext) + `"}`},
		{name: "unpadded url-safe base64", body: `{"plaintext":"` + base64.RawURLEncoding.EncodeToString(nonStdPlaintext) + `"}`},
		{name: "unpadded base64", body: `{"plaintext":"` + base64.RawStdEncoding.EncodeToString(nonStdPlaintext) + `"}`},
		{name: "trailing garbage", body: validReply + "x"},
		{name: "second json object", body: validReply + `{"plaintext":""}`},
		{name: "oversized body", body: oversize},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, testCase.body))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if err == nil {
				t.Fatalf("reply was accepted and returned %d plaintext bytes", len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if text := err.Error(); strings.Contains(text, "not base64") {
				t.Fatalf("refusal echoed the response body: %q", text)
			}
		})
	}
}

// TestGCPKMSResponseBounds pins both documented reply bounds with literals: a
// valid reply padded to exactly the bound is accepted and one byte more is
// refused, for the 64 KiB decrypt reply and the 16 KiB token reply.
func TestGCPKMSResponseBounds(t *testing.T) {
	const (
		decryptBound = 64 << 10 // the documented bounds, spelled as literals
		tokenBound   = 16 << 10
	)
	reply := gcpReply(t, gcpTestKEK())
	token := gcpTokenReply(gcpTestToken, gcpHourToken)
	pad := func(body string, total int) string {
		return body + strings.Repeat(" ", total-len(body))
	}
	cases := []struct {
		name     string
		token    string
		reply    string
		accepted bool
	}{
		{name: "decrypt reply of exactly the bound", token: token, reply: pad(reply, decryptBound), accepted: true},
		{name: "decrypt reply one byte over", token: token, reply: pad(reply, decryptBound+1)},
		{name: "token reply of exactly the bound", token: pad(token, tokenBound), reply: reply, accepted: true},
		{name: "token reply one byte over", token: pad(token, tokenBound+1), reply: reply},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadata, kms gcpRecorder
			client := gcpClient(t, gcpTokenBody(&metadata, http.StatusOK, testCase.token), gcpDecrypts(&kms, http.StatusOK, testCase.reply))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
			if testCase.accepted {
				if err != nil {
					t.Fatalf("a reply of exactly the bound was refused: %v", err)
				}
				if !bytes.Equal(plaintext, gcpTestKEK()) {
					t.Fatalf("plaintext = %x; want the reply plaintext", plaintext)
				}
				return
			}
			if err == nil {
				t.Fatalf("a reply one byte over the bound returned %d plaintext bytes", len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
		})
	}
}

// TestGCPKMSNeverFollowsRedirects proves neither request is replayed to a
// redirect target: the bearer token never follows a Cloud KMS redirect, and a
// token is never accepted from wherever the metadata address redirects to.
func TestGCPKMSNeverFollowsRedirects(t *testing.T) {
	redirect := func(recorder, target *gcpRecorder, final http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/redirect-target" {
				target.record(request)
				final.ServeHTTP(writer, request)
				return
			}
			recorder.record(request)
			writer.Header().Set("Metadata-Flavor", "Google")
			writer.Header().Set("Location", "/redirect-target")
			writer.WriteHeader(http.StatusTemporaryRedirect)
		})
	}

	t.Run("cloud kms", func(t *testing.T) {
		var metadata, kms, target gcpRecorder
		client := gcpClient(t, gcpTokens(&metadata, gcpHourToken),
			redirect(&kms, &target, gcpDecrypts(&gcpRecorder{}, http.StatusOK, gcpReply(t, gcpTestKEK()))))
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
		if err == nil {
			t.Fatalf("redirect response was followed and returned %d plaintext bytes", len(plaintext))
		}
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if target.count() != 0 {
			t.Fatalf("redirect target received %d requests; the token must never be replayed", target.count())
		}
		if kms.count() != 1 {
			t.Fatalf("decrypt requests = %d; want exactly 1", kms.count())
		}
	})

	t.Run("metadata server", func(t *testing.T) {
		var metadata, kms, target gcpRecorder
		client := gcpClient(t, redirect(&metadata, &target, gcpTokens(&gcpRecorder{}, gcpHourToken)),
			gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
		if err == nil {
			t.Fatalf("metadata redirect was followed and returned %d plaintext bytes", len(plaintext))
		}
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if target.count() != 0 {
			t.Fatalf("metadata redirect target received %d requests", target.count())
		}
		if metadata.count() != 1 {
			t.Fatalf("metadata requests = %d; want exactly 1", metadata.count())
		}
		if kms.count() != 0 {
			t.Fatalf("a redirected token read still made %d decrypt requests", kms.count())
		}
	})
}

// gcpBlocking is a handler that reports its first request and then blocks
// until the client goes away or the test releases it.
func gcpBlocking(recorder *gcpRecorder, entered chan<- struct{}, release <-chan struct{}) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		once.Do(func() { close(entered) })
		select {
		case <-request.Context().Done():
		case <-release:
		}
	})
}

// TestGCPKMSHonoursContextCancellation proves a context cancelled before the
// call, during the metadata request or during the decrypt request returns the
// bare context error: never a success and never a fault class.
func TestGCPKMSHonoursContextCancellation(t *testing.T) {
	t.Run("before the call", func(t *testing.T) {
		var metadata, kms gcpRecorder
		client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Decrypt(ctx, secret.KeyRef(gcpTestRef), []byte("blob"))
		gcpRequireContextError(t, err, context.Canceled)
		// Cancellation is checked before the key-reference grammar: a cancelled
		// caller learns the call was cancelled, not that its reference was bad.
		_, err = client.Decrypt(ctx, secret.KeyRef("not-a-gcp-reference"), []byte("blob"))
		gcpRequireContextError(t, err, context.Canceled)
		if metadata.count() != 0 || kms.count() != 0 {
			t.Fatalf("pre-cancelled call made %d metadata and %d decrypt requests", metadata.count(), kms.count())
		}

		// An expired deadline is reported the same way.
		expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancelExpired()
		_, err = client.Decrypt(expired, secret.KeyRef(gcpTestRef), []byte("blob"))
		gcpRequireContextError(t, err, context.DeadlineExceeded)
	})

	during := func(t *testing.T, client *keys.GCPKMS, entered <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := client.Decrypt(ctx, secret.KeyRef(gcpTestRef), []byte("blob"))
			result <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("server did not receive the request")
		}
		cancel()
		select {
		case err := <-result:
			gcpRequireContextError(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled Decrypt did not return")
		}
	}

	t.Run("during the metadata request", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		// The deferred close unblocks the handler before the server cleanup
		// waits for it, including on the failure paths.
		defer close(release)
		var metadata, kms gcpRecorder
		client := gcpClient(t, gcpBlocking(&metadata, entered, release), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
		during(t, client, entered)
		if kms.count() != 0 {
			t.Fatalf("a cancelled token read still made %d decrypt requests", kms.count())
		}
	})

	t.Run("during the decrypt request", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		var metadata, kms gcpRecorder
		client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpBlocking(&kms, entered, release))
		during(t, client, entered)
		if kms.count() != 1 {
			t.Fatalf("a cancelled decrypt made %d requests; want exactly 1 (cancellation is not retried)", kms.count())
		}
	})
}

// TestGCPKMSErrorsDiscloseNothingSensitive drives every failure family and
// proves no error string carries the bearer token, the ciphertext, the
// plaintext or either endpoint URL.
func TestGCPKMSErrorsDiscloseNothingSensitive(t *testing.T) {
	blob := []byte("gcp-ciphertext-sentinel-bytes")
	plaintext := []byte("gcp-plaintext-sentinel-material!")
	reply := gcpReply(t, plaintext)
	badChecksum := `{"plaintext":"` + base64.StdEncoding.EncodeToString(plaintext) + `","plaintextCrc32c":"1"}`
	echo := "server-body-sentinel " + string(plaintext) + " " + base64.StdEncoding.EncodeToString(blob)
	cases := []struct {
		name        string
		reference   string
		metadata    func(*gcpRecorder) http.Handler
		kms         func(*gcpRecorder) http.Handler
		closeKMS    bool
		closeTokens bool
	}{
		{name: "bad reference", reference: "gcp-kms:projects/x"},
		{name: "kms 400", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusBadRequest, echo) }},
		{name: "kms 401", kms: func(r *gcpRecorder) http.Handler { return gcpRejecting(r, 16, reply) }},
		{name: "kms 403", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusForbidden, echo) }},
		{name: "kms 404", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusNotFound, echo) }},
		{name: "kms 429", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusTooManyRequests, echo) }},
		{name: "kms 503", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusServiceUnavailable, echo) }},
		{name: "kms 302", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusFound, reply) }},
		{name: "kms malformed reply", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusOK, reply[:len(reply)-1]) }},
		{name: "kms bad base64", kms: func(r *gcpRecorder) http.Handler {
			return gcpDecrypts(r, http.StatusOK, `{"plaintext":"`+string(plaintext)+`"}`)
		}},
		{name: "kms checksum mismatch", kms: func(r *gcpRecorder) http.Handler { return gcpDecrypts(r, http.StatusOK, badChecksum) }},
		{name: "kms oversized reply", kms: func(r *gcpRecorder) http.Handler {
			return gcpDecrypts(r, http.StatusOK, reply+strings.Repeat(" ", 64<<10))
		}},
		{name: "kms unreachable", closeKMS: true},
		{name: "metadata 403", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusForbidden, gcpTokenReply(gcpTestToken, gcpHourToken))
		}},
		{name: "metadata 404", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusNotFound, gcpTokenReply(gcpTestToken, gcpHourToken))
		}},
		{name: "metadata 503", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusServiceUnavailable, gcpTokenReply(gcpTestToken, gcpHourToken))
		}},
		{name: "metadata malformed reply", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusOK, `{"access_token":"`+gcpTestToken+`","expires_in":`)
		}},
		{name: "metadata unusable token", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusOK, `{"access_token":"`+gcpTestToken+` x","expires_in":3600,"token_type":"Bearer"}`)
		}},
		{name: "metadata non-bearer token", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusOK, `{"access_token":"`+gcpTestToken+`","expires_in":3600,"token_type":"MAC"}`)
		}},
		{name: "metadata unidentified responder", metadata: func(r *gcpRecorder) http.Handler {
			return gcpDecrypts(r, http.StatusOK, gcpTokenReply(gcpTestToken, gcpHourToken))
		}},
		{name: "metadata oversized reply", metadata: func(r *gcpRecorder) http.Handler {
			return gcpTokenBody(r, http.StatusOK, gcpTokenReply(gcpTestToken, gcpHourToken)+strings.Repeat(" ", 16<<10))
		}},
		{name: "metadata unreachable", closeTokens: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var metadataRecorder, kmsRecorder gcpRecorder
			metadata := gcpTokens(&metadataRecorder, gcpHourToken)
			if testCase.metadata != nil {
				metadata = testCase.metadata(&metadataRecorder)
			}
			kms := gcpDecrypts(&kmsRecorder, http.StatusOK, reply)
			if testCase.kms != nil {
				kms = testCase.kms(&kmsRecorder)
			}
			fixture := gcpStart(t, metadata, kms, nil)
			if testCase.closeKMS {
				fixture.kms.Close()
			}
			if testCase.closeTokens {
				fixture.metadata.Close()
			}
			reference := gcpTestRef
			if testCase.reference != "" {
				reference = testCase.reference
			}

			got, err := fixture.client.Decrypt(context.Background(), secret.KeyRef(reference), blob)
			if err == nil {
				t.Fatalf("failure case returned %d plaintext bytes", len(got))
			}
			if got != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(got))
			}
			if _, classified := fault.ClassOf(err); !classified {
				t.Fatalf("failure is not a fault: %q", err.Error())
			}
			text := err.Error()
			if message, _ := fault.MessageOf(err); message != "" {
				text += "\n" + message
			}
			sensitive := map[string]string{
				"the bearer token":          gcpTestToken,
				"the bearer scheme":         "Bearer",
				"the raw ciphertext":        string(blob),
				"the base64 ciphertext":     base64.StdEncoding.EncodeToString(blob),
				"the hex ciphertext":        hex.EncodeToString(blob),
				"the raw plaintext":         string(plaintext),
				"the base64 plaintext":      base64.StdEncoding.EncodeToString(plaintext),
				"the hex plaintext":         hex.EncodeToString(plaintext),
				"the response body":         "server-body-sentinel",
				"the cloud kms URL":         fixture.kms.URL,
				"the cloud kms address":     fixture.kms.Listener.Addr().String(),
				"the metadata URL":          fixture.metadata.URL,
				"the metadata address":      fixture.metadata.Listener.Addr().String(),
				"a loopback address":        "127.0.0.1",
				"a URL scheme":              "://",
				"the token path":            "computeMetadata",
				"the decrypt path":          ":decrypt",
				"another provider's client": "scaleway",
			}
			for what, value := range sensitive {
				if strings.Contains(text, value) {
					t.Fatalf("error disclosed %s: %q", what, text)
				}
			}
		})
	}
}

// TestGCPKMSTransportFailureIsClassifiedAndSanitized proves a transport
// failure is a dependency fault that keeps its private cause reachable and
// that an underlying dial error carrying the token is never rendered.
func TestGCPKMSTransportFailureIsClassifiedAndSanitized(t *testing.T) {
	var metadata gcpRecorder
	metadataServer := httptest.NewServer(gcpTokens(&metadata, gcpHourToken))
	t.Cleanup(metadataServer.Close)
	dialCause := errors.New("dial-refused-sentinel")
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("dial refused for token %s: %w", gcpIssuedToken(1), dialCause)
		},
	}
	client, err := keys.NewGCPKMS(keys.GCPKMSConfig{
		KMSEndpoint:      "https://cloudkms.example.invalid",
		MetadataEndpoint: metadataServer.URL,
		HTTPClient:       &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("transport failure returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, dialCause) {
		t.Fatalf("transport failure dropped its private cause: %v", err)
	}
	if strings.Contains(err.Error(), gcpTestToken) || strings.Contains(err.Error(), dialCause.Error()) {
		t.Fatalf("transport refusal disclosed the token or the private cause: %q", err.Error())
	}
	// A transport failure is not a rejected token: the token stays cached and
	// the call is not retried.
	if _, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob")); err == nil {
		t.Fatal("second transport failure succeeded")
	}
	if metadata.count() != 1 {
		t.Fatalf("token was read %d times across two transport failures; want 1", metadata.count())
	}
}

// TestGCPKMSVerifiesTLSByDefault proves the client-owned transport keeps
// certificate verification on: an untrusted TLS server is refused and its
// handler is never reached.
func TestGCPKMSVerifiesTLSByDefault(t *testing.T) {
	var metadata, kms gcpRecorder
	kmsServer := httptest.NewTLSServer(gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
	t.Cleanup(kmsServer.Close)
	metadataServer := httptest.NewServer(gcpTokens(&metadata, gcpHourToken))
	t.Cleanup(metadataServer.Close)

	client, err := keys.NewGCPKMS(keys.GCPKMSConfig{KMSEndpoint: kmsServer.URL, MetadataEndpoint: metadataServer.URL})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(gcpTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("untrusted TLS server was accepted and returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if kms.count() != 0 {
		t.Fatalf("untrusted TLS server handler was reached %d times", kms.count())
	}
	if strings.Contains(err.Error(), gcpTestToken) {
		t.Fatal("TLS refusal disclosed the token")
	}
}

// TestGCPKMSConfigurationRefusals pins constructor refusals: no downgrade of
// the Cloud KMS origin to cleartext, no embedded URL credential, no ambient
// proxy or opaque transport, and no metadata origin that could leave the node.
func TestGCPKMSConfigurationRefusals(t *testing.T) {
	cases := []struct {
		name   string
		config keys.GCPKMSConfig
	}{
		{name: "kms plain http", config: keys.GCPKMSConfig{KMSEndpoint: "http://cloudkms.example.com"}},
		{name: "kms plain http loopback", config: keys.GCPKMSConfig{KMSEndpoint: "http://127.0.0.1:8080"}},
		{name: "kms no scheme", config: keys.GCPKMSConfig{KMSEndpoint: "cloudkms.example.com"}},
		{name: "kms no host", config: keys.GCPKMSConfig{KMSEndpoint: "https://"}},
		{name: "kms userinfo", config: keys.GCPKMSConfig{KMSEndpoint: "https://user:url-password-sentinel@cloudkms.example.com"}},
		{name: "kms query", config: keys.GCPKMSConfig{KMSEndpoint: "https://cloudkms.example.com?token=query-token-sentinel"}},
		{name: "kms forced query", config: keys.GCPKMSConfig{KMSEndpoint: "https://cloudkms.example.com?"}},
		{name: "kms fragment", config: keys.GCPKMSConfig{KMSEndpoint: "https://cloudkms.example.com#x"}},
		{name: "kms whitespace", config: keys.GCPKMSConfig{KMSEndpoint: "https://cloudkms.example.com /x"}},
		{name: "kms unparsable", config: keys.GCPKMSConfig{KMSEndpoint: "https://[::1"}},
		{name: "shared default transport", config: keys.GCPKMSConfig{HTTPClient: &http.Client{Transport: http.DefaultTransport}}},
		{name: "typed nil transport", config: keys.GCPKMSConfig{HTTPClient: &http.Client{Transport: (*http.Transport)(nil)}}},
		{name: "opaque transport", config: keys.GCPKMSConfig{HTTPClient: &http.Client{Transport: gcpRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("must not be used")
		})}}},

		// The metadata server is plain HTTP by design; the GKE metadata server
		// does not serve https, so an https override is refused rather than
		// silently pointed at something else.
		{name: "metadata https", config: keys.GCPKMSConfig{MetadataEndpoint: "https://169.254.169.254"}},
		{name: "metadata https loopback", config: keys.GCPKMSConfig{MetadataEndpoint: "https://127.0.0.1:8443"}},
		{name: "metadata https name", config: keys.GCPKMSConfig{MetadataEndpoint: "https://metadata.google.internal"}},
		{name: "metadata no scheme", config: keys.GCPKMSConfig{MetadataEndpoint: "169.254.169.254"}},
		{name: "metadata no host", config: keys.GCPKMSConfig{MetadataEndpoint: "http://"}},
		{name: "metadata port only", config: keys.GCPKMSConfig{MetadataEndpoint: "http://:80"}},
		// The token source can never be an arbitrary network host.
		{name: "metadata private address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://10.0.0.1"}},
		{name: "metadata private address 192", config: keys.GCPKMSConfig{MetadataEndpoint: "http://192.168.1.1:8080"}},
		{name: "metadata carrier grade nat address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://100.64.0.1"}},
		{name: "metadata public address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://8.8.8.8"}},
		{name: "metadata unspecified address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://0.0.0.0"}},
		{name: "metadata public ipv6", config: keys.GCPKMSConfig{MetadataEndpoint: "http://[2001:db8::1]"}},
		{name: "metadata unique local ipv6", config: keys.GCPKMSConfig{MetadataEndpoint: "http://[fd00::1]"}},
		{name: "metadata localhost name", config: keys.GCPKMSConfig{MetadataEndpoint: "http://localhost:8080"}},
		{name: "metadata other name", config: keys.GCPKMSConfig{MetadataEndpoint: "http://metadata.example.com"}},
		{name: "metadata short name", config: keys.GCPKMSConfig{MetadataEndpoint: "http://metadata"}},
		{name: "metadata name suffix", config: keys.GCPKMSConfig{MetadataEndpoint: "http://metadata.google.internal.example.com"}},
		{name: "metadata name prefix", config: keys.GCPKMSConfig{MetadataEndpoint: "http://evil-metadata.google.internal"}},
		{name: "metadata rooted name", config: keys.GCPKMSConfig{MetadataEndpoint: "http://metadata.google.internal."}},
		{name: "metadata uppercase name", config: keys.GCPKMSConfig{MetadataEndpoint: "http://METADATA.GOOGLE.INTERNAL"}},
		{name: "metadata decimal address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://2852039166"}},
		{name: "metadata short address", config: keys.GCPKMSConfig{MetadataEndpoint: "http://127.1"}},
		{name: "metadata userinfo", config: keys.GCPKMSConfig{MetadataEndpoint: "http://user:url-password-sentinel@169.254.169.254"}},
		{name: "metadata userinfo host confusion", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254@metadata.example.com"}},
		{name: "metadata query", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254?token=query-token-sentinel"}},
		{name: "metadata forced query", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254?"}},
		{name: "metadata fragment", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254#x"}},
		{name: "metadata path", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254/computeMetadata/v1"}},
		{name: "metadata whitespace", config: keys.GCPKMSConfig{MetadataEndpoint: "http://169.254.169.254 "}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, err := keys.NewGCPKMS(testCase.config)
			if err == nil {
				t.Fatalf("configuration was accepted: %+v", testCase.config)
			}
			if client != nil {
				t.Fatal("refused configuration returned a client")
			}
			requireFaultClass(t, err, fault.InvalidInput)
			for _, sentinel := range []string{"url-password-sentinel", "query-token-sentinel", "example.com", "scaleway"} {
				if strings.Contains(err.Error(), sentinel) {
					t.Fatalf("refusal echoed configuration material %q: %q", sentinel, err.Error())
				}
			}
		})
	}

	// The defaults and every on-node metadata origin are accepted, and
	// construction makes no request.
	accepted := []keys.GCPKMSConfig{
		{},
		{KMSEndpoint: "https://cloudkms.googleapis.com/"},
		{KMSEndpoint: "https://cloudkms.europe-west1.rep.googleapis.com"},
		{MetadataEndpoint: "http://169.254.169.254"},
		{MetadataEndpoint: "http://169.254.169.254/"},
		{MetadataEndpoint: "http://169.254.169.254:80"},
		{MetadataEndpoint: "http://169.254.169.252:988"},
		{MetadataEndpoint: "http://127.0.0.1:8080"},
		{MetadataEndpoint: "http://[::1]:8080"},
		{MetadataEndpoint: "http://[fe80::1]"},
		{MetadataEndpoint: "http://metadata.google.internal"},
		{HTTPClient: &http.Client{}},
		{HTTPClient: &http.Client{Transport: &http.Transport{}}},
	}
	for _, config := range accepted {
		if client, err := keys.NewGCPKMS(config); err != nil {
			t.Fatalf("NewGCPKMS refused %+v: %v", config, err)
		} else if client == nil {
			t.Fatalf("NewGCPKMS returned a nil client for %+v", config)
		}
	}
}

// TestGCPProviderRoundTripThroughTheSharedProvider drives the real Cloud KMS
// client through keys.NewGCP end to end: the key-encryption key is unwrapped
// once with one token, wraps a data key, and a second provider over the same
// endpoints unwraps it again.
func TestGCPProviderRoundTripThroughTheSharedProvider(t *testing.T) {
	var metadata, kms gcpRecorder
	blob := []byte("wrapped-kek-v1")
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), gcpDecrypts(&kms, http.StatusOK, gcpReply(t, gcpTestKEK())))
	config := keys.CloudConfig{
		KeyRef:   secret.KeyRef(gcpTestRef),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: blob},
	}
	provider, err := keys.NewGCP(client, config, deterministicRandom(0x61))
	if err != nil {
		t.Fatalf("NewGCP: %v", err)
	}
	if metadata.count() != 0 || kms.count() != 0 {
		t.Fatalf("construction made %d metadata and %d decrypt requests; want none", metadata.count(), kms.count())
	}
	associatedData := testAssociatedBytes(t, testAssociated(1))
	request := secret.WrapRequest{
		KeyRef:         secret.KeyRef(gcpTestRef),
		KeyVersion:     1,
		Plaintext:      testDEK(),
		AssociatedData: associatedData,
	}
	wrapped, err := provider.Wrap(context.Background(), request)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	// The wrap is exactly AES-256-GCM under the key Cloud KMS returned.
	if want := gcmSeal(t, gcpTestKEK(), wrapped.Nonce, testDEK(), associatedData); !bytes.Equal(wrapped.Wrapped, want) {
		t.Fatal("wrapped key is not sealed under the key-encryption key Cloud KMS returned")
	}
	if _, err := provider.Wrap(context.Background(), request); err != nil {
		t.Fatalf("second Wrap: %v", err)
	}
	requests := kms.requests()
	if len(requests) != 1 || metadata.count() != 1 {
		t.Fatalf("two wraps made %d decrypt and %d metadata requests; want 1 and 1", len(requests), metadata.count())
	}
	if requests[0].path != gcpTestPath {
		t.Fatalf("decrypt path = %q; want %q", requests[0].path, gcpTestPath)
	}
	wantBody := `{"ciphertext":"` + base64.StdEncoding.EncodeToString(blob) + `","ciphertextCrc32c":"` + gcpChecksum(blob) + `"}`
	if requests[0].body != wantBody {
		t.Fatalf("decrypt body = %s; want the configured blob %s", requests[0].body, wantBody)
	}

	opening, err := keys.NewGCP(client, config, deterministicRandom(0x62))
	if err != nil {
		t.Fatalf("NewGCP: %v", err)
	}
	plaintext, err := opening.Unwrap(context.Background(), secret.UnwrapRequest{
		KeyRef:         secret.KeyRef(gcpTestRef),
		KeyVersion:     1,
		Wrapped:        wrapped.Wrapped,
		Nonce:          wrapped.Nonce,
		AssociatedData: associatedData,
	})
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(plaintext, testDEK()) {
		t.Fatal("unwrapped data key differs from the wrapped one")
	}
	// The second provider unwrapped the key again but shares the client's
	// cached token.
	if kms.count() != 2 || metadata.count() != 1 {
		t.Fatalf("totals = %d decrypt and %d metadata requests; want 2 and 1", kms.count(), metadata.count())
	}
}

// TestGCPProviderEnforcesKEKLengthAndDoesNotCacheFailure drives the real
// client through the shared cloud provider. A 31-byte reply is an
// invalid_input refusal (the provider owns the 32-byte rule), a denial keeps
// its class, and neither is cached: the next call retries the decrypt.
func TestGCPProviderEnforcesKEKLengthAndDoesNotCacheFailure(t *testing.T) {
	var metadata, kms gcpRecorder
	reply := gcpReply(t, gcpTestKEK())
	shortReply := gcpReply(t, bytes.Repeat([]byte{0x33}, secret.DataKeyBytes-1))
	client := gcpClient(t, gcpTokens(&metadata, gcpHourToken), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch kms.record(request) {
		case 1:
			_, _ = io.WriteString(writer, shortReply)
		case 2:
			writer.WriteHeader(http.StatusForbidden)
		default:
			_, _ = io.WriteString(writer, reply)
		}
	}))

	provider, err := keys.NewGCP(client, keys.CloudConfig{
		KeyRef:   secret.KeyRef(gcpTestRef),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: []byte("wrapped-kek-v1")},
	}, deterministicRandom(0x63))
	if err != nil {
		t.Fatalf("NewGCP: %v", err)
	}
	request := secret.WrapRequest{
		KeyRef:         secret.KeyRef(gcpTestRef),
		KeyVersion:     1,
		Plaintext:      testDEK(),
		AssociatedData: testAssociatedBytes(t, testAssociated(1)),
	}
	_, err = provider.Wrap(context.Background(), request)
	requireFaultClass(t, err, fault.InvalidInput)
	_, err = provider.Wrap(context.Background(), request)
	requireFaultClass(t, err, fault.Forbidden)
	if _, err := provider.Wrap(context.Background(), request); err != nil {
		t.Fatalf("third Wrap after a short KEK and a denial: %v", err)
	}
	if kms.count() != 3 {
		t.Fatalf("decrypt requests = %d; want 3 (neither failure may be cached)", kms.count())
	}
	if metadata.count() != 1 {
		t.Fatalf("metadata requests = %d; want 1 (neither failure rejects the token)", metadata.count())
	}
}
