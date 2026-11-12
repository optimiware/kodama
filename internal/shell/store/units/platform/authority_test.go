package platform_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type authorityClock struct{}

func (authorityClock) Now() time.Time { return fixedNow }

// TestIngressMembershipRoleRequirement asserts the commit boundary re-evaluates
// the stored membership roles: a role downgrade that lands after the
// capability's read is refused before any write, while the required role
// commits.
func TestIngressMembershipRoleRequirement(t *testing.T) {
	cases := []struct {
		name    string
		roles   []tenancy.Role
		wantErr bool
	}{
		{name: "downgraded role is refused", roles: []tenancy.Role{tenancy.RoleViewer}, wantErr: true},
		{name: "required role commits", roles: []tenancy.Role{tenancy.RoleEngineer}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedMembershipRoles(t, s, testScope(), testMembership, testActor, testCase.roles)
			adapter, err := platform.NewIngressAdapter(s, s)
			if err != nil {
				t.Fatalf("NewIngressAdapter: %v", err)
			}
			unit := ingressUnit(t, event.SourceShell, testActor, "")
			_, err = adapter.Ingest(context.Background(), unit)
			if testCase.wantErr {
				requireClass(t, err, fault.Forbidden)
				if _, found := s.Receipt(unit.Receipt.Key); found {
					t.Error("a role-refused ingress committed its receipt")
				}
				if audits := s.Audit(testScope()); len(audits) != 0 {
					t.Errorf("audit rows = %d after role refusal, want 0", len(audits))
				}
				if events := s.Events(testScope()); len(events) != 0 {
					t.Errorf("events = %d after role refusal, want 0", len(events))
				}
				return
			}
			if err != nil {
				t.Fatalf("Ingest with the required role: %v", err)
			}
			if events := s.Events(testScope()); len(events) != 1 {
				t.Errorf("events = %d, want 1", len(events))
			}
		})
	}
}

// TestRecordServiceRoleRequirement asserts a verification record re-evaluates
// the calling service's stored roles and refuses a role-lacking service with no
// verification row, receipt, audit or event persisted.
func TestRecordServiceRoleRequirement(t *testing.T) {
	cases := []struct {
		name    string
		roles   []tenancy.Role
		wantErr bool
	}{
		{name: "service lacks the required role", roles: []tenancy.Role{}, wantErr: true},
		{name: "service holds the required role", roles: []tenancy.Role{tenancy.RoleEngineer}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedServiceRoles(t, s, testScope(), testService, "enabled", testCase.roles)
			seedBinding(t, s, testScope(), testBinding, 7)
			seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
			seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			adapter, err := platform.NewRecordAdapter(s, s)
			if err != nil {
				t.Fatalf("NewRecordAdapter: %v", err)
			}
			unit := recordUnit(t)
			_, err = adapter.Record(context.Background(), unit)
			if testCase.wantErr {
				requireClass(t, err, fault.Forbidden)
				if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
					t.Error("a role-refused record persisted its verification row")
				}
				if _, found := s.Receipt(unit.Receipt.Key); found {
					t.Error("a role-refused record committed its receipt")
				}
				if audits := s.Audit(testScope()); len(audits) != 0 {
					t.Errorf("audit rows = %d after role refusal, want 0", len(audits))
				}
				if events := s.Events(testScope()); len(events) != 0 {
					t.Errorf("events = %d after role refusal, want 0", len(events))
				}
				return
			}
			if err != nil {
				t.Fatalf("Record with the required role: %v", err)
			}
			if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err != nil {
				t.Errorf("verification row was not committed: %v", err)
			}
		})
	}
}

