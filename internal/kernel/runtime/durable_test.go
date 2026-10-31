package runtime_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
)

func outputsPlan() rt.Plan {
	return rt.Plan{Digest: "sha256:outputs", Steps: []rt.PlanStep{
		{Key: "build", Kind: rt.StepContainer, Outputs: []string{"commit", "branch"},
			Workload: &rt.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "deploy", Kind: rt.StepContainer, DependsOn: []domain.StepID{"build"},
			Workload: &rt.WorkloadSpec{Image: "alpine:3", Commands: []string{"deploy"}}},
	}}
}

// settled returns the run before and after build settles as succeeded.
func settled(t *testing.T) (plan rt.Plan, prev, next rt.Run) {
	t.Helper()
	plan = outputsPlan()
	run, err := rt.NewRun(scope, "run1", "pipe1", plan, 2)
	must(t, err)
	must(t, run.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
	prev = run.Clone()
	must(t, run.FinishStep("build", rt.StepSucceeded, rt.ReasonNone, okRes))
	return plan, prev, run
}

// Test_unit_kernel_runtime_outputs proves a settled step's declared outputs are a
// durable, bounded part of the run record: recorded only for a declared name, only
// with the settlement of a succeeded container step, once, and read back from the
// record alone.
func Test_unit_kernel_runtime_outputs(t *testing.T) {
	plan, prev, next := settled(t)
	good := rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": "c1", "branch": ""}}

	t.Run("declared outputs are recorded with the settlement and read from the record", func(t *testing.T) {
		got, err := rt.CheckOutputs(plan, prev, next, nil, []rt.StepOutputs{good})
		if err != nil || len(got) != 1 {
			t.Fatalf("CheckOutputs: %v %v", got, err)
		}
		rec := rt.RunRecord{Run: next, Plan: plan, Outputs: got}
		if err := rec.Validate(); err != nil {
			t.Fatalf("the record is coherent: %v", err)
		}
		if v, ok := rec.Output("build", "commit"); !ok || v != "c1" {
			t.Errorf("commit = %q, %v", v, ok)
		}
		if v, ok := rec.Output("build", "branch"); !ok || v != "" {
			t.Errorf("an empty value is present, not missing: %q, %v", v, ok)
		}
		if _, ok := rec.Output("build", "other"); ok {
			t.Error("a name that was not recorded is missing")
		}
		if _, ok := rec.Output("deploy", "commit"); ok {
			t.Error("another step has no outputs")
		}
		back, _, _ := roundTrip(t, rec)
		if v, _ := back.Output("build", "commit"); v != "c1" {
			t.Errorf("outputs survive the JSON encoding: %q", v)
		}
	})

	refused := map[string]struct {
		out  rt.StepOutputs
		want error
	}{
		"an undeclared name":      {rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"secret": "x"}}, domain.ErrInvalidValue},
		"a malformed name":        {rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"Commit": "x"}}, domain.ErrInvalidValue},
		"an oversized value":      {rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": strings.Repeat("a", rt.MaxOutputValueBytes+1)}}, domain.ErrInvalidValue},
		"a value that is no text": {rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": "\xff"}}, domain.ErrInvalidValue},
		"no values":               {rt.StepOutputs{StepKey: "build", Attempt: 1}, domain.ErrInvalidValue},
		"no attempt":              {rt.StepOutputs{StepKey: "build", Values: map[string]string{"commit": "x"}}, domain.ErrInvalidValue},
		"another attempt":         {rt.StepOutputs{StepKey: "build", Attempt: 2, Values: map[string]string{"commit": "x"}}, rt.ErrIllegalTransition},
		"an unknown step":         {rt.StepOutputs{StepKey: "nope", Attempt: 1, Values: map[string]string{"commit": "x"}}, rt.ErrIllegalTransition},
		"a step that is not settled as succeeded": {rt.StepOutputs{StepKey: "deploy", Attempt: 1, Values: map[string]string{"commit": "x"}}, rt.ErrIllegalTransition},
	}
	for name, tc := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := rt.CheckOutputs(plan, prev, next, nil, []rt.StepOutputs{tc.out}); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("a bound at its limit is accepted", func(t *testing.T) {
		o := rt.StepOutputs{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": strings.Repeat("a", rt.MaxOutputValueBytes)}}
		if _, err := rt.CheckOutputs(plan, prev, next, nil, []rt.StepOutputs{o}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("outputs are recorded in the settling commit only, and once", func(t *testing.T) {
		// build already succeeded in the stored run: the outputs would appear late.
		if _, err := rt.CheckOutputs(plan, next, next, nil, []rt.StepOutputs{good}); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("late outputs: %v", err)
		}
		recorded, err := rt.CheckOutputs(plan, prev, next, nil, []rt.StepOutputs{good})
		must(t, err)
		if _, err := rt.CheckOutputs(plan, prev, next, recorded, []rt.StepOutputs{good}); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("a second record for the step: %v", err)
		}
		if _, err := rt.CheckOutputs(plan, prev, next, nil, []rt.StepOutputs{good, good}); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("a duplicate in one transition: %v", err)
		}
	})

	t.Run("the input is not aliased", func(t *testing.T) {
		recorded := []rt.StepOutputs{}
		got, err := rt.CheckOutputs(plan, prev, next, recorded, []rt.StepOutputs{good})
		must(t, err)
		if len(recorded) != 0 || len(got) != 1 {
			t.Errorf("recorded %v got %v", recorded, got)
		}
	})

	t.Run("a stored record is coherent with its plan and run", func(t *testing.T) {
		bad := rt.RunRecord{Run: prev, Plan: plan, Outputs: []rt.StepOutputs{good}}
		if err := bad.Validate(); err == nil {
			t.Error("outputs of a step that is still running must be refused")
		}
		undeclared := rt.RunRecord{Run: next, Plan: plan, Outputs: []rt.StepOutputs{{StepKey: "build", Attempt: 1, Values: map[string]string{"x": "y"}}}}
		if err := undeclared.Validate(); err == nil {
			t.Error("an undeclared output must be refused")
		}
		twice := rt.RunRecord{Run: next, Plan: plan, Outputs: []rt.StepOutputs{good, good}}
		if err := twice.Validate(); err == nil {
			t.Error("outputs recorded twice must be refused")
		}
	})

	t.Run("a transition validates what it carries", func(t *testing.T) {
		tr := rt.Transition{Scope: scope, RunID: "run1", Now: t0, ExpectedVersion: prev.Version, Run: next,
			Claim: rt.Claim{Scope: scope, RunID: "run1", Owner: "w1", Purpose: rt.ClaimExecute, Fence: 1, Expires: t0.Add(time.Minute)}}
		tr.Run.Version = prev.Version + 1
		tr.Outputs = []rt.StepOutputs{{StepKey: "build", Attempt: 1}}
		if err := tr.Validate(); err == nil {
			t.Error("an empty outputs record must be refused")
		}
		tr.Outputs = []rt.StepOutputs{good}
		if err := tr.Validate(); err != nil {
			t.Errorf("a well-formed record: %v", err)
		}
	})
}

// Test_unit_kernel_runtime_materialization proves the durable, scoped identity of
// secret material: derived from scope, run, step and attempt, tamper-evident, and
// moving only from issued to revoked.
func Test_unit_kernel_runtime_materialization(t *testing.T) {
	plan, _, _ := settled(t)
	run, err := rt.NewRun(scope, "run1", "pipe1", plan, 2)
	must(t, err)
	must(t, run.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))

	ref, err := rt.NewMaterializationRef(scope, "run1", "build", 1)
	must(t, err)
	other, _ := rt.NewMaterializationRef(domain.Scope{TenantID: "t2", ProjectID: "p1"}, "run1", "build", 1)
	if ref.ID == other.ID || !strings.Contains(ref.ID, "t1") {
		t.Errorf("the identity is scoped: %q vs %q", ref.ID, other.ID)
	}
	if _, err := rt.NewMaterializationRef(scope, "run1", "build", 0); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("an attempt is required: %v", err)
	}
	forged := ref
	forged.ID = other.ID
	if err := forged.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("an identity that is not derived is refused: %v", err)
	}

	issued := rt.MaterializationRecord{Ref: ref, State: rt.MaterializationIssued, IssuedAt: t0}
	revoked := issued
	revoked.State = rt.MaterializationRevoked

	got, err := rt.CheckMaterializations(run, nil, []rt.MaterializationRecord{issued})
	must(t, err)
	got, err = rt.CheckMaterializations(run, got, []rt.MaterializationRecord{revoked})
	must(t, err)
	if len(got) != 1 || got[0].State != rt.MaterializationRevoked {
		t.Fatalf("revoked: %+v", got)
	}
	if _, err := rt.CheckMaterializations(run, got, []rt.MaterializationRecord{revoked}); err != nil {
		t.Errorf("a repeat of the stored record changes nothing: %v", err)
	}
	if _, err := rt.CheckMaterializations(run, got, []rt.MaterializationRecord{issued}); !errors.Is(err, rt.ErrIllegalTransition) {
		t.Errorf("revoked is final: %v", err)
	}
	moved := issued
	moved.IssuedAt = t0.Add(time.Second)
	if _, err := rt.CheckMaterializations(run, []rt.MaterializationRecord{issued}, []rt.MaterializationRecord{moved}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("an identity cannot change: %v", err)
	}
	if _, err := rt.CheckMaterializations(run, nil, []rt.MaterializationRecord{revoked}); !errors.Is(err, rt.ErrIllegalTransition) {
		t.Errorf("a new record is issued, not revoked: %v", err)
	}
	foreign := rt.MaterializationRecord{Ref: other, State: rt.MaterializationIssued, IssuedAt: t0}
	if _, err := rt.CheckMaterializations(run, nil, []rt.MaterializationRecord{foreign}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("another tenant's identity: %v", err)
	}
	unknown, _ := rt.NewMaterializationRef(scope, "run1", "nope", 1)
	if _, err := rt.CheckMaterializations(run, nil, []rt.MaterializationRecord{{Ref: unknown, State: rt.MaterializationIssued, IssuedAt: t0}}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("an unknown step: %v", err)
	}
	if _, err := rt.CheckMaterializations(run, nil, []rt.MaterializationRecord{issued, issued}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("the same identity twice in one transition: %v", err)
	}

	// Material is issued for the attempt that is running, not for a settled one.
	done := run.Clone()
	must(t, done.FinishStep("build", rt.StepSucceeded, rt.ReasonNone, okRes))
	if _, err := rt.CheckMaterializations(done, nil, []rt.MaterializationRecord{issued}); !errors.Is(err, rt.ErrIllegalTransition) {
		t.Errorf("issuing for a settled step: %v", err)
	}

	t.Run("pending revocations are read from the record", func(t *testing.T) {
		rec := rt.RunRecord{Run: run, Plan: plan, Materializations: []rt.MaterializationRecord{issued}}
		if err := rec.Validate(); err != nil {
			t.Fatal(err)
		}
		if p := rec.PendingRevocations(); len(p) != 0 {
			t.Errorf("material of the running attempt is live: %v", p)
		}
		rec.Run = done
		if p := rec.PendingRevocations(); len(p) != 1 || p[0].Ref.ID != ref.ID {
			t.Errorf("a settled step's issued material is owed: %v", p)
		}
		rec.Materializations = []rt.MaterializationRecord{revoked}
		if p := rec.PendingRevocations(); len(p) != 0 {
			t.Errorf("revoked material is not owed: %v", p)
		}
		back, _, _ := roundTrip(t, rec)
		if len(back.Materializations) != 1 || back.Materializations[0].Ref != ref {
			t.Errorf("the identity survives the JSON encoding: %+v", back.Materializations)
		}
	})
}

