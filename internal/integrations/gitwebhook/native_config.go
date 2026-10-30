package gitwebhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Refusals of trigger configuration that a transport maps to their own codes
// (422 hosting_mismatch, 409 repository_archived). Both are also ErrInvalid.
var (
	ErrHostingMismatch    = fmt.Errorf("%w: hosting_mismatch", ErrInvalid)
	ErrRepositoryArchived = fmt.Errorf("%w: repository_archived", ErrInvalid)
)

// ActionView reads native trigger and task route configuration (project
// settings read authority). Creating and altering (including disabling) use ActionInstall,
// the same project settings authority that installs the HMAC hook trigger.
const ActionView Action = "git_trigger.view"

// NativeConfigDependencies are the collaborators of NativeConfig. All but Now
// and NewID are required.
type NativeConfigDependencies struct {
	Store        NativeStore
	Authorizer   Authorizer
	Definitions  Definitions
	Repositories PushRepositories
	Now          func() time.Time
	NewID        func() (string, error)
}

// NativeConfig is the authorized management of native triggers and the
// project's task route (GR-10, GR-11, PX-24). Every method authorizes the
// principal first, with current state, and a missing, foreign or hidden object
// is the same ErrNotFound. Altering a trigger makes the altering principal its
// configurer: admission re-checks *that* principal's run authority on every
// start, so no one can start runs under an authority they do not hold.
type NativeConfig struct{ d NativeConfigDependencies }

// NewNativeConfig validates the dependencies; a missing one is a construction
// error, never an allow-all default.
func NewNativeConfig(d NativeConfigDependencies) (*NativeConfig, error) {
	if d.Store == nil || d.Authorizer == nil || d.Definitions == nil || d.Repositories == nil {
		return nil, errors.New("gitwebhook: native store, authorizer, definitions and repositories are required")
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = randomTriggerID
	}
	return &NativeConfig{d: d}, nil
}

func randomTriggerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "trg_" + hex.EncodeToString(b[:]), nil
}

func (c *NativeConfig) authorize(ctx context.Context, principal string, scope domain.Scope, action Action) error {
	if err := scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if principal == "" || len(principal) > maxPrincipalLen {
		return ErrDenied
	}
	err := c.d.Authorizer.Authorize(ctx, principal, scope, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDenied):
		return ErrDenied
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	default:
		return unavailable("authorization", err)
	}
}

func nativeStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict), errors.Is(err, ErrInvalid):
		return err
	default:
		return unavailable("store", err)
	}
}

func (c *NativeConfig) checkDefinition(ctx context.Context, scope domain.Scope, id domain.DefinitionID) error {
	if err := id.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.d.Definitions.Registered(ctx, scope, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return unavailable("definition lookup", err)
	}
	return nil
}

// CreateTriggerCommand creates a native trigger. The identifier is assigned by
// the server; the repository must be an internally hosted one of the project.
type CreateTriggerCommand struct {
	Scope             domain.Scope
	Principal         string
	RepositoryID      string
	RefPatterns       []string
	ExcludeOriginRuns *bool
	DefinitionID      domain.DefinitionID
	Enabled           bool
}

// CreateTrigger authorizes the principal, checks that the repository is an
// internal, not archived repository of the project and that the definition is
// registered, and stores the trigger.
func (c *NativeConfig) CreateTrigger(ctx context.Context, cmd CreateTriggerCommand) (PushTrigger, error) {
	if err := c.authorize(ctx, cmd.Principal, cmd.Scope, ActionInstall); err != nil {
		return PushTrigger{}, err
	}
	id, err := c.d.NewID()
	if err != nil {
		return PushTrigger{}, unavailable("identifier", err)
	}
	now := c.d.Now().UTC()
	t := PushTrigger{
		Scope: cmd.Scope, ConfiguredBy: cmd.Principal, Version: 1, EnabledAt: now, CreatedAt: now, UpdatedAt: now,
		Config: normalize(gitv1.TriggerConfig{
			TriggerID: id, RepositoryID: cmd.RepositoryID, RefPatterns: cmd.RefPatterns, ExcludeOriginRuns: cmd.ExcludeOriginRuns,
			DefinitionID: string(cmd.DefinitionID), Enabled: cmd.Enabled,
		}),
	}
	if err := t.Validate(); err != nil {
		return PushTrigger{}, err
	}
	if err := c.checkRepository(ctx, cmd.Scope, cmd.RepositoryID); err != nil {
		return PushTrigger{}, err
	}
	if err := c.checkDefinition(ctx, cmd.Scope, cmd.DefinitionID); err != nil {
		return PushTrigger{}, err
	}
	audit := c.audit(AuditPushTriggerCreated, t.Scope, t.ID(), cmd.Principal, now, map[string]string{"repository": t.Config.RepositoryID, "definition": t.Config.DefinitionID})
	if err := c.d.Store.InsertPushTrigger(ctx, t, audit); err != nil {
		return PushTrigger{}, nativeStoreError(err)
	}
	return t.Clone(), nil
}

