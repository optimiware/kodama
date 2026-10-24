package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/gateway"
)

type manifest struct {
	ProtocolRevision string `json:"protocolRevision"`
	Accepted         []string
	Rejected         []struct{ File, Reason string }
	Streams          []struct {
		File    string
		Outcome string
		Frames  int
		Usage   any
		Finish  string
		Model   string
	}
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "chat-completions", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "chat-completions", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func drain(t *testing.T, raw []byte, lim StreamLimits) (chunks []gateway.Chunk, err error) {
	t.Helper()
	s := newStream(io.NopCloser(bytes.NewReader(raw)), lim, func(error) {}, gateway.Destination{})
	defer s.Close()
	for {
		ch, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		if ch.Done {
			continue
		}
		chunks = append(chunks, ch)
	}
}

// Test_chat_completions_compatibility replays the frozen fixtures. It proves
// the protocol subset (GM-12 deterministic part); it is explicitly not the real
// OpenCode smoke test, which T-031 owns.
func Test_chat_completions_compatibility(t *testing.T) {
	m := loadManifest(t)
	if m.ProtocolRevision != ProtocolRevision {
		t.Fatalf("manifest revision %q != code revision %q", m.ProtocolRevision, ProtocolRevision)
	}

	t.Run("accepted requests decode and re-encode only supported members", func(t *testing.T) {
		for _, f := range m.Accepted {
			c, err := DecodeRequest(fixture(t, f), DefaultRequestLimits())
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out, err := EncodeRequest(c, 64)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(out, &members); err != nil {
				t.Fatal(err)
			}
			for k := range members {
				switch k {
				case "model", "messages", "stream", "stream_options", "max_tokens", "temperature", "top_p", "stop", "tools", "tool_choice", "parallel_tool_calls":
				default:
					t.Errorf("%s: outbound member %q is outside the subset", f, k)
				}
			}
			if string(members["stream"]) != "true" || string(members["stream_options"]) != `{"include_usage":true}` || string(members["max_tokens"]) != "64" {
				t.Errorf("%s: the gateway must force stream, usage reporting and its own output limit: %s", f, out)
			}
			// A second pass through the decoder is a fixed point.
			again, err := DecodeRequest(out, DefaultRequestLimits())
			if err != nil {
				t.Fatalf("%s: re-decode: %v", f, err)
			}
			if again.Model != c.Model || len(again.Messages) != len(c.Messages) || len(again.Tools) != len(c.Tools) {
				t.Errorf("%s: round trip changed the request", f)
			}
		}
	})

	t.Run("unsupported modes are rejected", func(t *testing.T) {
		for _, r := range m.Rejected {
			_, err := DecodeRequest(fixture(t, r.File), DefaultRequestLimits())
			if !errors.Is(err, gateway.ErrInvalid) || !strings.Contains(err.Error(), r.Reason) {
				t.Errorf("%s: got %v, want invalid containing %q", r.File, err, r.Reason)
			}
		}
	})

	t.Run("tool loop messages keep their structure", func(t *testing.T) {
		c, err := DecodeRequest(fixture(t, "requests/tool_loop.json"), DefaultRequestLimits())
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Messages) != 3 || c.Messages[1].Content != nil || len(c.Messages[1].ToolCalls) != 1 || c.Messages[2].ToolCallID != "call_1" ||
			c.ToolChoice.Mode != "function" || c.ToolChoice.Function != "get_weather" || c.MaxTokens != 256 || c.ParallelToolCalls == nil || *c.ParallelToolCalls {
			t.Fatalf("decoded: %+v", c)
		}
	})

	t.Run("streams", func(t *testing.T) {
		for _, s := range m.Streams {
			chunks, err := drain(t, fixture(t, s.File), DefaultStreamLimits())
			var se *gateway.StreamError
			switch s.Outcome {
			case "complete":
				if err != nil || len(chunks) != s.Frames {
					t.Fatalf("%s: %d chunks, %v", s.File, len(chunks), err)
				}
				var usage *gateway.TokenUsage
				invalid := false
				finish, model := "", ""
				for _, c := range chunks {
					if c.Usage != nil {
						usage = c.Usage
					}
					invalid = invalid || c.UsageInvalid
					if c.Finish != "" {
						finish = c.Finish
					}
					if c.Model != "" {
						model = c.Model
					}
					if bytes.Contains(c.Data, []byte("system_fingerprint")) || bytes.Contains(c.Data, []byte("prompt_tokens_details")) {
						t.Errorf("%s: an unsupported provider member was forwarded: %s", s.File, c.Data)
					}
				}
				switch want := s.Usage.(type) {
				case nil:
					if usage != nil || invalid {
						t.Errorf("%s: usage %v invalid=%v, want none", s.File, usage, invalid)
					}
				case string:
					if usage != nil || !invalid {
						t.Errorf("%s: usage %v invalid=%v, want invalid", s.File, usage, invalid)
					}
				case map[string]any:
					if usage == nil || float64(usage.Input) != want["input"] || float64(usage.Output) != want["output"] {
						t.Errorf("%s: usage %+v, want %v", s.File, usage, want)
					}
				}
				if finish != s.Finish || model != s.Model {
					t.Errorf("%s: finish %q model %q", s.File, finish, model)
				}
			case "truncated":
				if !errors.As(err, &se) || se.Kind != gateway.FaultTruncated {
					t.Errorf("%s: got %v, want truncated", s.File, err)
				}
			case "protocol":
				if !errors.As(err, &se) || se.Kind != gateway.FaultProtocol {
					t.Errorf("%s: got %v, want protocol failure", s.File, err)
				}
			}
		}
	})

	t.Run("tool call deltas are re-encoded with their index and fragments", func(t *testing.T) {
		chunks, err := drain(t, fixture(t, "streams/tool_calls.sse"), DefaultStreamLimits())
		if err != nil {
			t.Fatal(err)
		}
		var args strings.Builder
		for _, c := range chunks {
			var o struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Index    int `json:"index"`
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(c.Data, &o); err != nil {
				t.Fatal(err)
			}
			for _, ch := range o.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					if tc.Index != 0 {
						t.Fatalf("index %d", tc.Index)
					}
					args.WriteString(tc.Function.Arguments)
				}
			}
		}
		if args.String() != `{"city":"Oslo"}` {
			t.Fatalf("arguments = %q", args.String())
		}
	})
}

