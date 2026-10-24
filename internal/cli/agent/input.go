package agent

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

// maxProfileBytes only protects the CLI from an unbounded read; the server
// remains the authority on profile content and size limits.
const maxProfileBytes = 512 << 10

// reader reads one bounded profile input. "-" selects the invocation's
// standard input.
type reader struct {
	ctx   context.Context
	stdin io.Reader
}

func (r *reader) read(flagName, path string, max int64) ([]byte, error) {
	if path == "" {
		return nil, root.Usagef("--%s must not be empty", flagName)
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if path == "-" {
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

// readProfile returns the input as exactly one JSON object, byte-for-byte as
// the caller wrote it. It is deliberately not decoded into a struct: unknown
// and duplicate members survive to the server, which owns the strict schema
// and rejects them, rather than being silently erased here.
func (r *reader) readProfile(flagName, path string) (json.RawMessage, error) {
	b, err := r.read(flagName, path, maxProfileBytes)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, root.Usagef("--%s: the profile must be UTF-8 JSON", flagName)
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil, root.Usagef("--%s: the input is empty", flagName)
	}
	if !json.Valid(trimmed) {
		return nil, root.Usagef("--%s: the input is not exactly one well-formed JSON document", flagName)
	}
	if trimmed[0] != '{' {
		return nil, root.Usagef("--%s: the profile must be a JSON object", flagName)
	}
	return json.RawMessage(trimmed), nil
}
