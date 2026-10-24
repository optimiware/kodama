package memory_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/memory"
)

// TestUnitSharedKernelAppendOnly proves the append-only rows (history, audit,
// evidence) survive a second Store over the same kernel, such as a rebuilt
// adapter or two control-plane components: sequence keys come from the kernel's
// transactional state, so nothing the first store wrote is replaced.
func TestUnitSharedKernelAppendOnly(t *testing.T) {
	ctx := context.Background()
	m, p := fixture(t)
	run := m.MustAdmit(p, define(m, job("a", readRun), job("b", readRun)), nil)
	first, second := m.MustClaim(scopeA.TenantID), m.MustClaim(scopeA.TenantID)
	m.Start(first)

	other, err := memory.NewStore(m.DB, m.World.Peers(m.Auth), memory.Options{Clock: m.Clock.Now, NewID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(s *memory.Store) (hist []memory.History, audit []memory.Audit, ev []memory.Evidence) {
		t.Helper()
		var err error
		if hist, err = s.History(ctx, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		if audit, err = s.AuditRows(ctx, scopeA); err != nil {
			t.Fatal(err)
		}
		for _, id := range []domain.AttemptID{first.AttemptID, second.AttemptID} {
			rows, err := s.EvidenceOf(ctx, scopeA, id)
			if err != nil {
				t.Fatal(err)
			}
			ev = append(ev, rows...)
		}
		return hist, audit, ev
	}
	// Evidence of the first store, then rows of the first and of the second store
	// interleaved.
	record := func(s *memory.Store, at domain.AttemptID, kind execution.EvidenceKind) {
		t.Helper()
		if _, _, err := s.Update(ctx, scopeA, run.ID, execution.UpdateOptions{}, func(_ context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
			agg.RecordEvidence(at, kind, map[string]any{"by": "test"}, env.Now())
			return execution.Effects{}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(m.Mem, first.AttemptID, execution.EvidenceLateObservation)
	h0, a0, e0 := snapshot(m.Mem)
	if len(h0) == 0 || len(a0) == 0 || len(e0) != 1 {
		t.Fatalf("setup wrote %d history, %d audit, %d evidence rows", len(h0), len(a0), len(e0))
	}

	record(other, second.AttemptID, execution.EvidenceLateResult)
	for i, s := range []*memory.Store{other, m.Mem, other} {
		if _, _, err := s.Update(ctx, scopeA, run.ID, execution.UpdateOptions{}, func(_ context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
			agg.RequestCancel(execution.ReasonCancelled, env.Now()) // changes the run once, then repeats
			return execution.Effects{}, nil
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	record(m.Mem, first.AttemptID, execution.EvidenceTermination)

	h1, a1, e1 := snapshot(other)
	if len(h1) <= len(h0) || len(a1) <= len(a0) || len(e1) != 3 {
		t.Fatalf("rows after the second store: %d/%d history, %d/%d audit, %d evidence", len(h1), len(h0), len(a1), len(a0), len(e1))
	}
	// Every earlier row is still there, unchanged and in the same order.
	for i, h := range h0 {
		if h1[i] != h {
			t.Fatalf("history row %d was replaced: %+v became %+v", i, h, h1[i])
		}
	}
	for i, a := range a0 {
		if a1[i].Seq != a.Seq || a1[i].Action != a.Action || a1[i].ResourceID != a.ResourceID || a1[i].Version != a.Version {
			t.Fatalf("audit row %d was replaced: %+v became %+v", i, a, a1[i])
		}
	}
	kinds := map[execution.EvidenceKind]int{}
	for _, e := range e1 {
		kinds[e.Kind]++
	}
	if kinds[execution.EvidenceLateObservation] != 1 || kinds[execution.EvidenceLateResult] != 1 || kinds[execution.EvidenceTermination] != 1 {
		t.Fatalf("evidence %v", kinds)
	}
	// Keys are unique and strictly increasing across both stores.
	seen := map[string]bool{}
	last := ""
	for _, h := range h1 {
		if seen["h"+h.Seq] || h.Seq <= last {
			t.Fatalf("history sequence %q is not strictly increasing", h.Seq)
		}
		seen["h"+h.Seq], last = true, h.Seq
	}
	// The first store sees the same rows.
	if h2, a2, e2 := snapshot(m.Mem); len(h2) != len(h1) || len(a2) != len(a1) || len(e2) != len(e1) {
		t.Fatal("the two stores disagree about the rows of one kernel")
	}
}
