package task_test

import (
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const stageTwoPipeline = pipeline.PipelineID("pipeline-integrate")

func restartCommand(actor authorize.Principal, expectedLifecycle uint64, reason string) task.RestartCommand {
	return task.RestartCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		ExpectedLifecycleVersion: expectedLifecycle,
		Reason:                   reason,
		CallerKey:                "caller-restart",
		Content:                  receiptContent(`{"restart":"task"}`),
	}
}

func TestStartAtomicPipelineAndInputSwitch(t *testing.T) {
	t.Run("engineer switches pipeline and inputs atomically on open task", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.reader.put(openTaskWithoutExecution())

		cmd := startCommand(actor, 4)
		cmd.PipelineID = stageTwoPipeline
		cmd.Inputs = map[id.InputKey]string{"branch": "work/feat-1", "repository": "shop/web"}

		result, err := env.service.Start(t.Context(), cmd)
		requireNoError(t, err, "Start with pipeline switch")
		if result.ExecutionID == "" || result.LifecycleVersion != 5 {
			t.Fatalf("start result = %+v", result)
		}

		admitted := env.admission.requestsSnapshot()[0]
		if admitted.Definition.ID != stageTwoPipeline {
			t.Fatalf("admitted pipeline = %q; want %q", admitted.Definition.ID, stageTwoPipeline)
		}
		if admitted.Inputs["branch"] != "work/feat-1" || admitted.Inputs["repository"] != "shop/web" {
			t.Fatalf("admitted inputs = %+v", admitted.Inputs)
		}

		unit := env.ports.lastStart(t)
		if unit.PipelineID != stageTwoPipeline || !unit.UpdateInputs || !unit.UpdateInitiator {
			t.Fatalf("start unit flags = pipeline:%q updateInputs:%v updateInitiator:%v", unit.PipelineID, unit.UpdateInputs, unit.UpdateInitiator)
		}
		if unit.ExpectedState != task.StateOpen {
			t.Fatalf("expected state = %q; want open", unit.ExpectedState)
		}
		if unit.Initiator != actor.ID {
			t.Fatalf("initiator = %q; want selecting actor %q", unit.Initiator, actor.ID)
		}
		requireAuthority(t, unit.Authority, "task.start", authorize.CheckMembership)
		requireAuthority(t, unit.Authority, "task.select_pipeline", authorize.CheckMembership)
	})

	t.Run("engineer transitions done task to a new pipeline stage atomically", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		done := doneTask()
		done.CurrentExecution = executionOne
		env.reader.put(done)
		env.executions.put(task.ExecutionView{
			ID: executionOne, Version: 5, State: kernel.ExecutionSucceeded, StopEvidence: confirmedStop(),
		})

		cmd := startCommand(actor, done.LifecycleVersion)
		cmd.PipelineID = stageTwoPipeline
		cmd.Inputs = map[id.InputKey]string{"branch": "work/feat-1"}

		_, err := env.service.Start(t.Context(), cmd)
		requireNoError(t, err, "Start stage transition from done")
		unit := env.ports.lastStart(t)
		if unit.ExpectedState != task.StateDone {
			t.Fatalf("unit expected state = %q; want done", unit.ExpectedState)
		}
		if unit.PipelineID != stageTwoPipeline || !unit.UpdateInputs || !unit.UpdateInitiator {
			t.Fatalf("unit = %+v", unit)
		}
	})

	t.Run("done task refuses start when pipeline is unchanged", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		done := doneTask()
		env.reader.put(done)

		cmd := startCommand(actor, done.LifecycleVersion)
		cmd.PipelineID = done.SelectedPipeline
		_, err := env.service.Start(t.Context(), cmd)
		requireFaultClass(t, err, fault.VersionConflict)
		requireNoPortCalls(t, env.ports)
	})

	t.Run("waiting and cancelled tasks refuse start even with a new pipeline", func(t *testing.T) {
		for name, value := range map[string]task.Task{
			"waiting":   waitingTask(t),
			"cancelled": cancelledTask(),
		} {
			t.Run(name, func(t *testing.T) {
				env := newTestEnv(t)
				actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
				env.reader.put(value)

				cmd := startCommand(actor, value.LifecycleVersion)
				cmd.PipelineID = stageTwoPipeline
				_, err := env.service.Start(t.Context(), cmd)
				requireFaultClass(t, err, fault.VersionConflict)
				requireNoPortCalls(t, env.ports)
			})
		}
	})

	t.Run("operator-only caller cannot switch pipeline or override inputs on task.start", func(t *testing.T) {
		for name, mutate := range map[string]func(*task.StartCommand){
			"different pipeline": func(cmd *task.StartCommand) { cmd.PipelineID = stageTwoPipeline },
			"override inputs":    func(cmd *task.StartCommand) { cmd.Inputs = map[id.InputKey]string{"repository": "evil/repo"} },
		} {
			t.Run(name, func(t *testing.T) {
				env := newTestEnv(t)
				env.reader.put(failedOpenTask(t, env, 3))
				actor := humanActor(operatorPrincipal, tenancy.RoleOperator)

				cmd := startCommand(actor, 4)
				mutate(&cmd)
				_, err := env.service.Start(t.Context(), cmd)
				requireFaultClass(t, err, fault.Forbidden)
				if env.admission.callCount() != 0 {
					t.Fatalf("admission called for unauthorized operator selection")
				}
				requireNoPortCalls(t, env.ports)
			})
		}
	})
}

