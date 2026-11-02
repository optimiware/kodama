package gitpushtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
)

// Scenario is one named behavior every store adapter must show.
type Scenario struct {
	Name string
	Run  func(*testing.T, *Fixture)
}

// Scenarios are the shared Contract: the same code runs on the memory adapter
// (unit layer) and on PostgreSQL (adapter integration layer), so the memory
// adapter is held to its SQL twin (SD-13/SD-14).
func Scenarios() []Scenario {
	return []Scenario{
		{"authorize_records_before_update", AuthorizeRecordsBeforeUpdate},
		{"default_branch_denied_by_default", DefaultBranchDeniedByDefault},
		{"seed_consumed_once", SeedConsumedOnce},
		{"seed_single_ref_only", SeedSingleRefOnly},
		{"concurrent_seeds_one_wins", ConcurrentSeedsOneWins},
		{"unknown_seed_blocks_new_seed", UnknownSeedBlocksNewSeed},
		{"current_authority", CurrentAuthority},
		{"commit_intent_refused_after_settlement", CommitIntentRefusedAfterSettlement},
		{"commit_intent_fenced_against_cancellation", CommitIntentFencedAgainstCancellation},
		{"repository_change_fenced_at_authorization", RepositoryChangeFencedAtAuthorization},
		{"identical_tuples_attributed_by_receive", IdenticalTuplesAttributedByReceive},
		{"per_ref_settlement", PerRefSettlement},
		{"reconcile_from_ref_journal", ReconcileFromRefJournal},
		{"reconcile_consistent_snapshots", ReconcileConsistentSnapshots},
		{"reconcile_latches_regression", ReconcileLatchesRegression},
		{"commit_intent_deadline_rechecked_after_locks", CommitIntentDeadlineRecheckedAfterLocks},
		{"aba_settles_refused", ABASettlesRefused},
		{"outcome_emits_one_event", OutcomeEmitsOneEvent},
		{"unknown_blocks_writer", UnknownBlocksWriter},
		{"list_and_tenant_isolation", ListAndTenantIsolation},
		{"operator_resolves_unknown", OperatorResolvesUnknown},
		{"acknowledge_restore", AcknowledgeRestore},
		{"unsettled_on_ref", UnsettledOnRef},
		{"applied_on_ref", AppliedOnRef},
		{"authorize_rechecks_writer_after_commit", AuthorizeRechecksWriterAfterCommit},
	}
}

// Run runs one scenario by name on a fresh environment.
func Run(t *testing.T, open Open, name string) {
	t.Helper()
	for _, s := range Scenarios() {
		if s.Name == name {
			s.Run(t, New(t, open))
			return
		}
	}
	t.Fatalf("no scenario %q", name)
}

// Contract runs every scenario, each on a fresh environment.
func Contract(t *testing.T, open Open) {
	for _, s := range Scenarios() {
		t.Run(s.Name, func(t *testing.T) { s.Run(t, New(t, open)) })
	}
}

// ---- helpers ----

var bg = context.Background()

// same compares two values by their wire form.
func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func wantCode(t testing.TB, err error, code domain.ErrorCode) {
	t.Helper()
	if !gitpush.IsCode(err, code) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

// verdicts maps ref to its deny reason ("" for allow).
func verdicts(resp gitv1.PushAuthorizeResponse) map[string]string {
	out := map[string]string{}
	for _, v := range resp.Updates {
		if v.Verdict == gitv1.VerdictAllow {
			out[v.Ref] = ""
		} else {
			out[v.Ref] = *v.Reason
		}
	}
	return out
}

func wantVerdicts(t testing.TB, resp gitv1.PushAuthorizeResponse, want map[string]string) {
	t.Helper()
	got := verdicts(resp)
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}
	for ref, reason := range want {
		if g, ok := got[ref]; !ok || g != reason {
			t.Fatalf("verdicts = %v, want %v", got, want)
		}
	}
}

func wantState(t testing.TB, rec gitv1.PushRecord, ref string, state gitv1.EntryState, reason string) {
	t.Helper()
	if got, want := States(rec)[ref], state; got != want || Reasons(rec)[ref] != reason {
		t.Fatalf("%s is %s (%q), want %s (%q); record %+v", ref, got, Reasons(rec)[ref], state, reason, rec.Entries)
	}
}

func wantStatus(t testing.TB, rec gitv1.PushRecord, status gitv1.PushStatus) {
	t.Helper()
	if rec.Status != status {
		t.Fatalf("status = %s, want %s; entries %+v", rec.Status, status, rec.Entries)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("the record is not a valid PushRecord: %v", err)
	}
}

func (f *Fixture) mustReconcile(repo string) {
	f.T.Helper()
	if err := f.Reconcile(repo); err != nil {
		f.T.Fatalf("reconcile: %v", err)
	}
}

func (f *Fixture) mustIntent(pushID string, req gitv1.PushAuthorizeRequest, refs ...string) {
	f.T.Helper()
	if err := f.Intent(pushID, req, refs...); err != nil {
		f.T.Fatalf("commit intent: %v", err)
	}
}

func (f *Fixture) events(scope domain.Scope, repo string) []eventsv1.Envelope {
	return f.Env.Events(f.T, scope, repo)
}

func (f *Fixture) wantEvents(repo string, n int) []eventsv1.Envelope {
	f.T.Helper()
	evs := f.events(ScopeA(), repo)
	if len(evs) != n {
		f.T.Fatalf("%d git.refs_updated events, want %d", len(evs), n)
	}
	return evs
}

// authorized creates one authorized record of the engineer in RepoMain.
func (f *Fixture) authorized(grant, receive string, updates ...U) (gitv1.PushAuthorizeRequest, gitv1.PushAuthorizeResponse) {
	f.T.Helper()
	req := Request(RepoMain, grant, receive, false, updates...)
	resp := f.MustAuthorize(req)
	if !resp.Allowed() {
		f.T.Fatalf("not allowed: %+v", resp.Updates)
	}
	return req, resp
}

// ---- scenarios ----

// AuthorizeRecordsBeforeUpdate: an authorized receive has a durable record with
// one entry per ref and its persisted kind before anything changes; a repeated
// receive replays the stored response, a different request under the same
// receive conflicts, and the bound, tenancy and unknown objects are refused (GR-09,
// GR-A06, GR-A13).
func AuthorizeRecordsBeforeUpdate(t *testing.T, f *Fixture) {
	grant := f.Human(Engineer, RepoMain)
	req := Request(RepoMain, grant, "rcv_1", false,
		Advance(Branch("feature"), OID("1"), OID("2")), Create(Branch("created"), OID("3")),
		Rewrite(Branch("rewritten"), OID("4"), OID("5")), Delete(Branch("gone"), OID("6")))
	resp := f.MustAuthorize(req)
	if !resp.Allowed() || len(resp.Updates) != 4 {
		t.Fatalf("response: %+v", resp)
	}
	if deadline, err := resp.Deadline.Time(); err != nil || !deadline.Equal(f.Clock.Now().Add(15*time.Minute)) {
		t.Fatalf("deadline %s (%v)", resp.Deadline, err)
	}

	// The record is durable before the response and nothing is applied.
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(t, rec, gitv1.PushAuthorized)
	if rec.ReceiveID != "rcv_1" || rec.GrantID != grant || rec.RepositoryID != RepoMain || rec.OriginRunID != nil ||
		rec.Actor.Type != gitv1.ActorUser || rec.Actor.ID != string(Engineer) {
		t.Fatalf("record: %+v", rec)
	}
	kinds := map[string]gitv1.UpdateKind{}
	for _, e := range rec.Entries {
		kinds[e.Ref] = e.Kind
		if e.State != gitv1.EntryAuthorized || e.Reason != nil {
			t.Fatalf("entry %+v", e)
		}
	}
	if kinds[Branch("feature")] != gitv1.KindFastForward || kinds[Branch("created")] != gitv1.KindCreate ||
		kinds[Branch("rewritten")] != gitv1.KindRewrite || kinds[Branch("gone")] != gitv1.KindDelete {
		t.Fatalf("persisted kinds: %v", kinds)
	}
	f.wantEvents(RepoMain, 0)
	if got := f.Env.AuditActions(t, ScopeA(), resp.PushID); len(got) != 1 || got[0] != gitpush.AuditAuthorized {
		t.Fatalf("audit = %v", got)
	}

	// Replay: the identical request returns the same response and one record.
	again, err := f.Authorize(req)
	if err != nil || again.PushID != resp.PushID || !same(again, resp) {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, resp)
	}
	page, err := f.Svc.List(bg, "usr_reader", ScopeA(), RepoMain, gitpush.ListQuery{Limit: 10})
	if err != nil || len(page) != 1 {
		t.Fatalf("records = %d, %v", len(page), err)
	}
	// A different request under the same receive conflicts.
	other := Request(RepoMain, grant, "rcv_1", false, Create(Branch("other"), OID("7")))
	_, err = f.Authorize(other)
	wantCode(t, err, domain.CodeIdempotencyConflict)
	// The same receive of another repository is its own record.
	other2 := Request(RepoDirect, f.Human(Engineer, RepoDirect), "rcv_1", false, Create(Branch("other"), OID("7")))
	if r := f.MustAuthorize(other2); r.PushID == resp.PushID {
		t.Fatal("a receive is unique per repository, not per tenant")
	}

	// Tenancy and unknown objects: not_found, indistinguishable.
	_, err = f.Svc.Authorize(bg, TenantB, req)
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Authorize(Request("r130_nope", grant, "rcv_x", false, Create(Branch("x"), OID("1"))))
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Authorize(Request(RepoMain, "grant_nope", "rcv_y", false, Create(Branch("x"), OID("1"))))
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Authorize(Request(RepoOther, grant, "rcv_z", false, Create(Branch("x"), OID("1"))))
	wantCode(t, err, domain.CodeNotFound) // a grant never spans another project's repository

	// The installation's bound is an explicit refusal.
	f.Reconfigure(func(o *gitpush.Options) { o.MaxUpdates = 2 })
	_, err = f.Authorize(Request(RepoMain, grant, "rcv_big", false,
		Create(Branch("a"), OID("1")), Create(Branch("b"), OID("1")), Create(Branch("c"), OID("1"))))
	wantCode(t, err, gitv1.CodeTooManyUpdates)
	if _, err := f.Authorize(Request(RepoMain, grant, "rcv_ok", false, Create(Branch("a"), OID("1")), Create(Branch("b"), OID("1")))); err != nil {
		t.Fatalf("within the bound: %v", err)
	}
}

// DefaultBranchDeniedByDefault: the default branch advances only by an
// integration grant bound to the approved candidate, or by an engineer's
// fast-forward where the repository opted in; an omitted policy is deny, and a
// receive with one deny fails whole and is settled refused (GR-07, GR-19,
// GR-A05, GR-A08, GR-A15).
func DefaultBranchDeniedByDefault(t *testing.T, f *Fixture) {
	main := Branch("main")
	eng := f.Human(Engineer, RepoMain)
	req := Request(RepoMain, eng, "rcv_d1", false, Advance(main, OID("1"), OID("2")))
	resp := f.MustAuthorize(req)
	wantVerdicts(t, resp, map[string]string{main: gitv1.ReasonRefProtected})
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(t, rec, gitv1.PushRefused)
	wantState(t, rec, main, gitv1.EntryRefused, gitv1.ReasonRefProtected)
	f.wantEvents(RepoMain, 0)
	if got := f.Env.AuditActions(t, ScopeA(), resp.PushID); len(got) != 1 || got[0] != gitpush.AuditRefused {
		t.Fatalf("audit = %v", got)
	}
	// The refused record is final: no commit intent, replay returns the verdicts.
	wantCode(t, f.Intent(resp.PushID, req), gitv1.CodePushSettled)
	if again, err := f.Authorize(req); err != nil || !same(again, resp) {
		t.Fatalf("replay of a refused receive: %+v, %v", again, err)
	}

	// A viewer's credential records no update right at all.
	view := f.Human(Viewer, RepoMain)
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, view, "rcv_d2", false, Advance(main, OID("1"), OID("2")))), map[string]string{main: gitv1.ReasonGrantScope})

	// A workload naming the pattern still never updates the default branch.
	att := f.Attempt("att_d", "run_d", RepoMain, main)
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att, "rcv_d3", false, Advance(main, OID("1"), OID("2")))), map[string]string{main: gitv1.ReasonRefProtected})

	// Opted in: an engineer's fast-forward, nothing else.
	dir := f.Human(Engineer, RepoDirect)
	wantVerdicts(t, f.MustAuthorize(Request(RepoDirect, dir, "rcv_d4", false, Advance(main, OID("1"), OID("2")))), map[string]string{main: ""})
	wantVerdicts(t, f.MustAuthorize(Request(RepoDirect, dir, "rcv_d5", false, Rewrite(main, OID("1"), OID("2")))), map[string]string{main: gitv1.ReasonNotFastForward})
	wantVerdicts(t, f.MustAuthorize(Request(RepoDirect, dir, "rcv_d6", false, Delete(main, OID("1")))), map[string]string{main: gitv1.ReasonRefProtected})
	wantVerdicts(t, f.MustAuthorize(Request(RepoDirect, dir, "rcv_d7", false, Create(main, OID("1")))), map[string]string{main: gitv1.ReasonRefProtected})

	// Integration: exactly the approved update, as a fast-forward.
	c := gitv1.IntegrationConstraint{Ref: main, From: OID("2"), To: OID("3"), GateID: "gate_1"}
	integ := f.Integration("att_i", RepoMain, c)
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, integ, "rcv_i1", false, Advance(main, c.From, c.To))), map[string]string{main: ""})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, integ, "rcv_i2", false, Advance(main, c.From, OID("4")))), map[string]string{main: gitv1.ReasonIntegrationMismatch})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, integ, "rcv_i3", false, Advance(main, OID("5"), c.To))), map[string]string{main: gitv1.ReasonIntegrationMismatch})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, integ, "rcv_i4", false, Advance(Branch("release"), c.From, c.To))), map[string]string{Branch("release"): gitv1.ReasonIntegrationMismatch})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, integ, "rcv_i5", false, Rewrite(main, c.From, c.To))), map[string]string{main: gitv1.ReasonNotFastForward})

	// One deny fails the whole receive: the allowed ref is settled with it.
	mixed := Request(RepoMain, eng, "rcv_d8", false, Advance(Branch("feature"), OID("1"), OID("2")), Advance(main, OID("1"), OID("2")))
	mresp := f.MustAuthorize(mixed)
	wantVerdicts(t, mresp, map[string]string{Branch("feature"): "", main: gitv1.ReasonRefProtected})
	mrec := f.Record(RepoMain, mresp.PushID)
	wantStatus(t, mrec, gitv1.PushRefused)
	wantState(t, mrec, Branch("feature"), gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
	wantCode(t, f.Intent(mresp.PushID, mixed, Branch("feature")), gitv1.CodePushSettled)
}

