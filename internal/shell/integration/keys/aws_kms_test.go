package keys_test

// The AWS KMS decrypt client is plain net/http: one unsigned form POST to STS
// (AssumeRoleWithWebIdentity, the EKS IRSA exchange) and one SigV4-signed JSON
// POST to KMS (TrentService.Decrypt). These tests pin both request shapes, the
// signature (verified by an independent implementation in the fake KMS), the
// region-before-credential ordering, the token-file and credential-cache
// lifecycle, the single retry on refused credentials, the error tables, the
// credential and body hygiene of every error, the redirect/TLS/transport
// boundary, the response bounds and the interaction with the shared cloud
// provider.
//
// Every server is a loopback httptest TLS server, the clock is injected and
// the token is a temporary file; no test contacts a real cloud endpoint.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	awsTestRegion = "eu-west-3"
	// awsTestKeyARN is the key ARN form: the reference names its own region.
	awsTestKeyARN = "arn:aws:kms:eu-west-3:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	awsTestRef    = "aws-kms:" + awsTestKeyARN
	// The alias forms: a bare alias name depends on the configured region, an
	// alias ARN names its own.
	awsTestAliasName = "alias/kodama-kek"
	awsTestAliasARN  = "arn:aws:kms:eu-west-3:111122223333:alias/kodama-kek"
	awsTestRoleARN   = "arn:aws:iam::111122223333:role/kodama-kms"
	// awsTestToken stands in for the projected service-account JWT.
	awsTestToken = "eyJhbGciOiJSUzI1NiJ9.web-identity-token-sentinel.c2lnbmF0dXJl"
	// awsTestBodySentinel is written into every refusal body: no error may
	// render it.
	awsTestBodySentinel = "server-body-sentinel"
)

// awsTestBlob is the wrapped key the tests send. It holds bytes that are not
// valid UTF-8 and encodes with '+' and '/' under the standard alphabet.
func awsTestBlob() []byte {
	return []byte("aws-wrapped-kek-blob-sentinel\x00\xfb\xff\xfe")
}

// awsTestKEK is the 32-byte key-encryption key the reply unwraps to.
func awsTestKEK() []byte {
	return bytes.Repeat([]byte{0x6b}, secret.DataKeyBytes)
}

// awsTestClock is the injected clock. The client reads it for signing and for
// credential expiry; the fake STS reads it to date the credentials it issues.
type awsTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *awsTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *awsTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// awsTestCall is one request as a fake endpoint received it.
type awsTestCall struct {
	method     string
	path       string
	requestURI string
	rawQuery   string
	host       string
	header     http.Header
	body       string
}

// awsTestCredentials is one set of role credentials the fake STS issued.
type awsTestCredentials struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
}

// awsTestOptions adjusts one fixture. The reply hooks run on a handler
// goroutine: they never touch testing.T, receive the 1-based call number and
// return false to fall through to the default reply.
type awsTestOptions struct {
	sts       func(call int, writer http.ResponseWriter, request *http.Request) bool
	kms       func(call int, writer http.ResponseWriter, request *http.Request) bool
	plaintext []byte
	lifetime  time.Duration
	configure func(config *keys.AWSKMSConfig)
}

// awsTestFixture is a loopback STS, a loopback KMS, a token file and a clock,
// wired to one client.
type awsTestFixture struct {
	options   awsTestOptions
	sts       *httptest.Server
	kms       *httptest.Server
	tokenFile string
	clock     *awsTestClock
	client    *keys.AWSKMS
	plaintext []byte

	mu                sync.Mutex
	tokens            []string
	stsCalls          []awsTestCall
	kmsCalls          []awsTestCall
	issued            []awsTestCredentials
	signatureFailures []string
}

func newAWSTestFixture(t *testing.T, options awsTestOptions) *awsTestFixture {
	t.Helper()
	fixture := &awsTestFixture{
		options:   options,
		tokenFile: filepath.Join(t.TempDir(), "token"),
		clock:     &awsTestClock{now: time.Date(2026, time.November, 11, 9, 30, 0, 0, time.UTC)},
		plaintext: options.plaintext,
	}
	if fixture.plaintext == nil {
		fixture.plaintext = awsTestKEK()
	}
	if fixture.options.lifetime == 0 {
		fixture.options.lifetime = time.Hour
	}
	fixture.writeToken(t, awsTestToken+"\n")
	fixture.sts = httptest.NewTLSServer(http.HandlerFunc(fixture.serveSTS))
	t.Cleanup(fixture.sts.Close)
	fixture.kms = httptest.NewTLSServer(http.HandlerFunc(fixture.serveKMS))
	t.Cleanup(fixture.kms.Close)

	// NewAWSKMS must own the transport (clear any proxy, refuse redirects)
	// without losing the test's explicit TLS roots.
	roots := x509.NewCertPool()
	roots.AddCert(fixture.sts.Certificate())
	roots.AddCert(fixture.kms.Certificate())
	config := keys.AWSKMSConfig{
		Region:               awsTestRegion,
		RoleARN:              awsTestRoleARN,
		WebIdentityTokenFile: fixture.tokenFile,
		KMSEndpoint:          fixture.kms.URL,
		STSEndpoint:          fixture.sts.URL,
		HTTPClient:           &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}},
		Now:                  fixture.clock.Now,
	}
	if options.configure != nil {
		options.configure(&config)
	}
	client, err := keys.NewAWSKMS(config)
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	fixture.client = client
	// Every request that reached the fake KMS must have carried a signature
	// the independent verifier accepts, in every test.
	t.Cleanup(func() {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		for _, failure := range fixture.signatureFailures {
			t.Errorf("fake KMS refused a request signature: %s", failure)
		}
	})
	return fixture
}

// writeToken replaces the token file, as the kubelet does on rotation.
func (fixture *awsTestFixture) writeToken(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(fixture.tokenFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if token := strings.TrimSpace(content); token != "" {
		fixture.tokens = append(fixture.tokens, token)
	}
}

func awsTestRecord(request *http.Request, body []byte) awsTestCall {
	return awsTestCall{
		method:     request.Method,
		path:       request.URL.Path,
		requestURI: request.RequestURI,
		rawQuery:   request.URL.RawQuery,
		host:       request.Host,
		header:     request.Header.Clone(),
		body:       string(body),
	}
}

// awsTestSTSReply renders the documented AssumeRoleWithWebIdentity answer, in
// the element order and with the sibling elements of the API reference sample.
func awsTestSTSReply(credentials awsTestCredentials, expiration string) string {
	return `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult>
    <SubjectFromWebIdentityToken>system:serviceaccount:kodama:kodama</SubjectFromWebIdentityToken>
    <Audience>sts.amazonaws.com</Audience>
    <AssumedRoleUser>
      <Arn>arn:aws:sts::111122223333:assumed-role/kodama-kms/kodama</Arn>
      <AssumedRoleId>AROACLKWSDQRAOEXAMPLE:kodama</AssumedRoleId>
    </AssumedRoleUser>
    <Credentials>
      <SessionToken>` + credentials.sessionToken + `</SessionToken>
      <SecretAccessKey>` + credentials.secretAccessKey + `</SecretAccessKey>
      <Expiration>` + expiration + `</Expiration>
      <AccessKeyId>` + credentials.accessKeyID + `</AccessKeyId>
    </Credentials>
    <Provider>arn:aws:iam::111122223333:oidc-provider/oidc.eks.eu-west-3.amazonaws.com/id/EXAMPLE</Provider>
  </AssumeRoleWithWebIdentityResult>
  <ResponseMetadata>
    <RequestId>ad4156e9-bce1-11e2-82e6-6b6efEXAMPLE</RequestId>
  </ResponseMetadata>
</AssumeRoleWithWebIdentityResponse>
`
}

// issue mints the next credentials. Each exchange gets its own values so a
// test can tell which exchange signed a request.
func (fixture *awsTestFixture) issue() awsTestCredentials {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	number := len(fixture.issued) + 1
	credentials := awsTestCredentials{
		accessKeyID:     fmt.Sprintf("ASIATESTACCESSKEY%04d", number),
		secretAccessKey: fmt.Sprintf("test-secret-access-key-sentinel-%04d/+=", number),
		sessionToken:    fmt.Sprintf("test-session-token-sentinel-%04d//++==", number),
	}
	fixture.issued = append(fixture.issued, credentials)
	return credentials
}

func (fixture *awsTestFixture) serveSTS(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	fixture.mu.Lock()
	fixture.stsCalls = append(fixture.stsCalls, awsTestRecord(request, body))
	call := len(fixture.stsCalls)
	fixture.mu.Unlock()
	if fixture.options.sts != nil && fixture.options.sts(call, writer, request) {
		return
	}
	expiration := fixture.clock.Now().Add(fixture.options.lifetime).UTC().Format(time.RFC3339)
	writer.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(writer, awsTestSTSReply(fixture.issue(), expiration))
}

func (fixture *awsTestFixture) serveKMS(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	fixture.mu.Lock()
	fixture.kmsCalls = append(fixture.kmsCalls, awsTestRecord(request, body))
	call := len(fixture.kmsCalls)
	issued := slices.Clone(fixture.issued)
	fixture.mu.Unlock()

	if err := awsTestVerifySignature(request, body, awsTestRegion, "kms", func(accessKeyID string) (awsTestCredentials, bool) {
		for _, credentials := range issued {
			if credentials.accessKeyID == accessKeyID {
				return credentials, true
			}
		}
		return awsTestCredentials{}, false
	}); err != nil {
		fixture.mu.Lock()
		fixture.signatureFailures = append(fixture.signatureFailures, fmt.Sprintf("call %d: %v", call, err))
		fixture.mu.Unlock()
		writer.Header().Set("X-Amzn-Errortype", "InvalidSignatureException")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if fixture.options.kms != nil && fixture.options.kms(call, writer, request) {
		return
	}
	// KMS reports the key ARN it used: the requested one, or the key behind
	// an alias.
	var decoded struct {
		KeyID string `json:"KeyId"`
	}
	_ = json.Unmarshal(body, &decoded)
	keyARN := awsTestKeyARN
	if strings.Contains(decoded.KeyID, ":key/") {
		keyARN = decoded.KeyID
	}
	writer.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_, _ = io.WriteString(writer, awsTestKMSReply(keyARN, fixture.plaintext))
}

// awsTestKMSReply renders the documented Decrypt answer.
func awsTestKMSReply(keyARN string, plaintext []byte) string {
	return `{"KeyId":"` + keyARN + `","KeyMaterialId":"0b7fd7ddbac6eef27907413567cad8c810e2883dc8a7534067a82ee1142fc1e6","Plaintext":"` +
		base64.StdEncoding.EncodeToString(plaintext) + `","EncryptionAlgorithm":"SYMMETRIC_DEFAULT"}`
}

// awsTestVerifySignature is an independent SigV4 check, written against the
// AWS signing documentation and sharing no code with the client. It rebuilds
// the canonical request from the request as the server received it and
// recomputes the signature with the secret the fake STS issued. It supports
// what the client may send: a path of unreserved bytes and no query.
func awsTestVerifySignature(request *http.Request, body []byte, region, service string, lookup func(string) (awsTestCredentials, bool)) error {
	authorization := request.Header.Get("Authorization")
	fields, ok := strings.CutPrefix(authorization, "AWS4-HMAC-SHA256 ")
	if !ok {
		return errors.New("authorization is not AWS4-HMAC-SHA256")
	}
	parts := strings.Split(fields, ", ")
	if len(parts) != 3 {
		return fmt.Errorf("authorization has %d parts; want 3", len(parts))
	}
	scopeValue, okCredential := strings.CutPrefix(parts[0], "Credential=")
	signedHeaders, okHeaders := strings.CutPrefix(parts[1], "SignedHeaders=")
	signature, okSignature := strings.CutPrefix(parts[2], "Signature=")
	if !okCredential || !okHeaders || !okSignature {
		return errors.New("authorization parts are not Credential, SignedHeaders, Signature")
	}
	scope := strings.Split(scopeValue, "/")
	if len(scope) != 5 || scope[2] != region || scope[3] != service || scope[4] != "aws4_request" {
		return fmt.Errorf("credential scope is not <key>/<date>/%s/%s/aws4_request", region, service)
	}
	credentials, known := lookup(scope[0])
	if !known {
		return errors.New("access key id was not issued by the fake STS")
	}
	if credentials.sessionToken != "" && request.Header.Get("X-Amz-Security-Token") != credentials.sessionToken {
		return errors.New("session token does not belong to the access key id")
	}
	timestamp := request.Header.Get("X-Amz-Date")
	if len(timestamp) != len("20060102T150405Z") || !strings.HasPrefix(timestamp, scope[1]+"T") {
		return errors.New("x-amz-date does not match the credential scope date")
	}
	if request.URL.RawQuery != "" || strings.ContainsAny(request.URL.EscapedPath(), "%+ ") {
		return errors.New("verifier supports only an unreserved path and no query")
	}
	names := strings.Split(signedHeaders, ";")
	if !slices.IsSorted(names) {
		return errors.New("signed headers are not sorted")
	}
	canonical := request.Method + "\n" + request.URL.EscapedPath() + "\n\n"
	for _, name := range names {
		value := request.Host
		if name != "host" {
			values := request.Header.Values(name)
			if len(values) != 1 {
				return fmt.Errorf("signed header %q has %d values; want 1", name, len(values))
			}
			value = strings.TrimSpace(values[0])
		}
		canonical += name + ":" + value + "\n"
	}
	payloadHash := sha256.Sum256(body)
	canonical += "\n" + signedHeaders + "\n" + hex.EncodeToString(payloadHash[:])
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n" + timestamp + "\n" + strings.Join(scope[1:], "/") + "\n" + hex.EncodeToString(canonicalHash[:])
	key := []byte("AWS4" + credentials.secretAccessKey)
	for _, step := range []string{scope[1], region, service, "aws4_request", stringToSign} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(step))
		key = mac.Sum(nil)
	}
	if want := hex.EncodeToString(key); !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("signature does not match the recomputed signature")
	}
	return nil
}

