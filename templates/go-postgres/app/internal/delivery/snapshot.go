package delivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
)

// SnapshotFormat is the serialization tag the factory's environment snapshot
// digest covers.
const SnapshotFormat = "kodama/environment-snapshot/v1"

// Snapshot is the nonsecret environment snapshot supplied through a declared
// pipeline input. The merged runtime context carries only the environment ID and
// the snapshot digest plus secret bindings, not the settings themselves, and
// delivery receipts omit them too; this explicit copy is the bridge. It is never
// trusted on its own: Verify requires its digest to equal the pinned digest.
//
// Field order is the serialization order: it must match the factory's canonical
// document exactly (a root-module contract test compares digests against the
// factory's own implementation).
type Snapshot struct {
	Format      string            `json:"format"`
	Tenant      string            `json:"tenant"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Kind        string            `json:"kind"`
	Target      string            `json:"target"`
	Namespace   string            `json:"namespace"`
	Config      map[string]string `json:"config"`
	TTLSeconds  int64             `json:"previewTtlSeconds"`
	Kubeconfig  string            `json:"kubeconfig"`
	Secrets     []Binding         `json:"secrets"`
}

// Digest is the canonical SHA-256 identity of the snapshot content.
func (s Snapshot) Digest() (string, error) {
	secrets := append([]Binding{}, s.Secrets...)
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	doc := s
	doc.Config, doc.Secrets = cfg, secrets
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// bindingNameRE is the runtime contract's SafeName pattern.
var bindingNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var nsRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ErrSnapshot reports a snapshot that does not match the pinned context.
var ErrSnapshot = errors.New("delivery: the environment snapshot does not match the pinned context")

// ParseSnapshot strictly decodes the explicit input.
func ParseSnapshot(raw string) (Snapshot, error) {
	var s Snapshot
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("%w: malformed JSON: %v", ErrSnapshot, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Snapshot{}, fmt.Errorf("%w: trailing data", ErrSnapshot)
	}
	return s, nil
}

// Verify checks the snapshot against the immutable pinned context before
// anything is applied: ownership, environment, digest, namespace, kind and the
// exact credential bindings. kind is "preview" or "stage".
func (s Snapshot) Verify(c Context, kind string) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSnapshot, fmt.Sprintf(f, a...)) }
	if c.Environment == nil {
		return bad("the attempt has no pinned environment")
	}
	switch {
	case s.Format != SnapshotFormat:
		return bad("unknown format %q", s.Format)
	case s.Tenant != c.TenantID || s.Project != c.ProjectID:
		return bad("it belongs to another tenant or project")
	case s.Environment != c.Environment.ID:
		return bad("it describes another environment")
	case s.Kind != kind:
		return bad("it is a %s environment but this script deploys %s", s.Kind, kind)
	case !nsRE.MatchString(s.Namespace):
		return bad("the namespace is not a valid Kubernetes namespace name")
	}
	got, err := s.Digest()
	if err != nil || got != c.Environment.ConfigDigest {
		return bad("its digest %s is not the pinned configuration digest %s", got, c.Environment.ConfigDigest)
	}
	// Every snapshot binding must be present in the pinned context with the
	// identical reference, version, name and delivery form; the deployment
	// credential must be one of them. Unrelated bindings the context declares
	// are permitted and never consumed. Names must be valid and unique.
	have := map[string]Binding{}
	for _, b := range c.Secrets {
		have[b.Name] = b
	}
	seen := map[string]bool{}
	found := false
	for _, b := range s.Secrets {
		switch {
		case !bindingNameRE.MatchString(b.Name):
			return bad("the secret binding name %q is not valid", b.Name)
		case seen[b.Name]:
			return bad("the secret binding %q is listed twice", b.Name)
		}
		seen[b.Name] = true
		if got, ok := have[b.Name]; !ok || got != b {
			return bad("the secret binding %q is missing from, or differs in, the pinned context", b.Name)
		}
		if b.Name == s.Kubeconfig {
			found = true
		}
	}
	if !found {
		return bad("the deployment credential %q is not among the pinned bindings", s.Kubeconfig)
	}
	return nil
}
