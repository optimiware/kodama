package execution

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// This file drives the run_shared resource lifecycle from durable intent. Every
// effect is intent-first: the resource record (spec, fingerprint, names) was
// committed at start; before an Ensure the attempt is committed; the runtime is
// called outside any SQL transaction; the verified result is committed with its
// history and audit rows. A crash anywhere leaves a state the next pass
// resumes: observe, adopt, re-Ensure an unbound intent that never was ready and
// before any command began, or mark loss. Duplicate and reordered wakeups are
// harmless because each pass reads durable state and the port is idempotent.

// sharedMutation runs fn on the locked aggregate and then completes the usual
// bookkeeping (skip propagation, settlement, terminal completion and the
// release of the run reservation).
func sharedMutation(fn func(env Env, agg *Aggregate, now time.Time) error) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		if err := fn(env, agg, env.Now()); err != nil {
			return Effects{}, err
		}
		after, err := advance(env, agg)
		return Effects{After: after}, err
	}
}

func (s *Service) updateShared(ctx context.Context, ref RunRef, fn func(env Env, agg *Aggregate, now time.Time) error) (*Aggregate, error) {
	agg, _, err := s.d.Store.Update(ctx, ref.Scope, ref.ID, UpdateOptions{}, sharedMutation(fn))
	return agg, err
}

// refreshResources observes every live resource whose last observation is older
// than maxAge and records the verified answer. An observation error is an
// unknown outcome (never absent, never loss); it blocks dispatch.
func (s *Service) refreshResources(ctx context.Context, ref RunRef, maxAge time.Duration) error {
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return err
	}
	if view.Run.Shared == nil {
		return nil
	}
	_, res, err := s.sharedCleanup(ctx, view.Run)
	if err != nil {
		return err
	}
	now := s.now()
	for _, r := range view.Resources {
		if r.Removed() || r.Stopping() || (!r.Bound() && r.EnsureRequestedAt == nil) {
			continue
		}
		if r.Kind == KindPreparation && r.Stopped() {
			continue
		}
		if r.ObservedAt != nil && now.Sub(*r.ObservedAt) <= maxAge && r.Phase == PhaseReady {
			continue
		}
		if _, err := s.observeResource(ctx, ref, res, r.Key, nil); err != nil {
			return err
		}
	}
	return nil
}

// answerOf maps an adapter's reply to an ObservationAnswer. An error is an
// unknown outcome, never absence; a collision reply is the adapter's statement
// that the object at the name is not the resource's.
func answerOf(h ResourceHandle, o ResourceObservation, err error) ObservationAnswer {
	switch {
	case errors.Is(err, ErrResourceCollision):
		return ObservationAnswer{Collision: true}
	case err != nil:
		return ObservationAnswer{Obs: ResourceObservation{Handle: h, Phase: PhaseUnknown, Reason: reasonResourceUnobservable}}
	}
	if o.Handle.Name == "" {
		uid := o.Handle.UID
		o.Handle = h
		if uid != "" {
			o.Handle.UID = uid
		}
	}
	return ObservationAnswer{Obs: o}
}

func (s *Service) backoff(r *RunResource, reason ResourceReason, now time.Time) {
	r.noteFailure(reason, s.o.ResourceBackoffBase, s.o.ResourceBackoffMax, now)
}

