package runtime_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
)

// sdlcPlan is the pipeline format's frozen example (.kodama/sdlc.yaml) as plain
// values: join any, output-backed request and candidate, a binding, typed inputs,
// a protected step and a service. Parsing the YAML is not this package's work.
func sdlcPlan() rt.Plan {
	tenant := domain.Owner{TenantID: "t1"}
	agent := func(cmd string) *rt.WorkloadSpec {
		return &rt.WorkloadSpec{Image: "ghcr.io/sst/opencode:latest", Commands: []string{cmd},
			Secrets: []rt.SecretRef{{Owner: tenant, Name: "ANTHROPIC_API_KEY"}}}
	}
	return rt.Plan{
		Digest: "sha256:sdlc",
		Inputs: map[string]rt.InputValue{"base_branch": rt.StringInput("main")},
		Services: []rt.ServiceSpec{{Name: "db", Lifetime: rt.LifetimePipeline, Image: "postgres:16-alpine",
			Env: map[string]string{"POSTGRES_USER": "kodama", "POSTGRES_DB": "app"}, Ports: []int{5432},
			Secrets: []rt.SecretRef{{Owner: tenant, Name: "TEST_DB_PASSWORD"}}}},
		Steps: []rt.PlanStep{
			{Key: "plan", Kind: rt.StepContainer, Workload: agent("opencode run plan")},
			{Key: "implement", Kind: rt.StepContainer, DependsOn: []domain.StepID{"plan"}, Outputs: []string{"commit", "question"},
				Workload: agent("opencode run implement")},
			{Key: "clarify", Kind: rt.StepWait, DependsOn: []domain.StepID{"implement"}, When: `implement.outputs.question != ""`,
				Wait: &rt.WaitSpec{Assign: "engineer", Request: "implement.outputs.question", Input: "answer",
					Outcomes: []string{"answered"}, Timeout: 24 * time.Hour}},
			{Key: "review", Kind: rt.StepWait, DependsOn: []domain.StepID{"implement", "clarify"}, Join: rt.JoinAny,
				Wait: &rt.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "implement.outputs.commit"},
					Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved",
					Timeout: 72 * time.Hour, OnExpiry: rt.ExpiryFail}},
			{Key: "test", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, When: `review.outcome == "approved"`,
				Workload: &rt.WorkloadSpec{Image: "golang:1.27-alpine", Commands: []string{"go test -v ./..."},
					Env:     map[string]string{"DATABASE_URL": "postgres://kodama:${TEST_DB_PASSWORD}@db:5432/app?sslmode=disable"},
					Secrets: []rt.SecretRef{{Owner: tenant, Name: "TEST_DB_PASSWORD"}}}},
			{Key: "integrate", Kind: rt.StepContainer, DependsOn: []domain.StepID{"test"}, Authority: rt.AuthorityIntegrate,
				Requires: []rt.Requirement{{Wait: "review", Outcome: "approved"}},
				Workload: &rt.WorkloadSpec{Image: "ghcr.io/example/integrator:latest", Commands: []string{"integrate-candidate"}}},
			{Key: "rework", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, When: `review.outcome == "rejected"`,
				Bindings: map[string]string{"REVIEW_NOTES": "review.answer"}, Workload: agent("opencode run rework")},
		},
	}
}

