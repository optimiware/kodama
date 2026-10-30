package kube

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Stop and Remove of the run-resource adapter. Cleanup deletes only objects it can
// prove are the resource's, by UID precondition:
//
//   - a primary object: its UID is the one execution bound or the record
//     holds; with none committed (a create whose outcome was never confirmed) it
//     must carry the ownership marks AND equal the rendering of the committed spec;
//   - a companion object (a service's Service and ingress policy, a network's
//     policies): its UID is the one recorded when it was verified; with none recorded
//     it must carry the marks and the instance anchor AND equal its rendering.
//
// Marks are copyable; a UID and the controlled spec are not. A same-name object
// that is not provably ours is left alone and never blocks the stop.

// ---- verification of objects whose UID was never committed ----

// specView lazily renders the committed spec of a resource's record: cleanup only
// needs the pinned policy when it meets an object with no committed UID.
type specView struct{ get func() (*runRender, error) }

func (r *RunResources) specViewOf(ctx context.Context, a *resRec) specView {
	var once sync.Once
	var x *runRender
	var err error
	return specView{get: func() (*runRender, error) {
		once.Do(func() {
			if len(a.secret.Data[KeyResourceSpec]) == 0 {
				return // a fence with no spec: nothing can be verified by spec
			}
			var spec execution.ResourceSpec
			if spec, err = a.spec(); err != nil {
				return
			}
			var rx runRender
			if rx, err = r.render(ctx, spec); err == nil {
				x = &rx
			}
		})
		return x, err
	}}
}