// observeResource asks the backend about one resource and records the answer.
// The request generation is committed BEFORE the runtime call and the answer is
// applied only if no newer generation was applied meanwhile, so a delayed older
// answer (a ready that left the adapter before a newer unknown was committed)
// is discarded instead of clearing the newer uncertainty. after, if set, runs in
// the same transaction as the application, on the state that results from it.
func (s *Service) observeResource(ctx context.Context, ref RunRef, res RunResources, key string, after func(agg *Aggregate, now time.Time) error) (*Aggregate, error) {
	var h ResourceHandle
	var gen int64
	var requestedAt time.Time
	var call bool
	view, err := s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		call = false
		r := agg.Resource(key)
		if r == nil || r.Removed() {
			return nil
		}
		gen, requestedAt = r.RequestObservation(now)
		h, call = r.Handle(), true
		return nil
	})
	if err != nil || !call {
		return view, err
	}
	obs, oerr := res.Observe(ctx, h)
	ans := answerOf(h, obs, oerr)
	return s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		if cur := agg.Resource(key); cur == nil || cur.Removed() {
			return nil
		}
		if _, err := agg.ApplyObservation(key, gen, requestedAt, ans, now); err != nil {
			return err
		}
		if after != nil {
			return after(agg, now)
		}
		return nil
	})
}

// ensureResource commits the Ensure intent and its request generation, calls
// the backend outside any transaction and records the verified observation
// (binding the UID). Any error but a collision or fence leaves the outcome
// unknown: the resource is retried later with backoff and keeps blocking
// dispatch.
func (s *Service) ensureResource(ctx context.Context, ref RunRef, res RunResources, key string) (*Aggregate, error) {
	var spec ResourceSpec
	var gen int64
	var requestedAt time.Time
	view, err := s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		spec = ResourceSpec{}
		r := agg.Resource(key)
		if r == nil || r.Removed() || r.Stopping() || r.Bound() || !r.Due(now) {
			return nil
		}
		if r.EnsureRequestedAt != nil && (len(agg.Begins) > 0 || r.ReadyAt != nil) {
			// A create whose outcome was lost, after a command began or after the
			// resource was ready, is never retried: it is a loss.
			from := r.state()
			r.Reason = r.lossReason()
			r.touch(now)
			agg.recordResource(r, from, now)
			agg.RecordFailure(Reason(r.Reason), "", now)
			return nil
		}
		from := r.state()
		if err := r.MarkEnsureRequested(now); err != nil {
			return nil
		}
		gen, requestedAt = r.RequestObservation(now)
		agg.recordResource(r, from, now)
		spec = r.Spec
		return nil
	})
	if err != nil || spec.Key == "" {
		return view, err
	}
	obs, eerr := res.Ensure(ctx, spec)
	// EVERY outcome goes through the generation fence: a delayed older result of
	// any kind (ready, collision, fenced or an outage) can neither supersede nor
	// clear what a newer request committed. An ambiguous outcome (an error whose
	// effect is unknown: the object may exist) is persisted as an unknown
	// observation of this generation, with its backoff, before anything else.
	var ans ObservationAnswer
	outage := false
	switch {
	case errors.Is(eerr, ErrResourceFenced):
		ans = ObservationAnswer{Fenced: true}
	case eerr != nil && !errors.Is(eerr, ErrResourceCollision):
		ans = answerOf(spec.Handle(""), obs, eerr)
		ans.Obs.Reason = reasonResourceUnknown
		outage = true
	default:
		ans = answerOf(spec.Handle(""), obs, eerr)
	}
	return s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		r := agg.Resource(key)
		if r == nil || r.Removed() {
			return nil
		}
		applied, err := agg.ApplyObservation(key, gen, requestedAt, ans, now)
		if err != nil || !applied {
			return err
		}
		from := r.state()
		if outage {
			s.backoff(r, "", now)
		} else {
			r.clearFailure(now)
		}
		agg.recordResource(r, from, now)
		return nil
	})
}

