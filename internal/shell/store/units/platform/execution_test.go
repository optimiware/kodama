package platform_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
)

type fakePermission struct {
	permission authorize.ExecutionPermission
	err        error
	facts      platform.ExecutionFacts
	calls      int
}

func (fake *fakePermission) Permission(_ context.Context, facts platform.ExecutionFacts) (authorize.ExecutionPermission, error) {
	fake.calls++
	fake.facts = facts
	return fake.permission, fake.err
}

func newExecutionReader(t *testing.T, s *memoryStoreAlias) (*platform.ExecutionReader, *fakePermission) {
	t.Helper()
	permissions := &fakePermission{permission: authorize.ExecConfiguredTools}
	reader, err := platform.NewExecutionReader(s.Controller(), permissions)
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	return reader, permissions
}

// TestExecutionReaderReturnsCurrentFacts asserts the reader returns the
// execution identity/scope/version/state, the single running container action,
// its deadline and the injected permission.
func TestExecutionReaderReturnsCurrentFacts(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	deadline := fixedNow.Add(30 * time.Minute)
	seedContainerAction(t, s, testScope(), testAction, testExecution, "running", deadline)
	reader, permissions := newExecutionReader(t, s)

	admitted, err := reader.ReadExecution(context.Background(), testExecution)
	if err != nil {
		t.Fatalf("ReadExecution: %v", err)
	}
	if admitted.ExecutionID != testExecution {
		t.Errorf("execution id = %q, want %q", admitted.ExecutionID, testExecution)
	}
	if admitted.Principal.ID != testPrincipal || admitted.Principal.Kind != identity.KindExecution {
		t.Errorf("principal = %#v, want the execution principal", admitted.Principal)
	}
	if admitted.Principal.Scope != testScope() {
		t.Errorf("principal scope = %#v, want %#v", admitted.Principal.Scope, testScope())
	}
	if !admitted.Principal.Enabled {
		t.Error("running execution principal is not enabled")
	}
	if admitted.Principal.Version != 1 {
		t.Errorf("principal version = %d, want the execution version 1", admitted.Principal.Version)
	}
	if admitted.ActionID != testAction {
		t.Errorf("action id = %q, want %q", admitted.ActionID, testAction)
	}
	if !admitted.Deadline.Equal(deadline) {
		t.Errorf("deadline = %v, want %v", admitted.Deadline, deadline)
	}
	if admitted.Permission != authorize.ExecConfiguredTools {
		t.Errorf("permission = %q, want the injected configured_tools", admitted.Permission)
	}
	if permissions.calls != 1 {
		t.Errorf("permission source calls = %d, want 1", permissions.calls)
	}
	if permissions.facts.ActionKind != "container" || permissions.facts.ActionState != "running" {
		t.Errorf("permission facts action = %q/%q, want container/running", permissions.facts.ActionKind, permissions.facts.ActionState)
	}
}

// TestExecutionReaderStoppedExecutionIsDisabled asserts a non-running execution
// is reported disabled so the authorizer refuses it.
func TestExecutionReaderStoppedExecutionIsDisabled(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "succeeded")
	reader, _ := newExecutionReader(t, s)

	admitted, err := reader.ReadExecution(context.Background(), testExecution)
	if err != nil {
		t.Fatalf("ReadExecution: %v", err)
	}
	if admitted.Principal.Enabled {
		t.Error("a succeeded execution principal is enabled")
	}
	if admitted.ActionID != "" {
		t.Errorf("action id = %q, want none", admitted.ActionID)
	}
	if !admitted.Deadline.IsZero() {
		t.Errorf("deadline = %v, want zero", admitted.Deadline)
	}
}

// TestExecutionReaderWithoutRunningContainerAction asserts an execution with no
// running container action still returns its identity with no action/deadline.
func TestExecutionReaderWithoutRunningContainerAction(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedAction(t, s, testScope(), testAction, testExecution, "succeeded", "pass")
	reader, _ := newExecutionReader(t, s)

	admitted, err := reader.ReadExecution(context.Background(), testExecution)
	if err != nil {
		t.Fatalf("ReadExecution: %v", err)
	}
	if admitted.ActionID != "" {
		t.Errorf("action id = %q, want none for a non-container action", admitted.ActionID)
	}
	if !admitted.Deadline.IsZero() {
		t.Errorf("deadline = %v, want zero", admitted.Deadline)
	}
}

// TestExecutionReaderRefusesMultipleRunningContainerActions asserts durable
// corruption is reported instead of picking one action.
func TestExecutionReaderRefusesMultipleRunningContainerActions(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedContainerAction(t, s, testScope(), "action-1", testExecution, "running", fixedNow.Add(time.Hour))
	seedContainerActionAt(t, s, testScope(), "action-2", testExecution, 2, "run-again", "running", fixedNow.Add(time.Hour))
	reader, _ := newExecutionReader(t, s)

	_, err := reader.ReadExecution(context.Background(), testExecution)
	requireClass(t, err, fault.InternalFailure)
}

// TestExecutionReaderMissingExecution asserts an unknown execution is reported
// as not found.
func TestExecutionReaderMissingExecution(t *testing.T) {
	s := newStore()
	reader, _ := newExecutionReader(t, s)
	_, err := reader.ReadExecution(context.Background(), testExecution)
	requireClass(t, err, fault.NotFound)
}

// TestExecutionReaderPropagatesPermissionSourceFailure asserts the injected
// source's error is returned unchanged.
func TestExecutionReaderPropagatesPermissionSourceFailure(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	permissions := &fakePermission{err: fault.New(fault.DependencyUnavailable, "configuration unavailable")}
	reader, err := platform.NewExecutionReader(s.Controller(), permissions)
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	_, err = reader.ReadExecution(context.Background(), testExecution)
	requireClass(t, err, fault.DependencyUnavailable)
}

// TestExecutionReaderReadsAcrossScopes asserts the controller seam resolves an
// execution in another tenant and reports its owning scope.
func TestExecutionReaderReadsAcrossScopes(t *testing.T) {
	s := newStore()
	seedExecution(t, s, otherScope(), testExecution, testPrincipal, "running")
	reader, _ := newExecutionReader(t, s)

	admitted, err := reader.ReadExecution(context.Background(), testExecution)
	if err != nil {
		t.Fatalf("ReadExecution: %v", err)
	}
	if admitted.Principal.Scope != otherScope() {
		t.Errorf("scope = %#v, want the owning %#v", admitted.Principal.Scope, otherScope())
	}
}
