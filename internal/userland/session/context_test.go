package session_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/session"
)

// tenByteRecord builds five messages of exactly ten bytes each
// (len(role)+len(content)), so byte limits are exact.
func tenByteRecord() session.Record {
	record := baseRecord()
	record.Messages = []session.Message{
		{Role: "user", Content: strings.Repeat("a", 6)},
		{Role: "tool", Content: strings.Repeat("b", 6)},
		{Role: "user", Content: strings.Repeat("c", 6)},
		{Role: "tool", Content: strings.Repeat("d", 6)},
		{Role: "user", Content: strings.Repeat("e", 6)},
	}
	return record
}

// TestContextWindowTrimsOldestDeterministically pins a bounded, deterministic
// history window: the newest messages survive in order and the dropped count is
// reported.
func TestContextWindowTrimsOldestDeterministically(t *testing.T) {
	record := tenByteRecord()
	limits := session.ContextLimits{MaxMessages: 2, MaxBytes: 1 << 20}

	window, err := record.Window(limits)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if window.Trimmed != 3 {
		t.Fatalf("Trimmed = %d; want 3", window.Trimmed)
	}
	if len(window.Messages) != 2 {
		t.Fatalf("window messages = %d; want 2", len(window.Messages))
	}
	if window.Messages[0].Content != strings.Repeat("d", 6) || window.Messages[1].Content != strings.Repeat("e", 6) {
		t.Fatalf("window kept %+v; want the two newest messages", window.Messages)
	}

	again, err := record.Window(limits)
	if err != nil {
		t.Fatalf("second Window: %v", err)
	}
	if !reflect.DeepEqual(window, again) {
		t.Fatalf("Window is not deterministic: %+v vs %+v", window, again)
	}
}

// TestContextWindowExactByteLimit pins byte accounting at the exact boundary.
func TestContextWindowExactByteLimit(t *testing.T) {
	record := tenByteRecord()

	cases := []struct {
		maxBytes int
		want     int
		trimmed  int
	}{
		{20, 2, 3},
		{19, 1, 4},
		{10, 1, 4},
	}
	for _, testCase := range cases {
		window, err := record.Window(session.ContextLimits{MaxMessages: 100, MaxBytes: testCase.maxBytes})
		if err != nil {
			t.Fatalf("Window(max_bytes=%d): %v", testCase.maxBytes, err)
		}
		if len(window.Messages) != testCase.want {
			t.Errorf("Window(max_bytes=%d) kept %d messages; want %d", testCase.maxBytes, len(window.Messages), testCase.want)
		}
		if window.Trimmed != testCase.trimmed {
			t.Errorf("Window(max_bytes=%d) Trimmed = %d; want %d", testCase.maxBytes, window.Trimmed, testCase.trimmed)
		}
	}
}

