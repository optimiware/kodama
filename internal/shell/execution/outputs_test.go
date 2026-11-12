package execution_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestStageOutputsBuildsCompleteTransition(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	actor := executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID)
	content := receiptContent(`{"candidate":"abc"}`)
	outputs := kernel.Outputs{"candidate": "abc"}

	result, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  outputs,
		CallerKey:                "outputs-key-1",
		Content:                  content,
	})
	requireNoError(t, err, "StageOutputs")
	if result.NextAction != kernel.ActionRunning {
		t.Fatalf("next action = %q; want running", result.NextAction)
	}
	unit := fixture.transitions.captured()[0]
	if unit.Owner != scope || unit.ExecutionID != aggregate.ID {
		t.Fatalf("unit scope/execution = %+v/%q", unit.Owner, unit.ExecutionID)
	}
	if unit.ExpectedExecutionVersion != aggregate.Version || unit.ExpectedActionVersion != aggregate.Actions[0].Version {
		t.Fatalf("unit versions = %d/%d; want %d/%d",
			unit.ExpectedExecutionVersion, unit.ExpectedActionVersion, aggregate.Version, aggregate.Actions[0].Version)
	}
	if unit.Commit.Output.Action != flow.OutputStage {
		t.Fatalf("output action = %q; want stage", unit.Commit.Output.Action)
	}
	if unit.Commit.Output.Values["candidate"] != "abc" {
		t.Fatalf("staged values = %+v; want candidate=abc", unit.Commit.Output.Values)
	}
	if unit.Commit.InvalidateCredential {
		t.Fatal("staging outputs revoked the execution credential")
	}
	if unit.Commit.Answer != nil || unit.Commit.Subject != nil {
		t.Fatal("output staging carried decision or subject metadata")
	}

	requireReceipt(t, unit.Receipt, scope, actor.ID, "execution.action.outputs", "outputs-key-1", content, testNow)
	requireAudit(t, unit.Audit, scope, actor.ID, "execution.action.outputs", testNow)
	requireEvent(t, unit.Events, event.TypeActionTransition, scope, string(aggregate.ID), actor.ID, testNow)
	requireAuthority(t, unit.Authority, "execution.action.outputs", authorize.CheckExecutionRunning)
	requireAuthority(t, unit.Authority, "execution.action.outputs", authorize.CheckActionDeadline)
	requireAuthority(t, unit.Authority, "execution.action.outputs", authorize.CheckExecutionVersion)
	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)

	stored, _ := fixture.store.Execution(aggregate.ID)
	if !stored.Actions[0].StagedOutputsSet || stored.Actions[0].StagedOutputs["candidate"] != "abc" {
		t.Fatalf("stored staging = %+v; want candidate=abc", stored.Actions[0].StagedOutputs)
	}
	if len(stored.Actions[0].Outputs) != 0 {
		t.Fatalf("staged outputs were promoted before a confirmed success: %+v", stored.Actions[0].Outputs)
	}
}

func TestStageOutputsEnforcesDeclaredBounds(t *testing.T) {
	nineKeys := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
	nineOutputs := make(kernel.Outputs, len(nineKeys))
	for _, key := range nineKeys {
		nineOutputs[id.OutputKey(key)] = "v"
	}

	cases := []struct {
		name    string
		action  kernel.Action
		outputs kernel.Outputs
	}{
		{
			name:    "more than eight keys",
			action:  runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, nineKeys...)).Actions[0],
			outputs: nineOutputs,
		},
		{
			name:    "value over 4096 bytes",
			action:  runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")).Actions[0],
			outputs: kernel.Outputs{"candidate": strings.Repeat("a", kernel.MaxOutputValueBytes+1)},
		},
		{
			name:    "invalid utf8",
			action:  runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")).Actions[0],
			outputs: kernel.Outputs{"candidate": string([]byte{0xff, 0xfe})},
		},
		{
			name:    "undeclared key",
			action:  runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")).Actions[0],
			outputs: kernel.Outputs{"other": "v"},
		},
		{
			name:    "request action",
			action:  runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, "role/reviewer", literalSubject(map[string]string{"operation": "op"}))).Actions[0],
			outputs: kernel.Outputs{},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			scope := projectScope("tenant-1", "project-1")
			aggregate := baseExecution(testCase.action)
			aggregate.State = kernel.ExecutionRunning
			aggregate.CurrentAction = 0
			aggregate.Actions[0].State = kernel.ActionRunning
			aggregate.Actions[0].Version = 2
			aggregate.Actions[0].ActivatedAt = testNow
			aggregate.Actions[0].Deadline = testNow.Add(aggregate.Actions[0].Timeout)
			if aggregate.Actions[0].Kind == definition.ActionKindContainer {
				aggregate.Actions[0].Container.WorkloadID = id.WorkloadID("workload-1")
				aggregate.Actions[0].Container.WorkspaceID = aggregate.WorkspaceID
			}
			fixture.store.Seed(scope, aggregate)
			_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
				Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
				Scope:                    scope,
				ExecutionID:              aggregate.ID,
				ActionID:                 aggregate.Actions[0].ID,
				ExpectedExecutionVersion: aggregate.Version,
				Outputs:                  testCase.outputs,
				CallerKey:                "outputs-key-1",
				Content:                  receiptContent(`{}`),
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if fixture.transitions.callCount() != 0 {
				t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
			}
		})
	}
}

