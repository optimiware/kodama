package flow_test

// These tests pin the pure lifecycle reducer. The reducer is the only place
// where the flow facts become durable state changes:
//
//	func Reduce(execution kernel.Execution, fact flow.Fact) (kernel.Execution, *flow.Commit, error)
//
// Semantics the tests rely on:
//   - a nil commit with a nil error is a converged no-op (duplicate fact or
//     already-recorded evidence) and returns the input unchanged;
//   - a non-nil error is a refusal and returns the input unchanged with no
//     commit;
//   - a non-nil commit is one atomic compare-and-commit payload and the
//     returned execution is the post-commit projection of that payload.
//
// Time is always an explicit fact field. No test starts a process, reads a
// wall clock, or performs any I/O.

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func containerAction(actionID, name string, index int, timeout time.Duration, outputs ...string) kernel.Action {
	declared := make([]id.OutputKey, len(outputs))
	for i, key := range outputs {
		declared[i] = id.OutputKey(key)
	}
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindContainer,
		State:   kernel.ActionPending,
		Version: 1,
		Timeout: timeout,
		Container: &kernel.ContainerAction{
			Definition: definition.Container{
				Image:    "registry.example.com/tool:1",
				Commands: []string{"run"},
				Outputs:  declared,
			},
		},
	}
}

func requestAction(actionID, name string, index int, timeout time.Duration, subject definition.Subject, outputs ...string) kernel.Action {
	declared := make([]id.OutputKey, len(outputs))
	for i, key := range outputs {
		declared[i] = id.OutputKey(key)
	}
	return kernel.Action{
		ID:      id.ActionID(actionID),
		Index:   index,
		Name:    id.ActionName(name),
		Kind:    definition.ActionKindRequest,
		State:   kernel.ActionPending,
		Version: 1,
		Timeout: timeout,
		Request: &kernel.RequestAction{
			Definition: definition.Request{
				Assignment: "role/reviewer",
				Subject:    subject,
				Outputs:    declared,
			},
		},
	}
}

func literalSubject(fields map[string]string) definition.Subject {
	values := make(map[string]definition.SubjectValue, len(fields))
	for key, value := range fields {
		values[key] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: value}
	}
	return definition.NewSubject(values)
}

func newExecution(actions ...kernel.Action) kernel.Execution {
	return kernel.Execution{
		ID:            id.ExecutionID("exec-1"),
		Version:       1,
		State:         kernel.ExecutionQueued,
		WorkspaceID:   id.WorkspaceID("ws-1"),
		Actions:       actions,
		CurrentAction: -1,
		Snapshot:      definition.NewSnapshot(definition.SnapshotInput{}),
		Cleanup:       kernel.Cleanup{State: kernel.CleanupNone},
	}
}

func withInputs(exec kernel.Execution, inputs map[string]string) kernel.Execution {
	exec.Snapshot = definition.NewSnapshot(definition.SnapshotInput{Inputs: inputs})
	return exec
}

func runningContainerExecution(actions ...kernel.Action) kernel.Execution {
	exec := newExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = baseTime
	exec.Actions[0].Deadline = baseTime.Add(exec.Actions[0].Timeout)
	if exec.Actions[0].Container != nil {
		exec.Actions[0].Container.WorkloadID = id.WorkloadID("wl-0")
		exec.Actions[0].Container.WorkspaceID = exec.WorkspaceID
		exec.Actions[0].Container.Launch = kernel.LaunchIntent{
			WorkloadID:     id.WorkloadID("wl-0"),
			State:          kernel.LaunchIssued,
			IdempotencyKey: "start-wl-0",
		}
	}
	return exec
}

func runningRequestExecution(actions ...kernel.Action) kernel.Execution {
	exec := newExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 0
	exec.Actions[0].State = kernel.ActionRunning
	exec.Actions[0].Version = 2
	exec.Actions[0].ActivatedAt = baseTime
	exec.Actions[0].Deadline = baseTime.Add(exec.Actions[0].Timeout)
	if exec.Actions[0].Request != nil {
		resolved := literalSubject(map[string]string{"operation": "review"})
		exec.Actions[0].Request.Subject = &resolved
	}
	return exec
}

// ---------------------------------------------------------------------------
// Reducer oracle helpers
// ---------------------------------------------------------------------------

func factTime(fact flow.Fact) time.Time {
	switch f := fact.(type) {
	case flow.ActivateFact:
		return f.At
	case flow.ObserveFact:
		return f.At
	case flow.OutputFact:
		return f.At
	case flow.AnswerFact:
		return f.At
	case flow.CancelFact:
		return f.At
	case flow.ExpireFact:
		return f.At
	case flow.WorkspaceLostFact:
		return f.At
	case flow.RetiredFact:
		return f.At
	default:
		return time.Time{}
	}
}

func findActionIndex(exec kernel.Execution, actionID id.ActionID) int {
	for i, action := range exec.Actions {
		if action.ID == actionID {
			return i
		}
	}
	return -1
}

func cloneOutputs(values kernel.Outputs) kernel.Outputs {
	if values == nil {
		return nil
	}
	out := make(kernel.Outputs, len(values))
	maps.Copy(out, values)
	return out
}

// applyCommit is the independent oracle for the post-commit projection. It
// mirrors the state-port semantics: one atomic payload applies the
// primary action, every pending cancellation, the current index, evidence and
// the output change, and increments every touched version exactly once.
func applyCommit(before kernel.Execution, commit *flow.Commit) kernel.Execution {
	next := before
	next.Version = commit.ExpectedExecutionVersion + 1
	next.State = commit.NextExecutionState
	next.Actions = append([]kernel.Action(nil), before.Actions...)
	if commit.Failure != nil {
		failure := *commit.Failure
		next.Failure = &failure
	}
	if update := commit.CleanupUpdate; update != nil {
		next.Cleanup = kernel.Cleanup{
			State:       update.State,
			WorkloadID:  update.WorkloadID,
			WorkspaceID: update.WorkspaceID,
			Attempts:    update.Attempts,
			LastError:   update.LastError,
		}
	}
	if commit.NextCurrentAction != nil {
		next.CurrentAction = *commit.NextCurrentAction
	}
	if commit.ActionID != "" {
		index := findActionIndex(next, commit.ActionID)
		if index < 0 {
			return next
		}
		action := &next.Actions[index]
		if action.Container != nil {
			container := *action.Container
			action.Container = &container
		}
		if action.Request != nil {
			request := *action.Request
			action.Request = &request
		}
		action.Version = commit.ExpectedActionVersion + 1
		action.State = commit.NextActionState
		if !commit.Deadline.IsZero() {
			action.Deadline = commit.Deadline
		}
		if action.State == kernel.ActionRunning && action.ActivatedAt.IsZero() {
			action.ActivatedAt = commit.At
		}
		switch commit.Output.Action {
		case flow.OutputStage:
			action.StagedOutputs = cloneOutputs(commit.Output.Values)
			action.StagedOutputsSet = true
		case flow.OutputPromote:
			action.Outputs = cloneOutputs(commit.Output.Values)
			action.StagedOutputs = nil
			action.StagedOutputsSet = false
		case flow.OutputDiscard:
			action.StagedOutputs = nil
			action.StagedOutputsSet = false
		}
		if commit.Subject != nil {
			if action.Request == nil {
				action.Request = &kernel.RequestAction{}
			}
			subject := *commit.Subject
			action.Request.Subject = &subject
		}
		if commit.Answer != nil {
			if action.Request == nil {
				action.Request = &kernel.RequestAction{}
			}
			answer := *commit.Answer
			action.Request.Answer = &answer
		}
		if commit.Observation != nil {
			if action.Container == nil {
				action.Container = &kernel.ContainerAction{}
			}
			action.Container.Observation = *commit.Observation
		}
		if commit.StopEvidence != nil {
			action.Stop = *commit.StopEvidence
		}
		if commit.Launch != nil {
			if action.Container == nil {
				action.Container = &kernel.ContainerAction{}
			}
			action.Container.Launch = *commit.Launch
			action.Container.WorkloadID = commit.Launch.WorkloadID
			if action.Container.WorkspaceID == "" {
				action.Container.WorkspaceID = next.WorkspaceID
			}
		}
	}
	for _, update := range commit.PendingCancellations {
		index := findActionIndex(next, update.ActionID)
		if index < 0 {
			continue
		}
		next.Actions[index].Version = update.ExpectedVersion + 1
		next.Actions[index].State = update.NextState
	}
	return next
}

func mustReduce(t *testing.T, exec kernel.Execution, fact flow.Fact) (kernel.Execution, *flow.Commit) {
	t.Helper()
	before := fingerprint(exec)
	next, commit, err := flow.Reduce(exec, fact)
	if err != nil {
		t.Fatalf("Reduce(%T) error = %v, want nil", fact, err)
	}
	if commit == nil {
		t.Fatalf("Reduce(%T) returned a nil commit, want one transition", fact)
	}
	if got := fingerprint(exec); got != before {
		t.Fatalf("Reduce(%T) mutated its input\nbefore:\n%s\nafter:\n%s", fact, before, got)
	}
	if commit.ExecutionID != exec.ID {
		t.Fatalf("commit execution id = %q, want %q", commit.ExecutionID, exec.ID)
	}
	if commit.ExpectedExecutionVersion != exec.Version {
		t.Fatalf("commit expected execution version = %d, want %d", commit.ExpectedExecutionVersion, exec.Version)
	}
	if want := factTime(fact); !commit.At.Equal(want) {
		t.Fatalf("commit.At = %v, want authoritative fact time %v", commit.At, want)
	}
	want := applyCommit(exec, commit)
	if !reflect.DeepEqual(next, want) {
		t.Fatalf("Reduce(%T) next state diverges from its own commit\n got: %#v\nwant: %#v", fact, next, want)
	}
	return next, commit
}

