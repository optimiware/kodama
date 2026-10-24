// Package openai is the provider adapter of the model gateway for one
// protocol: OpenAI-compatible streaming chat completions. It decodes the
// workload's request strictly into the supported subset, builds the outbound
// request from validated fields only, enforces approved destinations at every
// connection, and validates and re-encodes each stream event.
//
// The supported subset and its compatibility manifest are frozen in
// internal/gateway/protocol.md and testdata/chat-completions/. This package
// proves protocol conformance with deterministic fixtures; it does not claim
// that any real provider, model or harness version is qualified (T-031).
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/gateway"
)

// ProtocolRevision names the frozen request/response subset. A change to the
// accepted fields or the stream grammar needs a new revision and new fixtures.
const ProtocolRevision = "chat-completions-stream/1"

// RequestLimits bound a decoded request. The zero value is invalid; use
// DefaultRequestLimits and tighten per run.
type RequestLimits struct {
	MaxBytes        int64
	MaxMessages     int
	MaxContentBytes int
	MaxTools        int
	MaxToolBytes    int
	MaxToolCalls    int
	MaxDepth        int
	MaxNodes        int
}

// DefaultRequestLimits are the hard ceilings of this protocol revision.
func DefaultRequestLimits() RequestLimits {
	return RequestLimits{
		MaxBytes: gateway.MaxBytesCeiling, MaxMessages: 256, MaxContentBytes: 512 << 10, MaxTools: 64,
		MaxToolBytes: 32 << 10, MaxToolCalls: 32, MaxDepth: 16, MaxNodes: 50_000,
	}
}

var (
	namePattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolCallIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

var allowedTop = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "stop": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true, "n": true,
}

// ErrUnsupported marks a request member or mode outside the supported subset.
// It is wrapped by gateway.ErrInvalid errors so callers classify one way.
var ErrUnsupported = errors.New("openai: unsupported")

func invalid(format string, args ...any) error {
	return gateway.Failf(gateway.ErrInvalid, format, args...)
}

// object decodes one JSON object into its raw members, rejecting duplicate
// keys, which json.Unmarshal would silently merge.
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, invalid("a JSON object is required")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, invalid("the JSON is malformed")
		}
		key, _ := kt.(string)
		if _, dup := out[key]; dup {
			return nil, invalid("the request repeats a member")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, invalid("the JSON is malformed")
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, invalid("the JSON is malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("the JSON has trailing data")
	}
	return out, nil
}

// checkStructure bounds nesting depth and node count before any value is
// decoded into memory.
func checkStructure(body []byte, maxDepth, maxNodes int) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	depth, nodes := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return invalid("the JSON is malformed")
		}
		nodes++
		if nodes > maxNodes {
			return invalid("the request is too complex")
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
				if depth > maxDepth {
					return invalid("the request is nested too deeply")
				}
			} else {
				depth--
			}
		}
	}
}

func str(raw json.RawMessage, what string, max int) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", invalid("%s must be a string", what)
	}
	if len(s) > max || !utf8.ValidString(s) {
		return "", invalid("%s is too long or not valid text", what)
	}
	return s, nil
}

func integer(raw json.RawMessage, what string, lo, hi int64) (int64, error) {
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&n); err != nil {
		return 0, invalid("%s must be an integer", what)
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || v < lo || v > hi {
		return 0, invalid("%s must be an integer between %d and %d", what, lo, hi)
	}
	return v, nil
}

func number(raw json.RawMessage, what string, lo, hi float64, loOpen bool) (*float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, invalid("%s must be a number", what)
	}
	if f < lo || f > hi || (loOpen && f == lo) {
		return nil, invalid("%s is out of range", what)
	}
	return &f, nil
}

func boolean(raw json.RawMessage, what string) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, invalid("%s must be a boolean", what)
	}
	return b, nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func only(m map[string]json.RawMessage, what string, keys ...string) error {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "%s has an unsupported member", what)
		}
	}
	return nil
}

