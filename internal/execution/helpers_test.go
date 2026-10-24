package execution

import (
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

var t0 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

const (
	testImage = "registry.example.invalid/tools@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	commitA   = "1111111111111111111111111111111111111111"
	commitB   = "2222222222222222222222222222222222222222"
)

// stepOpt customizes a test step.
type stepOpt func(*pipeline.Step)

func container(id string, opts ...stepOpt) pipeline.Step {
	s := pipeline.Step{ID: id, Kind: pipeline.KindContainer, Image: testImage, Command: []string{"/bin/sh", "-ec"}, Args: []string{}, TimeoutSeconds: 60,
		Workspace: &pipeline.Workspace{Access: pipeline.AccessRead, From: pipeline.RunSource}, Retry: &pipeline.Retry{MaxAttempts: 1}}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func needs(ids ...string) stepOpt { return func(s *pipeline.Step) { s.Needs = ids } }
func writer(from string) stepOpt {
	return func(s *pipeline.Step) { s.Workspace = &pipeline.Workspace{Access: pipeline.AccessWrite, From: from} }
}
func reader(from string) stepOpt {
	return func(s *pipeline.Step) { s.Workspace = &pipeline.Workspace{Access: pipeline.AccessRead, From: from} }
}
func noWorkspace() stepOpt {
	return func(s *pipeline.Step) { s.Workspace = &pipeline.Workspace{Access: pipeline.AccessNone} }
}
func retry(max, backoff int64, on ...pipeline.RetryClass) stepOpt {
	return func(s *pipeline.Step) { s.Retry = &pipeline.Retry{MaxAttempts: max, BackoffSeconds: backoff, On: on} }
}
func when(input string, equals pipeline.Scalar) stepOpt {
	return func(s *pipeline.Step) { s.When = &pipeline.Condition{Input: input, Equals: equals} }
}
func args(a ...string) stepOpt { return func(s *pipeline.Step) { s.Args = a } }
func outputs(names ...string) stepOpt {
	return func(s *pipeline.Step) {
		s.Outputs = map[string]pipeline.Output{}
		for _, n := range names {
			s.Outputs[n] = pipeline.Output{Type: pipeline.TypeString}
		}
	}
}
func gate(id string, needs ...string) pipeline.Step {
	return pipeline.Step{ID: id, Kind: pipeline.KindGate, Needs: needs, Gate: &pipeline.GateSpec{Evidence: []string{}}}
}

func newAgg(t testing.TB, inputs []PinnedInput, steps ...pipeline.Step) *Aggregate {
	t.Helper()
	run := &Run{
		Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}, ID: "run_1", Purpose: PurposeWork, WorkItemID: "wi_1", WorkspaceID: "ws_1",
		DefinitionID: "pd_1", DefinitionDigest: "sha256:" + string(make([]byte, 0)) + "2222222222222222222222222222222222222222222222222222222222222222",
		ContentRevision: 1, Specification: &Specification{Spec: "s"}, SourceCommit: commitA, Snapshot: Snapshot{Inputs: inputs},
		SourceRetention: RetentionConfirmed, RetainedRefID: "pin_1", RequestedBy: Actor{Type: ActorUser, ID: "u1"},
		Status: RunQueued, Reason: ReasonCreated, Version: 1, CreatedAt: t0, UpdatedAt: t0,
	}
	agg := &Aggregate{Run: run}
	for i, s := range steps {
		agg.Steps = append(agg.Steps, NewStepRun(run.ID, i, s, t0))
	}
	return agg
}

var hooks = EnabledKinds{pipeline.KindContainer}

// claim claims the next runnable step with a fresh attempt ID.
func claim(t testing.TB, agg *Aggregate, now time.Time, attempt string) *Attempt {
	t.Helper()
	s := agg.NextRunnable(now, hooks)
	if s == nil {
		t.Fatal("nothing runnable")
	}
	b, err := agg.SourceFor(s)
	if err != nil {
		t.Fatal(err)
	}
	at, err := agg.Claim(ClaimParams{
		StepID: s.ID, AttemptID: domain.AttemptID(attempt), Access: b.Access, Workspace: "ws_1", Input: b.Commit, Tip: commitA,
		Plan:    WorkloadPlan{WorkloadName: "kdm-" + attempt, SpecFingerprint: "sha256:" + hex64("f"), ContextDigest: "sha256:" + hex64("c"), Context: []byte(`{}`)},
		Timeout: time.Minute, Startup: time.Minute, Lease: time.Second,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func hex64(c string) string {
	s := ""
	for i := 0; i < 64; i++ {
		s += c
	}
	return s
}

func okResult(outputs map[string]runtimev1.Value, commit string) runtimev1.Result {
	zero := int64(0)
	r := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	if r.Outputs == nil {
		r.Outputs = map[string]runtimev1.Value{}
	}
	if commit != "" {
		c := runtimev1.GitCommit(commit)
		r.SourceCommit = &c
	}
	return r
}

func settleAll(t testing.TB, agg *Aggregate, now time.Time) {
	t.Helper()
	for _, at := range agg.Attempts {
		if _, err := agg.Settle(at, now); err != nil && err != ErrOutcomeUnknown {
			t.Fatal(err)
		}
	}
}
