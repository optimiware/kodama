package work_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

var allStates = []work.State{work.StateBacklog, work.StateReady, work.StateActive, work.StateReview, work.StateDone, work.StateCancelled}

func goodRequest(it work.Item, to work.State) work.TransitionRequest {
	req := work.TransitionRequest{To: to, Reason: "because"}
	switch {
	case it.State == work.StateActive && to == work.StateReview && it.Kind != work.KindEpic:
		req.Review = &work.ReviewClaim{CandidateCommit: commit1, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_1"}}}
	case to == work.StateDone:
		req.Completion = &work.CompletionClaim{ContentRevision: it.ContentRevision, AcceptedCriteria: []string{"AC-1"}}
		if it.Kind != work.KindEpic {
			req.Completion.CandidateCommit = commit1
		}
	}
	return req
}

func goodEnv() *fakeEnv {
	return &fakeEnv{children: work.ChildSummary{Total: 2, Done: 2}}
}

// Test_transition_matrix: every state pair, the exact allowed edges, required
// reasons, blocking and specification prerequisites, writer-busy departures,
// epic review, completion bindings and fail-closed guards. No external services.
func Test_transition_matrix(t *testing.T) {
	allowed := map[[2]work.State]bool{
		{work.StateBacklog, work.StateReady}: true, {work.StateReady, work.StateBacklog}: true,
		{work.StateReady, work.StateActive}: true, {work.StateActive, work.StateReady}: true,
		{work.StateActive, work.StateReview}: true, {work.StateReview, work.StateActive}: true,
		{work.StateReview, work.StateDone}: true, {work.StateDone, work.StateActive}: true,
		{work.StateBacklog, work.StateCancelled}: true, {work.StateReady, work.StateCancelled}: true,
		{work.StateActive, work.StateCancelled}: true, {work.StateReview, work.StateCancelled}: true,
	}

	t.Run("edges", func(t *testing.T) {
		for _, kind := range []work.Kind{work.KindStory, work.KindEpic} {
			for _, from := range allStates {
				for _, to := range allStates {
					it := inState(kind, from)
					actor := engineer
					if to == work.StateDone {
						actor = reviewer
					}
					next, h, err := transition(it, goodEnv(), goodRequest(it, to), actor)
					name := fmt.Sprintf("%s %s->%s", kind, from, to)
					if work.AllowedEdge(from, to) != allowed[[2]work.State{from, to}] {
						t.Errorf("%s: AllowedEdge disagrees with the specification", name)
					}
					if !allowed[[2]work.State{from, to}] {
						if !is(err, work.ErrInvalidTransition) {
							t.Errorf("%s: want invalid transition, got %v", name, err)
						}
						continue
					}
					if err != nil {
						t.Errorf("%s: %v", name, err)
						continue
					}
					if next.State != to || next.Version != it.Version+1 || next.ContentRevision != it.ContentRevision {
						t.Errorf("%s: state/version/revision = %s/%d/%d", name, next.State, next.Version, next.ContentRevision)
					}
					if h.Kind != work.ChangeTransitioned || h.From != from || h.To != to || h.Version != next.Version || h.ID != "wih_2" || h.Actor != actor {
						t.Errorf("%s: history = %+v", name, h)
					}
				}
			}
		}
	})

	t.Run("cancelled_has_no_outgoing_edge", func(t *testing.T) {
		for _, to := range allStates {
			if _, _, err := transition(inState(work.KindTask, work.StateCancelled), goodEnv(), work.TransitionRequest{To: to, Reason: "x"}, engineer); !is(err, work.ErrInvalidTransition) {
				t.Errorf("cancelled -> %s: %v", to, err)
			}
		}
	})

	t.Run("reasons_required", func(t *testing.T) {
		for _, e := range []struct{ from, to work.State }{
			{work.StateReady, work.StateBacklog}, {work.StateActive, work.StateReady}, {work.StateReview, work.StateActive},
			{work.StateDone, work.StateActive}, {work.StateBacklog, work.StateCancelled}, {work.StateReady, work.StateCancelled},
			{work.StateActive, work.StateCancelled}, {work.StateReview, work.StateCancelled},
		} {
			it := inState(work.KindStory, e.from)
			env := goodEnv()
			req := goodRequest(it, e.to)
			req.Reason = ""
			if _, _, err := transition(it, env, req, engineer); !is(err, work.ErrPrerequisite) {
				t.Errorf("%s->%s without a reason: %v", e.from, e.to, err)
			}
			req.Reason = strings.Repeat("r", work.MaxReasonChars+1)
			if _, _, err := transition(it, env, req, engineer); !is(err, work.ErrInvalid) {
				t.Errorf("%s->%s with an oversized reason: %v", e.from, e.to, err)
			}
		}
		// backlog->ready, ready->active and active->review need no reason.
		for _, e := range []struct{ from, to work.State }{{work.StateBacklog, work.StateReady}, {work.StateReady, work.StateActive}, {work.StateActive, work.StateReview}} {
			it := inState(work.KindStory, e.from)
			req := goodRequest(it, e.to)
			req.Reason = ""
			if _, _, err := transition(it, goodEnv(), req, engineer); err != nil {
				t.Errorf("%s->%s: %v", e.from, e.to, err)
			}
		}
	})

	t.Run("ready_requires_specification_and_acceptance", func(t *testing.T) {
		for name, mutate := range map[string]func(*work.Item){
			"no spec":     func(i *work.Item) { i.Spec = " \n" },
			"no criteria": func(i *work.Item) { i.Criteria = nil },
		} {
			it := inState(work.KindStory, work.StateBacklog)
			mutate(&it)
			if _, _, err := transition(it, goodEnv(), work.TransitionRequest{To: work.StateReady}, engineer); !is(err, work.ErrPrerequisite) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("blocking_and_dependencies_gate_ready_to_active", func(t *testing.T) {
		it := inState(work.KindTask, work.StateReady)
		it.BlockedReason = "waiting for ERP access"
		env := goodEnv()
		if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateActive}, engineer); !is(err, work.ErrPrerequisite) || env.admitCalls != 0 {
			t.Fatalf("blocked item: err %v, admissions %d", err, env.admitCalls)
		}
		it.BlockedReason = ""
		env = goodEnv()
		env.depsErr = fmt.Errorf("%w: predecessor open", work.ErrPrerequisite)
		if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateActive}, engineer); !is(err, work.ErrPrerequisite) || env.admitCalls != 0 {
			t.Fatalf("unsatisfied dependency: err %v, admissions %d", err, env.admitCalls)
		}
		// A blocked reopened or reworked item is not refused by the ready-only rule.
		rework := inState(work.KindTask, work.StateReview)
		rework.BlockedReason = "x"
		if _, _, err := transition(rework, goodEnv(), work.TransitionRequest{To: work.StateActive, Reason: "rework"}, engineer); err != nil {
			t.Fatalf("rework of a blocked item: %v", err)
		}
	})

	t.Run("every_entry_into_active_uses_wip_admission", func(t *testing.T) {
		for _, from := range []work.State{work.StateReady, work.StateReview, work.StateDone} {
			it := inState(work.KindStory, from)
			env := goodEnv()
			if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateActive, Reason: "go"}, engineer); err != nil || env.admitCalls != 1 {
				t.Fatalf("%s->active: err %v, admissions %d", from, err, env.admitCalls)
			}
			env = goodEnv()
			env.admitErr = work.ErrWIPLimit
			if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateActive, Reason: "go"}, engineer); !is(err, work.ErrWIPLimit) {
				t.Fatalf("%s->active at capacity: %v", from, err)
			}
		}
		for _, e := range []struct{ from, to work.State }{{work.StateActive, work.StateReady}, {work.StateActive, work.StateReview}, {work.StateBacklog, work.StateReady}, {work.StateActive, work.StateCancelled}} {
			it := inState(work.KindStory, e.from)
			env := goodEnv()
			if _, _, err := transition(it, env, goodRequest(it, e.to), engineer); err != nil || env.admitCalls != 0 {
				t.Fatalf("%s->%s: err %v, admissions %d", e.from, e.to, err, env.admitCalls)
			}
		}
	})

	t.Run("leaving_active_waits_for_the_writer_but_cancellation_does_not", func(t *testing.T) {
		it := inState(work.KindStory, work.StateActive)
		env := goodEnv()
		env.writerBusy = true
		for _, to := range []work.State{work.StateReady, work.StateReview} {
			if _, _, err := transition(it, env, goodRequest(it, to), engineer); !is(err, work.ErrWorkspaceBusy) {
				t.Errorf("active->%s with an unsettled writer: %v", to, err)
			}
		}
		if next, _, err := transition(it, env, goodRequest(it, work.StateCancelled), engineer); err != nil || next.State != work.StateCancelled {
			t.Errorf("cancellation with an unsettled writer: %v", err)
		}
		env.writerErr = errors.New("workspace store down")
		if _, _, err := transition(it, env, goodRequest(it, work.StateReady), engineer); err == nil {
			t.Error("a failing writer guard must not allow leaving active")
		}
	})

	t.Run("review_evidence", func(t *testing.T) {
		it := inState(work.KindStory, work.StateActive)
		for name, req := range map[string]work.TransitionRequest{
			"no claim":     {To: work.StateReview},
			"no evidence":  {To: work.StateReview, Review: &work.ReviewClaim{CandidateCommit: commit1}},
			"no candidate": {To: work.StateReview, Review: &work.ReviewClaim{Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "a"}}}},
		} {
			if _, _, err := transition(it, goodEnv(), req, engineer); !is(err, work.ErrPrerequisite) {
				t.Errorf("%s: %v", name, err)
			}
		}
		req := goodRequest(it, work.StateReview)
		req.Review.CandidateCommit = "XYZ"
		if _, _, err := transition(it, goodEnv(), req, engineer); !is(err, work.ErrInvalid) {
			t.Errorf("malformed commit: %v", err)
		}
		env := goodEnv()
		env.subjectErr = fmt.Errorf("%w: artifact art_1", work.ErrNotFound)
		if _, _, err := transition(it, env, goodRequest(it, work.StateReview), engineer); !is(err, work.ErrNotFound) {
			t.Errorf("unknown evidence: %v", err)
		}
		next, h, err := transition(it, goodEnv(), goodRequest(it, work.StateReview), engineer)
		if err != nil || next.ReviewSubject == nil || next.ReviewSubject.CandidateCommit != commit1 || next.ReviewSubject.ContentRevision != it.ContentRevision || h.Review == nil {
			t.Fatalf("review subject: %+v %v", next.ReviewSubject, err)
		}
	})

	t.Run("epic_review_uses_the_child_summary", func(t *testing.T) {
		it := inState(work.KindEpic, work.StateActive)
		for name, sum := range map[string]work.ChildSummary{
			"no children": {}, "open child": {Total: 2, Done: 1}, "all cancelled": {Total: 2, Cancelled: 2},
		} {
			env := goodEnv()
			env.children = sum
			if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateReview}, engineer); !is(err, work.ErrPrerequisite) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, _, err := transition(it, goodEnv(), work.TransitionRequest{To: work.StateReview, Review: &work.ReviewClaim{CandidateCommit: commit1}}, engineer); !is(err, work.ErrInvalid) {
			t.Errorf("an epic has no candidate: %v", err)
		}
		next, _, err := transition(it, goodEnv(), work.TransitionRequest{To: work.StateReview}, engineer)
		if err != nil || next.ReviewSubject == nil || next.ReviewSubject.ChildSummary == nil || next.ReviewSubject.ChildSummary.Done != 2 {
			t.Fatalf("epic review: %+v %v", next.ReviewSubject, err)
		}
	})

	t.Run("an_epic_never_asks_for_a_writer", func(t *testing.T) {
		it := inState(work.KindEpic, work.StateActive)
		env := goodEnv()
		env.writerErr = errors.New("must not be consulted")
		if _, _, err := transition(it, env, work.TransitionRequest{To: work.StateReady, Reason: "x"}, engineer); err != nil {
			t.Fatalf("epic active->ready: %v", err)
		}
	})

	t.Run("exact_edge_permission_is_required", func(t *testing.T) {
		for _, e := range []struct {
			from, to work.State
			perm     work.Permission
		}{
			{work.StateBacklog, work.StateReady, work.PermRefine}, {work.StateReady, work.StateActive, work.PermWork},
			{work.StateDone, work.StateActive, work.PermReopen}, {work.StateReview, work.StateDone, work.PermComplete},
			{work.StateActive, work.StateCancelled, work.PermCancel},
		} {
			it := inState(work.KindStory, e.from)
			env := goodEnv()
			env.denied = map[work.Permission]error{e.perm: work.ErrDenied}
			actor := engineer
			if e.to == work.StateDone {
				actor = reviewer
			}
			if _, _, err := transition(it, env, goodRequest(it, e.to), actor); !is(err, work.ErrDenied) {
				t.Errorf("%s->%s without %s: %v", e.from, e.to, e.perm, err)
			}
		}
	})

	t.Run("fail_closed_guards", func(t *testing.T) {
		var missing work.TxGuards // no guard configured at all
		it := inState(work.KindStory, work.StateActive)
		if _, _, err := it.Transition(context.Background(), guardEnv{missing}, goodRequest(it, work.StateReady), engineer, now, "wih_2", ""); !is(err, work.ErrUnavailable) {
			t.Fatalf("missing permission guard: %v", err)
		}
		ready := inState(work.KindStory, work.StateReady)
		g := work.TxGuards{Permissions: allowAll{}}
		if _, _, err := ready.Transition(context.Background(), guardEnv{g}, work.TransitionRequest{To: work.StateActive}, engineer, now, "wih_2", ""); !is(err, work.ErrUnavailable) {
			t.Fatalf("missing dependency guard: %v", err)
		}
		if _, _, err := it.Transition(context.Background(), guardEnv{g}, goodRequest(it, work.StateReady), engineer, now, "wih_2", ""); !is(err, work.ErrUnavailable) {
			t.Fatalf("missing writer guard: %v", err)
		}
		g.Writers = writerSettled{err: errors.New("db down")}
		if _, _, err := it.Transition(context.Background(), guardEnv{g}, goodRequest(it, work.StateReady), engineer, now, "wih_2", ""); !is(err, work.ErrUnavailable) {
			t.Fatalf("an unknown writer-guard failure must be unavailability, got %v", err)
		}
	})
}

// guardEnv adapts TxGuards to TransitionEnv with no WIP or child support, to
// test that missing guards fail closed.
type guardEnv struct{ work.TxGuards }

func (guardEnv) AdmitActive(context.Context, work.Item) error { return nil }
func (guardEnv) ChildSummary(context.Context, work.Item) (work.ChildSummary, error) {
	return work.ChildSummary{}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}

type writerSettled struct{ err error }

func (w writerSettled) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, w.err
}
