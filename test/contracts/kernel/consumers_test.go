package kernelcontract

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// ---------------------------------------------------------------------------
// Contract fakes: these prove the declared ports are implementable without
// Shell, SQL, identity, processes or any infrastructure.
// ---------------------------------------------------------------------------

type fakeClock struct {
	now time.Time
}

func (f fakeClock) Now() time.Time { return f.now }

var _ engine.Clock = fakeClock{}

type fakeState struct {
	loaded  map[id.ExecutionID]kernel.Execution
	commits []flow.Commit
	scans   []engine.ScanRequest
}

func (f *fakeState) Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	return f.loaded[executionID], nil
}

// Commit builds the next execution solely from the commit payload: every
// durable change the reducer can request must be reachable through flow.Commit.
// If a field is missing from Commit, this fake cannot persist it and the
// contract tests fail instead of silently dropping evidence.
func (f *fakeState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	f.commits = append(f.commits, commit)
	execution := f.loaded[commit.ExecutionID]
	execution.Version = commit.ExpectedExecutionVersion + 1
	execution.State = commit.NextExecutionState
	execution.Actions = append([]kernel.Action(nil), execution.Actions...)
	if commit.Failure != nil {
		failure := *commit.Failure
		execution.Failure = &failure
	}
	if update := commit.CleanupUpdate; update != nil {
		execution.Cleanup = kernel.Cleanup{
			State:       update.State,
			WorkloadID:  update.WorkloadID,
			WorkspaceID: update.WorkspaceID,
			Attempts:    update.Attempts,
			LastError:   update.LastError,
		}
	}
	if commit.NextCurrentAction != nil {
		execution.CurrentAction = *commit.NextCurrentAction
	}

	if commit.ActionID != "" {
		action := findAction(&execution, commit.ActionID)
		if action == nil {
			execution.Actions = append(execution.Actions, kernel.Action{
				ID:    commit.ActionID,
				Index: len(execution.Actions),
			})
			action = &execution.Actions[len(execution.Actions)-1]
		}
		// Copy kind-specific storage so repeated commits do not alias.
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
			// A nil Values map is a submitted JSON null. The presence marker
			// keeps that distinct from never submitting.
			action.StagedOutputs = commit.Output.Values
			action.StagedOutputsSet = true
		case flow.OutputPromote:
			action.Outputs = commit.Output.Values
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
			action.Request.Subject = commit.Subject
		}
		if commit.Answer != nil {
			if action.Request == nil {
				action.Request = &kernel.RequestAction{}
			}
			action.Request.Answer = commit.Answer
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
				action.Container.WorkspaceID = execution.WorkspaceID
			}
		}
	}

	// Additional action updates are part of the same atomic commit: a
	// failure/cancellation settles the current action and cancels every
	// pending successor without a second write.
	for _, update := range commit.PendingCancellations {
		action := findAction(&execution, update.ActionID)
		if action == nil {
			continue
		}
		action.Version = update.ExpectedVersion + 1
		action.State = update.NextState
	}

	f.loaded[commit.ExecutionID] = execution
	return execution, nil
}

func findAction(execution *kernel.Execution, actionID id.ActionID) *kernel.Action {
	if actionID == "" {
		return nil
	}
	for index := range execution.Actions {
		if execution.Actions[index].ID == actionID {
			return &execution.Actions[index]
		}
	}
	return nil
}

func (f *fakeState) Scan(ctx context.Context, request engine.ScanRequest) ([]id.ExecutionID, error) {
	f.scans = append(f.scans, request)
	return []id.ExecutionID{"exec-1"}, nil
}

var _ engine.State = (*fakeState)(nil)

type fakeRuntime struct {
	capabilities engine.Capabilities
	start        engine.StartResult
	observations []kernel.Observation
	stops        []kernel.StopEvidence
	prepared     int
	removed      []engine.RemoveRequest
	refreshed    []engine.CredentialRequest
}

func (f *fakeRuntime) Capabilities(ctx context.Context) (engine.Capabilities, error) {
	return f.capabilities, nil
}

func (f *fakeRuntime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	f.prepared++
	return engine.Preparation{WorkspaceID: request.WorkspaceID, Detail: "prepared"}, nil
}

func (f *fakeRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	return f.start, nil
}

func (f *fakeRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if len(f.observations) == 0 {
		return kernel.UnknownObservation(), nil
	}
	observation := f.observations[0]
	f.observations = f.observations[1:]
	return observation, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, request engine.LogsRequest) (engine.LogBatch, error) {
	return engine.LogBatch{NextOffset: request.AfterOffset}, nil
}

func (f *fakeRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	if len(f.stops) == 0 {
		return kernel.PendingStop("stop response lost"), nil
	}
	stop := f.stops[0]
	f.stops = f.stops[1:]
	return stop, nil
}

func (f *fakeRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	f.removed = append(f.removed, request)
	return engine.Removal{Removed: true}, nil
}

func (f *fakeRuntime) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	f.refreshed = append(f.refreshed, request)
	return engine.CredentialRefresh{Delivered: true}, nil
}

var _ engine.Runtime = (*fakeRuntime)(nil)

type fakeWorkspace struct {
	available bool
	retained  []id.WorkspaceID
	released  []id.WorkspaceID
}

func (f *fakeWorkspace) Status(ctx context.Context, workspaceID id.WorkspaceID) (engine.WorkspaceObservation, error) {
	return engine.WorkspaceObservation{WorkspaceID: workspaceID, Available: f.available}, nil
}

func (f *fakeWorkspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	f.retained = append(f.retained, workspaceID)
	return nil
}

func (f *fakeWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	f.released = append(f.released, workspaceID)
	return nil
}

var _ engine.Workspace = (*fakeWorkspace)(nil)

// ---------------------------------------------------------------------------
// Value vocabulary
// ---------------------------------------------------------------------------

func TestExecutionAndActionStatesAreExact(t *testing.T) {
	wantExecution := []kernel.ExecutionState{
		kernel.ExecutionQueued,
		kernel.ExecutionRunning,
		kernel.ExecutionSucceeded,
		kernel.ExecutionFailed,
		kernel.ExecutionCancelled,
	}
	gotExecution := kernel.ExecutionStates()
	if len(gotExecution) != len(wantExecution) {
		t.Fatalf("ExecutionStates() length = %d, want %d", len(gotExecution), len(wantExecution))
	}
	for index, want := range wantExecution {
		if gotExecution[index] != want {
			t.Fatalf("ExecutionStates()[%d] = %q, want %q", index, gotExecution[index], want)
		}
		if !want.Valid() {
			t.Fatalf("ExecutionState %q is not valid", want)
		}
	}
	if kernel.ExecutionState("pending").Valid() {
		t.Fatal("execution state pending is valid; pending belongs to actions only")
	}
	if kernel.ExecutionState("stopped").Valid() {
		t.Fatal("execution state stopped is valid; stop is evidence, not a public state")
	}

	wantActions := []kernel.ActionState{
		kernel.ActionPending,
		kernel.ActionRunning,
		kernel.ActionSucceeded,
		kernel.ActionFailed,
		kernel.ActionCancelled,
	}
	gotActions := kernel.ActionStates()
	if len(gotActions) != len(wantActions) {
		t.Fatalf("ActionStates() length = %d, want %d", len(gotActions), len(wantActions))
	}
	for index, want := range wantActions {
		if gotActions[index] != want {
			t.Fatalf("ActionStates()[%d] = %q, want %q", index, gotActions[index], want)
		}
		if !want.Valid() {
			t.Fatalf("ActionState %q is not valid", want)
		}
	}
	if kernel.ActionState("queued").Valid() {
		t.Fatal("action state queued is valid; queued belongs to executions only")
	}
}

func TestDefinitionActionKindsAreExact(t *testing.T) {
	want := []definition.ActionKind{definition.ActionKindContainer, definition.ActionKindRequest}
	got := definition.ActionKinds()
	if len(got) != len(want) {
		t.Fatalf("ActionKinds() length = %d, want %d", len(got), len(want))
	}
	for index, kind := range want {
		if got[index] != kind {
			t.Fatalf("ActionKinds()[%d] = %q, want %q", index, got[index], kind)
		}
		if !kind.Valid() {
			t.Fatalf("ActionKind %q is not valid", kind)
		}
	}
	if definition.ActionKind("task").Valid() {
		t.Fatal("ActionKind task is valid; requests are actions, not a task kind")
	}
}

