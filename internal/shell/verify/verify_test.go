package verify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// TestVerifyApprovesExactApprovalEvidence is the successful service-plus-actor
// path: the calling service is authorized by the current binding, the actor
// credential resolves to the execution principal, and earlier same-execution
// human-role approval evidence matches the exact operation, resource and
// subject. The committed record is returned as facts and never carries the
// forwarded credential.
func TestVerifyApprovesExactApprovalEvidence(t *testing.T) {
	h := newHarness(t)

	record, err := h.verifier.Verify(context.Background(), attempt())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if record.Result != verify.ResultApproved {
		t.Errorf("result = %q, want %q", record.Result, verify.ResultApproved)
	}
	if !record.Approved {
		t.Error("matching approval evidence was not recorded as an approval fact")
	}
	if record.ID != "verification-1" {
		t.Errorf("record id = %q, want the committed result id %q", record.ID, "verification-1")
	}
	if record.Scope != testScope() {
		t.Errorf("record scope = %#v, want %#v", record.Scope, testScope())
	}
	if record.CallingService != testServiceID {
		t.Errorf("record calling service = %q, want %q", record.CallingService, testServiceID)
	}
	if record.BindingID != testBindingID {
		t.Errorf("record binding = %q, want %q", record.BindingID, testBindingID)
	}
	if !reflect.DeepEqual(record.Actor, actorPrincipal()) {
		t.Errorf("record actor = %#v, want the current resolved principal %#v", record.Actor, actorPrincipal())
	}
	if record.ExecutionID != testExecution {
		t.Errorf("record execution = %q, want %q", record.ExecutionID, testExecution)
	}
	if record.ActionID != testAction {
		t.Errorf("record action = %q, want %q", record.ActionID, testAction)
	}
	if record.ResourceID != testResourceID {
		t.Errorf("record resource = %q, want %q", record.ResourceID, testResourceID)
	}
	if record.Operation != string(resource.OperationGitRefUpdate) {
		t.Errorf("record operation = %q, want %q", record.Operation, resource.OperationGitRefUpdate)
	}
	if !reflect.DeepEqual(record.Subject, requiredSubject()) {
		t.Errorf("record subject = %#v, want %#v", record.Subject, requiredSubject())
	}
	if record.RequestedApproval != roleApproval(tenancy.RoleReviewer) {
		t.Errorf("record requested approval = %#v, want %#v", record.RequestedApproval, roleApproval(tenancy.RoleReviewer))
	}
	if !record.At.Equal(fixedNow) {
		t.Errorf("record time = %v, want the injected clock time %v", record.At, fixedNow)
	}

	wantBindingQuery := verify.BindingQuery{Scope: testScope(), ResourceID: testResourceID, ServiceID: testServiceID}
	if got := h.bindings.lastQuery(); got != wantBindingQuery {
		t.Errorf("binding query = %#v, want %#v", got, wantBindingQuery)
	}
	wantEvidenceQuery := verify.EvidenceQuery{
		Scope:       testScope(),
		ExecutionID: testExecution,
		ActionID:    testAction,
		Assignment:  roleApproval(tenancy.RoleReviewer),
		Operation:   string(resource.OperationGitRefUpdate),
		ResourceID:  testResourceID,
		Subject:     requiredSubject(),
	}
	if got := h.evidence.lastQuery(); !reflect.DeepEqual(got, wantEvidenceQuery) {
		t.Errorf("evidence query = %#v, want %#v", got, wantEvidenceQuery)
	}

	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
	unit := h.records.unitAt(0)
	if unit.Owner != testScope() {
		t.Errorf("record unit owner = %#v, want %#v", unit.Owner, testScope())
	}
	if !reflect.DeepEqual(unit.Record, record) {
		t.Errorf("record unit record = %#v, want %#v", unit.Record, record)
	}
	assertAuthorityReads(t, unit, gitBinding(), actorPrincipal())
	assertNoCredential(t, unit, record)
}

// TestVerifyApprovesEarlierActionApprovalEvidence pins S05 §4: approval
// evidence is an earlier succeeded request in the actor's own execution. In
// the real flow the workload holds no authority while waiting, so the current
// verifying action is a later action; the evidence action id must not have to
// equal the attempt's current action id. The current action remains the
// authority-read target, the evidence-query hint and the recorded action.
func TestVerifyApprovesEarlierActionApprovalEvidence(t *testing.T) {
	h := newHarness(t)
	h.evidence.set([]verify.Evidence{mutatedEvidence(func(evidence *verify.Evidence) {
		evidence.ActionID = testPriorAction
	})}, nil)

	input := attempt()
	if input.ActionID == testPriorAction {
		t.Fatalf("test setup: attempt action %q must differ from the earlier evidence action %q", input.ActionID, testPriorAction)
	}

	record, err := h.verifier.Verify(context.Background(), input)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if record.Result != verify.ResultApproved {
		t.Errorf("result = %q, want %q", record.Result, verify.ResultApproved)
	}
	if !record.Approved {
		t.Error("earlier-action same-execution approval evidence was not recorded as an approval fact")
	}
	if record.ActionID != testAction {
		t.Errorf("record action = %q, want the current action %q", record.ActionID, testAction)
	}
	wantEvidenceQuery := verify.EvidenceQuery{
		Scope:       testScope(),
		ExecutionID: testExecution,
		ActionID:    testAction,
		Assignment:  roleApproval(tenancy.RoleReviewer),
		Operation:   string(resource.OperationGitRefUpdate),
		ResourceID:  testResourceID,
		Subject:     requiredSubject(),
	}
	if got := h.evidence.lastQuery(); !reflect.DeepEqual(got, wantEvidenceQuery) {
		t.Errorf("evidence query = %#v, want current-action hint %#v", got, wantEvidenceQuery)
	}
	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
	if unit := h.records.unitAt(0); unit.Record.ActionID != testAction {
		t.Errorf("record unit action = %q, want the current action %q", unit.Record.ActionID, testAction)
	}
}

