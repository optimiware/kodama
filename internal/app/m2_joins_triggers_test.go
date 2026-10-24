package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters/ledgertest"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/pipeline"
)

// installerAuthority is the identity authority of the trigger tests: the
// installer is a member with project settings and run management, everyone else
// holds nothing.
type installerAuthority struct{}

func (installerAuthority) Authorize(_ context.Context, actor identity.Actor, _ identity.Target, perm identity.Permission) error {
	if actor.Type == identity.ActorUser && actor.ID == gitwebhooktest.Installer && (perm == identity.PermProjectSettings || perm == identity.PermRunManage) {
		return nil
	}
	return identity.ErrDenied
}

// hookPlanner is the core's run planning behind the trigger: one plan per
// request with a fresh run, recording what it was asked to start.
type hookPlanner struct {
	mu    sync.Mutex
	reqs  []gw.RunRequest
	steps []*execution.StepRun
	err   error
}

func (p *hookPlanner) Plan(_ context.Context, req gw.RunRequest) (*execution.AdmissionPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if p.err != nil {
		return nil, p.err
	}
	return &execution.AdmissionPlan{Aggregate: &execution.Aggregate{
		Run:   &execution.Run{Scope: req.Scope, ID: domain.RunID(fmt.Sprintf("run_hook_%d", len(p.reqs)))},
		Steps: p.steps,
	}}, nil
}

func (p *hookPlanner) requests() []gw.RunRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gw.RunRequest(nil), p.reqs...)
}

type hookWorld struct {
	f       *gitwebhooktest.Fixture
	planner *hookPlanner
	ledger  *ledgertest.Memory
	state   *ledgertest.State
	in      GitTriggerInputs
}

func newHookWorld(t *testing.T, cfg GitTriggerConfig) *hookWorld {
	t.Helper()
	f := gitwebhooktest.NewFixture(t)
	w := &hookWorld{f: f, planner: &hookPlanner{}, state: ledgertest.NewState()}
	w.ledger = ledgertest.NewMemory(w.state)
	w.in = GitTriggerInputs{
		Env: Test, Config: cfg, Store: f.Store, Secrets: f.Secrets, Definitions: f.Defs,
		Authority: installerAuthority{}, Repositories: f.Repos, Git: f.Git, Planner: w.planner, Ledger: w.ledger,
	}
	return w
}