// testPlanRepresentation proves the admitted plan carries join, outputs,
// bindings and typed inputs.
func testPlanRepresentation(t *testing.T) {
	t.Run("the frozen example is representable and round-trips", func(t *testing.T) {
		plan := sdlcPlan()
		must(t, plan.Validate())
		got, a, b := roundTrip(t, plan)
		if a != b {
			t.Fatalf("round trip changed the plan:\n%s\n%s", a, b)
		}
		must(t, got.Validate())
		if !reflect.DeepEqual(got, plan) {
			t.Fatalf("the decoded plan differs:\n%+v\n%+v", got, plan)
		}
		by := func(k domain.StepID) rt.PlanStep {
			for _, s := range got.Steps {
				if s.Key == k {
					return s
				}
			}
			t.Fatalf("no step %s", k)
			return rt.PlanStep{}
		}
		if review := by("review"); review.Join != rt.JoinAny || review.Join.Effective() != rt.JoinAny || len(review.DependsOn) != 2 {
			t.Errorf("review join %+v", review)
		}
		if by("plan").Join.Effective() != rt.JoinAll {
			t.Error("the default join is all")
		}
		if !slicesEqual(by("implement").Outputs, []string{"commit", "question"}) || by("rework").Bindings["REVIEW_NOTES"] != "review.answer" {
			t.Errorf("outputs or bindings lost: %+v %+v", by("implement").Outputs, by("rework").Bindings)
		}
		// The wait's request and candidate read declared outputs of a prerequisite.
		declared := func(ref string) bool {
			step, rest, _ := strings.Cut(ref, ".outputs.")
			return rest != "" && slicesContains(by(domain.StepID(step)).Outputs, rest)
		}
		if !declared(by("clarify").Wait.Request) || !declared(by("review").Wait.Correlate["candidate"]) {
			t.Error("request and candidate must be output-backed")
		}
		if v := got.Inputs["base_branch"]; v.Type != rt.InputString || v.String != "main" {
			t.Errorf("input %+v", v)
		}

		// The same plan admits: one step per plan step and a pending wait per wait step.
		scopeRun := rt.Admission{}
		run, err := rt.NewRun(scope, "run1", "pipe1", got, 2)
		must(t, err)
		waits := []rt.Wait{}
		for _, s := range got.Steps {
			if s.Kind == rt.StepWait {
				w, err := rt.NewWait(scope, rt.WaitID("w-"+string(s.Key)), "run1", s.Key, *s.Wait)
				must(t, err)
				waits = append(waits, w)
			}
		}
		scopeRun = rt.Admission{Run: run, Plan: got, Waits: waits}
		must(t, scopeRun.Validate())
		adm, x, y := roundTrip(t, scopeRun)
		if x != y {
			t.Fatalf("admission round trip:\n%s\n%s", x, y)
		}
		must(t, adm.Validate())
	})

	t.Run("join, outputs and bindings obey the format", func(t *testing.T) {
		for name, mutate := range map[string]func(*rt.Plan){
			"unknown join":                   func(p *rt.Plan) { p.Steps[1].Join = "some" },
			"join any with one prerequisite": func(p *rt.Plan) { p.Steps[1].Join = rt.JoinAny },
			"join any with none":             func(p *rt.Plan) { p.Steps[0].Join = rt.JoinAny },
			"outputs on a wait":              func(p *rt.Plan) { p.Steps[2].Outputs = []string{"x"} },
			"bindings on a wait":             func(p *rt.Plan) { p.Steps[3].Bindings = map[string]string{"X": "inputs.base_branch"} },
			"nine outputs": func(p *rt.Plan) {
				p.Steps[1].Outputs = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
			},
			"duplicate output": func(p *rt.Plan) { p.Steps[1].Outputs = []string{"commit", "commit"} },
			"malformed output": func(p *rt.Plan) { p.Steps[1].Outputs = []string{"Commit"} },
			"nine bindings": func(p *rt.Plan) {
				p.Steps[6].Bindings = map[string]string{"A": "x", "B": "x", "C": "x", "D": "x", "E": "x", "F": "x", "G": "x", "H": "x", "I": "x"}
			},
			"binding collides with env": func(p *rt.Plan) {
				p.Steps[6].Bindings = map[string]string{"DATABASE_URL": "x"}
				p.Steps[6].Workload.Env = map[string]string{"DATABASE_URL": "y"}
			},
			"binding collides with a secret": func(p *rt.Plan) { p.Steps[6].Bindings = map[string]string{"ANTHROPIC_API_KEY": "review.answer"} },
			"binding collides with built-in": func(p *rt.Plan) { p.Steps[6].Bindings = map[string]string{"KODAMA_TOKEN": "review.answer"} },
			"malformed binding name":         func(p *rt.Plan) { p.Steps[6].Bindings = map[string]string{"1X": "review.answer"} },
			"empty binding reference":        func(p *rt.Plan) { p.Steps[6].Bindings = map[string]string{"NOTES": ""} },
		} {
			p := sdlcPlan()
			mutate(&p)
			if err := p.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// The bounds themselves are allowed.
		p := sdlcPlan()
		p.Steps[1].Outputs = []string{"a", "b", "c", "d", "e", "f", "g", "h"}
		p.Steps[6].Bindings = map[string]string{"A": "x", "B": "x", "C": "x", "D": "x", "E": "x", "F": "x", "G": "x", "H": "x"}
		must(t, p.Validate())
	})

	t.Run("admitted inputs keep their declared type", func(t *testing.T) {
		values := map[string]rt.InputValue{
			"s_text": rt.StringInput("true"), "s_one": rt.StringInput("1"), "s_empty": rt.StringInput(""),
			"b_true": rt.BooleanInput(true), "b_false": rt.BooleanInput(false),
			"i_one": rt.IntegerInput(1), "i_zero": rt.IntegerInput(0), "i_neg": rt.IntegerInput(-9223372036854775808),
			"i_max": rt.IntegerInput(9223372036854775807),
		}
		for name, v := range values {
			must(t, v.Validate())
			if v.Type == "" {
				t.Errorf("%s has no type", name)
			}
		}
		// Values with the same text but different types are different values: no coercion.
		for _, pair := range [][2]rt.InputValue{
			{rt.StringInput("true"), rt.BooleanInput(true)}, {rt.StringInput("1"), rt.IntegerInput(1)},
			{rt.StringInput(""), rt.BooleanInput(false)}, {rt.StringInput("0"), rt.IntegerInput(0)}, {rt.BooleanInput(false), rt.IntegerInput(0)},
		} {
			if pair[0] == pair[1] || pair[0].Type == pair[1].Type {
				t.Errorf("%+v and %+v must differ", pair[0], pair[1])
			}
			if pair[0].Text() == pair[1].Text() && pair[0] == pair[1] {
				t.Errorf("%+v and %+v compared equal", pair[0], pair[1])
			}
		}
		// The type survives storage even when the value is the type's zero value.
		plan := sdlcPlan()
		plan.Inputs = values
		must(t, plan.Validate())
		got, a, b := roundTrip(t, plan)
		if a != b || !reflect.DeepEqual(got.Inputs, values) {
			t.Fatalf("typed inputs changed in storage:\n%+v", got.Inputs)
		}
		for name, v := range values {
			if got.Inputs[name].Type != v.Type {
				t.Errorf("%s lost its type: %q", name, got.Inputs[name].Type)
			}
		}
		if rt.IntegerInput(42).Text() != "42" || rt.BooleanInput(true).Text() != "true" || rt.StringInput("x").Text() != "x" {
			t.Error("canonical text")
		}

		for name, bad := range map[string]rt.InputValue{
			"no type":              {},
			"unknown type":         {Type: "float"},
			"string with integer":  {Type: rt.InputString, String: "x", Integer: 1},
			"string with boolean":  {Type: rt.InputString, Boolean: true},
			"integer with string":  {Type: rt.InputInteger, String: "1"},
			"integer with boolean": {Type: rt.InputInteger, Boolean: true},
			"boolean with string":  {Type: rt.InputBoolean, String: "true"},
			"boolean with integer": {Type: rt.InputBoolean, Integer: 1},
			"oversize string":      rt.StringInput(strings.Repeat("a", rt.MaxInputStringBytes+1)),
			"invalid utf-8":        rt.StringInput("\xff"),
		} {
			if bad.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		must(t, rt.StringInput(strings.Repeat("a", rt.MaxInputStringBytes)).Validate())

		p := sdlcPlan()
		p.Inputs = map[string]rt.InputValue{"Bad": rt.StringInput("x")}
		if p.Validate() == nil {
			t.Error("malformed input name accepted")
		}
		p.Inputs = map[string]rt.InputValue{"x": {Type: "float"}}
		if p.Validate() == nil {
			t.Error("untyped input accepted")
		}
		p.Inputs = map[string]rt.InputValue{}
		for i := 0; i < rt.MaxInputs+1; i++ {
			p.Inputs["in_"+string(rune('a'+i))] = rt.IntegerInput(int64(i))
		}
		if p.Validate() == nil {
			t.Error("seventeen inputs accepted")
		}
		delete(p.Inputs, "in_a")
		must(t, p.Validate())
	})
}

// testPredicateEvaluation proves a step retains its when evaluation.
func testPredicateEvaluation(t *testing.T) {
	t.Run("unevaluated differs from evaluated false", func(t *testing.T) {
		var none rt.PredicateEvaluation
		falsy := predicate(t, false)
		truthy := predicate(t, true)
		if none.Evaluated || none.Validate() != nil || !falsy.Evaluated || falsy.Result || !truthy.Result {
			t.Fatalf("%+v %+v %+v", none, falsy, truthy)
		}
		if none == falsy || falsy == truthy {
			t.Error("the three states must be distinct")
		}
		for name, bad := range map[string]rt.PredicateEvaluation{
			"unevaluated with a result": {Result: true},
			"unevaluated with a digest": {InputDigest: "sha256:x"},
			"evaluated without digest":  {Evaluated: true},
			"evaluated without time":    {Evaluated: true, InputDigest: "sha256:x"},
		} {
			if bad.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		r := mustRun(t)
		rec := rt.RunRecord{Run: r, Plan: mustPlan(t)}
		if _, ok := rec.Predicate("notify"); ok {
			t.Error("an unevaluated step reports an evaluation")
		}
		must(t, r.RecordPredicate("notify", falsy))
		rec.Run = r
		got, ok := rec.Predicate("notify")
		if !ok || got.Result || got != falsy {
			t.Errorf("evaluated false: %+v %v", got, ok)
		}
	})

	t.Run("an evaluation is recorded once, before the step moves, and never again", func(t *testing.T) {
		r := mustRun(t)
		v := r.Version
		must(t, r.RecordPredicate("test", predicate(t, true)))
		if r.Version != v+1 || r.Steps[3].Version != 2 || !r.Steps[3].Predicate.Result {
			t.Fatalf("recorded %+v", r.Steps[3])
		}
		before := r
		before.Steps = append([]rt.Step(nil), r.Steps...)
		// Recovery reuses the record: even an identical second record is refused, so nothing re-evaluates.
		for name, ev := range map[string]rt.PredicateEvaluation{"identical": predicate(t, true), "opposite": predicate(t, false)} {
			if err := r.RecordPredicate("test", ev); !errors.Is(err, rt.ErrIllegalTransition) || !reflect.DeepEqual(r, before) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, tc := range map[string]struct {
			key domain.StepID
			ev  rt.PredicateEvaluation
		}{
			"unknown step":   {"nope", predicate(t, true)},
			"unevaluated":    {"notify", rt.PredicateEvaluation{}},
			"invalid digest": {"notify", rt.PredicateEvaluation{Evaluated: true, At: t0}},
		} {
			if err := r.RecordPredicate(tc.key, tc.ev); err == nil || !reflect.DeepEqual(r, before) {
				t.Errorf("%s accepted", name)
			}
		}
		must(t, r.AdvanceStep("test", rt.StepRunning, rt.ReasonNone))
		if err := r.RecordPredicate("test", predicate(t, true)); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("a started step: %v", err)
		}
		// A settled run records nothing.
		s := mustRun(t)
		s.RequestCancel()
		for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
			must(t, s.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
		}
		must(t, s.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
		if err := s.RecordPredicate("notify", predicate(t, false)); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("settled run: %v", err)
		}
	})

	t.Run("a false evaluation ends in a condition_false skip and only then", func(t *testing.T) {
		// Skipped for condition_false needs the false evaluation.
		r := mustRun(t)
		if err := r.AdvanceStep("notify", rt.StepSkipped, rt.ReasonConditionFalse); !errors.Is(err, domain.ErrInvalidValue) || r.Steps[2].State != rt.StepPending {
			t.Errorf("unevaluated condition_false skip: %v", err)
		}
		// A true evaluation cannot be a condition_false skip.
		must(t, r.RecordPredicate("notify", predicate(t, true)))
		if err := r.AdvanceStep("notify", rt.StepSkipped, rt.ReasonConditionFalse); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("true evaluation skipped as false: %v", err)
		}
		// Other skip reasons do not need an evaluation (join and requires are checked first or after).
		q := mustRun(t)
		must(t, q.AdvanceStep("notify", rt.StepSkipped, rt.ReasonPrerequisiteSkipped))
		if q.Steps[2].Predicate.Evaluated {
			t.Error("a prerequisite_skipped step never evaluated its when")
		}
		// A false evaluation can end only in skipped (or cancelled).
		f := mustRun(t)
		must(t, f.RecordPredicate("test", predicate(t, false)))
		if err := f.AdvanceStep("test", rt.StepRunning, rt.ReasonNone); !errors.Is(err, domain.ErrInvalidValue) || f.Steps[3].State != rt.StepPending {
			t.Errorf("a false evaluation started the step: %v", err)
		}
		must(t, f.AdvanceStep("test", rt.StepSkipped, rt.ReasonConditionFalse))
		g := mustRun(t)
		must(t, g.RecordPredicate("test", predicate(t, false)))
		g.RequestCancel()
		must(t, g.AdvanceStep("test", rt.StepCancelled, rt.ReasonCancelled))
	})

	t.Run("the record validates against the plan and round-trips with the step", func(t *testing.T) {
		rec := evidenceRecord(t)
		must(t, rec.Validate())
		if ev, ok := rec.Predicate("notify"); !ok || ev.Result {
			t.Fatalf("notify: %+v %v", ev, ok)
		}
		if ev, ok := rec.Predicate("test"); !ok || !ev.Result {
			t.Fatalf("test: %+v %v", ev, ok)
		}
		if _, ok := rec.Predicate("build"); ok {
			t.Error("build has no when")
		}
		got, a, b := roundTrip(t, rec)
		if a != b || got.Run.Steps[2].Predicate != rec.Run.Steps[2].Predicate || got.Run.Steps[3].Predicate != rec.Run.Steps[3].Predicate {
			t.Fatalf("predicate lost in storage:\n%s\n%s", a, b)
		}
		must(t, got.Validate())
		// Unevaluated and evaluated-false stay distinct after storage.
		if got.Run.Steps[0].Predicate.Evaluated || !got.Run.Steps[2].Predicate.Evaluated || got.Run.Steps[2].Predicate.Result {
			t.Errorf("states after storage: %+v %+v", got.Run.Steps[0].Predicate, got.Run.Steps[2].Predicate)
		}

		for name, mutate := range map[string]func(*rt.RunRecord){
			"evaluation on a step without when": func(r *rt.RunRecord) { r.Run.Steps[0].Predicate = predicate(t, true) },
			"started without an evaluation":     func(r *rt.RunRecord) { r.Run.Steps[3].Predicate = rt.PredicateEvaluation{} },
			"started after a false evaluation":  func(r *rt.RunRecord) { r.Run.Steps[3].Predicate = predicate(t, false) },
		} {
			x := evidenceRecord(t)
			mutate(&x)
			if x.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("a transition persists the evaluation with the step move", func(t *testing.T) {
		run := mustRun(t)
		next := run
		next.Steps = append([]rt.Step(nil), run.Steps...)
		skipForCondition(t, &next, "notify")
		must(t, next.RecordPredicate("test", predicate(t, true)))
		must(t, next.AdvanceStep("test", rt.StepRunning, rt.ReasonNone))
		claim := rt.Claim{Scope: scope, RunID: "run1", Owner: "w", Purpose: rt.ClaimExecute, Fence: 1, Expires: t0.Add(minute)}
		// One transition carries several step moves and their evaluations together.
		tr := rt.Transition{Scope: scope, RunID: "run1", Claim: claim, Now: t0, ExpectedVersion: next.Version - 1, Run: next}
		must(t, tr.Validate())
		got, a, b := roundTrip(t, tr)
		must(t, got.Validate())
		if a != b || got.Run.Steps[2].Predicate != next.Steps[2].Predicate || !got.Run.Steps[2].Predicate.Evaluated || got.Run.Steps[2].Predicate.Result ||
			!got.Run.Steps[3].Predicate.Result {
			t.Fatalf("transition lost the evaluations:\n%s\n%s", a, b)
		}
	})

	t.Run("a stored update never rewrites an evaluation or a final step", func(t *testing.T) {
		base := mustRun(t)
		bump := func(r rt.Run) rt.Run {
			r.Steps = append([]rt.Step(nil), r.Steps...)
			r.Version++
			return r
		}
		// Record, then skip: legal updates.
		a := bump(base)
		a.Steps[2].Predicate = predicate(t, false)
		must(t, rt.CheckRunUpdate(base, a))
		b := bump(a)
		b.Steps[2].State, b.Steps[2].Reason = rt.StepSkipped, rt.ReasonConditionFalse
		must(t, rt.CheckRunUpdate(a, b))

		for name, tc := range map[string]struct {
			prev rt.Run
			next func(rt.Run) rt.Run
		}{
			"flip the evaluation":     {a, func(r rt.Run) rt.Run { r = bump(r); r.Steps[2].Predicate = predicate(t, true); return r }},
			"erase the evaluation":    {a, func(r rt.Run) rt.Run { r = bump(r); r.Steps[2].Predicate = rt.PredicateEvaluation{}; return r }},
			"re-time the evaluation":  {a, func(r rt.Run) rt.Run { r = bump(r); r.Steps[2].Predicate.At = t0.Add(time.Hour); return r }},
			"reopen a skipped step":   {b, func(r rt.Run) rt.Run { r = bump(r); r.Steps[2].State, r.Steps[2].Reason = rt.StepPending, ""; return r }},
			"change a final reason":   {b, func(r rt.Run) rt.Run { r = bump(r); r.Steps[2].Reason = rt.ReasonRequiresUnmet; return r }},
			"skip a version":          {a, func(r rt.Run) rt.Run { r = bump(r); r.Version++; return r }},
			"another run":             {a, func(r rt.Run) rt.Run { r = bump(r); r.ID = "run2"; return r }},
			"drop a step":             {a, func(r rt.Run) rt.Run { r = bump(r); r.Steps = r.Steps[:3]; return r }},
			"retarget a step":         {a, func(r rt.Run) rt.Run { r = bump(r); r.Steps[0].Kind = rt.StepWait; return r }},
			"withdraw a cancellation": {func() rt.Run { r := bump(base); r.CancelRequested = true; return r }(), func(r rt.Run) rt.Run { r = bump(r); r.CancelRequested = false; return r }},
		} {
			if err := rt.CheckRunUpdate(tc.prev, tc.next(tc.prev)); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		// A settled outcome never changes.
		s := mustRun(t)
		s.RequestCancel()
		for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
			must(t, s.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
		}
		must(t, s.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
		changed := bump(s)
		changed.Outcome = rt.OutcomeFailed
		if err := rt.CheckRunUpdate(s, changed); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("settled outcome rewritten: %v", err)
		}
		// Attempts never go backwards.
		w := mustRun(t)
		must(t, w.RecordPredicate("test", predicate(t, true)))
		must(t, w.AdvanceStep("test", rt.StepRunning, rt.ReasonNone))
		back := bump(w)
		back.Steps[3].Attempt = 0
		if err := rt.CheckRunUpdate(w, back); err == nil {
			t.Error("attempt went backwards")
		}
	})

	t.Run("the digest of the values read is canonical", func(t *testing.T) {
		a := []rt.PredicateRead{
			{Ref: "review.outcome", Type: rt.InputString, Present: true, Value: "approved"},
			{Ref: "inputs.retries", Type: rt.InputInteger, Present: true, Value: "1"},
		}
		d1, err := rt.DigestPredicateReads(a)
		must(t, err)
		d2, err := rt.DigestPredicateReads([]rt.PredicateRead{a[1], a[0]})
		must(t, err)
		if d1 != d2 || !strings.HasPrefix(d1, "sha256:") {
			t.Fatalf("order changes the digest: %s %s", d1, d2)
		}
		digest := func(r rt.PredicateRead) string {
			d, err := rt.DigestPredicateReads([]rt.PredicateRead{r})
			must(t, err)
			return d
		}
		base := rt.PredicateRead{Ref: "x", Type: rt.InputString, Present: true, Value: "1"}
		seen := map[string]string{digest(base): "base"}
		for name, r := range map[string]rt.PredicateRead{
			"integer 1 is not string 1": {Ref: "x", Type: rt.InputInteger, Present: true, Value: "1"},
			"missing is not empty":      {Ref: "x", Type: rt.InputString, Present: false},
			"empty is not missing":      {Ref: "x", Type: rt.InputString, Present: true, Value: ""},
			"another ref":               {Ref: "y", Type: rt.InputString, Present: true, Value: "1"},
			"another value":             {Ref: "x", Type: rt.InputString, Present: true, Value: "2"},
		} {
			d := digest(r)
			if prev, dup := seen[d]; dup {
				t.Errorf("%s collides with %s", name, prev)
			}
			seen[d] = name
		}
		empty, err := rt.DigestPredicateReads(nil)
		must(t, err)
		if _, dup := seen[empty]; dup || empty == "" {
			t.Error("an evaluation that read nothing still has a digest")
		}
		for name, bad := range map[string][]rt.PredicateRead{
			"duplicate ref":        {base, base},
			"no ref":               {{Type: rt.InputString, Present: true}},
			"no type":              {{Ref: "x", Present: true}},
			"missing with a value": {{Ref: "x", Type: rt.InputString, Value: "v"}},
		} {
			if _, err := rt.DigestPredicateReads(bad); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})
}

func slicesEqual(a, b []string) bool { return reflect.DeepEqual(a, b) }

func slicesContains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
