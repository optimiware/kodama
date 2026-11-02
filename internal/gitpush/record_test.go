package gitpush_test

import (
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
)

func newRecord(states ...gitv1.EntryState) *gitpush.Record {
	r := &gitpush.Record{
		ID: "push_1", Scope: domain.Scope{TenantID: "t_1", ProjectID: "p_1"}, RepositoryID: repoID, ReceiveID: "rcv_1", GrantID: "g",
		Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"}, Deadline: time.Date(2026, 10, 30, 10, 15, 0, 0, time.UTC), Version: 1,
		CreatedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
	}
	for i, s := range states {
		e := gitpush.Entry{Ref: "refs/heads/b" + string(rune('a'+i)), Old: oid("1"), New: oid("2"), Kind: gitv1.KindFastForward, State: s}
		switch s {
		case gitv1.EntryRefused:
			e.Reason = gitv1.ReasonNoCommitIntent
		case gitv1.EntryUnknown:
			e.Reason = gitv1.ReasonEvidenceMissing
		}
		r.Entries = append(r.Entries, e)
	}
	return r
}

func Test_unit_gitpush_entry_transitions(t *testing.T) {
	all := []gitv1.EntryState{gitv1.EntryAuthorized, gitv1.EntryCommitting, gitv1.EntryApplied, gitv1.EntryRefused, gitv1.EntryUnknown}
	allowed := map[[2]gitv1.EntryState]bool{
		{gitv1.EntryAuthorized, gitv1.EntryCommitting}: true, {gitv1.EntryAuthorized, gitv1.EntryRefused}: true, {gitv1.EntryAuthorized, gitv1.EntryUnknown}: true,
		{gitv1.EntryCommitting, gitv1.EntryApplied}: true, {gitv1.EntryCommitting, gitv1.EntryRefused}: true, {gitv1.EntryCommitting, gitv1.EntryUnknown}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := gitpush.CanTransition(from, to); got != allowed[[2]gitv1.EntryState{from, to}] {
				t.Errorf("%s -> %s = %v", from, to, got)
			}
		}
	}
}

func Test_unit_gitpush_record_status_and_commit_intent(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 1, 0, 0, time.UTC)
	r := newRecord(gitv1.EntryAuthorized, gitv1.EntryAuthorized)
	if r.Status() != gitv1.PushAuthorized || !r.Unsettled() {
		t.Fatalf("status %s", r.Status())
	}
	upd := []gitv1.RefUpdate{{Ref: r.Entries[0].Ref, Old: oid("1"), New: oid("2")}}
	if changed, err := r.CommitIntent(upd, now); err != nil || !changed || r.Entries[0].State != gitv1.EntryCommitting || r.Version != 2 {
		t.Fatalf("intent: %v %v %+v", changed, err, r)
	}
	if changed, err := r.CommitIntent(upd, now); err != nil || changed || r.Version != 2 {
		t.Fatalf("a repeated intent changes nothing: %v %v", changed, err)
	}
	if _, err := r.CommitIntent(upd, r.Deadline); !gitpush.IsCode(err, gitv1.CodeReceiveExpired) {
		t.Fatalf("at the deadline: %v", err)
	}
	wrong := []gitv1.RefUpdate{{Ref: r.Entries[0].Ref, Old: oid("1"), New: oid("3")}}
	if _, err := r.CommitIntent(wrong, now); !gitpush.IsCode(err, domain.CodeForbidden) {
		t.Fatalf("another new value: %v", err)
	}
	// A settled entry refuses the whole request and changes nothing.
	r.Entries[1].State, r.Entries[1].Reason = gitv1.EntryRefused, gitv1.ReasonNoCommitIntent
	both := append(upd, gitv1.RefUpdate{Ref: r.Entries[1].Ref, Old: oid("1"), New: oid("2")})
	if _, err := r.CommitIntent(both, now); !gitpush.IsCode(err, gitv1.CodePushSettled) {
		t.Fatalf("settled entry: %v", err)
	}
	if r.Entries[0].State != gitv1.EntryCommitting {
		t.Fatal("a refused request changed an entry")
	}
}

