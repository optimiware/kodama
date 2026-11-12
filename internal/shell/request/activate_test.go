package request_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func activationFixture(t *testing.T, stop kernel.StopEvidence) (*testService, tenancy.Scope, kernel.Execution, *definitionSubject) {
	t.Helper()
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update", "resource": "repo-1"})
	container := containerAction("act-0", "build", 0, time.Hour, "candidate")
	container.State = kernel.ActionSucceeded
	container.Version = 2
	container.Stop = stop
	review := requestAction("act-1", "review", 1, 2*time.Hour, "role/reviewer", subject, "approved")
	aggregate := baseExecution(container, review)
	aggregate.State = kernel.ExecutionRunning
	aggregate.CurrentAction = -1
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	return fixture, scope, aggregate, &definitionSubject{subject: subject}
}

type definitionSubject struct{ subject definition.Subject }

func TestActivateBuildsUnitAfterConfirmedStop(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	content := receiptContent(`{"execution_id":"exec-1","action_id":"act-1"}`)

	result, err := fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  content,
	})
	requireNoError(t, err, "Activate")
	if !result.Deadline.Equal(testNow.Add(2 * time.Hour)) {
		t.Fatalf("activation deadline = %v; want %v", result.Deadline, testNow.Add(2*time.Hour))
	}
	if fixture.activations.callCount() != 1 {
		t.Fatalf("activation port calls = %d; want one", fixture.activations.callCount())
	}
	unit := fixture.activations.captured()[0]
	if unit.Owner != scope || unit.ExecutionID != aggregate.ID || unit.ActionID != aggregate.Actions[1].ID {
		t.Fatalf("activation unit target = %+v/%q/%q", unit.Owner, unit.ExecutionID, unit.ActionID)
	}
	if unit.ExpectedExecutionVersion != aggregate.Version || unit.ExpectedActionVersion != aggregate.Actions[1].Version {
		t.Fatalf("activation versions = %d/%d; want %d/%d",
			unit.ExpectedExecutionVersion, unit.ExpectedActionVersion, aggregate.Version, aggregate.Actions[1].Version)
	}
	if unit.Subject.Len() != 2 || unit.Subject.Fields()["operation"].Literal != "git.ref.update" {
		t.Fatalf("activation subject = %+v; want the immutable resolved subject", unit.Subject.Fields())
	}
	if unit.Assignment != assignment {
		t.Fatalf("activation assignment = %+v; want %+v", unit.Assignment, assignment)
	}
	if unit.Timeout != 2*time.Hour {
		t.Fatalf("activation timeout = %v; want 2h", unit.Timeout)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("activation time = %v; want %v", unit.At, testNow)
	}
	if !unit.Deadline.Equal(testNow.Add(2*time.Hour)) || unit.Deadline.Location() != time.UTC {
		t.Fatalf("persisted deadline = %v; want the UTC activation time plus timeout", unit.Deadline)
	}
	if !unit.StopEvidence.IsConfirmed() {
		t.Fatalf("activation stop evidence = %+v; want the earlier confirmed stop", unit.StopEvidence)
	}
	if unit.CredentialUpdate == nil || unit.CredentialUpdate.Present() {
		t.Fatalf("activation credential update = %+v; want an explicit clear", unit.CredentialUpdate)
	}
	if unit.Receipt.ID == "" || unit.Receipt.Key.TenantID != scope.TenantID || unit.Receipt.Key.ProjectID != scope.ProjectID {
		t.Fatalf("activation receipt = %+v; want a scoped receipt", unit.Receipt)
	}
	if !unit.Receipt.At.Equal(testNow) {
		t.Fatalf("activation receipt time = %v; want %v", unit.Receipt.At, testNow)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Actor != testEngineActor || unit.Audit[0].Scope != scope || !unit.Audit[0].At.Equal(testNow) {
		t.Fatalf("activation audit = %+v; want one scoped actor/time record", unit.Audit)
	}
	requireEvent(t, unit.Events, event.TypeActionRequested, scope, string(aggregate.ID), testEngineActor, testNow)
	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)

	runningRead, ok := findAuthority(unit.Authority, authorize.CheckExecutionRunning)
	if !ok {
		t.Fatalf("activation authority = %v; want execution_running", unit.Authority)
	}
	if runningRead.TargetID != string(aggregate.ID) || runningRead.PrincipalID != testEngineActor || runningRead.Operation != "" {
		t.Fatalf("activation execution running read = %+v; want the internal execution target", runningRead)
	}
	versionRead, ok := findAuthority(unit.Authority, authorize.CheckExecutionVersion)
	if !ok {
		t.Fatalf("activation authority = %v; want execution_version", unit.Authority)
	}
	if versionRead.TargetID != string(aggregate.ID) || versionRead.ExpectedVersion != aggregate.Version {
		t.Fatalf("activation execution version read = %+v; want version %d", versionRead, aggregate.Version)
	}
	if len(unit.Authority) != 2 {
		t.Fatalf("activation authority reads = %d; want exactly execution_running and execution_version", len(unit.Authority))
	}
}

func TestActivateRefusesUnconfirmedStop(t *testing.T) {
	cases := map[string]kernel.StopEvidence{
		"unconfirmed": kernel.PendingStop("stop acknowledgement is not confirmation"),
		"absent":      {},
	}
	for name, stop := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, scope, aggregate, subject := activationFixture(t, stop)
			assignment, err := request.ParseAssignment("role/reviewer")
			requireNoError(t, err, "ParseAssignment")
			_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
				Caller:                   testEngineCaller(t),
				Scope:                    scope,
				ExecutionID:              aggregate.ID,
				ActionID:                 aggregate.Actions[1].ID,
				ExpectedExecutionVersion: aggregate.Version,
				ExpectedActionVersion:    aggregate.Actions[1].Version,
				Subject:                  &subject.subject,
				Assignment:               assignment,
				Timeout:                  2 * time.Hour,
				CallerKey:                "activate-key-1",
				Content:                  receiptContent(`{}`),
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if fixture.activations.callCount() != 0 {
				t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
			}
		})
	}
}