func (w *hookWorld) send(t *testing.T, h http.Handler, trig gw.TriggerID, delivery string, secret []byte, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	ts := time.Now().Unix() // the service checks freshness against the wall clock
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-hooks/"+string(trig), strings.NewReader(string(body)))
	req.Header.Set(gw.HeaderDeliveryID, delivery)
	req.Header.Set(gw.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(gw.HeaderSignature, gw.Sign(secret, trig, delivery, ts, body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Test_app_git_trigger_mounted: `git-webhooks` is registered when the
// installation configured the trigger and reported disabled otherwise.
func Test_app_git_trigger_mounted(t *testing.T) {
	ctx := context.Background()
	exec := func() *ExecutionSlice {
		return &ExecutionSlice{Disabled: []string{"runtime-secret-resolution", "gates", "git-webhooks", "chatops"}}
	}

	t.Run("configured, the hook is mounted behind the process handler and starts a run", func(t *testing.T) {
		w := newHookWorld(t, GitTriggerConfig{RateLimitPerMinute: 100})
		slice, err := NewGitTriggers(w.in)
		if err != nil || slice == nil {
			t.Fatalf("slice %v, %v", slice, err)
		}
		if slice.Pattern != "POST /api/v1/git-hooks/{triggerId}" || len(slice.Routes) != 1 || slice.Routes[0] != "git-webhooks" {
			t.Fatalf("slice %+v", slice)
		}
		if got := DisabledCapabilitiesM2(exec(), nil, M2Slices{Triggers: slice}); contains(got, "git-webhooks") {
			t.Fatalf("a mounted trigger is reported disabled: %v", got)
		}

		// Management goes through the identity authority: a member who may not
		// manage project settings installs nothing.
		if _, err := slice.Service.Install(ctx, gw.InstallCommand{Scope: w.f.Scope, Principal: "user-viewer", DefinitionID: gitwebhooktest.DefinitionName, Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef}); !errors.Is(err, gw.ErrDenied) {
			t.Fatalf("a viewer installed a trigger: %v", err)
		}
		trig, err := slice.Service.Install(ctx, gw.InstallCommand{
			Scope: w.f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: gitwebhooktest.DefinitionName, WorkItemID: "wi_hook",
			Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if err != nil {
			t.Fatal(err)
		}

		c := &Core{Triggers: slice, runtime: http.NotFoundHandler()}
		h := c.Handler()
		body := gitwebhooktest.Marshal(t, w.f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))
		rec := w.send(t, h, trig.ID, "dlv-1", w.f.Secret, body)
		if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"outcome":"started"`) {
			t.Fatalf("a signed push: %d %s", rec.Code, rec.Body.String())
		}
		reqs := w.planner.requests()
		if len(reqs) != 1 || reqs[0].Commit != gitwebhooktest.CommitA || reqs[0].RequestedBy != gitwebhooktest.Installer || reqs[0].WorkItemID != "wi_hook" {
			t.Fatalf("the run was planned as %+v", reqs)
		}
		if w.state.Runs(w.f.Scope) != 1 {
			t.Fatalf("%d runs admitted", w.state.Runs(w.f.Scope))
		}
		// A replay of the delivery is the same run, never a second one.
		if rec := w.send(t, h, trig.ID, "dlv-1", w.f.Secret, body); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
			t.Fatalf("a replayed delivery: %d %s", rec.Code, rec.Body.String())
		}
		if w.state.Runs(w.f.Scope) != 1 {
			t.Fatal("a replayed delivery admitted a second run")
		}
		// A forged signature is refused before anything is recorded or planned.
		if rec := w.send(t, h, trig.ID, "dlv-2", []byte(strings.Repeat("x", 48)), body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a forged signature: %d %s", rec.Code, rec.Body.String())
		}
		if len(w.planner.requests()) != 1 {
			t.Fatal("a forged delivery reached the planner")
		}
		// The installer's authority is re-checked for every delivery: once it is
		// gone the push is refused and starts nothing.
		w.in.Authority = identityNone{}
		revoked, err := NewGitTriggers(w.in)
		if err != nil {
			t.Fatal(err)
		}
		before := len(w.planner.requests())
		rec = w.send(t, (&Core{Triggers: revoked, runtime: http.NotFoundHandler()}).Handler(), trig.ID, "dlv-3", w.f.Secret, body)
		if rec.Code == http.StatusAccepted || len(w.planner.requests()) != before {
			t.Fatalf("a push by a principal without run authority: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("not configured, nothing is built and the capability is reported disabled", func(t *testing.T) {
		w := newHookWorld(t, GitTriggerConfig{})
		// Even with every collaborator missing nothing is required or built.
		for _, in := range []GitTriggerInputs{w.in, {}} {
			slice, err := NewGitTriggers(in)
			if slice != nil || err != nil {
				t.Fatalf("slice %v, %v", slice, err)
			}
		}
		if got := DisabledCapabilitiesM2(exec(), nil, M2Slices{}); !contains(got, "git-webhooks") {
			t.Fatalf("git-webhooks is not reported disabled: %v", got)
		}
		_, reasons := m2JoinReport(nil, JoinState{})
		if reasons[capGitWebhooks] == "" {
			t.Fatalf("no reason: %v", reasons)
		}
		_, reasons = m2JoinReport(nil, JoinState{GitTriggers: true})
		if reasons[capGitWebhooks] != "" {
			t.Fatalf("a mounted trigger has a disabled reason: %v", reasons)
		}
		// Unmounted, the hook path is answered by the deny-by-default router.
		router, err := httpapi.NewRouter(httpapi.RouterConfig{
			Authenticator: runtimeAuthenticator{}, Authorizer: runtimeAuthorizer{}, RateLimiter: newRateLimiter(10, nil), Prefix: APIPrefix,
		})
		if err != nil {
			t.Fatal(err)
		}
		h := (&Core{router: router, runtime: http.NotFoundHandler()}).Handler()
		body := gitwebhooktest.Marshal(t, w.f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))
		if rec := w.send(t, h, w.f.Trigger.ID, "dlv-1", w.f.Secret, body); rec.Code == http.StatusAccepted || rec.Code == http.StatusOK || len(w.planner.requests()) != 0 {
			t.Fatalf("an unconfigured installation took a hook: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a configured trigger with a missing collaborator or limit is a construction error", func(t *testing.T) {
		for name, mutate := range map[string]func(*GitTriggerInputs){
			"no store":        func(in *GitTriggerInputs) { in.Store = nil },
			"no secrets":      func(in *GitTriggerInputs) { in.Secrets = nil },
			"no definitions":  func(in *GitTriggerInputs) { in.Definitions = nil },
			"no authority":    func(in *GitTriggerInputs) { in.Authority = nil },
			"no repositories": func(in *GitTriggerInputs) { in.Repositories = nil },
			"no git reader":   func(in *GitTriggerInputs) { in.Git = nil },
			"no planner":      func(in *GitTriggerInputs) { in.Planner = nil },
			"no ledger":       func(in *GitTriggerInputs) { in.Ledger = nil },
			"no environment":  func(in *GitTriggerInputs) { in.Env = "" },
		} {
			w := newHookWorld(t, GitTriggerConfig{RateLimitPerMinute: 10})
			mutate(&w.in)
			if slice, err := NewGitTriggers(w.in); err == nil || slice != nil {
				t.Errorf("%s: built %v", name, slice)
			}
		}
		w := newHookWorld(t, GitTriggerConfig{RateLimitPerMinute: -1})
		if _, err := NewGitTriggers(w.in); err == nil {
			t.Error("a negative limit was accepted")
		}
		if err := (GitTriggerConfig{RateLimitPerMinute: maxGitHookRateLimit + 1}).Validate(); err == nil {
			t.Error("an unbounded limit was accepted")
		}
		// Production refuses the module's development doubles.
		w = newHookWorld(t, GitTriggerConfig{RateLimitPerMinute: 10})
		w.in.Env = Production
		w.in.Store = devHookStore{w.f.Store}
		if _, err := NewGitTriggers(w.in); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("production composed the trigger doubles: %v", err)
		}
	})

	t.Run("the planned run's agent steps are frozen like the ordinary admission routes'", func(t *testing.T) {
		w := newHookWorld(t, GitTriggerConfig{RateLimitPerMinute: 10})
		resolver := &hookAgents{}
		expander, err := agentsadapters.NewExpander(resolver)
		if err != nil {
			t.Fatal(err)
		}
		w.in.Expander = &expander
		w.planner.steps = []*execution.StepRun{{ID: "agent", Status: execution.StepPending, Spec: pipeline.Step{ID: "agent", Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: "coding@1"}}}}
		slice, err := NewGitTriggers(w.in)
		if err != nil {
			t.Fatal(err)
		}
		trig, err := slice.Service.Install(ctx, gw.InstallCommand{
			Scope: w.f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: gitwebhooktest.DefinitionName, WorkItemID: "wi_hook",
			Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if err != nil {
			t.Fatal(err)
		}
		h := (&Core{Triggers: slice, runtime: http.NotFoundHandler()}).Handler()
		body := gitwebhooktest.Marshal(t, w.f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))

		// A profile that is no longer supported is a permanent refusal of the
		// delivery: no run exists and the refusal is recorded.
		resolver.err = execution.Refusedf("the agent profile is not supported by this installation")
		if rec := w.send(t, h, trig.ID, "dlv-refused", w.f.Secret, body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("an unsupported profile: %d %s", rec.Code, rec.Body.String())
		}
		if w.state.Runs(w.f.Scope) != 0 {
			t.Fatal("a run was admitted with an unresolvable agent step")
		}
		// An outage of the profile store is retryable and decides nothing.
		resolver.err = execution.Unavailablef(errors.New("db"), "down")
		if rec := w.send(t, h, trig.ID, "dlv-outage", w.f.Secret, body); rec.Code < 500 {
			t.Fatalf("a profile outage: %d %s", rec.Code, rec.Body.String())
		}
		if w.state.Runs(w.f.Scope) != 0 {
			t.Fatal("a run was admitted during an outage")
		}
		// Resolved, the agent step is admitted frozen (the resolver's command).
		resolver.err = nil
		if rec := w.send(t, h, trig.ID, "dlv-ok", w.f.Secret, body); rec.Code != http.StatusAccepted {
			t.Fatalf("a resolvable profile: %d %s", rec.Code, rec.Body.String())
		}
		if resolver.calls == 0 || w.state.Runs(w.f.Scope) != 1 {
			t.Fatalf("resolver calls %d, runs %d", resolver.calls, w.state.Runs(w.f.Scope))
		}
	})
}

type identityNone struct{}

func (identityNone) Authorize(context.Context, identity.Actor, identity.Target, identity.Permission) error {
	return identity.ErrDenied
}

// hookAgents resolves every agent step to the same frozen command, or fails.
type hookAgents struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (a *hookAgents) ResolveAgent(context.Context, domain.Scope, pipeline.Step) (execution.AgentExpansion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return execution.AgentExpansion{}, a.err
	}
	return execution.AgentExpansion{
		Image: "registry.invalid/agent@sha256:" + strings.Repeat("1", 64), Command: []string{"/usr/local/bin/wrapper"}, WallSeconds: 600,
		MaxRequests: 10, MaxTokens: 1000,
	}, nil
}
