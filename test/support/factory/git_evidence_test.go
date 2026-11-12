package factory

// Positive control for the read-only credential-leak scanner. Every production
// assertion in this suite expects an empty result, so a scanner that silently
// stopped walking (or that skipped the composition log) would make those
// checks vacuous. This test plants one credential in each scanned surface and
// requires the scanner to find it, with a clean-control needle that must not
// match.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCredentialLeaksFindsEveryPlantedSurface(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "repositories")
	journal := filepath.Join(root, "journal")
	for _, directory := range []string{filepath.Join(storage, "shop", "web"), journal} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}

	const planted = "kodsvc.test.real-credential-value"
	if err := os.WriteFile(filepath.Join(storage, "shop", "web", "config"), []byte("remote="+planted+"\n"), 0o600); err != nil {
		t.Fatalf("write storage credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(journal, "entry.json"), []byte(`{"credential":"`+planted+`"}`), 0o600); err != nil {
		t.Fatalf("write journal credential: %v", err)
	}

	fixture := &GitFixture{
		StorageRoot:      storage,
		JournalDirectory: journal,
		h:                &Harness{logs: &safeBuffer{}},
	}
	if _, err := fixture.h.logs.Write([]byte("composition started with " + planted)); err != nil {
		t.Fatalf("write composition log: %v", err)
	}

	if hits := fixture.ScanCredentialLeaks(t); len(hits) != 0 {
		t.Fatalf("an empty needle set reported leaks: %v", hits)
	}
	if hits := fixture.ScanCredentialLeaks(t, "not-planted-anywhere"); len(hits) != 0 {
		t.Fatalf("the scanner reported a clean control as a leak: %v", hits)
	}

	hits := fixture.ScanCredentialLeaks(t, planted)
	found := map[string]bool{}
	for _, hit := range hits {
		found[hit] = true
	}
	for _, want := range []string{
		filepath.Join(storage, "shop", "web", "config"),
		filepath.Join(journal, "entry.json"),
		"composition-log",
	} {
		if !found[want] {
			t.Fatalf("the scanner missed the planted credential in %q; hits = %v", want, hits)
		}
	}
}