func mustNoop(t *testing.T, exec kernel.Execution, fact flow.Fact) kernel.Execution {
	t.Helper()
	before := fingerprint(exec)
	next, commit, err := flow.Reduce(exec, fact)
	if err != nil {
		t.Fatalf("Reduce(%T) error = %v, want a converged no-op", fact, err)
	}
	if commit != nil {
		t.Fatalf("Reduce(%T) commit = %#v, want a nil no-op commit", fact, commit)
	}
	if got := fingerprint(next); got != before {
		t.Fatalf("Reduce(%T) no-op changed state\nbefore:\n%s\nafter:\n%s", fact, before, got)
	}
	return next
}

func mustRefuse(t *testing.T, exec kernel.Execution, fact flow.Fact, class fault.Class) error {
	t.Helper()
	before := fingerprint(exec)
	next, commit, err := flow.Reduce(exec, fact)
	if err == nil {
		t.Fatalf("Reduce(%T) error = nil, want refusal", fact)
	}
	if commit != nil {
		t.Fatalf("Reduce(%T) commit = %#v, want nil on refusal", fact, commit)
	}
	if got := fingerprint(next); got != before {
		t.Fatalf("Reduce(%T) refusal changed state\nbefore:\n%s\nafter:\n%s", fact, before, got)
	}
	if class != "" {
		got, ok := fault.ClassOf(err)
		if !ok || got != class {
			t.Fatalf("Reduce(%T) refusal class = (%q, %v), want %q (err: %v)", fact, got, ok, class, err)
		}
	}
	return err
}

// fingerprint is a deterministic rendering of every field the reducer may
// change. It is used for no-op, refusal and terminal-immutability assertions
// without aliasing the maps, slices or pointers inside kernel.Execution.
func fingerprint(exec kernel.Execution) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exec=%s v=%d state=%s current=%d ws=%s\n", exec.ID, exec.Version, exec.State, exec.CurrentAction, exec.WorkspaceID)
	for _, action := range exec.Actions {
		fmt.Fprintf(&b, " action=%s idx=%d name=%s kind=%s state=%s v=%d timeout=%s activated=%s deadline=%s outputs=%v staged=%v stagedSet=%t stop=%v\n",
			action.ID, action.Index, action.Name, action.Kind, action.State, action.Version,
			action.Timeout, action.ActivatedAt, action.Deadline, action.Outputs,
			action.StagedOutputs, action.StagedOutputsSet, action.Stop)
		if action.Container != nil {
			code, hasCode := action.Container.Observation.ExitStatus()
			fmt.Fprintf(&b, "  container wl=%s ws=%s launch=%+v observation=%s proof=%s exit=(%d,%v) stop=%v\n",
				action.Container.WorkloadID, action.Container.WorkspaceID, action.Container.Launch,
				action.Container.Observation.Kind, action.Container.Observation.Proof,
				code, hasCode, action.Container.Observation.Stop)
		}
		if action.Request != nil {
			subject := "<nil>"
			if action.Request.Subject != nil {
				subject = fmt.Sprintf("%v", action.Request.Subject.Fields())
			}
			fmt.Fprintf(&b, "  request subject=%s answer=%+v\n", subject, action.Request.Answer)
		}
	}
	if exec.Failure == nil {
		b.WriteString(" failure=<nil>")
	} else {
		fmt.Fprintf(&b, " failure=%s/%s/%s", exec.Failure.Kind, exec.Failure.Reason, exec.Failure.Detail)
	}
	fmt.Fprintf(&b, " cleanup=%+v", exec.Cleanup)
	return b.String()
}

func assertSingleActiveAction(t *testing.T, exec kernel.Execution) {
	t.Helper()
	running := -1
	for i, action := range exec.Actions {
		if action.State != kernel.ActionRunning {
			continue
		}
		if running != -1 {
			t.Fatalf("execution has multiple running actions: %d and %d", running, i)
		}
		running = i
	}
	if exec.CurrentAction != running {
		t.Fatalf("CurrentAction = %d, running action index = %d", exec.CurrentAction, running)
	}
	if exec.State == kernel.ExecutionSucceeded {
		for i, action := range exec.Actions {
			if action.State != kernel.ActionSucceeded {
				t.Fatalf("succeeded execution action %d state = %q", i, action.State)
			}
		}
	}
	if exec.State == kernel.ExecutionFailed || exec.State == kernel.ExecutionCancelled {
		if running != -1 {
			t.Fatalf("terminal execution still has running action %d", running)
		}
	}
}

func assertPrimaryAndPendingSettlement(t *testing.T, before kernel.Execution, commit *flow.Commit, primaryState, pendingState kernel.ActionState) {
	t.Helper()
	if before.CurrentAction < 0 || before.CurrentAction >= len(before.Actions) {
		t.Fatalf("execution has no current action to settle")
	}
	current := before.Actions[before.CurrentAction]
	if commit.ActionID != current.ID {
		t.Fatalf("commit primary action = %q, want %q", commit.ActionID, current.ID)
	}
	if commit.ExpectedActionVersion != current.Version {
		t.Fatalf("commit primary expected version = %d, want %d", commit.ExpectedActionVersion, current.Version)
	}
	if commit.NextActionState != primaryState {
		t.Fatalf("commit primary next state = %q, want %q", commit.NextActionState, primaryState)
	}
	want := make(map[id.ActionID]uint64)
	for _, action := range before.Actions {
		if action.State == kernel.ActionPending {
			want[action.ID] = action.Version
		}
	}
	if len(commit.PendingCancellations) != len(want) {
		t.Fatalf("pending cancellations = %d, want %d (%+v)", len(commit.PendingCancellations), len(want), commit.PendingCancellations)
	}
	for _, update := range commit.PendingCancellations {
		version, ok := want[update.ActionID]
		if !ok {
			t.Fatalf("unexpected pending cancellation for %q", update.ActionID)
		}
		if update.ExpectedVersion != version {
			t.Fatalf("pending cancellation %q expected version = %d, want %d", update.ActionID, update.ExpectedVersion, version)
		}
		if update.NextState != pendingState {
			t.Fatalf("pending cancellation %q next state = %q, want %q", update.ActionID, update.NextState, pendingState)
		}
		delete(want, update.ActionID)
	}
	if len(want) != 0 {
		t.Fatalf("pending actions were not cancelled: %v", want)
	}
}

func assertAllActionsSettled(t *testing.T, before kernel.Execution, commit *flow.Commit, state kernel.ActionState) {
	t.Helper()
	covered := make(map[id.ActionID]bool)
	if commit.ActionID != "" {
		index := findActionIndex(before, commit.ActionID)
		if index < 0 {
			t.Fatalf("commit primary action %q does not exist", commit.ActionID)
		}
		if commit.ExpectedActionVersion != before.Actions[index].Version {
			t.Fatalf("commit primary expected version = %d, want %d", commit.ExpectedActionVersion, before.Actions[index].Version)
		}
		if commit.NextActionState != state {
			t.Fatalf("commit primary next state = %q, want %q", commit.NextActionState, state)
		}
		covered[commit.ActionID] = true
	}
	for _, update := range commit.PendingCancellations {
		if covered[update.ActionID] {
			t.Fatalf("action %q settled twice in one commit", update.ActionID)
		}
		index := findActionIndex(before, update.ActionID)
		if index < 0 {
			t.Fatalf("pending cancellation action %q does not exist", update.ActionID)
		}
		if update.ExpectedVersion != before.Actions[index].Version {
			t.Fatalf("pending cancellation %q expected version = %d, want %d", update.ActionID, update.ExpectedVersion, before.Actions[index].Version)
		}
		if update.NextState != state {
			t.Fatalf("pending cancellation %q next state = %q, want %q", update.ActionID, update.NextState, state)
		}
		covered[update.ActionID] = true
	}
	for _, action := range before.Actions {
		if action.State != kernel.ActionPending && action.State != kernel.ActionRunning {
			continue
		}
		if !covered[action.ID] {
			t.Fatalf("action %q was not settled by the commit", action.ID)
		}
	}
}

func assertNoPending(t *testing.T, exec kernel.Execution) {
	t.Helper()
	for _, action := range exec.Actions {
		if action.State == kernel.ActionPending {
			t.Fatalf("action %q remains pending", action.ID)
		}
	}
}

func assertFailure(t *testing.T, commit *flow.Commit, kind kernel.FailureKind) {
	t.Helper()
	if commit.Failure == nil {
		t.Fatalf("commit failure = nil, want %q", kind)
	}
	if commit.Failure.Kind != kind {
		t.Fatalf("commit failure kind = %q, want %q", commit.Failure.Kind, kind)
	}
	if commit.Failure.Reason == "" {
		t.Fatal("commit failure reason is empty")
	}
	if commit.Failure.Detail == "" {
		t.Fatal("commit failure detail is empty")
	}
}

