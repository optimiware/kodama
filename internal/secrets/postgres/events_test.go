package postgres_test

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/storage"
)

func (e *env) outboxEvents(tenant domain.TenantID) []eventsv1.Envelope {
	e.t.Helper()
	var out []eventsv1.Envelope
	err := storage.WithTenant(e.ctx(), e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload FROM outbox WHERE tenant_id = $1 AND event_type = 'configuration.changed'`, string(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			env, err := eventsv1.Decode(payload)
			if err != nil {
				return err
			}
			out = append(out, env)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out
}

func identity(key, route string, body string) httpapi.RequestIdentity {
	return httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: "alice"}, TenantID: tenantA, ProjectID: projectA1,
		Method: http.MethodPost, Route: route, Key: httpapi.ClientKey(key),
		Hash: httpapi.RequestHash{byte(len(body)), 1, 2, 3},
	}
}

func allowReplay(context.Context, httpapi.Reference) error { return nil }

// keyedCreate creates a secret through the keyed (idempotent) path, the way the
// HTTP handler does.
func (e *env) keyedCreate(key, name, value string, tamper func(ctx context.Context, w secrets.Writer) error) (httpapi.StoredResponse, bool, error) {
	ctx := e.ctx()
	m := e.mutation("alice")
	p, err := e.svc.PrepareSecret(ctx, m, scopeA1, name, secrets.KindGeneric, []byte(value))
	if err != nil {
		return httpapi.StoredResponse{}, false, err
	}
	resp, replayed, err := e.keyed.Execute(ctx, identity(key, "/api/v1/tenants/t_acme/projects/p_inventory/secrets", name), allowReplay,
		func(ctx context.Context, w secrets.Writer) (httpapi.Outcome, error) {
			s, err := e.svc.CreateSecret(ctx, w, m, p)
			if err != nil {
				return httpapi.Outcome{}, err
			}
			if tamper != nil {
				if err := tamper(ctx, w); err != nil {
					return httpapi.Outcome{}, err
				}
			}
			body := mustJSON(e.t, map[string]any{"id": s.ID(), "name": s.Name(), "version": int64(s.Version())})
			return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 201, Body: body}, Target: httpapi.Reference{Kind: "secret", ID: s.ID()}}, nil
		})
	return resp, replayed, err
}

// Test_configuration_atomicity: business state, history, audit, event and keyed
// response commit or roll back together; backlog rejection preserves state.
func Test_configuration_atomicity(t *testing.T) {
	t.Run("everything commits together with the right event", func(t *testing.T) {
		e := newEnv(t)
		resp, replayed, err := e.keyedCreate("k1", "erp-token", "atomic-SENTINEL", nil)
		if err != nil || replayed || resp.Status != 201 {
			t.Fatalf("create: %v %v", err, replayed)
		}
		s := e.state()
		if s.secrets != 1 || s.versions != 1 || s.scopeKeys != 1 || s.wraps != 1 || s.events != 1 {
			t.Fatalf("state %+v", s)
		}
		if n := e.count(`SELECT count(*) FROM idempotency`); n != 1 {
			t.Fatalf("%d idempotency records", n)
		}
		if a := e.auditActions(); !contains(a, "secret.create") || !contains(a, "secret_scope_key.create") {
			t.Fatalf("audit %v", a)
		}
		evs := e.outboxEvents(tenantA)
		if len(evs) != 1 {
			t.Fatalf("%d events", len(evs))
		}
		ev := evs[0]
		data, err := eventsv1.DecodeData(ev)
		if err != nil {
			t.Fatal(err)
		}
		cc, ok := data.(*eventsv1.ConfigurationChanged)
		if !ok {
			cv, ok2 := data.(eventsv1.ConfigurationChanged)
			if !ok2 {
				t.Fatalf("payload type %T", data)
			}
			cc = &cv
		}
		if ev.Type != "configuration.changed" || ev.Resource.Type != "configuration" || ev.Resource.Version != 1 ||
			ev.TenantID != tenantA || ev.ProjectID != projectA1 || ev.Actor.Type != eventsv1.ActorUser || ev.Actor.ID != "alice" ||
			cc.Kind != "secret" || cc.Change != "created" || cc.Scope != "project" || cc.Version != 1 {
			t.Fatalf("event %+v data %+v", ev, cc)
		}
		// Replay: same key, same request, no new effect.
		_, replayed, err = e.keyedCreate("k1", "erp-token", "atomic-SENTINEL", nil)
		if err != nil || !replayed {
			t.Fatalf("replay: %v %v", err, replayed)
		}
		if e.state() != s {
			t.Fatalf("replay changed state: %+v", e.state())
		}
		notContains(t, "persisted metadata", e.dumpAll(), "atomic-SENTINEL")
	})

	t.Run("event backlog rejection preserves state", func(t *testing.T) {
		e := newEnv(t, withBacklog(1))
		if _, _, err := e.keyedCreate("k1", "first", "one", nil); err != nil {
			t.Fatal(err)
		}
		before := e.state()
		_, _, err := e.keyedCreate("k2", "second", "two-SENTINEL", nil)
		if err == nil {
			t.Fatal("create succeeded with a full backlog")
		}
		if after := e.state(); after != before {
			t.Fatalf("a rejected event left state behind: %+v -> %+v", before, after)
		}
		if n := e.count(`SELECT count(*) FROM idempotency`); n != 1 {
			t.Fatalf("%d idempotency records", n)
		}
		// The unkeyed path reports the dedicated sentinel and rolls back as well.
		m := e.mutation("alice")
		p, err := e.svc.PrepareSecret(e.ctx(), m, scopeA1, "third", secrets.KindGeneric, []byte("three"))
		if err != nil {
			t.Fatal(err)
		}
		err = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.CreateSecret(ctx, w, m, p)
			return err
		})
		if !errors.Is(err, secrets.ErrBackpressure) {
			t.Fatalf("backpressure not classified: %v", err)
		}
		if e.state() != before {
			t.Fatal("state changed after backpressure")
		}
		// Variables share the rule.
		err = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.CreateVariable(ctx, w, m, secrets.CreateVariableInput{Scope: scopeA1, Name: "region", Value: strValue("eu")})
			return err
		})
		if !errors.Is(err, secrets.ErrBackpressure) || e.count(`SELECT count(*) FROM variables`) != 0 || e.count(`SELECT count(*) FROM variable_versions`) != 0 {
			t.Fatalf("variable create under backpressure: %v", err)
		}
	})

	t.Run("audit failure rolls back state, event and key", func(t *testing.T) {
		e := newEnv(t)
		e.ownerExec(`CREATE FUNCTION t012_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action = 'secret.create' THEN RAISE EXCEPTION 'audit store unavailable'; END IF; RETURN NEW; END $$;
CREATE TRIGGER t012_fail_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t012_fail_audit();`)
		if _, _, err := e.keyedCreate("k1", "erp-token", "x", nil); err == nil {
			t.Fatal("create succeeded without its audit row")
		}
		s := e.state()
		// The scope key was created by its own short transaction before the failed write; it is
		// not part of the business mutation and stays consistent with its wrap and audit.
		if s.secrets != 0 || s.versions != 0 || s.events != 0 || e.count(`SELECT count(*) FROM idempotency`) != 0 {
			t.Fatalf("partial state after audit failure: %+v", s)
		}
		if s.scopeKeys != s.wraps {
			t.Fatalf("scope key and wrap diverged: %+v", s)
		}
		e.ownerExec(`DROP TRIGGER t012_fail_audit ON audit`)
		// Retrying with the same key now executes (nothing was recorded).
		if _, replayed, err := e.keyedCreate("k1", "erp-token", "x", nil); err != nil || replayed {
			t.Fatalf("retry: %v %v", err, replayed)
		}
	})

	t.Run("a failing callback after the writes rolls everything back", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("callback failed after the writes")
		_, _, err := e.keyedCreate("k1", "erp-token", "x", func(context.Context, secrets.Writer) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("callback error: %v", err)
		}
		s := e.state()
		if s.secrets != 0 || s.versions != 0 || s.events != 0 || s.audits > 1 || e.count(`SELECT count(*) FROM idempotency`) != 0 {
			t.Fatalf("partial state: %+v", s)
		}
	})

	t.Run("an unstorable response rolls the mutation back", func(t *testing.T) {
		e := newEnv(t)
		m := e.mutation("alice")
		p, err := e.svc.PrepareSecret(e.ctx(), m, scopeA1, "erp-token", secrets.KindGeneric, []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = e.keyed.Execute(e.ctx(), identity("k9", "/api/v1/tenants/t_acme/projects/p_inventory/secrets", "x"), allowReplay,
			func(ctx context.Context, w secrets.Writer) (httpapi.Outcome, error) {
				if _, err := e.svc.CreateSecret(ctx, w, m, p); err != nil {
					return httpapi.Outcome{}, err
				}
				return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 201, Body: []byte("not json")}}, nil
			})
		if err == nil {
			t.Fatal("an unstorable response was accepted")
		}
		if s := e.state(); s.secrets != 0 || s.events != 0 {
			t.Fatalf("mutation survived an unstorable response: %+v", s)
		}
	})

	t.Run("event vocabulary maps every operation", func(t *testing.T) {
		e := newEnv(t)
		v := e.createVariable(scopeA1, "region", "eu")
		val := strValue("us")
		mustTx := func(fn func(ctx context.Context, w secrets.Writer) error) {
			t.Helper()
			if err := e.svc.Atomically(e.ctx(), tenantA, fn); err != nil {
				t.Fatal(err)
			}
		}
		mustTx(func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.EditVariable(ctx, w, e.mutation("alice"), scopeA1, v.ID(), 1, secrets.VariableEdit{Value: &val})
			return err
		})
		mustTx(func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.DeleteVariable(ctx, w, e.mutation("alice"), scopeA1, v.ID(), 2)
			return err
		})
		sec := e.createSecret(scopeA1, "erp-token", "one")
		e.rotate(scopeA1, sec.ID(), "two")
		e.revoke(scopeA1, sec.ID(), 1)
		if _, err := e.svc.RewrapScopeKey(e.ctx(), e.mutation("ops"), tenantA, ""); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, ev := range e.outboxEvents(tenantA) {
			data, err := eventsv1.DecodeData(ev)
			if err != nil {
				t.Fatal(err)
			}
			var cc eventsv1.ConfigurationChanged
			switch d := data.(type) {
			case *eventsv1.ConfigurationChanged:
				cc = *d
			case eventsv1.ConfigurationChanged:
				cc = d
			}
			got = append(got, cc.Kind+":"+cc.Change+":"+cc.Scope)
			if err := ev.Validate(); err != nil {
				t.Fatalf("invalid event %s: %v", ev.ID, err)
			}
		}
		want := []string{
			"variable:created:project", "variable:updated:project", "variable:deleted:project",
			"secret:created:project", "secret:rotated:project", "secret:updated:project", "secret:updated:tenant",
		}
		if len(got) != len(want) {
			t.Fatalf("events %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("events %v, want %v", got, want)
			}
		}
	})
}
