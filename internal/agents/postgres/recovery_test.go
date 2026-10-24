package postgres_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/storage"
)

func TestRegistrationRecovery(t *testing.T) {
	t.Run("audit failure rolls the profile back and a retry succeeds", func(t *testing.T) {
		e := newEnv(t)
		e.ownerExec(`CREATE FUNCTION t030_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't030 audit failure'; END $$`)
		e.ownerExec(`CREATE TRIGGER t030_fail_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t030_fail_audit()`)
		_, err := e.register(scopeA, engineer, e.draft)
		if err == nil {
			t.Fatal("registration must fail when its audit row cannot be written")
		}
		if errors.Is(err, agents.ErrOutcomeUnknown) {
			t.Fatalf("a statement failure is not an unknown outcome: %v", err)
		}
		if e.profiles(scopeA) != 0 || e.audits(scopeA) != 0 {
			t.Fatalf("partial write: %d profiles, %d audits", e.profiles(scopeA), e.audits(scopeA))
		}
		e.ownerExec(`DROP TRIGGER t030_fail_audit ON audit`)
		res, err := e.register(scopeA, engineer, e.draft)
		if err != nil || !res.Created || res.Profile.Version != 1 {
			t.Fatalf("retry after the fault: %+v %v", res, err)
		}
		if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("the retry must leave one profile and one audit row")
		}
	})

	t.Run("cancellation rolls back and is unavailable", func(t *testing.T) {
		e := newEnv(t)
		e.ownerExec(`CREATE FUNCTION t030_slow_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(5); RETURN NEW; END $$`)
		e.ownerExec(`CREATE TRIGGER t030_slow_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t030_slow_audit()`)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := e.svc.Register(ctx, agents.RegisterCommand{Scope: scopeA, Caller: agents.Caller{PrincipalID: engineer}, Draft: e.draft})
		if !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("the statement was not cancelled")
		}
		if e.profiles(scopeA) != 0 || e.audits(scopeA) != 0 {
			t.Fatal("a cancelled registration must leave nothing behind")
		}
		e.ownerExec(`DROP TRIGGER t030_slow_audit ON audit`)
		if res, err := e.register(scopeA, engineer, e.draft); err != nil || !res.Created {
			t.Fatalf("the pool must be usable after cancellation: %+v %v", res, err)
		}
	})

	t.Run("a lost commit reply is unknown and an identical retry converges on the original", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		flaky.mode = commitLostReply
		_, err := e.register(scopeA, engineer, e.draft)
		if !errors.Is(err, agents.ErrUnavailable) || !errors.Is(err, agents.ErrOutcomeUnknown) {
			t.Fatalf("a lost reply must be reported as an unknown outcome, got %v", err)
		}
		if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatalf("the commit did happen: %d profiles, %d audits", e.profiles(scopeA), e.audits(scopeA))
		}
		var original string
		if err := e.owner.QueryRow(context.Background(), `SELECT id FROM agent_profiles WHERE tenant_id = $1`, tenantA).Scan(&original); err != nil {
			t.Fatal(err)
		}
		flaky.mode = commitNormal
		res, err := e.register(scopeA, engineer, e.draft)
		if err != nil || res.Created || res.Profile.Version != 1 || string(res.Profile.ID) != original {
			t.Fatalf("the retry must find the committed profile: %+v %v", res, err)
		}
		if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("the retry must not add rows")
		}
	})

	t.Run("a commit that never landed is unknown and a retry creates one profile", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		flaky.mode = commitNeverLands
		if _, err := e.register(scopeA, engineer, e.draft); !errors.Is(err, agents.ErrOutcomeUnknown) {
			t.Fatalf("expected an unknown outcome, got %v", err)
		}
		if e.profiles(scopeA) != 0 || e.audits(scopeA) != 0 {
			t.Fatal("nothing was committed")
		}
		flaky.mode = commitNormal
		res, err := e.register(scopeA, engineer, e.draft)
		if err != nil || !res.Created || res.Profile.Version != 1 {
			t.Fatalf("retry: %+v %v", res, err)
		}
		if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("one profile and one audit row expected")
		}
	})

	t.Run("a lost reply on a duplicate is a plain unavailable failure", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		e.mustRegister(scopeA, engineer, e.draft)
		flaky.mode = commitLostReply
		_, err := e.register(scopeA, engineer, e.draft)
		if err == nil || errors.Is(err, agents.ErrOutcomeUnknown) {
			t.Fatalf("a read-only duplicate lookup has no unknown write outcome: %v", err)
		}
		if e.profiles(scopeA) != 1 {
			t.Fatal("no change expected")
		}
	})

	t.Run("immutable rows resist direct modification", func(t *testing.T) {
		e := newEnv(t)
		res := e.mustRegister(scopeA, engineer, e.draft)
		ctx := context.Background()
		// The runtime role has no UPDATE or DELETE privilege at all.
		if err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agent_profiles SET created_by_id = 'tamper' WHERE tenant_id = $1`, tenantA)
			return err
		}); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the runtime role must not update a profile: %v", err)
		}
		if err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM agent_profiles WHERE tenant_id = $1`, tenantA)
			return err
		}); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the runtime role must not delete a profile: %v", err)
		}
		// The owner is bound by forced row level security: no policy permits an
		// UPDATE or DELETE, so neither touches a row.
		for _, stmt := range []string{
			`UPDATE agent_profiles SET created_by_id = 'tamper'`,
			`UPDATE agent_profiles SET body = body`,
			`DELETE FROM agent_profiles`,
		} {
			tag, err := e.owner.Exec(ctx, stmt)
			if err == nil && tag.RowsAffected() != 0 {
				t.Fatalf("%s changed %d rows", stmt, tag.RowsAffected())
			}
		}
		// The stored content must match its digest: a forged row is refused.
		_, err := e.owner.Exec(ctx, `INSERT INTO agent_profiles (tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
			VALUES ($1, $2, 'ap_t030_forged', 'coding', 9, $3, $4, 'user', 'x', now())`,
			tenantA, projectA, string(res.Profile.Digest), string(res.Profile.Content.Canonical())+" ")
		if err == nil {
			t.Fatal("a body that does not hash to its digest must be refused")
		}
		got, err := e.store.Get(ctx, scopeA, "coding", 1)
		if err != nil || got.Digest != res.Profile.Digest || got.CreatedBy.ID != engineer {
			t.Fatalf("the stored profile is unchanged: %+v %v", got, err)
		}
	})

	t.Run("a corrupted stored row is never returned as a profile", func(t *testing.T) {
		e := newEnv(t)
		res := e.mustRegister(scopeA, engineer, e.draft)
		// Replace the row through the owner with table-level checks disabled, as a
		// corrupted restore would.
		e.ownerExec(`ALTER TABLE agent_profiles DROP CONSTRAINT agent_profiles_digest_matches_body`)
		e.ownerExec(`ALTER TABLE agent_profiles DISABLE ROW LEVEL SECURITY`)
		e.ownerExec(`UPDATE agent_profiles SET body = replace(body, '"wallSeconds":1800', '"wallSeconds":1801') WHERE id = $1`, string(res.Profile.ID))
		if _, err := e.store.Get(context.Background(), scopeA, "coding", 1); err == nil || agents.IsBusinessError(err) {
			t.Fatalf("corruption must surface as an internal failure, got %v", err)
		}
		if _, err := e.store.List(context.Background(), scopeA, agents.ListQuery{Limit: 10}); err == nil {
			t.Fatal("a listing must not return a corrupted row")
		}
	})
}