func TestObservationVocabularyKeepsUncertainty(t *testing.T) {
	kinds := kernel.ObservationKinds()
	if len(kinds) != 5 {
		t.Fatalf("ObservationKinds() length = %d, want 5", len(kinds))
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Fatalf("ObservationKind %q is not valid", kind)
		}
	}

	absent := kernel.AbsentObservation()
	if absent.Kind != kernel.ObservationAbsent {
		t.Fatalf("absent kind = %q", absent.Kind)
	}
	if absent.ProofOfNonexecution() {
		t.Fatal("absent proves nonexecution; absence alone is not affirmative evidence")
	}
	if absent.StopConfirmed() {
		t.Fatal("absent proves stop; absence alone is not stop evidence")
	}

	unknown := kernel.UnknownObservation()
	if unknown.Kind != kernel.ObservationUnknown {
		t.Fatalf("unknown kind = %q", unknown.Kind)
	}
	if unknown.ProofOfNonexecution() {
		t.Fatal("unknown proves nonexecution")
	}
	if unknown.StopConfirmed() {
		t.Fatal("unknown proves stop")
	}

	neverStarted := kernel.NeverStarted("runtime confirms no object was created")
	if neverStarted.Kind != kernel.ObservationNeverStarted {
		t.Fatalf("never-started kind = %q", neverStarted.Kind)
	}
	if !neverStarted.ProofOfNonexecution() {
		t.Fatal("never-started observation must be affirmative proof of nonexecution")
	}
	if neverStarted.StopConfirmed() {
		t.Fatal("never-started observation must not claim stop evidence")
	}

	running := kernel.RunningObservation()
	if running.Kind != kernel.ObservationRunning {
		t.Fatalf("running kind = %q", running.Kind)
	}
	if running.ProofOfNonexecution() || running.StopConfirmed() {
		t.Fatal("running observation must not claim nonexecution or stop")
	}

	exited := kernel.ExitedObservation(7, kernel.ConfirmedStop("termination observed"))
	if exited.Kind != kernel.ObservationExited {
		t.Fatalf("exited kind = %q", exited.Kind)
	}
	code, ok := exited.ExitStatus()
	if !ok || code != 7 {
		t.Fatalf("ExitStatus() = (%d, %v), want (7, true)", code, ok)
	}
	if !exited.StopConfirmed() {
		t.Fatal("exited observation with confirmed stop reports unconfirmed")
	}

	pendingStop := kernel.ExitedObservation(7, kernel.PendingStop("stop barrier still pending"))
	if pendingStop.StopConfirmed() {
		t.Fatal("exited observation with pending stop reports confirmed; stop is a separate barrier")
	}
}

func TestOutputBoundsDeclaredKeysAndTypes(t *testing.T) {
	declared := func(keys ...string) []id.OutputKey {
		out := make([]id.OutputKey, len(keys))
		for index, key := range keys {
			out[index] = id.OutputKey(key)
		}
		return out
	}

	var null kernel.Outputs
	if null != nil {
		t.Fatal("zero Outputs must be nil (JSON null)")
	}
	if err := null.Validate(declared("candidate")); err != nil {
		t.Fatalf("null Outputs.Validate() error = %v, want nil", err)
	}
	if len(null) != 0 {
		t.Fatalf("null Outputs length = %d", len(null))
	}

	empty := kernel.Outputs{}
	if encoded, err := json.Marshal(empty); err != nil || string(encoded) != "{}" {
		t.Fatalf("empty Outputs JSON = %s, error %v; want {}: a non-nil empty object, distinct from null", encoded, err)
	}
	if err := empty.Validate(declared("candidate")); err != nil {
		t.Fatalf("empty Outputs.Validate() error = %v, want nil", err)
	}

	eightKeys := declared("k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7")
	eight := kernel.Outputs{"k0": "v0", "k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5", "k6": "v6", "k7": "v7"}
	if err := eight.Validate(eightKeys); err != nil {
		t.Fatalf("8-value Outputs.Validate() error = %v, want nil", err)
	}

	nine := kernel.Outputs{"k0": "v0", "k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5", "k6": "v6", "k7": "v7", "k8": "v8"}
	if err := nine.Validate(declared("k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8")); err == nil {
		t.Fatal("9-value Outputs.Validate() = nil, want rejection")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("9-value Outputs.Validate() class = (%q, %v), want invalid_input", class, ok)
	}

	exact := kernel.Outputs{"candidate": strings.Repeat("a", kernel.MaxOutputValueBytes)}
	if err := exact.Validate(declared("candidate")); err != nil {
		t.Fatalf("4096-byte Outputs.Validate() error = %v, want nil", err)
	}
	tooLong := kernel.Outputs{"candidate": strings.Repeat("a", kernel.MaxOutputValueBytes+1)}
	if err := tooLong.Validate(declared("candidate")); err == nil {
		t.Fatal("4097-byte Outputs.Validate() = nil, want rejection")
	}

	multibyteExact := kernel.Outputs{"candidate": strings.Repeat("é", kernel.MaxOutputValueBytes/2)}
	if len(multibyteExact["candidate"]) != kernel.MaxOutputValueBytes {
		t.Fatalf("multibyte fixture length = %d, want %d", len(multibyteExact["candidate"]), kernel.MaxOutputValueBytes)
	}
	if err := multibyteExact.Validate(declared("candidate")); err != nil {
		t.Fatalf("multibyte 4096-byte Outputs.Validate() error = %v, want nil", err)
	}
	multibyteTooLong := kernel.Outputs{"candidate": strings.Repeat("é", kernel.MaxOutputValueBytes/2+1)}
	if err := multibyteTooLong.Validate(declared("candidate")); err == nil {
		t.Fatal("multibyte 4098-byte Outputs.Validate() = nil, want byte-length rejection")
	}

	invalidUTF8 := kernel.Outputs{"candidate": string([]byte{0xff, 0xfe})}
	if err := invalidUTF8.Validate(declared("candidate")); err == nil {
		t.Fatal("invalid UTF-8 Outputs.Validate() = nil, want rejection")
	}

	undeclared := kernel.Outputs{"other": "value"}
	if err := undeclared.Validate(declared("candidate")); err == nil {
		t.Fatal("undeclared Outputs.Validate() = nil, want rejection")
	}

	noDeclarations := kernel.Outputs{"candidate": "value"}
	if err := noDeclarations.Validate(nil); err == nil {
		t.Fatal("non-empty Outputs.Validate(nil) = nil, want rejection when no keys are declared")
	}

	nonString := kernel.Outputs{}
	if err := json.Unmarshal([]byte(`{"candidate":1}`), &nonString); err == nil {
		t.Fatal("JSON number into Outputs accepted, want string-only rejection")
	}
}

func TestAnswerVisibilitySeparatesInspectionFromDownstreamEligibility(t *testing.T) {
	failed := kernel.Answer{
		Decision: kernel.DecisionFail,
		Outputs:  kernel.Outputs{"reason": "checks failed"},
		Actor:    id.PrincipalID("principal-1"),
		At:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if len(failed.Outputs) != 1 {
		t.Fatal("failed answer response must remain inspectable")
	}
	if failed.DownstreamEligible() {
		t.Fatal("failed answer became downstream-eligible")
	}

	passed := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"candidate": "abc"},
		Actor:    id.PrincipalID("principal-1"),
		At:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if !passed.DownstreamEligible() {
		t.Fatal("passed answer is not downstream-eligible")
	}
}