func TestRestartSupersedesWaitingRequestWithoutFailVerdict(t *testing.T) {
	env := newTestEnv(t)
	aggregate := runningRequestAggregate()
	seedAggregate(env, aggregate)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	cmd := restartCommand(actor, 4, "rebase onto latest main")
	cmd.PipelineID = stageTwoPipeline
	cmd.Inputs = map[id.InputKey]string{"branch": "work/rebased"}

	result, err := env.service.Restart(t.Context(), cmd)
	requireNoError(t, err, "Restart while waiting at request")
	if result.ExecutionID == "" || result.SupersededExecution != executionOne || result.LifecycleVersion != 5 {
		t.Fatalf("restart result = %+v; want superseded %q and lifecycle 5", result, executionOne)
	}

	unit := env.ports.lastStart(t)
	if unit.CancelCommit == nil {
		t.Fatal("restart of active execution carried no cancel commit")
	}
	if unit.CancelCommit.NextExecutionState != kernel.ExecutionCancelled ||
		unit.CancelCommit.NextActionState != kernel.ActionCancelled {
		t.Fatalf("cancel commit states = %q/%q; want cancelled/cancelled", unit.CancelCommit.NextExecutionState, unit.CancelCommit.NextActionState)
	}
	if unit.CancelCommit.Answer != nil {
		t.Fatalf("restart fabricated a request answer: %+v", unit.CancelCommit.Answer)
	}
	if unit.CancelCommit.Failure == nil ||
		unit.CancelCommit.Failure.Kind != kernel.FailureCancelled ||
		unit.CancelCommit.Failure.Reason != string(flow.CancelSuperseded) ||
		unit.CancelCommit.Failure.Detail != "rebase onto latest main" {
		t.Fatalf("cancel commit failure = %+v; want cancelled/superseded/reason", unit.CancelCommit.Failure)
	}
	if unit.CancelCommit.Stop != nil {
		t.Fatalf("request wait cancel emitted unexpected stop intent: %+v", unit.CancelCommit.Stop)
	}
	if unit.CredentialUpdate == nil || unit.CredentialUpdate.Present() {
		t.Fatalf("credential update = %+v; want revoked empty credential", unit.CredentialUpdate)
	}
	if unit.ExpectedExecutionVersion != aggregate.Version {
		t.Fatalf("expected execution version = %d; want %d", unit.ExpectedExecutionVersion, aggregate.Version)
	}
	if unit.PipelineID != stageTwoPipeline || !unit.UpdateInputs || !unit.UpdateInitiator {
		t.Fatalf("unit stage update = %q/%v/%v", unit.PipelineID, unit.UpdateInputs, unit.UpdateInitiator)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Operation != "task.restart" || unit.Audit[0].Detail != "rebase onto latest main" {
		t.Fatalf("audit = %+v; want task.restart with detail", unit.Audit)
	}

	// Replacement execution must NOT dispatch immediately while predecessor is being superseded.
	requirePublish(t, unit.PostCommit)
	requireNoDispatch(t, unit.PostCommit)

	// Authority reads must enforce restart, unconditional start (no operator), cancel, select_pipeline and version pins.
	requireAuthority(t, unit.Authority, "task.restart", authorize.CheckMembership)
	foundUnconditionalStart := false
	for _, read := range unit.Authority {
		if read.Operation == "task.start" && read.Check == authorize.CheckMembership {
			if len(read.Required.HumanRoles) != 1 || read.Required.HumanRoles[0] != tenancy.RoleEngineer {
				t.Fatalf("task.start membership read in restart = %+v; want engineer-only unconditional start", read.Required)
			}
			foundUnconditionalStart = true
		}
	}
	if !foundUnconditionalStart {
		t.Fatal("restart unit missing unconditional task.start membership read")
	}
	requireAuthority(t, unit.Authority, "task.cancel", authorize.CheckMembership)
	requireAuthority(t, unit.Authority, "task.select_pipeline", authorize.CheckMembership)
	requireAuthority(t, unit.Authority, "task.restart", authorize.CheckTaskVersion)
	requireAuthority(t, unit.Authority, "task.restart", authorize.CheckPipelineVersion)
	execPin := requireAuthority(t, unit.Authority, "task.restart", authorize.CheckExecutionVersion)
	if execPin.TargetID != string(executionOne) || execPin.ExpectedVersion != aggregate.Version {
		t.Fatalf("execution pin = %+v; want %q@%d", execPin, executionOne, aggregate.Version)
	}

	// Events must include queued replacement + projected cancellation of predecessor.
	requireEvent(t, unit.Events, event.TypeExecutionQueued, actor.Scope, string(result.ExecutionID), 1, testNow)
	requireEvent(t, unit.Events, event.TypeExecutionCancelled, actor.Scope, string(executionOne), aggregate.Version+1, testNow)
	requireEvent(t, unit.Events, event.TypeCleanup, actor.Scope, string(executionOne), aggregate.Version+1, testNow)
}

func TestRestartWithRunningContainerEmitsStopAndWaitsForBarrier(t *testing.T) {
	env := newTestEnv(t)
	aggregate := runningContainerAggregate()
	seedAggregate(env, aggregate)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	result, err := env.service.Restart(t.Context(), restartCommand(actor, 4, "stuck container"))
	requireNoError(t, err, "Restart running container")
	if result.SupersededExecution != executionOne {
		t.Fatalf("superseded = %q; want %q", result.SupersededExecution, executionOne)
	}
	unit := env.ports.lastStart(t)
	if unit.CancelCommit == nil || unit.CancelCommit.Stop == nil || unit.CancelCommit.Stop.WorkloadID != cancelWorkload {
		t.Fatalf("cancel commit stop = %+v; want workload %q", unit.CancelCommit, cancelWorkload)
	}
	requirePublish(t, unit.PostCommit)
	requireNoDispatch(t, unit.PostCommit)
}

func TestRestartWhenNoActiveExecutionStartsCleanly(t *testing.T) {
	t.Run("no prior execution", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.reader.put(openTaskWithoutExecution())

		result, err := env.service.Restart(t.Context(), restartCommand(actor, 4, ""))
		requireNoError(t, err, "Restart without prior execution")
		if result.SupersededExecution != "" {
			t.Fatalf("superseded = %q; want empty", result.SupersededExecution)
		}
		unit := env.ports.lastStart(t)
		if unit.CancelCommit != nil || unit.ExpectedExecutionVersion != 0 {
			t.Fatalf("unexpected cancel fields: commit=%+v ver=%d", unit.CancelCommit, unit.ExpectedExecutionVersion)
		}
		requireDispatch(t, unit.PostCommit)
	})

	for _, state := range []kernel.ExecutionState{kernel.ExecutionFailed, kernel.ExecutionCancelled, kernel.ExecutionSucceeded} {
		t.Run("terminal "+string(state)+" predecessor with confirmed stop", func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
			aggregate := terminalAggregate(state, 6, kernel.Cleanup{State: kernel.CleanupComplete})
			value := baseTask()
			value.CurrentExecution = aggregate.ID
			env.reader.put(value)
			env.executions.put(task.ExecutionView{
				ID: aggregate.ID, Version: aggregate.Version, State: aggregate.State, StopEvidence: confirmedStop(),
			})
			env.executions.putAggregate(aggregate)

			result, err := env.service.Restart(t.Context(), restartCommand(actor, 4, "fresh attempt"))
			requireNoError(t, err, "Restart after terminal predecessor")
			if result.SupersededExecution != "" {
				t.Fatalf("superseded = %q; want empty for terminal predecessor", result.SupersededExecution)
			}
			unit := env.ports.lastStart(t)
			if unit.CancelCommit != nil {
				t.Fatalf("terminal predecessor must not carry a cancel commit: %+v", unit.CancelCommit)
			}
			if unit.ExpectedExecutionVersion != aggregate.Version {
				t.Fatalf("expected execution version = %d; want %d", unit.ExpectedExecutionVersion, aggregate.Version)
			}
			requireDispatch(t, unit.PostCommit)
		})
	}
}

