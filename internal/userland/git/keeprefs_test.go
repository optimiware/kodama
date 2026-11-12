package git_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	git "github.com/optimiware/kodama/internal/userland/git"
)

var (
	keepA      = strings.Repeat("a", 40)
	keepB      = strings.Repeat("b", 40)
	keepC      = strings.Repeat("c", 40)
	keepAbsent = strings.Repeat("d", 40)
)

// TestKeepRefsCreatesRefreshesAndReleases pins the keep-ref sweep: a retained
// commit gets a ref under refs/kodama/keep/<sha>, a missing commit is skipped,
// a ref no longer named by the retained set is released, and a repeated sweep
// with the same set writes nothing.
func TestKeepRefsCreatesRefreshesAndReleases(t *testing.T) {
	f := newFixture(t)
	f.executor.setObject("shop/web", keepA, true)
	f.executor.setObject("shop/web", keepB, true)
	f.executor.setObject("shop/web", keepC, true)
	// A stale managed ref from an earlier sweep.
	f.executor.setRef("shop/web", git.KeepRefPrefix+keepC, keepC)

	result, err := f.service.KeepRefs(context.Background(), "shop/web", []string{keepA, keepAbsent})
	if err != nil {
		t.Fatalf("KeepRefs: %v", err)
	}
	if !reflect.DeepEqual(result.Kept, []string{keepA}) {
		t.Fatalf("kept = %v; want [%s]", result.Kept, keepA)
	}
	if !reflect.DeepEqual(result.Missing, []string{keepAbsent}) {
		t.Fatalf("missing = %v; want [%s]", result.Missing, keepAbsent)
	}
	if !reflect.DeepEqual(result.Released, []string{keepC}) {
		t.Fatalf("released = %v; want [%s]", result.Released, keepC)
	}
	if got := f.executor.commit("shop/web", git.KeepRefPrefix+keepA); got != keepA {
		t.Fatalf("keep ref = %q; want %q", got, keepA)
	}
	if got := f.executor.commit("shop/web", git.KeepRefPrefix+keepC); got != "" {
		t.Fatalf("stale keep ref = %q; want it released", got)
	}
	if got := f.executor.commit("shop/web", git.KeepRefPrefix+keepAbsent); got != "" {
		t.Fatalf("missing commit got keep ref %q", got)
	}

	// The second identical sweep is idempotent: no ref is written again.
	before := f.executor.updateRefCount()
	result, err = f.service.KeepRefs(context.Background(), "shop/web", []string{keepA})
	if err != nil {
		t.Fatalf("second KeepRefs: %v", err)
	}
	if got := f.executor.updateRefCount(); got != before {
		t.Fatalf("second sweep ran %d update-ref commands; want 0", got-before)
	}
	if !reflect.DeepEqual(result.Kept, []string{keepA}) || len(result.Released) != 0 {
		t.Fatalf("second sweep result = %+v; want only %s kept", result, keepA)
	}

	// Refreshing an existing ref that points elsewhere corrects it.
	f.executor.setRef("shop/web", git.KeepRefPrefix+keepB, keepC)
	if _, err := f.service.KeepRefs(context.Background(), "shop/web", []string{keepB}); err != nil {
		t.Fatalf("refresh KeepRefs: %v", err)
	}
	if got := f.executor.commit("shop/web", git.KeepRefPrefix+keepB); got != keepB {
		t.Fatalf("refreshed keep ref = %q; want %q", got, keepB)
	}
}

// TestKeepRefsRefusals pins the safety boundaries: an invalid name is reported
// and never becomes a ref, an unknown repository refuses before any native
// command, and a native uncertainty aborts before any release.
func TestKeepRefsRefusals(t *testing.T) {
	f := newFixture(t)
	f.executor.setObject("shop/web", keepA, true)

	result, err := f.service.KeepRefs(context.Background(), "shop/web", []string{keepA, "not-a-sha", ""})
	if err != nil {
		t.Fatalf("KeepRefs: %v", err)
	}
	if !reflect.DeepEqual(result.Invalid, []string{"not-a-sha"}) {
		t.Fatalf("invalid = %v; want [not-a-sha]", result.Invalid)
	}
	if !reflect.DeepEqual(result.Kept, []string{keepA}) {
		t.Fatalf("kept = %v; want [%s]", result.Kept, keepA)
	}

	before := len(f.executor.commandsSnapshot())
	if _, err := f.service.KeepRefs(context.Background(), "shop/missing", []string{keepA}); !errors.Is(err, git.ErrRepositoryUnknown) {
		t.Fatalf("unknown repository error = %v; want ErrRepositoryUnknown", err)
	}
	if after := len(f.executor.commandsSnapshot()); after != before {
		t.Fatalf("unknown repository ran %d commands", after-before)
	}

	f.executor.setRef("shop/web", git.KeepRefPrefix+keepC, keepC)
	f.executor.failAfter("rev-parse", errors.New("rev-parse unavailable"))
	if _, err := f.service.KeepRefs(context.Background(), "shop/web", []string{keepA}); err == nil {
		t.Fatal("KeepRefs ignored a rev-parse failure and released refs")
	}
	if got := f.executor.commit("shop/web", git.KeepRefPrefix+keepC); got != keepC {
		t.Fatalf("uncertain sweep released keep ref %q", got)
	}
}
