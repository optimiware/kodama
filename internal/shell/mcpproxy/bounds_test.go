// Bound behavior: the exact declared 1 MiB request/response limits are accepted
// and one byte more is refused before (request) or after (response) the effect.
package mcpproxy_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
)

func TestCallRequestBound(t *testing.T) {
	limit := mcpproxy.DefaultBounds.MaxRequestBytes

	accepted := jsonStringOfSize(limit)
	fake := newFakes()
	proxy := fake.newProxy(t)
	request := mcpproxy.CallRequest{
		Credential: testToken,
		Server:     "files",
		Tool:       "echo",
		Arguments:  accepted,
	}
	result, err := proxy.Call(context.Background(), request)
	if err != nil {
		t.Fatalf("Call refused the exact request bound: %v", err)
	}
	if result.IsError {
		t.Fatal("Call reported the exact-bound request as a tool error")
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1", fake.transport.callCount())
	}

	over := jsonStringOfSize(limit + 1)
	fake = newFakes()
	proxy = fake.newProxy(t)
	request.Arguments = over
	result, err = proxy.Call(context.Background(), request)
	if err == nil {
		t.Fatal("Call accepted a request one byte over the bound")
	}
	assertFaultClass(t, err, fault.InvalidInput)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("oversized request returned a result: %+v", result)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

func TestCallResponseBound(t *testing.T) {
	limit := mcpproxy.DefaultBounds.MaxResponseBytes

	accepted := jsonResponseOfSize(limit)
	body := newTrackedBody(accepted)
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return body, nil
	})
	proxy := fake.newProxy(t)
	result, err := proxy.Call(context.Background(), readCallRequest())
	if err != nil {
		t.Fatalf("Call refused the exact response bound: %v", err)
	}
	if result.IsError {
		t.Fatal("Call reported the exact-bound response as a tool error")
	}
	if body.closeCount() != 1 {
		t.Fatalf("response body closes = %d; want 1", body.closeCount())
	}

	over := newTrackedBody(jsonResponseOfSize(limit + 1))
	fake = newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return over, nil
	})
	proxy = fake.newProxy(t)
	result, err = proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted a response one byte over the bound")
	}
	assertFaultClass(t, err, fault.UncertainExternalEffect)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("oversized response returned a result: %+v", result)
	}
	if over.closeCount() != 1 {
		t.Fatalf("oversized response body closes = %d; want 1", over.closeCount())
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
}

func TestCallHonoursConfiguredSmallerBounds(t *testing.T) {
	const custom = 1024
	fake := newFakes()
	config := fake.config()
	config.Bounds = mcpproxy.Bounds{MaxRequestBytes: custom, MaxResponseBytes: custom}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New with smaller bounds returned error: %v", err)
	}

	request := mcpproxy.CallRequest{
		Credential: testToken,
		Server:     "files",
		Tool:       "echo",
		Arguments:  jsonStringOfSize(custom),
	}
	if _, err := proxy.Call(context.Background(), request); err != nil {
		t.Fatalf("Call refused the exact custom request bound: %v", err)
	}

	request.Arguments = jsonStringOfSize(custom + 1)
	if _, err := proxy.Call(context.Background(), request); err == nil {
		t.Fatal("Call ignored the configured request bound")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
}

func jsonStringOfSize(size int) json.RawMessage {
	const prefix = `{"text":"`
	const suffix = `"}`
	if size < len(prefix)+len(suffix) {
		panic("jsonStringOfSize size is too small")
	}
	return json.RawMessage(prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix)
}

func jsonResponseOfSize(size int) []byte {
	const prefix = `{"content":"`
	const suffix = `","isError":false}`
	if size < len(prefix)+len(suffix) {
		panic("jsonResponseOfSize size is too small")
	}
	return []byte(prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix)
}