func (fixture *awsTestFixture) stsCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return len(fixture.stsCalls)
}

func (fixture *awsTestFixture) kmsCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return len(fixture.kmsCalls)
}

func (fixture *awsTestFixture) stsCall(t *testing.T, number int) awsTestCall {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if number < 1 || number > len(fixture.stsCalls) {
		t.Fatalf("STS received %d requests; want at least %d", len(fixture.stsCalls), number)
	}
	return fixture.stsCalls[number-1]
}

func (fixture *awsTestFixture) kmsCall(t *testing.T, number int) awsTestCall {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if number < 1 || number > len(fixture.kmsCalls) {
		t.Fatalf("KMS received %d requests; want at least %d", len(fixture.kmsCalls), number)
	}
	return fixture.kmsCalls[number-1]
}

func (fixture *awsTestFixture) requireCounts(t *testing.T, sts, kms int) {
	t.Helper()
	if got := fixture.stsCount(); got != sts {
		t.Fatalf("STS exchanges = %d; want %d", got, sts)
	}
	if got := fixture.kmsCount(); got != kms {
		t.Fatalf("KMS requests = %d; want %d", got, kms)
	}
}

func (fixture *awsTestFixture) decrypt(reference string) ([]byte, error) {
	return fixture.client.Decrypt(context.Background(), secret.KeyRef(reference), awsTestBlob())
}

// awsTestForm decodes a recorded STS form body.
func awsTestForm(t *testing.T, call awsTestCall) url.Values {
	t.Helper()
	form, err := url.ParseQuery(call.body)
	if err != nil {
		t.Fatalf("STS body is not a form: %v", err)
	}
	return form
}

// awsTestSignature extracts the request signature from an Authorization value.
func awsTestSignature(authorization string) string {
	_, signature, _ := strings.Cut(authorization, "Signature=")
	return signature
}

// requireCleanError proves an error renders nothing sensitive: no web identity
// token, access key id, secret access key, session token, request signature,
// ciphertext, plaintext, response body, endpoint or token-file path.
func (fixture *awsTestFixture) requireCleanError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want a refusal")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	sensitive := map[string][]string{
		"the web identity token": fixture.tokens,
		"the response body":      {awsTestBodySentinel},
		"the token file path":    {fixture.tokenFile},
		"an endpoint": {
			fixture.sts.URL, fixture.kms.URL,
			strings.TrimPrefix(fixture.sts.URL, "https://"), strings.TrimPrefix(fixture.kms.URL, "https://"),
		},
		"the ciphertext": {string(awsTestBlob()), base64.StdEncoding.EncodeToString(awsTestBlob())},
		"the plaintext": {
			string(fixture.plaintext), base64.StdEncoding.EncodeToString(fixture.plaintext), hex.EncodeToString(fixture.plaintext),
		},
	}
	for _, credentials := range fixture.issued {
		sensitive["an access key id"] = append(sensitive["an access key id"], credentials.accessKeyID)
		sensitive["a secret access key"] = append(sensitive["a secret access key"], credentials.secretAccessKey)
		sensitive["a session token"] = append(sensitive["a session token"], credentials.sessionToken)
	}
	for _, call := range fixture.kmsCalls {
		sensitive["a request signature"] = append(sensitive["a request signature"], awsTestSignature(call.header.Get("Authorization")))
	}
	text := err.Error()
	for label, values := range sensitive {
		for _, value := range values {
			if value != "" && strings.Contains(text, value) {
				t.Fatalf("error disclosed %s: %q", label, text)
			}
		}
	}
}

// awsTestRoundTripFunc is an opaque RoundTripper used to prove the constructor
// refuses a transport it cannot make proxy-free and redirect-free.
type awsTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip awsTestRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// awsTestRefusal writes one AWS JSON refusal.
func awsTestRefusal(writer http.ResponseWriter, status int, header, body string) {
	if header != "" {
		writer.Header().Set("x-amzn-ErrorType", header)
	}
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

// TestAWSTestVerifierAcceptsThePublishedSessionTokenVector anchors the fake
// KMS's independent signature check to a published vector, so "the signature
// verifies" below means "verifies as AWS specifies". The request and signature
// are the aws-sig-v4-test-suite case post-sts-header-before (see
// aws_sigv4_internal_test.go for the source), which signs host, x-amz-date and
// x-amz-security-token.
func TestAWSTestVerifierAcceptsThePublishedSessionTokenVector(t *testing.T) {
	const (
		token         = "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA=="
		authorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead"
	)
	lookup := func(accessKeyID string) (awsTestCredentials, bool) {
		return awsTestCredentials{
			accessKeyID:     "AKIDEXAMPLE",
			secretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
			sessionToken:    token,
		}, accessKeyID == "AKIDEXAMPLE"
	}
	build := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
		request.Header.Set("X-Amz-Date", "20150830T123600Z")
		request.Header.Set("X-Amz-Security-Token", token)
		request.Header.Set("Authorization", authorization)
		return request
	}
	if err := awsTestVerifySignature(build(), nil, "us-east-1", "service", lookup); err != nil {
		t.Fatalf("verifier refused the published vector: %v", err)
	}

	// A changed date, a changed body or another region no longer verifies.
	tampered := build()
	tampered.Header.Set("X-Amz-Date", "20150830T123601Z")
	if err := awsTestVerifySignature(tampered, nil, "us-east-1", "service", lookup); err == nil {
		t.Fatal("verifier accepted a changed x-amz-date")
	}
	if err := awsTestVerifySignature(build(), []byte("x"), "us-east-1", "service", lookup); err == nil {
		t.Fatal("verifier accepted a changed body")
	}
	if err := awsTestVerifySignature(build(), nil, "us-east-2", "service", lookup); err == nil {
		t.Fatal("verifier accepted another region")
	}
}

// TestAWSKMSDecryptRequestAndResult pins the exact wire shape of both requests:
// the unsigned AssumeRoleWithWebIdentity form POST and the signed
// TrentService.Decrypt JSON POST, and the decoded reply.
func TestAWSKMSDecryptRequestAndResult(t *testing.T) {
	fixture := newAWSTestFixture(t, awsTestOptions{})

	got, err := fixture.decrypt(awsTestRef)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, awsTestKEK()) {
		t.Fatalf("plaintext = %x; want %x", got, awsTestKEK())
	}
	fixture.requireCounts(t, 1, 1)

	// The exchange: one unsigned form POST carrying the token in its body.
	sts := fixture.stsCall(t, 1)
	if sts.method != http.MethodPost || sts.path != "/" || sts.requestURI != "/" || sts.rawQuery != "" {
		t.Fatalf("STS request = %s %q (query %q); want POST / with no query", sts.method, sts.requestURI, sts.rawQuery)
	}
	if got := sts.header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("STS Content-Type = %q; want application/x-www-form-urlencoded", got)
	}
	for name := range sts.header {
		if name == "Authorization" || strings.HasPrefix(name, "X-Amz-") {
			t.Fatalf("STS request carried %q; the exchange is unsigned", name)
		}
	}
	wantForm := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {awsTestRoleARN},
		"RoleSessionName":  {"kodama"},
		"WebIdentityToken": {awsTestToken},
	}
	if form := awsTestForm(t, sts); fmt.Sprint(form) != fmt.Sprint(wantForm) {
		t.Fatalf("STS form = %v; want %v", form, wantForm)
	}
	if sts.body != wantForm.Encode() {
		t.Fatalf("STS body = %q; want %q", sts.body, wantForm.Encode())
	}
	for name, values := range sts.header {
		for _, value := range values {
			if strings.Contains(value, awsTestToken) {
				t.Fatalf("STS header %q carried the web identity token", name)
			}
		}
	}

	// The decrypt: one signed JSON POST.
	fixture.mu.Lock()
	issued := fixture.issued[0]
	fixture.mu.Unlock()
	kms := fixture.kmsCall(t, 1)
	if kms.method != http.MethodPost || kms.path != "/" || kms.requestURI != "/" || kms.rawQuery != "" {
		t.Fatalf("KMS request = %s %q (query %q); want POST / with no query", kms.method, kms.requestURI, kms.rawQuery)
	}
	if kms.host != strings.TrimPrefix(fixture.kms.URL, "https://") {
		t.Fatalf("KMS host = %q; want the endpoint host", kms.host)
	}
	wantHeaders := map[string]string{
		"Content-Type":         "application/x-amz-json-1.1",
		"X-Amz-Target":         "TrentService.Decrypt",
		"X-Amz-Date":           "20261111T093000Z",
		"X-Amz-Security-Token": issued.sessionToken,
		"Authorization": "AWS4-HMAC-SHA256 Credential=" + issued.accessKeyID + "/20261111/eu-west-3/kms/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-amz-target, " +
			"Signature=" + awsTestSignature(kms.header.Get("Authorization")),
	}
	for name, want := range wantHeaders {
		if values := kms.header.Values(name); len(values) != 1 || values[0] != want {
			t.Fatalf("KMS header %s = %q; want exactly %q", name, values, want)
		}
	}
	if signature := awsTestSignature(kms.header.Get("Authorization")); len(signature) != 64 || strings.Trim(signature, "0123456789abcdef") != "" {
		t.Fatalf("KMS signature = %q; want 64 lowercase hex digits", signature)
	}
	wantBody := `{"CiphertextBlob":"` + base64.StdEncoding.EncodeToString(awsTestBlob()) + `","KeyId":"` + awsTestKeyARN + `"}`
	if kms.body != wantBody {
		t.Fatalf("KMS body = %s; want %s", kms.body, wantBody)
	}
	// Neither the long-lived token nor the signing secret reaches KMS.
	for name, values := range kms.header {
		for _, value := range values {
			if strings.Contains(value, awsTestToken) || strings.Contains(value, issued.secretAccessKey) {
				t.Fatalf("KMS header %q carried the web identity token or the secret access key", name)
			}
		}
	}
	if strings.Contains(kms.body, awsTestToken) || strings.Contains(kms.body, issued.secretAccessKey) || strings.Contains(kms.body, issued.sessionToken) {
		t.Fatal("KMS body carried a credential")
	}
}

