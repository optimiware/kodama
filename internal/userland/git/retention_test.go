package git_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	git "github.com/optimiware/kodama/internal/userland/git"
)

// TestRetentionUsesAuthoritativeQuery pins retained-reference discovery: the
// service pages the authorized retention query to exhaustion, deduplicates in
// first-seen order, and treats event hints as advisory only.
func TestRetentionUsesAuthoritativeQuery(t *testing.T) {
	f := newFixture(t)
	f.retention.pages = []git.CommitRefs{
		{Commits: []string{"c1", "c2"}, Next: api.PageInfo{NextCursor: "page-2"}},
		{Commits: []string{"c2", "c3"}, Next: api.PageInfo{}},
	}

	plan, err := f.service.Retention(context.Background(), "shop/web", []string{"c9", "c1"})
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if plan.Repository != "shop/web" {
		t.Fatalf("plan repository = %q", plan.Repository)
	}
	if !reflect.DeepEqual(plan.Commits, []string{"c1", "c2", "c3"}) {
		t.Fatalf("commits = %v; want deduplicated first-seen order", plan.Commits)
	}
	if plan.Pages != 2 {
		t.Fatalf("pages = %d; want 2", plan.Pages)
	}
	for _, commit := range plan.Commits {
		if commit == "c9" {
			t.Fatal("an event hint became a retained commit without the authoritative query")
		}
	}

	requests := f.retention.pageRequests()
	if len(requests) != 2 {
		t.Fatalf("retention requests = %d; want 2", len(requests))
	}
	if requests[0].Cursor != "" {
		t.Fatalf("first cursor = %q; want empty", requests[0].Cursor)
	}
	if requests[1].Cursor != "page-2" {
		t.Fatalf("second cursor = %q; want page-2", requests[1].Cursor)
	}
}

// TestRetentionRefusals pins fail-closed discovery: an unconfigured or invalid
// repository refuses before the query, a query failure is not replaced by event
// hints, and a repeated cursor cannot loop forever.
func TestRetentionRefusals(t *testing.T) {
	f := newFixture(t)

	if _, err := f.service.Retention(context.Background(), "shop/missing", nil); !errors.Is(err, git.ErrRepositoryUnknown) {
		t.Fatalf("unknown repository error = %v; want ErrRepositoryUnknown", err)
	}
	if _, err := f.service.Retention(context.Background(), "../escape", nil); !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("escaped repository error = %v; want ErrInvalidRepository", err)
	}
	if calls := f.retention.callCount(); calls != 0 {
		t.Fatalf("refused retention queried %d times", calls)
	}

	f.retention.mu.Lock()
	f.retention.err = errors.New("retention query unavailable")
	f.retention.mu.Unlock()
	if _, err := f.service.Retention(context.Background(), "shop/web", []string{"c1"}); err == nil {
		t.Fatal("Retention ignored a query failure and trusted hints")
	}

	looping := newFixture(t)
	looping.retention.pages = []git.CommitRefs{
		{Commits: []string{"c1"}, Next: api.PageInfo{NextCursor: "same"}},
		{Commits: []string{"c2"}, Next: api.PageInfo{NextCursor: "same"}},
	}
	if _, err := looping.service.Retention(context.Background(), "shop/web", nil); err == nil {
		t.Fatal("Retention followed a repeated cursor forever")
	}
	if calls := looping.retention.callCount(); calls > 2 {
		t.Fatalf("repeated cursor queried %d times; want a bounded refusal", calls)
	}
}

// TestBackupManifestIsTenantScoped pins the backup plan: the manifest names the
// tenant-scoped path, the sorted observed refs and the retained commits, and it
// carries no credential material.
func TestBackupManifestIsTenantScoped(t *testing.T) {
	f := newFixture(t)
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setRef("shop/web", "refs/heads/work/x", "work-1")
	f.retention.pages = []git.CommitRefs{
		{Commits: []string{"work-1", testBase}, Next: api.PageInfo{}},
	}

	manifest, err := f.service.BackupManifest(context.Background(), "shop/web")
	if err != nil {
		t.Fatalf("BackupManifest: %v", err)
	}
	if manifest.TenantID != testTenant || manifest.Repository != "shop/web" {
		t.Fatalf("manifest identity = %q/%q", manifest.TenantID, manifest.Repository)
	}
	if manifest.Path != f.repositoryPath("shop/web") {
		t.Fatalf("manifest path = %q; want %q", manifest.Path, f.repositoryPath("shop/web"))
	}
	want := []git.RefState{
		{Ref: testRef, Commit: testBase},
		{Ref: "refs/heads/work/x", Commit: "work-1"},
	}
	if !reflect.DeepEqual(manifest.Refs, want) {
		t.Fatalf("manifest refs = %+v; want %+v", manifest.Refs, want)
	}
	if !reflect.DeepEqual(manifest.Retained, []string{"work-1", testBase}) {
		t.Fatalf("manifest retained = %v", manifest.Retained)
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	for _, forbidden := range []string{"credential", "password", "secret", "token", testCredential} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("manifest contains %q: %s", forbidden, encoded)
		}
	}

	// An unconfigured repository is refused before any repository command.
	before := len(f.executor.commandsSnapshot())
	if _, err := f.service.BackupManifest(context.Background(), "shop/missing"); !errors.Is(err, git.ErrRepositoryUnknown) {
		t.Fatalf("unknown repository error = %v; want ErrRepositoryUnknown", err)
	}
	if after := len(f.executor.commandsSnapshot()); after != before {
		t.Fatalf("unknown repository ran %d commands", after-before)
	}
}
