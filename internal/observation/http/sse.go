package observationhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/observation"
)

// Frame names that are not event types.
const (
	frameSnapshot   = "snapshot"
	frameCheckpoint = "checkpoint"
	frameResync     = "resync_required"
)

// probeStreaming refuses a writer that cannot stream before any header is
// committed: it must support per-write deadlines (so a stalled reader is
// disconnected) and flushing (so frames reach the client).
func probeStreaming(w http.ResponseWriter, timeout time.Duration) error {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("the response writer cannot set write deadlines: %w", err)
	}
	for cur := w; ; {
		switch v := cur.(type) {
		case interface{ FlushError() error }:
			return nil
		case http.Flusher:
			return nil
		case interface{ Unwrap() http.ResponseWriter }:
			cur = v.Unwrap()
		default:
			return errors.New("the response writer cannot flush: " + http.ErrNotSupported.Error())
		}
	}
}

// sseSink writes the stream's frames. Frames collect in a bounded buffer that is
// written under a write deadline when it reaches maxBuffered bytes and whenever
// the service flushes (after each page and heartbeat). A reader that does not
// accept data within the deadline ends the stream as a slow client; frames are
// never dropped, because the stream is a pull from the durable log and resumes
// from the last cursor the client received.
type sseSink struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	codec        *httpapi.CursorCodec
	cs           httpapi.CursorScope
	buf          bytes.Buffer
	maxBuffered  int
	writeTimeout time.Duration
}

var _ observation.Sink = (*sseSink)(nil)

func newSink(w http.ResponseWriter, codec *httpapi.CursorCodec, cs httpapi.CursorScope, o HandlerOptions) *sseSink {
	return &sseSink{w: w, rc: http.NewResponseController(w), codec: codec, cs: cs, maxBuffered: o.MaxBufferedBytes, writeTimeout: o.WriteTimeout}
}

func (s *sseSink) frame(event, id string, data []byte) error {
	if strings.ContainsAny(event+id, "\r\n") || bytes.ContainsAny(data, "\r\n") {
		return errors.New("observationhttp: a frame field contains a line break")
	}
	s.buf.WriteString("event: ")
	s.buf.WriteString(event)
	s.buf.WriteByte('\n')
	if id != "" {
		s.buf.WriteString("id: ")
		s.buf.WriteString(id)
		s.buf.WriteByte('\n')
	}
	s.buf.WriteString("data: ")
	s.buf.Write(data)
	s.buf.WriteString("\n\n")
	if s.buf.Len() >= s.maxBuffered {
		return s.flush()
	}
	return nil
}

// flush writes the buffer under a fresh deadline.
func (s *sseSink) flush() error {
	if s.buf.Len() == 0 {
		return nil
	}
	defer s.buf.Reset()
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		return err
	}
	_, err := s.w.Write(s.buf.Bytes())
	if err == nil {
		err = s.rc.Flush()
	}
	return slowOrErr(err)
}

func slowOrErr(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%w: %v", observation.ErrSlowClient, err)
	}
	return err
}

func (s *sseSink) cursor(position int64) (string, error) { return issue(s.codec, s.cs, position) }

// Snapshot implements observation.Sink.
func (s *sseSink) Snapshot(_ context.Context, snap observation.Snapshot) error {
	cur, err := s.cursor(snap.Position)
	if err != nil {
		return err
	}
	data, err := json.Marshal(snapshotDTO(cur, snap))
	if err != nil {
		return err
	}
	return s.frame(frameSnapshot, cur, data)
}

// Event implements observation.Sink. The data is the retained envelope.
func (s *sseSink) Event(_ context.Context, d observation.Delivery) error {
	cur, err := s.cursor(d.Position)
	if err != nil {
		return err
	}
	data, err := json.Marshal(d.Envelope)
	if err != nil {
		return err
	}
	return s.frame(d.Envelope.Type, cur, data)
}

// Checkpoint implements observation.Sink.
func (s *sseSink) Checkpoint(_ context.Context, position int64) error {
	cur, err := s.cursor(position)
	if err != nil {
		return err
	}
	data, err := json.Marshal(Checkpoint{Cursor: cur})
	if err != nil {
		return err
	}
	return s.frame(frameCheckpoint, cur, data)
}

// Heartbeat implements observation.Sink with an SSE comment line.
func (s *sseSink) Heartbeat(context.Context) error {
	s.buf.WriteString(": heartbeat\n\n")
	return nil
}

// Flush implements observation.Sink.
func (s *sseSink) Flush(context.Context) error { return s.flush() }

// Resync implements observation.Sink. The frame deliberately carries no id, so a
// client's Last-Event-ID stays at the last real position instead of advancing.
func (s *sseSink) Resync(_ context.Context, cause error) error {
	if !errors.Is(cause, observation.ErrCursorExpired) {
		return nil
	}
	data, err := json.Marshal(ResyncRequired{
		Code:    ResyncRequiredCodeCursorExpired,
		Message: "the position is older than the retained history; fetch a snapshot and resume from its cursor",
	})
	if err != nil {
		return err
	}
	return s.frame(frameResync, "", data)
}

// flushHeaders commits the response head before the first frame.
func (s *sseSink) flushHeaders() error {
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		return err
	}
	return slowOrErr(s.rc.Flush())
}