// TestAWSKMSSignatureIsVerifiedByTheFakeKMS proves the fixture's signature
// check is live: a client whose signing clock disagrees with the date it sends
// cannot exist, so the control instead breaks the secret. The fake STS hands
// out one secret and the fake KMS is told another; the request is refused as a
// signature failure and the client retries exactly once.
func TestAWSKMSSignatureIsVerifiedByTheFakeKMS(t *testing.T) {
	var fixture *awsTestFixture
	fixture = newAWSTestFixture(t, awsTestOptions{
		sts: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
			issued := fixture.issue()
			// The client signs with a secret the fake KMS does not hold.
			issued.secretAccessKey = "a-different-secret-access-key"
			_, _ = io.WriteString(writer, awsTestSTSReply(issued, fixture.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
			return true
		},
	})
	_, err := fixture.decrypt(awsTestRef)
	requireFaultClass(t, err, fault.Unauthenticated)
	fixture.requireCounts(t, 2, 2)

	fixture.mu.Lock()
	failures := len(fixture.signatureFailures)
	// This test wants the refusals; clear them so the fixture cleanup, which
	// fails a test on any signature refusal, stays quiet.
	fixture.signatureFailures = nil
	fixture.mu.Unlock()
	if failures != 2 {
		t.Fatalf("fake KMS refused %d signatures; want both requests refused", failures)
	}
}

// TestAWSKMSKeyIDForms pins the KeyId sent for each accepted reference form. It
// is always the validated reference itself, so KMS refuses a ciphertext that
// was wrapped under any other key.
func TestAWSKMSKeyIDForms(t *testing.T) {
	cases := []struct {
		name      string
		reference string
		keyID     string
	}{
		{name: "key arn", reference: awsTestRef, keyID: awsTestKeyARN},
		{name: "multi-region key arn", reference: "aws-kms:arn:aws:kms:eu-west-3:111122223333:key/mrk-1234abcd12ab34cd56ef1234567890ab",
			keyID: "arn:aws:kms:eu-west-3:111122223333:key/mrk-1234abcd12ab34cd56ef1234567890ab"},
		{name: "alias name", reference: "aws-kms:" + awsTestAliasName, keyID: awsTestAliasName},
		{name: "alias arn", reference: "aws-kms:" + awsTestAliasARN, keyID: awsTestAliasARN},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{})
			plaintext, err := fixture.decrypt(testCase.reference)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(plaintext, awsTestKEK()) {
				t.Fatalf("plaintext = %x; want the reply plaintext", plaintext)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(fixture.kmsCall(t, 1).body), &body); err != nil {
				t.Fatalf("KMS body is not a JSON object of strings: %v", err)
			}
			want := map[string]string{
				"CiphertextBlob": base64.StdEncoding.EncodeToString(awsTestBlob()),
				"KeyId":          testCase.keyID,
			}
			if fmt.Sprint(body) != fmt.Sprint(want) {
				t.Fatalf("KMS body = %v; want exactly %v", body, want)
			}
		})
	}
}

// TestAWSKMSReturnsDecodedPlaintextUnchanged proves the client decodes base64
// and does not round, truncate or pad the decoded bytes. Length enforcement
// belongs to the shared cloud provider (proved below).
func TestAWSKMSReturnsDecodedPlaintextUnchanged(t *testing.T) {
	for _, plaintext := range [][]byte{
		awsTestKEK(),
		bytes.Repeat([]byte{0x01}, secret.DataKeyBytes-1),
		bytes.Repeat([]byte{0xfb}, secret.DataKeyBytes+1),
	} {
		t.Run(fmt.Sprintf("length %d", len(plaintext)), func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{plaintext: plaintext})
			got, err := fixture.decrypt(awsTestRef)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("plaintext = %x; want the exact decoded reply %x", got, plaintext)
			}
		})
	}
}

// TestAWSKMSRefusesReferencesBeforeAnyIO pins the ordering: the reference
// grammar and the configured region are settled before the token file is read,
// before any STS exchange and before any KMS request. The token file does not
// exist, so a read attempted first would surface as a token-file fault instead
// of the reference refusal; aws_kms_internal_test.go counts the reads directly.
func TestAWSKMSRefusesReferencesBeforeAnyIO(t *testing.T) {
	fixture := newAWSTestFixture(t, awsTestOptions{})
	if err := os.Remove(fixture.tokenFile); err != nil {
		t.Fatalf("remove token file: %v", err)
	}
	refused := []string{
		// A well-formed reference for another region.
		"aws-kms:arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:111122223333:alias/kodama-kek",
		"aws-kms:arn:aws-cn:kms:cn-north-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		// An alias that breaks the alias rules.
		"aws-kms:alias/",
		"aws-kms:alias/has space",
		"aws-kms:alias/alias/nested",
		"aws-kms:alias/trailing/",
		"aws-kms:alias/double//slash",
		"aws-kms:alias/" + strings.Repeat("a", 251),
		"aws-kms:arn:aws:kms:eu-west-3:111122223333:alias/has.dot",
		// Not an AWS KMS reference at all.
		"",
		"aws-kms:",
		"aws-kms:1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-3:111122223333:key/not-a-key-id",
		"aws-kms:arn:aws:kms:eu-west-3:11112222333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:s3:eu-west-3:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-3:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab\n",
		"aws-kms:arn:aws:kms:eu-west-3:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab?x=1",
		"AWS-KMS:" + awsTestKeyARN,
		"gcp-kms:projects/p/locations/l/keyRings/r/cryptoKeys/k",
		"scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
		awsTestKeyARN,
	}
	for _, reference := range refused {
		plaintext, err := fixture.decrypt(reference)
		if err == nil {
			t.Fatalf("reference %q was accepted and returned %d plaintext bytes", reference, len(plaintext))
		}
		requireFaultClass(t, err, fault.InvalidInput)
		if strings.Contains(err.Error(), "AWS_WEB_IDENTITY_TOKEN_FILE") {
			t.Fatalf("reference %q was refused by a token-file read, not by the reference check: %q", reference, err.Error())
		}
	}
	fixture.requireCounts(t, 0, 0)

	// The same client still serves a reference for its own region once the
	// token is back.
	fixture.writeToken(t, awsTestToken)
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("valid reference was refused: %v", err)
	}
	fixture.requireCounts(t, 1, 1)
}

// TestAWSKMSRereadsTheTokenFileOnEveryExchange proves the projected token is
// never cached: after the kubelet rotates the file, the next exchange presents
// the new content.
func TestAWSKMSRereadsTheTokenFileOnEveryExchange(t *testing.T) {
	fixture := newAWSTestFixture(t, awsTestOptions{})
	const rotated = "eyJhbGciOiJSUzI1NiJ9.rotated-token-sentinel.c2lnbmF0dXJl"

	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("first Decrypt: %v", err)
	}
	fixture.writeToken(t, rotated+"\n")
	// The credentials are still fresh: no exchange, so no read is observable.
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("second Decrypt: %v", err)
	}
	fixture.requireCounts(t, 1, 2)

	fixture.clock.Advance(time.Hour)
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt after expiry: %v", err)
	}
	fixture.requireCounts(t, 2, 3)
	if got := awsTestForm(t, fixture.stsCall(t, 1)).Get("WebIdentityToken"); got != awsTestToken {
		t.Fatalf("first exchange token = %q; want the original file content", got)
	}
	if got := awsTestForm(t, fixture.stsCall(t, 2)).Get("WebIdentityToken"); got != rotated {
		t.Fatalf("second exchange token = %q; want the rotated file content", got)
	}
}

// TestAWSKMSCachesCredentialsUntilTheExpiryMargin pins the cache: many decrypts
// share one exchange, the credentials are used up to but not into the last five
// minutes before their expiration, and the refreshed credentials sign the
// following requests.
func TestAWSKMSCachesCredentialsUntilTheExpiryMargin(t *testing.T) {
	const margin = 5 * time.Minute // the documented margin, spelled as a literal
	fixture := newAWSTestFixture(t, awsTestOptions{lifetime: time.Hour})

	for attempt := range 5 {
		if _, err := fixture.decrypt(awsTestRef); err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
	}
	fixture.requireCounts(t, 1, 5)

	// One second before the margin the cached credentials still sign.
	fixture.clock.Advance(time.Hour - margin - time.Second)
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt before the margin: %v", err)
	}
	fixture.requireCounts(t, 1, 6)

	// At the margin they are replaced.
	fixture.clock.Advance(time.Second)
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt at the margin: %v", err)
	}
	fixture.requireCounts(t, 2, 7)

	fixture.mu.Lock()
	first, second := fixture.issued[0], fixture.issued[1]
	fixture.mu.Unlock()
	if got := fixture.kmsCall(t, 6).header.Get("X-Amz-Security-Token"); got != first.sessionToken {
		t.Fatal("request before the margin was not signed with the first credentials")
	}
	last := fixture.kmsCall(t, 7)
	if got := last.header.Get("X-Amz-Security-Token"); got != second.sessionToken {
		t.Fatal("request at the margin was not signed with the refreshed credentials")
	}
	if !strings.Contains(last.header.Get("Authorization"), "Credential="+second.accessKeyID+"/") {
		t.Fatal("request at the margin did not name the refreshed access key id")
	}
	// The signing time follows the injected clock.
	if got := last.header.Get("X-Amz-Date"); got != "20261111T102500Z" {
		t.Fatalf("X-Amz-Date = %q; want the clock time 20261111T102500Z", got)
	}
}

// TestAWSKMSRefusesCredentialsAlreadyInsideTheMargin proves credentials that
// would be stale on arrival are neither used nor cached: no KMS request is
// signed with them and the next call exchanges again.
func TestAWSKMSRefusesCredentialsAlreadyInsideTheMargin(t *testing.T) {
	for _, lifetime := range []time.Duration{5 * time.Minute, time.Minute, -time.Hour} {
		t.Run(lifetime.String(), func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{lifetime: lifetime})
			for attempt := 1; attempt <= 2; attempt++ {
				plaintext, err := fixture.decrypt(awsTestRef)
				if plaintext != nil {
					t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
				}
				requireFaultClass(t, err, fault.DependencyUnavailable)
				fixture.requireCleanError(t, err)
				fixture.requireCounts(t, attempt, 0)
			}
		})
	}

	// One second outside the margin is accepted.
	fixture := newAWSTestFixture(t, awsTestOptions{lifetime: 5*time.Minute + time.Second})
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("credentials one second outside the margin were refused: %v", err)
	}
	fixture.requireCounts(t, 1, 1)
}

