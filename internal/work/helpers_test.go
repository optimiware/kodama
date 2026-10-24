package work_test

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

var (
	scope    = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	engineer = work.Actor{ID: "u_eng", Type: work.ActorUser}
	reviewer = work.Actor{ID: "u_rev", Type: work.ActorUser}
	now      = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
)

const commit1 = "1111111111111111111111111111111111111111"

// fakeEnv is a TransitionEnv whose answers a test sets; calls are recorded.
type fakeEnv struct {
	denied     map[work.Permission]error
	admitErr   error
	admitCalls int
	writerBusy bool
	writerErr  error
	depsErr    error
	subject    work.VerifiedSubject
	subjectErr error
	children   work.ChildSummary
	authorized []work.Permission
}

func (f *fakeEnv) Authorize(_ context.Context, _ work.Actor, _ domain.Scope, p work.Permission) error {
	f.authorized = append(f.authorized, p)
	return f.denied[p]
}
func (f *fakeEnv) AdmitActive(context.Context, work.Item) error { f.admitCalls++; return f.admitErr }
func (f *fakeEnv) WriterSettled(context.Context, work.Item) (bool, error) {
	return !f.writerBusy, f.writerErr
}
func (f *fakeEnv) DependenciesSatisfied(context.Context, work.Item) error { return f.depsErr }
func (f *fakeEnv) VerifySubject(_ context.Context, _ work.Item, c work.ReviewClaim) (work.VerifiedSubject, error) {
	if f.subjectErr != nil {
		return work.VerifiedSubject{}, f.subjectErr
	}
	if f.subject.CandidateCommit == "" {
		return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
	}
	return f.subject, nil
}
func (f *fakeEnv) ChildSummary(context.Context, work.Item) (work.ChildSummary, error) {
	return f.children, nil
}

func newItem(kind work.Kind) work.Item {
	it, _, err := work.NewItem(work.NewItemParams{
		Scope: scope, ID: "wi_1", Kind: kind, Title: "Resolve inventory exceptions",
		Spec: "# Plan\nDo it.", Criteria: []work.Criterion{{ID: "AC-1", Text: "A reviewer resolves an exception."}},
		Actor: engineer, Now: now, HistoryID: "wih_1",
	})
	if err != nil {
		panic(err)
	}
	return it
}

// inState returns an item forced into state with the evidence that state implies.
func inState(kind work.Kind, state work.State) work.Item {
	it := newItem(kind)
	it.State = state
	it.Version = 5
	switch state {
	case work.StateReview, work.StateDone:
		subj := &work.ReviewSubject{ContentRevision: it.ContentRevision, RecordedAt: now}
		if kind == work.KindEpic {
			subj.ChildSummary = &work.ChildSummary{Total: 2, Done: 2}
		} else {
			subj.CandidateCommit = commit1
			subj.Evidence = []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_1"}}
		}
		it.ReviewSubject = subj
		if state == work.StateDone {
			it.Completion = &work.CompletionRecord{CompletedBy: reviewer, ContentRevision: it.ContentRevision}
		}
	}
	return it
}

func transition(it work.Item, env work.TransitionEnv, req work.TransitionRequest, actor work.Actor) (work.Item, work.HistoryEntry, error) {
	return it.Transition(context.Background(), env, req, actor, now.Add(time.Minute), "wih_2", "req_1")
}

func is(err, target error) bool { return errors.Is(err, target) }
