// Package deliveryv1 defines the frozen ADR-0012 tool-neutral environment
// snapshots, generic deployment records, and versioned gate subjects.
package deliveryv1

import (
	"fmt"
	"regexp"
	"strings"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	EnvironmentSnapshotFormatV2 = "kodama/environment-snapshot/v2"
	GateSubjectFormatV2         = "kodama/gate-subject/v2"

	MaxConfigEntries   = 64
	MaxConfigValue     = 1024
	MaxSecretBindings  = 32
	MaxExternalRefs    = 64
	MaxExternalRefVal  = 512
	MaxSubjectEvidence = 256
)

var (
	envNameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	targetRe    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)
	configKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	safeNameRe  = regexp.MustCompile(runtimev1.Patterns["SafeName"])
	secretRefRe = regexp.MustCompile(`^secret://project/[a-z][a-z0-9-]{0,62}$`)
	imageRefRe  = regexp.MustCompile(runtimev1.Patterns["ImageRef"])
)

// OutcomeAttribution distinguishes a generic exit report from an explicit tool observation.
type OutcomeAttribution string

const (
	AttributionCommandReported OutcomeAttribution = "command_reported"
	AttributionToolObserved    OutcomeAttribution = "tool_observed"
)

// LogicalSecretBinding binds a logical secret reference, name, and delivery form
// to an environment without embedding a secret value version in the config digest.
type LogicalSecretBinding struct {
	Name     string                   `json:"name"`
	Ref      string                   `json:"ref"`
	Delivery runtimev1.SecretDelivery `json:"delivery"`
}

func (b LogicalSecretBinding) Validate() error {
	if !safeNameRe.MatchString(b.Name) {
		return fmt.Errorf("%w: invalid secret binding name %q", domain.ErrInvalidValue, b.Name)
	}
	if !secretRefRe.MatchString(b.Ref) {
		return fmt.Errorf("%w: secret ref must match secret://project/<name>", domain.ErrInvalidValue)
	}
	if b.Delivery != runtimev1.DeliveryFile && b.Delivery != runtimev1.DeliveryEnv {
		return fmt.Errorf("%w: delivery must be file or env", domain.ErrInvalidValue)
	}
	return nil
}

// EnvironmentSnapshotV2Input builds a tool-neutral environment configuration snapshot.
// Unlike v1, namespace and kubeconfig are optional config/secret entries and secret
// value versions are pinned per run rather than hashed into ConfigDigest.
type EnvironmentSnapshotV2Input struct {
	Scope                domain.Scope
	Environment          domain.EnvironmentID
	Name                 string
	Target               string
	Config               map[string]string
	PreviewTTLSeconds    *int64
	CleanupDefinition    *string
	Secrets              []LogicalSecretBinding
	LegacySnapshotDigest *runtimev1.Digest
}

// ScriptIdentity records the digest-pinned container image and command of a deployment step.
type ScriptIdentity struct {
	Image   string   `json:"image"`
	Command []string `json:"command"`
}

func (s ScriptIdentity) Validate() error {
	if !imageRefRe.MatchString(s.Image) {
		return fmt.Errorf("%w: script image must be digest-pinned", domain.ErrInvalidValue)
	}
	if len(s.Command) == 0 || len(s.Command) > 64 || s.Command[0] == "" {
		return fmt.Errorf("%w: script command requires 1..64 non-empty elements", domain.ErrInvalidValue)
	}
	return nil
}

// ReleasePin identifies an immutable release and its canonical manifest digest.
type ReleasePin struct {
	ID             domain.ReleaseID `json:"id"`
	ManifestDigest runtimev1.Digest `json:"manifestDigest"`
}

func (p ReleasePin) Validate() error {
	if err := p.ID.Validate(); err != nil {
		return err
	}
	return p.ManifestDigest.Validate()
}

