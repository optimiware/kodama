package kernelcontracts_test

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/runstate"
	kc "github.com/optimiware/kodama/test/support/kernelcontracts"
)

// recorder is a TB that records failures instead of failing a real test, so the
// suite can be run against an adapter that is meant to fail it.
type recorder struct {
	failed   []string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failed = append(r.failed, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	goruntime.Goexit()
}

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

// failures runs one case against the factory and returns what it reported. A
// panic in the adapter counts as a failure of the case.
func failures(f kc.Factory, c kc.Case) []string {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			for _, c := range slices.Backward(r.cleanups) {
				c()
			}
		}()
		defer func() {
			if p := recover(); p != nil {
				r.Errorf("panic: %v", p)
			}
		}()
		kc.Check(r, f, c)
	}()
	<-done
	return r.failed
}

func failedCases(f kc.Factory) map[string][]string {
	out := map[string][]string{}
	for _, c := range kc.Cases() {
		if fs := failures(f, c); len(fs) > 0 {
			out[c.Name] = fs
		}
	}
	return out
}

type subject = kc.Subject

// hooks replace individual operations of an otherwise correct adapter; a nil
// hook delegates. Each deliberately broken adapter below is the memory adapter
// with one policy undone.
type hooks struct {
	admit    func(in subject, ctx context.Context, a rt.Admission) (rt.RunRecord, error)
	load     func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunRecord, error)
	claim    func(in subject, ctx context.Context, req rt.ClaimRequest, now time.Time) (rt.RunRecord, error)
	renew    func(in subject, ctx context.Context, held rt.Claim, ttl time.Duration, now time.Time) (rt.Claim, error)
	commit   func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error)
	runnable func(in subject, ctx context.Context, q rt.RunnableQuery) ([]rt.RunRef, error)
	lookup   func(in subject, ctx context.Context, id rt.SignalIdentity) (rt.SignalLookup, error)
	decide   func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error)
	view     func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunView, error)
	outbox   func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) ([]rt.Event, error)
}

type broken struct {
	subject
	h hooks
}

func (b *broken) Admit(ctx context.Context, a rt.Admission) (rt.RunRecord, error) {
	if b.h.admit != nil {
		return b.h.admit(b.subject, ctx, a)
	}
	return b.subject.Admit(ctx, a)
}

func (b *broken) Load(ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunRecord, error) {
	if b.h.load != nil {
		return b.h.load(b.subject, ctx, s, id)
	}
	return b.subject.Load(ctx, s, id)
}

func (b *broken) Claim(ctx context.Context, req rt.ClaimRequest, now time.Time) (rt.RunRecord, error) {
	if b.h.claim != nil {
		return b.h.claim(b.subject, ctx, req, now)
	}
	return b.subject.Claim(ctx, req, now)
}

func (b *broken) Renew(ctx context.Context, held rt.Claim, ttl time.Duration, now time.Time) (rt.Claim, error) {
	if b.h.renew != nil {
		return b.h.renew(b.subject, ctx, held, ttl, now)
	}
	return b.subject.Renew(ctx, held, ttl, now)
}

func (b *broken) Commit(ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
	if b.h.commit != nil {
		return b.h.commit(b.subject, ctx, t)
	}
	return b.subject.Commit(ctx, t)
}

func (b *broken) Runnable(ctx context.Context, q rt.RunnableQuery) ([]rt.RunRef, error) {
	if b.h.runnable != nil {
		return b.h.runnable(b.subject, ctx, q)
	}
	return b.subject.Runnable(ctx, q)
}

func (b *broken) LookupSignal(ctx context.Context, id rt.SignalIdentity) (rt.SignalLookup, error) {
	if b.h.lookup != nil {
		return b.h.lookup(b.subject, ctx, id)
	}
	return b.subject.LookupSignal(ctx, id)
}

func (b *broken) DecideSignal(ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
	if b.h.decide != nil {
		return b.h.decide(b.subject, ctx, req, now)
	}
	return b.subject.DecideSignal(ctx, req, now)
}

func (b *broken) View(ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunView, error) {
	if b.h.view != nil {
		return b.h.view(b.subject, ctx, s, id)
	}
	return b.subject.View(ctx, s, id)
}

func (b *broken) Outbox(ctx context.Context, s domain.Scope, id domain.RunID) ([]rt.Event, error) {
	if b.h.outbox != nil {
		return b.h.outbox(b.subject, ctx, s, id)
	}
	return b.subject.Outbox(ctx, s, id)
}

func isCode(err error, code domain.ErrorCode) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

