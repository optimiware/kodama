package verify_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// Implementation surface used by these tests:
//
//	type verify.Clock interface{ Now() time.Time }
//	type verify.Config struct {
//	    Principals authorize.CurrentPrincipal
//	    Bindings   verify.BindingReader
//	    Evidence   verify.EvidenceReader
//	    Records    verify.RecordPort
//	    Clock      verify.Clock
//	    NewID      func() (verify.VerificationID, error)
//	}
//	func verify.New(verify.Config) (*verify.Verifier, error)
//	type verify.Attempt struct {
//	    Scope      tenancy.Scope
//	    Service    identity.Principal
//	    Credential string
//	    ResourceID resource.ResourceID
//	    Operation  resource.Operation
//	    Subject    verify.Subject
//	    Approval   request.Assignment
//	    ActionID   id.ActionID
//	}
//	func (*verify.Verifier) Verify(context.Context, verify.Attempt) (verify.Record, error)

const (
	testTenant      = id.TenantID("tenant-a")
	testProject     = id.ProjectID("project-a")
	otherTenant     = id.TenantID("tenant-b")
	otherProject    = id.ProjectID("project-b")
	testExecution   = id.ExecutionID("exec-1")
	testOtherExec   = id.ExecutionID("exec-2")
	testAction      = id.ActionID("action-1")
	testPriorAction = id.ActionID("action-0")
	testActor       = id.PrincipalID("principal-exec")
	testHumanActor  = id.PrincipalID("principal-human")
	testServiceID   = service.ServiceID("svc-git")
	testBindingID   = resource.BindingID("binding-1")
	testResourceID  = resource.ResourceID("repo-1")
	actorCredential = "kodex.exec-1.credential-material"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: otherTenant, ProjectID: otherProject}
}

// actorPrincipal is the current eligible execution the fake credential
// resolves to.
func actorPrincipal() identity.Principal {
	return identity.Principal{
		ID:          testActor,
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     4,
		ExecutionID: testExecution,
	}
}

func humanActor() identity.Principal {
	return identity.Principal{
		ID:      testHumanActor,
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 2,
	}
}

// serviceActor is a service account with the required role: service permission
// never substitutes for the human-role approval evidence.
func serviceActor() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(testServiceID),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 5,
	}
}

// callingService is the independently verified service that calls verify.
func callingService() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(testServiceID),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 3,
	}
}

func gitBinding() resource.Binding {
	return resource.Binding{
		ID:               testBindingID,
		ResourceID:       testResourceID,
		Scope:            testScope(),
		EnforcingService: testServiceID,
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          7,
	}
}

// requiredSubject is the exact effect subject the enforcing service supplies.
func requiredSubject() verify.Subject {
	return verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "expected_base", Kind: verify.KindString, Value: "sha-base"},
		{Name: "candidate", Kind: verify.KindString, Value: "sha-candidate"},
	}}
}

// evidenceSubject is the immutable request subject recorded at resolution: the
// required fields plus fields the enforcing service did not supply, which are
// display context.
func evidenceSubject() verify.Subject {
	fields := append([]verify.SubjectField(nil), requiredSubject().Fields...)
	return verify.Subject{Fields: append(fields,
		verify.SubjectField{Name: "author_display", Kind: verify.KindString, Value: "Alice"},
		verify.SubjectField{Name: "pull_request", Kind: verify.KindNumber, Value: "42"},
		verify.SubjectField{Name: "draft", Kind: verify.KindBoolean, Value: "true"},
	)}
}

func roleApproval(role tenancy.Role) request.Assignment {
	return request.Assignment{Kind: request.AssignmentRole, Role: role}
}

func passingEvidence() verify.Evidence {
	return verify.Evidence{
		ExecutionID: testExecution,
		ActionID:    testAction,
		Assignment:  roleApproval(tenancy.RoleReviewer),
		Operation:   string(resource.OperationGitRefUpdate),
		ResourceID:  testResourceID,
		Subject:     evidenceSubject(),
		Decision:    request.DecisionPass,
		ResolvedAt:  fixedNow.Add(-time.Minute),
	}
}

