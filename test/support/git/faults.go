package git

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faults holds deterministic failure injection around the receive-pack POST,
// the request that actually updates a ref.
type faults struct {
	mu        sync.Mutex
	holds     []*Hold
	allHolds  []*Hold // every hold ever created, so cleanup releases parked requests too
	dropAcks  int
	infoDelay time.Duration
	released  bool
	// inflight counts receive-pack requests past their hold, still running.
	inflight atomic.Int64
}

// Hold blocks the next receive-pack POST before the backend sees it, so a test
// can act at an exact point: for example push as a human between the
// publisher's ref advertisement and its update, or let the client time out
// while the server still completes the request later.
type Hold struct {
	// Reached is closed when a receive-pack POST is parked.
	Reached chan struct{}
	release chan struct{}
	once    sync.Once
	reached sync.Once
	// DropAck makes the released request still reach the backend but answers
	// with a broken connection, as a lost acknowledgment would.
	DropAck bool
}

// Release lets the parked request proceed.
func (h *Hold) Release() { h.once.Do(func() { close(h.release) }) }

// HoldReceivePack parks the next receive-pack POST until Release.
func (s *Server) HoldReceivePack() *Hold {
	h := &Hold{Reached: make(chan struct{}), release: make(chan struct{})}
	s.faults.mu.Lock()
	s.faults.holds = append(s.faults.holds, h)
	s.faults.allHolds = append(s.faults.allHolds, h)
	s.faults.mu.Unlock()
	return h
}

// DropNextReceivePackAcks lets the next n receive-pack POSTs reach the backend
// (the ref is updated) but breaks the connection instead of answering.
func (s *Server) DropNextReceivePackAcks(n int) {
	s.faults.mu.Lock()
	s.faults.dropAcks = n
	s.faults.mu.Unlock()
}

// DelayInfoRefs delays every upload-pack ref advertisement, honouring client
// disconnects.
func (s *Server) DelayInfoRefs(d time.Duration) {
	s.faults.mu.Lock()
	s.faults.infoDelay = d
	s.faults.mu.Unlock()
}

func (f *faults) releaseAll() {
	f.mu.Lock()
	f.released = true
	for _, h := range f.allHolds {
		h.Release()
	}
	f.mu.Unlock()
}

// delayInfoRefs sleeps for the configured delay; it reports false when the
// client disconnected first.
func (f *faults) delayInfoRefs(r *http.Request) bool {
	f.mu.Lock()
	d := f.infoDelay
	f.mu.Unlock()
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

func (f *faults) receivePack(s *Server, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var hold *Hold
	if len(f.holds) > 0 {
		hold, f.holds = f.holds[0], f.holds[1:]
	}
	drop := f.dropAcks > 0
	if drop {
		f.dropAcks--
	}
	f.mu.Unlock()

	if hold == nil && !drop {
		s.backend.ServeHTTP(w, r)
		return
	}
	f.inflight.Add(1)
	defer f.inflight.Add(-1)
	body, _ := io.ReadAll(r.Body)
	if hold != nil {
		hold.reached.Do(func() { close(hold.Reached) })
		<-hold.release
		drop = drop || hold.DropAck
	}
	if drop {
		// Run the update to completion, then break the connection.
		s.serveBuffered(httptest.NewRecorder(), r.WithContext(context.WithoutCancel(r.Context())), body)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	s.serveBuffered(w, r.WithContext(context.WithoutCancel(r.Context())), body)
}

// WaitIdle blocks until every request the server is currently handling,
// including ones released after their client disconnected, has finished.
func (s *Server) WaitIdle(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.faults.inflightCount() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("server requests did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *faults) inflightCount() int64 { return f.inflight.Load() }
