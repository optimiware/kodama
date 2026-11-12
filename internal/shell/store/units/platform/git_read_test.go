package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// readBindings is the enforcing Git service's binding: it declares the
// repository-level read operation alongside the ref-update effect.
type readBindings struct{}

func (readBindings) ReadBinding(_ context.Context, query verify.BindingQuery) (resource.Binding, error) {
	return resource.Binding{
		ID:               resource.BindingID(testBinding),
		ResourceID:       query.ResourceID,
		Scope:            query.Scope,
		EnforcingService: service.ServiceID(query.ServiceID),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          7,
	}, nil
}

// capturingRecords observes the record unit the real verifier built and can
// simulate a concurrent change before the adapter commits it.
type capturingRecords struct {
	inner  verify.RecordPort
	units  []verify.RecordUnit
	before func(verify.RecordUnit) error
}

func (records *capturingRecords) Record(ctx context.Context, unit verify.RecordUnit) (verify.RecordResult, error) {
	records.units = append(records.units, unit)
	if records.before != nil {
		if err := records.before(unit); err != nil {
			return verify.RecordResult{}, err
		}
	}
	return records.inner.Record(ctx, unit)
}

// readVerifier builds the real verifier with the real store-backed membership
// reader and record adapter for one human Git read actor.
func readVerifier(t *testing.T, s *memoryStoreAlias) (*verify.Verifier, *capturingRecords) {
	t.Helper()
	records, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	memberships, err := identitystore.New(identitystore.Config{Executor: s, Scanner: s, Clock: fixedClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	observer := &capturingRecords{inner: records}
	// The credential resolves at tenant scope with the tenant-wide viewer grant
	// only; the exact-project engineer grant must come from the scoped read.
	actor := identity.Principal{
		ID:      testActor,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant},
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	next := 0
	verifier, err := verify.New(verify.Config{
		Principals: fixedPrincipals{principal: actor},
		Bindings:   readBindings{},
		Evidence:   emptyEvidence{},
		Membership: memberships,
		Records:    observer,
		Clock:      fixedClock{},
		NewID: func() (verify.VerificationID, error) {
			next++
			return verify.VerificationID(fmt.Sprintf("verification-%d", next)), nil
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	return verifier, observer
}

// seedGitReadFixtures seeds the calling service, its binding and both
// membership rows of the human read actor.
func seedGitReadFixtures(t *testing.T, s *memoryStoreAlias) {
	t.Helper()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedMembershipRoles(t, s, tenancy.Scope{TenantID: testTenant}, "membership-tenant", testActor, []tenancy.Role{tenancy.RoleViewer})
	seedMembershipRoles(t, s, testScope(), "membership-project", testActor, []tenancy.Role{tenancy.RoleEngineer})
}

func gitReadAttempt() verify.Attempt {
	return verify.Attempt{
		Scope:      testScope(),
		Service:    testServicePrincipal(),
		Credential: "actor-credential",
		ResourceID: resource.ResourceID(testResource),
		Operation:  resource.OperationGitRead,
		Subject:    verify.Subject{},
	}
}

// TestRealVerifierGitReadCommitsScopedRoles asserts a repository read through
// the real verifier, membership reader and record adapter: the record is
// approved with the union of both membership rows, the durable verification
// carries the empty read subject, and the principal_enabled authority read pins
// both rows.
func TestRealVerifierGitReadCommitsScopedRoles(t *testing.T) {
	s := newStore()
	seedGitReadFixtures(t, s)
	verifier, observer := readVerifier(t, s)

	record, err := verifier.Verify(context.Background(), gitReadAttempt())
	if err != nil {
		t.Fatalf("real git.read Verify: %v", err)
	}
	if record.Result != verify.ResultApproved || record.Approved {
		t.Errorf("record = %#v, want an approved read without an approval fact", record)
	}
	wantRoles := []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}
	if !reflect.DeepEqual(record.Actor.Roles, wantRoles) {
		t.Errorf("verified roles = %v, want the scoped union %v", record.Actor.Roles, wantRoles)
	}
	if len(observer.units) != 1 {
		t.Fatalf("record units = %d, want 1", len(observer.units))
	}
	read := readAuthority(t, observer.units[0], authorize.CheckPrincipalEnabled)
	wantTargets := []authorize.AuthorityTarget{
		{Scope: testScope(), RowID: "membership-project", ExpectedVersion: 1},
		{Scope: tenancy.Scope{TenantID: testTenant}, RowID: "membership-tenant", ExpectedVersion: 1},
	}
	if !reflect.DeepEqual(read.Targets, wantTargets) {
		t.Errorf("principal_enabled pins = %+v, want both rows %+v", read.Targets, wantTargets)
	}
	row, err := s.Read(context.Background(), testScope(), "verifications", string(record.ID))
	if err != nil {
		t.Fatalf("read committed verification: %v", err)
	}
	if row.Values["operation"] != string(resource.OperationGitRead) {
		t.Errorf("durable read operation = %v, want git.read", row.Values["operation"])
	}
	subjectBytes, ok := row.Values["subject"].([]byte)
	if !ok {
		t.Fatalf("durable read subject = %#v, want JSON bytes", row.Values["subject"])
	}
	var stored verify.Subject
	if err := json.Unmarshal(subjectBytes, &stored); err != nil {
		t.Fatalf("decode durable read subject: %v", err)
	}
	if len(stored.Fields) != 0 {
		t.Errorf("durable read subject = %#v, want an empty subject", stored)
	}
}

// TestRealVerifierGitReadPinsBothRowsBeforeCommit mutates the tenant-wide
// membership version after the capability observed it and before the adapter
// commits. The commit refuses with version_conflict and persists no
// verification row, proving both rows are pinned.
func TestRealVerifierGitReadPinsBothRowsBeforeCommit(t *testing.T) {
	s := newStore()
	seedGitReadFixtures(t, s)
	verifier, observer := readVerifier(t, s)
	observer.before = func(verify.RecordUnit) error {
		observer.before = nil
		updateVersion(t, s, tenancy.Scope{TenantID: testTenant}, "memberships", "membership-tenant", 1)
		return nil
	}

	record, err := verifier.Verify(context.Background(), gitReadAttempt())
	requireClass(t, err, fault.VersionConflict)
	if record.ID != "" {
		t.Errorf("record id = %q, want no committed read fact", record.ID)
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", "verification-1"); err == nil {
		t.Error("a refused read verification persisted a row")
	}
}

func readAuthority(t *testing.T, unit verify.RecordUnit, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	for _, read := range unit.Authority {
		if read.Check == check {
			return read
		}
	}
	t.Fatalf("record unit carries no %q authority read", check)
	return authorize.AuthorityRead{}
}
