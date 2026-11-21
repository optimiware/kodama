package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodema/api"
)

const (
	// maxLLMProxyUsageSSELineBytes caps the per-line buffer used while scanning
	// streaming SSE events for model and token usage metadata.
	maxLLMProxyUsageSSELineBytes = 512 << 10
	// maxLLMProxyUsageJSONBytes caps the non-streaming JSON body copy kept for
	// token usage extraction. Larger responses still stream unchanged to the
	// caller without unbounded memory growth.
	maxLLMProxyUsageJSONBytes = 4 << 20
	// maxLLMProxyModelNameBytes bounds stored model identifiers.
	maxLLMProxyModelNameBytes = 256
)

type llmUsagePayload struct {
	Model    string          `json:"model"`
	Usage    *llmUsageCounts `json:"usage"`
	Message  *llmNestedBody  `json:"message"`
	Response *llmNestedBody  `json:"response"`
}

type llmNestedBody struct {
	Model string          `json:"model"`
	Usage *llmUsageCounts `json:"usage"`
}

type llmUsageCounts struct {
	PromptTokens             *int `json:"prompt_tokens"`
	CompletionTokens         *int `json:"completion_tokens"`
	TotalTokens              *int `json:"total_tokens"`
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

// llmProxyUsageTracker extracts model and token usage from streamed SSE events
// or bounded non-streaming JSON responses for OpenAI-compatible (Chat
// Completions and Responses) and Anthropic Messages formats.
type llmProxyUsageTracker struct {
	sse           bool
	jsonMode      bool
	lineBuf       []byte
	lineOversized bool
	jsonBuf       []byte
	jsonOversized bool

	model        string
	inputTokens  int
	outputTokens int
	hasUsage     bool
}

func newLLMProxyUsageTracker(contentType string) *llmProxyUsageTracker {
	switch {
	case isEventStream(contentType):
		return &llmProxyUsageTracker{sse: true}
	case isLLMProxyJSONContentType(contentType):
		return &llmProxyUsageTracker{jsonMode: true}
	default:
		return &llmProxyUsageTracker{}
	}
}

func isLLMProxyJSONContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// Observe inspects one streamed response chunk without blocking or modifying
// bytes written to the caller.
func (t *llmProxyUsageTracker) Observe(chunk []byte) {
	if t == nil || len(chunk) == 0 {
		return
	}
	if t.sse {
		t.observeSSEChunk(chunk)
		return
	}
	if t.jsonMode && !t.jsonOversized {
		if len(t.jsonBuf)+len(chunk) > maxLLMProxyUsageJSONBytes {
			t.jsonOversized = true
			t.jsonBuf = nil
			return
		}
		t.jsonBuf = append(t.jsonBuf, chunk...)
	}
}

func (t *llmProxyUsageTracker) observeSSEChunk(chunk []byte) {
	remaining := chunk
	for len(remaining) > 0 {
		nl := bytes.IndexByte(remaining, '\n')
		if nl < 0 {
			if !t.lineOversized {
				if len(t.lineBuf)+len(remaining) > maxLLMProxyUsageSSELineBytes {
					t.lineOversized = true
					t.lineBuf = nil
				} else {
					t.lineBuf = append(t.lineBuf, remaining...)
				}
			}
			return
		}
		part := remaining[:nl]
		remaining = remaining[nl+1:]
		if t.lineOversized {
			t.lineOversized = false
			t.lineBuf = nil
			continue
		}
		if len(t.lineBuf) == 0 {
			if len(part) <= maxLLMProxyUsageSSELineBytes {
				t.observeSSELine(part)
			}
			continue
		}
		if len(t.lineBuf)+len(part) <= maxLLMProxyUsageSSELineBytes {
			t.lineBuf = append(t.lineBuf, part...)
			t.observeSSELine(t.lineBuf)
		}
		t.lineBuf = t.lineBuf[:0]
	}
}

func (t *llmProxyUsageTracker) observeSSELine(line []byte) {
	line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte{'\r'}))
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(payload, []byte(`"usage"`)) && !bytes.Contains(payload, []byte(`"model"`)) {
		return
	}
	t.observeJSONPayload(payload)
}

// Finish finalizes any buffered line/body and returns the extracted model name,
// normalized ChatUsage (with TotalTokens == PromptTokens + CompletionTokens),
// and whether token counts were observed.
func (t *llmProxyUsageTracker) Finish() (string, api.ChatUsage, bool) {
	if t == nil {
		return "", api.ChatUsage{}, false
	}
	if t.sse && !t.lineOversized && len(t.lineBuf) > 0 {
		t.observeSSELine(t.lineBuf)
		t.lineBuf = nil
	}
	if t.jsonMode && !t.jsonOversized && len(t.jsonBuf) > 0 {
		t.observeJSONPayload(t.jsonBuf)
		t.jsonBuf = nil
	}
	if !t.hasUsage {
		return t.model, api.ChatUsage{}, false
	}
	usage := api.ChatUsage{
		PromptTokens:     t.inputTokens,
		CompletionTokens: t.outputTokens,
		TotalTokens:      t.inputTokens + t.outputTokens,
	}
	if err := usage.Validate(); err != nil {
		return t.model, api.ChatUsage{}, false
	}
	return t.model, usage, true
}

