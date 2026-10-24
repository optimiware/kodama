package logs

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// lineReader reads newline-terminated lines of bounded size. A line longer
// than max is cut at max bytes and the rest of it is discarded up to the next
// newline, so memory is bounded by max whatever the input holds. It also
// bounds the total bytes it will consume from the source.
type lineReader struct {
	br      *bufio.Reader
	max     int
	left    int64 // remaining scan budget
	scanned int64
}

func newLineReader(src io.Reader, maxLine int, scanLimit int64) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(src, 4096), max: maxLine, left: scanLimit}
}

var (
	errScanLimit = errors.New("logs: scan limit reached")
)

// next returns the next line without its newline. truncated is true when the
// line was cut at the bound. At the end of input it returns io.EOF; when the
// scan budget is spent it returns errScanLimit.
func (l *lineReader) next() (line []byte, truncated bool, err error) {
	var out []byte
	for {
		if l.left <= 0 {
			if len(out) > 0 {
				return out, truncated, nil
			}
			return nil, false, errScanLimit
		}
		chunk, perr := l.br.ReadSlice('\n')
		n := len(chunk)
		if int64(n) > l.left {
			// Consume only the budget; the rest stays unread and unseen.
			chunk = chunk[:l.left]
			n = len(chunk)
			perr = errScanLimit
		}
		l.left -= int64(n)
		l.scanned += int64(n)
		nl := len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		body := chunk
		if nl {
			body = chunk[:len(chunk)-1]
		}
		if room := l.max - len(out); room > 0 {
			if len(body) > room {
				body, truncated = body[:room], true
			}
			out = append(out, body...)
		} else if len(body) > 0 {
			truncated = true
		}
		switch {
		case nl:
			return trimCR(out), truncated, nil
		case perr == nil, errors.Is(perr, bufio.ErrBufferFull):
			continue
		case errors.Is(perr, errScanLimit):
			if len(out) > 0 {
				return trimCR(out), truncated, nil
			}
			return nil, false, errScanLimit
		case errors.Is(perr, io.EOF):
			if len(out) > 0 {
				return trimCR(out), truncated, nil
			}
			return nil, false, io.EOF
		default:
			return nil, false, perr
		}
	}
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// sanitize makes a line safe to carry in a JSON string: invalid UTF-8 (binary
// output, a cut multi-byte sequence) becomes U+FFFD. Redaction ran on the raw
// bytes before this, so sanitizing cannot reintroduce a value.
func sanitize(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// window keeps the last n lines within a byte budget.
type window struct {
	n, maxBytes int
	lines       []windowLine
	bytes       int
	evicted     bool // a line was dropped to honor the byte budget
}

type windowLine struct {
	text      string
	truncated bool
}

func (w *window) push(text string, truncated bool) {
	w.lines = append(w.lines, windowLine{text, truncated})
	w.bytes += len(text)
	for len(w.lines) > w.n {
		w.drop(false)
	}
	for w.bytes > w.maxBytes && len(w.lines) > 1 {
		w.drop(true)
	}
}

func (w *window) drop(forBytes bool) {
	w.bytes -= len(w.lines[0].text)
	w.lines[0] = windowLine{}
	w.lines = w.lines[1:]
	if forBytes {
		w.evicted = true
	}
}
