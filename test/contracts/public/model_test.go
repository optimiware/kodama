package publiccontract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestChatCompletionSubsetIsExplicitNonStreaming pins the OpenAI-
// compatible completion subset version and the non-streaming choice: stream:true
// refuses because the subset declares no chunk schema, end marker or stream
// limits.
func TestChatCompletionSubsetIsExplicitNonStreaming(t *testing.T) {
	if api.ModelSubsetVersion != "openai-chat-completions.v1" {
		t.Fatalf("ModelSubsetVersion = %q; want openai-chat-completions.v1", api.ModelSubsetVersion)
	}

	request, err := api.DecodeChatCompletionRequest([]byte(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"temperature":0.5}`))
	if err != nil {
		t.Fatalf("non-streaming request refused: %v", err)
	}
	if request.Stream {
		t.Fatal("decoded request reports stream=true; the subset is non-streaming")
	}

	if _, err := api.DecodeChatCompletionRequest([]byte(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)); err == nil {
		t.Fatal("DecodeChatCompletionRequest accepted stream:true; want refusal")
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}
	if err := (api.ChatCompletionRequest{
		Model:    "fixture-model",
		Messages: []api.ChatMessage{{Role: "user", Content: "hello"}},
		Stream:   true,
	}).Validate(); err == nil {
		t.Fatal("ChatCompletionRequest.Validate accepted stream:true; want refusal")
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var roundTripped api.ChatCompletionRequest
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, request) {
		t.Fatalf("request round trip = %+v; want %+v", roundTripped, request)
	}

	response := api.ChatCompletionResponse{
		ID:    "chat-1",
		Model: "fixture-model",
		Choices: []api.ChatChoice{{
			Index:        0,
			Message:      api.ChatMessage{Role: "assistant", Content: "hi"},
			FinishReason: "stop",
		}},
		Usage: api.ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("valid response refused: %v", err)
	}
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var roundTrippedResponse api.ChatCompletionResponse
	if err := json.Unmarshal(encodedResponse, &roundTrippedResponse); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if err := roundTrippedResponse.Validate(); err != nil {
		t.Fatalf("round-tripped response refused: %v", err)
	}

	refusals := []string{
		`{"model":"fixture-model","messages":[{"role":"user","content":"x"}],"n":2}`,
		`{"model":"","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"fixture-model","messages":[]}`,
		`{"model":"fixture-model","messages":[{"role":"narrator","content":"x"}]}`,
		`{"model":"fixture-model","messages":[{"role":"user","content":"x"}],"max_tokens":` + strings.Repeat("9", 8) + `}`,
		`{"model":"fixture-model","messages":[{"role":"user","content":"x"}],"temperature":2.5}`,
	}
	for _, body := range refusals {
		if _, err := api.DecodeChatCompletionRequest([]byte(body)); err == nil {
			t.Errorf("DecodeChatCompletionRequest(%s) was accepted; want refusal", body)
		}
	}
}
