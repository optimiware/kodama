//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func errorResponse(status int, code, message string) *http.Response {
	body, _ := json.Marshal(api.Error{Code: code, Message: message})
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{api.MediaType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func successResponse(t *testing.T, operation string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal result body: %v", err)
	}
	result, err := json.Marshal(api.Result{Operation: operation, Body: encoded})
	if err != nil {
		t.Fatalf("Marshal result: %v", err)
	}
	return &http.Response{
		StatusCode: api.HTTPStatusSuccess,
		Header:     http.Header{"Content-Type": []string{api.MediaType}},
		Body:       io.NopCloser(bytes.NewReader(result)),
	}
}

func TestTransportReadsRotatedMountedTokenOnEveryAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token-A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var authorizations []string
	var bodies [][]byte
	attempts := 0
	transport := &client.HTTPTransport{
		BaseURL: "https://factory.example",
		RoundTripper: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			authorizations = append(authorizations, request.Header.Get("Authorization"))
			body, _ := io.ReadAll(request.Body)
			bodies = append(bodies, body)
			if attempts == 1 {
				if err := os.WriteFile(path, []byte("token-B\n"), 0o600); err != nil {
					t.Errorf("rotate token: %v", err)
				}
				return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "transient"), nil
			}
			return successResponse(t, "resource.report", api.ReportResponse{
				VerificationID: "verification-1",
				EventID:        "event-1",
			}), nil
		}),
		Source: client.FileCredentialSource{Path: path},
		Retry:  client.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep:  func(time.Duration) {},
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Report(context.Background(), validReportRequest("verification-1"))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if response.EventID != "event-1" {
		t.Fatalf("response = %+v", response)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d; want 2", attempts)
	}
	if len(authorizations) != 2 || authorizations[0] != "Bearer token-A" || authorizations[1] != "Bearer token-B" {
		t.Fatalf("authorizations = %v; want the rotated mounted token on the retry", authorizations)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("retry body changed between attempts: %q vs %q", bodies[0], bodies[1])
	}
	if !bytes.Contains(bodies[0], []byte(`"idempotency_key":"source-event-1"`)) {
		t.Fatalf("retry body lost the idempotency key: %s", bodies[0])
	}
}

func TestTransportRetryPolicyMatrix(t *testing.T) {
	cases := []struct {
		name       string
		operation  string
		input      any
		hasKey     bool
		code       string
		wantStatus int
		wantCalls  int32
	}{
		{"read dependency", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, api.CodeDependencyUnavailable, http.StatusServiceUnavailable, 3},
		{"read internal", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, api.CodeInternalFailure, http.StatusInternalServerError, 3},
		{"read uncertain", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, api.CodeUncertainExternalEffect, http.StatusBadGateway, 1},
		{"read version conflict", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, api.CodeVersionConflict, http.StatusConflict, 1},
		{"mutation dependency with key", "resource.report", nil, true, api.CodeDependencyUnavailable, http.StatusServiceUnavailable, 3},
		{"mutation dependency without key", "resource.report", nil, false, api.CodeDependencyUnavailable, http.StatusServiceUnavailable, 1},
		{"mutation internal with key", "resource.report", nil, true, api.CodeInternalFailure, http.StatusInternalServerError, 1},
		{"mutation uncertain with key", "resource.report", nil, true, api.CodeUncertainExternalEffect, http.StatusBadGateway, 1},
		{"mutation changed content with key", "resource.report", nil, true, api.CodeChangedIdempotencyContent, http.StatusUnprocessableEntity, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := &client.HTTPTransport{
				BaseURL: "https://factory.example",
				RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return errorResponse(testCase.wantStatus, testCase.code, "failure"), nil
				}),
				Source: tokenSource(client.KindService, "service-token"),
				Retry:  client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:  func(time.Duration) {},
				Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
			}
			invocation := api.Invocation{Operation: testCase.operation}
			if testCase.input != nil {
				invocation.Request.Input = mustJSON(t, testCase.input)
			} else {
				invocation.Request.Input = mustJSON(t, validReportRequest("verification-1"))
			}
			if testCase.hasKey {
				invocation.Request.IdempotencyKey = "source-event-1"
			}
			_, err := transport.Invoke(context.Background(), invocation)
			if err == nil {
				t.Fatal("Invoke succeeded against a failing endpoint")
			}
			class, _ := fault.ClassFor(fault.Code(testCase.code))
			requireFaultClass(t, err, class)
			requireNoLeakedSecrets(t, testCase.name, []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
			if got := calls.Load(); got != testCase.wantCalls {
				t.Fatalf("attempts = %d; want %d", got, testCase.wantCalls)
			}
		})
	}
}

// countingReader wraps an io.Reader and records total bytes read.
type countingReader struct {
	reader io.Reader
	read   int
}

