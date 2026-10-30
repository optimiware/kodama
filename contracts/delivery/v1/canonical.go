package deliveryv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type envSnapshotWireV2 struct {
	Format               string                 `json:"format"`
	Tenant               string                 `json:"tenant"`
	Project              string                 `json:"project"`
	Environment          string                 `json:"environment"`
	Name                 string                 `json:"name"`
	Target               string                 `json:"target"`
	Config               map[string]string      `json:"config"`
	PreviewTTLSeconds    *int64                 `json:"previewTtlSeconds"`
	CleanupDefinition    *string                `json:"cleanupDefinition"`
	Secrets              []LogicalSecretBinding `json:"secrets"`
	LegacySnapshotDigest *string                `json:"legacySnapshotDigest"`
}

// EnvironmentSnapshotV2 is the validated, canonical tool-neutral environment snapshot.
type EnvironmentSnapshotV2 struct {
	in        EnvironmentSnapshotV2Input
	canonical []byte
	digest    runtimev1.Digest
}

// NewEnvironmentSnapshotV2 validates and computes the canonical v2 configuration snapshot.
func NewEnvironmentSnapshotV2(in EnvironmentSnapshotV2Input) (EnvironmentSnapshotV2, error) {
	if err := in.Scope.Validate(); err != nil {
		return EnvironmentSnapshotV2{}, err
	}
	if err := in.Environment.Validate(); err != nil {
		return EnvironmentSnapshotV2{}, err
	}
	if !envNameRe.MatchString(in.Name) {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: invalid environment name %q", domain.ErrInvalidValue, in.Name)
	}
	if !targetRe.MatchString(in.Target) {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: invalid target %q", domain.ErrInvalidValue, in.Target)
	}
	if len(in.Config) > MaxConfigEntries {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: too many config entries", domain.ErrInvalidValue)
	}
	cfg := make(map[string]string, len(in.Config))
	for k, v := range in.Config {
		if !configKeyRe.MatchString(k) || len(v) > MaxConfigValue || strings.ContainsRune(v, 0) {
			return EnvironmentSnapshotV2{}, fmt.Errorf("%w: invalid config entry %q", domain.ErrInvalidValue, k)
		}
		cfg[k] = v
	}
	if in.PreviewTTLSeconds != nil && (*in.PreviewTTLSeconds < 60 || *in.PreviewTTLSeconds > 30*24*3600) {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: previewTtlSeconds out of bounds", domain.ErrInvalidValue)
	}
	if in.CleanupDefinition != nil && !envNameRe.MatchString(*in.CleanupDefinition) {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: invalid cleanupDefinition %q", domain.ErrInvalidValue, *in.CleanupDefinition)
	}
	if len(in.Secrets) > MaxSecretBindings {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: too many secret bindings", domain.ErrInvalidValue)
	}
	names := map[string]bool{}
	refs := map[string]bool{}
	secrets := make([]LogicalSecretBinding, 0, len(in.Secrets))
	for _, s := range in.Secrets {
		if err := s.Validate(); err != nil {
			return EnvironmentSnapshotV2{}, err
		}
		if names[s.Name] || refs[s.Ref] {
			return EnvironmentSnapshotV2{}, fmt.Errorf("%w: duplicate secret binding name or ref", domain.ErrInvalidValue)
		}
		names[s.Name], refs[s.Ref] = true, true
		secrets = append(secrets, s)
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })

	var legacy *string
	if in.LegacySnapshotDigest != nil {
		if err := in.LegacySnapshotDigest.Validate(); err != nil {
			return EnvironmentSnapshotV2{}, err
		}
		s := string(*in.LegacySnapshotDigest)
		legacy = &s
	}

	w := envSnapshotWireV2{
		Format:               EnvironmentSnapshotFormatV2,
		Tenant:               string(in.Scope.TenantID),
		Project:              string(in.Scope.ProjectID),
		Environment:          string(in.Environment),
		Name:                 in.Name,
		Target:               in.Target,
		Config:               cfg,
		PreviewTTLSeconds:    in.PreviewTTLSeconds,
		CleanupDefinition:    in.CleanupDefinition,
		Secrets:              secrets,
		LegacySnapshotDigest: legacy,
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return EnvironmentSnapshotV2{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return EnvironmentSnapshotV2{}, fmt.Errorf("%w: snapshot not canonicalizable: %v", domain.ErrInvalidValue, err)
	}
	sum := sha256.Sum256(canonical)
	digest := runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
	in.Config = cfg
	in.Secrets = secrets
	return EnvironmentSnapshotV2{in: in, canonical: canonical, digest: digest}, nil
}

func (s EnvironmentSnapshotV2) Pin() runtimev1.EnvironmentPin {
	return runtimev1.EnvironmentPin{ID: s.in.Environment, ConfigDigest: s.digest}
}