// TestVerifyWithoutApprovalRequestIsApprovedWithoutApprovalFact asserts a
// verification that requests no approval still verifies the actor, does not
// read evidence, and records no approval fact.
func TestVerifyWithoutApprovalRequestIsApprovedWithoutApprovalFact(t *testing.T) {
	h := newHarness(t)
	input := attempt()
	input.Approval = request.Assignment{}

	record, err := h.verifier.Verify(context.Background(), input)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if record.Result != verify.ResultApproved {
		t.Errorf("result = %q, want %q", record.Result, verify.ResultApproved)
	}
	if record.Approved {
		t.Error("verification recorded an approval fact although none was requested")
	}
	if record.RequestedApproval != (request.Assignment{}) {
		t.Errorf("record requested approval = %#v, want none", record.RequestedApproval)
	}
	if h.evidence.queryCount() != 0 {
		t.Errorf("evidence reads = %d, want 0 when no approval is requested", h.evidence.queryCount())
	}
	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
}

// TestVerifyRefusesNonMatchingEvidence covers prior/foreign/failed/wrong-role/
// wrong-subject evidence: every mismatch is refused, and the refusal itself is
// persisted as a refused verification fact.
func TestVerifyRefusesNonMatchingEvidence(t *testing.T) {
	cases := []struct {
		name     string
		evidence verify.Evidence
	}{
		{"foreign execution", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.ExecutionID = testOtherExec
		})},
		{"foreign operation", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Operation = "git.ref.read"
		})},
		{"foreign resource", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.ResourceID = resource.ResourceID("repo-2")
		})},
		{"failed decision", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Decision = request.DecisionFail
		})},
		{"service assignment", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Assignment = request.Assignment{Kind: request.AssignmentService, ServiceID: testServiceID}
		})},
		{"wrong approval role", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Assignment = roleApproval(tenancy.RoleOperator)
		})},
		{"wrong subject value", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Subject = subjectWith("candidate", verify.KindString, "sha-other")
		})},
		{"wrong subject kind", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Subject = subjectWith("candidate", verify.KindNumber, "1")
		})},
		{"missing required field", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Subject = verify.Subject{Fields: []verify.SubjectField{
				{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
			}}
		})},
		{"duplicate recorded name", mutatedEvidence(func(evidence *verify.Evidence) {
			fields := append([]verify.SubjectField(nil), evidenceSubject().Fields...)
			evidence.Subject = verify.Subject{Fields: append(fields,
				verify.SubjectField{Name: "candidate", Kind: verify.KindString, Value: "sha-candidate"},
			)}
		})},
		{"unknown recorded kind", mutatedEvidence(func(evidence *verify.Evidence) {
			evidence.Subject = subjectWith("candidate", verify.ValueKind("date"), "sha-candidate")
		})},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			h.evidence.set([]verify.Evidence{testCase.evidence}, nil)

			record, err := h.verifier.Verify(context.Background(), attempt())
			assertRefusedRecord(t, h, record, err)
		})
	}
}

// TestVerifyIgnoresDisplayContextInEvidence asserts recorded fields the
// enforcing service did not supply are immutable display context and never
// change the match.
func TestVerifyIgnoresDisplayContextInEvidence(t *testing.T) {
	h := newHarness(t)
	// passingEvidence already carries display-only fields.
	record, err := h.verifier.Verify(context.Background(), attempt())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !record.Approved || record.Result != verify.ResultApproved {
		t.Fatalf("display context changed the approval match: %#v", record)
	}
}

// TestVerifyTreatsEverySuppliedSubjectFieldAsRequired asserts a subject field
// supplied by the enforcing service is required, never ignorable.
func TestVerifyTreatsEverySuppliedSubjectFieldAsRequired(t *testing.T) {
	h := newHarness(t)
	input := attempt()
	input.Subject = verify.Subject{Fields: append(
		append([]verify.SubjectField(nil), requiredSubject().Fields...),
		verify.SubjectField{Name: "note", Kind: verify.KindString, Value: "hello"},
	)}

	record, err := h.verifier.Verify(context.Background(), input)
	assertRefusedRecord(t, h, record, err)
}