// rebaseWaits makes a commit that the store refused with a version conflict
// succeed by replacing the waits selected by bypass with the stored ones, so
// only that one wait-version rejection is undone and valid setup is untouched.
func rebaseWaits(bypass func(w, stored rt.Wait) bool) func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
	return func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
		rec, err := in.Commit(ctx, t)
		if !isCode(err, domain.CodeVersionConflict) || len(t.Waits) == 0 {
			return rec, err
		}
		cur, lerr := in.Load(ctx, t.Scope, t.RunID)
		if lerr != nil {
			return rec, err
		}
		waits := slices.Clone(t.Waits)
		for i, w := range waits {
			if j := slices.IndexFunc(cur.Waits, func(x rt.Wait) bool { return x.ID == w.ID }); j >= 0 && bypass(w, cur.Waits[j]) {
				waits[i] = cur.Waits[j]
			}
		}
		t.Waits = waits
		return in.Commit(ctx, t)
	}
}

func memory(_ kc.TB, clock rt.Clock) kc.Subject { return runstate.NewMemory(clock) }

// brokenAdapters maps each defect to the case that must catch it.
func brokenAdapters() []struct {
	name   string
	mustBe string
	msg    string
	build  func(in subject) subject
} {
	type mutation = struct {
		name   string
		mustBe string
		msg    string
		build  func(in subject) subject
	}
	wrap := func(h hooks) func(in subject) subject {
		return func(in subject) subject { return &broken{subject: in, h: h} }
	}
	return []mutation{
		{"commit ignores the fence", "claim/takeover_rejects_the_stale_writer", "", wrap(hooks{
			commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
				rec, err := in.Commit(ctx, t)
				if cur, lerr := in.Load(ctx, t.Scope, t.RunID); err != nil && lerr == nil && cur.Claim.Active(t.Now) {
					t.Claim = cur.Claim
					return in.Commit(ctx, t)
				}
				return rec, err
			}})},
		{"commit ignores lease expiry", "claim/expired_lease_cannot_commit_or_renew", "", wrap(hooks{
			commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
				if t.Now.After(t.Claim.Expires) || t.Now.Equal(t.Claim.Expires) {
					t.Now = t.Claim.Expires.Add(-time.Nanosecond)
				}
				return in.Commit(ctx, t)
			}})},
		{"renewal bumps the fence", "claim/renewal_keeps_the_fence_and_is_persisted", "", wrap(hooks{
			renew: func(in subject, ctx context.Context, held rt.Claim, ttl time.Duration, now time.Time) (rt.Claim, error) {
				c, err := in.Renew(ctx, held, ttl, now)
				c.Fence++
				return c, err
			}})},
		{"an active lease can be claimed again", "claim/unexpired_lease_is_held_even_for_its_owner", "", wrap(hooks{
			claim: func(in subject, ctx context.Context, req rt.ClaimRequest, now time.Time) (rt.RunRecord, error) {
				rec, err := in.Claim(ctx, req, now)
				if err != nil && errors.Is(err, rt.ErrClaimHeld) {
					return in.Load(ctx, req.Scope, req.RunID)
				}
				return rec, err
			}})},
		{"version conflicts are rebased away", "commit/optimistic_version_check", "", wrap(hooks{
			commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
				rec, err := in.Commit(ctx, t)
				if isCode(err, domain.CodeVersionConflict) {
					cur, lerr := in.Load(ctx, t.Scope, t.RunID)
					if lerr != nil {
						return rec, err
					}
					t.ExpectedVersion, t.Run.Version = cur.Run.Version, cur.Run.Version+1
					return in.Commit(ctx, t)
				}
				return rec, err
			}})},
		{"a duplicate transition is applied again", "commit/duplicate_transition_applies_once", "", wrap(hooks{
			commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
				rec, err := in.Commit(ctx, t)
				if isCode(err, domain.CodeVersionConflict) {
					return in.Load(ctx, t.Scope, t.RunID)
				}
				return rec, err
			}})},
		{"stale wait updates are accepted", "commit/wait_updates_are_version_checked", "a stale wait", wrap(hooks{
			commit: rebaseWaits(func(w, stored rt.Wait) bool { return w.Version < stored.Version })})},
		{"unversioned wait changes are accepted", "commit/wait_updates_are_version_checked", "a wait changed without a new version", wrap(hooks{
			commit: rebaseWaits(func(w, stored rt.Wait) bool { return w.Version == stored.Version })})},
		{"a wait opens without its step", "commit/wait_opens_only_with_its_step", "a wait opened without its step entering waiting", wrap(hooks{
			commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
				rec, err := in.Commit(ctx, t)
				if err != nil && strings.Contains(err.Error(), "opens only with its step") {
					return in.Load(ctx, t.Scope, t.RunID)
				}
				return rec, err
			}})},
		{"outbox written outside the transaction", "commit/rollback_discards_state_and_outbox_together", "", func() func(in subject) subject {
			var leaked []rt.Event
			return wrap(hooks{
				commit: func(in subject, ctx context.Context, t rt.Transition) (rt.RunRecord, error) {
					leaked = append(leaked, t.Events...)
					return in.Commit(ctx, t)
				},
				outbox: func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) ([]rt.Event, error) {
					base, err := in.Outbox(ctx, s, id)
					if err != nil {
						return nil, err
					}
					for _, e := range leaked {
						if e.Scope == s && e.RunID == id && !slices.ContainsFunc(base, func(b rt.Event) bool { return b.ID == e.ID }) {
							base = append(base, e)
						}
					}
					return base, nil
				}})
		}()},
		{"admission outbox written outside the transaction", "admit/rolls_back_with_its_outbox", "", func() func(in subject) subject {
			var leaked []rt.Event
			return wrap(hooks{
				admit: func(in subject, ctx context.Context, a rt.Admission) (rt.RunRecord, error) {
					leaked = append(leaked, a.Events...)
					return in.Admit(ctx, a)
				},
				outbox: func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) ([]rt.Event, error) {
					base, err := in.Outbox(ctx, s, id)
					for _, e := range leaked {
						if e.Scope == s && e.RunID == id && !slices.ContainsFunc(base, func(b rt.Event) bool { return b.ID == e.ID }) {
							base, err = append(base, e), nil
						}
					}
					return base, err
				}})
		}()},
		{"cancellation does not block execution claims", "cancel/blocks_execution_but_allows_reconcile_and_cleanup", "", wrap(hooks{
			claim: func(in subject, ctx context.Context, req rt.ClaimRequest, now time.Time) (rt.RunRecord, error) {
				rec, err := in.Claim(ctx, req, now)
				if err != nil && req.Purpose == rt.ClaimExecute {
					rec, lerr := in.Load(ctx, req.Scope, req.RunID)
					return rec, lerr
				}
				return rec, err
			}})},
		{"an exhausted run refuses cleanup claims", "cleanup/exhaustion_then_takeover_confirms_what_remains", "", wrap(hooks{
			claim: func(in subject, ctx context.Context, req rt.ClaimRequest, now time.Time) (rt.RunRecord, error) {
				if cur, err := in.Load(ctx, req.Scope, req.RunID); err == nil && cur.Run.Cleanup.State == rt.CleanupExhausted {
					return rt.RunRecord{}, rt.ErrExecutionClosed
				}
				return in.Claim(ctx, req, now)
			}})},
		{"key reuse replays the recorded result", "wait/duplicate_signal_converges", "", wrap(hooks{
			decide: func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
				res, err := in.DecideSignal(ctx, req, now)
				if err != nil && errors.Is(err, rt.ErrKeyReuse) {
					if l, lerr := in.LookupSignal(ctx, req.Identity); lerr == nil && l.Record != nil {
						r := l.Record.Result()
						r.Replayed = true
						return r, nil
					}
				}
				return res, err
			}})},
		{"a replay is reported as a fresh decision", "wait/duplicate_signal_converges", "", wrap(hooks{
			decide: func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
				res, err := in.DecideSignal(ctx, req, now)
				res.Replayed = false
				return res, err
			}})},
		{"a targetless refusal is rerouted on retry", "wait/targetless_refusal_never_reroutes_to_a_later_wait", "", wrap(hooks{
			decide: func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
				if l, err := in.LookupSignal(ctx, req.Identity); err == nil && l.Tombstone != nil {
					req.Identity.Key += "-retry"
				}
				return in.DecideSignal(ctx, req, now)
			}})},
		{"the signal lookup forgets closed waits", "wait/lookup_returns_the_decision_after_the_wait_closes", "", wrap(hooks{
			lookup: func(in subject, ctx context.Context, id rt.SignalIdentity) (rt.SignalLookup, error) {
				l, err := in.LookupSignal(ctx, id)
				if err != nil || l.Record == nil {
					return l, err
				}
				if rec, lerr := in.Load(ctx, id.Scope, l.Record.Target.RunID); lerr == nil && rec.Run.Terminal() {
					return rt.SignalLookup{}, nil
				}
				return l, nil
			}})},
		{"the deadline loses to a late resolution", "wait/expiry_wins_at_the_deadline", "", wrap(hooks{
			decide: func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
				return in.DecideSignal(ctx, req, now.Add(-30*time.Minute))
			}})},
		{"the run id alone finds a run in any project", "scope/foreign_or_mismatched_scope_is_not_found", "", wrap(hooks{
			load: func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunRecord, error) {
				rec, err := in.Load(ctx, s, id)
				if !isCode(err, domain.CodeNotFound) {
					return rec, err
				}
				for _, p := range []domain.ProjectID{"p1", "p2"} {
					if rec, lerr := in.Load(ctx, domain.Scope{TenantID: s.TenantID, ProjectID: p}, id); lerr == nil {
						return rec, nil
					}
				}
				return rec, err
			}})},
		{"signals reach waits of other scopes", "scope/signal_identity_is_scoped_by_tenant_project_and_caller", "", func() func(in subject) subject {
			var homes []domain.Scope
			return wrap(hooks{
				admit: func(in subject, ctx context.Context, a rt.Admission) (rt.RunRecord, error) {
					if !slices.Contains(homes, a.Run.Scope) {
						homes = append(homes, a.Run.Scope)
					}
					return in.Admit(ctx, a)
				},
				decide: func(in subject, ctx context.Context, req rt.SignalRequest, now time.Time) (rt.SignalResult, error) {
					res, err := in.DecideSignal(ctx, req, now)
					if err != nil || res.Refusal != rt.RefusalUnknownWait || res.Replayed {
						return res, err
					}
					for _, h := range homes {
						if h != req.Identity.Scope {
							req.Identity.Scope = h
							if r, derr := in.DecideSignal(ctx, req, now); derr == nil && r.Target != (rt.SignalTarget{}) {
								return r, nil
							}
						}
					}
					return res, err
				}})
		}()},
		{"the signal key is not bound to its caller", "scope/signal_identity_is_scoped_by_tenant_project_and_caller", "", wrap(hooks{
			lookup: func(in subject, ctx context.Context, id rt.SignalIdentity) (rt.SignalLookup, error) {
				id.Caller = "alice"
				return in.LookupSignal(ctx, id)
			}})},
		{"the view keeps a stale status", "view/is_rebuilt_from_retained_records_at_every_stage", "", wrap(hooks{
			view: func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunView, error) {
				v, err := in.View(ctx, s, id)
				v.Status = rt.StatusQueued
				return v, err
			}})},
		{"claimed runs are listed as runnable", "claim/runnable_lists_unclaimed_and_expired_work", "", wrap(hooks{
			runnable: func(in subject, ctx context.Context, q rt.RunnableQuery) ([]rt.RunRef, error) {
				q.Now = q.Now.Add(1000 * time.Hour)
				return in.Runnable(ctx, q)
			}})},
		{"a returned record aliases the store", "admit/returns_copies", "", func(in subject) subject {
			// An adapter that hands out the one shared record: the first mutation shows in later loads.
			var cache *rt.RunRecord
			return &broken{subject: in, h: hooks{
				admit: func(in subject, ctx context.Context, a rt.Admission) (rt.RunRecord, error) {
					rec, err := in.Admit(ctx, a)
					cache = &rec
					return rec, err
				},
				load: func(in subject, ctx context.Context, s domain.Scope, id domain.RunID) (rt.RunRecord, error) {
					if cache != nil && cache.Run.ID == id && cache.Run.Scope == s {
						return *cache, nil
					}
					return in.Load(ctx, s, id)
				}}}
		}},
	}
}