// GenericDeploymentIntent is the tool-neutral pre-apply registration payload.
// Infrastructure-only deployments may have Release=nil and Artifacts=[] while
// binding Sources, Environment and Script.
type GenericDeploymentIntent struct {
	Scope                  domain.Scope                    `json:"scope"`
	ActionKey              string                          `json:"actionKey"`
	RunID                  domain.RunID                    `json:"runId"`
	StepID                 string                          `json:"stepId"`
	AttemptID              domain.AttemptID                `json:"attemptId"`
	Environment            runtimev1.EnvironmentPin        `json:"environment"`
	Release                *ReleasePin                     `json:"release"`
	Artifacts              []artifactsv1.ArtifactDigestRef `json:"artifacts"`
	Sources                []artifactsv1.SourceRef         `json:"sources"`
	Script                 ScriptIdentity                  `json:"script"`
	SupersededDeploymentID *string                         `json:"supersededDeploymentId"`
}

func (d GenericDeploymentIntent) Validate() error {
	if err := d.Scope.Validate(); err != nil {
		return err
	}
	if len(d.ActionKey) < 1 || len(d.ActionKey) > 256 {
		return fmt.Errorf("%w: actionKey is required", domain.ErrInvalidValue)
	}
	if err := d.RunID.Validate(); err != nil {
		return err
	}
	if len(d.StepID) < 1 || len(d.StepID) > 128 {
		return fmt.Errorf("%w: stepId is required", domain.ErrInvalidValue)
	}
	if err := d.AttemptID.Validate(); err != nil {
		return err
	}
	if err := d.Environment.ID.Validate(); err != nil || d.Environment.ConfigDigest.Validate() != nil {
		return fmt.Errorf("%w: valid environment pin is required", domain.ErrInvalidValue)
	}
	if d.Release != nil {
		if err := d.Release.Validate(); err != nil {
			return err
		}
	}
	for _, a := range d.Artifacts {
		if err := a.Validate(); err != nil {
			return err
		}
	}
	for _, s := range d.Sources {
		if err := s.Validate(); err != nil {
			return err
		}
	}
	if d.Release == nil && len(d.Artifacts) == 0 && len(d.Sources) == 0 {
		return fmt.Errorf("%w: deployment requires at least one release, artifact, or source reference", domain.ErrInvalidValue)
	}
	if d.SupersededDeploymentID != nil {
		if err := domain.ArtifactID(*d.SupersededDeploymentID).Validate(); err != nil {
			return fmt.Errorf("%w: invalid supersededDeploymentId", domain.ErrInvalidValue)
		}
	}
	return d.Script.Validate()
}

// GenericDeploymentOutcome reports the result and optional tool observations of a deployment.
type GenericDeploymentOutcome struct {
	Status       string             `json:"status"`
	Reason       *string            `json:"reason"`
	Attribution  OutcomeAttribution `json:"attribution"`
	ExternalRefs map[string]string  `json:"externalRefs"`
}

func (o GenericDeploymentOutcome) Validate() error {
	switch o.Status {
	case "applying", "succeeded", "failed", "unknown":
	default:
		return fmt.Errorf("%w: invalid outcome status %q", domain.ErrInvalidValue, o.Status)
	}
	switch o.Attribution {
	case AttributionCommandReported, AttributionToolObserved:
	default:
		return fmt.Errorf("%w: invalid outcome attribution %q", domain.ErrInvalidValue, o.Attribution)
	}
	if len(o.ExternalRefs) > MaxExternalRefs {
		return fmt.Errorf("%w: too many externalRefs", domain.ErrInvalidValue)
	}
	for k, v := range o.ExternalRefs {
		if !configKeyRe.MatchString(k) || len(v) == 0 || len(v) > MaxExternalRefVal || strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: invalid externalRef %q", domain.ErrInvalidValue, k)
		}
	}
	return nil
}

// GateSubjectV2Input constructs a versioned gate subject that can bind a source
// set and/or an exact release or artifact set, without synthesizing fake commits
// or work items for release-only gates.
type GateSubjectV2Input struct {
	Scope                  domain.Scope
	WorkItemID             *domain.WorkItemID
	ContentRevision        *domain.ContentRevision
	Sources                []artifactsv1.SourceRef
	Release                *ReleasePin
	Artifacts              []artifactsv1.ArtifactDigestRef
	DefinitionID           domain.DefinitionID
	Evidence               []artifactsv1.ArtifactDigestRef
	DeploymentID           *string
	DeploymentConfigDigest *runtimev1.Digest
}
