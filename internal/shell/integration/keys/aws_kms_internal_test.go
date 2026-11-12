package keys

// White-box pins for the AWS KMS client invariants that a loopback server
// cannot observe: the exact default endpoints, the number of token-file reads
// (none before the region check, one per exchange), the hard bounds of the
// token and response readers (the input is cut off, not merely refused after
// buffering), cancellation that lands inside a body read, statuses whose real
// responses carry no body, and the error-type sanitising rules.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	awsStubRegion = "eu-west-3"
	awsStubKeyARN = "arn:aws:kms:eu-west-3:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	awsStubRef    = "aws-kms:" + awsStubKeyARN
	awsStubRole   = "arn:aws:iam::111122223333:role/kodama-kms"
	awsStubToken  = "stub-web-identity-token"
)

// TestNewAWSKMSDefaultEndpointsArePinned pins the literal default origins and
// the default session name. A moved default (another host, a global endpoint
// or the wrong partition suffix) would send the service-account token or a
// signed request to the wrong endpoint; the constructor succeeding is not
// evidence that the origin is right.
func TestNewAWSKMSDefaultEndpointsArePinned(t *testing.T) {
	cases := []struct {
		region string
		role   string
		kms    string
		sts    string
	}{
		{region: "eu-west-3", role: awsStubRole,
			kms: "https://kms.eu-west-3.amazonaws.com", sts: "https://sts.eu-west-3.amazonaws.com"},
		{region: "us-east-1", role: awsStubRole,
			kms: "https://kms.us-east-1.amazonaws.com", sts: "https://sts.us-east-1.amazonaws.com"},
		{region: "ap-southeast-2", role: awsStubRole,
			kms: "https://kms.ap-southeast-2.amazonaws.com", sts: "https://sts.ap-southeast-2.amazonaws.com"},
		{region: "us-gov-west-1", role: "arn:aws-us-gov:iam::111122223333:role/kodama-kms",
			kms: "https://kms.us-gov-west-1.amazonaws.com", sts: "https://sts.us-gov-west-1.amazonaws.com"},
		{region: "cn-north-1", role: "arn:aws-cn:iam::111122223333:role/kodama-kms",
			kms: "https://kms.cn-north-1.amazonaws.com.cn", sts: "https://sts.cn-north-1.amazonaws.com.cn"},
		{region: "cn-northwest-1", role: "arn:aws-cn:iam::111122223333:role/kodama-kms",
			kms: "https://kms.cn-northwest-1.amazonaws.com.cn", sts: "https://sts.cn-northwest-1.amazonaws.com.cn"},
	}
	for _, testCase := range cases {
		client, err := NewAWSKMS(AWSKMSConfig{
			Region:               testCase.region,
			RoleARN:              testCase.role,
			WebIdentityTokenFile: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
		})
		if err != nil {
			t.Fatalf("NewAWSKMS(%s): %v", testCase.region, err)
		}
		if client.kmsEndpoint != testCase.kms {
			t.Fatalf("%s default KMS endpoint = %q; want %q", testCase.region, client.kmsEndpoint, testCase.kms)
		}
		if client.stsEndpoint != testCase.sts {
			t.Fatalf("%s default STS endpoint = %q; want %q", testCase.region, client.stsEndpoint, testCase.sts)
		}
		if client.sessionName != "kodama" {
			t.Fatalf("default session name = %q; want kodama", client.sessionName)
		}
		if client.cached != nil {
			t.Fatal("a new client already holds credentials")
		}
	}

	// An override that is only path separators is no override: it selects the
	// AWS default, never another provider's.
	for _, blank := range []string{"/", "//"} {
		client, err := NewAWSKMS(AWSKMSConfig{
			Region:               "eu-west-3",
			RoleARN:              awsStubRole,
			WebIdentityTokenFile: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
			KMSEndpoint:          blank,
			STSEndpoint:          blank,
		})
		if err != nil {
			t.Fatalf("NewAWSKMS with endpoint %q: %v", blank, err)
		}
		if client.kmsEndpoint != "https://kms.eu-west-3.amazonaws.com" || client.stsEndpoint != "https://sts.eu-west-3.amazonaws.com" {
			t.Fatalf("endpoint %q selected %q and %q; want the AWS defaults", blank, client.kmsEndpoint, client.stsEndpoint)
		}
	}
}

