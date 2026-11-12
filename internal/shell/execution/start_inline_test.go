package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// inlineAdmission is the exact task-free inline-YAML admission result: an empty
// pipeline identity, version zero and the immutable admitted snapshot.
func inlineAdmission(t *testing.T, yaml []byte, actions ...pipeline.AdmittedAction) pipeline.AdmissionResult {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml})
	if err != nil {
		t.Fatalf("pipeline.NewSnapshot: %v", err)
	}
	return pipeline.AdmissionResult{
		Snapshot: snapshot,
		Actions:  actions,
	}
}

// TestStartInlineBuildsPipelineLessUnit pins the inline admission commit unit:
// no pipeline identity or version is carried, only the membership authority
// read remains, and the immutable snapshot/actions/receipt are unchanged.
func TestStartInlineBuildsPipelineLessUnit(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer)
	admission := inlineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"),
		admittedContainerAction(0, "build", time.Minute))

	command := execution.StartCommand{
		Actor:              actor,
		Scope:              scope,
		Admission:          admission,
		Initiator:          id.PrincipalID("principal-1"),
		ExecutionPrincipal: id.PrincipalID("principal-execution"),
		CallerKey:          "inline-start-1",
		Content:            receiptContent(`{"yaml_sha256":"sha256:inline","inputs":{}}`),
	}
	result, err := fixture.service.Start(context.Background(), command)
	requireNoError(t, err, "inline Start")
	if result.Replayed {
		t.Fatal("fresh inline start reported a replay")
	}
	if result.PipelineVersion != 0 {
		t.Fatalf("inline result pipeline version = %d; want 0", result.PipelineVersion)
	}
	if fixture.starts.callCount() != 1 {
		t.Fatalf("start port calls = %d; want exactly one", fixture.starts.callCount())
	}
	unit := fixture.starts.captured()[0]
	if unit.PipelineID != "" {
		t.Fatalf("inline start pipeline id = %q; want empty", unit.PipelineID)
	}
	if unit.ExpectedPipelineVersion != 0 {
		t.Fatalf("inline start pipeline version = %d; want 0", unit.ExpectedPipelineVersion)
	}
	if unit.Snapshot.YAMLSHA256 != admission.Snapshot.YAMLSHA256 {
		t.Fatal("inline start snapshot hash differs from the immutable admission snapshot")
	}
	if len(unit.Actions) != 1 || unit.Actions[0].Name != "build" {
		t.Fatalf("inline start actions = %+v; want the admitted ordered action rows", unit.Actions)
	}
	if unit.Initiator != id.PrincipalID("principal-1") || unit.ExecutionPrincipal != id.PrincipalID("principal-execution") {
		t.Fatalf("inline start principals = %q/%q", unit.Initiator, unit.ExecutionPrincipal)
	}
	if result.ExecutionID != unit.NewExecutionID {
		t.Fatalf("inline result execution = %q; want %q", result.ExecutionID, unit.NewExecutionID)
	}

	// Inline admission omits ONLY the pipeline-version authority read. The
	// membership check and the task-free boundary stay.
	requireAuthority(t, unit.Authority, "pipeline.start", authorize.CheckMembership)
	requireNoAuthority(t, unit.Authority, authorize.CheckPipelineVersion)
	requireNoAuthority(t, unit.Authority, authorize.CheckTaskVersion)
	if len(unit.Authority) != 1 {
		t.Fatalf("inline authority reads = %v; want exactly the membership read", unit.Authority)
	}

	requireReceipt(t, unit.Receipt, scope, actor.ID, "pipeline.start", command.CallerKey, command.Content, testNow)
	if unit.Receipt.SnapshotHash == nil || *unit.Receipt.SnapshotHash != admission.Snapshot.YAMLSHA256 {
		t.Fatal("inline receipt does not pin the admitted snapshot hash")
	}
	requireAudit(t, unit.Audit, scope, actor.ID, "pipeline.start", testNow)
	envelope := requireEvent(t, unit.Events, event.TypeExecutionQueued, scope, string(unit.NewExecutionID), actor.ID, testNow)
	if envelope.SubjectVersion != 1 {
		t.Fatalf("queued inline execution event version = %d; want 1", envelope.SubjectVersion)
	}
	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)
	requirePostCommit(t, unit.PostCommit, event.PostDispatch)
}

