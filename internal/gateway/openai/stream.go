package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/gateway"
)

// StreamLimits bound one provider stream.
type StreamLimits struct {
	// MaxFrameBytes bounds one SSE line; MaxResponseBytes the whole stream;
	// MaxFrames the number of events.
	MaxFrameBytes    int
	MaxResponseBytes int64
	MaxFrames        int
	// IdleTimeout bounds the wait for the next frame.
	IdleTimeout time.Duration
}

// DefaultStreamLimits are conservative defaults.
func DefaultStreamLimits() StreamLimits {
	return StreamLimits{MaxFrameBytes: 64 << 10, MaxResponseBytes: 8 << 20, MaxFrames: 20_000, IdleTimeout: 60 * time.Second}
}

func (l StreamLimits) valid() bool {
	return l.MaxFrameBytes >= 1024 && l.MaxResponseBytes >= int64(l.MaxFrameBytes) && l.MaxFrames >= 1 && l.IdleTimeout > 0
}

var (
	errIdle = errors.New("openai: stream idle")
)

// sseStream reads a validated event stream. It implements gateway.Stream.
type sseStream struct {
	body   io.ReadCloser
	rd     *bufio.Reader
	lim    StreamLimits
	cancel context.CancelCauseFunc
	idle   *time.Timer
	dest   gateway.Destination

	idleHit atomic.Bool
	mu      sync.Mutex
	read    int64
	frames  int
	done    bool
	failed  error
	closed  bool
}

var _ gateway.Stream = (*sseStream)(nil)

func newStream(body io.ReadCloser, lim StreamLimits, cancel context.CancelCauseFunc, dest gateway.Destination) *sseStream {
	s := &sseStream{body: body, rd: bufio.NewReaderSize(body, 4096), lim: lim, cancel: cancel, dest: dest}
	s.idle = time.AfterFunc(time.Hour, func() {
		s.idleHit.Store(true)
		cancel(errIdle)
	})
	s.idle.Stop()
	return s
}

func (s *sseStream) Destination() gateway.Destination { return s.dest }

func (s *sseStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.idle.Stop()
	s.cancel(context.Canceled)
	return s.body.Close()
}

// line reads one bounded line without its terminator.
func (s *sseStream) line() ([]byte, error) {
	var buf []byte
	for {
		part, err := s.rd.ReadSlice('\n')
		s.read += int64(len(part))
		if s.read > s.lim.MaxResponseBytes {
			return nil, &gateway.StreamError{Kind: gateway.FaultTooLarge, Err: errors.New("response too large")}
		}
		buf = append(buf, part...)
		if len(buf) > s.lim.MaxFrameBytes {
			return nil, &gateway.StreamError{Kind: gateway.FaultTooLarge, Err: errors.New("frame too large")}
		}
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, err
	}
}

// Next implements gateway.Stream.
func (s *sseStream) Next(ctx context.Context) (gateway.Chunk, error) {
	if s.failed != nil {
		return gateway.Chunk{}, s.failed
	}
	if s.done {
		return gateway.Chunk{}, io.EOF
	}
	ch, err := s.next(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		s.failed = err
	}
	return ch, err
}

func (s *sseStream) next(ctx context.Context) (gateway.Chunk, error) {
	var data []byte
	haveData := false
	for {
		s.idle.Reset(s.lim.IdleTimeout)
		line, err := s.line()
		s.idle.Stop()
		if err != nil {
			return gateway.Chunk{}, s.classifyRead(ctx, err)
		}
		switch {
		case len(line) == 0:
			if !haveData {
				continue
			}
			return s.event(data)
		case line[0] == ':':
			continue // comment / keep-alive
		case bytes.HasPrefix(line, []byte("data:")):
			if haveData {
				return gateway.Chunk{}, protocolFault("a multi-line event")
			}
			data = bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			haveData = true
		default:
			return gateway.Chunk{}, protocolFault("an unsupported SSE field")
		}
	}
}

func protocolFault(what string) error {
	return &gateway.StreamError{Kind: gateway.FaultProtocol, Err: errors.New(what)}
}

func (s *sseStream) classifyRead(ctx context.Context, err error) error {
	var se *gateway.StreamError
	switch {
	case errors.As(err, &se):
		return err
	case s.idleHit.Load():
		return &gateway.StreamError{Kind: gateway.FaultIdle, Err: errIdle}
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	// The cause is not wrapped: io.EOF here means "ended before [DONE]", and
	// io.EOF must stay the sole signal of a clean end.
	return &gateway.StreamError{Kind: gateway.FaultTruncated, Err: errors.New("the stream ended before its terminator")}
}

func (s *sseStream) event(data []byte) (gateway.Chunk, error) {
	s.frames++
	if s.frames > s.lim.MaxFrames {
		return gateway.Chunk{}, &gateway.StreamError{Kind: gateway.FaultTooLarge, Err: errors.New("too many events")}
	}
	if string(data) == "[DONE]" {
		s.done = true
		return gateway.Chunk{Done: true}, nil
	}
	ch, err := ParseChunk(data)
	if err != nil {
		return gateway.Chunk{}, &gateway.StreamError{Kind: gateway.FaultProtocol, Err: err}
	}
	return ch, nil
}

// ---- chunk grammar ------------------------------------------------------------

type wireChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created *int64       `json:"created"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   json.RawMessage
}

type wireChoice struct {
	Index        *int64    `json:"index"`
	Delta        wireDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type wireDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content"`
	ToolCalls []wireDeltaCall `json:"tool_calls,omitempty"`
}

