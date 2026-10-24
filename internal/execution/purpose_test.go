package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// ---- fakes of the consumer-owned ports ----

type fakeDefs struct {
	defs map[domain.DefinitionID]pipeline.Definition
}

func (f fakeDefs) Get(_ context.Context, _ domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	d, ok := f.defs[id]
	if !ok {
		return pipeline.Definition{}, ErrNotFound
	}
	return d, nil
}

type allowAuthority struct{}

func (allowAuthority) ResolveActor(_ context.Context, _ domain.Scope, p string) (Actor, error) {
	return Actor{Type: ActorUser, ID: p}, nil
}
func (allowAuthority) Authorize(context.Context, Actor, domain.Scope, Permission) error { return nil }

type fakeWork struct{ snap WorkSnapshot }

func (f fakeWork) Snapshot(context.Context, domain.Scope, domain.WorkItemID) (WorkSnapshot, error) {
	return f.snap, nil
}

type emptyConfig struct{}

func (emptyConfig) Resolve(context.Context, ConfigRequest) (ConfigResult, error) {
	return ConfigResult{}, nil
}

type fakeSources struct{ verified int }

func (f *fakeSources) Verify(context.Context, domain.Scope, domain.WorkItemID, string) (SourceInfo, error) {
	f.verified++
	return SourceInfo{WorkspaceID: "ws_1", RepositoryURL: "https://git.example.invalid/r.git", Branch: "refs/heads/w", AcceptedTip: commitA}, nil
}
func (*fakeSources) Describe(context.Context, domain.Scope, string, bool) (SourceInfo, error) {
	return SourceInfo{}, nil
}
func (*fakeSources) Retain(context.Context, domain.Scope, string, domain.RunID, string) (string, error) {
	return "", nil
}

type fixedLimits struct{}

func (fixedLimits) Limits(context.Context, domain.Scope) (Limit, error) { return testLimit(), nil }

func testLimit() Limit {
	return Limit{TenantConcurrency: 5, ProjectConcurrency: 5, StartupBudget: 60e9, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 1 << 20, DefaultTimeout: 600e9, MaxTimeout: 3600e9}
}

type nullStore struct{ Store }

type allowCleanup struct{ err error }

func (a allowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return a.err }

var scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}

