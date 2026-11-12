package keys_test

// The Scaleway Key Manager decrypt client is plain net/http: one bounded,
// authenticated JSON POST to the v1alpha1 decrypt route. These tests pin the
// request shape, the status-to-fault table, the credential and body hygiene,
// the key-reference grammar, the redirect/TLS/transport boundary, the response
// bound and the interaction with the shared cloud provider (32-byte key
// enforcement and failure caching).
//
// Every server is a loopback httptest TLS server; no test contacts a real
// cloud endpoint.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	scalewayTestToken = "scaleway-test-secret-token"
	// scalewayTestRef is the accepted grammar: the v1alpha1 region and key id,
	// both plain lowercase identifiers.
	scalewayTestRef = "scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9"
	// scalewayTestPath is the only route the client may call. The v1 path is
	// not served by Scaleway Key Manager.
	scalewayTestPath = "/key-manager/v1alpha1/regions/fr-par/keys/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9/decrypt"
)

// scalewayTestKEK is the 32-byte key-encryption key the reply unwraps to.
func scalewayTestKEK() []byte {
	return bytes.Repeat([]byte{0x5a}, secret.DataKeyBytes)
}

// scalewayReply encodes one decrypt reply. It is called before a server starts
// so a handler goroutine never touches testing.T.
func scalewayReply(t *testing.T, plaintext []byte) string {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Plaintext string `json:"plaintext"`
	}{base64.StdEncoding.EncodeToString(plaintext)})
	if err != nil {
		t.Fatalf("marshal decrypt reply: %v", err)
	}
	return string(encoded)
}

// scalewayTLSClient starts one TLS test server and returns a client whose
// injected transport trusts that server's certificate. NewScalewayKMS must own
// the transport (clear any proxy, refuse redirects) without losing the test's
// explicit TLS roots.
func scalewayTLSClient(t *testing.T, handler http.Handler) (*keys.ScalewayKMS, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{
		SecretKey:  scalewayTestToken,
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewScalewayKMS: %v", err)
	}
	return client, server
}

// scalewayRoundTripFunc is an opaque RoundTripper used to prove the constructor
// refuses a transport it cannot make proxy-free and redirect-free.
type scalewayRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip scalewayRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// TestScalewayKMSDecryptRequestAndResult pins the exact v1alpha1 decrypt
// request: POST, the one served route, the X-Auth-Token credential header, the
// JSON ciphertext body and the decoded reply.
func TestScalewayKMSDecryptRequestAndResult(t *testing.T) {
	var calls atomic.Int32
	var method, path, requestURI, rawQuery, token, accept, contentType, body string
	var headers http.Header
	plaintext := scalewayTestKEK()
	reply := scalewayReply(t, plaintext)
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		method = request.Method
		path = request.URL.Path
		requestURI = request.RequestURI
		rawQuery = request.URL.RawQuery
		token = request.Header.Get("X-Auth-Token")
		headers = request.Header.Clone()
		accept = request.Header.Get("Accept")
		contentType = request.Header.Get("Content-Type")
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_, _ = io.WriteString(writer, reply)
	}))

	blob := []byte{0, 1, 2, 255}
	got, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext = %x; want %x", got, plaintext)
	}
	if calls.Load() != 1 {
		t.Fatalf("request count = %d; want exactly 1", calls.Load())
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q; want POST", method)
	}
	if path != scalewayTestPath {
		t.Fatalf("path = %q; want %q", path, scalewayTestPath)
	}
	if rawQuery != "" {
		t.Fatalf("request URL carried query %q; the credential belongs only in X-Auth-Token", rawQuery)
	}
	if requestURI != scalewayTestPath {
		t.Fatalf("request URI = %q; want the exact path %q", requestURI, scalewayTestPath)
	}
	if strings.Contains(path, "/v1/") {
		t.Fatalf("path = %q; the v1 route is not served", path)
	}
	if token != scalewayTestToken {
		t.Fatal("the credential was not sent in X-Auth-Token")
	}
	if values := headers.Values("X-Auth-Token"); len(values) != 1 || values[0] != scalewayTestToken {
		t.Fatalf("X-Auth-Token values = %q; want the credential exactly once", values)
	}
	for name, values := range headers {
		if http.CanonicalHeaderKey(name) == "X-Auth-Token" {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, scalewayTestToken) {
				t.Fatalf("header %q repeated the credential outside X-Auth-Token", name)
			}
		}
	}
	if accept != "application/json" {
		t.Fatalf("Accept = %q; want application/json", accept)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", contentType)
	}
	wantBody := `{"ciphertext":"` + base64.StdEncoding.EncodeToString(blob) + `"}`
	if body != wantBody {
		t.Fatalf("body = %s; want %s", body, wantBody)
	}
	if strings.Contains(body, scalewayTestToken) {
		t.Fatal("request body contained the credential")
	}
}

