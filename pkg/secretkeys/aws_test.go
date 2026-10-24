package secretkeys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/smithy-go"
)

func awsAgainst(t *testing.T, h http.Handler) *Cloud {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := awskms.New(awskms.Options{
		Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDSIM", "secret-sim", ""), HTTPClient: srv.Client(),
	})
	p, err := NewAWS(client, simAWSKeyA, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAWSClassification(t *testing.T) {
	for code, want := range map[string]error{
		"InvalidCiphertextException": ErrCiphertext, "IncorrectKeyException": ErrCiphertext,
		"NotFoundException":     ErrNotFound,
		"AccessDeniedException": ErrDenied, "UnrecognizedClientException": ErrDenied, "ExpiredTokenException": ErrDenied,
		"DisabledException": ErrDenied, "KMSInvalidStateException": ErrDenied,
		"ThrottlingException": ErrUnavailable, "LimitExceededException": ErrUnavailable, "KMSInternalException": ErrUnavailable,
		"DependencyTimeoutException": ErrUnavailable, "KeyUnavailableException": ErrUnavailable,
		"SomethingNew": ErrUnavailable,
	} {
		err := classifyAWS(context.Background(), fmt.Errorf("op: %w", &smithy.GenericAPIError{Code: code, Message: leak}))
		if err != want {
			t.Errorf("%s: got %v want %v", code, err, want)
		}
	}
	if err := classifyAWS(context.Background(), errors.New("dial tcp: "+leak)); err != ErrUnavailable {
		t.Fatalf("transport failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := classifyAWS(ctx, &smithy.GenericAPIError{Code: "AccessDeniedException"}); err != ErrUnavailable {
		t.Fatalf("cancelled request: %v", err)
	}
}

func TestAWSWireContractAndMalformedReplies(t *testing.T) {
	binding := []byte("binding-bytes")
	var seen []simCall
	s := &sim{}
	p := awsAgainst(t, s.awsHandler(t, simAWSKeyA))
	w, err := p.Wrap(context.Background(), scopeKey, binding)
	if err != nil {
		t.Fatal(err)
	}
	seen = s.log()
	if len(seen) != 1 || seen[0].Op != "Encrypt" || seen[0].Key != simAWSKeyA || seen[0].PTLen != 32 || seen[0].AAD != bindingContext(binding) {
		t.Fatalf("encrypt request %+v", seen)
	}
	if bytes.Contains(w.Ciphertext, scopeKey) {
		t.Fatal("ciphertext contains the key")
	}
	if _, err := p.Unwrap(context.Background(), w, binding); err != nil {
		t.Fatal(err)
	}
	if seen = s.log(); seen[1].Op != "Decrypt" || seen[1].AAD != bindingContext(binding) {
		t.Fatalf("decrypt request %+v", seen[1])
	}

	// Invalid plaintext lengths never reach the provider.
	before := s.count()
	for _, n := range []int{0, 1, 31, 33, 64} {
		if _, err := p.Wrap(context.Background(), make([]byte, n), binding); !errors.Is(err, ErrInvalid) {
			t.Fatalf("length %d accepted: %v", n, err)
		}
	}
	if s.count() != before {
		t.Fatal("invalid plaintext reached the provider")
	}

	reply := func(body string) *Cloud {
		return awsAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_, _ = w.Write([]byte(body))
		}))
	}
	if _, err := reply(`{"CiphertextBlob":""}`).Wrap(context.Background(), scopeKey, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatalf("empty ciphertext: %v", err)
	}
	if _, err := reply(`{"Ciphe`).Wrap(context.Background(), scopeKey, binding); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("truncated reply: %v", err)
	}
	for _, body := range []string{`{"Plaintext":"AAAA"}`, `{"Plaintext":""}`, `{}`} {
		key, err := reply(body).Unwrap(context.Background(), w, binding)
		if !errors.Is(err, ErrCiphertext) || key != nil {
			t.Fatalf("%s: key=%v err=%v", body, key != nil, err)
		}
	}
	oversize := bytes.Repeat([]byte{1}, 9000)
	if _, err := p.Unwrap(context.Background(), Wrapped{Provider: "aws", Format: awsFormat, KeyRef: simAWSKeyA, Ciphertext: oversize}, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatal("oversized ciphertext accepted")
	}
}
