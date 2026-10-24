package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

func (e *env) deleteSecret(scope domain.Scope, id string, expected domain.Version) (secrets.Secret, secrets.RetentionReport, error) {
	var (
		s secrets.Secret
		r secrets.RetentionReport
	)
	err := e.svc.Atomically(e.ctx(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		s, r, err = e.svc.DeleteSecret(ctx, w, e.mutation("alice"), scope, id, expected)
		return err
	})
	return s, r, err
}

// Test_deletion_retention covers VS-12, VS-A09: pins, unknown backup retention
// and pending consumer handling remain visible after deletion; tombstones and
// audit survive; nothing is physically purged.
func Test_deletion_retention(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const v1, v2 = "old-SENTINEL", "new-SENTINEL"

	sec := e.createSecret(scopeA1, "erp-token", v1)
	if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.pin(scopeA1, "run_2", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	sec = e.rotate(scopeA1, sec.ID(), v2)
	if err := e.pin(scopeA1, "run_3", "erp-token", 2); err != nil {
		t.Fatal(err)
	}
	e.revoke(scopeA1, sec.ID(), 1) // run_1 and run_2 now owe handling
	sec, _ = e.store.GetSecret(ctx, scopeA1, sec.ID())
	preCiphertexts := e.count(`SELECT count(*) FROM secret_versions`)

	// A stale ETag cannot delete.
	if _, _, err := e.deleteSecret(scopeA1, sec.ID(), sec.Version()-1); err == nil {
		t.Fatal("stale delete accepted")
	} else {
		var c *secrets.ConflictError
		if !errors.As(err, &c) {
			t.Fatalf("stale delete: %v", err)
		}
	}
	if cur, _ := e.store.GetSecret(ctx, scopeA1, sec.ID()); cur.Deleted() {
		t.Fatal("a stale delete deleted the secret")
	}

	deleted, report, err := e.deleteSecret(scopeA1, sec.ID(), sec.Version())
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Deleted() || deleted.Version() != sec.Version()+1 {
		t.Fatalf("deleted secret: deleted %v version %d", deleted.Deleted(), deleted.Version())
	}
	// The report accounts for pins, pending consumer handling and unresolved backup retention.
	if report.PinnedVersions != 3 || report.ActivePins != 3 || report.PendingHandling != 4 || report.BackupRetention != secrets.BackupRetentionUnresolved {
		t.Fatalf("retention report %+v", report)
	}
	if again, err := e.store.RetentionReport(ctx, scopeA1, sec.ID()); err != nil || again != report {
		t.Fatalf("the report is not reproducible after deletion: %+v %v", again, err)
	}

	// The tombstone remains readable; list excludes it; its name is never reused.
	tomb, err := e.store.GetSecret(ctx, scopeA1, sec.ID())
	if err != nil || !tomb.Deleted() || tomb.Name() != "erp-token" || tomb.DeletedAt() == nil {
		t.Fatalf("tombstone: %v", err)
	}
	if list, err := e.store.ListSecrets(ctx, scopeA1, "", 10); err != nil || len(list) != 0 {
		t.Fatalf("deleted secret listed: %d %v", len(list), err)
	}
	if _, err := createSecretErr(e, scopeA1, "erp-token", "reuse"); !errors.Is(err, secrets.ErrNameTaken) {
		t.Fatalf("a deleted name was reused: %v", err)
	}
	if _, err := e.rotateErr(scopeA1, sec.ID(), "late"); !errors.Is(err, secrets.ErrDeleted) {
		t.Fatalf("a version was added to a deleted secret: %v", err)
	}
	if _, err := e.store.CurrentVersion(ctx, scopeA1, "erp-token"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a deleted secret is pinnable by name: %v", err)
	}
	if err := e.pin(scopeA1, "run_new", "erp-token", 2); !errors.Is(err, secrets.ErrDeleted) {
		t.Fatalf("a new pin of a deleted secret: %v", err)
	}

	// Deleting again is a no-op: no second audit entry, same report.
	deletes := func() int { return e.count(`SELECT count(*) FROM audit WHERE action = 'secret.delete'`) }
	if deletes() != 1 {
		t.Fatalf("%d delete audits", deletes())
	}
	if again, rep, err := e.deleteSecret(scopeA1, sec.ID(), deleted.Version()); err != nil || !again.Deleted() || rep != report || deletes() != 1 {
		t.Fatalf("repeated delete: %v %+v", err, rep)
	}

	// Retained: every version's ciphertext, every pin, every handling record, the audit trail and KEK references.
	if n := e.count(`SELECT count(*) FROM secret_versions`); n != preCiphertexts {
		t.Fatalf("ciphertext rows changed: %d -> %d", preCiphertexts, n)
	}
	if e.count(`SELECT count(*) FROM secret_pins`) != 3 || e.count(`SELECT count(*) FROM secret_consumer_handling WHERE state = 'pending'`) != 4 {
		t.Fatal("pins or handling were purged")
	}
	if e.count(`SELECT count(*) FROM secret_scope_key_wraps WHERE kek_version = 'v1'`) != 1 {
		t.Fatal("the KEK reference was not retained")
	}
	for _, a := range []string{"secret.create", "secret.rotate", "secret.revoke_version", "secret.delete"} {
		if !contains(e.auditActions(), a) {
			t.Errorf("audit lost %s", a)
		}
	}
	if pending, _ := e.store.PendingHandling(ctx, scopeA1, "", 10); len(pending) != 4 {
		t.Fatalf("pending handling after deletion: %d", len(pending))
	}

	// Pinned, unrevoked values stay resolvable for their runs (documented assumption);
	// the revoked pinned version stays blocked.
	auth := newAuthority()
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_3", AttemptID: "att_3", Bindings: []runtimeBinding{binding("erp-token", 2)}})
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
	r := e.resolver(auth)
	if resp, err := r.Resolve(ctx, "att_3", ref("erp-token", 2), "r"); err != nil || decode(t, resp) != v2 {
		t.Fatalf("a pinned value of a deleted secret: %v", err)
	}
	if _, err := r.Resolve(ctx, "att_1", ref("erp-token", 1), "r"); !errors.Is(err, secrets.ErrRevoked) {
		t.Fatalf("a revoked pinned value of a deleted secret: %v", err)
	}
	// A deleted secret can still be revoked (incident response).
	if res, err := e.revokeErr(scopeA1, sec.ID(), 2); err != nil || !res.Changed || res.PendingHandlingCount() != 2 {
		t.Fatalf("revoking a deleted secret's pinned version: %v", err)
	}
	if _, err := r.Resolve(ctx, "att_3", ref("erp-token", 2), "r"); !errors.Is(err, secrets.ErrRevoked) {
		t.Fatalf("resolution after revoking a deleted secret: %v", err)
	}

	// Releasing a run's pins lowers the active count but keeps the pin retained.
	e.releasePins(scopeA1, "run_1")
	rep, err := e.store.RetentionReport(ctx, scopeA1, sec.ID())
	if err != nil || rep.PinnedVersions != 3 || rep.ActivePins != 2 || rep.BackupRetention != secrets.BackupRetentionUnresolved {
		t.Fatalf("report after release: %+v %v", rep, err)
	}

	// Other scopes see no retention information.
	for _, sc := range []domain.Scope{scopeA2, scopeB1} {
		if _, err := e.store.RetentionReport(ctx, sc, sec.ID()); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("retention report visible to %v: %v", sc, err)
		}
	}
	notContains(t, "persisted metadata", e.dumpAll(), v1, v2, "SENTINEL")

	// Variables: logical deletion keeps the name and the value history.
	t.Run("variables", func(t *testing.T) {
		v := e.createVariable(scopeA1, "region", "eu")
		err := e.svc.Atomically(ctx, tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.DeleteVariable(ctx, w, e.mutation("alice"), scopeA1, v.ID(), 1)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if list, _ := e.store.ListVariables(ctx, scopeA1, "", 10); len(list) != 0 {
			t.Fatal("a deleted variable is listed")
		}
		if _, err := e.store.ActiveVariable(ctx, scopeA1, "region"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("a deleted variable is active: %v", err)
		}
		err = e.svc.Atomically(ctx, tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.CreateVariable(ctx, w, e.mutation("alice"), secrets.CreateVariableInput{Scope: scopeA1, Name: "region", Value: strValue("us")})
			return err
		})
		if !errors.Is(err, secrets.ErrNameTaken) {
			t.Fatalf("a deleted variable name was reused: %v", err)
		}
		if e.count(`SELECT count(*) FROM variable_versions`) != 1 {
			t.Fatal("variable history was purged")
		}
	})
}