// TestAWSKMSRetriesOnceOnRefusedCredentials pins the retry: when KMS says the
// credentials themselves are expired, unknown or mis-signed, the client drops
// them, exchanges once more and retries exactly once.
func TestAWSKMSRetriesOnceOnRefusedCredentials(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
		body   string
	}{
		{name: "expired token header", status: 400, header: "ExpiredTokenException"},
		{name: "expired token body", status: 400, body: `{"__type":"ExpiredTokenException","message":"` + awsTestBodySentinel + `"}`},
		{name: "expired token documented status", status: 403, header: "ExpiredTokenException"},
		{name: "unrecognized client header", status: 400, header: "UnrecognizedClientException"},
		{name: "unrecognized client body", status: 400, body: `{"__type":"UnrecognizedClientException","message":"` + awsTestBodySentinel + `"}`},
		{name: "invalid signature header", status: 400, header: "InvalidSignatureException"},
		{name: "invalid signature namespaced body", status: 403, body: `{"__type":"com.amazon.coral.service#InvalidSignatureException","message":"` + awsTestBodySentinel + `"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name+" then accepted", func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(call int, writer http.ResponseWriter, _ *http.Request) bool {
					if call != 1 {
						return false
					}
					awsTestRefusal(writer, testCase.status, testCase.header, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(plaintext, awsTestKEK()) {
				t.Fatalf("plaintext = %x; want the reply plaintext", plaintext)
			}
			fixture.requireCounts(t, 2, 2)
			fixture.mu.Lock()
			first, second := fixture.issued[0], fixture.issued[1]
			fixture.mu.Unlock()
			if got := fixture.kmsCall(t, 1).header.Get("X-Amz-Security-Token"); got != first.sessionToken {
				t.Fatal("first request was not signed with the first credentials")
			}
			if got := fixture.kmsCall(t, 2).header.Get("X-Amz-Security-Token"); got != second.sessionToken {
				t.Fatal("retry was not signed with freshly exchanged credentials")
			}
			// The fresh credentials are cached: the next call exchanges nothing.
			if _, err := fixture.decrypt(awsTestRef); err != nil {
				t.Fatalf("Decrypt after the retry: %v", err)
			}
			fixture.requireCounts(t, 2, 3)
		})

		t.Run(testCase.name+" never loops", func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					awsTestRefusal(writer, testCase.status, testCase.header, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			requireFaultClass(t, err, fault.Unauthenticated)
			fixture.requireCleanError(t, err)
			fixture.requireCounts(t, 2, 2)
			// The refused credentials are not kept: the next call starts with
			// its own exchange and again retries exactly once.
			_, err = fixture.decrypt(awsTestRef)
			requireFaultClass(t, err, fault.Unauthenticated)
			fixture.requireCounts(t, 4, 4)
		})
	}
}

// TestAWSKMSMapsRefusals pins the KMS error table. AWS JSON services answer
// most refusals with HTTP 400, so the error type decides when it is known (the
// X-Amzn-Errortype header first, then the body's "__type" or "code") and the
// status decides otherwise. Only the three credential types of the retry test
// cause a second request.
func TestAWSKMSMapsRefusals(t *testing.T) {
	typed := func(name string) string {
		return `{"__type":"` + name + `","message":"` + awsTestBodySentinel + `"}`
	}
	untyped := `{"message":"` + awsTestBodySentinel + `"}`
	cases := []struct {
		name   string
		status int
		header string
		body   string
		class  fault.Class
	}{
		// Documented Decrypt errors, by header.
		{name: "access denied", status: 400, header: "AccessDeniedException", body: untyped, class: fault.Forbidden},
		{name: "not found", status: 400, header: "NotFoundException", body: untyped, class: fault.NotFound},
		{name: "disabled", status: 400, header: "DisabledException", body: untyped, class: fault.InvalidInput},
		{name: "invalid state", status: 400, header: "KMSInvalidStateException", body: untyped, class: fault.InvalidInput},
		{name: "invalid ciphertext", status: 400, header: "InvalidCiphertextException", body: untyped, class: fault.InvalidInput},
		{name: "incorrect key", status: 400, header: "IncorrectKeyException", body: untyped, class: fault.InvalidInput},
		{name: "invalid key usage", status: 400, header: "InvalidKeyUsageException", body: untyped, class: fault.InvalidInput},
		{name: "invalid grant token", status: 400, header: "InvalidGrantTokenException", body: untyped, class: fault.InvalidInput},
		{name: "dry run", status: 400, header: "DryRunOperationException", body: untyped, class: fault.InvalidInput},
		{name: "dependency timeout", status: 500, header: "DependencyTimeoutException", body: untyped, class: fault.DependencyUnavailable},
		{name: "key unavailable", status: 500, header: "KeyUnavailableException", body: untyped, class: fault.DependencyUnavailable},
		{name: "kms internal", status: 500, header: "KMSInternalException", body: untyped, class: fault.DependencyUnavailable},
		// Common errors.
		{name: "throttling", status: 400, header: "ThrottlingException", body: untyped, class: fault.DependencyUnavailable},
		{name: "internal failure", status: 500, header: "InternalFailure", body: untyped, class: fault.DependencyUnavailable},
		{name: "service unavailable", status: 503, header: "ServiceUnavailable", body: untyped, class: fault.DependencyUnavailable},
		{name: "request timeout", status: 408, header: "RequestTimeoutException", body: untyped, class: fault.DependencyUnavailable},
		{name: "missing authentication token", status: 400, header: "MissingAuthenticationTokenException", body: untyped, class: fault.Unauthenticated},
		{name: "missing authentication token short", status: 403, header: "MissingAuthenticationToken", body: untyped, class: fault.Unauthenticated},
		{name: "incomplete signature", status: 400, header: "IncompleteSignatureException", body: untyped, class: fault.Unauthenticated},
		{name: "incomplete signature short", status: 403, header: "IncompleteSignature", body: untyped, class: fault.Unauthenticated},
		{name: "validation", status: 400, header: "ValidationException", body: untyped, class: fault.InvalidInput},
		// The same types by body only.
		{name: "body access denied", status: 400, body: typed("AccessDeniedException"), class: fault.Forbidden},
		{name: "body not found", status: 400, body: typed("NotFoundException"), class: fault.NotFound},
		{name: "body throttling", status: 400, body: typed("ThrottlingException"), class: fault.DependencyUnavailable},
		{name: "body invalid ciphertext", status: 400, body: typed("InvalidCiphertextException"), class: fault.InvalidInput},
		{name: "body disabled", status: 400, body: typed("DisabledException"), class: fault.InvalidInput},
		{name: "body code member", status: 400, body: `{"code":"AccessDeniedException","message":"` + awsTestBodySentinel + `"}`, class: fault.Forbidden},
		// Legacy decorations are stripped before the comparison.
		{name: "header uri suffix", status: 400, header: "AccessDeniedException:http://internal.amazon.com/coral/com.amazon.coral.service/", body: untyped, class: fault.Forbidden},
		{name: "body namespace prefix", status: 400, body: typed("com.amazonaws.kms#NotFoundException"), class: fault.NotFound},
		{name: "header namespace and suffix", status: 400, header: "com.amazonaws.kms#KMSInternalException:http://internal.amazon.com/", body: untyped, class: fault.DependencyUnavailable},
		// The header wins over the body; a known type wins over the status.
		{name: "header over body", status: 400, header: "AccessDeniedException", body: typed("NotFoundException"), class: fault.Forbidden},
		{name: "type over forbidden status", status: 403, header: "NotFoundException", body: untyped, class: fault.NotFound},
		{name: "type over server status", status: 500, header: "AccessDeniedException", body: untyped, class: fault.Forbidden},
		// No usable type: the status decides.
		{name: "status 400", status: 400, body: untyped, class: fault.InvalidInput},
		{name: "status 401", status: 401, body: untyped, class: fault.Unauthenticated},
		{name: "status 403", status: 403, body: untyped, class: fault.Forbidden},
		{name: "status 404", status: 404, body: untyped, class: fault.NotFound},
		{name: "status 409", status: 409, body: untyped, class: fault.InvalidInput},
		{name: "status 413", status: 413, body: untyped, class: fault.InvalidInput},
		{name: "status 418", status: 418, body: untyped, class: fault.InvalidInput},
		{name: "status 429", status: 429, body: untyped, class: fault.DependencyUnavailable},
		{name: "status 500", status: 500, body: untyped, class: fault.DependencyUnavailable},
		{name: "status 502", status: 502, body: "<html>" + awsTestBodySentinel + "</html>", class: fault.DependencyUnavailable},
		{name: "status 503", status: 503, body: untyped, class: fault.DependencyUnavailable},
		{name: "unknown type", status: 400, header: "SomeFutureException", body: untyped, class: fault.InvalidInput},
		{name: "unknown type forbidden", status: 403, body: typed("SomeFutureException"), class: fault.Forbidden},
		{name: "empty body", status: 403, class: fault.Forbidden},
		{name: "malformed body", status: 400, body: `{"__type":`, class: fault.InvalidInput},
		{name: "non-string type", status: 403, body: `{"__type":7}`, class: fault.Forbidden},
		// An error body over the bound is not parsed: the status decides.
		{name: "oversized typed body", status: 403, body: `{"__type":"NotFoundException","pad":"` + strings.Repeat("x", 100<<10) + `"}`, class: fault.Forbidden},
		// Unexpected statuses never yield plaintext, even with a valid body.
		{name: "status 201", status: 201, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK()), class: fault.DependencyUnavailable},
		{name: "status 202", status: 202, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK()), class: fault.DependencyUnavailable},
		{name: "status 302", status: 302, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK()), class: fault.DependencyUnavailable},
		{name: "status 307", status: 307, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK()), class: fault.DependencyUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					awsTestRefusal(writer, testCase.status, testCase.header, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			requireFaultClass(t, err, testCase.class)
			if plaintext != nil {
				t.Fatalf("refusal returned %d plaintext bytes", len(plaintext))
			}
			fixture.requireCleanError(t, err)
			for _, name := range []string{testCase.header, "__type", "Exception"} {
				if name != "" && strings.Contains(err.Error(), name) {
					t.Fatalf("error rendered the response error type: %q", err.Error())
				}
			}
			fixture.requireCounts(t, 1, 1)
		})
	}
}

// TestAWSKMSMapsExchangeRefusals pins the STS error table: the error code
// decides when it is known and the status otherwise. A refused exchange signs
// nothing, sends nothing to KMS and is never cached.
func TestAWSKMSMapsExchangeRefusals(t *testing.T) {
	coded := func(code string) string {
		return `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <Error>
    <Type>Sender</Type>
    <Code>` + code + `</Code>
    <Message>` + awsTestBodySentinel + ` ` + awsTestToken + `</Message>
  </Error>
  <RequestId>4fd4f13d-3ea2-4b5b-8e0c-EXAMPLE</RequestId>
</ErrorResponse>`
	}
	cases := []struct {
		name   string
		status int
		header string
		body   string
		class  fault.Class
	}{
		// Documented AssumeRoleWithWebIdentity errors.
		{name: "invalid identity token", status: 400, body: coded("InvalidIdentityToken"), class: fault.Unauthenticated},
		{name: "expired token", status: 400, body: coded("ExpiredTokenException"), class: fault.Unauthenticated},
		{name: "expired token short", status: 400, body: coded("ExpiredToken"), class: fault.Unauthenticated},
		{name: "idp rejected claim", status: 403, body: coded("IDPRejectedClaim"), class: fault.Unauthenticated},
		{name: "idp communication error", status: 400, body: coded("IDPCommunicationError"), class: fault.DependencyUnavailable},
		{name: "access denied", status: 403, body: coded("AccessDenied"), class: fault.Forbidden},
		{name: "access denied exception", status: 403, body: coded("AccessDeniedException"), class: fault.Forbidden},
		{name: "access denied with a 400", status: 400, body: coded("AccessDenied"), class: fault.Forbidden},
		{name: "region disabled", status: 403, body: coded("RegionDisabledException"), class: fault.Forbidden},
		{name: "malformed policy document", status: 400, body: coded("MalformedPolicyDocument"), class: fault.InvalidInput},
		{name: "packed policy too large", status: 400, body: coded("PackedPolicyTooLarge"), class: fault.InvalidInput},
		{name: "validation error", status: 400, body: coded("ValidationError"), class: fault.InvalidInput},
		// Throttling and outages.
		{name: "throttling", status: 400, body: coded("Throttling"), class: fault.DependencyUnavailable},
		{name: "throttling exception", status: 400, body: coded("ThrottlingException"), class: fault.DependencyUnavailable},
		{name: "service unavailable", status: 503, body: coded("ServiceUnavailable"), class: fault.DependencyUnavailable},
		{name: "internal failure", status: 500, body: coded("InternalFailure"), class: fault.DependencyUnavailable},
		// A header or a JSON document is understood too.
		{name: "header code", status: 400, header: "InvalidIdentityToken", class: fault.Unauthenticated},
		{name: "json code", status: 400, body: `{"__type":"IDPCommunicationError","message":"` + awsTestBodySentinel + `"}`, class: fault.DependencyUnavailable},
		// No usable code: the status decides.
		{name: "status 400", status: 400, body: awsTestBodySentinel, class: fault.InvalidInput},
		{name: "status 401", status: 401, body: awsTestBodySentinel, class: fault.Unauthenticated},
		{name: "status 403", status: 403, class: fault.Forbidden},
		{name: "status 404", status: 404, body: awsTestBodySentinel, class: fault.InvalidInput},
		{name: "status 429", status: 429, body: awsTestBodySentinel, class: fault.DependencyUnavailable},
		{name: "status 500", status: 500, body: awsTestBodySentinel, class: fault.DependencyUnavailable},
		{name: "status 503", status: 503, body: "<html>" + awsTestBodySentinel + "</html>", class: fault.DependencyUnavailable},
		{name: "unknown code", status: 400, body: coded("SomeFutureCode"), class: fault.InvalidInput},
		{name: "oversized coded body", status: 403, body: coded("InvalidIdentityToken") + strings.Repeat(" ", 100<<10), class: fault.Forbidden},
		// Unexpected statuses never yield credentials, even with a valid body.
		{name: "status 201", status: 201, body: awsTestSTSReply(awsTestCredentials{"ASIAUNEXPECTED", "unexpected-secret", "unexpected-token"}, "2030-01-01T00:00:00Z"), class: fault.DependencyUnavailable},
		{name: "status 302", status: 302, body: awsTestSTSReply(awsTestCredentials{"ASIAUNEXPECTED", "unexpected-secret", "unexpected-token"}, "2030-01-01T00:00:00Z"), class: fault.DependencyUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				sts: func(call int, writer http.ResponseWriter, _ *http.Request) bool {
					if call != 1 {
						return false
					}
					awsTestRefusal(writer, testCase.status, testCase.header, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			requireFaultClass(t, err, testCase.class)
			if plaintext != nil {
				t.Fatalf("refusal returned %d plaintext bytes", len(plaintext))
			}
			fixture.requireCleanError(t, err)
			if !strings.Contains(err.Error(), "aws sts") {
				t.Fatalf("exchange refusal does not name STS: %q", err.Error())
			}
			for _, code := range []string{
				"InvalidIdentityToken", "ExpiredToken", "IDPRejectedClaim", "IDPCommunicationError", "AccessDenied",
				"RegionDisabled", "MalformedPolicyDocument", "PackedPolicyTooLarge", "ValidationError", "Throttling",
				"ServiceUnavailable", "InternalFailure", "SomeFutureCode", "Exception", "__type",
			} {
				if strings.Contains(err.Error(), code) {
					t.Fatalf("error rendered the response error code: %q", err.Error())
				}
			}
			fixture.requireCounts(t, 1, 0)

			// The refusal was not cached: the next call exchanges again.
			if _, err := fixture.decrypt(awsTestRef); err != nil {
				t.Fatalf("Decrypt after a refused exchange: %v", err)
			}
			fixture.requireCounts(t, 2, 1)
		})
	}
}

// TestAWSKMSTokenFileFaults pins the token-file refusals. Each names the
// AWS_WEB_IDENTITY_TOKEN_FILE role of the file, renders neither its path nor
// its content, reaches no endpoint and is not cached.
func TestAWSKMSTokenFileFaults(t *testing.T) {
	const bound = 20000 // the documented STS WebIdentityToken maximum, spelled as a literal
	cases := []struct {
		name    string
		prepare func(t *testing.T, fixture *awsTestFixture)
	}{
		{name: "missing", prepare: func(t *testing.T, fixture *awsTestFixture) {
			if err := os.Remove(fixture.tokenFile); err != nil {
				t.Fatalf("remove token file: %v", err)
			}
		}},
		{name: "directory", prepare: func(t *testing.T, fixture *awsTestFixture) {
			if err := os.Remove(fixture.tokenFile); err != nil {
				t.Fatalf("remove token file: %v", err)
			}
			if err := os.Mkdir(fixture.tokenFile, 0o700); err != nil {
				t.Fatalf("make directory: %v", err)
			}
		}},
		{name: "empty", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, "") }},
		{name: "newline only", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, "\n") }},
		{name: "two lines", prepare: func(t *testing.T, fixture *awsTestFixture) {
			fixture.writeToken(t, "first-token-line-sentinel\nsecond-token-line-sentinel\n")
		}},
		{name: "two trailing newlines", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, awsTestToken+"\n\n") }},
		{name: "carriage return", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, awsTestToken+"\r\n") }},
		{name: "leading space", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, " "+awsTestToken) }},
		{name: "inner space", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, "token with-space-sentinel") }},
		{name: "nul", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, "token\x00with-nul-sentinel") }},
		{name: "non ascii", prepare: func(t *testing.T, fixture *awsTestFixture) { fixture.writeToken(t, "tokén-sentinel") }},
		{name: "one byte over the bound", prepare: func(t *testing.T, fixture *awsTestFixture) {
			fixture.writeToken(t, strings.Repeat("a", bound+1))
		}},
		{name: "far over the bound", prepare: func(t *testing.T, fixture *awsTestFixture) {
			fixture.writeToken(t, strings.Repeat("a", 1<<20))
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{})
			testCase.prepare(t, fixture)
			for range 2 {
				plaintext, err := fixture.decrypt(awsTestRef)
				requireFaultClass(t, err, fault.Unauthenticated)
				if plaintext != nil {
					t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
				}
				if !strings.Contains(err.Error(), "AWS_WEB_IDENTITY_TOKEN_FILE") {
					t.Fatalf("token-file fault does not name AWS_WEB_IDENTITY_TOKEN_FILE: %q", err.Error())
				}
				fixture.requireCleanError(t, err)
				for _, sentinel := range []string{"sentinel", "aaaa", filepath.Base(filepath.Dir(fixture.tokenFile))} {
					if strings.Contains(err.Error(), sentinel) {
						t.Fatalf("token-file fault rendered the file content or path: %q", err.Error())
					}
				}
			}
			fixture.requireCounts(t, 0, 0)

			// Not cached: once the kubelet writes a usable token the same
			// client works.
			if err := os.RemoveAll(fixture.tokenFile); err != nil {
				t.Fatalf("remove token file: %v", err)
			}
			fixture.writeToken(t, awsTestToken+"\n")
			if _, err := fixture.decrypt(awsTestRef); err != nil {
				t.Fatalf("Decrypt after the token file was repaired: %v", err)
			}
			fixture.requireCounts(t, 1, 1)
		})
	}

	// Exactly the bound is accepted, with or without the trailing newline, and
	// is sent whole.
	for _, suffix := range []string{"", "\n"} {
		fixture := newAWSTestFixture(t, awsTestOptions{})
		token := strings.Repeat("a", bound)
		fixture.writeToken(t, token+suffix)
		if _, err := fixture.decrypt(awsTestRef); err != nil {
			t.Fatalf("a token of exactly the bound (suffix %q) was refused: %v", suffix, err)
		}
		if got := awsTestForm(t, fixture.stsCall(t, 1)).Get("WebIdentityToken"); got != token {
			t.Fatalf("exchange sent a %d-byte token; want the %d-byte file content", len(got), len(token))
		}
	}

	// Only one trailing newline is removed: nothing else is trimmed or
	// rewritten on the way to STS.
	fixture := newAWSTestFixture(t, awsTestOptions{})
	const punctuated = "a.b-c_d~e+f/g=h"
	fixture.writeToken(t, punctuated+"\n")
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got := awsTestForm(t, fixture.stsCall(t, 1)).Get("WebIdentityToken"); got != punctuated {
		t.Fatalf("exchange token = %q; want the file content verbatim %q", got, punctuated)
	}
}

// TestAWSKMSRefusesMalformedExchangeReplies pins sanitized dependency failures
// for an STS answer that is not the documented credential document. Nothing is
// signed with it and nothing is cached.
func TestAWSKMSRefusesMalformedExchangeReplies(t *testing.T) {
	good := awsTestCredentials{"ASIAMALFORMEDCASE", "malformed-case-secret-sentinel", "malformed-case-token-sentinel"}
	const expiration = "2026-11-11T10:30:00Z"
	reply := func(credentials awsTestCredentials, expiration string) string {
		return awsTestSTSReply(credentials, expiration)
	}
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "not xml", body: awsTestBodySentinel},
		{name: "json", body: `{"Credentials":{"AccessKeyId":"ASIAJSON"}}`},
		{name: "truncated", body: reply(good, expiration)[:400]},
		{name: "wrong root", body: strings.ReplaceAll(reply(good, expiration), "AssumeRoleWithWebIdentityResponse", "AssumeRoleResponse")},
		{name: "error document", body: `<ErrorResponse><Error><Code>AccessDenied</Code></Error></ErrorResponse>`},
		{name: "no result", body: `<AssumeRoleWithWebIdentityResponse></AssumeRoleWithWebIdentityResponse>`},
		{name: "credentials outside the result", body: strings.ReplaceAll(reply(good, expiration), "AssumeRoleWithWebIdentityResult", "AssumeRoleResult")},
		{name: "no access key id", body: reply(awsTestCredentials{"", good.secretAccessKey, good.sessionToken}, expiration)},
		{name: "no secret access key", body: reply(awsTestCredentials{good.accessKeyID, "", good.sessionToken}, expiration)},
		{name: "no session token", body: reply(awsTestCredentials{good.accessKeyID, good.secretAccessKey, ""}, expiration)},
		{name: "space in access key id", body: reply(awsTestCredentials{"ASIA MALFORMED", good.secretAccessKey, good.sessionToken}, expiration)},
		{name: "newline in session token", body: reply(awsTestCredentials{good.accessKeyID, good.secretAccessKey, "token\nX-Injected: 1"}, expiration)},
		{name: "non ascii secret", body: reply(awsTestCredentials{good.accessKeyID, "sécret", good.sessionToken}, expiration)},
		{name: "no expiration", body: reply(good, "")},
		{name: "expiration not a time", body: reply(good, "tomorrow")},
		{name: "expiration without zone", body: reply(good, "2026-11-11T10:30:00")},
		{name: "expiration epoch seconds", body: reply(good, "1794393000")},
		{name: "oversized", body: reply(good, expiration) + strings.Repeat(" ", 64<<10)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				sts: func(call int, writer http.ResponseWriter, _ *http.Request) bool {
					if call != 1 {
						return false
					}
					_, _ = io.WriteString(writer, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			fixture.requireCleanError(t, err)
			for _, sentinel := range []string{good.accessKeyID, good.secretAccessKey, good.sessionToken, "sentinel", "ASIA"} {
				if strings.Contains(err.Error(), sentinel) {
					t.Fatalf("refusal echoed the response: %q", err.Error())
				}
			}
			fixture.requireCounts(t, 1, 0)
			if _, err := fixture.decrypt(awsTestRef); err != nil {
				t.Fatalf("Decrypt after a malformed exchange reply: %v", err)
			}
			fixture.requireCounts(t, 2, 1)
		})
	}
}

// TestAWSKMSAcceptsDocumentedExchangeReplyVariants pins what the XML decoder
// tolerates: surrounding whitespace, fractional seconds and a numeric zone in
// the expiration, an XML declaration and no namespace.
func TestAWSKMSAcceptsDocumentedExchangeReplyVariants(t *testing.T) {
	credentials := awsTestCredentials{"ASIAVARIANTCASE0001", "variant-secret/+=", "variant-token//++=="}
	plain := awsTestSTSReply(credentials, "2026-11-11T10:30:00Z")
	cases := []struct {
		name string
		body string
	}{
		{name: "fractional seconds", body: awsTestSTSReply(credentials, "2026-11-11T10:30:00.123Z")},
		{name: "numeric zone", body: awsTestSTSReply(credentials, "2026-11-11T11:30:00+01:00")},
		{name: "xml declaration", body: `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + plain},
		{name: "no namespace", body: strings.Replace(plain, ` xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`, "", 1)},
		{name: "whitespace around values", body: strings.NewReplacer(
			"<AccessKeyId>", "<AccessKeyId>\n        ", "</AccessKeyId>", "\n      </AccessKeyId>",
			"<Expiration>", "<Expiration> ", "</Expiration>", " </Expiration>",
		).Replace(plain)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var fixture *awsTestFixture
			fixture = newAWSTestFixture(t, awsTestOptions{
				sts: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					fixture.mu.Lock()
					fixture.issued = append(fixture.issued, credentials)
					fixture.mu.Unlock()
					_, _ = io.WriteString(writer, testCase.body)
					return true
				},
			})
			if _, err := fixture.decrypt(awsTestRef); err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			fixture.requireCounts(t, 1, 1)
			if got := fixture.kmsCall(t, 1).header.Get("X-Amz-Security-Token"); got != credentials.sessionToken {
				t.Fatalf("session token = %q; want the reply value untouched", got)
			}
		})
	}
}

