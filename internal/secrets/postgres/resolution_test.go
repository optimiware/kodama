package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

func ref(name string, version int64) runtimev1.SecretResolveRequest {
	return runtimev1.SecretResolveRequest{Ref: "secret://project/" + name, Version: version}
}

// Test_revoked_resolution is acceptance revoked_resolution (VS-08, VS-A06):
// revocation durably blocks later resolution, including for runs that already
// pinned the version, and records explicit consumer handling.
func Test_revoked_resolution(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const v1 = "revocable-SENTINEL"

	sec := e.createSecret(scopeA1, "erp-token", v1)
	for _, run := range []string{"run_1", "run_2", "run_3"} {
		if err := e.pin(scopeA1, run, "erp-token", 1); err != nil {
			t.Fatal(err)
		}
	}
	auth := newAuthority()
	for i, run := range []string{"run_1", "run_2", "run_3"} {
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: domain.RunID(run), AttemptID: domain.AttemptID("att_" + run[4:]), Bindings: []runtimeBinding{binding("erp-token", 1)}})
		_ = i
	}
	r := e.resolver(auth)

	// run_1 has already received the value; run_2 has not; run_3 terminated before revocation.
	if resp, err := r.Resolve(ctx, "att_1", ref("erp-token", 1), "req_a"); err != nil || decode(t, resp) != v1 {
		t.Fatalf("resolution before revocation: %v", err)
	}
	if n := e.releasePins(scopeA1, "run_3"); n != 1 {
		t.Fatalf("released %d pins", n)
	}

	res := e.revoke(scopeA1, sec.ID(), 1)
	if !res.Changed || !res.Version.Revoked() || res.Secret.Version() != 2 || res.Secret.CurrentVersion() != 1 {
		t.Fatalf("revocation result: changed %v revoked %v version %d", res.Changed, res.Version.Revoked(), res.Secret.Version())
	}
	// Two effects for each of the two live consumers; the released run owes nothing.
	if len(res.Handling) != 4 || res.PendingHandlingCount() != 4 {
		t.Fatalf("handling %d pending %d", len(res.Handling), res.PendingHandlingCount())
	}
	delivered := map[string]bool{}
	effects := map[secrets.HandlingEffect]int{}
	for _, h := range res.Handling {
		rec := h.Record()
		delivered[rec.Consumer.ID] = rec.Delivered
		effects[rec.Effect]++
		if rec.State != secrets.HandlingPending || rec.Consumer.ID == "run_3" {
			t.Fatalf("unexpected handling %+v", rec)
		}
	}
	if !delivered["run_1"] || delivered["run_2"] {
		t.Fatalf("delivery accounting: %v", delivered)
	}
	if effects[secrets.EffectTerminateConsumer] != 2 || effects[secrets.EffectRotateIssuedCredential] != 2 {
		t.Fatalf("effects %v", effects)
	}

	// Later resolution is blocked for every pinned consumer; there is no fallback to another version.
	for _, att := range []string{"att_1", "att_2"} {
		if resp, err := r.Resolve(ctx, domain.AttemptID(att), ref("erp-token", 1), "req_b"); !errors.Is(err, secrets.ErrRevoked) || resp.ValueBase64 != "" {
			t.Fatalf("%s resolved a revoked version: %v", att, err)
		}
	}
	if _, err := e.store.CurrentVersion(ctx, scopeA1, "erp-token"); !errors.Is(err, secrets.ErrNoActiveVersion) {
		t.Fatalf("revoked current version is pinnable: %v", err)
	}
	if err := e.pin(scopeA1, "run_9", "erp-token", 1); !errors.Is(err, secrets.ErrRevoked) {
		t.Fatalf("new pin of a revoked version: %v", err)
	}
	if e.count(`SELECT count(*) FROM secret_pins WHERE consumer_id = 'run_9'`) != 0 {
		t.Fatal("a refused pin left a row")
	}

	// Revocation is idempotent and irreversible.
	again := e.revoke(scopeA1, sec.ID(), 1)
	if again.Changed || len(again.Handling) != 4 || again.Secret.Version() != 2 {
		t.Fatalf("repeated revocation: changed %v handling %d version %d", again.Changed, len(again.Handling), again.Secret.Version())
	}
	if !again.Version.RevokedAt().Equal(*res.Version.RevokedAt()) {
		t.Fatal("repeated revocation moved the revocation time")
	}
	if _, err := e.revokeErr(scopeA1, sec.ID(), 7); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("revoking a missing version: %v", err)
	}
	if _, err := e.revokeErr(scopeA2, sec.ID(), 1); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("revoking through another project: %v", err)
	}
	if _, err := e.revokeErr(scopeB1, sec.ID(), 1); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("revoking through another tenant: %v", err)
	}

	// Rotation after revocation yields a new, resolvable version; the old one stays revoked.
	e.rotate(scopeA1, sec.ID(), "replacement-SENTINEL")
	if v, err := e.store.CurrentVersion(ctx, scopeA1, "erp-token"); err != nil || v != 2 {
		t.Fatalf("current version after rotation: %d %v", v, err)
	}
	if _, err := r.Resolve(ctx, "att_1", ref("erp-token", 1), "req_c"); !errors.Is(err, secrets.ErrRevoked) {
		t.Fatalf("revoked version resolvable after rotation: %v", err)
	}

	// Pending handling is restart-safe: read page by page, acknowledge, read again.
	var all []secrets.ConsumerHandling
	after := ""
	for {
		page, err := e.store.PendingHandling(ctx, scopeA1, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].ID()
	}
	if len(all) != 4 {
		t.Fatalf("pending handling listed %d", len(all))
	}
	ack := func(id string, outcome secrets.HandlingState, actor string) (secrets.ConsumerHandling, error) {
		var out secrets.ConsumerHandling
		err := e.svc.Atomically(ctx, tenantA, func(ctx context.Context, w secrets.Writer) error {
			var err error
			m := e.mutation(actor)
			out, err = w.AcknowledgeHandling(ctx, m, scopeA1, id, outcome)
			return err
		})
		return out, err
	}
	h, err := ack(all[0].ID(), secrets.HandlingCompleted, "ops")
	if err != nil || h.State() != secrets.HandlingCompleted {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := ack(all[0].ID(), secrets.HandlingCompleted, "ops-2"); err != nil {
		t.Fatalf("repeating an acknowledgement: %v", err)
	}
	if _, err := ack(all[0].ID(), secrets.HandlingNotApplicable, "ops"); !errors.Is(err, secrets.ErrInvalidTransition) {
		t.Fatalf("rewriting an acknowledgement: %v", err)
	}
	if _, err := ack(all[1].ID(), secrets.HandlingNotApplicable, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := ack("ch_missing", secrets.HandlingCompleted, "ops"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("acknowledging a missing record: %v", err)
	}
	pending, err := e.store.PendingHandling(ctx, scopeA1, "", 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending after two acknowledgements: %d %v", len(pending), err)
	}
	// Other projects and tenants see nothing.
	for _, sc := range []domain.Scope{scopeA2, scopeB1} {
		if p, err := e.store.PendingHandling(ctx, sc, "", 10); err != nil || len(p) != 0 {
			t.Fatalf("handling visible to %v: %d %v", sc, len(p), err)
		}
	}
	acks := 0
	for _, a := range e.auditActions() {
		if a == "secret.handling_acknowledged" {
			acks++
		}
	}
	if acks != 2 || !contains(e.auditActions(), "secret.revoke_version") || !contains(e.auditActions(), "secret.resolve") {
		t.Fatalf("audit actions %v", e.auditActions())
	}
	notContains(t, "persisted metadata", e.dumpAll(), v1, "SENTINEL")
}

// Test_resolution_revocation_race: revocation during unwrap is observed by
// finalization; audit failure and an unknown commit release no plaintext.
func Test_resolution_revocation_race(t *testing.T) {
	setup := func(t *testing.T) (*env, secrets.Secret, *secrets.Resolver) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "raced-SENTINEL")
		if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
			t.Fatal(err)
		}
		auth := newAuthority()
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
		return e, sec, e.resolver(auth)
	}
	resolutions := func(e *env) int { return e.count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve'`) }
	counter := func(e *env) int {
		return e.count(`SELECT resolution_count FROM secret_pins WHERE consumer_id = 'run_1'`)
	}

	t.Run("revocation during unwrap", func(t *testing.T) {
		e, sec, r := setup(t)
		fired := false
		e.wrapper.set(func(f *faultyWrapper) {
			f.beforeUnwrap = func(ctx context.Context) {
				if !fired {
					fired = true
					e.revoke(scopeA1, sec.ID(), 1) // committed while the value is being decrypted
				}
			}
		})
		resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_1")
		if !errors.Is(err, secrets.ErrRevoked) {
			t.Fatalf("revocation during decryption was not observed: %v", err)
		}
		if resp.ValueBase64 != "" || resp.Ref != "" {
			t.Fatal("a response accompanied the revocation")
		}
		if resolutions(e) != 0 || counter(e) != 0 {
			t.Fatal("a resolution was audited for a revoked version")
		}
	})

	t.Run("pin released during unwrap (cancellation)", func(t *testing.T) {
		e, _, r := setup(t)
		fired := false
		e.wrapper.set(func(f *faultyWrapper) {
			f.beforeUnwrap = func(ctx context.Context) {
				if !fired {
					fired = true
					e.releasePins(scopeA1, "run_1")
				}
			}
		})
		if _, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_1"); !errors.Is(err, secrets.ErrAttemptInactive) {
			t.Fatalf("cancellation during decryption was not observed: %v", err)
		}
		if resolutions(e) != 0 {
			t.Fatal("a resolution was audited after the pins were released")
		}
	})

	t.Run("audit failure releases no plaintext", func(t *testing.T) {
		e, _, r := setup(t)
		e.ownerExec(`CREATE FUNCTION t012_fail_resolve_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action = 'secret.resolve' THEN RAISE EXCEPTION 'audit store unavailable'; END IF; RETURN NEW; END $$;
CREATE TRIGGER t012_fail_resolve_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t012_fail_resolve_audit();`)
		resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_1")
		if err == nil || resp.ValueBase64 != "" || resp.Ref != "" {
			t.Fatalf("resolved despite audit failure: %v", err)
		}
		if counter(e) != 0 {
			t.Fatal("resolution counter advanced without an audit entry")
		}
		e.ownerExec(`DROP TRIGGER t012_fail_resolve_audit ON audit`)
		if resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_2"); err != nil || decode(t, resp) != "raced-SENTINEL" {
			t.Fatalf("resolution after the audit store recovered: %v", err)
		}
		if resolutions(e) != 1 || counter(e) != 1 {
			t.Fatalf("resolutions %d counter %d", resolutions(e), counter(e))
		}
		// A retry is a new, separately audited resolution.
		if _, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_3"); err != nil {
			t.Fatal(err)
		}
		if resolutions(e) != 2 {
			t.Fatalf("retry was not audited separately: %d", resolutions(e))
		}
	})

	t.Run("unknown commit releases no plaintext", func(t *testing.T) {
		e, _, _ := setup(t)
		flaky := &commitFailDB{TxBeginner: e.base.Runtime}
		store, err := secretspg.NewStore(secretspg.Config{DB: flaky, Events: secretspg.EventsFrom(e.ev), IDs: secrets.RandomIDs{}})
		if err != nil {
			t.Fatal(err)
		}
		src, err := secretspg.NewVersionSource(store, e.wrapper, installation)
		if err != nil {
			t.Fatal(err)
		}
		auth := newAuthority()
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
		r, _ := secrets.NewResolver(auth, src)
		flaky.failCommits(true)
		resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_1")
		if !errors.Is(err, secrets.ErrFinalizationUncertain) || resp.ValueBase64 != "" {
			t.Fatalf("unknown commit: %v", err)
		}
		flaky.failCommits(false)
		if _, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_2"); err != nil {
			t.Fatalf("retry after an unknown commit: %v", err)
		}
	})
}

// commitFailDB makes every Commit fail (after rolling back), standing in for a
// connection lost at commit time.
type commitFailDB struct {
	storage.TxBeginner
	fail bool
}

func (d *commitFailDB) failCommits(v bool) { d.fail = v }

func (d *commitFailDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.TxBeginner.BeginTx(ctx, opts)
	if err != nil || !d.fail || opts.AccessMode == pgx.ReadOnly {
		return tx, err
	}
	return &failingCommitTx{Tx: tx}, nil
}

type failingCommitTx struct{ pgx.Tx }

func (t *failingCommitTx) Commit(ctx context.Context) error {
	_ = t.Tx.Rollback(ctx)
	return errors.New("connection reset during commit")
}