func Test_unit_gitpush_record_resolve_guards(t *testing.T) {
	now := time.Now()
	r := newRecord(gitv1.EntryCommitting)
	ref := r.Entries[0].Ref
	// Evidence gathered for a state the entry has left is not applied.
	if ok, err := r.Resolve(ref, gitv1.EntryAuthorized, gitv1.Decision{Resolution: gitv1.ResolveApplied}, now); err != nil || ok {
		t.Fatalf("stale evidence: %v %v", ok, err)
	}
	if ok, err := r.Resolve(ref, gitv1.EntryCommitting, gitv1.Decision{Resolution: gitv1.ResolveWait, Reason: gitv1.ReasonReceiveRunning}, now); err != nil || ok {
		t.Fatalf("wait: %v %v", ok, err)
	}
	if ok, err := r.Resolve(ref, gitv1.EntryCommitting, gitv1.Decision{Resolution: gitv1.ResolveUnknown, Reason: gitv1.ReasonRefDiverged}, now); err != nil || !ok {
		t.Fatalf("unknown: %v %v", ok, err)
	}
	if r.Status() != gitv1.PushUnknown {
		t.Fatalf("status %s", r.Status())
	}
	// A final or unknown entry never moves again.
	if ok, err := r.Resolve(ref, gitv1.EntryUnknown, gitv1.Decision{Resolution: gitv1.ResolveApplied}, now); err == nil || ok {
		t.Fatalf("unknown -> applied must be refused: %v %v", ok, err)
	}
	if _, err := r.ApplyOutcome([]gitv1.RefUpdate{{Ref: ref, Old: oid("1"), New: oid("2")}}, now); err != nil || r.Entries[0].State != gitv1.EntryUnknown {
		t.Fatalf("an outcome never resolves an unknown entry: %v %+v", err, r.Entries[0])
	}
}

func Test_unit_gitpush_record_outcome_rules(t *testing.T) {
	now := time.Now()
	r := newRecord(gitv1.EntryCommitting, gitv1.EntryAuthorized, gitv1.EntryRefused, gitv1.EntryCommitting)
	r.RepositoryEmpty = true
	u := func(i int) gitv1.RefUpdate {
		return gitv1.RefUpdate{Ref: r.Entries[i].Ref, Old: oid("1"), New: oid("2")}
	}
	res, err := r.ApplyOutcome([]gitv1.RefUpdate{u(0), u(2)}, now)
	if err != nil || !res.Changed || len(res.Contradicted) != 1 || res.Contradicted[0] != r.Entries[2].Ref {
		t.Fatalf("outcome: %+v %v", res, err)
	}
	want := []gitv1.EntryState{gitv1.EntryApplied, gitv1.EntryRefused, gitv1.EntryRefused, gitv1.EntryRefused}
	for i, s := range want {
		if r.Entries[i].State != s {
			t.Fatalf("entry %d is %s, want %s", i, r.Entries[i].State, s)
		}
	}
	if r.Entries[1].Reason != gitv1.ReasonNoCommitIntent || r.Entries[3].Reason != gitv1.ReasonReceiveAborted || !r.EmptyReportPending || r.Status() != gitv1.PushApplied {
		t.Fatalf("record: %+v", r)
	}
	// Repeating the outcome changes no entry; the contradiction is recorded again.
	if res, err := r.ApplyOutcome([]gitv1.RefUpdate{u(0), u(2)}, now); err != nil || len(res.Contradicted) != 1 || r.Entries[2].State != gitv1.EntryRefused {
		t.Fatalf("repeat: %+v %v", res, err)
	}
	if res, err := r.ApplyOutcome([]gitv1.RefUpdate{u(0)}, now); err != nil || res.Changed {
		t.Fatalf("a repeat without a contradiction is a no-op: %+v %v", res, err)
	}
	dto, err := r.DTO()
	if err != nil || dto.Status != gitv1.PushApplied || dto.OriginRunID != nil {
		t.Fatalf("dto: %+v %v", dto, err)
	}
}
