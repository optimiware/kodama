// Execution authority reads: an execution caller's current-authority reads name
// the exact admitted execution and action, so a reader keyed on TargetID can
// evaluate whether that execution is running and whether that action's deadline
// still holds before any external effect.
package mcpproxy_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// readFor returns the first read for check, or nil.
func readFor(reads []authorize.AuthorityRead, check authorize.AuthorityCheck) *authorize.AuthorityRead {
	for index := range reads {
		if reads[index].Check == check {
			return &reads[index]
		}
	}
	return nil
}

// keyedAuthority answers like a real reader keyed on TargetID: the execution
// fact holds only for the one running execution and the deadline fact only for
// the active action. An empty or foreign target is denied.
func keyedAuthority(running id.ExecutionID, action id.ActionID) func(int, []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	return func(_ int, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := make([]authorize.AuthorityFact, 0, len(reads))
		for _, read := range reads {
			allowed := true
			switch read.Check {
			case authorize.CheckExecutionRunning:
				allowed = read.TargetID == string(running)
			case authorize.CheckActionDeadline:
				allowed = read.TargetID == string(action)
			}
			facts = append(facts, authorize.AuthorityFact{
				Check:       read.Check,
				Scope:       read.Scope,
				PrincipalID: read.PrincipalID,
				TargetID:    read.TargetID,
				Allowed:     allowed,
			})
		}
		return facts, nil
	}
}

// TestExecutionAuthorityReadsBindAdmittedTargets proves both the initial
// authorization and the pre-transport refresh carry the execution and action
// identity a target-keyed reader needs.
func TestExecutionAuthorityReadsBindAdmittedTargets(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)

	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	calls := fake.authority.readCalls()
	// The initial Call authorization and the refresh immediately before the
	// transport.
	if len(calls) != 2 {
		t.Fatalf("authority read calls = %d; want 2 (call, pre-transport refresh)", len(calls))
	}
	for index, reads := range calls {
		running := readFor(reads, authorize.CheckExecutionRunning)
		if running == nil {
			t.Fatalf("authority read call %d has no execution_running read: %+v", index, reads)
		}
		if running.TargetID != string(testExecution) {
			t.Fatalf("authority read call %d execution_running target = %q; want %q", index, running.TargetID, testExecution)
		}
		deadline := readFor(reads, authorize.CheckActionDeadline)
		if deadline == nil {
			t.Fatalf("authority read call %d has no action_deadline read: %+v", index, reads)
		}
		if deadline.TargetID != string(testAction) {
			t.Fatalf("authority read call %d action_deadline target = %q; want %q", index, deadline.TargetID, testAction)
		}
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1", fake.transport.callCount())
	}
}

// TestExecutionAuthorityKeyedReaderAcceptsExactTarget proves the bound targets
// satisfy a reader that only allows the actual running execution and action.
func TestExecutionAuthorityKeyedReaderAcceptsExactTarget(t *testing.T) {
	fake := newFakes()
	fake.authority.onRead = keyedAuthority(testExecution, testAction)
	proxy := fake.newProxy(t)

	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call refused the admitted running execution: %v", err)
	}
	if fake.transport.callCount() != 1 {
		t.Fatalf("transport calls = %d; want 1", fake.transport.callCount())
	}
}

// TestExecutionAuthorityKeyedReaderDeniesAnotherExecution proves the read names
// the admitted execution: when another execution is the running one, the fact is
// denied and no upstream call is made.
func TestExecutionAuthorityKeyedReaderDeniesAnotherExecution(t *testing.T) {
	fake := newFakes()
	// The admitted context names exec-2 while only exec-1 is running.
	other := admittedExecution()
	other.ExecutionID = id.ExecutionID("exec-2")
	other.Principal.ExecutionID = id.ExecutionID("exec-2")
	fake.executions.setAdmitted(other)
	principal := executionPrincipal()
	principal.ExecutionID = id.ExecutionID("exec-2")
	fake.credentials.setPrincipal(principal)
	fake.authority.onRead = keyedAuthority(testExecution, testAction)
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted an execution that is not the running execution")
	}
	assertFaultClass(t, err, fault.Forbidden)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("stopped execution returned a result: %+v", result)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 before any external effect", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

// TestExecutionReaderCannotSubstituteAnotherPermission proves the proxy's
// admitted permission is bounded by the trusted operation registry: a reader
// that derives a different (including a wider model) permission than
// tool.invoke requires is refused before any secret, usage row or transport
// call. No execution permission is ever read from a caller field or column.
func TestExecutionReaderCannotSubstituteAnotherPermission(t *testing.T) {
	for _, permission := range []authorize.ExecutionPermission{
		authorize.ExecNone,
		authorize.ExecOwnTaskContent,
		authorize.ExecConfiguredModels,
	} {
		t.Run(string(permission), func(t *testing.T) {
			fake := newFakes()
			admitted := admittedExecution()
			admitted.Permission = permission
			fake.executions.setAdmitted(admitted)
			proxy := fake.newProxy(t)

			result, err := proxy.Call(context.Background(), readCallRequest())
			if err == nil {
				t.Fatalf("Call accepted admitted permission %q for tool.invoke", permission)
			}
			assertFaultClass(t, err, fault.Forbidden)
			if len(result.Content) != 0 || result.IsError {
				t.Fatalf("substituted permission returned a result: %+v", result)
			}
			if fake.authority.callCount() != 0 {
				t.Fatalf("authority reads = %d; want 0 before the grant check", fake.authority.callCount())
			}
			if fake.resolver.callCount() != 0 {
				t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
			}
			if fake.transport.callCount() != 0 {
				t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
			}
			if fake.store.callCount() != 0 {
				t.Fatalf("usage writes = %d; want 0", fake.store.callCount())
			}
		})
	}
}

// TestExecutionReaderForeignScopeDenied proves the current execution's owning
// scope, not the presented principal, must cover the configured proxy scope.
// The reader facts carry the scope; a foreign execution is refused before any
// disclosure or effect.
func TestExecutionReaderForeignScopeDenied(t *testing.T) {
	fake := newFakes()
	admitted := admittedExecution()
	admitted.Principal.Scope = otherScope()
	fake.executions.setAdmitted(admitted)
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted an execution in another project")
	}
	assertFaultClass(t, err, fault.Forbidden)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("foreign-scope execution returned a result: %+v", result)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

// TestExecutionWithoutActionIDRefused pins that an admitted execution with no
// action identity cannot satisfy the registry's action-deadline fact and is
// refused before any authority read or external effect.
func TestExecutionWithoutActionIDRefused(t *testing.T) {
	fake := newFakes()
	admitted := admittedExecution()
	admitted.ActionID = ""
	fake.executions.setAdmitted(admitted)
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted an execution with no action identity")
	}
	assertFaultClass(t, err, fault.Forbidden)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("execution without an action returned a result: %+v", result)
	}
	if fake.authority.callCount() != 0 {
		t.Fatalf("authority reads = %d; want 0 without an action target", fake.authority.callCount())
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}