func TestExecutionAndActionAggregateValues(t *testing.T) {
	deadline := time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)
	action := kernel.Action{
		ID:               "act-1",
		Name:             "build",
		Kind:             definition.ActionKindContainer,
		State:            kernel.ActionRunning,
		Version:          3,
		Timeout:          time.Hour,
		ActivatedAt:      deadline.Add(-time.Hour),
		Deadline:         deadline,
		Outputs:          kernel.Outputs{"candidate": "abc"},
		StagedOutputs:    kernel.Outputs{"candidate": "abc"},
		StagedOutputsSet: true,
		Stop:             kernel.PendingStop("stop barrier pending"),
	}
	execution := kernel.Execution{
		ID:            "exec-1",
		State:         kernel.ExecutionRunning,
		Version:       5,
		WorkspaceID:   "ws-1",
		Actions:       []kernel.Action{action},
		CurrentAction: 0,
		Cleanup: kernel.Cleanup{
			State:       kernel.CleanupPending,
			WorkloadID:  "wl-1",
			WorkspaceID: "ws-1",
		},
	}

	if execution.CurrentAction != 0 || len(execution.Actions) != 1 {
		t.Fatalf("execution active action = %d of %d", execution.CurrentAction, len(execution.Actions))
	}
	active := execution.Actions[0]
	if active.State != kernel.ActionRunning || active.Version != 3 {
		t.Fatalf("active action = %+v", active)
	}
	if !active.Deadline.Equal(deadline) {
		t.Fatalf("action deadline = %v, want %v", active.Deadline, deadline)
	}
	if got := active.Deadline.Sub(active.ActivatedAt); got != active.Timeout {
		t.Fatalf("persisted deadline offset = %v, want timeout %v", got, active.Timeout)
	}
	if active.Outputs["candidate"] != "abc" || active.StagedOutputs["candidate"] != "abc" {
		t.Fatalf("action outputs = %+v staged = %+v", active.Outputs, active.StagedOutputs)
	}
	if active.Stop.IsConfirmed() {
		t.Fatal("action stop evidence collapsed pending cleanup into confirmed stop")
	}
	if execution.State != kernel.ExecutionRunning || execution.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("execution state/cleanup = %q/%q", execution.State, execution.Cleanup.State)
	}
}

func TestTerminalOutcomeAndCleanupAreSeparate(t *testing.T) {
	execution := kernel.Execution{
		ID:      id.ExecutionID("exec-1"),
		State:   kernel.ExecutionFailed,
		Version: 9,
		Cleanup: kernel.Cleanup{
			State:       kernel.CleanupPending,
			WorkloadID:  id.WorkloadID("wl-1"),
			WorkspaceID: id.WorkspaceID("ws-1"),
			Attempts:    2,
			LastError:   "runtime unavailable",
		},
	}
	if execution.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q", execution.State)
	}
	if !execution.Cleanup.IsPending() {
		t.Fatal("cleanup pending must be visible independently of the terminal outcome")
	}
	if execution.Cleanup.WorkloadID != "wl-1" || execution.Cleanup.WorkspaceID != "ws-1" {
		t.Fatal("pending cleanup must be attributable to stable resource identities")
	}
	if execution.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q", execution.Cleanup.State)
	}
}

func TestDefinitionValues(t *testing.T) {
	container := definition.Container{
		Image:    "registry.example.com/tool:1",
		Commands: []string{"run", "test"},
		Env:      []definition.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
		Secrets:  []id.SecretName{"TEST_DB_PASSWORD"},
		Outputs:  []id.OutputKey{"candidate"},
	}
	request := definition.Request{
		Assignment: "role/reviewer",
		Subject: definition.NewSubject(map[string]definition.SubjectValue{
			"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		}),
		Outputs: []id.OutputKey{"decision"},
	}
	pipeline := definition.Pipeline{
		Inputs: []id.InputKey{"repository"},
		Env:    []definition.EnvVar{{Name: "PIPELINE_ENV", Value: "value"}},
		Actions: []definition.Action{
			{Name: "build", Timeout: time.Minute, Container: &container},
			{Name: "review", Timeout: time.Hour, Request: &request},
		},
	}

	if got := pipeline.Actions[0].Container.Image; got != "registry.example.com/tool:1" {
		t.Fatalf("container image = %q", got)
	}
	if got := pipeline.Actions[0].Container.Secrets[0]; got != "TEST_DB_PASSWORD" {
		t.Fatalf("container secret name = %q", got)
	}
	if got := pipeline.Actions[1].Request.Assignment; got != "role/reviewer" {
		t.Fatalf("request assignment = %q", got)
	}
	if got := pipeline.Actions[1].Request.Subject.Len(); got != 1 {
		t.Fatalf("request subject length = %d", got)
	}
	if pipeline.Actions[0].Container == nil || pipeline.Actions[1].Request == nil {
		t.Fatal("action kind pointers lost")
	}
}

func TestSnapshotIsImmutable(t *testing.T) {
	digest := [32]byte{1, 2, 3}
	pipeline := definition.Pipeline{
		Inputs: []id.InputKey{"repository"},
		Env:    []definition.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
		Actions: []definition.Action{{
			Name:      "implement",
			Timeout:   time.Minute,
			Container: &definition.Container{Image: "registry.example.com/tool:1", Commands: []string{"run"}},
		}},
	}
	input := definition.SnapshotInput{
		Pipeline:   pipeline,
		YAML:       []byte("actions:\n- name: implement\n"),
		YAMLDigest: digest,
		Inputs:     map[string]string{"repository": "example/repo"},
		Runtime:    definition.RuntimeRequirements{Capabilities: []string{"container"}},
		Task:       []byte(`{"title":"opaque"}`),
	}
	snapshot := definition.NewSnapshot(input)

	input.YAML[0] = 'X'
	input.Inputs["repository"] = "mutated"
	input.Task[0] = 'X'
	input.Pipeline.Actions[0].Name = "mutated"
	input.Runtime.Capabilities[0] = "mutated"

	if got := string(snapshot.YAML()); got != "actions:\n- name: implement\n" {
		t.Fatalf("snapshot YAML mutated through input: %q", got)
	}
	if got := snapshot.Inputs()["repository"]; got != "example/repo" {
		t.Fatalf("snapshot inputs mutated through input: %q", got)
	}
	if task, ok := snapshot.Task(); !ok || string(task) != `{"title":"opaque"}` {
		t.Fatalf("snapshot task mutated through input: (%q, %v)", task, ok)
	}
	if got := snapshot.Pipeline().Actions[0].Name; got != "implement" {
		t.Fatalf("snapshot pipeline mutated through input: %q", got)
	}
	if got := snapshot.Runtime().Capabilities[0]; got != "container" {
		t.Fatalf("snapshot runtime mutated through input: %q", got)
	}
	if got := snapshot.YAMLDigest(); got != digest {
		t.Fatalf("snapshot digest = %v, want %v", got, digest)
	}

	// Accessors must also hand out copies, not internal storage.
	snapshot.YAML()[0] = 'Y'
	snapshot.Inputs()["repository"] = "mutated-again"
	task, _ := snapshot.Task()
	task[0] = 'Y'
	snapshot.Pipeline().Actions[0].Name = "mutated-again"
	snapshot.Runtime().Capabilities[0] = "mutated-again"

	if got := string(snapshot.YAML()); got != "actions:\n- name: implement\n" {
		t.Fatalf("snapshot YAML mutated through accessor: %q", got)
	}
	if got := snapshot.Inputs()["repository"]; got != "example/repo" {
		t.Fatalf("snapshot inputs mutated through accessor: %q", got)
	}
	if task, _ := snapshot.Task(); string(task) != `{"title":"opaque"}` {
		t.Fatalf("snapshot task mutated through accessor: %q", task)
	}
	if got := snapshot.Pipeline().Actions[0].Name; got != "implement" {
		t.Fatalf("snapshot pipeline mutated through accessor: %q", got)
	}
	if got := snapshot.Runtime().Capabilities[0]; got != "container" {
		t.Fatalf("snapshot runtime mutated through accessor: %q", got)
	}
}

// TestSnapshotPresenceSurvivesKernelClone pins that the Kernel's defensive
// snapshot copy (testkit.Seed/Load) preserves the nil-vs-empty task distinction
// instead of turning a present-empty task into a task-free execution.
func TestSnapshotPresenceSurvivesKernelClone(t *testing.T) {
	cases := []struct {
		name    string
		task    []byte
		present bool
		taskID  id.TaskID
	}{
		{name: "task-free", task: nil, present: false, taskID: ""},
		{name: "empty-task", task: []byte{}, present: true, taskID: "task-empty"},
		{name: "opaque-task", task: []byte(`{"title":"opaque"}`), present: true, taskID: "task-1"},
	}
	for index, testCase := range cases {
		executionID := id.ExecutionID(fmt.Sprintf("exec-clone-%d", index))
		execution := kernel.Execution{
			ID:          executionID,
			WorkspaceID: "ws-clone",
			Snapshot: definition.NewSnapshot(definition.SnapshotInput{
				YAML:   []byte("actions:\n- name: implement\n"),
				Task:   testCase.task,
				TaskID: testCase.taskID,
			}),
		}
		store := testkit.NewStore()
		store.Seed(execution)

		loaded, ok := store.Execution(executionID)
		if !ok {
			t.Fatalf("%s: seeded execution is missing from the store", testCase.name)
		}
		task, present := loaded.Snapshot.Task()
		if present != testCase.present {
			t.Fatalf("%s: cloned task presence = %v, want %v", testCase.name, present, testCase.present)
		}
		if testCase.task == nil {
			if task != nil {
				t.Fatalf("%s: cloned absent task = %#v, want nil", testCase.name, task)
			}
		} else if task == nil {
			t.Fatalf("%s: cloned present task collapsed to nil", testCase.name)
		}
		if string(task) != string(testCase.task) {
			t.Fatalf("%s: cloned task bytes = %q, want %q", testCase.name, task, testCase.task)
		}
		if got := loaded.Snapshot.TaskID(); got != testCase.taskID {
			t.Fatalf("%s: cloned task id = %q, want %q", testCase.name, got, testCase.taskID)
		}
	}
}

func TestSubjectIsImmutable(t *testing.T) {
	fields := map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"candidate": {
			Kind:      definition.SubjectReference,
			Reference: definition.Reference{Action: "implement", Key: "candidate"},
		},
	}
	subject := definition.NewSubject(fields)
	if subject.Len() != 2 {
		t.Fatalf("subject.Len() = %d, want 2", subject.Len())
	}

	fields["operation"] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: "mutated"}
	fields["extra"] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: "extra"}
	if subject.Len() != 2 {
		t.Fatalf("subject mutated through input map: Len() = %d", subject.Len())
	}
	if got := subject.Fields()["operation"].Literal; got != "git.ref.update" {
		t.Fatalf("subject mutated through input map: operation = %q", got)
	}

	subject.Fields()["operation"] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: "mutated-again"}
	if got := subject.Fields()["operation"].Literal; got != "git.ref.update" {
		t.Fatalf("subject mutated through accessor: operation = %q", got)
	}
}