func (c *NativeConfig) checkRepository(ctx context.Context, scope domain.Scope, repositoryID string) error {
	repo, err := c.d.Repositories.Repository(ctx, scope, repositoryID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case err != nil:
		return unavailable("repository lookup", err)
	case repo.Hosting != gitv1.HostingInternal:
		return fmt.Errorf("%w: a native trigger needs an internally hosted repository; external hosts use the receive-hook trigger", ErrHostingMismatch)
	case repo.Status == gitv1.StatusArchived:
		return fmt.Errorf("%w: the repository is archived", ErrRepositoryArchived)
	}
	return nil
}

func (c *NativeConfig) audit(action string, scope domain.Scope, id, principal string, now time.Time, meta map[string]string) AuditRecord {
	return AuditRecord{Action: action, Scope: scope, TriggerID: TriggerID(id), Principal: principal, At: now, Metadata: meta}
}

// TriggerPatch names the members an alteration changes; nil leaves a member as
// it is. The repository binding never changes: a trigger for another
// repository is a new trigger.
type TriggerPatch struct {
	RefPatterns       *[]string
	ExcludeOriginRuns *bool
	DefinitionID      *domain.DefinitionID
	Enabled           *bool
}

// UpdateTrigger alters a trigger at the expected version. The altering
// principal becomes the configurer.
func (c *NativeConfig) UpdateTrigger(ctx context.Context, principal string, scope domain.Scope, id string, expected int64, patch TriggerPatch) (PushTrigger, error) {
	if err := c.authorize(ctx, principal, scope, ActionInstall); err != nil {
		return PushTrigger{}, err
	}
	cur, err := c.d.Store.PushTrigger(ctx, scope, id)
	if err != nil {
		return PushTrigger{}, nativeStoreError(err)
	}
	if cur.Version != expected {
		return PushTrigger{}, ErrConflict
	}
	next := cur.Clone()
	if patch.RefPatterns != nil {
		next.Config.RefPatterns = append([]string(nil), (*patch.RefPatterns)...)
	}
	if patch.ExcludeOriginRuns != nil {
		v := *patch.ExcludeOriginRuns
		next.Config.ExcludeOriginRuns = &v
	}
	if patch.DefinitionID != nil {
		next.Config.DefinitionID = string(*patch.DefinitionID)
	}
	if patch.Enabled != nil {
		next.Config.Enabled = *patch.Enabled
	}
	now := c.d.Now().UTC()
	next.ConfiguredBy, next.UpdatedAt = principal, now
	if next.Config.Enabled && !cur.Config.Enabled {
		next.EnabledAt = now // a trigger watches pushes from the moment it is (re-)enabled
	}
	if err := next.Validate(); err != nil {
		return PushTrigger{}, err
	}
	if patch.DefinitionID != nil {
		if err := c.checkDefinition(ctx, scope, *patch.DefinitionID); err != nil {
			return PushTrigger{}, err
		}
	}
	if patch.Enabled != nil && *patch.Enabled {
		// Enabling re-checks what creation checked: an archived repository must
		// not get a live trigger.
		if err := c.checkRepository(ctx, scope, next.Config.RepositoryID); err != nil {
			return PushTrigger{}, err
		}
	}
	audit := c.audit(AuditPushTriggerUpdated, scope, id, principal, now, map[string]string{"definition": next.Config.DefinitionID})
	out, err := c.d.Store.UpdatePushTrigger(ctx, next, expected, audit)
	if err != nil {
		return PushTrigger{}, nativeStoreError(err)
	}
	return out.Clone(), nil
}

// GetTrigger returns one trigger.
func (c *NativeConfig) GetTrigger(ctx context.Context, principal string, scope domain.Scope, id string) (PushTrigger, error) {
	if err := c.authorize(ctx, principal, scope, ActionView); err != nil {
		return PushTrigger{}, err
	}
	t, err := c.d.Store.PushTrigger(ctx, scope, id)
	if err != nil {
		return PushTrigger{}, nativeStoreError(err)
	}
	return t.Clone(), nil
}

// ListTriggers returns the project's triggers ordered by ID after the given ID.
func (c *NativeConfig) ListTriggers(ctx context.Context, principal string, scope domain.Scope, after string, limit int) ([]PushTrigger, error) {
	if err := c.authorize(ctx, principal, scope, ActionView); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, invalidf("limit must be between 1 and 1000")
	}
	out, err := c.d.Store.ListPushTriggers(ctx, scope, after, limit)
	return out, nativeStoreError(err)
}

// ConfigureTaskRouteCommand creates (ExpectedVersion 0) or alters the project's
// task route.
type ConfigureTaskRouteCommand struct {
	Scope           domain.Scope
	Principal       string
	DefinitionID    domain.DefinitionID
	Enabled         bool
	ExpectedVersion int64
}