// TestScalewayKMSReturnsDecodedPlaintextUnchanged proves the client decodes
// base64 and does not round, truncate or pad the decoded bytes. Length
// enforcement belongs to the shared cloud provider (proved below).
func TestScalewayKMSReturnsDecodedPlaintextUnchanged(t *testing.T) {
	cases := [][]byte{
		scalewayTestKEK(),
		bytes.Repeat([]byte{0x01}, secret.DataKeyBytes-1),
		bytes.Repeat([]byte{0x02}, secret.DataKeyBytes+1),
	}
	for _, plaintext := range cases {
		t.Run(fmt.Sprintf("length %d", len(plaintext)), func(t *testing.T) {
			reply := scalewayReply(t, plaintext)
			client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, reply)
			}))
			got, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("plaintext = %x; want the exact decoded reply %x", got, plaintext)
			}
		})
	}
}

// TestScalewayKMSMapsStatuses pins the status-to-fault table. The provider
// documents 401/403 as credential faults, 404 as not found, 429 and 5xx as an
// unavailable dependency, and every other 4xx as invalid input.
func TestScalewayKMSMapsStatuses(t *testing.T) {
	cases := []struct {
		status int
		class  fault.Class
	}{
		{http.StatusUnauthorized, fault.Unauthenticated},
		{http.StatusForbidden, fault.Forbidden},
		{http.StatusNotFound, fault.NotFound},
		{http.StatusTooManyRequests, fault.DependencyUnavailable},
		{http.StatusInternalServerError, fault.DependencyUnavailable},
		{http.StatusBadGateway, fault.DependencyUnavailable},
		{http.StatusServiceUnavailable, fault.DependencyUnavailable},
		{http.StatusBadRequest, fault.InvalidInput},
		{http.StatusMethodNotAllowed, fault.InvalidInput},
		{http.StatusConflict, fault.InvalidInput},
		{http.StatusUnprocessableEntity, fault.InvalidInput},
		{http.StatusTeapot, fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("status %d", testCase.status), func(t *testing.T) {
			// The body repeats the credential and a server message: a refusal
			// must render neither.
			client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, `{"message":"server-body-sentinel `+scalewayTestToken+`"}`)
			}))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
			requireFaultClass(t, err, testCase.class)
			if plaintext != nil {
				t.Fatalf("status %d returned %d plaintext bytes on failure", testCase.status, len(plaintext))
			}
			if text := err.Error(); strings.Contains(text, scalewayTestToken) || strings.Contains(text, "server-body-sentinel") {
				t.Fatalf("status %d error disclosed the credential or the response body: %q", testCase.status, text)
			}
		})
	}
}

// TestScalewayKMSRefusesUnexpectedStatusesWithAValidBody proves the status
// table's default arm: an unexpected 2xx or any 3xx is refused even when the
// body is a well-formed, valid decrypt reply. Accepting 201/204 or replaying a
// 302/307 body as plaintext would be a silent authority and integrity failure.
func TestScalewayKMSRefusesUnexpectedStatusesWithAValidBody(t *testing.T) {
	reply := scalewayReply(t, scalewayTestKEK())
	for _, status := range []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusFound,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
	} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			var calls atomic.Int32
			client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, reply)
			}))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
			if err == nil {
				t.Fatalf("status %d with a valid body returned %d plaintext bytes", status, len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("status %d returned %d plaintext bytes on failure", status, len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if calls.Load() != 1 {
				t.Fatalf("status %d made %d requests; want exactly 1", status, calls.Load())
			}
		})
	}
}

// TestScalewayKMSClassifiesStatusBeforeReadingTheBody proves a non-200 status
// is classified from the status line alone: a 403 carrying a body larger than
// the response bound is still a forbidden refusal, not a dependency failure
// caused by the bound.
func TestScalewayKMSClassifiesStatusBeforeReadingTheBody(t *testing.T) {
	oversize := strings.Repeat("x", 100<<10)
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, oversize)
	}))
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("a forbidden response with an oversized body returned %d plaintext bytes", len(plaintext))
	}
	if plaintext != nil {
		t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.Forbidden)
}

