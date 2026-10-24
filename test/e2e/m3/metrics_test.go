package m3_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestM3Metrics (m3_metrics) is WM-A08 and the tenant half of WM-12 over the
// history the real API recorded: an item is admitted, blocked, unblocked,
// paused, admitted again and cancelled with real pauses between the calls, and
// the SQL flow summary of the assembled core (the business-loop slice's flow
// service over the runtime role's database) must equal the intervals computed
// independently from the timestamps of the item's public history. A window
// that opens mid-flight, one that closes before the item existed and one that
// ends in the future are reported as what they are, an item still open is
// capped at the window's end without being called complete, and another
// tenant's scope sees nothing of it.
// Review intervals and reopen history need an item in review or done, which
// the assembly cannot reach (TestM3ReviewGuardNotWired); their SQL semantics
// stay qualified by the module's own database tests (T-042).
func TestM3Metrics(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true, NoChatOps: true})
	e.SetWIP(5)
	pause := func(d time.Duration) { time.Sleep(d) }

	p := e.NewBareItem("Flow subject", "# flow subject")
	pause(300 * time.Millisecond)
	p = e.Ready(p)
	pause(300 * time.Millisecond)
	p = e.MustTransition("engineer", p, "active", nil)
	pause(700 * time.Millisecond)
	p = e.Block(p, "waiting for the ERP sandbox")
	pause(700 * time.Millisecond)
	p = e.Block(p, "")
	pause(400 * time.Millisecond)
	p = e.MustTransition("engineer", p, "ready", map[string]any{"reason": "paused"})
	pause(300 * time.Millisecond)
	p = e.MustTransition("engineer", p, "active", nil)
	pause(500 * time.Millisecond)
	p = e.MustTransition("engineer", p, "cancelled", map[string]any{"reason": "superseded"})
	pause(200 * time.Millisecond)

	q := e.NewBareItem("Still open", "# still open")
	q = e.Admit(q)
	pause(600 * time.Millisecond)

	h := e.History(p.Id)
	if len(h) != 8 {
		t.Fatalf("history of %d entries: %+v", len(h), h)
	}
	at := func(i int) time.Time { return h[i].OccurredAt.UTC() }
	wantStates := []string{"backlog", "ready", "active", "active", "active", "ready", "active", "cancelled"}
	for i, ent := range h {
		if ent.To != nil && string(*ent.To) != wantStates[i] {
			t.Fatalf("history entry %d goes to %s, want %s", i, *ent.To, wantStates[i])
		}
		if i > 0 && ent.OccurredAt.Before(h[i-1].OccurredAt) {
			t.Fatalf("history is out of order at %d", i)
		}
	}
	wantActive := at(5).Sub(at(2)) + at(7).Sub(at(6))
	wantBlocked := at(4).Sub(at(3))
	wantLead := at(7).Sub(at(0))

	scope := domain.Scope{TenantID: domain.TenantID(m2.TenantA), ProjectID: domain.ProjectID(e.World.Project)}
	reader := e.UserID("viewer")
	flow := e.Loop().Flow
	now := time.Now().UTC().Truncate(time.Microsecond)
	win := func(from, to time.Time) metrics.Window {
		w, err := flow.Window(from, to)
		if err != nil {
			t.Fatalf("window %s..%s: %v", from, to, err)
		}
		return w
	}
	summarize := func(sc domain.Scope, who string, w metrics.Window, ids ...string) ([]metrics.WorkSummary, error) {
		var ws []domain.WorkItemID
		for _, id := range ids {
			ws = append(ws, domain.WorkItemID(id))
		}
		return flow.Summarize(context.Background(), sc, who, ws, w)
	}

	// ---- the full window: every figure equals the public history's arithmetic ----
	from := at(0).Add(-time.Second).Truncate(time.Microsecond)
	sums, err := summarize(scope, reader, win(from, now), p.Id, q.Id)
	if err != nil || len(sums) != 2 {
		t.Fatalf("summaries: %+v %v", sums, err)
	}
	sp, sq := sums[0], sums[1]
	if sp.ItemID != domain.WorkItemID(p.Id) || sq.ItemID != domain.WorkItemID(q.Id) {
		t.Fatalf("summaries are out of request order: %s %s", sp.ItemID, sq.ItemID)
	}
	if sp.Status != metrics.StatusOK || sp.StateAtEnd != "cancelled" || sp.OpenAtEnd || sp.InFlightAtStart {
		t.Fatalf("cancelled item: %+v", sp)
	}
	if sp.Active != wantActive || sp.Blocked != wantBlocked || sp.Lead != wantLead || sp.Review != 0 || sp.Reopened != 0 {
		t.Fatalf("durations active %s blocked %s lead %s review %s reopened %s, history says active %s blocked %s lead %s",
			sp.Active, sp.Blocked, sp.Lead, sp.Review, sp.Reopened, wantActive, wantBlocked, wantLead)
	}
	if sp.BlockedIntervals != 1 || sp.CancelledEpisodes != 1 || sp.CompletedEpisodes != 0 || sp.ReopenCount != 0 || sp.CompletedLead != 0 || sp.ObservedVersion != domain.Version(p.Version) {
		t.Fatalf("counts: %+v (item version %d)", sp, p.Version)
	}
	if wantActive < 1100*time.Millisecond || wantBlocked < 600*time.Millisecond {
		t.Fatalf("the scenario measured nothing: active %s blocked %s", wantActive, wantBlocked)
	}
	// The item still open is capped at the window's end, never called complete.
	if sq.Status != metrics.StatusOK || !sq.OpenAtEnd || sq.StateAtEnd != "active" || sq.CompletedEpisodes != 0 || sq.Active < 500*time.Millisecond || sq.Active > now.Sub(from) || sq.Blocked != 0 {
		t.Fatalf("open item: %+v", sq)
	}

	// ---- incomplete windows are reported as such ----
	mid := at(3).Add(-300 * time.Millisecond).Truncate(time.Microsecond) // inside the first active stretch
	sums, err = summarize(scope, reader, win(mid, now), p.Id)
	if err != nil || len(sums) != 1 {
		t.Fatalf("mid-flight window: %+v %v", sums, err)
	}
	sm := sums[0]
	if !sm.InFlightAtStart || sm.Lead != at(7).Sub(mid) || sm.Active != (at(5).Sub(mid)+at(7).Sub(at(6))) || sm.Blocked != wantBlocked {
		t.Fatalf("a window opening mid-flight counted time before it: %+v (want lead %s)", sm, at(7).Sub(mid))
	}
	if sums, err = summarize(scope, reader, win(from, at(0).Add(-time.Millisecond).Truncate(time.Microsecond)), p.Id); err != nil || len(sums) != 1 || sums[0].Status != metrics.StatusNotStarted || sums[0].Lead != 0 {
		t.Fatalf("a window that closes before the item existed: %+v %v", sums, err)
	}
	if _, err := flow.Window(from, time.Now().UTC().Add(time.Hour)); !errors.Is(err, metrics.ErrInvalid) {
		t.Fatalf("a window ending in the future: %v", err)
	}
	// A window that ends inside the blocked stretch caps the open blocked interval.
	cut := at(3).Add(400 * time.Millisecond).Truncate(time.Microsecond)
	sums, err = summarize(scope, reader, win(from, cut), p.Id)
	if err != nil || len(sums) != 1 || !sums[0].OpenAtEnd || sums[0].Blocked != cut.Sub(at(3)) || sums[0].BlockedIntervals != 1 || sums[0].StateAtEnd != "active" || sums[0].CancelledEpisodes != 0 {
		t.Fatalf("a window ending inside the blocked interval: %+v %v", sums, err)
	}

	// ---- tenant scope (IT-03): another tenant's scope and callers see nothing ----
	foreign := domain.Scope{TenantID: domain.TenantID(m2.TenantB), ProjectID: domain.ProjectID(e.World.ProjectB)}
	if sums, err := summarize(foreign, e.UserID("analyst"), win(from, now), p.Id, q.Id); err != nil || len(sums) != 0 {
		t.Fatalf("tenant B's scope summarized tenant A's items: %+v %v", sums, err)
	}
	if sums, err := summarize(scope, e.UserID("foreign"), win(from, now), p.Id); err == nil && len(sums) != 0 {
		t.Fatalf("a tenant B principal read tenant A's flow: %+v", sums)
	}
	if sums, err := summarize(scope, e.UserID("outsider"), win(from, now), p.Id); err == nil && len(sums) != 0 {
		t.Fatalf("a caller without a binding read the flow: %+v", sums)
	}
	// Reading changed nothing.
	var after client.WorkItem = e.GetItem(p.Id)
	if after.Version != p.Version || len(e.History(p.Id)) != 8 {
		t.Fatalf("summarizing changed the item: %+v", after)
	}
}
