package worktest

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// The scopes of the contract. Open seeds nothing; the contract seeds each scope
// it uses through Env.SeedProject.
var (
	ScopeA  = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_web"}
	ScopeA2 = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_api"}
	ScopeB  = domain.Scope{TenantID: "t088_globex", ProjectID: "p088_ops"}
)

// Env is the work module under contract: the use cases over the persistence
// adapter, plus the stand-ins of the peers the module consumes (identity's
// principals and grants, the event bus, the project) and the observation hooks
// the contract needs.
type Env interface {
	Service() *work.Service
	Repository() work.Repository
	Cancellations() work.CancellationSource
	// SeedProject creates the tenant (if new) and the project with its active
	// WIP limit.
	SeedProject(t testing.TB, scope domain.Scope, wip int)
	// Principal registers a principal with its actor type and grants.
	Principal(t testing.TB, tenant domain.TenantID, id string, typ work.ActorType, perms ...work.Permission)
	Revoke(t testing.TB, tenant domain.TenantID, id string, perm work.Permission)
	// FailEvents makes every event append fail with err after the event was
	// written in the mutation's transaction; nil restores it.
	FailEvents(err error)
	// EventIDs lists the tenant's outbox event IDs.
	EventIDs(t testing.TB, tenant domain.TenantID) []string
	// Admit runs the workspace/execution admission seam in its own transaction.
	Admit(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (workpg.Admission, error)
}

// Open builds an empty Env.
type Open func(testing.TB) Env

const (
	lead     = "usr_lead"     // human: creates, refines, works, cancels
	reviewer = "usr_reviewer" // human: completes and reopens
	bot      = "svc_bot"      // workload: holds the permission but is not a human
	reader   = "usr_reader"
)

var (
	leadPerms = []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermCancel}
	commit    = strings.Repeat("a", 40)
)