func TestOutputChangeVocabulary(t *testing.T) {
	want := []flow.OutputAction{flow.OutputNone, flow.OutputStage, flow.OutputPromote, flow.OutputDiscard}
	got := flow.OutputActions()
	if len(got) != len(want) {
		t.Fatalf("OutputActions() length = %d, want %d", len(got), len(want))
	}
	for index, action := range want {
		if got[index] != action {
			t.Fatalf("OutputActions()[%d] = %q, want %q", index, got[index], action)
		}
		if !action.Valid() {
			t.Fatalf("OutputAction %q is not valid", action)
		}
	}
	if flow.OutputAction("replace").Valid() {
		t.Fatal("OutputAction replace is valid; replacements must be refused")
	}
}

func TestFactsAreDistinctAndPure(t *testing.T) {
	facts := []flow.Fact{
		flow.ActivateFact{ExecutionID: "exec-1", ActionID: "act-1", At: time.Unix(0, 0)},
		flow.ObserveFact{ExecutionID: "exec-1", ActionID: "act-1", Observation: kernel.RunningObservation()},
		flow.OutputFact{ExecutionID: "exec-1", ActionID: "act-1", Outputs: kernel.Outputs{"candidate": "abc"}},
		flow.AnswerFact{ExecutionID: "exec-1", ActionID: "act-1", Answer: kernel.Answer{Decision: kernel.DecisionPass}},
		flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested},
		flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-1"},
		flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1"},
		flow.RetiredFact{ExecutionID: "exec-1", ActionID: "act-1", WorkloadID: "wl-1"},
	}

	seen := make(map[flow.FactKind]bool)
	for _, fact := range facts {
		kind := fact.Kind()
		if !kind.Valid() {
			t.Fatalf("%T.Kind() = %q is not a valid fact kind", fact, kind)
		}
		if seen[kind] {
			t.Fatalf("fact kind %q is produced by more than one fact type", kind)
		}
		seen[kind] = true
	}
	if len(seen) != len(flow.FactKinds()) {
		t.Fatalf("distinct fact kinds = %d, FactKinds() = %d", len(seen), len(flow.FactKinds()))
	}
	if !flow.CancelUpstreamFailed.Valid() {
		t.Fatal("CancelUpstreamFailed is not a valid cancel reason")
	}
}

func TestFactKindVocabularyIsExact(t *testing.T) {
	want := []flow.FactKind{
		flow.FactActivate,
		flow.FactObserve,
		flow.FactOutput,
		flow.FactAnswer,
		flow.FactCancel,
		flow.FactExpire,
		flow.FactWorkspaceLost,
		flow.FactRetired,
	}
	got := flow.FactKinds()
	if len(got) != len(want) {
		t.Fatalf("FactKinds() length = %d, want %d", len(got), len(want))
	}
	for index, kind := range want {
		if got[index] != kind {
			t.Fatalf("FactKinds()[%d] = %q, want %q", index, got[index], kind)
		}
		if !kind.Valid() {
			t.Fatalf("FactKind %q is not valid", kind)
		}
	}
	if flow.FactKind("remove").Valid() {
		t.Fatal("FactKind remove is valid; removal confirmation is reported as retired")
	}
}

// ---------------------------------------------------------------------------
// Port shape
// ---------------------------------------------------------------------------

func TestStatePortCommitsOneCompleteTransitionPayload(t *testing.T) {
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
	})
	deadline := time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {ID: "exec-1", State: kernel.ExecutionRunning, Version: 7},
	}}

	answer := &kernel.Answer{
		Decision:   kernel.DecisionPass,
		Outputs:    kernel.Outputs{"decision": "pass"},
		Actor:      "principal-1",
		At:         time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		Provenance: []byte("signature"),
	}
	observation := kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))
	stopEvidence := kernel.ConfirmedStop("termination observed")
	cleanupUpdate := &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkloadID:  "wl-1",
		WorkspaceID: "ws-1",
		Attempts:    2,
		LastError:   "runtime unavailable",
	}
	failure := &flow.Failure{Kind: kernel.FailureTimeout, Reason: "deadline_expired", Detail: "deadline reached"}
	launch := &kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchIssued, IdempotencyKey: "start-wl-1"}

	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 7,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-2",
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output: flow.OutputChange{
			Action: flow.OutputStage,
			Values: kernel.Outputs{"candidate": "abc123"},
		},
		InvalidateCredential: true,
		Stop:                 &flow.StopIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1"},
		StopEvidence:         &stopEvidence,
		Cleanup:              &flow.CleanupIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1", Reason: "cancelled"},
		CleanupUpdate:        cleanupUpdate,
		Subject:              &subject,
		Answer:               answer,
		Observation:          &observation,
		Launch:               launch,
		Failure:              failure,
		Deadline:             deadline,
		At:                   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	updated, err := state.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if updated.Version != 8 {
		t.Fatalf("updated version = %d, want 8", updated.Version)
	}
	if len(state.commits) != 1 {
		t.Fatalf("state received %d commits, want exactly 1 atomic transition", len(state.commits))
	}

	got := state.commits[0]
	if got.ExecutionID != "exec-1" || got.ExpectedExecutionVersion != 7 {
		t.Fatalf("commit identity/version lost: %+v", got)
	}
	if got.NextExecutionState != kernel.ExecutionRunning || got.NextActionState != kernel.ActionRunning {
		t.Fatalf("commit next states lost: %+v", got)
	}
	if got.Output.Action != flow.OutputStage || got.Output.Values["candidate"] != "abc123" {
		t.Fatalf("commit output staging lost: %+v", got.Output)
	}
	if !got.InvalidateCredential {
		t.Fatal("commit credential-invalidation intent lost")
	}
	if got.Stop == nil || got.Stop.WorkloadID != "wl-1" || got.Stop.WorkspaceID != "ws-1" {
		t.Fatalf("commit stop intent lost: %+v", got.Stop)
	}
	if got.Cleanup == nil || got.Cleanup.WorkloadID != "wl-1" || got.Cleanup.Reason != "cancelled" {
		t.Fatalf("commit cleanup intent lost: %+v", got.Cleanup)
	}
	if got.Subject == nil || got.Subject.Len() != 1 {
		t.Fatalf("commit subject lost: %+v", got.Subject)
	}
	if !got.Deadline.Equal(deadline) {
		t.Fatalf("commit deadline = %v, want %v", got.Deadline, deadline)
	}
	if got.StopEvidence == nil || !got.StopEvidence.IsConfirmed() {
		t.Fatalf("commit stop evidence lost: %+v", got.StopEvidence)
	}
	if got.CleanupUpdate == nil || got.CleanupUpdate.State != kernel.CleanupPending || got.CleanupUpdate.Attempts != 2 || got.CleanupUpdate.LastError == "" {
		t.Fatalf("commit cleanup progress lost: %+v", got.CleanupUpdate)
	}
	if got.Answer == nil || got.Answer.Decision != kernel.DecisionPass || got.Answer.Actor != "principal-1" {
		t.Fatalf("commit answer lost: %+v", got.Answer)
	}
	if got.Observation == nil || got.Observation.Kind != kernel.ObservationExited || !got.Observation.StopConfirmed() {
		t.Fatalf("commit observation lost: %+v", got.Observation)
	}
	if got.Launch == nil || got.Launch.State != kernel.LaunchIssued || got.Launch.IdempotencyKey != "start-wl-1" {
		t.Fatalf("commit launch intent lost: %+v", got.Launch)
	}
	if got.Failure == nil || got.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("commit failure diagnostic lost: %+v", got.Failure)
	}
}