// TestScalewayKMSResponseBound pins the documented 64 KiB reply bound: a valid
// plaintext JSON body padded to exactly the bound is accepted, one byte more is
// refused, and the read stops at the bound instead of buffering the whole body.
func TestScalewayKMSResponseBound(t *testing.T) {
	const responseBound = 64 << 10 // the documented bound, spelled as a literal
	reply := scalewayReply(t, scalewayTestKEK())
	pad := func(total int) string {
		return reply + strings.Repeat(" ", total-len(reply))
	}

	t.Run("exactly the bound accepted", func(t *testing.T) {
		if len(pad(responseBound)) != responseBound {
			t.Fatal("fixture is not exactly the bound")
		}
		client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, pad(responseBound))
		}))
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
		if err != nil {
			t.Fatalf("a reply of exactly the bound was refused: %v", err)
		}
		if !bytes.Equal(plaintext, scalewayTestKEK()) {
			t.Fatalf("plaintext = %x; want the reply plaintext", plaintext)
		}
	})

	t.Run("one byte over refused", func(t *testing.T) {
		client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, pad(responseBound+1))
		}))
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
		if err == nil {
			t.Fatalf("a reply one byte over the bound returned %d plaintext bytes", len(plaintext))
		}
		if plaintext != nil {
			t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
		}
		requireFaultClass(t, err, fault.DependencyUnavailable)
	})
}

func TestScalewayKMSNeverFollowsRedirects(t *testing.T) {
	var finalHits atomic.Int32
	var finalToken atomic.Value
	reply := scalewayReply(t, scalewayTestKEK())
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect-target" {
			finalHits.Add(1)
			finalToken.Store(request.Header.Get("X-Auth-Token"))
			_, _ = io.WriteString(writer, reply)
			return
		}
		writer.Header().Set("Location", "/redirect-target")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))

	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("redirect response was followed and returned %d plaintext bytes", len(plaintext))
	}
	if hits := finalHits.Load(); hits != 0 {
		t.Fatalf("redirect target received %d requests; the credential must never be replayed", hits)
	}
	if token, _ := finalToken.Load().(string); token != "" {
		t.Fatal("redirect target observed the credential")
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
}

// TestScalewayKMSConfigurationRefusals pins constructor refusals: no
// downgrade to cleartext, no embedded URL credential, no ambient proxy and no
// opaque transport.
func TestScalewayKMSConfigurationRefusals(t *testing.T) {
	cases := []struct {
		name   string
		config keys.ScalewayKMSConfig
	}{
		{name: "empty secret", config: keys.ScalewayKMSConfig{BaseURL: "https://api.example.com"}},
		{name: "whitespace secret", config: keys.ScalewayKMSConfig{SecretKey: "   \t", BaseURL: "https://api.example.com"}},
		// The credential is a header value: a control character would be a
		// header injection, so construction refuses it rather than deferring
		// to net/http. Leading/trailing spaces are refused, not trimmed.
		{name: "carriage return secret", config: keys.ScalewayKMSConfig{SecretKey: "secret\rvalue", BaseURL: "https://api.example.com"}},
		{name: "line feed secret", config: keys.ScalewayKMSConfig{SecretKey: "secret\nvalue", BaseURL: "https://api.example.com"}},
		{name: "nul secret", config: keys.ScalewayKMSConfig{SecretKey: "secret\x00value", BaseURL: "https://api.example.com"}},
		{name: "tab secret", config: keys.ScalewayKMSConfig{SecretKey: "secret\tvalue", BaseURL: "https://api.example.com"}},
		{name: "delete secret", config: keys.ScalewayKMSConfig{SecretKey: "secret\x7fvalue", BaseURL: "https://api.example.com"}},
		{name: "leading space secret", config: keys.ScalewayKMSConfig{SecretKey: " secret", BaseURL: "https://api.example.com"}},
		{name: "trailing space secret", config: keys.ScalewayKMSConfig{SecretKey: "secret ", BaseURL: "https://api.example.com"}},
		{name: "plain http", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "http://api.example.com"}},
		{name: "plain http loopback", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "http://127.0.0.1:8080"}},
		{name: "no scheme", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "api.example.com"}},
		{name: "no host", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://"}},
		{name: "userinfo", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://user:url-password-sentinel@api.example.com"}},
		{name: "query", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://api.example.com?token=query-token-sentinel"}},
		{name: "forced query", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://api.example.com?"}},
		{name: "fragment", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://api.example.com#x"}},
		{name: "whitespace", config: keys.ScalewayKMSConfig{SecretKey: "k", BaseURL: "https://api.example.com /x"}},
		{name: "shared default transport", config: keys.ScalewayKMSConfig{SecretKey: "k", HTTPClient: &http.Client{Transport: http.DefaultTransport}}},
		{name: "opaque transport", config: keys.ScalewayKMSConfig{SecretKey: "k", HTTPClient: &http.Client{Transport: scalewayRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("must not be used")
		})}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, err := keys.NewScalewayKMS(testCase.config)
			if err == nil {
				t.Fatalf("configuration was accepted: %+v", testCase.config)
			}
			if client != nil {
				t.Fatal("refused configuration returned a client")
			}
			requireFaultClass(t, err, fault.InvalidInput)
			for _, sentinel := range []string{"url-password-sentinel", "query-token-sentinel"} {
				if strings.Contains(err.Error(), sentinel) {
					t.Fatalf("refusal echoed configuration material %q: %q", sentinel, err.Error())
				}
			}
		})
	}

	// The default origin is the https API and injects no request at
	// construction.
	if client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{SecretKey: "k"}); err != nil {
		t.Fatalf("NewScalewayKMS refused the default https origin: %v", err)
	} else if client == nil {
		t.Fatal("NewScalewayKMS returned a nil client for the default origin")
	}
}