// TestVerifyEmptySubjectIsNotAWildcard asserts an empty required subject only
// matches an empty recorded subject.
func TestVerifyEmptySubjectIsNotAWildcard(t *testing.T) {
	t.Run("empty required against display-only evidence", func(t *testing.T) {
		h := newHarness(t)
		input := attempt()
		input.Subject = verify.Subject{}

		record, err := h.verifier.Verify(context.Background(), input)
		assertRefusedRecord(t, h, record, err)
	})

	t.Run("empty required against empty evidence", func(t *testing.T) {
		h := newHarness(t)
		evidence := passingEvidence()
		evidence.Subject = verify.Subject{}
		h.evidence.set([]verify.Evidence{evidence}, nil)
		input := attempt()
		input.Subject = verify.Subject{}

		record, err := h.verifier.Verify(context.Background(), input)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !record.Approved {
			t.Fatal("an empty required subject did not match an empty recorded subject")
		}
	})
}

// gitReadBinding declares the repository-level read operation the smart-HTTP
// read verifier submits.
func gitReadBinding() resource.Binding {
	binding := gitBinding()
	binding.Operations = []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	return binding
}

// readAttempt is the repository-level `git.read` verification attempt: an empty
// subject and no requested approval.
func readAttempt() verify.Attempt {
	input := attempt()
	input.Operation = resource.OperationGitRead
	input.Subject = verify.Subject{}
	input.Approval = request.Assignment{}
	input.ActionID = ""
	return input
}

// TestVerifyGitReadRequiresEmptySubjectAndNoApproval asserts the read shape is
// enforced structurally: a read carrying effect fields or a role approval is
// invalid input and touches no dependency.
func TestVerifyGitReadRequiresEmptySubjectAndNoApproval(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*verify.Attempt)
	}{
		{"read carries a subject", func(input *verify.Attempt) {
			input.Operation = resource.OperationGitRead
			input.Subject = requiredSubject()
			input.Approval = request.Assignment{}
		}},
		{"read requests an approval", func(input *verify.Attempt) {
			input.Operation = resource.OperationGitRead
			input.Subject = verify.Subject{}
			input.Approval = roleApproval(tenancy.RoleReviewer)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			input := attempt()
			testCase.mutate(&input)

			record, err := h.verifier.Verify(context.Background(), input)
			wantFaultClass(t, err, fault.InvalidInput)
			if record.ID != "" {
				t.Errorf("record id = %q, want no record for a malformed read", record.ID)
			}
			if h.principals.count() != 0 || h.bindings.queryCount() != 0 || h.records.count() != 0 {
				t.Errorf("malformed read touched a dependency: principals=%d bindings=%d records=%d",
					h.principals.count(), h.bindings.queryCount(), h.records.count())
			}
		})
	}
}

// TestVerifyGitReadWithEmptySubjectIsApproved asserts the canonical read shape
// commits one approved verification fact with an empty subject.
func TestVerifyGitReadWithEmptySubjectIsApproved(t *testing.T) {
	h := newHarness(t)
	h.bindings.set(gitReadBinding(), nil)

	record, err := h.verifier.Verify(context.Background(), readAttempt())
	if err != nil {
		t.Fatalf("Verify a repository read: %v", err)
	}
	if record.Result != verify.ResultApproved || record.Approved {
		t.Errorf("record = %#v, want an approved read without an approval fact", record)
	}
	if record.Operation != string(resource.OperationGitRead) || len(record.Subject.Fields) != 0 {
		t.Errorf("read record operation/subject = %q/%+v; want git.read and an empty subject",
			record.Operation, record.Subject)
	}
	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
	if unit := h.records.unitAt(0); unit.Record.Operation != string(resource.OperationGitRead) {
		t.Errorf("committed read operation = %q; want git.read", unit.Record.Operation)
	}
}

// scopedMembershipRow is one observed membership row for the human actor.
func scopedMembershipRow(scope tenancy.Scope, rowID string, version uint64, roles ...tenancy.Role) authorize.MembershipRow {
	return authorize.MembershipRow{
		RowID: rowID,
		Membership: &tenancy.Membership{
			Scope:       scope,
			PrincipalID: testHumanActor,
			Roles:       roles,
			Version:     version,
		},
	}
}

