package logs

import (
	"bytes"
	"errors"
	"io"
)

// Marker replaces every redacted value.
const Marker = "[REDACTED]"

// Bounds of the redaction set.
const (
	MaxRedactValues   = 256
	MaxRedactValueLen = 8 << 10
)

// Redactor removes known values from a byte stream, however the stream is
// split into chunks. It holds back only the bytes that could still become the
// start of a value (at most the longest value minus one, plus one pending match
// region), so the buffer is bounded by the set, never by the stream.
//
// Overlapping or adjacent matches merge into one marker, so no suffix or prefix
// of a value survives next to another one. Matching is byte-exact: encodings of
// a value (base64, URL escapes, split across lines by the producer) are not
// recognized. That limit is documented, and arbitrary tenant output still needs
// access control and retention.
type Redactor struct {
	values  [][]byte
	maxLen  int
	holdCap int
	first   [256][]int // value indexes by first byte
	buf     []byte
}

// NewRedactor validates and copies the set. Empty values are rejected: they
// would match everywhere.
func NewRedactor(values [][]byte) (*Redactor, error) {
	if len(values) > MaxRedactValues {
		return nil, invalidf("too many values to redact")
	}
	r := &Redactor{}
	seen := map[string]bool{}
	for _, v := range values {
		if len(v) == 0 || len(v) > MaxRedactValueLen {
			return nil, invalidf("a value to redact must be 1..%d bytes", MaxRedactValueLen)
		}
		if seen[string(v)] {
			continue
		}
		seen[string(v)] = true
		cp := append([]byte(nil), v...)
		r.first[cp[0]] = append(r.first[cp[0]], len(r.values))
		r.values = append(r.values, cp)
		if len(cp) > r.maxLen {
			r.maxLen = len(cp)
		}
	}
	r.holdCap = 4*r.maxLen + 4096
	return r, nil
}

// matchEnd returns the end of the longest value that matches at i, or 0.
func (r *Redactor) matchEnd(b []byte, i int) int {
	end := 0
	for _, idx := range r.first[b[i]] {
		v := r.values[idx]
		if len(b)-i >= len(v) && bytes.Equal(b[i:i+len(v)], v) && i+len(v) > end {
			end = i + len(v)
		}
	}
	return end
}

// partial reports whether b[i:] is a proper prefix of some value, i.e. a value
// may still complete when more bytes arrive.
func (r *Redactor) partial(b []byte, i int) bool {
	rest := b[i:]
	for _, idx := range r.first[b[i]] {
		v := r.values[idx]
		if len(v) > len(rest) && bytes.Equal(v[:len(rest)], rest) {
			return true
		}
	}
	return false
}

// Write consumes p and returns the bytes that are safe to emit now.
func (r *Redactor) Write(p []byte) []byte {
	if len(r.values) == 0 {
		return append([]byte(nil), p...)
	}
	r.buf = append(r.buf, p...)
	return r.process(false)
}

// Flush ends the stream: held bytes can no longer complete a value.
func (r *Redactor) Flush() []byte {
	if len(r.values) == 0 {
		return nil
	}
	return r.process(true)
}

func (r *Redactor) process(final bool) []byte {
	b := r.buf
	out := make([]byte, 0, len(b))
	i := 0
loop:
	for i < len(b) {
		end := r.matchEnd(b, i)
		if end == 0 {
			if !final && r.partial(b, i) {
				break
			}
			out = append(out, b[i])
			i++
			continue
		}
		// A longer value that starts here and is still incomplete must be held:
		// emitting the marker for the shorter match now would release the rest
		// of the longer value when it arrives in the next chunk.
		if !final && r.partial(b, i) {
			if len(b)-i > r.holdCap {
				out = append(out, Marker...)
				i = len(b)
				break loop
			}
			break loop
		}
		// Merge every match that starts inside the region into one marker.
		for j := i + 1; j < end; j++ {
			if e := r.matchEnd(b, j); e > end {
				end = e
			}
			if !final && r.partial(b, j) {
				if len(b)-i > r.holdCap {
					// A pathological chain of overlapping values: redact the whole
					// pending region rather than grow without bound.
					out = append(out, Marker...)
					i = len(b)
					break loop
				}
				break loop // hold from i until the region can be resolved
			}
		}
		out = append(out, Marker...)
		i = end
	}
	r.buf = append(r.buf[:0], b[i:]...)
	return out
}

// redactingReader applies a Redactor to a stream.
type redactingReader struct {
	src     io.Reader
	r       *Redactor
	pending []byte
	scratch []byte
	err     error
}

func newRedactingReader(src io.Reader, r *Redactor) io.Reader {
	if r == nil || len(r.values) == 0 {
		return src
	}
	return &redactingReader{src: src, r: r, scratch: make([]byte, 4096)}
}

func (rr *redactingReader) Read(p []byte) (int, error) {
	for len(rr.pending) == 0 {
		if rr.err != nil {
			return 0, rr.err
		}
		n, err := rr.src.Read(rr.scratch)
		if n > 0 {
			rr.pending = rr.r.Write(rr.scratch[:n])
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			rr.pending = append(rr.pending, rr.r.Flush()...)
			rr.err = io.EOF
		default:
			// A failed stream must not release the held tail: it may be the start
			// of a value. Drop it and report the error.
			rr.r.buf = rr.r.buf[:0]
			rr.err = err
		}
	}
	n := copy(p, rr.pending)
	rr.pending = rr.pending[n:]
	return n, nil
}
