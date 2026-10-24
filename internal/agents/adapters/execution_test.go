package adapters_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const engineerID = "u030_engineer"

type world struct {
	repo      *agentstest.MemRepo
	approvals *agentstest.Approvals
	svc       *agents.Service
	draft     agents.Draft
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{repo: &agentstest.MemRepo{}, approvals: agentstest.NewApprovals(), draft: agentstest.Draft(t, "coding")}
	auth := agentstest.NewAuthority()
	auth.Grant(scope, engineerID, agents.ActorUser, agents.PermRegister, agents.PermList)
	svc, err := agents.NewService(agents.ServiceConfig{
		Repository: w.repo, Authority: auth, Compatibility: agentstest.Catalogue(t, w.draft), Approvals: w.approvals,
		Clock: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	return w
}

func (w *world) register(t *testing.T, d agents.Draft) agents.ProfileVersion {
	t.Helper()
	res, err := w.svc.Register(context.Background(), agents.RegisterCommand{Scope: scope, Caller: agents.Caller{PrincipalID: engineerID}, Draft: d})
	if err != nil {
		t.Fatal(err)
	}
	return res.Profile
}

func TestPipelineLookup(t *testing.T) {
	w := newWorld(t)
	p := w.register(t, w.draft)
	lookup, err := adapters.NewLookup(w.svc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	info, err := lookup.LookupProfile(ctx, scope, p.Ref())
	if err != nil || !info.Immutable {
		t.Fatalf("an exact qualified version is immutable: %+v %v", info, err)
	}
	for _, alias := range []string{"coding", "coding@latest", "coding@0", "coding@01", "coding@"} {
		info, err := lookup.LookupProfile(ctx, scope, alias)
		if err != nil || info.Immutable {
			t.Errorf("%q is a moving alias and must not be immutable: %+v %v", alias, info, err)
		}
	}
	if _, err := lookup.LookupProfile(ctx, scope, "coding@9"); !errors.Is(err, pipeline.ErrReferenceNotFound) {
		t.Fatalf("an unknown version: %v", err)
	}
	other := domain.Scope{TenantID: "t030_globex", ProjectID: "p030_billing"}
	if _, err := lookup.LookupProfile(ctx, other, p.Ref()); !errors.Is(err, pipeline.ErrReferenceNotFound) {
		t.Fatalf("a foreign reference is indistinguishable from an unknown one: %v", err)
	}
	other2 := domain.Scope{TenantID: scope.TenantID, ProjectID: "p030_other"}
	if _, err := lookup.LookupProfile(ctx, other2, p.Ref()); !errors.Is(err, pipeline.ErrReferenceNotFound) {
		t.Fatalf("another project of the tenant: %v", err)
	}

	t.Run("a withdrawn approval makes the version unusable, not unknown", func(t *testing.T) {
		w.approvals.WithdrawModel("t030-model")
		info, err := lookup.LookupProfile(ctx, scope, p.Ref())
		if err != nil || info.Immutable {
			t.Fatalf("expected a non-immutable verdict, got %+v %v", info, err)
		}
	})

	t.Run("an outage is an outage, never a verdict", func(t *testing.T) {
		w2 := newWorld(t)
		p2 := w2.register(t, w2.draft)
		l2, _ := adapters.NewLookup(w2.svc)
		w2.approvals.Err = errors.New("approval backend down")
		if _, err := l2.LookupProfile(ctx, scope, p2.Ref()); !errors.Is(err, pipeline.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	})

	t.Run("pipeline validation uses the lookup", func(t *testing.T) {
		w3 := newWorld(t)
		p3 := w3.register(t, w3.draft)
		l3, _ := adapters.NewLookup(w3.svc)
		v, err := pipeline.NewValidator(pipeline.Config{
			Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent}, Profiles: l3, References: pipelinetest.References{},
		})
		if err != nil {
			t.Fatal(err)
		}
		doc := func(ref string) string {
			return pipelinetest.Doc("{id: implement, kind: agent, agent: {profile: " + ref + "}, timeoutSeconds: 600, workspace: {access: write, from: run}}")
		}
		actor := pipeline.Actor{ID: engineerID, Type: pipeline.ActorUser}
		if _, err := v.ValidateYAML(ctx, scope, actor, []byte(doc(p3.Ref()))); err != nil {
			t.Fatalf("an exact reference validates: %v", err)
		}
		for ref, code := range map[string]string{"coding": pipeline.CodeMutableProfile, "coding@9": pipeline.CodeUnknownProfile} {
			_, err := v.ValidateYAML(ctx, scope, actor, []byte(doc(ref)))
			var ve *pipeline.ValidationError
			if !errors.As(err, &ve) || len(ve.Diagnostics) == 0 || ve.Diagnostics[0].Code != code {
				t.Errorf("%s must be rejected with %s: %v", ref, code, err)
			}
		}
	})

	if _, err := adapters.NewLookup(nil); err == nil {
		t.Fatal("a profile reader is required")
	}
}

func TestResolver(t *testing.T) {
	w := newWorld(t)
	p := w.register(t, w.draft)
	r, err := adapters.NewResolver(w.svc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	step := func(ref string) pipeline.Step {
		return pipeline.Step{ID: "implement", Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: ref}, TimeoutSeconds: 600}
	}

	exp, err := r.ResolveAgent(ctx, scope, step(p.Ref()))
	if err != nil || exp.Image != agentstest.Image || !reflect.DeepEqual(exp.Command, agentstest.Command) || exp.WallSeconds != 1800 ||
		exp.MaxRequests != 100 || exp.MaxTokens != 200000 || !agents.HasPayload(exp.Args) {
		t.Fatalf("resolve: %+v %v", exp, err)
	}
	for name, ref := range map[string]string{"alias": "coding", "unknown": "coding@9"} {
		if _, err := r.ResolveAgent(ctx, scope, step(ref)); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Errorf("%s must refuse admission: %v", name, err)
		}
	}
	other := domain.Scope{TenantID: "t030_globex", ProjectID: "p030_billing"}
	if _, err := r.ResolveAgent(ctx, other, step(p.Ref())); !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("a foreign profile refuses admission: %v", err)
	}
	if _, err := r.ResolveAgent(ctx, scope, pipeline.Step{ID: "x", Kind: pipeline.KindContainer}); !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("a container step is not an agent step: %v", err)
	}
	w.approvals.WithdrawModel("t030-model")
	if _, err := r.ResolveAgent(ctx, scope, step(p.Ref())); !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("a withdrawn approval refuses new runs: %v", err)
	}
	w.approvals.Err = errors.New("down")
	if _, err := r.ResolveAgent(ctx, scope, step(p.Ref())); !errors.Is(err, execution.ErrUnavailable) {
		t.Fatalf("an outage is unavailability: %v", err)
	}
	if _, err := adapters.NewResolver(nil); err == nil {
		t.Fatal("a profile reader is required")
	}
}