func assertCleanup(t *testing.T, exec kernel.Execution, commit *flow.Commit, workloadID id.WorkloadID) {
	t.Helper()
	if commit.Cleanup == nil {
		t.Fatalf("commit cleanup intent = nil, want an attributable cleanup")
	}
	if commit.Cleanup.WorkspaceID != exec.WorkspaceID {
		t.Fatalf("cleanup workspace = %q, want %q", commit.Cleanup.WorkspaceID, exec.WorkspaceID)
	}
	if workloadID != "" && commit.Cleanup.WorkloadID != workloadID {
		t.Fatalf("cleanup workload = %q, want %q", commit.Cleanup.WorkloadID, workloadID)
	}
	if commit.Cleanup.Reason == "" {
		t.Fatal("cleanup reason is empty")
	}
	if commit.CleanupUpdate == nil {
		t.Fatalf("commit cleanup update = nil, want visible pending cleanup")
	}
	if commit.CleanupUpdate.State != kernel.CleanupPending {
		t.Fatalf("cleanup update state = %q, want pending", commit.CleanupUpdate.State)
	}
	if commit.CleanupUpdate.WorkspaceID != exec.WorkspaceID {
		t.Fatalf("cleanup update workspace = %q, want %q", commit.CleanupUpdate.WorkspaceID, exec.WorkspaceID)
	}
	if workloadID != "" && commit.CleanupUpdate.WorkloadID != workloadID {
		t.Fatalf("cleanup update workload = %q, want %q", commit.CleanupUpdate.WorkloadID, workloadID)
	}
}

func assertStopIntent(t *testing.T, commit *flow.Commit, workloadID id.WorkloadID, workspaceID id.WorkspaceID) {
	t.Helper()
	if commit.Stop == nil {
		t.Fatalf("commit stop intent = nil, want termination of %q", workloadID)
	}
	if commit.Stop.WorkloadID != workloadID {
		t.Fatalf("stop workload = %q, want %q", commit.Stop.WorkloadID, workloadID)
	}
	if commit.Stop.WorkspaceID != workspaceID {
		t.Fatalf("stop workspace = %q, want %q", commit.Stop.WorkspaceID, workspaceID)
	}
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

func TestActivateContainerPersistsDeadlineAndLaunchIntent(t *testing.T) {
	// Activation happens a day after admission: queue time must consume none of
	// the timeout. The authoritative time is the explicit fact time.
	at := baseTime.Add(24 * time.Hour)
	timeout := 10 * time.Minute
	exec := newExecution(containerAction("act-0", "build", 0, timeout, "candidate"))
	fact := flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at}

	next, commit := mustReduce(t, exec, fact)
	if next.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want running", next.State)
	}
	if next.CurrentAction != 0 {
		t.Fatalf("CurrentAction = %d, want 0", next.CurrentAction)
	}
	action := next.Actions[0]
	if action.State != kernel.ActionRunning {
		t.Fatalf("action state = %q, want running", action.State)
	}
	if !action.ActivatedAt.Equal(at) {
		t.Fatalf("ActivatedAt = %v, want %v", action.ActivatedAt, at)
	}
	wantDeadline := at.Add(timeout)
	if !action.Deadline.Equal(wantDeadline) {
		t.Fatalf("Deadline = %v, want %v", action.Deadline, wantDeadline)
	}
	if action.Deadline.Location() != time.UTC {
		t.Fatalf("Deadline location = %v, want UTC", action.Deadline.Location())
	}
	if action.Container == nil {
		t.Fatal("container value = nil after container activation")
	}
	if action.Container.WorkloadID == "" {
		t.Fatal("activation did not allocate a stable workload identity")
	}
	if action.Container.WorkspaceID != exec.WorkspaceID {
		t.Fatalf("container workspace = %q, want %q", action.Container.WorkspaceID, exec.WorkspaceID)
	}
	if action.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("launch state = %q, want not_issued", action.Container.Launch.State)
	}
	if action.Container.Launch.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("launch workload = %q, want %q", action.Container.Launch.WorkloadID, action.Container.WorkloadID)
	}
	if action.Container.Launch.IdempotencyKey == "" {
		t.Fatal("activation did not persist an idempotency key")
	}
	if commit.Launch == nil {
		t.Fatal("commit launch intent = nil")
	}
	if *commit.Launch != action.Container.Launch {
		t.Fatalf("commit launch = %+v, want %+v", *commit.Launch, action.Container.Launch)
	}
	if commit.NextActionState != kernel.ActionRunning {
		t.Fatalf("commit next action state = %q, want running", commit.NextActionState)
	}
	if commit.NextCurrentAction == nil || *commit.NextCurrentAction != 0 {
		t.Fatalf("commit next current action = %v, want 0", commit.NextCurrentAction)
	}
	if !commit.Deadline.Equal(wantDeadline) {
		t.Fatalf("commit deadline = %v, want %v", commit.Deadline, wantDeadline)
	}
	if commit.InvalidateCredential {
		t.Fatal("container activation invalidated the execution credential")
	}
	if commit.Subject != nil || commit.Answer != nil || commit.Stop != nil || commit.Cleanup != nil || commit.Failure != nil {
		t.Fatalf("container activation carried unrelated evidence: %+v", commit)
	}
	assertSingleActiveAction(t, next)

	// Identity allocation is deterministic for the same input.
	again, againCommit := mustReduce(t, exec, fact)
	if again.Actions[0].Container.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("repeat activation allocated a different workload id: %q vs %q", again.Actions[0].Container.WorkloadID, action.Container.WorkloadID)
	}
	if againCommit.Launch == nil || againCommit.Launch.IdempotencyKey != action.Container.Launch.IdempotencyKey {
		t.Fatalf("repeat activation allocated a different idempotency key: %+v", againCommit.Launch)
	}
}

func TestActivateRequestResolvesSubjectAndInvalidatesCredential(t *testing.T) {
	at := baseTime
	timeout := time.Hour
	build := containerAction("act-0", "build", 0, timeout, "candidate")
	build.State = kernel.ActionSucceeded
	build.Version = 2
	build.Outputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
	build.Stop = kernel.ConfirmedStop("termination observed")
	build.ActivatedAt = at.Add(-time.Hour)
	build.Deadline = at

	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"resource":  {Kind: definition.SubjectReference, Reference: definition.Reference{Input: true, Key: "repository"}},
		"candidate": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "build", Key: "candidate"}},
	})
	review := requestAction("act-1", "review", 1, timeout, subject, "decision")
	exec := newExecution(build, review)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = -1
	exec.Version = 4
	exec = withInputs(exec, map[string]string{"repository": "example/repo"})

	next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at})
	if next.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want running", next.State)
	}
	if next.CurrentAction != 1 {
		t.Fatalf("CurrentAction = %d, want 1", next.CurrentAction)
	}
	action := next.Actions[1]
	if action.State != kernel.ActionRunning {
		t.Fatalf("request state = %q, want running", action.State)
	}
	if action.Request == nil || action.Request.Subject == nil {
		t.Fatal("request activation did not persist a resolved subject")
	}
	fields := action.Request.Subject.Fields()
	want := map[string]string{
		"operation": "git.ref.update",
		"resource":  "example/repo",
		"candidate": "abc",
	}
	for key, value := range want {
		got, ok := fields[key]
		if !ok || got.Kind != definition.SubjectLiteral || got.Literal != value {
			t.Fatalf("resolved subject %q = %+v, want literal %q", key, got, value)
		}
	}
	if commit.Subject == nil {
		t.Fatal("commit subject = nil, want the immutable resolved subject")
	}
	if !reflect.DeepEqual(commit.Subject.Fields(), fields) {
		t.Fatalf("commit subject = %v, want %v", commit.Subject.Fields(), fields)
	}
	if !commit.InvalidateCredential {
		t.Fatal("request activation did not invalidate the execution credential")
	}
	if commit.Launch != nil {
		t.Fatalf("request activation carried a container launch intent: %+v", commit.Launch)
	}
	if !commit.Deadline.Equal(at.Add(timeout)) {
		t.Fatalf("request deadline = %v, want %v", commit.Deadline, at.Add(timeout))
	}
	if commit.NextActionState != kernel.ActionRunning {
		t.Fatalf("commit next action state = %q, want running", commit.NextActionState)
	}
	if commit.NextCurrentAction == nil || *commit.NextCurrentAction != 1 {
		t.Fatalf("commit next current action = %v, want 1", commit.NextCurrentAction)
	}
	assertSingleActiveAction(t, next)
}

func assertActivationFailure(t *testing.T, before, next kernel.Execution, commit *flow.Commit, failedIndex int) {
	t.Helper()
	if next.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[failedIndex].State != kernel.ActionFailed {
		t.Fatalf("failed action %d state = %q, want failed", failedIndex, next.Actions[failedIndex].State)
	}
	if next.Actions[failedIndex].Request == nil || next.Actions[failedIndex].Request.Subject != nil {
		t.Fatalf("activation failure exposed a request subject: %+v", next.Actions[failedIndex].Request)
	}
	if commit.Subject != nil {
		t.Fatalf("commit exposed a partial subject: %v", commit.Subject.Fields())
	}
	if commit.Answer != nil {
		t.Fatalf("commit carried an answer for an unanswerable request: %+v", commit.Answer)
	}
	assertFailure(t, commit, kernel.FailurePreparation)
	if commit.NextExecutionState != kernel.ExecutionFailed {
		t.Fatalf("commit next execution state = %q, want failed", commit.NextExecutionState)
	}
	if commit.NextCurrentAction == nil || *commit.NextCurrentAction != -1 {
		t.Fatalf("commit next current action = %v, want -1", commit.NextCurrentAction)
	}
	if commit.ActionID != before.Actions[failedIndex].ID {
		t.Fatalf("commit primary action = %q, want %q", commit.ActionID, before.Actions[failedIndex].ID)
	}
	if commit.ExpectedActionVersion != before.Actions[failedIndex].Version {
		t.Fatalf("commit primary expected version = %d, want %d", commit.ExpectedActionVersion, before.Actions[failedIndex].Version)
	}
	if commit.NextActionState != kernel.ActionFailed {
		t.Fatalf("commit primary next state = %q, want failed", commit.NextActionState)
	}
	want := make(map[id.ActionID]uint64)
	for i, action := range before.Actions {
		if i == failedIndex {
			continue
		}
		if action.State == kernel.ActionPending {
			want[action.ID] = action.Version
		}
	}
	if len(commit.PendingCancellations) != len(want) {
		t.Fatalf("pending cancellations = %d, want %d", len(commit.PendingCancellations), len(want))
	}
	for _, update := range commit.PendingCancellations {
		version, ok := want[update.ActionID]
		if !ok {
			t.Fatalf("unexpected pending cancellation for %q", update.ActionID)
		}
		if update.ExpectedVersion != version {
			t.Fatalf("pending cancellation %q expected version = %d, want %d", update.ActionID, update.ExpectedVersion, version)
		}
		if update.NextState != kernel.ActionCancelled {
			t.Fatalf("pending cancellation %q next state = %q, want cancelled", update.ActionID, update.NextState)
		}
		delete(want, update.ActionID)
	}
	if len(want) != 0 {
		t.Fatalf("pending actions were not cancelled: %v", want)
	}
	assertNoPending(t, next)
	assertSingleActiveAction(t, next)
}

