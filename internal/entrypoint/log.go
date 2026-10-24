package entrypoint

// Retained-log collection: the raw bounded capture of the command's output is
// sanitized (known secret values and issued credentials are removed), frozen
// once, uploaded as `command.log`, and described by a completeness sidecar
// (`command.log.meta.json`, internal/logs/contract) bound to the acknowledged
// artifact's ID, digest and size. A log without a sidecar has unknown
// completeness; nothing here ever claims more than was observed.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

const (
	// sidecarReserve is the aggregate diagnostic budget held back for the
	// completeness sidecar when the budget allows it. The worst-case encoded
	// document (a 128-character artifact ID, two reasons) is below it.
	sidecarReserve = 640
	// notesReserveMax bounds the budget held back for entrypoint.log.
	notesReserveMax = 4096
	// sanitizeChunk is the fixed chunk size of retained-byte sanitization.
	sanitizeChunk = 32 << 10
)

// errRedactionSet means the values to remove cannot be handled by the
// redactor; retained bytes are then withheld rather than uploaded unscrubbed.
var errRedactionSet = errors.New("the redaction set is not usable")

// commandLogCapacity is the share of the aggregate diagnostic budget the
// command log may use: the sidecar and a small notes allowance are reserved
// when the budget can afford them. A smaller budget goes entirely to the log
// and the sidecar is attempted only if bytes remain.
func commandLogCapacity(budget int64) int64 {
	if budget < 2*sidecarReserve {
		return budget
	}
	return budget - sidecarReserve - min(budget/16, notesReserveMax)
}

// dataCapacity is the share of a bound left for data once the truncation
// marker is reserved (small bounds keep it all).
func dataCapacity(limit int64) int64 {
	if limit > int64(4*len(truncationMarker)) {
		return limit - int64(len(truncationMarker))
	}
	return limit
}

// redactionSet drops empty values (they carry no information and would match
// everywhere) and builds the redactor. Any value the redactor refuses fails
// closed.
func redactionSet(needles [][]byte) (*logs.Redactor, error) {
	vals := make([][]byte, 0, len(needles))
	for _, n := range needles {
		if len(n) > 0 {
			vals = append(vals, n)
		}
	}
	r, err := logs.NewRedactor(vals)
	if err != nil {
		return nil, errRedactionSet
	}
	return r, nil
}

// frozenLog is the sanitized, immutable command log and its declaration.
type frozenLog struct {
	path         string
	size         int64
	completeness contract.Completeness
	reasons      []contract.Reason
}

func (f *frozenLog) cleanup() {
	if f != nil && f.path != "" {
		_ = os.Remove(f.path)
	}
}

// captureEvidence is everything known about how the raw capture ended.
type captureEvidence struct {
	state      captureState
	drainErr   error
	incomplete bool // pipe still open after the drain grace period
	stopped    bool // the command was stopped (timeout, interrupt)
}

// whole reports whether the raw capture observed the entire output of a
// command that ran to its own end.
func (e captureEvidence) whole() bool {
	s := e.state
	return !s.truncated && s.writeErr == nil && s.closeErr == nil && e.drainErr == nil && !e.incomplete && !e.stopped
}

// reasons are the conservative explanations of a non-whole capture, in a
// fixed order.
func (e captureEvidence) reasons(sanitizedTruncated bool) []contract.Reason {
	var out []contract.Reason
	s := e.state
	if s.truncated || s.writeErr != nil || sanitizedTruncated {
		out = append(out, contract.ReasonTruncated)
	}
	if e.stopped || e.incomplete || e.drainErr != nil || s.closeErr != nil {
		out = append(out, contract.ReasonInterrupted)
	}
	return out
}

// sanitizeCapture reads the frozen raw capture in fixed chunks, removes the
// known values, and writes the result (at most capacity bytes, with a marker
// when output was dropped) to a new private file. The raw file is not touched
// beyond reading. When the raw capture is not whole, the redactor's held tail
// is discarded: it may be the start of a value whose rest was never seen.
func sanitizeCapture(rawPath, dir string, needles [][]byte, capacity int64, ev captureEvidence) (*frozenLog, error) {
	red, err := redactionSet(needles)
	if err != nil {
		return nil, err
	}
	in, err := os.Open(rawPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, "kodama-command-clean-*.log")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	dataCap := dataCapacity(capacity)
	var size int64
	dropped := false
	emit := func(p []byte) error {
		if dropped || len(p) == 0 {
			return nil
		}
		room := dataCap - size
		if int64(len(p)) > room {
			p = p[:max(room, 0)]
			dropped = true
		}
		n, err := out.Write(p)
		size += int64(n)
		return err
	}
	buf := make([]byte, sanitizeChunk)
	for !dropped {
		n, rerr := in.Read(buf)
		if n > 0 {
			if err := emit(red.Write(buf[:n])); err != nil {
				return nil, err
			}
		}
		if rerr == io.EOF {
			if ev.whole() {
				if err := emit(red.Flush()); err != nil {
					return nil, err
				}
			}
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if dropped || ev.state.truncated {
		if size+int64(len(truncationMarker)) <= capacity {
			n, err := out.WriteString(truncationMarker)
			size += int64(n)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	ok = true
	fl := &frozenLog{path: out.Name(), size: size}
	if reasons := ev.reasons(dropped); len(reasons) == 0 && ev.whole() {
		fl.completeness = contract.Complete
	} else {
		fl.completeness = contract.Partial
		fl.reasons = reasons
		if len(reasons) == 0 {
			fl.completeness, fl.reasons = contract.Unknown, []contract.Reason{contract.ReasonUnverified}
		}
	}
	return fl, nil
}

// sanitizeNotes removes known values from the entrypoint's own notes and
// bounds the result to room bytes. It fails closed.
func sanitizeNotes(notes string, needles [][]byte, room int64) ([]byte, error) {
	if room <= 0 {
		return nil, nil
	}
	red, err := redactionSet(needles)
	if err != nil {
		return nil, err
	}
	b := append(red.Write([]byte(notes)), red.Flush()...)
	if int64(len(b)) <= room {
		return b, nil
	}
	cut := room
	if room > int64(4*len(truncationMarker)) {
		cut = room - int64(len(truncationMarker))
	}
	b = b[:cut]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	if room > int64(4*len(truncationMarker)) {
		b = append(b, truncationMarker...)
	}
	return b, nil
}

// diagnosticLeft is the aggregate diagnostic budget not yet spent or
// committed to an upload attempt whose outcome is unknown.
func (a *attempt) diagnosticLeft() int64 {
	return a.c.Limits.MaxDiagnosticBytes - a.diagUsed
}

// commandLogMetadata declares the completeness of the acknowledged log.
func commandLogMetadata(id domain.ArtifactID, digest runtimev1.Digest, fl *frozenLog) ([]byte, error) {
	m := contract.Metadata{
		Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
		Log:          contract.Ref{ArtifactID: id, Digest: string(digest), SizeBytes: fl.size},
		Completeness: fl.completeness, Reasons: fl.reasons,
	}
	b, err := contract.Encode(m)
	if err != nil {
		return nil, err
	}
	dec, err := contract.Decode(b)
	if err != nil {
		return nil, err
	}
	if err := dec.Bind(id, string(digest), fl.size); err != nil {
		return nil, fmt.Errorf("sidecar binding: %w", err)
	}
	return b, nil
}