// TestScalewayKMSCredentialBoundary pins the credential grammar on both sides.
// A real Scaleway IAM secret key is a UUID (8-4-4-4-12 hex with dashes); the
// validator deliberately admits every printable non-space ASCII byte, so
// uppercase hex, digits, mixed case and punctuation must all survive to the
// wire verbatim. Refusal rows keep header injection (space, control, DEL,
// non-ASCII) and the 256-byte bound closed.
func TestScalewayKMSCredentialBoundary(t *testing.T) {
	const credentialBound = 256 // the documented bound, spelled as a literal
	reply := scalewayReply(t, scalewayTestKEK())

	accepted := []struct {
		name string
		key  string
	}{
		{name: "scaleway uuid", key: "3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9"},
		{name: "all zero uuid", key: "00000000-0000-0000-0000-000000000000"},
		{name: "all f uppercase uuid", key: "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF"},
		{name: "digits only", key: "0123456789"},
		{name: "mixed case alphanumeric", key: "0123456789abcdefABCDEF"},
		{name: "punctuation", key: "a_b.c~d+e/f=g"},
		{name: "all printable punctuation", key: "!#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"},
		{name: "single byte", key: "k"},
		{name: "exactly the bound", key: strings.Repeat("a", credentialBound)},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			observed := make(chan string, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				observed <- request.Header.Get("X-Auth-Token")
				_, _ = io.WriteString(writer, reply)
			}))
			t.Cleanup(server.Close)
			client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{
				SecretKey:  testCase.key,
				BaseURL:    server.URL,
				HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("NewScalewayKMS refused %q: %v", testCase.name, err)
			}
			if _, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob")); err != nil {
				t.Fatalf("Decrypt with %q: %v", testCase.name, err)
			}
			token := <-observed
			if !bytes.Equal([]byte(token), []byte(testCase.key)) {
				t.Fatalf("X-Auth-Token = %q (%d bytes); want the configured value verbatim (%d bytes)",
					token, len(token), len(testCase.key))
			}
		})
	}

	refused := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "space in the middle", key: "secret value"},
		{name: "leading space", key: " sentinel"},
		{name: "trailing space", key: "sentinel "},
		{name: "tab", key: "secret\tvalue"},
		{name: "carriage return", key: "secret\rvalue"},
		{name: "line feed", key: "secret\nvalue"},
		{name: "nul", key: "secret\x00value"},
		{name: "delete", key: "secret\x7fvalue"},
		{name: "non ascii text", key: "secreté"},
		{name: "non ascii byte", key: "secret\x80value"},
		{name: "one byte over the bound", key: strings.Repeat("a", credentialBound+1)},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{
				SecretKey: testCase.key,
				BaseURL:   "https://api.example.com",
			})
			if err == nil {
				t.Fatalf("credential %q was accepted", testCase.name)
			}
			if client != nil {
				t.Fatal("refused credential returned a client")
			}
			requireFaultClass(t, err, fault.InvalidInput)
			if testCase.key != "" && strings.Contains(err.Error(), testCase.key) {
				t.Fatalf("refusal echoed the credential: %q", err.Error())
			}
		})
	}
}

