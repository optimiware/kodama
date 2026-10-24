package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
)

type hookAuth struct {
	inner chatopspg.TxAuthorizer
	after func()
}

func (h hookAuth) Authorize(ctx context.Context, tx pgx.Tx, actor chatops.Actor, scope domain.Scope, perm chatops.Permission) error {
	err := h.inner.Authorize(ctx, tx, actor, scope, perm)
	if err == nil && h.after != nil {
		h.after()
	}
	return err
}

type outageAuth struct{}

func (outageAuth) Authorize(context.Context, pgx.Tx, chatops.Actor, domain.Scope, chatops.Permission) error {
	return errors.New("identity authority unreachable")
}

// TestAuthorizationRevocation: real T-010 transaction-bound authorization orders
// a revocation against configuration and replay; missing bindings and authority
// outages fail closed. Real PostgreSQL.
func TestAuthorizationRevocation(t *testing.T) {
	ctx := context.Background()
	e := chatopstest.New(t, chatopstest.Options{})
	e.SeedProject(scopeA)
	owner := e.Actors(scopeA, "owner")
	dest := e.Destination(owner.Admin, scopeA, "gated", "https://hooks.t041.example/g")
	_, ids := seed(t, e, owner, "gatedseed", 1)
	newAdmin := func(label string) (chatops.Caller, identity.UserID, string) {
		u := e.User("admin-" + label)
		b := e.Bind(scopeA.TenantID, u, scopeA.ProjectID, identity.RoleAdmin)
		return chatopstest.CallerOf(u), u, b
	}
	rename := func(s chatops.Store, c chatops.Caller, name string, expected int64) error {
		_, err := s.UpdateDestination(ctx, c, scopeA, dest.ID, expected, chatops.DestinationEdit{Name: &name}, time.Now())
		return err
	}
	version := func() int64 {
		var v int64
		if err := e.Owner.QueryRow(ctx, `SELECT version FROM chatops_destinations WHERE id = $1`, string(dest.ID)).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("the permission map is project:settings for configuration and replay and project:read for inspection", func(t *testing.T) {
		if _, err := e.Svc.GetDestination(ctx, owner.Viewer, scopeA, dest.ID); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("a viewer read destination configuration: %v", err)
		}
		if _, err := e.Svc.ListDestinations(ctx, owner.Viewer, scopeA, "", 10); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("a viewer listed destination configuration: %v", err)
		}
		if _, err := e.Svc.CreateDestination(ctx, owner.Viewer, scopeA, chatops.DestinationInput{Name: "v", Endpoint: "https://hooks.t041.example/v", Events: chatopstest.AllEvents(), Enabled: true}); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("a viewer created a destination: %v", err)
		}
		view, err := e.Svc.GetDelivery(ctx, owner.Viewer, scopeA, ids[0])
		if err != nil {
			t.Fatalf("a viewer could not inspect a delivery: %v", err)
		}
		if _, err := e.Svc.ReplayDelivery(ctx, owner.Viewer, scopeA, ids[0], view.Delivery.Version, "no"); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("a viewer replayed: %v", err)
		}
		// Engineers and reviewers hold project:read only; an administrator both.
		eng := e.User("engineer-x")
		e.Bind(scopeA.TenantID, eng, scopeA.ProjectID, identity.RoleEngineer)
		if _, err := e.Svc.GetDelivery(ctx, chatopstest.CallerOf(eng), scopeA, ids[0]); err != nil {
			t.Fatalf("an engineer could not inspect: %v", err)
		}
		if _, err := e.Svc.GetDestination(ctx, chatopstest.CallerOf(eng), scopeA, dest.ID); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("an engineer read configuration: %v", err)
		}
		// A non-member learns nothing.
		stranger := chatopstest.CallerOf(e.User("stranger"))
		if _, err := e.Svc.GetDelivery(ctx, stranger, scopeA, ids[0]); !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("a stranger: %v", err)
		}
		// Non-human actors and unknown permissions are denied.
		svc := chatops.Caller{Actor: chatops.Actor{Type: "workload", ID: "att_1"}}
		if _, err := e.Svc.GetDelivery(ctx, svc, scopeA, ids[0]); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("a workload actor: %v", err)
		}
		if err := e.Store.CheckAccess(ctx, owner.Admin, scopeA, "project:made_up"); !errors.Is(err, chatops.ErrDenied) {
			t.Fatalf("an unknown permission: %v", err)
		}
	})

	t.Run("a revocation committed first is seen by the next operation", func(t *testing.T) {
		c, u, binding := newAdmin("early")
		if err := rename(e.Store, c, "before-revoke", dest.Version); err != nil {
			t.Fatal(err)
		}
		e.Unbind(scopeA.TenantID, binding, u)
		v := version()
		if err := rename(e.Store, c, "after-revoke", v); !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("a revoked administrator renamed: %v", err)
		}
		if _, err := e.Svc.CreateDestination(ctx, c, scopeA, chatops.DestinationInput{Name: "late", Endpoint: "https://hooks.t041.example/late", Events: chatopstest.AllEvents(), Enabled: true}); err == nil {
			t.Fatal("a revoked administrator created a destination")
		}
		if _, err := e.Svc.ReplayDelivery(ctx, c, scopeA, ids[0], 1, "late"); err == nil {
			t.Fatal("a revoked administrator replayed")
		}
		if version() != v {
			t.Fatal("a refused operation changed the destination")
		}
	})

	t.Run("a revocation racing a mutation is ordered by the authorization revision", func(t *testing.T) {
		// (a) The mutation is authorized and still open: the revocation waits for it
		// and returns only after it committed; the next operation is refused.
		c, u, binding := newAdmin("inflight")
		authorized, release := make(chan struct{}), make(chan struct{})
		store := e.NewStore(nil, hookAuth{inner: chatopspg.IdentityAuthorizer{}, after: func() { close(authorized); <-release }})
		mutationDone := make(chan error, 1)
		v := version()
		go func() { mutationDone <- rename(store, c, "inflight-rename", v) }()
		<-authorized
		revoked := make(chan struct{})
		go func() {
			e.Unbind(scopeA.TenantID, binding, u)
			close(revoked)
		}()
		select {
		case <-revoked:
			t.Fatal("the revocation returned while a mutation relying on the old grant was still open")
		case <-time.After(400 * time.Millisecond):
		}
		close(release)
		if err := <-mutationDone; err != nil {
			t.Fatalf("the in-flight mutation: %v", err)
		}
		select {
		case <-revoked:
		case <-time.After(5 * time.Second):
			t.Fatal("the revocation never completed")
		}
		if err := rename(e.Store, c, "after-inflight", version()); !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("after the revocation: %v", err)
		}

		// (b) The revocation is open first: the mutation waits for it and is then refused.
		c2, u2, binding2 := newAdmin("revoking")
		tx, err := e.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT revision FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`, string(scopeA.TenantID), string(u2)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND id = $2`, string(scopeA.TenantID), binding2); err != nil {
			t.Fatal(err)
		}
		before := version()
		replayDone := make(chan error, 1)
		go func() { replayDone <- rename(e.Store, c2, "blocked-rename", before) }()
		select {
		case err := <-replayDone:
			t.Fatalf("the mutation did not wait for the revocation: %v", err)
		case <-time.After(400 * time.Millisecond):
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`, string(scopeA.TenantID), string(u2)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-replayDone; !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("a mutation that waited for a revocation: %v", err)
		}
		if version() != before {
			t.Fatal("a mutation committed after the revocation")
		}
	})

	t.Run("a revoked replay is refused and writes nothing", func(t *testing.T) {
		// Make the seeded delivery failed so only authorization stands in the way.
		b, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 50, policy)
		var cl chatops.Claim
		for _, c := range b.Claims {
			if c.Delivery.ID == ids[0] {
				cl = c
			}
		}
		failed, err := e.Store.Finish(ctx, cl, chatops.Outcome{Class: chatops.ClassRejected, HTTPStatus: 400}, now(), policy)
		if err != nil {
			t.Fatal(err)
		}
		c, u, binding := newAdmin("replayer")
		e.Unbind(scopeA.TenantID, binding, u)
		if _, err := e.Svc.ReplayDelivery(ctx, c, scopeA, ids[0], failed.Version, "after revocation"); !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("revoked replay = %v", err)
		}
		if d := e.Delivery(scopeA, ids[0]); d.State != chatops.StateFailed || d.Replays != 0 {
			t.Fatalf("a revoked replay changed the delivery: %+v", d)
		}
		if n := e.Count(`SELECT count(*) FROM chatops_replays`); n != 0 {
			t.Fatalf("replay rows = %d", n)
		}
	})

	t.Run("missing bindings and authority outages fail closed", func(t *testing.T) {
		if _, err := chatopspg.NewStore(chatopspg.Config{DB: e.Runtime}); err == nil {
			t.Fatal("a store without an authorizer was constructed")
		}
		if _, err := chatops.NewService(chatops.Config{Store: e.Store, MaxDestinationsPerProject: 1}); err == nil {
			t.Fatal("a service without an endpoint policy was constructed")
		}
		out := e.NewStore(nil, outageAuth{})
		before := version()
		if err := rename(out, owner.Admin, "during-outage", before); !errors.Is(err, chatops.ErrUnavailable) {
			t.Fatalf("an authority outage allowed or misclassified a mutation: %v", err)
		}
		if _, err := out.GetDelivery(ctx, owner.Viewer, scopeA, ids[0]); !errors.Is(err, chatops.ErrUnavailable) {
			t.Fatalf("an authority outage allowed a read: %v", err)
		}
		if _, err := out.ReplayDelivery(ctx, owner.Admin, scopeA, ids[0], 1, "outage", time.Now()); !errors.Is(err, chatops.ErrUnavailable) {
			t.Fatalf("an authority outage allowed a replay: %v", err)
		}
		if version() != before {
			t.Fatal("an outage changed the destination")
		}
		// The outage text never reaches the caller.
		if err := rename(out, owner.Admin, "x", before); err != nil && containsAny(err.Error(), "unreachable", "identity authority") {
			t.Fatalf("the outage detail leaked: %v", err)
		}
	})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