// awsStubTransport answers both endpoints in process. A request carrying
// X-Amz-Target is the KMS decrypt; anything else is the STS exchange. A nil
// reply function selects a valid default answer.
type awsStubTransport struct {
	now func() time.Time
	sts func(call int, request *http.Request) *http.Response
	kms func(call int, request *http.Request) *http.Response

	mu       sync.Mutex
	stsCalls int
	kmsCalls int
}

func awsStubResponse(request *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: request}
}

func awsStubBody(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

func awsStubCredentials(expiration time.Time) string {
	return `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>` +
		`<AccessKeyId>ASIASTUBACCESSKEY</AccessKeyId><SecretAccessKey>stub-secret-access-key</SecretAccessKey>` +
		`<SessionToken>stub-session-token</SessionToken><Expiration>` + expiration.UTC().Format(time.RFC3339) + `</Expiration>` +
		`</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`
}

func awsStubPlaintext() string {
	return `{"KeyId":"` + awsStubKeyARN + `","Plaintext":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, secret.DataKeyBytes)) + `"}`
}

func (transport *awsStubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	decrypt := request.Header.Get("X-Amz-Target") != ""
	var call int
	if decrypt {
		transport.kmsCalls++
		call = transport.kmsCalls
	} else {
		transport.stsCalls++
		call = transport.stsCalls
	}
	transport.mu.Unlock()

	if decrypt {
		if transport.kms != nil {
			if response := transport.kms(call, request); response != nil {
				return response, nil
			}
		}
		return awsStubResponse(request, http.StatusOK, awsStubBody(awsStubPlaintext())), nil
	}
	if transport.sts != nil {
		if response := transport.sts(call, request); response != nil {
			return response, nil
		}
	}
	return awsStubResponse(request, http.StatusOK, awsStubBody(awsStubCredentials(transport.now().Add(time.Hour)))), nil
}

func (transport *awsStubTransport) counts() (sts, kms int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.stsCalls, transport.kmsCalls
}

// awsStubClient is a client over the stub transport with a mutable clock and a
// counted token-file reader.
type awsStubClient struct {
	*AWSKMS
	transport *awsStubTransport
	now       time.Time
	opens     int
}

func newAWSStubClient(t *testing.T) *awsStubClient {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(awsStubToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	stub := &awsStubClient{now: time.Date(2026, time.November, 11, 9, 30, 0, 0, time.UTC)}
	clock := func() time.Time { return stub.now }
	client, err := NewAWSKMS(AWSKMSConfig{
		Region:               awsStubRegion,
		RoleARN:              awsStubRole,
		WebIdentityTokenFile: tokenFile,
		Now:                  clock,
	})
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	stub.AWSKMS = client
	stub.transport = &awsStubTransport{now: clock}
	client.client.Transport = stub.transport
	open := client.openTokenFile
	client.openTokenFile = func(name string) (io.ReadCloser, error) {
		stub.opens++
		if name != tokenFile {
			t.Errorf("token reader opened %q; want the configured token file", name)
		}
		return open(name)
	}
	return stub
}

func (stub *awsStubClient) requireIO(t *testing.T, opens, sts, kms int) {
	t.Helper()
	gotSTS, gotKMS := stub.transport.counts()
	if stub.opens != opens || gotSTS != sts || gotKMS != kms {
		t.Fatalf("token-file reads, STS exchanges, KMS requests = %d, %d, %d; want %d, %d, %d",
			stub.opens, gotSTS, gotKMS, opens, sts, kms)
	}
}

// TestAWSKMSChecksTheRegionBeforeAnyCredentialIO counts the I/O directly: a
// reference that names another region, or is not an AWS reference, is refused
// with zero token-file reads, zero exchanges and zero KMS requests.
func TestAWSKMSChecksTheRegionBeforeAnyCredentialIO(t *testing.T) {
	stub := newAWSStubClient(t)
	for _, reference := range []string{
		"aws-kms:arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:111122223333:alias/kodama-kek",
		"aws-kms:alias/has space",
		"aws-kms:not-a-reference",
		"scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
	} {
		plaintext, err := stub.Decrypt(context.Background(), secret.KeyRef(reference), []byte("blob"))
		if err == nil {
			t.Fatalf("reference %q returned %d plaintext bytes", reference, len(plaintext))
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("reference %q fault class = %q (fault %v); want %q", reference, class, ok, fault.InvalidInput)
		}
	}
	stub.requireIO(t, 0, 0, 0)
	if stub.cached != nil {
		t.Fatal("a refused reference left credentials behind")
	}
}

// TestAWSKMSReadsTheTokenFileOncePerExchange counts the token-file reads: one
// per exchange and none while the cached credentials are in use, so the token
// is never held between exchanges.
func TestAWSKMSReadsTheTokenFileOncePerExchange(t *testing.T) {
	stub := newAWSStubClient(t)
	for attempt := range 3 {
		if _, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob")); err != nil {
			t.Fatalf("Decrypt %d: %v", attempt, err)
		}
	}
	stub.requireIO(t, 1, 1, 3)

	stub.now = stub.now.Add(time.Hour)
	if _, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob")); err != nil {
		t.Fatalf("Decrypt after expiry: %v", err)
	}
	stub.requireIO(t, 2, 2, 4)

	// A retry after refused credentials is one more exchange and one more read.
	stub.transport.kms = func(call int, request *http.Request) *http.Response {
		if call != 5 {
			return nil
		}
		response := awsStubResponse(request, http.StatusBadRequest, awsStubBody(`{}`))
		response.Header.Set("X-Amzn-Errortype", "ExpiredTokenException")
		return response
	}
	if _, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob")); err != nil {
		t.Fatalf("Decrypt with a refused credential: %v", err)
	}
	stub.requireIO(t, 3, 3, 6)
}