func TestRestartRefusesTerminalWaitingUnauthorizedAndOversizedReason(t *testing.T) {
	t.Run("refuses waiting, done, and cancelled tasks", func(t *testing.T) {
		for name, value := range map[string]task.Task{
			"waiting":   waitingTask(t),
			"done":      doneTask(),
			"cancelled": cancelledTask(),
		} {
			t.Run(name, func(t *testing.T) {
				env := newTestEnv(t)
				actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
				env.reader.put(value)

				_, err := env.service.Restart(t.Context(), restartCommand(actor, value.LifecycleVersion, "retry"))
				requireFaultClass(t, err, fault.VersionConflict)
				requireNoPortCalls(t, env.ports)
			})
		}
	})

	t.Run("refuses non-engineer principals", func(t *testing.T) {
		for name, actor := range map[string]authorize.Principal{
			"operator":  humanActor(operatorPrincipal, tenancy.RoleOperator),
			"reviewer":  humanActor(reviewerPrincipal, tenancy.RoleReviewer),
			"viewer":    humanActor(id.PrincipalID("human-viewer"), tenancy.RoleViewer),
			"admin":     humanActor(id.PrincipalID("human-admin"), tenancy.RoleAdmin),
			"execution": executionActor(id.PrincipalID("exec-principal-1"), executionOne),
		} {
			t.Run(name, func(t *testing.T) {
				env := newTestEnv(t)
				env.principals.put(identity.Principal{
					ID: actor.ID, Kind: actor.Kind, Scope: actor.Scope, Roles: actor.Roles,
					ExecutionID: actor.ExecutionID, Enabled: true, Version: 1,
				})
				seedAggregate(env, runningRequestAggregate())

				_, err := env.service.Restart(t.Context(), restartCommand(actor, 4, "retry"))
				requireFaultClass(t, err, fault.Forbidden)
				requireNoPortCalls(t, env.ports)
			})
		}
	})

	t.Run("refuses oversized or non-UTF-8 reason", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.reader.put(openTaskWithoutExecution())

		_, err := env.service.Restart(t.Context(), restartCommand(actor, 4, strings.Repeat("a", 4097)))
		requireFaultClass(t, err, fault.InvalidInput)
		_, err = env.service.Restart(t.Context(), restartCommand(actor, 4, string([]byte{0xff, 0xfe})))
		requireFaultClass(t, err, fault.InvalidInput)
		requireNoPortCalls(t, env.ports)
	})
}