func TestCommitCancelsPendingActionsAtomically(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionRunning,
			Version:       3,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{
				{ID: "act-1", Index: 0, Name: "build", Kind: definition.ActionKindContainer, State: kernel.ActionRunning, Version: 2},
				{ID: "act-2", Index: 1, Name: "test", Kind: definition.ActionKindContainer, State: kernel.ActionPending, Version: 1},
				{ID: "act-3", Index: 2, Name: "review", Kind: definition.ActionKindRequest, State: kernel.ActionPending, Version: 1},
			},
		},
	}}
	noActiveAction := -1
	failure := &flow.Failure{Kind: kernel.FailureUpstreamFailed, Reason: "action_failed", Detail: "build failed"}

	_, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 3,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 "act-1",
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionFailed,
		PendingCancellations: []flow.ActionUpdate{
			{ActionID: "act-2", ExpectedVersion: 1, NextState: kernel.ActionCancelled},
			{ActionID: "act-3", ExpectedVersion: 1, NextState: kernel.ActionCancelled},
		},
		NextCurrentAction: &noActiveAction,
		Failure:           failure,
		At:                at,
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if len(state.commits) != 1 {
		t.Fatalf("state received %d commits, want exactly 1 atomic transition", len(state.commits))
	}

	reloaded, err := state.Load(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if reloaded.State != kernel.ExecutionFailed {
		t.Fatalf("reloaded execution state = %q, want failed", reloaded.State)
	}
	if reloaded.CurrentAction != -1 {
		t.Fatalf("reloaded current action = %d, want -1 (no active action)", reloaded.CurrentAction)
	}
	if len(reloaded.Actions) != 3 {
		t.Fatalf("reloaded actions = %d, want 3", len(reloaded.Actions))
	}
	wantStates := []kernel.ActionState{kernel.ActionFailed, kernel.ActionCancelled, kernel.ActionCancelled}
	wantVersions := []uint64{3, 2, 2}
	for index, want := range wantStates {
		if got := reloaded.Actions[index].State; got != want {
			t.Fatalf("reloaded action %d state = %q, want %q", index, got, want)
		}
		if got := reloaded.Actions[index].Version; got != wantVersions[index] {
			t.Fatalf("reloaded action %d version = %d, want %d", index, got, wantVersions[index])
		}
	}
	if reloaded.Failure == nil || reloaded.Failure.Kind != kernel.FailureUpstreamFailed {
		t.Fatalf("reloaded failure = %+v, want upstream_failed", reloaded.Failure)
	}
	if reloaded.Failure.Reason != "action_failed" || reloaded.Failure.Detail != "build failed" {
		t.Fatalf("reloaded failure lost reason/detail: %+v", reloaded.Failure)
	}
}

func TestStagedOutputNullVsUnsubmitted(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionRunning,
			Version:       1,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{{
				ID:        "act-1",
				Index:     0,
				Name:      "build",
				Kind:      definition.ActionKindContainer,
				State:     kernel.ActionRunning,
				Version:   1,
				Container: &kernel.ContainerAction{Definition: definition.Container{Image: "registry.example.com/tool:1"}},
			}},
		},
	}}
	reload := func() kernel.Action {
		t.Helper()
		execution, err := state.Load(context.Background(), "exec-1")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		return execution.Actions[0]
	}
	commitOutput := func(version uint64, change flow.OutputChange, next kernel.ActionState) {
		t.Helper()
		if _, err := state.Commit(context.Background(), flow.Commit{
			ExecutionID:              "exec-1",
			ExpectedExecutionVersion: version,
			NextExecutionState:       kernel.ExecutionRunning,
			ActionID:                 "act-1",
			ExpectedActionVersion:    version,
			NextActionState:          next,
			Output:                   change,
			At:                       at,
		}); err != nil {
			t.Fatalf("Commit(%q) error = %v", change.Action, err)
		}
	}

	unsubmitted := reload()
	if unsubmitted.StagedOutputsSet {
		t.Fatal("initially unsubmitted action reports staged outputs present")
	}
	if unsubmitted.StagedOutputs != nil {
		t.Fatalf("initially unsubmitted staged outputs = %+v, want nil", unsubmitted.StagedOutputs)
	}

	// Stage a submitted JSON null: presence is set while the map stays nil.
	commitOutput(1, flow.OutputChange{Action: flow.OutputStage, Values: nil}, kernel.ActionRunning)
	stagedNull := reload()
	if !stagedNull.StagedOutputsSet {
		t.Fatal("submitted null did not set staged-output presence")
	}
	if stagedNull.StagedOutputs != nil {
		t.Fatalf("submitted null staged outputs = %+v, want nil", stagedNull.StagedOutputs)
	}

	// Stage a value map: presence and values are both set.
	commitOutput(2, flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"candidate": "abc"}}, kernel.ActionRunning)
	stagedValues := reload()
	if !stagedValues.StagedOutputsSet {
		t.Fatal("staged values did not set staged-output presence")
	}
	if stagedValues.StagedOutputs["candidate"] != "abc" {
		t.Fatalf("staged outputs = %+v", stagedValues.StagedOutputs)
	}

	// Discard clears both presence and values.
	commitOutput(3, flow.OutputChange{Action: flow.OutputDiscard}, kernel.ActionRunning)
	discarded := reload()
	if discarded.StagedOutputsSet {
		t.Fatal("discard left staged-output presence set")
	}
	if discarded.StagedOutputs != nil {
		t.Fatalf("discard left staged outputs = %+v, want nil", discarded.StagedOutputs)
	}

	// Re-stage, then promote: values move to Outputs and presence clears.
	commitOutput(4, flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"candidate": "abc"}}, kernel.ActionRunning)
	promoted, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 5,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    5,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputPromote, Values: kernel.Outputs{"candidate": "abc"}},
		At:                       at,
	})
	if err != nil {
		t.Fatalf("Commit(promote) error = %v", err)
	}
	if promoted.Actions[0].StagedOutputsSet {
		t.Fatal("promote left staged-output presence set")
	}
	if promoted.Actions[0].StagedOutputs != nil {
		t.Fatalf("promote left staged outputs = %+v, want nil", promoted.Actions[0].StagedOutputs)
	}
	if promoted.Actions[0].Outputs["candidate"] != "abc" {
		t.Fatalf("promoted outputs = %+v, want candidate=abc", promoted.Actions[0].Outputs)
	}
}

