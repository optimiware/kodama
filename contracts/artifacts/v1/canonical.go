package artifactsv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type manifestWire struct {
	Format       string                   `json:"format"`
	TenantID     string                   `json:"tenantId"`
	ProjectID    string                   `json:"projectId"`
	Name         string                   `json:"name"`
	VersionLabel string                   `json:"versionLabel"`
	Artifacts    map[string]ManifestEntry `json:"artifacts"`
	Sources      []SourceRef              `json:"sources"`
	Producers    []ProducerRef            `json:"producers"`
}

// ReleaseManifest is the validated, normalized and canonicalized form of an
// immutable release manifest.
type ReleaseManifest struct {
	scope     domain.Scope
	name      string
	version   string
	artifacts map[string]ManifestEntry
	sources   []SourceRef
	producers []ProducerRef
	canonical []byte
	digest    runtimev1.Digest
}

// NewReleaseManifest validates, normalizes and canonicalizes a release manifest.
func NewReleaseManifest(in ReleaseManifestInput) (ReleaseManifest, error) {
	if err := in.Scope.Validate(); err != nil {
		return ReleaseManifest{}, err
	}
	if !releaseNameRe.MatchString(in.Name) {
		return ReleaseManifest{}, fmt.Errorf("%w: invalid release name %q", domain.ErrInvalidValue, in.Name)
	}
	if !versionLabelRe.MatchString(in.VersionLabel) {
		return ReleaseManifest{}, fmt.Errorf("%w: invalid versionLabel %q", domain.ErrInvalidValue, in.VersionLabel)
	}
	if len(in.Artifacts) == 0 || len(in.Artifacts) > MaxReleaseEntries {
		return ReleaseManifest{}, fmt.Errorf("%w: a release manifest needs 1..%d artifact entries", domain.ErrInvalidValue, MaxReleaseEntries)
	}
	entries := make(map[string]ManifestEntry, len(in.Artifacts))
	byArtifact := make(map[domain.ArtifactID]runtimev1.Digest, len(in.Artifacts))
	for name, entry := range in.Artifacts {
		if !entryNameRe.MatchString(name) {
			return ReleaseManifest{}, fmt.Errorf("%w: invalid entry name %q", domain.ErrInvalidValue, name)
		}
		if err := entry.Validate(); err != nil {
			return ReleaseManifest{}, fmt.Errorf("entry %s: %w", name, err)
		}
		if prev, exists := byArtifact[entry.ArtifactID]; exists && prev != entry.Digest {
			return ReleaseManifest{}, fmt.Errorf("%w: artifact %s listed with conflicting digests", domain.ErrInvalidValue, entry.ArtifactID)
		}
		byArtifact[entry.ArtifactID] = entry.Digest
		entries[name] = cloneEntry(entry)
	}

	sources, err := normalizeSources(in.Sources)
	if err != nil {
		return ReleaseManifest{}, err
	}
	producers, err := normalizeProducers(in.Producers, byArtifact)
	if err != nil {
		return ReleaseManifest{}, err
	}

	w := manifestWire{
		Format:       ReleaseManifestFormat,
		TenantID:     string(in.Scope.TenantID),
		ProjectID:    string(in.Scope.ProjectID),
		Name:         in.Name,
		VersionLabel: in.VersionLabel,
		Artifacts:    entries,
		Sources:      sources,
		Producers:    producers,
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return ReleaseManifest{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("%w: manifest is not canonicalizable: %v", domain.ErrInvalidValue, err)
	}
	if len(canonical) > MaxManifestBytes {
		return ReleaseManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", domain.ErrInvalidValue, MaxManifestBytes)
	}
	sum := sha256.Sum256(canonical)
	digest := runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
	return ReleaseManifest{
		scope:     in.Scope,
		name:      in.Name,
		version:   in.VersionLabel,
		artifacts: entries,
		sources:   sources,
		producers: producers,
		canonical: canonical,
		digest:    digest,
	}, nil
}

// ParseReleaseManifest restores a stored canonical manifest and verifies that its
// bytes are already in exact canonical form.
func ParseReleaseManifest(canonical []byte) (ReleaseManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var w manifestWire
	if err := dec.Decode(&w); err != nil {
		return ReleaseManifest{}, fmt.Errorf("%w: release manifest: %v", domain.ErrInvalidValue, err)
	}
	if w.Format != ReleaseManifestFormat {
		return ReleaseManifest{}, fmt.Errorf("%w: unsupported release manifest format %q", domain.ErrInvalidValue, w.Format)
	}
	m, err := NewReleaseManifest(ReleaseManifestInput{
		Scope:        domain.Scope{TenantID: domain.TenantID(w.TenantID), ProjectID: domain.ProjectID(w.ProjectID)},
		Name:         w.Name,
		VersionLabel: w.VersionLabel,
		Artifacts:    w.Artifacts,
		Sources:      w.Sources,
		Producers:    w.Producers,
	})
	if err != nil {
		return ReleaseManifest{}, err
	}
	if !bytes.Equal(m.canonical, canonical) {
		return ReleaseManifest{}, fmt.Errorf("%w: release manifest is not in canonical form", domain.ErrInvalidValue)
	}
	return m, nil
}

func (m ReleaseManifest) Scope() domain.Scope      { return m.scope }
func (m ReleaseManifest) Name() string             { return m.name }
func (m ReleaseManifest) VersionLabel() string     { return m.version }
func (m ReleaseManifest) Digest() runtimev1.Digest { return m.digest }
func (m ReleaseManifest) Canonical() []byte        { return append([]byte(nil), m.canonical...) }
func (m ReleaseManifest) Sources() []SourceRef     { return cloneSources(m.sources) }
func (m ReleaseManifest) Producers() []ProducerRef { return cloneProducers(m.producers) }
func (m ReleaseManifest) Entries() map[string]ManifestEntry {
	out := make(map[string]ManifestEntry, len(m.artifacts))
	for k, v := range m.artifacts {
		out[k] = cloneEntry(v)
	}
	return out
}

func cloneEntry(e ManifestEntry) ManifestEntry {
	if e.MediaType != nil {
		mt := *e.MediaType
		e.MediaType = &mt
	}
	return e
}

func cloneSources(in []SourceRef) []SourceRef {
	out := make([]SourceRef, len(in))
	for i, s := range in {
		if s.RepositoryID != nil {
			id := *s.RepositoryID
			s.RepositoryID = &id
		}
		out[i] = s
	}
	return out
}

func cloneProducers(in []ProducerRef) []ProducerRef {
	out := make([]ProducerRef, len(in))
	for i, p := range in {
		if p.ToolImage != nil {
			img := *p.ToolImage
			p.ToolImage = &img
		}
		if p.WorkspaceState != nil {
			ws := *p.WorkspaceState
			p.WorkspaceState = &ws
		}
		p.ConsumedArtifacts = append([]ArtifactDigestRef(nil), p.ConsumedArtifacts...)
		out[i] = p
	}
	return out
}

func normalizeSources(in []SourceRef) ([]SourceRef, error) {
	out := make([]SourceRef, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if err := s.Validate(); err != nil {
			return nil, err
		}
		repoID := ""
		if s.RepositoryID != nil {
			repoID = *s.RepositoryID
		}
		key := repoID + "\x00" + s.RepositoryURL + "\x00" + s.Commit
		if seen[key] {
			continue
		}
		seen[key] = true
		if s.RepositoryID != nil {
			cp := *s.RepositoryID
			s.RepositoryID = &cp
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepositoryURL != out[j].RepositoryURL {
			return out[i].RepositoryURL < out[j].RepositoryURL
		}
		if out[i].Commit != out[j].Commit {
			return out[i].Commit < out[j].Commit
		}
		var a, b string
		if out[i].RepositoryID != nil {
			a = *out[i].RepositoryID
		}
		if out[j].RepositoryID != nil {
			b = *out[j].RepositoryID
		}
		return a < b
	})
	return out, nil
}

func normalizeProducers(in []ProducerRef, expected map[domain.ArtifactID]runtimev1.Digest) ([]ProducerRef, error) {
	byID := make(map[domain.ArtifactID]ProducerRef, len(in))
	for _, p := range in {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, ok := expected[p.ArtifactID]; !ok {
			return nil, fmt.Errorf("%w: producer references undeclared artifact %s", domain.ErrInvalidValue, p.ArtifactID)
		}
		consumed, err := normalizeConsumed(p.ConsumedArtifacts)
		if err != nil {
			return nil, err
		}
		p.ConsumedArtifacts = consumed
		if prev, exists := byID[p.ArtifactID]; exists {
			if !sameProducer(prev, p) {
				return nil, fmt.Errorf("%w: conflicting producer lineage for %s", domain.ErrInvalidValue, p.ArtifactID)
			}
			continue
		}
		byID[p.ArtifactID] = cloneProducers([]ProducerRef{p})[0]
	}
	if len(byID) != len(expected) {
		return nil, fmt.Errorf("%w: every release artifact entry requires producer lineage", domain.ErrInvalidValue)
	}
	out := make([]ProducerRef, 0, len(byID))
	for _, p := range byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArtifactID < out[j].ArtifactID })
	return out, nil
}

