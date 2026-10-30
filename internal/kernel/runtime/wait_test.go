package runtime_test

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
)

// openWaitFor returns an open wait of its own run and identity for the given
// candidate correlation, opened at t0.
func openWaitFor(t *testing.T, id rt.WaitID, run domain.RunID, candidate string) rt.Wait {
	t.Helper()
	w, err := rt.NewWait(scope, id, run, "review", reviewSpec())
	must(t, err)
	must(t, w.Open(t0, map[string]string{"candidate": candidate}, ""))
	return w
}

// protectedPlan is implement -> review (wait) -> integrate (protected).
func protectedPlan(mutate func(*rt.WaitSpec, *rt.PlanStep)) rt.Plan {
	spec := rt.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "implement.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected", "needs_work"}, Approving: "approved",
		Timeout: time.Hour, OnExpiry: rt.ExpiryContinue}
	integrate := rt.PlanStep{Key: "integrate", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"},
		Authority: rt.AuthorityIntegrate, Requires: []rt.Requirement{{Wait: "review", Outcome: "approved"}},
		Workload: &rt.WorkloadSpec{Image: "integrator:1", Commands: []string{"integrate-candidate"}}}
	if mutate != nil {
		mutate(&spec, &integrate)
	}
	return rt.Plan{Digest: "sha256:p", Steps: []rt.PlanStep{
		{Key: "implement", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "agent:1", Commands: []string{"agent"}}},
		{Key: "review", Kind: rt.StepWait, DependsOn: []domain.StepID{"implement"}, Wait: &spec},
		integrate,
	}}
}