func (counter *countingReader) Read(buffer []byte) (int, error) {
	count, err := counter.reader.Read(buffer)
	counter.read += count
	return count, err
}

// failingReader returns an error after the response headers, modelling a
// connection dropped mid-body.
type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

// TestTransportRoundTripFailuresRespectMutationShape covers the ambiguous
// effect the HTTP-error matrix cannot: RoundTrip itself fails after the server
// may already have committed. A keyed mutation may be replayed with the same
// key, a keyless mutation must never be replayed, and a response body that
// cannot be read is an internal failure that is not replayed.
func TestTransportRoundTripFailuresRespectMutationShape(t *testing.T) {
	type failureMode int
	const (
		roundTripError failureMode = iota
		bodyReadError
	)
	cases := []struct {
		name      string
		operation string
		input     any
		hasKey    bool
		mode      failureMode
		wantCalls int32
		wantClass fault.Class
	}{
		{"read round-trip error retries", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, roundTripError, 3, fault.DependencyUnavailable},
		{"keyed mutation round-trip error retries", "resource.report", nil, true, roundTripError, 3, fault.DependencyUnavailable},
		{"keyless mutation round-trip error is not replayed", "resource.report", nil, false, roundTripError, 1, fault.DependencyUnavailable},
		{"read body read error is not replayed", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false, bodyReadError, 1, fault.InternalFailure},
		{"keyed mutation body read error is not replayed", "resource.report", nil, true, bodyReadError, 1, fault.InternalFailure},
		{"keyless mutation body read error is not replayed", "resource.report", nil, false, bodyReadError, 1, fault.InternalFailure},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			var bodies [][]byte
			transport := &client.HTTPTransport{
				BaseURL: "https://factory.example",
				RoundTripper: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					body, _ := io.ReadAll(request.Body)
					bodies = append(bodies, body)
					if testCase.mode == roundTripError {
						return nil, errors.New("connection reset by peer after commit")
					}
					return &http.Response{
						StatusCode: api.HTTPStatusSuccess,
						Header:     http.Header{"Content-Type": []string{api.MediaType}},
						Body:       io.NopCloser(failingReader{err: errors.New("connection reset while reading the body")}),
					}, nil
				}),
				Source: tokenSource(client.KindService, "service-token"),
				Retry:  client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:  func(time.Duration) {},
				Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
			}
			invocation := api.Invocation{Operation: testCase.operation}
			if testCase.input != nil {
				invocation.Request.Input = mustJSON(t, testCase.input)
			} else {
				invocation.Request.Input = mustJSON(t, validReportRequest("verification-1"))
			}
			if testCase.hasKey {
				invocation.Request.IdempotencyKey = "source-event-1"
			}
			_, err := transport.Invoke(context.Background(), invocation)
			requireFaultClass(t, err, testCase.wantClass)
			requireNoLeakedSecrets(t, testCase.name, []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
			if got := calls.Load(); got != testCase.wantCalls {
				t.Fatalf("attempts = %d; want %d", got, testCase.wantCalls)
			}
			if testCase.hasKey && testCase.mode == roundTripError {
				if len(bodies) != int(testCase.wantCalls) {
					t.Fatalf("recorded bodies = %d; want %d", len(bodies), testCase.wantCalls)
				}
				for index, body := range bodies {
					if !bytes.Equal(body, bodies[0]) {
						t.Fatalf("attempt %d body changed: %s vs %s", index, body, bodies[0])
					}
					if !bytes.Contains(body, []byte(`"idempotency_key":"source-event-1"`)) {
						t.Fatalf("attempt %d lost the idempotency key: %s", index, body)
					}
				}
			}
		})
	}
}

// TestTransportPreCancelledContextSendsNothing pins that a canceled context is
// refused before any credential read or RoundTrip call, for reads and
// mutations with or without a key.
func TestTransportPreCancelledContextSendsNothing(t *testing.T) {
	cases := []struct {
		name       string
		operation  string
		input      any
		idempotent bool
	}{
		{"read", "execution.inspect", map[string]string{"execution_id": "execution-1"}, false},
		{"keyed mutation", "resource.report", nil, true},
		{"keyless mutation", "resource.report", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := &client.HTTPTransport{
				BaseURL: "https://factory.example",
				RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return successResponse(t, testCase.operation, map[string]string{"id": "x"}), nil
				}),
				Source: tokenSource(client.KindService, "service-token"),
				Retry:  client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:  func(time.Duration) {},
				Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
			}
			invocation := api.Invocation{Operation: testCase.operation}
			if testCase.input != nil {
				invocation.Request.Input = mustJSON(t, testCase.input)
			} else {
				invocation.Request.Input = mustJSON(t, validReportRequest("verification-1"))
			}
			if testCase.idempotent {
				invocation.Request.IdempotencyKey = "source-event-1"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := transport.Invoke(ctx, invocation)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v; want context.Canceled", err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("attempts = %d; want 0", got)
			}
			requireNoLeakedSecrets(t, testCase.name, []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
		})
	}
}

