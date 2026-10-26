// Package diag renders the safe part of an error chain for operator logs.
//
// A failure that crosses several modules is classified (unavailable, denied, ...) and its message is
// deliberately not rendered, because messages may wrap peer errors that carry credentials, signed URLs or
// provider bodies. That leaves operators with a bare class. An error opts in to being described by
// implementing Safe: its author vouches that the text is fixed, written by the developer, and never carries
// a credential, a secret value, a signed URL, a response body or a foreign identifier. Describe renders only
// those fragments, from the outermost to the innermost, and nothing else of the chain.
package diag

import (
	"errors"
	"strings"
)

// Safe is implemented by errors whose SafeDiagnostic text may be logged verbatim.
type Safe interface{ SafeDiagnostic() string }

const (
	maxFragments = 6
	maxBytes     = 400
	separator    = " <- "
)

// Describe returns the safe fragments of err's chain, outermost first, or "" when no error in the chain is Safe.
func Describe(err error) string {
	var out []string
	seen := map[string]bool{}
	var walk func(error)
	walk = func(e error) {
		if e == nil || len(out) >= maxFragments {
			return
		}
		if s, ok := e.(Safe); ok {
			if text := clean(s.SafeDiagnostic()); text != "" && !seen[text] {
				seen[text] = true
				out = append(out, text)
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	joined := strings.Join(out, separator)
	if len(joined) > maxBytes {
		joined = joined[:maxBytes]
	}
	return joined
}

// clean keeps printable ASCII only, so a fragment can never smuggle control characters into a log line.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return strings.TrimSpace(b.String())
}

// Is reports whether err's chain contains target; it exists so callers need not import errors for one test.
func Is(err, target error) bool { return errors.Is(err, target) }
