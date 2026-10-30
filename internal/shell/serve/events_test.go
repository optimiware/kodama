package serve_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/serve"
	"github.com/optimiware/kodama/internal/shell/store"
)

// noFiles is the file reader of a test that never reads a definition.
type noFiles struct{}

func (noFiles) ReadFiles(context.Context, domain.TenantID, gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
	return gitv1.ReadFilesResponse{}, errors.New("no worker")
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Test_unit_kernel_git_outbox_publishes_once: every committed event is published
// once under kodama.v1.<tenant-key>.<project-key>.<type> (_ for a tenant-wide
// event), marked published and never published again; a failed publication
// counts an attempt and backs the next one off without losing the row; the event
// keeps its identity across the retry.
func Test_unit_kernel_git_outbox_publishes_once(t *testing.T) {
	ctx := t.Context()
	clock := &fakeClock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMemoryGit()
	mem.SetClock(clock.Now)
	mem.AddTenant("t_acme", "acme")
	mem.AddProject("t_acme", "p_erp", "erp")
	b := bus.NewMemory()
	ev, err := serve.NewEvents(ctx, serve.EventsConfig{Store: mem, Files: noFiles{}, Bus: b, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()

	var mu sync.Mutex
	var got []bus.Message
	stop, err := b.Subscribe(ctx, "collector", "kodama.v1.>", func(_ context.Context, m bus.Message) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	delivered := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }

	// A project repository and the tenant-level one each become ready.
	project, _, err := mem.CreateRepository(ctx, store.Repository{TenantID: "t_acme", ProjectID: "p_erp", ID: "repo_1", Name: "backend", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.CreateRepository(ctx, store.Repository{TenantID: "t_acme", ID: "repo_2", Name: "context", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"repo_1", "repo_2"} {
		if _, err := mem.ConfirmInitialized(ctx, "t_acme", id, "main", true); err != nil {
			t.Fatal(err)
		}
	}
	if project.ID != "repo_1" || len(mem.Events()) != 2 {
		t.Fatalf("events before publishing: %+v", mem.Events())
	}

	res, err := ev.Publisher.Drain(ctx)
	if err != nil || res.Published != 2 || res.Failed != 0 {
		t.Fatalf("first drain = %+v, %v", res, err)
	}
	subjects := map[string]string{}
	for _, p := range b.Published() {
		subjects[p.Message.ResourceID] = p.Subject
	}
	if subjects["repo_1"] != "kodama.v1.acme.erp.repository.created" || subjects["repo_2"] != "kodama.v1.acme._.repository.created" {
		t.Fatalf("subjects = %v", subjects)
	}
	for _, e := range mem.Events() {
		if e.PublishedAt == nil || e.Attempts != 0 {
			t.Fatalf("a published event is marked %+v", e)
		}
	}
	if delivered() != 2 {
		t.Fatalf("delivered %d messages, want 2", delivered())
	}
	// Published rows are never published again, however often the publisher runs.
	for i := 0; i < 3; i++ {
		if res, err := ev.Publisher.Drain(ctx); err != nil || res.Published != 0 || res.Failed != 0 {
			t.Fatalf("a later drain republished: %+v %v", res, err)
		}
	}
	if len(b.Published()) != 2 || delivered() != 2 {
		t.Fatalf("published %d, delivered %d after the repeats", len(b.Published()), delivered())
	}

	// A broker outage: the attempt is counted, the retry deferred, the row kept.
	if _, err := mem.SetRepositoryStatus(ctx, "t_acme", "repo_1", store.RepoArchived, "", "user", "u_admin"); err != nil {
		t.Fatal(err)
	}
	b.FailNext(1, errors.New("broker down"))
	res, err = ev.Publisher.Drain(ctx)
	if err != nil || res.Published != 0 || res.Failed != 1 {
		t.Fatalf("drain during the outage = %+v, %v", res, err)
	}
	var pending store.MemoryEvent
	for _, e := range mem.Events() {
		if e.Type == "repository.state_changed" {
			pending = e
		}
	}
	if pending.PublishedAt != nil || pending.Attempts != 1 || !pending.NextAttempt.Equal(clock.Now().Add(time.Second)) {
		t.Fatalf("the failed row is %+v", pending)
	}
	if res, _ := ev.Publisher.Drain(ctx); res.Published != 0 || res.Failed != 0 {
		t.Fatalf("a backed-off row was retried early: %+v", res)
	}
	// The next failure doubles the delay.
	clock.Advance(time.Second)
	b.FailNext(1, errors.New("broker down"))
	if res, _ := ev.Publisher.Drain(ctx); res.Failed != 1 {
		t.Fatalf("second outage drain: %+v", res)
	}
	for _, e := range mem.Events() {
		if e.ID == pending.ID && (e.Attempts != 2 || !e.NextAttempt.Equal(clock.Now().Add(2*time.Second))) {
			t.Fatalf("after two failures: %+v", e)
		}
	}
	// The broker is back: the row is published exactly once, under its own ID.
	clock.Advance(2 * time.Second)
	if res, err := ev.Publisher.Drain(ctx); err != nil || res.Published != 1 || res.Failed != 0 {
		t.Fatalf("drain after recovery = %+v, %v", res, err)
	}
	if res, _ := ev.Publisher.Drain(ctx); res.Published != 0 {
		t.Fatalf("a recovered row was published twice: %+v", res)
	}
	count := 0
	for _, p := range b.Published() {
		if p.Message.ID == pending.ID {
			count++
			if p.Subject != "kodama.v1.acme.erp.repository.state_changed" || p.Message.Type != "repository.state_changed" {
				t.Fatalf("recovered message = %+v", p)
			}
		}
	}
	if count != 1 || delivered() != 3 {
		t.Fatalf("the event was published %d times, %d messages delivered", count, delivered())
	}
}
