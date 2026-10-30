// Package statev1 defines the provider-neutral state envelope shared by the
// server, runtime clients and container plugins. Values are opaque JSON; the
// server never interprets plugin-specific fields.
package statev1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// MaxValueBytes bounds one encoded value. Large documents belong in
	// artifacts, not state entries.
	MaxValueBytes = 64 << 10
	// MaxKeyBytes bounds a normalized relative key.
	MaxKeyBytes = 512
	// MaxArtifactReferences bounds explicit retention roots on one entry.
	MaxArtifactReferences = 32
	// DefaultListLimit and MaxListLimit bound list pages.
	DefaultListLimit = 50
	MaxListLimit     = 200
)

var (
	keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	digest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ArtifactReference is an explicit retention root held by a state entry.
type ArtifactReference struct {
	ArtifactID string `json:"artifactId"`
	Digest     string `json:"digest"`
}

// Entry is one scoped state document.
type Entry struct {
	Namespace          string              `json:"namespace"`
	Key                string              `json:"key"`
	Value              json.RawMessage     `json:"value"`
	ArtifactReferences []ArtifactReference `json:"artifactReferences,omitempty"`
	Generation         int64               `json:"generation"`
	Revision           int64               `json:"revision"`
	UpdatedAt          time.Time           `json:"updatedAt"`
	UpdatedBy          string              `json:"updatedBy,omitempty"`
}

// ETag is the conditional-write token of this entry. It includes the
// generation so a stale token cannot match a deleted and recreated key.
func (e Entry) ETag() string {
	return fmt.Sprintf("kodama-state-v1:%d:%d", e.Generation, e.Revision)
}

// Validate checks the entry's invariants.
func (e Entry) Validate() error {
	if err := ValidateKey(e.Key); err != nil {
		return err
	}
	if e.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if e.Generation < 1 || e.Revision < 1 {
		return fmt.Errorf("generation and revision must be positive")
	}
	if err := ValidateValue(e.Value); err != nil {
		return err
	}
	return ValidateArtifactReferences(e.ArtifactReferences)
}

// ValidateValue checks the bounded opaque JSON value.
func ValidateValue(value json.RawMessage) error {
	if len(value) == 0 || len(value) > MaxValueBytes {
		return fmt.Errorf("value must be 1..%d bytes", MaxValueBytes)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("value is not JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("value has trailing data")
	}
	return nil
}

// ValidateKey checks a normalized relative key.
func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return fmt.Errorf("key must be 1..%d bytes", MaxKeyBytes)
	}
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("key has invalid characters")
	}
	if strings.Contains(key, "//") || strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("key is not a normalized relative path")
	}
	return nil
}

// InPrefix reports whether key is under a binding prefix.
func InPrefix(prefix, key string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(key, prefix)
}

// ValidateArtifactReferences checks references and their stable order.
func ValidateArtifactReferences(refs []ArtifactReference) error {
	if len(refs) > MaxArtifactReferences {
		return fmt.Errorf("at most %d artifact references are allowed", MaxArtifactReferences)
	}
	seen := map[string]bool{}
	for i, ref := range refs {
		if ref.ArtifactID == "" || len(ref.ArtifactID) > 128 {
			return fmt.Errorf("artifact reference %d has an invalid id", i)
		}
		if !digest.MatchString(ref.Digest) {
			return fmt.Errorf("artifact reference %d has an invalid digest", i)
		}
		if seen[ref.ArtifactID] {
			return fmt.Errorf("artifact reference %s is duplicated", ref.ArtifactID)
		}
		seen[ref.ArtifactID] = true
		if i > 0 && ref.ArtifactID < refs[i-1].ArtifactID {
			return fmt.Errorf("artifact references must be sorted by artifact id")
		}
	}
	return nil
}

// WriteRequest is the body of a conditional create or update.
type WriteRequest struct {
	Value              json.RawMessage     `json:"value"`
	ArtifactReferences []ArtifactReference `json:"artifactReferences,omitempty"`
}

// Validate checks the write request.
func (w WriteRequest) Validate() error {
	if err := ValidateValue(w.Value); err != nil {
		return err
	}
	if err := ValidateArtifactReferences(w.ArtifactReferences); err != nil {
		return err
	}
	if !sort.SliceIsSorted(w.ArtifactReferences, func(i, j int) bool {
		return w.ArtifactReferences[i].ArtifactID < w.ArtifactReferences[j].ArtifactID
	}) {
		return fmt.Errorf("artifact references must be sorted by artifact id")
	}
	return nil
}

// DigestValue returns a stable digest over the value and references.
func DigestValue(value json.RawMessage, refs []ArtifactReference) string {
	h := sha256.New()
	h.Write(value)
	for _, ref := range refs {
		h.Write([]byte{0})
		h.Write([]byte(ref.ArtifactID))
		h.Write([]byte{0})
		h.Write([]byte(ref.Digest))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
