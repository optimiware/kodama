package delivery

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ProjectLegacySnapshot projects a legacy v1 version-bearing ConfigurationSnapshot
// into an explicitly linked v2 generic snapshot without altering the legacy
// snapshot's own digest or stored bytes.
func ProjectLegacySnapshot(legacy ConfigurationSnapshot) (deliveryv1.EnvironmentSnapshotV2, error) {
	if err := legacy.Validate(); err != nil {
		return deliveryv1.EnvironmentSnapshotV2{}, err
	}
	cfg := maps.Clone(legacy.Config)
	if cfg == nil {
		cfg = map[string]string{}
	}
	if legacy.Namespace != "" {
		if _, exists := cfg["KODAMA_LEGACY_NAMESPACE"]; !exists {
			cfg["KODAMA_LEGACY_NAMESPACE"] = legacy.Namespace
		}
	}
	var ttl *int64
	if legacy.PreviewTTL > 0 {
		sec := int64(legacy.PreviewTTL / time.Second)
		ttl = &sec
	}
	logical := make([]deliveryv1.LogicalSecretBinding, 0, len(legacy.Secrets))
	for _, s := range legacy.Secrets {
		logical = append(logical, deliveryv1.LogicalSecretBinding{
			Name:     s.Name,
			Ref:      s.Ref,
			Delivery: s.Delivery,
		})
	}
	legDigest := legacy.Digest
	name := strings.ReplaceAll(strings.ToLower(string(legacy.Environment)), "_", "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "env-" + name
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return deliveryv1.NewEnvironmentSnapshotV2(deliveryv1.EnvironmentSnapshotV2Input{
		Scope:                legacy.Scope,
		Environment:          legacy.Environment,
		Name:                 name,
		Target:               legacy.Target,
		Config:               cfg,
		PreviewTTLSeconds:    ttl,
		Secrets:              logical,
		LegacySnapshotDigest: &legDigest,
	})
}

// ResolveRunCredentials resolves the logical secret bindings of a v2 generic
// environment snapshot to the currently authorized, non-revoked secret versions
// for a newly admitted run (including rollback and cleanup runs), merging any
// explicit step-declared secret bindings under VS-15.
func ResolveRunCredentials(
	ctx context.Context,
	scope domain.Scope,
	snap deliveryv1.EnvironmentSnapshotV2,
	creds Credentials,
	stepBindings []runtimev1.SecretBinding,
) ([]runtimev1.SecretBinding, error) {
	if creds == nil {
		return nil, Fail(ErrNotConfigured, nil, "credential resolver is required")
	}
	logical := snap.Secrets()
	versions := make(map[string]int64, len(logical))
	for _, l := range logical {
		info, err := creds.Current(ctx, scope, l.Ref)
		if err != nil {
			return nil, Fail(ErrCredentialUnavailable, err, "environment credential is unavailable or revoked")
		}
		versions[l.Ref] = info.Version
	}
	merged, err := deliveryv1.MergeStepAndEnvironmentSecrets(logical, versions, stepBindings)
	if err != nil {
		return nil, Fail(ErrConflict, err, "step and environment secret bindings conflict")
	}
	return merged, nil
}

// GenericDeploymentRecord holds the persisted state of a tool-neutral v2 deployment.
type GenericDeploymentRecord struct {
	ID                 DeploymentID
	Intent             deliveryv1.GenericDeploymentIntent
	Status             Status
	Reason             string
	Attribution        deliveryv1.OutcomeAttribution
	ExternalRefs       map[string]string
	ExplicitlySettled  bool
	SettledBy          *Actor
	Cleanup            CleanupState
	CleanupReason      string
	CleanupRequestedAt *time.Time
	DeletedAt          *time.Time
	ExpiresAt          *time.Time
	Version            domain.Version
	CreatedAt          time.Time
	UpdatedAt          time.Time
	History            []HistoryEntry
}

// GenericDeployment is the tool-neutral deployment aggregate (ADR-0012, DQ-20–DQ-22).
type GenericDeployment struct {
	rec GenericDeploymentRecord
}

// NewGenericDeployment validates the pre-apply intent and creates version 1 in
// StatusPending with deterministic identity derived from (scope, actionKey).
func NewGenericDeployment(intent deliveryv1.GenericDeploymentIntent, actor Actor, ttl time.Duration, now time.Time) (GenericDeployment, error) {
	if err := intent.Validate(); err != nil {
		return GenericDeployment{}, invalidf("%v", err)
	}
	if err := actor.Validate(); err != nil {
		return GenericDeployment{}, err
	}
	t := now.UTC().Truncate(time.Microsecond)
	id := DeploymentIDFor(intent.Scope, intent.ActionKey)
	var exp *time.Time
	if ttl > 0 {
		e := t.Add(ttl)
		exp = &e
	}
	rec := GenericDeploymentRecord{
		ID:           id,
		Intent:       cloneGenericIntent(intent),
		Status:       StatusPending,
		Reason:       ReasonRegistered,
		Attribution:  deliveryv1.AttributionCommandReported,
		ExternalRefs: map[string]string{},
		ExpiresAt:    exp,
		Version:      1,
		CreatedAt:    t,
		UpdatedAt:    t,
		History: []HistoryEntry{{
			Seq:        1,
			Kind:       "registered",
			Version:    1,
			FromStatus: StatusPending,
			ToStatus:   StatusPending,
			Reason:     ReasonRegistered,
			Actor:      actor,
			Attempt:    intent.AttemptID,
			At:         t,
		}},
	}
	return GenericDeployment{rec: rec}, nil
}

func (d GenericDeployment) ID() DeploymentID                           { return d.rec.ID }
func (d GenericDeployment) Scope() domain.Scope                        { return d.rec.Intent.Scope }
func (d GenericDeployment) ActionKey() string                          { return d.rec.Intent.ActionKey }
func (d GenericDeployment) Status() Status                             { return d.rec.Status }
func (d GenericDeployment) Reason() string                             { return d.rec.Reason }
func (d GenericDeployment) Version() domain.Version                    { return d.rec.Version }
func (d GenericDeployment) ExplicitlySettled() bool                    { return d.rec.ExplicitlySettled }
func (d GenericDeployment) Attribution() deliveryv1.OutcomeAttribution { return d.rec.Attribution }
func (d GenericDeployment) ExternalRefs() map[string]string            { return maps.Clone(d.rec.ExternalRefs) }
func (d GenericDeployment) Intent() deliveryv1.GenericDeploymentIntent {
	return cloneGenericIntent(d.rec.Intent)
}
func (d GenericDeployment) Record() GenericDeploymentRecord {
	cp := d.rec
	cp.Intent = cloneGenericIntent(d.rec.Intent)
	cp.ExternalRefs = maps.Clone(d.rec.ExternalRefs)
	cp.History = slices.Clone(d.rec.History)
	return cp
}

// MatchesIntent reports whether a replayed registration carries the exact same
// immutable intent.
func (d GenericDeployment) MatchesIntent(other deliveryv1.GenericDeploymentIntent) bool {
	a, b := d.rec.Intent, other
	if a.Scope != b.Scope || a.ActionKey != b.ActionKey || a.RunID != b.RunID || a.StepID != b.StepID ||
		a.Environment != b.Environment || a.Script.Image != b.Script.Image || !slices.Equal(a.Script.Command, b.Script.Command) {
		return false
	}
	if (a.Release == nil) != (b.Release == nil) || (a.Release != nil && *a.Release != *b.Release) {
		return false
	}
	if (a.SupersededDeploymentID == nil) != (b.SupersededDeploymentID == nil) ||
		(a.SupersededDeploymentID != nil && *a.SupersededDeploymentID != *b.SupersededDeploymentID) {
		return false
	}
	return slices.Equal(a.Artifacts, b.Artifacts) && sameSourceRefs(a.Sources, b.Sources)
}

// ApplyOutcome records a script's pre-apply, succeeded, failed, or unknown report.
// While Reason == ReasonOutcomeUnknown, a new "applying" call is refused with
// ErrOutcomeUnknown until the target is observed and settled.
func (d *GenericDeployment) ApplyOutcome(out deliveryv1.GenericDeploymentOutcome, actor Actor, attempt domain.AttemptID, now time.Time) error {
	if err := out.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return invalidf("%v", err)
	}
	t := now.UTC().Truncate(time.Microsecond)

	if d.rec.Status.Terminal() {
		// Idempotent replay of the exact same terminal outcome succeeds; any other
		// transition on a settled deployment conflicts.
		if (out.Status == string(d.rec.Status)) && d.rec.Attribution == out.Attribution && maps.Equal(d.rec.ExternalRefs, out.ExternalRefs) {
			return nil
		}
		return Fail(ErrConflict, nil, "deployment outcome is already settled")
	}
	if d.rec.Status == StatusRunning && d.rec.Reason == ReasonOutcomeUnknown && out.Status == "applying" {
		return Fail(ErrOutcomeUnknown, nil, "deployment outcome is unknown; observe and settle before applying again")
	}

	from := d.rec.Status
	switch out.Status {
	case "applying":
		d.rec.Status = StatusRunning
		d.rec.Reason = ReasonApplying
	case "unknown":
		d.rec.Status = StatusRunning
		d.rec.Reason = ReasonOutcomeUnknown
	case "succeeded":
		d.rec.Status = StatusSucceeded
		d.rec.Reason = ReasonReady
	case "failed":
		reason := "apply_failed"
		if out.Reason != nil && reasonCode.MatchString(*out.Reason) {
			reason = *out.Reason
		}
		d.rec.Status = StatusFailed
		d.rec.Reason = reason
	}
	d.rec.Attribution = out.Attribution
	if len(out.ExternalRefs) > 0 {
		if d.rec.ExternalRefs == nil {
			d.rec.ExternalRefs = map[string]string{}
		}
		for k, v := range out.ExternalRefs {
			d.rec.ExternalRefs[k] = v
		}
	}
	d.rec.Version++
	d.rec.UpdatedAt = t
	d.rec.History = append(d.rec.History, HistoryEntry{
		Seq:        int64(len(d.rec.History) + 1),
		Kind:       out.Status,
		Version:    d.rec.Version,
		FromStatus: from,
		ToStatus:   d.rec.Status,
		Reason:     d.rec.Reason,
		Actor:      actor,
		Attempt:    attempt,
		At:         t,
	})
	return nil
}