// Test_unit_kernel_state_contract_rejects_broken_adapters proves the suite
// cannot pass vacuously: the memory adapter passes every case, and each
// deliberately broken variant fails the case that guards the policy it undoes.
func Test_unit_kernel_state_contract_rejects_broken_adapters(t *testing.T) {
	cases := kc.Cases()
	if len(cases) < 30 {
		t.Fatalf("the suite shrank to %d cases", len(cases))
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
	}
	groups := map[kc.Group]bool{}
	for _, c := range cases {
		groups[c.Group] = true
	}
	for _, g := range []kc.Group{kc.GroupAdmission, kc.GroupClaims, kc.GroupCommits, kc.GroupCancel, kc.GroupCleanup, kc.GroupWaits, kc.GroupScope, kc.GroupViews} {
		if !groups[g] {
			t.Errorf("no case in group %s", g)
		}
	}

	t.Run("the memory adapter passes", func(t *testing.T) {
		if got := failedCases(memory); len(got) > 0 {
			t.Fatalf("the reference adapter failed: %v", got)
		}
	})
	for _, m := range brokenAdapters() {
		t.Run(m.name, func(t *testing.T) {
			if !seen[m.mustBe] {
				t.Fatalf("no case named %q", m.mustBe)
			}
			factory := func(tb kc.TB, clock rt.Clock) kc.Subject { return m.build(memory(tb, clock)) }
			got := failedCases(factory)
			if m.msg != "" {
				// The designated assertion must be the one that caught it, not an earlier setup step.
				if !slices.ContainsFunc(got[m.mustBe], func(f string) bool { return strings.Contains(f, m.msg) }) {
					t.Fatalf("%q failed for another reason than %q: %v", m.mustBe, m.msg, got[m.mustBe])
				}
			}
			if _, ok := got[m.mustBe]; !ok {
				names := make([]string, 0, len(got))
				for n := range got {
					names = append(names, n)
				}
				t.Fatalf("the suite did not fail %q for a broken adapter (%q); it failed %v", m.mustBe, m.name, names)
			}
		})
	}
}
