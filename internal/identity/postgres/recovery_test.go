package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// state is everything a successful mutation would add.
type state struct{ bindings, revisions, audits, events, projectVersion int }

func (f *fixture) state() state {
	return state{
		bindings:       f.Count(`SELECT count(*) FROM memberships WHERE tenant_id = $1`, tenantA),
		revisions:      int(f.Revision(tenantA, f.engineer)),
		audits:         f.CountIn(tenantA, `SELECT count(*) FROM audit WHERE tenant_id = $1`, tenantA),
		events:         f.CountIn(tenantA, `SELECT count(*) FROM outbox WHERE tenant_id = $1`, tenantA),
		projectVersion: int(f.projectVersion(projectA)),
	}
}

// lostCommitDB commits and then reports a failure, as a connection lost after
// the server applied COMMIT would.
type lostCommitDB struct {
	inner identitypg.DB
	armed bool
}

type lostCommitTx struct {
	pgx.Tx
	db *lostCommitDB
}

func (t lostCommitTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.db.armed {
		return errors.New("connection reset by peer")
	}
	return nil
}

func (d *lostCommitDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.inner.BeginTx(ctx, o)
	return lostCommitTx{Tx: tx, db: d}, err
}

func (d *lostCommitDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return d.inner.QueryRow(ctx, sql, args...)
}

func TestMutationAtomicity(t *testing.T) {
	t.Run("an audit failure rolls everything back", func(t *testing.T) {
		f := newFixture(t, idtest0())
		f.OwnerExec(`ALTER TABLE audit ADD CONSTRAINT t010_no_grants CHECK (action <> 'membership.granted')`)
		before := f.state()
		if _, err := f.grant(f.admin, f.engineer, projectA, identity.RoleEngineer); !is(err, identity.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if got := f.state(); got != before {
			t.Fatalf("partial state: before %+v after %+v", before, got)
		}
		if f.Revision(tenantA, f.engineer) != 0 {
			t.Fatal("authorization revision leaked from a rolled-back mutation")
		}
	})

	t.Run("event failure and backlog exhaustion roll everything back", func(t *testing.T) {
		f := newFixture(t, idtest0WithBacklog(1))
		first := must(f.grant(f.admin, f.engineer, "", identity.RoleViewer)) // fills the backlog
		before := f.state()
		for name, run := range map[string]func() error{
			"grant": func() error { _, err := f.grant(f.admin, f.reviewer, projectA, identity.RoleReviewer); return err },
			"role change": func() error {
				_, err := f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: first.ID, Expected: 1, Role: identity.RoleReviewer})
				return err
			},
			"revoke": func() error {
				_, err := f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: first.ID, Expected: 1})
				return err
			},
			"project create": func() error {
				_, err := f.Service.CreateProject(ctx, identity.CreateProjectCommand{Caller: f.caller(f.admin), Tenant: tenantA, Key: "full"})
				return err
			},
			"project update": func() error {
				n := "renamed"
				_, err := f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: 1, Patch: identity.ProjectPatch{DisplayName: &n}})
				return err
			},
		} {
			if err := run(); !is(err, identity.ErrUnavailable) {
				t.Fatalf("%s: %v", name, err)
			}
			if got := f.state(); got != before {
				t.Fatalf("%s left partial state: before %+v after %+v", name, before, got)
			}
		}
		if f.Count(`SELECT count(*) FROM memberships WHERE id = $1 AND role = 'viewer' AND version = 1`, string(first.ID)) != 1 {
			t.Fatal("the original binding changed")
		}
		if f.Count(`SELECT count(*) FROM projects WHERE tenant_id = $1 AND key = 'full'`, tenantA) != 0 {
			t.Fatal("the project survived a failed create")
		}
	})

	t.Run("cancellation and lock timeouts leave nothing", func(t *testing.T) {
		f := newFixture(t, idtest0())
		before := f.state()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := f.Service.Grant(cctx, identity.GrantCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Role: identity.RoleViewer}); !is(err, identity.ErrUnavailable) {
			t.Fatalf("cancelled: %v", err)
		}
		// Hold the admin's authorization revision exclusively; the grant waits and times out.
		hold, err := f.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `SELECT set_config('kodama.tenant_id', $1, true)`, tenantA); err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(ctx, `SELECT 1 FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`, tenantA, string(f.admin)); err != nil {
			t.Fatal(err)
		}
		tctx, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel2()
		if _, err := f.Service.Grant(tctx, identity.GrantCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Role: identity.RoleViewer}); !is(err, identity.ErrUnavailable) {
			t.Fatalf("lock wait: %v", err)
		}
		hold.Rollback(ctx)
		if got := f.state(); got != before {
			t.Fatalf("partial state: before %+v after %+v", before, got)
		}
		// The same request succeeds once the lock is gone: nothing was half done.
		if _, err := f.grant(f.admin, f.engineer, "", identity.RoleViewer); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an unknown commit outcome is unavailable and a reread plus retry cannot duplicate", func(t *testing.T) {
		f := newFixture(t, idtest0())
		db := &lostCommitDB{inner: f.Runtime, armed: true}
		store := must(identitypg.NewStore(db, f.Events))
		svc := must(identity.NewService(identity.Config{Repository: store, Directory: store}))
		_, err := svc.Grant(ctx, identity.GrantCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Role: identity.RoleEngineer})
		if !is(err, identity.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		// The write did commit: the reread observes it.
		db.armed = false
		got := must(svc.ListBindings(ctx, identity.ListBindingsCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Limit: 10}))
		if len(got) != 1 || got[0].Role != identity.RoleEngineer {
			t.Fatalf("reread: %+v", got)
		}
		// Retrying the same grant does not duplicate it.
		if _, err := svc.Grant(ctx, identity.GrantCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Role: identity.RoleEngineer}); !is(err, identity.ErrConflict) {
			t.Fatalf("retry: %v", err)
		}
		if f.Revision(tenantA, f.engineer) != 1 || f.eventCount(eventsv1.TypeMembershipChanged) != 1 || f.auditCount("membership.granted") != 1 {
			t.Fatal("the lost-response retry duplicated an effect")
		}
	})

	t.Run("committed events are valid catalog events with stable identity", func(t *testing.T) {
		f := newFixture(t, idtest0())
		b := must(f.grant(f.admin, f.engineer, projectA, identity.RoleEngineer))
		must(f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: b.ID, Expected: 1, Role: identity.RoleViewer}))
		n := "Renamed"
		must(f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: 3, Patch: identity.ProjectPatch{DisplayName: &n}}))
		must(f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: b.ID, Expected: 2}))
		ids := map[string]bool{}
		for _, typ := range []string{eventsv1.TypeMembershipChanged, eventsv1.TypeProjectSettingsChanged} {
			for _, payload := range f.Outbox(tenantA, typ) {
				env, err := eventsv1.Decode([]byte(payload))
				if err != nil {
					t.Fatalf("%s: %v", typ, err)
				}
				if env.TenantID != tenantA || env.Actor.ID != string(f.admin) || env.CorrelationID == "" || ids[env.ID] {
					t.Fatalf("%+v", env)
				}
				ids[env.ID] = true
			}
		}
		if len(ids) != 7 { // grant(m+p), role(m+p), project update(p), revoke(m+p)
			t.Fatalf("%d events: %v", len(ids), ids)
		}
	})
}

func idtest0WithBacklog(n int) idtestOptions {
	return idtestOptions{Events: func(o *eventspg.Options) { o.Backlog.MaxEvents = n }}
}

var _ = storage.TenantSetting
