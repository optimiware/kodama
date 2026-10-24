package entrypoint

// Container-log mirror (task T-078). The command's merged stdout/stderr is
// already captured for the retained `command.log`; this file additionally
// copies the same bytes, after redaction and within the same bound, to the
// container's own log stream (the entrypoint's stderr). The Kubernetes log
// stream is what the controller fallback (T-064) can still read when the
// wrapper cannot upload its retained log, so the mirror is a best-effort
// transport for diagnostics, never durable evidence: it carries no sidecar, is
// never described as complete, and can lose output (queue overflow, a failed
// sink, a held tail that is discarded). Nothing here changes the retained
// capture, the sidecar, the result or the process-tree handling.
//
// Safety properties (each has a test in mirror_test.go):
//
//   - Redaction is the same logs.Redactor and the same value set as the
//     retained log (injected file/env secrets, the admission credential, every
//     issued or refreshed token, and every representation of the checkout
//     credential). Bytes reach the sink only through the redactor.
//   - The mirror fails closed: a redaction set the redactor refuses disables
//     the mirror; unredacted bytes are never emitted as a fallback.
//   - Tokens issued while the command runs enter the redaction set before the
//     next byte is processed. The redactor cannot take new values, so it is
//     rebuilt. The bytes it holds back (a possible start of a value) are never
//     flushed and never dropped, because the rest of the value could follow: the
//     mirror keeps a window of the latest raw input and replays it into the new
//     redactor, discarding the replay's output (that output was already
//     emitted or is still held). The tradeoff is over-redaction at most: a
//     replayed window can start inside a value that was already redacted and
//     merge a few more bytes into a marker.
//   - The capture path never waits for the sink: redacted bytes go to a
//     bounded queue drained by one goroutine; a full queue drops output, a
//     failed sink stops writing. Close waits at most the kill grace for the
//     queue to drain and then abandons the sink.
//   - Output is bounded: at most the retained capture capacity of raw input
//     and of redacted output, then one truncation marker.

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/logs"
)

// Mirror-only markers. They are constant text and may appear in the container
// log; T-064's fallback treats any of them as proof that the mirror is partial.
const (
	// MirrorTruncationMarker ends a mirror that reached its bound (it is the
	// same marker the retained command.log carries when it is truncated).
	MirrorTruncationMarker = truncationMarker
	// MirrorGapMarker is written once, before the first output that follows
	// output the mirror had to drop (queue overflow).
	MirrorGapMarker = "\n[kodama: container log mirror dropped output]\n"
	// MirrorDisabledMarker replaces all output when the mirror cannot redact.
	MirrorDisabledMarker = "\n[kodama: container log mirror disabled: redaction unavailable]\n"
)

const (
	// mirrorQueueChunks and mirrorChunkMax bound the redacted output waiting
	// for the sink (512 KiB at most).
	mirrorQueueChunks = 64
	mirrorChunkMax    = 8 << 10
	// mirrorLineMax is the longest unterminated line the mirror holds back.
	// Whole lines are written so that the entrypoint's own JSON log lines,
	// written to the same stream, never land inside a command line.
	mirrorLineMax = 4 << 10
)

// needleSource returns the complete set of values to remove and a generation
// that changes whenever the set grows.
type needleSource func() ([][]byte, uint64)

type mirrorState int

const (
	mirrorActive  mirrorState = iota
	mirrorStopped             // bound reached, closed, or disabled
)

// MirrorStats describes what a closed mirror did. It never contains output.
type MirrorStats struct {
	Emitted   int64 // bytes handed to the sink queue
	Truncated bool  // the bound was reached
	Dropped   bool  // output was lost to a full queue or a failed sink
	Disabled  bool  // the redaction set was unusable
	SinkErr   error
	Abandoned bool // the sink did not drain within the grace period
}

type containerMirror struct {
	sink io.Writer
	src  needleSource
	log  *slog.Logger

	mu       sync.Mutex
	state    mirrorState
	gen      uint64
	red      *logs.Redactor
	rawLeft  int64
	outLeft  int64
	window   []byte // latest raw input, replayed into a rebuilt redactor
	pending  []byte // redacted bytes of an unterminated line
	last     byte   // last byte queued, 0 when nothing was queued
	ch       chan []byte
	gap      bool
	gapDone  bool
	stats    MirrorStats
	abandon  atomic.Bool
	done     chan struct{}
	sinkErr  atomic.Value // error
	sinkFail atomic.Bool
}

// newContainerMirror returns nil when there is no sink. The set is built
// before the first byte; an unusable set yields a disabled mirror that writes
// only MirrorDisabledMarker.
func newContainerMirror(sink io.Writer, capacity int64, src needleSource, log *slog.Logger) *containerMirror {
	if sink == nil {
		return nil
	}
	m := &containerMirror{
		sink: sink, src: src, log: log,
		rawLeft: capacity, outLeft: capacity,
		ch: make(chan []byte, mirrorQueueChunks), done: make(chan struct{}),
	}
	go m.drain()
	m.mu.Lock()
	m.rebuildLocked()
	m.mu.Unlock()
	return m
}

