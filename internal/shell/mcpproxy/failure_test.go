// Failure behavior: known unavailability is distinct from an ambiguous
// dispatched outcome, a valid tool error is a result, and the response body is
// always closed.
package mcpproxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestCallKnownUnavailability(t *testing.T) {
	body := newTrackedBody(nil)
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return body, &mcpproxy.UpstreamError{
			Class:      fault.DependencyUnavailable,
			Message:    "upstream is down",
			Dispatched: false,
		}
	})
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call hid a known upstream unavailability")
	}
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("unavailability returned a result: %+v", result)
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
	if body.closeCount() != 1 {
		t.Fatalf("response body closes = %d; want 1", body.closeCount())
	}
}

func TestCallAmbiguousDispatchedOutcome(t *testing.T) {
	body := newTrackedBody(nil)
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return body, &mcpproxy.UpstreamError{
			Class:      fault.InternalFailure,
			Message:    "connection reset after write",
			Dispatched: true,
		}
	})
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call hid an ambiguous dispatched outcome")
	}
	assertFaultClass(t, err, fault.UncertainExternalEffect)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("ambiguous outcome returned a result: %+v", result)
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
	if body.closeCount() != 1 {
		t.Fatalf("response body closes = %d; want 1", body.closeCount())
	}
}

func TestCallPlainTransportErrorIsUncertain(t *testing.T) {
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return nil, errors.New("connection reset")
	})
	proxy := fake.newProxy(t)

	if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
		t.Fatal("Call hid a transport error")
	} else {
		assertFaultClass(t, err, fault.UncertainExternalEffect)
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
}

func TestCallPreservesValidToolError(t *testing.T) {
	body := newTrackedBody([]byte(`{"content":{"message":"bad input"},"isError":true}`))
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return body, nil
	})
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err != nil {
		t.Fatalf("Call turned a valid tool error into a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("Call lost the tool-error flag")
	}
	var content struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatalf("tool-error content is not JSON: %v", err)
	}
	if content.Message != "bad input" {
		t.Fatalf("tool-error content message = %q; want bad input", content.Message)
	}
	if body.closeCount() != 1 {
		t.Fatalf("response body closes = %d; want 1", body.closeCount())
	}
	unit, _, ok := fake.store.lastUnit()
	if !ok || len(unit.Audit) == 0 {
		t.Fatal("tool error did not record usage")
	}
	detail := decodeDetail(t, unit.Audit[0].Detail)
	if detail["outcome"] != "tool_error" {
		t.Fatalf("usage outcome = %v; want tool_error", detail["outcome"])
	}
}

func TestCallMalformedResponseIsUncertain(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"invalid json", []byte(`not json`)},
		{"missing content", []byte(`{"isError":false}`)},
		{"unknown field", []byte(`{"content":"x","extra":true}`)},
		{"empty body", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := newTrackedBody(testCase.body)
			fake := newFakes()
			fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
				return body, nil
			})
			proxy := fake.newProxy(t)

			if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
				t.Fatalf("Call accepted a %s response", testCase.name)
			} else {
				assertFaultClass(t, err, fault.UncertainExternalEffect)
			}
			if fake.transport.callCount() != 1 {
				t.Fatalf("transport calls = %d; want 1", fake.transport.callCount())
			}
			if body.closeCount() != 1 {
				t.Fatalf("response body closes = %d; want 1", body.closeCount())
			}
		})
	}
}

func TestCallTimeoutBeforeDispatch(t *testing.T) {
	fake := newFakes()
	gate := newGate()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		if err := gate.wait(context.Background()); err != nil {
			return nil, err
		}
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: request.Scope}}, nil
	}
	proxy := fake.newProxy(t)

	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(context.Background(), readCallRequest())
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the secret-resolution barrier")
	}
	if !fake.clock.Fire(30 * time.Second) {
		t.Fatal("Call did not arm the configured timeout")
	}
	gate.releaseAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call proceeded after the pre-effect timeout")
		}
		assertFaultClass(t, err, fault.DeadlineExpired)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the pre-effect timeout")
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 after pre-effect timeout", fake.transport.callCount())
	}
}

func TestCallCancellationBeforeDispatch(t *testing.T) {
	fake := newFakes()
	gate := newGate()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		if err := gate.wait(context.Background()); err != nil {
			return nil, err
		}
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: request.Scope}}, nil
	}
	proxy := fake.newProxy(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(ctx, readCallRequest())
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the secret-resolution barrier")
	}
	cancel()
	gate.releaseAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call proceeded after cancellation")
		}
		assertFaultClass(t, err, fault.DeadlineExpired)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after cancellation")
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 after cancellation", fake.transport.callCount())
	}
}

func TestCallTimeoutAfterDispatch(t *testing.T) {
	fake := newFakes()
	barrier := newGate()
	fake.transport.setBarrier(barrier)
	proxy := fake.newProxy(t)

	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(context.Background(), readCallRequest())
		done <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the transport barrier")
	}
	if !fake.clock.Fire(30 * time.Second) {
		t.Fatal("Call did not arm the configured timeout")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call hid a post-dispatch timeout")
		}
		assertFaultClass(t, err, fault.UncertainExternalEffect)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the post-dispatch timeout")
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
}

func TestCallCancellationAfterDispatch(t *testing.T) {
	fake := newFakes()
	barrier := newGate()
	fake.transport.setBarrier(barrier)
	proxy := fake.newProxy(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(ctx, readCallRequest())
		done <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the transport barrier")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call hid a post-dispatch cancellation")
		}
		assertFaultClass(t, err, fault.UncertainExternalEffect)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after post-dispatch cancellation")
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
}

func TestCallBodyClosedOnBlockedReadCancellation(t *testing.T) {
	body := newBlockingBody()
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return body, nil
	})
	proxy := fake.newProxy(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(ctx, readCallRequest())
		done <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(2 * time.Second):
		cancel()
		_ = body.Close()
		t.Fatal("Call did not start reading the response body")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call hid a cancellation during the response read")
		}
		assertFaultClass(t, err, fault.UncertainExternalEffect)
	case <-time.After(2 * time.Second):
		_ = body.Close()
		t.Fatal("Call did not return after cancelling the response read")
	}
	if body.closeCount() != 1 {
		t.Fatalf("response body closes = %d; want 1", body.closeCount())
	}
}

func TestCallTransportErrorNeverLeaksCredential(t *testing.T) {
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return nil, &mcpproxy.UpstreamError{
			Class:      fault.DependencyUnavailable,
			Message:    "upstream rejected credential " + testSecret,
			Dispatched: false,
		}
	})
	proxy := fake.newProxy(t)

	if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
		t.Fatal("Call hid a transport error")
	} else if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("transport error leaked the credential: %v", err)
	}
	unit, _, ok := fake.store.lastUnit()
	if !ok {
		t.Fatal("failed call did not record usage")
	}
	for _, audit := range unit.Audit {
		if strings.Contains(audit.Detail, testSecret) {
			t.Fatalf("usage audit leaked the credential: %q", audit.Detail)
		}
	}
}
