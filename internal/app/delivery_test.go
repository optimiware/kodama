package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	agentshttp "github.com/optimiware/kodama/internal/agents/http"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	"github.com/optimiware/kodama/internal/gates"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// ---- doubles of the delivery ports of execution ----------------------------------

// dvBoundary is delivery's cleanup authorization, retained view and snapshot
// resolution: only the cleanup authorization answers, from the record table.
type dvBoundary struct{ rec *dvRecords }

func (dvBoundary) DevelopmentOnly() bool { return true }
func (b dvBoundary) AuthorizeCleanup(ctx context.Context, scope domain.Scope, id string) error {
	if err := b.rec.AuthorizeCleanup(ctx, scope, id); err != nil {
		return delivery.Fail(delivery.ErrDenied, err, "denied")
	}
	return nil
}
func (dvBoundary) RetainedView(context.Context, domain.Scope, delivery.DeploymentID) (delivery.CleanupView, error) {
	return delivery.CleanupView{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}
func (dvBoundary) ResolveSnapshot(context.Context, domain.Scope, delivery.Actor, domain.EnvironmentID) (delivery.ConfigurationSnapshot, error) {
	return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

// dvBindings is the cleanup-run reader: run -> deployment.
type dvBindings struct {
	mu  sync.Mutex
	m   map[domain.RunID]delivery.DeploymentID
	err error
}

func (*dvBindings) DevelopmentOnly() bool { return true }
func (b *dvBindings) Binding(_ context.Context, _ domain.Scope, run domain.RunID) (delivery.DeploymentID, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return "", false, b.err
	}
	id, ok := b.m[run]
	return id, ok, nil
}

// dvRuns is the execution store seen by the effect reconciler: runs by identity.
type dvRuns struct {
	execution.Store
	aggs map[domain.RunID]*execution.Aggregate
	err  error
}

func (*dvRuns) DevelopmentOnly() bool { return true }
func (r *dvRuns) Get(_ context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	if r.err != nil {
		return nil, r.err
	}
	a, ok := r.aggs[id]
	if !ok || a.Run.Scope != scope {
		return nil, execution.ErrNotFound
	}
	return a, nil
}

func dvAggregate(scope domain.Scope, run domain.RunID, steps ...pipeline.Step) *execution.Aggregate {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	agg := &execution.Aggregate{Run: &execution.Run{Scope: scope, ID: run}}
	for i, s := range steps {
		agg.Steps = append(agg.Steps, execution.NewStepRun(run, i, s, now))
	}
	return agg
}

var (
	dvNow    = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	dvAction = "act:0123456789abcdef"
)

func dvDeployment(t testing.TB, scope domain.Scope, run domain.RunID, step string, states ...func(*delivery.Deployment) error) delivery.Deployment {
	t.Helper()
	exp := dvNow.Add(24 * time.Hour)
	d, err := delivery.NewDeployment(delivery.Registration{
		Scope: scope, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: "preview",
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      run, Step: step, ActionKey: dvAction, Attempt: "att_deploy", ExpiresAt: &exp,
	}, dvNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if err := s(&d); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

const (
	dvToolImage  = "registry.example.invalid/tools@sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	dvOtherImage = "registry.example.invalid/other@sha256:" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func dvTools() []ToolQualification {
	return []ToolQualification{
		{Protocol: ToolApply, Image: dvToolImage, Command: []string{"/opt/kodama/apply.sh"}},
		{Protocol: ToolCleanup, Image: dvToolImage, Command: []string{"/opt/kodama/delete.sh"}},
	}
}

func TestToolQualifications(t *testing.T) {
	good := dvTools()
	if _, err := NewToolQualifications(good); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]ToolQualification{
		"unknown protocol": {Protocol: "x", Image: dvToolImage, Command: []string{"/a"}},
		"unpinned image":   {Protocol: ToolApply, Image: "registry.example.invalid/tools:latest", Command: []string{"/a"}},
		"no command":       {Protocol: ToolApply, Image: dvToolImage},
	} {
		if _, err := NewToolQualifications([]ToolQualification{q}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	tq, _ := NewToolQualifications(append(good, ToolQualification{Protocol: ToolApply, Image: dvOtherImage, Command: []string{"/x"}, Args: []string{"--strict"}}))
	step := func(img string, cmd, args []string) pipeline.Step {
		return pipeline.Step{Kind: pipeline.KindContainer, Image: img, Command: cmd, Args: args}
	}
	if !tq.Qualifies(ToolApply, step(dvToolImage, []string{"/opt/kodama/apply.sh"}, []string{"anything"})) {
		t.Error("a qualified script with its own arguments")
	}
	if tq.Qualifies(ToolCleanup, step(dvToolImage, []string{"/opt/kodama/apply.sh"}, nil)) || tq.Qualifies(ToolApply, step(dvToolImage, []string{"/opt/kodama/apply.sh", "x"}, nil)) {
		t.Error("another protocol or command qualified")
	}
	if tq.Qualifies(ToolApply, step(dvOtherImage, []string{"/x"}, []string{"--loose"})) || !tq.Qualifies(ToolApply, step(dvOtherImage, []string{"/x"}, []string{"--strict"})) {
		t.Error("qualified arguments are matched exactly")
	}
	if (ToolQualifications{}).Qualifies(ToolApply, step(dvToolImage, []string{"/opt/kodama/apply.sh"}, nil)) {
		t.Error("an empty set qualified a step")
	}
	agent := step(dvToolImage, []string{"/opt/kodama/apply.sh"}, nil)
	agent.Kind = pipeline.KindAgent
	if tq.Qualifies(ToolApply, agent) {
		t.Error("a non-container step qualified")
	}
}

func dvApplyWithObject(d *delivery.Deployment) error {
	_, err := d.BeginApply([]delivery.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "preview", Name: "web"}}, dvNow)
	return err
}

func dvSucceeded(d *delivery.Deployment) error {
	obj := delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "preview", Name: "web", UID: "00000000-0000-4000-8000-000000000001", Ready: true}
	if _, err := d.BeginApply([]delivery.ObjectRef{{APIVersion: obj.APIVersion, Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name}}, dvNow); err != nil {
		return err
	}
	_, err := d.Settle(delivery.Settlement{Succeeded: true, Objects: []delivery.ObjectRef{obj}}, dvNow)
	return err
}

func dvRequestCleanup(d *delivery.Deployment) error { _, err := d.RequestCleanup(dvNow); return err }

// ---- agent_delivery_wiring ----------------------------------------------------------

// fakeResolver is an environment resolver; AssembleExecution must refuse it
// over a store that cannot bind environments.
type fakeResolver struct{}

func (fakeResolver) ResolveEnvironment(context.Context, execution.EnvironmentRequest) (execution.EnvironmentSnapshot, error) {
	return execution.EnvironmentSnapshot{}, execution.ErrNotFound
}

// noCreator is the keyed run creator of the HTTP foundation: not exercised.
type noCreator struct{}

func (noCreator) CreateKeyed(context.Context, httpapi.RequestIdentity, func(context.Context, httpapi.Reference) error,
	*execution.AdmissionPlan, func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error) {
	return httpapi.StoredResponse{}, false, nil, errors.New("not used")
}

var _ executionhttp.Creator = noCreator{}

const agentStep = "{id: implement, kind: agent, agent: {profile: %s}, timeoutSeconds: 600, workspace: {access: none}, outputs: {summary: {type: string}}}"

// agentWorld is an execution fixture assembled with the agent chain: the
// expanding store, the planner decoration, the step kinds and the keyed creator
// bound together as the one documented bundle.
type agentWorld struct {
	*fixture
	svc     *agents.Service
	draft   agents.Draft
	profile agents.ProfileVersion
	drv     executiontest.Driver
	validat *pipeline.Validator
}

func newAgentWorld(t *testing.T) *agentWorld {
	t.Helper()
	w := &agentWorld{}
	repo := &agentstest.MemRepo{}
	w.draft = agentstest.Draft(t, "coding")
	auth := agentstest.NewAuthority()
	auth.Grant(fxScope, executiontest.Engineer, agents.ActorUser, agents.PermRegister, agents.PermList)
	svc, err := agents.NewService(agents.ServiceConfig{
		Repository: repo, Authority: auth, Compatibility: agentstest.Catalogue(t, w.draft), Approvals: agentstest.NewApprovals(),
		Clock: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	res, err := svc.Register(context.Background(), agents.RegisterCommand{Scope: fxScope, Caller: agents.Caller{PrincipalID: executiontest.Engineer}, Draft: w.draft})
	if err != nil {
		t.Fatal(err)
	}
	w.profile = res.Profile

	w.fixture = fxBuild(t, Test, func(f *fixture) {
		resolver, err := agentsadapters.NewResolver(svc)
		if err != nil {
			t.Fatal(err)
		}
		lookup, err := agentsadapters.NewLookup(svc)
		if err != nil {
			t.Fatal(err)
		}
		chain, err := agentsadapters.NewChain(agentsadapters.ChainConfig{Resolver: resolver, Store: f.plain, Creator: noCreator{}})
		if err != nil {
			t.Fatal(err)
		}
		f.in.Plain = Decorated(chain.Store, f.plain)
		f.in.AgentPlanner = chain.Planner
		f.in.Deps.Hooks = chain.Hooks
		f.in.Deps.Capabilities = chain.Capabilities(pipeline.KindGate)
		w.validat, err = pipeline.NewValidator(pipeline.Config{
			Capabilities: chain.Capabilities(pipeline.KindGate), Profiles: lookup, References: pipelinetest.References{},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	slice, err := AssembleExecution(w.in)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	w.slice = slice
	w.drv = executiontest.Driver{T: t, Svc: slice.Trusted, Store: w.m.Mem, Rev: w.m.ContentRevision}
	return w
}

// define registers a definition validated against the real profile lookup.
func (w *agentWorld) define(t *testing.T, steps ...string) (pipeline.Definition, error) {
	t.Helper()
	actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
	v, err := w.validat.ValidateYAML(context.Background(), fxScope, actor, []byte(pipelinetest.Doc(steps...)))
	if err != nil {
		return pipeline.Definition{}, err
	}
	def, _, err := w.m.Defs.RegisterOrGet(context.Background(), pipeline.Candidate{
		Scope: fxScope, ID: domain.DefinitionID(w.m.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: w.m.Clock.Now(),
	})
	return def, err
}

func agentStepFor(ref string) string { return strings.Replace(agentStep, "%s", ref, 1) }

func TestAgentDeliveryWiring(t *testing.T) {
	t.Run("the delivery ports are bound from the environment's adapters", func(t *testing.T) {
		now := func() time.Time { return dvNow }
		rec := newDVRecords(now)
		rec.add(fxScope, "dep_1", time.Hour)
		in := DeliveryPortInputs{
			Env: Test, Delivery: dvBoundary{rec}, Records: newDVStore(), Runs: &dvRuns{}, Bindings: &dvBindings{},
		}
		ports, err := BindDelivery(in)
		if err != nil {
			t.Fatal(err)
		}
		if ports.Cleanup == nil || ports.EffectReconciler == nil {
			t.Fatalf("the cleanup authorization and the effect reconciler are the M2 bindings: %+v", ports)
		}
		if ports.Environments != nil || ports.CleanupEnvironments != nil {
			t.Fatal("a store that cannot bind environments must not receive an environment resolver")
		}
		in.AdmitsEnvironments = true
		if ports, err = BindDelivery(in); err != nil || ports.Environments == nil || ports.CleanupEnvironments == nil {
			t.Fatalf("a binding store gets both environment ports: %+v %v", ports, err)
		}

		// A cleanup run is authorized only for a recorded deployment whose cleanup was requested.
		ctx := context.Background()
		if err := ports.Cleanup.AuthorizeCleanup(ctx, fxScope, "dep_1"); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("cleanup that was not requested: %v", err)
		}
		if err := rec.RequestCleanup(ctx, fxScope, "dep_1"); err != nil {
			t.Fatal(err)
		}
		if err := ports.Cleanup.AuthorizeCleanup(ctx, fxScope, "dep_1"); err != nil {
			t.Fatalf("requested cleanup: %v", err)
		}
		if err := ports.Cleanup.AuthorizeCleanup(ctx, executiontest.ScopeB, "dep_1"); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("a foreign scope: %v", err)
		}

		for name, mutate := range map[string]func(*DeliveryPortInputs){
			"no delivery service": func(i *DeliveryPortInputs) { i.Delivery = nil },
			"no records":          func(i *DeliveryPortInputs) { i.Records = nil },
			"no execution store":  func(i *DeliveryPortInputs) { i.Runs = nil },
			"no bindings":         func(i *DeliveryPortInputs) { i.Bindings = nil },
		} {
			bad := in
			mutate(&bad)
			if _, err := BindDelivery(bad); !errors.Is(err, execution.ErrNotConfigured) {
				t.Errorf("%s: %v", name, err)
			}
		}
		bad := in
		bad.Env = ""
		if _, err := BindDelivery(bad); !errors.Is(err, ErrEnvUnknown) {
			t.Errorf("an unknown environment: %v", err)
		}
		bad.Env = Production
		if _, err := BindDelivery(bad); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("production accepted development doubles: %v", err)
		}
	})

	t.Run("the effect reconciler speaks only for qualified delivery scripts", func(t *testing.T) {
		ctx := context.Background()
		const run, cleanupRun = domain.RunID("run_deploy"), domain.RunID("run_cleanup")
		store := newDVStore()
		runs := &dvRuns{aggs: map[domain.RunID]*execution.Aggregate{
			run: dvAggregate(fxScope, run,
				pipeline.Step{ID: "build", Kind: pipeline.KindContainer, Image: dvToolImage, Command: []string{"/bin/build"}},
				// qualified apply script, without and with a declared environment
				pipeline.Step{ID: "deploy", Kind: pipeline.KindContainer, Environment: "env_1", Image: dvToolImage, Command: []string{"/opt/kodama/apply.sh"}},
				pipeline.Step{ID: "noenv", Kind: pipeline.KindContainer, Image: dvToolImage, Command: []string{"/opt/kodama/apply.sh"}},
				// an arbitrary command bound to an environment, and a look-alike entrypoint of another image
				pipeline.Step{ID: "arbitrary", Kind: pipeline.KindContainer, Environment: "env_1", Image: dvToolImage, Command: []string{"/bin/sh", "-ec"}},
				pipeline.Step{ID: "otherimg", Kind: pipeline.KindContainer, Environment: "env_1", Image: dvOtherImage, Command: []string{"/opt/kodama/apply.sh"}},
				// the cleanup protocol is not the apply protocol
				pipeline.Step{ID: "wrongproto", Kind: pipeline.KindContainer, Environment: "env_1", Image: dvToolImage, Command: []string{"/opt/kodama/delete.sh"}}),
			cleanupRun: dvAggregate(fxScope, cleanupRun,
				pipeline.Step{ID: "cleanup", Kind: pipeline.KindContainer, Image: dvToolImage, Command: []string{"/opt/kodama/delete.sh"}},
				pipeline.Step{ID: "notify", Kind: pipeline.KindContainer, Image: dvToolImage, Command: []string{"/bin/notify"}},
				pipeline.Step{ID: "applyincleanup", Kind: pipeline.KindContainer, Image: dvToolImage, Command: []string{"/opt/kodama/apply.sh"}}),
		}}
		bindings := &dvBindings{m: map[domain.RunID]delivery.DeploymentID{cleanupRun: delivery.DeploymentIDFor(fxScope, dvAction)}}
		rec := newDVRecords(func() time.Time { return dvNow })
		ports, err := BindDelivery(DeliveryPortInputs{Env: Test, Delivery: dvBoundary{rec}, Records: store, Runs: runs, Bindings: bindings, Tools: dvTools()})
		if err != nil {
			t.Fatal(err)
		}
		q := func(run domain.RunID, step string) execution.EffectQuery {
			return execution.EffectQuery{Scope: fxScope, RunID: run, StepID: step, AttemptID: "att_1", ActionKey: dvAction}
		}
		verdict := func(q execution.EffectQuery) (execution.Verdict, error) {
			ev, err := ports.EffectReconciler.Reconcile(ctx, q)
			return ev.Verdict, err
		}

		// A general command is never replayed on the strength of an absent record.
		if v, err := verdict(q(run, "build")); err != nil || v != execution.VerdictStillUnknown {
			t.Fatalf("a general command: %v %v", v, err)
		}
		// An arbitrary command that merely declares an environment says nothing
		// about what it did: no record is not "nothing was applied" for it.
		for _, step := range []string{"arbitrary", "otherimg", "wrongproto", "noenv"} {
			if v, err := verdict(q(run, step)); err != nil || v != execution.VerdictStillUnknown {
				t.Fatalf("%s without a qualified apply script: %v %v", step, v, err)
			}
		}
		// A qualified apply script with no record applied nothing: replaying it is safe.
		if v, err := verdict(q(run, "deploy")); err != nil || v != execution.VerdictReplaySafe {
			t.Fatalf("a qualified deployment step without a record: %v %v", v, err)
		}
		// An applying record that names no object cannot be observed.
		store.put(dvDeployment(t, fxScope, run, "deploy", func(d *delivery.Deployment) error {
			rec := d.Record()
			rec.Status, rec.Reason = delivery.StatusRunning, delivery.ReasonApplying
			nd, err := delivery.RestoreDeployment(rec)
			*d = nd
			return err
		}))
		if v, err := verdict(q(run, "deploy")); err != nil || v != execution.VerdictStillUnknown {
			t.Fatalf("an applying record without objects: %v %v", v, err)
		}
		store.put(dvDeployment(t, fxScope, run, "deploy", dvApplyWithObject))
		if v, err := verdict(q(run, "deploy")); err != nil || v != execution.VerdictReplaySafe {
			t.Fatalf("an applying record with objects: %v %v", v, err)
		}
		// A cleanup run takes its evidence from the deployment it is bound to.
		store.put(dvDeployment(t, fxScope, "run_other", "deploy"))
		if v, err := verdict(q(cleanupRun, "cleanup")); err != nil || v != execution.VerdictStillUnknown {
			t.Fatalf("cleanup before it was requested: %v %v", v, err)
		}
		store.put(dvDeployment(t, fxScope, "run_other", "deploy", dvSucceeded, dvRequestCleanup))
		if v, err := verdict(q(cleanupRun, "cleanup")); err != nil || v != execution.VerdictReplaySafe {
			t.Fatalf("requested cleanup: %v %v", v, err)
		}
		// Another step of the cleanup run is not the cleanup script: the request
		// of the deployment says nothing about what it did. The same holds for the
		// apply script and an unknown step.
		for _, step := range []string{"notify", "applyincleanup", "missing"} {
			if v, err := verdict(q(cleanupRun, step)); err != nil || v != execution.VerdictStillUnknown {
				t.Fatalf("cleanup run step %s: %v %v", step, v, err)
			}
		}
		// Without any qualification nothing is resolved from a record.
		bare, err := BindDelivery(DeliveryPortInputs{Env: Test, Delivery: dvBoundary{rec}, Records: store, Runs: runs, Bindings: bindings})
		if err != nil {
			t.Fatal(err)
		}
		for _, qq := range []execution.EffectQuery{q(run, "deploy"), q(cleanupRun, "cleanup")} {
			if ev, err := bare.EffectReconciler.Reconcile(ctx, qq); err != nil || ev.Verdict != execution.VerdictStillUnknown {
				t.Fatalf("an unqualified installation resolved %s: %v %v", qq.StepID, ev, err)
			}
		}
		// An unknown run proves nothing; an outage is an error, never a verdict.
		if v, err := verdict(q("run_missing", "deploy")); err != nil || v != execution.VerdictStillUnknown {
			t.Fatalf("an unknown run: %v %v", v, err)
		}
		runs.err = errors.New("database down")
		if _, err := verdict(q(run, "deploy")); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a run outage: %v", err)
		}
		runs.err = nil
		bindings.err = errors.New("database down")
		if _, err := verdict(q(run, "deploy")); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a binding outage: %v", err)
		}
		bindings.err = nil
		if _, err := verdict(execution.EffectQuery{Scope: fxScope, RunID: "bad id"}); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("an invalid query: %v", err)
		}
	})

	t.Run("execution refuses an environment resolver its store cannot bind", func(t *testing.T) {
		f := fxBuild(t, Test, func(f *fixture) { f.in.Environments = fakeResolver{} })
		if _, err := AssembleExecution(f.in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a resolver over a store without the transactional binding: %v", err)
		}
	})

	t.Run("a decorated store keeps the capabilities of the store it wraps", func(t *testing.T) {
		m := executiontest.NewMem(t)
		wrapped := Decorated(&fxStore{Store: m.Mem}, m.Mem)
		if !IsDevelopmentAdapter(wrapped) {
			t.Fatal("a decoration hid the development flag of a memory store: production would accept it")
		}
		if admitsEnvironments(wrapped) {
			t.Fatal("a store that does not bind environments was reported as binding them")
		}
		if _, err := Admit(Production, PortStore, wrapped); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted a decorated memory store: %v", err)
		}
	})

	t.Run("an agent step is frozen from its pinned profile and runs deterministically", func(t *testing.T) {
		w := newAgentWorld(t)
		def, err := w.define(t, agentStepFor(w.profile.Ref()))
		if err != nil {
			t.Fatal(err)
		}
		admit := func() (executiontest.Project, *execution.Run) {
			p := w.m.NewProject(fxScope)
			run, err := w.drv.Admit(p, def, nil)
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			return p, run
		}
		_, first := admit()
		_, second := admit()

		stepOf := func(run *execution.Run) pipeline.Step { return w.m.Get(run).Steps[0].Spec }
		a, b := stepOf(first), stepOf(second)
		if a.Image != w.draft.Image || !slices.Equal(a.Command, w.draft.Wrapper.Command) {
			t.Fatalf("the step was not frozen as the profile's harness command: %s %v", a.Image, a.Command)
		}
		if !slices.Equal(a.Args, b.Args) || a.Image != b.Image {
			t.Fatal("the same profile expanded twice must freeze identical steps")
		}
		if _, present, err := agents.DecodeArgs(a.Args); err != nil || !present {
			t.Fatalf("the frozen payload is not the agent wrapper's: %v %v", present, err)
		}

		// Dispatch claims the attempt through the agent-aware planner: the profile
		// and the design baseline are pins of the attempt's immutable context.
		first1 := w.drv.MustClaim(fxScope.TenantID)
		pinned, err := runtimev1.DecodeRequest[runtimev1.Context](first1.Context)
		if err != nil {
			t.Fatal(err)
		}
		if pinned.Profile == nil || pinned.DesignBaseline == nil || pinned.Profile.Name != "coding" || pinned.Profile.Version != 1 {
			t.Fatalf("the attempt context does not pin its profile and baseline: %+v", pinned)
		}
		w.drv.Start(first1)

		// The scripted results go through the guarded result path. Prose or a bare
		// exit never completes an agent step (AR-06): the attempt fails; the
		// structured completion with the declared output succeeds.
		zero := int64(0)
		submit := func(in *execution.AttemptSpec, res runtimev1.Result) {
			t.Helper()
			hash, err := runtimev1.ResultHash(res)
			if err != nil {
				t.Fatal(err)
			}
			rec := w.do("POST", "/runtime/v1/attempts/"+string(in.AttemptID)+"/result", "token-"+string(in.AttemptID), runtimev1.ResultRequest{ResultHash: hash, Result: res})
			if rec.Code != http.StatusOK {
				t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
			}
		}
		bare := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		submit(first1, bare)
		if st := w.m.Get(first).Attempts[0].Status; st == execution.AttemptSucceeded {
			t.Fatalf("a completion without the declared output succeeded: %s", st)
		}

		second1 := w.drv.MustClaim(fxScope.TenantID)
		if second1.RunID != second.ID {
			t.Fatalf("the next claim is %s, want the second run", second1.RunID)
		}
		w.drv.Start(second1)
		done := bare
		done.Outputs = map[string]runtimev1.Value{"summary": runtimev1.StringValue("scripted answer")}
		submit(second1, done)
		if st := w.m.Get(second).Attempts[0].Status; st != execution.AttemptSucceeded {
			t.Fatalf("attempt %s", st)
		}
	})

	t.Run("admission refuses what the profile owner did not resolve", func(t *testing.T) {
		w := newAgentWorld(t)
		p := w.m.NewProject(fxScope)

		// A container step that opens with the reserved payload marker would claim
		// a profile and baseline it never resolved.
		def := w.m.Define(fxScope, pipelinetest.Doc(pipelinetest.Container("build")))
		args := func() []string {
			d, err := w.define(t, agentStepFor(w.profile.Ref()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := w.drv.Admit(w.m.NewProject(fxScope), d, nil)
			if err != nil {
				t.Fatal(err)
			}
			return w.m.Get(run).Steps[0].Spec.Args
		}()
		forged := w.m.Define(fxScope, pipelinetest.Doc(pipelinetest.Container("build", "args: ["+quoteAll(args)+"]")))
		if _, err := w.drv.Admit(p, forged, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a container step carrying the agent payload: %v", err)
		}
		if _, err := w.drv.Admit(p, def, nil); err != nil {
			t.Fatalf("an ordinary container step: %v", err)
		}

		// An unknown profile cannot even be defined; an alias is not immutable.
		if _, err := w.define(t, agentStepFor("missing@1")); err == nil {
			t.Fatal("a definition named an unknown profile")
		}
		if _, err := w.define(t, agentStepFor("coding")); err == nil {
			t.Fatal("a definition named a mutable profile alias")
		}
	})

	t.Run("without the agent chain the M1 behavior is unchanged", func(t *testing.T) {
		f := newFixture(t, Test)
		p := f.m.NewProject(fxScope)
		actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
		doc := pipelinetest.Doc("{id: implement, kind: agent, agent: {profile: coding-v1}, timeoutSeconds: 60, workspace: {access: none}}")
		v, err := f.m.Validator.ValidateYAML(context.Background(), fxScope, actor, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		def, _, err := f.m.Defs.RegisterOrGet(context.Background(), pipeline.Candidate{
			Scope: fxScope, ID: domain.DefinitionID(f.m.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: f.m.Clock.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer}, execution.AdmitCommand{
			Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
		})
		if err == nil {
			t.Fatal("the M1 container-only composition admitted an agent step")
		}
	})

	t.Run("the deployment callbacks are served on the runtime API only where delivery is wired", func(t *testing.T) {
		issue := func(f *fixture) string {
			now := time.Now()
			tok, err := f.in.Auth.Tokens.Issue(runtimeauth.IssueRequest{
				Tenant: fxScope.TenantID, Session: "ses_deploy0123456789abcdefg", Generation: f.in.Auth.Tokens.Generation(), IssuedAt: now, ExpiresAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}
		const path = "/runtime/v1/attempts/att_deploy1/deployments/by-action/act:0123456789abcdef"

		unwired := newFixture(t, Test)
		if rec := unwired.do("GET", path, issue(unwired), nil); rec.Code != http.StatusNotFound {
			t.Fatalf("an unwired delivery answered %d: %s", rec.Code, rec.Body.String())
		}
		if contains(unwired.slice.Routes, "deployment-callbacks") {
			t.Fatal("the callbacks are declared though nothing serves them")
		}

		svc := &recordingRuntime{}
		f := fxBuild(t, Test, func(f *fixture) { f.in.Deployments = svc })
		slice, err := AssembleExecution(f.in)
		if err != nil {
			t.Fatal(err)
		}
		f.slice = slice
		if !contains(slice.Routes, "deployment-callbacks") {
			t.Fatalf("the callbacks are served and not declared: %v", slice.Routes)
		}
		// No credential, and a user's token, never reach the service.
		if rec := f.do("GET", path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("no bearer: %d", rec.Code)
		}
		if rec := f.do("GET", path, "a-user-token", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a foreign bearer: %d", rec.Code)
		}
		if svc.calls.Load() != 0 {
			t.Fatal("an unauthenticated request reached the delivery service")
		}
		// A runtime token reaches it with the route's attempt and the bearer, which
		// the service authenticates again for deployment.call.
		tok := issue(f)
		rec := f.do("GET", path, tok, nil)
		if rec.Code != http.StatusNotFound || svc.calls.Load() != 1 || svc.attempt != "att_deploy1" || svc.bearer != tok {
			t.Fatalf("%d %s (calls %d, attempt %q)", rec.Code, rec.Body.String(), svc.calls.Load(), svc.attempt)
		}
		// Only the delivery actions are opened to a runtime principal.
		z := runtimeAuthorizer{}
		p := httpapi.Principal{ID: "runtime:" + string(fxScope.TenantID) + ":ses_deploy0123456789abcdefg"}
		target := httpapi.Target{Resource: httpapi.Reference{Kind: deliveryhttp.ResourceAttempt, ID: "att_deploy1"}}
		for _, r := range deliveryhttp.RuntimeRoutes(deliveryhttp.DefaultRuntimeActions()) {
			if err := z.Authorize(context.Background(), p, r.Action, target); err != nil {
				t.Errorf("%s: %v", r.Action, err)
			}
			if err := z.Authorize(context.Background(), httpapi.Principal{ID: "u_1"}, r.Action, target); !errors.Is(err, httpapi.ErrDenied) {
				t.Errorf("%s for a user: %v", r.Action, err)
			}
			if err := z.Authorize(context.Background(), p, r.Action, httpapi.Target{Resource: httpapi.Reference{Kind: "run", ID: "att_deploy1"}}); !errors.Is(err, httpapi.ErrDenied) {
				t.Errorf("%s on another resource: %v", r.Action, err)
			}
		}
		if err := z.Authorize(context.Background(), p, "deployment.create", target); !errors.Is(err, httpapi.ErrDenied) {
			t.Errorf("a human action for a runtime principal: %v", err)
		}
	})

	t.Run("the delivery slice refuses an incomplete or unguarded assembly", func(t *testing.T) {
		for name, mutate := range map[string]func(*dvWorld){
			"no execution slice":                     func(w *dvWorld) { w.f.slice = nil; w.in.Slice = nil },
			"no cursor codec":                        func(w *dvWorld) { w.in.Cursors = nil },
			"no profile service":                     func(w *dvWorld) { w.in.Profiles = nil },
			"no gate service":                        func(w *dvWorld) { w.in.Gates = nil },
			"no gate decider":                        func(w *dvWorld) { w.in.GateDecisions = nil },
			"no delivery service":                    func(w *dvWorld) { w.in.Delivery = nil },
			"no registrar":                           func(w *dvWorld) { w.in.Deployments = nil },
			"no scopes":                              func(w *dvWorld) { w.in.Scopes = nil },
			"no definitions":                         func(w *dvWorld) { w.in.Definitions = nil },
			"no repair interval":                     func(w *dvWorld) { w.in.Tuning.ReconcileInterval = 0 },
			"a negative sweep":                       func(w *dvWorld) { w.in.CleanupEvery = -time.Second },
			"unknown environment":                    func(w *dvWorld) { w.in.Env = "" },
			"production refuses development doubles": func(w *dvWorld) { w.in.Env = Production },
		} {
			f := newFixture(t, Test)
			w := newDVInputs(t, f)
			w.in.Slice = f.slice
			mutate(w)
			s, err := AssembleDelivery(w.in)
			if err == nil || s != nil {
				t.Errorf("%s: the slice was assembled", name)
			}
		}
		f := newFixture(t, Test)
		w := newDVInputs(t, f)
		w.in.Env = Production
		w.in.Slice = f.slice
		if _, err := AssembleDelivery(w.in); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production composed development adapters: %v", err)
		}
	})

	t.Run("the human routes of profiles, gates and deployments are served behind their rules", func(t *testing.T) {
		f := newFixture(t, Test)
		w := newDVInputs(t, f)
		h := gatesmemory.NewHarness(t)
		w.in.Gates = h.E.Svc
		s := w.assemble(t)

		for _, want := range []string{"agent-profiles", "gates", "environments", "deployments"} {
			if !contains(s.Routes, want) {
				t.Errorf("route group %s is not declared: %v", want, s.Routes)
			}
		}
		// Every action the slice registers has a rule, and the permissions are the
		// documented ones (an action without a rule is denied).
		rules := coreRules()
		wantPerm := map[httpapi.Action]identity.Permission{
			agentshttp.ActionList: identity.PermRunRead, agentshttp.ActionRegister: identity.PermRunManage,
			gateshttp.ActionList: identity.PermEvidenceRead, gateshttp.ActionRead: identity.PermEvidenceRead, gateshttp.ActionDecide: identity.PermGateDecide,
		}
		for _, r := range deliveryhttp.PublicRoutes(deliveryhttp.DefaultActions()) {
			perm := identity.PermDeploymentManage
			if strings.HasSuffix(string(r.Action), ".list") || strings.HasSuffix(string(r.Action), ".read") {
				perm = identity.PermRunRead
			}
			wantPerm[r.Action] = perm
		}
		for action, perm := range wantPerm {
			rule, ok := rules[action]
			if !ok || rule.Level != identityhttp.LevelProject || len(rule.Any) != 1 || rule.Any[0] != perm {
				t.Errorf("action %s: rule %+v (want %s)", action, rule, perm)
			}
		}

		router, err := httpapi.NewRouter(httpapi.RouterConfig{
			Authenticator: dvAuthn{}, Authorizer: fxAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: APIPrefix,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RegisterPublic(router); err != nil {
			t.Fatal(err)
		}
		get := func(token string, scope domain.Scope, path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("GET", APIPrefix+"/tenants/"+string(scope.TenantID)+"/projects/"+string(scope.ProjectID)+path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			return rec
		}
		if rec := get(executiontest.Engineer, fxScope, "/agent-profiles"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "items") {
			t.Fatalf("agent profiles: %d %s", rec.Code, rec.Body.String())
		}
		if rec := get(gatesmemory.Reviewer, gatesmemory.ScopeA, "/gates"); rec.Code != http.StatusOK {
			t.Fatalf("gates: %d %s", rec.Code, rec.Body.String())
		}
		// The service decides, not the router: a stranger sees nothing of the gates.
		if rec := get(gatesmemory.Stranger, gatesmemory.ScopeA, "/gates"); rec.Code == http.StatusOK {
			t.Fatalf("a caller without a binding listed gates: %d %s", rec.Code, rec.Body.String())
		}
		if rec := get(executiontest.Engineer, fxScope, "/environments"); rec.Code != http.StatusOK {
			t.Fatalf("environments: %d %s", rec.Code, rec.Body.String())
		}
		if rec := get(executiontest.Engineer, fxScope, "/deployments"); rec.Code != http.StatusOK {
			t.Fatalf("deployments: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the delivery slice activates what the execution slice recorded as pending", func(t *testing.T) {
		f := newFixture(t, Test)
		w := newDVInputs(t, f)
		w.in.Unavailable = []string{"deployment-gates"}
		s := w.assemble(t)
		got := DisabledCapabilities(f.slice, s)
		for _, activated := range []string{"gates", "agents", "delivery", "cleanup-runs"} {
			if contains(got, activated) {
				t.Errorf("%q is served and must not be reported disabled: %v", activated, got)
			}
		}
		for _, still := range []string{"runtime-secret-resolution", "git-webhooks", "chatops", "deployment-gates"} {
			if !contains(got, still) {
				t.Errorf("%q is not served and must stay recorded: %v", still, got)
			}
		}
		// Without the slice nothing is activated (the M1 composition).
		if all := DisabledCapabilities(f.slice, nil); !contains(all, "gates") || !contains(all, "delivery") {
			t.Fatalf("the M1 composition must keep reporting them: %v", all)
		}
	})
}

// recordingRuntime stands for the delivery service's runtime callbacks.
type recordingRuntime struct {
	deliveryhttp.RuntimeService
	calls   atomic.Int64
	attempt domain.AttemptID
	bearer  string
}

func (r *recordingRuntime) DevelopmentOnly() bool { return true }
func (r *recordingRuntime) LookupByAction(_ context.Context, attempt domain.AttemptID, bearer, _ string) (delivery.Receipt, error) {
	r.calls.Add(1)
	r.attempt, r.bearer = attempt, bearer
	return delivery.Receipt{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func quoteAll(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(a) + `"`
	}
	return strings.Join(out, ", ")
}

// dvAuthn names the principal by its bearer: the token is the principal's ID.
type dvAuthn struct{}

func (dvAuthn) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	_, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || token == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: token}, nil
}

// ---- gate_wiring ------------------------------------------------------------------

// A gate waits in durable records and consumes no compute: the assembled gate
// reconciler requests the gate of a ready step, a human decision is recorded
// without progressing anything, and the next pass completes the step once.
func TestGateWiring(t *testing.T) {
	ctx := context.Background()
	newWorld := func(t *testing.T) (*dvWorld, *gatesmemory.Harness) {
		t.Helper()
		h := gatesmemory.NewHarness(t)
		h.E.SetTenants(gatesmemory.ScopeA.TenantID)
		f := newFixture(t, Test)
		w := newDVInputs(t, f)
		w.in.Gates = h.E.Svc
		w.in.Tuning.ReconcileInterval = 5 * time.Millisecond
		w.assemble(t)
		return w, h
	}
	decide := func(t *testing.T, h *gatesmemory.Harness, c gatesmemoryCase, who string, d gates.Decision) error {
		t.Helper()
		g, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step)
		if err != nil {
			t.Fatalf("the reconciler did not request the gate: %v", err)
		}
		_, err = h.E.Svc.Decide(ctx, gates.Caller{PrincipalID: who, RequestID: "req_t072"}, g.Scope, g.ID, gates.DecideCommand{
			Decision: d, Reason: "Reviewed the identified source and test evidence.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
		})
		return err
	}
	eventually := func(t *testing.T, what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	t.Run("a decision waits for the worker and the step completes once without compute", func(t *testing.T) {
		w, h := newWorld(t)
		c := h.NewCase(t, false)
		if st := h.StepStatus(t, c); st != execution.StepPending {
			t.Fatalf("step %s", st)
		}
		g := w.start()
		defer func() { _ = g.Stop(context.Background()) }()
		eventually(t, "the gate request", func() bool { _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); return err == nil })
		// Waiting holds no executor capacity and starts nothing.
		time.Sleep(30 * time.Millisecond)
		if st := h.StepStatus(t, c); st != execution.StepPending || h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("a waiting gate allocated compute: step %s, %d attempts, %d reservations", h.StepStatus(t, c), h.Attempts(t, c), h.E.Reservations())
		}
		if err := decide(t, h, c, gatesmemory.Engineer, gates.DecisionApprove); !errors.Is(err, gates.ErrDenied) {
			t.Fatalf("an engineer decided a gate: %v", err)
		}
		if err := decide(t, h, c, gatesmemory.Reviewer, gates.DecisionApprove); err != nil {
			t.Fatalf("the reviewer's decision: %v", err)
		}
		eventually(t, "the step transition", func() bool { return h.StepStatus(t, c) == execution.StepSucceeded })
		if h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("progression allocated compute: %d attempts, %d reservations", h.Attempts(t, c), h.E.Reservations())
		}
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
		ev := h.Events(t, c, mustGate(t, h, c).ID)
		if ev.Requested != 1 || ev.Decided != 1 {
			t.Fatalf("one request and one decision: %+v", ev)
		}
		// The loop keeps running over the settled step: nothing repeats.
		time.Sleep(30 * time.Millisecond)
		if ev2 := h.Events(t, c, mustGate(t, h, c).ID); ev2 != ev {
			t.Fatalf("a later pass repeated an effect: %+v -> %+v", ev, ev2)
		}
	})

	t.Run("a revoked reviewer's approval is not consumed", func(t *testing.T) {
		w, h := newWorld(t)
		c := h.NewCase(t, false)
		g := w.start()
		eventually(t, "the gate request", func() bool { _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); return err == nil })
		stop, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := g.Stop(stop); err != nil {
			t.Fatal(err)
		}
		cancel()
		// The decision is durable while the reviewer is current; the grant is
		// revoked before any worker consumes it, so the step must keep waiting.
		if err := decide(t, h, c, gatesmemory.Reviewer, gates.DecisionApprove); err != nil {
			t.Fatal(err)
		}
		h.Revoke(t, gatesmemory.Reviewer)
		g = w.start()
		defer func() { _ = g.Stop(context.Background()) }()
		time.Sleep(40 * time.Millisecond)
		if st := h.StepStatus(t, c); st != execution.StepPending || h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("a revoked approval progressed: step %s, %d attempts, %d reservations", st, h.Attempts(t, c), h.E.Reservations())
		}
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a restarted process converges on the durable decision", func(t *testing.T) {
		w, h := newWorld(t)
		c := h.NewCase(t, false)
		g := w.start()
		eventually(t, "the gate request", func() bool { _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); return err == nil })
		stop, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := g.Stop(stop); err != nil {
			t.Fatal(err)
		}
		cancel()
		// The decision lands while no worker runs: nothing progresses.
		if err := decide(t, h, c, gatesmemory.Reviewer, gates.DecisionApprove); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if st := h.StepStatus(t, c); st != execution.StepPending {
			t.Fatalf("a stopped worker progressed the step: %s", st)
		}
		g = w.start()
		defer func() { _ = g.Stop(context.Background()) }()
		eventually(t, "the redriven transition", func() bool { return h.StepStatus(t, c) == execution.StepSucceeded })
		if h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("recovery allocated compute: %d attempts, %d reservations", h.Attempts(t, c), h.E.Reservations())
		}
	})

	t.Run("a rejection fails the step and starts nothing", func(t *testing.T) {
		w, h := newWorld(t)
		c := h.NewCase(t, false)
		g := w.start()
		defer func() { _ = g.Stop(context.Background()) }()
		eventually(t, "the gate request", func() bool { _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); return err == nil })
		if err := decide(t, h, c, gatesmemory.Reviewer, gates.DecisionReject); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the step failure", func() bool { return h.StepStatus(t, c) == execution.StepFailed })
		if h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("a rejected gate allocated compute: %d attempts, %d reservations", h.Attempts(t, c), h.E.Reservations())
		}
	})

	t.Run("an outage of the gate scan is retried and never stops the worker", func(t *testing.T) {
		w, h := newWorld(t)
		c := h.NewCase(t, false)
		h.E.FailCommits(errors.New("database unavailable"))
		g := w.start()
		defer func() { _ = g.Stop(context.Background()) }()
		time.Sleep(40 * time.Millisecond)
		if err := g.Healthy(); err != nil {
			t.Fatalf("an outage stopped the loop: %v", err)
		}
		if _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); err == nil {
			t.Fatal("a gate was recorded while every commit failed")
		}
		h.E.FailCommits(nil)
		eventually(t, "the gate request after recovery", func() bool { _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); return err == nil })
	})

	t.Run("deployment gates fail closed when the join is not recorded as available", func(t *testing.T) {
		got := deliveryUnavailable(false)
		for _, want := range []string{"deployment-gates", "agent-candidate-gates", "agent-steps", "model-gateway", "mcp-proxy", "step-environments"} {
			if !contains(got, want) {
				t.Errorf("%q is not recorded as unavailable: %v", want, got)
			}
		}
		if contains(deliveryUnavailable(true), "step-environments") {
			t.Error("step environments are available when the store binds them")
		}
	})
}

