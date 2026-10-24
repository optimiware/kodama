package observationtest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Frame is one parsed server-sent event or comment.
type Frame struct {
	Event   string
	ID      string
	Data    string
	Comment string
}

// IsHeartbeat reports a heartbeat comment.
func (f Frame) IsHeartbeat() bool { return f.Comment == "heartbeat" }

// ParseFrames splits an event-stream body into frames.
func ParseFrames(body string) []Frame {
	var out []Frame
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f Frame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ": "):
				f.Comment = strings.TrimPrefix(line, ": ")
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, f)
	}
	return out
}

// Stream is an open event-stream response read by a goroutine.
type Stream struct {
	Resp *http.Response
	// Body holds the response body when the response was not an event stream.
	Body   []byte
	Frames chan Frame
	cancel context.CancelFunc
	done   chan struct{}
}

// Connect issues an authenticated GET asking for the event stream. headers
// may add or override request headers. The caller reads Frames.
func Connect(t testing.TB, url, token string, headers map[string]string) *Stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	s := &Stream{Resp: resp, Frames: make(chan Frame, 1024), cancel: cancel, done: make(chan struct{})}
	t.Cleanup(s.Close)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		s.Body, _ = io.ReadAll(resp.Body)
		close(s.Frames)
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		defer close(s.Frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line != "" {
				cur = append(cur, line)
				continue
			}
			if len(cur) > 0 {
				fs := ParseFrames(strings.Join(cur, "\n"))
				cur = nil
				if len(fs) == 1 {
					s.Frames <- fs[0]
				}
			}
		}
	}()
	return s
}

// Close ends the request.
func (s *Stream) Close() {
	s.cancel()
	_ = s.Resp.Body.Close()
	<-s.done
}

// Next returns the next non-heartbeat frame, or false at the end of the stream
// or after the timeout.
func (s *Stream) Next(timeout time.Duration) (Frame, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-s.Frames:
			if !ok {
				return Frame{}, false
			}
			if f.IsHeartbeat() {
				continue
			}
			return f, true
		case <-deadline:
			return Frame{}, false
		}
	}
}

// NextN reads n frames (excluding heartbeats).
func (s *Stream) NextN(t testing.TB, n int, timeout time.Duration) []Frame {
	t.Helper()
	var out []Frame
	for len(out) < n {
		f, ok := s.Next(timeout)
		if !ok {
			t.Fatalf("stream ended or stalled after %d of %d frames: %+v", len(out), n, out)
		}
		out = append(out, f)
	}
	return out
}

// Ended waits for the server to end the stream.
func (s *Stream) Ended(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-s.Frames:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// GetJSON issues an authenticated JSON GET and returns status, headers and body.
func GetJSON(t testing.TB, url, token string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

// ErrorCode extracts error.code from an error envelope ("" when absent).
func ErrorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code
}