func TestRequestActivationFailsClosedOnMissingSubject(t *testing.T) {
	at := baseTime
	timeout := time.Hour

	t.Run("missing input reference", func(t *testing.T) {
		subject := definition.NewSubject(map[string]definition.SubjectValue{
			"resource": {Kind: definition.SubjectReference, Reference: definition.Reference{Input: true, Key: "repository"}},
		})
		request := requestAction("act-0", "review", 0, timeout, subject, "decision")
		successor := containerAction("act-1", "test", 1, timeout)
		exec := newExecution(request, successor)

		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})
		assertActivationFailure(t, exec, next, commit, 0)
	})

	t.Run("missing earlier output", func(t *testing.T) {
		build := containerAction("act-0", "build", 0, timeout, "candidate")
		build.State = kernel.ActionSucceeded
		build.Version = 2
		build.Stop = kernel.ConfirmedStop("termination observed")
		build.ActivatedAt = at.Add(-timeout)
		build.Deadline = at
		subject := definition.NewSubject(map[string]definition.SubjectValue{
			"candidate": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "build", Key: "candidate"}},
		})
		review := requestAction("act-1", "review", 1, timeout, subject, "decision")
		exec := newExecution(build, review)
		exec.State = kernel.ExecutionRunning
		exec.CurrentAction = -1

		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at})
		assertActivationFailure(t, exec, next, commit, 1)
		if next.Actions[0].State != kernel.ActionSucceeded {
			t.Fatalf("earlier succeeded action state = %q, want succeeded", next.Actions[0].State)
		}
	})

	t.Run("forward reference is not visible", func(t *testing.T) {
		subject := definition.NewSubject(map[string]definition.SubjectValue{
			"candidate": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "test", Key: "out"}},
		})
		request := requestAction("act-0", "review", 0, timeout, subject, "decision")
		successor := containerAction("act-1", "test", 1, timeout, "out")
		exec := newExecution(request, successor)

		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})
		assertActivationFailure(t, exec, next, commit, 0)
	})
}

func TestActivationRefusesOverlapOutOfOrderAndTerminal(t *testing.T) {
	at := baseTime

	running := newExecution(
		containerAction("act-0", "build", 0, time.Hour),
		containerAction("act-1", "test", 1, time.Hour),
	)
	running.State = kernel.ExecutionRunning
	running.CurrentAction = 0
	running.Actions[0].State = kernel.ActionRunning
	running.Actions[0].Version = 2
	mustRefuse(t, running, flow.ActivateFact{ExecutionID: running.ID, ActionID: "act-1", At: at}, fault.InvalidInput)

	queued := newExecution(
		containerAction("act-0", "build", 0, time.Hour),
		containerAction("act-1", "test", 1, time.Hour),
	)
	mustRefuse(t, queued, flow.ActivateFact{ExecutionID: queued.ID, ActionID: "act-1", At: at}, fault.InvalidInput)
	mustRefuse(t, queued, flow.ActivateFact{ExecutionID: queued.ID, ActionID: "missing", At: at}, fault.InvalidInput)

	done := newExecution(containerAction("act-0", "build", 0, time.Hour))
	done.State = kernel.ExecutionSucceeded
	done.Actions[0].State = kernel.ActionSucceeded
	mustRefuse(t, done, flow.ActivateFact{ExecutionID: done.ID, ActionID: "act-0", At: at}, fault.InvalidInput)

	foreign := flow.ActivateFact{ExecutionID: "other-exec", ActionID: "act-0", At: at}
	mustRefuse(t, queued, foreign, fault.InvalidInput)
}

func TestZeroActionExecutionCancelsAndRefusesActivation(t *testing.T) {
	at := baseTime
	exec := newExecution()

	mustRefuse(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at}, fault.InvalidInput)
	mustRefuse(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: kernel.RunningObservation(), At: at}, fault.InvalidInput)

	next, commit := mustReduce(t, exec, flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: at})
	if next.State != kernel.ExecutionCancelled {
		t.Fatalf("execution state = %q, want cancelled", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if len(next.Actions) != 0 {
		t.Fatalf("actions = %d, want 0", len(next.Actions))
	}
	if commit.NextExecutionState != kernel.ExecutionCancelled {
		t.Fatalf("commit next execution state = %q, want cancelled", commit.NextExecutionState)
	}
	if commit.NextCurrentAction == nil || *commit.NextCurrentAction != -1 {
		t.Fatalf("commit next current action = %v, want -1", commit.NextCurrentAction)
	}
	if !commit.InvalidateCredential {
		t.Fatal("cancel did not revoke execution authority")
	}
	if commit.Failure != nil {
		t.Fatalf("cancel carried a failure: %+v", commit.Failure)
	}
	if commit.ActionID != "" {
		t.Fatalf("cancel of a zero-action execution settled action %q", commit.ActionID)
	}
	if len(commit.PendingCancellations) != 0 {
		t.Fatalf("cancel of a zero-action execution carried cancellations: %+v", commit.PendingCancellations)
	}
	assertSingleActiveAction(t, next)
}

// ---------------------------------------------------------------------------
// Ordered success, failure and cancellation
// ---------------------------------------------------------------------------

func TestOnlySuccessAdvancesAndPromotesAfterConfirmedStop(t *testing.T) {
	at := baseTime
	timeout := time.Hour
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, timeout, "candidate"),
		containerAction("act-1", "test", 1, timeout),
	)
	staged := kernel.Outputs{id.OutputKey("candidate"): "abc"}

	next, commit := mustReduce(t, exec, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: staged, At: at.Add(time.Minute)})
	if commit.Output.Action != flow.OutputStage {
		t.Fatalf("output action = %q, want stage", commit.Output.Action)
	}
	if !reflect.DeepEqual(next.Actions[0].StagedOutputs, staged) || !next.Actions[0].StagedOutputsSet {
		t.Fatalf("staged outputs = %v set=%t, want %v set=true", next.Actions[0].StagedOutputs, next.Actions[0].StagedOutputsSet, staged)
	}
	if next.Actions[0].Outputs != nil {
		t.Fatalf("settled outputs before success = %v, want nil", next.Actions[0].Outputs)
	}

	pending := kernel.ExitedObservation(0, kernel.PendingStop("stop barrier pending"))
	next, commit = mustReduce(t, next, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: pending, At: at.Add(2 * time.Minute)})
	if next.State != kernel.ExecutionRunning {
		t.Fatalf("state after unconfirmed stop = %q, want running", next.State)
	}
	if next.CurrentAction != 0 || next.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("unconfirmed stop advanced: current=%d state=%q", next.CurrentAction, next.Actions[0].State)
	}
	if commit.Output.Action == flow.OutputPromote {
		t.Fatal("promoted staged outputs before stop confirmation")
	}
	if next.Actions[0].Outputs != nil {
		t.Fatalf("settled outputs before stop confirmation = %v, want nil", next.Actions[0].Outputs)
	}
	if !next.Actions[0].StagedOutputsSet {
		t.Fatal("unconfirmed stop cleared the staging presence marker")
	}
	if commit.StopEvidence == nil || commit.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v, want unconfirmed", commit.StopEvidence)
	}

	confirmed := kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))
	next, commit = mustReduce(t, next, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: confirmed, At: at.Add(3 * time.Minute)})
	if next.State != kernel.ExecutionRunning {
		t.Fatalf("state after first success = %q, want running", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction after success = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].State != kernel.ActionSucceeded {
		t.Fatalf("action 0 state = %q, want succeeded", next.Actions[0].State)
	}
	if next.Actions[1].State != kernel.ActionPending {
		t.Fatalf("action 1 state = %q, want pending", next.Actions[1].State)
	}
	if commit.Output.Action != flow.OutputPromote {
		t.Fatalf("output action after success = %q, want promote", commit.Output.Action)
	}
	if !reflect.DeepEqual(commit.Output.Values, staged) {
		t.Fatalf("promoted values = %v, want %v", commit.Output.Values, staged)
	}
	if !reflect.DeepEqual(next.Actions[0].Outputs, staged) {
		t.Fatalf("settled outputs = %v, want %v", next.Actions[0].Outputs, staged)
	}
	if next.Actions[0].StagedOutputsSet {
		t.Fatal("staging presence marker was not cleared after promotion")
	}
	if !next.Actions[0].Stop.IsConfirmed() {
		t.Fatalf("action stop = %+v, want confirmed", next.Actions[0].Stop)
	}
	assertSingleActiveAction(t, next)

	next, _ = mustReduce(t, next, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at.Add(4 * time.Minute)})
	if next.CurrentAction != 1 || next.Actions[1].State != kernel.ActionRunning {
		t.Fatalf("next activation current=%d state=%q, want 1/running", next.CurrentAction, next.Actions[1].State)
	}
	assertSingleActiveAction(t, next)

	next, commit = mustReduce(t, next, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-1", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(5 * time.Minute)})
	if next.State != kernel.ExecutionSucceeded {
		t.Fatalf("final state = %q, want succeeded", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("final CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[1].State != kernel.ActionSucceeded {
		t.Fatalf("final action state = %q, want succeeded", next.Actions[1].State)
	}
	if commit.NextExecutionState != kernel.ExecutionSucceeded {
		t.Fatalf("commit next execution state = %q, want succeeded", commit.NextExecutionState)
	}
	assertSingleActiveAction(t, next)
}

func TestFailureCancelsEveryPendingSuccessorAtomically(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		requestAction("act-1", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
		containerAction("act-2", "test", 2, time.Hour),
	)
	exec.Version = 7
	exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
	exec.Actions[0].StagedOutputsSet = true

	next, commit := mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")),
		At:          at.Add(time.Minute),
	})
	if next.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].State != kernel.ActionFailed {
		t.Fatalf("action 0 state = %q, want failed", next.Actions[0].State)
	}
	if next.Actions[1].State != kernel.ActionCancelled || next.Actions[2].State != kernel.ActionCancelled {
		t.Fatalf("pending successors = %q/%q, want cancelled/cancelled", next.Actions[1].State, next.Actions[2].State)
	}
	if next.Actions[0].StagedOutputsSet || next.Actions[0].StagedOutputs != nil {
		t.Fatalf("failed action retained staging: %v set=%t", next.Actions[0].StagedOutputs, next.Actions[0].StagedOutputsSet)
	}
	assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
	assertFailure(t, commit, kernel.FailureRuntime)
	assertNoPending(t, next)
	if commit.Output.Action != flow.OutputDiscard {
		t.Fatalf("output action = %q, want discard", commit.Output.Action)
	}
	if commit.StopEvidence == nil || !commit.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v, want confirmed", commit.StopEvidence)
	}
	assertSingleActiveAction(t, next)
}