func (f *Fixture) emptySeedRequest(grant, receive string, updates ...U) gitv1.PushAuthorizeRequest {
	return Request(RepoEmpty, grant, receive, true, updates...)
}

// SeedConsumedOnce: a seed is consumed when its receive is authorized, even
// while it is still settling; a replay of that receive is idempotent, any other
// receive with the grant is seed_consumed, and no new seed is issued while the
// first receive is unsettled (GR-18, GR-A14).
func SeedConsumedOnce(t *testing.T, f *Fixture) {
	grant, err := f.Seed(Engineer, RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}
	main := Branch("main")
	req := f.emptySeedRequest(grant, "rcv_s1", Create(main, OID("a")))
	resp := f.MustAuthorize(req)
	wantVerdicts(t, resp, map[string]string{main: ""})
	rec := f.Record(RepoEmpty, resp.PushID)
	wantStatus(t, rec, gitv1.PushAuthorized)
	if again, err := f.Authorize(req); err != nil || again.PushID != resp.PushID {
		t.Fatalf("replay of the consuming receive: %+v, %v", again, err)
	}

	second := f.MustAuthorize(f.emptySeedRequest(grant, "rcv_s2", Create(main, OID("b"))))
	wantVerdicts(t, second, map[string]string{main: gitv1.ReasonSeedConsumed})
	wantStatus(t, f.Record(RepoEmpty, second.PushID), gitv1.PushRefused)

	// The first receive is still settling: no new seed grant.
	_, err = f.Seed(Engineer, RepoEmpty)
	wantCode(t, err, gitv1.CodeSeedPending)
	if busy, err := f.Svc.UnsettledPush(bg, ScopeA(), RepoEmpty); err != nil || !busy {
		t.Fatalf("UnsettledPush = %v, %v", busy, err)
	}

	// Settled applied: the repository is no longer empty and is never seedable.
	f.mustIntent(resp.PushID, req)
	if _, err := f.Outcome(resp.PushID, req, main); err != nil {
		t.Fatal(err)
	}
	if f.Repos.IsEmpty(TenantA, RepoEmpty) {
		t.Fatal("the repository module was not told the repository is no longer empty")
	}
	_, err = f.Seed(Engineer, RepoEmpty)
	wantCode(t, err, gitv1.CodeRepositoryNotEmpty)
	// A consumed seed also refuses a repeated receive that claims emptiness.
	third := f.MustAuthorize(f.emptySeedRequest(grant, "rcv_s3", Create(main, OID("c"))))
	wantVerdicts(t, third, map[string]string{main: gitv1.ReasonSeedConsumed})
}

// SeedSingleRefOnly: a seed receive with any other ref is refused as a whole
// without consuming the grant; a seed of a repository that has refs is refused;
// a refused seed leaves the repository empty and a new grant can be requested
// (GR-18, GR-A14).
func SeedSingleRefOnly(t *testing.T, f *Fixture) {
	grant, err := f.Seed(Engineer, RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}
	main, tag := Branch("main"), gitv1.TagsPrefix+"v1"
	extra := f.emptySeedRequest(grant, "rcv_e1", Create(main, OID("a")), Create(tag, OID("a")))
	resp := f.MustAuthorize(extra)
	wantVerdicts(t, resp, map[string]string{main: gitv1.ReasonSeedOnly, tag: gitv1.ReasonSeedOnly})
	wantStatus(t, f.Record(RepoEmpty, resp.PushID), gitv1.PushRefused)

	other := f.MustAuthorize(f.emptySeedRequest(grant, "rcv_e2", Create(Branch("feature"), OID("a"))))
	wantVerdicts(t, other, map[string]string{Branch("feature"): gitv1.ReasonSeedOnly})

	// Not empty any more according to the worker: never seedable.
	full := f.MustAuthorize(Request(RepoEmpty, grant, "rcv_e3", false, Create(main, OID("a"))))
	wantVerdicts(t, full, map[string]string{main: gitv1.ReasonRefProtected})

	// None of the refused receives consumed the grant: the proper seed works.
	good := f.MustAuthorize(f.emptySeedRequest(grant, "rcv_e4", Create(main, OID("a"))))
	wantVerdicts(t, good, map[string]string{main: ""})

	// A seed that settles refused leaves the repository seedable by a new grant.
	f.mustReconcile(RepoEmpty) // no receive on the worker: the authorized entry is refused
	rec := f.Record(RepoEmpty, good.PushID)
	wantStatus(t, rec, gitv1.PushRefused)
	wantState(t, rec, main, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
	if _, err := f.Seed(Engineer, RepoEmpty); err != nil {
		t.Fatalf("a new seed after a refused one: %v", err)
	}
	if !f.Repos.IsEmpty(TenantA, RepoEmpty) {
		t.Fatal("a refused seed changed the repository")
	}
}

// ConcurrentSeedsOneWins: concurrent seed receives with one grant authorize
// exactly one; the others are seed_consumed (GR-18, GR-A14).
func ConcurrentSeedsOneWins(t *testing.T, f *Fixture) {
	grant, err := f.Seed(Engineer, RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	main := Branch("main")
	results := make([]gitv1.PushAuthorizeResponse, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.Authorize(f.emptySeedRequest(grant, fmt.Sprintf("rcv_c%d", i), Create(main, OID("a"))))
		}()
	}
	wg.Wait()
	allowed := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("receive %d: %v", i, errs[i])
		}
		switch got := verdicts(results[i])[main]; got {
		case "":
			allowed++
		case gitv1.ReasonSeedConsumed:
		default:
			t.Fatalf("receive %d: verdict %q", i, got)
		}
	}
	if allowed != 1 {
		t.Fatalf("%d seed receives authorized, want exactly 1", allowed)
	}
	page, err := f.Svc.List(bg, "usr_reader", ScopeA(), RepoEmpty, gitpush.ListQuery{Limit: 100})
	if err != nil || len(page) != n {
		t.Fatalf("records = %d, %v", len(page), err)
	}
	open := 0
	for _, p := range page {
		if p.Record.Status == gitv1.PushAuthorized {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("%d unsettled records, want 1", open)
	}
}

// UnknownSeedBlocksNewSeed: a seed that settles unknown keeps the repository
// from getting another seed grant until it is resolved (GR-18, GR-A14).
func UnknownSeedBlocksNewSeed(t *testing.T, f *Fixture) {
	grant, err := f.Seed(Engineer, RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}
	main := Branch("main")
	req := f.emptySeedRequest(grant, "rcv_u1", Create(main, OID("a")))
	resp := f.MustAuthorize(req)
	f.mustIntent(resp.PushID, req)
	// The receive ended, but the journal holds nothing for it: no evidence.
	f.mustReconcile(RepoEmpty)
	rec := f.Record(RepoEmpty, resp.PushID)
	wantStatus(t, rec, gitv1.PushUnknown)
	wantState(t, rec, main, gitv1.EntryUnknown, gitv1.ReasonEvidenceMissing)
	_, err = f.Seed(Engineer, RepoEmpty)
	wantCode(t, err, gitv1.CodeSeedPending)
	// Reconciling again does not guess.
	f.mustReconcile(RepoEmpty)
	wantStatus(t, f.Record(RepoEmpty, resp.PushID), gitv1.PushUnknown)
	if f.Repos.IsEmpty(TenantA, RepoEmpty) == false {
		t.Fatal("an unknown seed must not report the repository as seeded")
	}
	f.wantEvents(RepoEmpty, 0)
}

// CurrentAuthority: every receive is authorized against the current roles, the
// attempt's liveness, the grant's validity, the repository's state and the
// writer reservation, not what was true at issuance (GR-06, GR-A04, WS-03).
func CurrentAuthority(t *testing.T, f *Fixture) {
	feature := Branch("feature")
	eng := f.Human(Engineer, RepoMain)
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, eng, "rcv_a1", false, Advance(feature, OID("1"), OID("2")))), map[string]string{feature: ""})

	// The person is demoted: the unexpired credential no longer updates anything.
	f.Roles.Set(TenantA, ProjectA, Engineer, "viewer")
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, eng, "rcv_a2", false, Advance(feature, OID("1"), OID("2")))), map[string]string{feature: gitv1.ReasonGrantScope})
	// ... or loses every role: the credential is no longer valid.
	f.Roles.Remove(TenantA, ProjectA, Engineer)
	_, err := f.Authorize(Request(RepoMain, eng, "rcv_a3", false, Advance(feature, OID("1"), OID("2"))))
	wantCode(t, err, domain.CodeInvalidCredential)
	f.Roles.Set(TenantA, ProjectA, Engineer, "engineer")

	// A cancelled attempt can no longer push; a live one pushes its run's branches.
	run := "run_a"
	agent := Agent("wi_1", run, "work")
	att := f.Attempt("att_a", run, RepoMain)
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att, "rcv_a4", false, Create(agent, OID("1")))), map[string]string{agent: ""})
	f.Live(ScopeA(), "att_a", false)
	_, err = f.Authorize(Request(RepoMain, att, "rcv_a5", false, Create(agent, OID("1"))))
	wantCode(t, err, domain.CodeInvalidCredential)

	// A revoked and an expired grant.
	revoked := f.Human(Engineer, RepoDirect)
	if err := f.Auth.Revoke(bg, TenantA, revoked); err != nil {
		t.Fatal(err)
	}
	_, err = f.Authorize(Request(RepoDirect, revoked, "rcv_a6", false, Advance(feature, OID("1"), OID("2"))))
	wantCode(t, err, domain.CodeInvalidCredential)
	expiring := f.Human(Engineer, RepoMain)
	f.Clock.Advance(2 * time.Hour)
	_, err = f.Authorize(Request(RepoMain, expiring, "rcv_a7", false, Advance(feature, OID("1"), OID("2"))))
	wantCode(t, err, domain.CodeInvalidCredential)

	// Repository state: archived refuses every update, others are explicit errors.
	eng = f.Human(Engineer, RepoMain)
	f.SetStatus(TenantA, RepoMain, gitv1.StatusArchived)
	resp := f.MustAuthorize(Request(RepoMain, eng, "rcv_a8", false, Advance(feature, OID("1"), OID("2")), Create(Branch("n"), OID("1"))))
	wantVerdicts(t, resp, map[string]string{feature: gitv1.ReasonRepositoryArchived, Branch("n"): gitv1.ReasonRepositoryArchived})
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushRefused)
	f.SetStatus(TenantA, RepoMain, gitv1.StatusReady)
	// A grant is only issued for a servable repository; the state may change after.
	booting, external := f.Human(Engineer, RepoBooting), f.Human(Engineer, RepoExternal)
	f.SetStatus(TenantA, RepoBooting, gitv1.StatusProvisioning)
	_, err = f.Authorize(Request(RepoBooting, booting, "rcv_a9", false, Create(feature, OID("1"))))
	wantCode(t, err, gitv1.CodeRepositoryNotReady)
	f.Repos.Update(TenantA, RepoExternal, func(r *Repository) { r.Facts.Hosting = gitv1.HostingExternal })
	_, err = f.Authorize(Request(RepoExternal, external, "rcv_a10", false, Create(feature, OID("1"))))
	wantCode(t, err, gitv1.CodeHostingMismatch)

	// Writer reservation (WS-03): the attempt updates the task branch only while
	// it holds the work item's reservation, and only by fast-forward.
	task := Task("wi_2")
	att2 := f.Attempt("att_b", "run_b", RepoMain, task)
	update := Request(RepoMain, att2, "rcv_w1", false, Create(task, OID("1")))
	wantVerdicts(t, f.MustAuthorize(update), map[string]string{task: gitv1.ReasonRefProtected})
	f.Reservations.Hold(ScopeA(), "wi_2", "att_b")
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att2, "rcv_w2", false, Create(task, OID("1")))), map[string]string{task: ""})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att2, "rcv_w3", false, Advance(task, OID("1"), OID("2")))), map[string]string{task: ""})
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att2, "rcv_w4", false, Rewrite(task, OID("1"), OID("2")))), map[string]string{task: gitv1.ReasonNotFastForward})
	f.Reservations.Release(ScopeA(), "wi_2", "att_b")
	wantVerdicts(t, f.MustAuthorize(Request(RepoMain, att2, "rcv_w5", false, Advance(task, OID("1"), OID("2")))), map[string]string{task: gitv1.ReasonRefProtected})
	// An engineer rewrites it, recorded as a history rewrite.
	rw := Request(RepoMain, f.Human(Engineer, RepoMain), "rcv_w6", false, Rewrite(task, OID("1"), OID("2")))
	rresp := f.MustAuthorize(rw)
	wantVerdicts(t, rresp, map[string]string{task: ""})
	if k := f.Record(RepoMain, rresp.PushID).Entries[0].Kind; k != gitv1.KindRewrite {
		t.Fatalf("kind = %s", k)
	}
}

