package request_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// answerReduction independently derives the Kernel reduction for one authorized
// answer. The service must attach exactly this reduction, never re-derive state
// in the Shell.
func answerReduction(
	t *testing.T,
	execution kernel.Execution,
	actorID id.PrincipalID,
	decision request.Decision,
	response kernel.Outputs,
	at time.Time,
) flow.Commit {
	t.Helper()
	kernelDecision, ok := decision.KernelDecision()
	if !ok {
		t.Fatalf("decision %q has no Kernel decision", decision)
	}
	_, commit, err := flow.Reduce(execution, flow.AnswerFact{
		ExecutionID: execution.ID,
		ActionID:    execution.Actions[0].ID,
		Answer: kernel.Answer{
			Decision: kernelDecision,
			Outputs:  response,
			Actor:    actorID,
			At:       at,
		},
		At: at,
	})
	requireNoError(t, err, "independent answer reduction")
	if commit == nil {
		t.Fatalf("independent reduction for %q produced no commit", decision)
	}
	return *commit
}

func TestResolveFirstSettlementAttachesValidatedKernelCommit(t *testing.T) {
	cases := []struct {
		name             string
		decision         request.Decision
		response         kernel.Outputs
		nextExecution    kernel.ExecutionState
		nextAction       kernel.ActionState
		wantPromoted     kernel.Outputs
		wantFailure      bool
		wantCancellation int
	}{
		{
			name:          "pass settles succeeded action and keeps the planned wait running",
			decision:      request.DecisionPass,
			response:      kernel.Outputs{"approved": "yes"},
			nextExecution: kernel.ExecutionRunning,
			nextAction:    kernel.ActionSucceeded,
			wantPromoted:  kernel.Outputs{"approved": "yes"},
		},
		{
			name:             "fail settles failed execution and cancels the successor",
			decision:         request.DecisionFail,
			response:         kernel.Outputs{"approved": "no"},
			nextExecution:    kernel.ExecutionFailed,
			nextAction:       kernel.ActionFailed,
			wantFailure:      true,
			wantCancellation: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scope := projectScope("tenant-1", "project-1")
			subject := literalSubject(map[string]string{"operation": "git.ref.update"})
			aggregate := runningRequestExecution(
				requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
				containerAction("act-1", "deploy", 1, time.Hour, "result"),
			)
			fixture := newTestService(t)
			fixture.store.Seed(scope, aggregate)
			fixture.clock.now = testNow.Add(time.Minute)
			actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)

			_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, testCase.decision, testCase.response))
			requireNoError(t, err, "Resolve")
			if fixture.resolutions.callCount() != 1 {
				t.Fatalf("resolve port calls = %d; want one", fixture.resolutions.callCount())
			}
			unit := fixture.resolutions.captured()[0]
			if err := unit.Validate(); err != nil {
				t.Fatalf("first-settlement unit does not validate its kernel commit: %v", err)
			}
			want := answerReduction(t, aggregate, actor.ID, testCase.decision, testCase.response, fixture.clock.now)
			if !reflect.DeepEqual(unit.Commit, want) {
				t.Fatalf("unit commit = %+v; want the reducer's %+v", unit.Commit, want)
			}
			if unit.Commit.NextExecutionState != testCase.nextExecution || unit.Commit.NextActionState != testCase.nextAction {
				t.Fatalf("commit next states = %q/%q; want %q/%q",
					unit.Commit.NextExecutionState, unit.Commit.NextActionState, testCase.nextExecution, testCase.nextAction)
			}
			if unit.Commit.ExpectedExecutionVersion != unit.ExpectedExecutionVersion || unit.Commit.ExpectedActionVersion != unit.ExpectedActionVersion {
				t.Fatalf("commit expected versions = %d/%d; want the unit's %d/%d",
					unit.Commit.ExpectedExecutionVersion, unit.Commit.ExpectedActionVersion,
					unit.ExpectedExecutionVersion, unit.ExpectedActionVersion)
			}
			if unit.Commit.Answer == nil || unit.Commit.Answer.Decision != want.Answer.Decision || unit.Commit.Answer.Actor != actor.ID {
				t.Fatalf("commit answer = %+v; want decision %q and actor %q", unit.Commit.Answer, testCase.decision, actor.ID)
			}
			if testCase.wantFailure {
				if unit.Commit.Failure == nil {
					t.Fatal("failing answer commit carries no typed failure")
				}
				if !reflect.DeepEqual(unit.Commit.Output, flow.OutputChange{Action: flow.OutputDiscard}) {
					t.Fatalf("failing output change = %+v; want discard", unit.Commit.Output)
				}
			} else {
				if !reflect.DeepEqual(unit.Commit.Output.Values, testCase.wantPromoted) {
					t.Fatalf("passing output promotion = %+v; want %+v", unit.Commit.Output, testCase.wantPromoted)
				}
			}
			if len(unit.Commit.PendingCancellations) != testCase.wantCancellation {
				t.Fatalf("pending cancellations = %d; want %d", len(unit.Commit.PendingCancellations), testCase.wantCancellation)
			}
			if fixture.resolutions.validationCount() != 1 {
				t.Fatalf("port first-settlement validations = %d; want one", fixture.resolutions.validationCount())
			}
		})
	}
}

func TestResolveReplayCarriesZeroCommitAndUsesReceiptFirst(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	original := resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})
	first, err := fixture.service.Resolve(context.Background(), original)
	requireNoError(t, err, "first Resolve")

	second, err := fixture.service.Resolve(context.Background(), original)
	requireNoError(t, err, "retried Resolve")
	if !second.Replayed {
		t.Fatal("identical retry did not report a replay")
	}
	units := fixture.resolutions.captured()
	if len(units) != 2 {
		t.Fatalf("resolve port calls = %d; want two", len(units))
	}
	if units[0].Commit.ExecutionID == "" {
		t.Fatal("first settlement unit carried no kernel commit")
	}
	if !reflect.DeepEqual(units[1].Commit, flow.Commit{}) {
		t.Fatalf("replay unit carried a kernel commit: %+v", units[1].Commit)
	}
	if err := units[1].ValidateReplay(); err != nil {
		t.Fatalf("replay unit does not validate as a receipt-first replay: %v", err)
	}
	if second.ExecutionVersion != first.ExecutionVersion || second.ActionVersion != first.ActionVersion {
		t.Fatalf("replay result = %+v; want the recorded %+v", second, first)
	}
	// The port must reach the receipt before requiring a reduction; only the
	// first settlement may be validated as a fresh commit.
	if fixture.resolutions.validationCount() != 1 {
		t.Fatalf("port first-settlement validations = %d; want one (replay is receipt-first)", fixture.resolutions.validationCount())
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one (replay applies nothing)", fixture.store.commitCount())
	}
}

func TestResolveFirstSettlementCannotForgeAUnitCommitMismatch(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
	requireNoError(t, err, "Resolve")
	unit := fixture.resolutions.captured()[0]

	tamperedActor := unit
	tamperedActor.Actor = "attacker"
	requireFaultClass(t, tamperedActor.Validate(), fault.InvalidInput)

	tamperedDecision := unit
	tamperedDecision.Decision = request.DecisionFail
	requireFaultClass(t, tamperedDecision.Validate(), fault.InvalidInput)

	tamperedVersion := unit
	tamperedVersion.ExpectedExecutionVersion++
	requireFaultClass(t, tamperedVersion.Validate(), fault.InvalidInput)

	missingCommit := unit
	missingCommit.Commit = flow.Commit{}
	requireFaultClass(t, missingCommit.Validate(), fault.InvalidInput)
}