// TestScalewayKMSVerifiesTLSByDefault proves the client-owned transport keeps
// certificate verification on: an untrusted TLS server is refused and its
// handler is never reached.
func TestScalewayKMSVerifiesTLSByDefault(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(writer, scalewayReply(t, scalewayTestKEK()))
	}))
	t.Cleanup(server.Close)

	client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{SecretKey: scalewayTestToken, BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewScalewayKMS: %v", err)
	}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("untrusted TLS server was accepted and returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if hits.Load() != 0 {
		t.Fatalf("untrusted TLS server handler was reached %d times", hits.Load())
	}
	if strings.Contains(err.Error(), scalewayTestToken) {
		t.Fatal("TLS refusal disclosed the credential")
	}
}

// TestScalewayKMSRefusesBadKeyReferences pins the scw-kms:<region>/<id>
// grammar. Both parts are request path segments, so a slash, dot traversal,
// empty part, uppercase, whitespace or foreign prefix is refused before any
// request.
func TestScalewayKMSRefusesBadKeyReferences(t *testing.T) {
	var hits atomic.Int32
	reply := scalewayReply(t, scalewayTestKEK())
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(writer, reply)
	}))

	bad := []string{
		"",
		"scw-kms:",
		"scw-kms:fr-par",
		"scw-kms:/kid",
		"scw-kms:fr-par/",
		"scw-kms:fr-par/kid/extra",
		"scw-kms://kid",
		"scw-kms:fr-par/..",
		"scw-kms:../kid",
		"scw-kms:fr-par/./kid",
		"scw-kms:../../kid",
		"scw-kms:FR-PAR/kid",
		"scw-kms:fr-par/KID",
		"scw-kms:fr par/kid",
		"scw-kms: fr-par/kid",
		"scw-kms:fr-par /kid",
		"scw-kms:fr-par/kid ",
		"scw-kms:fr-par/kid\n",
		"SCW-KMS:fr-par/kid",
		"scaleway-kms:fr-par/kid",
		"aws-kms:fr-par/kid",
		"gcp-kms:fr-par/kid",
		"ephemeral:fr-par/kid",
		"scw-kms:fr-par/kid?x=1",
		"scw-kms:fr-par/kid#frag",
		"scw-kms:fr-par/kid%2Fextra",
		"scw-kms:fr-par/kid\\extra",
		"scw-kms:fr-par/kid:x",
		"scw-kms:fr-par/kid_x",
		// Percent-escaped traversal, newline and query bytes are refused by
		// the grammar alone, before url.PathEscape could encode them again.
		"scw-kms:fr-par/%2e%2e",
		"scw-kms:%2e%2e/kid",
		"scw-kms:fr-par/%2E%2E",
		"scw-kms:fr-par/%0a",
		"scw-kms:fr%0apar/kid",
		"scw-kms:fr-par/%3f",
		"scw-kms:fr-par/kid%3fx",
	}
	for _, reference := range bad {
		plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(reference), []byte("blob"))
		if err == nil {
			t.Fatalf("reference %q was accepted and returned %d plaintext bytes", reference, len(plaintext))
		}
		requireFaultClass(t, err, fault.InvalidInput)
	}
	if hits.Load() != 0 {
		t.Fatalf("bad references made %d requests; the grammar must be checked first", hits.Load())
	}

	// Valid plain identifiers reach the request path.
	for _, reference := range []string{
		scalewayTestRef,
		"scw-kms:nl-ams/kodama",
		"scw-kms:fr-par/0",
	} {
		if _, err := client.Decrypt(context.Background(), secret.KeyRef(reference), []byte("blob")); err != nil {
			t.Fatalf("valid reference %q was refused: %v", reference, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("valid references made %d requests; want 3", hits.Load())
	}
}

// TestScalewayKMSRefusesMalformedReplies pins sanitized dependency failures for
// a malformed body, a missing or empty plaintext, a non-base64 plaintext and a
// body over the documented bound.
func TestScalewayKMSRefusesMalformedReplies(t *testing.T) {
	oversize := scalewayReply(t, bytes.Repeat([]byte{0x2a}, 96<<10))
	// The reply plaintext below encodes with '+' and '/' under StdEncoding, so
	// the standard alphabet is distinguishable from the URL-safe one.
	nonStdPlaintext := bytes.Repeat([]byte{0xfb}, secret.DataKeyBytes)
	validReply := scalewayReply(t, scalewayTestKEK())
	cases := []struct {
		name string
		body string
	}{
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
			client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, testCase.body)
			}))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
			if err == nil {
				t.Fatalf("reply was accepted and returned %d plaintext bytes", len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if text := err.Error(); strings.Contains(text, "plaintext") && strings.Contains(text, "not base64") {
				t.Fatalf("refusal echoed the response body: %q", text)
			}
		})
	}
}