// CommitIntentRefusedAfterSettlement: a receive delayed past the settlement of
// its authorized record is refused commit intent and changes nothing; so is one
// past its deadline or with a no longer live attempt; a repeated intent while the
// conditions hold succeeds (GR-09, GR-A13).
func CommitIntentRefusedAfterSettlement(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	feature := Branch("feature")

	req, resp := f.authorized(eng, "rcv_i1", Advance(feature, OID("1"), OID("2")))
	f.mustReconcile(RepoMain) // the worker has never heard of the receive: refused
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(t, rec, gitv1.PushRefused)
	wantState(t, rec, feature, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
	wantCode(t, f.Intent(resp.PushID, req), gitv1.CodePushSettled)

	// While it runs, the intent is accepted, and repeated.
	req, resp = f.authorized(eng, "rcv_i2", Advance(feature, OID("1"), OID("2")), Create(Branch("n"), OID("3")))
	f.mustIntent(resp.PushID, req, feature)
	f.mustIntent(resp.PushID, req, feature)
	f.mustIntent(resp.PushID, req)
	rec = f.Record(RepoMain, resp.PushID)
	wantState(t, rec, feature, gitv1.EntryCommitting, "")
	wantState(t, rec, Branch("n"), gitv1.EntryCommitting, "")
	// An update the record never authorized is forbidden; another receive's ID and
	// another tenant see not_found.
	bad := Request(RepoMain, eng, "rcv_i2", false, Advance(feature, OID("1"), OID("9")))
	wantCode(t, f.Intent(resp.PushID, bad), domain.CodeForbidden)
	_, err := f.Svc.CommitIntent(bg, TenantA, resp.PushID, gitv1.CommitIntentRequest{ReceiveID: "rcv_other", Updates: Updates(req)})
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.CommitIntent(bg, TenantB, resp.PushID, gitv1.CommitIntentRequest{ReceiveID: req.ReceiveID, Updates: Updates(req)})
	wantCode(t, err, domain.CodeNotFound)

	// An applied record is settled too.
	oreq, oresp := f.authorized(eng, "rcv_i5", Create(Branch("o"), OID("1")))
	f.mustIntent(oresp.PushID, oreq)
	if _, err := f.Outcome(oresp.PushID, oreq, Branch("o")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Intent(oresp.PushID, oreq), gitv1.CodePushSettled)

	// The deadline: new intents and repeated ones are refused afterwards.
	dreq, dresp := f.authorized(eng, "rcv_i3", Advance(Branch("late"), OID("1"), OID("2")))
	f.Clock.Advance(16 * time.Minute)
	wantCode(t, f.Intent(dresp.PushID, dreq), gitv1.CodeReceiveExpired)
	wantCode(t, f.Intent(resp.PushID, req), gitv1.CodeReceiveExpired)
	f.Worker.SetRunning(TenantA, RepoMain, "rcv_i3", true) // an overrunning receive is still refused and settled
	f.mustReconcile(RepoMain)
	wantState(t, f.Record(RepoMain, dresp.PushID), Branch("late"), gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)

	// A cancelled attempt is refused commit intent.
	run := "run_i"
	agent := Agent("wi_1", run, "work")
	att := f.Attempt("att_i", run, RepoMain)
	areq := Request(RepoMain, att, "rcv_i4", false, Create(agent, OID("1")))
	aresp := f.MustAuthorize(areq)
	f.Live(ScopeA(), "att_i", false)
	wantCode(t, f.Intent(aresp.PushID, areq), gitv1.CodeAttemptInactive)
	f.Live(ScopeA(), "att_i", true)
	f.mustIntent(aresp.PushID, areq)
	f.Live(ScopeA(), "att_i", false)
	wantCode(t, f.Intent(aresp.PushID, areq), gitv1.CodeAttemptInactive) // a repeat needs a live attempt too
}

// CommitIntentFencedAgainstCancellation: the attempt's cancellation (its grants
// are revoked in the cancelling transaction) and a commit intent are serialized
// by the grant row inside the intent's transaction: a cancellation that commits
// after the liveness read but before the intent is refused it, one that commits
// after the intent lets that transaction complete (GR-06, GR-09, GR-A04).
func CommitIntentFencedAgainstCancellation(t *testing.T, f *Fixture) {
	run, agent := "run_f", Agent("wi_1", "run_f", "work")
	att := f.Attempt("att_f", run, RepoMain)
	cancel := func(attempt string) {
		if err := f.Auth.RevokeAttempt(bg, ScopeA(), domain.AttemptID(attempt)); err != nil {
			t.Error(err)
		}
		f.Live(ScopeA(), attempt, false)
	}

	// Cancelled between the liveness read and the transaction: refused, and the
	// record is unchanged.
	req := Request(RepoMain, att, "rcv_f1", false, Create(agent, OID("1")))
	resp := f.MustAuthorize(req)
	f.AfterLiveness = func() { f.AfterLiveness = nil; cancel("att_f") }
	wantCode(t, f.Intent(resp.PushID, req), gitv1.CodeAttemptInactive)
	wantState(t, f.Record(RepoMain, resp.PushID), agent, gitv1.EntryAuthorized, "")

	// Cancelled after the intent: the transaction that obtained it completes and
	// is recorded.
	run2 := "run_g"
	agent2 := Agent("wi_1", run2, "work")
	att = f.Attempt("att_g", run2, RepoMain)
	req = Request(RepoMain, att, "rcv_f2", false, Create(agent2, OID("1")))
	resp = f.MustAuthorize(req)
	f.mustIntent(resp.PushID, req)
	cancel("att_g")
	if _, err := f.Outcome(resp.PushID, req, agent2); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushApplied)
	f.wantEvents(RepoMain, 1)
	// Nothing new is accepted afterwards.
	req = Request(RepoMain, att, "rcv_f3", false, Create(Agent("wi_1", run2, "more"), OID("1")))
	_, err := f.Authorize(req)
	wantCode(t, err, domain.CodeInvalidCredential)
}