// TestVerifyResolvesScopedMembershipRolesAndPinsBothRows proves a human actor
// whose credential resolves at tenant scope with only the tenant-wide viewer
// role receives the union of the tenant-wide and exact-project rows in the
// verification scope, and that both rows are pinned on the record unit.
func TestVerifyResolvesScopedMembershipRolesAndPinsBothRows(t *testing.T) {
	h := newHarness(t)
	h.bindings.set(gitReadBinding(), nil)
	// The credential resolves at tenant scope with the tenant-wide viewer grant
	// only; the project engineer grant is not in the resolver's roles.
	resolverActor := humanActor()
	resolverActor.Scope = tenancy.Scope{TenantID: testTenant}
	resolverActor.Roles = []tenancy.Role{tenancy.RoleViewer}
	h.principals.set(actorCredential, resolverActor, nil)

	membership := h.useMembership(t)
	membership.set(authorize.MembershipRows{
		TenantWide: scopedMembershipRow(tenancy.Scope{TenantID: testTenant}, "membership-tenant", 3, tenancy.RoleViewer),
		Project:    scopedMembershipRow(testScope(), "membership-project", 4, tenancy.RoleEngineer),
	}, nil)

	record, err := h.verifier.Verify(context.Background(), readAttempt())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantRoles := []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}
	if !reflect.DeepEqual(record.Actor.Roles, wantRoles) {
		t.Errorf("verified roles = %v, want the scoped union %v", record.Actor.Roles, wantRoles)
	}
	if membership.queryCount() != 1 {
		t.Fatalf("membership reads = %d, want exactly one scoped read", membership.queryCount())
	}
	unit := h.records.unitAt(0)
	read := principalEnabledRead(t, unit)
	wantTargets := []authorize.AuthorityTarget{
		{Scope: testScope(), RowID: "membership-project", ExpectedVersion: 4},
		{Scope: tenancy.Scope{TenantID: testTenant}, RowID: "membership-tenant", ExpectedVersion: 3},
	}
	if !reflect.DeepEqual(read.Targets, wantTargets) {
		t.Errorf("principal_enabled pins = %+v, want both rows %+v", read.Targets, wantTargets)
	}
	if !reflect.DeepEqual(read.Required.HumanRoles, wantRoles) {
		t.Errorf("principal_enabled required roles = %v, want the observed union %v", read.Required.HumanRoles, wantRoles)
	}
}

// TestVerifyRefusesHumanWithoutScopedMembership asserts an authenticated human
// with no applicable membership row in the verification scope is refused and
// commits no verification fact.
func TestVerifyRefusesHumanWithoutScopedMembership(t *testing.T) {
	h := newHarness(t)
	h.bindings.set(gitReadBinding(), nil)
	resolverActor := humanActor()
	resolverActor.Scope = tenancy.Scope{TenantID: testTenant}
	h.principals.set(actorCredential, resolverActor, nil)

	membership := h.useMembership(t)
	membership.set(authorize.MembershipRows{
		TenantWide: authorize.MembershipRow{
			RowID: "membership-other",
			Membership: &tenancy.Membership{
				Scope: tenancy.Scope{TenantID: otherTenant}, PrincipalID: testHumanActor,
				Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 1,
			},
		},
	}, nil)

	record, err := h.verifier.Verify(context.Background(), readAttempt())
	wantFaultClass(t, err, fault.Forbidden)
	if record.ID != "" {
		t.Errorf("record id = %q, want no record without a scoped membership", record.ID)
	}
	if h.records.count() != 0 {
		t.Errorf("record port calls = %d, want 0 without a scoped membership", h.records.count())
	}
}

// TestVerifyMembershipDependencyFailureCreatesNoRecord asserts an unavailable
// membership source never yields a successful verification or a partial record.
func TestVerifyMembershipDependencyFailureCreatesNoRecord(t *testing.T) {
	h := newHarness(t)
	h.bindings.set(gitReadBinding(), nil)
	resolverActor := humanActor()
	resolverActor.Scope = tenancy.Scope{TenantID: testTenant}
	h.principals.set(actorCredential, resolverActor, nil)

	membership := h.useMembership(t)
	membership.set(authorize.MembershipRows{}, fault.New(fault.DependencyUnavailable, "membership store unavailable"))

	record, err := h.verifier.Verify(context.Background(), readAttempt())
	wantFaultClass(t, err, fault.DependencyUnavailable)
	if record.ID != "" || h.records.count() != 0 {
		t.Errorf("membership dependency failure produced record %q with %d commits", record.ID, h.records.count())
	}
}

func principalEnabledRead(t *testing.T, unit verify.RecordUnit) authorize.AuthorityRead {
	t.Helper()
	for _, read := range unit.Authority {
		if read.Check == authorize.CheckPrincipalEnabled {
			return read
		}
	}
	t.Fatal("record unit carries no principal_enabled authority read")
	return authorize.AuthorityRead{}
}

// TestVerifyRefusesApprovalForNonExecutionActor asserts humans and services can
// never carry execution approval evidence, even when they hold the requested
// role.
func TestVerifyRefusesApprovalForNonExecutionActor(t *testing.T) {
	cases := []struct {
		name      string
		principal identity.Principal
	}{
		{"human holding the role", humanActor()},
		{"service holding the role", serviceActor()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			h.principals.set(actorCredential, testCase.principal, nil)

			record, err := h.verifier.Verify(context.Background(), attempt())
			assertRefusedRecordWithActor(t, h, record, err, testCase.principal)
			if h.evidence.queryCount() != 0 {
				t.Errorf("evidence reads = %d, want 0 for a non-execution actor", h.evidence.queryCount())
			}
		})
	}
}

// TestVerifyRefusesActorOutsideScope asserts the actor credential must resolve
// to a principal in the verification scope.
func TestVerifyRefusesActorOutsideScope(t *testing.T) {
	h := newHarness(t)
	foreign := actorPrincipal()
	foreign.Scope = otherScope()
	h.principals.set(actorCredential, foreign, nil)

	record, err := h.verifier.Verify(context.Background(), attempt())
	assertRefusedRecordWithActor(t, h, record, err, foreign)
}