// stopResource commits the stop intent, stops the backend object outside any
// transaction and records the verified evidence. A resource never ensured has
// nothing that could exist: its stop is recorded locally without a call. A
// collision reply is NEVER evidence: for a resource that was bound or may have
// been created it proves only that the object at the name is not the one we
// can verify, not that ours is gone or that no mount or process uses the
// volume, so the stop stays unconfirmed (visible, retried, nonterminal, the
// reservation kept) until an affirmative settlement. A never-owned foreign
// object at the name of an unbound resource is answered by the adapter with a
// verified stop that touches nothing foreign.
func (s *Service) stopResource(ctx context.Context, ref RunRef, res RunResources, key string, reason ResourceReason) (*Aggregate, error) {
	var handle ResourceHandle
	var call bool
	view, err := s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		call = false
		r := agg.Resource(key)
		if r == nil || r.Stopped() || r.Removed() {
			return nil
		}
		from := r.state()
		r.requestStop(reason, now)
		if !r.Bound() && r.EnsureRequestedAt == nil {
			// Definitely never issued: nothing of ours can exist.
			if r.recordStop(ResourceStopEvidence{Stopped: true, Launch: LaunchNotStarted, Released: true}, now) {
				agg.recordResource(r, from, now)
			}
			return nil
		}
		agg.recordResource(r, from, now)
		if !r.Due(now) {
			return nil
		}
		handle, call = r.Handle(), true
		return nil
	})
	if err != nil || !call {
		return view, err
	}
	ev, serr := res.Stop(ctx, handle)
	return s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		r := agg.Resource(key)
		if r == nil || r.Stopped() {
			return nil
		}
		from := r.state()
		if serr != nil {
			reason := ReasonStopUnconfirmed
			if errors.Is(serr, ErrResourceCollision) {
				reason = ReasonResourceCollision
			}
			s.backoff(r, reason, now)
			agg.recordResource(r, from, now)
			return nil
		}
		if err := agg.RecordStopEvidence(key, ev, now); err != nil {
			return err
		}
		if !ev.Stopped || (r.Kind == KindWorkspace && !ev.Released) {
			s.backoff(r, ReasonStopUnconfirmed, now)
		} else {
			r.clearFailure(now)
		}
		return nil
	})
}

// removeResource removes a verified-stopped resource and records the verified
// absence. Removal is never requested before a verified stop and a collision
// reply is never confirmation of absence (see stopResource).
func (s *Service) removeResource(ctx context.Context, ref RunRef, res RunResources, key string) (*Aggregate, error) {
	var handle ResourceHandle
	var call bool
	view, err := s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		call = false
		r := agg.Resource(key)
		if r == nil || !r.Stopped() || r.Removed() {
			return nil
		}
		if !r.Bound() && r.EnsureRequestedAt == nil {
			return agg.RecordRemoveEvidence(key, ResourceRemoveEvidence{Removed: true}, now)
		}
		if !r.Due(now) {
			return nil
		}
		handle, call = r.Handle(), true
		return nil
	})
	if err != nil || !call {
		return view, err
	}
	ev, rerr := res.Remove(ctx, handle)
	return s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		r := agg.Resource(key)
		if r == nil || r.Removed() {
			return nil
		}
		from := r.state()
		if rerr != nil {
			reason := ReasonCleanupPending
			if errors.Is(rerr, ErrResourceCollision) {
				reason = ReasonResourceCollision
			}
			s.backoff(r, reason, now)
			agg.recordResource(r, from, now)
			return nil
		}
		if err := agg.RecordRemoveEvidence(key, ev, now); err != nil {
			return err
		}
		if !ev.Removed {
			s.backoff(r, ReasonCleanupPending, now)
		} else {
			r.clearFailure(now)
		}
		return nil
	})
}

// teardownReason is the safe reason recorded as the stop reason of a run's
// resources.
func teardownReason(run *Run) ResourceReason {
	switch {
	case run.FailureReason != "":
		return ResourceReason(run.FailureReason)
	case run.CancelReason != "":
		return ResourceReason(run.CancelReason)
	}
	return ResourceReasonCompleted
}