func (s EnvironmentSnapshotV2) Digest() runtimev1.Digest { return s.digest }
func (s EnvironmentSnapshotV2) Canonical() []byte        { return append([]byte(nil), s.canonical...) }
func (s EnvironmentSnapshotV2) Secrets() []LogicalSecretBinding {
	return append([]LogicalSecretBinding(nil), s.in.Secrets...)
}
func (s EnvironmentSnapshotV2) LegacySnapshotDigest() *runtimev1.Digest {
	return s.in.LegacySnapshotDigest
}

// MergeStepAndEnvironmentSecrets computes the authorized union of environment
// logical bindings (resolved to current run versions) and explicit step secret
// bindings, collapsing identical duplicates and rejecting ambiguous collisions.
func MergeStepAndEnvironmentSecrets(envLogical []LogicalSecretBinding, envVersions map[string]int64, stepBindings []runtimev1.SecretBinding) ([]runtimev1.SecretBinding, error) {
	resolvedEnv := make([]runtimev1.SecretBinding, 0, len(envLogical))
	for _, l := range envLogical {
		if err := l.Validate(); err != nil {
			return nil, err
		}
		ver, ok := envVersions[l.Ref]
		if !ok || ver < 1 || ver > domain.MaxSafeInteger {
			return nil, fmt.Errorf("%w: missing pinned version for environment secret %s", domain.ErrInvalidValue, l.Ref)
		}
		resolvedEnv = append(resolvedEnv, runtimev1.SecretBinding{
			Name:     l.Name,
			Ref:      l.Ref,
			Version:  ver,
			Delivery: l.Delivery,
		})
	}

	out := make([]runtimev1.SecretBinding, 0, len(resolvedEnv)+len(stepBindings))
	byName := map[string]runtimev1.SecretBinding{}
	byRef := map[string]runtimev1.SecretBinding{}

	add := func(b runtimev1.SecretBinding) error {
		if !safeNameRe.MatchString(b.Name) || !secretRefRe.MatchString(b.Ref) || b.Version < 1 || b.Version > domain.MaxSafeInteger ||
			(b.Delivery != runtimev1.DeliveryFile && b.Delivery != runtimev1.DeliveryEnv) {
			return fmt.Errorf("%w: invalid secret binding %q", domain.ErrInvalidValue, b.Name)
		}
		if prev, exists := byName[b.Name]; exists {
			if prev != b {
				return fmt.Errorf("%w: conflicting secret binding name %q", domain.ErrInvalidValue, b.Name)
			}
			return nil
		}
		if prev, exists := byRef[b.Ref]; exists {
			if prev != b {
				return fmt.Errorf("%w: conflicting binding for secret ref %q", domain.ErrInvalidValue, b.Ref)
			}
			return nil
		}
		byName[b.Name] = b
		byRef[b.Ref] = b
		out = append(out, b)
		return nil
	}

	for _, b := range stepBindings {
		if err := add(b); err != nil {
			return nil, err
		}
	}
	for _, b := range resolvedEnv {
		if err := add(b); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type gateSubjectWireV2 struct {
	Format                 string                          `json:"format"`
	TenantID               string                          `json:"tenantId"`
	ProjectID              string                          `json:"projectId"`
	WorkItemID             *string                         `json:"workItemId"`
	ContentRevision        *int64                          `json:"contentRevision"`
	Sources                []artifactsv1.SourceRef         `json:"sources"`
	Release                *ReleasePin                     `json:"release"`
	Artifacts              []artifactsv1.ArtifactDigestRef `json:"artifacts"`
	DefinitionID           string                          `json:"definitionId"`
	Evidence               []artifactsv1.ArtifactDigestRef `json:"evidence"`
	DeploymentID           *string                         `json:"deploymentId"`
	DeploymentConfigDigest *string                         `json:"deploymentConfigDigest"`
}

// GateSubjectV2 is the validated, canonicalized v2 gate subject.
type GateSubjectV2 struct {
	in        GateSubjectV2Input
	canonical []byte
	digest    runtimev1.Digest
}

// NewGateSubjectV2 validates and canonicalizes a v2 gate subject.
func NewGateSubjectV2(in GateSubjectV2Input) (GateSubjectV2, error) {
	if err := in.Scope.Validate(); err != nil {
		return GateSubjectV2{}, err
	}
	if err := in.DefinitionID.Validate(); err != nil {
		return GateSubjectV2{}, err
	}
	if (in.WorkItemID == nil) != (in.ContentRevision == nil) {
		return GateSubjectV2{}, fmt.Errorf("%w: workItemId and contentRevision must be set together", domain.ErrInvalidValue)
	}
	var wi *string
	var rev *int64
	if in.WorkItemID != nil {
		if err := in.WorkItemID.Validate(); err != nil {
			return GateSubjectV2{}, err
		}
		if err := in.ContentRevision.Validate(); err != nil {
			return GateSubjectV2{}, err
		}
		wStr := string(*in.WorkItemID)
		rVal := int64(*in.ContentRevision)
		wi, rev = &wStr, &rVal
	}
	if (in.DeploymentID == nil) != (in.DeploymentConfigDigest == nil) {
		return GateSubjectV2{}, fmt.Errorf("%w: deploymentId and deploymentConfigDigest must be set together", domain.ErrInvalidValue)
	}
	var depID, depCfg *string
	if in.DeploymentID != nil {
		if err := domain.ArtifactID(*in.DeploymentID).Validate(); err != nil {
			return GateSubjectV2{}, fmt.Errorf("%w: invalid deploymentId", domain.ErrInvalidValue)
		}
		if err := in.DeploymentConfigDigest.Validate(); err != nil {
			return GateSubjectV2{}, err
		}
		dStr := *in.DeploymentID
		cStr := string(*in.DeploymentConfigDigest)
		depID, depCfg = &dStr, &cStr
	}

	sources, err := normalizeSubjectSources(in.Sources)
	if err != nil {
		return GateSubjectV2{}, err
	}
	var rel *ReleasePin
	if in.Release != nil {
		if err := in.Release.Validate(); err != nil {
			return GateSubjectV2{}, err
		}
		cp := *in.Release
		rel = &cp
	}
	artifacts, err := normalizeSubjectRefs(in.Artifacts)
	if err != nil {
		return GateSubjectV2{}, err
	}
	evidence, err := normalizeSubjectRefs(in.Evidence)
	if err != nil {
		return GateSubjectV2{}, err
	}
	if len(sources) == 0 && rel == nil && len(artifacts) == 0 {
		return GateSubjectV2{}, fmt.Errorf("%w: gate subject requires at least one source, release, or artifact subject", domain.ErrInvalidValue)
	}

	w := gateSubjectWireV2{
		Format:                 GateSubjectFormatV2,
		TenantID:               string(in.Scope.TenantID),
		ProjectID:              string(in.Scope.ProjectID),
		WorkItemID:             wi,
		ContentRevision:        rev,
		Sources:                sources,
		Release:                rel,
		Artifacts:              artifacts,
		DefinitionID:           string(in.DefinitionID),
		Evidence:               evidence,
		DeploymentID:           depID,
		DeploymentConfigDigest: depCfg,
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return GateSubjectV2{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return GateSubjectV2{}, fmt.Errorf("%w: gate subject v2 not canonicalizable: %v", domain.ErrInvalidValue, err)
	}
	sum := sha256.Sum256(canonical)
	digest := runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
	stored := GateSubjectV2Input{
		Scope:        in.Scope,
		Sources:      cloneSubjectSources(sources),
		Release:      cloneReleasePin(rel),
		Artifacts:    append([]artifactsv1.ArtifactDigestRef(nil), artifacts...),
		DefinitionID: in.DefinitionID,
		Evidence:     append([]artifactsv1.ArtifactDigestRef(nil), evidence...),
	}
	if in.WorkItemID != nil {
		id := *in.WorkItemID
		stored.WorkItemID = &id
	}
	if in.ContentRevision != nil {
		revVal := *in.ContentRevision
		stored.ContentRevision = &revVal
	}
	if in.DeploymentID != nil {
		d := *in.DeploymentID
		stored.DeploymentID = &d
	}
	if in.DeploymentConfigDigest != nil {
		c := *in.DeploymentConfigDigest
		stored.DeploymentConfigDigest = &c
	}
	return GateSubjectV2{in: stored, canonical: canonical, digest: digest}, nil
}

// ParseGateSubjectV2 restores and verifies a stored canonical v2 gate subject.
func ParseGateSubjectV2(canonical []byte) (GateSubjectV2, error) {
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var w gateSubjectWireV2
	if err := dec.Decode(&w); err != nil {
		return GateSubjectV2{}, fmt.Errorf("%w: gate subject v2: %v", domain.ErrInvalidValue, err)
	}
	if w.Format != GateSubjectFormatV2 {
		return GateSubjectV2{}, fmt.Errorf("%w: unsupported gate subject format %q", domain.ErrInvalidValue, w.Format)
	}
	in := GateSubjectV2Input{
		Scope:        domain.Scope{TenantID: domain.TenantID(w.TenantID), ProjectID: domain.ProjectID(w.ProjectID)},
		Sources:      w.Sources,
		Release:      w.Release,
		Artifacts:    w.Artifacts,
		DefinitionID: domain.DefinitionID(w.DefinitionID),
		Evidence:     w.Evidence,
	}
	if w.WorkItemID != nil {
		id := domain.WorkItemID(*w.WorkItemID)
		in.WorkItemID = &id
	}
	if w.ContentRevision != nil {
		r := domain.ContentRevision(*w.ContentRevision)
		in.ContentRevision = &r
	}
	if w.DeploymentID != nil {
		d := *w.DeploymentID
		in.DeploymentID = &d
	}
	if w.DeploymentConfigDigest != nil {
		c := runtimev1.Digest(*w.DeploymentConfigDigest)
		in.DeploymentConfigDigest = &c
	}
	s, err := NewGateSubjectV2(in)
	if err != nil {
		return GateSubjectV2{}, err
	}
	if !bytes.Equal(s.canonical, canonical) {
		return GateSubjectV2{}, fmt.Errorf("%w: gate subject v2 is not in canonical form", domain.ErrInvalidValue)
	}
	return s, nil
}

func (s GateSubjectV2) Scope() domain.Scope               { return s.in.Scope }
func (s GateSubjectV2) DefinitionID() domain.DefinitionID { return s.in.DefinitionID }
func (s GateSubjectV2) Digest() runtimev1.Digest          { return s.digest }
func (s GateSubjectV2) Canonical() []byte                 { return append([]byte(nil), s.canonical...) }
func (s GateSubjectV2) Sources() []artifactsv1.SourceRef  { return cloneSubjectSources(s.in.Sources) }
func (s GateSubjectV2) Release() *ReleasePin              { return cloneReleasePin(s.in.Release) }
func (s GateSubjectV2) Artifacts() []artifactsv1.ArtifactDigestRef {
	return append([]artifactsv1.ArtifactDigestRef(nil), s.in.Artifacts...)
}
func (s GateSubjectV2) Evidence() []artifactsv1.ArtifactDigestRef {
	return append([]artifactsv1.ArtifactDigestRef(nil), s.in.Evidence...)
}

// WorkItem returns deep copies of the optional workItemId and contentRevision.
func (s GateSubjectV2) WorkItem() (*domain.WorkItemID, *domain.ContentRevision) {
	if s.in.WorkItemID == nil || s.in.ContentRevision == nil {
		return nil, nil
	}
	id := *s.in.WorkItemID
	rev := *s.in.ContentRevision
	return &id, &rev
}

// Deployment returns deep copies of the optional deploymentId and deploymentConfigDigest.
func (s GateSubjectV2) Deployment() (*string, *runtimev1.Digest) {
	if s.in.DeploymentID == nil || s.in.DeploymentConfigDigest == nil {
		return nil, nil
	}
	id := *s.in.DeploymentID
	cfg := *s.in.DeploymentConfigDigest
	return &id, &cfg
}

func cloneReleasePin(p *ReleasePin) *ReleasePin {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func cloneSubjectSources(in []artifactsv1.SourceRef) []artifactsv1.SourceRef {
	out := make([]artifactsv1.SourceRef, len(in))
	for i, s := range in {
		if s.RepositoryID != nil {
			id := *s.RepositoryID
			s.RepositoryID = &id
		}
		out[i] = s
	}
	return out
}

func normalizeSubjectSources(in []artifactsv1.SourceRef) ([]artifactsv1.SourceRef, error) {
	out := make([]artifactsv1.SourceRef, 0, len(in))
	seen := map[string]string{}
	for _, s := range in {
		if err := s.Validate(); err != nil {
			return nil, err
		}
		repoKey := s.RepositoryURL
		if s.RepositoryID != nil {
			repoKey = *s.RepositoryID + "|" + s.RepositoryURL
		}
		if prevCommit, ok := seen[repoKey]; ok {
			if prevCommit != s.Commit {
				return nil, fmt.Errorf("%w: conflicting commits for repository %s", domain.ErrInvalidValue, s.RepositoryURL)
			}
			continue
		}
		seen[repoKey] = s.Commit
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepositoryURL != out[j].RepositoryURL {
			return out[i].RepositoryURL < out[j].RepositoryURL
		}
		return out[i].Commit < out[j].Commit
	})
	return out, nil
}

func normalizeSubjectRefs(in []artifactsv1.ArtifactDigestRef) ([]artifactsv1.ArtifactDigestRef, error) {
	out := make([]artifactsv1.ArtifactDigestRef, 0, len(in))
	byID := map[domain.ArtifactID]runtimev1.Digest{}
	for _, r := range in {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if prev, ok := byID[r.ID]; ok {
			if prev != r.Digest {
				return nil, fmt.Errorf("%w: artifact %s listed with conflicting digests", domain.ErrInvalidValue, r.ID)
			}
			continue
		}
		byID[r.ID] = r.Digest
		out = append(out, r)
	}
	if len(out) > MaxSubjectEvidence {
		return nil, fmt.Errorf("%w: too many subject artifact references", domain.ErrInvalidValue)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
