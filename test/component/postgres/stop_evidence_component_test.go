//go:build component

package postgrescomponent

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/execution"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestExecutionReaderStopEvidenceOnPostgreSQL proves the task service's
// predecessor gate is derived from rows the Kernel actually persisted: a
// non-terminal execution, a terminal execution whose cleanup is still owed, and
// a terminal execution with an unconfirmed container stop barrier all report
// pending evidence, while completed cleanup or every possibly started
// container's confirmed barrier report confirmed evidence.
func TestExecutionReaderStopEvidenceOnPostgreSQL(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID)
		want  bool
	}{
		"non-terminal execution": {
			setup: func(*testing.T, context.Context, *serviceEnv, id.ExecutionID) {},
			want:  false,
		},
		"terminal with cleanup pending and no possibly started action": {
			setup: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				writeCleanupRow(t, ctx, env, executionID, 1, kernel.CleanupPending, kernel.ExecutionFailed, "workload-1")
			},
			want: false,
		},
		"terminal with cleanup pending and an unconfirmed container barrier": {
			setup: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				version := writeCleanupRow(t, ctx, env, executionID, 1, kernel.CleanupPending, kernel.ExecutionFailed, "workload-1")
				writeStopRow(t, ctx, env, executionID, version, 1,
					kernel.PendingStop("stop response lost"), kernel.ExecutionFailed)
			},
			want: false,
		},
		"terminal with completed cleanup": {
			setup: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				writeCleanupRow(t, ctx, env, executionID, 1, kernel.CleanupComplete, kernel.ExecutionSucceeded, "workload-1")
			},
			want: true,
		},
		"terminal with cleanup pending and every container barrier confirmed": {
			setup: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				version := writeCleanupRow(t, ctx, env, executionID, 1, kernel.CleanupPending, kernel.ExecutionCancelled, "workload-1")
				writeStopRow(t, ctx, env, executionID, version, 1,
					kernel.ConfirmedStop("runtime confirmed termination"), kernel.ExecutionCancelled)
			},
			want: true,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			env := newServiceEnv(t)
			executionID := startedExecution(t, env)
			testCase.setup(t, ctx, env, executionID)
			view, err := env.executions.Get(ctx, env.scope, executionID)
			if err != nil {
				t.Fatalf("Get: %v%s", err, pgtest.CauseText(err))
			}
			if got := view.StopEvidence.IsConfirmed(); got != testCase.want {
				t.Fatalf("stop evidence = %+v (confirmed=%v); want confirmed=%v",
					view.StopEvidence, got, testCase.want)
			}
		})
	}
}