// mirrorWindow bounds the replayed raw input: the redactor holds at most
// 4*maxLen+4096 bytes plus one region, and no value exceeds
// logs.MaxRedactValueLen.
const mirrorWindow = 5*logs.MaxRedactValueLen + 4096

// rebuildLocked replaces the redactor with one over the current set, primed
// with the window so that it holds exactly what a redactor of the new set that
// had seen the whole stream would hold.
func (m *containerMirror) rebuildLocked() {
	needles, gen := m.src()
	red, err := redactionSet(needles)
	if err != nil {
		m.disableLocked()
		return
	}
	_ = red.Write(m.window) // already emitted, or held again; never re-emitted
	m.red, m.gen = red, gen
}

func (m *containerMirror) disableLocked() {
	if m.state != mirrorActive {
		return
	}
	m.stats.Disabled = true
	m.red, m.pending, m.window = nil, nil, nil
	m.state = mirrorStopped
	m.enqueueLocked([]byte(MirrorDisabledMarker))
	m.log.Warn("container log mirror disabled", "reason", "redaction set not usable")
}

// Write offers command output to the mirror. It never fails and never waits
// for the sink.
func (m *containerMirror) Write(p []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != mirrorActive || len(p) == 0 {
		return
	}
	if _, gen := m.src(); gen != m.gen {
		if m.rebuildLocked(); m.state != mirrorActive {
			return
		}
	}
	take := int64(len(p))
	capped := false
	if take >= m.rawLeft {
		take, capped = m.rawLeft, take > m.rawLeft
	}
	m.rawLeft -= take
	m.window = append(m.window, p[:take]...)
	if over := len(m.window) - mirrorWindow; over > 0 {
		m.window = append(m.window[:0], m.window[over:]...)
	}
	// A held tail is withheld when input stops short: it may start a value
	// whose rest was never seen.
	m.acceptLocked(m.red.Write(p[:take]), capped)
}

// acceptLocked adds redacted bytes, applying the output bound.
func (m *containerMirror) acceptLocked(out []byte, capped bool) {
	if int64(len(out)) >= m.outLeft {
		capped = capped || int64(len(out)) > m.outLeft
		out = out[:m.outLeft]
	}
	m.outLeft -= int64(len(out))
	m.pending = append(m.pending, out...)
	m.flushLocked(capped)
	if capped {
		m.stats.Truncated = true
		m.enqueueLocked([]byte(MirrorTruncationMarker))
		m.red, m.window, m.state = nil, nil, mirrorStopped
	}
}

// flushLocked queues whole lines; with force it queues everything.
func (m *containerMirror) flushLocked(force bool) {
	if len(m.pending) == 0 {
		return
	}
	cut := bytes.LastIndexByte(m.pending, '\n') + 1
	// Only the unterminated tail is bounded: complete lines queued with it must
	// not drag a partial line along, or a dropped chunk would leave half a line.
	if force || len(m.pending)-cut > mirrorLineMax {
		cut = len(m.pending)
	}
	if cut == 0 {
		return
	}
	m.enqueueLocked(m.pending[:cut])
	m.pending = append(m.pending[:0], m.pending[cut:]...)
}

// enqueueLocked copies b into the bounded queue without blocking. Output that
// does not fit is dropped (it is already redacted) and noted once.
func (m *containerMirror) enqueueLocked(b []byte) {
	for len(b) > 0 {
		n := min(len(b), mirrorChunkMax)
		// Keep a chunk on a line boundary whenever one exists in reach. The
		// container stream drops whole writes when its queue is full; splitting
		// a line here would let a drop leave a partial command line.
		if n < len(b) {
			if i := bytes.LastIndexByte(b[:n], '\n'); i >= 0 {
				n = i + 1
			} else if i := bytes.IndexByte(b[n:], '\n'); i >= 0 {
				n += i + 1
			}
		}
		chunk := append([]byte(nil), b[:n]...)
		b = b[n:]
		if m.gap && !m.gapDone {
			select {
			case m.ch <- []byte(MirrorGapMarker):
				m.gap, m.gapDone = false, true
			default:
			}
		}
		select {
		case m.ch <- chunk:
			m.stats.Emitted += int64(len(chunk))
			m.last = chunk[len(chunk)-1]
		default:
			m.gap, m.stats.Dropped = true, true
		}
	}
}

// drain is the only goroutine that touches the sink.
func (m *containerMirror) drain() {
	defer close(m.done)
	for b := range m.ch {
		if m.abandon.Load() || m.sinkFail.Load() {
			continue
		}
		if _, err := m.sink.Write(b); err != nil {
			m.sinkErr.Store(err)
			m.sinkFail.Store(true)
		}
	}
}

