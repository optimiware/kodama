package workspace_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// Test_unit_cumulative_writer_publication (WS-16): in a run_shared run the later
// writers of one work item publish on top of the earlier writers' accepted
// commits, each through its own expected-tip compare-and-swap; the pinned input of
// the run is never rewritten, a human push still wins the race, and a writer that
// builds on a stale head is refused instead of overwriting the earlier work.
func Test_unit_cumulative_writer_publication(t *testing.T) {
	ctx := context.Background()
	e := wt.NewMemServiceEnv(t)
	w := e.Ready(wt.ScopeA, "wi_1")
	initial := e.Repo.Initial

	publish := func(attempt string, base workspace.Commit, name, content string) workspace.Commit {
		t.Helper()
		reserved := e.Writer(w, domainAttempt(attempt))
		if reserved.Writer == nil || reserved.Writer.InputCommit != base || reserved.Writer.ExpectedTip != base {
			t.Fatalf("%s reserves input %v tip %v, want both %s (the run's head so far)", attempt, reserved.Writer, reserved.Writer, base)
		}
		in := e.Proposal(w, domainAttempt(attempt), base, attempt, wt.File{Path: name, Content: content})
		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposeAccepted || res.AcceptedCommit != in.Commit {
			t.Fatalf("%s: %+v %v", attempt, res, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit {
			t.Fatalf("%s: tip %q, want %q", attempt, tip, in.Commit)
		}
		// An identical repeat after a lost response observes the same outcome.
		again, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || again.Status != workspace.ProposeAccepted || again.Commit != in.Commit {
			t.Fatalf("%s repeat: %+v %v", attempt, again, err)
		}
		// The step settles (its result is accepted) before the next writer of the run
		// is admitted, exactly as execution orders the workspace holders.
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{
			Scope: w.Scope, WorkspaceID: w.ID, AttemptID: domainAttempt(attempt), Outcome: workspace.OutcomeSucceeded, SourceCommit: in.Commit,
		}); err != nil {
			t.Fatalf("%s settle: %v", attempt, err)
		}
		return in.Commit
	}

	t1 := publish("att_1", initial, "generated.txt", "from step one")
	t2 := publish("att_2", t1, "report.txt", "from step two, on top of step one")

	// The history is linear: input <- T1 <- T2. Neither writer rewrote another's work.
	if !e.Repo.IsAncestor(t, initial, t1) || !e.Repo.IsAncestor(t, t1, t2) || e.Repo.IsAncestor(t, t2, t1) {
		t.Fatal("the later writer did not publish on top of the earlier one")
	}
	if tip := e.Repo.Tip(t, e.Repo.BaseRef); tip != initial {
		t.Fatalf("the base branch moved to %q", tip)
	}
	cur, err := e.Svc.Get(ctx, wt.Engineer, w.Scope, w.ID)
	if err != nil || cur.AcceptedCommit != t2 || cur.Pending != nil || cur.Writer != nil {
		t.Fatalf("workspace after two writers: %+v %v", cur, err)
	}

	t.Run("a writer built on a stale head is refused", func(t *testing.T) {
		e.Writer(w, "att_3") // reserved at T2
		// Built on the run's input instead of the cumulative head: it would drop T1 and T2.
		stale := e.Proposal(w, "att_3", initial, "stale", wt.File{Path: "x.txt", Content: "x"})
		stale.ExpectedTip = t2
		_, err := e.Svc.ProposeSource(ctx, stale)
		var be *workspace.BundleError
		if !errors.As(err, &be) {
			t.Fatalf("a bundle not descending from the cumulative head: %v", err)
		}
		// Naming the stale tip is refused by the reservation itself.
		wrong := e.Proposal(w, "att_3", initial, "stale", wt.File{Path: "y.txt", Content: "y"})
		if _, err := e.Svc.ProposeSource(ctx, wrong); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("an expected tip other than the reservation's: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != t2 {
			t.Fatalf("a refused proposal moved the branch to %q", tip)
		}
	})

	t.Run("a human push between the writers still wins", func(t *testing.T) {
		human := e.Repo.HumanPush(t, w.WorkBranch, "human fix", wt.File{Path: "h.txt", Content: "h"})
		// att_3 holds the reservation at T2 and builds on it: the branch moved.
		in := e.Proposal(w, "att_3", t2, "late", wt.File{Path: "late.txt", Content: "late"})
		_, err := e.Svc.ProposeSource(ctx, in)
		var conflict *workspace.ConflictError
		if !errors.Is(err, workspace.ErrPublicationConflict) || !errors.As(err, &conflict) || conflict.ObservedTip != human {
			t.Fatalf("propose after a human push: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != human {
			t.Fatalf("the human commit was lost: %q", tip)
		}
		// The conflict is the attempt's outcome and is observed again, never overwritten.
		if _, err := e.Svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrPublicationConflict) {
			t.Fatalf("a repeat of a conflicted proposal: %v", err)
		}
	})
}

func domainAttempt(s string) domain.AttemptID { return domain.AttemptID(s) }
