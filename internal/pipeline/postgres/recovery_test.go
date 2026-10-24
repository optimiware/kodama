package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

func Test_registration_recovery(t *testing.T) {
	src := pipelinetest.Doc(pipelinetest.Container("a", "args: [recover]"))

	t.Run("audit failure rolls the definition back and a retry succeeds", func(t *testing.T) {
		e := newEnv(t)
		e.ownerExec(`CREATE FUNCTION t014_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't014 audit failure'; END $$`)
		e.ownerExec(`CREATE TRIGGER t014_fail_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t014_fail_audit()`)
		_, err := e.register(scopeA, engineer, src)
		if err == nil {
			t.Fatal("registration must fail when its audit row cannot be written")
		}
		if errors.Is(err, pipeline.ErrOutcomeUnknown) {
			t.Fatalf("a statement failure is not an unknown outcome: %v", err)
		}
		if e.definitions(scopeA) != 0 || e.audits(scopeA) != 0 {
			t.Fatalf("partial write: %d definitions, %d audits", e.definitions(scopeA), e.audits(scopeA))
		}
		e.ownerExec(`DROP TRIGGER t014_fail_audit ON audit`)
		res, err := e.register(scopeA, engineer, src)
		if err != nil || !res.Created || res.Definition.Version != 1 {
			t.Fatalf("retry after the fault: %+v %v", res, err)
		}
		if e.definitions(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("the retry must leave one definition and one audit row")
		}
	})

	t.Run("cancellation rolls back and is unavailable", func(t *testing.T) {
		e := newEnv(t)
		e.ownerExec(`CREATE FUNCTION t014_slow_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(5); RETURN NEW; END $$`)
		e.ownerExec(`CREATE TRIGGER t014_slow_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t014_slow_audit()`)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := e.svc.Register(ctx, pipeline.RegisterCommand{
			Scope: scopeA, Caller: pipeline.Caller{PrincipalID: engineer}, Format: pipeline.FormatYAML, Body: []byte(src)})
		if !errors.Is(err, pipeline.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("the statement was not cancelled")
		}
		if e.definitions(scopeA) != 0 || e.audits(scopeA) != 0 {
			t.Fatal("a cancelled registration must leave nothing behind")
		}
		e.ownerExec(`DROP TRIGGER t014_slow_audit ON audit`)
		if res, err := e.register(scopeA, engineer, src); err != nil || !res.Created {
			t.Fatalf("the pool must be usable after cancellation: %+v %v", res, err)
		}
	})

	t.Run("a lost commit reply is unknown and a retry converges on one definition", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		flaky.mode = commitLostReply
		_, err := e.register(scopeA, engineer, src)
		if !errors.Is(err, pipeline.ErrUnavailable) || !errors.Is(err, pipeline.ErrOutcomeUnknown) {
			t.Fatalf("a lost reply must be reported as an unknown outcome, got %v", err)
		}
		if e.definitions(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatalf("the commit did happen: %d definitions, %d audits", e.definitions(scopeA), e.audits(scopeA))
		}
		flaky.mode = commitNormal
		res, err := e.register(scopeA, engineer, src)
		if err != nil || res.Created || res.Definition.Version != 1 {
			t.Fatalf("the retry must find the committed definition: %+v %v", res, err)
		}
		if e.definitions(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("the retry must not add rows")
		}
	})

	t.Run("a commit that never landed is unknown and a retry creates one definition", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		flaky.mode = commitNeverLands
		_, err := e.register(scopeA, engineer, src)
		if !errors.Is(err, pipeline.ErrOutcomeUnknown) {
			t.Fatalf("expected an unknown outcome, got %v", err)
		}
		if e.definitions(scopeA) != 0 {
			t.Fatal("nothing was committed")
		}
		flaky.mode = commitNormal
		res, err := e.register(scopeA, engineer, src)
		if err != nil || !res.Created || res.Definition.Version != 1 {
			t.Fatalf("retry: %+v %v", res, err)
		}
		if e.definitions(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("one definition and one audit row expected")
		}
	})

	t.Run("a lost reply on a duplicate is a plain unavailable failure", func(t *testing.T) {
		flaky := &flakyDB{}
		e := newEnvWith(t, envOptions{wrap: func(inner storage.TxBeginner) storage.TxBeginner { flaky.inner = inner; return flaky }})
		e.mustRegister(scopeA, engineer, src)
		flaky.mode = commitLostReply
		_, err := e.register(scopeA, engineer, src)
		if err == nil || errors.Is(err, pipeline.ErrOutcomeUnknown) {
			t.Fatalf("a read-only duplicate lookup has no unknown write outcome: %v", err)
		}
		if e.definitions(scopeA) != 1 {
			t.Fatal("no change expected")
		}
	})

	t.Run("lock timeout reports unavailability, duplicates need no lock", func(t *testing.T) {
		e := newEnvWith(t, envOptions{store: pipelinepg.Options{LockTimeout: 250 * time.Millisecond}})
		existing := e.mustRegister(scopeA, engineer, src)

		ctx := context.Background()
		holder, err := e.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holder.Rollback(ctx)
			}
		}
		defer release()
		if _, err := holder.Exec(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantA, projectA); err != nil {
			t.Fatal(err)
		}

		started := time.Now()
		_, err = e.register(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a", "args: [blocked]")))
		if !errors.Is(err, pipeline.ErrUnavailable) || errors.Is(err, pipeline.ErrOutcomeUnknown) {
			t.Fatalf("expected unavailable after the lock timeout, got %v", err)
		}
		if d := time.Since(started); d > 3*time.Second {
			t.Fatalf("lock wait was not bounded: %v", d)
		}
		dup, err := e.register(scopeA, engineer, src)
		if err != nil || dup.Created || dup.Definition.ID != existing.Definition.ID {
			t.Fatalf("an existing definition is returned without the project lock: %+v %v", dup, err)
		}
		other, err := e.register(scopeA2, engineer, src)
		if err != nil || !other.Created {
			t.Fatalf("another project is unaffected: %+v %v", other, err)
		}
		release()
		if res, err := e.register(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a", "args: [blocked]"))); err != nil || !res.Created || res.Definition.Version != 2 {
			t.Fatalf("after the lock is released: %+v %v", res, err)
		}
		// The transaction-local lock timeout must not leak into pooled sessions.
		if err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var v string
			if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&v); err != nil {
				return err
			}
			if v != "0" {
				t.Errorf("lock_timeout leaked into the session: %s", v)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an unknown project is not found", func(t *testing.T) {
		e := newEnv(t)
		e.auth.Grant(scopeA, "u_other", pipeline.ActorUser, pipeline.PermRegister)
		ghost := scopeA
		ghost.ProjectID = "p_ghost"
		e.auth.Grant(ghost, engineer, pipeline.ActorUser, pipeline.PermRegister)
		_, err := e.register(ghost, engineer, src)
		if !errors.Is(err, pipeline.ErrNotFound) {
			t.Fatalf("expected not found, got %v", err)
		}
	})

	t.Run("the store rejects malformed candidates", func(t *testing.T) {
		e := newEnv(t)
		v, _ := e.v.ValidateYAML(context.Background(), scopeA, pipeline.Actor{ID: engineer, Type: pipeline.ActorUser}, []byte(src))
		long := make([]byte, 125)
		for i := range long {
			long[i] = 'a'
		}
		_, _, err := e.store.RegisterOrGet(context.Background(), pipeline.Candidate{
			Scope: scopeA, ID: domain.DefinitionID("pd_" + string(long)), Content: v, CreatedBy: pipeline.Actor{ID: engineer, Type: pipeline.ActorUser}, CreatedAt: time.Now()})
		if !errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("an over-long identifier must be rejected: %v", err)
		}
		_, _, err = e.store.RegisterOrGet(context.Background(), pipeline.Candidate{})
		if !errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("an empty candidate must be rejected: %v", err)
		}
		if e.definitions(scopeA) != 0 {
			t.Fatal("nothing may be written")
		}
	})
}