// RepositoryChangeFencedAtAuthorization: the repository facts a receive is
// evaluated under are re-read in the authorization transaction; an archive, a
// new default branch or a disabled direct-push policy that lands between the
// read and the record refuses the receive as retryable, creates no record, and
// the retry evaluates the fresh facts. A new commit intent is not given on an
// archived repository (GR-07, GR-14, GR-19).
func RepositoryChangeFencedAtAuthorization(t *testing.T, f *Fixture) {
	main, feature := Branch("main"), Branch("feature")
	count := func(repo string) int {
		page, err := f.Svc.List(bg, "usr_reader", ScopeA(), repo, gitpush.ListQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(page)
	}
	changeOnce := func(repo string, mutate func(*gitpush.RepositoryFacts)) {
		f.Repos.AfterRead = func() { f.Repos.AfterRead = nil; f.ChangeRepository(TenantA, repo, mutate) }
	}
	retryable := func(err error) {
		t.Helper()
		if !errors.Is(err, gitpush.ErrUnavailable) {
			t.Fatalf("error = %v, want a retryable refusal", err)
		}
	}

	// Direct push disabled between the read and the record.
	dir := f.Human(Engineer, RepoDirect)
	req := Request(RepoDirect, dir, "rcv_r1", false, Advance(main, OID("1"), OID("2")))
	changeOnce(RepoDirect, func(r *gitpush.RepositoryFacts) { r.DirectPushToDefault = false })
	_, err := f.Authorize(req)
	retryable(err)
	if count(RepoDirect) != 0 {
		t.Fatal("a refused authorization left a record")
	}
	wantVerdicts(t, f.MustAuthorize(req), map[string]string{main: gitv1.ReasonRefProtected})

	// Archived between the read and the record.
	eng := f.Human(Engineer, RepoMain)
	req = Request(RepoMain, eng, "rcv_r2", false, Advance(feature, OID("1"), OID("2")))
	changeOnce(RepoMain, func(r *gitpush.RepositoryFacts) { r.Status = gitv1.StatusArchived })
	_, err = f.Authorize(req)
	retryable(err)
	if count(RepoMain) != 0 {
		t.Fatal("a refused authorization left a record")
	}
	wantVerdicts(t, f.MustAuthorize(req), map[string]string{feature: gitv1.ReasonRepositoryArchived})
	f.SetStatus(TenantA, RepoMain, gitv1.StatusReady)

	// A new default branch between the read and the record.
	req = Request(RepoMain, eng, "rcv_r3", false, Advance(main, OID("1"), OID("2")))
	changeOnce(RepoMain, func(r *gitpush.RepositoryFacts) { r.DefaultBranch = "trunk" })
	_, err = f.Authorize(req)
	retryable(err)
	// main is an ordinary branch of the repository now.
	wantVerdicts(t, f.MustAuthorize(req), map[string]string{main: ""})
	f.ChangeRepository(TenantA, RepoMain, func(r *gitpush.RepositoryFacts) { r.DefaultBranch = "main" })

	// An unchanged repository is not refused by an irrelevant version bump.
	f.Repos.Update(TenantA, RepoMain, func(r *Repository) { r.Empty = false })
	if _, err := f.Authorize(Request(RepoMain, eng, "rcv_r4", false, Create(Branch("n"), OID("1")))); err != nil {
		t.Fatal(err)
	}

	// A new commit intent is not given on an archived repository; an intent that
	// was given completes.
	req1, r1 := f.authorized(eng, "rcv_r5", Create(Branch("a"), OID("1")))
	req2, r2 := f.authorized(eng, "rcv_r6", Create(Branch("b"), OID("1")))
	f.mustIntent(r2.PushID, req2)
	f.SetStatus(TenantA, RepoMain, gitv1.StatusArchived)
	wantCode(t, f.Intent(r1.PushID, req1), gitv1.CodeRepositoryArchived)
	wantState(t, f.Record(RepoMain, r1.PushID), Branch("a"), gitv1.EntryAuthorized, "")
	f.mustIntent(r2.PushID, req2) // a repeat is not a new intent
	if _, err := f.Outcome(r2.PushID, req2, Branch("b")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.Record(RepoMain, r2.PushID), gitv1.PushApplied)
	f.SetStatus(TenantA, RepoMain, gitv1.StatusReady)
	f.mustIntent(r1.PushID, req1)
}

// CommitIntentDeadlineRecheckedAfterLocks: a commit intent that waits for a lock
// held by another transaction and wakes up after the receive deadline is
// refused, even though the lock was released unchanged: the deadline is decided
// last, with every lock held, and the entries stay as they were (GR-09, GR-A13).
func CommitIntentDeadlineRecheckedAfterLocks(t *testing.T, f *Fixture) {
	att := f.Attempt("att_d", "run_d", RepoMain)
	agent := Agent("wi_1", "run_d", "work")
	req := Request(RepoMain, att, "rcv_dl1", false, Create(agent, OID("1")))
	resp := f.MustAuthorize(req)

	release := f.Env.HoldGrantLock(t, ScopeA(), att)
	done := make(chan error, 1)
	go func() { done <- f.Intent(resp.PushID, req) }()
	select {
	case err := <-done:
		t.Fatalf("the commit intent did not wait for the lock (returned %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	f.Clock.Advance(16 * time.Minute) // the wait crossed the 15 minute deadline
	release()
	wantCode(t, <-done, gitv1.CodeReceiveExpired)
	wantState(t, f.Record(RepoMain, resp.PushID), agent, gitv1.EntryAuthorized, "")
}

// IdenticalTuplesAttributedByReceive: two receives with the same (ref, old, new)
// are two records; the journal attributes each transaction by receive ID, so
// one applies and the other, whose transaction aborted, is refused (GR-09,
// GR-A13).
func IdenticalTuplesAttributedByReceive(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	ref, a, b := Branch("feature"), OID("a"), OID("b")
	req1, r1 := f.authorized(eng, "rcv_t1", Advance(ref, a, b))
	req2, r2 := f.authorized(eng, "rcv_t2", Advance(ref, a, b))
	if r1.PushID == r2.PushID {
		t.Fatal("identical tuples are separate records")
	}
	f.mustIntent(r1.PushID, req1)
	f.mustIntent(r2.PushID, req2)
	// R2's transaction ran first and aborted; R1's ran later and committed.
	w := f.Worker
	w.Prepared(TenantA, RepoMain, "rcv_t2", ref, a, b, a)
	w.Aborted(TenantA, RepoMain, "rcv_t2", ref, a, b)
	w.Prepared(TenantA, RepoMain, "rcv_t1", ref, a, b, a)
	w.Committed(TenantA, RepoMain, "rcv_t1", ref, a, b)
	w.SetRef(TenantA, RepoMain, ref, b)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, r1.PushID), gitv1.PushApplied)
	rec2 := f.Record(RepoMain, r2.PushID)
	wantStatus(t, rec2, gitv1.PushRefused)
	wantState(t, rec2, ref, gitv1.EntryRefused, gitv1.ReasonReceiveAborted)
	// Exactly one event: the ref's value alone (b for both) never settled R2.
	evs := f.wantEvents(RepoMain, 1)
	if evs[0].Resource.ID != f.Record(RepoMain, r1.PushID).ReceiveID || evs[0].Resource.Type != eventsv1.ResourceTypeGitPush {
		t.Fatalf("event: %+v", evs[0])
	}

	// A transaction that only prepared is decided by the next prepared entry's
	// observed value, whichever receive wrote it.
	req3, r3 := f.authorized(eng, "rcv_t3", Advance(Branch("other"), a, b))
	req4, r4 := f.authorized(eng, "rcv_t4", Advance(Branch("other"), a, b))
	f.mustIntent(r3.PushID, req3)
	f.mustIntent(r4.PushID, req4)
	other := Branch("other")
	w.Prepared(TenantA, RepoMain, "rcv_t3", other, a, b, a) // crashed after prepared
	w.Prepared(TenantA, RepoMain, "rcv_t4", other, a, b, a) // the next transaction observed the old value again
	w.Committed(TenantA, RepoMain, "rcv_t4", other, a, b)
	w.SetRef(TenantA, RepoMain, other, b)
	f.mustReconcile(RepoMain)
	rec3 := f.Record(RepoMain, r3.PushID)
	wantState(t, rec3, other, gitv1.EntryRefused, gitv1.ReasonRefUnchanged)
	wantStatus(t, f.Record(RepoMain, r4.PushID), gitv1.PushApplied)
	f.wantEvents(RepoMain, 2)
}

// PerRefSettlement: each ref settles on its own; a non-atomic receive applies
// some refs and refuses others, the record is applied with one event counting
// the applied updates (GR-09, GR-10, GR-A06).
func PerRefSettlement(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	r1, r2, r3 := Branch("one"), Branch("two"), Branch("three")
	req, resp := f.authorized(eng, "rcv_p1", Advance(r1, OID("1"), OID("2")), Create(r2, OID("3")), Rewrite(r3, OID("4"), OID("5")))

	// Only two refs ever reached commit intent; the third is refused by the outcome.
	f.mustIntent(resp.PushID, req, r1, r2)
	out, err := f.Outcome(resp.PushID, req, r1, r2)
	if err != nil || out.Status != gitv1.PushApplied || out.PushID != resp.PushID {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	rec := f.Record(RepoMain, resp.PushID)
	wantState(t, rec, r1, gitv1.EntryApplied, "")
	wantState(t, rec, r2, gitv1.EntryApplied, "")
	wantState(t, rec, r3, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
	ev := f.wantEvents(RepoMain, 1)[0]
	data, err := eventsv1.DecodeData(ev)
	if err != nil {
		t.Fatal(err)
	}
	got := data.(*eventsv1.GitRefsUpdated)
	if len(got.Updates) != 2 || got.Updates[0].Kind != eventsv1.GitKindFastForward || got.Updates[1].Kind != eventsv1.GitKindCreate {
		t.Fatalf("event payload: %+v", got)
	}
	// The rewrite that did not apply is not audited as one.
	for _, a := range f.Env.AuditActions(t, ScopeA(), resp.PushID) {
		if a == gitpush.AuditHistoryRewrite {
			t.Fatal("an unapplied rewrite was audited as applied")
		}
	}

	// A worker that reports a settled-refused entry applied contradicts the fence:
	// the entry stays refused, nothing is emitted and the contradiction is audited.
	if _, err := f.Outcome(resp.PushID, req, r1, r3); err != nil {
		t.Fatal(err)
	}
	wantState(t, f.Record(RepoMain, resp.PushID), r3, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
	f.wantEvents(RepoMain, 1)
	conflict := false
	for _, a := range f.Env.AuditActions(t, ScopeA(), resp.PushID) {
		conflict = conflict || a == gitpush.AuditOutcomeConflict
	}
	if !conflict {
		t.Fatal("the contradiction was not audited")
	}

	// post-receive lists exactly the refs Git updated: a transaction that reached
	// committing and is not listed did not change its ref and is refused with the
	// outcome, even if the journal's committed hook ran for it (Git runs it for a
	// rejected update of a batched transaction). The record settles applied and
	// the event is emitted by the outcome.
	req, resp = f.authorized(eng, "rcv_p2", Create(Branch("x"), OID("1")), Create(Branch("y"), OID("1")))
	f.mustIntent(resp.PushID, req)
	f.Worker.Prepared(TenantA, RepoMain, "rcv_p2", Branch("x"), Zero, OID("1"), Zero)
	f.Worker.Prepared(TenantA, RepoMain, "rcv_p2", Branch("y"), Zero, OID("1"), Zero)
	f.Worker.Committed(TenantA, RepoMain, "rcv_p2", Branch("x"), Zero, OID("1"))
	f.Worker.Committed(TenantA, RepoMain, "rcv_p2", Branch("y"), Zero, OID("1"))
	f.Worker.SetRef(TenantA, RepoMain, Branch("x"), OID("1"))
	out, err = f.Outcome(resp.PushID, req, Branch("x"))
	if err != nil || out.Status != gitv1.PushApplied {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	rec = f.Record(RepoMain, resp.PushID)
	wantState(t, rec, Branch("y"), gitv1.EntryRefused, gitv1.ReasonReceiveAborted)
	f.wantEvents(RepoMain, 2)

	// The same receive with the confirmation lost: the journal's committed entry
	// of the rejected ref is contradicted by the ref itself, so it is unknown
	// (never applied, never refused on a guess), and the applied ref settles.
	req, resp = f.authorized(eng, "rcv_p2b", Create(Branch("x2"), OID("1")), Create(Branch("y2"), OID("1")))
	f.mustIntent(resp.PushID, req)
	for _, ref := range []string{Branch("x2"), Branch("y2")} {
		f.Worker.Prepared(TenantA, RepoMain, "rcv_p2b", ref, Zero, OID("1"), Zero)
	}
	for _, ref := range []string{Branch("x2"), Branch("y2")} {
		f.Worker.Committed(TenantA, RepoMain, "rcv_p2b", ref, Zero, OID("1"))
	}
	f.Worker.SetRef(TenantA, RepoMain, Branch("x2"), OID("1"))
	f.mustReconcile(RepoMain)
	rec = f.Record(RepoMain, resp.PushID)
	wantState(t, rec, Branch("x2"), gitv1.EntryApplied, "")
	wantState(t, rec, Branch("y2"), gitv1.EntryUnknown, gitv1.ReasonConflictingEvidence)
	wantStatus(t, rec, gitv1.PushUnknown)
	f.wantEvents(RepoMain, 2)

	// A transaction that aborted and a post-receive that lists nothing: refused
	// as a whole; no event.
	req, resp = f.authorized(eng, "rcv_p3", Create(Branch("z"), OID("1")))
	f.mustIntent(resp.PushID, req)
	f.Worker.Prepared(TenantA, RepoMain, "rcv_p3", Branch("z"), Zero, OID("1"), Zero)
	f.Worker.Aborted(TenantA, RepoMain, "rcv_p3", Branch("z"), Zero, OID("1"))
	if out, err = f.Outcome(resp.PushID, req); err != nil || out.Status != gitv1.PushRefused {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	wantState(t, f.Record(RepoMain, resp.PushID), Branch("z"), gitv1.EntryRefused, gitv1.ReasonReceiveAborted)
	f.wantEvents(RepoMain, 2)

	// With no post-receive at all the journal's own aborted entry refuses it.
	req, resp = f.authorized(eng, "rcv_p3b", Create(Branch("z2"), OID("1")))
	f.mustIntent(resp.PushID, req)
	f.Worker.Prepared(TenantA, RepoMain, "rcv_p3b", Branch("z2"), Zero, OID("1"), Zero)
	f.Worker.Aborted(TenantA, RepoMain, "rcv_p3b", Branch("z2"), Zero, OID("1"))
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushRefused)
	f.wantEvents(RepoMain, 2)

	// An outcome naming an update the record does not hold changes nothing.
	req, resp = f.authorized(eng, "rcv_p4", Create(Branch("w"), OID("1")))
	f.mustIntent(resp.PushID, req)
	_, err = f.Svc.Outcome(bg, TenantA, resp.PushID, gitv1.OutcomeRequest{ReceiveID: req.ReceiveID, Applied: []gitv1.RefUpdate{{Ref: Branch("w"), Old: Zero, New: OID("2")}}})
	wantCode(t, err, domain.CodeForbidden)
	wantState(t, f.Record(RepoMain, resp.PushID), Branch("w"), gitv1.EntryCommitting, "")
	// Applied without commit intent contradicts the fence: unknown, never applied.
	req, resp = f.authorized(eng, "rcv_p5", Create(Branch("v"), OID("1")))
	if out, err = f.Outcome(resp.PushID, req, Branch("v")); err != nil || out.Status != gitv1.PushUnknown {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	wantState(t, f.Record(RepoMain, resp.PushID), Branch("v"), gitv1.EntryUnknown, gitv1.ReasonConflictingEvidence)
}

// ReconcileFromRefJournal: a lost post-receive confirmation is settled from the
// worker's ref journal; waiting, unreadable evidence and a restored older
// volume never produce a guess (GR-09, GR-A06, GR-A13).
func ReconcileFromRefJournal(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	w := f.Worker
	a, b, c := OID("a"), OID("b"), OID("c")

	// Lost post-receive, own committed entry: applied, one event, repeatable.
	ref := Branch("lost")
	req, resp := f.authorized(eng, "rcv_r1", Advance(ref, a, b))
	f.mustIntent(resp.PushID, req)
	w.SetRunning(TenantA, RepoMain, "rcv_r1", true)
	w.Prepared(TenantA, RepoMain, "rcv_r1", ref, a, b, a)
	w.Committed(TenantA, RepoMain, "rcv_r1", ref, a, b)
	w.SetRef(TenantA, RepoMain, ref, b)
	f.mustReconcile(RepoMain) // still running: wait
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushAuthorized)
	w.SetRunning(TenantA, RepoMain, "rcv_r1", false)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushApplied)
	f.mustReconcile(RepoMain)
	f.wantEvents(RepoMain, 1)

	// Only prepared: the next prepared entry's observed value decides (here a
	// server operation's), never the ref's own value.
	ref2 := Branch("next")
	req, resp = f.authorized(eng, "rcv_r2", Advance(ref2, a, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_r2", ref2, a, b, a)
	op := "op_1"
	w.append(TenantA, RepoMain, gitv1.JournalEntry{OperationID: &op, Ref: ref2, Old: b, New: c, Observed: &b, State: gitv1.JournalPrepared})
	w.SetRef(TenantA, RepoMain, ref2, a) // a misleading current value: ignored
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushApplied)

	// Only prepared, no later entry: the value under the lock, once quiescent.
	ref3 := Branch("current")
	req, resp = f.authorized(eng, "rcv_r3", Advance(ref3, a, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_r3", ref3, a, b, a)
	w.SetRef(TenantA, RepoMain, ref3, a)
	w.SetQuiescent(TenantA, RepoMain, false)
	f.mustReconcile(RepoMain)
	wantState(t, f.Record(RepoMain, resp.PushID), ref3, gitv1.EntryCommitting, "") // wait
	w.SetQuiescent(TenantA, RepoMain, true)
	f.mustReconcile(RepoMain)
	wantState(t, f.Record(RepoMain, resp.PushID), ref3, gitv1.EntryRefused, gitv1.ReasonRefUnchanged)
	ref4 := Branch("current_new")
	req, resp = f.authorized(eng, "rcv_r4", Advance(ref4, a, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_r4", ref4, a, b, a)
	w.SetRef(TenantA, RepoMain, ref4, b)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushApplied)
	// A value that is neither old nor new is not guessed.
	ref5 := Branch("diverged")
	req, resp = f.authorized(eng, "rcv_r5", Advance(ref5, a, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_r5", ref5, a, b, a)
	w.SetRef(TenantA, RepoMain, ref5, c)
	f.mustReconcile(RepoMain)
	wantState(t, f.Record(RepoMain, resp.PushID), ref5, gitv1.EntryUnknown, gitv1.ReasonRefDiverged)
	events := len(f.events(ScopeA(), RepoMain))

	// A volume restored from an older snapshot: its journal head is below the one
	// seen before, so even an own committed entry settles nothing (GR-A13).
	repo := RepoDirect
	grant := f.Human(Engineer, repo)
	for i := range 4 { // the control plane sees a journal of head 8
		w.Prepared(TenantA, repo, fmt.Sprintf("srv_%d", i), Branch("filler"), a, b, a)
		w.Committed(TenantA, repo, fmt.Sprintf("srv_%d", i), Branch("filler"), a, b)
	}
	rreq := Request(repo, grant, "rcv_r7", false, Advance(Branch("restored"), a, b))
	rresp := f.MustAuthorize(rreq)
	f.mustIntent(rresp.PushID, rreq)
	w.Prepared(TenantA, repo, "rcv_r7", Branch("restored"), a, b, a)
	w.Committed(TenantA, repo, "rcv_r7", Branch("restored"), a, b)
	w.SetRef(TenantA, repo, Branch("restored"), b)
	// First pass sees head 10 for a record that is applied by its own entry.
	f.mustReconcile(repo)
	wantStatus(t, f.Record(repo, rresp.PushID), gitv1.PushApplied)
	// The volume is replaced by an older snapshot that holds a later receive's
	// entries but a shorter journal.
	w.Wipe(TenantA, repo)
	qreq := Request(repo, grant, "rcv_r8", false, Advance(Branch("after_restore"), a, b))
	qresp := f.MustAuthorize(qreq)
	f.mustIntent(qresp.PushID, qreq)
	w.Prepared(TenantA, repo, "rcv_r8", Branch("after_restore"), a, b, a)
	w.Committed(TenantA, repo, "rcv_r8", Branch("after_restore"), a, b)
	f.mustReconcile(repo)
	qrec := f.Record(repo, qresp.PushID)
	wantStatus(t, qrec, gitv1.PushUnknown)
	wantState(t, qrec, Branch("after_restore"), gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)

	// A worker failure leaves the entry unchanged and is reported for another pass.
	ref6 := Branch("outage")
	req, resp = f.authorized(eng, "rcv_r6", Advance(ref6, a, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_r6", ref6, a, b, a)
	w.Committed(TenantA, RepoMain, "rcv_r6", ref6, a, b)
	w.SetRef(TenantA, RepoMain, ref6, b)
	w.Fail = errors.New("worker unreachable")
	if err := f.Reconcile(RepoMain); err == nil {
		t.Fatal("a worker outage must be reported")
	}
	w.Fail = nil
	wantState(t, f.Record(RepoMain, resp.PushID), ref6, gitv1.EntryCommitting, "")
	// A journal the worker affirms unreadable waits for the evidence timeout, and
	// only that answer then settles unknown with a visible reason. A transport
	// failure never settles, however long it lasts.
	w.Journal = fmt.Errorf("%w: damaged", gitpush.ErrJournalUnreadable)
	if err := f.Reconcile(RepoMain); err == nil {
		t.Fatal("an unreadable journal must be reported")
	}
	wantState(t, f.Record(RepoMain, resp.PushID), ref6, gitv1.EntryCommitting, "")
	f.Clock.Advance(15*time.Minute + 11*time.Minute)
	w.Journal = errors.New("connection reset")
	if err := f.Reconcile(RepoMain); err == nil {
		t.Fatal("a transport failure must be reported")
	}
	wantState(t, f.Record(RepoMain, resp.PushID), ref6, gitv1.EntryCommitting, "")
	w.Journal = fmt.Errorf("%w: damaged", gitpush.ErrJournalUnreadable)
	_ = f.Reconcile(RepoMain)
	wantState(t, f.Record(RepoMain, resp.PushID), ref6, gitv1.EntryUnknown, gitv1.ReasonJournalUnreadable)
	w.Journal = nil
	f.mustReconcile(RepoMain) // unknown stays unknown: an operator resolves it
	wantState(t, f.Record(RepoMain, resp.PushID), ref6, gitv1.EntryUnknown, gitv1.ReasonJournalUnreadable)
	if got := len(f.events(ScopeA(), RepoMain)); got != events {
		t.Fatalf("%d events, want %d: unknown records emit nothing", got, events)
	}
}

// ReconcileConsistentSnapshots: a settlement never combines journal and ref
// snapshots from different moments. When another receive completes B to A
// between the journal read and the value read, the entry is decided by the newer
// prepared entry, not by the value that now equals its old one; a volume
// restored between the reads, or between the journal reads, is a regression
// (unknown), never a settlement (GR-09, GR-A13).
func ReconcileConsistentSnapshots(t *testing.T, f *Fixture) {
	w := f.Worker
	a, b := OID("a"), OID("b")
	filler := func(repo string, pairs int) {
		for i := range pairs {
			id := fmt.Sprintf("srv_%s_%d", repo, i)
			w.Prepared(TenantA, repo, id, Branch("filler"), a, b, a)
			w.Committed(TenantA, repo, id, Branch("filler"), a, b)
		}
	}
	prepared := func(repo string, receive string, ref string, grant string) gitv1.PushRecord {
		f.T.Helper()
		req := Request(repo, grant, receive, false, Advance(ref, a, b))
		resp := f.MustAuthorize(req)
		f.mustIntent(resp.PushID, req)
		return f.Record(repo, resp.PushID)
	}

	// Interleaved ABA between the two reads: A to B committed (its committed entry
	// is lost with the confirmation), then B to A completed before the value was read.
	ref := Branch("interleaved")
	rec := prepared(RepoMain, "rcv_c1", ref, f.Human(Engineer, RepoMain))
	w.Prepared(TenantA, RepoMain, "rcv_c1", ref, a, b, a)
	w.SetRef(TenantA, RepoMain, ref, b)
	w.Hook = func(op string, _ int) {
		if op != "values" {
			return
		}
		w.Hook = nil
		w.Prepared(TenantA, RepoMain, "rcv_other", ref, b, a, b)
		w.Committed(TenantA, RepoMain, "rcv_other", ref, b, a)
		w.SetRef(TenantA, RepoMain, ref, a)
	}
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, rec.PushID), gitv1.PushApplied)
	f.wantEvents(RepoMain, 1)

	// A restore between the journal read and the value read.
	ref2 := Branch("restore_values")
	filler(RepoDirect, 4)
	rec = prepared(RepoDirect, "rcv_c2", ref2, f.Human(Engineer, RepoDirect))
	w.Prepared(TenantA, RepoDirect, "rcv_c2", ref2, a, b, a)
	w.SetRef(TenantA, RepoDirect, ref2, a)
	w.Hook = func(op string, _ int) {
		if op == "values" {
			w.Hook = nil
			w.Restore(TenantA, RepoDirect, 5)
		}
	}
	f.mustReconcile(RepoDirect)
	got := f.Record(RepoDirect, rec.PushID)
	wantStatus(t, got, gitv1.PushUnknown)
	wantState(t, got, ref2, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)

	// A restore between the receive's journal read and the ref's journal read.
	ref3 := Branch("restore_journal")
	filler(RepoEmpty, 2)
	rec = prepared(RepoEmpty, "rcv_c3", ref3, f.Human(Engineer, RepoEmpty))
	w.Prepared(TenantA, RepoEmpty, "rcv_c3", ref3, a, b, a)
	w.Committed(TenantA, RepoEmpty, "rcv_c3", ref3, a, b)
	filler(RepoEmpty, 2)
	reads := 0
	w.Hook = func(op string, _ int) {
		if op != "journal" {
			return
		}
		if reads++; reads == 2 {
			w.Hook = nil
			w.Restore(TenantA, RepoEmpty, 5) // keeps the receive's entries, loses the later head
		}
	}
	f.mustReconcile(RepoEmpty)
	got = f.Record(RepoEmpty, rec.PushID)
	wantStatus(t, got, gitv1.PushUnknown)
	wantState(t, got, ref3, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	f.wantEvents(RepoEmpty, 0)
}

// ReconcileLatchesRegression: every head a worker reports counts at once and a
// regression is latched for the whole settlement. A journal that reads lower
// than the persisted head at first and catches up on the next read, a head that
// drops between the pages of one read, and a head seen only on a non-quiescent
// value snapshot all make the entries unknown (journal_regressed), whatever the
// later reads show (GR-09, GR-A13).
func ReconcileLatchesRegression(t *testing.T, f *Fixture) {
	w := f.Worker
	a, b := OID("a"), OID("b")
	pairs := func(repo, prefix string, n int) {
		for i := range n {
			id := fmt.Sprintf("srv_%s_%d", prefix, i)
			w.Prepared(TenantA, repo, id, Branch("filler"), a, b, a)
			w.Committed(TenantA, repo, id, Branch("filler"), a, b)
		}
	}
	counter := func(op string, onCall func(n int)) {
		n := 0
		w.Hook = func(got string, _ int) {
			if got != op {
				return
			}
			n++
			onCall(n)
		}
	}
	intent := func(repo, grant, receive string, us ...U) gitv1.PushRecord {
		t.Helper()
		req := Request(repo, grant, receive, false, us...)
		resp := f.MustAuthorize(req)
		f.mustIntent(resp.PushID, req)
		return f.Record(repo, resp.PushID)
	}

	// The first read is behind the persisted head and the second catches up.
	repo := RepoMain
	eng := f.Human(Engineer, repo)
	pairs(repo, "m1", 40) // 80 entries
	w.Prepared(TenantA, repo, "rcv_l1", Branch("r1"), a, b, a)
	w.Committed(TenantA, repo, "rcv_l1", Branch("r1"), a, b)
	pairs(repo, "m2", 8) // 98 entries
	w.Prepared(TenantA, repo, "rcv_l0", Branch("r0"), a, b, a)
	w.Committed(TenantA, repo, "rcv_l0", Branch("r0"), a, b)
	w.SetRef(TenantA, repo, Branch("r0"), b)
	w.SetRef(TenantA, repo, Branch("r1"), b)
	r0 := intent(repo, eng, "rcv_l0", Advance(Branch("r0"), a, b))
	f.mustReconcile(repo) // the control plane has now seen head 100
	wantStatus(t, f.Record(repo, r0.PushID), gitv1.PushApplied)
	r1 := intent(repo, eng, "rcv_l1", Advance(Branch("r1"), a, b))
	counter("journal", func(n int) {
		switch n {
		case 1:
			w.Restore(TenantA, repo, 90) // the receive's journal is read from an older volume
		case 2:
			pairs(repo, "m3", 5) // and the ref's journal from one that caught up to head 100
		}
	})
	f.mustReconcile(repo)
	w.Hook = nil
	got := f.Record(repo, r1.PushID)
	wantStatus(t, got, gitv1.PushUnknown)
	wantState(t, got, Branch("r1"), gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)

	// The head drops between the pages of one read.
	repo = RepoDirect
	eng = f.Human(Engineer, repo)
	w.PageLimit = 2
	w.Prepared(TenantA, repo, "rcv_l2", Branch("p1"), a, b, a)
	w.Prepared(TenantA, repo, "rcv_l2", Branch("p2"), a, b, a)
	w.Committed(TenantA, repo, "rcv_l2", Branch("p1"), a, b)
	w.Committed(TenantA, repo, "rcv_l2", Branch("p2"), a, b)
	pairs(repo, "d1", 2)
	w.SetRef(TenantA, repo, Branch("p1"), b)
	w.SetRef(TenantA, repo, Branch("p2"), b)
	r2 := intent(repo, eng, "rcv_l2", Advance(Branch("p1"), a, b), Advance(Branch("p2"), a, b))
	counter("journal", func(n int) {
		if n == 2 {
			w.Restore(TenantA, repo, 6) // the second page comes from an older volume
		}
	})
	f.mustReconcile(repo)
	w.Hook, w.PageLimit = nil, 0
	got = f.Record(repo, r2.PushID)
	wantStatus(t, got, gitv1.PushUnknown)
	wantState(t, got, Branch("p1"), gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	wantState(t, got, Branch("p2"), gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)

	// A head seen only on a value snapshot that was not quiescent.
	repo = RepoEmpty
	eng = f.Human(Engineer, repo)
	x, y := Branch("x"), Branch("y")
	w.Prepared(TenantA, repo, "rcv_l3", y, a, b, a)
	w.Committed(TenantA, repo, "rcv_l3", y, a, b)
	w.Prepared(TenantA, repo, "rcv_l3", x, a, b, a)
	w.SetRef(TenantA, repo, y, b)
	w.SetRef(TenantA, repo, x, a)
	r3 := intent(repo, eng, "rcv_l3", Advance(x, a, b), Advance(y, a, b))
	counter("journal", func(n int) {
		if n == 3 { // the journal read of the second entry
			w.Restore(TenantA, repo, 5)
		}
	})
	valuesHook := w.Hook
	w.Hook = func(op string, n int) {
		if op == "values" {
			pairs(repo, "e1", 2) // head 7
			w.SetQuiescent(TenantA, repo, false)
			return
		}
		valuesHook(op, n)
	}
	f.mustReconcile(repo)
	w.Hook = nil
	got = f.Record(repo, r3.PushID)
	// The regression taints the whole settlement: the entry that was still
	// waiting is unknown too, not left to settle once the journal catches up.
	wantStatus(t, got, gitv1.PushUnknown)
	wantState(t, got, x, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	wantState(t, got, y, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	// The journal catches up and the ref is quiescent: nothing settles normally,
	// because unknown is terminal until an operator resolves it.
	w.SetQuiescent(TenantA, repo, true)
	w.SetRef(TenantA, repo, x, b)
	pairs(repo, "e2", 4)
	f.mustReconcile(repo)
	got = f.Record(repo, r3.PushID)
	wantState(t, got, x, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	wantState(t, got, y, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	f.wantEvents(repo, 0)
}

// ABASettlesRefused: a predecessor's committed entry appended after a
// successor's prepared entry (A to B committed late, then B to A prepared and
// crashed) settles the successor refused, not applied; the value of the ref
// under the lock decides, never the order of outcome entries (GR-09, GR-A13).
func ABASettlesRefused(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	ref, a, b := Branch("aba"), OID("a"), OID("b")
	req1, r1 := f.authorized(eng, "rcv_x1", Advance(ref, a, b))
	req2, r2 := f.authorized(eng, "rcv_x2", Advance(ref, b, a))
	f.mustIntent(r1.PushID, req1)
	f.mustIntent(r2.PushID, req2)
	w := f.Worker
	w.Prepared(TenantA, RepoMain, "rcv_x1", ref, a, b, a)
	w.Prepared(TenantA, RepoMain, "rcv_x2", ref, b, a, b) // observed the result of x1
	w.Committed(TenantA, RepoMain, "rcv_x1", ref, a, b)   // appended late, after x2's prepared entry
	w.SetRef(TenantA, RepoMain, ref, b)                   // x2 crashed before it changed the ref
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, r1.PushID), gitv1.PushApplied)
	rec2 := f.Record(RepoMain, r2.PushID)
	wantStatus(t, rec2, gitv1.PushRefused)
	wantState(t, rec2, ref, gitv1.EntryRefused, gitv1.ReasonRefUnchanged)
	f.wantEvents(RepoMain, 1)

	// The same shape where the successor did commit is applied by its own entry.
	ref2 := Branch("aba2")
	req3, r3 := f.authorized(eng, "rcv_x3", Advance(ref2, a, b))
	req4, r4 := f.authorized(eng, "rcv_x4", Advance(ref2, b, a))
	f.mustIntent(r3.PushID, req3)
	f.mustIntent(r4.PushID, req4)
	w.Prepared(TenantA, RepoMain, "rcv_x3", ref2, a, b, a)
	w.Prepared(TenantA, RepoMain, "rcv_x4", ref2, b, a, b)
	w.Committed(TenantA, RepoMain, "rcv_x3", ref2, a, b)
	w.Committed(TenantA, RepoMain, "rcv_x4", ref2, b, a)
	w.SetRef(TenantA, RepoMain, ref2, a)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, r3.PushID), gitv1.PushApplied)
	wantStatus(t, f.Record(RepoMain, r4.PushID), gitv1.PushApplied)
	f.wantEvents(RepoMain, 3)
}

// OutcomeEmitsOneEvent: an applied record commits exactly one reference-only
// git.refs_updated event with its audit in the settling transaction; replays,
// reconciliation and a failing append do not duplicate or lose it; refused and
// unknown records emit none (GR-10, GR-11, GR-A06).
func OutcomeEmitsOneEvent(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	req, resp := f.authorized(eng, "rcv_o1", Create(Branch("c"), OID("1")), Rewrite(Branch("r"), OID("2"), OID("3")))
	f.mustIntent(resp.PushID, req)
	if _, err := f.Outcome(resp.PushID, req, Branch("c"), Branch("r")); err != nil {
		t.Fatal(err)
	}
	ev := f.wantEvents(RepoMain, 1)[0]
	if ev.Type != eventsv1.TypeGitRefsUpdated || ev.Resource.ID != f.Record(RepoMain, resp.PushID).ReceiveID || ev.Resource.Type != eventsv1.ResourceTypeGitPush || ev.Resource.Version < 2 ||
		ev.ProjectID != ProjectA || ev.TenantID != TenantA {
		t.Fatalf("envelope: %+v", ev)
	}
	if err := ev.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := eventsv1.DecodeData(ev)
	if err != nil {
		t.Fatal(err)
	}
	got := data.(*eventsv1.GitRefsUpdated)
	if ev.Resource.ID != f.Record(RepoMain, resp.PushID).ReceiveID || got.ReceiveID != f.Record(RepoMain, resp.PushID).ReceiveID || got.RepositoryID != RepoMain || got.OriginRunID != nil || got.Actor.Type != "user" || got.Actor.ID != string(Engineer) ||
		len(got.Updates) != 2 || got.Updates[0].Kind != eventsv1.GitKindCreate || got.Updates[1].Kind != eventsv1.GitKindRewrite {
		t.Fatalf("payload: %+v", got)
	}
	// The record a consumer reloads agrees with the event.
	rec := f.Record(RepoMain, resp.PushID)
	if want, err := eventsv1.NewGitRefsUpdated(rec); err != nil || fmt.Sprint(want.Updates) != fmt.Sprint(got.Updates) || want.ReceiveID != got.ReceiveID {
		t.Fatalf("event and record disagree: %+v %+v %v", want, got, err)
	}
	audit := f.Env.AuditActions(t, ScopeA(), resp.PushID)
	if fmt.Sprint(audit) != fmt.Sprint([]string{gitpush.AuditHistoryRewrite, gitpush.AuditApplied, gitpush.AuditAuthorized}) {
		t.Fatalf("audit = %v", audit)
	}

	// A retried post-receive, and reconciliation, add nothing.
	for range 2 {
		if out, err := f.Outcome(resp.PushID, req, Branch("c"), Branch("r")); err != nil || out.Status != gitv1.PushApplied {
			t.Fatalf("replayed outcome: %+v, %v", out, err)
		}
	}
	f.mustReconcile(RepoMain)
	f.wantEvents(RepoMain, 1)

	// A failing event append rolls the settlement back; the retry emits once.
	req, resp = f.authorized(eng, "rcv_o2", Create(Branch("d"), OID("1")))
	f.mustIntent(resp.PushID, req)
	f.Env.FailEvents(errors.New("event bus down"))
	if _, err := f.Outcome(resp.PushID, req, Branch("d")); err == nil {
		t.Fatal("the outcome must fail with the event append")
	}
	f.Env.FailEvents(nil)
	wantState(t, f.Record(RepoMain, resp.PushID), Branch("d"), gitv1.EntryCommitting, "")
	f.wantEvents(RepoMain, 1)
	if _, err := f.Outcome(resp.PushID, req, Branch("d")); err != nil {
		t.Fatal(err)
	}
	f.wantEvents(RepoMain, 2)

	// A run's push carries the run as its origin and the workload as actor.
	run := "run_o"
	att := f.Attempt("att_o", run, RepoMain)
	agent := Agent("wi_1", run, "work")
	areq := Request(RepoMain, att, "rcv_o3", false, Create(agent, OID("1")))
	aresp := f.MustAuthorize(areq)
	f.mustIntent(aresp.PushID, areq)
	if _, err := f.Outcome(aresp.PushID, areq, agent); err != nil {
		t.Fatal(err)
	}
	evs := f.wantEvents(RepoMain, 3)
	// Every push is its own aggregate: distinct resource IDs, none of them the
	// repository the lifecycle events of the repository module use.
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Resource.Type != eventsv1.ResourceTypeGitPush || e.Resource.ID == RepoMain || seen[e.Resource.ID] || seen[e.ID] {
			t.Fatalf("event resources: %+v", e.Resource)
		}
		seen[e.Resource.ID], seen[e.ID] = true, true
	}
	adata, err := eventsv1.DecodeData(evs[2])
	if err != nil {
		t.Fatal(err)
	}
	ag := adata.(*eventsv1.GitRefsUpdated)
	if ag.OriginRunID == nil || *ag.OriginRunID != run || ag.Actor.Type != "workload" || ag.Actor.ID != "att_o" {
		t.Fatalf("workload payload: %+v", ag)
	}

	// Refused and unknown records emit nothing.
	refused := Request(RepoMain, eng, "rcv_o4", false, Advance(Branch("main"), OID("1"), OID("2")))
	f.MustAuthorize(refused)
	f.authorizedUnknown(eng, "rcv_o5")
	f.wantEvents(RepoMain, 3)

	// The repository module learns of the first refs of an empty repository, and
	// when that report fails the obligation survives for reconciliation.
	grant, err := f.Seed(Engineer, RepoEmpty)
	if err != nil {
		t.Fatal(err)
	}
	sreq := f.emptySeedRequest(grant, "rcv_o6", Create(Branch("main"), OID("a")))
	sresp := f.MustAuthorize(sreq)
	f.mustIntent(sresp.PushID, sreq)
	f.Repos.ReportFail = errors.New("repository module down")
	if out, err := f.Outcome(sresp.PushID, sreq, Branch("main")); err != nil || out.Status != gitv1.PushApplied {
		t.Fatalf("the settlement must not depend on the report: %+v, %v", out, err)
	}
	if !f.Repos.IsEmpty(TenantA, RepoEmpty) {
		t.Fatal("reported despite the failure")
	}
	if err := f.Reconcile(RepoEmpty); err == nil {
		t.Fatal("a failing report must be reported")
	}
	f.Repos.ReportFail = nil
	f.mustReconcile(RepoEmpty)
	if f.Repos.IsEmpty(TenantA, RepoEmpty) {
		t.Fatal("reconciliation did not repeat the report")
	}
	reports := f.Repos.Reports
	f.mustReconcile(RepoEmpty)
	if f.Repos.Reports != reports {
		t.Fatal("a delivered report is not repeated")
	}
}

// authorizedUnknown creates a record whose only entry settles unknown.
func (f *Fixture) authorizedUnknown(grant, receive string) gitv1.PushRecord {
	f.T.Helper()
	req, resp := f.authorized(grant, receive, Create(Branch("unknown_"+receive), OID("1")))
	f.mustIntent(resp.PushID, req)
	f.mustReconcile(RepoMain) // the journal has nothing: unknown
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(f.T, rec, gitv1.PushUnknown)
	return rec
}

// UnknownBlocksWriter: an unknown entry blocks reassignment of the writer of
// the affected task branch and nothing else, and stays until an operator
// resolves it (GR-09, WS-11).
func UnknownBlocksWriter(t *testing.T, f *Fixture) {
	task, other := Task("wi_7"), Task("wi_8")
	att := f.Attempt("att_w", "run_w", RepoMain, task)
	f.Reservations.Hold(ScopeA(), "wi_7", "att_w")
	req := Request(RepoMain, att, "rcv_k1", false, Create(task, OID("1")))
	resp := f.MustAuthorize(req)
	wantVerdicts(t, resp, map[string]string{task: ""})
	if blocked, err := f.Svc.UnknownBlocksWriter(bg, ScopeA(), RepoMain, task); err != nil || blocked {
		t.Fatalf("an authorized push blocks nothing: %v, %v", blocked, err)
	}
	f.mustIntent(resp.PushID, req)
	f.mustReconcile(RepoMain) // no evidence
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(t, rec, gitv1.PushUnknown)
	wantState(t, rec, task, gitv1.EntryUnknown, gitv1.ReasonEvidenceMissing)
	if *rec.OriginRunID != "run_w" {
		t.Fatalf("origin: %+v", rec)
	}

	blocks := func(scope domain.Scope, repo, ref string) bool {
		t.Helper()
		b, err := f.Svc.UnknownBlocksWriter(bg, scope, repo, ref)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !blocks(ScopeA(), RepoMain, task) {
		t.Fatal("an unknown entry must block reassignment of its writer")
	}
	if blocks(ScopeA(), RepoMain, other) || blocks(ScopeA(), RepoDirect, task) || blocks(ScopeX(), RepoMain, task) || blocks(ScopeB(), RepoMain, task) {
		t.Fatal("only the affected ref of the affected repository of the affected project is blocked")
	}
	f.mustReconcile(RepoMain)
	f.mustReconcile(RepoMain)
	if !blocks(ScopeA(), RepoMain, task) {
		t.Fatal("reconciliation never resolves an unknown entry")
	}
	if busy, err := f.Svc.UnsettledPush(bg, ScopeA(), RepoMain); err != nil || !busy {
		t.Fatalf("UnsettledPush = %v, %v", busy, err)
	}
	if _, err := f.Svc.UnknownBlocksWriter(bg, ScopeA(), RepoMain, "not a ref"); err == nil {
		t.Fatal("a malformed ref is refused")
	}
}

// ListAndTenantIsolation: push records are listed newest first, paged, and a
// foreign tenant, project or repository sees nothing (GR-01, GR-05).
func ListAndTenantIsolation(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	var ids []string
	for i := range 3 {
		f.Clock.Advance(time.Second)
		_, resp := f.authorized(eng, fmt.Sprintf("rcv_l%d", i), Create(Branch(fmt.Sprintf("b%d", i)), OID("1")))
		ids = append(ids, resp.PushID)
	}
	page, err := f.Svc.List(bg, "usr_reader", ScopeA(), RepoMain, gitpush.ListQuery{Limit: 2})
	if err != nil || len(page) != 2 || page[0].Record.PushID != ids[2] || page[1].Record.PushID != ids[1] {
		t.Fatalf("first page: %+v, %v", page, err)
	}
	next, err := f.Svc.List(bg, "usr_reader", ScopeA(), RepoMain, gitpush.ListQuery{Limit: 2, After: &gitpush.Position{CreatedAt: page[1].CreatedAt, ID: page[1].Record.PushID}})
	if err != nil || len(next) != 1 || next[0].Record.PushID != ids[0] {
		t.Fatalf("second page: %+v, %v", next, err)
	}
	// Reads need the project's read authority.
	_, err = f.Svc.List(bg, "usr_stranger", ScopeA(), RepoMain, gitpush.ListQuery{Limit: 2})
	wantCode(t, err, domain.CodeForbidden)
	_, err = f.Svc.Get(bg, "usr_stranger", ScopeA(), RepoMain, ids[0])
	wantCode(t, err, domain.CodeForbidden)
	// A record of another repository, project or tenant is not found.
	for name, scope := range map[string]domain.Scope{"project": ScopeX(), "tenant": ScopeB()} {
		if _, err := f.Svc.Reload(bg, scope, RepoMain, ids[0]); !gitpush.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("reload from another %s: %v", name, err)
		}
	}
	if _, err := f.Svc.Reload(bg, ScopeA(), RepoDirect, ids[0]); !gitpush.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("reload through another repository: %v", err)
	}
	f.Reads.Allow("usr_reader", ScopeX())
	if _, err := f.Svc.List(bg, "usr_reader", ScopeX(), RepoMain, gitpush.ListQuery{Limit: 2}); !gitpush.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("list of a repository through another project: %v", err)
	}
	if busy, err := f.Svc.UnsettledPush(bg, ScopeB(), RepoMain); err != nil || busy {
		t.Fatalf("another tenant's repository has no unsettled push: %v, %v", busy, err)
	}
}

func (f *Fixture) resolve(principal string, scope domain.Scope, repo, pushID, ref string, to gitv1.EntryState, reason string, expected domain.Version) (gitpush.Resolved, error) {
	return f.Svc.ResolveUnknown(bg, gitpush.ResolveCommand{Principal: principal, Scope: scope, RepositoryID: repo, PushID: pushID, Ref: ref,
		Resolution: to, Reason: reason, Expected: expected})
}

func (f *Fixture) version(repo, pushID string) domain.Version {
	f.T.Helper()
	v, err := f.Svc.GetVersioned(bg, "usr_reader", ScopeA(), repo, pushID)
	if err != nil {
		f.T.Fatal(err)
	}
	return v.Version
}

// OperatorResolvesUnknown: an administrator who inspected the repository settles
// an unknown entry as applied or refused with a reason. It needs the admin role
// and the record's current version, only an unknown entry resolves, every
// resolution is audited, the event of a record that becomes applied is emitted
// exactly once, and the writer and the seed are unblocked when nothing unknown
// is left (GR-09, WS-11, GR-18).
func OperatorResolvesUnknown(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	x, y := Branch("res_x"), Branch("res_y")
	req, resp := f.authorized(eng, "rcv_o1", Create(x, OID("1")), Create(y, OID("2")))
	f.mustIntent(resp.PushID, req)
	f.mustReconcile(RepoMain) // the journal has nothing: both unknown
	rec := f.Record(RepoMain, resp.PushID)
	wantStatus(t, rec, gitv1.PushUnknown)
	wantState(t, rec, x, gitv1.EntryUnknown, gitv1.ReasonEvidenceMissing)
	f.wantEvents(RepoMain, 0)
	if busy, err := f.Svc.UnsettledPush(bg, ScopeA(), RepoMain); err != nil || !busy {
		t.Fatalf("an unknown record keeps the seed blocked: %v, %v", busy, err)
	}
	v := f.version(RepoMain, resp.PushID)
	why := "inspected the repository: refs/heads/res_x does not exist"

	// Authority, input and freshness are checked before anything changes.
	_, err := f.resolve("usr_reader", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v)
	wantCode(t, err, domain.CodeForbidden)
	_, err = f.resolve(string(Engineer), ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v)
	wantCode(t, err, domain.CodeForbidden)
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, "", v)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryCommitting, why, v)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v+1)
	wantCode(t, err, domain.CodeVersionConflict)
	_, err = f.resolve("usr_admin", ScopeB(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v)
	wantCode(t, err, domain.CodeForbidden) // no authority in another tenant's project
	f.Admins.Allow("usr_admin", ScopeB())
	_, err = f.resolve("usr_admin", ScopeB(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v)
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.resolve("usr_admin", ScopeA(), RepoDirect, resp.PushID, x, gitv1.EntryRefused, why, v)
	wantCode(t, err, domain.CodeNotFound) // through another repository
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, Branch("nope"), gitv1.EntryRefused, why, v)
	if err == nil {
		t.Fatal("an unknown ref names no entry")
	}
	if f.version(RepoMain, resp.PushID) != v {
		t.Fatal("a refused resolution changed the record")
	}

	// The first entry is refused: the record stays unknown, nothing is emitted,
	// the other entry still blocks.
	got, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, why, v)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != v+1 {
		t.Fatalf("version %d, want %d", got.Version, v+1)
	}
	wantStatus(t, got.Record, gitv1.PushUnknown)
	wantState(t, got.Record, x, gitv1.EntryRefused, gitv1.ReasonRefUnchanged)
	wantState(t, got.Record, y, gitv1.EntryUnknown, gitv1.ReasonEvidenceMissing)
	f.wantEvents(RepoMain, 0)
	if blocked, _ := f.Svc.UnknownBlocksWriter(bg, ScopeA(), RepoMain, x); blocked {
		t.Fatal("the resolved ref is unblocked")
	}
	if blocked, _ := f.Svc.UnknownBlocksWriter(bg, ScopeA(), RepoMain, y); !blocked {
		t.Fatal("the ref still unknown stays blocked")
	}
	// The same version is stale now; a resolved entry is not unknown any more.
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, y, gitv1.EntryApplied, why, v)
	wantCode(t, err, domain.CodeVersionConflict)
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryApplied, why, got.Version)
	wantCode(t, err, gitv1.CodePushSettled)

	// The second entry is applied: the record becomes applied and emits its one event.
	w := f.Worker
	w.SetRef(TenantA, RepoMain, y, OID("2"))
	fin, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, y, gitv1.EntryApplied, "inspected: refs/heads/res_y is at the pushed commit", got.Version)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, fin.Record, gitv1.PushApplied)
	wantState(t, fin.Record, y, gitv1.EntryApplied, "")
	evs := f.wantEvents(RepoMain, 1)
	data, err := eventsv1.DecodeData(evs[0])
	if err != nil || evs[0].Resource.ID != f.Record(RepoMain, resp.PushID).ReceiveID || len(data.(*eventsv1.GitRefsUpdated).Updates) != 1 {
		t.Fatalf("the event: %+v %v", data, err)
	}
	if blocked, _ := f.Svc.UnknownBlocksWriter(bg, ScopeA(), RepoMain, y); blocked {
		t.Fatal("nothing unknown is left")
	}
	if busy, err := f.Svc.UnsettledPush(bg, ScopeA(), RepoMain); err != nil || busy {
		t.Fatalf("the seed is unblocked: %v, %v", busy, err)
	}
	actions := f.Env.AuditActions(t, ScopeA(), resp.PushID)
	n := 0
	for _, a := range actions {
		if a == gitpush.AuditOperatorResolved {
			n++
		}
	}
	if n != 2 || !contains(actions, gitpush.AuditApplied) {
		t.Fatalf("audit %v", actions)
	}
	// Nothing settles it again and nothing is emitted twice.
	f.mustReconcile(RepoMain)
	f.mustReconcile(RepoMain)
	f.wantEvents(RepoMain, 1)
	_, err = f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, y, gitv1.EntryRefused, why, fin.Version)
	wantCode(t, err, gitv1.CodePushSettled)
	f.wantEvents(RepoMain, 1)

	// A record with a single unknown entry resolved refused settles refused: no event.
	req2, resp2 := f.authorized(eng, "rcv_o2", Create(Branch("res_z"), OID("3")))
	f.mustIntent(resp2.PushID, req2)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp2.PushID), gitv1.PushUnknown)
	r2, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp2.PushID, Branch("res_z"), gitv1.EntryRefused, why, f.version(RepoMain, resp2.PushID))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, r2.Record, gitv1.PushRefused)
	f.wantEvents(RepoMain, 1)
}

