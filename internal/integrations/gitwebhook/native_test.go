package gitwebhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
)

const (
	sha1A = "1111111111111111111111111111111111111111"
	sha1B = "2222222222222222222222222222222222222222"
	sha1C = "3333333333333333333333333333333333333333"
	sha1Z = "0000000000000000000000000000000000000000"
)

var (
	scopeA = domain.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	scopeB = domain.Scope{TenantID: "tenant-b", ProjectID: "project-b"}
	t0     = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

// native is the native trigger wired over its doubles.
type native struct {
	t        *testing.T
	store    *gitwebhooktest.NativeStore
	pushes   *gitwebhooktest.PushRecords
	runs     *gitwebhooktest.SourceRuns
	work     *gitwebhooktest.WorkItems
	repos    *gitwebhooktest.NativeRepositories
	auth     *gitwebhooktest.Authorizer
	defs     *gitwebhooktest.Definitions
	config   *gw.NativeConfig
	consumer *gw.PushConsumer
	router   *gw.TaskRouter
	seq      int
	// clock is the configuration's time; tests move it.
	clock time.Time
}

func newNative(t *testing.T) *native {
	t.Helper()
	n := &native{
		t: t, store: gitwebhooktest.NewNativeStore(), pushes: &gitwebhooktest.PushRecords{}, runs: &gitwebhooktest.SourceRuns{},
		work: &gitwebhooktest.WorkItems{}, repos: &gitwebhooktest.NativeRepositories{}, auth: &gitwebhooktest.Authorizer{}, defs: &gitwebhooktest.Definitions{},
		clock: t0,
	}
	n.repos.Put(scopeA, gw.RepositoryFacts{ID: "repo_1", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady})
	n.repos.Put(scopeA, gw.RepositoryFacts{ID: "repo_2", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady})
	n.defs.Register(scopeA, "pd_ci")
	n.defs.Register(scopeA, "pd_task")
	for _, action := range []gw.Action{gw.ActionInstall, gw.ActionView} {
		n.auth.Grant("admin", scopeA, action)
		n.auth.Grant("admin2", scopeA, action)
	}
	var err error
	n.config, err = gw.NewNativeConfig(gw.NativeConfigDependencies{
		Store: n.store, Authorizer: n.auth, Definitions: n.defs, Repositories: n.repos, Now: func() time.Time { return n.clock },
		NewID: func() (string, error) { n.seq++; return fmt.Sprintf("trg_%d", n.seq), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := gw.ConsumerDependencies{Store: n.store, Pushes: n.pushes, Work: n.work, Runs: n.runs, Holder: "consumer-1", Now: func() time.Time { return t0 }}
	if n.consumer, err = gw.NewPushConsumer(deps); err != nil {
		t.Fatal(err)
	}
	if n.router, err = gw.NewTaskRouter(deps); err != nil {
		t.Fatal(err)
	}
	return n
}

func (n *native) trigger(repo string, patterns ...string) gw.PushTrigger {
	n.t.Helper()
	tr, err := n.config.CreateTrigger(context.Background(), gw.CreateTriggerCommand{
		Scope: scopeA, Principal: "admin", RepositoryID: repo, RefPatterns: patterns, DefinitionID: "pd_ci", Enabled: true,
	})
	if err != nil {
		n.t.Fatalf("create trigger: %v", err)
	}
	return tr
}

func entry(ref, old, new string, kind gitv1.UpdateKind, state gitv1.EntryState) gitv1.PushEntry {
	e := gitv1.PushEntry{Ref: ref, Old: old, New: new, Kind: kind, State: state}
	if state == gitv1.EntryRefused {
		r := gitv1.ReasonRefProtected
		e.Reason = &r
	}
	return e
}

func applied(ref, old, new string) gitv1.PushEntry {
	kind := gitv1.KindFastForward
	switch {
	case old == sha1Z:
		kind = gitv1.KindCreate
	case new == sha1Z:
		kind = gitv1.KindDelete
	}
	return entry(ref, old, new, kind, gitv1.EntryApplied)
}

// record builds a valid push record of a person (run == "") or of a run.
func record(id, repo, run string, entries ...gitv1.PushEntry) gitv1.PushRecord {
	rec := gitv1.PushRecord{
		PushID: id, RepositoryID: repo, ReceiveID: "rcv_" + id, GrantID: "grant_1", Entries: entries, CreatedAt: "2026-03-01T12:00:00Z",
		Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: "usr_1"},
	}
	if run != "" {
		rec.Actor = gitv1.Actor{Type: gitv1.ActorWorkload, ID: "att_1", RunID: &run}
		rec.OriginRunID = &run
	}
	rec.Status = gitv1.DeriveStatus(entries)
	return rec
}

// push stores the record under scope and returns its git.refs_updated envelope.
func (n *native) push(scope domain.Scope, rec gitv1.PushRecord) eventsv1.Envelope {
	n.t.Helper()
	n.pushes.Put(scope, rec)
	return refsUpdated(n.t, scope, rec)
}

func refsUpdated(t *testing.T, scope domain.Scope, rec gitv1.PushRecord) eventsv1.Envelope {
	t.Helper()
	payload, err := eventsv1.NewGitRefsUpdated(rec)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env := eventsv1.Envelope{
		ID: "evt_" + rec.ReceiveID, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeGitRefsUpdated, OccurredAt: t0,
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, Resource: eventsv1.Resource{Type: eventsv1.ResourceTypeGitPush, ID: rec.ReceiveID, Version: 2},
		Actor: eventsv1.Actor{Type: eventsv1.ActorService, ID: "git"}, CorrelationID: "corr_1", Data: data,
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("the test event is invalid: %v", err)
	}
	return env
}

func workEvent(t *testing.T, scope domain.Scope, typ, item string, payload any) eventsv1.Envelope {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env := eventsv1.Envelope{
		ID: fmt.Sprintf("evt_%s_%d", item, time.Now().UnixNano()), SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: t0,
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, Resource: eventsv1.Resource{Type: "work_item", ID: item, Version: 3},
		Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_1"}, CorrelationID: "corr_1", Data: data,
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("the test event is invalid: %v", err)
	}
	return env
}

func becameReady(t *testing.T, scope domain.Scope, item, transition string) eventsv1.Envelope {
	return workEvent(t, scope, eventsv1.TypeWorkItemTransitioned, item, eventsv1.WorkItemTransitioned{From: "backlog", To: "ready", TransitionID: transition})
}

func (n *native) handle(env eventsv1.Envelope) []gw.Admission {
	n.t.Helper()
	out, err := n.consumer.HandleRefsUpdated(context.Background(), env)
	if err != nil {
		n.t.Fatalf("HandleRefsUpdated: %v", err)
	}
	return out
}

func started(out []gw.Admission) (count int) {
	for _, a := range out {
		if a.Outcome == gw.OutcomeStarted {
			count++
		}
	}
	return count
}

// Test_unit_trigger_dedup_by_push_and_ref: a push record starts at most one run
// per (trigger, push record, ref), whatever the redelivery, restart, concurrency
// or lost acknowledgement (GR-10, PX-24, GR-A07).
func Test_unit_trigger_dedup_by_push_and_ref(t *testing.T) {
	t.Run("redelivery replays the decided keys and starts nothing", func(t *testing.T) {
		n := newNative(t)
		tr := n.trigger("repo_1", "refs/heads/main", "refs/heads/release/*")
		env := n.push(scopeA, record("push_1", "repo_1", "",
			applied("refs/heads/main", sha1A, sha1B), applied("refs/heads/release/1", sha1Z, sha1C)))
		first := n.handle(env)
		if started(first) != 2 || n.runs.Created() != 2 {
			t.Fatalf("two refs, two runs: %+v (%d)", first, n.runs.Created())
		}
		for i := 0; i < 3; i++ {
			again := n.handle(env)
			if len(again) != 2 || started(again) != 2 || n.runs.Created() != 2 {
				t.Fatalf("redelivery %d: %+v (%d runs)", i, again, n.runs.Created())
			}
			for _, a := range again {
				if !a.Replay {
					t.Fatalf("a decided key is a replay: %+v", a)
				}
			}
		}
		if len(n.runs.Requests) != 2 {
			t.Fatalf("admission was called once per key, not per delivery: %d", len(n.runs.Requests))
		}
		// The run identity is derived from (trigger, push, ref) and nothing else.
		want := gw.InboxKey{Scope: scopeA, Kind: gw.InboxPush, Source: tr.ID(), Subject: "push_1", Ref: "refs/heads/main"}.RunID()
		var found bool
		for _, r := range n.runs.Runs() {
			found = found || r.Request.RunID == want
		}
		if !found {
			t.Fatalf("run ID %s is not among the created runs", want)
		}
		key := gw.InboxKey{Scope: scopeA, Kind: gw.InboxPush, Source: tr.ID(), Subject: "push_1", Ref: "refs/heads/main"}
		if got := key.RunID(); got != want {
			t.Fatalf("the run ID is stable: %s %s", got, want)
		}
	})

	t.Run("another push, ref or trigger is another key", func(t *testing.T) {
		n := newNative(t)
		t1 := n.trigger("repo_1", "refs/heads/main")
		t2 := n.trigger("repo_1", "refs/heads/main")
		n.handle(n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
		if n.runs.Created() != 2 {
			t.Fatalf("two triggers start one run each: %d", n.runs.Created())
		}
		n.handle(n.push(scopeA, record("push_2", "repo_1", "", applied("refs/heads/main", sha1B, sha1C))))
		if n.runs.Created() != 4 {
			t.Fatalf("a later push on the same ref is a new key: %d", n.runs.Created())
		}
		ids := map[domain.RunID]bool{}
		for _, r := range n.runs.Runs() {
			ids[r.Request.RunID] = true
		}
		if len(ids) != 4 {
			t.Fatalf("distinct run identities: %v", ids)
		}
		_ = t1
		_ = t2
	})

	t.Run("two consumers racing on one event start one run", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		second, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: n.store, Pushes: n.pushes, Runs: n.runs, Holder: "consumer-2", Now: func() time.Time { return t0 }})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, c := range []*gw.PushConsumer{n.consumer, second, n.consumer, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A busy key is a retryable error; the loser's redelivery replays.
				if _, err := c.HandleRefsUpdated(context.Background(), env); err != nil && !errors.Is(err, gw.ErrUnavailable) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		n.handle(env)
		if n.runs.Created() != 1 {
			t.Fatalf("one run, got %d", n.runs.Created())
		}
	})

	t.Run("a lost admission response is recovered without a second run", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		n.runs.LoseResponse = true
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), env); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("a lost response is retryable: %v", err)
		}
		if n.runs.Created() != 1 {
			t.Fatalf("the run exists: %d", n.runs.Created())
		}
		out := n.handle(env)
		if started(out) != 1 || n.runs.Created() != 1 || len(n.runs.Requests) != 1 {
			t.Fatalf("recovered the run by identity, no second admission: %+v (%d requests)", out, len(n.runs.Requests))
		}
	})

	t.Run("a lost outcome record is recovered even after the configurer lost authority", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		n.store.FailFinishes = 1
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), env); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("a lost finish is retryable: %v", err)
		}
		// Admission would now refuse (authority revoked), yet the run exists.
		n.runs.Err = fmt.Errorf("%w: revoked", gw.ErrRunRefused)
		out := n.handle(env)
		if started(out) != 1 || n.runs.Created() != 1 {
			t.Fatalf("the existing run wins over a refusal: %+v", out)
		}
		if len(n.runs.Requests) != 1 {
			t.Fatalf("no second admission attempt: %d", len(n.runs.Requests))
		}
	})

	t.Run("a refused admission is decided once", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		n.runs.Err = fmt.Errorf("%w: definition refused", gw.ErrRunRefused)
		out := n.handle(env)
		if len(out) != 1 || out[0].Outcome != gw.OutcomeRejected || out[0].Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("refusal recorded: %+v", out)
		}
		n.runs.Err = nil // the cause is fixed later: the decided key is not retried
		out = n.handle(env)
		if out[0].Outcome != gw.OutcomeRejected || !out[0].Replay || n.runs.Created() != 0 || len(n.runs.Requests) != 1 {
			t.Fatalf("the refusal is replayed: %+v (%d requests)", out, len(n.runs.Requests))
		}
		var refused bool
		for _, a := range n.store.Audits() {
			refused = refused || (a.Action == gw.AuditPushRunRefused && a.Metadata["push"] == "push_1")
		}
		if !refused {
			t.Fatalf("the refusal is audited: %+v", n.store.Audits())
		}
	})

	t.Run("an outage is retried and decides nothing", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		n.runs.Err, n.runs.ErrOnce = fmt.Errorf("%w: quota", gw.ErrUnavailable), true
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), env); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("an outage: %v", err)
		}
		if out := n.handle(env); started(out) != 1 || n.runs.Created() != 1 {
			t.Fatalf("the retry starts the run: %+v", out)
		}
	})
}

