package verify_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/resource"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/verify"
)

type fakeDecisionReader struct {
	ctx verify.DecisionContext
	err error
}

func (reader *fakeDecisionReader) ReadDecision(_ context.Context, _ verify.DecisionQuery) (verify.DecisionContext, error) {
	if reader.err != nil {
		return verify.DecisionContext{}, reader.err
	}
	return reader.ctx, nil
}

func baseApprovalDecision(now time.Time) verify.ApprovalDecision {
	return verify.ApprovalDecision{
		ID:          id.DecisionID("dec_0123456789abcdef0123456789abcdef"),
		Scope:       testScope(),
		TaskID:      id.TaskID("task-a"),
		ExecutionID: id.ExecutionID("exec-review-1"),
		ActionID:    id.ActionID("act-review-1"),
		Assignment:  roleApproval(tenancy.RoleReviewer),
		Operation:   string(resource.OperationGitRefUpdate),
		ResourceID:  testResourceID,
		Subject:     evidenceSubject(),
		Approver:    id.PrincipalID("human-reviewer"),
		ResolvedAt:  now.Add(-time.Hour),
		ExpiresAt:   now.Add(71 * time.Hour),
		Version:     1,
	}
}

func TestVerifyCrossExecutionDecisionIDSecurityMatrix(t *testing.T) {
	now := fixedNow

	newVerifier := func(t *testing.T, actor identity.Principal, reader verify.DecisionReader) (*verify.Verifier, *fakeRecords) {
		t.Helper()
		principals := newFakePrincipals()
		principals.set(actorCredential, actor, nil)
		bindings := &fakeBindings{}
		bindings.set(gitBinding(), nil)
		records := &fakeRecords{}
		verifier, err := verify.New(verify.Config{
			Principals: principals,
			Bindings:   bindings,
			Evidence:   &fakeEvidence{},
			Decisions:  reader,
			Records:    records,
			Clock:      &fakeClock{now: now},
			NewID:      newIDSequence("verification"),
		})
		if err != nil {
			t.Fatalf("verify.New: %v", err)
		}
		return verifier, records
	}

	execActor := identity.Principal{
		ID:          id.PrincipalID("exec-integrate-2"),
		Kind:        identity.KindExecution,
		ExecutionID: id.ExecutionID("exec-integrate-2"),
		Scope:       testScope(),
		Enabled:     true,
		Version:     1,
	}
	mergeService := identity.Principal{
		ID:      id.PrincipalID("merge-coordinator"),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Enabled: true,
		Version: 1,
	}

	makeAttempt := func(decisionID id.DecisionID) verify.Attempt {
		a := attempt()
		a.DecisionID = decisionID
		return a
	}

	t.Run("same task cross-execution approval succeeds and pins decision version", func(t *testing.T) {
		decision := baseApprovalDecision(now)
		reader := &fakeDecisionReader{ctx: verify.DecisionContext{
			Found:       true,
			Decision:    decision,
			TaskState:   "open",
			ActorTaskID: id.TaskID("task-a"),
		}}
		verifier, records := newVerifier(t, execActor, reader)
		record, err := verifier.Verify(context.Background(), makeAttempt(decision.ID))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !record.Approved || record.Result != verify.ResultApproved || record.DecisionID != decision.ID {
			t.Fatalf("record = %+v; want approved with decision_id=%q", record, decision.ID)
		}
		units := records.all()
		if len(units) != 1 || units[0].DecisionVersion != 1 {
			t.Fatalf("recorded units = %+v; want DecisionVersion=1", units)
		}
	})

	t.Run("same execution decision succeeds even when task-free", func(t *testing.T) {
		decision := baseApprovalDecision(now)
		decision.TaskID = ""
		decision.ExecutionID = execActor.ExecutionID
		reader := &fakeDecisionReader{ctx: verify.DecisionContext{
			Found:    true,
			Decision: decision,
		}}
		verifier, _ := newVerifier(t, execActor, reader)
		record, err := verifier.Verify(context.Background(), makeAttempt(decision.ID))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !record.Approved {
			t.Fatalf("expected approved=true for same-execution decision")
		}
	})

	t.Run("assigned service account succeeds with active merge request", func(t *testing.T) {
		decision := baseApprovalDecision(now)
		reader := &fakeDecisionReader{ctx: verify.DecisionContext{
			Found:           true,
			Decision:        decision,
			TaskState:       "open",
			ServiceAssigned: true,
		}}
		verifier, _ := newVerifier(t, mergeService, reader)
		a := makeAttempt(decision.ID)
		a.ActionID = ""
		record, err := verifier.Verify(context.Background(), a)
		if err != nil {
			t.Fatalf("Verify(service): %v", err)
		}
		if !record.Approved || record.DecisionID != decision.ID {
			t.Fatalf("record = %+v; want approved service verification", record)
		}
	})

	rejectCases := []struct {
		name   string
		actor  identity.Principal
		reader verify.DecisionReader
		mutate func(*verify.Attempt)
	}{
		{
			name:  "foreign task execution refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-b"),
			}},
		},
		{
			name:  "task-free execution borrowing tasked decision refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "open",
				ActorTaskID: "",
			}},
		},
		{
			name:  "task-free execution borrowing another task-free execution decision refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.TaskID = ""
					return d
				}(),
			}},
		},
		{
			name:  "cross-project decision refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.Scope.ProjectID = id.ProjectID("other-project")
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "cross-tenant decision refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.Scope.TenantID = id.TenantID("other-tenant")
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "expired decision at exact expires_at refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.ExpiresAt = now
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "expired decision past expires_at refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.ExpiresAt = now.Add(-time.Second)
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "revoked decision refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.RevokedAt = now.Add(-time.Minute)
					d.RevokedReason = "manual_revoke"
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "cancelled task decision refused even if revoked_at not set",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "cancelled",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "wrong approval role refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
			mutate: func(a *verify.Attempt) {
				a.Approval = request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator}
			},
		},
		{
			name:  "mismatched resource refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found: true,
				Decision: func() verify.ApprovalDecision {
					d := baseApprovalDecision(now)
					d.ResourceID = resource.ResourceID("other-repo")
					return d
				}(),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
		},
		{
			name:  "mismatched subject candidate refused",
			actor: execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
			mutate: func(a *verify.Attempt) {
				for i := range a.Subject.Fields {
					if a.Subject.Fields[i].Name == "candidate" {
						a.Subject.Fields[i].Value = "different-candidate-sha"
					}
				}
			},
		},
		{
			name:  "human actor presenting decision_id refused",
			actor: humanActor(),
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:       true,
				Decision:    baseApprovalDecision(now),
				TaskState:   "open",
				ActorTaskID: id.TaskID("task-a"),
			}},
			mutate: func(a *verify.Attempt) { a.ActionID = "" },
		},
		{
			name:  "unassigned service account refused",
			actor: mergeService,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{
				Found:           true,
				Decision:        baseApprovalDecision(now),
				TaskState:       "open",
				ServiceAssigned: false,
			}},
			mutate: func(a *verify.Attempt) { a.ActionID = "" },
		},
		{
			name:   "nonexistent forged decision_id refused",
			actor:  execActor,
			reader: &fakeDecisionReader{ctx: verify.DecisionContext{Found: false}},
		},
		{
			name:   "nil DecisionReader fails closed",
			actor:  execActor,
			reader: nil,
		},
	}

	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			verifier, records := newVerifier(t, tc.actor, tc.reader)
			a := makeAttempt(baseApprovalDecision(now).ID)
			if tc.mutate != nil {
				tc.mutate(&a)
			}
			record, err := verifier.Verify(context.Background(), a)
			if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
				t.Fatalf("Verify err = %v; want forbidden", err)
			}
			if record.Approved || record.Result != verify.ResultRefused {
				t.Fatalf("record = %+v; want refused and approved=false", record)
			}
			if record.DecisionID != a.DecisionID {
				t.Errorf("record.DecisionID = %q; want %q for audit provenance", record.DecisionID, a.DecisionID)
			}
			units := records.all()
			if len(units) != 1 || units[0].DecisionVersion != 0 {
				t.Fatalf("refused verification should record unit with DecisionVersion=0; got %+v", units)
			}
		})
	}
}