// TestAWSKMSDoesNotCacheAFailedExchange looks at the cache itself: a refused
// exchange, a malformed answer and a token-file fault each leave it empty.
func TestAWSKMSDoesNotCacheAFailedExchange(t *testing.T) {
	failures := map[string]func(stub *awsStubClient){
		"refused": func(stub *awsStubClient) {
			stub.transport.sts = func(_ int, request *http.Request) *http.Response {
				return awsStubResponse(request, http.StatusForbidden, awsStubBody(`<ErrorResponse><Error><Code>AccessDenied</Code></Error></ErrorResponse>`))
			}
		},
		"malformed": func(stub *awsStubClient) {
			stub.transport.sts = func(_ int, request *http.Request) *http.Response {
				return awsStubResponse(request, http.StatusOK, awsStubBody(`<AssumeRoleWithWebIdentityResponse/>`))
			}
		},
		"already expiring": func(stub *awsStubClient) {
			stub.transport.sts = func(_ int, request *http.Request) *http.Response {
				return awsStubResponse(request, http.StatusOK, awsStubBody(awsStubCredentials(stub.now.Add(time.Minute))))
			}
		},
		"token file": func(stub *awsStubClient) {
			stub.openTokenFile = func(string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
		},
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			stub := newAWSStubClient(t)
			fail(stub)
			if _, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob")); err == nil {
				t.Fatal("failed exchange returned no error")
			}
			if stub.cached != nil {
				t.Fatal("a failed exchange was cached")
			}
			if _, kms := stub.transport.counts(); kms != 0 {
				t.Fatalf("a failed exchange was followed by %d KMS requests", kms)
			}
		})
	}
}

// TestAWSKMSForgetKeepsReplacedCredentials pins the cache hand-over between
// concurrent callers: a caller whose credentials were refused drops them only
// while they are still the cached ones, never the fresh credentials another
// caller has already exchanged.
func TestAWSKMSForgetKeepsReplacedCredentials(t *testing.T) {
	stub := newAWSStubClient(t)
	refused := &awsRoleCredentials{accessKeyID: "ASIAREFUSED"}
	fresh := &awsRoleCredentials{accessKeyID: "ASIAFRESH"}

	stub.cached = fresh
	stub.forget(refused)
	if stub.cached != fresh {
		t.Fatal("forgetting refused credentials dropped the credentials that replaced them")
	}
	stub.forget(fresh)
	if stub.cached != nil {
		t.Fatal("forgetting the cached credentials left them cached")
	}
	stub.forget(fresh)
	if stub.cached != nil {
		t.Fatal("forgetting twice restored credentials")
	}
}

// awsEndlessReader serves 'x' forever up to a hard ceiling. The ceiling exists
// only so a mutant that removes a limit still terminates; a compliant reader
// stops after the bound and never reaches it.
type awsEndlessReader struct {
	served  int64
	ceiling int64
}