// Test_unit_trigger_excludes_origin_run: a push made by a run is excluded from
// the event alone, before anything is reloaded; a person's push (explicit null)
// is not; excludeOriginRuns=false opts in (GR-11).
func Test_unit_trigger_excludes_origin_run(t *testing.T) {
	n := newNative(t)
	dflt := n.trigger("repo_1", "refs/heads/main")
	if !dflt.Excludes() || dflt.Config.ExcludeOriginRuns == nil || !*dflt.Config.ExcludeOriginRuns {
		t.Fatalf("an omitted excludeOriginRuns is stored as true: %+v", dflt.Config)
	}
	runPush := record("push_run", "repo_1", "run_9", applied("refs/heads/main", sha1A, sha1B))
	env := n.push(scopeA, runPush)

	out := n.handle(env)
	if started(out) != 0 || n.runs.Created() != 0 || len(out) != 1 || out[0].Reason != gw.ReasonOriginRunExcluded {
		t.Fatalf("a run's own push starts nothing: %+v", out)
	}
	if n.pushes.Reloads != 0 {
		t.Fatalf("the exclusion is decided from the event alone, the record was reloaded %d times", n.pushes.Reloads)
	}
	if _, ok := n.store.Inbox(gw.InboxKey{Scope: scopeA, Kind: gw.InboxPush, Source: dflt.ID(), Subject: "push_run", Ref: "refs/heads/main"}); ok {
		t.Fatalf("an excluded push is never claimed")
	}

	person := n.push(scopeA, record("push_person", "repo_1", "", applied("refs/heads/main", sha1B, sha1C)))
	if out := n.handle(person); started(out) != 1 {
		t.Fatalf("a person's push (originRunId null) starts a run: %+v", out)
	}

	optIn, err := n.config.CreateTrigger(context.Background(), gw.CreateTriggerCommand{
		Scope: scopeA, Principal: "admin", RepositoryID: "repo_1", RefPatterns: []string{"refs/heads/main"}, DefinitionID: "pd_ci",
		ExcludeOriginRuns: ptr(false), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out = n.handle(env)
	if started(out) != 1 {
		t.Fatalf("only the trigger that opted in reacts to a run's push: %+v", out)
	}
	var reacted string
	for _, a := range out {
		if a.Outcome == gw.OutcomeStarted {
			reacted = a.Source
		}
	}
	if reacted != optIn.ID() {
		t.Fatalf("started by %s, want the opted-in %s", reacted, optIn.ID())
	}

	// The record is authoritative about who pushed: an event that says a person
	// pushed (null) for a run's push is still excluded.
	forged := runPush
	forged.PushID = "push_run2"
	forged.ReceiveID = "rcv_push_run2"
	n.pushes.Put(scopeA, forged)
	noRun := refsUpdated(t, scopeA, forged)
	var payload eventsv1.GitRefsUpdated
	_ = json.Unmarshal(noRun.Data, &payload)
	payload.OriginRunID, payload.Actor = nil, eventsv1.GitRefActor{Type: "user", ID: "usr_1"}
	noRun.Data, _ = json.Marshal(payload)
	before := n.runs.Created()
	out = n.handle(noRun)
	if n.runs.Created() != before+1 { // only the opted-in trigger
		t.Fatalf("the record's origin excludes it for the default trigger: %+v", out)
	}
}

func ptr[T any](v T) *T { return &v }

// Test_unit_trigger_exact_pushed_commit: the run's source is the `new` value of
// the applied entry of the record, never a later branch tip, a delete or a
// refused entry (PX-24, GR-A07).
func Test_unit_trigger_exact_pushed_commit(t *testing.T) {
	n := newNative(t)
	n.trigger("repo_1", "refs/heads/main", "refs/heads/dev")
	rec := record("push_1", "repo_1", "",
		applied("refs/heads/main", sha1A, sha1B),
		applied("refs/heads/dev", sha1Z, sha1A),
		entry("refs/heads/release", sha1A, sha1C, gitv1.KindFastForward, gitv1.EntryRefused),
		applied("refs/heads/gone", sha1A, sha1Z))
	env := n.push(scopeA, rec)
	out := n.handle(env)
	if started(out) != 2 {
		t.Fatalf("main and dev start: %+v", out)
	}
	pinned := map[string]string{}
	for _, r := range n.runs.Runs() {
		pinned[r.Request.Origin.Ref] = r.Commit
		if r.Request.Origin.PushID != "push_1" || r.Request.Origin.RepositoryID != "repo_1" {
			t.Fatalf("origin names the record and repository: %+v", r.Request.Origin)
		}
	}
	if pinned["refs/heads/main"] != sha1B || pinned["refs/heads/dev"] != sha1A {
		t.Fatalf("each run pins its own entry's new commit: %+v", pinned)
	}

	// The branch moves on: a later push is a new record. Replaying the first
	// event changes nothing, and the first run still pins B.
	n.handle(n.push(scopeA, record("push_2", "repo_1", "", applied("refs/heads/main", sha1B, sha1C))))
	n.handle(env)
	pinned = map[string]string{}
	for _, r := range n.runs.Runs() {
		pinned[r.Request.Origin.PushID+r.Request.Origin.Ref] = r.Commit
	}
	if pinned["push_1refs/heads/main"] != sha1B || pinned["push_2refs/heads/main"] != sha1C || len(pinned) != 3 {
		t.Fatalf("pins after the branch moved: %+v", pinned)
	}

	// Deletes, refused entries and unrelated refs are never admitted; the
	// request carries no tip, only the record's commit.
	for _, a := range out {
		if a.Ref == "refs/heads/gone" || a.Ref == "refs/heads/release" {
			t.Fatalf("an unrelated ref produced a decision: %+v", a)
		}
	}
	del := n.trigger("repo_1", "refs/heads/gone")
	out = n.handle(env)
	var sawDelete bool
	for _, a := range out {
		if a.Source == del.ID() && a.Ref == "refs/heads/gone" {
			sawDelete = true
			if a.Outcome != gw.OutcomeIgnored || a.Reason != gw.ReasonRefDeleted {
				t.Fatalf("a delete is ignored: %+v", a)
			}
		}
	}
	if !sawDelete {
		t.Fatalf("the delete entry was not considered: %+v", out)
	}
	refusing := n.trigger("repo_1", "refs/heads/release")
	for _, a := range n.handle(env) {
		if a.Source == refusing.ID() && (a.Outcome != gw.OutcomeIgnored || a.Reason != gw.ReasonEntryNotApplied) {
			t.Fatalf("a refused entry is ignored: %+v", a)
		}
	}
	for _, r := range n.runs.Runs() {
		if r.Commit == "" || r.Commit == sha1Z {
			t.Fatalf("a run without an exact commit: %+v", r)
		}
	}

	t.Run("a record that is not applied or not the event's starts nothing", func(t *testing.T) {
		n := newNative(t)
		n.trigger("repo_1", "refs/heads/main")
		open := record("push_open", "repo_1", "", entry("refs/heads/main", sha1A, sha1B, gitv1.KindFastForward, gitv1.EntryAuthorized))
		env := refsUpdated2(t, scopeA, "push_open", "repo_1")
		n.pushes.Put(scopeA, open)
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), env); !errors.Is(err, gw.ErrInvalid) || n.runs.Created() != 0 {
			t.Fatalf("an unsettled record: %v", err)
		}
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), refsUpdated2(t, scopeA, "push_missing", "repo_1")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a dangling event: %v", err)
		}
		// A record of another tenant under the same push ID is never read.
		n.pushes.Put(scopeB, record("push_x", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), refsUpdated2(t, scopeA, "push_x", "repo_1")); !errors.Is(err, gw.ErrNotFound) || n.runs.Created() != 0 {
			t.Fatalf("a foreign record: %v", err)
		}
	})
}