func TestCancelAndTimeoutRevokeAuthorityAndStopWorkload(t *testing.T) {
	at := baseTime

	t.Run("cancel running container", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
		exec.Actions[0].StagedOutputsSet = true

		next, commit := mustReduce(t, exec, flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: at.Add(time.Minute)})
		if next.State != kernel.ExecutionCancelled {
			t.Fatalf("execution state = %q, want cancelled", next.State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if next.Actions[0].State != kernel.ActionCancelled || next.Actions[1].State != kernel.ActionCancelled {
			t.Fatalf("actions = %q/%q, want cancelled/cancelled", next.Actions[0].State, next.Actions[1].State)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("cancel retained staged outputs")
		}
		assertAllActionsSettled(t, exec, commit, kernel.ActionCancelled)
		if commit.Failure != nil {
			t.Fatalf("cancel carried a failure: %+v", commit.Failure)
		}
		if commit.Output.Action != flow.OutputDiscard {
			t.Fatalf("output action = %q, want discard", commit.Output.Action)
		}
		if !commit.InvalidateCredential {
			t.Fatal("cancel did not revoke execution authority")
		}
		assertStopIntent(t, commit, id.WorkloadID("wl-0"), exec.WorkspaceID)
		assertCleanup(t, exec, commit, id.WorkloadID("wl-0"))
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})

	t.Run("timeout running container", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
		exec.Actions[0].StagedOutputsSet = true
		deadline := exec.Actions[0].Deadline

		next, commit := mustReduce(t, exec, flow.ExpireFact{ExecutionID: exec.ID, ActionID: "act-0", At: deadline})
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if next.Actions[0].State != kernel.ActionFailed || next.Actions[1].State != kernel.ActionCancelled {
			t.Fatalf("actions = %q/%q, want failed/cancelled", next.Actions[0].State, next.Actions[1].State)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("timeout retained staged outputs")
		}
		assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
		assertFailure(t, commit, kernel.FailureTimeout)
		if !commit.Deadline.Equal(deadline) {
			t.Fatalf("commit deadline = %v, want persisted deadline %v", commit.Deadline, deadline)
		}
		if commit.Output.Action != flow.OutputDiscard {
			t.Fatalf("output action = %q, want discard", commit.Output.Action)
		}
		if !commit.InvalidateCredential {
			t.Fatal("timeout did not revoke execution authority")
		}
		assertStopIntent(t, commit, id.WorkloadID("wl-0"), exec.WorkspaceID)
		assertCleanup(t, exec, commit, id.WorkloadID("wl-0"))
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})

	t.Run("timeout running request", func(t *testing.T) {
		exec := runningRequestExecution(
			requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		deadline := exec.Actions[0].Deadline

		next, commit := mustReduce(t, exec, flow.ExpireFact{ExecutionID: exec.ID, ActionID: "act-0", At: deadline})
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if next.Actions[0].State != kernel.ActionFailed || next.Actions[1].State != kernel.ActionCancelled {
			t.Fatalf("actions = %q/%q, want failed/cancelled", next.Actions[0].State, next.Actions[1].State)
		}
		assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
		assertFailure(t, commit, kernel.FailureTimeout)
		if !commit.Deadline.Equal(deadline) {
			t.Fatalf("commit deadline = %v, want persisted deadline %v", commit.Deadline, deadline)
		}
		if !commit.InvalidateCredential {
			t.Fatal("request timeout did not revoke execution authority")
		}
		if commit.Stop != nil {
			t.Fatalf("request timeout carried a workload stop intent: %+v", commit.Stop)
		}
		if commit.Answer != nil {
			t.Fatalf("request timeout carried an answer: %+v", commit.Answer)
		}
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})

	t.Run("cancel running request", func(t *testing.T) {
		exec := runningRequestExecution(
			requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		next, commit := mustReduce(t, exec, flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: at.Add(time.Minute)})
		if next.State != kernel.ExecutionCancelled {
			t.Fatalf("execution state = %q, want cancelled", next.State)
		}
		if next.Actions[0].State != kernel.ActionCancelled || next.Actions[1].State != kernel.ActionCancelled {
			t.Fatalf("actions = %q/%q, want cancelled/cancelled", next.Actions[0].State, next.Actions[1].State)
		}
		assertAllActionsSettled(t, exec, commit, kernel.ActionCancelled)
		if commit.Stop != nil {
			t.Fatalf("request cancel carried a workload stop intent: %+v", commit.Stop)
		}
		if !commit.InvalidateCredential {
			t.Fatal("request cancel did not revoke execution authority")
		}
		assertCleanup(t, exec, commit, "")
		if next.Actions[0].Request != nil && next.Actions[0].Request.Answer != nil {
			t.Fatal("request cancel invented an answer")
		}
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})

	t.Run("cancel queued execution", func(t *testing.T) {
		exec := newExecution(
			containerAction("act-0", "build", 0, time.Hour),
			requestAction("act-1", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
		)
		next, commit := mustReduce(t, exec, flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: at})
		if next.State != kernel.ExecutionCancelled {
			t.Fatalf("execution state = %q, want cancelled", next.State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		for i, action := range next.Actions {
			if action.State != kernel.ActionCancelled {
				t.Fatalf("action %d state = %q, want cancelled", i, action.State)
			}
		}
		assertAllActionsSettled(t, exec, commit, kernel.ActionCancelled)
		if commit.Stop != nil {
			t.Fatalf("queued cancel carried a workload stop intent: %+v", commit.Stop)
		}
		if !commit.InvalidateCredential {
			t.Fatal("queued cancel did not revoke execution authority")
		}
		assertCleanup(t, exec, commit, "")
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})
}

// ---------------------------------------------------------------------------
// Deadlines
// ---------------------------------------------------------------------------

func TestDeadlineBoundaryAndRestart(t *testing.T) {
	at := baseTime

	t.Run("activation deadline is UTC and queue time is free", func(t *testing.T) {
		zone := time.FixedZone("factory", 2*60*60)
		factAt := at.In(zone)
		timeout := 30 * time.Minute
		exec := newExecution(containerAction("act-0", "build", 0, timeout))
		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: factAt})
		want := factAt.UTC().Add(timeout)
		if !next.Actions[0].Deadline.Equal(want) {
			t.Fatalf("Deadline = %v, want %v", next.Actions[0].Deadline, want)
		}
		if next.Actions[0].Deadline.Location() != time.UTC {
			t.Fatalf("Deadline location = %v, want UTC", next.Actions[0].Deadline.Location())
		}
		if !commit.Deadline.Equal(want) {
			t.Fatalf("commit deadline = %v, want %v", commit.Deadline, want)
		}
	})

	t.Run("answer at equality expires", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		deadline := exec.Actions[0].Deadline
		answer := kernel.Answer{
			Decision: kernel.DecisionPass,
			Outputs:  kernel.Outputs{id.OutputKey("decision"): "pass"},
			Actor:    id.PrincipalID("principal-1"),
			At:       deadline.Add(-time.Minute),
		}
		mustRefuse(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: deadline}, fault.DeadlineExpired)

		next, commit := mustReduce(t, exec, flow.ExpireFact{ExecutionID: exec.ID, ActionID: "act-0", At: deadline})
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		assertFailure(t, commit, kernel.FailureTimeout)
		if !commit.Deadline.Equal(deadline) {
			t.Fatalf("commit deadline = %v, want %v", commit.Deadline, deadline)
		}
	})

	t.Run("sender timestamp is ignored", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		deadline := exec.Actions[0].Deadline

		earlySender := kernel.Answer{
			Decision: kernel.DecisionPass,
			Outputs:  kernel.Outputs{id.OutputKey("decision"): "pass"},
			Actor:    id.PrincipalID("principal-1"),
			At:       deadline.Add(-time.Minute),
		}
		mustRefuse(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: earlySender, At: deadline}, fault.DeadlineExpired)

		lateSender := earlySender
		lateSender.At = deadline.Add(time.Minute)
		next, commit := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: lateSender, At: deadline.Add(-time.Minute)})
		if next.Actions[0].State != kernel.ActionSucceeded {
			t.Fatalf("action state = %q, want succeeded", next.Actions[0].State)
		}
		if commit.Answer == nil || !commit.Answer.At.Equal(lateSender.At) {
			t.Fatalf("retained answer = %+v, want sender time %v", commit.Answer, lateSender.At)
		}
	})

	t.Run("restart uses the persisted deadline", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
		persisted := exec.Actions[0].Deadline
		// A restart must not recalculate the deadline from a changed timeout.
		exec.Actions[0].Timeout = 4 * time.Hour
		mustRefuse(t, exec, flow.ExpireFact{ExecutionID: exec.ID, ActionID: "act-0", At: persisted.Add(-time.Nanosecond)}, fault.InvalidInput)

		next, commit := mustReduce(t, exec, flow.ExpireFact{ExecutionID: exec.ID, ActionID: "act-0", At: persisted})
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("state at persisted deadline = %q, want failed", next.State)
		}
		assertFailure(t, commit, kernel.FailureTimeout)
		if !commit.Deadline.Equal(persisted) {
			t.Fatalf("commit deadline = %v, want persisted %v", commit.Deadline, persisted)
		}
	})

	t.Run("late answer after deadline is refused", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		deadline := exec.Actions[0].Deadline
		answer := kernel.Answer{
			Decision: kernel.DecisionFail,
			Outputs:  kernel.Outputs{id.OutputKey("decision"): "reject"},
			Actor:    id.PrincipalID("principal-1"),
			At:       deadline.Add(-time.Minute),
		}
		mustRefuse(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: deadline.Add(time.Nanosecond)}, fault.DeadlineExpired)
	})
}

