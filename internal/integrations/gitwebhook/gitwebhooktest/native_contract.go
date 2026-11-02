package gitwebhooktest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

// NativeSubject is one gitwebhook.NativeStore under test with what the contract
// needs to seed around it.
type NativeSubject struct {
	Store gw.NativeStore
	// Restart returns a new handle over the same persistence, as after a process
	// restart.
	Restart func() gw.NativeStore
	// ScopeA and ScopeB are scopes of different tenants; ScopeA2 is another
	// project of ScopeA's tenant.
	ScopeA, ScopeB, ScopeA2 domain.Scope
	// NewTrigger returns a valid, enabled trigger with the given ID in scope, with
	// its references (repository, definition) seeded where the implementation
	// requires it. Calls with the same scope return the same repository.
	NewTrigger func(t testing.TB, scope domain.Scope, id string) gw.PushTrigger
	// NewRoute returns a valid enabled route of the scope at version 1.
	NewRoute func(t testing.TB, scope domain.Scope) gw.TaskRoute
	// Audits counts the audit records whose action is action and whose resource ID
	// is resource, in the scope.
	Audits func(t testing.TB, scope domain.Scope, action, resource string) int
}

// NativeStoreContract is the behavior every gitwebhook.NativeStore the native
// trigger depends on has: scoped, versioned configuration and a deduplicating
// inbox with a write-once outcome. The memory store and the PostgreSQL adapter
// both run it.
func NativeStoreContract(t *testing.T, open func(t testing.TB) NativeSubject) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	audit := func(action string, scope domain.Scope, id string) gw.AuditRecord {
		return gw.AuditRecord{Action: action, Scope: scope, TriggerID: gw.TriggerID(id), Principal: "usr_1", At: at, Metadata: map[string]string{"k": "v"}}
	}

	t.Run("trigger configuration is scoped, versioned and audited", func(t *testing.T) {
		s := open(t)
		a := s.NewTrigger(t, s.ScopeA, "trg_a1")
		if err := s.Store.InsertPushTrigger(ctx, a, audit(gw.AuditPushTriggerCreated, s.ScopeA, "trg_a1")); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.InsertPushTrigger(ctx, a, audit(gw.AuditPushTriggerCreated, s.ScopeA, "trg_a1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a duplicate identifier: %v", err)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushTriggerCreated, "trg_a1"); n != 1 {
			t.Fatalf("one audit for one creation, got %d", n)
		}
		got, err := s.Store.PushTrigger(ctx, s.ScopeA, "trg_a1")
		if err != nil || got.Config.RepositoryID != a.Config.RepositoryID || got.Version != 1 || !got.Excludes() || got.ConfiguredBy != a.ConfiguredBy || !got.EnabledAt.Equal(a.EnabledAt) {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		for _, foreign := range []domain.Scope{s.ScopeB, s.ScopeA2} {
			if _, err := s.Store.PushTrigger(ctx, foreign, "trg_a1"); !errors.Is(err, gw.ErrNotFound) {
				t.Fatalf("a trigger of another scope is not found: %v", err)
			}
		}

		next := got.Clone()
		next.Config.RefPatterns = []string{"refs/heads/main", "refs/heads/release/*"}
		next.Config.Enabled = false
		next.ConfiguredBy = "usr_2"
		next.EnabledAt = at.Add(time.Hour)
		upd, err := s.Store.UpdatePushTrigger(ctx, next, 1, audit(gw.AuditPushTriggerUpdated, s.ScopeA, "trg_a1"))
		if err != nil || upd.Version != 2 || upd.Config.Enabled || upd.ConfiguredBy != "usr_2" || len(upd.Config.RefPatterns) != 2 || !upd.CreatedAt.Equal(got.CreatedAt) || !upd.EnabledAt.Equal(at.Add(time.Hour)) {
			t.Fatalf("update: %+v %v", upd, err)
		}
		if _, err := s.Store.UpdatePushTrigger(ctx, next, 1, audit(gw.AuditPushTriggerUpdated, s.ScopeA, "trg_a1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		moved := upd.Clone()
		moved.Config.RepositoryID = "repo_other"
		if _, err := s.Store.UpdatePushTrigger(ctx, moved, 2, audit(gw.AuditPushTriggerUpdated, s.ScopeA, "trg_a1")); err == nil {
			t.Fatalf("the repository binding is immutable")
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushTriggerUpdated, "trg_a1"); n != 1 {
			t.Fatalf("a refused update writes no audit, got %d", n)
		}
		enabled, err := s.Store.EnabledPushTriggers(ctx, s.ScopeA, a.Config.RepositoryID)
		if err != nil || len(enabled) != 0 {
			t.Fatalf("a disabled trigger is not offered: %+v %v", enabled, err)
		}
	})

	t.Run("listing and the enabled selection are ordered and scoped", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"trg_c", "trg_a", "trg_b"} {
			if err := s.Store.InsertPushTrigger(ctx, s.NewTrigger(t, s.ScopeA, id), audit(gw.AuditPushTriggerCreated, s.ScopeA, id)); err != nil {
				t.Fatal(err)
			}
		}
		off := s.NewTrigger(t, s.ScopeA, "trg_d")
		off.Config.Enabled = false
		if err := s.Store.InsertPushTrigger(ctx, off, audit(gw.AuditPushTriggerCreated, s.ScopeA, "trg_d")); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.InsertPushTrigger(ctx, s.NewTrigger(t, s.ScopeB, "trg_a"), audit(gw.AuditPushTriggerCreated, s.ScopeB, "trg_a")); err != nil {
			t.Fatalf("the same identifier in another tenant is independent: %v", err)
		}
		page, err := s.Store.ListPushTriggers(ctx, s.ScopeA, "", 3)
		if err != nil || len(page) != 3 || page[0].ID() != "trg_a" || page[1].ID() != "trg_b" || page[2].ID() != "trg_c" {
			t.Fatalf("first page: %v %v", ids(page), err)
		}
		page, err = s.Store.ListPushTriggers(ctx, s.ScopeA, "trg_c", 3)
		if err != nil || len(page) != 1 || page[0].ID() != "trg_d" {
			t.Fatalf("second page: %v %v", ids(page), err)
		}
		repo := s.NewTrigger(t, s.ScopeA, "x").Config.RepositoryID
		on, err := s.Store.EnabledPushTriggers(ctx, s.ScopeA, repo)
		if err != nil || len(on) != 3 || on[0].ID() != "trg_a" || on[2].ID() != "trg_c" {
			t.Fatalf("enabled: %v %v", ids(on), err)
		}
		other, err := s.Store.EnabledPushTriggers(ctx, s.ScopeA, "repo_unrelated")
		if err != nil || len(other) != 0 {
			t.Fatalf("another repository: %v %v", ids(other), err)
		}
		b, err := s.Store.EnabledPushTriggers(ctx, s.ScopeB, s.NewTrigger(t, s.ScopeB, "x").Config.RepositoryID)
		if err != nil || len(b) != 1 {
			t.Fatalf("tenant B sees only its own: %v %v", ids(b), err)
		}
	})

	t.Run("a task route is one per project, versioned and scoped", func(t *testing.T) {
		s := open(t)
		if _, err := s.Store.TaskRoute(ctx, s.ScopeA); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("no route yet: %v", err)
		}
		r := s.NewRoute(t, s.ScopeA)
		put, err := s.Store.PutTaskRoute(ctx, r, 0, audit(gw.AuditTaskRouteConfigured, s.ScopeA, string(s.ScopeA.ProjectID)))
		if err != nil || put.Version != 1 {
			t.Fatalf("create: %+v %v", put, err)
		}
		if _, err := s.Store.PutTaskRoute(ctx, r, 0, audit(gw.AuditTaskRouteConfigured, s.ScopeA, string(s.ScopeA.ProjectID))); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a second creation: %v", err)
		}
		r.Enabled, r.ConfiguredBy = false, "usr_9"
		r.EnabledAt = at.Add(2 * time.Hour)
		put, err = s.Store.PutTaskRoute(ctx, r, 1, audit(gw.AuditTaskRouteConfigured, s.ScopeA, string(s.ScopeA.ProjectID)))
		if err != nil || put.Version != 2 || put.Enabled || put.ConfiguredBy != "usr_9" || !put.EnabledAt.Equal(at.Add(2*time.Hour)) {
			t.Fatalf("alter: %+v %v", put, err)
		}
		if _, err := s.Store.PutTaskRoute(ctx, r, 1, audit(gw.AuditTaskRouteConfigured, s.ScopeA, string(s.ScopeA.ProjectID))); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a stale alteration: %v", err)
		}
		if _, err := s.Store.TaskRoute(ctx, s.ScopeA2); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("another project has no route: %v", err)
		}
		if _, err := s.Store.TaskRoute(ctx, s.ScopeB); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("another tenant has no route: %v", err)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditTaskRouteConfigured, string(s.ScopeA.ProjectID)); n != 2 {
			t.Fatalf("two audited changes, got %d", n)
		}
	})

	pushKey := func(scope domain.Scope, trigger, push, ref string) gw.InboxKey {
		return gw.InboxKey{Scope: scope, Kind: gw.InboxPush, Source: trigger, Subject: push, Ref: ref}
	}

	t.Run("the inbox claims a key once and its lease excludes another holder until it expires", func(t *testing.T) {
		s := open(t)
		k := pushKey(s.ScopeA, "trg_1", "push_1", "refs/heads/main")
		rec, acquired, err := s.Store.ClaimInbox(ctx, k, "w1", at, time.Minute)
		if err != nil || !acquired || rec.Done || rec.LeaseHolder != "w1" {
			t.Fatalf("first claim: %+v %v %v", rec, acquired, err)
		}
		if _, acquired, err = s.Store.ClaimInbox(ctx, k, "w2", at.Add(30*time.Second), time.Minute); err != nil || acquired {
			t.Fatalf("another holder under an unexpired lease: %v %v", acquired, err)
		}
		if _, acquired, err = s.Store.ClaimInbox(ctx, k, "w1", at.Add(30*time.Second), time.Minute); err != nil || !acquired {
			t.Fatalf("the holder re-acquires its own lease: %v %v", acquired, err)
		}
		rec, acquired, err = s.Store.ClaimInbox(ctx, k, "w2", at.Add(3*time.Minute), time.Minute)
		if err != nil || !acquired || rec.LeaseHolder != "w2" {
			t.Fatalf("takeover after expiry: %+v %v %v", rec, acquired, err)
		}
		if err := s.Store.ReleaseInbox(ctx, k, "w1"); err != nil {
			t.Fatal(err)
		}
		if _, acquired, _ = s.Store.ClaimInbox(ctx, k, "w3", at.Add(3*time.Minute+time.Second), time.Minute); acquired {
			t.Fatalf("releasing another holder's lease changed it")
		}
		if err := s.Store.ReleaseInbox(ctx, k, "w2"); err != nil {
			t.Fatal(err)
		}
		if _, acquired, _ = s.Store.ClaimInbox(ctx, k, "w3", at.Add(3*time.Minute+time.Second), time.Minute); !acquired {
			t.Fatalf("a released lease is free")
		}
		// Different push, ref, trigger, kind and scope are different keys.
		for _, other := range []gw.InboxKey{
			pushKey(s.ScopeA, "trg_1", "push_2", "refs/heads/main"), pushKey(s.ScopeA, "trg_1", "push_1", "refs/heads/dev"),
			pushKey(s.ScopeA, "trg_2", "push_1", "refs/heads/main"), pushKey(s.ScopeB, "trg_1", "push_1", "refs/heads/main"),
			pushKey(s.ScopeA2, "trg_1", "push_1", "refs/heads/main"),
			{Scope: s.ScopeA, Kind: gw.InboxTaskReady, Source: "trg_1", Subject: "push_1"},
		} {
			if _, acquired, err := s.Store.ClaimInbox(ctx, other, "w9", at, time.Minute); err != nil || !acquired {
				t.Fatalf("an independent key %+v: %v %v", other, acquired, err)
			}
		}
		if _, _, err := s.Store.ClaimInbox(ctx, gw.InboxKey{Scope: s.ScopeA, Kind: gw.InboxPush, Source: "trg_1"}, "w1", at, time.Minute); err == nil {
			t.Fatalf("an incomplete key is refused")
		}
	})

	t.Run("an inbox outcome is write-once, fenced by the lease and audited atomically", func(t *testing.T) {
		s := open(t)
		k := pushKey(s.ScopeA, "trg_1", "push_1", "refs/heads/main")
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_1"}, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("finishing an unclaimed key: %v", err)
		}
		if _, acquired, err := s.Store.ClaimInbox(ctx, k, "w1", at, time.Minute); err != nil || !acquired {
			t.Fatal(err)
		}
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Holder: "w2"}, at, audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a refusal by a non-holder: %v", err)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushRunRefused, "trg_1"); n != 0 {
			t.Fatalf("a refused finish writes no audit, got %d", n)
		}
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeStarted, Holder: "w1"}, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); err == nil {
			t.Fatalf("started needs a run")
		}
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeIgnored, Holder: "w1"}, at, audit(gw.AuditPushRunIgnored, s.ScopeA, "trg_1")); err == nil {
			t.Fatalf("a non-started outcome states its reason")
		}
		// A run exists, so a started finish needs no lease (a stalled holder lost it).
		rec, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_1", Holder: "w2"}, at.Add(time.Second), audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1"))
		if err != nil || !rec.Done || rec.Outcome != gw.OutcomeStarted || rec.RunID != "run_1" || rec.LeaseHolder != "" {
			t.Fatalf("started: %+v %v", rec, err)
		}
		again, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Holder: "w1"}, at.Add(2*time.Second), audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1"))
		if err != nil || again.Outcome != gw.OutcomeStarted || again.RunID != "run_1" {
			t.Fatalf("the first outcome wins: %+v %v", again, err)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushRunStarted, "trg_1"); n != 1 {
			t.Fatalf("one audit for the one outcome, got %d", n)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushRunRefused, "trg_1"); n != 0 {
			t.Fatalf("a losing finish writes no audit, got %d", n)
		}
		rec, acquired, err := s.Store.ClaimInbox(ctx, k, "w3", at.Add(time.Hour), time.Minute)
		if err != nil || acquired || !rec.Done || rec.RunID != "run_1" {
			t.Fatalf("a finished key replays its record: %+v %v %v", rec, acquired, err)
		}

		rk := pushKey(s.ScopeA, "trg_1", "push_1", "refs/heads/dev")
		if _, _, err := s.Store.ClaimInbox(ctx, rk, "w1", at, time.Minute); err != nil {
			t.Fatal(err)
		}
		rec, err = s.Store.FinishInbox(ctx, rk, gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Holder: "w1"}, at, audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1"))
		if err != nil || rec.Outcome != gw.OutcomeRejected || rec.Reason != gw.ReasonAdmissionRefused || rec.RunID != "" {
			t.Fatalf("rejected: %+v %v", rec, err)
		}
	})

	t.Run("an outcome other than started is fenced by a valid lease, and the finish shape is validated first", func(t *testing.T) {
		s := open(t)
		k := pushKey(s.ScopeA, "trg_1", "push_e", "refs/heads/main")
		if _, acquired, err := s.Store.ClaimInbox(ctx, k, "w1", at, time.Minute); err != nil || !acquired {
			t.Fatal(err)
		}
		rejected := gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: gw.ReasonAdmissionRefused, Holder: "w1"}
		// The lease lapsed: the stalled holder cannot record a refusal or an
		// ignore, however it still names itself, because another attempt may now
		// be creating the run.
		if _, err := s.Store.FinishInbox(ctx, k, rejected, at.Add(2*time.Minute), audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("a refusal after the lease expired: %v", err)
		}
		ignored := gw.InboxFinish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonNotReady, Holder: "w1"}
		if _, err := s.Store.FinishInbox(ctx, k, ignored, at.Add(2*time.Minute), audit(gw.AuditPushRunIgnored, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("an ignore after the lease expired: %v", err)
		}
		if n := s.Audits(t, s.ScopeA, gw.AuditPushRunRefused, "trg_1"); n != 0 {
			t.Fatalf("a fenced finish writes no audit, got %d", n)
		}
		// The takeover holder decides.
		if _, acquired, err := s.Store.ClaimInbox(ctx, k, "w2", at.Add(2*time.Minute), time.Minute); err != nil || !acquired {
			t.Fatal(err)
		}
		if _, err := s.Store.FinishInbox(ctx, k, rejected, at.Add(2*time.Minute+time.Second), audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrConflict) {
			t.Fatalf("the stalled holder after a takeover: %v", err)
		}
		rejected.Holder = "w2"
		if rec, err := s.Store.FinishInbox(ctx, k, rejected, at.Add(2*time.Minute+time.Second), audit(gw.AuditPushRunRefused, s.ScopeA, "trg_1")); err != nil || rec.Outcome != gw.OutcomeRejected {
			t.Fatalf("the holder records its refusal: %+v %v", rec, err)
		}
		// A started outcome needs no lease, however late.
		sk := pushKey(s.ScopeA, "trg_1", "push_e", "refs/heads/dev")
		if _, _, err := s.Store.ClaimInbox(ctx, sk, "w1", at, time.Minute); err != nil {
			t.Fatal(err)
		}
		if rec, err := s.Store.FinishInbox(ctx, sk, gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_late"}, at.Add(time.Hour), audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); err != nil || rec.RunID != "run_late" {
			t.Fatalf("a late started: %+v %v", rec, err)
		}

		// The shape is validated before the key is looked up or its decision
		// returned: a malformed finish is ErrInvalid for a decided and an unknown key.
		bad := gw.InboxFinish{Outcome: gw.OutcomeStarted}
		if _, err := s.Store.FinishInbox(ctx, k, bad, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("a malformed finish of a decided key: %v", err)
		}
		unknown := pushKey(s.ScopeA, "trg_1", "push_unknown", "refs/heads/main")
		if _, err := s.Store.FinishInbox(ctx, unknown, bad, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("a malformed finish of an unknown key: %v", err)
		}
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: "bogus", Reason: "x"}, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("an unknown outcome: %v", err)
		}
	})

	t.Run("the latest decision is the status read", func(t *testing.T) {
		s := open(t)
		if _, ok, err := s.Store.LatestInbox(ctx, s.ScopeA, gw.InboxPush, "trg_1", ""); err != nil || ok {
			t.Fatalf("nothing decided yet: %v %v", ok, err)
		}
		decide := func(scope domain.Scope, push, ref string, fin gw.InboxFinish, when time.Time) {
			t.Helper()
			k := pushKey(scope, "trg_1", push, ref)
			if _, _, err := s.Store.ClaimInbox(ctx, k, "w1", when, time.Minute); err != nil {
				t.Fatal(err)
			}
			fin.Holder = "w1"
			if _, err := s.Store.FinishInbox(ctx, k, fin, when, audit(gw.AuditPushRunRefused, scope, "trg_1")); err != nil {
				t.Fatal(err)
			}
		}
		decide(s.ScopeA, "push_1", "refs/heads/main", gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: "run_authority_denied"}, at)
		decide(s.ScopeA, "push_2", "refs/heads/main", gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_2"}, at.Add(time.Second))
		decide(s.ScopeA, "push_3", "refs/heads/main", gw.InboxFinish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonRefDeleted}, at.Add(2*time.Second))
		decide(s.ScopeB, "push_9", "refs/heads/main", gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: "other_tenant"}, at.Add(time.Hour))
		decide(s.ScopeA2, "push_9", "refs/heads/main", gw.InboxFinish{Outcome: gw.OutcomeRejected, Reason: "other_project"}, at.Add(time.Hour))
		last, ok, err := s.Store.LatestInbox(ctx, s.ScopeA, gw.InboxPush, "trg_1", "")
		if err != nil || !ok || last.Key.Subject != "push_3" || last.Outcome != gw.OutcomeIgnored || !last.Done {
			t.Fatalf("latest decision: %+v %v %v", last, ok, err)
		}
		refused, ok, err := s.Store.LatestInbox(ctx, s.ScopeA, gw.InboxPush, "trg_1", gw.OutcomeRejected)
		if err != nil || !ok || refused.Key.Subject != "push_1" || refused.Reason != "run_authority_denied" || refused.Key.Ref != "refs/heads/main" {
			t.Fatalf("latest refusal: %+v %v %v", refused, ok, err)
		}
		if _, ok, _ := s.Store.LatestInbox(ctx, s.ScopeA, gw.InboxPush, "trg_other", ""); ok {
			t.Fatal("another trigger has no decision")
		}
		if _, ok, _ := s.Store.LatestInbox(ctx, s.ScopeA, gw.InboxTaskReady, "trg_1", ""); ok {
			t.Fatal("another kind has no decision")
		}
	})

	t.Run("concurrent claims of one key have one holder and survive a restart", func(t *testing.T) {
		s := open(t)
		k := pushKey(s.ScopeA, "trg_1", "push_c", "refs/heads/main")
		var won atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, ok, err := s.Store.ClaimInbox(ctx, k, fmt.Sprintf("w%d", i), at, time.Hour); err != nil {
					t.Error(err)
				} else if ok {
					won.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("exactly one claim wins, got %d", won.Load())
		}
		if _, err := s.Store.FinishInbox(ctx, k, gw.InboxFinish{Outcome: gw.OutcomeStarted, RunID: "run_c"}, at, audit(gw.AuditPushRunStarted, s.ScopeA, "trg_1")); err != nil {
			t.Fatal(err)
		}
		restarted := s.Restart()
		rec, acquired, err := restarted.ClaimInbox(ctx, k, "w9", at, time.Minute)
		if err != nil || acquired || !rec.Done || rec.RunID != "run_c" {
			t.Fatalf("after a restart: %+v %v %v", rec, acquired, err)
		}
		if err := restarted.InsertPushTrigger(ctx, s.NewTrigger(t, s.ScopeA, "trg_r"), audit(gw.AuditPushTriggerCreated, s.ScopeA, "trg_r")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Restart().PushTrigger(ctx, s.ScopeA, "trg_r"); err != nil {
			t.Fatalf("configuration survives a restart: %v", err)
		}
	})
}

func ids(ts []gw.PushTrigger) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID()
	}
	return out
}
