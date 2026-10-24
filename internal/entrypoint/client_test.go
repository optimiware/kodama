package entrypoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type fixtureCase struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/runtime/v1/fixtures/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fixtureCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c.Payload
		}
	}
	t.Fatalf("no fixture %q", name)
	return nil
}

func fixtureAs[T any, P interface {
	*T
	Validate() error
}](t *testing.T, name string) T {
	t.Helper()
	v, err := runtimev1.DecodeResponse[T, P](fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var fastRetry = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

func newTestClient(t *testing.T, h http.Handler, retry RetryPolicy) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "att_01", "", retry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func errorBody(code domain.ErrorCode, details string) []byte {
	return []byte(`{"error":{"code":"` + string(code) + `","message":"m","requestId":"req_1","details":` + details + `}}`)
}

func TestClientAdmitSendsFixedNonceAndStoresToken(t *testing.T) {
	var calls atomic.Int32
	var nonces []string
	var auth []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runtime/v1/attempts/att_01/admit" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		req, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](body)
		if err != nil {
			t.Errorf("request is not a valid strict AdmitRequest: %v", err)
		}
		nonces = append(nonces, req.Nonce)
		auth = append(auth, r.Header.Get("Authorization"))
		if calls.Add(1) < 3 {
			writeJSON(w, 503, errorBody(domain.CodeUnavailable, "{}"))
			return
		}
		writeJSON(w, 200, fixture(t, "admit_response_reader"))
	}), fastRetry)
	req := fixtureAs[runtimev1.AdmitRequest](t, "admit_request")
	resp, err := c.Admit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || nonces[0] != nonces[1] || nonces[1] != nonces[2] || nonces[0] != req.Nonce {
		t.Fatalf("calls=%d nonces=%v: the nonce must be reused verbatim on retry", calls.Load(), nonces)
	}
	for _, a := range auth {
		if a != "" {
			t.Fatalf("admit must not send a bearer token, got %q", a)
		}
	}
	if c.bearer() != resp.Token.Token || c.bearer() == "" {
		t.Fatal("token not stored")
	}
}

func TestClientSendsBearerAndRoutes(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		writeJSON(w, 200, fixture(t, "context_reader"))
	}), fastRetry)
	c.setToken("rt_secretsecretsecretsecret")
	if _, err := c.Context(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer rt_secretsecretsecretsecret" || gotPath != "/runtime/v1/attempts/att_01/context" || gotMethod != "GET" {
		t.Fatalf("auth=%q path=%q method=%q", gotAuth, gotPath, gotMethod)
	}
}

func TestClientRequiresTokenBeforeAuthenticatedCalls(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), fastRetry)
	_, err := c.Context(context.Background())
	var pe *ProtocolError
	if !errors.As(err, &pe) || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestClientRetriesOnlyTransientFailures(t *testing.T) {
	result := fixtureAs[runtimev1.ResultRequest](t, "result_failed_with_diagnostics")
	cases := []struct {
		name      string
		status    int
		body      []byte
		wantCalls int32
		check     func(t *testing.T, err error)
	}{
		{"503 exhausts retries", 503, errorBody(domain.CodeUnavailable, "{}"), 4, func(t *testing.T, err error) {
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("err = %T %v", err, err)
			}
		}},
		{"500 without body", 500, nil, 4, func(t *testing.T, err error) {
			if !isTransport(err) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"429", 429, errorBody(domain.CodeRateLimited, "{}"), 4, func(t *testing.T, err error) {
			if !isTransport(err) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"409 result_conflict is terminal", 409, errorBody(runtimev1.CodeResultConflict, "{}"), 1, func(t *testing.T, err error) {
			var api *APIError
			if !errors.As(err, &api) || api.Code != runtimev1.CodeResultConflict || api.Status != 409 || api.RequestID != "req_1" {
				t.Fatalf("err = %#v", err)
			}
		}},
		{"403 attempt_revoked is terminal", 403, errorBody(runtimev1.CodeAttemptRevoked, "{}"), 1, func(t *testing.T, err error) {
			if !isRevoked(err) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"422 hash mismatch is terminal", 422, errorBody(runtimev1.CodeResultHashMismatch, "{}"), 1, func(t *testing.T, err error) {
			if apiCode(err) != runtimev1.CodeResultHashMismatch {
				t.Fatalf("err = %v", err)
			}
		}},
		{"4xx with garbage body is terminal, empty code", 400, []byte("<html>"), 1, func(t *testing.T, err error) {
			var api *APIError
			if !errors.As(err, &api) || api.Code != "" || api.Status != 400 {
				t.Fatalf("err = %#v", err)
			}
		}},
		{"200 with invalid body is a protocol error, not retried", 200, []byte(`{"attemptId":"att_01"}`), 1, func(t *testing.T, err error) {
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T %v", err, err)
			}
		}},
		{"unexpected 2xx is a protocol error", 204, nil, 1, func(t *testing.T, err error) {
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T %v", err, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeJSON(w, tc.status, tc.body)
			}), fastRetry)
			c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
			_, err := c.SubmitResult(context.Background(), result)
			if err == nil {
				t.Fatal("expected error")
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
			tc.check(t, err)
		})
	}
}

func TestClientRetriesTransportErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) == 1 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close() // lost response: connection drops mid-request
			return
		}
		writeJSON(w, 200, fixture(t, "result_response"))
	}), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	resp, err := c.SubmitResult(context.Background(), fixtureAs[runtimev1.ResultRequest](t, "result_completed_reader"))
	if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("the identical payload (same resultHash) must be replayed: %q", bodies)
	}
}

func TestClientRetryAfterIsHonoredButCapped(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			writeJSON(w, 429, errorBody(domain.CodeRateLimited, "{}"))
			return
		}
		writeJSON(w, 200, fixture(t, "token_response"))
	}), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	start := time.Now()
	if _, err := c.RenewToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Retry-After was not capped by MaxDelay: %v", time.Since(start))
	}
}

func TestClientStopsRetryingOnContextCancel(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 503, errorBody(domain.CodeUnavailable, "{}"))
	}), RetryPolicy{MaxAttempts: 50, BaseDelay: 20 * time.Millisecond, MaxDelay: 20 * time.Millisecond})
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.RenewToken(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() >= 50 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestClientPublicationConflictCarriesObservedTip(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, errorBody(runtimev1.CodePublicationConflict, `{"observedTip":"3333333333333333333333333333333333333333"}`))
	}), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	_, err := c.ProposeSource(context.Background(), fixtureAs[runtimev1.SourceProposalRequest](t, "source_request"))
	var api *APIError
	if !errors.As(err, &api) || api.Code != runtimev1.CodePublicationConflict || api.Details["observedTip"] != "3333333333333333333333333333333333333333" {
		t.Fatalf("err = %#v", err)
	}
}

func TestClientProposeSourceStatusMustMatchHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		payload string
		ok      bool
		want    runtimev1.PublishStatus
	}{
		{"accepted 200", 200, "source_accepted", true, runtimev1.PublishAccepted},
		{"pending 202", 202, "source_pending", true, runtimev1.PublishPending},
		{"pending body on 200", 200, "source_pending", false, ""},
		{"accepted body on 202", 202, "source_accepted", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.status, fixture(t, tc.payload))
			}), fastRetry)
			c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
			resp, err := c.ProposeSource(context.Background(), fixtureAs[runtimev1.SourceProposalRequest](t, "source_request"))
			if (err == nil) != tc.ok || (tc.ok && resp.Status != tc.want) {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
		})
	}
}

func TestClientReserveArtifactAccepts200And201(t *testing.T) {
	for _, status := range []int{200, 201} {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, status, fixture(t, "reserve_response_reserved"))
		}), fastRetry)
		c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
		if _, err := c.ReserveArtifact(context.Background(), fixtureAs[runtimev1.ArtifactReserveRequest](t, "reserve_file")); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
	}
}