// SettleUnknown explicitly settles a deployment whose outcome was unknown after
// an authorized operator or reconciliation script observed the target. It marks
// ExplicitlySettled=true so execution reconciliation records VerdictSettled
// (never VerdictReplaySafe).
func (d *GenericDeployment) SettleUnknown(actor Actor, finalStatus Status, reason string, externalRefs map[string]string, now time.Time) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if finalStatus != StatusSucceeded && finalStatus != StatusFailed {
		return invalidf("explicit settlement requires succeeded or failed status")
	}
	if d.rec.Status != StatusRunning || d.rec.Reason != ReasonOutcomeUnknown {
		return Fail(ErrConflict, nil, "only an unknown-outcome running deployment can be explicitly settled")
	}
	if reason == "" {
		if finalStatus == StatusSucceeded {
			reason = ReasonReady
		} else {
			reason = "reconciled_failed"
		}
	}
	if !reasonCode.MatchString(reason) {
		return invalidf("invalid settlement reason %q", reason)
	}
	for k, v := range externalRefs {
		if len(k) == 0 || len(v) == 0 || len(v) > deliveryv1.MaxExternalRefVal {
			return invalidf("invalid externalRef %q", k)
		}
	}
	t := now.UTC().Truncate(time.Microsecond)
	from := d.rec.Status
	d.rec.Status = finalStatus
	d.rec.Reason = reason
	d.rec.Attribution = deliveryv1.AttributionToolObserved
	d.rec.ExplicitlySettled = true
	actCopy := actor
	d.rec.SettledBy = &actCopy
	if len(externalRefs) > 0 {
		d.rec.ExternalRefs = maps.Clone(externalRefs)
	}
	d.rec.Version++
	d.rec.UpdatedAt = t
	d.rec.History = append(d.rec.History, HistoryEntry{
		Seq:        int64(len(d.rec.History) + 1),
		Kind:       fmt.Sprintf("settled_%s", finalStatus),
		Version:    d.rec.Version,
		FromStatus: from,
		ToStatus:   finalStatus,
		Reason:     reason,
		Actor:      actor,
		At:         t,
	})
	return nil
}