var errAWSEndlessCeiling = errors.New("endless reader ceiling reached")

func (reader *awsEndlessReader) Read(buffer []byte) (int, error) {
	if reader.served >= reader.ceiling {
		return 0, errAWSEndlessCeiling
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

func (*awsEndlessReader) Close() error { return nil }

// TestAWSReadWebIdentityTokenStopsAtTheBound proves the token reader takes at
// most two bytes over the 20000-byte token bound from an endless file (a
// trailing newline and the byte that proves the file is too large). Without
// the io.LimitReader the whole file would be buffered before it was refused.
func TestAWSReadWebIdentityTokenStopsAtTheBound(t *testing.T) {
	const bound = 20000 // the documented STS WebIdentityToken maximum, spelled as a literal
	stub := newAWSStubClient(t)
	reader := &awsEndlessReader{ceiling: bound * 8}
	stub.openTokenFile = func(string) (io.ReadCloser, error) { return reader, nil }
	token, err := stub.readWebIdentityToken()
	if err == nil {
		t.Fatalf("endless token file was accepted with %d bytes", len(token))
	}
	if token != "" {
		t.Fatalf("refusal returned %d token bytes", len(token))
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.Unauthenticated)
	}
	if reader.served > bound+2 {
		t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, bound+2)
	}
}

// TestAWSKMSResponseReadsStopAtTheBound proves both accepted bodies are read at
// most one byte over the 64 KiB bound from an endless response.
func TestAWSKMSResponseReadsStopAtTheBound(t *testing.T) {
	const bound = 64 << 10 // the documented bound, spelled as a literal
	t.Run("sts", func(t *testing.T) {
		stub := newAWSStubClient(t)
		reader := &awsEndlessReader{ceiling: bound * 4}
		stub.transport.sts = func(_ int, request *http.Request) *http.Response {
			return awsStubResponse(request, http.StatusOK, reader)
		}
		_, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob"))
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.DependencyUnavailable)
		}
		if reader.served > bound+1 {
			t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, bound+1)
		}
	})
	t.Run("kms", func(t *testing.T) {
		stub := newAWSStubClient(t)
		reader := &awsEndlessReader{ceiling: bound * 4}
		stub.transport.kms = func(_ int, request *http.Request) *http.Response {
			return awsStubResponse(request, http.StatusOK, reader)
		}
		plaintext, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob"))
		if plaintext != nil {
			t.Fatalf("refusal returned %d plaintext bytes", len(plaintext))
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.DependencyUnavailable)
		}
		if reader.served > bound+1 {
			t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, bound+1)
		}
	})
	t.Run("kms refusal", func(t *testing.T) {
		stub := newAWSStubClient(t)
		reader := &awsEndlessReader{ceiling: bound * 4}
		stub.transport.kms = func(_ int, request *http.Request) *http.Response {
			return awsStubResponse(request, http.StatusForbidden, reader)
		}
		_, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob"))
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("error class = %q (fault %v); want %q", class, ok, fault.Forbidden)
		}
		if reader.served > bound+1 {
			t.Fatalf("reader served %d bytes; the bound must stop it at %d", reader.served, bound+1)
		}
	})
}

// awsBlockingBody blocks in Read until the request context is cancelled, then
// returns the context error. A loopback server cannot signal the client that
// the body read has started, so this stub makes a cancellation that lands
// squarely inside the body read deterministic.
type awsBlockingBody struct {
	ctx context.Context
}

func (body *awsBlockingBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*awsBlockingBody) Close() error { return nil }