// refsUpdated2 is an event whose record may not exist or match.
func refsUpdated2(t *testing.T, scope domain.Scope, push, repo string) eventsv1.Envelope {
	t.Helper()
	return refsUpdated(t, scope, record(push, repo, "", applied("refs/heads/main", sha1A, sha1B)))
}

// Test_unit_trigger_without_work_item: a push run starts without a work item,
// workspace or content revision (GR-A07, PX-24); a trigger has no work item
// member at all.
func Test_unit_trigger_without_work_item(t *testing.T) {
	n := newNative(t)
	n.trigger("repo_1", "refs/heads/main")
	n.handle(n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
	if len(n.runs.Requests) != 1 {
		t.Fatalf("one admission: %d", len(n.runs.Requests))
	}
	req := n.runs.Requests[0]
	if req.Origin.WorkItemID != "" || req.ContentRevision != 0 {
		t.Fatalf("a push run has no work item and no content revision: %+v", req)
	}
	if req.Origin.PushID == "" || req.Origin.RepositoryID == "" || req.Origin.Ref == "" {
		t.Fatalf("a push run names its push, repository and ref: %+v", req.Origin)
	}
	if req.DefinitionID != "pd_ci" || req.Principal != "admin" || req.RunID == "" || req.Scope != scopeA {
		t.Fatalf("request: %+v", req)
	}
	// The stored trigger shape cannot carry a work item.
	raw, err := json.Marshal(n.trigger("repo_1", "refs/heads/x").Config)
	if err != nil || strings.Contains(strings.ToLower(string(raw)), "workitem") {
		t.Fatalf("a trigger configuration never names a work item: %s %v", raw, err)
	}
}

// Test_unit_trigger_repository_and_ref_filters: repository and ref patterns
// select the pushes a trigger reacts to; scope, enablement and the repository of
// the record bound every decision (GR-10).
func Test_unit_trigger_repository_and_ref_filters(t *testing.T) {
	n := newNative(t)
	main1 := n.trigger("repo_1", "refs/heads/main")
	rel1 := n.trigger("repo_1", "refs/heads/release/*", "refs/tags/v1")
	n.trigger("repo_2", "refs/heads/main")
	off := n.trigger("repo_1", "refs/heads/main")
	if _, err := n.config.UpdateTrigger(context.Background(), "admin", scopeA, off.ID(), 1, gw.TriggerPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	// A trigger of another tenant for the same repository ID.
	n.repos.Put(scopeB, gw.RepositoryFacts{ID: "repo_1", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady})
	n.defs.Register(scopeB, "pd_ci")
	n.auth.Grant("admin", scopeB, gw.ActionInstall)
	if _, err := n.config.CreateTrigger(context.Background(), gw.CreateTriggerCommand{Scope: scopeB, Principal: "admin", RepositoryID: "repo_1", RefPatterns: []string{"refs/heads/main"}, DefinitionID: "pd_ci", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	startedBy := func(out []gw.Admission) map[string][]string {
		m := map[string][]string{}
		for _, a := range out {
			if a.Outcome == gw.OutcomeStarted {
				m[a.Source] = append(m[a.Source], a.Ref)
			}
		}
		return m
	}

	out := n.handle(n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
	if got := startedBy(out); len(got) != 1 || len(got[main1.ID()]) != 1 {
		t.Fatalf("only the matching, enabled trigger of the pushed repository runs: %+v", got)
	}
	out = n.handle(n.push(scopeA, record("push_2", "repo_1", "",
		applied("refs/heads/release/2", sha1Z, sha1B), applied("refs/heads/release", sha1Z, sha1B), applied("refs/heads/feature", sha1Z, sha1C), applied("refs/tags/v1", sha1Z, sha1C))))
	got := startedBy(out)
	if len(got) != 1 || len(got[rel1.ID()]) != 2 {
		t.Fatalf("the wildcard matches one or more further characters and the exact tag matches: %+v", got)
	}
	for _, r := range got[rel1.ID()] {
		if r != "refs/heads/release/2" && r != "refs/tags/v1" {
			t.Fatalf("unexpected ref %s", r)
		}
	}
	out = n.handle(n.push(scopeA, record("push_3", "repo_2", "", applied("refs/heads/main", sha1A, sha1B))))
	if got := startedBy(out); len(got) != 1 {
		t.Fatalf("repo_2's own trigger: %+v", got)
	}
	out = n.handle(n.push(scopeA, record("push_4", "repo_1", "", applied("refs/heads/feature", sha1A, sha1B))))
	if len(out) != 0 || len(n.runs.Requests) != 4 {
		t.Fatalf("no pattern matches a feature branch: %+v (%d admissions)", out, len(n.runs.Requests))
	}
	for _, r := range n.runs.Runs() {
		if r.Request.Scope != scopeA {
			t.Fatalf("a run was admitted outside the event's scope: %+v", r.Request)
		}
	}
	// A tenant-B event never reaches tenant A's triggers.
	n.pushes.Put(scopeB, record("push_b", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
	before := len(n.runs.Requests)
	n.handle(refsUpdated(t, scopeB, record("push_b", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
	if len(n.runs.Requests) != before+1 || n.runs.Requests[before].Scope != scopeB {
		t.Fatalf("scope B's event starts only scope B's trigger: %+v", n.runs.Requests[before:])
	}

	t.Run("malformed events are refused", func(t *testing.T) {
		env := refsUpdated(t, scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		wrong := env
		wrong.Type = eventsv1.TypeWorkItemTransitioned
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), wrong); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("another event type: %v", err)
		}
		noProject := env
		noProject.ProjectID = ""
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), noProject); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("a tenant-wide event: %v", err)
		}
		mismatch := env
		mismatch.Resource.ID = "rcv_other"
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), mismatch); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("resource id differs from the payload's receive: %v", err)
		}
		// A receive no record settled is a dangling event.
		dangling := record("push_9", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))
		if _, err := n.consumer.HandleRefsUpdated(context.Background(), refsUpdated(t, scopeA, dangling)); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an event of a receive with no record: %v", err)
		}
	})

	t.Run("a store that returns a foreign trigger is not obeyed", func(t *testing.T) {
		n := newNative(t)
		consumer, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: wrongRepoStore{n.store}, Pushes: n.pushes, Runs: n.runs, Holder: "c"})
		if err != nil {
			t.Fatal(err)
		}
		n.trigger("repo_2", "refs/heads/main")
		env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		if out, err := consumer.HandleRefsUpdated(context.Background(), env); err != nil || len(out) != 0 || n.runs.Created() != 0 {
			t.Fatalf("%+v %v", out, err)
		}
	})
}