// Close ends the mirror after the command and its output copier have stopped.
// whole reports that the output was read to its end of a command that ran to
// its own end: only then is the redactor's held tail released. Otherwise it is
// discarded, exactly as for the retained log. Close waits at most wait for the
// sink and returns what happened.
func (m *containerMirror) Close(whole bool, wait time.Duration) MirrorStats {
	m.mu.Lock()
	if m.state == mirrorActive {
		if _, gen := m.src(); gen != m.gen {
			m.rebuildLocked()
		}
	}
	if m.state == mirrorActive {
		if whole {
			m.acceptLocked(m.red.Flush(), false)
		}
		m.flushLocked(true)
		if m.last != 0 && m.last != '\n' {
			// Keep the entrypoint's next log line off the command's last line.
			m.enqueueLocked([]byte("\n"))
		}
		m.state = mirrorStopped
	}
	m.state = mirrorStopped
	close(m.ch)
	m.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-m.done:
	case <-timer.C:
		m.abandon.Store(true)
	}
	m.mu.Lock()
	if m.abandon.Load() {
		m.stats.Abandoned, m.stats.Dropped = true, true
	}
	if err, _ := m.sinkErr.Load().(error); err != nil {
		m.stats.SinkErr, m.stats.Dropped = err, true
	}
	stats := m.stats
	m.mu.Unlock()
	if stats.Dropped || stats.Abandoned || stats.SinkErr != nil {
		// Logged outside the lock; the real logger writes to a containerStream,
		// which never waits for a stalled container log.
		m.log.Warn("container log mirror is partial", "dropped", stats.Dropped, "abandoned", stats.Abandoned, "sinkFailed", stats.SinkErr != nil)
	}
	return stats
}

// mirrorTee writes the command's output to the retained capture first and
// then offers the same bytes to the mirror. The capture's result is returned
// unchanged and the mirror cannot fail or delay it.
type mirrorTee struct {
	capture io.Writer
	mirror  *containerMirror
}

func (t mirrorTee) Write(p []byte) (int, error) {
	n, err := t.capture.Write(p)
	t.mirror.Write(p)
	return n, err
}

// Container stream bounds. The stream is what the real CLI hands to both the
// entrypoint's own log lines and the mirror, so lifecycle code (capture
// freeze, diagnostic upload, result) must never wait for it.
const (
	// streamQueueBytes bounds the output waiting for the underlying writer.
	streamQueueBytes = 1 << 20
	// streamFlushWait bounds the final flush at process exit.
	streamFlushWait = 2 * time.Second
	// streamDropMarker is written once before the first output that follows
	// output the stream had to drop because the underlying writer stalled or
	// failed. It is constant text.
	streamDropMarker = "\n[kodama: container log dropped output]\n"
)

// containerStream is an io.Writer that never blocks, never fails and never
// waits for the underlying writer. Each Write is queued whole (so a line from
// one producer is never split by another) and one goroutine, the only one
// that touches the underlying writer, delivers the queue in order. When the
// queue is full, or the underlying writer failed, the Write is dropped and a
// single marker precedes the next delivered output.
type containerStream struct {
	w io.Writer

	mu      sync.Mutex
	queue   [][]byte
	queued  int
	dropped bool
	closed  bool
	failed  bool
	wake    chan struct{}
	done    chan struct{}
}

func newContainerStream(w io.Writer) *containerStream {
	s := &containerStream{w: w, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go s.run()
	return s
}

// Write always reports success: delivery is best-effort diagnostics.
func (s *containerStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	if s.closed || s.failed || s.queued+len(p) > streamQueueBytes {
		s.dropped = true
		s.mu.Unlock()
		return len(p), nil
	}
	s.queue = append(s.queue, append([]byte(nil), p...))
	s.queued += len(p)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (s *containerStream) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		batch, closed, dropped := s.queue, s.closed, s.dropped
		s.queue, s.queued = nil, 0
		if len(batch) > 0 {
			s.dropped = false
		}
		s.mu.Unlock()
		if len(batch) == 0 {
			if closed {
				return
			}
			<-s.wake
			continue
		}
		if dropped {
			batch = append([][]byte{[]byte(streamDropMarker)}, batch...)
		}
		for _, b := range batch {
			if _, err := s.w.Write(b); err != nil {
				s.mu.Lock()
				s.failed, s.queue, s.queued = true, nil, 0
				s.mu.Unlock()
				break
			}
		}
	}
}

// Close stops accepting output and waits at most wait for what is queued to
// reach the underlying writer. A stalled writer is abandoned: its goroutine
// ends when the writer returns.
func (s *containerStream) Close(wait time.Duration) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
	}
}
