package app

import (
	"context"
	"regexp"
	"strings"
	"testing"

	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

func TestMigrationBaselineFiles(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	cs := Contributions()
	if len(cs) != 2 || cs[0].Module != "storage" || len(cs[0].Migrations) != 3 {
		t.Fatalf("contributions = %+v", cs)
	}
	if cs[1].Module != "execution-run-resources" || len(cs[1].Migrations) != 1 || cs[1].Migrations[0].ID != execpg.RunResourcesMigrationID {
		t.Fatalf("the appended contribution is %+v", cs[1])
	}
	ms := m.Migrations()
	if len(ms) != 4 || ms[0].ID != "001_schema.sql" || ms[1].ID != "002_grants.sql" || ms[2].ID != "003_delivery_targets.sql" || ms[3].ID != execpg.RunResourcesMigrationID {
		t.Fatalf("manifest = %+v", ms)
	}
	// The schema file defines objects: the only privilege change it may make is a
	// PUBLIC revocation beside a definition, never a grant to the runtime role.
	// The grants file holds runtime-role privileges and nothing else, so DDL
	// cannot be smuggled into it.
	grant := regexp.MustCompile(`(?m)^\s*(GRANT|REVOKE)\b`)
	for _, stmt := range statements(ms[0].SQL) {
		if strings.HasPrefix(stmt, "GRANT ") {
			t.Errorf("the schema file grants a privilege: %.80s", stmt)
		}
		if strings.HasPrefix(stmt, "REVOKE ") && !strings.Contains(stmt, "FROM PUBLIC") {
			t.Errorf("the schema file revokes a non-PUBLIC privilege: %.80s", stmt)
		}
	}
	grants := statements(ms[1].SQL)
	if len(grants) == 0 {
		t.Fatal("the grants file grants nothing")
	}
	for _, stmt := range grants {
		if !grant.MatchString(stmt) {
			t.Errorf("the grants file holds a non-privilege statement: %.80s", stmt)
		}
		if !strings.Contains(stmt, storage.RuntimeRolePlaceholder) {
			t.Errorf("a grants-file statement does not name the runtime role: %.80s", stmt)
		}
	}
}

func TestMigrateRefusesUnsafeIdentities(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]MigrateConfig{
		"no owner":           {RuntimeURL: "postgres://rt:p@h/db"},
		"no runtime":         {MigrationURL: "postgres://own:p@h/db"},
		"same role":          {MigrationURL: "postgres://same:p@h/db", RuntimeURL: "postgres://same:q@h/db"},
		"anonymous owner":    {MigrationURL: "postgres://h/db", RuntimeURL: "postgres://rt:p@h/db"},
		"same role by name":  {MigrationURL: "postgres://same:p@h/db", RuntimeRole: "same"},
		"both runtime forms": {MigrationURL: "postgres://own:p@h/db", RuntimeURL: "postgres://rt:p@h/db", RuntimeRole: "rt"},
	} {
		if _, err := Migrate(ctx, tc); err == nil {
			t.Errorf("%s: migration was attempted", name)
		}
	}
}

// TestRunResourceMigrationIsAppendedLastAndPreservesTheLedgerPrefix: the T-112
// run_shared schema is the final contribution, so the ledger of every earlier
// release is a strict prefix of the new manifest in the same order (the ledger
// refuses a pending migration ordered before an applied one), and it is the only
// addition: nothing earlier moved, nothing else was added.
func TestRunResourceMigrationIsAppendedLastAndPreservesTheLedgerPrefix(t *testing.T) {
	cs := Contributions()
	last := cs[len(cs)-1]
	if last.Module != "execution-run-resources" || len(last.Migrations) != 1 || last.Migrations[0].ID != execpg.RunResourcesMigrationID {
		t.Fatalf("the final contribution is %+v", last.Module)
	}
	prior := cs[:len(cs)-1]
	if got := prior[len(prior)-1].Module; got != "storage" {
		t.Fatalf("the previous last contribution moved: %s", got)
	}
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	all := m.Migrations()
	var priorIDs []string
	for _, c := range prior {
		for _, mig := range c.Migrations {
			priorIDs = append(priorIDs, mig.ID)
		}
	}
	if len(all) != len(priorIDs)+1 {
		t.Fatalf("%d migrations, want the %d of the prior release plus one", len(all), len(priorIDs))
	}
	for i, id := range priorIDs {
		if all[i].ID != id {
			t.Fatalf("migration %d is %s, the prior ledger has %s", i, all[i].ID, id)
		}
	}
	if all[len(all)-1].ID != execpg.RunResourcesMigrationID {
		t.Fatalf("last migration %s", all[len(all)-1].ID)
	}
	// The migration is additive and enables nothing: it creates tables and columns
	// only and no composition option exists in this package for the new format.
	sql := last.Migrations[0].SQL
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE", "DELETE FROM", "ALTER COLUMN"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("the migration is not additive: %s", forbidden)
		}
	}
}

// statements splits migration SQL into top-level statements, honouring
// comments, quoting and dollar-quoted function bodies so a semicolon inside one
// is not a boundary.
func statements(sql string) []string {
	var out []string
	var b strings.Builder
	i := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				i = len(sql)
			} else {
				i += j + 1
			}
			b.WriteByte('\n')
		case strings.HasPrefix(sql[i:], "/*"):
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				i = len(sql)
			} else {
				i += j + 4
			}
			b.WriteByte(' ')
		case sql[i] == '\'':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(sql[i:j])
			i = j
		case sql[i] == '$':
			j := strings.IndexByte(sql[i+1:], '$')
			if j >= 0 && namePattern.MatchString(sql[i+1:i+1+j]) {
				tag := sql[i : i+j+2]
				b.WriteString(tag)
				i += len(tag)
				k := strings.Index(sql[i:], tag)
				if k < 0 {
					b.WriteString(sql[i:])
					i = len(sql)
					continue
				}
				b.WriteString(sql[i : i+k])
				b.WriteString(tag)
				i += k + len(tag)
				continue
			}
			b.WriteByte(sql[i])
			i++
		case sql[i] == ';':
			if stmt := cleanStatement(b.String()); stmt != "" {
				out = append(out, stmt)
			}
			b.Reset()
			i++
		default:
			b.WriteByte(sql[i])
			i++
		}
	}
	if stmt := cleanStatement(b.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

var namePattern = regexp.MustCompile(`^[A-Za-z_0-9]*$`)

// cleanStatement normalizes whitespace.
func cleanStatement(s string) string { return strings.Join(strings.Fields(s), " ") }
