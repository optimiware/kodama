package pgtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// TestEvidenceRootsAreRunUnique proves KODAMA_EVIDENCE_DIR is only a parent:
// every run resolves a fresh, existing child directory, and two runs that share
// one parent never write into the same files. It also proves the resolved run
// directory (not the supplied parent) is what gets published to the harness.
func TestEvidenceRootsAreRunUnique(t *testing.T) {
	parent := t.TempDir()
	getenv := func(key string) string {
		if key == EvidenceDirEnv {
			return parent
		}
		return ""
	}
	published := map[string]string{}
	first := &evidenceStore{
		getenv: getenv,
		setenv: func(key, value string) error {
			published[key] = value
			return nil
		},
	}
	second := &evidenceStore{getenv: getenv}

	firstDir := first.runDir()
	secondDir := second.runDir()

	if firstDir == secondDir {
		t.Fatalf("two runs shared evidence directory %q; want run-unique children of %q", firstDir, parent)
	}
	for _, dir := range []string{firstDir, secondDir} {
		if dir == parent {
			t.Fatalf("run directory %q reused the supplied parent itself", dir)
		}
		if filepath.Dir(dir) != parent {
			t.Fatalf("run directory %q is not a direct child of parent %q", dir, parent)
		}
		if !strings.HasPrefix(filepath.Base(dir), "run-") {
			t.Fatalf("run directory %q lacks a run-unique name", dir)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("run directory %q was not created: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("run path %q is not a directory", dir)
		}
	}

	// One run is stable: callers of the same run must land in the same place.
	if again := first.runDir(); again != firstDir {
		t.Fatalf("run directory changed within one run: %q then %q", firstDir, again)
	}
	if got := first.parentDir(); got != parent {
		t.Fatalf("parent after publish = %q; want the supplied parent %q", got, parent)
	}

	if got := published[EvidenceDirEnv]; got != firstDir {
		t.Fatalf("published %s = %q; want the actual run directory %q", EvidenceDirEnv, got, firstDir)
	}

	if got := evidenceParent(func(string) string { return "" }); !filepath.IsAbs(got) || !strings.HasSuffix(got, filepath.Join(".harness", "evidence", "I02b")) {
		t.Fatalf("fallback parent = %q; want module-rooted .harness/evidence/I02b path", got)
	}
	if got := evidenceParent(getenv); got != parent {
		t.Fatalf("supplied parent = %q; want %q", got, parent)
	}
}

// TestServerEvidencePreservesAllShards proves each shard's server identity
// survives in both the evidence file name and its content. A fixed
// postgres-server.json lets the last shard overwrite every earlier one.
func TestServerEvidencePreservesAllShards(t *testing.T) {
	dir := t.TempDir()
	shards := []struct {
		id      string
		version string
	}{
		{"postgres-testfirst-1111-aaaa0001", "18.0"},
		{"postgres-testsecond-1111-bbbb0002", "18.1"},
		{"postgres-testthird-1111-cccc0003", "18.2"},
	}
	started := time.Now()
	for _, shard := range shards {
		subject := &Shard{
			Harness:       &harness.Shard{ID: shard.id},
			ImageTag:      "docker.io/library/postgres:18-alpine",
			ImageDigest:   "sha256:fixture-" + shard.version,
			ServerVersion: shard.version,
			Database:      "kodama",
		}
		if err := subject.writeServerEvidence(dir); err != nil {
			t.Fatalf("write evidence for shard %q: %v", shard.id, err)
		}
	}
	finished := time.Now()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence dir: %v", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		files = append(files, entry.Name())
	}
	if len(files) != len(shards) {
		t.Fatalf("evidence files = %v; want one file per shard (%d)", files, len(shards))
	}

	names := map[string]string{}
	for _, shard := range shards {
		name := serverEvidenceName(shard.id)
		if name == "postgres-server.json" {
			t.Fatalf("shard %q uses the fixed evidence file name", shard.id)
		}
		if other, ok := names[name]; ok {
			t.Fatalf("shards %q and %q share evidence file %q", other, shard.id, name)
		}
		names[name] = shard.id
		identity := shard.id[len(shard.id)-8:]
		if !strings.Contains(name, identity) {
			t.Fatalf("evidence file %q does not carry shard identity suffix %q", name, identity)
		}
		payload, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read evidence for shard %q: %v", shard.id, err)
		}
		var got serverEvidence
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("decode evidence for shard %q: %v", shard.id, err)
		}
		if got.Shard != shard.id {
			t.Fatalf("evidence for shard %q records shard %q", shard.id, got.Shard)
		}
		if got.ServerVersion != shard.version {
			t.Fatalf("evidence for shard %q records version %q; want %q", shard.id, got.ServerVersion, shard.version)
		}
		if got.Image != "docker.io/library/postgres:18-alpine" {
			t.Fatalf("evidence for shard %q records image %q", shard.id, got.Image)
		}
		if got.Digest != "sha256:fixture-"+shard.version {
			t.Fatalf("evidence for shard %q records digest %q", shard.id, got.Digest)
		}
		if got.Database != "kodama" {
			t.Fatalf("evidence for shard %q records database %q", shard.id, got.Database)
		}
		if got.RecordedAt.Before(started.UTC()) || got.RecordedAt.After(finished.UTC()) {
			t.Fatalf("evidence for shard %q records time %s outside [%s, %s]", shard.id, got.RecordedAt, started, finished)
		}
	}
}

// TestWriteEvidenceConcurrentWriters proves atomic evidence writes stage each
// document in its own file. A shared "<path>.tmp" staging file lets one writer
// rename the file another writer is still filling: the loser fails with a
// missing staging file, or the winner publishes bytes it did not write. Every
// writer here starts from one barrier, so the fixed-staging-file form is red.
func TestWriteEvidenceConcurrentWriters(t *testing.T) {
	const writers = 48

	dir := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			record := map[string]any{
				"writer":  index,
				"payload": strings.Repeat(fmt.Sprintf("%03d-", index), 512),
			}
			errs <- WriteEvidence(dir, "shared.json", record)
		}(index)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent evidence write failed: %v", err)
		}
	}

	payload, err := os.ReadFile(filepath.Join(dir, "shared.json"))
	if err != nil {
		t.Fatalf("read published evidence: %v", err)
	}
	var published struct {
		Writer  int    `json:"writer"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(payload, &published); err != nil {
		t.Fatalf("decode published evidence: %v", err)
	}
	if published.Writer < 0 || published.Writer >= writers {
		t.Fatalf("published writer = %d; want one of 0..%d", published.Writer, writers-1)
	}
	want := strings.Repeat(fmt.Sprintf("%03d-", published.Writer), 512)
	if published.Payload != want {
		t.Fatalf("published evidence interleaved writer %d content: got %d bytes, want %d", published.Writer, len(published.Payload), len(want))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("staging file %q survived a published write", entry.Name())
		}
	}
}