func TestCommitPersistsRequestAnswers(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name     string
		decision kernel.Decision
		next     kernel.ActionState
		eligible bool
	}{
		{name: "pass", decision: kernel.DecisionPass, next: kernel.ActionSucceeded, eligible: true},
		{name: "fail", decision: kernel.DecisionFail, next: kernel.ActionFailed, eligible: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newRequestState()
			answer := &kernel.Answer{
				Decision:   tc.decision,
				Outputs:    kernel.Outputs{"decision": string(tc.decision)},
				Actor:      "principal-1",
				At:         at,
				Provenance: []byte("signed"),
			}
			updated, err := state.Commit(context.Background(), flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 "act-1",
				ExpectedActionVersion:    1,
				NextActionState:          tc.next,
				Answer:                   answer,
				At:                       at,
			})
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			request := updated.Actions[0].Request
			if request == nil || request.Answer == nil {
				t.Fatalf("request answer was not persisted: %+v", request)
			}
			if request.Answer.Decision != tc.decision {
				t.Fatalf("answer decision = %q, want %q", request.Answer.Decision, tc.decision)
			}
			if request.Answer.Outputs["decision"] != string(tc.decision) {
				t.Fatalf("answer outputs = %+v", request.Answer.Outputs)
			}
			if request.Answer.Actor != "principal-1" || !request.Answer.At.Equal(at) || string(request.Answer.Provenance) != "signed" {
				t.Fatalf("answer metadata lost: %+v", request.Answer)
			}
			if got := request.Answer.DownstreamEligible(); got != tc.eligible {
				t.Fatalf("DownstreamEligible() = %v, want %v", got, tc.eligible)
			}
		})
	}
}

func TestCommitPersistsObservationUncertaintyDistinctly(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name        string
		observation kernel.Observation
	}{
		{name: "unknown", observation: kernel.UnknownObservation()},
		{name: "absent", observation: kernel.AbsentObservation()},
		{name: "never_started", observation: kernel.NeverStarted("runtime confirms no object was created")},
	}
	seen := make(map[kernel.ObservationKind]bool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newContainerState()
			updated, err := state.Commit(context.Background(), flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 "act-1",
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				Observation:              &tc.observation,
				At:                       at,
			})
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			container := updated.Actions[0].Container
			if container == nil {
				t.Fatal("container evidence lost")
			}
			if container.Observation.Kind != tc.observation.Kind {
				t.Fatalf("observation kind = %q, want %q", container.Observation.Kind, tc.observation.Kind)
			}
			if tc.observation.Kind == kernel.ObservationNeverStarted && !container.Observation.ProofOfNonexecution() {
				t.Fatal("never-started proof lost")
			}
			if tc.observation.Kind != kernel.ObservationNeverStarted && container.Observation.ProofOfNonexecution() {
				t.Fatal("non-never-started observation became proof of nonexecution")
			}
			seen[container.Observation.Kind] = true
		})
	}
	if len(seen) != 3 {
		t.Fatalf("distinct persisted observations = %d, want 3", len(seen))
	}
}

func TestCommitPersistsPendingAndConfirmedStopEvidence(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name     string
		evidence kernel.StopEvidence
		confirm  bool
	}{
		{name: "pending", evidence: kernel.PendingStop("stop response lost"), confirm: false},
		{name: "confirmed", evidence: kernel.ConfirmedStop("termination observed"), confirm: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newContainerState()
			updated, err := state.Commit(context.Background(), flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 "act-1",
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				Stop:                     &flow.StopIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1"},
				StopEvidence:             &tc.evidence,
				At:                       at,
			})
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			action := updated.Actions[0]
			if action.Stop.State != tc.evidence.State || action.Stop.Detail != tc.evidence.Detail {
				t.Fatalf("stop evidence = %+v, want %+v", action.Stop, tc.evidence)
			}
			if action.Stop.IsConfirmed() != tc.confirm {
				t.Fatalf("IsConfirmed() = %v, want %v", action.Stop.IsConfirmed(), tc.confirm)
			}
		})
	}
}

func TestCommitPersistsCleanupProgress(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {ID: "exec-1", State: kernel.ExecutionFailed, Version: 1, WorkspaceID: "ws-1"},
	}}

	pending, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionFailed,
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  "wl-1",
			WorkspaceID: "ws-1",
			Attempts:    1,
			LastError:   "runtime unavailable",
		},
		At: at,
	})
	if err != nil {
		t.Fatalf("Commit(pending) error = %v", err)
	}
	if !pending.Cleanup.IsPending() || pending.Cleanup.WorkloadID != "wl-1" || pending.Cleanup.WorkspaceID != "ws-1" {
		t.Fatalf("pending cleanup lost identity: %+v", pending.Cleanup)
	}
	if pending.Cleanup.Attempts != 1 || pending.Cleanup.LastError != "runtime unavailable" {
		t.Fatalf("pending cleanup lost retry attribution: %+v", pending.Cleanup)
	}

	complete, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionFailed,
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupComplete,
			WorkloadID:  "wl-1",
			WorkspaceID: "ws-1",
			Attempts:    2,
			LastError:   "",
		},
		At: at,
	})
	if err != nil {
		t.Fatalf("Commit(complete) error = %v", err)
	}
	if complete.Cleanup.State != kernel.CleanupComplete || complete.Cleanup.IsPending() {
		t.Fatalf("cleanup did not move pending -> complete: %+v", complete.Cleanup)
	}
	if complete.Cleanup.Attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", complete.Cleanup.Attempts)
	}
	if complete.State != kernel.ExecutionFailed {
		t.Fatalf("terminal outcome changed with cleanup: %q", complete.State)
	}
}

func TestCommitPersistsTypedFailure(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	wantKinds := []kernel.FailureKind{
		kernel.FailureTimeout,
		kernel.FailureCancelled,
		kernel.FailurePreparation,
		kernel.FailureWorkspaceLost,
		kernel.FailureUpstreamFailed,
		kernel.FailureRuntime,
	}
	gotKinds := kernel.FailureKinds()
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("FailureKinds() length = %d, want %d", len(gotKinds), len(wantKinds))
	}
	for index, kind := range wantKinds {
		if gotKinds[index] != kind || !kind.Valid() {
			t.Fatalf("failure kind %d = %q valid=%v, want %q", index, gotKinds[index], gotKinds[index].Valid(), kind)
		}
		t.Run(string(kind), func(t *testing.T) {
			state := newContainerState()
			failure := &flow.Failure{Kind: kind, Reason: "reason-" + string(kind), Detail: "detail-" + string(kind)}
			updated, err := state.Commit(context.Background(), flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionFailed,
				ActionID:                 "act-1",
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionFailed,
				Failure:                  failure,
				At:                       at,
			})
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			if updated.Failure == nil {
				t.Fatal("failure diagnostic was not persisted")
			}
			if updated.Failure.Kind != kind {
				t.Fatalf("failure kind = %q, want %q", updated.Failure.Kind, kind)
			}
			if updated.Failure.Reason != "reason-"+string(kind) || updated.Failure.Detail != "detail-"+string(kind) {
				t.Fatalf("failure reason/detail lost: %+v", updated.Failure)
			}
		})
	}
	if kernel.FailureKind("skipped").Valid() {
		t.Fatal("unknown failure kind is valid")
	}
}