// TestAWSKMSRefusesMalformedReplies pins sanitized dependency failures for a
// malformed decrypt body, a missing or empty plaintext, a non-base64 plaintext,
// a body over the bound and a reply naming a key other than the requested key
// ARN.
func TestAWSKMSRefusesMalformedReplies(t *testing.T) {
	// The reply plaintext below encodes with '+' and '/' under StdEncoding, so
	// the standard alphabet is distinguishable from the URL-safe one.
	nonStdPlaintext := bytes.Repeat([]byte{0xfb}, secret.DataKeyBytes)
	valid := awsTestKMSReply(awsTestKeyARN, awsTestKEK())
	const otherKey = "arn:aws:kms:eu-west-3:111122223333:key/ffffffff-ffff-ffff-ffff-ffffffffffff"
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "malformed json", body: `{"Plaintext":`},
		{name: "missing plaintext", body: `{"KeyId":"` + awsTestKeyARN + `"}`},
		{name: "null plaintext", body: `{"Plaintext":null}`},
		{name: "empty plaintext", body: `{"Plaintext":""}`},
		{name: "lowercase member only", body: `{"ciphertext":"` + base64.StdEncoding.EncodeToString(awsTestKEK()) + `"}`},
		{name: "not base64", body: `{"Plaintext":"not base64 !!!"}`},
		{name: "url-safe base64", body: `{"Plaintext":"` + base64.URLEncoding.EncodeToString(nonStdPlaintext) + `"}`},
		{name: "unpadded base64", body: `{"Plaintext":"` + base64.RawStdEncoding.EncodeToString(nonStdPlaintext) + `"}`},
		{name: "trailing garbage", body: valid + "x"},
		{name: "second json object", body: valid + `{"Plaintext":""}`},
		{name: "xml", body: "<DecryptResponse>" + awsTestBodySentinel + "</DecryptResponse>"},
		{name: "oversized", body: awsTestKMSReply(awsTestKeyARN, bytes.Repeat([]byte{0x2a}, 96<<10))},
		{name: "another key", body: awsTestKMSReply(otherKey, awsTestKEK())},
		{name: "another region's key", body: awsTestKMSReply(strings.Replace(awsTestKeyARN, "eu-west-3", "eu-west-1", 1), awsTestKEK())},
		{name: "bare key id", body: awsTestKMSReply("1234abcd-12ab-34cd-56ef-1234567890ab", awsTestKEK())},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					_, _ = io.WriteString(writer, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			if err == nil {
				t.Fatalf("reply was accepted and returned %d plaintext bytes", len(plaintext))
			}
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			fixture.requireCleanError(t, err)
			if strings.Contains(err.Error(), "ffffffff") || strings.Contains(err.Error(), "not base64") {
				t.Fatalf("refusal echoed the response body: %q", err.Error())
			}
			fixture.requireCounts(t, 1, 1)
		})
	}
}

