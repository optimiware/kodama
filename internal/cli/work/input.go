package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/cli/root"
)

// Local input bounds. They only protect the CLI from unbounded reads; the
// server remains the authority on content limits.
const (
	maxSpecBytes     = 64 << 10  // api/work.yaml: specification Markdown, at most 65536 bytes
	maxStructuredLen = 512 << 10 // acceptance, evidence and claim documents
)

// reader reads bounded input files. "-" selects the invocation's standard
// input, which can be consumed once.
type reader struct {
	ctx       context.Context
	stdin     io.Reader
	stdinUsed bool
}

func (r *reader) read(flagName, path string, max int64) ([]byte, error) {
	if path == "" {
		return nil, root.Usagef("--%s must not be empty", flagName)
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if path == "-" {
		if r.stdinUsed {
			return nil, root.Usagef("only one input can be read from standard input")
		}
		r.stdinUsed = true
		if r.stdin == nil {
			return nil, root.Usagef("--%s -: no standard input is available", flagName)
		}
		return readLimited(r.ctx, flagName, r.stdin, max, nil)
	}
	// Check the type before opening: opening a FIFO with no writer blocks.
	pre, err := os.Stat(path)
	if err != nil {
		return nil, root.Usagef("--%s: cannot read %q: %s", flagName, path, reason(err))
	}
	if !pre.Mode().IsRegular() {
		return nil, root.Usagef("--%s: %q is not a regular file", flagName, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, root.Usagef("--%s: cannot read %q: %s", flagName, path, reason(err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, root.Usagef("--%s: cannot read %q: %s", flagName, path, reason(err))
	}
	if !st.Mode().IsRegular() || !os.SameFile(pre, st) {
		return nil, root.Usagef("--%s: %q is not a regular file", flagName, path)
	}
	return readLimited(r.ctx, flagName, f, max, func() { _ = f.Close() })
}

func reason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such file"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	return "unreadable"
}

func readLimited(ctx context.Context, flagName string, r io.Reader, max int64, wake func()) ([]byte, error) {
	b, err := readBounded(ctx, r, max, wake)
	if err != nil {
		if isContextError(err) {
			return nil, err
		}
		return nil, root.Usagef("--%s: the input could not be read", flagName)
	}
	if int64(len(b)) > max {
		return nil, root.Usagef("--%s: the input is larger than %d bytes", flagName, max)
	}
	return b, nil
}

// readSpec reads specification Markdown. It must be valid UTF-8 text.
func (r *reader) readSpec(flagName, path string) (string, error) {
	b, err := r.read(flagName, path, maxSpecBytes)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", root.Usagef("--%s: the specification must be UTF-8 text", flagName)
	}
	return string(b), nil
}

// readJSON decodes one JSON document into v, rejecting unknown members, so a
// misspelled field cannot be silently dropped from a claim.
func (r *reader) readJSON(flagName, path string, v any) error {
	b, err := r.read(flagName, path, maxStructuredLen)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return root.Usagef("--%s: the file is not the expected JSON document", flagName)
	}
	if _, err := dec.Token(); err != io.EOF {
		return root.Usagef("--%s: the file has trailing data after the JSON document", flagName)
	}
	return nil
}
