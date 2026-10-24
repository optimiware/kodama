package postgres

import (
	"strings"
	"testing"
)

func TestManifestDigestIdentifiesOrderedEntries(t *testing.T) {
	a := ManifestDigest("T002_001|aa", "T002_002|bb")
	if a != ManifestDigest("T002_001|aa", "T002_002|bb") {
		t.Fatal("digest is not deterministic")
	}
	for name, other := range map[string]string{
		"reordered": ManifestDigest("T002_002|bb", "T002_001|aa"),
		"edited":    ManifestDigest("T002_001|aa", "T002_002|bc"),
		"added":     ManifestDigest("T002_001|aa", "T002_002|bb", "T002_003|cc"),
		"joined":    ManifestDigest("T002_001|aaT002_002|bb"),
	} {
		if other == a {
			t.Errorf("%s manifest has the same digest", name)
		}
	}
}

func TestNewTemplateNamesAreBoundedAndValidated(t *testing.T) {
	const admin = "postgres://postgres:secret@127.0.0.1:5432/postgres"
	tpl, err := newTemplate(admin, ManifestDigest("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{tpl.name, tpl.migRole, tpl.rtRole} {
		if !strings.HasPrefix(name, TemplatePrefix) || len(name) > 63 {
			t.Errorf("name %q must start with %q and fit an identifier", name, TemplatePrefix)
		}
	}
	if tpl.migRole == tpl.rtRole || tpl.migPassword == tpl.rtPassword || strings.Contains(tpl.migPassword, "secret") {
		t.Error("roles must be distinct with derived passwords")
	}
	again, _ := newTemplate(admin, ManifestDigest("x"))
	if again != tpl {
		t.Error("the same digest must resolve to the same template in every process")
	}
	if other, _ := newTemplate(admin, ManifestDigest("y")); other.name == tpl.name {
		t.Error("different digests share a template")
	}
	if prefixed, err := newTemplate(admin, "sha256:"+ManifestDigest("x")); err != nil || prefixed.name != tpl.name {
		t.Errorf("sha256: prefix: %v", err)
	}
	for _, bad := range []string{"", "short", "ABCDEF0123456789ABCDEF", "x'; DROP DATABASE postgres; --", strings.Repeat("a", 65)} {
		if _, err := newTemplate(admin, bad); err == nil {
			t.Errorf("digest %q accepted", bad)
		}
	}
}