// ---- expansion over admission plans ----

func containerStep(id string, args ...string) pipeline.Step {
	return pipeline.Step{ID: id, Kind: pipeline.KindContainer, Image: pipelinetest.Image, Command: []string{"/bin/sh", "-ec"}, Args: args, TimeoutSeconds: 60,
		Workspace: &pipeline.Workspace{Access: pipeline.AccessRead, From: pipeline.RunSource}, Retry: &pipeline.Retry{MaxAttempts: 1}}
}

func agentStep(id, ref string, timeout int64, needs ...string) pipeline.Step {
	return pipeline.Step{ID: id, Kind: pipeline.KindAgent, Needs: needs, Agent: &pipeline.AgentSpec{Profile: ref}, TimeoutSeconds: timeout,
		Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource},
		Retry:     &pipeline.Retry{MaxAttempts: 2, BackoffSeconds: 5, On: []pipeline.RetryClass{pipeline.RetryTransient}},
		Outputs:   map[string]pipeline.Output{"summary": {Type: pipeline.TypeString}},
		When:      &pipeline.Condition{Input: "go", Equals: pipeline.Scalar{Type: pipeline.TypeBoolean, Bool: true}}}
}

func planOf(steps ...pipeline.Step) *execution.AdmissionPlan {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	run := &execution.Run{
		Scope: scope, ID: "run_t030_1", Purpose: execution.PurposeWork, WorkItemID: "wi_t030", WorkspaceID: "ws_t030", DefinitionID: "pd_t030",
		DefinitionDigest: "sha256:" + string(repeatByte('2', 64)), ContentRevision: 1, SourceCommit: agentstest.Commit,
		RequestedBy: execution.Actor{ID: engineerID, Type: execution.ActorUser}, Status: execution.RunQueued, Reason: execution.ReasonCreated, Version: 1,
	}
	agg := &execution.Aggregate{Run: run}
	for i, s := range steps {
		agg.Steps = append(agg.Steps, execution.NewStepRun(run.ID, i, s, now))
	}
	return &execution.AdmissionPlan{Aggregate: agg, Actor: run.RequestedBy, RequestID: "req_t030"}
}

