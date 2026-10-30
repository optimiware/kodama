package adapters_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube/adapters"
)

// Test_attempts_carry_the_committed_shared_launch: the committed attempt view of a
// run_shared attempt carries the launch derived from the run's immutable pins and
// its bound workspace, equal to the one execution hands the runtime at Start, and
// survives the workspace being stopped (the Job's expectation must stay
// rebuildable while the Job exists). An attempt_private run carries none.
func Test_attempts_carry_the_committed_shared_launch(t *testing.T) {
	ctx := context.Background()
	mem, s := executiontest.NewSharedMem(t)
	_ = mem
	run := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(false, executiontest.SharedStep("build")))
	run.Start()
	run.Up()
	spec, err := run.Claim()
	if err != nil || spec == nil {
		t.Fatalf("claim: %v", err)
	}
	attempts, err := adapters.NewAttempts(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := attempts.Attempt(ctx, spec.Scope, spec.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Shared == nil {
		t.Fatal("a run_shared attempt has no committed launch")
	}
	agg := run.Get()
	want, err := execution.SharedLaunchOf(agg, spec.StepID)
	if err != nil {
		t.Fatal(err)
	}
	got := *rec.Shared
	if got.Policy != want.Policy || got.RoleDigest != want.RoleDigest || got.Workspace != want.Workspace || got.Network != want.Network || got.PodSecurity != want.PodSecurity {
		t.Fatalf("committed launch %+v, want %+v", got, *want)
	}
	if d, err := got.Role.Digest(); err != nil || d != got.RoleDigest {
		t.Fatalf("the committed role does not hash to its pinned digest: %v", err)
	}
	if got.Workspace.UID == "" {
		t.Fatal("the committed launch binds no workspace instance")
	}
}