// reconcileSharedResources performs one pass of the run_shared lifecycle for a
// run (any status). It is safe to repeat from any replica. Two authorities are
// kept apart: LAUNCH (create, adopt, redrive, dispatch) needs the verified pins,
// the policy and every pinned capability; CLEANUP (stop, observe, remove the
// committed, owned UIDs) needs only that this controller implements the run
// model and the companion port is the run's pinned backend. A launch check that
// fails records the run's failure with its safe reason and settles the
// resources through the cleanup authority; a run of an unsupported model or of
// another backend is left untouched.
func (s *Service) reconcileSharedResources(ctx context.Context, ref RunRef) (ReconcileReport, error) {
	var rep ReconcileReport
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return rep, err
	}
	if view.Run.Shared == nil {
		return rep, nil
	}
	rt, res, err := s.sharedCleanup(ctx, view.Run)
	if err != nil {
		// A controller that does not implement the run model never touches it.
		return rep, err
	}
	var first error
	tearing := func(v *Aggregate) bool { return v.Run.Status.Terminal() || v.Run.Draining() || v.completionDecided() }
	if !tearing(view) {
		if _, _, reason, lerr := s.sharedLaunch(ctx, view.Run); lerr != nil {
			switch {
			case reason == ReasonPolicyUnavailable:
				// The pinned policy cannot be verified: fail the run and settle what it
				// holds. A queued run that never started is failed the same way.
				if view, err = s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
					agg.RecordFailure(Reason(reason), "", now)
					return nil
				}); err != nil {
					return rep, err
				}
			case view.Run.Status == RunQueued:
				return rep, lerr // waits for the capabilities it was admitted for
			default:
				if view, err = s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
					agg.RecordFailure(Reason(reason), "", now)
					return nil
				}); err != nil {
					return rep, err
				}
			}
		}
	}
	if view.Run.Status == RunQueued && len(view.Resources) == 0 {
		if !tearing(view) {
			if err := s.StartShared(ctx, ref); err != nil && !errors.Is(err, ErrQuotaExceeded) && !errors.Is(err, ErrBusy) {
				return rep, err
			}
		}
		return rep, nil
	}
	if len(view.Resources) == 0 {
		return rep, nil
	}
	// Verified stop of every attempt's workload, successful ones included: the
	// workspace is handed off or stopped only after no process can use it.
	for _, at := range view.Attempts {
		c := view.Control(at.ID)
		if at.Status.Terminal() && c.TerminationConfirmedAt == nil {
			s.terminate(ctx, rt, at)
			rep.Terminated++
		}
	}
	view, err = s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return rep, err
	}
	step := func(a *Aggregate, e error) *Aggregate {
		rep.Resources++
		if e != nil && first == nil {
			first = e
		}
		if a == nil {
			return view
		}
		return a
	}
	if tearing(view) {
		view = s.teardown(ctx, ref, res, view, &rep, step)
	} else {
		view = s.provision(ctx, ref, res, view, &rep, step)
	}
	if _, err := s.updateShared(ctx, ref, func(Env, *Aggregate, time.Time) error { return nil }); err != nil && first == nil {
		first = err
	}
	return rep, first
}

func (s *Service) provision(ctx context.Context, ref RunRef, res RunResources, view *Aggregate, rep *ReconcileReport, step func(*Aggregate, error) *Aggregate) *Aggregate {
	drive := func(key string) {
		r := view.Resource(key)
		if r == nil || r.Removed() || r.Stopping() {
			return
		}
		if !r.Bound() {
			view = step(s.ensureResource(ctx, ref, res, key))
			return
		}
		if r.Kind == KindPreparation && r.HasReceipt() {
			return // handled below: stopped, never observed as running again
		}
		view = step(s.observeResource(ctx, ref, res, key, nil))
	}
	drive(KeyNetwork)
	if n := view.Resource(KeyNetwork); n == nil || !n.IsReady() {
		return view
	}
	drive(KeyWorkspace)
	if w := view.Resource(KeyWorkspace); w == nil || !w.IsReady() {
		return view
	}
	if view.Run.Shared.SourceBacked {
		drive(KeyPreparation)
		if p := view.Resource(KeyPreparation); p != nil && p.HasReceipt() && !p.Stopped() && !view.Run.Draining() {
			// The receipt is recorded: the preparation workload must be verified
			// stopped (its credentials gone) before the workspace is ready.
			view = step(s.stopResource(ctx, ref, res, KeyPreparation, ResourceReasonCompleted))
		}
	}
	for _, r := range append([]*RunResource(nil), view.Resources...) {
		if r.Kind == KindService {
			drive(r.Key)
		}
	}
	// Hand the workspace to the next attempt only on current release evidence.
	if h := view.ActiveHolder(); h != nil && !view.Run.Draining() {
		at := view.Attempt(h.AttemptID)
		if at != nil && view.Control(at.ID).Settled() && view.attemptQuiescent(at) {
			view = step(s.releaseHolder(ctx, ref, res, h.AttemptID))
		}
	}
	return view
}