// AcknowledgeRestore: after a volume restored from an older snapshot (every
// committing entry became unknown by the regression latch), the restored
// journal becomes the baseline once nothing is in flight, so later receives
// settle normally; before that it is refused (GR-15, GR-A10, GR-A13).
func AcknowledgeRestore(t *testing.T, f *Fixture) {
	w := f.Worker
	eng := f.Human(Engineer, RepoMain)
	a, b := OID("a"), OID("b")
	for i := range 6 {
		id := fmt.Sprintf("srv_r%d", i)
		w.Prepared(TenantA, RepoMain, id, Branch("filler"), a, b, a)
		w.Committed(TenantA, RepoMain, id, Branch("filler"), a, b)
	}
	ref := Branch("restore_1")
	req, resp := f.authorized(eng, "rcv_s1", Create(ref, b))
	f.mustIntent(resp.PushID, req)
	w.Prepared(TenantA, RepoMain, "rcv_s1", ref, Zero, b, Zero)
	w.Committed(TenantA, RepoMain, "rcv_s1", ref, Zero, b)
	w.SetRef(TenantA, RepoMain, ref, b)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushApplied)
	known := w.Head(TenantA, RepoMain)

	ref2 := Branch("restore_2")
	req2, resp2 := f.authorized(eng, "rcv_s2", Create(ref2, b))
	f.mustIntent(resp2.PushID, req2)
	st, err := f.Svc.Settlement(bg, TenantA, RepoMain)
	if err != nil || st.InFlight != 1 || st.KnownHead != known {
		t.Fatalf("settlement %+v %v", st, err)
	}
	w.Restore(TenantA, RepoMain, 4) // the volume comes back from an older snapshot
	why := "restored from snapshot s-4"
	if _, err := f.Svc.AcknowledgeRestore(bg, ScopeA(), RepoMain, 4, "usr_admin", why); !gitpush.IsCode(err, gitv1.CodePushSettled) {
		t.Fatalf("an acknowledgement with entries in flight: %v", err)
	}
	f.mustReconcile(RepoMain)
	wantState(t, f.Record(RepoMain, resp2.PushID), ref2, gitv1.EntryUnknown, gitv1.ReasonJournalRegressed)
	st, err = f.Svc.Settlement(bg, TenantA, RepoMain)
	if err != nil || st.InFlight != 0 || st.Unknown != 1 {
		t.Fatalf("settlement %+v %v", st, err)
	}
	if changed, err := f.Svc.AcknowledgeRestore(bg, ScopeA(), RepoMain, known+5, "usr_admin", why); err != nil || changed {
		t.Fatalf("a head not below the known one changes nothing: %v, %v", changed, err)
	}
	if changed, err := f.Svc.AcknowledgeRestore(bg, ScopeA(), RepoMain, 4, "usr_admin", why); err != nil || !changed {
		t.Fatalf("acknowledge: %v, %v", changed, err)
	}
	if st, _ = f.Svc.Settlement(bg, TenantA, RepoMain); st.KnownHead != 4 {
		t.Fatalf("known head %d", st.KnownHead)
	}
	if _, err := f.Svc.AcknowledgeRestore(bg, ScopeA(), RepoMain, 3, "", why); err == nil {
		t.Fatal("an acknowledgement names its actor")
	}
	// A later receive settles against the restored journal, not as a regression.
	ref3 := Branch("restore_3")
	req3, resp3 := f.authorized(eng, "rcv_s3", Create(ref3, b))
	f.mustIntent(resp3.PushID, req3)
	w.Prepared(TenantA, RepoMain, "rcv_s3", ref3, Zero, b, Zero)
	w.Committed(TenantA, RepoMain, "rcv_s3", ref3, Zero, b)
	w.SetRef(TenantA, RepoMain, ref3, b)
	f.mustReconcile(RepoMain)
	wantStatus(t, f.Record(RepoMain, resp3.PushID), gitv1.PushApplied)
}