// TestVerifyRefusesRevokedActor asserts a currently disabled actor is refused
// even though the credential resolved, and the refusal is recorded.
func TestVerifyRefusesRevokedActor(t *testing.T) {
	h := newHarness(t)
	revoked := actorPrincipal()
	revoked.Enabled = false
	h.principals.set(actorCredential, revoked, nil)

	record, err := h.verifier.Verify(context.Background(), attempt())
	assertRefusedRecordWithActor(t, h, record, err, revoked)
}

// TestVerifyRefusesUnauthenticatedActor asserts an unresolvable credential is
// an authentication refusal and creates no verification fact.
func TestVerifyRefusesUnauthenticatedActor(t *testing.T) {
	h := newHarness(t)
	h.principals.set(actorCredential, identity.Principal{}, fault.New(fault.Unauthenticated, "credential expired"))

	record, err := h.verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.Unauthenticated)
	if record.ID != "" {
		t.Errorf("record id = %q, want no record for an unauthenticated actor", record.ID)
	}
	if h.bindings.queryCount() != 0 {
		t.Errorf("binding reads = %d, want 0 when the actor cannot authenticate", h.bindings.queryCount())
	}
	if h.records.count() != 0 {
		t.Errorf("record port calls = %d, want 0 when the actor cannot authenticate", h.records.count())
	}
}

// TestVerifyRefusesUnauthorizedCallingService asserts the binding must cover the
// calling service, the resource, the scope and the attempted operation.
func TestVerifyRefusesUnauthorizedCallingService(t *testing.T) {
	cases := []struct {
		name          string
		mutateAttempt func(*verify.Attempt)
		mutateHarness func(*harness)
		class         fault.Class
	}{
		{
			name: "binding for another service",
			mutateHarness: func(h *harness) {
				binding := gitBinding()
				binding.EnforcingService = "svc-other"
				h.bindings.set(binding, nil)
			},
			class: fault.Forbidden,
		},
		{
			name: "binding in another scope",
			mutateHarness: func(h *harness) {
				binding := gitBinding()
				binding.Scope = otherScope()
				h.bindings.set(binding, nil)
			},
			class: fault.Forbidden,
		},
		{
			name: "binding for another resource",
			mutateHarness: func(h *harness) {
				binding := gitBinding()
				binding.ResourceID = resource.ResourceID("repo-2")
				h.bindings.set(binding, nil)
			},
			class: fault.Forbidden,
		},
		{
			name: "binding does not declare the operation",
			mutateHarness: func(h *harness) {
				binding := gitBinding()
				binding.Operations = nil
				h.bindings.set(binding, nil)
			},
			class: fault.Forbidden,
		},
		{
			name: "disabled calling service",
			mutateAttempt: func(input *verify.Attempt) {
				input.Service.Enabled = false
			},
			class: fault.Forbidden,
		},
		{
			name: "calling service outside the scope",
			mutateAttempt: func(input *verify.Attempt) {
				input.Service.Scope = otherScope()
			},
			class: fault.Forbidden,
		},
		{
			name: "binding reader reports not found",
			mutateHarness: func(h *harness) {
				h.bindings.set(resource.Binding{}, fault.New(fault.NotFound, "no binding in scope"))
			},
			class: fault.NotFound,
		},
		{
			name: "binding reader unavailable",
			mutateHarness: func(h *harness) {
				h.bindings.set(resource.Binding{}, fault.New(fault.DependencyUnavailable, "binding store unavailable"))
			},
			class: fault.DependencyUnavailable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			if testCase.mutateHarness != nil {
				testCase.mutateHarness(h)
			}
			input := attempt()
			if testCase.mutateAttempt != nil {
				testCase.mutateAttempt(&input)
			}

			record, err := h.verifier.Verify(context.Background(), input)
			wantFaultClass(t, err, testCase.class)
			if record.ID != "" {
				t.Errorf("record id = %q, want no record for an unauthorized calling service", record.ID)
			}
			if h.records.count() != 0 {
				t.Errorf("record port calls = %d, want 0 for an unauthorized calling service", h.records.count())
			}
		})
	}
}

