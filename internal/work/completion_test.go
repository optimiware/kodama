package work_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/work"
)

func doneRequest(it work.Item) work.TransitionRequest {
	return work.TransitionRequest{To: work.StateDone, Reason: "accepted", Completion: &work.CompletionClaim{
		ContentRevision: it.ContentRevision, CandidateCommit: commit1, AcceptedCriteria: []string{"AC-1"},
	}}
}

// Test_completion_decision: the human decision is bound to the observed
// revision, the recorded candidate, every criterion and every finding; nothing
// but an explicit human request can produce it.
func Test_completion_decision(t *testing.T) {
	t.Run("records_the_decision", func(t *testing.T) {
		it := inState(work.KindStory, work.StateReview)
		next, h, err := transition(it, goodEnv(), doneRequest(it), reviewer)
		if err != nil {
			t.Fatal(err)
		}
		c := next.Completion
		if c == nil || c.CompletedBy != reviewer || c.ContentRevision != it.ContentRevision || c.CandidateCommit != commit1 ||
			len(c.AcceptedCriteria) != 1 || h.Completion == nil || h.Review == nil {
			t.Fatalf("completion = %+v history = %+v", c, h)
		}
	})

	t.Run("only_a_human_completes", func(t *testing.T) {
		it := inState(work.KindStory, work.StateReview)
		for _, a := range []work.Actor{
			{ID: "svc_pipeline", Type: work.ActorService}, {ID: "wl_attempt", Type: work.ActorWorkload}, {ID: "ops_1", Type: work.ActorOperator},
		} {
			env := goodEnv()
			if _, _, err := transition(it, env, doneRequest(it), a); !is(err, work.ErrNotHuman) {
				t.Errorf("%s completed: %v", a.Type, err)
			}
			if len(env.authorized) != 0 {
				t.Errorf("%s: authority consulted before the human check", a.Type)
			}
		}
	})

	t.Run("bound_to_revision_candidate_and_evidence", func(t *testing.T) {
		it := inState(work.KindStory, work.StateReview)
		req := doneRequest(it)
		req.Completion.ContentRevision = it.ContentRevision + 1
		if _, _, err := transition(it, goodEnv(), req, reviewer); !is(err, work.ErrStaleSubject) {
			t.Errorf("another revision: %v", err)
		}
		edited := it
		edited.ContentRevision++ // the specification changed after review
		if _, _, err := transition(edited, goodEnv(), doneRequest(edited), reviewer); !is(err, work.ErrStaleSubject) {
			t.Errorf("edit after review: %v", err)
		}
		req = doneRequest(it)
		req.Completion.CandidateCommit = "2222222222222222222222222222222222222222"
		if _, _, err := transition(it, goodEnv(), req, reviewer); !is(err, work.ErrStaleSubject) {
			t.Errorf("another candidate: %v", err)
		}
		env := goodEnv()
		env.subject = work.VerifiedSubject{CandidateCommit: "2222222222222222222222222222222222222222"}
		if _, _, err := transition(it, env, doneRequest(it), reviewer); !is(err, work.ErrStaleSubject) {
			t.Errorf("candidate no longer current: %v", err)
		}
		none := it
		none.ReviewSubject = nil
		if _, _, err := transition(none, goodEnv(), doneRequest(none), reviewer); !is(err, work.ErrPrerequisite) {
			t.Errorf("no recorded subject: %v", err)
		}
		if _, _, err := transition(it, goodEnv(), work.TransitionRequest{To: work.StateDone, Reason: "x"}, reviewer); !is(err, work.ErrPrerequisite) {
			t.Errorf("no completion claim: %v", err)
		}
	})

	t.Run("acceptance_names_every_criterion", func(t *testing.T) {
		it := inState(work.KindStory, work.StateReview)
		it.Criteria = append(it.Criteria, work.Criterion{ID: "AC-2", Text: "Audit exists."})
		req := doneRequest(it)
		if _, _, err := transition(it, goodEnv(), req, reviewer); !is(err, work.ErrPrerequisite) {
			t.Errorf("partial acceptance: %v", err)
		}
		req.Completion.AcceptedCriteria = []string{"AC-1", "AC-9"}
		if _, _, err := transition(it, goodEnv(), req, reviewer); !is(err, work.ErrInvalid) {
			t.Errorf("unknown criterion: %v", err)
		}
		req.Completion.AcceptedCriteria = []string{"AC-1", "AC-1"}
		if _, _, err := transition(it, goodEnv(), req, reviewer); !is(err, work.ErrPrerequisite) {
			t.Errorf("repeated criterion: %v", err)
		}
		req.Completion.AcceptedCriteria = []string{"AC-2", "AC-1"}
		if _, _, err := transition(it, goodEnv(), req, reviewer); err != nil {
			t.Errorf("full acceptance: %v", err)
		}
	})

	t.Run("findings_are_acknowledged_by_name_in_the_reason", func(t *testing.T) {
		it := inState(work.KindStory, work.StateReview)
		env := goodEnv()
		env.subject = work.VerifiedSubject{CandidateCommit: commit1, Evidence: it.ReviewSubject.Evidence,
			Findings: []work.Finding{{ID: "F-7", Summary: "slow export"}}}
		req := doneRequest(it)
		if _, _, err := transition(it, env, req, reviewer); !is(err, work.ErrPrerequisite) {
			t.Fatalf("unacknowledged finding: %v", err)
		}
		req.Completion.AcknowledgedFindings = []work.Finding{{ID: "F-7"}}
		req.Reason = "accepted despite findings"
		if _, _, err := transition(it, env, req, reviewer); !is(err, work.ErrPrerequisite) {
			t.Fatalf("reason does not name the finding: %v", err)
		}
		req.Reason = "waiving F-7: export speed is a follow-up"
		next, _, err := transition(it, env, req, reviewer)
		if err != nil || len(next.Completion.AcknowledgedFindings) != 1 {
			t.Fatalf("named acknowledgment: %v", err)
		}
		req.Completion.AcknowledgedFindings = []work.Finding{{ID: "F-7"}, {ID: "F-9"}}
		req.Reason = "F-7 and F-9"
		if _, _, err := transition(it, env, req, reviewer); !is(err, work.ErrInvalid) {
			t.Fatalf("acknowledging a finding that does not exist: %v", err)
		}
	})

	t.Run("epic_completion_binds_the_child_summary", func(t *testing.T) {
		it := inState(work.KindEpic, work.StateReview)
		req := work.TransitionRequest{To: work.StateDone, Reason: "ok", Completion: &work.CompletionClaim{ContentRevision: it.ContentRevision, AcceptedCriteria: []string{"AC-1"}}}
		if _, _, err := transition(it, goodEnv(), req, reviewer); err != nil {
			t.Fatalf("epic completion: %v", err)
		}
		env := goodEnv()
		env.children = work.ChildSummary{Total: 3, Done: 2}
		if _, _, err := transition(it, env, req, reviewer); !is(err, work.ErrStaleSubject) {
			t.Fatalf("children changed after review: %v", err)
		}
	})

	t.Run("reopening_clears_the_current_decision_but_keeps_the_revision", func(t *testing.T) {
		it := inState(work.KindStory, work.StateDone)
		next, h, err := transition(it, goodEnv(), work.TransitionRequest{To: work.StateActive, Reason: "regression found"}, engineer)
		if err != nil {
			t.Fatal(err)
		}
		if next.Completion != nil || next.ReviewSubject != nil || next.ContentRevision != it.ContentRevision || h.Reason != "regression found" {
			t.Fatalf("reopened item = %+v", next)
		}
		if it.Completion == nil {
			t.Fatal("the source item was mutated")
		}
	})
}