func newDefinition(t *testing.T, scope domain.Scope, yaml string) pipeline.Definition {
	t.Helper()
	v := pipelinetest.NewValidator(t, pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate)
	actor := pipeline.Actor{ID: "u1", Type: pipeline.ActorUser}
	validated, err := v.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	def, err := pipeline.NewDefinition(pipeline.Candidate{Scope: scope, ID: domain.DefinitionID("pd_" + validated.Name()), Content: validated, CreatedBy: actor, CreatedAt: t0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func service(t *testing.T, d Dependencies) *Service {
	t.Helper()
	d.Store, d.Authority = nullStore{}, allowAuthority{}
	d.Capabilities, d.Configuration, d.Limits, d.Hooks = pipeline.EnabledKinds{pipeline.KindContainer}, emptyConfig{}, fixedLimits{}, hooks
	d.Clock = func() time.Time { return t0 }
	s, err := NewService(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeEnv is a transaction-less Env that records what a mutation touched.
type fakeEnv struct {
	workCalls, authCalls, writerCalls int
	next                              int
}

func (e *fakeEnv) Now() time.Time { return t0 }
func (e *fakeEnv) NewID(p string) string {
	e.next++
	return p + "_" + string(rune('a'+e.next))
}
func (e *fakeEnv) Authorize(context.Context, Actor, domain.Scope, Permission) error {
	e.authCalls++
	return nil
}
func (e *fakeEnv) Work() (WorkAdmission, error)                                 { e.workCalls++; return WorkAdmission{}, nil }
func (e *fakeEnv) CapacityAvailable(context.Context, domain.Scope, Limit) error { return nil }
func (e *fakeEnv) ReserveCapacity(context.Context, *Attempt) error              { return nil }
func (e *fakeEnv) ReleaseCapacity(context.Context, *Attempt) error              { return nil }
func (e *fakeEnv) ReserveWriter(context.Context, *Attempt) error                { e.writerCalls++; return nil }
func (e *fakeEnv) SettleWriter(context.Context, *Attempt, WriterOutcome, string) error {
	return nil
}
func (e *fakeEnv) ReleaseSecrets(context.Context, *Run) error { return nil }

// Test_purpose_and_disabled_hooks: PX-A09, PX-09, PX-13, M2 seams. Public
// cleanup admission is impossible, deployment-bound internal admission has its
// requirements, a cleanup run has no workspace or work item, and disabled agent
// and gate hooks never claim Jobs.
func Test_purpose_and_disabled_hooks(t *testing.T) {
	ctx := context.Background()
	defs := fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{}}
	clean := newDefinition(t, scope, pipelinetest.DocNamed("clean", "{}", "{id: teardown, kind: container, image: "+pipelinetest.Image+", command: [/bin/true], timeoutSeconds: 60, workspace: {access: none}}"))
	reads := newDefinition(t, scope, pipelinetest.DocNamed("reads", "{}", pipelinetest.Container("r", "workspace: {access: read, from: run}")))
	for _, d := range []pipeline.Definition{clean, reads} {
		defs.defs[d.ID] = d
	}
	src := &fakeSources{}
	svc := service(t, Dependencies{Definitions: defs, Work: fakeWork{WorkSnapshot{ID: "wi_1", Actionable: true, ContentRevision: 1, Spec: "s"}}, Sources: src, Cleanup: allowCleanup{}})

	t.Run("the public admission path creates only work runs", func(t *testing.T) {
		plan, err := svc.Prepare(ctx, Caller{PrincipalID: "u1"}, AdmitCommand{Scope: scope, WorkItemID: "wi_1", DefinitionID: clean.ID, ContentRevision: 1, SourceCommit: commitA})
		if err != nil {
			t.Fatal(err)
		}
		r := plan.Aggregate.Run
		if r.Purpose != PurposeWork || r.WorkItemID == "" || r.WorkspaceID == "" || r.SourceRetention != RetentionPending || r.DeploymentID != "" {
			t.Fatalf("public admission produced %+v", r)
		}
		// AdmitCommand has no purpose, deployment or workspace field: there is no
		// way for a caller to request cleanup through this path.
	})

	t.Run("cleanup admission needs the deployment-bound authorization", func(t *testing.T) {
		m1 := service(t, Dependencies{Definitions: defs, Work: fakeWork{}, Sources: src}) // no Cleanup port: the M1 wiring
		if _, err := m1.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID}); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("M1 wiring: %v", err)
		}
		if _, err := m1.AdmitCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID}); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("M1 wiring admit: %v", err)
		}
		deny := service(t, Dependencies{Definitions: defs, Work: fakeWork{}, Sources: src, Cleanup: allowCleanup{err: ErrDenied}})
		if _, err := deny.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID}); !errors.Is(err, ErrDenied) {
			t.Fatalf("denied deployment: %v", err)
		}
		outage := service(t, Dependencies{Definitions: defs, Work: fakeWork{}, Sources: src, Cleanup: allowCleanup{err: errors.New("deployments down")}})
		if _, err := outage.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("authorization outage: %v", err)
		}
		for name, cmd := range map[string]CleanupCommand{
			"no deployment":     {Scope: scope, DefinitionID: clean.ID},
			"bad deployment ID": {Scope: scope, DeploymentID: "not valid!", DefinitionID: clean.ID},
			"no definition":     {Scope: scope, DeploymentID: "dep_1"},
			"invalid scope":     {DeploymentID: "dep_1", DefinitionID: clean.ID},
		} {
			if _, err := svc.PrepareCleanup(ctx, cmd); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := svc.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: reads.ID}); !errors.Is(err, ErrAdmissionRefused) {
			t.Fatalf("a cleanup definition that reads the workspace: %v", err)
		}
		if src.verified != 1 { // only the public admission above touched a workspace
			t.Fatalf("%d workspace verifications", src.verified)
		}
	})

	t.Run("a cleanup run has no workspace, work item or WIP", func(t *testing.T) {
		plan, err := svc.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID})
		if err != nil {
			t.Fatal(err)
		}
		r := plan.Aggregate.Run
		if r.Purpose != PurposeCleanup || r.WorkItemID != "" || r.WorkspaceID != "" || r.SourceCommit != "" || r.Specification != nil ||
			r.DeploymentID != "dep_1" || r.SourceRetention != RetentionNotRequired || r.RetainedRefID != "" || r.RequestedBy != System {
			t.Fatalf("cleanup run %+v", r)
		}
		if !r.AcceptsDispatch() {
			t.Fatal("a cleanup run waits for a source retention it does not need")
		}
		// Dispatch: no work item lock (so a done or cancelled item is irrelevant),
		// no user authorization, no workspace reservation, and a context with no
		// work item and no workspace.
		env := &fakeEnv{}
		fx, err := svc.claimMutation("teardown", SourceInfo{}, testLimit())(ctx, env, plan.Aggregate)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := fx.Result.(claimOutcome)
		if out.Intent == nil || out.Intent.Purpose != PurposeCleanup || out.Intent.Access != AccessNone {
			t.Fatalf("intent %+v", out.Intent)
		}
		if env.workCalls != 0 || env.authCalls != 0 || env.writerCalls != 0 || len(fx.After) != 1 {
			t.Fatalf("work=%d auth=%d writer=%d after=%d", env.workCalls, env.authCalls, env.writerCalls, len(fx.After))
		}
		at := plan.Aggregate.Attempts[0]
		ctxDoc, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
		if err != nil {
			t.Fatal(err)
		}
		if ctxDoc.WorkItem != nil || ctxDoc.Workspace.Access != runtimev1.AccessNone || ctxDoc.Workspace.Source != nil {
			t.Fatalf("cleanup context carries a work item or workspace: %+v", ctxDoc.Workspace)
		}
		// A cleanup run not bound to a deployment is never dispatched.
		orphan, _ := svc.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID})
		orphan.Aggregate.Run.DeploymentID = ""
		if _, err := svc.claimMutation("teardown", SourceInfo{}, testLimit())(ctx, &fakeEnv{}, orphan.Aggregate); !errors.Is(err, ErrDenied) {
			t.Fatalf("unbound cleanup claim: %v", err)
		}
	})

	t.Run("a work run dispatch rechecks the work item and the requester", func(t *testing.T) {
		plan, err := svc.Prepare(ctx, Caller{PrincipalID: "u1"}, AdmitCommand{Scope: scope, WorkItemID: "wi_1", DefinitionID: clean.ID, ContentRevision: 1, SourceCommit: commitA})
		if err != nil {
			t.Fatal(err)
		}
		plan.Aggregate.Run.SourceRetention = RetentionConfirmed
		env := &fakeEnv{}
		if _, err := svc.claimMutation("teardown", SourceInfo{RepositoryURL: "https://git.example.invalid/r.git"}, testLimit())(ctx, env, plan.Aggregate); err != nil {
			t.Fatal(err)
		}
		if env.workCalls != 1 || env.authCalls != 1 {
			t.Fatalf("work=%d auth=%d", env.workCalls, env.authCalls)
		}
	})

	t.Run("disabled agent and gate hooks never claim Jobs", func(t *testing.T) {
		agent := pipeline.Step{ID: "implement", Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: "coding-v1"},
			Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}, Retry: &pipeline.Retry{MaxAttempts: 1}}
		agg := newAgg(t, nil, agent, gate("review", "implement"), container("ship", needs("review")))
		env := &fakeEnv{}
		fx, err := svc.claimMutation("implement", SourceInfo{AcceptedTip: commitA, Branch: "refs/heads/w", RepositoryURL: "https://git.example.invalid/r.git"}, testLimit())(ctx, env, agg)
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := fx.Result.(claimOutcome); out.Intent != nil || len(agg.Attempts) != 0 || len(fx.After) != 0 || env.writerCalls != 0 {
			t.Fatalf("a disabled hook claimed work: %+v attempts %d", out.Intent, len(agg.Attempts))
		}
		if agg.Step("implement").Status != StepPending || agg.Run.Status != RunQueued {
			t.Fatalf("the waiting step is %s, run %s", agg.Step("implement").Status, agg.Run.Status)
		}
	})
}