// UnsettledOnRef: a push record whose entry for the ref is authorized,
// committing or unknown makes the ref unsettled, whichever other entries it has;
// settled, refused and applied entries, other refs, repositories, projects and
// tenants do not; a foreign repository is not_found (the workspace's RefPushes).
func UnsettledOnRef(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	a, b := Branch("ur_a"), Branch("ur_b")
	unsettled := func(scope domain.Scope, repo, ref string) bool {
		t.Helper()
		got, err := f.Svc.UnsettledOnRef(bg, scope, repo, ref)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if unsettled(ScopeA(), RepoMain, a) {
		t.Fatal("nothing is unsettled before any push")
	}
	req, resp := f.authorized(eng, "rcv_u1", Create(a, OID("1")), Create(b, OID("2")))
	if !unsettled(ScopeA(), RepoMain, a) || !unsettled(ScopeA(), RepoMain, b) {
		t.Fatal("an authorized entry is unsettled")
	}
	if unsettled(ScopeA(), RepoMain, Branch("ur_other")) || unsettled(ScopeA(), RepoDirect, a) {
		t.Fatal("only the ref of the repository it names is unsettled")
	}
	f.mustIntent(resp.PushID, req, a)
	if !unsettled(ScopeA(), RepoMain, a) {
		t.Fatal("a committing entry is unsettled")
	}
	// a applied, b never reached committing: refused.
	if _, err := f.Outcome(resp.PushID, req, a); err != nil {
		t.Fatal(err)
	}
	if unsettled(ScopeA(), RepoMain, a) || unsettled(ScopeA(), RepoMain, b) {
		t.Fatalf("applied and refused entries are settled: %+v", f.Record(RepoMain, resp.PushID).Entries)
	}

	// An unknown entry stays unsettled for its ref only, until an operator resolves it.
	x, y := Branch("ur_x"), Branch("ur_y")
	req, resp = f.authorized(eng, "rcv_u2", Create(x, OID("3")), Create(y, OID("4")))
	f.mustIntent(resp.PushID, req)
	f.mustReconcile(RepoMain) // no evidence: both unknown
	if !unsettled(ScopeA(), RepoMain, x) || !unsettled(ScopeA(), RepoMain, y) {
		t.Fatal("an unknown entry is unsettled")
	}
	v := f.version(RepoMain, resp.PushID)
	if _, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, x, gitv1.EntryRefused, "inspected: no such branch", v); err != nil {
		t.Fatal(err)
	}
	if unsettled(ScopeA(), RepoMain, x) || !unsettled(ScopeA(), RepoMain, y) {
		t.Fatal("a resolved entry settles its ref only")
	}

	// Scope: the same ref and repository ID of another tenant or project is not visible.
	// TenantB has its own repository of that ID: it exists, with nothing unsettled.
	if got, err := f.Svc.UnsettledOnRef(bg, ScopeB(), RepoMain, y); err != nil || got {
		t.Fatalf("another tenant sees the unsettled entry: %v, %v", got, err)
	}
	_, err := f.Svc.UnsettledOnRef(bg, ScopeX(), RepoMain, y) // RepoMain belongs to ProjectA
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.UnsettledOnRef(bg, ScopeA(), "r130_missing", y)
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.UnsettledOnRef(bg, ScopeA(), RepoMain, "not a ref")
	wantCode(t, err, domain.CodeInvalidRequest)
}