func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func newExpander(t *testing.T, w *world) adapters.Expander {
	t.Helper()
	r, err := adapters.NewResolver(w.svc)
	if err != nil {
		t.Fatal(err)
	}
	e, err := adapters.NewExpander(r)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExpandPlan(t *testing.T) {
	w := newWorld(t)
	p := w.register(t, w.draft)
	exp := newExpander(t, w)
	ctx := context.Background()

	t.Run("agent steps are frozen as container commands and everything else is preserved", func(t *testing.T) {
		plan := planOf(containerStep("build", "-c", "make"), agentStep("implement", p.Ref(), 600, "build"))
		before := plan.Aggregate.Steps[1].Spec
		out, err := exp.Expand(ctx, plan)
		if err != nil {
			t.Fatal(err)
		}
		if out == plan || out.Aggregate == plan.Aggregate || out.Aggregate.Run != plan.Aggregate.Run {
			t.Fatal("the plan must be copied, keeping the run identity and pins")
		}
		if !reflect.DeepEqual(plan.Aggregate.Steps[1].Spec, before) || plan.Aggregate.Steps[1].Spec.Image != "" {
			t.Fatal("the original plan must never be modified")
		}
		if !reflect.DeepEqual(out.Aggregate.Steps[0].Spec, plan.Aggregate.Steps[0].Spec) {
			t.Fatal("a container step is untouched")
		}
		got, orig := out.Aggregate.Steps[1], plan.Aggregate.Steps[1]
		if got.Spec.Image != agentstest.Image || !reflect.DeepEqual(got.Spec.Command, agentstest.Command) || !agents.HasPayload(got.Spec.Args) {
			t.Fatalf("frozen command: %+v", got.Spec)
		}
		if got.Spec.TimeoutSeconds != 600 {
			t.Fatalf("an explicit step restriction narrows the profile limit: %d", got.Spec.TimeoutSeconds)
		}
		if got.Spec.Agent == nil || got.Spec.Agent.Profile != p.Ref() || got.Spec.Kind != pipeline.KindAgent {
			t.Fatal("agent attribution must be retained")
		}
		for _, same := range []struct {
			name string
			a, b any
		}{
			{"id", got.ID, orig.ID}, {"ordinal", got.Ordinal, orig.Ordinal}, {"needs", got.Spec.Needs, orig.Spec.Needs}, {"when", got.Spec.When, orig.Spec.When},
			{"workspace", got.Spec.Workspace, orig.Spec.Workspace}, {"retry", got.Spec.Retry, orig.Spec.Retry}, {"outputs", got.Spec.Outputs, orig.Spec.Outputs},
			{"action key", got.ActionKey, orig.ActionKey}, {"max attempts", got.MaxAttempts, orig.MaxAttempts}, {"status", got.Status, orig.Status},
		} {
			if !reflect.DeepEqual(same.a, same.b) {
				t.Errorf("%s changed: %v != %v", same.name, same.a, same.b)
			}
		}
		// The frozen step round-trips through the persistence codec.
		raw, err := execution.EncodeStep(got.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := execution.DecodeStep(raw); err != nil || !reflect.DeepEqual(back, got.Spec) {
			t.Fatalf("frozen step round trip: %v", err)
		}
	})

	t.Run("the profile wall time caps a longer or missing step timeout", func(t *testing.T) {
		out, err := exp.Expand(ctx, planOf(agentStep("implement", p.Ref(), 7200)))
		if err != nil || out.Aggregate.Steps[0].Spec.TimeoutSeconds != 1800 {
			t.Fatalf("a step may narrow but never widen the profile limit: %v %d", err, out.Aggregate.Steps[0].Spec.TimeoutSeconds)
		}
		out, err = exp.Expand(ctx, planOf(agentStep("implement", p.Ref(), 0)))
		if err != nil || out.Aggregate.Steps[0].Spec.TimeoutSeconds != 1800 {
			t.Fatalf("an unset step timeout takes the profile limit: %v", err)
		}
	})

	t.Run("a skipped step is not resolved", func(t *testing.T) {
		plan := planOf(agentStep("implement", "coding@404", 600))
		plan.Aggregate.Propagate(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)) // condition input "go" is unset: skipped
		if plan.Aggregate.Steps[0].Status != execution.StepSkipped {
			t.Fatal("fixture: the step must be skipped")
		}
		if _, err := exp.Expand(ctx, plan); err != nil {
			t.Fatalf("a step that can never run needs no profile: %v", err)
		}
	})

	t.Run("failures write nothing and leave the plan intact", func(t *testing.T) {
		for name, tc := range map[string]struct {
			plan *execution.AdmissionPlan
			want error
		}{
			"a moving alias":                        {planOf(agentStep("a", "coding", 60)), execution.ErrAdmissionRefused},
			"an unknown version":                    {planOf(agentStep("a", "coding@9", 60)), execution.ErrAdmissionRefused},
			"one of two steps":                      {planOf(agentStep("a", p.Ref(), 60), agentStep("b", "coding@9", 60)), execution.ErrAdmissionRefused},
			"a reserved marker on a container step": {planOf(containerStep("build", agents.PayloadMarker, "x")), execution.ErrAdmissionRefused},
		} {
			before := reflect.ValueOf(tc.plan.Aggregate.Steps[0].Spec).Interface()
			out, err := exp.Expand(ctx, tc.plan)
			if out != nil || !errors.Is(err, tc.want) {
				t.Errorf("%s: %v %v", name, out, err)
			}
			if !reflect.DeepEqual(tc.plan.Aggregate.Steps[0].Spec, before) {
				t.Errorf("%s: the plan was modified", name)
			}
		}
	})

	t.Run("a resolver outage is unavailability", func(t *testing.T) {
		w2 := newWorld(t)
		p2 := w2.register(t, w2.draft)
		e2 := newExpander(t, w2)
		w2.approvals.Err = errors.New("down")
		if _, err := e2.Expand(ctx, planOf(agentStep("a", p2.Ref(), 60))); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	})

	t.Run("run-wide allowances must agree", func(t *testing.T) {
		tokens := int64(5000)
		other := w.draft
		other.Name = "review"
		other.Limits.MaxTokens = &tokens
		p2 := w.register(t, other)
		if _, err := exp.Expand(ctx, planOf(agentStep("a", p.Ref(), 60), agentStep("b", p2.Ref(), 60))); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("different run-wide token allowances must be refused, not merged: %v", err)
		}
		same := w.draft
		same.Name = "review2"
		same.Instructions += " Different words, identical allowances."
		p3 := w.register(t, same)
		if _, err := exp.Expand(ctx, planOf(agentStep("a", p.Ref(), 60), agentStep("b", p3.Ref(), 60))); err != nil {
			t.Fatalf("equal allowances are compatible: %v", err)
		}
		none := w.draft
		none.Name = "review3"
		none.Limits.MaxRequests, none.Limits.MaxTokens, none.Limits.MaxToolCalls = nil, nil, nil
		p4 := w.register(t, none)
		if _, err := exp.Expand(ctx, planOf(agentStep("a", p.Ref(), 60), agentStep("b", p4.Ref(), 60))); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a claimed and an unclaimed allowance are incompatible: %v", err)
		}
	})

	t.Run("a nil plan or resolver fails closed", func(t *testing.T) {
		if _, err := exp.Expand(ctx, nil); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("nil plan: %v", err)
		}
		if _, err := adapters.NewExpander(nil); err == nil {
			t.Fatal("a resolver is required")
		}
		if _, err := execution.ExpandAgents(ctx, nil, planOf(agentStep("a", p.Ref(), 60))); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("agent steps without a resolver: %v", err)
		}
		if out, err := execution.ExpandAgents(ctx, nil, planOf(containerStep("build"))); err != nil || out == nil {
			t.Fatalf("a plan without agent steps needs no resolver: %v", err)
		}
	})

	t.Run("a resolver returning an unsafe expansion is refused", func(t *testing.T) {
		for name, e := range map[string]execution.AgentExpansion{
			"a mutable image":      {Image: "registry.example.invalid/agent:latest", Command: []string{"/w"}},
			"no command":           {Image: agentstest.Image},
			"an expression marker": {Image: agentstest.Image, Command: []string{"/w"}, Args: []string{"${{ inputs.x }}"}},
			"a negative limit":     {Image: agentstest.Image, Command: []string{"/w"}, MaxTokens: -1},
		} {
			_, err := execution.ExpandAgents(ctx, fixedResolver{e}, planOf(agentStep("a", p.Ref(), 60)))
			if !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

type fixedResolver struct{ exp execution.AgentExpansion }

func (f fixedResolver) ResolveAgent(context.Context, domain.Scope, pipeline.Step) (execution.AgentExpansion, error) {
	return f.exp, nil
}

// ---- planner ----

type spyPlanner struct {
	got   []runtimev1.Context
	inner execution.Planner
}

func (s *spyPlanner) Plan(tenant domain.TenantID, c runtimev1.Context) (execution.WorkloadPlan, error) {
	s.got = append(s.got, c)
	return s.inner.Plan(tenant, c)
}

func contextOf(image string, command, args []string) runtimev1.Context {
	return runtimev1.Context{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_t030_1", StepID: "implement", AttemptID: "att_t030_1", ActionKey: "run_t030_1:implement",
		Definition: runtimev1.Definition{ID: "pd_t030", Digest: runtimev1.Digest("sha256:" + string(repeatByte('2', 64)))},
		Command:    runtimev1.Command{Image: image, Command: command, Args: args, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone}, Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{},
		Secrets: []runtimev1.SecretBinding{}, Limits: runtimev1.Limits{TimeoutSeconds: 600, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 1 << 20},
	}
}

func TestPlannerPinsFromTheFrozenPayload(t *testing.T) {
	w := newWorld(t)
	p := w.register(t, w.draft)
	r, _ := adapters.NewResolver(w.svc)
	exp, err := r.ResolveAgent(context.Background(), scope, agentStep("implement", p.Ref(), 600))
	if err != nil {
		t.Fatal(err)
	}

	spy := &spyPlanner{inner: execution.DefaultPlanner{}}
	chain, err := adapters.NewChain(adapters.ChainConfig{Resolver: r, Store: &spyStore{}, Creator: &spyCreator{}, Planner: spy})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an agent attempt gets its profile and design baseline pins in the canonical context", func(t *testing.T) {
		c := contextOf(exp.Image, exp.Command, exp.Args)
		plan, err := chain.Planner.Plan(scope.TenantID, c)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := runtimev1.DecodeRequest[runtimev1.Context](plan.Context)
		if err != nil {
			t.Fatalf("the planned context must be a valid runtime context: %v", err)
		}
		if decoded.Profile == nil || *decoded.Profile != (runtimev1.Profile{Name: "coding", Version: int64(p.Version), WrapperVersion: "1.4.2"}) {
			t.Fatalf("profile pin: %+v", decoded.Profile)
		}
		if decoded.DesignBaseline == nil || decoded.DesignBaseline.Digest != w.draft.DesignBaseline.Digest {
			t.Fatalf("design baseline pin: %+v", decoded.DesignBaseline)
		}
		if c.Profile != nil || c.DesignBaseline != nil {
			t.Fatal("the caller's context must not be modified")
		}
		// The pins are part of the digest: the same command without them differs.
		bare, err := execution.DefaultPlanner{}.Plan(scope.TenantID, c)
		if err != nil || bare.ContextDigest == plan.ContextDigest || bare.SpecFingerprint == plan.SpecFingerprint {
			t.Fatal("the pins must change the context digest and spec fingerprint")
		}
		again, err := chain.Planner.Plan(scope.TenantID, c)
		if err != nil || again.ContextDigest != plan.ContextDigest || again.WorkloadName != plan.WorkloadName {
			t.Fatal("planning is deterministic")
		}
	})

	t.Run("a registered newer profile, a restart or a retry changes nothing", func(t *testing.T) {
		before, _ := chain.Planner.Plan(scope.TenantID, contextOf(exp.Image, exp.Command, exp.Args))
		newer := w.draft
		newer.Instructions += " NEW"
		w.register(t, newer)
		after, _ := chain.Planner.Plan(scope.TenantID, contextOf(exp.Image, exp.Command, exp.Args))
		if after.ContextDigest != before.ContextDigest {
			t.Fatal("planning reads only the frozen payload")
		}
	})

	t.Run("an ordinary container command is delegated unchanged", func(t *testing.T) {
		spy.got = nil
		c := contextOf(pipelinetest.Image, []string{"/bin/sh", "-ec"}, []string{"go test ./..."})
		plan, err := chain.Planner.Plan(scope.TenantID, c)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := execution.DefaultPlanner{}.Plan(scope.TenantID, c)
		if plan.WorkloadName != want.WorkloadName || plan.SpecFingerprint != want.SpecFingerprint || plan.ContextDigest != want.ContextDigest || string(plan.Context) != string(want.Context) {
			t.Fatal("a container context must not change")
		}
		if len(spy.got) != 1 || spy.got[0].Profile != nil || spy.got[0].DesignBaseline != nil {
			t.Fatal("no pins may be added to a container command")
		}
	})

	t.Run("a payload that disagrees with its command is refused", func(t *testing.T) {
		other := "registry.example.invalid/other@" + agentstest.Digest1
		for name, c := range map[string]runtimev1.Context{
			"another image":     contextOf(other, exp.Command, exp.Args),
			"another command":   contextOf(exp.Image, []string{"/bin/sh"}, exp.Args),
			"a damaged payload": contextOf(exp.Image, exp.Command, append([]string{agents.PayloadMarker}, "!!!")),
		} {
			if _, err := chain.Planner.Plan(scope.TenantID, c); !errors.Is(err, execution.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("planning performs no profile lookup", func(t *testing.T) {
		before := w.repo.Reads + w.repo.Calls
		for i := 0; i < 3; i++ {
			if _, err := chain.Planner.Plan(scope.TenantID, contextOf(exp.Image, exp.Command, exp.Args)); err != nil {
				t.Fatal(err)
			}
		}
		if got := w.repo.Reads + w.repo.Calls; got != before {
			t.Fatalf("Plan reached the profile repository: %d -> %d", before, got)
		}
	})
}

// ---- chain, store and creator decorators ----

type spyStore struct {
	execution.Store
	admitted []*execution.AdmissionPlan
	err      error
}

func (s *spyStore) Admit(_ context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	s.admitted = append(s.admitted, p)
	if s.err != nil {
		return nil, false, s.err
	}
	return p.Aggregate.Run, true, nil
}

type spyCreator struct{ planned []*execution.AdmissionPlan }

func (s *spyCreator) CreateKeyed(_ context.Context, _ httpapi.RequestIdentity, _ func(context.Context, httpapi.Reference) error,
	p *execution.AdmissionPlan, _ func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error) {
	s.planned = append(s.planned, p)
	return httpapi.StoredResponse{}, false, p.Aggregate.Run, nil
}

var _ executionhttp.Creator = (*spyCreator)(nil)

func TestChainExpandsBothAdmissionPathsIdentically(t *testing.T) {
	w := newWorld(t)
	p := w.register(t, w.draft)
	r, _ := adapters.NewResolver(w.svc)
	store, creator := &spyStore{}, &spyCreator{}
	chain, err := adapters.NewChain(adapters.ChainConfig{Resolver: r, Store: store, Creator: creator})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, _, err := chain.Store.Admit(ctx, planOf(agentStep("implement", p.Ref(), 600))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := chain.Creator.CreateKeyed(ctx, httpapi.RequestIdentity{}, nil, planOf(agentStep("implement", p.Ref(), 600)), nil); err != nil {
		t.Fatal(err)
	}
	if len(store.admitted) != 1 || len(creator.planned) != 1 {
		t.Fatalf("each path commits exactly once: %d %d", len(store.admitted), len(creator.planned))
	}
	a, b := store.admitted[0].Aggregate.Steps[0].Spec, creator.planned[0].Aggregate.Steps[0].Spec
	if !reflect.DeepEqual(a, b) || a.Image != agentstest.Image {
		t.Fatalf("both admission paths must freeze identical steps:\n%+v\n%+v", a, b)
	}

	t.Run("an expansion failure never reaches the committing delegate", func(t *testing.T) {
		store.admitted, creator.planned = nil, nil
		bad := planOf(agentStep("implement", "coding@404", 600))
		if _, _, err := chain.Store.Admit(ctx, bad); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("store path: %v", err)
		}
		if _, _, _, err := chain.Creator.CreateKeyed(ctx, httpapi.RequestIdentity{}, nil, bad, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("keyed path: %v", err)
		}
		if len(store.admitted) != 0 || len(creator.planned) != 0 {
			t.Fatal("nothing may be committed after a failed expansion")
		}
	})

	t.Run("the delegate's own failure is returned unchanged", func(t *testing.T) {
		store.err = execution.ErrConflict
		if _, _, err := chain.Store.Admit(ctx, planOf(agentStep("implement", p.Ref(), 600))); !errors.Is(err, execution.ErrConflict) {
			t.Fatal(err)
		}
	})
}

func TestChainIsTheOnlySourceOfAgentHooks(t *testing.T) {
	w := newWorld(t)
	r, _ := adapters.NewResolver(w.svc)
	good := adapters.ChainConfig{Resolver: r, Store: &spyStore{}, Creator: &spyCreator{}}
	for name, mutate := range map[string]func(*adapters.ChainConfig){
		"no resolver": func(c *adapters.ChainConfig) { c.Resolver = nil },
		"no store":    func(c *adapters.ChainConfig) { c.Store = nil },
		"no creator":  func(c *adapters.ChainConfig) { c.Creator = nil },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := adapters.NewChain(cfg); !errors.Is(err, execution.ErrNotConfigured) {
			t.Errorf("%s: a partial chain must be refused: %v", name, err)
		}
	}
	chain, err := adapters.NewChain(good)
	if err != nil {
		t.Fatal(err)
	}
	if !chain.Hooks.Enabled(pipeline.KindAgent) || !chain.Hooks.Enabled(pipeline.KindContainer) || chain.Hooks.Enabled(pipeline.KindGate) {
		t.Fatal("hooks enable containers and agents only")
	}
	caps := chain.Capabilities(pipeline.KindGate)
	for _, k := range []pipeline.StepKind{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate} {
		if ok, err := caps.StepKindEnabled(context.Background(), k); err != nil || !ok {
			t.Errorf("%s must be enabled: %v %v", k, ok, err)
		}
	}
	if ok, _ := chain.Capabilities().StepKindEnabled(context.Background(), pipeline.KindGate); ok {
		t.Fatal("gates are the integration owner's to enable")
	}
}
