package configuration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/optimiware/kodama/internal/cli/root"
)

// MaxSecretBytes is the merged API's bound on a secret value (api/configuration.yaml).
const MaxSecretBytes = 131072

// secretInput reads secret bytes. They come only from a protected regular file
// or from standard input that was explicitly redirected: never from an
// argument, an environment variable, a prompt or a temporary file. Every error
// here is content-free; it names the source's problem, never its bytes.
type secretInput struct {
	ctx   context.Context
	stdin io.Reader
}

// read returns the secret bytes. The caller owns the slice and should clear it
// after encoding.
func (s secretInput) read(flagName, path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, root.Usagef("--%s must not be empty", flagName)
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = s.readStdin(flagName)
	} else {
		data, err = readProtectedFile(s.ctx, flagName, path)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case len(data) == 0:
		return nil, root.Usagef("--%s: the secret value is empty; a secret holds 1 to %d bytes", flagName, MaxSecretBytes)
	case len(data) > MaxSecretBytes:
		clear(data)
		return nil, root.Usagef("--%s: the secret value is larger than %d bytes", flagName, MaxSecretBytes)
	}
	return data, nil
}

func (s secretInput) readStdin(flagName string) ([]byte, error) {
	if s.stdin == nil {
		return nil, root.Usagef("--%s -: no standard input is available", flagName)
	}
	if f, ok := s.stdin.(*os.File); ok {
		st, err := f.Stat()
		if err != nil || st.Mode()&os.ModeCharDevice != 0 {
			// A terminal would echo what is typed, and a device is not a file or
			// a pipe the caller redirected on purpose.
			return nil, root.Usagef("--%s -: standard input is a terminal or device; redirect a file or pipe instead (the CLI never prompts for secrets)", flagName)
		}
	}
	return readSecretBytes(s.ctx, flagName, s.stdin, nil)
}

func readProtectedFile(ctx context.Context, flagName, path string) ([]byte, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, root.Usagef("--%s: cannot read %q: %s", flagName, path, reason(err))
	}
	if !lst.Mode().IsRegular() {
		// Includes symlinks: a link could be retargeted between check and use.
		return nil, root.Usagef("--%s: %q is not a regular file (symbolic links and devices are refused)", flagName, path)
	}
	if runtime.GOOS != "windows" && lst.Mode().Perm()&0o077 != 0 {
		return nil, root.Usagef("--%s: %q is accessible by group or others; restrict it with chmod 600", flagName, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, root.Usagef("--%s: cannot read %q: %s", flagName, path, reason(err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(lst, st) || !st.Mode().IsRegular() {
		return nil, root.Usagef("--%s: %q changed while it was being opened", flagName, path)
	}
	// Recheck on the opened file: a chmod between Lstat and Open must not
	// leave a group- or world-readable secret accepted.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, root.Usagef("--%s: %q is accessible by group or others; restrict it with chmod 600", flagName, path)
	}
	return readSecretBytes(ctx, flagName, f, func() { _ = f.Close() })
}

// readSecretBytes reads at most MaxSecretBytes+1 bytes. A cancellation or read
// failure returns no bytes and clears whatever was read; an interrupted
// command never keeps a partial secret.
func readSecretBytes(ctx context.Context, flagName string, r io.Reader, wake func()) ([]byte, error) {
	b, err := readBounded(ctx, r, MaxSecretBytes, wake)
	if err != nil {
		if isContextError(err) {
			return nil, err
		}
		return nil, root.Usagef("--%s: the secret input could not be read", flagName)
	}
	return b, nil
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

// encodeSecret returns the standard padded base64 wire value and clears b.
func encodeSecret(b []byte) string {
	v := base64.StdEncoding.EncodeToString(b)
	clear(b)
	return v
}

var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// parseScalar reads a JSON string, boolean or integer exactly as written, so
// the CLI never guesses a type from a bare string. Integers keep their literal
// spelling; the server owns their range.
func parseScalar(flagName, raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, root.Usagef("--%s must be a JSON string, boolean or integer", flagName)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, root.Usagef("--%s must be exactly one JSON value", flagName)
	}
	switch t := v.(type) {
	case string, bool:
		return t, nil
	case json.Number:
		if integerLiteral.MatchString(t.String()) {
			return t, nil
		}
	}
	return nil, root.Usagef("--%s must be a JSON string, boolean or integer", flagName)
}
