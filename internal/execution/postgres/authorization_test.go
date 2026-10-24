package postgres_test

import (
	"context"
	"errors"
	"github.com/optimiware/kodama/internal/domain"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	identity "github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"

	"github.com/jackc/pgx/v5"
)

type errAuthority struct{ err error }

func (a errAuthority) Authorize(context.Context, execution.Actor, domain.Scope, execution.Permission) error {
	return a.err
}

// Test_claim_authority_race: PX-06, PX-08. A role revocation committed during a
// lock wait prevents dispatch; a missing or failing authority fails closed.
func Test_claim_authority_race(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*executiontest.Env, executiontest.Project, *execution.Run) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("build", readRun, "timeoutSeconds: 60")))
		return e, p, e.MustAdmit(p, def, nil)
	}

	t.Run("revocation committed while the claim waits", func(t *testing.T) {
		e, p, run := setup(t)
		// The identity module's revocation transaction: the user's authorization
		// revision is taken for update, the binding removed. The claim reaches its
		// permission check and queues behind it on the same revision row.
		tx, err := e.Migration.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`,
			string(p.Scope.TenantID), executiontest.Engineer); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, string(p.Scope.TenantID), executiontest.Engineer); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := e.Claim(p.Scope.TenantID); done <- err }()
		select {
		case err := <-done:
			t.Fatalf("the claim did not wait for the revocation: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		err = <-done
		if !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("claim after revocation: %v", err)
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("a revoked requester's claim left an attempt or reservation")
		}
	})

	t.Run("a downgraded role (viewer) cannot dispatch", func(t *testing.T) {
		e, p, run := setup(t)
		e.Revoke(p.Scope, executiontest.Engineer)
		e.Grant(p.Scope, executiontest.Engineer, identity.RoleViewer)
		if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("claim: %v", err)
		}
		if attemptCount(e, run.ID) != 0 {
			t.Fatal("attempt created for a requester without run:manage")
		}
	})

	t.Run("a failing or missing authority fails closed", func(t *testing.T) {
		for name, binder := range map[string]execpg.AuthorityBinder{
			"outage":  func(pgx.Tx) execution.TxAuthority { return errAuthority{err: errors.New("identity down")} },
			"missing": func(pgx.Tx) execution.TxAuthority { return nil },
		} {
			e, p, run := setup(t)
			store, err := execpg.NewStore(e.Runtime, e.Events, execpg.Peers{Authority: binder, Workspaces: e.Workspaces, Secrets: e.Secrets}, execpg.Options{Clock: e.Clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			svc := e.NewService(executiontest.Options{Store: store})
			if _, err := svc.ClaimRunnable(ctx, p.Scope.TenantID); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("%s: claim: %v", name, err)
			}
			if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
				t.Fatalf("%s: dispatch proceeded without authority", name)
			}
		}
	})

	t.Run("a store cannot be built without an authority binding", func(t *testing.T) {
		e := executiontest.New(t)
		if _, err := execpg.NewStore(e.Runtime, e.Events, execpg.Peers{Workspaces: e.Workspaces, Secrets: e.Secrets}, execpg.Options{}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("public use cases check current permissions", func(t *testing.T) {
		e, p, run := setup(t)
		viewer := execution.Caller{PrincipalID: executiontest.Viewer}
		if _, err := e.Svc.Get(ctx, viewer, p.Scope, run.ID); err != nil {
			t.Fatalf("viewer read: %v", err)
		}
		if _, err := e.Svc.Cancel(ctx, viewer, p.Scope, run.ID); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("viewer cancel: %v", err)
		}
		stranger := execution.Caller{PrincipalID: executiontest.Stranger}
		if _, err := e.Svc.Get(ctx, stranger, p.Scope, run.ID); !errors.Is(err, execution.ErrHidden) {
			t.Fatalf("stranger read: %v", err)
		}
		other := execution.Caller{PrincipalID: executiontest.OtherEng}
		if _, err := e.Svc.Get(ctx, other, p.Scope, run.ID); !errors.Is(err, execution.ErrHidden) {
			t.Fatalf("another tenant's engineer: %v", err)
		}
	})
	_ = adapters.BindAuthority
}