func Test_chat_completions_bounds(t *testing.T) {
	t.Run("request structure", func(t *testing.T) {
		lim := DefaultRequestLimits()
		for name, body := range map[string]string{
			"duplicate key":     `{"model":"a","model":"b","stream":true,"messages":[{"role":"user","content":"x"}]}`,
			"trailing data":     `{"model":"a","stream":true,"messages":[{"role":"user","content":"x"}]} {}`,
			"not an object":     `[1]`,
			"empty":             ``,
			"deep nesting":      `{"model":"a","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t","parameters":` + strings.Repeat(`{"a":`, 20) + `1` + strings.Repeat(`}`, 20) + `}}]}`,
			"duplicate message": `{"model":"a","stream":true,"messages":[{"role":"user","role":"system","content":"x"}]}`,
		} {
			if _, err := DecodeRequest([]byte(body), lim); !errors.Is(err, gateway.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		tight := lim
		tight.MaxBytes = 50
		if _, err := DecodeRequest(fixture(t, "requests/full.json"), tight); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("size bound: %v", err)
		}
		tight = lim
		tight.MaxContentBytes = 4
		if _, err := DecodeRequest(fixture(t, "requests/minimal.json"), tight); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("content bound: %v", err)
		}
		tight = lim
		tight.MaxMessages = 1
		if _, err := DecodeRequest(fixture(t, "requests/full.json"), tight); !errors.Is(err, gateway.ErrInvalid) {
			t.Errorf("message bound: %v", err)
		}
	})

	t.Run("stream limits", func(t *testing.T) {
		line := func(n int) string {
			return `data: {"choices":[{"delta":{"content":"` + strings.Repeat("a", n) + `"}}]}` + "\n\n"
		}
		lim := DefaultStreamLimits()
		lim.MaxFrameBytes = 2048
		_, err := drain(t, []byte(line(4000)+"data: [DONE]\n\n"), lim)
		var se *gateway.StreamError
		if !errors.As(err, &se) || se.Kind != gateway.FaultTooLarge {
			t.Errorf("frame bound: %v", err)
		}
		lim = DefaultStreamLimits()
		lim.MaxFrames = 3
		_, err = drain(t, []byte(strings.Repeat(line(1), 5)+"data: [DONE]\n\n"), lim)
		if !errors.As(err, &se) || se.Kind != gateway.FaultTooLarge {
			t.Errorf("frame count bound: %v", err)
		}
		lim = DefaultStreamLimits()
		lim.MaxFrameBytes, lim.MaxResponseBytes = 2048, 2048
		_, err = drain(t, []byte(strings.Repeat(line(500), 6)+"data: [DONE]\n\n"), lim)
		if !errors.As(err, &se) || se.Kind != gateway.FaultTooLarge {
			t.Errorf("total bound: %v", err)
		}
	})

	t.Run("idle stream", func(t *testing.T) {
		pr, pw := io.Pipe()
		lim := DefaultStreamLimits()
		lim.IdleTimeout = 50 * time.Millisecond
		s := newStream(pr, lim, func(cause error) { _ = pw.CloseWithError(cause) }, gateway.Destination{})
		defer s.Close()
		go func() { _, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")) }()
		if _, err := s.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := s.Next(context.Background())
		var se *gateway.StreamError
		if !errors.As(err, &se) || se.Kind != gateway.FaultIdle {
			t.Fatalf("got %v, want idle", err)
		}
		if _, err := s.Next(context.Background()); !errors.As(err, &se) {
			t.Fatalf("a failed stream stays failed: %v", err)
		}
	})

	t.Run("caller cancellation is not a stream fault", func(t *testing.T) {
		pr, pw := io.Pipe()
		s := newStream(pr, DefaultStreamLimits(), func(cause error) { _ = pw.CloseWithError(cause) }, gateway.Destination{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(30 * time.Millisecond); cancel(); _ = pw.CloseWithError(context.Canceled) }()
		if _, err := s.Next(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func Test_estimator(t *testing.T) {
	if _, err := NewEstimator(nil); err == nil {
		t.Fatal("an empty table must be refused")
	}
	if _, err := NewEstimator(map[string]ModelAccounting{"gpt-a": {BytesPerToken: 0}}); err == nil {
		t.Fatal("zero bytes per token must be refused")
	}
	e, err := NewEstimator(map[string]ModelAccounting{"gpt-a": ConservativeAccounting, "gpt-b": {BytesPerToken: 4, RequestOverhead: 10}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeRequest(fixture(t, "requests/tool_loop.json"), DefaultRequestLimits())
	if err != nil {
		t.Fatal(err)
	}
	n1, err := e.InputBound(c)
	if err != nil || n1 < 100 {
		t.Fatalf("bound = %d %v", n1, err)
	}
	bytesInRequest := int64(0)
	for _, m := range c.Messages {
		if m.Content != nil {
			bytesInRequest += int64(len(*m.Content))
		}
	}
	if n1 < bytesInRequest {
		t.Fatalf("with one byte per token the bound %d must cover the %d content bytes", n1, bytesInRequest)
	}
	c.Model = "gpt-b"
	n2, _ := e.InputBound(c)
	if n2 >= n1 {
		t.Fatalf("a looser accounting must give a smaller bound: %d vs %d", n2, n1)
	}
	c.Model = "unknown"
	if _, err := e.InputBound(c); !errors.Is(err, gateway.ErrModelNotAllowed) {
		t.Fatalf("unknown model: %v", err)
	}
}
