package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// restoreOrder is the dependency order of the module's tables and their parents.
var restoreOrder = []string{
	"tenants", "projects", "secret_scope_keys", "secret_scope_key_wraps", "variables", "variable_versions",
	"secrets", "secret_versions", "secret_pins", "secret_consumer_handling",
}

type tableRows struct {
	table string
	cols  []string
	rows  [][]any
}

// backup reads every row of the module's tables as the owner: the encrypted
// state a database backup would hold. It contains no key material.
func (e *env) backup() []tableRows {
	e.t.Helper()
	var out []tableRows
	for _, tbl := range restoreOrder {
		colRows, err := e.base.Migration.Query(e.ctx(), `SELECT column_name FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, tbl)
		if err != nil {
			e.t.Fatal(err)
		}
		var cols []string
		for colRows.Next() {
			var c string
			if err := colRows.Scan(&c); err != nil {
				e.t.Fatal(err)
			}
			cols = append(cols, c)
		}
		colRows.Close()
		rows, err := e.base.Migration.Query(e.ctx(), fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(cols, ", "), tbl))
		if err != nil {
			e.t.Fatal(err)
		}
		tr := tableRows{table: tbl, cols: cols}
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				e.t.Fatal(err)
			}
			tr.rows = append(tr.rows, vals)
		}
		rows.Close()
		if rows.Err() != nil {
			e.t.Fatal(rows.Err())
		}
		out = append(out, tr)
	}
	return out
}

// restoreInto loads a backup into a fresh, migrated database in one
// transaction (deferred constraints are checked at commit) and returns a
// store over its runtime role.
func restoreInto(t *testing.T, b []tableRows) (*secretspg.Store, *pgxpool.Pool, *pgtest.Database) {
	t.Helper()
	ctx := context.Background()
	db := pgtest.New(t)
	mig := db.MigrationPool(ctx, 4)
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, mig, m, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate restore target: %v", err)
	}
	// Restored pins name runs; the restore target has no execution rows.
	pgtest.DropExecutionLinks(t, mig, "secret_pins", "secret_pins_run_fk")
	tx, err := mig.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, tr := range b {
		ph := make([]string, len(tr.cols))
		for i := range ph {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		sql := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, tr.table, strings.Join(tr.cols, ", "), strings.Join(ph, ", "))
		for _, row := range tr.rows {
			if _, err := tx.Exec(ctx, sql, row...); err != nil {
				t.Fatalf("restore %s: %v", tr.table, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("restore commit: %v", err)
	}
	rt := db.RuntimePool(ctx, 8)
	store, err := secretspg.NewStore(secretspg.Config{
		DB: rt, IDs: secrets.RandomIDs{},
		Events: func(context.Context, pgx.Tx, eventsv1Envelope) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, rt, db
}

// decryptAll decrypts every retained version of every secret in tenant t_acme
// using only the given provider, returning name/version -> plaintext.
func decryptAll(t *testing.T, store *secretspg.Store, provider secrets.KeyWrapper) (map[string]string, error) {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, scope := range []struct {
		name string
		s    secrets.Reader
	}{{"a1", store}} {
		list, err := scope.s.ListSecrets(ctx, scopeA1, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, sec := range list {
			vs, err := scope.s.ListSecretVersions(ctx, scopeA1, sec.ID(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vs {
				sk, err := store.GetScopeKey(ctx, tenantA, v.Record().ScopeKeyID)
				if err != nil {
					t.Fatal(err)
				}
				material, err := secrets.UnwrapScopeKey(ctx, provider, installation, sk)
				if err != nil {
					return out, err
				}
				pt, err := envelope.Open(material, v.EnvelopeContext(installation, sec.Name()), v.Envelope())
				clear(material)
				if err != nil {
					return out, err
				}
				out[fmt.Sprintf("%s/%d", sec.Name(), v.Version())] = string(pt)
			}
		}
	}
	return out, nil
}

// Test_development_restore is development restore evidence for VS-10 (the
// production qualification belongs to T-028/T-029): encrypted rows restored into
// a fresh database decrypt with the separately retained development KEKs, and a
// missing key fails closed. It does not qualify a production KMS.
func Test_development_restore(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const sentinel = "restore-SENTINEL"
	want := map[string]string{
		"erp-token/1": sentinel + "-erp-1", "erp-token/2": sentinel + "-erp-2", "git-deploy/1": sentinel + "-git-1",
	}
	erp := e.createSecret(scopeA1, "erp-token", want["erp-token/1"])
	e.createSecret(scopeA1, "git-deploy", want["git-deploy/1"])
	e.rotate(scopeA1, erp.ID(), want["erp-token/2"])
	if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	e.createVariable(scopeA1, "region", "eu")

	// Backup 1: taken while every wrap names KEK v1.
	backup1 := e.backup()

	// Rewrap under a new KEK; backup 2 holds revision 2 (KEK v2) and history.
	e.keys.add("v2")
	e.wrapper.set(func(f *faultyWrapper) { f.inner = e.keys.provider("v2", "v1", "v2") })
	if _, err := e.svc.RewrapScopeKey(ctx, e.mutation("ops"), tenantA, ""); err != nil {
		t.Fatal(err)
	}
	backup2 := e.backup()

	// A backup contains no plaintext and no key material.
	for _, b := range [][]tableRows{backup1, backup2} {
		for _, tr := range b {
			for _, row := range tr.rows {
				for _, v := range row {
					switch x := v.(type) {
					case string:
						if strings.Contains(x, "SENTINEL") {
							t.Fatalf("plaintext in backup table %s", tr.table)
						}
					case []byte:
						if bytes.Contains(x, []byte("SENTINEL")) {
							t.Fatalf("plaintext in backup table %s", tr.table)
						}
						for version, path := range e.keys.files {
							kek, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							if bytes.Contains(x, kek) {
								t.Fatalf("KEK %s found in backup table %s", version, tr.table)
							}
						}
					}
				}
			}
		}
	}

	only := func(versions ...string) *keyfile.Provider {
		return e.keys.provider(versions[len(versions)-1], versions...)
	}

	t.Run("backup 1 needs the retired KEK v1", func(t *testing.T) {
		store, _, _ := restoreInto(t, backup1)
		got, err := decryptAll(t, store, only("v1"))
		if err != nil {
			t.Fatalf("restore with the retained v1 key: %v", err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("decrypted %d versions, want %d", len(got), len(want))
		}
		// Both keys retained also works; only the new key fails closed.
		if _, err := decryptAll(t, store, only("v1", "v2")); err != nil {
			t.Fatal(err)
		}
		_, err = decryptAll(t, store, only("v2"))
		var ke *secrets.KeyError
		if !errors.As(err, &ke) || ke.Class != secrets.KeyNotFound {
			t.Fatalf("restore without the retained key did not fail closed: %v", err)
		}
	})

	t.Run("backup 2 needs the new KEK and keeps the old reference", func(t *testing.T) {
		store, _, db := restoreInto(t, backup2)
		got, err := decryptAll(t, store, only("v2"))
		if err != nil {
			t.Fatalf("restore with the new key: %v", err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		_, err = decryptAll(t, store, only("v1"))
		var ke *secrets.KeyError
		if !errors.As(err, &ke) || ke.Class != secrets.KeyNotFound {
			t.Fatalf("restore with only the retired key did not fail closed: %v", err)
		}
		// The wrap history (both KEK references) came back with the rows.
		mig := db.MigrationPool(context.Background(), 1)
		var versions string
		if err := mig.QueryRow(context.Background(), `SELECT string_agg(kek_version, ',' ORDER BY revision) FROM secret_scope_key_wraps`).Scan(&versions); err != nil || versions != "v1,v2" {
			t.Fatalf("restored wrap history %q: %v", versions, err)
		}

		// The restored store resolves through the normal path, including the restored pin.
		src, err := secretspg.NewVersionSource(store, only("v2"), installation)
		if err != nil {
			t.Fatal(err)
		}
		auth := newAuthority()
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
		r, _ := secrets.NewResolver(auth, src)
		resp, err := r.Resolve(context.Background(), "att_1", ref("erp-token", 1), "req")
		if err != nil || decode(t, resp) != want["erp-token/1"] {
			t.Fatalf("resolution in the restored database: %v", err)
		}
		// Variables are plaintext rows and simply restore.
		if v, err := store.ActiveVariable(context.Background(), scopeA1, "region"); err != nil || v.Value().String() != "eu" {
			t.Fatalf("restored variable: %v", err)
		}
		// With the wrong installation identity the same rows do not decrypt (AAD binding).
		srcOther, _ := secretspg.NewVersionSource(store, only("v2"), "another-installation")
		rOther, _ := secrets.NewResolver(auth, srcOther)
		if _, err := rOther.Resolve(context.Background(), "att_1", ref("erp-token", 1), "req"); err == nil {
			t.Fatal("rows decrypted under another installation identity")
		}
	})
}