// ---------------------------------------------------------------------------
// Requests and answers
// ---------------------------------------------------------------------------

func TestAnswerPassAndFailRetentionAndDownstreamEligibility(t *testing.T) {
	at := baseTime

	t.Run("pass is retained and becomes downstream output", func(t *testing.T) {
		review := requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
		nextSubject := definition.NewSubject(map[string]definition.SubjectValue{
			"candidate": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "review", Key: "decision"}},
		})
		consume := requestAction("act-1", "consume", 1, time.Hour, nextSubject, "result")
		exec := runningRequestExecution(review, consume)
		answer := kernel.Answer{
			Decision:   kernel.DecisionPass,
			Outputs:    kernel.Outputs{id.OutputKey("decision"): "approve"},
			Actor:      id.PrincipalID("principal-1"),
			At:         at.Add(time.Minute),
			Provenance: []byte("signed-shell-fact"),
		}

		next, commit := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(2 * time.Minute)})
		if next.Actions[0].State != kernel.ActionSucceeded {
			t.Fatalf("request state = %q, want succeeded", next.Actions[0].State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if next.Actions[1].State != kernel.ActionPending {
			t.Fatalf("downstream action state = %q, want pending", next.Actions[1].State)
		}
		if commit.Answer == nil || !reflect.DeepEqual(*commit.Answer, answer) {
			t.Fatalf("commit answer = %+v, want %+v", commit.Answer, answer)
		}
		if next.Actions[0].Request == nil || next.Actions[0].Request.Answer == nil || !reflect.DeepEqual(*next.Actions[0].Request.Answer, answer) {
			t.Fatalf("retained answer = %+v, want %+v", next.Actions[0].Request.Answer, answer)
		}
		if next.Actions[0].Request.Subject == nil {
			t.Fatal("answer settlement dropped the immutable resolved subject")
		}
		if got := next.Actions[0].Request.Subject.Fields()["operation"]; got.Kind != definition.SubjectLiteral || got.Literal != "review" {
			t.Fatalf("answer settlement rebound the subject: %+v", got)
		}
		if !commit.Answer.DownstreamEligible() {
			t.Fatal("passing answer is not downstream-eligible")
		}
		if commit.Output.Action != flow.OutputPromote {
			t.Fatalf("output action = %q, want promote", commit.Output.Action)
		}
		if next.Actions[0].Outputs[id.OutputKey("decision")] != "approve" {
			t.Fatalf("settled request outputs = %v, want decision=approve", next.Actions[0].Outputs)
		}

		next, _ = mustReduce(t, next, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at.Add(3 * time.Minute)})
		if next.Actions[1].Request == nil || next.Actions[1].Request.Subject == nil {
			t.Fatal("downstream request did not resolve its subject")
		}
		got := next.Actions[1].Request.Subject.Fields()["candidate"]
		if got.Kind != definition.SubjectLiteral || got.Literal != "approve" {
			t.Fatalf("downstream subject = %+v, want literal approve", got)
		}
	})

	t.Run("fail is retained but never downstream", func(t *testing.T) {
		review := requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
		consume := requestAction("act-1", "consume", 1, time.Hour, literalSubject(map[string]string{"operation": "consume"}), "result")
		exec := runningRequestExecution(review, consume)
		answer := kernel.Answer{
			Decision:   kernel.DecisionFail,
			Outputs:    kernel.Outputs{id.OutputKey("decision"): "reject"},
			Actor:      id.PrincipalID("principal-1"),
			At:         at.Add(time.Minute),
			Provenance: []byte("signed-shell-fact"),
		}

		next, commit := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(2 * time.Minute)})
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if next.Actions[0].State != kernel.ActionFailed {
			t.Fatalf("request state = %q, want failed", next.Actions[0].State)
		}
		if next.Actions[1].State != kernel.ActionCancelled {
			t.Fatalf("downstream action state = %q, want cancelled", next.Actions[1].State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if commit.Answer == nil || !reflect.DeepEqual(*commit.Answer, answer) {
			t.Fatalf("commit answer = %+v, want %+v", commit.Answer, answer)
		}
		if next.Actions[0].Request == nil || next.Actions[0].Request.Answer == nil {
			t.Fatal("failed answer was not retained for inspection")
		}
		if next.Actions[0].Request.Subject == nil {
			t.Fatal("failed answer dropped the immutable resolved subject")
		}
		if got := next.Actions[0].Request.Subject.Fields()["operation"]; got.Kind != definition.SubjectLiteral || got.Literal != "review" {
			t.Fatalf("failed answer rebound the subject: %+v", got)
		}
		if commit.Answer.DownstreamEligible() {
			t.Fatal("failed answer became downstream-eligible")
		}
		if commit.Output.Action == flow.OutputPromote {
			t.Fatal("failed answer promoted its outputs")
		}
		if _, ok := next.Actions[0].Outputs[id.OutputKey("decision")]; ok {
			t.Fatalf("failed answer output leaked into settled outputs: %v", next.Actions[0].Outputs)
		}
		assertFailure(t, commit, kernel.FailureRuntime)
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
	})

	t.Run("answer output validation and invalid facts", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		undeclared := kernel.Answer{Decision: kernel.DecisionPass, Outputs: kernel.Outputs{id.OutputKey("other"): "x"}, Actor: "principal-1", At: at}
		mustRefuse(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: undeclared, At: at.Add(time.Minute)}, fault.InvalidInput)

		invalidDecision := kernel.Answer{Decision: kernel.Decision("maybe"), Actor: "principal-1", At: at}
		mustRefuse(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: invalidDecision, At: at.Add(time.Minute)}, fault.InvalidInput)

		container := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
		answer := kernel.Answer{Decision: kernel.DecisionPass, Actor: "principal-1", At: at}
		mustRefuse(t, container, flow.AnswerFact{ExecutionID: container.ID, ActionID: "act-0", Answer: answer, At: at.Add(time.Minute)}, fault.InvalidInput)

		unresolved := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		unresolved.Actions[0].Request.Subject = nil
		mustRefuse(t, unresolved, flow.AnswerFact{ExecutionID: unresolved.ID, ActionID: "act-0", Answer: answer, At: at.Add(time.Minute)}, fault.InvalidInput)
	})
}