// wrongRepoStore answers EnabledPushTriggers for the wrong repository, as a
// faulty adapter might.
type wrongRepoStore struct{ gw.NativeStore }

func (w wrongRepoStore) EnabledPushTriggers(ctx context.Context, scope domain.Scope, _ string) ([]gw.PushTrigger, error) {
	return w.NativeStore.EnabledPushTriggers(ctx, scope, "repo_2")
}

// Test_unit_trigger_configuration_authorized: only a principal with the project
// settings authority creates, alters or deletes a trigger or the task route; a
// hidden object is a missing one; the configurer's current authority is what
// admission checks (GR-10).
func Test_unit_trigger_configuration_authorized(t *testing.T) {
	ctx := context.Background()
	n := newNative(t)
	create := func(principal string, mutate func(*gw.CreateTriggerCommand)) (gw.PushTrigger, error) {
		cmd := gw.CreateTriggerCommand{Scope: scopeA, Principal: principal, RepositoryID: "repo_1", RefPatterns: []string{"refs/heads/main"}, DefinitionID: "pd_ci", Enabled: true}
		if mutate != nil {
			mutate(&cmd)
		}
		return n.config.CreateTrigger(ctx, cmd)
	}

	n.auth.Grant("viewer", scopeA, gw.ActionView)
	n.auth.Grant("runner", scopeA, gw.ActionRun)
	for _, p := range []string{"", "nobody", "viewer", "runner"} {
		if _, err := create(p, nil); !errors.Is(err, gw.ErrDenied) {
			t.Fatalf("principal %q created a trigger: %v", p, err)
		}
	}
	if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.Scope = scopeB }); !errors.Is(err, gw.ErrDenied) {
		t.Fatalf("authority in one tenant is none in another: %v", err)
	}
	tr, err := create("admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ConfiguredBy != "admin" || tr.Version != 1 || !tr.Config.Enabled || !tr.Excludes() {
		t.Fatalf("created: %+v", tr)
	}
	if audits := n.store.Audits(); len(audits) != 1 || audits[0].Action != gw.AuditPushTriggerCreated || audits[0].Principal != "admin" {
		t.Fatalf("creation is audited with its principal: %+v", audits)
	}

	// Every mutation and read is authorized, with the same denial.
	denied := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, gw.ErrDenied) {
			t.Errorf("%s by a viewer: %v", name, err)
		}
	}
	_, err = n.config.UpdateTrigger(ctx, "viewer", scopeA, tr.ID(), 1, gw.TriggerPatch{Enabled: ptr(false)})
	denied("update", err)
	_, err = n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "viewer", DefinitionID: "pd_task", Enabled: true})
	denied("route", err)
	_, err = n.config.GetTrigger(ctx, "nobody", scopeA, tr.ID())
	denied("get", err)
	_, err = n.config.ListTriggers(ctx, "nobody", scopeA, "", 10)
	denied("list", err)
	_, err = n.config.GetTaskRoute(ctx, "nobody", scopeA)
	denied("get route", err)
	if got, err := n.config.GetTrigger(ctx, "viewer", scopeA, tr.ID()); err != nil || got.ID() != tr.ID() {
		t.Fatalf("a viewer reads: %+v %v", got, err)
	}
	if page, err := n.config.ListTriggers(ctx, "viewer", scopeA, "", 10); err != nil || len(page) != 1 {
		t.Fatalf("a viewer lists: %+v %v", page, err)
	}
	if got, _ := n.store.PushTrigger(ctx, scopeA, tr.ID()); got.Version != 1 || !got.Config.Enabled {
		t.Fatalf("a denied change changed nothing: %+v", got)
	}

	// Authority is checked with current state: revocation takes effect at once.
	n.auth.Revoke("admin2", scopeA, gw.ActionInstall)
	if _, err := n.config.UpdateTrigger(ctx, "admin2", scopeA, tr.ID(), 1, gw.TriggerPatch{Enabled: ptr(false)}); !errors.Is(err, gw.ErrDenied) {
		t.Fatalf("a revoked administrator: %v", err)
	}
	n.auth.Grant("admin2", scopeA, gw.ActionInstall)

	t.Run("validation", func(t *testing.T) {
		for name, mutate := range map[string]func(*gw.CreateTriggerCommand){
			"bare heads wildcard":   func(c *gw.CreateTriggerCommand) { c.RefPatterns = []string{"refs/heads/*"} },
			"server ref":            func(c *gw.CreateTriggerCommand) { c.RefPatterns = []string{"refs/kodama/runs/*"} },
			"inner wildcard":        func(c *gw.CreateTriggerCommand) { c.RefPatterns = []string{"refs/heads/a*/b"} },
			"no patterns":           func(c *gw.CreateTriggerCommand) { c.RefPatterns = nil },
			"too many patterns":     func(c *gw.CreateTriggerCommand) { c.RefPatterns = manyPatterns(33) },
			"branch name not a ref": func(c *gw.CreateTriggerCommand) { c.RefPatterns = []string{"main"} },
			"malformed definition":  func(c *gw.CreateTriggerCommand) { c.DefinitionID = "bad id" },
		} {
			if _, err := create("admin", mutate); !errors.Is(err, gw.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.RepositoryID = "repo_missing" }); !errors.Is(err, gw.ErrNotFound) {
			t.Errorf("an unknown repository: %v", err)
		}
		n.repos.Put(scopeB, gw.RepositoryFacts{ID: "repo_foreign", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady})
		if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.RepositoryID = "repo_foreign" }); !errors.Is(err, gw.ErrNotFound) {
			t.Errorf("a repository of another tenant is a missing one: %v", err)
		}
		n.repos.Put(scopeA, gw.RepositoryFacts{ID: "repo_ext", Hosting: gitv1.HostingExternal, Status: gitv1.StatusReady})
		if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.RepositoryID = "repo_ext" }); !errors.Is(err, gw.ErrHostingMismatch) || !errors.Is(err, gw.ErrInvalid) {
			t.Errorf("an externally hosted repository keeps the HMAC trigger: %v", err)
		}
		n.repos.Put(scopeA, gw.RepositoryFacts{ID: "repo_old", Hosting: gitv1.HostingInternal, Status: gitv1.StatusArchived})
		if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.RepositoryID = "repo_old" }); !errors.Is(err, gw.ErrRepositoryArchived) || !errors.Is(err, gw.ErrInvalid) {
			t.Errorf("an archived repository: %v", err)
		}
		if _, err := create("admin", func(c *gw.CreateTriggerCommand) { c.DefinitionID = "pd_unknown" }); !errors.Is(err, gw.ErrNotFound) {
			t.Errorf("an unregistered definition: %v", err)
		}
		n.auth.Err = errors.New("iam down")
		if _, err := create("admin", nil); !errors.Is(err, gw.ErrUnavailable) {
			t.Errorf("an authorization outage is never access: %v", err)
		}
		n.auth.Err = nil
		if got, _ := n.store.ListPushTriggers(ctx, scopeA, "", 100); len(got) != 1 {
			t.Errorf("refused creations stored nothing: %d", len(got))
		}
	})

	t.Run("alteration", func(t *testing.T) {
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 7, gw.TriggerPatch{Enabled: ptr(false)}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, "trg_unknown", 1, gw.TriggerPatch{Enabled: ptr(false)}); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unknown trigger: %v", err)
		}
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeB, tr.ID(), 1, gw.TriggerPatch{}); !errors.Is(err, gw.ErrDenied) {
			t.Fatalf("a trigger addressed through another scope: %v", err)
		}
		bad := []string{"refs/heads/*"}
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 1, gw.TriggerPatch{RefPatterns: &bad}); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("an invalid pattern: %v", err)
		}
		missing := domain.DefinitionID("pd_unknown")
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 1, gw.TriggerPatch{DefinitionID: &missing}); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unregistered definition: %v", err)
		}
		patterns := []string{"refs/heads/main", "refs/heads/dev/*"}
		upd, err := n.config.UpdateTrigger(ctx, "admin2", scopeA, tr.ID(), 1, gw.TriggerPatch{RefPatterns: &patterns, ExcludeOriginRuns: ptr(false)})
		if err != nil || upd.Version != 2 || upd.ConfiguredBy != "admin2" || upd.Excludes() || len(upd.Config.RefPatterns) != 2 || upd.Config.RepositoryID != "repo_1" {
			t.Fatalf("alteration: %+v %v", upd, err)
		}
		// Admission is attributed to the principal who last configured it.
		n.handle(n.push(scopeA, record("push_cfg", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
		if got := n.runs.Requests[len(n.runs.Requests)-1].Principal; got != "admin2" {
			t.Fatalf("admission checks the configurer's current authority: %q", got)
		}
		// Disabling is an alteration like another: versioned and authorized.
		if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 1, gw.TriggerPatch{Enabled: ptr(false)}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale disable: %v", err)
		}
		off, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 2, gw.TriggerPatch{Enabled: ptr(false)})
		if err != nil || off.Config.Enabled || off.Version != 3 || off.ConfiguredBy != "admin" {
			t.Fatalf("disable: %+v %v", off, err)
		}
		// A disabled trigger starts nothing, and a redelivery does not start the
		// decided key again.
		env := n.push(scopeA, record("push_cfg", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))
		before := len(n.runs.Requests)
		n.handle(env)
		n.handle(n.push(scopeA, record("push_cfg2", "repo_1", "", applied("refs/heads/main", sha1B, sha1C))))
		if len(n.runs.Requests) != before {
			t.Fatalf("a disabled trigger starts nothing")
		}
	})

	t.Run("task route", func(t *testing.T) {
		route, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_task", Enabled: true})
		if err != nil || route.Version != 1 || route.ConfiguredBy != "admin" {
			t.Fatalf("%+v %v", route, err)
		}
		if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_unknown", Enabled: true, ExpectedVersion: 1}); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("an unregistered definition: %v", err)
		}
		if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_task", Enabled: true}); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a second creation: %v", err)
		}
		if got, err := n.config.GetTaskRoute(ctx, "viewer", scopeA); err != nil || got.DefinitionID != "pd_task" {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("construction fails closed", func(t *testing.T) {
		if _, err := gw.NewNativeConfig(gw.NativeConfigDependencies{Store: n.store, Definitions: n.defs, Repositories: n.repos}); err == nil {
			t.Fatalf("no authorizer, no allow-all default")
		}
		if _, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: n.store, Runs: n.runs, Holder: "c"}); err == nil {
			t.Fatalf("no push record reload")
		}
		if _, err := gw.NewTaskRouter(gw.ConsumerDependencies{Store: n.store, Runs: n.runs, Holder: "c"}); err == nil {
			t.Fatalf("no work item reads")
		}
		if _, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: n.store, Pushes: n.pushes, Runs: n.runs}); err == nil {
			t.Fatalf("no holder identity")
		}
	})
}

