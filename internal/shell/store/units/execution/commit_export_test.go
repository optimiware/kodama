package executionstore

// The execution adapter exposes its non-committing execution mapper for reuse
// by the sibling work store unit. These tests pin the two exported seams:
//
//   - PlanScopedCommit maps one flow.Commit to exactly the locks and writes the
//     engine/transition paths use, without executing anything and
//     without projecting events (callers assemble those from ProjectLifecycle/
//     ExcludeOwned).
//   - Store.Load is the scoped aggregate reader the task service builds the
//     Kernel cancel fact from; an out-of-scope, unknown or missing execution is
//     uniformly not_found.
//
// The golden comparison keeps planCommit as the regression baseline: the
// exported mapper must produce the same locks and writes the existing paths
// produced for a representative commit set.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// exportScenario is one representative commit and the rows it was reduced from.
type exportScenario struct {
	adapter    *Store
	backend    *memory.Store
	scope      tenancy.Scope
	commit     flow.Commit
	credential *token.Credential
}

// source reads a scenario through the same scoped seam the
// production paths use.
func (scenario exportScenario) source() scopedSource {
	return scopedSource{executor: scenario.adapter.executor, scanner: scenario.adapter.scanner, scope: scenario.scope}
}

// reducedCommit reduces one fact against the persisted aggregate and fails the
// test when the reducer refuses or converges to no commit.
func reducedCommit(t *testing.T, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID, fact flow.Fact) flow.Commit {
	t.Helper()
	pre := loadExecution(t, adapter, scope, executionID)
	_, commit, err := flow.Reduce(pre, fact)
	if err != nil {
		t.Fatalf("reduce %T: %v", fact, err)
	}
	if commit == nil {
		t.Fatalf("reduce %T converged to no commit", fact)
	}
	return *commit
}

// activateAction drives one activation commit through the real Kernel state
// adapter so the persisted launch intent is exactly what the reducer produced.
func activateAction(t *testing.T, engineState *Engine, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, at time.Time) {
	t.Helper()
	commit := reducedCommit(t, adapter, scope, executionID, flow.ActivateFact{
		ExecutionID: executionID,
		ActionID:    actionID,
		At:          at,
	})
	if _, err := engineState.Commit(context.Background(), commit); err != nil {
		t.Fatalf("activate %q: %v", actionID, err)
	}
}

// issueLaunch persists the launch-issued marker the engine commits after a
// successful Runtime.Start, so a later cancellation owes a Stop for a workload
// that may genuinely be running.
func issueLaunch(t *testing.T, engineState *Engine, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	execution := loadExecution(t, adapter, scope, executionID)
	for _, action := range execution.Actions {
		if action.ID != actionID {
			continue
		}
		if action.Container == nil {
			t.Fatalf("action %q has no container value", actionID)
		}
		launch := action.Container.Launch
		launch.State = kernel.LaunchIssued
		if _, err := engineState.Commit(context.Background(), flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: execution.Version,
			NextExecutionState:       execution.State,
			ActionID:                 action.ID,
			ExpectedActionVersion:    action.Version,
			NextActionState:          action.State,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			Launch:                   &launch,
			At:                       testNow,
		}); err != nil {
			t.Fatalf("issue launch for %q: %v", actionID, err)
		}
		return
	}
	t.Fatalf("no action %q in execution %q", actionID, executionID)
}