// TestVerifyRefusesMalformedAttempt asserts structural problems are invalid
// input and touch no dependency.
func TestVerifyRefusesMalformedAttempt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*verify.Attempt)
	}{
		{"empty credential", func(input *verify.Attempt) { input.Credential = "" }},
		{"invalid resource id", func(input *verify.Attempt) { input.ResourceID = "bad/resource" }},
		{"empty operation", func(input *verify.Attempt) { input.Operation = "" }},
		{"unknown operation", func(input *verify.Attempt) { input.Operation = resource.Operation("git.ref.read") }},
		{"empty tenant", func(input *verify.Attempt) {
			input.Scope = tenancy.Scope{ProjectID: testProject}
		}},
		{"tenant-only scope", func(input *verify.Attempt) {
			input.Scope = tenancy.Scope{TenantID: testTenant}
		}},
		{"human calling service", func(input *verify.Attempt) { input.Service = humanActor() }},
		{"invalid calling service", func(input *verify.Attempt) {
			input.Service = identity.Principal{
				ID:      id.PrincipalID("bad service"),
				Kind:    identity.KindService,
				Scope:   testScope(),
				Enabled: true,
			}
		}},
		{"service approval assignment", func(input *verify.Attempt) {
			input.Approval = request.Assignment{Kind: request.AssignmentService, ServiceID: testServiceID}
		}},
		{"unknown approval role", func(input *verify.Attempt) {
			input.Approval = request.Assignment{Kind: request.AssignmentRole, Role: tenancy.Role("owner")}
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			input := attempt()
			testCase.mutate(&input)

			record, err := h.verifier.Verify(context.Background(), input)
			wantFaultClass(t, err, fault.InvalidInput)
			if record.ID != "" {
				t.Errorf("record id = %q, want no record for a malformed attempt", record.ID)
			}
			if h.principals.count() != 0 {
				t.Errorf("credential resolutions = %d, want 0 for a malformed attempt", h.principals.count())
			}
			if h.bindings.queryCount() != 0 {
				t.Errorf("binding reads = %d, want 0 for a malformed attempt", h.bindings.queryCount())
			}
			if h.records.count() != 0 {
				t.Errorf("record port calls = %d, want 0 for a malformed attempt", h.records.count())
			}
		})
	}
}

// TestVerifyPropagatesEvidenceDependencyFailureWithoutRecord asserts an
// unavailable evidence source never yields a successful verification or a
// partial record.
func TestVerifyPropagatesEvidenceDependencyFailureWithoutRecord(t *testing.T) {
	h := newHarness(t)
	h.evidence.set(nil, fault.New(fault.DependencyUnavailable, "evidence store unavailable"))

	record, err := h.verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.DependencyUnavailable)
	if record.ID != "" {
		t.Errorf("record id = %q, want no record when evidence cannot be read", record.ID)
	}
	if h.records.count() != 0 {
		t.Errorf("record port calls = %d, want 0 when evidence cannot be read", h.records.count())
	}
}

// TestVerifyPropagatesRecordCommitFailure asserts a refused commit unit is a
// stable failure with no successful verification, including stale versions.
func TestVerifyPropagatesRecordCommitFailure(t *testing.T) {
	for _, class := range []fault.Class{fault.DependencyUnavailable, fault.VersionConflict, fault.Forbidden} {
		t.Run(string(class), func(t *testing.T) {
			h := newHarness(t)
			h.records.setError(fault.New(class, "commit refused"))

			record, err := h.verifier.Verify(context.Background(), attempt())
			wantFaultClass(t, err, class)
			if record.ID != "" {
				t.Errorf("record id = %q, want no committed verification fact", record.ID)
			}
			if h.records.count() != 1 {
				t.Errorf("record port calls = %d, want exactly one commit attempt", h.records.count())
			}
		})
	}
}

// TestVerifyRetryAuthenticatesAfresh asserts every attempt re-reads the actor
// credential and the current binding: a revoked credential after a successful
// attempt fails authentication and creates no second fact.
func TestVerifyRetryAuthenticatesAfresh(t *testing.T) {
	h := newHarness(t)
	if _, err := h.verifier.Verify(context.Background(), attempt()); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	h.principals.set(actorCredential, identity.Principal{}, fault.New(fault.Unauthenticated, "credential revoked"))

	record, err := h.verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.Unauthenticated)
	if record.ID != "" {
		t.Errorf("record id = %q, want no record after current revocation", record.ID)
	}
	if h.principals.count() != 2 {
		t.Errorf("credential resolutions = %d, want one per attempt", h.principals.count())
	}
	if h.records.count() != 1 {
		t.Errorf("record port calls = %d, want the first attempt only", h.records.count())
	}
}

// TestVerifyReadsTheCurrentBindingVersionOnEachAttempt asserts the authority
// reads are rebuilt from the binding read in this attempt, so a stale version is
// never reused.
func TestVerifyReadsTheCurrentBindingVersionOnEachAttempt(t *testing.T) {
	h := newHarness(t)
	if _, err := h.verifier.Verify(context.Background(), attempt()); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	updated := gitBinding()
	updated.Version = 8
	h.bindings.set(updated, nil)
	if _, err := h.verifier.Verify(context.Background(), attempt()); err != nil {
		t.Fatalf("second Verify: %v", err)
	}

	units := h.records.all()
	if len(units) != 2 {
		t.Fatalf("record port calls = %d, want 2", len(units))
	}
	for index, want := range []uint64{7, 8} {
		read := bindingVersionRead(t, units[index])
		if read.ExpectedVersion != want {
			t.Errorf("attempt %d binding_version expected %d, want %d", index+1, read.ExpectedVersion, want)
		}
	}
}

