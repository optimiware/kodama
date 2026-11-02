// Package artifactsv1 defines the frozen ADR-0012 contract shapes for
// immutable release manifests, cross-run artifact input fetch, release
// admission bindings and verified destination publication receipts.
package artifactsv1

import (
	"fmt"
	"regexp"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	ReleaseManifestFormat    = "kodama/release-manifest/v1"
	PublicationReceiptFormat = "kodama/publication-receipt/v1"

	MaxReleaseEntries      = 256
	MaxManifestBytes       = 1 << 20
	MaxConsumedArtifacts   = 64
	MaxDestinationRefBytes = 512
)

var (
	releaseNameRe  = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)
	versionLabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	entryNameRe    = regexp.MustCompile(runtimev1.Patterns["SafeName"])
	mediaTypeRe    = regexp.MustCompile(runtimev1.Patterns["MediaType"])
	repoURLRe      = regexp.MustCompile(runtimev1.Patterns["RepositoryURL"])
	commitRe       = regexp.MustCompile(runtimev1.Patterns["GitCommit"])
	registryRefRe  = regexp.MustCompile(runtimev1.Patterns["RegistryRef"])
	s3ObjectRefRe  = regexp.MustCompile(`^s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/[A-Za-z0-9._/-]{1,400}$`)
)

// DestinationKind identifies the storage protocol of a published destination.
type DestinationKind string

const (
	DestinationOCI DestinationKind = "oci"
	DestinationS3  DestinationKind = "s3"
)

// ArtifactDigestRef pairs an immutable artifact ID with its verified content digest.
type ArtifactDigestRef struct {
	ID     domain.ArtifactID `json:"id"`
	Digest runtimev1.Digest  `json:"digest"`
}

func (r ArtifactDigestRef) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return fmt.Errorf("artifact ref id: %w", err)
	}
	if err := r.Digest.Validate(); err != nil {
		return fmt.Errorf("artifact ref digest: %w", err)
	}
	return nil
}

// SourceRef identifies one repository commit in a source set, compatible with
// ADR-0011 internal repositories and external HTTPS repositories.
type SourceRef struct {
	RepositoryID  *string `json:"repositoryId"`
	RepositoryURL string  `json:"repositoryUrl"`
	Commit        string  `json:"commit"`
}

func (s SourceRef) Validate() error {
	if s.RepositoryID != nil {
		if err := domain.ArtifactID(*s.RepositoryID).Validate(); err != nil {
			return fmt.Errorf("%w: invalid repository id", domain.ErrInvalidValue)
		}
	}
	if !repoURLRe.MatchString(s.RepositoryURL) {
		return fmt.Errorf("%w: repositoryUrl must be a credential-free HTTPS URL", domain.ErrInvalidValue)
	}
	if !commitRe.MatchString(s.Commit) {
		return fmt.Errorf("%w: commit must be a full hex object name", domain.ErrInvalidValue)
	}
	return nil
}

// ManifestEntry is one named deployable or evidence output in a release manifest.
type ManifestEntry struct {
	ArtifactID domain.ArtifactID      `json:"artifactId"`
	Digest     runtimev1.Digest       `json:"digest"`
	Kind       runtimev1.ArtifactKind `json:"kind"`
	MediaType  *string                `json:"mediaType"`
}

func (e ManifestEntry) Validate() error {
	if err := e.ArtifactID.Validate(); err != nil {
		return err
	}
	if err := e.Digest.Validate(); err != nil {
		return err
	}
	switch e.Kind {
	case runtimev1.KindFile:
		if e.MediaType == nil || !mediaTypeRe.MatchString(*e.MediaType) {
			return fmt.Errorf("%w: file release entry requires a valid mediaType", domain.ErrInvalidValue)
		}
	case runtimev1.KindOCI:
		if e.MediaType != nil && !mediaTypeRe.MatchString(*e.MediaType) {
			return fmt.Errorf("%w: invalid OCI mediaType", domain.ErrInvalidValue)
		}
	default:
		return fmt.Errorf("%w: unknown artifact kind %q", domain.ErrInvalidValue, e.Kind)
	}
	return nil
}

// ProducerRef records the verified producer lineage of one artifact in a release.
type ProducerRef struct {
	ArtifactID        domain.ArtifactID   `json:"artifactId"`
	AttemptID         domain.AttemptID    `json:"attemptId"`
	RunID             domain.RunID        `json:"runId"`
	StepID            string              `json:"stepId"`
	ToolImage         *string             `json:"toolImage"`
	WorkspaceState    *string             `json:"workspaceState"`
	ConsumedArtifacts []ArtifactDigestRef `json:"consumedArtifacts"`
}