// Contract is the behavior every work.Repository adapter shares, exercised
// through the use cases: atomic item/history/audit/event writes, tenant and
// project scoping, expected-version comparison, ordered history and lists,
// transition prerequisites, WIP admission, parent rules, rollback on a failed
// event append, the cancelled-item scan and the admission seam. It asserts
// outcomes, not lock order or isolation levels.
func Contract(t *testing.T, open Open) {
	t.Helper()
	ctx := context.Background()

	fresh := func(t *testing.T, wip int) Env {
		t.Helper()
		env := open(t)
		for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
			env.SeedProject(t, s, wip)
		}
		for _, tn := range []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID} {
			env.Principal(t, tn, lead, work.ActorUser, leadPerms...)
			env.Principal(t, tn, reviewer, work.ActorUser, work.PermRead, work.PermComplete, work.PermReopen)
			env.Principal(t, tn, bot, work.ActorWorkload, work.PermRead, work.PermWork, work.PermComplete)
			env.Principal(t, tn, reader, work.ActorUser, work.PermRead)
		}
		return env
	}
	as := func(p string) work.Caller { return work.Caller{PrincipalID: p, RequestID: "req_t088"} }
	create := func(t *testing.T, env Env, scope domain.Scope, kind work.Kind, title string) work.Item {
		t.Helper()
		it, err := env.Service().Create(ctx, work.CreateCommand{Scope: scope, Caller: as(lead), Kind: kind, Title: title,
			Spec: "# Plan\nDo " + title, Criteria: []work.Criterion{{ID: "AC-1", Text: "It works."}}})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return it
	}
	move := func(env Env, scope domain.Scope, who string, it work.Item, req work.TransitionRequest) (work.Item, error) {
		return env.Service().Transition(ctx, work.TransitionCommand{Scope: scope, Caller: as(who), ID: it.ID, Expected: it.Version, Request: req})
	}
	mustMove := func(t *testing.T, env Env, scope domain.Scope, who string, it work.Item, to work.State, reason string) work.Item {
		t.Helper()
		next, err := move(env, scope, who, it, work.TransitionRequest{To: to, Reason: reason})
		if err != nil {
			t.Fatalf("%s -> %s: %v", it.State, to, err)
		}
		return next
	}
	get := func(t *testing.T, env Env, scope domain.Scope, id domain.WorkItemID) work.Item {
		t.Helper()
		it, err := env.Repository().Get(ctx, scope, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return it
	}
	history := func(t *testing.T, env Env, scope domain.Scope, id domain.WorkItemID) []work.HistoryEntry {
		t.Helper()
		h, err := env.Repository().History(ctx, scope, id, work.HistoryQuery{Limit: 100})
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		return h
	}

	t.Run("a created item commits with its history and event", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "first")
		if it.State != work.StateBacklog || it.Version != 1 || it.ContentRevision != 1 || it.Scope != ScopeA || it.CreatedBy.ID != lead || it.CreatedBy.Type != work.ActorUser {
			t.Fatalf("created item: %+v", it)
		}
		stored := get(t, env, ScopeA, it.ID)
		if stored.Title != "first" || stored.Spec != it.Spec || len(stored.Criteria) != 1 || stored.Criteria[0].ID != "AC-1" || stored.Version != 1 {
			t.Fatalf("stored item: %+v", stored)
		}
		h := history(t, env, ScopeA, it.ID)
		if len(h) != 1 || h[0].Kind != work.ChangeCreated || h[0].Version != 1 || h[0].To != work.StateBacklog || h[0].Actor.ID != lead || h[0].RequestID != "req_t088" {
			t.Fatalf("history: %+v", h)
		}
		if ids := env.EventIDs(t, ScopeA.TenantID); len(ids) != 1 || ids[0] != "evt_"+h[0].ID {
			t.Fatalf("events %v for history %s", ids, h[0].ID)
		}
		if ids := env.EventIDs(t, ScopeB.TenantID); len(ids) != 0 {
			t.Fatalf("another tenant holds the event: %v", ids)
		}
	})

	t.Run("a returned item is a copy of the committed row", func(t *testing.T) {
		env := fresh(t, 5)
		scribble := func(it *work.Item) {
			if len(it.Criteria) > 0 {
				it.Criteria[0].Text = "scribbled"
			}
			if len(it.EvidenceLinks) > 0 {
				it.EvidenceLinks[0].ID = "art_scribbled"
			}
			if rs := it.ReviewSubject; rs != nil {
				rs.CandidateCommit = strings.Repeat("f", 40)
				if len(rs.Evidence) > 0 {
					rs.Evidence[0].ID = "art_scribbled"
				}
			}
			if c := it.Completion; c != nil {
				c.AcceptedCriteria = []string{"scribbled"}
				c.CompletedBy.ID = "usr_scribbled"
			}
			it.Title, it.State = "scribbled", work.StateDone
		}
		it, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(lead), Kind: work.KindStory, Title: "copy",
			Spec: "# Plan", Criteria: []work.Criterion{{ID: "AC-1", Text: "It works."}},
			EvidenceLinks: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_t088_link"}}})
		if err != nil {
			t.Fatal(err)
		}
		check := func(stage string, want work.Item) {
			t.Helper()
			got := get(t, env, ScopeA, want.ID)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: the committed item changed through a returned value:\n got %+v\nwant %+v", stage, got, want)
			}
		}
		before := get(t, env, ScopeA, it.ID)
		hist0 := history(t, env, ScopeA, it.ID)
		events0 := len(env.EventIDs(t, ScopeA.TenantID))
		scribble(&it)
		check("create", before)

		cur := before
		step := func(who string, req work.TransitionRequest) {
			t.Helper()
			next, err := move(env, ScopeA, who, cur, req)
			if err != nil {
				t.Fatalf("%s: %v", req.To, err)
			}
			want := get(t, env, ScopeA, cur.ID)
			scribble(&next)
			check(string(req.To), want)
			cur = want
		}
		step(lead, work.TransitionRequest{To: work.StateReady})
		step(lead, work.TransitionRequest{To: work.StateActive})
		step(lead, work.TransitionRequest{To: work.StateReview, Review: &work.ReviewClaim{CandidateCommit: commit, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_t088"}}}})
		step(reviewer, work.TransitionRequest{To: work.StateDone, Reason: "accepted", Completion: &work.CompletionClaim{ContentRevision: 1, CandidateCommit: commit, AcceptedCriteria: []string{"AC-1"}}})
		if cur.ReviewSubject == nil || cur.Completion == nil || cur.ReviewSubject.CandidateCommit != commit || cur.Completion.AcceptedCriteria[0] != "AC-1" {
			t.Fatalf("the committed decision records changed: %+v", cur)
		}

		// Values returned by the reads are copies too, and history is unchanged.
		listed, err := env.Repository().List(ctx, ScopeA, work.ListQuery{Limit: 10})
		if err != nil || len(listed) != 1 {
			t.Fatalf("list: %v %v", listed, err)
		}
		scribble(&listed[0])
		fetched := get(t, env, ScopeA, cur.ID)
		scribble(&fetched)
		check("reads", cur)
		hist := history(t, env, ScopeA, it.ID)
		if len(hist) != len(hist0)+4 {
			t.Fatalf("history %d entries, want %d", len(hist), len(hist0)+4)
		}
		for i := range hist {
			for _, f := range []*work.HistoryEntry{&hist[i]} {
				if f.Review != nil {
					f.Review.CandidateCommit = "scribbled"
				}
				if f.Completion != nil {
					f.Completion.AcceptedCriteria = nil
				}
				f.ChangedFields = append(f.ChangedFields, "scribbled")
			}
		}
		again := history(t, env, ScopeA, it.ID)
		last := again[len(again)-1]
		if last.Completion == nil || len(last.Completion.AcceptedCriteria) != 1 || last.Review == nil || last.Review.CandidateCommit != commit {
			t.Fatalf("history changed through a returned value: %+v", last)
		}
		if n := len(env.EventIDs(t, ScopeA.TenantID)); n != events0+4 {
			t.Fatalf("events %d, want %d", n, events0+4)
		}
		if s := get(t, env, ScopeA, cur.ID); s.Version != cur.Version {
			t.Fatalf("version moved to %d", s.Version)
		}
	})

	t.Run("an item is visible in its own project only", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "scoped")
		for _, other := range []domain.Scope{ScopeA2, ScopeB} {
			if _, err := env.Repository().Get(ctx, other, it.ID); !errors.Is(err, work.ErrNotFound) {
				t.Fatalf("get from %v: %v", other, err)
			}
			if _, err := env.Repository().History(ctx, other, it.ID, work.HistoryQuery{Limit: 10}); !errors.Is(err, work.ErrNotFound) {
				t.Fatalf("history from %v: %v", other, err)
			}
			if items, err := env.Repository().List(ctx, other, work.ListQuery{Limit: 10}); err != nil || len(items) != 0 {
				t.Fatalf("list from %v: %v %v", other, items, err)
			}
			_, err := env.Repository().Update(ctx, other, it.ID, it.Version, work.UpdateOptions{}, func(context.Context, work.Tx, work.Item) (work.Item, work.HistoryEntry, error) {
				t.Fatal("an update function ran for an item of another scope")
				return work.Item{}, work.HistoryEntry{}, nil
			})
			if !errors.Is(err, work.ErrNotFound) {
				t.Fatalf("update from %v: %v", other, err)
			}
		}
		// Identity is per tenant: a principal of tenant A does not exist in B.
		if _, err := env.Service().Get(ctx, ScopeB, as("usr_unknown"), it.ID); !errors.Is(err, work.ErrHidden) {
			t.Fatalf("unknown principal: %v", err)
		}
		if _, err := env.Service().Get(ctx, ScopeB, as(lead), it.ID); !errors.Is(err, work.ErrNotFound) {
			t.Fatalf("tenant B reading tenant A's item: %v", err)
		}
		if _, err := env.Service().Get(ctx, ScopeA, as(reader), it.ID); err != nil {
			t.Fatalf("a reader could not read: %v", err)
		}
		if _, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(reader), Kind: work.KindTask, Title: "x", Spec: "s"}); !errors.Is(err, work.ErrDenied) {
			t.Fatalf("a reader created an item: %v", err)
		}
	})

	t.Run("edits compare versions and keep an ordered history", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "edit me")
		title := "renamed"
		v2, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: 1, Patch: work.Patch{Title: &title}})
		if err != nil || v2.Version != 2 || v2.ContentRevision != 1 || v2.Title != title {
			t.Fatalf("rename: %+v %v", v2, err)
		}
		spec := "# New plan"
		v3, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: 2, Patch: work.Patch{Spec: &spec}})
		if err != nil || v3.Version != 3 || v3.ContentRevision != 2 {
			t.Fatalf("respec: %+v %v", v3, err)
		}
		if _, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: 1, Patch: work.Patch{Title: &title}}); !errors.Is(err, work.ErrVersionConflict) {
			t.Fatalf("a stale version was accepted: %v", err)
		}
		if stored := get(t, env, ScopeA, it.ID); stored.Version != 3 || stored.Title != title {
			t.Fatalf("a rejected edit changed the item: %+v", stored)
		}
		// Repeating an edit that changes nothing writes nothing.
		same, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: 3, Patch: work.Patch{Title: &title}})
		if err != nil || same.Version != 3 {
			t.Fatalf("no-op edit: %+v %v", same, err)
		}
		h := history(t, env, ScopeA, it.ID)
		if len(h) != 3 || h[0].Version != 1 || h[1].Version != 2 || h[2].Version != 3 || h[2].ContentRevision != 2 || !h[2].ContentChanged() {
			t.Fatalf("history: %+v", h)
		}
		if h[1].Before[work.FieldTitle] != "edit me" || h[1].After[work.FieldTitle] != title {
			t.Fatalf("history values: %+v / %+v", h[1].Before, h[1].After)
		}
		page, err := env.Repository().History(ctx, ScopeA, it.ID, work.HistoryQuery{AfterVersion: 1, Limit: 1})
		if err != nil || len(page) != 1 || page[0].Version != 2 {
			t.Fatalf("history page: %+v %v", page, err)
		}
		if n := len(env.EventIDs(t, ScopeA.TenantID)); n != 3 {
			t.Fatalf("a no-op edit or a rejected edit wrote an event: %d events", n)
		}
	})

	t.Run("lists are filtered, ordered and paged", func(t *testing.T) {
		env := fresh(t, 5)
		var items []work.Item
		for _, title := range []string{"one", "two", "three", "four"} {
			items = append(items, create(t, env, ScopeA, work.KindStory, title))
		}
		epic := create(t, env, ScopeA, work.KindEpic, "group")
		other := create(t, env, ScopeA2, work.KindStory, "elsewhere")
		_ = other
		mustMove(t, env, ScopeA, lead, items[0], work.StateReady, "")

		all, err := env.Repository().List(ctx, ScopeA, work.ListQuery{Limit: 100})
		if err != nil || len(all) != 5 {
			t.Fatalf("list: %d %v", len(all), err)
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool {
			if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
				return all[i].CreatedAt.Before(all[j].CreatedAt)
			}
			return all[i].ID < all[j].ID
		}) {
			t.Fatalf("list is not ordered by (created_at, id): %+v", all)
		}
		first, err := env.Repository().List(ctx, ScopeA, work.ListQuery{Limit: 2})
		if err != nil || len(first) != 2 || first[0].ID != all[0].ID || first[1].ID != all[1].ID {
			t.Fatalf("first page: %+v %v", first, err)
		}
		rest, err := env.Repository().List(ctx, ScopeA, work.ListQuery{Limit: 10, After: &work.Position{CreatedAt: first[1].CreatedAt, ID: first[1].ID}})
		if err != nil || len(rest) != 3 || rest[0].ID != all[2].ID {
			t.Fatalf("continuation: %+v %v", rest, err)
		}
		ready, err := env.Repository().List(ctx, ScopeA, work.ListQuery{States: []work.State{work.StateReady}, Limit: 10})
		if err != nil || len(ready) != 1 || ready[0].ID != items[0].ID {
			t.Fatalf("ready filter: %+v %v", ready, err)
		}
		epics, err := env.Repository().List(ctx, ScopeA, work.ListQuery{Kinds: []work.Kind{work.KindEpic}, Limit: 10})
		if err != nil || len(epics) != 1 || epics[0].ID != epic.ID {
			t.Fatalf("epic filter: %+v %v", epics, err)
		}
		if _, err := env.Service().List(ctx, ScopeA, as(lead), work.ListQuery{Limit: 0}); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("an unbounded list was accepted: %v", err)
		}
	})

	t.Run("transitions need an allowed edge, a reason and the current permission", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "moves")
		ready := mustMove(t, env, ScopeA, lead, it, work.StateReady, "")
		if ready.Version != 2 || ready.State != work.StateReady {
			t.Fatalf("ready: %+v", ready)
		}
		if _, err := move(env, ScopeA, reviewer, ready, work.TransitionRequest{To: work.StateDone}); !errors.Is(err, work.ErrInvalidTransition) {
			t.Fatalf("ready -> done: %v", err)
		}
		if _, err := move(env, ScopeA, lead, ready, work.TransitionRequest{To: work.StateBacklog}); !errors.Is(err, work.ErrPrerequisite) {
			t.Fatalf("back to backlog without a reason: %v", err)
		}
		if _, err := move(env, ScopeA, reader, ready, work.TransitionRequest{To: work.StateActive}); !errors.Is(err, work.ErrDenied) {
			t.Fatalf("a reader started work: %v", err)
		}
		if _, err := move(env, ScopeA, bot, ready, work.TransitionRequest{To: work.StateDone}); !errors.Is(err, work.ErrNotHuman) {
			t.Fatalf("a workload completed work: %v", err)
		}
		// The permission is read again at the write: a grant revoked after the
		// request was authorized stops the transition.
		env.Revoke(t, ScopeA.TenantID, lead, work.PermWork)
		if _, err := move(env, ScopeA, lead, ready, work.TransitionRequest{To: work.StateActive}); !errors.Is(err, work.ErrDenied) {
			t.Fatalf("a revoked grant started work: %v", err)
		}
		if stored := get(t, env, ScopeA, it.ID); stored.State != work.StateReady || stored.Version != 2 {
			t.Fatalf("a refused transition changed the item: %+v", stored)
		}
		back := mustMove(t, env, ScopeA, lead, ready, work.StateBacklog, "needs refinement")
		h := history(t, env, ScopeA, it.ID)
		if last := h[len(h)-1]; back.Version != 3 || last.Kind != work.ChangeTransitioned || last.From != work.StateReady || last.To != work.StateBacklog || last.Reason != "needs refinement" {
			t.Fatalf("history: %+v", h)
		}
	})

	t.Run("active admission honors the project limit", func(t *testing.T) {
		env := fresh(t, 1)
		a, b := create(t, env, ScopeA, work.KindStory, "a"), create(t, env, ScopeA, work.KindStory, "b")
		a, b = mustMove(t, env, ScopeA, lead, a, work.StateReady, ""), mustMove(t, env, ScopeA, lead, b, work.StateReady, "")
		a = mustMove(t, env, ScopeA, lead, a, work.StateActive, "")
		if _, err := move(env, ScopeA, lead, b, work.TransitionRequest{To: work.StateActive}); !errors.Is(err, work.ErrWIPLimit) {
			t.Fatalf("second active item: %v", err)
		}
		// The limit is per project and per tenant.
		c := create(t, env, ScopeA2, work.KindStory, "c")
		c = mustMove(t, env, ScopeA2, lead, c, work.StateReady, "")
		mustMove(t, env, ScopeA2, lead, c, work.StateActive, "")
		d := create(t, env, ScopeB, work.KindStory, "d")
		d = mustMove(t, env, ScopeB, lead, d, work.StateReady, "")
		mustMove(t, env, ScopeB, lead, d, work.StateActive, "")
		// Cancelling the active item frees its slot.
		mustMove(t, env, ScopeA, lead, a, work.StateCancelled, "no longer needed")
		mustMove(t, env, ScopeA, lead, b, work.StateActive, "")
	})

	t.Run("review and completion are bound to the recorded subject", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "ship")
		it = mustMove(t, env, ScopeA, lead, it, work.StateReady, "")
		it = mustMove(t, env, ScopeA, lead, it, work.StateActive, "")
		if _, err := move(env, ScopeA, lead, it, work.TransitionRequest{To: work.StateReview}); !errors.Is(err, work.ErrPrerequisite) {
			t.Fatalf("review without a candidate: %v", err)
		}
		claim := &work.ReviewClaim{CandidateCommit: commit, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_t088"}}}
		review, err := move(env, ScopeA, lead, it, work.TransitionRequest{To: work.StateReview, Review: claim})
		if err != nil || review.ReviewSubject == nil || review.ReviewSubject.CandidateCommit != commit || len(review.ReviewSubject.Evidence) != 1 {
			t.Fatalf("review: %+v %v", review, err)
		}
		stored := get(t, env, ScopeA, it.ID)
		if stored.ReviewSubject == nil || stored.ReviewSubject.CandidateCommit != commit || stored.ReviewSubject.ContentRevision != 1 {
			t.Fatalf("stored review subject: %+v", stored.ReviewSubject)
		}
		stale := &work.CompletionClaim{ContentRevision: 7, CandidateCommit: commit, AcceptedCriteria: []string{"AC-1"}}
		if _, err := move(env, ScopeA, reviewer, review, work.TransitionRequest{To: work.StateDone, Completion: stale}); !errors.Is(err, work.ErrStaleSubject) {
			t.Fatalf("completion of an unobserved revision: %v", err)
		}
		partial := &work.CompletionClaim{ContentRevision: 1, CandidateCommit: commit, AcceptedCriteria: nil}
		if _, err := move(env, ScopeA, reviewer, review, work.TransitionRequest{To: work.StateDone, Completion: partial}); !errors.Is(err, work.ErrPrerequisite) {
			t.Fatalf("completion without accepting the criteria: %v", err)
		}
		ok := &work.CompletionClaim{ContentRevision: 1, CandidateCommit: commit, AcceptedCriteria: []string{"AC-1"}}
		done, err := move(env, ScopeA, reviewer, review, work.TransitionRequest{To: work.StateDone, Completion: ok, Reason: "accepted"})
		if err != nil || done.State != work.StateDone || done.Completion == nil || done.Completion.CompletedBy.ID != reviewer || done.Completion.CandidateCommit != commit {
			t.Fatalf("completion: %+v %v", done, err)
		}
		title := "late edit"
		if _, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: done.Version, Patch: work.Patch{Title: &title}}); !errors.Is(err, work.ErrInvalidTransition) {
			t.Fatalf("a done item was edited: %v", err)
		}
		h := history(t, env, ScopeA, it.ID)
		last := h[len(h)-1]
		if last.To != work.StateDone || last.Completion == nil || last.Review == nil || last.Review.CandidateCommit != commit {
			t.Fatalf("completion history: %+v", last)
		}
		reopened, err := move(env, ScopeA, reviewer, done, work.TransitionRequest{To: work.StateActive, Reason: "found a defect"})
		if err != nil || reopened.State != work.StateActive || reopened.Completion != nil {
			t.Fatalf("reopen: %+v %v", reopened, err)
		}
	})

	t.Run("a parent is an acyclic epic of the same project", func(t *testing.T) {
		env := fresh(t, 5)
		epic := create(t, env, ScopeA, work.KindEpic, "epic")
		story, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(lead), Kind: work.KindStory, Title: "child", Spec: "s", ParentID: epic.ID})
		if err != nil || story.ParentID != epic.ID {
			t.Fatalf("child: %+v %v", story, err)
		}
		if _, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(lead), Kind: work.KindStory, Title: "bad", Spec: "s", ParentID: story.ID}); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("a story as parent: %v", err)
		}
		foreign := create(t, env, ScopeA2, work.KindEpic, "foreign epic")
		if _, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(lead), Kind: work.KindStory, Title: "bad", Spec: "s", ParentID: foreign.ID}); !errors.Is(err, work.ErrNotFound) {
			t.Fatalf("an epic of another project as parent: %v", err)
		}
		inner := create(t, env, ScopeA, work.KindEpic, "inner")
		parent := work.Some(epic.ID)
		inner, err = env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: inner.ID, Expected: inner.Version, Patch: work.Patch{Parent: parent}})
		if err != nil || inner.ParentID != epic.ID {
			t.Fatalf("nest: %+v %v", inner, err)
		}
		cycle := work.Some(inner.ID)
		if _, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: epic.ID, Expected: epic.Version, Patch: work.Patch{Parent: cycle}}); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("a cycle was accepted: %v", err)
		}
		children, err := env.Repository().List(ctx, ScopeA, work.ListQuery{ParentID: epic.ID, Limit: 10})
		if err != nil || len(children) != 2 {
			t.Fatalf("children: %+v %v", children, err)
		}
	})

	t.Run("a failed event append rolls the whole mutation back", func(t *testing.T) {
		env := fresh(t, 5)
		it := create(t, env, ScopeA, work.KindStory, "atomic")
		events0, history0 := len(env.EventIDs(t, ScopeA.TenantID)), len(history(t, env, ScopeA, it.ID))

		env.FailEvents(events.ErrBacklogFull)
		if _, err := env.Service().Create(ctx, work.CreateCommand{Scope: ScopeA, Caller: as(lead), Kind: work.KindTask, Title: "never stored", Spec: "s"}); !errors.Is(err, work.ErrUnavailable) {
			t.Fatalf("create with a full event backlog: %v", err)
		}
		title := "never stored"
		if _, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: &title}}); !errors.Is(err, work.ErrUnavailable) {
			t.Fatalf("edit with a full event backlog: %v", err)
		}
		if _, err := move(env, ScopeA, lead, it, work.TransitionRequest{To: work.StateReady}); !errors.Is(err, work.ErrUnavailable) {
			t.Fatalf("transition with a full event backlog: %v", err)
		}
		items, _ := env.Repository().List(ctx, ScopeA, work.ListQuery{Limit: 10})
		stored := get(t, env, ScopeA, it.ID)
		if len(items) != 1 || stored.Version != it.Version || stored.State != work.StateBacklog || stored.Title != "atomic" ||
			len(history(t, env, ScopeA, it.ID)) != history0 || len(env.EventIDs(t, ScopeA.TenantID)) != events0 {
			t.Fatalf("partial write: %d items, %+v, history %d, events %d", len(items), stored, len(history(t, env, ScopeA, it.ID)), len(env.EventIDs(t, ScopeA.TenantID)))
		}
		// Nothing was replayed: once the bus accepts events the same request
		// applies exactly once.
		env.FailEvents(nil)
		next, err := env.Service().Edit(ctx, work.EditCommand{Scope: ScopeA, Caller: as(lead), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: &title}})
		if err != nil || next.Version != it.Version+1 {
			t.Fatalf("retry: %+v %v", next, err)
		}
		if len(history(t, env, ScopeA, it.ID)) != history0+1 || len(env.EventIDs(t, ScopeA.TenantID)) != events0+1 {
			t.Fatal("the retry did not write exactly one history entry and one event")
		}
	})

	t.Run("cancelled items are scanned per tenant after a cursor", func(t *testing.T) {
		env := fresh(t, 5)
		want := map[domain.WorkItemID]domain.Scope{}
		for i, scope := range []domain.Scope{ScopeA, ScopeA, ScopeA2} {
			it := create(t, env, scope, work.KindStory, "doomed"+string(rune('a'+i)))
			it = mustMove(t, env, scope, lead, it, work.StateCancelled, "dropped")
			want[it.ID] = scope
		}
		keep := create(t, env, ScopeA, work.KindStory, "kept")
		inB := create(t, env, ScopeB, work.KindStory, "other tenant")
		inB = mustMove(t, env, ScopeB, lead, inB, work.StateCancelled, "dropped")

		var got []work.CancelledItem
		cursor := work.CancelledCursor{}
		for {
			page, err := env.Cancellations().CancelledItems(ctx, ScopeA.TenantID, cursor, 2)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, page...)
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			cursor = work.CancelledCursor{ProjectID: last.Scope.ProjectID, ID: last.WorkItemID}
		}
		if len(got) != len(want) {
			t.Fatalf("scan found %d of %d cancelled items: %+v", len(got), len(want), got)
		}
		for i, c := range got {
			if want[c.WorkItemID] != c.Scope || c.Version != 2 || c.WorkItemID == keep.ID {
				t.Fatalf("unexpected cancelled item %+v", c)
			}
			if i > 0 {
				p := got[i-1]
				if p.Scope.ProjectID > c.Scope.ProjectID || (p.Scope.ProjectID == c.Scope.ProjectID && p.WorkItemID >= c.WorkItemID) {
					t.Fatalf("scan is not ordered by (project, id): %+v", got)
				}
			}
		}
		only, err := env.Cancellations().CancelledItems(ctx, ScopeB.TenantID, work.CancelledCursor{}, 10)
		if err != nil || len(only) != 1 || only[0].WorkItemID != inB.ID {
			t.Fatalf("tenant B's scan: %+v %v", only, err)
		}
		if _, err := env.Cancellations().CancelledItems(ctx, ScopeA.TenantID, work.CancelledCursor{}, 0); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("an unbounded scan was accepted: %v", err)
		}
	})

	t.Run("admission refuses epics, cancelled and foreign items", func(t *testing.T) {
		env := fresh(t, 5)
		story := create(t, env, ScopeA, work.KindStory, "work on me")
		epic := create(t, env, ScopeA, work.KindEpic, "no workspace")
		gone := mustMove(t, env, ScopeA, lead, create(t, env, ScopeA, work.KindTask, "gone"), work.StateCancelled, "dropped")

		a, err := env.Admit(ctx, ScopeA, story.ID)
		if err != nil || a.Kind != work.KindStory || a.State != work.StateBacklog || a.Version != 1 || a.ContentRevision != 1 || a.ID != story.ID {
			t.Fatalf("admission: %+v %v", a, err)
		}
		if _, err := env.Admit(ctx, ScopeA, epic.ID); !errors.Is(err, work.ErrNotActionable) {
			t.Fatalf("epic: %v", err)
		}
		if _, err := env.Admit(ctx, ScopeA, gone.ID); !errors.Is(err, work.ErrCancelled) {
			t.Fatalf("cancelled: %v", err)
		}
		for _, scope := range []domain.Scope{ScopeA2, ScopeB} {
			if _, err := env.Admit(ctx, scope, story.ID); !errors.Is(err, work.ErrNotFound) {
				t.Fatalf("foreign scope %v: %v", scope, err)
			}
		}
		if _, err := env.Admit(ctx, ScopeA, "wi_unknown"); !errors.Is(err, work.ErrNotFound) {
			t.Fatalf("unknown item: %v", err)
		}
	})
}