// ResourceReasonCompleted is the stop reason of a resource that finished its job
// or of a run that completed.
const ResourceReasonCompleted = ResourceReason(ReasonCompleted)

// releaseHolder observes the workspace and, IN THE SAME TRANSACTION that applies
// the answer, releases the holder only if the resulting current state carries
// fresh, UID-bound release evidence observed after the attempt's verified stop.
// An intervening unreleased or unknown observation (a newer generation) discards
// this answer and the release finds the newer state, so the handoff is blocked.
func (s *Service) releaseHolder(ctx context.Context, ref RunRef, res RunResources, attempt domain.AttemptID) (*Aggregate, error) {
	return s.observeResource(ctx, ref, res, KeyWorkspace, func(agg *Aggregate, now time.Time) error {
		_, err := agg.ReleaseHolder(attempt, s.o.ResourceFreshness, now)
		if errors.Is(err, errNoHolder) || (err != nil && IsBusinessError(err)) {
			return nil
		}
		return err
	})
}

func (s *Service) teardown(ctx context.Context, ref RunRef, res RunResources, view *Aggregate, rep *ReconcileReport, step func(*Aggregate, error) *Aggregate) *Aggregate {
	// Record the stop intent on every resource first: it revokes admission and
	// credentials at once, and survives a crash.
	reason := teardownReason(view.Run)
	view = step(s.updateShared(ctx, ref, func(env Env, agg *Aggregate, now time.Time) error {
		agg.stopResources(reason, now)
		return nil
	}))
	// Services and preparation stop promptly, without waiting for uncertain source
	// or external effects, which settle through their own reconciliation.
	for _, r := range append([]*RunResource(nil), view.Resources...) {
		if r.Kind != KindService && r.Kind != KindPreparation {
			continue
		}
		if !r.Stopped() {
			view = step(s.stopResource(ctx, ref, res, r.Key, reason))
		}
		if cur := view.Resource(r.Key); cur != nil && cur.Stopped() && !cur.Removed() {
			view = step(s.removeResource(ctx, ref, res, r.Key))
		}
	}
	// The workspace is stopped only after no attempt can still use it.
	if ws := view.Resource(KeyWorkspace); ws != nil && !ws.Removed() && view.HoldersQuiescent() {
		if !ws.Stopped() {
			view = step(s.stopResource(ctx, ref, res, KeyWorkspace, reason))
		}
		if cur := view.Resource(KeyWorkspace); cur != nil && cur.Stopped() && !cur.Removed() {
			view = step(s.removeResource(ctx, ref, res, KeyWorkspace))
		}
	}
	// The network is created first and removed last.
	others := true
	for _, r := range view.Resources {
		if r.Kind != KindNetwork && !r.Removed() {
			others = false
		}
	}
	if net := view.Resource(KeyNetwork); net != nil && !net.Removed() && others {
		if !net.Stopped() {
			view = step(s.stopResource(ctx, ref, res, KeyNetwork, reason))
		}
		if cur := view.Resource(KeyNetwork); cur != nil && cur.Stopped() && !cur.Removed() {
			view = step(s.removeResource(ctx, ref, res, KeyNetwork))
		}
	}
	return view
}