// TestContextWindowPreservesReferences pins that durable context references are
// never trimmed and never inlined when history is trimmed.
func TestContextWindowPreservesReferences(t *testing.T) {
	record := tenByteRecord()
	first := mustRef(t, "kodama-checkpoint/v1/ctx-0001")
	second := mustRef(t, "kodama-checkpoint/v1/ctx-0002")
	record.ContextRefs = []session.Ref{first, second}

	window, err := record.Window(session.ContextLimits{MaxMessages: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(window.Messages) != 1 {
		t.Fatalf("window messages = %d; want 1", len(window.Messages))
	}
	if !reflect.DeepEqual(window.Refs, []session.Ref{first, second}) {
		t.Fatalf("window refs = %v; want %v", window.Refs, []session.Ref{first, second})
	}
	if window.Messages[0].Content != strings.Repeat("e", 6) {
		t.Fatalf("window inlined referenced content %q", window.Messages[0].Content)
	}
}

// TestContextWindowRefusesInvalidLimits pins the refusal boundaries: invalid
// limits and a newest message that cannot fit.
func TestContextWindowRefusesInvalidLimits(t *testing.T) {
	record := tenByteRecord()
	for _, limits := range []session.ContextLimits{
		{MaxMessages: 0, MaxBytes: 10},
		{MaxMessages: 1, MaxBytes: 0},
		{MaxMessages: -1, MaxBytes: 10},
		{MaxMessages: 1, MaxBytes: -1},
	} {
		if window, err := record.Window(limits); err == nil {
			t.Errorf("Window(%+v) = %+v; want refusal", limits, window)
		}
	}

	oversized := baseRecord()
	oversized.Messages = []session.Message{{Role: "user", Content: strings.Repeat("x", 100)}}
	if window, err := oversized.Window(session.ContextLimits{MaxMessages: 10, MaxBytes: 50}); err == nil {
		t.Errorf("Window kept an oversized newest message: %+v", window)
	}
}

// TestContextWindowEmptyRecord pins that a record with no messages yields an
// empty window instead of failing.
func TestContextWindowEmptyRecord(t *testing.T) {
	record := baseRecord()
	record.Messages = nil
	window, err := record.Window(session.DefaultContextLimits())
	if err != nil {
		t.Fatalf("Window(empty): %v", err)
	}
	if len(window.Messages) != 0 || window.Trimmed != 0 {
		t.Fatalf("empty window = %+v", window)
	}
}

// TestDefaultContextLimits pins the default window bounds.
func TestDefaultContextLimits(t *testing.T) {
	limits := session.DefaultContextLimits()
	if limits.MaxMessages != 128 {
		t.Fatalf("default MaxMessages = %d; want 128", limits.MaxMessages)
	}
	if limits.MaxBytes != 256<<10 {
		t.Fatalf("default MaxBytes = %d; want 262144", limits.MaxBytes)
	}
}

// TestContextWindowAccountsForToolCorrelation pins byte accounting for the
// tool-call correlation members: one message costs its role and content plus the
// tool-call id and every call's id, name and arguments string. Counting only
// role and content would keep one message too many at the 24-byte boundary.
func TestContextWindowAccountsForToolCorrelation(t *testing.T) {
	record := baseRecord()
	assistant := session.Message{
		Role:      "assistant",
		ToolCalls: []session.ToolCall{{ID: "c1", Name: "repo", Arguments: `{}`}},
	}
	tool := session.Message{Role: "tool", Content: "ok", ToolCallID: "c1"}
	record.Messages = []session.Message{assistant, tool}

	for _, testCase := range []struct {
		maxBytes int
		want     int
		trimmed  int
	}{
		{25, 2, 0},
		{24, 1, 1},
	} {
		window, err := record.Window(session.ContextLimits{MaxMessages: 10, MaxBytes: testCase.maxBytes})
		if err != nil {
			t.Fatalf("Window(max_bytes=%d): %v", testCase.maxBytes, err)
		}
		if len(window.Messages) != testCase.want || window.Trimmed != testCase.trimmed {
			t.Errorf("Window(max_bytes=%d) kept %d (trimmed %d); want %d (trimmed %d)",
				testCase.maxBytes, len(window.Messages), window.Trimmed, testCase.want, testCase.trimmed)
		}
	}

	single := baseRecord()
	single.Messages = []session.Message{assistant}
	if window, err := single.Window(session.ContextLimits{MaxMessages: 10, MaxBytes: 16}); err == nil {
		t.Fatalf("Window kept correlation whose exact cost exceeds the bound: %+v", window)
	}
}

// TestContextWindowCopiesToolCorrelation pins that a trimmed window never
// aliases the record's nested tool-call slice.
func TestContextWindowCopiesToolCorrelation(t *testing.T) {
	record := baseRecord()
	record.Messages = []session.Message{{
		Role:      "assistant",
		ToolCalls: []session.ToolCall{{ID: "c1", Name: "repo", Arguments: `{"path":"README.md"}`}},
	}}
	window, err := record.Window(session.DefaultContextLimits())
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	window.Messages[0].ToolCalls[0].Arguments = `{"mutated":true}`
	if record.Messages[0].ToolCalls[0].Arguments != `{"path":"README.md"}` {
		t.Fatalf("window aliased the record's tool calls: %+v", record.Messages[0].ToolCalls)
	}
}
