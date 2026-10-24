package run

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// FrameKind classifies one frame of the observation stream.
type FrameKind int

const (
	// FrameEvent is a retained event; Event holds its type and ID its cursor.
	FrameEvent FrameKind = iota
	// FrameSnapshot is the initial snapshot of a fresh stream.
	FrameSnapshot
	// FrameCheckpoint carries a cursor after a filtered stretch of events.
	FrameCheckpoint
	// FrameHeartbeat is an SSE comment line; it proves liveness only.
	FrameHeartbeat
	// FrameResync is the server's last frame when retention overtook the stream.
	FrameResync
)

// Frame is one complete server-sent event.
type Frame struct {
	Kind  FrameKind
	Event string
	ID    string
	Data  []byte
}

// Errors of the frame reader. Neither is retried: they describe a stream the
// client cannot interpret, and reconnecting would reproduce them.
var (
	errFrameTooLarge = errors.New("observation frame exceeds the size bound")
	errBadFrame      = errors.New("observation frame is malformed")
)

// frameReader parses text/event-stream. A frame is delivered only when its
// terminating blank line has been read, so the caller can advance its resume
// cursor only past complete frames; a frame cut off by a disconnect is dropped.
type frameReader struct {
	r   *bufio.Reader
	max int
	// first reports that no byte has been read yet (to strip one BOM).
	first bool
}

func newFrameReader(r io.Reader, max int) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 16<<10), max: max, first: true}
}

// readLine returns one line without its terminator, bounded by remaining.
func (f *frameReader) readLine(remaining int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := f.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > remaining+2 {
			return nil, errFrameTooLarge
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// next returns the next frame. io.EOF means the stream ended cleanly between
// frames; io.ErrUnexpectedEOF means it ended inside one.
func (f *frameReader) next() (Frame, error) {
	var (
		event, id string
		data      [][]byte
		have      bool
		used      int
	)
	for {
		line, err := f.readLine(f.max - used)
		if err != nil {
			if errors.Is(err, io.EOF) && have {
				return Frame{}, io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
		if f.first {
			f.first = false
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		used += len(line) + 1
		if used > f.max {
			return Frame{}, errFrameTooLarge
		}
		if len(line) == 0 {
			if !have {
				continue
			}
			return buildFrame(event, id, data)
		}
		if line[0] == ':' {
			if !have {
				return Frame{Kind: FrameHeartbeat}, nil
			}
			continue
		}
		name, value, _ := strings.Cut(string(line), ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			event, have = value, true
		case "id":
			if strings.ContainsRune(value, 0) {
				return Frame{}, errBadFrame
			}
			id, have = value, true
		case "data":
			data, have = append(data, []byte(value)), true
		case "retry":
			have = true
		default:
			have = true // unknown fields are ignored, per the SSE format
		}
	}
}

func buildFrame(event, id string, data [][]byte) (Frame, error) {
	f := Frame{Event: event, ID: id, Data: bytes.Join(data, []byte("\n"))}
	switch event {
	case "snapshot":
		f.Kind = FrameSnapshot
	case "checkpoint":
		f.Kind = FrameCheckpoint
	case "resync_required":
		f.Kind = FrameResync
	default:
		f.Kind = FrameEvent
		if event == "" {
			f.Event = "message"
		}
	}
	return f, nil
}