func TestActivateRefusesMissingSubject(t *testing.T) {
	fixture, scope, aggregate, _ := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  nil,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesAssignmentMismatch(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	wrong, err := request.ParseAssignment("role/operator")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               wrong,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesStaleVersion(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version + 1,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesNonPendingAction(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	aggregate.Actions[1].State = kernel.ActionRunning
	fixture.store.Seed(scope, aggregate)
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesCrossScopeExecution(t *testing.T) {
	fixture := newTestService(t)
	subject := literalSubject(map[string]string{"operation": "op"})
	aggregate := baseExecution(
		containerAction("act-0", "build", 0, time.Hour),
		requestAction("act-1", "review", 1, time.Hour, "role/reviewer", subject),
	)
	aggregate.State = kernel.ExecutionRunning
	aggregate.CurrentAction = -1
	aggregate.Actions[0].State = kernel.ActionSucceeded
	aggregate.Actions[0].Version = 2
	aggregate.Actions[0].Stop = kernel.ConfirmedStop("termination observed")
	fixture.store.Seed(projectScope("tenant-1", "project-1"), aggregate)
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    projectScope("tenant-2", "project-9"),
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject,
		Assignment:               assignment,
		Timeout:                  time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.NotFound)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesEmptySubject(t *testing.T) {
	fixture, scope, aggregate, _ := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	empty := definition.NewSubject(nil)
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &empty,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateCommandAcceptsNoPublicPrincipal(t *testing.T) {
	commandType := reflect.TypeFor[request.ActivateCommand]()
	principalType := reflect.TypeFor[authorize.Principal]()
	principalIDType := reflect.TypeFor[id.PrincipalID]()
	for field := range commandType.Fields() {
		if field.Type == principalType || field.Type == principalIDType || field.Name == "Actor" {
			t.Fatalf("ActivateCommand exposes public principal field %q", field.Name)
		}
	}
	callerType := reflect.TypeFor[request.EngineCaller]()
	for field := range callerType.Fields() {
		if field.PkgPath == "" {
			t.Fatalf("EngineCaller exposes settable field %q", field.Name)
		}
	}
}

func TestActivateRefusesZeroEngineCaller(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   request.EngineCaller{},
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestNewEngineCallerRejectsEmptyIdentity(t *testing.T) {
	_, err := request.NewEngineCaller("")
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestActivateRefusesCallerSuppliedSubjectMismatch(t *testing.T) {
	fixture, scope, aggregate, _ := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	forged := literalSubject(map[string]string{"operation": "git.ref.delete", "resource": "repo-1"})
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &forged,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateRefusesCallerSuppliedTimeoutMismatch(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  3 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}

func TestActivateDerivesDeadlineFromAdmittedTimeout(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	fixture.clock.now = testNow.Add(5 * time.Minute)
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	result, err := fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireNoError(t, err, "Activate")
	wantDeadline := fixture.clock.now.Add(aggregate.Actions[1].Timeout)
	if !result.Deadline.Equal(wantDeadline) {
		t.Fatalf("activation deadline = %v; want the admitted timeout from activation time %v", result.Deadline, wantDeadline)
	}
	unit := fixture.activations.captured()[0]
	if unit.Timeout != aggregate.Actions[1].Timeout || !unit.Deadline.Equal(wantDeadline) {
		t.Fatalf("activation unit timeout/deadline = %v/%v; want %v/%v",
			unit.Timeout, unit.Deadline, aggregate.Actions[1].Timeout, wantDeadline)
	}
}

func TestActivateDerivesSubjectFromAdmittedAction(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	declared := definition.NewSubject(map[string]definition.SubjectValue{
		"resource": {
			Kind:      definition.SubjectReference,
			Reference: definition.Reference{Input: true, Key: "repo"},
		},
	})
	container := containerAction("act-0", "build", 0, time.Hour, "candidate")
	container.State = kernel.ActionSucceeded
	container.Version = 2
	container.Stop = kernel.ConfirmedStop("termination observed")
	review := requestAction("act-1", "review", 1, 2*time.Hour, "role/reviewer", declared, "approved")
	aggregate := baseExecution(container, review)
	aggregate.State = kernel.ExecutionRunning
	aggregate.CurrentAction = -1
	aggregate.Snapshot = definition.NewSnapshot(definition.SnapshotInput{Inputs: map[string]string{"repo": "repo-42"}})
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	resolved := literalSubject(map[string]string{"resource": "repo-42"})
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &resolved,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireNoError(t, err, "Activate")
	unit := fixture.activations.captured()[0]
	if unit.Subject.Fields()["resource"].Literal != "repo-42" {
		t.Fatalf("activation subject = %+v; want the reducer-resolved input literal", unit.Subject.Fields())
	}
}

func TestActivateRefusesAdmittedEmptySubject(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	container := containerAction("act-0", "build", 0, time.Hour, "candidate")
	container.State = kernel.ActionSucceeded
	container.Version = 2
	container.Stop = kernel.ConfirmedStop("termination observed")
	review := requestAction("act-1", "review", 1, 2*time.Hour, "role/reviewer", definition.NewSubject(nil), "approved")
	aggregate := baseExecution(container, review)
	aggregate.State = kernel.ExecutionRunning
	aggregate.CurrentAction = -1
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")
	empty := definition.NewSubject(nil)
	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &empty,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero", fixture.activations.callCount())
	}
}
