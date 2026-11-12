package identity_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// executionCredentialUnit builds one complete execution credential mutation
// unit whose current-authority reads are the supplied execution-principal
// reads. It is the smallest commit unit that exercises the adapter's
// commit-boundary lock path for an execution actor.
func (h *harness) executionCredentialUnit(scope tenancy.Scope, principal, execution string, expected uint64, authority []authorize.AuthorityRead) token.MutationUnit {
	h.t.Helper()
	return token.MutationUnit{
		Owner:           scope,
		PrincipalID:     id.PrincipalID(principal),
		ExecutionID:     id.ExecutionID(execution),
		Kind:            token.MutationIssue,
		ExpectedVersion: expected,
		Credential:      token.Credential{Nonce: testNonce(0x5a), Expiry: testExpiry},
		Receipt:         h.receipt(scope, id.PrincipalID(principal), "event.publish", "caller-exec-"+execution, token.MutationResult{State: token.StateCurrent, Version: expected + 1}),
		Audit:           []event.Audit{h.audit(scope, id.PrincipalID(principal), "event.publish")},
		Events:          []event.Envelope{h.envelope(scope, id.PrincipalID(principal), execution, expected+1)},
		Authority:       authority,
	}
}

// executionOperationReads builds the current-authority reads one real
// capability emits for an execution principal: the registry's membership check
// plus, where the operation declares it, the execution_running fact for the
// principal's execution row. Versioned and action-deadline checks are omitted
// because they name a separate target row; the membership grant is the fact
// this test pins.
func executionOperationReads(t *testing.T, operation authorize.OperationName, scope tenancy.Scope, principal id.PrincipalID, execution id.ExecutionID) []authorize.AuthorityRead {
	t.Helper()
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("operation %q is not registered", operation)
	}
	reads := make([]authorize.AuthorityRead, 0, 2)
	for _, check := range registered.Authority {
		if check != authorize.CheckMembership && check != authorize.CheckExecutionRunning {
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   registered.Name,
			Required:    registered.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
		if check == authorize.CheckExecutionRunning {
			read.TargetID = string(execution)
		}
		reads = append(reads, read)
	}
	return reads
}

// executionMembershipRead builds the registry's bare membership read for one
// operation, matching what task.create/task.read/task.edit emit.
func executionMembershipRead(t *testing.T, operation authorize.OperationName, scope tenancy.Scope, principal id.PrincipalID) authorize.AuthorityRead {
	t.Helper()
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("operation %q is not registered", operation)
	}
	return authorize.AuthorityRead{
		Operation:   registered.Name,
		Required:    registered.Requires,
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}
}

// TestAuthorityReaderExecutionPrincipalHoldsRegistryGrant is a regression
// test: the real services emit a membership read for an EXECUTION
// principal (event.publish, model.invoke, tool.invoke, task.create/read/edit).
// The registry's execution permission, not a membership row, satisfies that
// read while the principal resolves to a running execution row in the same
// scope, so authorize.Confirm must accept it instead of turning it into
// "current authority check membership did not hold".
func TestAuthorityReaderExecutionPrincipalHoldsRegistryGrant(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	operations := []authorize.OperationName{
		"task.create",
		"task.read",
		"task.edit",
		"event.publish",
		"model.invoke",
		"tool.invoke",
	}
	for _, operation := range operations {
		t.Run(string(operation), func(t *testing.T) {
			reads := executionOperationReads(t, operation, scope, "principal-exec", "execution-a")
			if len(reads) == 0 {
				t.Fatalf("operation %q declares no membership read", operation)
			}
			facts, err := h.adapter.Read(context.Background(), reads)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			for index, fact := range facts {
				if !fact.Allowed {
					t.Fatalf("fact %d (%s) = %+v; want allowed for a running execution principal", index, fact.Check, fact)
				}
			}
			if err := authorize.Confirm(reads, facts); err != nil {
				t.Fatalf("Confirm: %v; want the running execution grant to hold", err)
			}
		})
	}
}

// TestAuthorityReaderExecutionPrincipalWithoutPermissionDenied proves the
// registry execution permission gates the membership fact: an operation that
// declares ExecNone (task.start) refuses every execution principal.
func TestAuthorityReaderExecutionPrincipalWithoutPermissionDenied(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	reads := []authorize.AuthorityRead{executionMembershipRead(t, "task.start", scope, "principal-exec")}
	if registered, _ := authorize.LookupOperation("task.start"); registered.Requires.Execution != authorize.ExecNone {
		t.Fatalf("task.start execution grant = %q; want ExecNone", registered.Requires.Execution)
	}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("fact = %+v; want denied because task.start grants no execution permission", facts[0])
	}
	requireFaultClass(t, authorize.Confirm(reads, facts), fault.Forbidden)
}

// TestAuthorityReaderExecutionPrincipalUnsetGrantDenied proves an authority
// read that carries no registry grant context cannot authorize an execution
// principal. An unset ExecutionPermission is not ExecNone and is not a declared
// permission either, so it is refused fail-closed rather than treated as a
// grant.
func TestAuthorityReaderExecutionPrincipalUnsetGrantDenied(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	reads := []authorize.AuthorityRead{{
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: "principal-exec",
	}}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("fact = %+v; want denied because the read carries no registry execution permission", facts[0])
	}
}

