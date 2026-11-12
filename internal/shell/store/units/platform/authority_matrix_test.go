package platform_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// The authority matrix pins the allow/refuse expectation for every principal
// kind each platform port authenticates. The membership union may only ADD
// grants: no single-row refusal becomes a silent pass, no pin is dropped, and
// the execution principal keeps its exemption (its own
// execution_running/action_deadline guards decide).

// TestAuthorityMatrixIngress covers every ingress source kind from the declared
// execution matrix: membership reads for a human or service principal and the
// execution exemption for an execution principal.
func TestAuthorityMatrixIngress(t *testing.T) {
	cases := []struct {
		name      string
		kind      event.SourceKind
		principal id.PrincipalID
		execution id.ExecutionID
		seed      func(t *testing.T, s *memoryStoreAlias)
		wantClass fault.Class // "" allows the commit
	}{
		{
			name: "human engineer", kind: event.SourceShell, principal: testActor,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name: "human viewer", kind: event.SourceShell, principal: testActor, wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleViewer})
			},
		},
		{
			name: "human empty roles", kind: event.SourceShell, principal: testActor, wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, nil)
			},
		},
		{name: "human no row", kind: event.SourceShell, principal: testActor, wantClass: fault.Forbidden},
		{
			name: "human other tenant", kind: event.SourceShell, principal: testActor, wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedMembershipRoles(t, s, otherScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name: "service enabled engineer", kind: event.SourceService, principal: testService,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedService(t, s, testScope(), testService, "enabled")
			},
		},
		{
			name: "service disabled", kind: event.SourceService, principal: testService, wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, testScope(), testService, "disabled", []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{name: "service no row", kind: event.SourceService, principal: testService, wantClass: fault.Forbidden},
		{
			name: "service other tenant", kind: event.SourceService, principal: testService, wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedService(t, s, otherScope(), testService, "enabled")
			},
		},
		{
			name: "execution running", kind: event.SourceExecution, principal: testPrincipal, execution: testExecution,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
			},
		},
		{
			name: "execution succeeded", kind: event.SourceExecution, principal: testPrincipal, execution: testExecution,
			wantClass: fault.VersionConflict,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "succeeded")
			},
		},
		{name: "execution no row", kind: event.SourceExecution, principal: testPrincipal, execution: testExecution, wantClass: fault.Forbidden},
		{
			name: "execution other tenant", kind: event.SourceExecution, principal: testPrincipal, execution: testExecution,
			wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedExecution(t, s, otherScope(), testExecution, testPrincipal, "running")
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			if testCase.seed != nil {
				testCase.seed(t, s)
			}
			adapter, err := platform.NewIngressAdapter(s, s)
			if err != nil {
				t.Fatalf("NewIngressAdapter: %v", err)
			}
			unit := ingressUnit(t, testCase.kind, testCase.principal, testCase.execution)
			_, err = adapter.Ingest(context.Background(), unit)
			if testCase.wantClass == "" {
				if err != nil {
					t.Fatalf("Ingest: %v", err)
				}
				if events := s.Events(testScope()); len(events) != 1 {
					t.Errorf("events = %d, want 1", len(events))
				}
				return
			}
			requireClass(t, err, testCase.wantClass)
			assertIngressRefusedCommittedNothing(t, s, unit)
		})
	}
}

// recordUnitForActor rebuilds the standard actor.verify unit for one actor
// kind: the principal-enablement read names the actor, and the execution and
// action targets exist only for an execution actor (a human or service actor is
// exempt from those reads).
func recordUnitForActor(t *testing.T, actor identity.Principal) verify.RecordUnit {
	t.Helper()
	unit := recordUnit(t)
	unit.Record.Actor = actor
	unit.Record.ExecutionID = actor.ExecutionID
	if actor.Kind != identity.KindExecution {
		unit.Record.ActionID = ""
	}
	for index := range unit.Authority {
		read := &unit.Authority[index]
		switch read.Check {
		case authorize.CheckPrincipalEnabled:
			read.PrincipalID = actor.ID
		case authorize.CheckExecutionRunning:
			read.TargetID = string(actor.ExecutionID)
		case authorize.CheckActionDeadline:
			if actor.Kind == identity.KindExecution {
				read.TargetID = string(testAction)
			} else {
				read.TargetID = ""
			}
		}
	}
	return unit
}

func humanAuthorityActor() identity.Principal {
	return identity.Principal{ID: testActor, Kind: identity.KindHuman, Scope: testScope(), Enabled: true, Version: 1}
}

func serviceAuthorityActor() identity.Principal {
	return identity.Principal{ID: testService, Kind: identity.KindService, Scope: testScope(), Enabled: true, Version: 1}
}

func executionAuthorityActor() identity.Principal {
	return identity.Principal{
		ID: testPrincipal, Kind: identity.KindExecution, Scope: testScope(),
		Enabled: true, Version: 1, ExecutionID: testExecution,
	}
}