// TestAWSKMSResponseKeyIDRule pins when the reported key is compared: only a
// key-ARN reference has a key ARN to compare with. An alias names whatever key
// it points at, and a reply without KeyId is accepted.
func TestAWSKMSResponseKeyIDRule(t *testing.T) {
	const otherKey = "arn:aws:kms:eu-west-3:111122223333:key/ffffffff-ffff-ffff-ffff-ffffffffffff"
	cases := []struct {
		name      string
		reference string
		body      string
	}{
		{name: "alias name with any key", reference: "aws-kms:" + awsTestAliasName, body: awsTestKMSReply(otherKey, awsTestKEK())},
		{name: "alias arn with any key", reference: "aws-kms:" + awsTestAliasARN, body: awsTestKMSReply(otherKey, awsTestKEK())},
		{name: "key arn without a reported key", reference: awsTestRef, body: `{"Plaintext":"` + base64.StdEncoding.EncodeToString(awsTestKEK()) + `"}`},
		{name: "key arn with the same key", reference: awsTestRef, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK())},
		{name: "unknown member ignored", reference: awsTestRef, body: `{"Future":{"x":1},"Plaintext":"` + base64.StdEncoding.EncodeToString(awsTestKEK()) + `"}`},
		{name: "trailing whitespace", reference: awsTestRef, body: awsTestKMSReply(awsTestKeyARN, awsTestKEK()) + " \r\n\t"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					_, _ = io.WriteString(writer, testCase.body)
					return true
				},
			})
			plaintext, err := fixture.decrypt(testCase.reference)
			if err != nil {
				t.Fatalf("reply was refused: %v", err)
			}
			if !bytes.Equal(plaintext, awsTestKEK()) {
				t.Fatalf("plaintext = %x; want %x", plaintext, awsTestKEK())
			}
		})
	}
}

// TestAWSKMSResponseBounds pins the documented 64 KiB bound on both accepted
// bodies: exactly the bound is accepted and one byte more is refused.
func TestAWSKMSResponseBounds(t *testing.T) {
	const responseBound = 64 << 10 // the documented bound, spelled as a literal
	pad := func(body string, total int) string {
		return body + strings.Repeat(" ", total-len(body))
	}
	kmsReply := awsTestKMSReply(awsTestKeyARN, awsTestKEK())
	bounded := awsTestCredentials{"ASIABOUNDCASE0001", "bound-case-secret", "bound-case-token"}
	stsReply := awsTestSTSReply(bounded, "2026-11-11T10:30:00Z")

	for _, testCase := range []struct {
		name     string
		total    int
		accepted bool
	}{
		{name: "exactly the bound", total: responseBound, accepted: true},
		{name: "one byte over", total: responseBound + 1, accepted: false},
	} {
		t.Run("kms "+testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					_, _ = io.WriteString(writer, pad(kmsReply, testCase.total))
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			if testCase.accepted {
				if err != nil || !bytes.Equal(plaintext, awsTestKEK()) {
					t.Fatalf("a reply of exactly the bound gave %x, %v", plaintext, err)
				}
				return
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if plaintext != nil {
				t.Fatalf("failure returned %d plaintext bytes", len(plaintext))
			}
		})

		t.Run("sts "+testCase.name, func(t *testing.T) {
			var fixture *awsTestFixture
			fixture = newAWSTestFixture(t, awsTestOptions{
				sts: func(_ int, writer http.ResponseWriter, _ *http.Request) bool {
					fixture.mu.Lock()
					fixture.issued = append(fixture.issued, bounded)
					fixture.mu.Unlock()
					_, _ = io.WriteString(writer, pad(stsReply, testCase.total))
					return true
				},
			})
			plaintext, err := fixture.decrypt(awsTestRef)
			if testCase.accepted {
				if err != nil || !bytes.Equal(plaintext, awsTestKEK()) {
					t.Fatalf("an exchange reply of exactly the bound gave %x, %v", plaintext, err)
				}
				return
			}
			requireFaultClass(t, err, fault.DependencyUnavailable)
			fixture.requireCounts(t, 1, 0)
		})
	}
}

// TestAWSKMSNeverFollowsRedirects proves neither request is replayed to a
// redirect target: the web identity token and the signed request stay with the
// configured origin.
func TestAWSKMSNeverFollowsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	redirect := func(writer http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path == "/redirect-target" {
			targetHits.Add(1)
			return false
		}
		writer.Header().Set("Location", "/redirect-target")
		writer.WriteHeader(http.StatusTemporaryRedirect)
		return true
	}

	t.Run("sts", func(t *testing.T) {
		targetHits.Store(0)
		fixture := newAWSTestFixture(t, awsTestOptions{
			sts: func(_ int, writer http.ResponseWriter, request *http.Request) bool { return redirect(writer, request) },
		})
		plaintext, err := fixture.decrypt(awsTestRef)
		if err == nil {
			t.Fatalf("redirect response was followed and returned %d plaintext bytes", len(plaintext))
		}
		requireFaultClass(t, err, fault.DependencyUnavailable)
		fixture.requireCleanError(t, err)
		if hits := targetHits.Load(); hits != 0 {
			t.Fatalf("redirect target received %d requests; the token must never be replayed", hits)
		}
		fixture.requireCounts(t, 1, 0)
	})

	t.Run("kms", func(t *testing.T) {
		targetHits.Store(0)
		fixture := newAWSTestFixture(t, awsTestOptions{
			kms: func(_ int, writer http.ResponseWriter, request *http.Request) bool { return redirect(writer, request) },
		})
		plaintext, err := fixture.decrypt(awsTestRef)
		if err == nil {
			t.Fatalf("redirect response was followed and returned %d plaintext bytes", len(plaintext))
		}
		requireFaultClass(t, err, fault.DependencyUnavailable)
		fixture.requireCleanError(t, err)
		if hits := targetHits.Load(); hits != 0 {
			t.Fatalf("redirect target received %d requests; the signed request must never be replayed", hits)
		}
		fixture.requireCounts(t, 1, 1)
	})
}

// TestAWSKMSIgnoresProxies proves neither the environment nor the caller's
// client routes a request through a proxy. Go never applies an environment
// proxy to a loopback destination and reads the environment once per process,
// so the environment variables alone would prove nothing here; the caller's
// explicit proxy function has no such exception, and the constructor must drop
// it. aws_kms_transport_internal_test.go pins Proxy == nil directly.
func TestAWSKMSIgnoresProxies(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var callerTransport *http.Transport
	fixture := newAWSTestFixture(t, awsTestOptions{
		configure: func(config *keys.AWSKMSConfig) {
			callerTransport = config.HTTPClient.Transport.(*http.Transport)
			callerTransport.Proxy = http.ProxyURL(proxyURL)
		},
	})
	plaintext, err := fixture.decrypt(awsTestRef)
	if err != nil {
		t.Fatalf("Decrypt with a proxy configured: %v", err)
	}
	if !bytes.Equal(plaintext, awsTestKEK()) {
		t.Fatalf("plaintext = %x; want the reply plaintext", plaintext)
	}
	if hits := proxyHits.Load(); hits != 0 {
		t.Fatalf("proxy received %d requests; the client must connect directly", hits)
	}
	fixture.requireCounts(t, 1, 1)
	if callerTransport.Proxy == nil {
		t.Fatal("constructor mutated the caller's transport")
	}
}

