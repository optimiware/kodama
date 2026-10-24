package integration_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/identity"
	identityconsumer "github.com/optimiware/kodama/internal/identity/consumer"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
	"github.com/optimiware/kodama/internal/work"
)

// permissive is the worst-case upstream authority for the QA workload: it
// resolves the attempt and allows every permission. The QA work store's own
// transaction-bound check must still refuse everything but filing defects.
type permissive struct {
	scope   domain.Scope
	attempt domain.AttemptID
}

func (p permissive) ResolveActor(_ context.Context, _ domain.Scope, id string) (work.Actor, error) {
	return work.Actor{ID: id, Type: work.ActorWorkload}, nil
}

func (permissive) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}

const reviewer = "u_t040_reviewer"

// Test_qa_cannot_approve proves DQ-06 and AR-12: QA produces evidence and
// defects; it cannot approve a gate, complete or move work, and its identity
// cannot satisfy a human permission, whatever a report says and whatever an
// upstream authority would allow.
func Test_qa_cannot_approve(t *testing.T) {
	s := newStack(t, qatest.Options{})
	s.X.Grant(scopeA, reviewer, identity.RoleReviewer)
	ctx := context.Background()
	attempt := string(s.Run.ID())

	reader, err := identitypg.NewStore(s.X.Runtime, s.X.Events)
	if err != nil {
		t.Fatal(err)
	}
	std, err := identity.NewStandaloneAuthority(reader)
	if err != nil {
		t.Fatal(err)
	}
	target := identity.Target{TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID}

	t.Run("the identity authority gives the QA workload neither decision permission", func(t *testing.T) {
		workload := identity.Actor{ID: attempt, Type: identity.ActorWorkload}
		for _, p := range []identity.Permission{identity.PermGateDecide, identity.PermWorkComplete, identity.PermWorkWork, identity.PermWorkCancel} {
			if err := std.Authorize(ctx, workload, target, p); err == nil {
				t.Fatalf("the QA workload holds %s", p)
			}
		}
		// Control: the same authority does grant a real reviewer, so the denial
		// above is the authority's decision and not a broken fixture.
		human := identity.UserActor(reviewer)
		for _, p := range []identity.Permission{identity.PermGateDecide, identity.PermWorkComplete} {
			if err := std.Authorize(ctx, human, target, p); err != nil {
				t.Fatalf("the reviewer lacks %s: %v", p, err)
			}
		}
	})

	t.Run("the gate and work bridges resolve no actor for the QA attempt", func(t *testing.T) {
		ga, err := gatesadapters.NewAuthority(std, reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ga.ResolveActor(ctx, scopeA, attempt); !errors.Is(err, gates.ErrHidden) {
			t.Fatalf("a gate actor was resolved for the attempt: %v", err)
		}
		if err := ga.Authorize(ctx, gates.Actor{ID: attempt, Type: gates.ActorWorkload}, scopeA, gates.PermDecide); err == nil {
			t.Fatal("the QA workload may decide a gate")
		}
		if err := ga.Authorize(ctx, gates.Actor{ID: reviewer, Type: gates.ActorUser}, scopeA, gates.PermDecide); err != nil {
			t.Fatalf("control: the reviewer may decide a gate: %v", err)
		}
		wa, err := identityconsumer.NewWorkAuthority(std, reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wa.ResolveActor(ctx, scopeA, attempt); !errors.Is(err, work.ErrHidden) {
			t.Fatalf("a work actor was resolved for the attempt: %v", err)
		}
		if err := wa.Authorize(ctx, work.Actor{ID: attempt, Type: work.ActorWorkload}, scopeA, work.PermComplete); err == nil {
			t.Fatal("the QA workload may complete work")
		}
	})

	t.Run("a report cannot carry a decision", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		r := s.Run.ReportFor(s.Dep, s.finding("Order form shows a blank page", "/orders/new", 0))
		raw, err := r.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		forged := strings.Replace(string(raw), `{"deployment"`, `{"approved":true,"gateDecision":"approved","deployment"`, 1)
		if forged == string(raw) {
			t.Fatal("the fixture did not forge the report")
		}
		s.Run.WriteRaw([]byte(forged))
		if err := s.Run.Finish(); err != nil {
			t.Fatal(err)
		}
		_, err = s.record()
		is(t, err, qa.ErrInvalid)
		s.nothingFiled(t, scopeA)
	})

	t.Run("only the control plane or the attempt's own identity may map, never a person", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		for name, c := range map[string]qa.Caller{
			"a reviewer":      {Actor: qa.Actor{Type: qa.ActorUser, ID: reviewer}},
			"an operator":     {Actor: qa.Actor{Type: qa.ActorOperator, ID: "op"}},
			"another attempt": {Actor: qa.Actor{Type: qa.ActorWorkload, ID: "att_someone_else"}},
		} {
			cmd := s.Run.Command()
			cmd.Caller = c
			if _, err := s.Service.Record(ctx, cmd); !errors.Is(err, qa.ErrDenied) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		s.nothingFiled(t, scopeA)
	})

	t.Run("even an all-permissive upstream authority cannot move work through the QA store", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		res := s.mustRecord(t)
		subject := s.Run.Subject()
		before, err := s.Work(scopeA, subject)
		if err != nil {
			t.Fatal(err)
		}
		defect, err := s.Work(scopeA, res.Defects[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		// Two worst-case upstream authorities: one that resolves the attempt as a
		// workload and allows everything, and one that resolves the bound mapping
		// principal (whose analyst binding really holds the refine permission) as a
		// user and allows everything.
		for name, c := range map[string]struct {
			authority work.Authority
			principal string
		}{
			"the attempt as a workload":       {permissive{scope: scopeA, attempt: s.Run.ID()}, attempt},
			"the mapping principal as a user": {grantedUser{}, qatest.Mapper},
		} {
			t.Run(name, func(t *testing.T) {
				authority, principal := c.authority, c.principal
				svc, err := work.NewService(work.Config{Repository: s.WorkStore, Authority: authority})
				if err != nil {
					t.Fatal(err)
				}
				caller := work.Caller{PrincipalID: principal, RequestID: "req_t040"}
				// The edges that exist in the work module's table from each starting
				// state: those must be refused by authority, not by the table.
				edges := map[work.State][]work.State{
					work.StateActive:  {work.StateReady, work.StateReview, work.StateCancelled},
					work.StateBacklog: {work.StateReady, work.StateCancelled},
				}
				for _, to := range []work.State{work.StateBacklog, work.StateReady, work.StateActive, work.StateReview, work.StateDone, work.StateCancelled} {
					for name, target := range map[string]struct {
						id      domain.WorkItemID
						version domain.Version
						from    work.State
					}{"the item under test": {subject, before.Version, before.State}, "its defect": {defect.ID, defect.Version, defect.State}} {
						_, err := svc.Transition(ctx, work.TransitionCommand{Scope: scopeA, Caller: caller, ID: target.id, Expected: target.version,
							Request: work.TransitionRequest{To: to, Reason: "QA says so"}})
						if err == nil {
							t.Fatalf("the QA identity moved %s to %s", name, to)
						}
						if slices.Contains(edges[target.from], to) {
							if !errors.Is(err, work.ErrDenied) && !errors.Is(err, work.ErrUnavailable) {
								t.Fatalf("%s %s -> %s exists in the table, so authority must refuse it: %v", name, target.from, to, err)
							}
						} else if !errors.Is(err, work.ErrDenied) && !errors.Is(err, work.ErrNotHuman) && !errors.Is(err, work.ErrInvalidTransition) {
							t.Fatalf("%s %s -> %s: unexpected failure class: %v", name, target.from, to, err)
						}
					}
				}
				// Technical fields (assignment, blocking) are not QA's either.
				blocked := "QA blocks it"
				if _, err := svc.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: caller, ID: subject, Expected: before.Version,
					Patch: work.Patch{BlockedReason: work.Some(blocked)}}); !errors.Is(err, work.ErrDenied) {
					t.Fatalf("the QA identity edited a technical field: %v", err)
				}
				after, _ := s.Work(scopeA, subject)
				if after.Version != before.Version || after.State != before.State || after.BlockedReason != "" || after.Completion != nil {
					t.Fatalf("the item under test changed: %+v -> %+v", before, after)
				}
				if again, _ := s.Work(scopeA, res.Defects[0].ID); again.Version != defect.Version || again.State != work.StateBacklog {
					t.Fatalf("the defect changed state: %+v", again)
				}

			})
		}
	})

	t.Run("a mapped report leaves the work item under test and every gate decision untouched", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		subject := s.Run.Subject()
		before, _ := s.Work(scopeA, subject)
		histBefore := s.historyRows(subject)
		transitions := s.workEvents("work_item.transitioned")
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0), s.finding("Totals are wrong", "/orders", 1))
		res := s.mustRecord(t)

		after, _ := s.Work(scopeA, subject)
		if after.Version != before.Version || after.State != before.State || after.ContentRevision != before.ContentRevision || after.Completion != nil || after.ReviewSubject != nil {
			t.Fatalf("QA changed the work item under test: %+v -> %+v", before, after)
		}
		if s.historyRows(subject) != histBefore || s.workEvents("work_item.transitioned") != transitions {
			t.Fatal("QA wrote history or a transition for the item under test")
		}
		for _, d := range res.Defects {
			it, _ := s.Work(scopeA, d.ID)
			if it.State != work.StateBacklog || it.Completion != nil || it.ReviewSubject != nil || it.AssigneeID != "" || it.BlockedReason != "" {
				t.Fatalf("a QA defect is not a plain backlog defect: %+v", it)
			}
			if it.CreatedBy.Type != work.ActorUser || it.CreatedBy.ID != qatest.Mapper {
				t.Fatalf("a QA defect is not attributed to the mapping principal: %+v", it.CreatedBy)
			}
		}
		// Everything QA wrote to the event stream is an ordinary defect creation:
		// no transition and nothing of the gate or deployment lifecycle.
		var types []string
		rows := s.X.CountTenant(scopeA.TenantID, fmt.Sprintf(`SELECT count(*) FROM outbox WHERE event_type LIKE 'gate.%%'`))
		if rows != 0 {
			t.Fatalf("gate events exist after QA: %v", types)
		}
		if got := s.workEvents("work_item.created"); got != len(res.Defects) {
			t.Fatalf("created events: %d", got)
		}
		if s.workEvents("work_item.transitioned") != transitions {
			t.Fatal("QA transitioned work")
		}
	})
}