// GenericRuntimeRegisterRequest is the untrusted runtime request body for
// registering a tool-neutral v2 deployment. Scope, run, step, attempt, action
// key, environment pin and script identity are NEVER taken from this request;
// they are derived from the attempt's verified runtime grant and immutable
// pinned context.
type GenericRuntimeRegisterRequest struct {
	Release                *deliveryv1.ReleasePin          `json:"release"`
	Artifacts              []artifactsv1.ArtifactDigestRef `json:"artifacts"`
	Sources                []artifactsv1.SourceRef         `json:"sources"`
	SupersededDeploymentID *string                         `json:"supersededDeploymentId"`
}

// GenericStore persists tool-neutral v2 deployment records.
type GenericStore interface {
	SaveGeneric(ctx context.Context, dep GenericDeployment) (GenericDeployment, bool, error)
	GetGeneric(ctx context.Context, scope domain.Scope, id DeploymentID) (GenericDeployment, error)
	FindGenericByAction(ctx context.Context, scope domain.Scope, actionKey string) (GenericDeployment, error)
	UpdateGeneric(ctx context.Context, scope domain.Scope, id DeploymentID, fn func(*GenericDeployment) error) (GenericDeployment, error)
}

// GenericServiceConfig wires the authenticated, durable tool-neutral delivery use cases.
type GenericServiceConfig struct {
	Store     GenericStore
	Access    Access
	Authority Authority
	Contexts  PinnedContexts
	Artifacts Artifacts
	Clock     func() time.Time
}