// testStepKindsAndWaits covers the removal of the condition kind and the wait
// outcome rules.
func testStepKindsAndWaits(t *testing.T) {
	t.Run("there is no condition step kind", func(t *testing.T) {
		for _, k := range []rt.StepKind{rt.StepContainer, rt.StepWait} {
			if !k.Valid() {
				t.Errorf("%s must be valid", k)
			}
		}
		if rt.StepKind("condition").Valid() || rt.StepKind("").Valid() {
			t.Error("condition or zero kind valid")
		}
		if _, err := rt.NewStep("gate", "condition"); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("a condition step: %v", err)
		}
		p := mustPlan(t)
		p.Steps[2] = rt.PlanStep{Key: "gate", Kind: "condition", DependsOn: []domain.StepID{"review"}, When: `review.outcome == "approved"`}
		if err := p.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("a plan with a condition step: %v", err)
		}
		// Conditional eligibility is a when predicate on a container or a wait step.
		w := mustPlan(t)
		w.Steps[1].When = `build.status == "succeeded"`
		must(t, w.Validate())
		must(t, mustPlan(t).Validate())
		if m := mustPlan(t).Steps[3]; m.When == "" || m.Kind != rt.StepContainer {
			t.Error("the fixture's conditional step is a container step with when")
		}
	})

	t.Run("a wait declares its outcomes, at most one approving outcome and bounds", func(t *testing.T) {
		ok := func() rt.WaitSpec { return reviewSpec() }
		must(t, ok().Validate())
		// A wait without an approving outcome is valid: it is just data to branch on.
		clarify := rt.WaitSpec{Assign: "engineer", Input: "answer", Outcomes: []string{"answered"}, Timeout: 24 * time.Hour}
		must(t, clarify.Validate())
		if _, has := clarify.ApprovingOutcome(); has {
			t.Error("clarify declares no approving outcome")
		}
		if got, has := ok().ApprovingOutcome(); !has || got != "approved" {
			t.Errorf("approving %q %v", got, has)
		}
		for name, mutate := range map[string]func(*rt.WaitSpec){
			"neither assign nor correlate": func(w *rt.WaitSpec) { w.Assign, w.Correlate = "", nil },
			"no outcomes":                  func(w *rt.WaitSpec) { w.Outcomes, w.Approving = nil, "" },
			"nine outcomes": func(w *rt.WaitSpec) {
				w.Outcomes = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
				w.Approving = "a"
			},
			"reserved expired":         func(w *rt.WaitSpec) { w.Outcomes = []string{"approved", "expired"} },
			"reserved cancelled":       func(w *rt.WaitSpec) { w.Outcomes = []string{"approved", "cancelled"} },
			"duplicate outcome":        func(w *rt.WaitSpec) { w.Outcomes = []string{"approved", "approved"} },
			"malformed outcome":        func(w *rt.WaitSpec) { w.Outcomes = []string{"Approved", "rejected"} },
			"approving not declared":   func(w *rt.WaitSpec) { w.Approving = "accepted" },
			"approving expired":        func(w *rt.WaitSpec) { w.Approving = "expired" },
			"no timeout":               func(w *rt.WaitSpec) { w.Timeout = 0 },
			"timeout under a minute":   func(w *rt.WaitSpec) { w.Timeout = time.Minute - 1 },
			"timeout over thirty days": func(w *rt.WaitSpec) { w.Timeout = rt.MaxWaitTimeout + 1 },
			"bad on_expiry":            func(w *rt.WaitSpec) { w.OnExpiry = "retry" },
			"bad input kind":           func(w *rt.WaitSpec) { w.Input = "Approval!" },
			"five correlation keys": func(w *rt.WaitSpec) {
				w.Correlate = map[string]string{"a": "x", "b": "x", "c": "x", "d": "x", "e": "x"}
			},
			"bad correlation key": func(w *rt.WaitSpec) { w.Correlate = map[string]string{"Candidate": "x"} },
			"empty reference":     func(w *rt.WaitSpec) { w.Correlate = map[string]string{"candidate": ""} },
		} {
			w := ok()
			mutate(&w)
			if err := w.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for _, d := range []time.Duration{rt.MinWaitTimeout, rt.MaxWaitTimeout} {
			w := ok()
			w.Timeout = d
			must(t, w.Validate())
		}
		a, b := ok(), ok()
		if !a.Equal(b) {
			t.Error("equal specs")
		}
		b.Correlate = map[string]string{"candidate": "other"}
		if a.Equal(b) {
			t.Error("different correlation compared equal")
		}
	})

	t.Run("a protected step can require only the approving outcome of a preceding wait", func(t *testing.T) {
		must(t, protectedPlan(nil).Validate())
		// A transitive prerequisite is enough.
		deep := protectedPlan(nil)
		deep.Steps = append(deep.Steps[:2:2],
			rt.PlanStep{Key: "test", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, Workload: deep.Steps[0].Workload},
			deep.Steps[2])
		deep.Steps[3].DependsOn = []domain.StepID{"test"}
		must(t, deep.Validate())

		for name, mutate := range map[string]func(*rt.WaitSpec, *rt.PlanStep){
			"requires a rejection":           func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Outcome = "rejected" },
			"requires another outcome":       func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Outcome = "needs_work" },
			"requires expired":               func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Outcome = "expired" },
			"requires an undeclared outcome": func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Outcome = "accepted" },
			"wait declares no approving":     func(w *rt.WaitSpec, _ *rt.PlanStep) { w.Approving = "" },
			"requires a container":           func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Wait = "implement" },
			"requires an unknown step":       func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Wait = "nowhere" },
			"requires a later step":          func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Wait = "integrate" },
			"not a prerequisite":             func(_ *rt.WaitSpec, s *rt.PlanStep) { s.DependsOn = []domain.StepID{"implement"} },
			"no candidate correlation":       func(w *rt.WaitSpec, _ *rt.PlanStep) { w.Correlate = nil },
			"authority without requires":     func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires = nil },
			"unknown authority":              func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Authority = "deploy" },
			"requires without authority":     func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Authority = "" },
			"five requirements": func(_ *rt.WaitSpec, s *rt.PlanStep) {
				s.Requires = append(s.Requires, s.Requires[0], s.Requires[0], s.Requires[0], s.Requires[0])
			},
		} {
			if err := protectedPlan(mutate).Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// The error names the step and the outcome.
		err := protectedPlan(func(_ *rt.WaitSpec, s *rt.PlanStep) { s.Requires[0].Outcome = "rejected" }).Validate()
		if err == nil || !strings.Contains(err.Error(), "integrate") || !strings.Contains(err.Error(), `"rejected"`) {
			t.Errorf("the refusal names step and outcome: %v", err)
		}
		// A wait step is never protected.
		p := protectedPlan(nil)
		p.Steps[1].Authority = rt.AuthorityIntegrate
		p.Steps[1].Requires = []rt.Requirement{{Wait: "review", Outcome: "approved"}}
		if p.Validate() == nil {
			t.Error("a protected wait step accepted")
		}
	})

	t.Run("recorded state alone decides whether requirements are met", func(t *testing.T) {
		plan := protectedPlan(nil)
		type scenario struct {
			name string
			want bool
			make func(t *testing.T) (rt.Run, rt.Wait)
		}
		build := func(t *testing.T, drive func(r *rt.Run, w *rt.Wait)) (rt.Run, rt.Wait) {
			r, err := rt.NewRun(scope, "run1", "pipe1", plan, 1)
			must(t, err)
			must(t, r.AdvanceStep("implement", rt.StepRunning, rt.ReasonNone))
			must(t, r.FinishStep("implement", rt.StepSucceeded, rt.ReasonNone, okRes))
			w, err := rt.NewWait(scope, "w1", "run1", "review", *plan.Steps[1].Wait)
			must(t, err)
			drive(&r, &w)
			return r, w
		}
		openIt := func(r *rt.Run, w *rt.Wait) {
			must(t, w.Open(t0, map[string]string{"candidate": "c1"}, ""))
			must(t, r.AdvanceStep("review", rt.StepWaiting, rt.ReasonNone))
		}
		resolve := func(outcome string) func(*rt.Run, *rt.Wait) {
			return func(r *rt.Run, w *rt.Wait) {
				openIt(r, w)
				req := signalReq(*w, "k")
				req.Outcome = outcome
				_, err := w.Resolve(req, t0.Add(minute))
				must(t, err)
				must(t, r.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
			}
		}
		for _, sc := range []scenario{
			{"approving outcome", true, func(t *testing.T) (rt.Run, rt.Wait) { return build(t, resolve("approved")) }},
			{"rejection", false, func(t *testing.T) (rt.Run, rt.Wait) { return build(t, resolve("rejected")) }},
			{"another declared outcome", false, func(t *testing.T) (rt.Run, rt.Wait) { return build(t, resolve("needs_work")) }},
			{"expired and continued", false, func(t *testing.T) (rt.Run, rt.Wait) {
				return build(t, func(r *rt.Run, w *rt.Wait) {
					openIt(r, w)
					_, err := w.Expire(t0.Add(time.Hour))
					must(t, err)
					must(t, r.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
				})
			}},
			{"still open", false, func(t *testing.T) (rt.Run, rt.Wait) { return build(t, openIt) }},
			{"cancelled", false, func(t *testing.T) (rt.Run, rt.Wait) {
				return build(t, func(r *rt.Run, w *rt.Wait) {
					openIt(r, w)
					_, err := w.Cancel(t0, rt.ReasonRunCancelled)
					must(t, err)
					must(t, r.AdvanceStep("review", rt.StepCancelled, rt.ReasonCancelled))
				})
			}},
			{"skipped", false, func(t *testing.T) (rt.Run, rt.Wait) {
				return build(t, func(r *rt.Run, w *rt.Wait) {
					must(t, r.AdvanceStep("review", rt.StepSkipped, rt.ReasonPrerequisiteSkipped))
				})
			}},
			{"failed before opening", false, func(t *testing.T) (rt.Run, rt.Wait) {
				return build(t, func(r *rt.Run, w *rt.Wait) {
					must(t, r.AdvanceStep("review", rt.StepFailed, rt.ReasonCorrelationMissing))
				})
			}},
		} {
			run, wait := sc.make(t)
			rec := rt.RunRecord{Run: run, Plan: plan, Waits: []rt.Wait{wait}}
			err := rec.RequirementsMet("integrate")
			if sc.want != (err == nil) || (!sc.want && !errors.Is(err, rt.ErrRequiresUnmet)) {
				t.Errorf("%s: met=%v err=%v", sc.name, sc.want, err)
			}
			must(t, rt.CheckStepWait(run.Steps[1], wait))
		}
		// A step without authority has no requirements; an unknown step is an error.
		rec := rt.RunRecord{Plan: plan}
		must(t, rec.RequirementsMet("implement"))
		if rec.RequirementsMet("nope") == nil {
			t.Error("unknown step")
		}
		// A missing wait record never satisfies a requirement.
		if err := (rt.RunRecord{Run: mustRun(t), Plan: plan}).RequirementsMet("integrate"); !errors.Is(err, rt.ErrRequiresUnmet) {
			t.Errorf("no wait record: %v", err)
		}
	})

	t.Run("step and wait must agree", func(t *testing.T) {
		pending, _ := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
		open := openWait(t)
		resolved := openWait(t)
		_, err := resolved.Resolve(signalReq(resolved, "k"), t0.Add(minute))
		must(t, err)
		expiredFail := openWait(t)
		_, err = expiredFail.Expire(t0.Add(time.Hour))
		must(t, err)
		contSpec := reviewSpec()
		contSpec.OnExpiry = rt.ExpiryContinue
		expiredCont, _ := rt.NewWait(scope, "w1", "run1", "review", contSpec)
		must(t, expiredCont.Open(t0, map[string]string{"candidate": "c1"}, ""))
		_, err = expiredCont.Expire(t0.Add(time.Hour))
		must(t, err)
		cancelledPending, _ := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
		_, err = cancelledPending.Cancel(t0, rt.ReasonRunFailed)
		must(t, err)
		cancelledOpen := openWait(t)
		_, err = cancelledOpen.Cancel(t0, rt.ReasonRunCancelled)
		must(t, err)

		const (
			P, W, S = rt.StepPending, rt.StepWaiting, rt.StepSucceeded
			F, K, C = rt.StepFailed, rt.StepSkipped, rt.StepCancelled
		)
		for _, tc := range []struct {
			step rt.StepState
			wait rt.Wait
			ok   bool
		}{
			{P, pending, true}, {P, open, false},
			{W, open, true}, {W, pending, false}, {W, resolved, false},
			{S, resolved, true}, {S, expiredCont, true}, {S, expiredFail, false}, {S, open, false}, {S, cancelledOpen, false},
			{F, expiredFail, true}, {F, cancelledOpen, true}, {F, pending, true}, {F, resolved, false}, {F, open, false},
			{K, pending, true}, {K, cancelledPending, true}, {K, resolved, false}, {K, open, false},
			{C, cancelledOpen, true}, {C, cancelledPending, true}, {C, resolved, false}, {C, open, false},
		} {
			step := rt.Step{Key: "review", Kind: rt.StepWait, State: tc.step, Version: 1}
			if tc.step == F || tc.step == K || tc.step == C {
				step.Reason = rt.ReasonRunFailed
			}
			if got := rt.CheckStepWait(step, tc.wait) == nil; got != tc.ok {
				t.Errorf("step %s with wait %s: agree=%v want %v", tc.step, tc.wait.State, got, tc.ok)
			}
		}
		cs, _ := rt.NewStep("build", rt.StepContainer)
		if rt.CheckStepWait(cs, pending) == nil {
			t.Error("a container step has no wait")
		}
	})
}

// testWaitDecisions proves a wait takes exactly one decision.
func testWaitDecisions(t *testing.T) {
	t.Run("opening records the values read and fixes the deadline", func(t *testing.T) {
		w, err := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
		must(t, err)
		if w.Assignment != "reviewer" || w.State != rt.WaitPending || w.Version != 1 {
			t.Fatalf("pending wait %+v", w)
		}
		before := w
		for name, corr := range map[string]map[string]string{
			"missing key": nil, "extra key": {"candidate": "c1", "other": "x"}, "wrong key": {"commit": "c1"}, "empty value": {"candidate": ""},
		} {
			if err := w.Open(t0, corr, ""); !errors.Is(err, domain.ErrInvalidValue) || !reflect.DeepEqual(w, before) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := w.Open(t0, map[string]string{"candidate": "c1"}, strings.Repeat("x", rt.MaxRequestBytes+1)); err == nil {
			t.Error("oversize request accepted")
		}
		must(t, w.Open(t0, map[string]string{"candidate": "c1"}, "please review"))
		if w.State != rt.WaitOpen || w.Version != 2 || !w.Deadline.Equal(t0.Add(time.Hour)) || w.Request != "please review" || w.Correlation["candidate"] != "c1" {
			t.Fatalf("open wait %+v", w)
		}
		if err := w.Open(t0, map[string]string{"candidate": "c1"}, ""); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("reopen: %v", err)
		}
	})

	t.Run("one resolution decides and later requests change nothing", func(t *testing.T) {
		w := openWait(t)
		d, err := w.Resolve(signalReq(w, "k1"), t0.Add(minute))
		must(t, err)
		want := rt.Decision{Kind: rt.DecisionResolved, SignalID: signalReq(w, "k1").Identity.ID(), Outcome: "approved",
			Answer: "looks good", Actor: "user:alice", Version: 3, At: t0.Add(minute)}
		if d != want || w.Decision != want || w.State != rt.WaitResolved || w.Version != 3 {
			t.Fatalf("decision %+v wait %+v", d, w)
		}
		must(t, w.Validate())
		before := w
		late := signalReq(w, "k2")
		late.Outcome = "rejected"
		if _, err := w.Resolve(late, t0.Add(2*minute)); !errors.Is(err, rt.ErrWaitClosed) || !reflect.DeepEqual(w, before) {
			t.Errorf("second resolution: %v", err)
		}
		if _, err := w.Expire(t0.Add(2 * time.Hour)); !errors.Is(err, rt.ErrWaitClosed) || !reflect.DeepEqual(w, before) {
			t.Errorf("expiry after resolution: %v", err)
		}
		if _, err := w.Cancel(t0, rt.ReasonRunCancelled); !errors.Is(err, rt.ErrWaitClosed) || !reflect.DeepEqual(w, before) {
			t.Errorf("cancellation after resolution: %v", err)
		}
	})

	t.Run("a refused resolution leaves the wait untouched", func(t *testing.T) {
		stale := func(r *rt.SignalRequest) { r.ExpectedVersion = 1 }
		for name, tc := range map[string]struct {
			mutate func(*rt.SignalRequest)
			now    time.Time
			want   error
		}{
			"stale version":      {stale, t0.Add(minute), rt.ErrWaitStale},
			"undeclared outcome": {func(r *rt.SignalRequest) { r.Outcome = "maybe" }, t0.Add(minute), rt.ErrInvalidOutcome},
			"reserved outcome":   {func(r *rt.SignalRequest) { r.Outcome = "expired" }, t0.Add(minute), rt.ErrInvalidOutcome},
			"at the deadline":    {func(*rt.SignalRequest) {}, t0.Add(time.Hour), rt.ErrWaitExpired},
			"after the deadline": {func(*rt.SignalRequest) {}, t0.Add(2 * time.Hour), rt.ErrWaitExpired},
			"malformed outcome":  {func(r *rt.SignalRequest) { r.Outcome = "Approved!" }, t0.Add(minute), domain.ErrInvalidValue},
			"oversize answer":    {func(r *rt.SignalRequest) { r.Answer = strings.Repeat("a", rt.MaxAnswerBytes+1) }, t0.Add(minute), domain.ErrInvalidValue},
			"no actor":           {func(r *rt.SignalRequest) { r.Actor = "" }, t0.Add(minute), domain.ErrInvalidValue},
		} {
			w := openWait(t)
			before := w
			req := signalReq(w, "k")
			tc.mutate(&req)
			if _, err := w.Resolve(req, tc.now); !errors.Is(err, tc.want) || !reflect.DeepEqual(w, before) {
				t.Errorf("%s: %v", name, err)
			}
		}
		p, _ := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
		req := signalReq(p, "k")
		req.ExpectedVersion = 0
		if _, err := p.Resolve(req, t0); !errors.Is(err, rt.ErrWaitNotOpen) {
			t.Errorf("unopened: %v", err)
		}
	})

	t.Run("expiry follows on_expiry and happens only at the deadline", func(t *testing.T) {
		w := openWait(t)
		if _, err := w.Expire(t0.Add(time.Hour - 1)); !errors.Is(err, rt.ErrWaitNotDue) {
			t.Fatalf("early expiry: %v", err)
		}
		d, err := w.Expire(t0.Add(time.Hour))
		must(t, err)
		if d.Kind != rt.DecisionExpired || d.Outcome != "" || w.State != rt.WaitExpired || w.Decision != d || d.Version != w.Version {
			t.Fatalf("fail expiry %+v", d)
		}
		must(t, w.Validate())

		spec := reviewSpec()
		spec.OnExpiry = rt.ExpiryContinue
		c, _ := rt.NewWait(scope, "w1", "run1", "review", spec)
		must(t, c.Open(t0, map[string]string{"candidate": "c1"}, ""))
		d, err = c.Expire(t0.Add(time.Hour))
		must(t, err)
		if d.Outcome != rt.WaitOutcomeExpired {
			t.Fatalf("continue expiry records the reserved outcome: %+v", d)
		}
		must(t, c.Validate())
		// An expired decision cannot carry the wrong outcome for its on_expiry.
		c.Decision.Outcome = ""
		if c.Validate() == nil {
			t.Error("continue expiry without its outcome accepted")
		}
	})

	t.Run("cancellation decides a pending or open wait with a run reason", func(t *testing.T) {
		for _, open := range []bool{false, true} {
			w, _ := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
			if open {
				must(t, w.Open(t0, map[string]string{"candidate": "c1"}, ""))
			}
			if _, err := w.Cancel(t0, rt.Reason("operator")); err == nil || w.State == rt.WaitCancelled {
				t.Errorf("open=%v: an unrelated reason accepted", open)
			}
			d, err := w.Cancel(t0.Add(minute), rt.ReasonRunFailed)
			must(t, err)
			if d.Kind != rt.DecisionCancelled || d.Reason != rt.ReasonRunFailed || w.State != rt.WaitCancelled || d.Version != w.Version {
				t.Errorf("open=%v: %+v", open, d)
			}
			must(t, w.Validate())
		}
	})

	t.Run("a wait whose fields disagree with its decision is invalid", func(t *testing.T) {
		decided := func() rt.Wait {
			w := openWait(t)
			_, err := w.Resolve(signalReq(w, "k"), t0.Add(minute))
			must(t, err)
			return w
		}
		for name, mutate := range map[string]func(*rt.Wait){
			"decision version":     func(w *rt.Wait) { w.Decision.Version = 1 },
			"undeclared outcome":   func(w *rt.Wait) { w.Decision.Outcome = "maybe" },
			"wrong kind":           func(w *rt.Wait) { w.State = rt.WaitExpired },
			"no actor":             func(w *rt.Wait) { w.Decision.Actor = "" },
			"deadline moved":       func(w *rt.Wait) { w.Deadline = w.Deadline.Add(time.Hour) },
			"correlation changed":  func(w *rt.Wait) { w.Correlation = map[string]string{"commit": "c1"} },
			"correlation dropped":  func(w *rt.Wait) { w.Correlation = nil },
			"open with a decision": func(w *rt.Wait) { w.State = rt.WaitOpen },
			"answer too long":      func(w *rt.Wait) { w.Decision.Answer = strings.Repeat("a", rt.MaxAnswerBytes+1) },
		} {
			w := decided()
			w.Correlation = maps.Clone(w.Correlation)
			mutate(&w)
			if err := w.Validate(); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		p, _ := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
		p.Correlation = map[string]string{"candidate": "c1"}
		if p.Validate() == nil {
			t.Error("a pending wait with values accepted")
		}
	})

	t.Run("event kinds follow the catalog", func(t *testing.T) {
		for got, want := range map[rt.EventKind]string{
			rt.EventWaitOpened: "wait.opened", rt.EventWaitResolved: "wait.resolved", rt.EventWaitExpired: "wait.expired",
			rt.EventWaitCancelled: "wait.cancelled", rt.EventSignalRefused: "signal.refused",
		} {
			if string(got) != want {
				t.Errorf("%q != %q", got, want)
			}
		}
	})
}

// testSignalRecords covers the signal identity, the durable records and routing.
func testSignalRecords(t *testing.T) {
	p2 := domain.Scope{TenantID: "t1", ProjectID: "p2"}

	t.Run("identity is the tuple of tenant, project, caller and key", func(t *testing.T) {
		base := rt.SignalIdentity{Scope: scope, Caller: "user-alice", Key: "key-1"}
		must(t, base.Validate())
		id := base.ID()
		must(t, id.Validate())
		if base.ID() != id || strings.Contains(string(id), "key-1") || strings.Contains(string(id), "alice") {
			t.Fatalf("id %q", id)
		}
		for name, other := range map[string]rt.SignalIdentity{
			"other tenant":  {Scope: domain.Scope{TenantID: "t2", ProjectID: "p1"}, Caller: "user-alice", Key: "key-1"},
			"other project": {Scope: p2, Caller: "user-alice", Key: "key-1"},
			"other caller":  {Scope: scope, Caller: "user-bob", Key: "key-1"},
			"other key":     {Scope: scope, Caller: "user-alice", Key: "key-2"},
			// A tuple boundary shift must not collide.
			"shifted boundary": {Scope: scope, Caller: "user-alic", Key: "ekey-1"},
		} {
			if other.ID() == id {
				t.Errorf("%s shares the signal id", name)
			}
		}
		for name, bad := range map[string]rt.SignalIdentity{
			"no scope": {Caller: "c", Key: "k"}, "no caller": {Scope: scope, Key: "k"}, "no key": {Scope: scope, Caller: "c"},
			"key too long": {Scope: scope, Caller: "c", Key: strings.Repeat("k", rt.MaxSignalKeyLength+1)},
		} {
			if bad.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		edge := rt.SignalIdentity{Scope: scope, Caller: "c", Key: strings.Repeat("é", rt.MaxSignalKeyLength)}
		must(t, edge.Validate())
		if (rt.SignalID("")).Validate() == nil {
			t.Error("empty signal id")
		}
	})

	t.Run("the request digest binds address, outcome and answer", func(t *testing.T) {
		w := openWait(t)
		a := signalReq(w, "k")
		if a.Digest() != signalReq(w, "other-key").Digest() {
			t.Error("the key is not part of the request digest")
		}
		for name, mutate := range map[string]func(*rt.SignalRequest){
			"outcome": func(r *rt.SignalRequest) { r.Outcome = "rejected" },
			"answer":  func(r *rt.SignalRequest) { r.Answer = "other" },
			"wait":    func(r *rt.SignalRequest) { r.WaitID = "w2" },
			"correlation": func(r *rt.SignalRequest) {
				r.WaitID, r.Correlation = "", map[string]string{"candidate": "c1"}
			},
		} {
			b := a
			mutate(&b)
			if b.Digest() == a.Digest() {
				t.Errorf("%s does not change the digest", name)
			}
		}
		c1 := rt.SignalRequest{Correlation: map[string]string{"a": "1", "b": "2"}}
		c2 := rt.SignalRequest{Correlation: map[string]string{"b": "2", "a": "1"}}
		if c1.Digest() != c2.Digest() {
			t.Error("correlation order changes the digest")
		}
		// Actor and expected version are not part of the content.
		b := a
		b.Actor, b.ExpectedVersion = "user:other", 0
		if b.Digest() != a.Digest() {
			t.Error("actor and expected version must not change the digest")
		}
		// Neither an address nor an invalid shape is a request.
		for name, bad := range map[string]rt.SignalRequest{
			"no address":              {Identity: a.Identity, Outcome: "approved", Actor: "a"},
			"5 correlations":          {Identity: a.Identity, Correlation: map[string]string{"a": "1", "b": "1", "c": "1", "d": "1", "e": "1"}, Outcome: "approved", Actor: "a"},
			"negative version":        func() rt.SignalRequest { r := a; r.ExpectedVersion = -1; return r }(),
			"empty correlation value": {Identity: a.Identity, Correlation: map[string]string{"candidate": ""}, Outcome: "approved", Actor: "a"},
		} {
			if bad.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("a winning signal resolves the wait and writes one decision record", func(t *testing.T) {
		w := openWait(t)
		req := signalReq(w, "k1")
		out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(minute))
		must(t, err)
		if !out.Result.Applied || out.Result.Replayed || out.Result.Refusal != "" || out.Wait == nil || out.Record == nil || out.Tombstone != nil {
			t.Fatalf("outcome %+v", out)
		}
		rec := *out.Record
		must(t, rec.Validate())
		d := rec.Decision
		if rec.Target != (rt.SignalTarget{RunID: "run1", StepKey: "review", WaitID: "w1"}) || !rec.Applied ||
			d.Kind != rt.DecisionResolved || d.Outcome != "approved" || d.Answer != "looks good" || d.Actor != "user:alice" ||
			d.Version != out.Wait.Version || !d.At.Equal(t0.Add(minute)) || d.SignalID != req.Identity.ID() || rec.Digest != req.Digest() {
			t.Fatalf("record %+v", rec)
		}
		if out.Wait.State != rt.WaitResolved {
			t.Errorf("updated wait %s", out.Wait.State)
		}
		if w.State != rt.WaitOpen {
			t.Error("routing must not mutate the caller's wait")
		}
	})

	// resolved returns the wait w resolved by alice's key under route, plus the record written.
	resolved := func(t *testing.T) (rt.Wait, rt.SignalRequest, rt.SignalRecord) {
		t.Helper()
		w := openWait(t)
		req := signalReq(w, "k1")
		out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(minute))
		must(t, err)
		return *out.Wait, req, *out.Record
	}

	t.Run("a recorded identity replays its decision whatever the wait now is", func(t *testing.T) {
		_, req, rec := resolved(t)
		prior := rt.SignalLookup{Record: &rec}
		// The wait, step and run records are gone (ordinary retention), a year later.
		later := t0.Add(400 * 24 * time.Hour)
		out, err := rt.RouteSignal(req, prior, nil, nil, later)
		must(t, err)
		want := rec.Result()
		want.Replayed = true
		if out.Result != want || out.Wait != nil || out.Record != nil || out.Tombstone != nil {
			t.Fatalf("replay %+v", out)
		}
		d := out.Result.Decision
		if !out.Result.Applied || d.Outcome != "approved" || d.Answer != "looks good" || d.Actor != "user:alice" || d.Version != 3 || !d.At.Equal(t0.Add(minute)) {
			t.Fatalf("the replay lost part of the decision: %+v", d)
		}
		// Another wait with an identical correlation has opened since; the replay is not rerouted.
		other := openWaitFor(t, "w2", "run2", "c1")
		out, err = rt.RouteSignal(req, prior, []rt.Wait{other}, nil, later)
		must(t, err)
		if out.Result != want || out.Wait != nil || out.Result.Target.WaitID != "w1" {
			t.Fatalf("rerouted: %+v", out)
		}
		// Correlation-addressed replay is also served from the record, not by matching.
		byCorr := req
		byCorr.WaitID, byCorr.Correlation = "", map[string]string{"candidate": "c1"}
		if _, err := rt.RouteSignal(byCorr, prior, []rt.Wait{other}, nil, later); !errors.Is(err, rt.ErrKeyReuse) {
			t.Fatalf("a changed address under a recorded key: %v", err)
		}
	})

	t.Run("a changed request under a recorded key is key reuse and changes nothing", func(t *testing.T) {
		_, req, rec := resolved(t)
		tomb, err := rt.RouteSignal(rt.SignalRequest{Identity: rt.SignalIdentity{Scope: scope, Caller: "user-alice", Key: "t"},
			Correlation: map[string]string{"candidate": "none"}, Outcome: "approved", Actor: "a"}, rt.SignalLookup{}, nil, nil, t0)
		must(t, err)
		priors := map[string]struct {
			req   rt.SignalRequest
			prior rt.SignalLookup
		}{
			"decision record": {req, rt.SignalLookup{Record: &rec}},
			"tombstone": {rt.SignalRequest{Identity: tomb.Tombstone.Identity, Correlation: map[string]string{"candidate": "none"}, Outcome: "approved", Actor: "a"},
				rt.SignalLookup{Tombstone: tomb.Tombstone}},
		}
		for name, tc := range priors {
			for _, change := range []func(*rt.SignalRequest){
				func(r *rt.SignalRequest) { r.Outcome = "rejected" },
				func(r *rt.SignalRequest) { r.Answer = "different" },
			} {
				bad := tc.req
				change(&bad)
				for i := 0; i < 3; i++ { // repeatedly
					out, err := rt.RouteSignal(bad, tc.prior, []rt.Wait{openWait(t)}, nil, t0.Add(2*minute))
					if !errors.Is(err, rt.ErrKeyReuse) || out.Record != nil || out.Tombstone != nil || out.Wait != nil || out.Result != (rt.SignalResult{}) {
						t.Fatalf("%s: %+v %v", name, out, err)
					}
				}
			}
		}
		if rec.Decision.Outcome != "approved" || !rec.Applied {
			t.Error("the recorded result changed")
		}
	})

	t.Run("an identical key from another tenant, project or caller is independent", func(t *testing.T) {
		w, _, rec := resolved(t)
		for name, id := range map[string]rt.SignalIdentity{
			"other caller": {Scope: scope, Caller: "user-bob", Key: "k1"},
		} {
			req := rt.SignalRequest{Identity: id, WaitID: w.ID, Outcome: "approved", Answer: "looks good", Actor: "user:bob"}
			// The decided wait refuses a new identity; it is not a replay of alice's record.
			out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(time.Hour))
			must(t, err)
			r := out.Result
			if r.Replayed || r.Applied || r.Refusal != rt.RefusalDecided || r.SignalID != id.ID() || r.SignalID == rec.ID ||
				out.Record == nil || out.Record.Decision != w.Decision || out.Wait != nil {
				t.Errorf("%s: %+v", name, out)
			}
			must(t, out.Record.Validate())
		}
		// Another project or tenant cannot see the wait at all, and its refusal is its own.
		for name, sc := range map[string]domain.Scope{"other project": p2, "other tenant": {TenantID: "t2", ProjectID: "p1"}} {
			req := rt.SignalRequest{Identity: rt.SignalIdentity{Scope: sc, Caller: "user-alice", Key: "k1"}, WaitID: w.ID, Outcome: "approved", Actor: "user:alice"}
			out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(minute))
			must(t, err)
			if out.Result.Refusal != rt.RefusalUnknownWait || out.Tombstone == nil || out.Tombstone.ID == rec.ID {
				t.Errorf("%s: %+v", name, out)
			}
		}
		if rec.Decision.Actor != "user:alice" {
			t.Error("the first reviewer's recorded result changed")
		}
	})

	t.Run("a new signal for a decided wait is refused with the decision it lost to", func(t *testing.T) {
		w, _, _ := resolved(t)
		req := signalReq(w, "k-new")
		req.Identity.Caller = "user-carol"
		req.Outcome, req.ExpectedVersion = "rejected", 0
		out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(time.Hour))
		must(t, err)
		rec := *out.Record
		must(t, rec.Validate())
		if out.Result.Applied || out.Result.Refusal != rt.RefusalDecided || out.Result.Decision != w.Decision || rec.Target != w.Target() || out.Wait != nil {
			t.Fatalf("outcome %+v", out)
		}
		// Its own replay returns the same refusal and the same decision, after the wait is gone.
		again, err := rt.RouteSignal(req, rt.SignalLookup{Record: &rec}, nil, nil, t0.Add(1000*time.Hour))
		must(t, err)
		want := out.Result
		want.Replayed = true
		if again.Result != want {
			t.Fatalf("refusal replay %+v", again.Result)
		}
	})

	t.Run("version conflict and invalid outcome are recorded against their target without a decision", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate func(*rt.SignalRequest)
			want   rt.RefusalReason
		}{
			"version conflict": {func(r *rt.SignalRequest) { r.ExpectedVersion = 1 }, rt.RefusalVersionConflict},
			"invalid outcome":  {func(r *rt.SignalRequest) { r.Outcome = "maybe" }, rt.RefusalInvalidOutcome},
		} {
			w := openWait(t)
			before := w
			req := signalReq(w, "k")
			tc.mutate(&req)
			out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(minute))
			must(t, err)
			rec := out.Record
			if out.Result.Refusal != tc.want || out.Result.Applied || rec == nil || rec.Decision != (rt.Decision{}) ||
				rec.Target != w.Target() || out.Wait != nil || !reflect.DeepEqual(w, before) {
				t.Fatalf("%s: %+v", name, out)
			}
			must(t, rec.Validate())
			// The wait is still open and a corrected request wins.
			req.ExpectedVersion, req.Outcome = w.Version, "approved"
			req.Identity.Key = "k2"
			win, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(2*minute))
			must(t, err)
			if !win.Result.Applied {
				t.Errorf("%s: the wait stayed open but a corrected request lost", name)
			}
		}
	})

	t.Run("the deadline wins over a late resolution", func(t *testing.T) {
		w := openWait(t)
		req := signalReq(w, "late")
		out, err := rt.RouteSignal(req, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(time.Hour))
		must(t, err)
		if out.Result.Applied || out.Result.Refusal != rt.RefusalDecided || out.Wait == nil || out.Wait.State != rt.WaitExpired ||
			out.Result.Decision.Kind != rt.DecisionExpired || out.Record == nil {
			t.Fatalf("outcome %+v", out)
		}
		must(t, out.Wait.Validate())
		must(t, out.Record.Validate())
	})

	t.Run("correlation routing matches only open waits of the caller's project", func(t *testing.T) {
		corr := map[string]string{"candidate": "c1"}
		req := func(key string) rt.SignalRequest {
			return rt.SignalRequest{Identity: rt.SignalIdentity{Scope: scope, Caller: "svc-ci", Key: key}, Correlation: corr, Outcome: "approved", Actor: "svc:ci"}
		}
		w1 := openWaitFor(t, "w1", "run1", "c1")

		// One open match decides it.
		out, err := rt.RouteSignal(req("a"), rt.SignalLookup{}, []rt.Wait{w1, openWaitFor(t, "w9", "run9", "other")}, nil, t0.Add(minute))
		must(t, err)
		if !out.Result.Applied || out.Result.Target.WaitID != "w1" {
			t.Fatalf("one match: %+v", out)
		}
		closed := *out.Wait

		// A closed wait is never matched: zero matches is unknown_wait with a tombstone.
		z, err := rt.RouteSignal(req("b"), rt.SignalLookup{}, []rt.Wait{closed}, nil, t0.Add(2*minute))
		must(t, err)
		if z.Result.Refusal != rt.RefusalUnknownWait || z.Tombstone == nil || z.Record != nil || z.Wait != nil || z.Result.Target != (rt.SignalTarget{}) {
			t.Fatalf("zero matches: %+v", z)
		}
		must(t, z.Tombstone.Validate())

		// Two open matches are ambiguous.
		w2 := openWaitFor(t, "w2", "run2", "c1")
		amb, err := rt.RouteSignal(req("c"), rt.SignalLookup{}, []rt.Wait{w1, w2}, nil, t0.Add(minute))
		must(t, err)
		if amb.Result.Refusal != rt.RefusalAmbiguousCorrelation || amb.Tombstone == nil || amb.Wait != nil || amb.Record != nil {
			t.Fatalf("two matches: %+v", amb)
		}

		// Replaying each tombstone returns the same refusal even after a matching wait has opened,
		// which it never decides.
		for name, tomb := range map[string]*rt.SignalTombstone{"zero": z.Tombstone, "ambiguous": amb.Tombstone} {
			r := req("b")
			if name == "ambiguous" {
				r = req("c")
			}
			again, err := rt.RouteSignal(r, rt.SignalLookup{Tombstone: tomb}, []rt.Wait{w1}, nil, t0.Add(3*minute))
			must(t, err)
			want := tomb.Result()
			want.Replayed = true
			if again.Result != want || again.Wait != nil || again.Record != nil || again.Tombstone != nil {
				t.Errorf("%s replay: %+v", name, again)
			}
		}

		// Both a wait id and a correlation must agree.
		both := req("d")
		both.WaitID = "w1"
		ok, err := rt.RouteSignal(both, rt.SignalLookup{}, []rt.Wait{w1}, nil, t0.Add(minute))
		must(t, err)
		if !ok.Result.Applied {
			t.Errorf("agreeing address: %+v", ok)
		}
		disagree := req("e")
		disagree.WaitID, disagree.Correlation = "w1", map[string]string{"candidate": "zzz"}
		bad, err := rt.RouteSignal(disagree, rt.SignalLookup{}, []rt.Wait{w1}, nil, t0.Add(minute))
		must(t, err)
		if bad.Result.Refusal != rt.RefusalUnknownWait {
			t.Errorf("disagreeing address: %+v", bad)
		}
	})

	t.Run("foreign, unknown, unauthorized and unopened waits are indistinguishable", func(t *testing.T) {
		w := openWait(t)
		pending, _ := rt.NewWait(scope, "wp", "run1", "review", reviewSpec())
		foreign := w
		foreign.Scope = domain.Scope{TenantID: "t2", ProjectID: "p1"}
		foreign.ID = "w-foreign"
		denied := func(rt.Wait) bool { return false }
		identity := rt.SignalIdentity{Scope: scope, Caller: "user-mallory", Key: "k"}
		request := func(id rt.WaitID) rt.SignalRequest {
			return rt.SignalRequest{Identity: identity, WaitID: id, Outcome: "approved", Actor: "user:mallory"}
		}
		results := map[string]rt.SignalResult{}
		for name, tc := range map[string]struct {
			id    rt.WaitID
			waits []rt.Wait
			auth  func(rt.Wait) bool
		}{
			"unknown":      {"w-nowhere", []rt.Wait{w}, nil},
			"foreign":      {"w-foreign", []rt.Wait{w, foreign}, nil},
			"unauthorized": {"w1", []rt.Wait{w}, denied},
			"unopened":     {"wp", []rt.Wait{pending}, nil},
		} {
			out, err := rt.RouteSignal(request(tc.id), rt.SignalLookup{}, tc.waits, tc.auth, t0.Add(minute))
			must(t, err)
			if out.Wait != nil || out.Record != nil || out.Tombstone == nil {
				t.Fatalf("%s: %+v", name, out)
			}
			results[name] = out.Result
		}
		for name, r := range results {
			if r != results["unknown"] || r.Refusal != rt.RefusalUnknownWait || r.Target != (rt.SignalTarget{}) || r.Decision != (rt.Decision{}) {
				t.Errorf("%s differs from unknown: %+v", name, r)
			}
		}
		if w.State != rt.WaitOpen {
			t.Error("the wait was touched")
		}
	})

	t.Run("records never expire and survive storage as they are", func(t *testing.T) {
		for _, typ := range []reflect.Type{reflect.TypeOf(rt.SignalRecord{}), reflect.TypeOf(rt.SignalTombstone{})} {
			for i := 0; i < typ.NumField(); i++ {
				n := strings.ToLower(typ.Field(i).Name)
				if strings.Contains(n, "expir") || strings.Contains(n, "ttl") || strings.Contains(n, "retain") {
					t.Errorf("%s has a retention field %s", typ.Name(), typ.Field(i).Name)
				}
			}
		}
		if _, ok := reflect.TypeOf(rt.SignalTombstone{}).FieldByName("Target"); ok {
			t.Error("a tombstone has no target")
		}
		if _, ok := reflect.TypeOf(rt.SignalTombstone{}).FieldByName("Decision"); ok {
			t.Error("a tombstone has no decision")
		}
		_, req, rec := resolved(t)
		rec.RecordedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		got, a, b := roundTrip(t, rec)
		if a != b || got != rec {
			t.Fatalf("the decision record does not survive storage:\n%s\n%s", a, b)
		}
		// Decoded from storage alone, it still replays, a century later.
		out, err := rt.RouteSignal(req, rt.SignalLookup{Record: &got}, nil, nil, time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC))
		must(t, err)
		if !out.Result.Replayed || !out.Result.Applied || out.Result.Decision != rec.Decision {
			t.Fatalf("replay %+v", out.Result)
		}
		tomb, err := rt.RouteSignal(rt.SignalRequest{Identity: rt.SignalIdentity{Scope: scope, Caller: "c", Key: "k"},
			Correlation: map[string]string{"candidate": "x"}, Outcome: "approved", Actor: "a"}, rt.SignalLookup{}, nil, nil, t0)
		must(t, err)
		gt, x, y := roundTrip(t, *tomb.Tombstone)
		if x != y || gt != *tomb.Tombstone {
			t.Fatalf("tombstone round trip:\n%s\n%s", x, y)
		}
		if _, err := json.Marshal(rt.SignalLookup{Record: &rec}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid records and lookups are rejected", func(t *testing.T) {
		_, _, rec := resolved(t)
		// A refused record for a decided wait, from another caller.
		w := openWait(t)
		_, err := w.Resolve(signalReq(w, "k1"), t0.Add(minute))
		must(t, err)
		loser := signalReq(w, "k9")
		loser.Identity.Caller = "user-bob"
		lo, err := rt.RouteSignal(loser, rt.SignalLookup{}, []rt.Wait{w}, nil, t0.Add(2*minute))
		must(t, err)
		refused := *lo.Record
		must(t, refused.Validate())

		for name, mutate := range map[string]func(*rt.SignalRecord){
			"id does not derive":        func(r *rt.SignalRecord) { r.ID = "sig-forged" },
			"no digest":                 func(r *rt.SignalRecord) { r.Digest = "" },
			"no target":                 func(r *rt.SignalRecord) { r.Target = rt.SignalTarget{} },
			"applied without decision":  func(r *rt.SignalRecord) { r.Decision = rt.Decision{} },
			"applied with a refusal":    func(r *rt.SignalRecord) { r.Refusal = rt.RefusalDecided },
			"applied by another signal": func(r *rt.SignalRecord) { r.Decision.SignalID = "sig-other" },
			"applied expiry":            func(r *rt.SignalRecord) { r.Decision = rt.Decision{Kind: rt.DecisionExpired, Version: 3, At: t0} },
			"no time":                   func(r *rt.SignalRecord) { r.RecordedAt = time.Time{} },
		} {
			r := rec
			mutate(&r)
			if r.Validate() == nil {
				t.Errorf("record: %s accepted", name)
			}
		}
		for name, mutate := range map[string]func(*rt.SignalRecord){
			"decided without decision":       func(r *rt.SignalRecord) { r.Decision = rt.Decision{} },
			"decided by itself":              func(r *rt.SignalRecord) { r.Decision.SignalID = r.ID },
			"version conflict with decision": func(r *rt.SignalRecord) { r.Refusal = rt.RefusalVersionConflict },
			"targetless reason in a record":  func(r *rt.SignalRecord) { r.Refusal, r.Decision = rt.RefusalUnknownWait, rt.Decision{} },
			"unknown refusal":                func(r *rt.SignalRecord) { r.Refusal = "key_reuse" },
		} {
			r := refused
			mutate(&r)
			if r.Validate() == nil {
				t.Errorf("refusal: %s accepted", name)
			}
		}
		tomb := rt.SignalTombstone{Identity: rec.Identity, ID: rec.ID, Digest: "d", Refusal: rt.RefusalUnknownWait, RecordedAt: t0}
		must(t, tomb.Validate())
		for name, mutate := range map[string]func(*rt.SignalTombstone){
			"a decided refusal":  func(x *rt.SignalTombstone) { x.Refusal = rt.RefusalDecided },
			"id does not derive": func(x *rt.SignalTombstone) { x.ID = "sig-forged" },
			"no digest":          func(x *rt.SignalTombstone) { x.Digest = "" },
			"no time":            func(x *rt.SignalTombstone) { x.RecordedAt = time.Time{} },
		} {
			x := tomb
			mutate(&x)
			if x.Validate() == nil {
				t.Errorf("tombstone: %s accepted", name)
			}
		}
		if (rt.SignalLookup{Record: &rec, Tombstone: &tomb}).Validate() == nil {
			t.Error("a lookup with both shapes accepted")
		}
		// A record of another identity under this request's lookup is corruption, not a replay.
		req := signalReq(openWait(t), "k1")
		req.Identity.Caller = "user-other"
		if _, err := rt.RouteSignal(req, rt.SignalLookup{Record: &rec}, nil, nil, t0); err == nil {
			t.Error("a record of another identity was replayed")
		}
		if rt.SignalResult(rt.SignalResult{}).Applied || (rt.SignalLookup{}).Found() {
			t.Error("zero lookup or result")
		}
		if _, err := rt.RouteSignal(rt.SignalRequest{}, rt.SignalLookup{}, nil, nil, t0); err == nil {
			t.Error("a zero request routed")
		}
		if _, err := rt.RouteSignal(req, rt.SignalLookup{}, nil, nil, time.Time{}); err == nil {
			t.Error("routing without the store time")
		}
	})
}