// TestStartAdmissionSelectionValidation pins the exclusive stored/inline
// selection: a stored admission carries a non-empty id and a positive version;
// an inline admission carries neither; any mismatched pair or an incomplete
// snapshot/action refuses before the start port.
func TestStartAdmissionSelectionValidation(t *testing.T) {
	validAction := admittedContainerAction(0, "build", time.Minute)
	validSnapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n")})
	if err != nil {
		t.Fatalf("pipeline.NewSnapshot: %v", err)
	}
	taskSnapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"),
		TaskID: id.TaskID("task-1"),
	})
	if err != nil {
		t.Fatalf("task-bearing pipeline.NewSnapshot: %v", err)
	}
	emptyAction := validAction
	emptyAction.Name = ""

	cases := []struct {
		name      string
		admission pipeline.AdmissionResult
		allowed   bool
	}{
		{
			name:      "stored pipeline id and positive version",
			admission: pipeline.AdmissionResult{PipelineID: "pipeline-1", PipelineVersion: 3, Snapshot: validSnapshot, Actions: []pipeline.AdmittedAction{validAction}},
			allowed:   true,
		},
		{
			name:      "inline empty id and zero version",
			admission: pipeline.AdmissionResult{Snapshot: validSnapshot, Actions: []pipeline.AdmittedAction{validAction}},
			allowed:   true,
		},
		{
			name:      "stored id with zero version",
			admission: pipeline.AdmissionResult{PipelineID: "pipeline-1", Snapshot: validSnapshot, Actions: []pipeline.AdmittedAction{validAction}},
		},
		{
			name:      "inline empty id with positive version",
			admission: pipeline.AdmissionResult{PipelineVersion: 3, Snapshot: validSnapshot, Actions: []pipeline.AdmittedAction{validAction}},
		},
		{
			name:      "missing admitted snapshot",
			admission: pipeline.AdmissionResult{Actions: []pipeline.AdmittedAction{validAction}},
		},
		{
			name:      "empty admitted action name",
			admission: pipeline.AdmissionResult{Snapshot: validSnapshot, Actions: []pipeline.AdmittedAction{emptyAction}},
		},
		{
			name:      "task-bearing snapshot at the task-free boundary",
			admission: pipeline.AdmissionResult{Snapshot: taskSnapshot, Actions: []pipeline.AdmittedAction{validAction}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestService(t)
			_, err := fixture.service.Start(context.Background(), execution.StartCommand{
				Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
				Scope:     projectScope("tenant-1", "project-1"),
				Admission: testCase.admission,
				CallerKey: "selection-key",
				Content:   receiptContent(`{}`),
			})
			if testCase.allowed {
				requireNoError(t, err, "Start")
				if fixture.starts.callCount() != 1 {
					t.Fatalf("start port calls = %d; want one", fixture.starts.callCount())
				}
				return
			}
			requireFaultClass(t, err, fault.InvalidInput)
			if fixture.starts.callCount() != 0 {
				t.Fatalf("start port calls = %d; want zero after an invalid selection", fixture.starts.callCount())
			}
		})
	}
}

