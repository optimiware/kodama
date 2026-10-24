package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/httpapi"
)

func cmdFor(g *gates.Gate, d gates.Decision, reason string) gates.DecideCommand {
	return gates.DecideCommand{Decision: d, Reason: reason, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest()}
}

// requestedGate runs a fresh run up to a pending gate.
func requestedGate(t *testing.T, e *gatestest.Env) *gates.Gate {
	t.Helper()
	p, run := reviewed(t, e)
	finishTest(t, e, p)
	if rep := reconcile(t, e, p.Scope.TenantID); rep.Requested != 1 {
		t.Fatalf("reconcile: %+v", rep)
	}
	return gateOf(t, e, run, "review")
}

// TestConcurrentDecision (DQ-03, DQ-06, DQ-A02, PX-05): concurrent approve and
// reject by authorized reviewers record exactly one immutable decision; version
// and digest checks refuse the rest without a trace; an identical keyed replay
// records nothing; a changed body under a used key conflicts; a lost response
// is recovered from the stored outcome with no duplicate audit or event.
func TestConcurrentDecision(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()

	t.Run("exactly one decision wins", func(t *testing.T) {
		g := requestedGate(t, e)
		const n = 8
		type outcome struct {
			d   gates.Decision
			err error
		}
		results := make(chan outcome, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				who, d := gatestest.Reviewer, gates.DecisionApprove
				if i%2 == 1 {
					who, d = gatestest.Author, gates.DecisionReject
				}
				_, err := e.Gates.Decide(ctx, gatestest.Caller(who), g.Scope, g.ID, cmdFor(g, d, fmt.Sprintf("decision %d", i)))
				results <- outcome{d, err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		var won []gates.Decision
		for r := range results {
			switch {
			case r.err == nil:
				won = append(won, r.d)
			case errors.Is(r.err, gates.ErrVersionConflict):
			default:
				t.Fatalf("unexpected refusal: %v", r.err)
			}
		}
		if len(won) != 1 {
			t.Fatalf("%d decisions won", len(won))
		}
		final := gateByID(t, e, g)
		if final.Decision == nil || final.Decision.Decision != won[0] || final.Status != won[0].Resulting() || final.Version != 2 {
			t.Fatalf("final gate %+v %+v does not match the winner %s", final, final.Decision, won[0])
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d decision audit rows", n)
		}
		if n := outboxCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d gate.decided events", n)
		}
		// The decision never changes afterwards, even for the loser's request
		// made with the new version.
		before := *final.Decision
		other := gates.DecisionReject
		if won[0] == gates.DecisionReject {
			other = gates.DecisionApprove
		}
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), g.Scope, g.ID, gates.DecideCommand{
			Decision: other, Reason: "second thoughts", ExpectedVersion: final.Version, SubjectDigest: final.SubjectDigest(),
		}); !errors.Is(err, gates.ErrInvalidTransition) {
			t.Fatalf("a second decision at the current version: %v", err)
		}
		again, _ := e.Store.Get(ctx, g.Scope, g.ID)
		if *again.Decision != before || again.Version != final.Version {
			t.Fatalf("the immutable decision changed: %+v", again.Decision)
		}
	})

	t.Run("version and digest are checked", func(t *testing.T) {
		g := requestedGate(t, e)
		bad := cmdFor(g, gates.DecisionApprove, "stale digest")
		bad.SubjectDigest = "sha256:" + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000"
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), g.Scope, g.ID, bad); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("wrong digest: %v", err)
		}
		stale := cmdFor(g, gates.DecisionApprove, "stale version")
		stale.ExpectedVersion = 9
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), g.Scope, g.ID, stale); !errors.Is(err, gates.ErrVersionConflict) {
			t.Fatalf("wrong version: %v", err)
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 0 {
			t.Fatalf("a refused decision left %d audit rows", n)
		}
		cur, _ := e.Store.Get(ctx, g.Scope, g.ID)
		if cur.Status != gates.StatusPending || cur.Version != g.Version || cur.Invalidation != nil {
			t.Fatalf("refusals changed the gate: %+v", cur)
		}
	})

	t.Run("keyed replay, changed body and lost response", func(t *testing.T) {
		g := requestedGate(t, e)
		cmd := cmdFor(g, gates.DecisionApprove, "Reviewed the identified source and test evidence.")
		first, err := decideKeyed(ctx, e, g, gatestest.Reviewer, "key-1", `{"a":1}`, cmd)
		if err != nil || first.Replayed || first.Gate.Status != gates.StatusApproved {
			t.Fatalf("first: %+v %v", first, err)
		}
		// The response was lost: the client repeats the identical request.
		again, err := decideKeyed(ctx, e, g, gatestest.Reviewer, "key-1", `{"a":1}`, cmd)
		if err != nil || !again.Replayed || string(again.Body) != string(first.Body) {
			t.Fatalf("replay: %+v %v", again, err)
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d audit rows after replay", n)
		}
		if n := outboxCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d events after replay", n)
		}
		// A different body under the used key conflicts and records nothing.
		_, err = decideKeyed(ctx, e, g, gatestest.Reviewer, "key-1", `{"a":2}`, cmdFor(g, gates.DecisionReject, "changed"))
		var api *httpapi.APIError
		if !errors.As(err, &api) || api.Status != 409 || string(api.Code) != "idempotency_conflict" {
			t.Fatalf("changed body: %v", err)
		}
		cur, _ := e.Store.Get(ctx, g.Scope, g.ID)
		if cur.Status != gates.StatusApproved || cur.Version != 2 {
			t.Fatalf("gate after conflict: %+v", cur)
		}
		// A different key for the same version after the decision cannot decide again.
		if _, err := decideKeyed(ctx, e, g, gatestest.Reviewer, "key-2", `{"a":3}`, cmdFor(g, gates.DecisionReject, "late")); err == nil {
			t.Fatalf("a second key recorded a second decision")
		}
		if n := auditCount(e, "gate.decided", g.ID); n != 1 {
			t.Fatalf("%d audit rows", n)
		}
	})
}