func manyPatterns(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("refs/heads/b%d", i)
	}
	return out
}

// Test_unit_trigger_task_ready_admission: a ready item starts one work_item run
// of the project's route through source-set admission, once per readiness
// transition, under the configurer's current authority and subject to what
// admission decides (quotas, cancellation) (GR-11, PX-A20).
func Test_unit_trigger_task_ready_admission(t *testing.T) {
	ctx := context.Background()
	n := newNative(t)
	if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_task", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ready := gw.WorkFacts{State: "ready", ContentRevision: 4, Actionable: true}
	n.work.Set(scopeA, "wi_1", ready)

	run := func(env eventsv1.Envelope) *gw.Admission {
		t.Helper()
		out, err := n.router.HandleWorkItemEvent(ctx, env)
		if err != nil {
			t.Fatalf("HandleWorkItemEvent: %v", err)
		}
		return out
	}

	out := run(becameReady(t, scopeA, "wi_1", "tr_1"))
	if out == nil || out.Outcome != gw.OutcomeStarted || n.runs.Created() != 1 {
		t.Fatalf("a ready item starts a run: %+v", out)
	}
	req := n.runs.Requests[0]
	if req.Origin.WorkItemID != "wi_1" || req.Origin.PushID != "" || req.ContentRevision != 4 || req.DefinitionID != "pd_task" || req.Principal != "admin" || req.Scope != scopeA {
		t.Fatalf("the work_item origin pins the item and its current content revision: %+v", req)
	}
	if again := run(becameReady(t, scopeA, "wi_1", "tr_1")); !again.Replay || again.RunID != out.RunID || n.runs.Created() != 1 || len(n.runs.Requests) != 1 {
		t.Fatalf("a redelivery of the transition is a replay: %+v", again)
	}
	if second := run(becameReady(t, scopeA, "wi_1", "tr_2")); second.Outcome != gw.OutcomeStarted || second.RunID == out.RunID || n.runs.Created() != 2 {
		t.Fatalf("leaving and re-entering ready is a new transition: %+v", second)
	}
	if created := run(workEvent(t, scopeA, eventsv1.TypeWorkItemCreated, "wi_1", eventsv1.WorkItemCreated{Kind: "task", State: "ready"})); created.Outcome != gw.OutcomeStarted || n.runs.Created() != 3 {
		t.Fatalf("an item created in ready: %+v", created)
	}

	t.Run("events that do not make an item ready are not ours", func(t *testing.T) {
		if got := run(workEvent(t, scopeA, eventsv1.TypeWorkItemTransitioned, "wi_1", eventsv1.WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_9"})); got != nil {
			t.Fatalf("to active: %+v", got)
		}
		if got := run(workEvent(t, scopeA, eventsv1.TypeWorkItemCreated, "wi_1", eventsv1.WorkItemCreated{Kind: "task", State: "backlog"})); got != nil {
			t.Fatalf("created in backlog: %+v", got)
		}
		if got := run(refsUpdated2(t, scopeA, "p", "repo_1")); got != nil {
			t.Fatalf("a push event: %+v", got)
		}
	})

	t.Run("the item's current state decides, and the decision is recorded", func(t *testing.T) {
		for name, tc := range map[string]struct {
			facts  gw.WorkFacts
			reason string
		}{
			"moved on":       {gw.WorkFacts{State: "active", ContentRevision: 4, Actionable: true}, gw.ReasonNotReady},
			"cancelled":      {gw.WorkFacts{State: "ready", ContentRevision: 4, Actionable: true, Cancelled: true}, gw.ReasonWorkCancelled},
			"epic":           {gw.WorkFacts{State: "ready", ContentRevision: 4}, gw.ReasonNotActionable},
			"blocked":        {gw.WorkFacts{State: "ready", ContentRevision: 4, Actionable: true, BlockedReason: "waiting"}, gw.ReasonWorkBlocked},
			"in backlog now": {gw.WorkFacts{State: "backlog", ContentRevision: 4, Actionable: true}, gw.ReasonNotReady},
		} {
			item := domain.WorkItemID("wi_" + strings.ReplaceAll(name, " ", "_"))
			n.work.Set(scopeA, item, tc.facts)
			before := n.runs.Created()
			got := run(becameReady(t, scopeA, string(item), "tr_x"))
			if got.Outcome != gw.OutcomeIgnored || got.Reason != tc.reason || n.runs.Created() != before {
				t.Errorf("%s: %+v", name, got)
			}
			// Becoming ready again later does not revive the decided transition.
			n.work.Set(scopeA, item, ready)
			if again := run(becameReady(t, scopeA, string(item), "tr_x")); !again.Replay || again.Outcome != gw.OutcomeIgnored || n.runs.Created() != before {
				t.Errorf("%s: a decided transition is not revived: %+v", name, again)
			}
		}
		if got := run(becameReady(t, scopeA, "wi_unknown", "tr_1")); got.Outcome != gw.OutcomeIgnored || got.Reason != gw.ReasonNotFoundTarget {
			t.Fatalf("a vanished item: %+v", got)
		}
	})

	t.Run("quota and outages defer, refusals are final", func(t *testing.T) {
		n.work.Set(scopeA, "wi_q", ready)
		n.runs.Err, n.runs.ErrOnce = fmt.Errorf("%w: concurrency limit reached", gw.ErrUnavailable), true
		before := n.runs.Created()
		if _, err := n.router.HandleWorkItemEvent(ctx, becameReady(t, scopeA, "wi_q", "tr_1")); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("a quota refusal is retried: %v", err)
		}
		if got := run(becameReady(t, scopeA, "wi_q", "tr_1")); got.Outcome != gw.OutcomeStarted || n.runs.Created() != before+1 {
			t.Fatalf("the retry starts it once: %+v", got)
		}
		n.work.Set(scopeA, "wi_r", ready)
		n.runs.Err = fmt.Errorf("%w: not actionable", gw.ErrRunRefused)
		if got := run(becameReady(t, scopeA, "wi_r", "tr_1")); got.Outcome != gw.OutcomeRejected || got.Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("a permanent refusal: %+v", got)
		}
		n.runs.Err = nil
		if got := run(becameReady(t, scopeA, "wi_r", "tr_1")); !got.Replay || got.Outcome != gw.OutcomeRejected {
			t.Fatalf("a refusal is decided once: %+v", got)
		}
		n.work.Err = errors.New("work store down")
		if _, err := n.router.HandleWorkItemEvent(ctx, becameReady(t, scopeA, "wi_s", "tr_1")); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("a work store outage: %v", err)
		}
		n.work.Err = nil
	})

	t.Run("no route, a disabled route and another project start nothing", func(t *testing.T) {
		if got := run(becameReady(t, scopeB, "wi_1", "tr_1")); got != nil {
			t.Fatalf("another tenant has no route: %+v", got)
		}
		route, err := n.store.TaskRoute(ctx, scopeA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin2", DefinitionID: "pd_task", Enabled: false, ExpectedVersion: route.Version}); err != nil {
			t.Fatal(err)
		}
		n.work.Set(scopeA, "wi_off", ready)
		before := len(n.runs.Requests)
		if got := run(becameReady(t, scopeA, "wi_off", "tr_1")); got != nil || len(n.runs.Requests) != before {
			t.Fatalf("a disabled route: %+v", got)
		}
		if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin2", DefinitionID: "pd_task", Enabled: true, ExpectedVersion: route.Version + 1}); err != nil {
			t.Fatal(err)
		}
		if got := run(becameReady(t, scopeA, "wi_off", "tr_1")); got == nil || got.Outcome != gw.OutcomeStarted {
			t.Fatalf("re-enabled: %+v", got)
		}
		if p := n.runs.Requests[len(n.runs.Requests)-1].Principal; p != "admin2" {
			t.Fatalf("the route's current configurer: %q", p)
		}
	})
}

