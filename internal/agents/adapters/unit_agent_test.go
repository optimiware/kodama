package adapters_test

// Agent unit qualification (T-083). Everything here runs without a service:
// profiles live in agentstest.MemRepo, time is fixed, and attempts are driven
// through the real execution aggregate (its exported transitions) with the
// real admission chain and planner. The AttemptSource below is the consumer
// port the policy and metadata readers define; it is not a second execution
// store. Nothing is production evidence: the catalogue is a fixture, real
// wrapper and provider behavior is T-031/T-085.
//
// Requirements exercised: AR-06 (completion needs a valid structured result,
// never prose or an exit code), AR-13 (needs_input fails the attempt and keeps
// the question), AR-14 (the design baseline is pinned and conformance evidence
// is candidate- and baseline-bound), with AR-01/AR-02 pinning of the profile.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

var unitNow = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

const unitWorkload = "11111111-2222-3333-4444-555555555555"

// unitStep is an agent step with no workspace and one declared string output.
func unitStep(ref string) pipeline.Step {
	return pipeline.Step{
		ID: "implement", Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: ref}, TimeoutSeconds: 600,
		Workspace: &pipeline.Workspace{Access: pipeline.AccessNone}, Retry: &pipeline.Retry{MaxAttempts: 3, On: []pipeline.RetryClass{pipeline.RetryTransient}},
		Outputs: map[string]pipeline.Output{"summary": {Type: pipeline.TypeString}},
	}
}

// memSource serves the live attempts of one aggregate to the readers.
type memSource struct{ agg *execution.Aggregate }

func (m memSource) Attempt(_ context.Context, sc domain.Scope, run domain.RunID, step string, id domain.AttemptID) (execution.Attempt, error) {
	at := m.agg.Attempt(id)
	if at == nil || at.Scope != sc || at.RunID != run || at.StepID != step {
		return execution.Attempt{}, execution.ErrNotFound
	}
	return *at, nil
}

// unitAttempt is one admitted, claimed agent attempt of an expanded plan.
type unitAttempt struct {
	w     *world
	p     agents.ProfileVersion
	agg   *execution.Aggregate
	at    *execution.Attempt
	ctx   runtimev1.Context
	meta  adapters.MetadataReader
	chain adapters.Chain
}