// TestAWSKMSContextCancellationDuringBodyRead proves a cancellation that
// arrives while an accepted response body is being read surfaces as the bare
// context error for both requests: errors.Is(err, context.Canceled) holds and
// the error is not a dependency_unavailable fault. Without the re-read of the
// context after the body read, the wrapped read failure would be classified as
// an unavailable dependency.
func TestAWSKMSContextCancellationDuringBodyRead(t *testing.T) {
	for _, endpoint := range []string{"sts", "kms"} {
		t.Run(endpoint, func(t *testing.T) {
			stub := newAWSStubClient(t)
			started := make(chan struct{}, 1)
			block := func(_ int, request *http.Request) *http.Response {
				started <- struct{}{}
				return awsStubResponse(request, http.StatusOK, &awsBlockingBody{ctx: request.Context()})
			}
			if endpoint == "sts" {
				stub.transport.sts = block
			} else {
				stub.transport.kms = block
			}

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, err := stub.Decrypt(ctx, secret.KeyRef(awsStubRef), []byte("blob"))
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

// TestAWSKMSWaitingCallerHonoursCancellation proves a caller that was cancelled
// while another caller held the exchange returns the context error instead of
// starting an exchange of its own.
func TestAWSKMSWaitingCallerHonoursCancellation(t *testing.T) {
	stub := newAWSStubClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	stub.mu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := stub.Decrypt(ctx, secret.KeyRef(awsStubRef), []byte("blob"))
		result <- err
	}()
	// The caller is past the entry check or about to be; either way it must
	// observe the cancellation no later than when it gets the lock.
	time.Sleep(20 * time.Millisecond)
	cancel()
	stub.mu.Unlock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting caller error = %v; want context.Canceled", err)
		}
		if class, ok := fault.ClassOf(err); ok {
			t.Fatalf("waiting caller carried fault class %q; want the bare context error", class)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting caller did not return")
	}
	stub.requireIO(t, 0, 0, 0)
}

// TestAWSKMSRefusesNonOKStatusWithAStubBody proves the status line decides the
// outcome even when a well-formed body is delivered: 201/202/204/302/304/307
// never yield credentials or plaintext. The stub is needed for 204/304 because
// a real HTTP response for those statuses has no body, which would make a
// narrow "accept 204/304" mutant look equivalent to a refusal.
func TestAWSKMSRefusesNonOKStatusWithAStubBody(t *testing.T) {
	for _, status := range []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusFound,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
	} {
		stub := newAWSStubClient(t)
		stub.transport.kms = func(_ int, request *http.Request) *http.Response {
			return awsStubResponse(request, status, awsStubBody(awsStubPlaintext()))
		}
		plaintext, err := stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob"))
		if err == nil {
			t.Fatalf("KMS status %d with a valid stub body returned %d plaintext bytes", status, len(plaintext))
		}
		if plaintext != nil {
			t.Fatalf("KMS status %d returned %d plaintext bytes", status, len(plaintext))
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("KMS status %d fault class = %q (fault %v); want %q", status, class, ok, fault.DependencyUnavailable)
		}

		stub = newAWSStubClient(t)
		stub.transport.sts = func(_ int, request *http.Request) *http.Response {
			return awsStubResponse(request, status, awsStubBody(awsStubCredentials(stub.now.Add(time.Hour))))
		}
		_, err = stub.Decrypt(context.Background(), secret.KeyRef(awsStubRef), []byte("blob"))
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("STS status %d fault class = %q (fault %v); want %q", status, class, ok, fault.DependencyUnavailable)
		}
		if stub.cached != nil {
			t.Fatalf("STS status %d with a valid stub body was cached as credentials", status)
		}
		stub.requireIO(t, 1, 1, 0)
	}
}