// TestScalewayKMSToleratesDuplicateKeysUnknownFieldsAndTrailingWhitespace pins
// the decoder's documented behaviour instead of tightening it: encoding/json
// keeps the last value of a duplicate key, ignores unknown fields and accepts
// trailing whitespace. Trailing non-whitespace garbage is refused above.
func TestScalewayKMSToleratesDuplicateKeysUnknownFieldsAndTrailingWhitespace(t *testing.T) {
	first := bytes.Repeat([]byte{0x11}, secret.DataKeyBytes)
	last := scalewayTestKEK()
	cases := []struct {
		name string
		body string
	}{
		{name: "duplicate key is last-wins", body: `{"plaintext":"` +
			base64.StdEncoding.EncodeToString(first) + `","plaintext":"` +
			base64.StdEncoding.EncodeToString(last) + `"}`},
		{name: "unknown field ignored", body: `{"unknown":"ignored","plaintext":"` +
			base64.StdEncoding.EncodeToString(last) + `"}`},
		{name: "trailing whitespace", body: scalewayReply(t, last) + " \r\n\t"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, testCase.body)
			}))
			plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
			if err != nil {
				t.Fatalf("reply was refused: %v", err)
			}
			if !bytes.Equal(plaintext, last) {
				t.Fatalf("plaintext = %x; want %x", plaintext, last)
			}
		})
	}
}

// TestScalewayKMSTransportFailureIsClassifiedAndSanitized proves a transport
// failure is a dependency fault and that an underlying dial error carrying the
// credential is never rendered by the public error.
func TestScalewayKMSTransportFailureIsClassifiedAndSanitized(t *testing.T) {
	dialCause := errors.New("dial-refused-sentinel")
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("dial refused for credential %s: %w", scalewayTestToken, dialCause)
		},
	}
	client, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{
		SecretKey:  scalewayTestToken,
		BaseURL:    "https://api.scaleway.com",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewScalewayKMS: %v", err)
	}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob"))
	if err == nil {
		t.Fatalf("transport failure returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, dialCause) {
		t.Fatalf("transport failure dropped its private cause: %v", err)
	}
	if strings.Contains(err.Error(), scalewayTestToken) || strings.Contains(err.Error(), dialCause.Error()) {
		t.Fatalf("transport refusal disclosed the credential or the private cause: %q", err.Error())
	}
}

// TestScalewayKMSHonoursContextCancellation proves a pre-cancelled or
// in-flight cancelled context returns the context error and never succeeds.
func TestScalewayKMSHonoursContextCancellation(t *testing.T) {
	t.Run("before request", func(t *testing.T) {
		var hits atomic.Int32
		client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(writer, scalewayReply(t, scalewayTestKEK()))
		}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Decrypt(ctx, secret.KeyRef(scalewayTestRef), []byte("blob"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled Decrypt error = %v; want context.Canceled", err)
		}
		if class, ok := fault.ClassOf(err); ok {
			t.Fatalf("pre-cancelled Decrypt carried fault class %q; want the bare context error", class)
		}
		// Cancellation is checked before the key-reference grammar: a cancelled
		// caller learns the call was cancelled, not that its reference was bad.
		if _, err := client.Decrypt(ctx, secret.KeyRef("not-a-scaleway-reference"), []byte("blob")); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled Decrypt with a foreign reference error = %v; want context.Canceled checked first", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("pre-cancelled request reached the server %d times", hits.Load())
		}
	})

	t.Run("during request", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		// The deferred close unblocks the handler before the server cleanup
		// waits for it, including on the failure paths.
		defer close(release)
		var once atomic.Bool
		client, _ := scalewayTLSClient(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			select {
			case <-request.Context().Done():
			case <-release:
			}
		}))
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := client.Decrypt(ctx, secret.KeyRef(scalewayTestRef), []byte("blob"))
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
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Decrypt error = %v; want context.Canceled", err)
			}
			if class, ok := fault.ClassOf(err); ok {
				t.Fatalf("cancelled Decrypt carried fault class %q; want the bare context error", class)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled Decrypt did not return")
		}
	})
}

