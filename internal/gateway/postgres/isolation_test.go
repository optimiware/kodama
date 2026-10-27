package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/storage"
)

var (
	bg     = context.Background()
	scopeA = executiontest.ScopeA
	scopeB = executiontest.ScopeB
)

func code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func wantCode(t *testing.T, err error, want, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: succeeded, want SQLSTATE %s", what, want)
		return
	}
	if got := code(err); got != want {
		t.Errorf("%s: SQLSTATE %q (%v), want %s", what, got, err, want)
	}
}

// seeded is a project with a configured gateway, one finished request with its
// usage and a second request that holds the run's slot.
type seeded struct {
	e       *gatewaytest.Env
	a       gatewaytest.Attempt
	done    gateway.RequestID
	holding gateway.RequestID
}

func seed(t *testing.T) *seeded {
	t.Helper()
	e := gatewaytest.New(t)
	e.ModelSecret(scopeA)
	e.Configure(e.Replica, scopeA, e.Edit())
	a := e.NewAttempt(scopeA)
	res, err := e.Invoke(e.Replica, a, gatewaytest.Body("gpt-a", "hello", 64), &gatewaytest.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := gateway.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Replica.Store.Admit(bg, gateway.AdmitCommand{Workload: a.Workload, RequestID: id, Model: "gpt-a", RequestBytes: 50, InputBound: 20, OutputMax: 64, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &seeded{e: e, a: a, done: res.RequestID, holding: id}
}

func (s *seeded) owner(sql string, args ...any) error {
	_, err := s.e.X.Migration.Exec(bg, sql, args...)
	return err
}

func (s *seeded) tenant(tenant domain.TenantID, sql string, args ...any) (int64, error) {
	var n int64
	err := storage.WithTenant(bg, s.e.X.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func (s *seeded) count(tenant domain.TenantID, table string) int {
	s.e.T.Helper()
	var n int
	err := storage.WithTenant(bg, s.e.X.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	})
	if err != nil {
		s.e.T.Fatalf("count %s: %v", table, err)
	}
	return n
}

var tables = []string{"model_settings_revisions", "model_settings", "model_run_bindings", "model_requests", "model_usage"}

// Test_gateway_storage_isolation: GM-01, GM-03, SD (tenant isolation), software
// design (persistence boundaries). Forced row level security, composite
// ownership, non-owner grants, monotonic counters, immutable policy and usage,
// pooled tenant-context cleanup, migration reapplication and atomic rollback.
func Test_gateway_storage_isolation(t *testing.T) {
	s := seed(t)

	t.Run("forced row level security confines every table to the tenant context", func(t *testing.T) {
		for _, tb := range tables {
			if n := s.count(scopeA.TenantID, tb); n == 0 {
				t.Errorf("%s: the owning tenant sees no rows", tb)
			}
			if n := s.count(scopeB.TenantID, tb); n != 0 {
				t.Errorf("%s: another tenant sees %d rows", tb, n)
			}
			var rls, forced bool
			if err := s.e.X.Migration.QueryRow(bg, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tb).Scan(&rls, &forced); err != nil || !rls || !forced {
				t.Errorf("%s: rowsecurity=%v forced=%v (%v)", tb, rls, forced, err)
			}
		}
		// A write under another tenant's context is refused by the policy.
		_, err := s.tenant(scopeB.TenantID, `INSERT INTO model_settings_revisions (tenant_id, project_id, revision, enabled, endpoint, credential_ref, credential_version, models,
			max_requests, token_allowance, max_request_bytes, max_input_tokens, max_output_tokens, recording, updated_at, updated_by)
			VALUES ($1, $2, 9, true, 'https://x.example', 'secret://project/k', 1, ARRAY['gpt-a'], 1, 1, 1, 1, 1, false, now(), 'u')`,
			string(scopeA.TenantID), string(scopeA.ProjectID))
		wantCode(t, err, "42501", "cross-tenant write")
		// No tenant context at all sees nothing.
		var n int
		if err := s.e.X.Runtime.QueryRow(bg, `SELECT count(*) FROM model_requests`).Scan(&n); err != nil || n != 0 {
			t.Errorf("without a tenant context: %d rows (%v)", n, err)
		}
	})

	t.Run("the runtime role is not an owner and can only do what the module needs", func(t *testing.T) {
		for _, tb := range tables {
			_, err := s.tenant(scopeA.TenantID, `DELETE FROM `+tb)
			wantCode(t, err, "42501", "DELETE "+tb)
			_, err = s.tenant(scopeA.TenantID, `TRUNCATE `+tb)
			wantCode(t, err, "42501", "TRUNCATE "+tb)
		}
		_, err := s.tenant(scopeA.TenantID, `UPDATE model_requests SET model = 'gpt-b'`)
		wantCode(t, err, "42501", "UPDATE model_requests.model")
		_, err = s.tenant(scopeA.TenantID, `UPDATE model_requests SET run_id = 'x'`)
		wantCode(t, err, "42501", "UPDATE model_requests.run_id")
		_, err = s.tenant(scopeA.TenantID, `UPDATE model_run_bindings SET endpoint = 'https://evil.invalid'`)
		wantCode(t, err, "42501", "UPDATE model_run_bindings.endpoint")
		_, err = s.tenant(scopeA.TenantID, `UPDATE model_run_bindings SET credential_version = 9`)
		wantCode(t, err, "42501", "UPDATE model_run_bindings.credential_version")
		_, err = s.tenant(scopeA.TenantID, `UPDATE model_usage SET charged_tokens = 0`)
		wantCode(t, err, "42501", "UPDATE model_usage")
		_, err = s.tenant(scopeA.TenantID, `UPDATE model_settings_revisions SET enabled = false`)
		wantCode(t, err, "42501", "UPDATE model_settings_revisions")
		_, err = s.tenant(scopeA.TenantID, `CREATE TABLE model_rogue (n int)`)
		wantCode(t, err, "42501", "CREATE TABLE")
		var super, bypass bool
		if err := s.e.X.Runtime.QueryRow(bg, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
			t.Fatalf("runtime role super=%v bypassrls=%v (%v)", super, bypass, err)
		}
	})

	t.Run("triggers keep the policy, usage and transitions immutable even for the owner", func(t *testing.T) {
		wantCode(t, s.owner(`UPDATE model_run_bindings SET endpoint = 'https://evil.invalid'`), "23514", "binding endpoint")
		wantCode(t, s.owner(`UPDATE model_run_bindings SET models = ARRAY['x']`), "23514", "binding models")
		wantCode(t, s.owner(`DELETE FROM model_run_bindings`), "23514", "binding delete")
		wantCode(t, s.owner(`UPDATE model_usage SET charged_tokens = 0`), "23514", "usage update")
		wantCode(t, s.owner(`DELETE FROM model_usage`), "23514", "usage delete")
		wantCode(t, s.owner(`DELETE FROM model_requests`), "23514", "request delete")
		wantCode(t, s.owner(`UPDATE model_requests SET model = 'gpt-b'`), "23514", "request identity")
		wantCode(t, s.owner(`UPDATE model_requests SET state = 'succeeded' WHERE id = $1`, string(s.holding)), "23514", "admitted -> succeeded")
		wantCode(t, s.owner(`UPDATE model_requests SET state = 'forwarding', forwarding_at = now() WHERE id = $1`, string(s.done)), "23514", "terminal reopened")
		wantCode(t, s.owner(`UPDATE model_requests SET outcome_code = 'other' WHERE id = $1`, string(s.done)), "23514", "settled outcome rewritten")
		wantCode(t, s.owner(`UPDATE model_settings_revisions SET enabled = false`), "23514", "revision rewrite")
		wantCode(t, s.owner(`DELETE FROM model_settings`), "23514", "settings delete")
		wantCode(t, s.owner(`UPDATE model_settings SET revision = revision + 5`), "23514", "revision skip")
	})

	t.Run("counters only increase and the cap is a database constraint", func(t *testing.T) {
		wantCode(t, s.owner(`UPDATE model_run_bindings SET tokens_charged = tokens_charged - 1`), "23514", "charge decreased")
		wantCode(t, s.owner(`UPDATE model_run_bindings SET requests_admitted = requests_admitted - 1`), "23514", "count decreased")
		wantCode(t, s.owner(`UPDATE model_run_bindings SET requests_admitted = requests_admitted + 2`), "23514", "count jumped")
		wantCode(t, s.owner(`UPDATE model_run_bindings SET tokens_charged = -1`), "23514", "negative charge")
		wantCode(t, s.owner(`UPDATE model_run_bindings SET tokens_charged = 9007199254740992`), "23514", "unbounded charge")
		// Raise the count to the cap one step at a time: the next step violates it.
		var admitted, max int64
		if err := s.e.X.Migration.QueryRow(bg, `SELECT requests_admitted, max_requests FROM model_run_bindings`).Scan(&admitted, &max); err != nil {
			t.Fatal(err)
		}
		for ; admitted < max; admitted++ {
			if err := s.owner(`UPDATE model_run_bindings SET requests_admitted = requests_admitted + 1`); err != nil {
				t.Fatalf("step %d: %v", admitted, err)
			}
		}
		wantCode(t, s.owner(`UPDATE model_run_bindings SET requests_admitted = requests_admitted + 1`), "23514", "count beyond the cap")
	})

	t.Run("one unresolved request per run and one usage per request", func(t *testing.T) {
		wantCode(t, s.owner(`INSERT INTO model_requests (id, tenant_id, project_id, run_id, attempt_id, ordinal, model, input_bound, output_max, charge_bound,
			recording, state, admitted_at, heartbeat_at) SELECT 'mrq_dup', tenant_id, project_id, run_id, attempt_id, 99, 'gpt-a', 1, 1, 2, false, 'admitted', now(), now()
			FROM model_requests WHERE id = $1`, string(s.holding)), "23505", "second unresolved request")
		wantCode(t, s.owner(`INSERT INTO model_usage (tenant_id, project_id, request_id, run_id, attempt_id, requested_model, model_confirmed, destination_origin,
			charged_tokens, basis, uncertainty, outcome, outcome_code, recorded_at)
			SELECT tenant_id, project_id, request_id, run_id, attempt_id, requested_model, false, destination_origin, 1, 'estimated', 'none', 'failed', 'x', now()
			FROM model_usage WHERE request_id = $1`, string(s.done)), "23505", "second usage record")
		wantCode(t, s.owner(`UPDATE model_requests SET state = 'cancelled', outcome_code = NULL, finished_at = now() WHERE id = $1`, string(s.holding)), "23514", "terminal request without an outcome code")
	})

	t.Run("composite ownership ties every row to one project and run", func(t *testing.T) {
		// A request whose attempt belongs to another run.
		other := s.e.NewAttempt(scopeA)
		wantCode(t, s.owner(`INSERT INTO model_requests (id, tenant_id, project_id, run_id, attempt_id, ordinal, model, input_bound, output_max, charge_bound,
			recording, state, admitted_at, heartbeat_at) SELECT 'mrq_cross', tenant_id, project_id, run_id, $2, 98, 'gpt-a', 1, 1, 2, false, 'failed', now(), now()
			FROM model_requests WHERE id = $1`, string(s.done), string(other.ID)), "23503", "attempt of another run")
		// A request whose run has no binding in this project.
		wantCode(t, s.owner(`INSERT INTO model_requests (id, tenant_id, project_id, run_id, attempt_id, ordinal, model, input_bound, output_max, charge_bound,
			recording, state, admitted_at, heartbeat_at) SELECT 'mrq_cross2', tenant_id, 'p020_billing', run_id, attempt_id, 97, 'gpt-a', 1, 1, 2, false, 'failed', now(), now()
			FROM model_requests WHERE id = $1`, string(s.done)), "23503", "request in another project")
		// A binding for a run of another project, and settings for a foreign project.
		wantCode(t, s.owner(`INSERT INTO model_run_bindings SELECT tenant_id, 'p020_billing', run_id, settings_revision, endpoint, credential_ref, credential_version, models,
			max_requests, token_allowance, max_request_bytes, max_input_tokens, max_output_tokens, recording, 0, 0, created_at, updated_at FROM model_run_bindings`), "23503", "binding in another project")
		wantCode(t, s.owner(`INSERT INTO model_settings_revisions SELECT 'no_such_tenant', project_id, revision, enabled, endpoint, credential_ref, credential_version, models,
			max_requests, token_allowance, max_request_bytes, max_input_tokens, max_output_tokens, recording, updated_at, updated_by FROM model_settings_revisions`), "23503", "settings of a foreign project")
	})

	t.Run("pooled connections do not carry a tenant context between uses", func(t *testing.T) {
		pool := s.e.X.Base.DB.RuntimePool(bg, 1)
		for i := 0; i < 3; i++ {
			var n int
			err := storage.WithTenant(bg, pool, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM model_requests`).Scan(&n)
			})
			if err != nil || n == 0 {
				t.Fatalf("tenant transaction: %d %v", n, err)
			}
			// The same single connection, outside a tenant transaction.
			if err := pool.QueryRow(bg, `SELECT count(*) FROM model_requests`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("a pooled connection kept the tenant context: %d rows (%v)", n, err)
			}
			var setting string
			if err := pool.QueryRow(bg, `SELECT coalesce(current_setting($1, true), '')`, storage.TenantSetting).Scan(&setting); err != nil || setting != "" {
				t.Fatalf("tenant setting left behind: %q (%v)", setting, err)
			}
		}
	})

	t.Run("the migration reapplies as a no-op and rejects edits", func(t *testing.T) {
		m := gatewaytest.Manifest(t)
		res, err := storage.Migrate(bg, s.e.X.Migration, m, storage.MigrateOptions{RuntimeRole: s.e.X.Base.DB.RuntimeRole})
		if err != nil || len(res.Applied) != 0 || res.Skipped != len(m.Migrations()) {
			t.Fatalf("reapply: %+v %v", res, err)
		}
		changed := storage.BaseContribution()
		changed.Migrations[0].SQL += "\n-- edited"
		bad, err := storage.NewManifest(changed, execpg.RunResourceMigrations())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(bg, s.e.X.Migration, bad, storage.MigrateOptions{RuntimeRole: s.e.X.Base.DB.RuntimeRole}); !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("an edited migration: %v", err)
		}
		if _, err := storage.Migrate(bg, s.e.X.Migration, gatewaytest.Manifest(t), storage.MigrateOptions{}); err == nil {
			t.Fatal("grants without a runtime role must be refused")
		}
	})

	t.Run("no table can hold content", func(t *testing.T) {
		rows, err := s.e.X.Migration.Query(bg, `SELECT table_name, column_name FROM information_schema.columns
			WHERE table_name = ANY ($1) AND (column_name ~ '(prompt|completion|content|message|argument|body|response|digest|hash|text)')`, tables)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var tb, col string
			if err := rows.Scan(&tb, &col); err != nil {
				t.Fatal(err)
			}
			t.Errorf("%s.%s looks like a content column", tb, col)
		}
	})
}

// Test_gateway_settings_revisions: settings are replaced under an expected
// revision, a no-change edit is idempotent, concurrent first writers have one
// winner, and a failed write rolls back its revision, audit row and event.
func Test_gateway_settings_revisions(t *testing.T) {
	e := gatewaytest.New(t)
	e.ModelSecret(scopeA)
	ctx := context.Background()
	admin := gateway.Actor{ID: gatewaytest.Admin}

	if cur, err := e.Replica.Svc.GetSettings(ctx, admin, scopeA); err != nil || cur.Revision != 1 || cur.Enabled {
		t.Fatalf("virtual revision: %+v %v", cur, err)
	}
	if _, err := e.Replica.Svc.ReplaceSettings(ctx, admin, scopeA, 7, e.Edit(), "r"); !errors.Is(err, gateway.ErrVersionConflict) {
		t.Fatalf("a wrong first revision: %v", err)
	}
	first, err := e.Replica.Svc.ReplaceSettings(ctx, admin, scopeA, 1, e.Edit(), "r1")
	if err != nil || !first.Changed || first.Settings.Revision != 2 {
		t.Fatalf("first write: %+v %v", first, err)
	}
	again, err := e.Replica.Svc.ReplaceSettings(ctx, admin, scopeA, 2, e.Edit(), "r2")
	if err != nil || again.Changed || again.Settings.Revision != 2 {
		t.Fatalf("an identical edit must not create a revision: %+v %v", again, err)
	}
	if _, err := e.Replica.Svc.ReplaceSettings(ctx, admin, scopeA, 1, e.Edit(), "r3"); !errors.Is(err, gateway.ErrVersionConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if n := e.Events(scopeA.TenantID, "project.settings_changed"); n != 1 {
		t.Fatalf("settings events %d, want 1", n)
	}
	var audits int
	e.Scalar(&audits, `SELECT count(*) FROM audit WHERE action = 'model_settings.update'`)
	if audits != 1 {
		t.Fatalf("audit rows %d", audits)
	}

	t.Run("concurrent first writers have one winner", func(t *testing.T) {
		e2 := gatewaytest.New(t)
		e2.ModelSecret(scopeA)
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ed := e2.Edit()
				ed.Limits.MaxRequests = int64(i + 1)
				_, errs[i] = e2.Replica.Svc.ReplaceSettings(ctx, admin, scopeA, 1, ed, fmt.Sprintf("r%d", i))
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, gateway.ErrVersionConflict):
			default:
				t.Errorf("unexpected: %v", err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d first writers won", wins)
		}
		var revs int
		e2.Scalar(&revs, `SELECT count(*) FROM model_settings_revisions`)
		if revs != 1 {
			t.Fatalf("%d revisions stored", revs)
		}
	})

	t.Run("a refused event write rolls the whole change back", func(t *testing.T) {
		e3 := gatewaytest.New(t)
		e3.ModelSecret(scopeA)
		failing := e3.NewReplica(gatewaytest.ReplicaOptions{Events: func(context.Context, pgx.Tx, eventsv1.Envelope) error {
			return fmt.Errorf("%w: full", gateway.ErrBackpressure)
		}})
		if _, err := e3.TryConfigure(failing, scopeA, e3.Edit()); !errors.Is(err, gateway.ErrBackpressure) {
			t.Fatalf("with a full backlog: %v", err)
		}
		for _, tb := range []string{"model_settings", "model_settings_revisions"} {
			var n int
			e3.Scalar(&n, `SELECT count(*) FROM `+tb)
			if n != 0 {
				t.Errorf("%s kept %d rows of a rolled-back change", tb, n)
			}
		}
		var audits int
		e3.Scalar(&audits, `SELECT count(*) FROM audit WHERE action = 'model_settings.update'`)
		if audits != 0 {
			t.Errorf("%d audit rows of a rolled-back change", audits)
		}
		if _, err := e3.TryConfigure(e3.Replica, scopeA, e3.Edit()); err != nil {
			t.Fatalf("after the backlog drained: %v", err)
		}
	})

	t.Run("invalid replacements are refused before any write", func(t *testing.T) {
		e4 := gatewaytest.New(t)
		e4.ModelSecret(scopeA)
		for name, mutate := range map[string]func(*gateway.SettingsEdit){
			"http endpoint":        func(ed *gateway.SettingsEdit) { ed.Endpoint = "http://" + ed.Endpoint[len("https://"):] },
			"endpoint credentials": func(ed *gateway.SettingsEdit) { ed.Endpoint = "https://user:pw@127.0.0.1:1/v1" },
			"unapproved endpoint":  func(ed *gateway.SettingsEdit) { ed.Endpoint = "https://api.example.invalid/v1" },
			"no models":            func(ed *gateway.SettingsEdit) { ed.Models = nil },
			"duplicate models":     func(ed *gateway.SettingsEdit) { ed.Models = []string{"gpt-a", "gpt-a"} },
			"zero cap":             func(ed *gateway.SettingsEdit) { ed.Limits.MaxRequests = 0 },
			"allowance overflow":   func(ed *gateway.SettingsEdit) { ed.Limits.TokenAllowance = domain.MaxSafeInteger },
			"bad credential ref":   func(ed *gateway.SettingsEdit) { ed.Credential.Ref = "secret://tenant/x" },
			"zero credential pin":  func(ed *gateway.SettingsEdit) { ed.Credential.Version = 0 },
			"huge output bound":    func(ed *gateway.SettingsEdit) { ed.Limits.MaxOutputTokens = gateway.MaxOutputCeiling + 1 },
		} {
			ed := e4.Edit()
			mutate(&ed)
			if _, err := e4.TryConfigure(e4.Replica, scopeA, ed); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		var n int
		e4.Scalar(&n, `SELECT count(*) FROM model_settings_revisions`)
		if n != 0 {
			t.Fatalf("%d revisions stored from invalid edits", n)
		}
	})
}