// Test_unit_kernel_runtime_release proves ReleaseClaim: it checks scope, owner and
// fence, ends only the lease and keeps the fence, and is a no-op for a claim the
// caller no longer holds.
func Test_unit_kernel_runtime_release(t *testing.T) {
	held := rt.Claim{Scope: scope, RunID: "run1", Owner: "w1", Purpose: rt.ClaimExecute, Fence: 3, Expires: t0.Add(time.Minute)}
	now := t0.Add(10 * time.Second)

	got, released, err := rt.ReleaseClaim(held, held, now)
	if err != nil || !released || got.Fence != 3 || got.Owner != "w1" || !got.Expires.Equal(now) || got.Active(now) {
		t.Fatalf("release: %+v %v %v", got, released, err)
	}
	next, err := rt.AcquireClaim(got, rt.ClaimRequest{Scope: scope, RunID: "run1", Owner: "w2", Purpose: rt.ClaimExecute, TTL: time.Minute}, now)
	if err != nil || next.Fence != 4 {
		t.Errorf("the next claim is a plain takeover: %+v %v", next, err)
	}
	if err := held.Authorizes(got, now); !errors.Is(err, rt.ErrClaimExpired) {
		t.Errorf("a late write from the releaser is refused: %v", err)
	}

	noop := map[string]struct{ current, held rt.Claim }{
		"never claimed":             {rt.Claim{}, held},
		"another owner":             {held, func() rt.Claim { c := held; c.Owner = "w2"; return c }()},
		"a superseded fence":        {func() rt.Claim { c := held; c.Fence = 4; c.Owner = "w2"; return c }(), held},
		"a fence from the future":   {held, func() rt.Claim { c := held; c.Fence = 4; return c }()},
		"an expired lease":          {func() rt.Claim { c := held; c.Expires = t0.Add(time.Second); return c }(), held},
		"an already released lease": {got, held},
	}
	for name, tc := range noop {
		t.Run("a no-op for "+name, func(t *testing.T) {
			c, released, err := rt.ReleaseClaim(tc.current, tc.held, now)
			if err != nil || released || c != tc.current {
				t.Errorf("%+v %v %v", c, released, err)
			}
		})
	}

	if _, _, err := rt.ReleaseClaim(held, rt.Claim{}, now); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("an invalid claim: %v", err)
	}
	if _, _, err := rt.ReleaseClaim(held, held, time.Time{}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("a release needs the time: %v", err)
	}
	foreign := held
	foreign.Scope = domain.Scope{TenantID: "t2", ProjectID: "p1"}
	if _, _, err := rt.ReleaseClaim(held, foreign, now); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("another tenant's claim: %v", err)
	}
	otherRun := held
	otherRun.RunID = "run2"
	if _, _, err := rt.ReleaseClaim(held, otherRun, now); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("another run's claim: %v", err)
	}
}