func TestStageOutputsRefusesNonCurrentAction(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour, "result"),
	)
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"result": "v"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestStageOutputsRefusesStaleVersion(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version + 1,
		Outputs:                  kernel.Outputs{"candidate": "v"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestStageOutputsRefusesCrossScopeExecution(t *testing.T) {
	fixture := newTestService(t)
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(projectScope("tenant-1", "project-1"), aggregate)
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-2", "project-9", "principal-9", aggregate.ID),
		Scope:                    projectScope("tenant-2", "project-9"),
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "v"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.NotFound)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestStageOutputsSettlementFirstRefusesStaging(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)

	current, err := fixture.store.Load(context.Background(), scope, aggregate.ID)
	requireNoError(t, err, "load before settlement")
	next, commit, err := flow.Reduce(current, flow.ObserveFact{
		ExecutionID: aggregate.ID,
		ActionID:    aggregate.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          testNow.Add(time.Minute),
	})
	requireNoError(t, err, "settlement reduce")
	if commit == nil {
		t.Fatal("settlement produced no commit")
	}
	if _, err := fixture.store.Commit(context.Background(), *commit); err != nil {
		t.Fatalf("settlement commit: %v", err)
	}
	if next.State != kernel.ExecutionSucceeded {
		t.Fatalf("settled state = %q; want succeeded", next.State)
	}

	_, err = fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: next.Version,
		Outputs:                  kernel.Outputs{"candidate": "late"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{"candidate":"late"}`),
	})
	requireFaultClassIn(t, err, fault.ChangedIdempotencyContent, fault.InvalidInput)
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.Actions[0].StagedOutputsSet {
		t.Fatal("late staging was accepted after settlement")
	}
	if stored.Actions[0].Outputs["candidate"] != "" {
		t.Fatalf("late staging rewrote settled outputs: %+v", stored.Actions[0].Outputs)
	}
}

func TestStageOutputsStagingFirstIsPromotedBySettlement(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	if _, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "abc"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{"candidate":"abc"}`),
	}); err != nil {
		t.Fatalf("StageOutputs: %v", err)
	}

	staged, _ := fixture.store.Execution(aggregate.ID)
	_, commit, err := flow.Reduce(staged, flow.ObserveFact{
		ExecutionID: aggregate.ID,
		ActionID:    aggregate.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          testNow.Add(time.Minute),
	})
	requireNoError(t, err, "settlement reduce")
	if commit == nil || commit.Output.Action != flow.OutputPromote {
		t.Fatalf("settlement output action = %+v; want promote", commit)
	}
	if _, err := fixture.store.Commit(context.Background(), *commit); err != nil {
		t.Fatalf("settlement commit: %v", err)
	}
	settled, _ := fixture.store.Execution(aggregate.ID)
	if settled.State != kernel.ExecutionSucceeded {
		t.Fatalf("state = %q; want succeeded", settled.State)
	}
	if settled.Actions[0].Outputs["candidate"] != "abc" {
		t.Fatalf("settled outputs = %+v; want candidate=abc", settled.Actions[0].Outputs)
	}
}

func TestStageOutputsAndSettlementRaceHaveOneWinner(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var firstCommit atomic.Bool
	fixture.store.setBeforeCommit(func() {
		// Only the first committer blocks. Later committers (the settlement)
		// must pass through so the race has one observable winner instead of
		// deadlocking on a shared one-shot lock.
		if !firstCommit.CompareAndSwap(false, true) {
			return
		}
		close(blocked)
		<-release
	})

	result := make(chan error, 1)
	go func() {
		_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
			Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
			Scope:                    scope,
			ExecutionID:              aggregate.ID,
			ActionID:                 aggregate.Actions[0].ID,
			ExpectedExecutionVersion: aggregate.Version,
			Outputs:                  kernel.Outputs{"candidate": "staged"},
			CallerKey:                "outputs-key-1",
			Content:                  receiptContent(`{"candidate":"staged"}`),
		})
		result <- err
	}()
	<-blocked

	current, err := fixture.store.Load(context.Background(), scope, aggregate.ID)
	requireNoError(t, err, "load during race")
	_, commit, err := flow.Reduce(current, flow.ObserveFact{
		ExecutionID: aggregate.ID,
		ActionID:    aggregate.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          testNow.Add(time.Minute),
	})
	requireNoError(t, err, "settlement reduce during race")
	if commit == nil {
		t.Fatal("settlement produced no commit during race")
	}
	if _, err := fixture.store.Commit(context.Background(), *commit); err != nil {
		t.Fatalf("settlement commit during race: %v", err)
	}
	close(release)

	requireFaultClass(t, <-result, fault.VersionConflict)
	settled, _ := fixture.store.Execution(aggregate.ID)
	if settled.State != kernel.ExecutionSucceeded {
		t.Fatalf("winner state = %q; want succeeded settlement", settled.State)
	}
	if settled.Actions[0].StagedOutputsSet {
		t.Fatal("losing staging was still persisted")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want only the settlement", fixture.store.commitCount())
	}
}