// cancelQueuedScenario starts one queued execution whose only action is a
// pending request, then reduces an explicit cancel: the pending request is
// cancelled with no StopIntent.
func cancelQueuedScenario(t *testing.T) exportScenario {
	t.Helper()
	adapter, backend := newStore(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-queued", requestOnlyAction())
	commit := reducedCommit(t, adapter, scope, "exec-queued", flow.CancelFact{
		ExecutionID: "exec-queued",
		Reason:      flow.CancelRequested,
		At:          testNow,
	})
	if commit.Stop != nil {
		t.Fatalf("queued cancellation carries a StopIntent: %+v", commit.Stop)
	}
	return exportScenario{adapter: adapter, backend: backend, scope: scope, commit: commit}
}

// cancelRunningScenario starts and activates one container action, then reduces
// an explicit cancel: the running container is cancelled with the conditional
// StopIntent for its workload and workspace.
func cancelRunningScenario(t *testing.T) exportScenario {
	t.Helper()
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-running", containerAction())
	activateAction(t, engineState, adapter, scope, "exec-running", actions[0], testNow)
	issueLaunch(t, engineState, adapter, scope, "exec-running", actions[0])
	commit := reducedCommit(t, adapter, scope, "exec-running", flow.CancelFact{
		ExecutionID: "exec-running",
		Reason:      flow.CancelRequested,
		At:          testNow.Add(time.Second),
	})
	if commit.Stop == nil || commit.Stop.WorkloadID == "" {
		t.Fatalf("running cancellation carries no StopIntent: %+v", commit.Stop)
	}
	return exportScenario{adapter: adapter, backend: backend, scope: scope, commit: commit}
}

// succeededActionScenario runs a container action to a confirmed successful exit
// while a request successor is still pending, so the commit settles one action
// and keeps the execution running.
func succeededActionScenario(t *testing.T) exportScenario {
	t.Helper()
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-succeeded", containerAction(), requestAction())
	activateAction(t, engineState, adapter, scope, "exec-succeeded", actions[0], testNow)
	commit := reducedCommit(t, adapter, scope, "exec-succeeded", flow.ObserveFact{
		ExecutionID: "exec-succeeded",
		ActionID:    actions[0],
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exit 0")),
		At:          testNow.Add(time.Second),
	})
	if commit.NextActionState != kernel.ActionSucceeded || commit.NextExecutionState != kernel.ExecutionRunning {
		t.Fatalf("unexpected succeeded-action commit: %+v", commit)
	}
	return exportScenario{adapter: adapter, backend: backend, scope: scope, commit: commit}
}

// requestActivationScenario activates the pending request successor after the
// container action succeeded, so the commit resolves the subject, writes the
// admitted assignment and clears the execution credential.
func requestActivationScenario(t *testing.T) exportScenario {
	t.Helper()
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	request := literalRequestAction()
	request.Index = 1
	actions := startedExecution(t, adapter, backend, scope, "exec-request", containerAction(), request)
	activateAction(t, engineState, adapter, scope, "exec-request", actions[0], testNow)
	succeeded := reducedCommit(t, adapter, scope, "exec-request", flow.ObserveFact{
		ExecutionID: "exec-request",
		ActionID:    actions[0],
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exit 0")),
		At:          testNow.Add(time.Second),
	})
	if _, err := engineState.Commit(context.Background(), succeeded); err != nil {
		t.Fatalf("succeed container: %v", err)
	}
	commit := reducedCommit(t, adapter, scope, "exec-request", flow.ActivateFact{
		ExecutionID: "exec-request",
		ActionID:    actions[1],
		At:          testNow.Add(2 * time.Second),
	})
	if commit.Subject == nil {
		t.Fatal("request activation commit carries no resolved subject")
	}
	credential := &token.Credential{Nonce: []byte("nonce-1"), Expiry: testNow.Add(time.Hour)}
	return exportScenario{adapter: adapter, backend: backend, scope: scope, commit: commit, credential: credential}
}

// terminalScenario runs a container action to its persisted deadline, so the
// commit carries a typed failure, the pending cleanup and the stop intent.
func terminalScenario(t *testing.T) exportScenario {
	t.Helper()
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-terminal", containerAction())
	activateAction(t, engineState, adapter, scope, "exec-terminal", actions[0], testNow)
	commit := reducedCommit(t, adapter, scope, "exec-terminal", flow.ExpireFact{
		ExecutionID: "exec-terminal",
		ActionID:    actions[0],
		At:          testNow.Add(90 * time.Second),
	})
	if commit.NextExecutionState != kernel.ExecutionFailed || commit.Failure == nil {
		t.Fatalf("unexpected terminal commit: %+v", commit)
	}
	return exportScenario{adapter: adapter, backend: backend, scope: scope, commit: commit}
}

func exportScenarios(t *testing.T) map[string]exportScenario {
	t.Helper()
	return map[string]exportScenario{
		"cancel queued":                cancelQueuedScenario(t),
		"cancel running container":     cancelRunningScenario(t),
		"action succeeded":             succeededActionScenario(t),
		"request activation":           requestActivationScenario(t),
		"terminal deadline expiration": terminalScenario(t),
	}
}

// TestPlanScopedCommitMatchesPlanCommit is the golden regression: for every
// representative commit the exported mapper produces exactly the locks and
// writes the existing planCommit path produced, for both the scoped
// (project=false) and projected engine (project=true) paths. It also proves the
// mapper commits nothing and, when unprojected, carries no events.
func TestPlanScopedCommitMatchesPlanCommit(t *testing.T) {
	ctx := context.Background()
	for name, scenario := range exportScenarios(t) {
		t.Run(name, func(t *testing.T) {
			source := scenario.source()
			before := loadExecution(t, scenario.adapter, scenario.scope, scenario.commit.ExecutionID)
			for _, project := range []bool{false, true} {
				legacy, err := planCommit(ctx, source, scenario.commit, scenario.credential, nil, project)
				if err != nil {
					t.Fatalf("planCommit(project=%t): %v", project, err)
				}
				plan, err := PlanScopedCommit(ctx, source.read, scenario.commit, scenario.credential, nil)
				if err != nil {
					t.Fatalf("PlanScopedCommit: %v", err)
				}
				if !reflect.DeepEqual(plan.Locks, legacy.locks) {
					t.Fatalf("locks diverged (project=%t):\nexported = %+v\nlegacy   = %+v", project, plan.Locks, legacy.locks)
				}
				if !reflect.DeepEqual(plan.Writes, legacy.writes) {
					t.Fatalf("writes diverged (project=%t):\nexported = %+v\nlegacy   = %+v", project, plan.Writes, legacy.writes)
				}
				if !project && len(legacy.events) != 0 {
					t.Fatalf("unprojected plan carries %d events", len(legacy.events))
				}
			}
			after := loadExecution(t, scenario.adapter, scenario.scope, scenario.commit.ExecutionID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("planning committed state: before=%+v after=%+v", before, after)
			}
		})
	}
}

// findWrite returns the one write for a table/id, failing when it is absent.
func findWrite(t *testing.T, writes []store.Write, table, identity string) store.Write {
	t.Helper()
	for _, write := range writes {
		if write.Table == table && write.ID == identity {
			return write
		}
	}
	t.Fatalf("no %s/%s write in %+v", table, identity, writes)
	return store.Write{}
}

// TestPlanScopedCommitCancelQueuedPersistsCancellation pins the queued-cancel
// mapping: the execution and its pending action cancel, the credential columns
// are cleared, the cleanup row is inserted pending and no StopIntent is written.
func TestPlanScopedCommitCancelQueuedPersistsCancellation(t *testing.T) {
	scenario := cancelQueuedScenario(t)
	plan, err := PlanScopedCommit(context.Background(), scenario.source().read, scenario.commit, nil, nil)
	if err != nil {
		t.Fatalf("PlanScopedCommit: %v", err)
	}
	executionWrite := findWrite(t, plan.Writes, tableExecutions, "exec-queued")
	if executionWrite.Values["state"] != string(kernel.ExecutionCancelled) {
		t.Fatalf("execution state = %v", executionWrite.Values["state"])
	}
	if executionWrite.Values["token_nonce"] != nil || executionWrite.Values["token_expires_at"] != nil {
		t.Fatalf("credential columns not cleared: %+v", executionWrite.Values)
	}
	if len(scenario.commit.PendingCancellations) != 1 {
		t.Fatalf("queued cancel pending cancellations = %d; want 1", len(scenario.commit.PendingCancellations))
	}
	actionWrite := findWrite(t, plan.Writes, tableActions, string(scenario.commit.PendingCancellations[0].ActionID))
	if actionWrite.Values["state"] != string(kernel.ActionCancelled) {
		t.Fatalf("pending action state = %v", actionWrite.Values["state"])
	}
	cleanupWrite := findWrite(t, plan.Writes, tableCleanup, cleanupID("exec-queued"))
	if !cleanupWrite.Insert || cleanupWrite.Values["state"] != string(kernel.CleanupPending) {
		t.Fatalf("cleanup write = %+v", cleanupWrite)
	}
}

// TestPlanScopedCommitCancelRunningContainerCarriesStop pins the running-cancel
// mapping: the running action cancels, the cleanup row records the workload and
// the commit still carries the reducer's StopIntent (the mapper persists exactly
// what the existing path persisted).
func TestPlanScopedCommitCancelRunningContainerCarriesStop(t *testing.T) {
	scenario := cancelRunningScenario(t)
	if scenario.commit.Stop == nil {
		t.Fatal("running cancel carries no StopIntent")
	}
	plan, err := PlanScopedCommit(context.Background(), scenario.source().read, scenario.commit, nil, nil)
	if err != nil {
		t.Fatalf("PlanScopedCommit: %v", err)
	}
	actionWrite := findWrite(t, plan.Writes, tableActions, string(scenario.commit.ActionID))
	if actionWrite.Values["state"] != string(kernel.ActionCancelled) {
		t.Fatalf("running action state = %v", actionWrite.Values["state"])
	}
	cleanupWrite := findWrite(t, plan.Writes, tableCleanup, cleanupID("exec-running"))
	if cleanupWrite.Values["workload_id"] != string(scenario.commit.Stop.WorkloadID) {
		t.Fatalf("cleanup workload = %v; want %q", cleanupWrite.Values["workload_id"], scenario.commit.Stop.WorkloadID)
	}
	if cleanupWrite.Values["workspace_id"] != string(scenario.commit.Stop.WorkspaceID) {
		t.Fatalf("cleanup workspace = %v; want %q", cleanupWrite.Values["workspace_id"], scenario.commit.Stop.WorkspaceID)
	}
}

// TestPlanScopedCommitRequestActivationPersistsSubjectAndCredential pins the
// request-activation mapping: the resolved subject and the admitted assignment
// are written beside the running request, and a present credential is stored.
func TestPlanScopedCommitRequestActivationPersistsSubjectAndCredential(t *testing.T) {
	scenario := requestActivationScenario(t)
	plan, err := PlanScopedCommit(context.Background(), scenario.source().read, scenario.commit, scenario.credential, nil)
	if err != nil {
		t.Fatalf("PlanScopedCommit: %v", err)
	}
	actionWrite := findWrite(t, plan.Writes, tableActions, string(scenario.commit.ActionID))
	if actionWrite.Values["state"] != string(kernel.ActionRunning) {
		t.Fatalf("request action state = %v", actionWrite.Values["state"])
	}
	if actionWrite.Values["request_subject"] == nil || actionWrite.Values["request_assignment"] == nil {
		t.Fatalf("request subject/assignment not written: %+v", actionWrite.Values)
	}
	if actionWrite.Values["deadline"] != scenario.commit.Deadline {
		t.Fatalf("deadline = %v; want %v", actionWrite.Values["deadline"], scenario.commit.Deadline)
	}
	executionWrite := findWrite(t, plan.Writes, tableExecutions, "exec-request")
	if !reflect.DeepEqual(executionWrite.Values["token_nonce"], scenario.credential.Nonce) {
		t.Fatalf("token nonce = %v; want %v", executionWrite.Values["token_nonce"], scenario.credential.Nonce)
	}
	if executionWrite.Values["token_expires_at"] != scenario.credential.Expiry {
		t.Fatalf("token expiry = %v; want %v", executionWrite.Values["token_expires_at"], scenario.credential.Expiry)
	}
}

// TestPlanScopedCommitTerminalPersistsFailure pins the terminal mapping: the
// typed failure is encoded onto the execution and the failed action row and the
// cleanup progress commit together.
func TestPlanScopedCommitTerminalPersistsFailure(t *testing.T) {
	scenario := terminalScenario(t)
	plan, err := PlanScopedCommit(context.Background(), scenario.source().read, scenario.commit, nil, nil)
	if err != nil {
		t.Fatalf("PlanScopedCommit: %v", err)
	}
	executionWrite := findWrite(t, plan.Writes, tableExecutions, "exec-terminal")
	if executionWrite.Values["state"] != string(kernel.ExecutionFailed) {
		t.Fatalf("execution state = %v", executionWrite.Values["state"])
	}
	if executionWrite.Values["failure"] == nil {
		t.Fatalf("terminal commit wrote no failure: %+v", executionWrite.Values)
	}
	actionWrite := findWrite(t, plan.Writes, tableActions, string(scenario.commit.ActionID))
	if actionWrite.Values["state"] != string(kernel.ActionFailed) {
		t.Fatalf("action state = %v", actionWrite.Values["state"])
	}
	cleanupWrite := findWrite(t, plan.Writes, tableCleanup, cleanupID("exec-terminal"))
	if cleanupWrite.Values["state"] != string(kernel.CleanupPending) {
		t.Fatalf("cleanup state = %v", cleanupWrite.Values["state"])
	}
}

// TestPlanScopedCommitRefusesForeignPrimaryAction proves the scoped read never
// leaks another scope's action: a commit naming a foreign action is the same
// stale-target version_conflict the write guard reports, and no plan is
// returned.
func TestPlanScopedCommitRefusesForeignPrimaryAction(t *testing.T) {
	adapter, backend := newStore(t)
	seedPipeline(t, backend, projectScope(), 3)
	seedPipeline(t, backend, otherScope(), 3)
	startedExecution(t, adapter, backend, projectScope(), "exec-1", containerAction())
	foreign := startedExecution(t, adapter, backend, otherScope(), "exec-2", containerAction())

	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 foreign[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	source := scopedSource{executor: adapter.executor, scanner: adapter.scanner, scope: projectScope()}
	plan, err := PlanScopedCommit(context.Background(), source.read, commit, nil, nil)
	requireClass(t, err, fault.VersionConflict)
	if len(plan.Locks) != 0 || len(plan.Writes) != 0 {
		t.Fatalf("refused plan carried rows: %+v", plan)
	}
}

// TestPlanScopedCommitRefusesDeadlineWithoutPrimaryAction proves the exported
// mapper preserves the invalid_input shape guard: a deadline guard cannot be
// applied to an execution-only commit.
func TestPlanScopedCommitRefusesDeadlineWithoutPrimaryAction(t *testing.T) {
	plan, err := PlanScopedCommit(context.Background(), func(context.Context, string, string) (store.Row, error) {
		t.Fatal("the shape guard must refuse before any read")
		return store.Row{}, nil
	}, executionOnlyCommit(), nil, &store.DeadlineGuard{Column: "deadline"})
	requireClass(t, err, fault.InvalidInput)
	if len(plan.Locks) != 0 || len(plan.Writes) != 0 {
		t.Fatalf("refused plan carried rows: %+v", plan)
	}
}

// TestStoreLoadSatisfiesExecutionAggregateReader proves the exported scoped
// loader has exactly the port shape and that a foreign-scope, unknown or
// missing execution is uniformly not_found, never forbidden.
func TestStoreLoadSatisfiesExecutionAggregateReader(t *testing.T) {
	var _ task.ExecutionAggregateReader = (*Store)(nil)
	adapter, backend := newStore(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-1", containerAction())

	aggregate, err := adapter.Load(context.Background(), scope, "exec-1")
	if err != nil {
		t.Fatalf("in-scope Load: %v", err)
	}
	if aggregate.ID != "exec-1" || aggregate.State != kernel.ExecutionQueued || len(aggregate.Actions) != 1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}

	cases := map[string]struct {
		scope tenancy.Scope
		id    id.ExecutionID
	}{
		"foreign project": {scope: otherScope(), id: "exec-1"},
		"foreign tenant":  {scope: tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-1"}, id: "exec-1"},
		"unknown":         {scope: projectScope(), id: "exec-missing"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.Load(context.Background(), testCase.scope, testCase.id); err == nil {
				t.Fatal("Load succeeded; want not_found")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
				t.Fatalf("class = %v; want not_found", err)
			}
		})
	}
}
