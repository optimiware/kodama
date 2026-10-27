package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/storage"
)

var chatopsTables = []string{"chatops_destinations", "chatops_deliveries", "chatops_delivery_attempts", "chatops_replays", "chatops_delivery_fences"}

func ledger(t *testing.T, e *chatopstest.Env) []string {
	t.Helper()
	rows, err := e.Owner.Query(context.Background(), `SELECT id || '|' || checksum FROM schema_migrations ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

func retained(t *testing.T, e *chatopstest.Env) string {
	t.Helper()
	var parts []string
	for _, table := range chatopsTables {
		var sum string
		if err := e.Owner.QueryRow(context.Background(), `SELECT coalesce(md5(string_agg(t::text, E'\n' ORDER BY t::text)), '') || ':' || count(*) FROM `+table+` t`).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, table+"="+sum)
	}
	return strings.Join(parts, ";")
}

func privileges(t *testing.T, e *chatopstest.Env) []string {
	t.Helper()
	rows, err := e.Owner.Query(context.Background(), `
SELECT 't:' || table_name || ':' || privilege_type FROM information_schema.table_privileges WHERE grantee = $1 AND table_name = ANY($2)
UNION ALL
SELECT 'c:' || table_name || '.' || column_name FROM information_schema.column_privileges WHERE grantee = $1 AND table_name = ANY($2) AND privilege_type = 'UPDATE'
ORDER BY 1`, e.DB.RuntimeRole, chatopsTables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

// TestMigrationReapply: migration ordering, reapplication, constraints and runtime
// grants. Real PostgreSQL.
func TestMigrationReapply(t *testing.T) {
	ctx := context.Background()
	e := chatopstest.New(t, chatopstest.Options{})
	e.SeedProject(scopeA)
	a := e.Actors(scopeA, "mig")
	_, ids := seed(t, e, a, "mig", 2)
	manifest, err := storage.NewManifest(chatopstest.Contributions()...)
	if err != nil {
		t.Fatal(err)
	}
	opts := storage.MigrateOptions{RuntimeRole: e.DB.RuntimeRole}

	t.Run("the baseline is recorded exactly once", func(t *testing.T) {
		var got []string
		for _, line := range ledger(t, e) {
			got = append(got, strings.SplitN(line, "|", 2)[0])
		}
		if fmt.Sprint(got) != "[001_schema.sql 002_grants.sql]" {
			t.Fatalf("ledger = %v", got)
		}
	})

	t.Run("reapplication is a no-op for the ledger, the records and the privileges", func(t *testing.T) {
		led, rec, priv := ledger(t, e), retained(t, e), privileges(t, e)
		for range 2 {
			res, err := storage.Migrate(ctx, e.Owner, manifest, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) != 0 {
				t.Fatalf("reapplication = %+v", res)
			}
		}
		if fmt.Sprint(ledger(t, e)) != fmt.Sprint(led) || retained(t, e) != rec || fmt.Sprint(privileges(t, e)) != fmt.Sprint(priv) {
			t.Fatal("reapplication changed the ledger, the retained records or the privileges")
		}
	})

	t.Run("a modified applied migration is refused", func(t *testing.T) {
		altered := storage.BaseContribution()
		altered.Migrations[0].SQL += "\nALTER TABLE chatops_deliveries ADD COLUMN t041_tamper int;\n"
		tampered, err := storage.NewManifest(altered)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, e.Owner, tampered, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		if e.Count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'chatops_deliveries' AND column_name = 't041_tamper'`) != 0 {
			t.Fatal("a tampered migration changed the schema")
		}
	})

	t.Run("the runtime role holds minimal privileges and nothing is deletable", func(t *testing.T) {
		priv := strings.Join(privileges(t, e), "\n")
		for _, forbidden := range []string{":DELETE", ":TRUNCATE", ":REFERENCES", ":TRIGGER"} {
			if strings.Contains(priv, forbidden) {
				t.Errorf("the runtime role holds %s:\n%s", forbidden, priv)
			}
		}
		for _, immutable := range []string{"chatops_deliveries.payload", "chatops_deliveries.event_id", "chatops_deliveries.destination_id", "chatops_deliveries.tenant_id",
			"chatops_destinations.tenant_id", "chatops_destinations.id", "chatops_delivery_attempts.claim_token", "chatops_replays.reason"} {
			if strings.Contains(priv, "c:"+immutable+"\n") || strings.HasSuffix(priv, "c:"+immutable) {
				t.Errorf("the runtime role may update immutable column %s", immutable)
			}
		}
		// Replay metadata and attempts have no UPDATE; destinations and deliveries only the listed columns.
		if strings.Contains(priv, "t:chatops_replays:UPDATE") || strings.Contains(priv, "c:chatops_replays.") {
			t.Error("replay audit metadata is updatable")
		}
		// Behaviourally: a direct DELETE and a direct payload rewrite are refused.
		for _, sql := range []string{`DELETE FROM chatops_deliveries`, `DELETE FROM chatops_destinations`, `DELETE FROM chatops_delivery_attempts`,
			`UPDATE chatops_deliveries SET event_id = 'forged'`, `UPDATE chatops_deliveries SET payload = '{"x":1}'::bytea`} {
			_, err := e.Runtime.Exec(ctx, sql)
			if err == nil {
				t.Errorf("%q succeeded for the runtime role", sql)
			}
		}
	})

	t.Run("constraints and guards reject malformed and non-transitional rows even for the owner", func(t *testing.T) {
		bad := map[string]string{
			"bad name":               `UPDATE chatops_destinations SET name = 'Not Valid'`,
			"unknown event":          `UPDATE chatops_destinations SET events = ARRAY['usage.recorded']`,
			"empty events":           `UPDATE chatops_destinations SET events = ARRAY[]::text[]`,
			"http endpoint":          `UPDATE chatops_destinations SET endpoint = 'http://x.example/h', binding_revision = binding_revision + 1, version = version + 1`,
			"userinfo endpoint":      `UPDATE chatops_destinations SET endpoint = 'https://u:p@x.example/h', binding_revision = binding_revision + 1, version = version + 1`,
			"identity rewrite":       `UPDATE chatops_destinations SET id = 'cdst_other', version = version + 1`,
			"version not advancing":  `UPDATE chatops_destinations SET name = 'other-name'`,
			"binding without cause":  `UPDATE chatops_destinations SET binding_revision = binding_revision + 1, version = version + 1`,
			"endpoint without bind":  `UPDATE chatops_destinations SET endpoint = 'https://x.example/h2', version = version + 1`,
			"state unknown":          `UPDATE chatops_deliveries SET state = 'done', version = version + 1`,
			"claimed without lease":  `UPDATE chatops_deliveries SET state = 'claimed', attempts = attempts + 1, claim_token = claim_token + 1, cycle_attempts = cycle_attempts + 1, version = version + 1`,
			"delivered without time": `UPDATE chatops_deliveries SET state = 'delivered', version = version + 1`,
			"failed without reason":  `UPDATE chatops_deliveries SET state = 'failed', version = version + 1`,
			"payload rewrite":        `UPDATE chatops_deliveries SET payload = '{"x":1}'::bytea, version = version + 1`,
			"token regression":       `UPDATE chatops_deliveries SET claim_token = claim_token - 1, version = version + 1`,
			"unknown class":          `UPDATE chatops_deliveries SET last_class = 'made_up', version = version + 1`,
			"receipt with space":     `UPDATE chatops_deliveries SET receipt = 'a b', version = version + 1`,
			"delete attempts":        `DELETE FROM chatops_delivery_attempts`,
			"skip a state":           `UPDATE chatops_deliveries SET state = 'delivered', delivered_at = now(), version = version + 1`,
		}
		// A claimed delivery with an attempt row, so attempt guards have a subject.
		b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), 60e9, 1, policy)
		if err != nil || len(b.Claims) != 1 {
			t.Fatalf("claim: %v %v", b, err)
		}
		for name, sql := range bad {
			if _, err := e.Owner.Exec(ctx, sql); err == nil {
				t.Errorf("%s: %q was accepted", name, sql)
			}
		}
		if _, err := e.Owner.Exec(ctx, `UPDATE chatops_delivery_attempts SET attempt = 9`); err == nil {
			t.Error("an attempt number was rewritten")
		}
		// A finished attempt is immutable.
		if _, err := e.Store.Finish(ctx, b.Claims[0], chatops.Outcome{Delivered: true, HTTPStatus: 200}, now(), policy); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Owner.Exec(ctx, `UPDATE chatops_delivery_attempts SET result = 'failed'`); err == nil {
			t.Error("a finished attempt was rewritten")
		}
		if _, err := e.Owner.Exec(ctx, `UPDATE chatops_deliveries SET state = 'pending', delivered_at = NULL, version = version + 1 WHERE state = 'delivered'`); err == nil {
			t.Error("a delivered notification reopened")
		}
		// The unique identity: the same (destination, event) cannot exist twice.
		if _, err := e.Owner.Exec(ctx, `INSERT INTO chatops_deliveries (tenant_id, project_id, id, destination_id, event_id, event_type, binding_revision, payload, payload_digest, state, next_attempt_at, version, created_at, updated_at)
SELECT tenant_id, project_id, 'cdl_duplicate', destination_id, event_id, event_type, binding_revision, payload, payload_digest, 'pending', now(), 1, now(), now() FROM chatops_deliveries LIMIT 1`); err == nil {
			t.Error("a second delivery for the same destination and event was inserted")
		}
		_ = ids
	})
}