// Test_unit_trigger_external_hmac_unchanged: the external receive-hook trigger
// keeps its wire protocol, its delivery protocol and its stores; native triggers
// neither share nor alter them (O-03, AC-07).
func Test_unit_trigger_external_hmac_unchanged(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	body := pushMain(f, gitwebhooktest.CommitA)

	// The signing string is the published one, verified independently of Sign.
	digest := sha256.Sum256(body)
	signing := strings.Join([]string{"kodama-git-hook-v1", string(f.Trigger.ID), "d1", fmt.Sprint(f.Clock.Now().Unix()), hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, f.Secret)
	mac.Write([]byte(signing))
	if want := "v1=" + hex.EncodeToString(mac.Sum(nil)); gw.Sign(f.Secret, f.Trigger.ID, "d1", f.Clock.Now().Unix(), body) != want {
		t.Fatalf("the HMAC signing string changed")
	}

	// The native path exists for the same project at the same time.
	n := newNative(t)
	n.trigger("repo_1", "refs/heads/main")
	n.handle(n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))

	r, err := f.Service.Receive(t.Context(), f.Sign("d1", body))
	if err != nil || r.Outcome != gw.OutcomeStarted || r.RunID == "" || r.Duplicate {
		t.Fatalf("a signed delivery starts one run: %+v %v", r, err)
	}
	if len(f.Runs.Requests) != 1 || f.Runs.Requests[0].IdempotencyKey != fmt.Sprintf("git-hook:%s:%s", f.Trigger.ID, "d1") ||
		f.Runs.Requests[0].Commit != gitwebhooktest.CommitA || f.Runs.Requests[0].RequestedBy != gitwebhooktest.Installer {
		t.Fatalf("admission request: %+v", f.Runs.Requests)
	}
	again, err := f.Service.Receive(t.Context(), f.Sign("d1", body))
	if err != nil || !again.Duplicate || again.RunID != r.RunID || f.Runs.Created() != 1 {
		t.Fatalf("a replayed delivery: %+v %v", again, err)
	}
	forged := f.Sign("d2", body)
	forged.Body = pushMain(f, gitwebhooktest.CommitB)
	if _, err := f.Service.Receive(t.Context(), forged); !errors.Is(err, gw.ErrUnauthenticated) || f.Runs.Created() != 1 {
		t.Fatalf("a forged delivery: %v", err)
	}
	// The HMAC trigger shape is the external one: a secret, branch names and no
	// native members.
	if f.Trigger.SecretRef == "" || len(f.Trigger.Branches) != 2 {
		t.Fatalf("trigger shape: %+v", f.Trigger)
	}
	if n.runs.Created() != 1 {
		t.Fatalf("the native run is independent: %d", n.runs.Created())
	}
}

