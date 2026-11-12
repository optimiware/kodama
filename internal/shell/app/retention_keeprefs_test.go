package app

// Keep-ref composition tests. They prove the retention loop reads the retained
// commit names from the retention store, aggregates a whole tenant before
// reconciling, keeps refs on any read uncertainty and releases only the commits
// no retained execution names.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
)

type keepRefsTestSweeper struct{}

func (keepRefsTestSweeper) Sweep(context.Context, []installation.RetentionScope, time.Time) (installation.RetentionSweepResult, error) {
	return installation.RetentionSweepResult{}, nil
}

type keepRefsTestCommits struct {
	mu      sync.Mutex
	byScope map[string][]string
	errs    map[string]error
	calls   []string
}

func (reader *keepRefsTestCommits) RetainedCommits(_ context.Context, scope installation.RetentionScope, _ time.Time) ([]string, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	key := scope.TenantID + "/" + scope.ProjectID
	reader.calls = append(reader.calls, key)
	if err := reader.errs[key]; err != nil {
		return nil, err
	}
	return append([]string(nil), reader.byScope[key]...), nil
}

type keepRefsTestKeeper struct {
	mu      sync.Mutex
	byScope map[string][]string
	calls   int
}

func (keeper *keepRefsTestKeeper) KeepRefs(_ context.Context, tenant string, retained []string) error {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	keeper.calls++
	if keeper.byScope == nil {
		keeper.byScope = make(map[string][]string)
	}
	keeper.byScope[tenant] = append([]string(nil), retained...)
	return nil
}

// TestRetentionLoopRefusesKeepRefsWithoutRetainedCommits pins the fail-loud
// guarantee: wiring a keeper without the authoritative commit source is a
// construction refusal, never a silent no-op that leaves every ref unprotected
// or releases everything.
func TestRetentionLoopRefusesKeepRefsWithoutRetainedCommits(t *testing.T) {
	_, err := newRetentionLoop(retentionLoopConfig{
		Sweeper:  keepRefsTestSweeper{},
		Scopes:   retentionTestScopes{},
		KeepRefs: &keepRefsTestKeeper{},
		Clock:    &retentionImmediateClock{now: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)},
		Interval: time.Hour,
		Logger:   retentionTestLogger(),
	})
	if !errors.Is(err, ErrKeepRefsWithoutRetainedCommits) {
		t.Fatalf("newRetentionLoop error = %v; want ErrKeepRefsWithoutRetainedCommits", err)
	}
}

// TestConfigKeepRefsWiring proves a storage root alone enables enumeration,
// while a repository list without a root is a named refusal.
func TestConfigKeepRefsWiring(t *testing.T) {
	config := validProductionConfig(t)
	config.GitStorageRoot = t.TempDir()
	if err := config.resolved().validate(); err != nil {
		t.Fatalf("validate refused a storage-root-only configuration: %v", err)
	}

	config.GitStorageRoot = ""
	config.GitRepositories = []string{"shop/web"}
	err := config.resolved().validate()
	if !errors.Is(err, ErrKeepRefsConfiguration) {
		t.Fatalf("validate error = %v; want ErrKeepRefsConfiguration", err)
	}
}

func TestRetentionLoopKeepsTenantWideRetainedCommits(t *testing.T) {
	clock := &retentionImmediateClock{now: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)}
	commits := &keepRefsTestCommits{byScope: map[string][]string{
		"tenant-1/project-1": {strings.Repeat("a", 40)},
		"tenant-1/project-2": {strings.Repeat("b", 40), strings.Repeat("a", 40)},
		"tenant-2/project-1": {strings.Repeat("c", 40)},
	}}
	keeper := &keepRefsTestKeeper{}
	loop, err := newRetentionLoop(retentionLoopConfig{
		Sweeper: keepRefsTestSweeper{},
		Scopes: retentionTestScopes{values: []installation.RetentionScope{
			{TenantID: "tenant-1", ProjectID: "project-1"},
			{TenantID: "tenant-1", ProjectID: "project-2"},
			{TenantID: "tenant-1"},
			{TenantID: "tenant-2", ProjectID: "project-1"},
		}},
		Commits:  commits,
		KeepRefs: keeper,
		Clock:    clock,
		Interval: time.Hour,
		Logger:   retentionTestLogger(),
	})
	if err != nil {
		t.Fatalf("newRetentionLoop: %v", err)
	}
	if err := loop.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	want := map[string][]string{
		"tenant-1": {strings.Repeat("a", 40), strings.Repeat("b", 40)},
		"tenant-2": {strings.Repeat("c", 40)},
	}
	if !reflect.DeepEqual(keeper.byScope, want) {
		t.Fatalf("kept = %v; want the tenant-wide aggregate %v", keeper.byScope, want)
	}
	if keeper.calls != 2 {
		t.Fatalf("keeper calls = %d; want one per tenant", keeper.calls)
	}

	// The tenant-wide scope is not evidence-bearing and is never queried.
	for _, call := range commits.calls {
		if strings.HasSuffix(call, "/") {
			t.Fatalf("queried the tenant-wide scope %q", call)
		}
	}

	// The next sweep with no retained evidence releases the whole tenant set.
	commits.mu.Lock()
	commits.byScope = map[string][]string{}
	commits.mu.Unlock()
	if err := loop.sweep(context.Background()); err != nil {
		t.Fatalf("release sweep: %v", err)
	}
	if !reflect.DeepEqual(keeper.byScope, map[string][]string{
		"tenant-1": nil,
		"tenant-2": nil,
	}) {
		t.Fatalf("released = %v; want empty tenant sets", keeper.byScope)
	}
}

func TestRetentionLoopKeepsRefsWhenTheSourceIsUncertain(t *testing.T) {
	clock := &retentionImmediateClock{now: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)}
	commits := &keepRefsTestCommits{
		byScope: map[string][]string{
			"tenant-2/project-1": {strings.Repeat("c", 40)},
		},
		errs: map[string]error{
			"tenant-1/project-1": errors.New("retention store unavailable"),
		},
	}
	keeper := &keepRefsTestKeeper{}
	loop, err := newRetentionLoop(retentionLoopConfig{
		Sweeper: keepRefsTestSweeper{},
		Scopes: retentionTestScopes{values: []installation.RetentionScope{
			{TenantID: "tenant-1", ProjectID: "project-1"},
			{TenantID: "tenant-1", ProjectID: "project-2"},
			{TenantID: "tenant-2", ProjectID: "project-1"},
		}},
		Commits:  commits,
		KeepRefs: keeper,
		Clock:    clock,
		Interval: time.Hour,
		Logger:   retentionTestLogger(),
	})
	if err != nil {
		t.Fatalf("newRetentionLoop: %v", err)
	}
	err = loop.sweep(context.Background())
	if err == nil {
		t.Fatal("sweep ignored an unavailable retention source")
	}
	if _, ok := keeper.byScope["tenant-1"]; ok {
		t.Fatal("an incomplete tenant read released or replaced tenant-1 keep refs")
	}
	if got := keeper.byScope["tenant-2"]; !reflect.DeepEqual(got, []string{strings.Repeat("c", 40)}) {
		t.Fatalf("tenant-2 kept = %v; want the unaffected tenant still reconciled", got)
	}
}
