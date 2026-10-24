package secretkeys

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gcpkms "cloud.google.com/go/kms/apiv1"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func gcpAgainst(t *testing.T, h http.Handler) *Cloud {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, err := gcpkms.NewKeyManagementRESTClient(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	client.CallOptions.Encrypt, client.CallOptions.Decrypt = nil, nil
	p, err := NewGCP(client, simGCPBase+"k1", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGCPClassification(t *testing.T) {
	for code, want := range map[codes.Code]error{
		codes.PermissionDenied: ErrDenied, codes.Unauthenticated: ErrDenied, codes.FailedPrecondition: ErrDenied,
		codes.NotFound:        ErrNotFound,
		codes.InvalidArgument: ErrCiphertext, codes.DataLoss: ErrCiphertext,
		codes.Unavailable: ErrUnavailable, codes.DeadlineExceeded: ErrUnavailable, codes.ResourceExhausted: ErrUnavailable,
		codes.Aborted: ErrUnavailable, codes.Internal: ErrUnavailable, codes.Unknown: ErrUnavailable,
	} {
		api, ok := apierror.FromError(status.Error(code, leak))
		if !ok {
			t.Fatal("not an API error")
		}
		if err := classifyGCP(context.Background(), api); err != want {
			t.Errorf("%s: got %v want %v", code, err, want)
		}
	}
	if err := classifyGCP(context.Background(), errors.New("dial: "+leak)); err != ErrUnavailable {
		t.Fatalf("transport failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api, _ := apierror.FromError(status.Error(codes.PermissionDenied, ""))
	if err := classifyGCP(ctx, api); err != ErrUnavailable {
		t.Fatalf("cancelled request: %v", err)
	}
}

func TestGCPWireContractAndMalformedReplies(t *testing.T) {
	binding := []byte{0, 1, 2, 'x', 255}
	s := &sim{}
	p := gcpAgainst(t, s.gcpHandler(t, simGCPBase+"k1"))
	w, err := p.Wrap(context.Background(), scopeKey, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Unwrap(context.Background(), w, binding); err != nil {
		t.Fatal(err)
	}
	log := s.log()
	if len(log) != 2 || log[0].Op != "encrypt" || log[1].Op != "decrypt" || log[0].Key != simGCPBase+"k1" || log[0].PTLen != 32 {
		t.Fatalf("requests %+v", log)
	}
	// The binding travels raw as additional authenticated data (base64 on the wire).
	for _, c := range log {
		if c.AAD != "AAECeP8=" {
			t.Fatalf("AAD on the wire %q", c.AAD)
		}
	}
	before := s.count()
	for _, n := range []int{0, 31, 33} {
		if _, err := p.Wrap(context.Background(), make([]byte, n), binding); !errors.Is(err, ErrInvalid) {
			t.Fatalf("length %d accepted: %v", n, err)
		}
	}
	if s.count() != before {
		t.Fatal("invalid plaintext reached the provider")
	}

	reply := func(body string) *Cloud {
		return gcpAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}
	if _, err := reply(`{"ciphertext":""}`).Wrap(context.Background(), scopeKey, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatalf("empty ciphertext: %v", err)
	}
	if _, err := reply(`{"ciphe`).Wrap(context.Background(), scopeKey, binding); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("truncated reply: %v", err)
	}
	for _, body := range []string{`{"plaintext":"AAAA"}`, `{}`} {
		key, err := reply(body).Unwrap(context.Background(), w, binding)
		if !errors.Is(err, ErrCiphertext) || key != nil {
			t.Fatalf("%s: key=%v err=%v", body, key != nil, err)
		}
	}
	if _, err := p.Unwrap(context.Background(), Wrapped{Provider: "gcp", Format: gcpFormat, KeyRef: simGCPBase + "k1", Ciphertext: bytes.Repeat([]byte{1}, 9000)}, binding); !errors.Is(err, ErrCiphertext) {
		t.Fatal("oversized ciphertext accepted")
	}
}