// finishHookStore runs before once, just before the wrapped store sees the
// next FinishInbox: the window between an attempt's last check and its write.
type finishHookStore struct {
	gw.NativeStore
	before func()
}

func (h *finishHookStore) FinishInbox(ctx context.Context, key gw.InboxKey, fin gw.InboxFinish, now time.Time, audit gw.AuditRecord) (gw.InboxRecord, error) {
	if h.before != nil {
		before := h.before
		h.before = nil
		before()
	}
	return h.NativeStore.FinishInbox(ctx, key, fin, now, audit)
}

// Test_unit_trigger_stalled_attempt_cannot_record_a_refusal: an attempt whose
// lease lapsed while another attempt created the run is fenced out of recording
// `rejected`; its error is the retryable busy class, and the redelivery replays
// the run (review findings 2 and 3).
func Test_unit_trigger_stalled_attempt_cannot_record_a_refusal(t *testing.T) {
	ctx := context.Background()
	n := newNative(t)
	n.trigger("repo_1", "refs/heads/main")
	env := n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B)))

	clock := t0
	hook := &finishHookStore{NativeStore: n.store}
	stalled, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: hook, Pushes: n.pushes, Runs: n.runs, Holder: "same-process", Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	other, err := gw.NewPushConsumer(gw.ConsumerDependencies{Store: n.store, Pushes: n.pushes, Runs: n.runs, Holder: "same-process", Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	// The stalled attempt's admission is refused once (it saw the world before
	// the other attempt), then the other attempt, with the same holder name,
	// takes the expired lease over, creates the run and loses its own outcome write.
	n.runs.Err, n.runs.ErrOnce = fmt.Errorf("%w: refused", gw.ErrRunRefused), true
	hook.before = func() {
		clock = t0.Add(10 * time.Minute)
		n.store.FailFinishes = 1
		if _, err := other.HandleRefsUpdated(ctx, env); !errors.Is(err, gw.ErrUnavailable) {
			t.Errorf("the other attempt loses its outcome write: %v", err)
		}
	}
	_, err = stalled.HandleRefsUpdated(ctx, env)
	if !errors.Is(err, gw.ErrAdmissionBusy) || !errors.Is(err, gw.ErrUnavailable) || errors.Is(err, gw.ErrConflict) {
		t.Fatalf("a fenced attempt is retryable busy, not a bare conflict: %v", err)
	}
	if n.runs.Created() != 1 {
		t.Fatalf("the other attempt created the run: %d", n.runs.Created())
	}
	if rec, ok := n.store.Inbox(gw.InboxKey{Scope: scopeA, Kind: gw.InboxPush, Source: "trg_1", Subject: "push_1", Ref: "refs/heads/main"}); !ok || rec.Done {
		t.Fatalf("no outcome was recorded by the fenced attempt: %+v", rec)
	}
	// The redelivery recovers the run: started, never rejected.
	out, err := stalled.HandleRefsUpdated(ctx, env)
	if err != nil || len(out) != 1 || out[0].Outcome != gw.OutcomeStarted || n.runs.Created() != 1 {
		t.Fatalf("redelivery: %+v %v (%d runs)", out, err, n.runs.Created())
	}
	for _, a := range n.store.Audits() {
		if a.Action == gw.AuditPushRunRefused {
			t.Fatalf("a refusal was audited for a key that started a run: %+v", a)
		}
	}
}

// codedRefusal is a permanent refusal carrying a reason code and a message that
// must never reach the record.
type codedRefusal struct{ code string }

func (c codedRefusal) Error() string {
	return "refused for user usr_secret in definition pd_private: " + c.code
}
func (c codedRefusal) Unwrap() error       { return gw.ErrRunRefused }
func (c codedRefusal) RefusalCode() string { return c.code }

// Test_unit_trigger_refusal_code_is_recorded_and_readable: a configurer who
// cannot start runs sees why in the status read, as a sanitized code only
// (review finding 4).
func Test_unit_trigger_refusal_code_is_recorded_and_readable(t *testing.T) {
	ctx := context.Background()
	n := newNative(t)
	tr := n.trigger("repo_1", "refs/heads/main")
	n.auth.Grant("viewer", scopeA, gw.ActionView)

	if _, st, err := n.config.TriggerStatus(ctx, "admin", scopeA, tr.ID()); err != nil || st.LastDecision != nil || st.LastRefusal != nil {
		t.Fatalf("nothing decided yet: %+v %v", st, err)
	}
	n.runs.Err = codedRefusal{"run_authority_denied"}
	out := n.handle(n.push(scopeA, record("push_1", "repo_1", "", applied("refs/heads/main", sha1A, sha1B))))
	if len(out) != 1 || out[0].Outcome != gw.OutcomeRejected || out[0].Reason != "run_authority_denied" {
		t.Fatalf("the refusal code is the recorded reason: %+v", out)
	}
	// A malformed code never reaches the record.
	n.runs.Err = codedRefusal{"Secret name: usr_secret"}
	out = n.handle(n.push(scopeA, record("push_2", "repo_1", "", applied("refs/heads/main", sha1B, sha1C))))
	if out[0].Reason != gw.ReasonAdmissionRefused {
		t.Fatalf("a code outside the syntax falls back: %+v", out)
	}
	n.runs.Err = nil
	n.handle(n.push(scopeA, record("push_3", "repo_1", "", applied("refs/heads/main", sha1C, sha1A))))

	// A viewer, who has no configuration authority, reads the status.
	got, st, err := n.config.TriggerStatus(ctx, "viewer", scopeA, tr.ID())
	if err != nil || got.ID() != tr.ID() {
		t.Fatalf("status: %+v %v", got, err)
	}
	if st.LastDecision == nil || st.LastDecision.Subject != "push_3" || st.LastDecision.Outcome != gw.OutcomeStarted || st.LastDecision.RunID == "" {
		t.Fatalf("last decision: %+v", st.LastDecision)
	}
	if st.LastRefusal == nil || st.LastRefusal.Subject != "push_2" || st.LastRefusal.Ref != "refs/heads/main" || st.LastRefusal.Reason != gw.ReasonAdmissionRefused {
		t.Fatalf("last refusal: %+v", st.LastRefusal)
	}
	first, ok, err := n.store.LatestInbox(ctx, scopeA, gw.InboxPush, tr.ID(), gw.OutcomeRejected)
	if err != nil || !ok {
		t.Fatal(err)
	}
	for _, v := range []string{first.Reason, st.LastRefusal.Reason} {
		if strings.Contains(v, "secret") || strings.Contains(v, "pd_private") {
			t.Fatalf("a name leaked into the record: %q", v)
		}
	}
	for _, a := range n.store.Audits() {
		if a.Action == gw.AuditPushRunRefused && (strings.Contains(a.Reason, "usr_secret") || strings.Contains(fmt.Sprint(a.Metadata), "usr_secret")) {
			t.Fatalf("a name leaked into the audit: %+v", a)
		}
	}
	if _, _, err := n.config.TriggerStatus(ctx, "nobody", scopeA, tr.ID()); !errors.Is(err, gw.ErrDenied) {
		t.Fatalf("the status read is authorized: %v", err)
	}
	if _, _, err := n.config.TriggerStatus(ctx, "viewer", scopeA, "trg_unknown"); !errors.Is(err, gw.ErrNotFound) {
		t.Fatalf("an unknown trigger: %v", err)
	}

	// The task route reports the same way.
	n.work.Set(scopeA, "wi_1", gw.WorkFacts{State: "ready", ContentRevision: 1, Actionable: true})
	if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_task", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n.runs.Err = codedRefusal{"run_authority_denied"}
	if got, err := n.router.HandleWorkItemEvent(ctx, becameReady(t, scopeA, "wi_1", "tr_1")); err != nil || got.Reason != "run_authority_denied" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, st, err := n.config.TaskRouteStatus(ctx, "viewer", scopeA); err != nil || st.LastRefusal == nil || st.LastRefusal.Reason != "run_authority_denied" || st.LastRefusal.Subject != "wi_1/transition:tr_1" {
		t.Fatalf("route status: %+v %v", st, err)
	}
}

// Test_unit_trigger_ignores_events_before_enablement: a trigger or route created
// or re-enabled after an event was committed never starts runs for it, on late
// redelivery too (review finding 5).
func Test_unit_trigger_ignores_events_before_enablement(t *testing.T) {
	ctx := context.Background()
	n := newNative(t)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	pushAt := func(id string, d time.Duration) eventsv1.Envelope {
		rec := record(id, "repo_1", "", applied("refs/heads/main", sha1A, sha1B))
		env := n.push(scopeA, rec)
		env.OccurredAt = at(d)
		return env
	}
	old := pushAt("push_old", 0)

	n.clock = at(time.Hour)
	tr := n.trigger("repo_1", "refs/heads/main")
	if !tr.EnabledAt.Equal(at(time.Hour)) {
		t.Fatalf("created enabled at its creation: %v", tr.EnabledAt)
	}
	before := n.pushes.Reloads
	out := n.handle(old)
	if len(out) != 1 || out[0].Outcome != gw.OutcomeIgnored || out[0].Reason != gw.ReasonBeforeEnabled || n.runs.Created() != 0 || n.pushes.Reloads != before {
		t.Fatalf("a push before the trigger existed: %+v (%d runs, %d reloads)", out, n.runs.Created(), n.pushes.Reloads-before)
	}
	if _, ok := n.store.Inbox(gw.InboxKey{Scope: scopeA, Kind: gw.InboxPush, Source: tr.ID(), Subject: "push_old", Ref: "refs/heads/main"}); ok {
		t.Fatal("an ignored late event is never claimed")
	}
	if out := n.handle(pushAt("push_new", 2*time.Hour)); started(out) != 1 {
		t.Fatalf("a push after creation starts a run: %+v", out)
	}
	// The same instant is not before.
	if out := n.handle(pushAt("push_same", time.Hour)); started(out) != 1 {
		t.Fatalf("a push at the creation instant: %+v", out)
	}

	// Disabled and re-enabled: events from the disabled window stay ignored.
	n.clock = at(3 * time.Hour)
	if _, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 1, gw.TriggerPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	n.clock = at(4 * time.Hour)
	re, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 2, gw.TriggerPatch{Enabled: ptr(true)})
	if err != nil || !re.EnabledAt.Equal(at(4*time.Hour)) {
		t.Fatalf("re-enabled: %+v %v", re, err)
	}
	if out := n.handle(pushAt("push_gap", 3*time.Hour+30*time.Minute)); started(out) != 0 || out[0].Reason != gw.ReasonBeforeEnabled {
		t.Fatalf("a push from the disabled window: %+v", out)
	}
	if out := n.handle(pushAt("push_after", 4*time.Hour)); started(out) != 1 {
		t.Fatalf("a push after re-enabling: %+v", out)
	}
	// An alteration that does not enable keeps the instant.
	n.clock = at(5 * time.Hour)
	patterns := []string{"refs/heads/main", "refs/heads/dev"}
	kept, err := n.config.UpdateTrigger(ctx, "admin", scopeA, tr.ID(), 3, gw.TriggerPatch{RefPatterns: &patterns})
	if err != nil || !kept.EnabledAt.Equal(at(4*time.Hour)) {
		t.Fatalf("an unrelated alteration keeps EnabledAt: %+v %v", kept, err)
	}

	t.Run("task route", func(t *testing.T) {
		n := newNative(t)
		n.work.Set(scopeA, "wi_1", gw.WorkFacts{State: "ready", ContentRevision: 1, Actionable: true})
		n.clock = at(time.Hour)
		route, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_task", Enabled: true})
		if err != nil || !route.EnabledAt.Equal(at(time.Hour)) {
			t.Fatalf("%+v %v", route, err)
		}
		stale := becameReady(t, scopeA, "wi_1", "tr_old")
		stale.OccurredAt = t0
		got, err := n.router.HandleWorkItemEvent(ctx, stale)
		if err != nil || got == nil || got.Outcome != gw.OutcomeIgnored || got.Reason != gw.ReasonBeforeEnabled || n.runs.Created() != 0 || len(n.runs.Requests) != 0 {
			t.Fatalf("a readiness before the route existed: %+v %v", got, err)
		}
		fresh := becameReady(t, scopeA, "wi_1", "tr_new")
		fresh.OccurredAt = at(2 * time.Hour)
		if got, err := n.router.HandleWorkItemEvent(ctx, fresh); err != nil || got.Outcome != gw.OutcomeStarted {
			t.Fatalf("%+v %v", got, err)
		}
		// Altering an enabled route keeps the instant; re-enabling moves it.
		n.clock = at(3 * time.Hour)
		same, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_ci", Enabled: true, ExpectedVersion: 1})
		if err != nil || !same.EnabledAt.Equal(at(time.Hour)) {
			t.Fatalf("%+v %v", same, err)
		}
		if _, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_ci", Enabled: false, ExpectedVersion: 2}); err != nil {
			t.Fatal(err)
		}
		n.clock = at(4 * time.Hour)
		again, err := n.config.ConfigureTaskRoute(ctx, gw.ConfigureTaskRouteCommand{Scope: scopeA, Principal: "admin", DefinitionID: "pd_ci", Enabled: true, ExpectedVersion: 3})
		if err != nil || !again.EnabledAt.Equal(at(4*time.Hour)) {
			t.Fatalf("%+v %v", again, err)
		}
	})
}