type gatesmemoryCase = struct {
	Scope      domain.Scope
	Run        domain.RunID
	Step       string
	Item       domain.WorkItemID
	Evidence   domain.ArtifactID
	Deployment string
}

func mustGate(t *testing.T, h *gatesmemory.Harness, c gatesmemoryCase) *gates.Gate {
	t.Helper()
	g, err := h.E.Store.ByStep(context.Background(), c.Scope, c.Run, c.Step)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// ---- delivery_worker_lifecycle -------------------------------------------------------

// The slice's workers start, stop and fail deliberately: cancellation reaches a
// blocked sweep, an outage is retried, and an expired deployment is cleaned by
// a run admitted from the project's cleanup definition, once.
func TestDeliveryWorkerLifecycle(t *testing.T) {
	cleanupDoc := pipelinetest.DocNamed(CleanupDefinitionName, "{deployment_id: {type: string, required: true}}",
		pipelinetest.Container("cleanup", "workspace: {access: none}"))

	// newCleanupWorld wires the execution fixture with deployment-bound cleanup
	// authorization from the record table and the project's cleanup definition
	// resolved by name through the pipeline repository.
	newCleanupWorld := func(t *testing.T, mutate ...func(*dvWorld)) *dvWorld {
		t.Helper()
		var rec *dvRecords
		f := fxBuildOpts(t, Test, executiontest.MemOptions{}, func(f *fixture) {
			rec = newDVRecords(f.m.Clock.Now)
			f.in.Deps.Cleanup = rec
		})
		w := newDVInputs(t, f)
		w.rec, w.in.Records = rec, rec
		w.in.Definitions = NamedCleanupDefinitions{Repo: f.m.Defs}
		for _, m := range mutate {
			m(w)
		}
		w.assemble(t)
		return w
	}
	stop := func(t *testing.T, g *workerGroup) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cleanupRuns := func(t *testing.T, w *dvWorld) []*execution.Run {
		t.Helper()
		all, err := w.f.m.Mem.List(context.Background(), fxScope, execution.ListQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var out []*execution.Run
		for _, r := range all {
			if r.Purpose == execution.PurposeCleanup {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("the workers are named, start together and stop on shutdown", func(t *testing.T) {
		w := newCleanupWorld(t)
		names := []string{}
		for _, wk := range w.slice.Workers {
			names = append(names, wk.Name)
		}
		if !slices.Equal(names, []string{"gate-reconciler", "deployment-cleanup"}) {
			t.Fatalf("workers %v", names)
		}
		g := w.start()
		if !slices.Equal(g.Names(), []string{"deployment-cleanup", "gate-reconciler"}) {
			t.Fatalf("started %v", g.Names())
		}
		w.f.eventually("the cleanup sweeps", func() bool { return w.scopes.calls.Load() >= 2 })
		stop(t, g)
		if err := g.Healthy(); err != nil {
			t.Fatalf("a clean shutdown is not a failure: %v", err)
		}
		calls := w.scopes.calls.Load()
		time.Sleep(30 * time.Millisecond)
		if w.scopes.calls.Load() != calls {
			t.Fatal("a sweep ran after shutdown")
		}
	})

	t.Run("cancellation reaches a sweep blocked on a dependency", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		w := newCleanupWorld(t, func(w *dvWorld) {
			w.scopes.hook = func(ctx context.Context) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-ctx.Done() // a dependency that never answers
				return ctx.Err()
			}
		})
		g := w.start()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep did not start")
		}
		stop(t, g) // returns only if the blocked call observed the cancellation
		if err := g.Healthy(); err != nil {
			t.Fatalf("a cancelled sweep is not a failure: %v", err)
		}
	})

	t.Run("an outage is retried by the next sweep and never stops the worker", func(t *testing.T) {
		var fail atomic.Bool
		fail.Store(true)
		w := newCleanupWorld(t, func(w *dvWorld) {
			w.scopes.hook = func(context.Context) error {
				if fail.Load() {
					return errors.New("tenant directory unavailable")
				}
				return nil
			}
		})
		w.f.m.Define(fxScope, cleanupDoc)
		w.rec.add(fxScope, "dep_preview", -time.Minute) // already expired
		g := w.start()
		defer stop(t, g)
		w.f.eventually("repeated failing sweeps", func() bool { return w.scopes.calls.Load() >= 3 })
		if err := g.Healthy(); err != nil {
			t.Fatalf("an outage stopped the loop: %v", err)
		}
		if n := len(cleanupRuns(t, w)); n != 0 {
			t.Fatalf("%d cleanup runs while the directory was down", n)
		}
		fail.Store(false)
		w.f.eventually("the cleanup run after recovery", func() bool { return len(cleanupRuns(t, w)) == 1 })
	})

	t.Run("an expired deployment is cleaned by one run admitted from the cleanup definition", func(t *testing.T) {
		w := newCleanupWorld(t)
		w.f.m.Define(fxScope, cleanupDoc)
		w.rec.add(fxScope, "dep_preview", time.Hour)
		g := w.start()
		defer stop(t, g)

		// Not due: swept repeatedly, nothing requested and nothing admitted.
		w.f.eventually("sweeps before the expiry", func() bool { return w.scopes.calls.Load() >= 3 })
		if got := w.rec.get(fxScope, "dep_preview"); got.RequestedAt != nil || len(cleanupRuns(t, w)) != 0 {
			t.Fatalf("a deployment before its expiry was acted on: %+v", got)
		}

		w.f.m.Advance(2 * time.Hour)
		w.f.eventually("the cleanup run", func() bool { return len(cleanupRuns(t, w)) == 1 })
		run := cleanupRuns(t, w)[0]
		if run.ID != execution.CleanupRunID(fxScope, "dep_preview", 1) || run.DeploymentID != "dep_preview" || run.WorkItemID != "" || run.WorkspaceID != "" {
			t.Fatalf("cleanup run %+v", run)
		}
		// The deployment reaches the cleanup step as the run input, set by admission
		// after the record owner authorized it.
		agg := w.f.m.Get(run)
		if v := agg.Run.Snapshot.Values()[execution.CleanupDeploymentInput]; !v.IsString() || v.String() != "dep_preview" {
			t.Fatalf("the cleanup run input is %v", v)
		}
		if got := w.rec.get(fxScope, "dep_preview"); got.RequestedAt == nil || got.State != deliverycleanup.RecordRequested {
			t.Fatalf("the cleanup request was not recorded: %+v", got)
		}
		// Further sweeps converge on the same run and allocate nothing else.
		calls := w.scopes.calls.Load()
		w.f.eventually("more sweeps", func() bool { return w.scopes.calls.Load() >= calls+3 })
		if n := len(cleanupRuns(t, w)); n != 1 {
			t.Fatalf("%d cleanup runs for one deployment", n)
		}
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a project without a cleanup definition is retried until it has one", func(t *testing.T) {
		w := newCleanupWorld(t)
		w.rec.add(fxScope, "dep_preview", -time.Minute)
		g := w.start()
		defer stop(t, g)
		w.f.eventually("sweeps without a definition", func() bool { return w.scopes.calls.Load() >= 3 })
		if n := len(cleanupRuns(t, w)); n != 0 {
			t.Fatalf("a cleanup run without a definition: %d", n)
		}
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
		w.f.m.Define(fxScope, cleanupDoc)
		w.f.eventually("the run once the definition exists", func() bool { return len(cleanupRuns(t, w)) == 1 })
	})

	t.Run("cleanup is authorized by the record owner, not by the scheduler", func(t *testing.T) {
		w := newCleanupWorld(t)
		w.f.m.Define(fxScope, cleanupDoc)
		// The record owner no longer authorizes this deployment (it was never
		// requested as far as the authorization sees): admission refuses the run.
		w.rec.add(fxScope, "dep_preview", -time.Minute)
		w.rec.mu.Lock()
		w.rec.deps[string(fxScope.TenantID)+"/"+string(fxScope.ProjectID)+"/dep_preview"].Deleted = true
		w.rec.mu.Unlock()
		g := w.start()
		defer stop(t, g)
		w.f.eventually("sweeps", func() bool { return w.scopes.calls.Load() >= 3 })
		if n := len(cleanupRuns(t, w)); n != 0 {
			t.Fatalf("a deleted deployment was cleaned again: %d runs", n)
		}
	})
}

// ---- the cleanup definition source ------------------------------------------------------

type fakeDefinitions struct {
	defs  []pipeline.Definition
	err   error
	pages int
}

func (f *fakeDefinitions) List(_ context.Context, scope domain.Scope, q pipeline.ListQuery) ([]pipeline.Definition, error) {
	f.pages++
	if f.err != nil {
		return nil, f.err
	}
	start := 0
	if q.After != nil {
		for i, d := range f.defs {
			if d.ID == q.After.ID {
				start = i + 1
			}
		}
	}
	var out []pipeline.Definition
	for _, d := range f.defs[start:] {
		if d.Scope == scope && (q.Name == "" || d.Name == q.Name) {
			out = append(out, d)
		}
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func TestNamedCleanupDefinitions(t *testing.T) {
	ctx := context.Background()
	mk := func(scope domain.Scope, id, name string, v domain.Version) pipeline.Definition {
		return pipeline.Definition{Scope: scope, ID: domain.DefinitionID(id), Name: name, Version: v, CreatedAt: dvNow.Add(time.Duration(v) * time.Minute)}
	}
	t.Run("the newest version of the named definition of the project is the one run", func(t *testing.T) {
		repo := &fakeDefinitions{defs: []pipeline.Definition{
			mk(fxScope, "pd_1", CleanupDefinitionName, 1), mk(fxScope, "pd_other", "build", 7), mk(fxScope, "pd_2", CleanupDefinitionName, 2),
			mk(executiontest.ScopeB, "pd_foreign", CleanupDefinitionName, 9),
		}}
		id, err := NamedCleanupDefinitions{Repo: repo}.CleanupDefinition(ctx, fxScope)
		if err != nil || id != "pd_2" {
			t.Fatalf("%q %v", id, err)
		}
	})
	t.Run("no definition is the scheduler's own refusal, an outage is an error", func(t *testing.T) {
		if _, err := (NamedCleanupDefinitions{Repo: &fakeDefinitions{}}).CleanupDefinition(ctx, fxScope); !errors.Is(err, deliverycleanup.ErrNoDefinition) {
			t.Fatalf("%v", err)
		}
		boom := errors.New("down")
		if _, err := (NamedCleanupDefinitions{Repo: &fakeDefinitions{err: boom}}).CleanupDefinition(ctx, fxScope); !errors.Is(err, boom) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a repository that answers another project's definition is not trusted", func(t *testing.T) {
		repo := &leakyDefinitions{def: mk(executiontest.ScopeB, "pd_foreign", CleanupDefinitionName, 1)}
		if _, err := (NamedCleanupDefinitions{Repo: repo}).CleanupDefinition(ctx, fxScope); !errors.Is(err, deliverycleanup.ErrNoDefinition) {
			t.Fatalf("a foreign definition was selected: %v", err)
		}
	})
	t.Run("many versions are paged", func(t *testing.T) {
		repo := &fakeDefinitions{}
		for i := 1; i <= 250; i++ {
			repo.defs = append(repo.defs, mk(fxScope, "pd_"+strings.Repeat("a", 1)+itoa(i), CleanupDefinitionName, domain.Version(i)))
		}
		id, err := NamedCleanupDefinitions{Repo: repo}.CleanupDefinition(ctx, fxScope)
		if err != nil || id != "pd_a250" || repo.pages != 3 {
			t.Fatalf("%q %v after %d pages", id, err, repo.pages)
		}
	})
}

type leakyDefinitions struct{ def pipeline.Definition }

func (l *leakyDefinitions) List(context.Context, domain.Scope, pipeline.ListQuery) ([]pipeline.Definition, error) {
	return []pipeline.Definition{l.def}, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// ---- project enumeration ----------------------------------------------------------------

func TestProjectScopesPageInOrderAndFailClosed(t *testing.T) {
	ctx := context.Background()
	projects := map[domain.TenantID][]domain.ProjectID{
		"t_a": {"p_1", "p_2", "p_3"}, "t_b": {}, "t_c": {"p_1"},
	}
	var failTenant domain.TenantID
	l := projectLister{
		tenants: fxTenants{"t_c", "t_a", "t_b"}, // unsorted on purpose
		projects: func(_ context.Context, tenant domain.TenantID, after domain.ProjectID, limit int) ([]domain.ProjectID, error) {
			if tenant == failTenant {
				return nil, errors.New("directory down")
			}
			var out []domain.ProjectID
			for _, p := range projects[tenant] {
				if p > after && len(out) < limit {
					out = append(out, p)
				}
			}
			return out, nil
		},
	}
	var all []domain.Scope
	var after *domain.Scope
	for {
		page, err := l.Scopes(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 2 {
			break
		}
		after = &page[len(page)-1]
	}
	want := []domain.Scope{{TenantID: "t_a", ProjectID: "p_1"}, {TenantID: "t_a", ProjectID: "p_2"}, {TenantID: "t_a", ProjectID: "p_3"}, {TenantID: "t_c", ProjectID: "p_1"}}
	if !slices.Equal(all, want) {
		t.Fatalf("%v", all)
	}
	if _, err := l.Scopes(ctx, nil, 0); err == nil {
		t.Fatal("a page without a limit")
	}
	failTenant = "t_a"
	if got, err := l.Scopes(ctx, nil, 10); err == nil || got != nil {
		t.Fatalf("a tenant that cannot be listed was skipped: %v %v", got, err)
	}
	l.tenants = tenantsDown{}
	if _, err := l.Scopes(ctx, nil, 10); err == nil {
		t.Fatal("a tenant outage yielded an empty sweep")
	}
}

type tenantsDown struct{}

func (tenantsDown) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return nil, errors.New("down")
}

var _ = agentstest.NewApprovals
var _ = deliveryadapters.EffectSource
var _ sync.Mutex

func TestDeliveryToolsFileIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "tools.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := loadDeliveryTools(write(`[{"protocol":"apply","image":"` + dvToolImage + `","command":["/opt/kodama/apply.sh"]}]`))
	if err != nil || len(got) != 1 || got[0].Protocol != ToolApply {
		t.Fatalf("%v %v", got, err)
	}
	for name, body := range map[string]string{
		"unknown field": `[{"protocol":"apply","image":"` + dvToolImage + `","command":["/a"],"trusted":true}]`,
		"unpinned":      `[{"protocol":"apply","image":"tools:latest","command":["/a"]}]`,
		"not a list":    `{}`,
	} {
		if _, err := loadDeliveryTools(write(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