// TestAWSErrorTypeSanitising pins how a refusal's error name is found and
// cleaned: header first, then a JSON "__type" or "code", then an XML Code;
// everything up to a '#' and everything from a ':' is dropped.
func TestAWSErrorTypeSanitising(t *testing.T) {
	cases := []struct {
		name  string
		reply awsReply
		want  string
	}{
		{name: "header", reply: awsReply{errorType: "AccessDeniedException"}, want: "AccessDeniedException"},
		{name: "header uri suffix", reply: awsReply{errorType: "FooError:http://internal.amazon.com/coral/com.amazon.coral.validate/"}, want: "FooError"},
		{name: "header namespace", reply: awsReply{errorType: "aws.protocoltests.restjson#FooError"}, want: "FooError"},
		{name: "header namespace and suffix", reply: awsReply{errorType: "aws.protocoltests.restjson#FooError:http://internal.amazon.com/coral/com.amazon.coral.validate/"}, want: "FooError"},
		{name: "header padded", reply: awsReply{errorType: " FooError "}, want: "FooError"},
		{name: "header over body", reply: awsReply{errorType: "FooError", body: []byte(`{"__type":"BarError"}`)}, want: "FooError"},
		{name: "json type", reply: awsReply{body: []byte(`{"__type":"FooError","message":"m"}`)}, want: "FooError"},
		{name: "json namespaced type", reply: awsReply{body: []byte(`{"__type":"com.amazonaws.kms#FooError"}`)}, want: "FooError"},
		{name: "json code", reply: awsReply{body: []byte(`{"code":"FooError"}`)}, want: "FooError"},
		{name: "json type over code", reply: awsReply{body: []byte(`{"code":"BarError","__type":"FooError"}`)}, want: "FooError"},
		{name: "xml code", reply: awsReply{body: []byte(`<ErrorResponse><Error><Type>Sender</Type><Code>FooError</Code><Message>m</Message></Error></ErrorResponse>`)}, want: "FooError"},
		{name: "xml padded code", reply: awsReply{body: []byte("<ErrorResponse><Error><Code>\n  FooError\n</Code></Error></ErrorResponse>")}, want: "FooError"},
		{name: "nothing", reply: awsReply{}, want: ""},
		{name: "oversized body", reply: awsReply{oversized: true}, want: ""},
		{name: "json without a type", reply: awsReply{body: []byte(`{"message":"FooError"}`)}, want: ""},
		{name: "json number type", reply: awsReply{body: []byte(`{"__type":7}`)}, want: ""},
		{name: "xml without a code", reply: awsReply{body: []byte(`<ErrorResponse><Error><Message>FooError</Message></Error></ErrorResponse>`)}, want: ""},
		{name: "text", reply: awsReply{body: []byte("FooError")}, want: ""},
	}
	for _, testCase := range cases {
		if got := awsErrorType(testCase.reply); got != testCase.want {
			t.Fatalf("%s: error type = %q; want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestValidAWSRoleARNAndSessionName pins the two grammars on their own, so a
// mutant that loosens one is caught independently of the constructor.
func TestValidAWSRoleARNAndSessionName(t *testing.T) {
	for role, want := range map[string]bool{
		"arn:aws:iam::111122223333:role/kodama-kms":                 true,
		"arn:aws-cn:iam::111122223333:role/kodama-kms":              true,
		"arn:aws-us-gov:iam::111122223333:role/kodama-kms":          true,
		"arn:aws:iam::111122223333:role/path/to/Kodama_KMS+=,.@-":   true,
		"arn:aws:iam::111122223333:role/" + strings.Repeat("a", 64): true,
		"":                                                            false,
		"arn:aws:iam::111122223333:role/":                             false,
		"arn:aws:iam::111122223333:role":                              false,
		"arn:aws:iam::111122223333:role/a/":                           false,
		"arn:aws:iam::111122223333:role//a":                           false,
		"arn:aws:iam::111122223333:role/a b":                          false,
		"arn:aws:iam::111122223333:role/a\tb":                         false,
		"arn:aws:iam::111122223333:role/a\nb":                         false,
		"arn:aws:iam::111122223333:role/a:b":                          false,
		"arn:aws:iam::111122223333:role/a&b":                          false,
		"arn:aws:iam::111122223333:role/a%20b":                        false,
		"arn:aws:iam::111122223333:role/a\x7fb":                       false,
		"arn:aws:iam::111122223333:role/é":                            false,
		"arn:aws:iam::111122223333:user/a":                            false,
		"arn:aws:iam::1111222233:role/a":                              false,
		"arn:aws:iam:eu-west-3:111122223333:role/a":                   false,
		"arn:aws:kms::111122223333:role/a":                            false,
		"arn:other:iam::111122223333:role/a":                          false,
		"ARN:aws:iam::111122223333:role/a":                            false,
		"arn:aws:iam::111122223333:role/" + strings.Repeat("a", 2048): false,
	} {
		if got := validAWSRoleARN(role); got != want {
			t.Fatalf("validAWSRoleARN(%q) = %v; want %v", role, got, want)
		}
	}
	for name, want := range map[string]bool{
		"kodama":                true,
		"ab":                    true,
		"A_b+c=d,e.f@g-h9":      true,
		strings.Repeat("s", 64): true,
		"":                      false,
		"a":                     false,
		strings.Repeat("s", 65): false,
		"a b":                   false,
		"a/b":                   false,
		"a:b":                   false,
		"a\nb":                  false,
		"aé":                    false,
	} {
		if got := validAWSRoleSessionName(name); got != want {
			t.Fatalf("validAWSRoleSessionName(%q) = %v; want %v", name, got, want)
		}
	}
}
