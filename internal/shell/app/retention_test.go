package app

// Retention composition tests. They prove the loop sweeps at startup and
// on the interval, reconstructs tenant/project scope from the controller
// execution read, and never invents a scope from an incomplete row.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type retentionTestSweeper struct {
	mu      sync.Mutex
	calls   int
	scopes  []installation.RetentionScope
	at      []time.Time
	results []installation.RetentionSweepResult
	errs    []error
	wake    chan struct{}
}

func (sweeper *retentionTestSweeper) Sweep(_ context.Context, scopes []installation.RetentionScope, now time.Time) (installation.RetentionSweepResult, error) {
	sweeper.mu.Lock()
	sweeper.calls++
	sweeper.scopes = append([]installation.RetentionScope(nil), scopes...)
	sweeper.at = append(sweeper.at, now)
	index := sweeper.calls - 1
	var result installation.RetentionSweepResult
	var err error
	if index < len(sweeper.results) {
		result = sweeper.results[index]
	}
	if index < len(sweeper.errs) {
		err = sweeper.errs[index]
	}
	sweeper.mu.Unlock()
	if sweeper.wake != nil {
		select {
		case sweeper.wake <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (sweeper *retentionTestSweeper) callCount() int {
	sweeper.mu.Lock()
	defer sweeper.mu.Unlock()
	return sweeper.calls
}

type retentionTestScopes struct {
	values []installation.RetentionScope
	err    error
}

func (reader retentionTestScopes) scopes(context.Context) ([]installation.RetentionScope, error) {
	return append([]installation.RetentionScope(nil), reader.values...), reader.err
}

type retentionImmediateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *retentionImmediateClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *retentionImmediateClock) Sleep(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}

func TestRetentionLoopSweepsAtStartupAndOnTheInterval(t *testing.T) {
	clock := &retentionImmediateClock{now: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)}
	sweeper := &retentionTestSweeper{wake: make(chan struct{}, 8)}
	loop, err := newRetentionLoop(retentionLoopConfig{
		Sweeper: sweeper,
		Scopes: retentionTestScopes{values: []installation.RetentionScope{
			{TenantID: "tenant-1", ProjectID: "project-1"},
		}},
		Clock:    clock,
		Interval: time.Hour,
		Logger:   retentionTestLogger(),
	})
	if err != nil {
		t.Fatalf("newRetentionLoop: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()
	for sweeper.callCount() < 2 {
		select {
		case <-sweeper.wake:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("retention loop made %d sweeps; want at least 2 (startup and interval)", sweeper.callCount())
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("retention loop returned nil after cancellation")
	}
	sweeper.mu.Lock()
	defer sweeper.mu.Unlock()
	if len(sweeper.scopes) == 0 || sweeper.scopes[0].TenantID != "tenant-1" {
		t.Errorf("swept scopes = %+v; want the configured scope", sweeper.scopes)
	}
	for index, at := range sweeper.at {
		if !at.Equal(clock.now) {
			t.Errorf("sweep %d time = %v; want %v", index, at, clock.now)
		}
	}
}

func TestRetentionLoopSurvivesASweepFailure(t *testing.T) {
	clock := &retentionImmediateClock{now: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)}
	sweeper := &retentionTestSweeper{
		wake: make(chan struct{}, 8),
		errs: []error{context.DeadlineExceeded, nil},
	}
	loop, err := newRetentionLoop(retentionLoopConfig{
		Sweeper:  sweeper,
		Scopes:   retentionTestScopes{values: []installation.RetentionScope{{TenantID: "tenant-1"}}},
		Clock:    clock,
		Interval: time.Hour,
		Logger:   retentionTestLogger(),
	})
	if err != nil {
		t.Fatalf("newRetentionLoop: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()
	for sweeper.callCount() < 2 {
		select {
		case <-sweeper.wake:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("retention loop stopped after a sweep failure; calls=%d", sweeper.callCount())
		}
	}
	cancel()
	<-done
}

type retentionPageController struct {
	mu     sync.Mutex
	pages  []store.QueryPage
	cursor []string
}

func (controller *retentionPageController) Read(context.Context, string, string) (store.Row, error) {
	return store.Row{}, nil
}

func (controller *retentionPageController) Scan(_ context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.cursor = append(controller.cursor, scan.After)
	if len(controller.pages) == 0 {
		return store.QueryPage{}, nil
	}
	page := controller.pages[0]
	controller.pages = controller.pages[1:]
	return page, nil
}

func (controller *retentionPageController) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, nil
}

func TestControllerRetentionScopesUsesEveryExecutionPage(t *testing.T) {
	controller := &retentionPageController{pages: []store.QueryPage{
		{
			Rows: []store.Row{
				{Table: "executions", Values: map[string]any{"tenant_id": "tenant-1", "project_id": "project-1"}},
				{Table: "executions", Values: map[string]any{"tenant_id": "tenant-1", "project_id": "project-2"}},
			},
			Next:    "cursor-1",
			HasMore: true,
		},
		{
			Rows: []store.Row{
				{Table: "executions", Values: map[string]any{"tenant_id": "tenant-2", "project_id": "project-3"}},
			},
		},
	}}
	scopes, err := (controllerRetentionScopes{controller: controller}).scopes(context.Background())
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	want := []installation.RetentionScope{
		{TenantID: "tenant-1"},
		{TenantID: "tenant-1", ProjectID: "project-1"},
		{TenantID: "tenant-1", ProjectID: "project-2"},
		{TenantID: "tenant-2"},
		{TenantID: "tenant-2", ProjectID: "project-3"},
	}
	if len(scopes) != len(want) {
		t.Fatalf("scopes = %+v; want %+v", scopes, want)
	}
	for index := range want {
		if scopes[index] != want[index] {
			t.Errorf("scope[%d] = %+v; want %+v", index, scopes[index], want[index])
		}
	}
	if len(controller.cursor) != 2 || controller.cursor[0] != "" || controller.cursor[1] != "cursor-1" {
		t.Errorf("scan cursors = %v; want [\"\" \"cursor-1\"]", controller.cursor)
	}
}

func TestControllerRetentionScopesSkipsRowsWithoutATenant(t *testing.T) {
	controller := &retentionPageController{pages: []store.QueryPage{{
		Rows: []store.Row{
			{Table: "executions", Values: map[string]any{"project_id": "project-1"}},
			{Table: "executions", Values: map[string]any{"tenant_id": "tenant-1"}},
		},
	}}}
	scopes, err := (controllerRetentionScopes{controller: controller}).scopes(context.Background())
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != (installation.RetentionScope{TenantID: "tenant-1"}) {
		t.Fatalf("scopes = %+v; want only the tenant-wide scope of the complete row", scopes)
	}
}

func TestControllerRetentionScopesIncludesConfiguredTenants(t *testing.T) {
	controller := &retentionPageController{}
	scopes, err := (controllerRetentionScopes{
		controller: controller,
		tenants:    []id.TenantID{"tenant-9"},
	}).scopes(context.Background())
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != (installation.RetentionScope{TenantID: "tenant-9"}) {
		t.Fatalf("scopes = %+v; want the configured tenant-wide scope", scopes)
	}
}

func retentionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordingRetentionStore struct {
	mu         sync.Mutex
	classes    []installation.RetentionClass
	candidates map[installation.RetentionClass][]installation.RetentionCandidate
	deleted    []string
}

func (store *recordingRetentionStore) Candidates(_ context.Context, _ installation.RetentionScope, class installation.RetentionClass, _ time.Time) ([]installation.RetentionCandidate, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.classes = append(store.classes, class)
	return append([]installation.RetentionCandidate(nil), store.candidates[class]...), nil
}

func (store *recordingRetentionStore) Delete(_ context.Context, _ installation.RetentionScope, _ installation.RetentionClass, candidate installation.RetentionCandidate, _ time.Time) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.deleted = append(store.deleted, candidate.ID)
	return true, nil
}

func TestRetentionRawContentModelPlumbingAndProjectScopes(t *testing.T) {
	if hasRecordedModelContent(nil) {
		t.Fatal("nil ModelsConfig reported recorded model content")
	}
	scopeA := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-a"}
	scopeB := tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-b"}
	disabled := &ModelsConfig{
		Models: []gateway.ModelConfig{
			{Scope: scopeA, Model: "m1", RecordContent: false},
		},
	}
	if hasRecordedModelContent(disabled) {
		t.Fatal("ModelsConfig with RecordContent=false reported recorded model content")
	}
	enabled := &ModelsConfig{
		Models: []gateway.ModelConfig{
			{Scope: scopeA, Model: "m1", RecordContent: false},
			{Scope: scopeB, Model: "m2", RecordContent: true},
			{Scope: scopeB, Model: "m3", RecordContent: true},
		},
	}
	if !hasRecordedModelContent(enabled) {
		t.Fatal("ModelsConfig with RecordContent=true did not report recorded model content")
	}

	cfg := Config{Models: enabled}
	projects := configuredRetentionProjects(cfg)
	if len(projects) != 2 || projects[0] != scopeA || projects[1] != scopeB {
		t.Fatalf("configuredRetentionProjects = %+v; want [%+v %+v]", projects, scopeA, scopeB)
	}

	scopes, err := (controllerRetentionScopes{
		controller: &retentionPageController{},
		projects:   projects,
	}).scopes(context.Background())
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	wantScopes := []installation.RetentionScope{
		{TenantID: "tenant-1"},
		{TenantID: "tenant-1", ProjectID: "project-a"},
		{TenantID: "tenant-2"},
		{TenantID: "tenant-2", ProjectID: "project-b"},
	}
	if len(scopes) != len(wantScopes) {
		t.Fatalf("scopes = %+v; want %+v", scopes, wantScopes)
	}
	for index := range wantScopes {
		if scopes[index] != wantScopes[index] {
			t.Fatalf("scope[%d] = %+v; want %+v", index, scopes[index], wantScopes[index])
		}
	}

	now := time.Date(2027, 1, 10, 12, 0, 0, 0, time.UTC)
	storeOff := &recordingRetentionStore{
		candidates: map[installation.RetentionClass][]installation.RetentionCandidate{
			installation.RetentionRawContent: {{ID: "content-expired", CreatedAt: now.Add(-8 * 24 * time.Hour)}},
		},
	}
	sweeperOff, err := installation.NewRetentionSweeper(installation.RetentionPolicy{RawContentOptIn: hasRecordedModelContent(disabled)}, storeOff)
	if err != nil {
		t.Fatalf("NewRetentionSweeper off: %v", err)
	}
	if _, err := sweeperOff.Sweep(context.Background(), []installation.RetentionScope{{TenantID: "tenant-2", ProjectID: "project-b"}}, now); err != nil {
		t.Fatalf("Sweep off: %v", err)
	}
	if len(storeOff.deleted) != 0 {
		t.Fatalf("disabled raw_content sweep deleted %v; want none", storeOff.deleted)
	}

	storeOn := &recordingRetentionStore{
		candidates: map[installation.RetentionClass][]installation.RetentionCandidate{
			installation.RetentionRawContent: {
				{ID: "content-expired", CreatedAt: now.Add(-8 * 24 * time.Hour), ActiveReferences: false},
				{ID: "content-held", CreatedAt: now.Add(-8 * 24 * time.Hour), ActiveReferences: true},
			},
		},
	}
	sweeperOn, err := installation.NewRetentionSweeper(installation.RetentionPolicy{RawContentOptIn: hasRecordedModelContent(enabled)}, storeOn)
	if err != nil {
		t.Fatalf("NewRetentionSweeper on: %v", err)
	}
	result, err := sweeperOn.Sweep(context.Background(), []installation.RetentionScope{{TenantID: "tenant-2", ProjectID: "project-b"}}, now)
	if err != nil {
		t.Fatalf("Sweep on: %v", err)
	}
	if result.DeletedByClass[installation.RetentionRawContent] != 1 || result.KeptByClass[installation.RetentionRawContent] != 1 {
		t.Fatalf("raw_content sweep result = %+v; want 1 deleted and 1 kept", result)
	}
	if len(storeOn.deleted) != 1 || storeOn.deleted[0] != "content-expired" {
		t.Fatalf("deleted = %v; want [content-expired]", storeOn.deleted)
	}
}