// TestStartInlineAuthorityMatrix proves inline admission skips only the
// pipeline-version authority read. The real operation matrix still decides the
// engineer grant for humans and services, refuses other roles, disabled
// actors, execution principals and foreign scopes, and stored admissions keep
// their pipeline-version pin.
func TestStartInlineAuthorityMatrix(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	admission := inlineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"),
		admittedContainerAction(0, "build", time.Minute))

	disabled := humanActor("tenant-1", "project-1", "principal-disabled", tenancy.RoleEngineer)
	disabled.Enabled = false

	cases := []struct {
		name    string
		actor   authorize.Principal
		allowed bool
	}{
		{name: "human engineer", actor: humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer), allowed: true},
		{name: "service engineer", actor: serviceActor("tenant-1", "project-1", "service-1", tenancy.RoleEngineer), allowed: true},
		{name: "human viewer", actor: humanActor("tenant-1", "project-1", "principal-viewer", tenancy.RoleViewer)},
		{name: "human reviewer", actor: humanActor("tenant-1", "project-1", "principal-reviewer", tenancy.RoleReviewer)},
		{name: "human operator", actor: humanActor("tenant-1", "project-1", "principal-operator", tenancy.RoleOperator)},
		{name: "human admin", actor: humanActor("tenant-1", "project-1", "principal-admin", tenancy.RoleAdmin)},
		{name: "disabled human engineer", actor: disabled},
		{name: "execution principal", actor: executionActor("tenant-1", "project-1", "principal-execution", "execution-1")},
		{name: "foreign project engineer", actor: humanActor("tenant-1", "project-2", "principal-foreign", tenancy.RoleEngineer)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestServiceWithAuthorizer(t, authorize.NewMatrix())
			_, err := fixture.service.Start(context.Background(), execution.StartCommand{
				Actor:     testCase.actor,
				Scope:     scope,
				Admission: admission,
				CallerKey: "matrix-key",
				Content:   receiptContent(`{}`),
			})
			if testCase.allowed {
				requireNoError(t, err, "inline Start")
				if fixture.starts.callCount() != 1 {
					t.Fatalf("start port calls = %d; want one", fixture.starts.callCount())
				}
				unit := fixture.starts.captured()[0]
				requireAuthority(t, unit.Authority, "pipeline.start", authorize.CheckMembership)
				requireNoAuthority(t, unit.Authority, authorize.CheckPipelineVersion)
				return
			}
			requireFaultClassIn(t, err, fault.Forbidden, fault.InvalidInput)
			if fixture.starts.callCount() != 0 {
				t.Fatalf("start port calls = %d; want zero after an authority refusal", fixture.starts.callCount())
			}
		})
	}

	t.Run("stored admission keeps the pipeline version pin", func(t *testing.T) {
		fixture := newTestServiceWithAuthorizer(t, authorize.NewMatrix())
		_, err := fixture.service.Start(context.Background(), execution.StartCommand{
			Actor:     humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
			Scope:     scope,
			Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 3, admittedContainerAction(0, "build", time.Minute)),
			CallerKey: "stored-matrix-key",
			Content:   receiptContent(`{}`),
		})
		requireNoError(t, err, "stored Start")
		unit := fixture.starts.captured()[0]
		read := requireAuthority(t, unit.Authority, "pipeline.start", authorize.CheckPipelineVersion)
		if read.TargetID != "pipeline-1" || read.ExpectedVersion != 3 {
			t.Fatalf("pipeline version read = %+v; want pipeline-1 at version 3", read)
		}
		if unit.ExpectedPipelineVersion != 3 {
			t.Fatalf("stored unit pipeline version = %d; want 3", unit.ExpectedPipelineVersion)
		}
	})
}

// newTestServiceWithAuthorizer builds the same fake-backed service as
// newTestService but with an explicit authorizer, so the real operation matrix
// can decide the grant.
func newTestServiceWithAuthorizer(t *testing.T, authorizer authorize.Authorizer) *testService {
	t.Helper()
	store := newFakeStore()
	reader := &fakeReader{}
	transitions := &fakeTransitionPort{store: store}
	starts := newFakeStartPort()
	authority := &fakeAuthorityReader{}
	clock := fixedClock{now: testNow}
	ids := &sequentialIDs{}
	service, err := execution.NewService(execution.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   authority,
		Reader:      reader,
		State:       store,
		Transitions: transitions,
		Starts:      starts,
	})
	if err != nil {
		t.Fatalf("execution.NewService: %v", err)
	}
	return &testService{
		service:     service,
		clock:       clock,
		ids:         ids,
		authorizer:  nil,
		authority:   authority,
		store:       store,
		reader:      reader,
		transitions: transitions,
		starts:      starts,
	}
}
