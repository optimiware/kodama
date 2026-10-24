package gateway

import (
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ModelSettings is the current model-access configuration of one project. It
// names one approved endpoint, one typed credential at an exact secret version,
// the models a run may ask for, the bounded allowances and whether raw content
// may be recorded (default off, independent of usage metadata).
//
// Settings are edited by replacing the whole representation under an expected
// revision. A project without a stored row has the virtual disabled revision
// one (see Disabled); the first stored revision is therefore two, so a PUT never
// needs wildcard creation semantics. Runs do not follow later edits: they
// capture an immutable RunBinding at their first model admission.
type ModelSettings struct {
	Scope      domain.Scope
	Revision   int64
	Enabled    bool
	Endpoint   Endpoint
	Credential CredentialRef
	Models     []string
	Limits     Limits
	Recording  bool
	UpdatedAt  time.Time
	// UpdatedBy is the user who stored this revision ("" for the virtual one).
	UpdatedBy string
}

// Disabled returns the virtual revision-one settings of a project that never
// configured model access.
func Disabled(scope domain.Scope) ModelSettings {
	return ModelSettings{Scope: scope, Revision: 1}
}

// Stored reports whether the settings come from a stored revision.
func (s ModelSettings) Stored() bool { return s.Revision > 1 }

// SettingsEdit is the replacement representation of a PUT.
type SettingsEdit struct {
	Enabled    bool
	Endpoint   string
	Credential CredentialRef
	Models     []string
	Limits     Limits
	Recording  bool
}

// Validate checks the replacement without applying it.
func (e SettingsEdit) Validate() error {
	_, err := e.normalize()
	return err
}

type normalized struct {
	endpoint Endpoint
	models   []string
}

func (e SettingsEdit) normalize() (normalized, error) {
	ep, err := ParseEndpoint(e.Endpoint)
	if err != nil {
		return normalized{}, err
	}
	if err := e.Credential.Validate(); err != nil {
		return normalized{}, err
	}
	if err := e.Limits.Validate(); err != nil {
		return normalized{}, err
	}
	if e.Limits.TokenAllowance > domain.MaxSafeInteger-e.Limits.RequestBound() {
		return normalized{}, fail(ErrInvalid, "the allowance plus one request bound exceeds the supported integer range")
	}
	if len(e.Models) < 1 || len(e.Models) > MaxModels {
		return normalized{}, fail(ErrInvalid, "the model allow-list must hold 1 to %d models", MaxModels)
	}
	models := slices.Clone(e.Models)
	slices.Sort(models)
	for i, m := range models {
		if !ValidModelName(m) {
			return normalized{}, fail(ErrInvalid, "a model name is not valid")
		}
		if i > 0 && models[i-1] == m {
			return normalized{}, fail(ErrInvalid, "the model allow-list repeats a model")
		}
	}
	return normalized{endpoint: ep, models: models}, nil
}

// Apply replaces the settings if expected is the current revision. It returns
// the new settings and the names of the changed members; an edit that changes
// nothing returns the settings unchanged (same revision, no names). The
// expected revision must be the current one: replaying a successful update with
// its old revision is a version conflict and the caller must reread, while a
// no-op replacement that carries the current revision changes nothing.
func (s ModelSettings) Apply(expected int64, e SettingsEdit, actor string, now time.Time) (ModelSettings, []string, error) {
	if expected != s.Revision {
		return ModelSettings{}, nil, fail(ErrVersionConflict, "the settings changed; reload and retry")
	}
	n, err := e.normalize()
	if err != nil {
		return ModelSettings{}, nil, err
	}
	if actor == "" || len(actor) > 256 {
		return ModelSettings{}, nil, fail(ErrInvalid, "an editing actor is required")
	}
	next := s
	next.Enabled, next.Endpoint, next.Credential = e.Enabled, n.endpoint, e.Credential
	next.Models, next.Limits, next.Recording = n.models, e.Limits, e.Recording
	var changed []string
	add := func(cond bool, name string) {
		if cond {
			changed = append(changed, name)
		}
	}
	add(s.Enabled != next.Enabled, "model.enabled")
	add(s.Endpoint != next.Endpoint, "model.endpoint")
	add(s.Credential != next.Credential, "model.credential")
	add(!slices.Equal(s.Models, next.Models), "model.models")
	add(s.Limits != next.Limits, "model.limits")
	add(s.Recording != next.Recording, "model.recording")
	if len(changed) == 0 && s.Stored() {
		return s, nil, nil
	}
	if len(changed) == 0 {
		// The first stored revision of a project that changes nothing from the
		// virtual one cannot be reached: an edit always sets an endpoint.
		changed = []string{"model.enabled"}
	}
	if s.Revision >= domain.MaxSafeInteger {
		return ModelSettings{}, nil, fail(ErrInvalid, "the settings revision is exhausted")
	}
	next.Revision = s.Revision + 1
	next.UpdatedAt, next.UpdatedBy = now.UTC(), actor
	return next, changed, nil
}

// Allows reports whether the model is on the allow-list.
func (s ModelSettings) Allows(model string) bool { return slices.Contains(s.Models, model) }

// RunBinding is the immutable model policy of one run: the settings revision,
// endpoint, model allow-list, limits and the exact credential version captured
// when the run first asked for a model. Editing the project settings, rotating
// the secret or removing a model changes later runs only; a run keeps its
// binding and its pinned credential version for its whole life.
type RunBinding struct {
	Scope            domain.Scope
	RunID            domain.RunID
	SettingsRevision int64
	Endpoint         Endpoint
	Credential       CredentialRef
	Models           []string
	Limits           Limits
	Recording        bool
	CreatedAt        time.Time
}

// BindRun captures the current enabled settings for a run.
func BindRun(s ModelSettings, run domain.RunID, now time.Time) (RunBinding, error) {
	if !s.Enabled || !s.Stored() {
		return RunBinding{}, fail(ErrNotConfigured, "model access is not enabled for this project")
	}
	if err := run.Validate(); err != nil {
		return RunBinding{}, fail(ErrInvalid, "the run identifier is malformed")
	}
	b := RunBinding{
		Scope: s.Scope, RunID: run, SettingsRevision: s.Revision, Endpoint: s.Endpoint, Credential: s.Credential,
		Models: slices.Clone(s.Models), Limits: s.Limits, Recording: s.Recording, CreatedAt: now.UTC(),
	}
	return b, b.Validate()
}

// Validate checks a stored or captured binding.
func (b RunBinding) Validate() error {
	if err := b.Scope.Validate(); err != nil {
		return fail(ErrInvalid, "the binding scope is malformed")
	}
	if err := b.RunID.Validate(); err != nil {
		return fail(ErrInvalid, "the binding run is malformed")
	}
	if b.SettingsRevision < 2 || b.Endpoint.IsZero() || len(b.Models) == 0 {
		return fail(ErrInvalid, "the binding is incomplete")
	}
	if err := b.Credential.Validate(); err != nil {
		return err
	}
	return b.Limits.Validate()
}

// Allows reports whether the model is on the run's allow-list.
func (b RunBinding) Allows(model string) bool { return slices.Contains(b.Models, model) }
