package gatewayhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/gateway"
)

// streamMessages are the fixed texts of in-stream failure events. They never
// carry provider output.
var streamMessages = map[string]string{
	"stream_truncated":      "the model stream ended before it completed",
	"stream_idle":           "the model stream stalled",
	"protocol_violation":    "the model stream violated the supported protocol",
	"response_too_large":    "the model response exceeded its bound",
	"bound_exceeded":        "the model reported usage beyond the run's bounds",
	"client_cancelled":      "the request was cancelled",
	"attempt_cancelled":     "the attempt is no longer authorized",
	"authority_unavailable": "the attempt's authority could not be confirmed",
	"deadline_exceeded":     "the request deadline passed",
	"unavailable":           "the outcome could not be recorded",
}

// sseSink writes one streaming response. It implements gateway.Sink: the first
// frame commits the status and headers, each frame is flushed under a write
// deadline so a stalled reader fails the request instead of holding it, and a
// failure is reported as one error event, never followed by [DONE].
type sseSink struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration

	mu      sync.Mutex
	started bool
	closed  bool
}

var _ gateway.Sink = (*sseSink)(nil)

func newSSE(w http.ResponseWriter, timeout time.Duration) *sseSink {
	return &sseSink{w: w, rc: http.NewResponseController(w), timeout: timeout}
}

// Started implements gateway.Sink.
func (s *sseSink) Started() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.started }

func (s *sseSink) begin() {
	if s.started {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
}

func (s *sseSink) write(parts ...[]byte) error {
	if s.closed {
		return errors.New("gatewayhttp: the stream is closed")
	}
	s.begin()
	if s.timeout > 0 {
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout)) // unsupported writers keep the server's own deadline
	}
	for _, p := range parts {
		if _, err := s.w.Write(p); err != nil {
			s.closed = true
			return err
		}
	}
	if err := s.rc.Flush(); err != nil {
		s.closed = true
		return err
	}
	return nil
}

// Frame implements gateway.Sink.
func (s *sseSink) Frame(ctx context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.write([]byte("data: "), data, []byte("\n\n"))
}

// Done implements gateway.Sink.
func (s *sseSink) Done(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { s.closed = true }()
	return s.write([]byte("data: [DONE]\n\n"))
}

// Fail implements gateway.Sink.
func (s *sseSink) Fail(_ context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, ok := streamMessages[code]
	if !ok {
		code, msg = "unavailable", streamMessages["unavailable"]
	}
	body, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": "kodama_gateway"}})
	if err != nil {
		return err
	}
	defer func() { s.closed = true }()
	return s.write([]byte("data: "), body, []byte("\n\n"))
}