// check runs fn against the rendering; an object that differs is simply not ours
// (false, nil), an unreadable policy or an outage is an error to retry.
func (sv specView) check(fn func(runRender) error) (bool, error) {
	x, err := sv.get()
	if err != nil {
		return false, err
	}
	if x == nil {
		return false, nil
	}
	if err := fn(*x); err != nil {
		if errors.Is(err, ErrForeignWorkload) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// uidsOf are the instance UIDs committed data names as the resource's: the
// handle's, the record's and those persisted as seen (verified late creates).
func uidsOf(h execution.ResourceHandle, a *resRec) []string {
	return nonEmpty(append([]string{h.UID, a.rec.UID}, a.rec.Seen...))
}

// persistSeen records, durably and before anything is deleted, the UID of a
// primary object that verified by its controlled spec although no UID was
// committed (a late create). Cleanup that has to be repeated (a volume's reclaim
// still pending, a restarted controller) then still knows which instance it
// deleted and finds every volume bound to it. A lost CAS is errRecMoved.
func (r *RunResources) persistSeen(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, uid string) error {
	if uid == "" || slices.Contains(uidsOf(h, a), uid) {
		return nil
	}
	a.rec.Seen = append(a.rec.Seen, uid)
	return r.saveRec(ctx, ns, a)
}

// eligible decides whether the primary object at a resource name is ours to
// delete. A committed UID decides: an object with another UID at a bound
// resource's name is a replacement and a collision, never touched. With no UID
// committed the object is ours only if it carries our marks and equals the
// rendering of the committed spec; otherwise it is somebody else's object on a
// name our create could not take, neither ours to delete nor a reason to keep the
// resource from stopping.
func (r *RunResources) eligible(ns string, m kubernetes.ObjectMeta, objName string, h execution.ResourceHandle, a *resRec, sv specView, verify func(runRender) error) (bool, error) {
	w := ownedOf(h, "")
	w.fingerprint = a.secret.Metadata.Annotations[AnnFingerprint]
	if committed := uidsOf(h, a); len(committed) > 0 {
		if !slices.Contains(committed, m.UID) {
			return false, collision("an object other than the bound instance holds the resource name")
		}
		w.uid = m.UID
		if err := checkOwned(m, ns, objName, w); err != nil {
			return false, asCollision(err)
		}
		return true, nil
	}
	if checkOwned(m, ns, objName, w) != nil {
		return false, nil
	}
	return sv.check(verify)
}

// companionVerdict is the outcome of verifying a companion object.
type companionVerdict int

const (
	// companionForeign: not the resource's (marks, anchor, recorded UID or spec differ).
	companionForeign companionVerdict = iota
	// companionOurs: proven the resource's.
	companionOurs
	// companionUnverified: carries the resource's marks and anchor but nothing
	// independent can prove it (the record, and with it the recorded UIDs and the
	// committed spec, is lost). It is never deleted and never counted as gone.
	companionUnverified
)

// eligibleCompanion decides whether a companion object is ours to delete: its
// recorded UID, or (never recorded) the marks, the instance anchor AND an exact
// rendering of the committed spec. Marks and the anchor annotation are copyable, so
// with nothing durable to check them against (no spec, no recorded UID) a look-alike
// is unverified: left untouched, and removal stays unconfirmed.
func (r *RunResources) eligibleCompanion(ns string, m kubernetes.ObjectMeta, objName string, h execution.ResourceHandle, a *resRec, anchor string, sv specView, verify func(runRender) error) (companionVerdict, error) {
	w := ownedOf(h, "")
	w.fingerprint = a.secret.Metadata.Annotations[AnnFingerprint]
	if checkOwned(m, ns, objName, w) != nil || anchor == "" || m.Annotations[AnnResourceUID] != anchor {
		return companionForeign, nil
	}
	if a.rec.Companions[objName] == m.UID && m.UID != "" {
		return companionOurs, nil
	}
	// Not the recorded instance (or none recorded): a recorded UID that differs is not
	// proof of a foreign replacement, because a create that was outstanding while the
	// recorded instance was deleted (its response lost, the record never updated)
	// produces a new instance of ours. The committed spec decides: the adapter creates
	// exactly its rendering, so an object that equals it is ours, one that differs is
	// not, and with no spec to compare with the object stays unverified.
	if a.rec.Untracked {
		return companionUnverified, nil
	}
	x, err := sv.get()
	if err != nil {
		return companionForeign, err
	}
	if x == nil {
		return companionUnverified, nil
	}
	if err := verify(*x); err != nil {
		if errors.Is(err, ErrForeignWorkload) {
			return companionForeign, nil
		}
		return companionForeign, err
	}
	return companionOurs, nil
}

func deleteQuiet(err error, what string) error {
	switch {
	case err == nil, isNotFound(err), errors.Is(err, kubernetes.ErrConflict):
		return nil
	}
	return unavailable(err, "deleting "+what)
}

// ---- Stop ----

// Stop implements execution.RunResources. It fences the resource first (a
// durable record, so no Ensure can create it again), then deletes only objects
// that verify as the resource's, and reports Stopped only once no process of it
// can run and no create of it can still land, companion creates included. For a
// workspace Released is the verified absence of every Pod referencing the claim
// and of every VolumeAttachment of its volume, found by the claim's identity.
func (r *RunResources) Stop(ctx context.Context, h execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	tgt, err := r.handleTarget(h)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	return retrying(func() (execution.ResourceStopEvidence, error) { return r.stopOnce(ctx, tgt, h) })
}

func (r *RunResources) stopOnce(ctx context.Context, tgt TenantTarget, h execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	ns := tgt.Namespace
	a, tomb, err := r.fenceRec(ctx, ns, h)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	if tomb != nil {
		// Removed earlier: nothing runs, nothing can arrive and no volume is held.
		return execution.ResourceStopEvidence{Stopped: true, Launch: tomb.rec.launch(), Released: true}, nil
	}
	return r.terminate(ctx, ns, h, a)
}

// fenceRec reads the resource's record and fences it. A resource with no record
// is either never planned or has lost its bookkeeping: it gets a tombstone so a
// create racing the stop is refused, and since nothing is known of earlier
// effects it is Untracked (never not_started evidence). A handle with a bound UID
// and no record is lost bookkeeping of a workload that may exist: the tombstone
// binds that UID, so the workload is found and deleted by it.
func (r *RunResources) fenceRec(ctx context.Context, ns string, h execution.ResourceHandle) (*resRec, *resRec, error) {
	a, err := readRec(ctx, r.cl, ns, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	if err != nil {
		return nil, nil, asCollision(err)
	}
	if a == nil {
		tomb, err := readTomb(ctx, r.cl, ns, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
		if err != nil {
			return nil, nil, asCollision(err)
		}
		if tomb != nil {
			if h.UID != "" && tomb.rec.UID != "" && h.UID != tomb.rec.UID {
				return nil, nil, collision("the removal marker binds another instance")
			}
			return nil, tomb, nil
		}
		rec := &resRecord{Version: 1, Name: h.Name, Phase: recPlanned, Untracked: true}
		if h.UID != "" {
			rec.Phase, rec.UID = recOwned, h.UID
		}
		rec.fence(r.now())
		ann := identOf(h)
		ann[AnnRecord] = rec.marshal()
		s := kubernetes.Secret{
			Metadata:  kubernetes.ObjectMeta{Name: recName(h.Name), Namespace: ns, Labels: ownerLabels(h.Scope, h.RunID, h.Kind), Annotations: ann},
			Type:      "Opaque",
			Immutable: ptr(true),
		}
		_, err = r.cl.CreateSecret(ctx, ns, s)
		switch {
		case err == nil, errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
		case errors.Is(err, kubernetes.ErrForbidden), errors.Is(err, kubernetes.ErrInvalid):
			return nil, nil, wrapCause(ErrPreflight, err, "the resource fence was refused")
		default:
			return nil, nil, unavailable(err, "creating the resource fence")
		}
		if a, err = readRec(ctx, r.cl, ns, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name); err != nil {
			return nil, nil, asCollision(err)
		}
		if a == nil {
			return nil, nil, wrap(ErrUnavailable, "the resource fence is not readable after its create")
		}
	}
	if h.UID != "" && a.rec.UID != "" && h.UID != a.rec.UID {
		return nil, nil, collision("the resource record binds another instance")
	}
	if a.rec.fence(r.now()) {
		if err := r.saveRec(ctx, ns, a); err != nil {
			return nil, nil, err
		}
	}
	return a, nil, nil
}

func (r *RunResources) terminate(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec) (execution.ResourceStopEvidence, error) {
	sv := r.specViewOf(ctx, a)
	switch h.Kind {
	case execution.KindWorkspace:
		return r.stopWorkspace(ctx, ns, h, a, sv)
	case execution.KindPreparation:
		return r.stopPreparation(ctx, ns, h, a, sv)
	case execution.KindService:
		return r.stopService(ctx, ns, h, a, sv)
	}
	// The network runs nothing: it is stopped once no create can still land. Its
	// policies stay until Remove.
	return execution.ResourceStopEvidence{Stopped: a.rec.quiescent(r.now(), r.opts.CreateQuiescence), Launch: a.rec.launch()}, nil
}

func (r *RunResources) stopWorkspace(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (execution.ResourceStopEvidence, error) {
	ev := execution.ResourceStopEvidence{Launch: execution.LaunchUnknown}
	c, ok, err := r.getClaim(ctx, ns, h.Name)
	if err != nil {
		return ev, err
	}
	var ours *kubernetes.PersistentVolumeClaim
	if ok {
		isOurs, err := r.eligible(ns, c.Metadata, h.Name, h, a, sv, func(x runRender) error {
			want, err := x.claim()
			if err != nil {
				return err
			}
			return verifyDoc("claim", want, c.Raw, benignClaim)
		})
		if err != nil {
			return ev, err
		}
		if isOurs {
			if err := r.persistSeen(ctx, ns, h, a, c.Metadata.UID); err != nil {
				return ev, err
			}
			ours = &c
		}
	}
	if ours != nil && ours.Spec.VolumeName != "" && a.rec.Volume != ours.Spec.VolumeName {
		a.rec.Volume = ours.Spec.VolumeName
		if err := r.saveRec(ctx, ns, a); err != nil {
			return ev, err
		}
	}
	// The fence is decided by the claim's durable identity even when the claim
	// object is gone or is not ours: Pods referencing its name and attachments of
	// its volumes still count.
	released, err := r.released(ctx, ns, h, a, ours)
	if err != nil {
		return ev, err
	}
	ev.Stopped = a.rec.quiescent(r.now(), r.opts.CreateQuiescence)
	ev.Released = ev.Stopped && released
	return ev, nil
}

func (r *RunResources) stopPreparation(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (execution.ResourceStopEvidence, error) {
	ev := execution.ResourceStopEvidence{Launch: a.rec.launch()}
	j, ok, err := r.getJob(ctx, ns, h.Name)
	if err != nil {
		return ev, err
	}
	uids := uidsOf(h, a)
	owned := ""
	if ok {
		ours, err := r.eligible(ns, j.Metadata, h.Name, h, a, sv, func(x runRender) error {
			want, err := x.prepJob()
			if err != nil {
				return err
			}
			return verifyDoc("Job", want, j.Raw, benignJob(j.Metadata.UID, j.Metadata.Name))
		})
		if err != nil {
			return ev, err
		}
		if ours {
			if err := r.persistSeen(ctx, ns, h, a, j.Metadata.UID); err != nil {
				return ev, err
			}
			owned = j.Metadata.UID
			uids = uidsOf(h, a)
		}
	}
	pods, err := r.podsOfJob(ctx, ns, h, uids...)
	if err != nil {
		return ev, err
	}
	// Launch evidence only gains information and is persisted before a Pod
	// disappears: it cannot be read again afterwards.
	if l := launchOf(pods); l != execution.LaunchUnknown && a.rec.noteLaunch(l) {
		if err := r.saveRec(ctx, ns, a); err != nil {
			return ev, err
		}
	}
	ev.Launch = a.rec.launch()
	if owned != "" {
		if err := deleteQuiet(r.cl.DeleteJob(ctx, ns, h.Name, kubernetes.DeleteOptions{UID: owned, Propagation: "Foreground"}), "the Job"); err != nil {
			return ev, err
		}
		// Foreground deletion is asynchronous: Stopped only once the Job and its
		// Pods are read as absent.
		if _, still, err := r.getJob(ctx, ns, h.Name); err != nil || still {
			return ev, err
		}
		left, err := r.podsOfJob(ctx, ns, h, uids...)
		if err != nil || len(left) > 0 {
			return ev, err
		}
		return r.finishStop(ev, a), nil
	}
	if len(pods) > 0 {
		for _, p := range pods {
			if err := deleteQuiet(r.cl.DeletePod(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Background"}), "a Pod"); err != nil {
				return ev, err
			}
		}
		return ev, nil
	}
	return r.finishStop(ev, a), nil
}

// finishStop decides Stopped once nothing of the workload remains: only when no
// create (of the primary object or of any companion) can still land. A resource
// whose create was never sent is affirmatively not started; a fence with no
// history says nothing.
func (r *RunResources) finishStop(ev execution.ResourceStopEvidence, a *resRec) execution.ResourceStopEvidence {
	if !a.rec.quiescent(r.now(), r.opts.CreateQuiescence) {
		return ev
	}
	ev.Stopped = true
	if ev.Launch == execution.LaunchUnknown && a.rec.neverIssued() {
		ev.Launch = execution.LaunchNotStarted
	}
	return ev
}

func (r *RunResources) stopService(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (execution.ResourceStopEvidence, error) {
	ev := execution.ResourceStopEvidence{Launch: a.rec.launch()}
	// Admission to the endpoint goes first: the ingress policy and the Service.
	if _, _, err := r.deleteOwnedCompanions(ctx, ns, h, a, sv); err != nil {
		return ev, err
	}
	p, ok, err := r.getPod(ctx, ns, h.Name)
	if err != nil {
		return ev, err
	}
	ours := false
	if ok {
		if ours, err = r.eligible(ns, p.Metadata, h.Name, h, a, sv, func(x runRender) error {
			want, err := x.servicePod()
			if err != nil {
				return err
			}
			return verifyDoc("Pod", want, p.Raw, benignPod)
		}); err != nil {
			return ev, err
		}
	}
	if ours {
		if err := r.persistSeen(ctx, ns, h, a, p.Metadata.UID); err != nil {
			return ev, err
		}
		if primaryStarted(p) && a.rec.noteLaunch(execution.LaunchStarted) {
			if err := r.saveRec(ctx, ns, a); err != nil {
				return ev, err
			}
		}
		ev.Launch = a.rec.launch()
		if err := deleteQuiet(r.cl.DeletePod(ctx, ns, h.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Background"}), "the Pod"); err != nil {
			return ev, err
		}
		// A Pod is gone only when it is read as absent (a graceful termination
		// keeps it with a deletion timestamp).
		if _, still, err := r.getPod(ctx, ns, h.Name); err != nil || still {
			return ev, err
		}
	}
	// Companions again: one created while the Pod was being deleted is found now.
	// Stopped speaks for processes (the workload is gone) and for creates that can
	// still land; an object whose ownership cannot be verified is not deleted and is
	// reported by Remove, which stays unconfirmed while it exists.
	if left, _, err := r.deleteOwnedCompanions(ctx, ns, h, a, sv); err != nil || left {
		return ev, err
	}
	return r.finishStop(ev, a), nil
}

// serviceHost is the endpoint host of a service handle.
func serviceHost(h execution.ResourceHandle) string {
	name, _ := execution.ServiceNameOfKey(h.Key)
	return execution.EndpointHost(h.Scope, h.RunID, name)
}

// deleteOwnedCompanions deletes the service's Service and ingress policy when
// they verify as the resource's. An object of the same name that is not ours is
// someone else's and is left alone; it neither blocks the stop nor counts as ours.
// It reports whether any companion of ours still exists and whether an object that
// carries our marks could not be verified (left untouched).
func (r *RunResources) deleteOwnedCompanions(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (left, unverified bool, err error) {
	host := serviceHost(h)
	svc, ok, err := r.getService(ctx, ns, host)
	if err != nil {
		return false, false, err
	}
	if !ok {
		// Verified absent: whatever instance was recorded is gone (a delete whose
		// response was lost included); forget it durably.
		if err := r.forgetCompanion(ctx, ns, a, host); err != nil {
			return false, false, err
		}
	}
	if ok {
		v, err := r.eligibleCompanion(ns, svc.Metadata, host, h, a, a.rec.UID, sv, func(x runRender) error {
			want, err := x.serviceObject()
			if err != nil {
				return err
			}
			return verifyDoc("Service", want, svc.Raw, benignService)
		})
		if err != nil {
			return false, false, err
		}
		switch v {
		case companionUnverified:
			unverified = true
		case companionOurs:
			if err := deleteQuiet(r.cl.DeleteService(ctx, ns, host, kubernetes.DeleteOptions{UID: svc.Metadata.UID}), "the Service"); err != nil {
				return false, false, err
			}
			_, still, err := r.getService(ctx, ns, host)
			if err != nil {
				return false, false, err
			}
			left = left || still
			if !still {
				if err := r.forgetCompanion(ctx, ns, a, host); err != nil {
					return false, false, err
				}
			}
		}
	}
	name := h.Name + suffixIngress
	pol, ok, err := r.getPolicy(ctx, ns, name)
	if err != nil {
		return false, false, err
	}
	if !ok {
		if err := r.forgetCompanion(ctx, ns, a, name); err != nil {
			return false, false, err
		}
	}
	if ok {
		v, err := r.eligibleCompanion(ns, pol.Metadata, name, h, a, a.rec.UID, sv, func(x runRender) error {
			return verifyDoc("network policy", x.ingressPolicy(x.body.Network.Isolation), pol.Raw, benignNone)
		})
		if err != nil {
			return false, false, err
		}
		switch v {
		case companionUnverified:
			unverified = true
		case companionOurs:
			if err := deleteQuiet(r.cl.DeleteNetworkPolicy(ctx, ns, name, kubernetes.DeleteOptions{UID: pol.Metadata.UID}), "the network policy"); err != nil {
				return false, false, err
			}
			_, still, err := r.getPolicy(ctx, ns, name)
			if err != nil {
				return false, false, err
			}
			left = left || still
			if !still {
				if err := r.forgetCompanion(ctx, ns, a, name); err != nil {
					return false, false, err
				}
			}
		}
	}
	return left, unverified, nil
}

// forgetCompanion drops the recorded UID of a companion that was deleted: an
// object created later under the name (a create that was outstanding) is a new
// instance, found and verified by its rendering like any other.
func (r *RunResources) forgetCompanion(ctx context.Context, ns string, a *resRec, name string) error {
	if _, ok := a.rec.Companions[name]; !ok {
		return nil
	}
	delete(a.rec.Companions, name)
	return r.saveRec(ctx, ns, a)
}

// ---- Remove ----

// Remove implements execution.RunResources. It requires a prior stop (a fenced
// record), re-verifies that nothing of the resource runs, deletes the verified
// objects by UID and, for a workspace, confirms the storage allocation is gone
// (delete reclaim). Removed is true only once the verified instance is confirmed
// absent. Last, a durable removal marker (tombstone) is written and only then the
// record is deleted: a removed resource is never created again.
func (r *RunResources) Remove(ctx context.Context, h execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	tgt, err := r.handleTarget(h)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	return retrying(func() (execution.ResourceRemoveEvidence, error) { return r.removeOnce(ctx, tgt.Namespace, h) })
}

func (r *RunResources) removeOnce(ctx context.Context, ns string, h execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	labels := ownerLabels(h.Scope, h.RunID, h.Kind)
	a, err := readRec(ctx, r.cl, ns, identOf(h), labels, h.Name)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, asCollision(err)
	}
	if a == nil {
		tomb, err := readTomb(ctx, r.cl, ns, identOf(h), labels, h.Name)
		switch {
		case err != nil:
			return execution.ResourceRemoveEvidence{}, asCollision(err)
		case tomb != nil:
			return execution.ResourceRemoveEvidence{Removed: true}, nil // removed earlier
		case h.UID == "":
			// Never bound and never recorded: nothing of ours exists, and nothing that
			// is not ours is touched.
			return execution.ResourceRemoveEvidence{Removed: true}, nil
		}
		// A bound workload whose record is gone (lost bookkeeping): success needs the
		// bound instance and its companions to be verifiably absent. Stop (which
		// rebinds a fence to that UID) is what deletes them.
		present, err := r.lostWorkloadPresent(ctx, ns, h)
		return execution.ResourceRemoveEvidence{Removed: err == nil && !present}, err
	}
	if !a.rec.Fenced {
		return execution.ResourceRemoveEvidence{}, fenced("the resource is removed before it was stopped")
	}
	if h.UID != "" && a.rec.UID != "" && h.UID != a.rec.UID {
		return execution.ResourceRemoveEvidence{}, collision("the resource record binds another instance")
	}
	ev, err := r.terminate(ctx, ns, h, a)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	if !ev.Stopped || (h.Kind == execution.KindWorkspace && !ev.Released) {
		return execution.ResourceRemoveEvidence{}, nil
	}
	sv := r.specViewOf(ctx, a)
	var done, unverified bool
	switch h.Kind {
	case execution.KindWorkspace:
		done, err = r.removeWorkspace(ctx, ns, h, a, sv)
	case execution.KindService:
		var left bool
		left, unverified, err = r.deleteOwnedCompanions(ctx, ns, h, a, sv)
		done = !left
	case execution.KindNetwork:
		done, unverified, err = r.removeNetwork(ctx, ns, h, a, sv)
	default:
		done = true
	}
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	if unverified {
		return execution.ResourceRemoveEvidence{}, wrap(ErrCleanupUnverified, "removal stays unconfirmed")
	}
	if !done {
		return execution.ResourceRemoveEvidence{}, nil
	}
	if err := r.writeTomb(ctx, ns, h, a); err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	// Last: the record that proved the objects ours (and held the credential).
	err = r.cl.DeleteSecret(ctx, ns, recName(h.Name), kubernetes.DeleteOptions{UID: a.secret.Metadata.UID})
	if err := deleteQuiet(err, "the resource record"); err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	return execution.ResourceRemoveEvidence{Removed: true}, nil
}

// writeTomb creates the removal marker: identity, fingerprint, bound UID and
// launch history, no credential and no spec. An existing one is verified.
func (r *RunResources) writeTomb(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec) error {
	rec := &resRecord{
		Version: 1, Name: h.Name, Phase: recPlanned, Fenced: true, FencedAt: a.rec.FencedAt, Removed: true, Untracked: a.rec.Untracked,
		UID: a.rec.UID, Launch: a.rec.Launch,
	}
	if rec.UID != "" {
		rec.Phase = recOwned
	}
	ann := identOf(h)
	if fp := a.secret.Metadata.Annotations[AnnFingerprint]; fp != "" {
		ann[AnnFingerprint] = fp
	}
	ann[AnnRecord] = rec.marshal()
	s := kubernetes.Secret{
		Metadata:  kubernetes.ObjectMeta{Name: tombName(h.Name), Namespace: ns, Labels: ownerLabels(h.Scope, h.RunID, h.Kind), Annotations: ann},
		Type:      "Opaque",
		Immutable: ptr(true),
	}
	_, err := r.cl.CreateSecret(ctx, ns, s)
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
	case errors.Is(err, kubernetes.ErrForbidden), errors.Is(err, kubernetes.ErrInvalid):
		return wrapCause(ErrPreflight, err, "the removal marker was refused")
	default:
		return unavailable(err, "creating the removal marker")
	}
	t, err := readTomb(ctx, r.cl, ns, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil:
		return asCollision(err)
	case t == nil:
		return wrap(ErrUnavailable, "the removal marker is not readable after its create")
	}
	return nil
}

// lostWorkloadPresent reports, by the bound UID alone, whether the workload (or
// anything stamped with it) of a resource whose record is gone still exists.
func (r *RunResources) lostWorkloadPresent(ctx context.Context, ns string, h execution.ResourceHandle) (bool, error) {
	w := ownedOf(h, "")
	switch h.Kind {
	case execution.KindWorkspace:
		c, ok, err := r.getClaim(ctx, ns, h.Name)
		if err != nil || (ok && c.Metadata.UID == h.UID) {
			return true, err
		}
		pvs, err := r.cl.ListPVs(ctx)
		if err != nil {
			return true, unavailable(err, "listing volumes")
		}
		for _, pv := range pvs {
			if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == ns && ref.Name == h.Name && ref.UID == h.UID {
				return true, nil
			}
		}
	case execution.KindPreparation:
		j, ok, err := r.getJob(ctx, ns, h.Name)
		if err != nil || (ok && j.Metadata.UID == h.UID) {
			return true, err
		}
		pods, err := r.podsOfJob(ctx, ns, h, h.UID)
		if err != nil || len(pods) > 0 {
			return true, err
		}
	case execution.KindService:
		p, ok, err := r.getPod(ctx, ns, h.Name)
		if err != nil || (ok && p.Metadata.UID == h.UID) {
			return true, err
		}
		host := serviceHost(h)
		svc, ok, err := r.getService(ctx, ns, host)
		if err != nil || (ok && checkOwned(svc.Metadata, ns, host, w) == nil && svc.Metadata.Annotations[AnnResourceUID] == h.UID) {
			return true, err
		}
		pol, ok, err := r.getPolicy(ctx, ns, h.Name+suffixIngress)
		if err != nil || (ok && checkOwned(pol.Metadata, ns, pol.Metadata.Name, w) == nil && pol.Metadata.Annotations[AnnResourceUID] == h.UID) {
			return true, err
		}
	case execution.KindNetwork:
		pols, err := r.cl.SelectNetworkPolicies(ctx, ns, LabelResourceOwner+"="+ValueResourceOwner+","+LabelRunKey+"="+RunKey(h.Scope, h.RunID)+","+LabelResourceKind+"="+string(execution.KindNetwork))
		if err != nil {
			return true, unavailable(err, "listing network policies")
		}
		for _, p := range pols {
			if hasNamePrefix(p.Metadata.Name, h.Name) && checkOwned(p.Metadata, ns, p.Metadata.Name, w) == nil && p.Metadata.Annotations[AnnResourceUID] == h.UID {
				return true, nil
			}
		}
	}
	return false, nil
}

// removeWorkspace deletes the claim and confirms the claim and every volume of its
// allocation are gone. The volume is deleted by the reclaim policy, never by us;
// volumes are found by the claim's identity (the recorded name, the provisioner's
// name for the claim UID and any volume whose claimRef is exactly this claim, by
// namespace, name and committed UID). One that still exists keeps cleanup pending
// and one of the expected name bound to another claim is a collision.
func (r *RunResources) removeWorkspace(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (bool, error) {
	c, ok, err := r.getClaim(ctx, ns, h.Name)
	if err != nil {
		return false, err
	}
	uids := uidsOf(h, a)
	ours := false
	if ok {
		if ours, err = r.eligible(ns, c.Metadata, h.Name, h, a, sv, func(x runRender) error {
			want, err := x.claim()
			if err != nil {
				return err
			}
			return verifyDoc("claim", want, c.Raw, benignClaim)
		}); err != nil {
			return false, err
		}
	}
	if ours {
		// The claim's UID is durable before it is deleted: a retry (reclaim pending,
		// a restarted controller) must still find its volumes by it.
		if err := r.persistSeen(ctx, ns, h, a, c.Metadata.UID); err != nil {
			return false, err
		}
		uids = uidsOf(h, a)
		if c.Spec.VolumeName != "" && a.rec.Volume != c.Spec.VolumeName {
			a.rec.Volume = c.Spec.VolumeName
			if err := r.saveRec(ctx, ns, a); err != nil {
				return false, err
			}
		}
		if c.Metadata.DeletionTimestamp == "" {
			if err := deleteQuiet(r.cl.DeletePVC(ctx, ns, h.Name, kubernetes.DeleteOptions{UID: c.Metadata.UID, Propagation: "Background"}), "the claim"); err != nil {
				return false, err
			}
		}
		// The claim is confirmed gone by reading, never assumed.
		if _, still, err := r.getClaim(ctx, ns, h.Name); err != nil || still {
			return false, err
		}
	}
	var named []string
	if a.rec.Volume != "" {
		named = append(named, a.rec.Volume)
	}
	for _, u := range uids {
		if "pvc-"+u != a.rec.Volume {
			named = append(named, "pvc-"+u)
		}
	}
	for _, vol := range named {
		pv, err := r.cl.GetPV(ctx, vol)
		switch {
		case isNotFound(err):
			continue
		case err != nil:
			return false, unavailable(err, "reading the volume")
		}
		if ref := pv.Spec.ClaimRef; ref == nil || !slices.Contains(uids, ref.UID) || ref.Name != h.Name || ref.Namespace != ns {
			return false, collision("the volume of the claim is bound to another claim")
		}
		return false, nil // reclaim pending
	}
	if len(uids) > 0 {
		pvs, err := r.cl.ListPVs(ctx)
		if err != nil {
			return false, unavailable(err, "listing volumes")
		}
		for _, pv := range pvs {
			if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == ns && ref.Name == h.Name && slices.Contains(uids, ref.UID) {
				return false, nil // a volume bound to this claim, whatever its name
			}
		}
	}
	return true, nil
}

func hasNamePrefix(name, base string) bool {
	return len(name) > len(base) && name[:len(base)] == base && name[len(base)] == '-'
}

// networkAnchor is the instance identity of a network's policies: the bound UID
// (the record's own UID, or the one a rebuilt fence took over).
func networkAnchor(a *resRec) string {
	if a.rec.UID != "" {
		return a.rec.UID
	}
	return a.secret.Metadata.UID
}

// removeNetwork deletes the policies that verify as this network's (recorded UID,
// or marks, anchor and exact rendering) and confirms none of them remains. Run
// policies that merely look like ours are not deleted: a foreign one is ignored, one
// that carries our marks but cannot be verified (lost record) leaves removal
// unconfirmed.
func (r *RunResources) removeNetwork(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, sv specView) (done, unverified bool, err error) {
	sel := LabelResourceOwner + "=" + ValueResourceOwner + "," + LabelRunKey + "=" + RunKey(h.Scope, h.RunID) + "," + LabelResourceKind + "=" + string(execution.KindNetwork)
	anchor := networkAnchor(a)
	scan := func() (ours []kubernetes.NetworkPolicy, unv bool, err error) {
		all, err := r.cl.SelectNetworkPolicies(ctx, ns, sel)
		if err != nil {
			return nil, false, unavailable(err, "listing network policies")
		}
		for _, p := range all {
			if !hasNamePrefix(p.Metadata.Name, h.Name) {
				continue
			}
			p := p
			v, err := r.eligibleCompanion(ns, p.Metadata, p.Metadata.Name, h, a, anchor, sv, func(x runRender) error {
				want, err := x.networkPolicies(anchor, storedResolver(a.secret.Data))
				if err != nil {
					return err
				}
				for _, w := range want {
					if w.Metadata.Name == p.Metadata.Name {
						return verifyDoc("network policy", w, p.Raw, benignNone)
					}
				}
				return wrap(ErrForeignWorkload, "not a policy of the network")
			})
			if err != nil {
				return nil, false, err
			}
			switch v {
			case companionOurs:
				ours = append(ours, p)
			case companionUnverified:
				unv = true
			}
		}
		sort.Slice(ours, func(i, j int) bool { return ours[i].Metadata.Name < ours[j].Metadata.Name })
		return ours, unv, nil
	}
	pols, unv, err := scan()
	if err != nil {
		return false, false, err
	}
	for _, p := range pols {
		if err := deleteQuiet(r.cl.DeleteNetworkPolicy(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID}), "a network policy"); err != nil {
			return false, false, err
		}
	}
	left, unv2, err := scan()
	return err == nil && len(left) == 0, unv || unv2, err
}