// TestReportServiceRoleRequirement asserts a verified report re-evaluates the
// reporting service's stored roles and refuses a role-lacking service with no
// receipt or accepted event persisted.
func TestReportServiceRoleRequirement(t *testing.T) {
	cases := []struct {
		name    string
		roles   []tenancy.Role
		wantErr bool
	}{
		{name: "service lacks the required role", roles: []tenancy.Role{}, wantErr: true},
		{name: "service holds the required role", roles: []tenancy.Role{tenancy.RoleEngineer}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedServiceRoles(t, s, testScope(), testService, "enabled", testCase.roles)
			seedBinding(t, s, testScope(), testBinding, 7)
			seedVerification(t, s, testScope(), testVerification)
			mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
				"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
			})
			adapter, err := platform.NewReportAdapter(s, s)
			if err != nil {
				t.Fatalf("NewReportAdapter: %v", err)
			}
			unit := reportUnit(t)
			_, err = adapter.Report(context.Background(), unit)
			if testCase.wantErr {
				requireClass(t, err, fault.Forbidden)
				if _, found := s.Receipt(unit.Receipt.Key); found {
					t.Error("a role-refused report committed its receipt")
				}
				if events := s.Events(testScope()); len(events) != 0 {
					t.Errorf("events = %d after role refusal, want 0", len(events))
				}
				return
			}
			if err != nil {
				t.Fatalf("Report with the required role: %v", err)
			}
			if events := s.Events(testScope()); len(events) != 1 {
				t.Errorf("events = %d, want 1", len(events))
			}
		})
	}
}

// TestMembershipRoleSemanticsMatchAuthorityReader cross-checks the commit
// boundary's membership role decision against the identity AuthorityReader
// for the same seeded rows. Both must allow exactly the same grants.
func TestMembershipRoleSemanticsMatchAuthorityReader(t *testing.T) {
	cases := []struct {
		name      string
		kind      event.SourceKind
		principal id.PrincipalID
		roles     []tenancy.Role
	}{
		{name: "human with the required role", kind: event.SourceShell, principal: testActor, roles: []tenancy.Role{tenancy.RoleEngineer}},
		{name: "human downgraded to viewer", kind: event.SourceShell, principal: testActor, roles: []tenancy.Role{tenancy.RoleViewer}},
		{name: "service with the required role", kind: event.SourceService, principal: testService, roles: []tenancy.Role{tenancy.RoleEngineer}},
		{name: "service with no roles", kind: event.SourceService, principal: testService, roles: []tenancy.Role{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			if testCase.kind == event.SourceShell {
				seedMembershipRoles(t, s, testScope(), testMembership, testCase.principal, testCase.roles)
			} else {
				seedServiceRoles(t, s, testScope(), testCase.principal, "enabled", testCase.roles)
			}
			reader, err := identitystore.New(identitystore.Config{
				Executor: s,
				Scanner:  s,
				Clock:    authorityClock{},
			})
			if err != nil {
				t.Fatalf("identity.New: %v", err)
			}
			operation, ok := authorize.LookupOperation("event.publish")
			if !ok {
				t.Fatal("event.publish is not registered")
			}
			read := authorize.AuthorityRead{
				Operation:   operation.Name,
				Required:    operation.Requires,
				Check:       authorize.CheckMembership,
				Scope:       testScope(),
				PrincipalID: testCase.principal,
			}
			facts, err := reader.Read(context.Background(), []authorize.AuthorityRead{read})
			if err != nil {
				t.Fatalf("AuthorityReader.Read: %v", err)
			}
			if len(facts) != 1 {
				t.Fatalf("facts = %d, want 1", len(facts))
			}

			adapter, err := platform.NewIngressAdapter(s, s)
			if err != nil {
				t.Fatalf("NewIngressAdapter: %v", err)
			}
			unit := ingressUnit(t, testCase.kind, testCase.principal, "")
			_, ingestErr := adapter.Ingest(context.Background(), unit)
			if allowed := ingestErr == nil; allowed != facts[0].Allowed {
				t.Fatalf("adapter allowed = %t, AuthorityReader allowed = %t (%v)", allowed, facts[0].Allowed, ingestErr)
			}
		})
	}
}