// TestAuthorityMatrixRecord covers the actor.verify commit boundary for the
// calling service's enablement and for every actor kind: a human membership, a
// service row and an execution row, including disabled, role-less, missing and
// other-tenant states.
func TestAuthorityMatrixRecord(t *testing.T) {
	type matrixCase struct {
		name      string
		actor     identity.Principal
		seed      func(t *testing.T, s *memoryStoreAlias)
		wantClass fault.Class
	}
	seedCallingService := func(roles ...tenancy.Role) func(t *testing.T, s *memoryStoreAlias) {
		return func(t *testing.T, s *memoryStoreAlias) {
			seedServiceRoles(t, s, testScope(), testService, "enabled", roles)
			seedBinding(t, s, testScope(), testBinding, 7)
		}
	}
	cases := []matrixCase{
		{
			name: "calling service enabled engineer", actor: executionAuthorityActor(),
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "calling service no roles", actor: executionAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService()(t, s)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "calling service disabled", actor: executionAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, testScope(), testService, "disabled", []tenancy.Role{tenancy.RoleEngineer})
				seedBinding(t, s, testScope(), testBinding, 7)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "calling service no row", actor: executionAuthorityActor(), wantClass: fault.VersionConflict,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedBinding(t, s, testScope(), testBinding, 7)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "calling service other tenant", actor: executionAuthorityActor(), wantClass: fault.VersionConflict,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, otherScope(), testService, "enabled", []tenancy.Role{tenancy.RoleEngineer})
				seedBinding(t, s, testScope(), testBinding, 7)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "human actor engineer", actor: humanAuthorityActor(),
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			// actor.verify carries no human-role requirement: the verifier's
			// own authorization decides, so the commit boundary only requires a
			// current human membership row.
			name: "human actor viewer", actor: humanAuthorityActor(),
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleViewer})
			},
		},
		{
			name: "human actor empty roles", actor: humanAuthorityActor(),
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedMembershipRoles(t, s, testScope(), testMembership, testActor, nil)
			},
		},
		{
			name: "human actor no row", actor: humanAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
			},
		},
		{
			name: "human actor other tenant", actor: humanAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedMembershipRoles(t, s, otherScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name: "service actor enabled", actor: serviceAuthorityActor(),
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
			},
		},
		{
			name: "service actor disabled", actor: serviceAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, testScope(), testService, "disabled", []tenancy.Role{tenancy.RoleEngineer})
				seedBinding(t, s, testScope(), testBinding, 7)
			},
		},
		{
			name: "execution actor succeeded", actor: executionAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
				seedExecution(t, s, testScope(), testExecution, testPrincipal, "succeeded")
				seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			},
		},
		{
			name: "execution actor no row", actor: executionAuthorityActor(), wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedCallingService(tenancy.RoleEngineer)(t, s)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			testCase.seed(t, s)
			adapter, err := platform.NewRecordAdapter(s, s)
			if err != nil {
				t.Fatalf("NewRecordAdapter: %v", err)
			}
			unit := recordUnitForActor(t, testCase.actor)
			_, err = adapter.Record(context.Background(), unit)
			if testCase.wantClass == "" {
				if err != nil {
					t.Fatalf("Record: %v", err)
				}
				if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err != nil {
					t.Errorf("verification row was not committed: %v", err)
				}
				return
			}
			requireClass(t, err, testCase.wantClass)
			assertRecordRefusedCommittedNothing(t, s, unit)
		})
	}
}

// TestAuthorityMatrixReport covers the verified-report commit boundary for the
// reporting service's enablement and union roles.
func TestAuthorityMatrixReport(t *testing.T) {
	cases := []struct {
		name      string
		seed      func(t *testing.T, s *memoryStoreAlias)
		wantClass fault.Class
	}{
		{
			name: "service enabled engineer",
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedService(t, s, testScope(), testService, "enabled")
			},
		},
		{
			name: "service no roles", wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, testScope(), testService, "enabled", nil)
			},
		},
		{
			name: "service disabled", wantClass: fault.Forbidden,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedServiceRoles(t, s, testScope(), testService, "disabled", []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name: "service other tenant", wantClass: fault.VersionConflict,
			seed: func(t *testing.T, s *memoryStoreAlias) {
				seedService(t, s, otherScope(), testService, "enabled")
			},
		},
		{
			name: "service no row", wantClass: fault.VersionConflict,
			seed: func(t *testing.T, s *memoryStoreAlias) {},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			testCase.seed(t, s)
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
			if testCase.wantClass == "" {
				if err != nil {
					t.Fatalf("Report: %v", err)
				}
				if events := s.Events(testScope()); len(events) != 1 {
					t.Errorf("events = %d, want 1", len(events))
				}
				return
			}
			requireClass(t, err, testCase.wantClass)
			if _, found := s.Receipt(unit.Receipt.Key); found {
				t.Error("a refused report committed its receipt")
			}
			if audits := s.Audit(testScope()); len(audits) != 0 {
				t.Errorf("audit rows = %d after refusal, want 0", len(audits))
			}
			if events := s.Events(testScope()); len(events) != 0 {
				t.Errorf("events = %d after refusal, want 0", len(events))
			}
		})
	}
}