func TestTransportTimeoutAndBoundedResponse(t *testing.T) {
	t.Run("context deadline", func(t *testing.T) {
		transport := &client.HTTPTransport{
			BaseURL: "https://factory.example",
			RoundTripper: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}),
			Source: tokenSource(client.KindService, "service-token"),
			Retry:  client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			Sleep:  func(time.Duration) {},
			Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := transport.Invoke(ctx, api.Invocation{
			Operation: "execution.inspect",
			Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v; want context deadline exceeded", err)
		}
		requireNoLeakedSecrets(t, "context deadline", []string{err.Error()}, secretRule{
			label: "service token", value: "service-token",
		})
	})

	t.Run("bounded response", func(t *testing.T) {
		exactInspect := api.ExecutionInspectResult{
			ExecutionID:   "execution-1",
			State:         "running",
			Version:       1,
			CurrentAction: 0,
			Actions: []api.ExecutionAction{{
				ActionID: "action-1",
				Index:    0,
				Name:     "work",
				Kind:     "container",
				State:    "running",
				Version:  1,
			}},
			Cleanup: api.ExecutionCleanup{State: "none"},
		}
		if err := exactInspect.Validate(); err != nil {
			t.Fatalf("exactInspect.Validate: %v", err)
		}
		exactPayload := mustJSON(t, api.Result{
			Operation: "execution.inspect",
			Body:      mustJSON(t, exactInspect),
		})
		if _, err := api.DecodeResult(exactPayload); err != nil {
			t.Fatalf("DecodeResult(exactPayload): %v", err)
		}
		exactLen := len(exactPayload)

		invocation := api.Invocation{
			Operation: "execution.inspect",
			Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
		}

		// Positive boundary: MaxResponseBytes == N accepts a valid response of
		// exact byte length N.
		exactCounter := &countingReader{reader: bytes.NewReader(exactPayload)}
		exactTransport := &client.HTTPTransport{
			BaseURL: "https://factory.example",
			RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: api.HTTPStatusSuccess,
					Header:     http.Header{"Content-Type": []string{api.MediaType}},
					Body:       io.NopCloser(exactCounter),
				}, nil
			}),
			Source:           tokenSource(client.KindService, "service-token"),
			Retry:            client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			Sleep:            func(time.Duration) {},
			Now:              func() time.Time { return time.Unix(1_700_000_000, 0) },
			MaxResponseBytes: exactLen,
		}
		result, err := exactTransport.Invoke(context.Background(), invocation)
		if err != nil {
			t.Fatalf("Invoke at exact MaxResponseBytes=%d: %v", exactLen, err)
		}
		if result.Operation != "execution.inspect" || exactCounter.read != exactLen {
			t.Fatalf("exact Invoke result=%+v read=%d; want execution.inspect and %d bytes", result, exactCounter.read, exactLen)
		}

		// Negative boundary: MaxResponseBytes == N-1 over a valid JSON payload
		// larger than N refuses as oversized while reading <= MaxResponseBytes+1.
		largeInspect := exactInspect
		largeInspect.Actions = []api.ExecutionAction{{
			ActionID: "action-1",
			Index:    0,
			Name:     "work",
			Kind:     "container",
			State:    "running",
			Version:  1,
			Outputs:  map[string]string{"summary": strings.Repeat("a", 2048)},
		}}
		if err := largeInspect.Validate(); err != nil {
			t.Fatalf("largeInspect.Validate: %v", err)
		}
		largePayload := mustJSON(t, api.Result{
			Operation: "execution.inspect",
			Body:      mustJSON(t, largeInspect),
		})
		if _, err := api.DecodeResult(largePayload); err != nil {
			t.Fatalf("DecodeResult(largePayload): %v", err)
		}
		if len(largePayload) <= exactLen {
			t.Fatalf("largePayload len = %d; want > %d", len(largePayload), exactLen)
		}

		maxBytes := exactLen - 1
		for _, payload := range [][]byte{exactPayload, largePayload} {
			counter := &countingReader{reader: bytes.NewReader(payload)}
			transport := &client.HTTPTransport{
				BaseURL: "https://factory.example",
				RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: api.HTTPStatusSuccess,
						Header:     http.Header{"Content-Type": []string{api.MediaType}},
						Body:       io.NopCloser(counter),
					}, nil
				}),
				Source:           tokenSource(client.KindService, "service-token"),
				Retry:            client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:            func(time.Duration) {},
				Now:              func() time.Time { return time.Unix(1_700_000_000, 0) },
				MaxResponseBytes: maxBytes,
			}
			_, err := transport.Invoke(context.Background(), invocation)
			requireFaultClass(t, err, fault.InternalFailure)
			if !strings.Contains(err.Error(), "oversized") {
				t.Fatalf("error = %q; want message containing \"oversized\"", err.Error())
			}
			if counter.read > maxBytes+1 {
				t.Fatalf("transport read %d bytes (payload %d); want <= %d", counter.read, len(payload), maxBytes+1)
			}
			requireNoLeakedSecrets(t, "bounded response", []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
		}
	})
}

func TestTransportNeverFollowsRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetHits.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	transport := &client.HTTPTransport{
		BaseURL:      redirect.URL,
		RoundTripper: redirect.Client().Transport,
		Source:       tokenSource(client.KindService, "service-token"),
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep:        func(time.Duration) {},
		Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	_, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "execution.inspect",
		Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
	})
	requireFaultClass(t, err, fault.InternalFailure)
	if targetHits.Load() != 0 {
		t.Fatalf("the transport followed the redirect to an unregistered target")
	}
	requireNoLeakedSecrets(t, "redirect refusal", []string{err.Error()}, secretRule{
		label: "service token", value: "service-token",
	})
}

func TestTransportErrorVocabularyMapping(t *testing.T) {
	for _, code := range api.ErrorCodes() {
		t.Run(code, func(t *testing.T) {
			status, ok := api.HTTPStatus(code)
			if !ok {
				t.Fatalf("no HTTP status for %q", code)
			}
			transport := &client.HTTPTransport{
				BaseURL: "https://factory.example",
				RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return errorResponse(status, code, "public failure"), nil
				}),
				Source: tokenSource(client.KindService, "service-token"),
				Retry:  client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:  func(time.Duration) {},
				Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
			}
			_, err := transport.Invoke(context.Background(), api.Invocation{
				Operation: "execution.inspect",
				Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
			})
			class, _ := fault.ClassFor(fault.Code(code))
			requireFaultClass(t, err, class)
			requireNoLeakedSecrets(t, code, []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
		})
	}
}

func TestTransportRejectsMismatchedStatusAndMalformedEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		response *http.Response
	}{
		{
			name: "status does not match code",
			response: func() *http.Response {
				body, _ := json.Marshal(api.Error{Code: api.CodeInvalidInput, Message: "bad"})
				return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{api.MediaType}}, Body: io.NopCloser(bytes.NewReader(body))}
			}(),
		},
		{
			name: "unknown result envelope member",
			response: func() *http.Response {
				body := []byte(`{"operation":"execution.inspect","body":{},"unexpected":true}`)
				return &http.Response{StatusCode: api.HTTPStatusSuccess, Header: http.Header{"Content-Type": []string{api.MediaType}}, Body: io.NopCloser(bytes.NewReader(body))}
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &client.HTTPTransport{
				BaseURL:      "https://factory.example",
				RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) { return testCase.response, nil }),
				Source:       tokenSource(client.KindService, "service-token"),
				Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Sleep:        func(time.Duration) {},
				Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
			}
			_, err := transport.Invoke(context.Background(), api.Invocation{
				Operation: "execution.inspect",
				Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
			})
			requireFaultClass(t, err, fault.InternalFailure)
			requireNoLeakedSecrets(t, testCase.name, []string{err.Error()}, secretRule{
				label: "service token", value: "service-token",
			})
		})
	}
}

// TestTransportRecoversAfterTransientFailures proves an exhausted retry budget
// does not poison the transport: a later invocation succeeds and still sends
// the credential.
func TestTransportRecoversAfterTransientFailures(t *testing.T) {
	var calls atomic.Int32
	transport := &client.HTTPTransport{
		BaseURL: "https://factory.example",
		RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) <= 3 {
				return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "transient"), nil
			}
			return successResponse(t, "execution.inspect", map[string]string{"execution_id": "execution-1", "status": "running"}), nil
		}),
		Source: tokenSource(client.KindService, "service-token"),
		Retry:  client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep:  func(time.Duration) {},
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	invocation := api.Invocation{
		Operation: "execution.inspect",
		Request:   api.Request{Input: mustJSON(t, map[string]string{"execution_id": "execution-1"})},
	}
	_, err := transport.Invoke(context.Background(), invocation)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	requireNoLeakedSecrets(t, "exhausted retry", []string{err.Error()}, secretRule{
		label: "service token", value: "service-token",
	})
	if _, err := transport.Invoke(context.Background(), invocation); err != nil {
		t.Fatalf("recovery after an exhausted retry budget: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("attempts = %d; want 4 (three failed retries then one success)", got)
	}
}
