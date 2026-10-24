package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// Canonical form (frozen here; golden fixtures guard it):
//
//   - the document is normalized first: only documented defaults are applied
//     (absent needs, args, env, secretFiles, outputs and inputs become empty;
//     input.required defaults to false; workspace defaults to {access: none};
//     retry defaults to one attempt with no backoff and no classes);
//   - it is serialized as UTF-8 JSON with object keys in byte order, no
//     insignificant whitespace, no HTML escaping and array order preserved;
//   - integers are written as plain decimal numbers (never exponent forms);
//   - strings are not otherwise rewritten, so commands keep their exact text.
//
// The digest is the SHA-256 of those bytes and therefore includes the document
// name (metadata.name) and excludes everything outside the document: database
// identity, ownership, timestamps, and the allocated version.

// Digest is a content digest in the form "sha256:" followed by 64 lowercase
// hexadecimal digits.
type Digest string

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Validate checks the digest syntax.
func (d Digest) Validate() error {
	if !digestRE.MatchString(string(d)) {
		return fmt.Errorf("pipeline: invalid digest")
	}
	return nil
}

// digestOf returns the digest of canonical bytes.
func digestOf(canonical []byte) Digest {
	sum := sha256.Sum256(canonical)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// DigestOf returns the digest of canonical bytes, for adapters that verify
// stored content.
func DigestOf(canonical []byte) Digest { return digestOf(canonical) }

// canonicalBytes serializes a normalized document.
func canonicalBytes(d Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d.tree()); err != nil {
		return nil, fmt.Errorf("pipeline: canonical encoding: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Validated is a document that passed the structural and semantic rules and
// capability lookups of one Validator, together with its canonical form and
// digest. It is immutable: every accessor returns a copy, so a caller cannot
// alter what was validated or registered.
type Validated struct {
	name      string
	canonical []byte
	digest    Digest
}

// NewValidatedFromCanonical rebuilds a Validated value from stored canonical
// bytes, checking that they are canonical and match the digest. It is for
// persistence adapters: it re-runs structural parsing, not capability lookups,
// because stored definitions were validated at registration and are immutable.
func NewValidatedFromCanonical(canonical []byte, digest Digest) (Validated, error) {
	if err := digest.Validate(); err != nil {
		return Validated{}, err
	}
	if digestOf(canonical) != digest {
		return Validated{}, fmt.Errorf("%w: stored content does not match its digest", ErrConflict)
	}
	doc, err := ParseJSON(canonical, storedOptions(len(canonical)))
	if err != nil {
		return Validated{}, fmt.Errorf("%w: stored content is not a valid document", ErrConflict)
	}
	again, err := canonicalBytes(normalize(doc))
	if err != nil || !bytes.Equal(again, canonical) {
		return Validated{}, fmt.Errorf("%w: stored content is not canonical", ErrConflict)
	}
	return Validated{name: doc.Metadata.Name, canonical: append([]byte(nil), canonical...), digest: digest}, nil
}

func newValidated(d Document) (Validated, error) {
	n := normalize(d)
	b, err := canonicalBytes(n)
	if err != nil {
		return Validated{}, err
	}
	return Validated{name: n.Metadata.Name, canonical: b, digest: digestOf(b)}, nil
}

// Name returns metadata.name.
func (v Validated) Name() string { return v.name }

// Digest returns the content digest.
func (v Validated) Digest() Digest { return v.digest }

// Canonical returns a copy of the canonical bytes.
func (v Validated) Canonical() []byte { return append([]byte(nil), v.canonical...) }

// IsZero reports whether v holds no validated document.
func (v Validated) IsZero() bool { return len(v.canonical) == 0 }

// Document decodes the canonical bytes into a fresh Document on every call, so
// callers never share maps or slices with the stored value.
func (v Validated) Document() (Document, error) {
	doc, err := ParseJSON(v.canonical, storedOptions(len(v.canonical)))
	if err != nil {
		return Document{}, fmt.Errorf("pipeline: stored document: %w", err)
	}
	return normalize(doc), nil
}

// storedOptions are the parser bounds for content this module itself produced:
// size is bounded by the content, depth and node counts are generous because
// the content was bounded when it was validated.
func storedOptions(n int) Options {
	return Options{MaxBytes: n + 1, MaxDepth: 64, MaxNodes: 1 << 22}
}
