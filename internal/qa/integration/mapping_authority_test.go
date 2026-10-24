package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/adapters"
	"github.com/optimiware/kodama/internal/qa/qatest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// grantedUser is the worst-case upstream work authority: it resolves the
// mapping principal as a user and allows every permission. Only the QA work
// store's own transaction-bound check, which reads the identity module's
// current bindings, stands between it and a mutation.
type grantedUser struct{}

func (grantedUser) ResolveActor(_ context.Context, _ domain.Scope, id string) (work.Actor, error) {
	return work.Actor{ID: id, Type: work.ActorUser}, nil
}

func (grantedUser) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}

// revokeAfterSubject revokes the mapping authority as soon as the subject was
// read: the revocation lands between the service's checks and the filing.
type revokeAfterSubject struct {
	qa.Defects
	revoke func()
}

func (r revokeAfterSubject) Subject(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (qa.SubjectFacts, error) {
	f, err := r.Defects.Subject(ctx, scope, id)
	r.revoke()
	return f, err
}

// Test_qa_mapping_authority proves AR-05 for the mapping itself. Mapping a
// finished QA attempt's report is a trusted control-plane operation, not a
// runtime tool action: the runtime grant model offers no such action and a
// finished attempt has no authority left, so a workload (the attempt's own
// included), a person or an operator may not ask. The defects themselves are
// filed under the work module's own current authority of the mapping principal
// (identity-backed, narrowed to read, create and evidence links), which is
// checked before any pin or write and again inside every mutating transaction.
func Test_qa_mapping_authority(t *testing.T) {
	ctx := context.Background()

	t.Run("a workload or an unattributed caller is denied before any pin or work write", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))

		// The real runtime authority has nothing to offer the finished attempt:
		// no grant can stand in for a mapping permission.
		for _, a := range []runtimeauth.Action{runtimeauth.ActionToolInvoke, runtimeauth.ActionDeploymentCall, runtimeauth.ActionContextRead} {
			if _, err := s.Auth.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: s.Run.ID(), Bearer: s.Run.Attempt.Token, Action: a}); err == nil {
				t.Fatalf("the finished QA attempt still holds %s", a)
			}
		}

		for name, c := range map[string]qa.Caller{
			"the attempt's own workload":     {Actor: qa.Actor{Type: qa.ActorWorkload, ID: string(s.Run.ID())}, RequestID: "req_t040"},
			"a workload under the system id": {Actor: qa.Actor{Type: qa.ActorWorkload, ID: "qa-reconciler"}, RequestID: "req_t040"},
			"a caller without authority":     {},
			"a reviewer":                     {Actor: qa.Actor{Type: qa.ActorUser, ID: reviewer}},
			"an operator":                    {Actor: qa.Actor{Type: qa.ActorOperator, ID: "op"}},
		} {
			cmd := s.Run.Command()
			cmd.Caller = c
			if _, err := s.Service.Record(ctx, cmd); !errors.Is(err, qa.ErrDenied) {
				t.Fatalf("%s: %v", name, err)
			}
			s.nothingFiled(t, scopeA)
		}

		// A revoked runtime grant changes nothing either way: the workload is
		// still denied, and the control plane does not depend on that grant.
		if err := s.Auth.Revoke(ctx, scopeA, s.Run.ID()); err != nil {
			t.Fatal(err)
		}
		cmd := s.Run.Command()
		cmd.Caller = qa.Caller{Actor: qa.Actor{Type: qa.ActorWorkload, ID: string(s.Run.ID())}}
		if _, err := s.Service.Record(ctx, cmd); !errors.Is(err, qa.ErrDenied) {
			t.Fatalf("a revoked workload: %v", err)
		}
		s.nothingFiled(t, scopeA)
		if res := s.mustRecord(t); len(res.Defects) != 1 || !res.Defects[0].Created {
			t.Fatalf("the control plane could not map after the attempt's grant was revoked: %+v", res)
		}
	})

	t.Run("a revoked mapping authority files nothing and pins nothing", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		s.RevokeMapper(scopeA)
		if _, err := s.record(); !errors.Is(err, qa.ErrDenied) {
			t.Fatalf("got %v", err)
		}
		s.nothingFiled(t, scopeA)
		if _, err := s.Defects.Subject(ctx, scopeA, s.Run.Subject()); !errors.Is(err, qa.ErrDenied) {
			t.Fatalf("a revoked principal reads the subject: %v", err)
		}

		// Control: restoring the binding restores the mapping, so the denial was
		// the identity module's current decision and not a broken fixture.
		s.X.Grant(scopeA, qatest.Mapper, identity.RoleAnalyst)
		if res := s.mustRecord(t); len(res.Defects) != 1 || !res.Defects[0].Created {
			t.Fatalf("after the grant: %+v", res)
		}
	})

	t.Run("a binding without the filing rights cannot file defects", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		s.RevokeMapper(scopeA)
		s.X.Grant(scopeA, qatest.Mapper, identity.RoleViewer) // read only
		if _, err := s.record(); !errors.Is(err, qa.ErrDenied) {
			t.Fatalf("got %v", err)
		}
		s.nothingFiled(t, scopeA)
	})

	t.Run("an authority revoked after the subject was read is refused before the first pin", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		svc, err := qa.NewService(qa.Config{Attempts: s.Attempts, Deployments: s.Deploys, Artifacts: s.Evidence,
			Defects:      revokeAfterSubject{Defects: s.Defects, revoke: func() { s.RevokeMapper(scopeA) }},
			ReportOutput: qatest.ReportOutput, DeploymentInput: qatest.DeploymentInput})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Record(ctx, s.Run.Command()); !errors.Is(err, qa.ErrDenied) {
			t.Fatalf("got %v", err)
		}
		s.nothingFiled(t, scopeA)
	})

	t.Run("the work store rechecks the identity module inside every mutating transaction", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		// The upstream authority allows everything for the mapping principal.
		svc, err := work.NewService(work.Config{Repository: s.WorkStore, Authority: grantedUser{}})
		if err != nil {
			t.Fatal(err)
		}
		create := func() error {
			_, err := svc.Create(ctx, work.CreateCommand{Scope: scopeA, Caller: work.Caller{PrincipalID: qatest.Mapper, RequestID: "req_t040"},
				Kind: work.KindDefect, Title: "tx guard probe", Spec: "probe"})
			return err
		}
		if err := create(); err != nil {
			t.Fatalf("control: the bound mapping principal could not file a defect: %v", err)
		}
		before := s.DefectRows(scopeA)
		s.RevokeMapper(scopeA)
		// Identity answers a principal without a binding with "hidden".
		if err := create(); !errors.Is(err, work.ErrDenied) && !errors.Is(err, work.ErrHidden) {
			t.Fatalf("a revoked principal filed a defect through an upstream allow: %v", err)
		}
		if s.DefectRows(scopeA) != before {
			t.Fatal("a revoked principal wrote a defect")
		}
	})

	t.Run("a store without a permission guard and a missing authority fail closed", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		if _, err := adapters.NewWork(s.WorkStore, nil, qatest.Mapper, nil); err == nil {
			t.Fatal("a mapping adapter without an authority was constructed")
		}
		if _, err := adapters.NewWork(s.WorkStore, s.WorkAuthority, "", nil); err == nil {
			t.Fatal("a mapping adapter without a principal was constructed")
		}
		unguarded, err := workpg.NewStore(s.X.Runtime, s.X.Events, adapters.WorkGuards(s.ArtStore, nil))
		if err != nil {
			t.Fatal(err)
		}
		defects, err := adapters.NewWork(unguarded, s.WorkAuthority, qatest.Mapper, nil)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := qa.NewService(qa.Config{Attempts: s.Attempts, Deployments: s.Deploys, Artifacts: s.Evidence, Defects: defects,
			ReportOutput: qatest.ReportOutput, DeploymentInput: qatest.DeploymentInput})
		if err != nil {
			t.Fatal(err)
		}
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		if _, err := svc.Record(ctx, s.Run.Command()); !errors.Is(err, qa.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if n := s.DefectRows(scopeA); n != 0 {
			t.Fatalf("a store without a permission guard filed %d defects", n)
		}
	})
}