func (t *llmProxyUsageTracker) observeJSONPayload(payload []byte) {
	var parsed llmUsagePayload
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return
	}
	if model := sanitizeLLMModelName(parsed.Model); model != "" {
		t.model = model
	}
	if parsed.Message != nil {
		if model := sanitizeLLMModelName(parsed.Message.Model); model != "" {
			t.model = model
		}
		t.applyCounts(parsed.Message.Usage)
	}
	if parsed.Response != nil {
		if model := sanitizeLLMModelName(parsed.Response.Model); model != "" {
			t.model = model
		}
		t.applyCounts(parsed.Response.Usage)
	}
	t.applyCounts(parsed.Usage)
}

func (t *llmProxyUsageTracker) applyCounts(counts *llmUsageCounts) {
	if counts == nil {
		return
	}
	if counts.PromptTokens != nil && *counts.PromptTokens >= 0 {
		t.inputTokens = *counts.PromptTokens
		t.hasUsage = true
	}
	if counts.CompletionTokens != nil && *counts.CompletionTokens >= 0 {
		t.outputTokens = *counts.CompletionTokens
		t.hasUsage = true
	}
	if counts.InputTokens != nil || counts.CacheCreationInputTokens != nil || counts.CacheReadInputTokens != nil {
		in := 0
		valid := true
		for _, ptr := range []*int{counts.InputTokens, counts.CacheCreationInputTokens, counts.CacheReadInputTokens} {
			if ptr == nil {
				continue
			}
			if *ptr < 0 {
				valid = false
				break
			}
			in += *ptr
		}
		if valid {
			if in > 0 || !t.hasUsage {
				t.inputTokens = in
			}
			t.hasUsage = true
		}
	}
	if counts.OutputTokens != nil && *counts.OutputTokens >= 0 {
		t.outputTokens = *counts.OutputTokens
		t.hasUsage = true
	}
}

func extractRequestedModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return sanitizeLLMModelName(payload.Model)
}

func sanitizeLLMModelName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !utf8.ValidString(trimmed) {
		return ""
	}
	if strings.ContainsAny(trimmed, "\r\n\x00") {
		return ""
	}
	if len(trimmed) > maxLLMProxyModelNameBytes {
		trimmed = trimmed[:maxLLMProxyModelNameBytes]
		for !utf8.ValidString(trimmed) && len(trimmed) > 0 {
			trimmed = trimmed[:len(trimmed)-1]
		}
	}
	return trimmed
}

func formatProxyUsageModels(providerName, requestedModel, responseModel string) (modelLabel, upstreamModel string) {
	effective := sanitizeLLMModelName(responseModel)
	if effective == "" {
		effective = sanitizeLLMModelName(requestedModel)
	}
	if effective == "" {
		return providerName, providerName
	}
	if effective == providerName {
		return providerName, providerName
	}
	return providerName + "/" + effective, effective
}

// ensureOpenAIStreamIncludeUsage injects `"stream_options": {"include_usage": true}`
// into streaming OpenAI-compatible `/chat/completions` JSON request bodies when
// `stream_options.include_usage` is unset, so upstream providers report token
// usage in the final SSE chunk instead of omitting usage on streamed calls.
func ensureOpenAIStreamIncludeUsage(upstreamPath string, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	if upstreamPath != "/chat/completions" && !strings.HasSuffix(upstreamPath, "/chat/completions") {
		return body
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
	}
	rawStream, hasStream := payload["stream"]
	if !hasStream {
		return body
	}
	var streaming bool
	if err := json.Unmarshal(rawStream, &streaming); err != nil || !streaming {
		return body
	}
	rawOpts, hasOpts := payload["stream_options"]
	if !hasOpts || bytes.Equal(bytes.TrimSpace(rawOpts), []byte("null")) {
		payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(rawOpts, &opts); err != nil || opts == nil {
			return body
		}
		if existing, ok := opts["include_usage"]; ok && bytes.Equal(bytes.TrimSpace(existing), []byte("true")) {
			return body
		}
		opts["include_usage"] = json.RawMessage("true")
		encodedOpts, err := json.Marshal(opts)
		if err != nil {
			return body
		}
		payload["stream_options"] = encodedOpts
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}