func TestCommitPersistsLaunchIntentAtActivation(t *testing.T) {
	activation := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deadline := activation.Add(time.Hour)
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionQueued,
			Version:       1,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{{
				ID:      "act-1",
				Name:    "build",
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionPending,
				Version: 1,
				Container: &kernel.ContainerAction{
					Definition: definition.Container{Image: "registry.example.com/tool:1"},
				},
			}},
		},
	}}

	activationLaunch := &kernel.LaunchIntent{
		WorkloadID:     "wl-1",
		State:          kernel.LaunchNotIssued,
		IdempotencyKey: "start-wl-1",
	}
	activated, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Launch:                   activationLaunch,
		Deadline:                 deadline,
		At:                       activation,
	})
	if err != nil {
		t.Fatalf("Commit(activation) error = %v", err)
	}
	action := activated.Actions[0]
	if action.Container == nil {
		t.Fatal("container state lost at activation")
	}
	if action.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("activation launch state = %q, want not_issued", action.Container.Launch.State)
	}
	if action.Container.WorkloadID != "wl-1" || action.Container.Launch.WorkloadID != "wl-1" {
		t.Fatalf("activation did not persist stable workload identity: %+v", action.Container)
	}
	if action.Container.WorkspaceID != "ws-1" {
		t.Fatalf("activation did not attach the execution workspace: %q", action.Container.WorkspaceID)
	}
	if action.Container.Launch.IdempotencyKey != "start-wl-1" {
		t.Fatalf("activation lost idempotency key: %+v", action.Container.Launch)
	}
	if !action.ActivatedAt.Equal(activation) || !action.Deadline.Equal(deadline) {
		t.Fatalf("activation time/deadline = %v/%v", action.ActivatedAt, action.Deadline)
	}

	issued, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-1",
			State:          kernel.LaunchIssued,
			IdempotencyKey: "start-wl-1",
		},
		At: activation.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("Commit(issued) error = %v", err)
	}
	if got := issued.Actions[0].Container.Launch.State; got != kernel.LaunchIssued {
		t.Fatalf("start-issued launch state = %q, want issued", got)
	}

	unknown, err := state.Commit(context.Background(), flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 3,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    3,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-1",
			State:          kernel.LaunchUnknown,
			IdempotencyKey: "start-wl-1",
		},
		At: activation.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("Commit(unknown) error = %v", err)
	}
	if got := unknown.Actions[0].Container.Launch.State; got != kernel.LaunchUnknown {
		t.Fatalf("start-outcome-unknown launch state = %q, want unknown", got)
	}

	states := kernel.LaunchStates()
	want := []kernel.LaunchState{kernel.LaunchNotIssued, kernel.LaunchIssued, kernel.LaunchUnknown, kernel.LaunchRetiring}
	if len(states) != len(want) {
		t.Fatalf("LaunchStates() length = %d, want %d", len(states), len(want))
	}
	for index, stateValue := range want {
		if states[index] != stateValue || !stateValue.Valid() {
			t.Fatalf("launch state %d = %q valid=%v, want %q", index, states[index], states[index].Valid(), stateValue)
		}
	}
	if got := string(kernel.LaunchRetiring); got != "retiring" {
		t.Fatalf("LaunchRetiring value = %q, want %q", got, "retiring")
	}
	if kernel.LaunchState("pending").Valid() {
		t.Fatal("unknown launch state is valid")
	}
}

func TestCommitPersistsRetireThenReplaceLaunchProtocol(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	proof := "runtime proved no process started"
	state := &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionQueued,
			Version:       1,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{{
				ID:      "act-1",
				Name:    "build",
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionPending,
				Version: 1,
				Container: &kernel.ContainerAction{
					Definition: definition.Container{Image: "registry.example.com/tool:1"},
				},
			}},
		},
	}}
	reload := func() kernel.Execution {
		t.Helper()
		execution, err := state.Load(context.Background(), "exec-1")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		return execution
	}
	apply := func(commit flow.Commit) kernel.Execution {
		t.Helper()
		updated, err := state.Commit(context.Background(), commit)
		if err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		return updated
	}
	containerOf := func(execution kernel.Execution) *kernel.ContainerAction {
		t.Helper()
		if len(execution.Actions) != 1 || execution.Actions[0].Container == nil {
			t.Fatalf("container state lost: %+v", execution.Actions)
		}
		return execution.Actions[0].Container
	}

	activated := apply(flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-1",
			State:          kernel.LaunchNotIssued,
			IdempotencyKey: "start-wl-1",
		},
		At: at,
	})
	if got := containerOf(activated).WorkloadID; got != "wl-1" {
		t.Fatalf("activation workload = %q, want wl-1", got)
	}

	issued := apply(flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-1",
			State:          kernel.LaunchIssued,
			IdempotencyKey: "start-wl-1",
		},
		At: at.Add(time.Second),
	})
	if got := containerOf(issued).Launch.State; got != kernel.LaunchIssued {
		t.Fatalf("issued launch state = %q, want issued", got)
	}

	// The never-started proof and the retirement intent are one commit, written
	// before Remove(A) so a crash after the removal effect still names A and
	// records why it is owed removal.
	neverStarted := kernel.NeverStarted(proof)
	retiring := apply(flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 3,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    3,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-1",
			State:          kernel.LaunchRetiring,
			IdempotencyKey: "retire-wl-1",
		},
		Observation: &neverStarted,
		At:          at.Add(2 * time.Second),
	})
	if got := containerOf(retiring).Launch.State; got != kernel.LaunchRetiring {
		t.Fatalf("retiring launch state = %q, want retiring", got)
	}

	persisted := reload()
	if persisted.State != kernel.ExecutionRunning {
		t.Fatalf("retiring commit changed the execution state to %q", persisted.State)
	}
	if persisted.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("retiring commit changed the action state to %q", persisted.Actions[0].State)
	}
	retiringContainer := containerOf(persisted)
	if retiringContainer.WorkloadID != "wl-1" || retiringContainer.Launch.WorkloadID != "wl-1" {
		t.Fatalf("retiring commit lost the retiring identity: %+v", retiringContainer)
	}
	if retiringContainer.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("reloaded retiring state = %q, want retiring", retiringContainer.Launch.State)
	}
	if retiringContainer.Launch.IdempotencyKey != "retire-wl-1" {
		t.Fatalf("reloaded retiring idempotency key = %q, want retire-wl-1", retiringContainer.Launch.IdempotencyKey)
	}
	if retiringContainer.Observation.Kind != kernel.ObservationNeverStarted || !retiringContainer.Observation.ProofOfNonexecution() {
		t.Fatalf("retiring commit lost affirmative never-started proof: %+v", retiringContainer.Observation)
	}
	if retiringContainer.Observation.Proof != proof {
		t.Fatalf("retiring proof = %q, want %q", retiringContainer.Observation.Proof, proof)
	}

	// A later commit that only advances cleanup progress carries no observation;
	// it must not erase the proof that justified the retirement.
	apply(flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 4,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    4,
		NextActionState:          kernel.ActionRunning,
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  "wl-1",
			WorkspaceID: "ws-1",
			Attempts:    1,
			LastError:   "removal response lost",
		},
		At: at.Add(3 * time.Second),
	})
	retained := containerOf(reload())
	if retained.Launch.State != kernel.LaunchRetiring || retained.WorkloadID != "wl-1" {
		t.Fatalf("unrelated commit lost the retirement: %+v", retained)
	}
	if !retained.Observation.ProofOfNonexecution() || retained.Observation.Proof != proof {
		t.Fatalf("unrelated commit erased the never-started proof: %+v", retained.Observation)
	}

	// Only after removal is confirmed does the replacement commit name B. B is a
	// fresh identity, and its observation resets to unknown rather than carrying
	// A's never-started proof.
	unknown := kernel.UnknownObservation()
	replaced := apply(flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 5,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 "act-1",
		ExpectedActionVersion:    5,
		NextActionState:          kernel.ActionRunning,
		Launch: &kernel.LaunchIntent{
			WorkloadID:     "wl-2",
			State:          kernel.LaunchNotIssued,
			IdempotencyKey: "start-wl-2",
		},
		Observation: &unknown,
		At:          at.Add(4 * time.Second),
	})
	replacement := containerOf(replaced)
	if replacement.WorkloadID != "wl-2" || replacement.Launch.WorkloadID != "wl-2" {
		t.Fatalf("replacement identity = %+v, want wl-2", replacement)
	}
	if replacement.WorkloadID == "wl-1" {
		t.Fatal("replacement reused the retired workload identity")
	}
	if replacement.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("replacement launch state = %q, want not_issued", replacement.Launch.State)
	}
	if replacement.Launch.IdempotencyKey != "start-wl-2" {
		t.Fatalf("replacement idempotency key = %q, want start-wl-2", replacement.Launch.IdempotencyKey)
	}
	if replacement.Observation.Kind != kernel.ObservationUnknown || replacement.Observation.ProofOfNonexecution() {
		t.Fatalf("replacement observation = %+v, want unknown without proof", replacement.Observation)
	}
}