// TestVerifyReplayReturnsTheCommittedRecordID asserts a retried attempt that
// the port reports as replayed converges on the already committed verification
// id and still authenticates afresh.
func TestVerifyReplayReturnsTheCommittedRecordID(t *testing.T) {
	h := newHarness(t)
	h.records.setResultFor(func(verify.RecordUnit) (verify.RecordResult, error) {
		return verify.RecordResult{ID: "verification-stable", Replayed: true}, nil
	})

	first, err := h.verifier.Verify(context.Background(), attempt())
	if err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	second, err := h.verifier.Verify(context.Background(), attempt())
	if err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if first.ID != "verification-stable" || second.ID != "verification-stable" {
		t.Fatalf("record ids = %q and %q, want the committed id", first.ID, second.ID)
	}
	if h.principals.count() != 2 {
		t.Errorf("credential resolutions = %d, want one per attempt", h.principals.count())
	}
	if h.bindings.queryCount() != 2 {
		t.Errorf("binding reads = %d, want one per attempt", h.bindings.queryCount())
	}
}

// TestVerifyReplayedReceiptCannotGrantAuthority asserts a port claiming a replay
// cannot turn a refused attempt into an approval.
func TestVerifyReplayedReceiptCannotGrantAuthority(t *testing.T) {
	h := newHarness(t)
	h.evidence.set(nil, nil)
	h.records.setResultFor(func(verify.RecordUnit) (verify.RecordResult, error) {
		return verify.RecordResult{ID: "verification-replayed", Replayed: true}, nil
	})

	record, err := h.verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.Forbidden)
	if record.Result != verify.ResultRefused {
		t.Errorf("result = %q, want %q despite a replayed receipt", record.Result, verify.ResultRefused)
	}
	if record.Approved {
		t.Error("a replayed receipt granted approval authority")
	}
}

// TestVerifyRefusesRecordResultWithoutID asserts a port result that does not
// identify the committed record is an internal failure, never a success.
func TestVerifyRefusesRecordResultWithoutID(t *testing.T) {
	h := newHarness(t)
	h.records.setResultFor(func(verify.RecordUnit) (verify.RecordResult, error) {
		return verify.RecordResult{}, nil
	})

	record, err := h.verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.InternalFailure)
	if record.ID != "" {
		t.Errorf("record id = %q, want no committed verification fact", record.ID)
	}
}