// ConfigureTaskRoute authorizes the principal and stores the project's route.
func (c *NativeConfig) ConfigureTaskRoute(ctx context.Context, cmd ConfigureTaskRouteCommand) (TaskRoute, error) {
	if err := c.authorize(ctx, cmd.Principal, cmd.Scope, ActionInstall); err != nil {
		return TaskRoute{}, err
	}
	now := c.d.Now().UTC()
	r := TaskRoute{Scope: cmd.Scope, DefinitionID: cmd.DefinitionID, Enabled: cmd.Enabled, ConfiguredBy: cmd.Principal, Version: cmd.ExpectedVersion + 1, EnabledAt: now, CreatedAt: now, UpdatedAt: now}
	if err := r.Validate(); err != nil {
		return TaskRoute{}, err
	}
	if cmd.ExpectedVersion > 0 {
		prior, err := c.d.Store.TaskRoute(ctx, cmd.Scope)
		switch {
		case errors.Is(err, ErrNotFound):
			return TaskRoute{}, ErrConflict
		case err != nil:
			return TaskRoute{}, nativeStoreError(err)
		case prior.Version != cmd.ExpectedVersion:
			return TaskRoute{}, ErrConflict
		case prior.Enabled && cmd.Enabled:
			r.EnabledAt = prior.EnabledAt
		}
	}
	if err := c.checkDefinition(ctx, cmd.Scope, cmd.DefinitionID); err != nil {
		return TaskRoute{}, err
	}
	audit := c.audit(AuditTaskRouteConfigured, cmd.Scope, string(cmd.Scope.ProjectID), cmd.Principal, now, map[string]string{"definition": string(cmd.DefinitionID)})
	out, err := c.d.Store.PutTaskRoute(ctx, r, cmd.ExpectedVersion, audit)
	return out, nativeStoreError(err)
}

// GetTaskRoute returns the project's route.
func (c *NativeConfig) GetTaskRoute(ctx context.Context, principal string, scope domain.Scope) (TaskRoute, error) {
	if err := c.authorize(ctx, principal, scope, ActionView); err != nil {
		return TaskRoute{}, err
	}
	r, err := c.d.Store.TaskRoute(ctx, scope)
	return r, nativeStoreError(err)
}

// InboxDecision is one decided inbox key as the status read shows it: the stable
// outcome and reason code, never a message, name or secret.
type InboxDecision struct {
	// Subject is the push record (push) or the item and transition (task route);
	// Ref is the pushed ref (empty for a task route).
	Subject string
	Ref     string
	Outcome Outcome
	Reason  string
	RunID   domain.RunID
	At      time.Time
}

func decisionOf(r InboxRecord) *InboxDecision {
	return &InboxDecision{Subject: r.Key.Subject, Ref: r.Key.Ref, Outcome: r.Outcome, Reason: r.Reason, RunID: r.RunID, At: r.CompletedAt}
}

// Status is the configuration with the last decisions taken for it. LastRefusal
// is the most recent `rejected` decision: it is how a configurer learns that
// every start is refused (for example because their run authority was revoked)
// without admission details leaking: only the stable reason code is kept.
type Status struct {
	LastDecision *InboxDecision
	LastRefusal  *InboxDecision
}

func (c *NativeConfig) status(ctx context.Context, scope domain.Scope, kind InboxKind, source string) (Status, error) {
	var st Status
	last, ok, err := c.d.Store.LatestInbox(ctx, scope, kind, source, "")
	if err != nil {
		return Status{}, nativeStoreError(err)
	}
	if ok {
		st.LastDecision = decisionOf(last)
	}
	refused, ok, err := c.d.Store.LatestInbox(ctx, scope, kind, source, OutcomeRejected)
	if err != nil {
		return Status{}, nativeStoreError(err)
	}
	if ok {
		st.LastRefusal = decisionOf(refused)
	}
	return st, nil
}

// TriggerStatus returns the trigger with its last decisions (read authority).
func (c *NativeConfig) TriggerStatus(ctx context.Context, principal string, scope domain.Scope, id string) (PushTrigger, Status, error) {
	t, err := c.GetTrigger(ctx, principal, scope, id)
	if err != nil {
		return PushTrigger{}, Status{}, err
	}
	st, err := c.status(ctx, scope, InboxPush, id)
	return t, st, err
}

// TaskRouteStatus returns the project's route with its last decisions.
func (c *NativeConfig) TaskRouteStatus(ctx context.Context, principal string, scope domain.Scope) (TaskRoute, Status, error) {
	r, err := c.GetTaskRoute(ctx, principal, scope)
	if err != nil {
		return TaskRoute{}, Status{}, err
	}
	st, err := c.status(ctx, scope, InboxTaskReady, string(scope.ProjectID))
	return r, st, err
}