// AppliedOnRef: the records with an applied entry for the ref come back newest
// first by apply order, bounded by the limit, as valid push records; refused,
// unknown and in-flight entries, other refs and other scopes do not appear.
func AppliedOnRef(t *testing.T, f *Fixture) {
	eng := f.Human(Engineer, RepoMain)
	main := Branch("ap_shared")
	applied := func(limit int) []gitv1.PushRecord {
		t.Helper()
		got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, main, limit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := applied(5); len(got) != 0 {
		t.Fatalf("nothing applied yet: %+v", got)
	}
	var ids []string
	old := Zero
	for i := range 3 {
		f.Clock.Advance(time.Second)
		next := OID(string(rune('1' + i)))
		var u U
		if i == 0 {
			u = Create(main, next)
		} else {
			u = Advance(main, old, next)
		}
		req, resp := f.authorized(eng, fmt.Sprintf("rcv_a%d", i), u)
		f.mustIntent(resp.PushID, req)
		if _, err := f.Outcome(resp.PushID, req, main); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, resp.PushID)
		old = next
	}
	// Records that must not appear: another ref, a refused entry and an unknown one.
	other := Branch("ap_other")
	f.Clock.Advance(time.Second)
	req, resp := f.authorized(eng, "rcv_a_other", Create(other, OID("9")))
	f.mustIntent(resp.PushID, req)
	if _, err := f.Outcome(resp.PushID, req, other); err != nil {
		t.Fatal(err)
	}
	got := applied(10)
	if len(got) != 3 || got[0].PushID != ids[2] || got[1].PushID != ids[1] || got[2].PushID != ids[0] {
		t.Fatalf("records %v, want newest first %v", pushIDs(got), []string{ids[2], ids[1], ids[0]})
	}
	for _, r := range got {
		if err := r.Validate(); err != nil || r.Status != gitv1.PushApplied || States(r)[main] != gitv1.EntryApplied {
			t.Fatalf("record %+v: %v", r, err)
		}
	}
	if got := applied(2); len(got) != 2 || got[0].PushID != ids[2] || got[1].PushID != ids[1] {
		t.Fatalf("limit 2: %v", pushIDs(got))
	}
	if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, other, 10); err != nil || len(got) != 1 {
		t.Fatalf("another ref: %v, %v", pushIDs(got), err)
	}

	// An entry that is not applied never appears: unknown, then operator-refused.
	u := Branch("ap_unknown")
	req, resp = f.authorized(eng, "rcv_a_unknown", Create(u, OID("5")))
	f.mustIntent(resp.PushID, req)
	f.mustReconcile(RepoMain)
	if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, u, 10); err != nil || len(got) != 0 {
		t.Fatalf("an unknown entry is not applied: %v, %v", pushIDs(got), err)
	}
	// An operator resolving it applied makes the record appear, newest.
	f.Clock.Advance(time.Second)
	if _, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp.PushID, u, gitv1.EntryApplied, "inspected: the branch exists", f.version(RepoMain, resp.PushID)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, u, 10); err != nil || len(got) != 1 || got[0].PushID != resp.PushID {
		t.Fatalf("a resolved-applied entry: %v, %v", pushIDs(got), err)
	}

	// The order is the order in which the entry for the ref applied, not the
	// record's last change. R1 holds two unknown entries p and q. The operator
	// applies p; then R2 applies p again (a fast-forward); then the operator
	// applies q of R1, which is the last change of R1. R2's entry for p applied
	// after R1's, so R2 is first, although R1 changed last; q has only R1.
	p, q := Branch("ap_p"), Branch("ap_q")
	f.Clock.Advance(time.Second)
	req1, resp1 := f.authorized(eng, "rcv_a_r1", Create(p, OID("7")), Create(q, OID("8")))
	f.mustIntent(resp1.PushID, req1)
	f.mustReconcile(RepoMain) // no evidence: both unknown
	f.Clock.Advance(time.Second)
	if _, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp1.PushID, p, gitv1.EntryApplied, "inspected: p exists", f.version(RepoMain, resp1.PushID)); err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(time.Second)
	req2, resp2 := f.authorized(eng, "rcv_a_r2", Advance(p, OID("7"), OID("6")))
	f.mustIntent(resp2.PushID, req2)
	if _, err := f.Outcome(resp2.PushID, req2, p); err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(time.Second)
	if _, err := f.resolve("usr_admin", ScopeA(), RepoMain, resp1.PushID, q, gitv1.EntryApplied, "inspected: q exists", f.version(RepoMain, resp1.PushID)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, p, 10); err != nil || len(got) != 2 || got[0].PushID != resp2.PushID || got[1].PushID != resp1.PushID {
		t.Fatalf("p: %v, want [%s %s] (apply order, not last change): %v", pushIDs(got), resp2.PushID, resp1.PushID, err)
	}
	if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, q, 10); err != nil || len(got) != 1 || got[0].PushID != resp1.PushID {
		t.Fatalf("q: %v %v", pushIDs(got), err)
	}
	// A record whose several entries applied together lists once per ref.
	f.Clock.Advance(time.Second)
	req3, resp3 := f.authorized(eng, "rcv_a_r3", Create(Branch("ap_m1"), OID("a")), Create(Branch("ap_m2"), OID("b")))
	f.mustIntent(resp3.PushID, req3)
	if _, err := f.Outcome(resp3.PushID, req3, Branch("ap_m1"), Branch("ap_m2")); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{Branch("ap_m1"), Branch("ap_m2")} {
		if got, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, ref, 10); err != nil || len(got) != 1 || got[0].PushID != resp3.PushID {
			t.Fatalf("%s: %v %v", ref, pushIDs(got), err)
		}
	}

	// Bounds, scope and tenant isolation.
	_, err := f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, main, 0)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.AppliedOnRef(bg, ScopeA(), RepoMain, main, gitpush.MaxAppliedOnRef+1)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.AppliedOnRef(bg, ScopeX(), RepoMain, main, 5)
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.AppliedOnRef(bg, ScopeA(), "r130_missing", main, 5)
	wantCode(t, err, domain.CodeNotFound)
	if got, err := f.Svc.AppliedOnRef(bg, ScopeB(), RepoMain, main, 5); err != nil || len(got) != 0 {
		t.Fatalf("another tenant sees applied pushes: %v, %v", pushIDs(got), err)
	}
}