func TestIdenticalRetryConvergesAndConflictingSettlementIsRefused(t *testing.T) {
	at := baseTime

	t.Run("output staging", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
		first := kernel.Outputs{id.OutputKey("candidate"): "abc"}
		next, _ := mustReduce(t, exec, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: first, At: at.Add(time.Minute)})
		mustNoop(t, next, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: first, At: at.Add(2 * time.Minute)})
		different := kernel.Outputs{id.OutputKey("candidate"): "different"}
		mustRefuse(t, next, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: different, At: at.Add(2 * time.Minute)}, fault.ChangedIdempotencyContent)
	})

	t.Run("submitted null converges", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
		next, commit := mustReduce(t, exec, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: nil, At: at.Add(time.Minute)})
		if commit.Output.Action != flow.OutputStage || commit.Output.Values != nil {
			t.Fatalf("null stage = %+v, want stage with nil values", commit.Output)
		}
		if !next.Actions[0].StagedOutputsSet || next.Actions[0].StagedOutputs != nil {
			t.Fatalf("staged null = %v set=%t, want nil set=true", next.Actions[0].StagedOutputs, next.Actions[0].StagedOutputsSet)
		}
		mustNoop(t, next, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: nil, At: at.Add(2 * time.Minute)})
	})

	t.Run("request answer", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		answer := kernel.Answer{
			Decision: kernel.DecisionPass,
			Outputs:  kernel.Outputs{id.OutputKey("decision"): "approve"},
			Actor:    id.PrincipalID("principal-1"),
			At:       at.Add(time.Minute),
		}
		next, _ := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(2 * time.Minute)})
		mustNoop(t, next, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(3 * time.Minute)})

		replacement := answer
		replacement.Decision = kernel.DecisionFail
		mustRefuse(t, next, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: replacement, At: at.Add(3 * time.Minute)}, fault.ChangedIdempotencyContent)

		newContent := answer
		newContent.Outputs = kernel.Outputs{id.OutputKey("decision"): "other"}
		mustRefuse(t, next, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: newContent, At: at.Add(3 * time.Minute)}, fault.ChangedIdempotencyContent)
	})

	t.Run("terminal facts", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
		success := kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))
		next, _ := mustReduce(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: success, At: at.Add(time.Minute)})
		if next.State != kernel.ExecutionSucceeded {
			t.Fatalf("execution state = %q, want succeeded", next.State)
		}
		mustNoop(t, next, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: success, At: at.Add(2 * time.Minute)})
		mustRefuse(t, next, flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)}, fault.ChangedIdempotencyContent)

		cancelled := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
		cancelledNext, _ := mustReduce(t, cancelled, flow.CancelFact{ExecutionID: cancelled.ID, Reason: flow.CancelRequested, At: at.Add(time.Minute)})
		mustNoop(t, cancelledNext, flow.CancelFact{ExecutionID: cancelled.ID, Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)})
		mustRefuse(t, cancelledNext, flow.CancelFact{ExecutionID: cancelled.ID, Reason: flow.CancelUpstreamFailed, At: at.Add(2 * time.Minute)}, fault.ChangedIdempotencyContent)

		expired := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
		deadline := expired.Actions[0].Deadline
		expiredNext, _ := mustReduce(t, expired, flow.ExpireFact{ExecutionID: expired.ID, ActionID: "act-0", At: deadline})
		if expiredNext.State != kernel.ExecutionFailed {
			t.Fatalf("expired state = %q, want failed", expiredNext.State)
		}
		mustNoop(t, expiredNext, flow.ExpireFact{ExecutionID: expired.ID, ActionID: "act-0", At: deadline.Add(time.Minute)})
		mustRefuse(t, expiredNext, flow.CancelFact{ExecutionID: expired.ID, Reason: flow.CancelRequested, At: deadline.Add(time.Minute)}, fault.ChangedIdempotencyContent)
	})
}

// ---------------------------------------------------------------------------
// Stale and unknown evidence
// ---------------------------------------------------------------------------

func TestStaleAndUnknownEvidenceNeverStartsWork(t *testing.T) {
	at := baseTime

	t.Run("unknown absent and running observations do not advance", func(t *testing.T) {
		for _, observation := range []kernel.Observation{
			kernel.UnknownObservation(),
			kernel.AbsentObservation(),
			kernel.RunningObservation(),
		} {
			exec := runningContainerExecution(
				containerAction("act-0", "build", 0, time.Hour, "candidate"),
				containerAction("act-1", "test", 1, time.Hour),
			)
			next, commit := mustReduce(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: observation, At: at.Add(time.Minute)})
			if next.State != kernel.ExecutionRunning {
				t.Fatalf("%s observation changed execution state to %q", observation.Kind, next.State)
			}
			if next.CurrentAction != 0 || next.Actions[0].State != kernel.ActionRunning {
				t.Fatalf("%s observation advanced current=%d state=%q", observation.Kind, next.CurrentAction, next.Actions[0].State)
			}
			if next.Actions[1].State != kernel.ActionPending {
				t.Fatalf("%s observation changed pending successor to %q", observation.Kind, next.Actions[1].State)
			}
			if commit.Launch != nil && commit.Launch.State == kernel.LaunchNotIssued {
				t.Fatalf("%s observation requested a fresh launch: %+v", observation.Kind, commit.Launch)
			}
			if commit.Output.Action == flow.OutputPromote {
				t.Fatalf("%s observation promoted outputs", observation.Kind)
			}
			if commit.Failure != nil {
				t.Fatalf("%s observation failed the execution: %+v", observation.Kind, commit.Failure)
			}
		}
	})

	t.Run("only affirmative never-started proof begins retirement", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		oldWorkload := exec.Actions[0].Container.WorkloadID
		oldKey := exec.Actions[0].Container.Launch.IdempotencyKey
		proof := "runtime proved no object was created"
		next, commit := mustReduce(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: kernel.NeverStarted(proof),
			At:          at.Add(time.Minute),
		})
		if next.Actions[0].State != kernel.ActionRunning {
			t.Fatalf("action state after never-started = %q, want running", next.Actions[0].State)
		}
		if commit.Launch == nil {
			t.Fatal("never-started proof did not persist a retiring launch intent")
		}
		if commit.Launch.State != kernel.LaunchRetiring {
			t.Fatalf("retiring launch state = %q, want retiring", commit.Launch.State)
		}
		if commit.Launch.WorkloadID != oldWorkload {
			t.Fatalf("retirement replaced workload identity %q with %q before removal was confirmed", oldWorkload, commit.Launch.WorkloadID)
		}
		if commit.Launch.IdempotencyKey != oldKey {
			t.Fatalf("retirement changed idempotency key %q to %q", oldKey, commit.Launch.IdempotencyKey)
		}
		if commit.Observation == nil || commit.Observation.Kind != kernel.ObservationNeverStarted || commit.Observation.Proof != proof {
			t.Fatalf("retirement did not retain the never-started proof: %+v", commit.Observation)
		}
		if next.Actions[0].Container.WorkloadID != commit.Launch.WorkloadID {
			t.Fatalf("action workload = %q, want %q", next.Actions[0].Container.WorkloadID, commit.Launch.WorkloadID)
		}
		if next.Actions[0].Container.Launch.State != kernel.LaunchRetiring {
			t.Fatalf("projected launch state = %q, want retiring", next.Actions[0].Container.Launch.State)
		}
		if !next.Actions[0].Container.Observation.ProofOfNonexecution() {
			t.Fatalf("projected observation lost the never-started proof: %+v", next.Actions[0].Container.Observation)
		}
		if next.Actions[1].State != kernel.ActionPending {
			t.Fatalf("pending successor state = %q, want pending", next.Actions[1].State)
		}
	})

	t.Run("malformed and stale evidence is refused", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour),
			containerAction("act-1", "test", 1, time.Hour),
		)
		mustRefuse(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: kernel.NeverStarted(""), At: at}, fault.InvalidInput)
		mustRefuse(t, exec, flow.ObserveFact{ExecutionID: "other-exec", ActionID: "act-0", Observation: kernel.RunningObservation(), At: at}, fault.InvalidInput)
		mustRefuse(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "missing", Observation: kernel.RunningObservation(), At: at}, fault.InvalidInput)

		request := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		mustRefuse(t, request, flow.ObserveFact{ExecutionID: request.ID, ActionID: "act-0", Observation: kernel.RunningObservation(), At: at}, fault.InvalidInput)

		mustRefuse(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: "other-workspace", At: at}, fault.InvalidInput)

		stale := newExecution(
			containerAction("act-0", "build", 0, time.Hour),
			containerAction("act-1", "test", 1, time.Hour),
		)
		stale.State = kernel.ExecutionRunning
		stale.CurrentAction = 1
		stale.Actions[0].State = kernel.ActionSucceeded
		stale.Actions[1].State = kernel.ActionRunning
		mustRefuse(t, stale, flow.ObserveFact{ExecutionID: stale.ID, ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("late")), At: at}, fault.InvalidInput)
	})
}

func TestWorkspaceLostFailsVisiblyAndCancelsPending(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
	exec.Actions[0].StagedOutputsSet = true

	next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(time.Minute)})
	if next.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].State != kernel.ActionFailed || next.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("actions = %q/%q, want failed/cancelled", next.Actions[0].State, next.Actions[1].State)
	}
	if next.Actions[0].StagedOutputsSet {
		t.Fatal("workspace loss retained staged outputs")
	}
	assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
	assertFailure(t, commit, kernel.FailureWorkspaceLost)
	assertNoPending(t, next)
	if commit.Output.Action != flow.OutputDiscard {
		t.Fatalf("output action = %q, want discard", commit.Output.Action)
	}
	if commit.Launch != nil && commit.Launch.State == kernel.LaunchNotIssued {
		t.Fatalf("workspace loss requested a fresh launch: %+v", commit.Launch)
	}
	assertSingleActiveAction(t, next)
}