// DecodeRequest strictly decodes a chat completions request into the supported
// subset. Unknown members, unsupported modes (non-streaming, multiple choices,
// response formats, multimodal content, legacy functions) and anything beyond
// the limits are rejected rather than forwarded.
func DecodeRequest(body []byte, lim RequestLimits) (gateway.Completion, error) {
	var c gateway.Completion
	if lim.MaxBytes < 1 || int64(len(body)) > lim.MaxBytes {
		return c, invalid("the request body is too large")
	}
	if err := checkStructure(body, lim.MaxDepth, lim.MaxNodes); err != nil {
		return c, err
	}
	top, err := object(body)
	if err != nil {
		return c, err
	}
	for k := range top {
		if !allowedTop[k] {
			return c, gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "the request has an unsupported member")
		}
	}
	rawModel, ok := top["model"]
	if !ok {
		return c, invalid("model is required")
	}
	if c.Model, err = str(rawModel, "model", 128); err != nil || !gateway.ValidModelName(c.Model) {
		return c, invalid("model is not a valid model name")
	}
	if raw, ok := top["stream"]; !ok {
		return c, gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "only streaming requests are supported")
	} else if b, err := boolean(raw, "stream"); err != nil || !b {
		return c, gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "only streaming requests are supported")
	}
	if raw, ok := top["stream_options"]; ok {
		so, err := object(raw)
		if err != nil {
			return c, err
		}
		if err := only(so, "stream_options", "include_usage"); err != nil {
			return c, err
		}
		if iu, ok := so["include_usage"]; ok {
			if b, err := boolean(iu, "include_usage"); err != nil || !b {
				return c, invalid("include_usage must be true")
			}
		}
	}
	if raw, ok := top["n"]; ok {
		if n, err := integer(raw, "n", 1, 1); err != nil || n != 1 {
			return c, gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "only one choice is supported")
		}
	}
	mt, hasMT := top["max_tokens"]
	mct, hasMCT := top["max_completion_tokens"]
	switch {
	case hasMT && hasMCT:
		return c, invalid("set only one of max_tokens and max_completion_tokens")
	case hasMT:
		c.MaxTokens, err = integer(mt, "max_tokens", 1, gateway.MaxOutputCeiling)
	case hasMCT:
		c.MaxTokens, err = integer(mct, "max_completion_tokens", 1, gateway.MaxOutputCeiling)
	}
	if err != nil {
		return c, err
	}
	if raw, ok := top["temperature"]; ok {
		if c.Temperature, err = number(raw, "temperature", 0, 2, false); err != nil {
			return c, err
		}
	}
	if raw, ok := top["top_p"]; ok {
		if c.TopP, err = number(raw, "top_p", 0, 1, true); err != nil {
			return c, err
		}
	}
	if raw, ok := top["stop"]; ok {
		if c.Stop, err = decodeStop(raw); err != nil {
			return c, err
		}
	}
	if c.Messages, err = decodeMessages(top["messages"], lim); err != nil {
		return c, err
	}
	if raw, ok := top["tools"]; ok {
		if c.Tools, err = decodeTools(raw, lim); err != nil {
			return c, err
		}
	}
	if raw, ok := top["tool_choice"]; ok {
		if c.ToolChoice, err = decodeToolChoice(raw, c.Tools); err != nil {
			return c, err
		}
	}
	if raw, ok := top["parallel_tool_calls"]; ok {
		b, err := boolean(raw, "parallel_tool_calls")
		if err != nil {
			return c, err
		}
		if len(c.Tools) == 0 {
			return c, invalid("parallel_tool_calls needs declared tools")
		}
		c.ParallelToolCalls = &b
	}
	return c, nil
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" || len(one) > 64 {
			return nil, invalid("a stop sequence is 1 to 64 bytes")
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) < 1 || len(many) > 4 {
		return nil, invalid("stop must be a string or up to four strings")
	}
	for _, s := range many {
		if s == "" || len(s) > 64 || !utf8.ValidString(s) {
			return nil, invalid("a stop sequence is 1 to 64 bytes")
		}
	}
	return many, nil
}

