package memory_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestUnitPeerJoins proves the peer joins (work admission, workspace writer
// slot and retained source, secret pins, events) share the store's transaction:
// a commit that fails after every peer wrote leaves none of their rows, and a
// replay after the failure commits them exactly once.
func TestUnitPeerJoins(t *testing.T) {
	ctx := context.Background()
	tenant := scopeA.TenantID
	var failing atomic.Bool
	fault := errors.New("commit lost")
	m, p := fixture(t, executiontest.MemOptions{CommitFault: func() error {
		if failing.Load() {
			return fault
		}
		return nil
	}})
	ref := m.World.CreateSecret(scopeA, "token")
	def := define(m, job("edit", writeRun, "env: {TOKEN: {secretRef: \""+ref+"\"}}"))
	cmd := execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: m.ContentRevision(p), SourceCommit: executiontest.CommitA}

	plan, err := m.Svc.Prepare(ctx, engineer, cmd)
	if err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	if _, _, err := m.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrUnavailable) || !errors.Is(err, fault) {
		t.Fatalf("admit with a failing commit: %v", err)
	}
	failing.Store(false)
	runID := plan.Aggregate.Run.ID
	if pins, _ := m.World.LivePins(ctx, scopeA, runID); len(pins) != 0 {
		t.Fatalf("secret pins survived a failed commit: %v", pins)
	}
	if pins, _ := m.World.PinsOf(ctx, scopeA, string(p.Workspace)); len(pins) != 0 {
		t.Fatalf("a retained-source intent survived a failed commit: %v", pins)
	}
	if evs, _ := m.World.Events(ctx, tenant); len(evs) != 0 {
		t.Fatalf("%d events survived a failed commit", len(evs))
	}
	if _, err := m.Store.Get(ctx, scopeA, runID); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("the run survived a failed commit: %v", err)
	}

	// The same plan commits everything once, and its replay changes nothing.
	for i, wantCreated := range []bool{true, false} {
		if _, created, err := m.Store.Admit(ctx, plan); err != nil || created != wantCreated {
			t.Fatalf("admit %d: created=%v err=%v", i, created, err)
		}
	}
	if pins, _ := m.World.LivePins(ctx, scopeA, runID); len(pins) != 1 {
		t.Fatalf("pins %v", pins)
	}
	if pins, _ := m.World.PinsOf(ctx, scopeA, string(p.Workspace)); len(pins) != 1 {
		t.Fatalf("workspace pins %v", pins)
	}
	if n := m.EventCount(tenant, "run.created"); n != 1 {
		t.Fatalf("%d run.created events", n)
	}
	hist, err := m.Mem.History(ctx, scopeA, runID)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, h := range hist {
		if h.Subject == execution.SubjectRun && h.From == "" {
			created++
		}
	}
	if created != 1 || len(hist) != 2 { // the run's creation and its one step
		t.Fatalf("%d creation rows in %d history rows: a repeated plan stacked its creation history", created, len(hist))
	}
	if _, err := m.Svc.ConfirmSource(ctx, scopeA, runID); err != nil {
		t.Fatal(err)
	}

	// A claim whose commit is lost leaves no attempt, reservation or writer slot.
	failing.Store(true)
	if _, err := m.Claim(tenant); err == nil {
		t.Fatal("a claim with a failing commit succeeded")
	}
	failing.Store(false)
	if agg, err := m.Store.Get(ctx, scopeA, runID); err != nil || len(agg.Attempts) != 0 || agg.Step("edit").AttemptsStarted != 0 {
		t.Fatalf("a lost claim left state: %v %v", agg, err)
	}
	if held(m) != 0 || writerOf(m, p) != "" {
		t.Fatalf("a lost claim left %d reservations and writer %q", held(m), writerOf(m, p))
	}
	in := m.MustClaim(tenant)
	if in.Ordinal != 1 || held(m) != 1 || writerOf(m, p) != in.AttemptID {
		t.Fatalf("the retried claim %+v: %d reservations, writer %q", in, held(m), writerOf(m, p))
	}
}