// GenericService coordinates authenticated runtime registration, artifact
// retention pinning, outcome reporting, and authorized human settlement for
// tool-neutral v2 deployments.
type GenericService struct {
	cfg GenericServiceConfig
	now func() time.Time
}

// NewGenericService validates GenericServiceConfig.
func NewGenericService(cfg GenericServiceConfig) (*GenericService, error) {
	if cfg.Store == nil || cfg.Access == nil || cfg.Authority == nil || cfg.Contexts == nil || cfg.Artifacts == nil {
		return nil, Fail(ErrNotConfigured, nil, "generic delivery requires store, access, authority, pinned contexts and artifacts")
	}
	now := cfg.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &GenericService{cfg: cfg, now: now}, nil
}

// RegisterRuntime authenticates the attempt's deployment.call grant, derives
// scope, run, step, action key, environment pin and script identity from the
// attempt's immutable pinned context, verifies and pins every referenced
// artifact in the same scope, and durably registers the pre-apply deployment.
func (s *GenericService) RegisterRuntime(ctx context.Context, attempt domain.AttemptID, bearer string, req GenericRuntimeRegisterRequest) (GenericDeployment, bool, error) {
	if err := attempt.Validate(); err != nil {
		return GenericDeployment{}, false, invalidf("%v", err)
	}
	p, err := s.cfg.Authority.Authenticate(ctx, attempt, bearer)
	if err != nil {
		return GenericDeployment{}, false, err
	}
	if p.Attempt != attempt || p.Scope.Validate() != nil || p.Run.Validate() != nil {
		return GenericDeployment{}, false, Fail(ErrDenied, nil, "runtime grant does not match route attempt")
	}
	pinnedCtx, err := s.cfg.Contexts.Pinned(ctx, p.Scope, p.Attempt)
	if err != nil {
		return GenericDeployment{}, false, Fail(ErrPinRequired, err, "pinned context is required")
	}
	if pinnedCtx.Scope() != p.Scope || pinnedCtx.RunID != p.Run || pinnedCtx.AttemptID != p.Attempt || pinnedCtx.ActionKey == "" {
		return GenericDeployment{}, false, Fail(ErrPinRequired, nil, "pinned context disagrees with runtime grant")
	}
	if pinnedCtx.Environment == nil {
		return GenericDeployment{}, false, Fail(ErrPinRequired, nil, "step has no pinned environment")
	}
	sources := slices.Clone(req.Sources)
	if len(sources) == 0 && pinnedCtx.Workspace.Source != nil {
		sources = []artifactsv1.SourceRef{{
			RepositoryURL: pinnedCtx.Workspace.Source.RepositoryURL,
			Commit:        string(pinnedCtx.Workspace.Source.Commit),
		}}
	}
	intent := deliveryv1.GenericDeploymentIntent{
		Scope:                  p.Scope,
		ActionKey:              pinnedCtx.ActionKey,
		RunID:                  p.Run,
		StepID:                 pinnedCtx.StepID,
		AttemptID:              p.Attempt,
		Environment:            *pinnedCtx.Environment,
		Release:                req.Release,
		Artifacts:              slices.Clone(req.Artifacts),
		Sources:                sources,
		Script:                 deliveryv1.ScriptIdentity{Image: pinnedCtx.Command.Image, Command: slices.Clone(pinnedCtx.Command.Command)},
		SupersededDeploymentID: req.SupersededDeploymentID,
	}
	dep, err := NewGenericDeployment(intent, Actor{Type: ActorWorkload, ID: string(p.Attempt)}, 0, s.now())
	if err != nil {
		return GenericDeployment{}, false, err
	}

	// Verify ownership/usability/digest and pin every referenced artifact BEFORE
	// committing the deployment record.
	for _, a := range intent.Artifacts {
		ev, err := s.cfg.Artifacts.Evidence(ctx, p.Scope, a.ID)
		if err != nil || ev.Scope != p.Scope || !ev.Usable || ev.Digest != a.Digest {
			return GenericDeployment{}, false, Fail(ErrProvenance, err, "deployment artifact is missing, foreign, incomplete or mismatched")
		}
		if err := s.cfg.Artifacts.Protect(ctx, p.Scope, a.ID, dep.ID()); err != nil {
			return GenericDeployment{}, false, err
		}
	}

	boundCtx, err := s.cfg.Authority.Bind(ctx, p)
	if err != nil {
		return GenericDeployment{}, false, err
	}
	saved, created, err := s.cfg.Store.SaveGeneric(boundCtx, dep)
	if err != nil {
		return GenericDeployment{}, false, err
	}
	return saved, created, nil
}