// TestAuthorityReaderExecutionPrincipalStoppedDenied proves a non-running
// execution principal holds no current grant even for an operation that grants
// an execution permission.
func TestAuthorityReaderExecutionPrincipalStoppedDenied(t *testing.T) {
	states := []string{"queued", "succeeded", "failed", "cancelled"}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			scope := projectScope("tenant-a", "project-a")
			h.seedExecution(scope, "execution-a", "principal-exec", 1, state, nil, nil)
			reads := []authorize.AuthorityRead{executionMembershipRead(t, "task.create", scope, "principal-exec")}
			facts, err := h.adapter.Read(context.Background(), reads)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if facts[0].Allowed {
				t.Fatalf("fact = %+v; want denied for a %s execution", facts[0], state)
			}
		})
	}
}

// TestAuthorityReaderExecutionPrincipalOtherScopeDenied proves scope
// containment: an execution row in one project cannot satisfy a membership read
// in another project or tenant.
func TestAuthorityReaderExecutionPrincipalOtherScopeDenied(t *testing.T) {
	h := newHarness(t)
	owner := projectScope("tenant-a", "project-a")
	h.seedExecution(owner, "execution-a", "principal-exec", 1, "running", nil, nil)
	scopes := []tenancy.Scope{
		projectScope("tenant-a", "project-b"),
		projectScope("tenant-b", "project-a"),
	}
	for _, scope := range scopes {
		t.Run(string(scope.TenantID)+"/"+string(scope.ProjectID), func(t *testing.T) {
			reads := []authorize.AuthorityRead{executionMembershipRead(t, "task.create", scope, "principal-exec")}
			facts, err := h.adapter.Read(context.Background(), reads)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if facts[0].Allowed {
				t.Fatalf("fact = %+v; want denied outside the execution's project", facts[0])
			}
		})
	}
}

// TestAuthorityReaderExecutionPrincipalMissingRowDenied proves a principal that
// resolves to no execution row in scope is refused; the grant comes from the
// committed running row, never from the principal identifier alone.
func TestAuthorityReaderExecutionPrincipalMissingRowDenied(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	reads := []authorize.AuthorityRead{executionMembershipRead(t, "task.create", scope, "principal-exec")}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("fact = %+v; want denied when no execution row exists", facts[0])
	}
}

// TestAuthorityGuardExecutionPrincipalCommitsRunning proves the commit-boundary
// lock path keeps the execution guards the unit itself lists: a running
// execution principal's membership read is satisfied by the registry grant, and
// the unit's own execution_running read pins the row while the credential write
// commits.
func TestAuthorityGuardExecutionPrincipalCommitsRunning(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionCredentialUnit(scope, "principal-exec", "execution-a", 1, []authorize.AuthorityRead{
		executionMembershipRead(t, "event.publish", scope, "principal-exec"),
		{
			Check:       authorize.CheckExecutionRunning,
			Scope:       scope,
			PrincipalID: "principal-exec",
			TargetID:    "execution-a",
		},
	})
	result, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if result.Version != 2 || result.State != token.StateCurrent {
		t.Fatalf("result = %+v; want a committed current pair at version 2", result)
	}
	row, found := h.snapshot(scope, "executions", "execution-a")
	if !found {
		t.Fatal("execution row disappeared")
	}
	if got := mustInt(t, row, "version"); got != 2 {
		t.Fatalf("execution version = %d; want 2", got)
	}
}

// TestAuthorityGuardExecutionPrincipalStopConflicts proves a concurrent stop
// between the membership evaluation and the commit is refused by the unit's own
// execution_running guard, and nothing is persisted.
func TestAuthorityGuardExecutionPrincipalStopConflicts(t *testing.T) {
	h := newBumpingHarness(t, "executions", "execution-a", 1, map[string]any{"state": "succeeded"})
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionCredentialUnit(scope, "principal-exec", "execution-a", 1, []authorize.AuthorityRead{
		executionMembershipRead(t, "event.publish", scope, "principal-exec"),
		{
			Check:       authorize.CheckExecutionRunning,
			Scope:       scope,
			PrincipalID: "principal-exec",
			TargetID:    "execution-a",
		},
	})
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the refused execution mutation left a receipt")
	}
	h.requireNoPublications(scope)
}

// TestAuthorityGuardExecutionPrincipalStoppedRefused proves a membership read
// for an already-stopped execution refuses the commit before any write.
func TestAuthorityGuardExecutionPrincipalStoppedRefused(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "succeeded", nil, nil)
	unit := h.executionCredentialUnit(scope, "principal-exec", "execution-a", 1, []authorize.AuthorityRead{
		executionMembershipRead(t, "event.publish", scope, "principal-exec"),
	})
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.Forbidden)
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the refused execution mutation left a receipt")
	}
	h.requireNoPublications(scope)
}

// TestAuthorityReaderPushServiceAdminDeniesExecutionPrincipal proves the
// conditional Admin check is never satisfied by an execution permission: Admin
// is a human membership role, so a running execution is refused even when the
// read's operation grant carries an execution permission.
func TestAuthorityReaderPushServiceAdminDeniesExecutionPrincipal(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	read := executionMembershipRead(t, "task.read", scope, "principal-exec")
	if read.Required.Execution == authorize.ExecNone {
		t.Fatal("task.read grants no execution permission; the vector needs an operation that does")
	}
	read.Check = authorize.CheckPushServiceAdmin
	reads := []authorize.AuthorityRead{read}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("fact = %+v; want denied because an execution never holds Admin", facts[0])
	}
	requireFaultClass(t, authorize.Confirm(reads, facts), fault.Forbidden)
}
