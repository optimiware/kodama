package storage

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestManifestRejectsDuplicateAcrossContributions(t *testing.T) {
	a := Contribution{Module: "a", Migrations: []Migration{{ID: "T010_001_a.sql", SQL: "SELECT 1"}}}
	b := Contribution{Module: "b", Migrations: []Migration{{ID: "T010_001_a.sql", SQL: "SELECT 2"}}}
	_, err := NewManifest(a, b)
	if !errors.Is(err, ErrDuplicateMigration) {
		t.Fatalf("err = %v, want ErrDuplicateMigration", err)
	}
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("error should name both modules: %v", err)
	}
}

func TestManifestRejectsDuplicateWithinContribution(t *testing.T) {
	c := Contribution{Module: "a", Migrations: []Migration{
		{ID: "T010_001_a.sql", SQL: "SELECT 1"}, {ID: "T010_001_a.sql", SQL: "SELECT 1"},
	}}
	if _, err := NewManifest(c); !errors.Is(err, ErrDuplicateMigration) {
		t.Fatalf("err = %v, want ErrDuplicateMigration", err)
	}
}

func TestManifestRejectsInvalidIDs(t *testing.T) {
	for _, id := range []string{"", "001_base.sql", "T10_001_a.sql", "T010_001_A.sql", "T010_001_a.SQL", "T010_001_a", "../T010_001_a.sql", "T010_001_a.sql\n"} {
		c := Contribution{Module: "a", Migrations: []Migration{{ID: id, SQL: "SELECT 1"}}}
		if _, err := NewManifest(c); !errors.Is(err, ErrInvalidMigrationID) {
			t.Errorf("id %q: err = %v, want ErrInvalidMigrationID", id, err)
		}
	}
	empty := Contribution{Module: "a", Migrations: []Migration{{ID: "T010_001_a.sql", SQL: " \n"}}}
	if _, err := NewManifest(empty); !errors.Is(err, ErrInvalidMigrationID) {
		t.Errorf("empty SQL: err = %v", err)
	}
}

func TestManifestOrderIsExplicitAcrossContributions(t *testing.T) {
	late := Contribution{Module: "late", Migrations: []Migration{{ID: "T001_001_x.sql", SQL: "SELECT 1"}}}
	early := Contribution{Module: "early", Migrations: []Migration{{ID: "T099_001_y.sql", SQL: "SELECT 1"}}}
	m, err := NewManifest(early, late) // lexical order would be the reverse
	if err != nil {
		t.Fatal(err)
	}
	got := m.Migrations()
	if got[0].ID != "T099_001_y.sql" || got[1].ID != "T001_001_x.sql" {
		t.Fatalf("manifest order = %v", got)
	}
	got[0].ID = "mutated"
	if m.Migrations()[0].ID != "T099_001_y.sql" {
		t.Error("Migrations must return a copy")
	}
}

func TestManifestRejectsUnorderedContribution(t *testing.T) {
	c := Contribution{Module: "a", Migrations: []Migration{{ID: "T010_002_b.sql", SQL: "SELECT 1"}, {ID: "T010_001_a.sql", SQL: "SELECT 1"}}}
	if _, err := NewManifest(c); !errors.Is(err, ErrMigrationOrder) {
		t.Fatalf("err = %v, want ErrMigrationOrder", err)
	}
}

func TestLoadFSOrdersAndValidates(t *testing.T) {
	fsys := fstest.MapFS{
		"m/T010_002_b.sql": {Data: []byte("SELECT 2")},
		"m/T010_001_a.sql": {Data: []byte("SELECT 1")},
		"m/README.md":      {Data: []byte("ignored")},
	}
	ms, err := LoadFS(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "T010_001_a.sql" || ms[1].SQL != "SELECT 2" {
		t.Fatalf("LoadFS = %+v", ms)
	}
	bad := fstest.MapFS{"m/base.sql": {Data: []byte("SELECT 1")}}
	if _, err := LoadFS(bad, "m"); !errors.Is(err, ErrInvalidMigrationID) {
		t.Fatalf("err = %v, want ErrInvalidMigrationID", err)
	}
}

func TestChecksumIsStableAndContentSensitive(t *testing.T) {
	a := Migration{ID: "T010_001_a.sql", SQL: "SELECT 1"}
	if a.Checksum() != (Migration{ID: "other", SQL: "SELECT 1"}).Checksum() || len(a.Checksum()) != 64 {
		t.Error("checksum must depend only on SQL and be hex SHA-256")
	}
	if a.Checksum() == (Migration{SQL: "SELECT 1 "}).Checksum() {
		t.Error("checksum must change with content")
	}
}

func TestBaseContributionIsValid(t *testing.T) {
	c := BaseContribution()
	if len(c.Migrations) == 0 {
		t.Fatal("no base migrations embedded")
	}
	if _, err := NewManifest(c); err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Migrations {
		if !strings.HasPrefix(m.ID, "T002_") {
			t.Errorf("base migration %q lacks the T002_ prefix", m.ID)
		}
	}
}

func TestPlanDetectsHistoryConflicts(t *testing.T) {
	a := Migration{ID: "T010_001_a.sql", SQL: "A"}
	b := Migration{ID: "T010_002_b.sql", SQL: "B"}
	c := Migration{ID: "T010_003_c.sql", SQL: "C"}
	ms := []Migration{a, b, c}

	pending, err := plan(ms, map[string]string{a.ID: a.Checksum()})
	if err != nil || len(pending) != 2 || pending[0] != 1 {
		t.Fatalf("pending = %v, err = %v", pending, err)
	}
	if _, err := plan(ms, map[string]string{a.ID: "deadbeef"}); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("modified: err = %v", err)
	}
	if _, err := plan(ms, map[string]string{a.ID: a.Checksum(), c.ID: c.Checksum()}); !errors.Is(err, ErrMigrationOrder) {
		t.Errorf("gap: err = %v", err)
	}
	if _, err := plan(ms, map[string]string{"T999_001_gone.sql": "x"}); !errors.Is(err, ErrUnknownMigration) {
		t.Errorf("unknown: err = %v", err)
	}
}

func TestExpandAllRuntimeRole(t *testing.T) {
	ms := []Migration{{ID: "T010_001_a.sql", SQL: "GRANT SELECT ON t TO " + RuntimeRolePlaceholder}, {ID: "T010_002_b.sql", SQL: "SELECT 1"}}
	if _, err := expandAll(ms, ""); !errors.Is(err, ErrRuntimeRoleRequired) {
		t.Fatalf("err = %v, want ErrRuntimeRoleRequired", err)
	}
	out, err := expandAll(ms, `weird"role`)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != `GRANT SELECT ON t TO "weird""role"` || out[1] != "SELECT 1" {
		t.Fatalf("expanded = %q", out)
	}
	if _, err := expandAll(ms[1:], ""); err != nil {
		t.Errorf("role not required without placeholder: %v", err)
	}
}