func attempt() verify.Attempt {
	return verify.Attempt{
		Scope:      testScope(),
		Service:    callingService(),
		Credential: actorCredential,
		ResourceID: testResourceID,
		Operation:  resource.OperationGitRefUpdate,
		Subject:    requiredSubject(),
		Approval:   roleApproval(tenancy.RoleReviewer),
		ActionID:   testAction,
	}
}

func mutatedEvidence(mutate func(*verify.Evidence)) verify.Evidence {
	evidence := passingEvidence()
	mutate(&evidence)
	return evidence
}

func subjectWith(name string, kind verify.ValueKind, value string) verify.Subject {
	fields := append([]verify.SubjectField(nil), evidenceSubject().Fields...)
	for index := range fields {
		if fields[index].Name == name {
			fields[index].Kind = kind
			fields[index].Value = value
			return verify.Subject{Fields: fields}
		}
	}
	return verify.Subject{Fields: append(fields, verify.SubjectField{Name: name, Kind: kind, Value: value})}
}

func newIDSequence(prefix string) func() (verify.VerificationID, error) {
	var mu sync.Mutex
	next := 0
	return func() (verify.VerificationID, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return verify.VerificationID(fmt.Sprintf("%s-%d", prefix, next)), nil
	}
}

// fakePrincipals is a deterministic CurrentPrincipal.
type fakePrincipals struct {
	mu         sync.Mutex
	principals map[string]identity.Principal
	failures   map[string]error
	calls      []string
}

func newFakePrincipals() *fakePrincipals {
	return &fakePrincipals{
		principals: make(map[string]identity.Principal),
		failures:   make(map[string]error),
	}
}

func (fake *fakePrincipals) set(token string, principal identity.Principal, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.principals[token] = principal
	if err != nil {
		fake.failures[token] = err
		return
	}
	delete(fake.failures, token)
}

func (fake *fakePrincipals) Resolve(_ context.Context, token string) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, token)
	if err, ok := fake.failures[token]; ok {
		return identity.Principal{}, err
	}
	principal, ok := fake.principals[token]
	if !ok {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "unknown credential")
	}
	return principal, nil
}

func (fake *fakePrincipals) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// fakeBindings is a deterministic BindingReader.
type fakeBindings struct {
	mu      sync.Mutex
	binding resource.Binding
	err     error
	queries []verify.BindingQuery
}

func (fake *fakeBindings) set(binding resource.Binding, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.binding = binding
	fake.err = err
}

func (fake *fakeBindings) ReadBinding(_ context.Context, query verify.BindingQuery) (resource.Binding, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.queries = append(fake.queries, query)
	if fake.err != nil {
		return resource.Binding{}, fake.err
	}
	return fake.binding, nil
}

func (fake *fakeBindings) lastQuery() verify.BindingQuery {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) == 0 {
		return verify.BindingQuery{}
	}
	return fake.queries[len(fake.queries)-1]
}

func (fake *fakeBindings) queryCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.queries)
}

// fakeEvidence is a deterministic EvidenceReader.
type fakeEvidence struct {
	mu       sync.Mutex
	evidence []verify.Evidence
	err      error
	queries  []verify.EvidenceQuery
}

func (fake *fakeEvidence) set(evidence []verify.Evidence, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.evidence = append([]verify.Evidence(nil), evidence...)
	fake.err = err
}

func (fake *fakeEvidence) ReadEvidence(_ context.Context, query verify.EvidenceQuery) ([]verify.Evidence, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.queries = append(fake.queries, query)
	if fake.err != nil {
		return nil, fake.err
	}
	return append([]verify.Evidence(nil), fake.evidence...), nil
}

func (fake *fakeEvidence) lastQuery() verify.EvidenceQuery {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) == 0 {
		return verify.EvidenceQuery{}
	}
	return fake.queries[len(fake.queries)-1]
}

func (fake *fakeEvidence) queryCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.queries)
}