type wireDeltaCall struct {
	Index    *int64         `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *wireDeltaFunc `json:"function,omitempty"`
}

type wireDeltaFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type outChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created *int64      `json:"created,omitempty"`
	Model   string      `json:"model"`
	Choices []outChoice `json:"choices"`
	Usage   *outUsage   `json:"usage,omitempty"`
}

type outChoice struct {
	Index        int64     `json:"index"`
	Delta        wireDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type outUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

var finishReasons = map[string]bool{"stop": true, "length": true, "tool_calls": true, "content_filter": true}

// ParseChunk validates one provider event and re-encodes only the members of
// the supported subset. Unknown provider members (system_fingerprint, usage
// details, obfuscation) are dropped, not forwarded.
func ParseChunk(data []byte) (gateway.Chunk, error) {
	if len(data) == 0 || data[0] != '{' {
		return gateway.Chunk{}, errors.New("an event is not a JSON object")
	}
	if err := checkStructure(data, 12, 5000); err != nil {
		return gateway.Chunk{}, errors.New("an event is too complex or malformed")
	}
	var w wireChunk
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&w); err != nil {
		return gateway.Chunk{}, errors.New("an event is malformed")
	}
	// The usage member is decoded separately so a malformed usage object is
	// reported as invalid usage rather than as a protocol failure.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	w.Usage = raw["usage"]
	if w.Object != "" && w.Object != "chat.completion.chunk" {
		return gateway.Chunk{}, errors.New("an event has an unexpected object type")
	}
	if len(w.ID) > 256 || len(w.Model) > 128 {
		return gateway.Chunk{}, errors.New("an event identifier is too long")
	}
	if w.Model != "" && !gateway.ValidModelName(w.Model) {
		return gateway.Chunk{}, errors.New("an event model is not a valid name")
	}
	if len(w.Choices) > 1 {
		return gateway.Chunk{}, errors.New("an event carries more than one choice")
	}
	out := outChunk{ID: w.ID, Object: "chat.completion.chunk", Created: w.Created, Model: w.Model, Choices: []outChoice{}}
	ch := gateway.Chunk{Model: w.Model}
	for _, c := range w.Choices {
		if c.Index != nil && *c.Index != 0 {
			return gateway.Chunk{}, errors.New("an event choice index is not zero")
		}
		if c.Delta.Role != "" && c.Delta.Role != "assistant" {
			return gateway.Chunk{}, errors.New("an event delta role is not assistant")
		}
		if len(c.Delta.ToolCalls) > 32 {
			return gateway.Chunk{}, errors.New("an event carries too many tool call deltas")
		}
		delta := wireDelta{Role: c.Delta.Role, Content: c.Delta.Content}
		for _, tc := range c.Delta.ToolCalls {
			if tc.Index == nil || *tc.Index < 0 || *tc.Index > 31 || (tc.Type != "" && tc.Type != "function") || len(tc.ID) > 64 {
				return gateway.Chunk{}, errors.New("a tool call delta is malformed")
			}
			nc := wireDeltaCall{Index: tc.Index, ID: tc.ID, Type: tc.Type}
			if tc.Function != nil {
				if len(tc.Function.Name) > 64 {
					return gateway.Chunk{}, errors.New("a tool call delta name is too long")
				}
				nc.Function = &wireDeltaFunc{Name: tc.Function.Name, Arguments: tc.Function.Arguments}
			}
			delta.ToolCalls = append(delta.ToolCalls, nc)
		}
		if c.FinishReason != nil {
			if !finishReasons[*c.FinishReason] {
				return gateway.Chunk{}, errors.New("an event finish reason is not supported")
			}
			ch.Finish = *c.FinishReason
		}
		out.Choices = append(out.Choices, outChoice{Index: 0, Delta: delta, FinishReason: c.FinishReason})
	}
	if len(w.Usage) > 0 && !isNull(w.Usage) {
		u, ok := parseUsage(w.Usage)
		if ok {
			ch.Usage = &u
			out.Usage = &outUsage{PromptTokens: u.Input, CompletionTokens: u.Output, TotalTokens: u.Total()}
		} else {
			ch.UsageInvalid = true
		}
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return gateway.Chunk{}, errors.New("an event cannot be encoded")
	}
	ch.Data = enc
	return ch, nil
}

// parseUsage accepts exactly integer counts; a missing total is derived, a
// total that disagrees with the parts makes the whole usage invalid.
func parseUsage(raw json.RawMessage) (gateway.TokenUsage, bool) {
	m, err := object(raw)
	if err != nil {
		return gateway.TokenUsage{}, false
	}
	count := func(key string) (int64, bool, bool) {
		v, ok := m[key]
		if !ok || isNull(v) {
			return 0, false, true
		}
		n := json.Number(string(bytes.TrimSpace(v)))
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil || i < 0 || i > gateway.MaxInputCeiling*1000 {
			return 0, false, false
		}
		return i, true, true
	}
	in, hasIn, ok1 := count("prompt_tokens")
	out, hasOut, ok2 := count("completion_tokens")
	total, hasTotal, ok3 := count("total_tokens")
	if !ok1 || !ok2 || !ok3 || !hasIn || !hasOut {
		return gateway.TokenUsage{}, false
	}
	if hasTotal && total != in+out {
		return gateway.TokenUsage{}, false
	}
	return gateway.TokenUsage{Input: in, Output: out}, true
}