// TestScalewayKMSDoesNotCacheReplies proves every Decrypt performs its own
// request; caching belongs to the shared cloud provider, not the client.
func TestScalewayKMSDoesNotCacheReplies(t *testing.T) {
	var hits atomic.Int32
	reply := scalewayReply(t, scalewayTestKEK())
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(writer, reply)
	}))
	for attempt := range 3 {
		if _, err := client.Decrypt(context.Background(), secret.KeyRef(scalewayTestRef), []byte("blob")); err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("Decrypt made %d requests; want one per call", hits.Load())
	}
}

// TestScalewayProviderEnforcesKEKLengthAndDoesNotCacheFailure drives the real
// Scaleway client through the shared cloud provider. A 31-byte reply is an
// invalid_input refusal (the provider owns the 32-byte rule) and is not cached;
// the next call retries the decrypt and succeeds with 32 bytes.
func TestScalewayProviderEnforcesKEKLengthAndDoesNotCacheFailure(t *testing.T) {
	var calls atomic.Int32
	reply := scalewayReply(t, scalewayTestKEK())
	shortReply := scalewayReply(t, bytes.Repeat([]byte{0x33}, secret.DataKeyBytes-1))
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(writer, shortReply)
			return
		}
		_, _ = io.WriteString(writer, reply)
	}))

	provider, err := keys.NewScaleway(client, keys.CloudConfig{
		KeyRef:   secret.KeyRef(scalewayTestRef),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: []byte("wrapped-kek-v1")},
	}, deterministicRandom(0x51))
	if err != nil {
		t.Fatalf("NewScaleway: %v", err)
	}
	request := secret.WrapRequest{
		KeyRef:         secret.KeyRef(scalewayTestRef),
		KeyVersion:     1,
		Plaintext:      testDEK(),
		AssociatedData: testAssociatedBytes(t, testAssociated(1)),
	}
	_, err = provider.Wrap(context.Background(), request)
	requireFaultClass(t, err, fault.InvalidInput)
	if calls.Load() != 1 {
		t.Fatalf("first Wrap made %d requests; want 1", calls.Load())
	}
	if _, err := provider.Wrap(context.Background(), request); err != nil {
		t.Fatalf("second Wrap after a short KEK: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("KMS requests = %d; want 2 (a short KEK must not be cached)", calls.Load())
	}
}

// TestScalewayProviderKeepsDeniedDecryptUncached proves a denied decrypt is
// classified as the credential fault and retried rather than cached.
func TestScalewayProviderKeepsDeniedDecryptUncached(t *testing.T) {
	var calls atomic.Int32
	reply := scalewayReply(t, scalewayTestKEK())
	client, _ := scalewayTLSClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(writer, `{"message":"denied"}`)
			return
		}
		_, _ = io.WriteString(writer, reply)
	}))
	provider, err := keys.NewScaleway(client, keys.CloudConfig{
		KeyRef:   secret.KeyRef(scalewayTestRef),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: []byte("wrapped-kek-v1")},
	}, deterministicRandom(0x52))
	if err != nil {
		t.Fatalf("NewScaleway: %v", err)
	}
	request := secret.WrapRequest{
		KeyRef:         secret.KeyRef(scalewayTestRef),
		KeyVersion:     1,
		Plaintext:      testDEK(),
		AssociatedData: testAssociatedBytes(t, testAssociated(1)),
	}
	_, err = provider.Wrap(context.Background(), request)
	requireFaultClass(t, err, fault.Forbidden)
	if _, err := provider.Wrap(context.Background(), request); err != nil {
		t.Fatalf("second Wrap after denial: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("KMS requests = %d; want 2 (a denial must not be cached)", calls.Load())
	}
}