// fakeRecords is a deterministic RecordPort.
type fakeRecords struct {
	mu        sync.Mutex
	err       error
	resultFor func(verify.RecordUnit) (verify.RecordResult, error)
	units     []verify.RecordUnit
}

func (fake *fakeRecords) setError(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.err = err
}

func (fake *fakeRecords) setResultFor(resultFor func(verify.RecordUnit) (verify.RecordResult, error)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.resultFor = resultFor
}

func (fake *fakeRecords) Record(_ context.Context, unit verify.RecordUnit) (verify.RecordResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.units = append(fake.units, unit)
	if fake.err != nil {
		return verify.RecordResult{}, fake.err
	}
	if fake.resultFor != nil {
		return fake.resultFor(unit)
	}
	return verify.RecordResult{ID: unit.Record.ID}, nil
}

func (fake *fakeRecords) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.units)
}

func (fake *fakeRecords) unitAt(index int) verify.RecordUnit {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.units[index]
}

func (fake *fakeRecords) all() []verify.RecordUnit {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]verify.RecordUnit(nil), fake.units...)
}

// fakeMembership is a deterministic authorize.MembershipReader.
type fakeMembership struct {
	mu      sync.Mutex
	rows    authorize.MembershipRows
	err     error
	queries []membershipQuery
}

type membershipQuery struct {
	scope     tenancy.Scope
	principal id.PrincipalID
}

func (fake *fakeMembership) set(rows authorize.MembershipRows, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.rows = rows
	fake.err = err
}

func (fake *fakeMembership) ReadMembership(_ context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.queries = append(fake.queries, membershipQuery{scope: scope, principal: principal})
	if fake.err != nil {
		return authorize.MembershipRows{}, fake.err
	}
	return fake.rows, nil
}

func (fake *fakeMembership) queryCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.queries)
}

// fakeClock is a deterministic Clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (fake *fakeClock) Now() time.Time {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.now
}

type harness struct {
	principals *fakePrincipals
	bindings   *fakeBindings
	evidence   *fakeEvidence
	membership *fakeMembership
	records    *fakeRecords
	clock      *fakeClock
	verifier   *verify.Verifier
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	instance := &harness{
		principals: newFakePrincipals(),
		bindings:   &fakeBindings{},
		evidence:   &fakeEvidence{},
		membership: &fakeMembership{},
		records:    &fakeRecords{},
		clock:      &fakeClock{now: fixedNow},
	}
	instance.principals.set(actorCredential, actorPrincipal(), nil)
	instance.bindings.set(gitBinding(), nil)
	instance.evidence.set([]verify.Evidence{passingEvidence()}, nil)

	instance.rebuild(t)
	return instance
}

// useMembership rebuilds the verifier with the scoped membership reader, so a
// human actor's returned roles come from the current union in the verification
// scope and both membership rows are pinned on the record unit.
func (instance *harness) useMembership(t *testing.T) *fakeMembership {
	t.Helper()
	instance.rebuild(t)
	return instance.membership
}

func (instance *harness) rebuild(t *testing.T) {
	t.Helper()
	verifier, err := verify.New(verify.Config{
		Principals: instance.principals,
		Bindings:   instance.bindings,
		Evidence:   instance.evidence,
		Membership: instance.membership,
		Records:    instance.records,
		Clock:      instance.clock,
		NewID:      newIDSequence("verification"),
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	instance.verifier = verifier
}

func operationNamed(t *testing.T, name authorize.OperationName) authorize.Operation {
	t.Helper()
	operation, ok := authorize.LookupOperation(name)
	if !ok {
		t.Fatalf("operation %q is not registered", name)
	}
	return operation
}

// wantFaultClass asserts that err is a fault whose class is one of classes.
func wantFaultClass(t *testing.T, err error, classes ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal error, got nil")
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no declared fault class", err)
	}
	if slices.Contains(classes, class) {
		return
	}
	t.Fatalf("fault class = %q, want one of %v", class, classes)
}