func decodeMessages(raw json.RawMessage, lim RequestLimits) ([]gateway.Message, error) {
	if len(raw) == 0 {
		return nil, invalid("messages are required")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 1 || len(items) > lim.MaxMessages {
		return nil, invalid("messages must be an array of 1 to %d messages", lim.MaxMessages)
	}
	out := make([]gateway.Message, 0, len(items))
	for _, it := range items {
		m, err := object(it)
		if err != nil {
			return nil, invalid("a message must be an object")
		}
		if err := only(m, "a message", "role", "content", "name", "tool_calls", "tool_call_id"); err != nil {
			return nil, err
		}
		role, err := str(m["role"], "role", 16)
		if err != nil {
			return nil, err
		}
		msg := gateway.Message{Role: gateway.Role(role)}
		switch msg.Role {
		case gateway.RoleSystem, gateway.RoleUser, gateway.RoleAssistant, gateway.RoleTool:
		default:
			return nil, invalid("the message role is not supported")
		}
		if raw, ok := m["name"]; ok {
			if msg.Name, err = str(raw, "name", 64); err != nil || !namePattern.MatchString(msg.Name) {
				return nil, invalid("a message name is not valid")
			}
		}
		if raw, ok := m["tool_calls"]; ok {
			if msg.Role != gateway.RoleAssistant {
				return nil, invalid("only assistant messages carry tool calls")
			}
			if msg.ToolCalls, err = decodeToolCalls(raw, lim); err != nil {
				return nil, err
			}
		}
		if raw, ok := m["tool_call_id"]; ok {
			if msg.Role != gateway.RoleTool {
				return nil, invalid("only tool messages carry a tool call id")
			}
			if msg.ToolCallID, err = str(raw, "tool_call_id", 64); err != nil || !toolCallIDPattern.MatchString(msg.ToolCallID) {
				return nil, invalid("a tool call id is not valid")
			}
		}
		if msg.Role == gateway.RoleTool && msg.ToolCallID == "" {
			return nil, invalid("a tool message needs a tool call id")
		}
		raw, hasContent := m["content"]
		switch {
		case !hasContent || isNull(raw):
			if msg.Role != gateway.RoleAssistant || len(msg.ToolCalls) == 0 {
				return nil, invalid("a message needs text content")
			}
		default:
			text, err := str(raw, "content", lim.MaxContentBytes)
			if err != nil {
				return nil, gateway.FailWith(gateway.ErrInvalid, ErrUnsupported, "message content must be text")
			}
			msg.Content = &text
		}
		out = append(out, msg)
	}
	return out, nil
}

func decodeToolCalls(raw json.RawMessage, lim RequestLimits) ([]gateway.ToolCall, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 1 || len(items) > lim.MaxToolCalls {
		return nil, invalid("tool_calls must hold 1 to %d calls", lim.MaxToolCalls)
	}
	out := make([]gateway.ToolCall, 0, len(items))
	for _, it := range items {
		m, err := object(it)
		if err != nil {
			return nil, invalid("a tool call must be an object")
		}
		if err := only(m, "a tool call", "id", "type", "function"); err != nil {
			return nil, err
		}
		typ, err := str(m["type"], "type", 16)
		if err != nil || typ != "function" {
			return nil, invalid("only function tool calls are supported")
		}
		id, err := str(m["id"], "id", 64)
		if err != nil || !toolCallIDPattern.MatchString(id) {
			return nil, invalid("a tool call id is not valid")
		}
		fn, err := object(m["function"])
		if err != nil {
			return nil, invalid("a tool call needs a function")
		}
		if err := only(fn, "a tool call function", "name", "arguments"); err != nil {
			return nil, err
		}
		name, err := str(fn["name"], "name", 64)
		if err != nil || !namePattern.MatchString(name) {
			return nil, invalid("a function name is not valid")
		}
		args, err := str(fn["arguments"], "arguments", lim.MaxToolBytes*2)
		if err != nil {
			return nil, err
		}
		out = append(out, gateway.ToolCall{ID: id, Name: name, Arguments: args})
	}
	return out, nil
}

func decodeTools(raw json.RawMessage, lim RequestLimits) ([]gateway.Tool, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 1 || len(items) > lim.MaxTools {
		return nil, invalid("tools must hold 1 to %d declarations", lim.MaxTools)
	}
	out := make([]gateway.Tool, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		m, err := object(it)
		if err != nil {
			return nil, invalid("a tool must be an object")
		}
		if err := only(m, "a tool", "type", "function"); err != nil {
			return nil, err
		}
		typ, err := str(m["type"], "type", 16)
		if err != nil || typ != "function" {
			return nil, invalid("only function tools are supported")
		}
		fn, err := object(m["function"])
		if err != nil {
			return nil, invalid("a tool needs a function")
		}
		if err := only(fn, "a tool function", "name", "description", "parameters"); err != nil {
			return nil, err
		}
		t := gateway.Tool{}
		if t.Name, err = str(fn["name"], "name", 64); err != nil || !namePattern.MatchString(t.Name) {
			return nil, invalid("a tool name is not valid")
		}
		if seen[t.Name] {
			return nil, invalid("tool names must be unique")
		}
		seen[t.Name] = true
		if raw, ok := fn["description"]; ok {
			if t.Description, err = str(raw, "description", 4096); err != nil {
				return nil, err
			}
		}
		params := fn["parameters"]
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if len(params) > lim.MaxToolBytes {
			return nil, invalid("a tool schema is too large")
		}
		if _, err := object(params); err != nil {
			return nil, invalid("tool parameters must be a JSON object")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, params); err != nil {
			return nil, invalid("tool parameters are malformed")
		}
		t.Parameters = compact.Bytes()
		out = append(out, t)
	}
	return out, nil
}

func decodeToolChoice(raw json.RawMessage, tools []gateway.Tool) (gateway.ToolChoice, error) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none":
			return gateway.ToolChoice{Mode: "none"}, nil
		case "auto", "required":
			if len(tools) == 0 {
				return gateway.ToolChoice{}, invalid("tool_choice needs declared tools")
			}
			return gateway.ToolChoice{Mode: mode}, nil
		}
		return gateway.ToolChoice{}, invalid("tool_choice is not supported")
	}
	m, err := object(raw)
	if err != nil {
		return gateway.ToolChoice{}, invalid("tool_choice is not supported")
	}
	if err := only(m, "tool_choice", "type", "function"); err != nil {
		return gateway.ToolChoice{}, err
	}
	typ, err := str(m["type"], "type", 16)
	if err != nil || typ != "function" {
		return gateway.ToolChoice{}, invalid("tool_choice is not supported")
	}
	fn, err := object(m["function"])
	if err != nil {
		return gateway.ToolChoice{}, invalid("tool_choice names no function")
	}
	if err := only(fn, "tool_choice function", "name"); err != nil {
		return gateway.ToolChoice{}, err
	}
	name, err := str(fn["name"], "name", 64)
	if err != nil {
		return gateway.ToolChoice{}, err
	}
	for _, t := range tools {
		if t.Name == name {
			return gateway.ToolChoice{Mode: "function", Function: name}, nil
		}
	}
	return gateway.ToolChoice{}, invalid("tool_choice names an undeclared function")
}