func newContainerState() *fakeState {
	return &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionRunning,
			Version:       1,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{{
				ID:        "act-1",
				Name:      "build",
				Kind:      definition.ActionKindContainer,
				State:     kernel.ActionRunning,
				Version:   1,
				Container: &kernel.ContainerAction{Definition: definition.Container{Image: "registry.example.com/tool:1"}},
			}},
		},
	}}
}

func newRequestState() *fakeState {
	return &fakeState{loaded: map[id.ExecutionID]kernel.Execution{
		"exec-1": {
			ID:            "exec-1",
			State:         kernel.ExecutionRunning,
			Version:       1,
			WorkspaceID:   "ws-1",
			CurrentAction: 0,
			Actions: []kernel.Action{{
				ID:      "act-1",
				Name:    "review",
				Kind:    definition.ActionKindRequest,
				State:   kernel.ActionRunning,
				Version: 1,
				Request: &kernel.RequestAction{Definition: definition.Request{Assignment: "role/reviewer"}},
			}},
		},
	}}
}

func TestStatePortScansDueAndUnfinishedWork(t *testing.T) {
	state := &fakeState{}
	for _, kind := range []engine.ScanKind{engine.ScanDue, engine.ScanUnfinished} {
		ids, err := state.Scan(context.Background(), engine.ScanRequest{
			Kind:  kind,
			Now:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Limit: 10,
		})
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", kind, err)
		}
		if len(ids) != 1 || ids[0] != "exec-1" {
			t.Fatalf("Scan(%q) = %v", kind, ids)
		}
	}
	if len(state.scans) != 2 {
		t.Fatalf("state received %d scans, want 2", len(state.scans))
	}
	if state.scans[0].Kind != engine.ScanDue || state.scans[1].Kind != engine.ScanUnfinished {
		t.Fatalf("scan kinds = %q, %q", state.scans[0].Kind, state.scans[1].Kind)
	}
}

func TestEffectIdentityCarriesStableWorkloadIdentity(t *testing.T) {
	identity := engine.EffectIdentity{
		ExecutionID:    "exec-1",
		ActionID:       "act-1",
		WorkloadID:     "wl-1",
		IdempotencyKey: "start-wl-1",
	}
	if identity.ExecutionID != "exec-1" || identity.ActionID != "act-1" || identity.WorkloadID != "wl-1" {
		t.Fatalf("effect identity = %+v", identity)
	}
	if identity.IdempotencyKey == "" {
		t.Fatal("effect identity must carry an idempotency key so retries stay attributable")
	}
}

func TestStatePortHasNoSplitSave(t *testing.T) {
	typ := reflect.TypeFor[engine.State]()
	for _, required := range []string{"Load", "Commit", "Scan"} {
		if _, ok := typ.MethodByName(required); !ok {
			t.Fatalf("engine.State is missing %s", required)
		}
	}
	for _, forbidden := range []string{"Save", "Update", "SetState", "Persist"} {
		if _, ok := typ.MethodByName(forbidden); ok {
			t.Fatalf("engine.State exposes %s; a transition must be one compare-and-commit payload", forbidden)
		}
	}
}

func TestRuntimePortMethodSet(t *testing.T) {
	typ := reflect.TypeFor[engine.Runtime]()
	required := []string{
		"Capabilities",
		"Prepare",
		"Start",
		"Observe",
		"Logs",
		"Stop",
		"Remove",
		"RefreshCredential",
	}
	for _, name := range required {
		if _, ok := typ.MethodByName(name); !ok {
			t.Fatalf("engine.Runtime is missing %s", name)
		}
	}
	for _, forbidden := range []string{"Save", "SetState"} {
		if _, ok := typ.MethodByName(forbidden); ok {
			t.Fatalf("engine.Runtime exposes lifecycle state mutation %s", forbidden)
		}
	}
}

func TestBlockingPortsTakeContextFirst(t *testing.T) {
	contextType := reflect.TypeFor[context.Context]()
	interfaces := []struct {
		name string
		typ  reflect.Type
	}{
		{"engine.State", reflect.TypeFor[engine.State]()},
		{"engine.Runtime", reflect.TypeFor[engine.Runtime]()},
		{"engine.Workspace", reflect.TypeFor[engine.Workspace]()},
	}
	for _, item := range interfaces {
		for method := range item.typ.Methods() {
			if method.Type.In(0) != contextType {
				t.Fatalf("%s.%s first parameter = %v, want context.Context", item.name, method.Name, method.Type.In(0))
			}
		}
	}
}

func TestRuntimeFakesRepresentUncertaintyAndStopEvidence(t *testing.T) {
	unknown := &fakeRuntime{
		start:        engine.StartResult{Outcome: engine.StartUnknown, WorkloadID: "wl-1"},
		observations: []kernel.Observation{kernel.UnknownObservation()},
		stops:        []kernel.StopEvidence{kernel.PendingStop("stop response lost")},
	}
	start, err := unknown.Start(context.Background(), engine.StartRequest{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if start.Outcome != engine.StartUnknown {
		t.Fatalf("StartOutcome = %q, want unknown", start.Outcome)
	}
	if start.Outcome == engine.StartProvenNotStarted {
		t.Fatal("unknown start outcome collapsed into proven-not-started")
	}

	observation, err := unknown.Observe(context.Background(), engine.ObserveRequest{})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.ProofOfNonexecution() {
		t.Fatal("unknown observation became proof of nonexecution")
	}

	stop, err := unknown.Stop(context.Background(), engine.StopRequest{})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if stop.IsConfirmed() {
		t.Fatal("pending stop evidence reported as confirmed")
	}

	confirmed := &fakeRuntime{
		start:        engine.StartResult{Outcome: engine.StartProvenNotStarted},
		observations: []kernel.Observation{kernel.NeverStarted("runtime confirms no object existed")},
		stops:        []kernel.StopEvidence{kernel.ConfirmedStop("termination observed")},
	}
	if start, _ := confirmed.Start(context.Background(), engine.StartRequest{}); start.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("proven-not-started outcome = %q", start.Outcome)
	}
	if observation, _ := confirmed.Observe(context.Background(), engine.ObserveRequest{}); !observation.ProofOfNonexecution() {
		t.Fatal("never-started observation lost affirmative proof")
	}
	if stop, _ := confirmed.Stop(context.Background(), engine.StopRequest{}); !stop.IsConfirmed() {
		t.Fatal("confirmed stop evidence lost")
	}
}

func TestWorkspacePortRepresentsMissingWorkspace(t *testing.T) {
	workspace := &fakeWorkspace{available: false}
	status, err := workspace.Status(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Available {
		t.Fatal("missing workspace reported available")
	}
	if status.WorkspaceID != "ws-1" {
		t.Fatalf("workspace identity = %q", status.WorkspaceID)
	}
	if err := workspace.Retain(context.Background(), "ws-1"); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	if err := workspace.Release(context.Background(), "ws-1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if len(workspace.retained) != 1 || len(workspace.released) != 1 {
		t.Fatalf("workspace calls = retained %d released %d", len(workspace.retained), len(workspace.released))
	}
}

func TestClockIsInjectable(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := fakeClock{now: now}
	if got := clock.Now(); !got.Equal(now) {
		t.Fatalf("Now() = %v, want %v", got, now)
	}
}

func TestCapabilitiesAreExplicit(t *testing.T) {
	capabilities := engine.Capabilities{Names: []string{"container", "logs"}}
	if !capabilities.Supports("container") {
		t.Fatal("Supports(container) = false")
	}
	if capabilities.Supports("gpu") {
		t.Fatal("Supports(gpu) = true")
	}
}