func TestStageOutputsCrashBeforeEffectThenRetrySucceedsOnce(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	fixture.store.failNextBefore(fault.New(fault.DependencyUnavailable, "commit store unavailable"))
	command := execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "abc"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{"candidate":"abc"}`),
	}
	if _, err := fixture.service.StageOutputs(context.Background(), command); err == nil {
		t.Fatal("staging unexpectedly succeeded across a storage failure")
	}
	before, _ := fixture.store.Execution(aggregate.ID)
	if before.Actions[0].StagedOutputsSet {
		t.Fatal("staging was persisted before the failed commit")
	}
	if fixture.store.commitCount() != 0 {
		t.Fatalf("commits = %d; want zero", fixture.store.commitCount())
	}
	if _, err := fixture.service.StageOutputs(context.Background(), command); err != nil {
		t.Fatalf("retried StageOutputs: %v", err)
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after retry = %d; want one", fixture.store.commitCount())
	}
}

func TestStageOutputsLostResponseConvergesWithoutSecondCommit(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	fixture.store.failNextAfter(fault.New(fault.DependencyUnavailable, "response lost after commit"))
	command := execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "abc"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{"candidate":"abc"}`),
	}
	if _, err := fixture.service.StageOutputs(context.Background(), command); err == nil {
		t.Fatal("staging unexpectedly returned a response across a lost commit response")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after lost response = %d; want one", fixture.store.commitCount())
	}
	result, err := fixture.service.StageOutputs(context.Background(), command)
	requireNoError(t, err, "retried StageOutputs")
	if !result.Replayed {
		t.Fatal("identical output retry did not report a replay")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after retry = %d; want one", fixture.store.commitCount())
	}
	command.Outputs = kernel.Outputs{"candidate": "changed"}
	command.Content = receiptContent(`{"candidate":"changed"}`)
	_, err = fixture.service.StageOutputs(context.Background(), command)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
}

func TestStageOutputsCannotSettleDecisionMetadata(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "decision", "actor", "authority"))
	fixture.store.Seed(scope, aggregate)
	outputs := kernel.Outputs{
		"decision":  "pass",
		"actor":     "attacker",
		"authority": "admin",
	}
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  outputs,
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{"decision":"pass","actor":"attacker","authority":"admin"}`),
	})
	requireNoError(t, err, "StageOutputs")
	unit := fixture.transitions.captured()[0]
	if unit.Commit.Answer != nil {
		t.Fatal("workload-controlled output strings created an authorized answer")
	}
	if unit.Commit.Subject != nil {
		t.Fatal("workload-controlled output strings created a request subject")
	}
	if unit.Commit.InvalidateCredential {
		t.Fatal("workload-controlled output strings changed authority")
	}
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.Actions[0].Outputs["decision"] != "" {
		t.Fatal("staged values were promoted before settlement")
	}
	if stored.Actions[0].StagedOutputs["decision"] != "pass" {
		t.Fatalf("staged values = %+v; want the plain output string only", stored.Actions[0].StagedOutputs)
	}
	if stored.Actions[0].Request != nil {
		t.Fatal("container staging created request decision metadata")
	}
}

func TestStageOutputsRequiresOwnOutputsAuthority(t *testing.T) {
	fixture := newTestService(t)
	fixture.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    serviceActor("tenant-1", "project-1", "service-1"),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "v"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestStageOutputsBindsExecutionPrincipalToItsOwnExecution(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    executionActor("tenant-1", "project-1", "principal-other", id.ExecutionID("exec-other")),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": "v"},
		CallerKey:                "outputs-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
	if fixture.store.commitCount() != 0 {
		t.Fatalf("commits = %d; want zero", fixture.store.commitCount())
	}
}

func TestStageOutputsRefusesServiceAndHumanPrincipals(t *testing.T) {
	cases := map[string]authorize.Principal{
		"service": serviceActor("tenant-1", "project-1", "service-1"),
		"human":   humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
	}
	for name, actor := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newTestService(t)
			scope := projectScope("tenant-1", "project-1")
			aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
			fixture.store.Seed(scope, aggregate)
			_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
				Actor:                    actor,
				Scope:                    scope,
				ExecutionID:              aggregate.ID,
				ActionID:                 aggregate.Actions[0].ID,
				ExpectedExecutionVersion: aggregate.Version,
				Outputs:                  kernel.Outputs{"candidate": "v"},
				CallerKey:                "outputs-key-1",
				Content:                  receiptContent(`{}`),
			})
			requireFaultClass(t, err, fault.Forbidden)
			if fixture.transitions.callCount() != 0 {
				t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
			}
			if fixture.store.commitCount() != 0 {
				t.Fatalf("commits = %d; want zero", fixture.store.commitCount())
			}
		})
	}
}
