package app

// The native Git push trigger of the core composition (T-101), mounted behind
// AssembleTriggers with T-100's production adapters.
//
// GitTriggerConfig (git-triggers.json) is the operator's explicit statement
// that the installation receives push hooks: without it nothing is built, the
// hook route stays unregistered (the deny-by-default router answers it) and
// `git-webhooks` is recorded as disabled. NewGitTriggers is a pure composition
// over injected ports so it is unit-tested with the module's doubles; NewCore
// supplies the PostgreSQL store and ledger, the core's run admission and the
// secrets module.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook"
	gwadapters "github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/workspace"
)

// maxGitHookRateLimit bounds the configured flood limit: the signature is the
// only credential of a hook, and the limit is an edge bound, not a quota.
const maxGitHookRateLimit = 100_000

// GitTriggerConfig selects the native Git trigger (git-triggers.json):
//
//	{"rateLimitPerMinute": 120}
//
// RateLimitPerMinute throttles unauthenticated hook traffic per remote host. It
// must be positive: there is no unlimited default, and zero (or no file) leaves
// the trigger unselected.
type GitTriggerConfig struct {
	RateLimitPerMinute int `json:"rateLimitPerMinute"`
}

// Configured reports whether the installation selected the trigger.
func (c GitTriggerConfig) Configured() bool { return c.RateLimitPerMinute > 0 }

// Validate rejects a negative or unbounded limit.
func (c GitTriggerConfig) Validate() error {
	if c.RateLimitPerMinute < 0 || c.RateLimitPerMinute > maxGitHookRateLimit {
		return fmt.Errorf("git triggers: the rate limit must be between 1 and %d per minute", maxGitHookRateLimit)
	}
	return nil
}

// LoadGitTriggers reads the strict JSON document. A file that selects nothing
// (zero limit) is an error: leave the file out to disable the trigger.
func LoadGitTriggers(path string) (GitTriggerConfig, error) {
	var c GitTriggerConfig
	if err := readStrictJSON(path, "git triggers file", &c); err != nil {
		return GitTriggerConfig{}, err
	}
	if !c.Configured() {
		return GitTriggerConfig{}, errors.New("git triggers file: rateLimitPerMinute is required (remove the file to disable the trigger)")
	}
	return c, c.Validate()
}

// GitTriggerInputs are the collaborators of the trigger.
type GitTriggerInputs struct {
	Env    Env
	Log    *slog.Logger
	Config GitTriggerConfig

	// Store, Secrets and Definitions are the trigger's persistence, hook secret
	// and pipeline definition ports.
	Store       gitwebhook.Store
	Secrets     gitwebhook.Secrets
	Definitions gitwebhook.Definitions
	// Authority authorizes the installing principal and the run (T-101's
	// TriggerAuthorizer over the identity authority).
	Authority identity.Authority
	// Repositories is the project's repository binding and Git the read-only ref
	// reader.
	Repositories workspace.Repositories
	Git          workspace.RefReader
	// Planner and Ledger admit a verified push as a run (gitwebhook adapters).
	Planner gwadapters.Planner
	Ledger  gwadapters.Ledger
	// Expander, when set, freezes the agent steps of a planned run exactly as the
	// ordinary admission routes do. It is required when the installation can run
	// agent steps (the planner returns a plan that must be expanded before it
	// becomes durable).
	Expander *agentsadapters.Expander
	// Adapters lists the adapters below the service for the construction guard.
	Adapters []any
}

// NewGitTriggers builds the trigger slice, or (nil, nil) when the installation
// did not select it: the capability stays disabled and nothing is mounted.
func NewGitTriggers(in GitTriggerInputs) (*TriggerSlice, error) {
	if err := in.Config.Validate(); err != nil {
		return nil, err
	}
	if !in.Config.Configured() {
		return nil, nil
	}
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	switch {
	case !in.Env.Valid():
		return nil, ErrEnvUnknown
	case in.Store == nil, in.Secrets == nil, in.Definitions == nil:
		return nil, missing("the trigger store, hook secrets and definitions")
	case in.Authority == nil:
		return nil, missing("the identity authority of the trigger")
	case in.Repositories == nil, in.Git == nil:
		return nil, missing("the repository binding and the Git reader")
	case in.Planner == nil, in.Ledger == nil:
		return nil, missing("the run planner and the admission ledger")
	}
	authorizer, err := NewTriggerAuthorizer(in.Authority)
	if err != nil {
		return nil, err
	}
	var planner gwadapters.Planner = in.Planner
	if in.Expander != nil {
		planner = expandingPlanner{inner: in.Planner, expander: *in.Expander}
	}
	runs, err := gwadapters.NewRuns(planner, in.Ledger)
	if err != nil {
		return nil, err
	}
	svc, err := gitwebhook.New(gitwebhook.Dependencies{
		Store: in.Store, Authorizer: authorizer, Secrets: in.Secrets, Definitions: in.Definitions,
		Repositories: in.Repositories, Git: in.Git, Runs: runs,
	})
	if err != nil {
		return nil, err
	}
	adapters := append([]any{in.Store, in.Secrets, in.Definitions, in.Repositories, in.Git, in.Planner, in.Ledger}, in.Adapters...)
	return AssembleTriggers(TriggerInputs{Env: in.Env, Log: in.Log, Service: svc, Adapters: adapters, RateLimitPerMinute: in.Config.RateLimitPerMinute})
}

// expandingPlanner freezes the agent steps of a planned run before the ledger
// admits it, through the one expansion path of the ordinary admission routes
// (agentsadapters.Expander). A refusal of the profile (unknown, unsupported,
// no longer approved) is a permanent run refusal of the delivery; an outage is
// retryable.
type expandingPlanner struct {
	inner    gwadapters.Planner
	expander agentsadapters.Expander
}

func (p expandingPlanner) Plan(ctx context.Context, req gitwebhook.RunRequest) (*execution.AdmissionPlan, error) {
	plan, err := p.inner.Plan(ctx, req)
	if err != nil {
		return nil, err
	}
	expanded, err := p.expander.Expand(ctx, plan)
	if err != nil {
		return nil, gwadapters.ClassifyAdmission(err)
	}
	return expanded, nil
}