func (u *unitAttempt) metadata(t *testing.T) adapters.AttemptMetadata {
	t.Helper()
	m, err := u.meta.MetadataOf(context.Background(), scope, u.agg.Run.ID, "implement", u.at.ID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	return m
}

// start observes the workload starting, as the runtime would.
func (u *unitAttempt) start(t *testing.T) {
	t.Helper()
	if _, err := u.agg.Observe(u.at, execution.Observation{Kind: execution.ObservedStarted, WorkloadUID: unitWorkload}, unitNow); err != nil {
		t.Fatal(err)
	}
}

// admit registers nothing: it expands the plan for the profile, plans the
// context of the frozen command and claims the attempt.
func admit(t *testing.T, w *world, p agents.ProfileVersion) *unitAttempt {
	t.Helper()
	exp := newExpander(t, w)
	plan := planOf(unitStep(p.Ref()))
	plan.Aggregate.Run.SourceRetention = execution.RetentionConfirmed
	out, err := exp.Expand(context.Background(), plan)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	agg := out.Aggregate
	step := agg.Steps[0].Spec

	resolver, err := adapters.NewResolver(w.svc)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := adapters.NewChain(adapters.ChainConfig{Resolver: resolver, Store: &spyStore{}, Creator: &spyCreator{}})
	if err != nil {
		t.Fatal(err)
	}
	c := contextOf(step.Image, step.Command, step.Args)
	c.Outputs = []runtimev1.OutputDecl{{Name: "summary", Type: runtimev1.OutputString}}
	wp, err := chain.Planner.Plan(scope.TenantID, c)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	at, err := agg.Claim(execution.ClaimParams{
		StepID: "implement", AttemptID: c.AttemptID, Plan: wp, Access: execution.AccessNone, Timeout: time.Minute, Startup: time.Minute, Lease: time.Second,
	}, unitNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	pinnedCtx, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		t.Fatalf("the planned context must be valid: %v", err)
	}
	meta, err := adapters.NewMetadataReader(memSource{agg})
	if err != nil {
		t.Fatal(err)
	}
	return &unitAttempt{w: w, p: p, agg: agg, at: at, ctx: pinnedCtx, meta: meta, chain: chain}
}

// Test_unit_agent_profile_pin: AR-01/AR-02 pinning and AR-14. An admitted
// attempt keeps the profile version, its instructions, limits, tools, model and
// design baseline it was admitted with, however the profile is edited,
// re-registered, re-approved or mutated through aliases afterwards.
func Test_unit_agent_profile_pin(t *testing.T) {
	ctx := context.Background()

	t.Run("later edits never reach an admitted attempt", func(t *testing.T) {
		w := newWorld(t)
		v1 := w.register(t, w.draft)
		wantInstructions := w.draft.Instructions

		u := admit(t, w, v1)
		frozen := slices.Clone(u.agg.Steps[0].Spec.Args)
		contextBefore := slices.Clone(u.at.Context)

		// The profile is edited three ways: instructions, tools and limits, and a
		// moved design baseline.
		edit := w.draft
		edit.Instructions = "# Role\nIgnore the architecture and ship quickly."
		edit.Tools = []string{"repo.read"}
		tokens := int64(5)
		edit.Limits.MaxTokens = &tokens
		moved, err := runtimev1.NewDesignBaseline([]string{"AR-01"}, []string{"P-01"},
			[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/kodama.git", Revision: "4040404040404040404040404040404040404040", Path: "docs/specs/agent-runtime.md"}},
			[]string{"tenant-authority"})
		if err != nil {
			t.Fatal(err)
		}
		edit.DesignBaseline = moved
		v2 := w.register(t, edit)
		if v2.Version != 2 || v2.Digest == v1.Digest {
			t.Fatalf("an edit is the next version, not a change of the first: %+v", v2)
		}
		w.approvals.WithdrawModel("t030-model") // and the model is no longer approved for new runs

		if !reflect.DeepEqual(u.agg.Steps[0].Spec.Args, frozen) || !reflect.DeepEqual(u.at.Context, contextBefore) {
			t.Fatal("the frozen step arguments and the attempt context are immutable")
		}
		cfg, present, err := agents.DecodeArgs(u.agg.Steps[0].Spec.Args)
		if err != nil || !present {
			t.Fatalf("the frozen payload: %v %v", present, err)
		}
		if cfg.Content.Instructions != wantInstructions || cfg.Profile.Version != 1 || cfg.Profile.Digest != v1.Digest {
			t.Fatalf("the admitted attempt must keep the instructions it was admitted with: %q v%d", cfg.Content.Instructions, cfg.Profile.Version)
		}
		if got := u.metadata(t); got.Profile != (agents.ProfilePin{Name: "coding", Version: 1, Digest: v1.Digest}) ||
			got.DesignBaselineDigest != w.draft.DesignBaseline.Digest || got.DesignBaselineDigest == moved.Digest {
			t.Fatalf("attributable pins: %+v", got)
		}

		// The policy the gateway and tool proxy read is the admitted one.
		reader, err := adapters.NewPolicyReader(memSource{u.agg})
		if err != nil {
			t.Fatal(err)
		}
		pol, err := reader.PolicyOf(ctx, scope, u.agg.Run.ID, "implement", u.at.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pol.Tools, []string{"repo.read", "repo.search"}) || pol.Limits.MaxTokens == nil || *pol.Limits.MaxTokens != 200000 ||
			pol.Model.Name != "t030-model" || pol.Profile.Version != 1 {
			t.Fatalf("pinned policy: %+v", pol)
		}

		// The stored versions are independent records.
		first, err := w.svc.Stored(ctx, scope, "coding@1")
		if err != nil || first.Content.Draft().Instructions != wantInstructions || first.Digest != v1.Digest {
			t.Fatalf("version one must be unchanged by version two: %v", err)
		}
	})

	t.Run("admission pins exactly the named version and refuses moving references", func(t *testing.T) {
		w := newWorld(t)
		v1 := w.register(t, w.draft)
		edit := w.draft
		edit.Instructions += " Version two."
		v2 := w.register(t, edit)
		if v2.Version != 2 {
			t.Fatal("fixture")
		}
		r, err := adapters.NewResolver(w.svc)
		if err != nil {
			t.Fatal(err)
		}
		exp, err := adapters.NewExpander(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			ref  string
			want string
		}{{v1.Ref(), w.draft.Instructions}, {v2.Ref(), edit.Instructions}} {
			out, err := exp.Expand(ctx, planOf(unitStep(tc.ref)))
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := agents.DecodeArgs(out.Aggregate.Steps[0].Spec.Args)
			if err != nil || cfg.Content.Instructions != tc.want {
				t.Fatalf("%s must pin its own instructions: %v", tc.ref, err)
			}
		}
		for _, alias := range []string{"coding", "coding@latest", "coding@0"} {
			if _, err := exp.Expand(ctx, planOf(unitStep(alias))); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%q is a moving reference and must not be admitted: %v", alias, err)
			}
		}
		// Re-registering the original content converges on version one.
		again, err := w.svc.Register(ctx, agents.RegisterCommand{Scope: scope, Caller: agents.Caller{PrincipalID: engineerID}, Draft: w.draft})
		if err != nil || again.Created || again.Profile.Version != 1 || again.Profile.ID != v1.ID {
			t.Fatalf("identical content is the existing version: %+v %v", again, err)
		}
		if got := len(w.repo.Versions()); got != 2 {
			t.Fatalf("%d stored versions", got)
		}
	})

	t.Run("mutating a draft, a returned content or an expansion changes nothing stored", func(t *testing.T) {
		w := newWorld(t)
		d := w.draft
		p := w.register(t, d)
		canonical := string(p.Content.Canonical())

		// The caller's draft shares memory with what it registered.
		d.Tools[0] = "evil.tool"
		*d.Limits.MaxTokens = 1
		d.Wrapper.Command[0] = "/bin/evil"
		d.DesignBaseline.Requirements[0] = "AR-99"

		// So does every copy handed back.
		got := p.Content.Draft()
		got.Tools[0], got.Wrapper.Command[0] = "other", "/bin/other"
		*got.Limits.MaxTokens = 2
		stored, err := w.svc.Stored(ctx, scope, p.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if string(stored.Content.Canonical()) != canonical || stored.Digest != p.Digest {
			t.Fatal("stored content changed through an alias")
		}
		cc := stored.Content.Canonical()
		cc[0] = '!'
		if string(stored.Content.Canonical()) != canonical {
			t.Fatal("the canonical bytes must be a copy")
		}

		r, _ := adapters.NewResolver(w.svc)
		first, err := r.ResolveAgent(ctx, scope, unitStep(p.Ref()))
		if err != nil {
			t.Fatal(err)
		}
		first.Command[0], first.Args[0] = "/bin/evil", "evil"
		second, err := r.ResolveAgent(ctx, scope, unitStep(p.Ref()))
		if err != nil || second.Command[0] != agentstest.Command[0] || second.Args[0] != agents.PayloadMarker {
			t.Fatalf("an expansion is a copy: %+v %v", second, err)
		}
	})

	t.Run("the design baseline is immutable and its digest is checked", func(t *testing.T) {
		w := newWorld(t)
		for name, mutate := range map[string]func(*agents.Draft){
			"a branch instead of a commit": func(d *agents.Draft) { d.DesignBaseline.Sources[0].Revision = "main" },
			"a tag instead of a commit":    func(d *agents.Draft) { d.DesignBaseline.Sources[0].Revision = "v1.0.0" },
			"an abbreviated commit":        func(d *agents.Draft) { d.DesignBaseline.Sources[0].Revision = "3030303" },
			"a copied digest beside edited references": func(d *agents.Draft) {
				d.DesignBaseline.Requirements = append(slices.Clone(d.DesignBaseline.Requirements), "AR-02")
			},
			"no baseline at all": func(d *agents.Draft) { d.DesignBaseline = runtimev1.DesignBaseline{} },
		} {
			d := w.draft
			d.DesignBaseline = cloneBaseline(d.DesignBaseline)
			mutate(&d)
			_, err := w.svc.Register(ctx, agents.RegisterCommand{Scope: scope, Caller: agents.Caller{PrincipalID: engineerID}, Draft: d})
			if !errors.Is(err, agents.ErrInvalid) {
				t.Errorf("%s must be refused: %v", name, err)
			}
		}
		if n := len(w.repo.Versions()); n != 0 {
			t.Fatalf("a refused baseline stored %d versions", n)
		}
	})

	t.Run("conformance evidence is bound to the pinned baseline and the candidate", func(t *testing.T) {
		w := newWorld(t)
		v1 := w.register(t, w.draft)
		u := admit(t, w, v1)
		pin := u.ctx.DesignBaseline
		if pin == nil || pin.Digest != w.draft.DesignBaseline.Digest {
			t.Fatalf("the attempt context carries the pinned baseline: %+v", pin)
		}

		candidate := runtimev1.GitCommit("5050505050505050505050505050505050505050")
		report := runtimev1.ConformanceReport{
			BaselineDigest: pin.Digest, CandidateCommit: candidate, Requirements: []string{"AR-01", "AR-14"},
			Checks: []runtimev1.ConformanceCheck{
				{ID: "module-boundaries", Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: []domain.ArtifactID{"art_t030_e1"}},
				{ID: "tenant-authority", Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: []domain.ArtifactID{"art_t030_e2"}},
			},
			UnresolvedDeviations: []runtimev1.Deviation{}, Disposition: runtimev1.Conforms,
		}
		if err := runtimev1.CheckConformance(report, *pin, candidate); err != nil {
			t.Fatalf("a current, complete report: %v", err)
		}

		// A later design change gives a later baseline; the old report is not
		// current evidence for it (SD-A06), and neither is it for a new candidate.
		edit := w.draft
		moved, _ := runtimev1.NewDesignBaseline([]string{"AR-01"}, []string{"P-01"},
			[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/kodama.git", Revision: "4040404040404040404040404040404040404040", Path: "docs/specs/agent-runtime.md"}},
			[]string{"tenant-authority"})
		edit.DesignBaseline = moved
		v2 := w.register(t, edit)
		next := admit(t, w, v2)
		if next.ctx.DesignBaseline.Digest == pin.Digest {
			t.Fatal("a moved baseline is a different pin")
		}
		for name, tc := range map[string]struct {
			baseline  runtimev1.DesignBaseline
			candidate runtimev1.GitCommit
			code      domain.ErrorCode
		}{
			"another baseline":  {*next.ctx.DesignBaseline, candidate, runtimev1.CodeConformanceBaselineMismatch},
			"another candidate": {*pin, "6060606060606060606060606060606060606060", runtimev1.CodeConformanceCandidateMismatch},
		} {
			var ce *runtimev1.ContractError
			if err := runtimev1.CheckConformance(report, tc.baseline, tc.candidate); !errors.As(err, &ce) || ce.Code != tc.code {
				t.Errorf("%s: %v, want %s", name, err, tc.code)
			}
		}
		// An unproven or deviating report is blocked, never conforming by assertion.
		blocked := report
		blocked.Checks = slices.Clone(report.Checks)
		blocked.Checks[1] = runtimev1.ConformanceCheck{ID: "tenant-authority", Outcome: runtimev1.CheckNotRun, EvidenceArtifactIDs: []domain.ArtifactID{}}
		blocked.Disposition = runtimev1.DispBlocked
		var ce *runtimev1.ContractError
		if err := runtimev1.CheckConformance(blocked, *pin, candidate); !errors.As(err, &ce) || ce.Code != runtimev1.CodeConformanceBlocked {
			t.Fatalf("a check that did not pass blocks: %v", err)
		}
		missing := report
		missing.Checks = report.Checks[:1]
		if err := runtimev1.CheckConformance(missing, *pin, candidate); !errors.As(err, &ce) || ce.Code != runtimev1.CodeConformanceIncomplete {
			t.Fatalf("a missing required check is incomplete: %v", err)
		}
	})
}

func cloneBaseline(b runtimev1.DesignBaseline) runtimev1.DesignBaseline {
	b.Requirements, b.Decisions = slices.Clone(b.Requirements), slices.Clone(b.Decisions)
	b.Sources, b.RequiredChecks = slices.Clone(b.Sources), slices.Clone(b.RequiredChecks)
	return b
}

func unitResult(outcome runtimev1.Outcome) runtimev1.Result {
	r := runtimev1.Result{Outcome: outcome, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	zero := int64(0)
	if outcome == runtimev1.OutcomeCompleted {
		r.ExitCode = &zero
		r.Outputs["summary"] = runtimev1.StringValue("added the inventory total")
	}
	return r
}

const unitHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Test_unit_agent_outcomes: AR-06, AR-13, AR-A04/AR-A05. A completed attempt is
// one whose structured result passed the runtime contract; prose, an exit code,
// an interruption or a missing result never complete it; needs_input is a
// failed attempt that keeps its question and is not retried.
func Test_unit_agent_outcomes(t *testing.T) {
	newAttempt := func(t *testing.T) *unitAttempt {
		t.Helper()
		w := newWorld(t)
		u := admit(t, w, w.register(t, w.draft))
		u.start(t)
		return u
	}

	t.Run("a valid structured result completes the attempt and the step", func(t *testing.T) {
		u := newAttempt(t)
		res := unitResult(runtimev1.OutcomeCompleted)
		if err := runtimev1.CheckResult(res, u.ctx, nil); err != nil {
			t.Fatalf("the result is valid: %v", err)
		}
		if err := u.agg.CompleteAttempt(u.at, unitHash, res, unitNow); err != nil {
			t.Fatal(err)
		}
		m := u.metadata(t)
		if m.Status != execution.AttemptSucceeded || m.Outcome != runtimev1.OutcomeCompleted || m.Question != "" || m.Profile.Version != 1 {
			t.Fatalf("metadata: %+v", m)
		}
		if s := u.agg.Step("implement"); s.Status != execution.StepSucceeded || s.Outputs["summary"].String() != "added the inventory total" {
			t.Fatalf("step: %s %v", s.Status, s.Outputs)
		}
	})

	t.Run("prose, a bare exit code or a wrong output is not completion", func(t *testing.T) {
		u := newAttempt(t)
		zero := int64(0)
		for name, res := range map[string]runtimev1.Result{
			"exit zero without the declared output": {Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
			"an undeclared output": {Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, DiagnosticArtifactIDs: []domain.ArtifactID{},
				Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("x"), "extra": runtimev1.StringValue("y")}},
			"an output of the wrong type": {Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, DiagnosticArtifactIDs: []domain.ArtifactID{},
				Outputs: map[string]runtimev1.Value{"summary": runtimev1.BoolValue(true)}},
			"completed with a nonzero exit": {Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(2)), Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("x")}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
			"completed with a question": {Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Question: ptr("which?"), DiagnosticArtifactIDs: []domain.ArtifactID{},
				Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("x")}},
			"an unknown outcome": {Outcome: "interrupted", Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
		} {
			if err := runtimev1.CheckResult(res, u.ctx, nil); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		// Nothing was committed: the attempt and the step are exactly as they were.
		m := u.metadata(t)
		if m.Status != execution.AttemptRunning || m.Outcome != "" || m.Question != "" || u.agg.Step("implement").Status == execution.StepSucceeded {
			t.Fatalf("a refused result must not complete anything: %+v", m)
		}
		if err := runtimev1.CheckResult(unitResult(runtimev1.OutcomeCompleted), u.ctx, nil); err != nil {
			t.Fatalf("the correct result is still accepted: %v", err)
		}
	})

	t.Run("needs_input fails the attempt, keeps the question and is not retried", func(t *testing.T) {
		u := newAttempt(t)
		res := unitResult(runtimev1.OutcomeNeedsInput)
		q := "Which currency does the inventory total use?"
		res.Question = &q
		if err := runtimev1.CheckResult(res, u.ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := u.agg.CompleteAttempt(u.at, unitHash, res, unitNow); err != nil {
			t.Fatal(err)
		}
		m := u.metadata(t)
		if m.Status != execution.AttemptFailed || m.Reason != execution.ReasonNeedsInput || m.Outcome != runtimev1.OutcomeNeedsInput || m.Question != q {
			t.Fatalf("metadata: %+v", m)
		}
		s := u.agg.Step("implement")
		if s.Status == execution.StepSucceeded || s.OutputCommit != "" || len(s.Outputs) != 0 {
			t.Fatalf("a question exposes no candidate or output: %s %q %v", s.Status, s.OutputCommit, s.Outputs)
		}
		if u.agg.Run.AcceptsDispatch() {
			t.Fatal("needs_input must stop dispatch: it is never retried automatically")
		}
		// Blank or missing questions are not needs_input.
		for name, bad := range map[string]*string{"missing": nil, "blank": ptr("   ")} {
			r := unitResult(runtimev1.OutcomeNeedsInput)
			r.Question = bad
			if err := runtimev1.CheckResult(r, u.ctx, nil); err == nil {
				t.Errorf("a %s question was accepted", name)
			}
		}
	})

	t.Run("a reported failure is a failure with its reason", func(t *testing.T) {
		u := newAttempt(t)
		for _, reason := range []string{"interrupted", "model_budget_exhausted"} {
			r := unitResult(runtimev1.OutcomeFailed)
			r.ExitCode, r.Reason = ptr(int64(1)), ptr(reason)
			if err := runtimev1.CheckResult(r, u.ctx, nil); err != nil {
				t.Fatalf("%s: %v", reason, err)
			}
		}
		r := unitResult(runtimev1.OutcomeFailed)
		r.ExitCode, r.Reason = ptr(int64(1)), ptr("interrupted")
		if err := u.agg.CompleteAttempt(u.at, unitHash, r, unitNow); err != nil {
			t.Fatal(err)
		}
		m := u.metadata(t)
		if m.Status != execution.AttemptFailed || m.Outcome != runtimev1.OutcomeFailed || m.Question != "" {
			t.Fatalf("metadata: %+v", m)
		}
		if s := u.agg.Step("implement"); s.Status == execution.StepSucceeded || len(s.Outputs) != 0 {
			t.Fatalf("a failed attempt succeeded its step: %s", s.Status)
		}
		// Even a late completed result cannot rewrite the failure.
		if err := u.agg.CompleteAttempt(u.at, unitHash, unitResult(runtimev1.OutcomeCompleted), unitNow); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a result after the outcome: %v", err)
		}
		if m := u.metadata(t); m.Status != execution.AttemptFailed || m.Outcome != runtimev1.OutcomeFailed {
			t.Fatalf("the outcome is immutable: %+v", m)
		}
	})

	t.Run("an interruption leaves no outcome and a late success cannot resurrect it", func(t *testing.T) {
		u := newAttempt(t)
		if !u.agg.RequestCancel(execution.ReasonCancelled, unitNow) {
			t.Fatal("the cancellation was not recorded")
		}
		m := u.metadata(t)
		if m.Status != execution.AttemptCancelled || m.Reason != execution.ReasonCancelled || m.Outcome != "" || m.Question != "" {
			t.Fatalf("an interrupted attempt invents no outcome: %+v", m)
		}
		if err := u.agg.CompleteAttempt(u.at, unitHash, unitResult(runtimev1.OutcomeCompleted), unitNow.Add(time.Second)); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("the cancelled attempt accepted a completion: %v", err)
		}
		if s := u.agg.Step("implement"); s.Status == execution.StepSucceeded || len(s.Outputs) != 0 {
			t.Fatalf("a late result completed the step: %s", s.Status)
		}
		if m := u.metadata(t); m.Status != execution.AttemptCancelled || m.Outcome != "" {
			t.Fatalf("metadata after the late result: %+v", m)
		}
	})

	t.Run("a deadline is a failure without an outcome", func(t *testing.T) {
		u := newAttempt(t)
		u.at.ExecutionDeadline = ptr(unitNow.Add(time.Minute))
		if failed := u.agg.CheckDeadlines(unitNow.Add(2 * time.Minute)); len(failed) != 1 {
			t.Fatalf("deadline failures: %d", len(failed))
		}
		m := u.metadata(t)
		if m.Status != execution.AttemptFailed || m.Reason != execution.ReasonTimeout || m.Outcome != "" {
			t.Fatalf("a timeout invents no result: %+v", m)
		}
	})

	t.Run("metadata is scoped to the run, step and tenant of the attempt", func(t *testing.T) {
		u := newAttempt(t)
		other := domain.Scope{TenantID: "t030_globex", ProjectID: scope.ProjectID}
		for name, call := range map[string]func() error{
			"another tenant": func() error {
				_, err := u.meta.MetadataOf(context.Background(), other, u.agg.Run.ID, "implement", u.at.ID)
				return err
			},
			"another run": func() error {
				_, err := u.meta.MetadataOf(context.Background(), scope, "run_other", "implement", u.at.ID)
				return err
			},
			"another step": func() error {
				_, err := u.meta.MetadataOf(context.Background(), scope, u.agg.Run.ID, "verify", u.at.ID)
				return err
			},
		} {
			if err := call(); !errors.Is(err, agents.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("an attempt whose context lacks the agent payload is not an agent attempt", func(t *testing.T) {
		u := newAttempt(t)
		plain := execution.Attempt{Scope: scope, ID: "att_plain", RunID: u.agg.Run.ID, StepID: "implement"}
		plainCtx := contextOf(agentstest.Image, agentstest.Command, []string{"go", "test"})
		plainCtx.AttemptID, plainCtx.RunID = plain.ID, plain.RunID
		wp, err := execution.DefaultPlanner{}.Plan(scope.TenantID, plainCtx)
		if err != nil {
			t.Fatal(err)
		}
		plain.Context = wp.Context
		src := fixedSource{plain}
		meta, _ := adapters.NewMetadataReader(src)
		if _, err := meta.MetadataOf(context.Background(), scope, plain.RunID, "implement", plain.ID); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("a container attempt is not found as an agent attempt: %v", err)
		}
	})
}

type fixedSource struct{ at execution.Attempt }

func (f fixedSource) Attempt(context.Context, domain.Scope, domain.RunID, string, domain.AttemptID) (execution.Attempt, error) {
	return f.at, nil
}

func ptr[T any](v T) *T { return &v }
