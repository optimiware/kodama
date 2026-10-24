package run

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/internal/cli/root"
)

var (
	commitPattern  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	revisionRE     = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
)

// inputSet collects run inputs. A name may be given once, either as a string
// (--input) or as an explicit JSON scalar (--input-json); the CLI never guesses
// a type from the spelling of a value.
type inputSet struct {
	values map[string]any
	order  []string
}

func newInputSet() *inputSet { return &inputSet{values: map[string]any{}} }

func (s *inputSet) add(flagName, spec string, parse func(string) (any, error)) error {
	name, raw, ok := strings.Cut(spec, "=")
	if !ok || name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\r\n") {
		return root.Usagef("--%s %q must be NAME=VALUE with a name", flagName, truncate(spec))
	}
	if _, dup := s.values[name]; dup {
		return root.Usagef("run input %q is given more than once", name)
	}
	v, err := parse(raw)
	if err != nil {
		return root.Usagef("--%s %s: %v", flagName, name, err)
	}
	s.values[name] = v
	s.order = append(s.order, name)
	return nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// stringScalar passes the text through unchanged. An artifact input is its
// artifact ID string, never a storage URL.
func stringScalar(raw string) (any, error) { return raw, nil }

// jsonScalar accepts exactly one JSON string, boolean or integer.
func jsonScalar(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errScalar
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errScalar
	}
	switch t := v.(type) {
	case string, bool:
		return t, nil
	case json.Number:
		if integerLiteral.MatchString(t.String()) {
			return t, nil
		}
	}
	return nil, errScalar
}

type scalarError struct{}

func (scalarError) Error() string { return "the value must be one JSON string, boolean or integer" }

var errScalar error = scalarError{}

// body returns the inputs object, or nil when no input was given.
func (s *inputSet) body() map[string]any {
	if len(s.values) == 0 {
		return nil
	}
	return s.values
}

// parseRevision reads the content revision exactly as written.
func parseRevision(v string) (json.Number, error) {
	if !revisionRE.MatchString(v) {
		return "", root.Usagef("--content-revision must be a positive integer")
	}
	return json.Number(v), nil
}

// compactJSON is used by tests and diagnostics to compare bodies.
func compactJSON(b []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return string(b)
	}
	return out.String()
}