func (p ProducerRef) Validate() error {
	if err := p.ArtifactID.Validate(); err != nil {
		return err
	}
	if err := p.AttemptID.Validate(); err != nil {
		return err
	}
	if err := p.RunID.Validate(); err != nil {
		return err
	}
	if len(p.StepID) < 1 || len(p.StepID) > 128 {
		return fmt.Errorf("%w: stepId must be 1..128 bytes", domain.ErrInvalidValue)
	}
	if p.WorkspaceState != nil {
		switch *p.WorkspaceState {
		case "none", "clean", "modified":
		default:
			return fmt.Errorf("%w: invalid workspaceState %q", domain.ErrInvalidValue, *p.WorkspaceState)
		}
	}
	if len(p.ConsumedArtifacts) > MaxConsumedArtifacts {
		return fmt.Errorf("%w: too many consumed artifacts", domain.ErrInvalidValue)
	}
	for _, c := range p.ConsumedArtifacts {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseManifestInput is the unnormalized input used to build a canonical ReleaseManifest.
type ReleaseManifestInput struct {
	Scope        domain.Scope
	Name         string
	VersionLabel string
	Artifacts    map[string]ManifestEntry
	Sources      []SourceRef
	Producers    []ProducerRef
}

// ReleaseSelection resolves a named release and maps its entries to pipeline inputs at admission.
type ReleaseSelection struct {
	Name         string            `json:"name"`
	VersionLabel string            `json:"versionLabel"`
	Bindings     map[string]string `json:"bindings"`
}

func (s ReleaseSelection) Validate() error {
	if !releaseNameRe.MatchString(s.Name) {
		return fmt.Errorf("%w: invalid release name %q", domain.ErrInvalidValue, s.Name)
	}
	if !versionLabelRe.MatchString(s.VersionLabel) {
		return fmt.Errorf("%w: invalid release version %q", domain.ErrInvalidValue, s.VersionLabel)
	}
	if len(s.Bindings) == 0 || len(s.Bindings) > MaxReleaseEntries {
		return fmt.Errorf("%w: release selection must bind 1..%d inputs", domain.ErrInvalidValue, MaxReleaseEntries)
	}
	for inputName, entryName := range s.Bindings {
		if !entryNameRe.MatchString(inputName) || !entryNameRe.MatchString(entryName) {
			return fmt.Errorf("%w: invalid release binding %q=%q", domain.ErrInvalidValue, inputName, entryName)
		}
	}
	return nil
}

// InputFetchRequest asks the runtime API for verified metadata and a short-lived
// download instruction for an admitted artifact input.
type InputFetchRequest struct {
	Input      string            `json:"input"`
	ArtifactID domain.ArtifactID `json:"artifactId"`
	Digest     runtimev1.Digest  `json:"digest"`
	Download   bool              `json:"download"`
}

func (r InputFetchRequest) Validate() error {
	if !entryNameRe.MatchString(r.Input) {
		return fmt.Errorf("%w: invalid input name %q", domain.ErrInvalidValue, r.Input)
	}
	if err := r.ArtifactID.Validate(); err != nil {
		return err
	}
	return r.Digest.Validate()
}

// InputFetchResponse returns the verified artifact input metadata and optional
// short-lived download instruction.
type InputFetchResponse struct {
	Input      string                 `json:"input"`
	ArtifactID domain.ArtifactID      `json:"artifactId"`
	Kind       runtimev1.ArtifactKind `json:"kind"`
	Digest     runtimev1.Digest       `json:"digest"`
	MediaType  *string                `json:"mediaType"`
	SizeBytes  *int64                 `json:"sizeBytes"`
	Registry   *string                `json:"registry"`
	Download   *runtimev1.Upload      `json:"download"`
}

// DestinationRef identifies an external registry repository or S3 object where
// an artifact copy was published and verified.
type DestinationRef struct {
	Kind      DestinationKind  `json:"kind"`
	Reference string           `json:"reference"`
	Digest    runtimev1.Digest `json:"digest"`
}

func (d DestinationRef) Validate() error {
	if len(d.Reference) == 0 || len(d.Reference) > MaxDestinationRefBytes {
		return fmt.Errorf("%w: destination reference out of bounds", domain.ErrInvalidValue)
	}
	if err := d.Digest.Validate(); err != nil {
		return err
	}
	switch d.Kind {
	case DestinationOCI:
		if !registryRefRe.MatchString(d.Reference) {
			return fmt.Errorf("%w: OCI destination must be canonical host/repository without tag", domain.ErrInvalidValue)
		}
	case DestinationS3:
		if !s3ObjectRefRe.MatchString(d.Reference) {
			return fmt.Errorf("%w: S3 destination must be s3://bucket/key without query or credentials", domain.ErrInvalidValue)
		}
	default:
		return fmt.Errorf("%w: unknown destination kind %q", domain.ErrInvalidValue, d.Kind)
	}
	return nil
}

// PublicationReceipt records an attributable verification that an artifact was
// copied to a destination with the expected digest.
type PublicationReceipt struct {
	ID             domain.PublicationID `json:"id"`
	Scope          domain.Scope         `json:"scope"`
	ArtifactID     domain.ArtifactID    `json:"artifactId"`
	ArtifactDigest runtimev1.Digest     `json:"artifactDigest"`
	Destination    DestinationRef       `json:"destination"`
	RunID          domain.RunID         `json:"runId"`
	AttemptID      domain.AttemptID     `json:"attemptId"`
	StepID         string               `json:"stepId"`
	CompleteGraph  bool                 `json:"completeGraph"`
	VerifiedAt     runtimev1.Timestamp  `json:"verifiedAt"`
}

func (r PublicationReceipt) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := r.ArtifactID.Validate(); err != nil {
		return err
	}
	if err := r.ArtifactDigest.Validate(); err != nil {
		return err
	}
	if err := r.Destination.Validate(); err != nil {
		return err
	}
	if r.Destination.Digest != r.ArtifactDigest {
		return fmt.Errorf("%w: same-content publication receipt requires destination digest to match artifact digest", domain.ErrInvalidValue)
	}
	if !r.CompleteGraph {
		return fmt.Errorf("%w: a verified publication receipt requires completeGraph=true", domain.ErrInvalidValue)
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if err := r.AttemptID.Validate(); err != nil {
		return err
	}
	if len(r.StepID) < 1 || len(r.StepID) > 128 {
		return fmt.Errorf("%w: stepId must be 1..128 bytes", domain.ErrInvalidValue)
	}
	return r.VerifiedAt.Validate()
}
