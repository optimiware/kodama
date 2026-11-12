// Usage behavior: one attributable audit row through the existing store
// contract; raw content is off by default, an explicit opt-in retains redacted
// content for seven days, and credentials are never recorded.
package mcpproxy_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestCallRecordsAttributableUsage(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	request := readCallRequest()
	if _, err := proxy.Call(context.Background(), request); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	unit, scope, ok := fake.store.lastUnit()
	if !ok {
		t.Fatal("Call did not record usage")
	}
	if scope != testScope() {
		t.Fatalf("usage store scope = %+v; want %+v", scope, testScope())
	}
	if len(unit.Audit) != 1 {
		t.Fatalf("usage audit rows = %d; want 1", len(unit.Audit))
	}
	audit := unit.Audit[0]
	if audit.ID == "" {
		t.Fatal("usage audit has no identity")
	}
	if audit.Operation != "tool.invoke" {
		t.Fatalf("usage audit operation = %q; want tool.invoke", audit.Operation)
	}
	if audit.Actor != executionPrincipal().ID {
		t.Fatalf("usage audit actor = %q; want %q", audit.Actor, executionPrincipal().ID)
	}
	if audit.Scope != testScope() {
		t.Fatalf("usage audit scope = %+v; want %+v", audit.Scope, testScope())
	}
	if !audit.At.Equal(fixedNow) {
		t.Fatalf("usage audit time = %v; want %v", audit.At, fixedNow)
	}

	detail := decodeDetail(t, audit.Detail)
	if detail["server"] != "files" {
		t.Fatalf("usage server = %v; want files", detail["server"])
	}
	if detail["tool"] != "read_file" {
		t.Fatalf("usage tool = %v; want read_file", detail["tool"])
	}
	if detail["outcome"] != "ok" {
		t.Fatalf("usage outcome = %v; want ok", detail["outcome"])
	}
	if got := int(detail["request_bytes"].(float64)); got != len(request.Arguments) {
		t.Fatalf("usage request_bytes = %d; want %d", got, len(request.Arguments))
	}
	if got := int(detail["response_bytes"].(float64)); got <= 0 {
		t.Fatalf("usage response_bytes = %d; want a positive settled size", got)
	}
	if _, ok := detail["duration_ms"].(float64); !ok {
		t.Fatalf("usage duration_ms is missing or not numeric: %v", detail["duration_ms"])
	}
	if retained, ok := detail["content_retained"].(bool); !ok || retained {
		t.Fatalf("usage content_retained = %v; want false by default", detail["content_retained"])
	}
	if _, ok := detail["content"]; ok {
		t.Fatalf("usage recorded raw content without opt-in: %v", detail["content"])
	}
	if _, ok := detail["retain_until"]; ok {
		t.Fatalf("usage recorded a retention deadline without content: %v", detail["retain_until"])
	}
	if strings.Contains(audit.Detail, testSecret) {
		t.Fatalf("usage audit leaked a credential: %q", audit.Detail)
	}
}

func TestCallRetainsRedactedContentWhenOptedIn(t *testing.T) {
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return newTrackedBody([]byte(`{"content":{"echo":"MCP-SECRET-123"},"isError":false}`)), nil
	})
	config := fake.config()
	config.Retention = mcpproxy.Retention{RawContent: true}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New with raw retention returned error: %v", err)
	}

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if strings.Contains(string(result.Content), testSecret) {
		t.Fatalf("CallResult.Content leaked the echoed credential: %q", result.Content)
	}

	unit, _, ok := fake.store.lastUnit()
	if !ok || len(unit.Audit) != 1 {
		t.Fatal("opted-in call did not record usage")
	}
	detail := decodeDetail(t, unit.Audit[0].Detail)
	if retained, ok := detail["content_retained"].(bool); !ok || !retained {
		t.Fatalf("usage content_retained = %v; want true", detail["content_retained"])
	}
	if len(unit.Writes) != 1 {
		t.Fatalf("opted-in call wrote %d content rows; want 1", len(unit.Writes))
	}
	write := unit.Writes[0]
	if write.Table != store.ContentRecordsTable {
		t.Fatalf("raw content target = %q; want %q", write.Table, store.ContentRecordsTable)
	}
	content, ok := write.Values["response_body"].([]byte)
	if !ok {
		t.Fatalf("content response_body is missing or not bytes: %v", write.Values["response_body"])
	}
	if len(content) == 0 {
		t.Fatal("content response_body is empty; want the redacted tool content")
	}
	if strings.Contains(string(content), testSecret) {
		t.Fatalf("retained content leaked the credential: %q", content)
	}
	retainUntil, ok := write.Values["retain_until"].(time.Time)
	if !ok {
		t.Fatalf("content retain_until is missing: %v", write.Values["retain_until"])
	}
	want := fixedNow.Add(mcpproxy.DefaultRetention)
	if !retainUntil.Equal(want) {
		t.Fatalf("content retain_until = %v; want %v", retainUntil, want)
	}
}

func TestCallRetentionUsesConfiguredDuration(t *testing.T) {
	fake := newFakes()
	config := fake.config()
	config.Retention = mcpproxy.Retention{RawContent: true, Duration: time.Hour}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	unit, _, ok := fake.store.lastUnit()
	if !ok || len(unit.Audit) != 1 {
		t.Fatal("call did not record usage")
	}
	if len(unit.Writes) != 1 {
		t.Fatalf("content rows = %d; want 1", len(unit.Writes))
	}
	retainUntil, ok := unit.Writes[0].Values["retain_until"].(time.Time)
	if !ok {
		t.Fatalf("content retain_until is missing: %v", unit.Writes[0].Values["retain_until"])
	}
	if !retainUntil.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("content retain_until = %v; want %v", retainUntil, fixedNow.Add(time.Hour))
	}
}

// TestCallUsageFailureAfterDispatchIsUncertain pins that an audit failure after
// the upstream tool already ran is never reported as a retryable pre-effect
// class: the caller must not discard a real effect and replay it.
func TestCallUsageFailureAfterDispatchIsUncertain(t *testing.T) {
	fake := newFakes()
	fake.store.err = fault.New(fault.DependencyUnavailable, "store unavailable")
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call hid a usage-record failure")
	}
	assertFaultClass(t, err, fault.UncertainExternalEffect)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("usage failure returned a result: %+v", result)
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1 (never replayed)", fake.transport.callCount())
	}
	if fake.store.callCount() != 1 {
		t.Fatalf("usage writes = %d; want 1", fake.store.callCount())
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("usage failure leaked the credential: %v", err)
	}
}

func TestCallUsageContentIsJSONSerializable(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	unit, _, ok := fake.store.lastUnit()
	if !ok || len(unit.Audit) != 1 {
		t.Fatal("call did not record usage")
	}
	if !json.Valid([]byte(unit.Audit[0].Detail)) {
		t.Fatalf("usage detail is not valid JSON: %q", unit.Audit[0].Detail)
	}
}