// ReportRuntimeOutcome authenticates the attempt's grant, verifies the deployment
// belongs to the attempt's own action key and scope, and records the outcome.
func (s *GenericService) ReportRuntimeOutcome(ctx context.Context, attempt domain.AttemptID, bearer string, id DeploymentID, out deliveryv1.GenericDeploymentOutcome) (GenericDeployment, error) {
	if err := attempt.Validate(); err != nil {
		return GenericDeployment{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return GenericDeployment{}, err
	}
	p, err := s.cfg.Authority.Authenticate(ctx, attempt, bearer)
	if err != nil {
		return GenericDeployment{}, err
	}
	pinnedCtx, err := s.cfg.Contexts.Pinned(ctx, p.Scope, p.Attempt)
	if err != nil || pinnedCtx.Scope() != p.Scope || pinnedCtx.AttemptID != p.Attempt {
		return GenericDeployment{}, Fail(ErrPinRequired, err, "pinned context is required")
	}
	if DeploymentIDFor(p.Scope, pinnedCtx.ActionKey) != id {
		return GenericDeployment{}, Fail(ErrDenied, nil, "deployment does not belong to this attempt's action")
	}
	boundCtx, err := s.cfg.Authority.Bind(ctx, p)
	if err != nil {
		return GenericDeployment{}, err
	}
	actor := Actor{Type: ActorWorkload, ID: string(p.Attempt)}
	return s.cfg.Store.UpdateGeneric(boundCtx, p.Scope, id, func(d *GenericDeployment) error {
		return d.ApplyOutcome(out, actor, p.Attempt, s.now())
	})
}

// SettleUnknown authorizes a human operator with deployment:manage and settles
// an unknown-outcome generic deployment.
func (s *GenericService) SettleUnknown(ctx context.Context, caller Caller, scope domain.Scope, id DeploymentID, finalStatus Status, reason string, externalRefs map[string]string) (GenericDeployment, error) {
	if err := scope.Validate(); err != nil {
		return GenericDeployment{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return GenericDeployment{}, err
	}
	if err := caller.Actor.Validate(); err != nil {
		return GenericDeployment{}, Fail(ErrDenied, err, "invalid caller")
	}
	if err := s.cfg.Access.Authorize(ctx, caller.Actor, scope, PermDeploymentManage); err != nil {
		return GenericDeployment{}, err
	}
	return s.cfg.Store.UpdateGeneric(ctx, scope, id, func(d *GenericDeployment) error {
		return d.SettleUnknown(caller.Actor, finalStatus, reason, externalRefs, s.now())
	})
}

func cloneGenericIntent(in deliveryv1.GenericDeploymentIntent) deliveryv1.GenericDeploymentIntent {
	if in.Release != nil {
		r := *in.Release
		in.Release = &r
	}
	if in.SupersededDeploymentID != nil {
		s := *in.SupersededDeploymentID
		in.SupersededDeploymentID = &s
	}
	in.Artifacts = slices.Clone(in.Artifacts)
	in.Sources = slices.Clone(in.Sources)
	in.Script.Command = slices.Clone(in.Script.Command)
	return in
}

func sameSourceRefs(a, b []artifactsv1.SourceRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		var idA, idB string
		if a[i].RepositoryID != nil {
			idA = *a[i].RepositoryID
		}
		if b[i].RepositoryID != nil {
			idB = *b[i].RepositoryID
		}
		if idA != idB || a[i].RepositoryURL != b[i].RepositoryURL || a[i].Commit != b[i].Commit {
			return false
		}
	}
	return true
}
