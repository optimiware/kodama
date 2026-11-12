//go:build e2e

package harness

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestHarnessLogReassemblyDecodesBase64Data proves retained chunks carry
// base64 JSON data and reassemble into the exact stdout bytes.
func TestHarnessLogReassemblyDecodesBase64Data(t *testing.T) {
	raw := []byte(`{"chunks":[
		{"action_id":"act-1","stream":"stdout","offset":0,"sequence":1,"data":"aGVsbG8g"},
		{"action_id":"act-1","stream":"stdout","offset":6,"sequence":2,"data":"d29ybGQK"}
	],"truncated":false}`)
	var result api.ExecutionLogsReadResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode log result: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("log result is invalid: %v", err)
	}
	if string(result.Chunks[0].Data) != "hello " {
		t.Fatalf("chunk 0 data = %q; want the decoded base64 bytes", result.Chunks[0].Data)
	}
	assembled, err := ReassembleLog(result.Chunks, "act-1", "stdout")
	if err != nil {
		t.Fatalf("reassemble log: %v", err)
	}
	if string(assembled) != "hello world\n" {
		t.Fatalf("assembled log = %q; want %q", assembled, "hello world\n")
	}
}

// TestHarnessLogReassemblyOrdersByOffset proves chunks returned out of order
// are reassembled by offset, and a gap or foreign chunk is refused.
func TestHarnessLogReassemblyOrdersByOffset(t *testing.T) {
	second := api.LogChunk{ActionID: "act-1", Stream: "stdout", Offset: 5, Sequence: 2, Data: []byte("world")}
	first := api.LogChunk{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("hello")}
	assembled, err := ReassembleLog([]api.LogChunk{second, first}, "act-1", "stdout")
	if err != nil {
		t.Fatalf("reassemble out-of-order chunks: %v", err)
	}
	if string(assembled) != "helloworld" {
		t.Fatalf("assembled log = %q; want helloworld", assembled)
	}

	if _, err := ReassembleLog([]api.LogChunk{second}, "act-1", "stdout"); err == nil {
		t.Fatal("a log gap was accepted")
	}
	if _, err := ReassembleLog([]api.LogChunk{first}, "act-2", "stdout"); err == nil {
		t.Fatal("a chunk from another action was accepted")
	}
	if _, err := ReassembleLog([]api.LogChunk{first}, "act-1", "stderr"); err == nil {
		t.Fatal("a chunk from another stream was accepted")
	}
}