// ---- outbound request --------------------------------------------------------

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolSchema `json:"function"`
}

type wireToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireRequest struct {
	Model             string        `json:"model"`
	Messages          []wireMessage `json:"messages"`
	Stream            bool          `json:"stream"`
	StreamOptions     streamOptions `json:"stream_options"`
	MaxTokens         int64         `json:"max_tokens"`
	Temperature       *float64      `json:"temperature,omitempty"`
	TopP              *float64      `json:"top_p,omitempty"`
	Stop              []string      `json:"stop,omitempty"`
	Tools             []wireTool    `json:"tools,omitempty"`
	ToolChoice        any           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// EncodeRequest builds the provider request from validated fields only. The
// output limit and usage reporting are set by the gateway, never by the caller.
func EncodeRequest(c gateway.Completion, maxTokens int64) ([]byte, error) {
	if maxTokens < 1 {
		return nil, invalid("an output limit is required")
	}
	w := wireRequest{
		Model: c.Model, Stream: true, StreamOptions: streamOptions{IncludeUsage: true}, MaxTokens: maxTokens,
		Temperature: c.Temperature, TopP: c.TopP, Stop: c.Stop, ParallelToolCalls: c.ParallelToolCalls,
	}
	for _, m := range c.Messages {
		wm := wireMessage{Role: string(m.Role), Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments}})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range c.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireToolSchema{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	switch c.ToolChoice.Mode {
	case "":
	case "function":
		w.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": c.ToolChoice.Function}}
	default:
		w.ToolChoice = c.ToolChoice.Mode
	}
	return json.Marshal(w)
}

// Decoder implements gateway.Decoder with the protocol's hard ceilings; the
// run's tighter size limit is enforced by admission.
type Decoder struct{ lim RequestLimits }

var _ gateway.Decoder = Decoder{}

// NewDecoder returns a decoder with DefaultRequestLimits.
func NewDecoder() Decoder { return Decoder{lim: DefaultRequestLimits()} }

// Decode implements gateway.Decoder.
func (d Decoder) Decode(body []byte) (gateway.Completion, error) { return DecodeRequest(body, d.lim) }

// AcceptedMembers lists the top-level request members of this protocol
// revision, sorted. api/gateway.yaml's ChatCompletionRequest must list exactly
// these (the HTTP contract test compares them).
func AcceptedMembers() []string {
	out := make([]string, 0, len(allowedTop))
	for k := range allowedTop {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