// TestEngineActivationRefusesUnconfirmedPredecessorOnPostgreSQL is the
// activation-commit regression on real PostgreSQL rows. A task reply commits a
// fresh queued execution while the predecessor's stop is still unconfirmed
// (its cleanup is owed and it carries no confirmed barrier). The Kernel state
// adapter must refuse the replacement's activation at the commit boundary and
// write nothing; once durable stop evidence exists, the same activation commits
// exactly once.
func TestEngineActivationRefusesUnconfirmedPredecessorOnPostgreSQL(t *testing.T) {
	cases := map[string]struct {
		release func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID)
	}{
		"confirmed barrier with cleanup pending": {
			release: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				writeStopRow(t, ctx, env, executionID, 2, 2,
					kernel.ConfirmedStop("runtime confirmed termination"), kernel.ExecutionCancelled)
			},
		},
		"completed cleanup": {
			release: func(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID) {
				writeCleanupRow(t, ctx, env, executionID, 2, kernel.CleanupComplete,
					kernel.ExecutionCancelled, "workload-1")
			},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			env := newServiceEnv(t)
			predecessor := startedExecution(t, env)
			const taskID = id.TaskID("task-1")
			_, err := env.task.Ask(ctx, task.AskCommand{
				Actor: env.actor(), Scope: env.scope, TaskID: taskID, ExpectedLifecycleVersion: 2,
				Question: "Which branch?", Assignment: requestAssignment(), CallerKey: "task-ask",
				Content: integrationContent("task-ask"),
			})
			if err != nil {
				t.Fatalf("task ask: %v%s", err, pgtest.CauseText(err))
			}
			waiting, err := env.reader.Get(ctx, env.scope, taskID)
			if err != nil {
				t.Fatalf("read waiting task: %v%s", err, pgtest.CauseText(err))
			}
			if waiting.State != task.StateWaiting || waiting.Question == nil {
				t.Fatalf("task after ask = %+v; want waiting with a question", waiting)
			}
			replied, err := env.task.Reply(ctx, task.ReplyCommand{
				Actor: env.actor(), Scope: env.scope, TaskID: taskID, ExpectedLifecycleVersion: 3,
				QuestionID: waiting.Question.ID, ExpectedQuestionVersion: waiting.Question.Version,
				Answer: "main", CallerKey: "task-reply", Content: integrationContent("task-reply"),
			})
			if err != nil {
				t.Fatalf("task reply: %v%s", err, pgtest.CauseText(err))
			}
			if replied.ExecutionID == "" || replied.ExecutionID == predecessor {
				t.Fatalf("reply execution = %q; want a fresh execution distinct from %q", replied.ExecutionID, predecessor)
			}
			// The reply committed while the predecessor stop was unconfirmed;
			// the reader must have refused to dispatch it, but the execution
			// row exists and is queued.
			view, err := env.executions.Get(ctx, env.scope, predecessor)
			if err != nil {
				t.Fatalf("read predecessor evidence: %v%s", err, pgtest.CauseText(err))
			}
			if view.StopEvidence.IsConfirmed() {
				t.Fatalf("predecessor stop evidence = %+v; want unconfirmed before the release", view.StopEvidence)
			}

			engine := executionstore.NewEngine(adapterController(t, env.shard))
			actionID := snapshotdoc.DeriveActionID(replied.ExecutionID, 0)
			activation := flow.Commit{
				ExecutionID: replied.ExecutionID, ExpectedExecutionVersion: 1,
				NextExecutionState: kernel.ExecutionRunning,
				ActionID:           actionID, ExpectedActionVersion: 1, NextActionState: kernel.ActionRunning,
				Output:   flow.OutputChange{Action: flow.OutputNone},
				Deadline: pgtest.FixtureTime.Add(time.Minute), At: pgtest.FixtureTime,
			}
			if _, err := engine.Commit(ctx, activation); !isFaultClass(err, fault.VersionConflict) {
				t.Fatalf("activation before confirmed stop = %v; want version_conflict%s", err, pgtest.CauseText(err))
			}
			held, err := engine.Load(ctx, replied.ExecutionID)
			if err != nil {
				t.Fatalf("load held replacement: %v%s", err, pgtest.CauseText(err))
			}
			if held.State != kernel.ExecutionQueued || held.Actions[0].State != kernel.ActionPending ||
				!held.Actions[0].ActivatedAt.IsZero() || !held.Actions[0].Deadline.IsZero() {
				t.Fatalf("held replacement = %+v; want queued with an unprepared pending action", held)
			}
			pgtest.RequireRowCount(t, ctx, env.shard.Admin(),
				`SELECT count(*)::text FROM events WHERE type = 'execution.started' AND subject_id = $1`,
				0, "refused activation event", string(replied.ExecutionID))

			testCase.release(t, ctx, env, predecessor)
			activated, err := engine.Commit(ctx, activation)
			if err != nil {
				t.Fatalf("activation after confirmed stop: %v%s", err, pgtest.CauseText(err))
			}
			if activated.State != kernel.ExecutionRunning || activated.Actions[0].State != kernel.ActionRunning {
				t.Fatalf("activated replacement = %+v; want running", activated)
			}
			if _, err := engine.Commit(ctx, activation); !isFaultClass(err, fault.VersionConflict) {
				t.Fatalf("second activation = %v; want version_conflict; the replacement must activate once", err)
			}
		})
	}
}

// isFaultClass reports whether err carries the exact public fault class.
func isFaultClass(err error, want fault.Class) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == want
}

// startedExecution applies one pipeline, creates one task, starts it through the
// real task service and returns the committed execution identity.
func startedExecution(t *testing.T, env *serviceEnv) id.ExecutionID {
	t.Helper()
	ctx := context.Background()
	taskID := applyPipelineAndCreateTask(t, env)
	started, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: taskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v%s", err, pgtest.CauseText(err))
	}
	return started.ExecutionID
}

// writeCleanupRow commits one cleanup progress row through the real execution
// adapter and returns the next execution version. next is the execution state
// the same commit persists, so a fixture can represent cleanup progress on a
// terminal execution without inventing a state the Kernel never writes.
func writeCleanupRow(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID,
	version uint64, state kernel.CleanupState, next kernel.ExecutionState, workload id.WorkloadID) uint64 {
	t.Helper()
	result, err := env.exec.Transition(ctx, execution.TransitionUnit{
		Owner:                    env.scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       next,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate: &flow.CleanupUpdate{
				State:       state,
				WorkloadID:  workload,
				WorkspaceID: snapshotdoc.DeriveWorkspaceID(executionID),
				Attempts:    1,
			},
			At: pgtest.FixtureTime,
		},
	})
	if err != nil {
		t.Fatalf("commit cleanup %q: %v%s", state, err, pgtest.CauseText(err))
	}
	return result.ExecutionVersion
}

// writeStopRow commits one persisted stop barrier on the execution's first
// action. next is the execution state the same commit persists, so a fixture can
// represent a barrier recorded while the execution is still running or after it
// reached a terminal state.
func writeStopRow(t *testing.T, ctx context.Context, env *serviceEnv, executionID id.ExecutionID,
	version, actionVersion uint64, evidence kernel.StopEvidence, next kernel.ExecutionState) uint64 {
	t.Helper()
	actionID := snapshotdoc.DeriveActionID(executionID, 0)
	result, err := env.exec.Transition(ctx, execution.TransitionUnit{
		Owner:                    env.scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		ExpectedActionVersion:    actionVersion,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       next,
			ActionID:                 actionID,
			ExpectedActionVersion:    actionVersion,
			NextActionState:          kernel.ActionPending,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			StopEvidence:             &evidence,
			At:                       pgtest.FixtureTime,
		},
	})
	if err != nil {
		t.Fatalf("commit stop evidence: %v%s", err, pgtest.CauseText(err))
	}
	return result.ExecutionVersion
}