func normalizeConsumed(in []ArtifactDigestRef) ([]ArtifactDigestRef, error) {
	out := make([]ArtifactDigestRef, 0, len(in))
	byID := make(map[domain.ArtifactID]runtimev1.Digest, len(in))
	for _, c := range in {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if prev, ok := byID[c.ID]; ok {
			if prev != c.Digest {
				return nil, fmt.Errorf("%w: consumed artifact %s has conflicting digests", domain.ErrInvalidValue, c.ID)
			}
			continue
		}
		byID[c.ID] = c.Digest
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func sameProducer(a, b ProducerRef) bool {
	if a.ArtifactID != b.ArtifactID || a.AttemptID != b.AttemptID || a.RunID != b.RunID || a.StepID != b.StepID {
		return false
	}
	if (a.ToolImage == nil) != (b.ToolImage == nil) || (a.ToolImage != nil && *a.ToolImage != *b.ToolImage) {
		return false
	}
	if (a.WorkspaceState == nil) != (b.WorkspaceState == nil) || (a.WorkspaceState != nil && *a.WorkspaceState != *b.WorkspaceState) {
		return false
	}
	if len(a.ConsumedArtifacts) != len(b.ConsumedArtifacts) {
		return false
	}
	for i := range a.ConsumedArtifacts {
		if a.ConsumedArtifacts[i] != b.ConsumedArtifacts[i] {
			return false
		}
	}
	return true
}
