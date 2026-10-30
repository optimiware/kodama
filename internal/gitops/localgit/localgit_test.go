package localgit_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
	"github.com/optimiware/kodama/internal/gitops/localgit"
)

const tenant domain.TenantID = "t138_local"

// Test_unit_gitops_localgit: ancestry, commit time and bundle (read-only) against
// real repositories; malformed identifiers never reach Git.
func Test_unit_gitops_localgit(t *testing.T) {
	ctx := context.Background()
	vol := gitopstest.NewVolume(t)
	o := localgit.Objects{Locator: vol}
	at := time.Date(2025, 5, 4, 3, 2, 1, 0, time.UTC)
	base := vol.Commit(tenant, "r_a", "", "base", at)
	next := vol.Commit(tenant, "r_a", base, "next", at.Add(time.Hour))
	side := vol.Commit(tenant, "r_a", base, "side", at.Add(2*time.Hour))
	vol.Init(tenant, "r_empty")

	for _, tc := range []struct {
		a, d string
		want bool
	}{{base, next, true}, {next, base, false}, {next, next, true}, {side, next, false}} {
		got, err := o.IsAncestor(ctx, tenant, "r_a", tc.a, tc.d)
		if err != nil || got != tc.want {
			t.Errorf("IsAncestor(%.7s, %.7s) = %v, %v; want %v", tc.a, tc.d, got, err, tc.want)
		}
	}
	if _, err := o.IsAncestor(ctx, tenant, "r_a", strings.Repeat("f", 40), next); err == nil {
		t.Fatal("a commit the repository lacks is an error, not false")
	}
	if ok, err := o.HasCommit(ctx, tenant, "r_a", strings.Repeat("f", 40)); err != nil || ok {
		t.Fatalf("HasCommit of a missing commit: %v %v", ok, err)
	}
	if got, err := o.CommitTime(ctx, tenant, "r_a", next); err != nil || !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("CommitTime = %v, %v", got, err)
	}
	for _, bad := range []string{"--upload-pack=x", "HEAD", "main", strings.Repeat("0", 40), ""} {
		if _, err := o.CommitTime(ctx, tenant, "r_a", bad); err == nil {
			t.Errorf("CommitTime accepted %q", bad)
		}
	}
	if _, err := o.HasCommit(ctx, tenant, "../escape", next); err == nil {
		t.Fatal("a repository ID that is not an ID")
	}
	if _, err := o.CreateBundle(ctx, tenant, "r_empty", &bytes.Buffer{}); !errors.Is(err, gitops.ErrEmptyRepository) {
		t.Fatalf("an empty repository: %v", err)
	}

	vol.SetRef(tenant, "r_a", "refs/heads/main", next)
	vol.SetRef(tenant, "r_a", "refs/heads/kodama/work/wi_1", side)
	o.JournalHead = func(domain.TenantID, string) int64 { return 42 }
	var bundle bytes.Buffer
	info, err := o.CreateBundle(ctx, tenant, "r_a", &bundle)
	if err != nil || len(info.Refs) != 2 || info.Refs[0].Ref != "refs/heads/kodama/work/wi_1" || info.Refs[1].Commit != next || info.JournalHead != 42 {
		t.Fatalf("bundle %+v %v", info, err)
	}
	// The bundle is a real bundle, and building it changed nothing.
	dir := t.TempDir()
	gitopstest.Git(t, dir, nil, "init", "--quiet")
	file := filepath.Join(dir, "r.bundle")
	if err := os.WriteFile(file, bundle.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	gitopstest.Git(t, dir, nil, "bundle", "verify", file)
	if got := vol.Refs(tenant, "r_a"); len(got) != 2 {
		t.Fatalf("refs after a bundle: %v", got)
	}
}
