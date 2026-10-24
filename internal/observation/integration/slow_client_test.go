package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
)

// syncBuffer is a log sink safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// smallSendBuffers shrinks the server's per-connection send buffer, so a client
// that stops reading blocks the server after kilobytes rather than megabytes.
type smallSendBuffers struct{ net.Listener }

func (l smallSendBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok && err == nil {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// dialSmallReceive connects with a tiny receive buffer set before the handshake.
func dialSmallReceive(t testing.TB, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048) }); err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// completeFrames returns the fully received frames of a partial event-stream body.
func completeFrames(body []byte) []ot.Frame {
	i := bytes.LastIndex(body, []byte("\n\n"))
	if i < 0 {
		return nil
	}
	return ot.ParseFrames(string(body[:i+2]))
}

func eventID(t testing.TB, fr ot.Frame) string {
	t.Helper()
	var e struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(fr.Data), &e); err != nil {
		t.Fatalf("frame data is not an event: %v: %.80s", err, fr.Data)
	}
	return e.ID
}

// Test_slow_client: a stalled HTTP reader reaches the configured buffer and
// write bounds, disconnects, releases workers and connections, does not delay
// another client, and can resume from its last received cursor.
func Test_slow_client(t *testing.T) {
	f := newFixture(t, ot.Options{Events: func(o *eventspg.Options) {
		o.Backlog = events.BacklogLimits{MaxEvents: 100000, MaxBytes: 256 << 20}
	}})
	logs := &syncBuffer{}
	ho := ot.HandlerOptions()
	ho.WriteTimeout = 400 * time.Millisecond
	ho.MaxBufferedBytes = 16 << 10
	ho.Logger = newTestLogger(logs)
	svc := f.Service()
	srv := f.NewServer(svc, f.Authority(), ho, ot.WithListener(func(l net.Listener) net.Listener { return smallSendBuffers{l} }))
	url := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA)
	token := string(f.viewer)

	// The stalled client: raw TCP so its receive buffer is tiny and it can stop reading.
	addr := strings.TrimPrefix(srv.URL, "http://")
	stalled := dialSmallReceive(t, addr)
	req := fmt.Sprintf("GET /api/v1/tenants/%s/projects/%s/events HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nAccept: text/event-stream\r\n\r\n",
		ot.TenantA, ot.ProjectA, addr, token)
	if _, err := io.WriteString(stalled, req); err != nil {
		t.Fatal(err)
	}
	// Read only until the snapshot frame is complete, then stop reading.
	var received bytes.Buffer
	buf := make([]byte, 512)
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !(bytes.Contains(received.Bytes(), []byte("event: snapshot")) && bytes.Contains(received.Bytes(), []byte("}\n\n"))) {
		n, err := stalled.Read(buf)
		received.Write(buf[:n])
		if err != nil {
			t.Fatalf("reading the head of the stalled stream: %v", err)
		}
	}

	// A healthy client connected at the same time.
	healthy := ot.Connect(t, url, token, nil)
	if fr := healthy.NextN(t, 1, 3*time.Second)[0]; fr.Event != "snapshot" {
		t.Fatalf("healthy first frame %+v", fr)
	}

	const total = 400
	var ids []string
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("t024_slow_%03d", i)
		ids = append(ids, id)
		f.Commit(ot.TenantA, nil, ot.BigWorkEvent(ot.TenantA, ot.ProjectA, id, "wi_slow", int64(i+1)))
	}
	committedAt := time.Now()

	t.Run("a healthy client is not delayed by the stalled one", func(t *testing.T) {
		got := healthy.NextN(t, total, 10*time.Second)
		if lag := time.Since(committedAt); lag > 8*time.Second {
			t.Fatalf("the healthy client needed %s", lag)
		}
		for i, fr := range got {
			if eventID(t, fr) != ids[i] {
				t.Fatalf("healthy frame %d is %s, want %s", i, eventID(t, fr), ids[i])
			}
		}
	})

	t.Run("the stalled client is disconnected at the write bound", func(t *testing.T) {
		waitFor(t, 10*time.Second, "the slow-client disconnect", func() bool { return strings.Contains(logs.String(), "reason=slow_client") })
		if n := strings.Count(logs.String(), "reason=slow_client"); n != 1 {
			t.Fatalf("%d slow-client disconnects logged, want exactly the stalled one:\n%s", n, logs.String())
		}
	})

	t.Run("no database connection is held and workers are released", func(t *testing.T) {
		waitFor(t, 5*time.Second, "all runtime connections to be released", func() bool { return f.Runtime.Stat().AcquiredConns() == 0 })
		// Only the healthy stream remains; ending it leaves no session behind.
		healthy.Close()
		waitFor(t, 5*time.Second, "the pool to be idle", func() bool { return f.Runtime.Stat().AcquiredConns() == 0 })
	})

	t.Run("the stalled client resumes from its last received cursor without a gap", func(t *testing.T) {
		// Drain what the kernel buffered; the server already closed its side.
		_ = stalled.SetReadDeadline(time.Now().Add(10 * time.Second))
		rest, err := io.ReadAll(stalled)
		if err != nil && !strings.Contains(err.Error(), "reset") {
			t.Fatalf("draining the stalled connection: %v", err)
		}
		received.Write(rest)
		body := received.Bytes()
		if i := bytes.Index(body, []byte("\r\n\r\n")); i >= 0 {
			body = body[i+4:]
		}
		body = dechunk(body)
		var frames []ot.Frame
		for _, fr := range completeFrames(body) {
			if fr.Comment == "" { // heartbeats carry no cursor
				frames = append(frames, fr)
			}
		}
		if len(frames) < 2 || len(frames) >= total {
			t.Fatalf("the stalled client received %d frames; it must receive some and not all %d", len(frames), total)
		}
		t.Logf("the stalled client received %d of %d frames before the disconnect", len(frames), total)
		last := frames[len(frames)-1]
		lastIdx := -1
		var seen []string
		for _, fr := range frames {
			if fr.Event == "snapshot" || fr.Event == "checkpoint" {
				continue
			}
			seen = append(seen, eventID(t, fr))
		}
		for i, id := range seen {
			if id != ids[i] {
				t.Fatalf("the stalled client's frame %d is %s, want %s", i, id, ids[i])
			}
			lastIdx = i
		}
		resumed := ot.Connect(t, url, token, map[string]string{"Last-Event-ID": last.ID})
		want := total - (lastIdx + 1)
		got := resumed.NextN(t, want, 15*time.Second)
		for i, fr := range got {
			if id := eventID(t, fr); id != ids[lastIdx+1+i] {
				t.Fatalf("resumed frame %d is %s, want %s: events were skipped or repeated", i, id, ids[lastIdx+1+i])
			}
		}
		if fr, ok := resumed.Next(200 * time.Millisecond); ok {
			t.Fatalf("unexpected extra frame %+v", fr)
		}
	})

}

// dechunk decodes an HTTP/1.1 chunked body, tolerating a truncated tail.
func dechunk(b []byte) []byte {
	var out []byte
	for len(b) > 0 {
		i := bytes.Index(b, []byte("\r\n"))
		if i < 0 {
			break
		}
		var n int
		if _, err := fmt.Sscanf(string(b[:i]), "%x", &n); err != nil || n == 0 {
			break
		}
		b = b[i+2:]
		if len(b) < n {
			out = append(out, b...)
			break
		}
		out = append(out, b[:n]...)
		b = b[n:]
		b = bytes.TrimPrefix(b, []byte("\r\n"))
	}
	return out
}