func pushIDs(rs []gitv1.PushRecord) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.PushID
	}
	return out
}

// AuthorizeRechecksWriterAfterCommit: the writer reservation read before the
// authorization transaction can be released (or the workspace archived) before
// the record commits, and the workspace cannot see an uncommitted record. So the
// reservation is read again once the authorized record is durable and before the
// verdicts reach the worker; a reservation lost since settles the record refused
// (the task-branch entry ref_protected, the others no_commit_intent, audited, no
// event) and the denial is returned, replayed identically, and nothing can apply.
// Commit intent reads the reservation once more (GR-07, WS-03, WS-11).
func AuthorizeRechecksWriterAfterCommit(t *testing.T, f *Fixture) {
	task, scratch := Task("wi_rc"), Branch("scratch_rc")
	scope := ScopeA()
	att := f.Attempt("att_rc", "run_rc", RepoMain, task, scratch)
	f.Reservations.Hold(scope, "wi_rc", "att_rc")
	unsettled := func() bool {
		t.Helper()
		got, err := f.Svc.UnsettledOnRef(bg, scope, RepoMain, task)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Ordering: the first read precedes the record, the second follows its commit.
	var visible []bool
	f.Reservations.AfterRead = func(int) { visible = append(visible, unsettled()) }
	req := Request(RepoMain, att, "rcv_rc1", false, Create(task, OID("1")), Create(scratch, OID("2")))
	resp := f.MustAuthorize(req)
	f.Reservations.AfterRead = nil
	if !resp.Allowed() || f.Reservations.Reads() != 2 || len(visible) != 2 || visible[0] || !visible[1] {
		t.Fatalf("the reservation is read before the record exists and again after it is visible: allowed=%v reads=%d visible=%v",
			resp.Allowed(), f.Reservations.Reads(), visible)
	}
	wantStatus(t, f.Record(RepoMain, resp.PushID), gitv1.PushAuthorized)

	// The writer is released between the first read and the commit of the record.
	task2 := Task("wi_rc2")
	att2 := f.Attempt("att_rc2", "run_rc2", RepoMain, task2, scratch)
	f.Reservations.Hold(scope, "wi_rc2", "att_rc2")
	before := f.Reservations.Reads()
	f.Reservations.AfterRead = func(n int) {
		if n == before+1 { // the read before the transaction
			f.Reservations.Release(scope, "wi_rc2", "att_rc2")
		}
	}
	req2 := Request(RepoMain, att2, "rcv_rc2", false, Create(task2, OID("3")), Create(scratch, OID("4")))
	denied, err := f.Authorize(req2)
	f.Reservations.AfterRead = nil
	if err != nil {
		t.Fatal(err)
	}
	if f.Reservations.Reads() != before+2 {
		t.Fatalf("reads %d, want %d", f.Reservations.Reads()-before, 2)
	}
	if denied.Allowed() {
		t.Fatalf("a push whose writer was released before the record committed was allowed: %+v", denied.Updates)
	}
	if got := verdicts(denied); got[task2] != gitv1.ReasonRefProtected {
		t.Fatalf("verdicts %v", got)
	}
	rec := f.Record(RepoMain, denied.PushID)
	wantStatus(t, rec, gitv1.PushRefused)
	wantState(t, rec, task2, gitv1.EntryRefused, gitv1.ReasonRefProtected)
	wantState(t, rec, scratch, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent) // the receive fails as a whole
	f.wantEvents(RepoMain, 0)
	if got := f.Env.AuditActions(t, scope, denied.PushID); fmt.Sprint(got) != fmt.Sprint([]string{gitpush.AuditAuthorized, gitpush.AuditRefused}) {
		t.Fatalf("audit = %v", got)
	}
	// Nothing can apply, and nothing stays unsettled for the workspace.
	err = f.Intent(denied.PushID, req2)
	wantCode(t, err, gitv1.CodePushSettled)
	if out, err := f.Outcome(denied.PushID, req2, task2); err != nil || out.Status != gitv1.PushRefused {
		t.Fatalf("an outcome cannot apply a refused record: %+v %v", out, err)
	}
	if applied, err := f.Svc.AppliedOnRef(bg, scope, RepoMain, task2, 5); err != nil || len(applied) != 0 {
		t.Fatalf("applied %v %v", applied, err)
	}
	if busy, err := f.Svc.UnsettledOnRef(bg, scope, RepoMain, task2); err != nil || busy {
		t.Fatalf("a refused record leaves the ref unsettled: %v %v", busy, err)
	}
	// A repeat of the receive returns the same denial.
	again, err := f.Authorize(req2)
	if err != nil || again.PushID != denied.PushID || !same(again, denied) {
		t.Fatalf("replay: %+v %v", again, err)
	}
	f.wantEvents(RepoMain, 0)

	// A reservation that cannot be read after the commit is a retryable outage:
	// nothing is decided, the record stays authorized, its replay is the stored
	// allow and commit intent reads the reservation again.
	task3 := Task("wi_rc3")
	att3 := f.Attempt("att_rc3", "run_rc3", RepoMain, task3)
	f.Reservations.Hold(scope, "wi_rc3", "att_rc3")
	f.Reservations.AfterRead = func(int) { // after the first read of this receive
		f.Reservations.Fail = errors.New("workspace unavailable")
		f.Reservations.AfterRead = nil
	}
	f.Clock.Advance(time.Second) // the newest record is the listed first
	req3 := Request(RepoMain, att3, "rcv_rc3", false, Create(task3, OID("5")))
	_, err = f.Authorize(req3)
	if !errors.Is(err, gitpush.ErrUnavailable) {
		t.Fatalf("an unreadable reservation after the commit: %v", err)
	}
	f.Reservations.Fail = nil
	page, err := f.Svc.List(bg, "usr_reader", scope, RepoMain, gitpush.ListQuery{Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("list: %v %v", page, err)
	}
	rec3 := page[0].Record
	wantStatus(t, rec3, gitv1.PushAuthorized)
	replayed := f.MustAuthorize(req3)
	if !replayed.Allowed() || replayed.PushID != rec3.PushID {
		t.Fatalf("replay of the authorized record: %+v", replayed)
	}

	// Commit intent: the reservation is read again before intent is given.
	f.Reservations.Release(scope, "wi_rc3", "att_rc3")
	wantCode(t, f.Intent(rec3.PushID, req3), domain.CodeForbidden)
	wantStatus(t, f.Record(RepoMain, rec3.PushID), gitv1.PushAuthorized)
	f.Reservations.Hold(scope, "wi_rc3", "att_rc3")
	if err := f.Intent(rec3.PushID, req3); err != nil {
		t.Fatalf("intent with the reservation held: %v", err)
	}
	// An entry that already holds intent is not a new one: a repeat after a release succeeds.
	f.Reservations.Release(scope, "wi_rc3", "att_rc3")
	if err := f.Intent(rec3.PushID, req3); err != nil {
		t.Fatalf("a repeated intent: %v", err)
	}

	// A person's push is not subject to the reservation and does not read it.
	reads := f.Reservations.Reads()
	eng := f.Human(Engineer, RepoMain)
	if resp := f.MustAuthorize(Request(RepoMain, eng, "rcv_rc4", false, Create(Task("wi_rc4"), OID("6")))); !resp.Allowed() || f.Reservations.Reads() != reads {
		t.Fatalf("a person's task-branch push reads the reservation: reads %d -> %d", reads, f.Reservations.Reads())
	}
}