// TestNewRequiresEveryDependency asserts the verifier refuses an incomplete
// composition instead of guessing a substitute.
func TestNewRequiresEveryDependency(t *testing.T) {
	base := verify.Config{
		Principals: newFakePrincipals(),
		Bindings:   &fakeBindings{},
		Evidence:   &fakeEvidence{},
		Records:    &fakeRecords{},
		Clock:      &fakeClock{now: fixedNow},
		NewID:      newIDSequence("verification"),
	}
	if _, err := verify.New(base); err != nil {
		t.Fatalf("complete config: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*verify.Config)
	}{
		{"missing principal resolver", func(config *verify.Config) { config.Principals = nil }},
		{"missing binding reader", func(config *verify.Config) { config.Bindings = nil }},
		{"missing evidence reader", func(config *verify.Config) { config.Evidence = nil }},
		{"missing record port", func(config *verify.Config) { config.Records = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			testCase.mutate(&config)
			_, err := verify.New(config)
			wantFaultClass(t, err, fault.InvalidInput)
		})
	}
}

// TestVerifyPropagatesIDGeneratorFailure asserts a failed identity generator
// refuses before any record is attempted.
func TestVerifyPropagatesIDGeneratorFailure(t *testing.T) {
	h := newHarness(t)
	verifier, err := verify.New(verify.Config{
		Principals: h.principals,
		Bindings:   h.bindings,
		Evidence:   h.evidence,
		Records:    h.records,
		Clock:      h.clock,
		NewID: func() (verify.VerificationID, error) {
			return "", fault.New(fault.InternalFailure, "no verification identity available")
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}

	record, err := verifier.Verify(context.Background(), attempt())
	wantFaultClass(t, err, fault.InternalFailure)
	if record.ID != "" {
		t.Errorf("record id = %q, want no record", record.ID)
	}
	if h.records.count() != 0 {
		t.Errorf("record port calls = %d, want 0", h.records.count())
	}
}

// TestVerifyConcurrentAttemptsAreIndependent asserts one verifier instance holds
// no per-attempt state: concurrent attempts each authenticate, read and commit
// their own fact.
func TestVerifyConcurrentAttemptsAreIndependent(t *testing.T) {
	h := newHarness(t)

	const attempts = 16
	ids := make([]verify.VerificationID, attempts)
	errs := make([]error, attempts)
	var group sync.WaitGroup
	for index := range attempts {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			record, err := h.verifier.Verify(context.Background(), attempt())
			ids[index] = record.ID
			errs[index] = err
		}(index)
	}
	group.Wait()

	seen := make(map[verify.VerificationID]bool, attempts)
	for index := range attempts {
		if errs[index] != nil {
			t.Errorf("attempt %d: %v", index, errs[index])
			continue
		}
		if ids[index] == "" {
			t.Errorf("attempt %d returned no verification id", index)
			continue
		}
		if seen[ids[index]] {
			t.Errorf("attempt %d reused verification id %q", index, ids[index])
		}
		seen[ids[index]] = true
	}
	if h.records.count() != attempts {
		t.Errorf("record port calls = %d, want %d", h.records.count(), attempts)
	}
	if h.principals.count() != attempts {
		t.Errorf("credential resolutions = %d, want %d", h.principals.count(), attempts)
	}
}

// TestVerifyNeverForwardsTheCredential asserts neither the returned record nor
// the committed unit serializes the forwarded actor credential.
func TestVerifyNeverForwardsTheCredential(t *testing.T) {
	h := newHarness(t)
	input := attempt()
	input.Credential = "kodex.exec-1.super-secret-material"
	h.principals.set(input.Credential, actorPrincipal(), nil)

	record, err := h.verifier.Verify(context.Background(), input)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
	for name, value := range map[string]any{"record": record, "record unit": h.records.unitAt(0)} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if bytes.Contains(data, []byte(input.Credential)) {
			t.Errorf("%s JSON carries the forwarded actor credential", name)
		}
	}
	if h.principals.count() != 1 {
		t.Errorf("credential resolutions = %d, want 1", h.principals.count())
	}
}

// assertRefusedRecord asserts a refusal is a forbidden fault with one persisted
// refused record for the current actor.
func assertRefusedRecord(t *testing.T, h *harness, record verify.Record, err error) {
	t.Helper()
	assertRefusedRecordWithActor(t, h, record, err, actorPrincipal())
}

func assertRefusedRecordWithActor(t *testing.T, h *harness, record verify.Record, err error, actor identity.Principal) {
	t.Helper()
	wantFaultClass(t, err, fault.Forbidden)
	if record.Result != verify.ResultRefused {
		t.Errorf("result = %q, want %q", record.Result, verify.ResultRefused)
	}
	if record.Approved {
		t.Error("a refused verification recorded an approval fact")
	}
	if record.ID == "" {
		t.Error("a refused verification did not persist a record identity")
	}
	if h.records.count() != 1 {
		t.Fatalf("record port calls = %d, want 1", h.records.count())
	}
	unit := h.records.unitAt(0)
	if unit.Record.Result != verify.ResultRefused {
		t.Errorf("committed record result = %q, want %q", unit.Record.Result, verify.ResultRefused)
	}
	assertAuthorityReads(t, unit, gitBinding(), actor)
	assertNoCredential(t, unit, record)
}

// assertAuthorityReads asserts the commit unit carries the actor.verify
// authority-read context the adapter needs: the registry checks in order, the
// operation and grant context, the scope, and the current target/version.
func assertAuthorityReads(t *testing.T, unit verify.RecordUnit, binding resource.Binding, actor identity.Principal) {
	t.Helper()
	operation := operationNamed(t, "actor.verify")
	if len(unit.Authority) != len(operation.Authority) {
		t.Fatalf("record unit carries %d authority reads; want %d (%v)",
			len(unit.Authority), len(operation.Authority), operation.Authority)
	}
	for index, read := range unit.Authority {
		wantCheck := operation.Authority[index]
		if read.Check != wantCheck {
			t.Fatalf("authority read %d = %q, want %q", index, read.Check, wantCheck)
		}
		if read.Operation != operation.Name {
			t.Errorf("authority read %q carries operation %q; want %q", read.Check, read.Operation, operation.Name)
		}
		if !reflect.DeepEqual(read.Required, operation.Requires) {
			t.Errorf("authority read %q carries grant context %#v; want %#v", read.Check, read.Required, operation.Requires)
		}
		if read.Scope != testScope() {
			t.Errorf("authority read %q carries scope %#v; want %#v", read.Check, read.Scope, testScope())
		}
		switch read.Check {
		case authorize.CheckBindingVersion:
			if read.TargetID != string(binding.ID) {
				t.Errorf("binding_version target = %q, want %q", read.TargetID, binding.ID)
			}
			if read.ExpectedVersion != binding.Version {
				t.Errorf("binding_version expected version = %d, want %d", read.ExpectedVersion, binding.Version)
			}
		case authorize.CheckServiceEnabled:
			if read.PrincipalID != id.PrincipalID(testServiceID) {
				t.Errorf("service_enabled principal = %q, want the calling service %q", read.PrincipalID, testServiceID)
			}
		case authorize.CheckPrincipalEnabled:
			if read.PrincipalID != actor.ID {
				t.Errorf("principal_enabled principal = %q, want the actor %q", read.PrincipalID, actor.ID)
			}
		case authorize.CheckExecutionRunning:
			if read.TargetID != string(actor.ExecutionID) {
				t.Errorf("execution_running target = %q, want %q", read.TargetID, actor.ExecutionID)
			}
		}
	}
}

func bindingVersionRead(t *testing.T, unit verify.RecordUnit) authorize.AuthorityRead {
	t.Helper()
	for _, read := range unit.Authority {
		if read.Check == authorize.CheckBindingVersion {
			return read
		}
	}
	t.Fatal("record unit carries no binding_version authority read")
	return authorize.AuthorityRead{}
}

// assertNoCredential asserts the forwarded credential is never serialized into
// the committed unit or the returned record.
func assertNoCredential(t *testing.T, unit verify.RecordUnit, record verify.Record) {
	t.Helper()
	for name, value := range map[string]any{"record unit": unit, "verification record": record} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if bytes.Contains(data, []byte(actorCredential)) {
			t.Errorf("%s JSON carries the forwarded actor credential", name)
		}
	}
}