// TestAWSKMSVerifiesTLSByDefault proves the client-owned transport keeps
// certificate verification on: an untrusted TLS server is refused and the
// token never reaches its handler.
func TestAWSKMSVerifiesTLSByDefault(t *testing.T) {
	fixture := newAWSTestFixture(t, awsTestOptions{
		configure: func(config *keys.AWSKMSConfig) { config.HTTPClient = nil },
	})
	plaintext, err := fixture.decrypt(awsTestRef)
	if err == nil {
		t.Fatalf("untrusted TLS server was accepted and returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	fixture.requireCleanError(t, err)
	fixture.requireCounts(t, 0, 0)
}

// TestAWSKMSTransportFailureIsClassifiedAndSanitized proves a transport failure
// is a dependency fault and that an underlying dial error carrying the token is
// never rendered by the public error.
func TestAWSKMSTransportFailureIsClassifiedAndSanitized(t *testing.T) {
	dialCause := errors.New("dial-refused-sentinel")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(awsTestToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	client, err := keys.NewAWSKMS(keys.AWSKMSConfig{
		Region:               awsTestRegion,
		RoleARN:              awsTestRoleARN,
		WebIdentityTokenFile: tokenFile,
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, fmt.Errorf("dial refused for token %s: %w", awsTestToken, dialCause)
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	plaintext, err := client.Decrypt(context.Background(), secret.KeyRef(awsTestRef), awsTestBlob())
	if err == nil {
		t.Fatalf("transport failure returned %d plaintext bytes", len(plaintext))
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, dialCause) {
		t.Fatalf("transport failure dropped its private cause: %v", err)
	}
	for _, sentinel := range []string{awsTestToken, dialCause.Error(), "amazonaws.com", "https://", tokenFile} {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("transport refusal disclosed %q: %q", sentinel, err.Error())
		}
	}
}

// TestAWSKMSHonoursContextCancellation proves a pre-cancelled context and a
// cancellation during either request return the context error itself, with no
// fault class, and never succeed.
func TestAWSKMSHonoursContextCancellation(t *testing.T) {
	t.Run("before the call", func(t *testing.T) {
		fixture := newAWSTestFixture(t, awsTestOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := fixture.client.Decrypt(ctx, secret.KeyRef(awsTestRef), awsTestBlob())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled Decrypt error = %v; want context.Canceled", err)
		}
		if class, ok := fault.ClassOf(err); ok {
			t.Fatalf("pre-cancelled Decrypt carried fault class %q; want the bare context error", class)
		}
		// Cancellation is checked before the key-reference grammar: a cancelled
		// caller learns the call was cancelled, not that its reference was bad.
		if _, err := fixture.client.Decrypt(ctx, secret.KeyRef("not-an-aws-reference"), awsTestBlob()); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled Decrypt with a foreign reference error = %v; want context.Canceled checked first", err)
		}
		fixture.requireCounts(t, 0, 0)
	})

	// block holds the first request of one endpoint until the test releases it.
	during := func(t *testing.T, options func(block func(http.ResponseWriter, *http.Request) bool) awsTestOptions, sts, kms int) {
		entered := make(chan struct{})
		release := make(chan struct{})
		// The deferred close unblocks the handler before the server cleanup
		// waits for it, including on the failure paths.
		defer close(release)
		var once atomic.Bool
		fixture := newAWSTestFixture(t, options(func(_ http.ResponseWriter, request *http.Request) bool {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			select {
			case <-request.Context().Done():
			case <-release:
			}
			return true
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := fixture.client.Decrypt(ctx, secret.KeyRef(awsTestRef), awsTestBlob())
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
		fixture.requireCounts(t, sts, kms)
	}

	t.Run("during the exchange", func(t *testing.T) {
		during(t, func(block func(http.ResponseWriter, *http.Request) bool) awsTestOptions {
			return awsTestOptions{sts: func(_ int, writer http.ResponseWriter, request *http.Request) bool {
				return block(writer, request)
			}}
		}, 1, 0)
	})

	t.Run("during the decrypt", func(t *testing.T) {
		during(t, func(block func(http.ResponseWriter, *http.Request) bool) awsTestOptions {
			return awsTestOptions{kms: func(_ int, writer http.ResponseWriter, request *http.Request) bool {
				return block(writer, request)
			}}
		}, 1, 1)
	})
}

// TestAWSKMSCancelledExchangeIsNotCached proves a cancellation during the
// exchange leaves nothing behind: the next call exchanges and succeeds.
func TestAWSKMSCancelledExchangeIsNotCached(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	fixture := newAWSTestFixture(t, awsTestOptions{
		sts: func(call int, _ http.ResponseWriter, request *http.Request) bool {
			if call != 1 {
				return false
			}
			close(entered)
			select {
			case <-request.Context().Done():
			case <-release:
			}
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := fixture.client.Decrypt(ctx, secret.KeyRef(awsTestRef), awsTestBlob())
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the exchange")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Decrypt error = %v; want context.Canceled", err)
	}
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt after a cancelled exchange: %v", err)
	}
	fixture.requireCounts(t, 2, 1)
}

// TestAWSKMSConcurrentDecryptsShareOneExchange proves the credential cache is
// guarded: concurrent first calls make one exchange and every call succeeds
// with the same credentials. The exchange is held open until every caller has
// started, so an unguarded cache would exchange once per caller. Run under
// -race.
func TestAWSKMSConcurrentDecryptsShareOneExchange(t *testing.T) {
	const callers = 16
	release := make(chan struct{})
	fixture := newAWSTestFixture(t, awsTestOptions{
		sts: func(_ int, _ http.ResponseWriter, _ *http.Request) bool {
			<-release
			return false
		},
	})
	var started sync.WaitGroup
	var finished sync.WaitGroup
	failures := make(chan error, callers)
	for range callers {
		started.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			started.Done()
			plaintext, err := fixture.decrypt(awsTestRef)
			if err == nil && !bytes.Equal(plaintext, awsTestKEK()) {
				err = errors.New("plaintext is not the reply plaintext")
			}
			failures <- err
		}()
	}
	started.Wait()
	// Give every caller time to reach the exchange or the lock in front of it.
	time.Sleep(100 * time.Millisecond)
	close(release)
	finished.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("concurrent Decrypt: %v", err)
		}
	}
	fixture.requireCounts(t, 1, callers)
	fixture.mu.Lock()
	issued := fixture.issued[0]
	fixture.mu.Unlock()
	for number := 1; number <= callers; number++ {
		if got := fixture.kmsCall(t, number).header.Get("X-Amz-Security-Token"); got != issued.sessionToken {
			t.Fatalf("request %d was not signed with the shared credentials", number)
		}
	}
}

// TestAWSKMSDoesNotCacheReplies proves every Decrypt performs its own KMS
// request; caching plaintext belongs to the shared cloud provider.
func TestAWSKMSDoesNotCacheReplies(t *testing.T) {
	fixture := newAWSTestFixture(t, awsTestOptions{})
	for attempt := range 3 {
		if _, err := fixture.decrypt(awsTestRef); err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
	}
	fixture.requireCounts(t, 1, 3)
}

// TestAWSKMSRoleSessionName pins the session name sent to STS: the configured
// one, or "kodama".
func TestAWSKMSRoleSessionName(t *testing.T) {
	for configured, want := range map[string]string{
		"":                       "kodama",
		"kodama-eu-west-3":       "kodama-eu-west-3",
		"a_b+c=d,e.f@g-h":        "a_b+c=d,e.f@g-h",
		"ab":                     "ab",
		strings.Repeat("s", 64):  strings.Repeat("s", 64),
		"Kodama.Control@Cluster": "Kodama.Control@Cluster",
	} {
		fixture := newAWSTestFixture(t, awsTestOptions{
			configure: func(config *keys.AWSKMSConfig) { config.RoleSessionName = configured },
		})
		if _, err := fixture.decrypt(awsTestRef); err != nil {
			t.Fatalf("Decrypt with session name %q: %v", configured, err)
		}
		if got := awsTestForm(t, fixture.stsCall(t, 1)).Get("RoleSessionName"); got != want {
			t.Fatalf("RoleSessionName = %q; want %q", got, want)
		}
	}
}

// TestAWSKMSConfigurationRefusals pins constructor refusals for every field:
// the region, the role ARN, the token-file path, the session name, both
// endpoints and the transport.
func TestAWSKMSConfigurationRefusals(t *testing.T) {
	valid := func() keys.AWSKMSConfig {
		return keys.AWSKMSConfig{
			Region:               awsTestRegion,
			RoleARN:              awsTestRoleARN,
			WebIdentityTokenFile: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
		}
	}
	type mutation func(config *keys.AWSKMSConfig)
	region := func(value string) mutation { return func(config *keys.AWSKMSConfig) { config.Region = value } }
	role := func(value string) mutation { return func(config *keys.AWSKMSConfig) { config.RoleARN = value } }
	tokenFile := func(value string) mutation {
		return func(config *keys.AWSKMSConfig) { config.WebIdentityTokenFile = value }
	}
	session := func(value string) mutation {
		return func(config *keys.AWSKMSConfig) { config.RoleSessionName = value }
	}
	kmsEndpoint := func(value string) mutation { return func(config *keys.AWSKMSConfig) { config.KMSEndpoint = value } }
	stsEndpoint := func(value string) mutation { return func(config *keys.AWSKMSConfig) { config.STSEndpoint = value } }
	transport := func(value http.RoundTripper) mutation {
		return func(config *keys.AWSKMSConfig) { config.HTTPClient = &http.Client{Transport: value} }
	}

	cases := []struct {
		name   string
		mutate mutation
	}{
		{name: "empty region", mutate: region("")},
		{name: "uppercase region", mutate: region("EU-WEST-3")},
		{name: "region without number", mutate: region("eu-west")},
		{name: "region with host bytes", mutate: region("eu-west-3.example.com")},
		{name: "region with slash", mutate: region("eu-west-3/x")},
		{name: "region with space", mutate: region("eu-west-3 ")},
		{name: "region with newline", mutate: region("eu-west-3\n")},
		{name: "scaleway region", mutate: region("fr-par")},

		{name: "empty role", mutate: role("")},
		{name: "role name only", mutate: role("kodama-kms")},
		{name: "role without name", mutate: role("arn:aws:iam::111122223333:role/")},
		{name: "role with region", mutate: role("arn:aws:iam:eu-west-3:111122223333:role/kodama-kms")},
		{name: "role with short account", mutate: role("arn:aws:iam::11112222333:role/kodama-kms")},
		{name: "role with letter account", mutate: role("arn:aws:iam::11112222333a:role/kodama-kms")},
		{name: "role without account", mutate: role("arn:aws:iam:::role/kodama-kms")},
		{name: "role with unknown partition", mutate: role("arn:aws-iso:iam::111122223333:role/kodama-kms")},
		{name: "role of another service", mutate: role("arn:aws:sts::111122223333:role/kodama-kms")},
		{name: "user arn", mutate: role("arn:aws:iam::111122223333:user/kodama-kms")},
		{name: "assumed role arn", mutate: role("arn:aws:sts::111122223333:assumed-role/kodama-kms/session")},
		{name: "role with space", mutate: role("arn:aws:iam::111122223333:role/kodama kms")},
		{name: "role with newline", mutate: role("arn:aws:iam::111122223333:role/kodama-kms\n")},
		{name: "role with nul", mutate: role("arn:aws:iam::111122223333:role/kodama\x00kms")},
		{name: "role with ampersand", mutate: role("arn:aws:iam::111122223333:role/kodama&RoleSessionName=x")},
		{name: "role with empty path segment", mutate: role("arn:aws:iam::111122223333:role//kodama-kms")},
		{name: "role with trailing slash", mutate: role("arn:aws:iam::111122223333:role/path/")},
		{name: "role with non ascii", mutate: role("arn:aws:iam::111122223333:role/kodamá")},
		{name: "role with leading space", mutate: role(" " + awsTestRoleARN)},
		{name: "oversized role", mutate: role("arn:aws:iam::111122223333:role/" + strings.Repeat("a", 2048))},

		{name: "empty token file", mutate: tokenFile("")},
		{name: "relative token file", mutate: tokenFile("token")},
		{name: "dot relative token file", mutate: tokenFile("./serviceaccount/token")},
		{name: "token file with nul", mutate: tokenFile("/var/run/secrets/token\x00")},

		{name: "one byte session name", mutate: session("k")},
		{name: "oversized session name", mutate: session(strings.Repeat("s", 65))},
		{name: "session name with space", mutate: session("kodama control")},
		{name: "session name with slash", mutate: session("kodama/control")},
		{name: "session name with colon", mutate: session("kodama:control")},
		{name: "session name with ampersand", mutate: session("kodama&x=1")},
		{name: "session name with newline", mutate: session("kodama\n")},
		{name: "session name with non ascii", mutate: session("kodamá")},

		{name: "kms plain http", mutate: kmsEndpoint("http://kms.example.com")},
		{name: "kms plain http loopback", mutate: kmsEndpoint("http://127.0.0.1:8080")},
		{name: "kms no scheme", mutate: kmsEndpoint("kms.example.com")},
		{name: "kms no host", mutate: kmsEndpoint("https://")},
		{name: "kms userinfo", mutate: kmsEndpoint("https://user:url-password-sentinel@kms.example.com")},
		{name: "kms query", mutate: kmsEndpoint("https://kms.example.com?token=query-token-sentinel")},
		{name: "kms forced query", mutate: kmsEndpoint("https://kms.example.com?")},
		{name: "kms fragment", mutate: kmsEndpoint("https://kms.example.com#x")},
		{name: "kms whitespace", mutate: kmsEndpoint("https://kms.example.com /x")},
		{name: "kms space in path", mutate: kmsEndpoint("https://kms.example.com/a b")},
		{name: "kms control byte", mutate: kmsEndpoint("https://kms.example.com/\x7f")},

		{name: "sts plain http", mutate: stsEndpoint("http://sts.example.com")},
		{name: "sts plain http loopback", mutate: stsEndpoint("http://127.0.0.1:8080")},
		{name: "sts no scheme", mutate: stsEndpoint("sts.example.com")},
		{name: "sts no host", mutate: stsEndpoint("https://")},
		{name: "sts userinfo", mutate: stsEndpoint("https://user:url-password-sentinel@sts.example.com")},
		{name: "sts query", mutate: stsEndpoint("https://sts.example.com?token=query-token-sentinel")},
		{name: "sts forced query", mutate: stsEndpoint("https://sts.example.com?")},
		{name: "sts fragment", mutate: stsEndpoint("https://sts.example.com#x")},
		{name: "sts whitespace", mutate: stsEndpoint("https://sts.example.com\t/x")},
		{name: "sts space in path", mutate: stsEndpoint("https://sts.example.com/a b")},

		{name: "shared default transport", mutate: transport(http.DefaultTransport)},
		{name: "typed nil transport", mutate: transport((*http.Transport)(nil))},
		{name: "opaque transport", mutate: transport(awsTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("must not be used")
		}))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := valid()
			testCase.mutate(&config)
			client, err := keys.NewAWSKMS(config)
			if err == nil {
				t.Fatalf("configuration was accepted: %+v", config)
			}
			if client != nil {
				t.Fatal("refused configuration returned a client")
			}
			requireFaultClass(t, err, fault.InvalidInput)
			for _, sentinel := range []string{"url-password-sentinel", "query-token-sentinel", "scaleway"} {
				if strings.Contains(err.Error(), sentinel) {
					t.Fatalf("refusal rendered %q: %q", sentinel, err.Error())
				}
			}
		})
	}

	// Each accepted shape constructs without a request or a file read: the
	// token path above does not exist on the test host.
	accepted := []struct {
		name   string
		mutate mutation
	}{
		{name: "defaults", mutate: func(*keys.AWSKMSConfig) {}},
		{name: "china region", mutate: func(config *keys.AWSKMSConfig) {
			config.Region = "cn-north-1"
			config.RoleARN = "arn:aws-cn:iam::111122223333:role/kodama-kms"
		}},
		{name: "govcloud region", mutate: func(config *keys.AWSKMSConfig) {
			config.Region = "us-gov-west-1"
			config.RoleARN = "arn:aws-us-gov:iam::111122223333:role/kodama-kms"
		}},
		{name: "role with path", mutate: role("arn:aws:iam::111122223333:role/service-role/eks/kodama-kms")},
		{name: "role with name punctuation", mutate: role("arn:aws:iam::111122223333:role/Kodama_KMS+a=b,c.d@e-f")},
		{name: "endpoint with trailing slash", mutate: kmsEndpoint("https://kms.example.com/")},
		{name: "endpoint with port and path", mutate: stsEndpoint("https://sts.example.com:8443/base")},
		{name: "explicit transport", mutate: transport(&http.Transport{})},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			config := valid()
			testCase.mutate(&config)
			client, err := keys.NewAWSKMS(config)
			if err != nil {
				t.Fatalf("NewAWSKMS refused a valid configuration: %v", err)
			}
			if client == nil {
				t.Fatal("NewAWSKMS returned a nil client")
			}
		})
	}
}