func TestClientRefusesInvalidRequestsWithoutSending(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := c.ResolveSecret(context.Background(), runtimev1.SecretResolveRequest{Ref: "bad", Version: 0}); err == nil {
		t.Fatal("invalid secret request accepted")
	}
	if _, err := c.CompleteArtifact(context.Background(), "../x", runtimev1.ArtifactCompleteRequest{}); err == nil {
		t.Fatal("invalid artifact ID accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("invalid requests reached the server")
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := c.Context(context.Background()); err == nil || leaked.Load() != 0 {
		t.Fatalf("err=%v leaked=%d: the bearer token must not follow a redirect", err, leaked.Load())
	}
}

func TestClientLogsNeverContainTokenOrSecrets(t *testing.T) {
	var logs bytes.Buffer
	log := newDebugLogger(&logs)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 503, errorBody(domain.CodeUnavailable, "{}"))
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "att_01", "", fastRetry, log)
	c.setToken("rt_TOKENVALUETOKENVALUE")
	c.SubmitResult(context.Background(), fixtureAs[runtimev1.ResultRequest](t, "result_failed_with_diagnostics"))
	c.ResolveSecret(context.Background(), runtimev1.SecretResolveRequest{Ref: "secret://project/erp-token", Version: 2})
	if strings.Contains(logs.String(), "TOKENVALUE") {
		t.Fatalf("token leaked into logs: %s", logs.String())
	}
}

// ---- upload ----

func TestUploadSendsHeadersBodyAndNoBearer(t *testing.T) {
	payload := []byte("report body")
	var gotAuth, gotCT, gotMethod string
	var gotBody []byte
	var gotLen int64
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCT, gotMethod, gotLen = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Method, r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}), fastRetry)
	c.setToken("rt_aaaaaaaaaaaaaaaaaaaaaaaa")
	err := c.Upload(context.Background(), runtimev1.Upload{Method: "PUT", URL: srv.URL + "/obj?sig=abc", Headers: map[string]string{"Content-Type": "application/json", "Content-Length": "999", "Host": "evil"}, ExpiresAt: "2026-10-19T10:15:00Z"}, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" || gotCT != "application/json" || gotMethod != "PUT" || string(gotBody) != string(payload) || gotLen != int64(len(payload)) {
		t.Fatalf("auth=%q ct=%q method=%q body=%q len=%d", gotAuth, gotCT, gotMethod, gotBody, gotLen)
	}
}

func TestUploadRetriesTransientAndRestartsBody(t *testing.T) {
	payload := []byte("0123456789")
	var calls atomic.Int32
	var bodies []string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) < 3 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}), fastRetry)
	err := c.Upload(context.Background(), runtimev1.Upload{Method: "PUT", URL: srv.URL + "/o", Headers: map[string]string{}}, bytes.NewReader(payload), int64(len(payload)))
	if err != nil || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	for _, b := range bodies {
		if b != string(payload) {
			t.Fatalf("a retry resent a partial body: %q", bodies)
		}
	}
}

func TestUploadTerminalAndExhaustedErrors(t *testing.T) {
	var calls atomic.Int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
	}), fastRetry)
	err := c.Upload(context.Background(), runtimev1.Upload{Method: "PUT", URL: srv.URL + "/o?sig=SECRETSIG"}, bytes.NewReader(nil), 0)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 403 || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	srv.Close()
	err = c.Upload(context.Background(), runtimev1.Upload{Method: "PUT", URL: srv.URL + "/o?sig=SECRETSIG"}, bytes.NewReader(nil), 0)
	if !isTransport(err) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRETSIG") {
		t.Fatalf("signed URL leaked in error: %v", err)
	}
}

// uploadAndVerify mirrors what the orchestrator does; the digest comes from the
// local file, never from the server.
func TestHashFileMatchesSHA256(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "h")
	f.WriteString("hello")
	sum := sha256.Sum256([]byte("hello"))
	d, err := hashFile(f, 5)
	if err != nil || string(d) != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("d=%v err=%v", d, err)
	}
	if _, err := hashFile(f, 6); err == nil {
		t.Fatal("size mismatch must fail")
	}
}