// TestWorkspaceLossKeepsStopAndCleanupAttributable pins S02 §2/§5: losing the
// retained workspace fails the execution and cancels pending work in one
// commit, but it is not evidence that a live workload stopped. A running
// container therefore carries a stop intent plus pending cleanup attribution in
// the same commit, while request and queued executions emit no stop. The fact
// stays idempotent and scratch state is never reconstructed.
func TestWorkspaceLossKeepsStopAndCleanupAttributable(t *testing.T) {
	at := baseTime

	t.Run("running container", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
		exec.Actions[0].StagedOutputsSet = true

		next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(time.Minute)})
		if next.State != kernel.ExecutionFailed || next.CurrentAction != -1 {
			t.Fatalf("execution = %q current=%d, want failed/-1", next.State, next.CurrentAction)
		}
		assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
		assertFailure(t, commit, kernel.FailureWorkspaceLost)
		assertNoPending(t, next)
		assertSingleActiveAction(t, next)
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("workspace loss retained staged outputs")
		}

		assertStopIntent(t, commit, id.WorkloadID("wl-0"), exec.WorkspaceID)
		assertCleanup(t, next, commit, id.WorkloadID("wl-0"))
		if commit.Cleanup.Reason != "workspace_lost" {
			t.Fatalf("cleanup reason = %q, want workspace_lost", commit.Cleanup.Reason)
		}
		if next.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("visible cleanup state = %q, want pending", next.Cleanup.State)
		}
		if next.Cleanup.WorkloadID != id.WorkloadID("wl-0") || next.Cleanup.WorkspaceID != exec.WorkspaceID {
			t.Fatalf("visible cleanup attribution = %+v, want workload/workspace", next.Cleanup)
		}

		// The workspace is never reconstructed; the effect identity stays
		// attributable for the pending stop and cleanup.
		if commit.Launch != nil {
			t.Fatalf("workspace loss requested a fresh launch: %+v", commit.Launch)
		}
		if next.Actions[0].Container == nil || next.Actions[0].Container.WorkloadID != id.WorkloadID("wl-0") {
			t.Fatalf("workspace loss dropped the workload identity: %+v", next.Actions[0].Container)
		}
	})

	t.Run("running request", func(t *testing.T) {
		exec := runningRequestExecution(
			requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
			requestAction("act-1", "consume", 1, time.Hour, literalSubject(map[string]string{"operation": "consume"}), "result"),
		)
		next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(time.Minute)})
		if commit.Stop != nil {
			t.Fatalf("request workspace loss emitted a stop intent: %+v", commit.Stop)
		}
		if commit.Cleanup == nil || commit.Cleanup.WorkloadID != "" {
			t.Fatalf("request cleanup = %+v, want workspace-only attribution", commit.Cleanup)
		}
		assertCleanup(t, next, commit, "")
		assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
		assertFailure(t, commit, kernel.FailureWorkspaceLost)
	})

	t.Run("queued execution", func(t *testing.T) {
		exec := newExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(time.Minute)})
		if next.State != kernel.ExecutionFailed || next.CurrentAction != -1 {
			t.Fatalf("execution = %q current=%d, want failed/-1", next.State, next.CurrentAction)
		}
		if commit.Stop != nil {
			t.Fatalf("queued workspace loss emitted a stop intent: %+v", commit.Stop)
		}
		if commit.ActionID != "" || commit.NextActionState != "" {
			t.Fatalf("queued workspace loss settled a primary action: %q/%q", commit.ActionID, commit.NextActionState)
		}
		if len(commit.PendingCancellations) != len(exec.Actions) {
			t.Fatalf("pending cancellations = %+v, want every queued action", commit.PendingCancellations)
		}
		assertNoPending(t, next)
		assertCleanup(t, next, commit, "")
		assertFailure(t, commit, kernel.FailureWorkspaceLost)
	})

	t.Run("duplicate fact converges", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour),
			containerAction("act-1", "test", 1, time.Hour),
		)
		next, commit := mustReduce(t, exec, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(time.Minute)})
		assertStopIntent(t, commit, id.WorkloadID("wl-0"), exec.WorkspaceID)
		mustNoop(t, next, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(2 * time.Minute)})
		mustNoop(t, next, flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: at.Add(3 * time.Minute)})
	})
}

// ---------------------------------------------------------------------------
// Cleanup, terminal races and versions
// ---------------------------------------------------------------------------

func TestCleanupIsSeparateFromTerminalOutcome(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))

	success := kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))
	next, _ := mustReduce(t, exec, flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: success, At: at.Add(time.Minute)})
	if next.State != kernel.ExecutionSucceeded {
		t.Fatalf("execution state = %q, want succeeded", next.State)
	}

	failed := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	failedNext, failedCommit := mustReduce(t, failed, flow.CancelFact{ExecutionID: failed.ID, Reason: flow.CancelRequested, At: at.Add(time.Minute)})
	if failedNext.State != kernel.ExecutionCancelled {
		t.Fatalf("cancelled execution state = %q", failedNext.State)
	}
	if failedNext.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cancelled execution cleanup state = %q, want pending", failedNext.Cleanup.State)
	}
	if failedCommit.CleanupUpdate == nil || failedCommit.CleanupUpdate.State != kernel.CleanupPending {
		t.Fatalf("cancel cleanup update = %+v, want pending", failedCommit.CleanupUpdate)
	}
	if failedNext.Failure != nil {
		t.Fatalf("cancelled execution carried a failure: %+v", failedNext.Failure)
	}
	// Terminal outcome stays immutable while cleanup is pending.
	mustNoop(t, failedNext, flow.CancelFact{ExecutionID: failed.ID, Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)})
}

func TestTerminalRacesPreserveTheFirstWinner(t *testing.T) {
	at := baseTime
	type race struct {
		name  string
		first flow.Fact
		last  flow.Fact
	}
	races := []race{
		{
			name:  "success then cancel",
			first: flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(time.Minute)},
			last:  flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)},
		},
		{
			name:  "cancel then success",
			first: flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(time.Minute)},
			last:  flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(2 * time.Minute)},
		},
		{
			name:  "success then expire",
			first: flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(time.Minute)},
			last:  flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
		},
		{
			name:  "expire then success",
			first: flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
			last:  flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(2 * time.Hour)},
		},
		{
			name:  "failure then cancel",
			first: flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")), At: at.Add(time.Minute)},
			last:  flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)},
		},
		{
			name:  "cancel then failure",
			first: flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(time.Minute)},
			last:  flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")), At: at.Add(2 * time.Minute)},
		},
		{
			name:  "cancel then expire",
			first: flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(time.Minute)},
			last:  flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
		},
		{
			name:  "expire then cancel",
			first: flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
			last:  flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(2 * time.Hour)},
		},
		{
			name:  "workspace lost then success",
			first: flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at.Add(time.Minute)},
			last:  flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(2 * time.Minute)},
		},
		{
			name:  "success then workspace lost",
			first: flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: at.Add(time.Minute)},
			last:  flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at.Add(2 * time.Minute)},
		},
		{
			name:  "failure then expire",
			first: flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")), At: at.Add(time.Minute)},
			last:  flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
		},
		{
			name:  "expire then failure",
			first: flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(time.Hour)},
			last:  flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-0", Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")), At: at.Add(2 * time.Hour)},
		},
	}

	for _, test := range races {
		t.Run(test.name, func(t *testing.T) {
			exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
			winner, winnerCommit := mustReduce(t, exec, test.first)
			if !winner.State.Terminal() {
				t.Fatalf("first fact did not settle the execution: state = %q", winner.State)
			}
			terminalFingerprint := fingerprint(winner)
			terminalCommitState := winnerCommit.NextExecutionState
			winnerState := winner.State

			mustRefuse(t, winner, test.last, "")
			if got := fingerprint(winner); got != terminalFingerprint {
				t.Fatalf("terminal state changed after conflicting fact\nbefore:\n%s\nafter:\n%s", terminalFingerprint, got)
			}
			if winner.State != winnerState || winnerCommit.NextExecutionState != terminalCommitState {
				t.Fatalf("terminal outcome changed: state=%q commit=%q", winner.State, winnerCommit.NextExecutionState)
			}
		})
	}
}

func TestAuthoritativeVersionsAreCarriedAndBumped(t *testing.T) {
	at := baseTime
	exec := newExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	exec.Version = 7
	exec.Actions[0].Version = 3

	next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})
	if commit.ExpectedExecutionVersion != 7 {
		t.Fatalf("expected execution version = %d, want 7", commit.ExpectedExecutionVersion)
	}
	if commit.ExpectedActionVersion != 3 {
		t.Fatalf("expected action version = %d, want 3", commit.ExpectedActionVersion)
	}
	if next.Version != 8 {
		t.Fatalf("next execution version = %d, want 8", next.Version)
	}
	if next.Actions[0].Version != 4 {
		t.Fatalf("next action version = %d, want 4", next.Actions[0].Version)
	}

	// A stale caller that reduces the same old revision must produce the same
	// compare-and-commit payload; the reducer never guesses a newer version.
	again, againCommit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})
	if !reflect.DeepEqual(commit, againCommit) {
		t.Fatalf("repeat reduction produced a different commit\nfirst:  %+v\nsecond: %+v", commit, againCommit)
	}
	if !reflect.DeepEqual(next, again) {
		t.Fatalf("repeat reduction produced a different next state")
	}
}

func TestReducerDoesNotAliasInputStorage(t *testing.T) {
	at := baseTime
	exec := newExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	next, _ := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})

	exec.Actions[0].State = kernel.ActionCancelled
	exec.Actions[0].Container.WorkloadID = id.WorkloadID("mutated")
	exec.Actions[0].Container.Launch.WorkloadID = id.WorkloadID("mutated")
	if next.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("next state aliases the input action state: %q", next.Actions[0].State)
	}
	if next.Actions[0].Container.WorkloadID == id.WorkloadID("mutated") {
		t.Fatal("next state aliases the input workload identity")
	}
	if next.Actions[0].Container.Launch.WorkloadID == id.WorkloadID("mutated") {
		t.Fatal("next state aliases the input launch intent")
	}
}