// TestAWSKMSEndpointPathIsSignedAsSent proves an endpoint with a base path is
// called and signed at that path: the fake KMS verifies the signature over the
// path it received.
func TestAWSKMSEndpointPathIsSignedAsSent(t *testing.T) {
	var fixture *awsTestFixture
	fixture = newAWSTestFixture(t, awsTestOptions{
		configure: func(config *keys.AWSKMSConfig) {
			config.KMSEndpoint += "/base/kms/"
			config.STSEndpoint += "/base/sts"
		},
	})
	if _, err := fixture.decrypt(awsTestRef); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got := fixture.stsCall(t, 1).requestURI; got != "/base/sts/" {
		t.Fatalf("STS request URI = %q; want /base/sts/", got)
	}
	if got := fixture.kmsCall(t, 1).requestURI; got != "/base/kms/" {
		t.Fatalf("KMS request URI = %q; want /base/kms/", got)
	}
}

// awsTestWrapRequest is the provider-level request the end-to-end tests wrap.
func awsTestWrapRequest(t *testing.T, reference string) secret.WrapRequest {
	t.Helper()
	return secret.WrapRequest{
		KeyRef:         secret.KeyRef(reference),
		KeyVersion:     1,
		Plaintext:      testDEK(),
		AssociatedData: testAssociatedBytes(t, testAssociated(1)),
	}
}

// TestAWSProviderWrapsAndUnwrapsThroughTheClient drives the real client through
// the shared cloud provider against the loopback endpoints: a data key wraps
// and unwraps, and the key-encryption key is fetched once.
func TestAWSProviderWrapsAndUnwrapsThroughTheClient(t *testing.T) {
	for _, reference := range []string{awsTestRef, "aws-kms:" + awsTestAliasName, "aws-kms:" + awsTestAliasARN} {
		t.Run(reference, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{})
			provider, err := keys.NewAWS(fixture.client, keys.CloudConfig{
				KeyRef:   secret.KeyRef(reference),
				Current:  1,
				Versions: map[secret.KeyVersion][]byte{1: awsTestBlob()},
			}, deterministicRandom(0x61))
			if err != nil {
				t.Fatalf("NewAWS: %v", err)
			}
			// Construction contacts nothing.
			fixture.requireCounts(t, 0, 0)

			request := awsTestWrapRequest(t, reference)
			wrapped, err := provider.Wrap(context.Background(), request)
			if err != nil {
				t.Fatalf("Wrap: %v", err)
			}
			plaintext, err := provider.Unwrap(context.Background(), secret.UnwrapRequest{
				KeyRef:         request.KeyRef,
				KeyVersion:     wrapped.KeyVersion,
				Wrapped:        wrapped.Wrapped,
				Nonce:          wrapped.Nonce,
				AssociatedData: request.AssociatedData,
			})
			if err != nil {
				t.Fatalf("Unwrap: %v", err)
			}
			if !bytes.Equal(plaintext, testDEK()) {
				t.Fatal("Unwrap did not return the wrapped data key")
			}
			// The wrap is AES-256-GCM under the key KMS returned.
			if want := gcmSeal(t, awsTestKEK(), wrapped.Nonce, testDEK(), request.AssociatedData); !bytes.Equal(wrapped.Wrapped, want) {
				t.Fatal("wrapped key was not sealed under the key-encryption key KMS returned")
			}
			fixture.requireCounts(t, 1, 1)
			var body struct {
				CiphertextBlob string
				KeyID          string `json:"KeyId"`
			}
			if err := json.Unmarshal([]byte(fixture.kmsCall(t, 1).body), &body); err != nil {
				t.Fatalf("KMS body: %v", err)
			}
			if body.CiphertextBlob != base64.StdEncoding.EncodeToString(awsTestBlob()) || "aws-kms:"+body.KeyID != reference {
				t.Fatalf("KMS request = %+v; want the configured blob and reference", body)
			}
		})
	}
}

// TestAWSProviderEnforcesKEKLengthAndDoesNotCacheFailure drives the real client
// through the shared cloud provider. A 31-byte reply is an invalid_input
// refusal (the provider owns the 32-byte rule) and is not cached; the next call
// retries the decrypt and succeeds with 32 bytes.
func TestAWSProviderEnforcesKEKLengthAndDoesNotCacheFailure(t *testing.T) {
	short := awsTestKMSReply(awsTestKeyARN, bytes.Repeat([]byte{0x33}, secret.DataKeyBytes-1))
	fixture := newAWSTestFixture(t, awsTestOptions{
		kms: func(call int, writer http.ResponseWriter, _ *http.Request) bool {
			if call != 1 {
				return false
			}
			_, _ = io.WriteString(writer, short)
			return true
		},
	})
	provider, err := keys.NewAWS(fixture.client, keys.CloudConfig{
		KeyRef:   secret.KeyRef(awsTestRef),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: awsTestBlob()},
	}, deterministicRandom(0x62))
	if err != nil {
		t.Fatalf("NewAWS: %v", err)
	}
	request := awsTestWrapRequest(t, awsTestRef)
	_, err = provider.Wrap(context.Background(), request)
	requireFaultClass(t, err, fault.InvalidInput)
	fixture.requireCounts(t, 1, 1)
	if _, err := provider.Wrap(context.Background(), request); err != nil {
		t.Fatalf("second Wrap after a short KEK: %v", err)
	}
	// The role credentials were reused; only the decrypt was repeated.
	fixture.requireCounts(t, 1, 2)
}

// TestAWSProviderKeepsRefusalsClassifiedAndUncached proves the classes the
// shared provider preserves survive it with the client's message, a refusal is
// retried rather than cached, and a reference for another region reaches no
// endpoint even through the provider.
func TestAWSProviderKeepsRefusalsClassifiedAndUncached(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		class   fault.Class
		message string
	}{
		{name: "denied", header: "AccessDeniedException", class: fault.Forbidden, message: "aws kms denied the request for this role"},
		{name: "not found", header: "NotFoundException", class: fault.NotFound, message: "aws kms key was not found"},
		{name: "throttled", header: "ThrottlingException", class: fault.DependencyUnavailable, message: "aws kms is unavailable"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAWSTestFixture(t, awsTestOptions{
				kms: func(call int, writer http.ResponseWriter, _ *http.Request) bool {
					if call != 1 {
						return false
					}
					awsTestRefusal(writer, http.StatusBadRequest, testCase.header, `{"message":"`+awsTestBodySentinel+`"}`)
					return true
				},
			})
			provider, err := keys.NewAWS(fixture.client, keys.CloudConfig{
				KeyRef:   secret.KeyRef(awsTestRef),
				Current:  1,
				Versions: map[secret.KeyVersion][]byte{1: awsTestBlob()},
			}, deterministicRandom(0x63))
			if err != nil {
				t.Fatalf("NewAWS: %v", err)
			}
			request := awsTestWrapRequest(t, awsTestRef)
			_, err = provider.Wrap(context.Background(), request)
			requireFaultClass(t, err, testCase.class)
			if message, _ := fault.MessageOf(err); message != testCase.message {
				t.Fatalf("provider message = %q; want the client's %q", message, testCase.message)
			}
			fixture.requireCleanError(t, err)
			if _, err := provider.Wrap(context.Background(), request); err != nil {
				t.Fatalf("second Wrap after a refusal: %v", err)
			}
			fixture.requireCounts(t, 1, 2)
		})
	}

	t.Run("another region", func(t *testing.T) {
		const reference = "aws-kms:arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
		fixture := newAWSTestFixture(t, awsTestOptions{})
		provider, err := keys.NewAWS(fixture.client, keys.CloudConfig{
			KeyRef:   secret.KeyRef(reference),
			Current:  1,
			Versions: map[secret.KeyVersion][]byte{1: awsTestBlob()},
		}, deterministicRandom(0x64))
		if err != nil {
			t.Fatalf("NewAWS: %v", err)
		}
		if _, err := provider.Wrap(context.Background(), awsTestWrapRequest(t, reference)); err == nil {
			t.Fatal("Wrap under a key in another region succeeded")
		}
		fixture.requireCounts(t, 0, 0)
	})
}
