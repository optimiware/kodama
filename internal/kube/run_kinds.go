package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// This file holds the per-kind effects of the run-resource adapter: how each
// kind's objects are read and verified, created, observed, stopped and removed.
// The record Secret (run_record.go) drives every transition.

// ---- reads ----

func (r *RunResources) getJob(ctx context.Context, ns, name string) (kubernetes.Job, bool, error) {
	j, err := r.cl.GetJob(ctx, ns, name)
	switch {
	case err == nil:
		return j, true, nil
	case isNotFound(err):
		return kubernetes.Job{}, false, nil
	}
	return kubernetes.Job{}, false, unavailable(err, "reading the Job")
}

func (r *RunResources) getPod(ctx context.Context, ns, name string) (kubernetes.Pod, bool, error) {
	p, err := r.cl.GetPod(ctx, ns, name)
	switch {
	case err == nil:
		return p, true, nil
	case isNotFound(err):
		return kubernetes.Pod{}, false, nil
	}
	return kubernetes.Pod{}, false, unavailable(err, "reading the Pod")
}

func (r *RunResources) getClaim(ctx context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, bool, error) {
	p, err := r.cl.GetPVC(ctx, ns, name)
	switch {
	case err == nil:
		return p, true, nil
	case isNotFound(err):
		return kubernetes.PersistentVolumeClaim{}, false, nil
	}
	return kubernetes.PersistentVolumeClaim{}, false, unavailable(err, "reading the claim")
}

// Binding mode and reclaim policy values of a storage class the adapter relies on.
const (
	bindWaitForFirstConsumer = "WaitForFirstConsumer"
	reclaimDelete            = "Delete"
)

// getClass reads the workspace's storage class. found is false only for a
// definite not-found; any other failure is an outage that concludes nothing.
func (r *RunResources) getClass(ctx context.Context, name string) (kubernetes.StorageClass, bool, error) {
	sc, err := r.cl.GetStorageClass(ctx, name)
	switch {
	case err == nil:
		return sc, true, nil
	case isNotFound(err):
		return kubernetes.StorageClass{}, false, nil
	}
	return kubernetes.StorageClass{}, false, unavailable(err, "reading the storage class")
}

// deletesVolumes reports that a class deletes the volume of a deleted claim: a
// Retain class would leave scratch allocations (and their contents) behind and is
// never accepted.
func deletesVolumes(sc kubernetes.StorageClass) bool { return sc.ReclaimPolicy == reclaimDelete }

func (r *RunResources) getService(ctx context.Context, ns, name string) (kubernetes.Service, bool, error) {
	s, err := r.cl.GetService(ctx, ns, name)
	switch {
	case err == nil:
		return s, true, nil
	case isNotFound(err):
		return kubernetes.Service{}, false, nil
	}
	return kubernetes.Service{}, false, unavailable(err, "reading the Service")
}

func (r *RunResources) getPolicy(ctx context.Context, ns, name string) (kubernetes.NetworkPolicy, bool, error) {
	p, err := r.cl.GetNetworkPolicy(ctx, ns, name)
	switch {
	case err == nil:
		return p, true, nil
	case isNotFound(err):
		return kubernetes.NetworkPolicy{}, false, nil
	}
	return kubernetes.NetworkPolicy{}, false, unavailable(err, "reading the network policy")
}

// primaryExistsOf reports whether an object holds the name of the kind's primary
// object. The network has no single primary object.
func (r *RunResources) primaryExistsOf(ctx context.Context, ns string, h execution.ResourceHandle) (bool, error) {
	var found bool
	var err error
	switch h.Kind {
	case execution.KindWorkspace:
		_, found, err = r.getClaim(ctx, ns, h.Name)
	case execution.KindPreparation:
		_, found, err = r.getJob(ctx, ns, h.Name)
	case execution.KindService:
		_, found, err = r.getPod(ctx, ns, h.Name)
	}
	return found, err
}

func (r *RunResources) primaryExists(ctx context.Context, x runRender) (bool, error) {
	return r.primaryExistsOf(ctx, x.tgt.Namespace, x.spec.Handle(""))
}

// ---- generic create-or-adopt of a primary object ----

// primaryOps read and create the primary object of a kind. read verifies what it
// finds against the committed identity and spec and reports (found, uid).
type primaryOps struct {
	read   func(ctx context.Context) (found bool, uid string, err error)
	create func(ctx context.Context) error
}

// ensurePrimary creates or adopts the primary object under the record: intent is
// recorded before the create is sent, the fence is rechecked after it, a create
// of unknown outcome is resolved by reading and a definite refusal returns the
// record to registered. An object that exists is adopted only if it verified; a
// bound resource whose object vanished is never recreated.
func (r *RunResources) ensurePrimary(ctx context.Context, x runRender, a *resRec, ops primaryOps) error {
	ns := x.tgt.Namespace
	found, uid, err := ops.read(ctx)
	if err != nil {
		return err
	}
	if found {
		return r.bind(ctx, ns, a, uid)
	}
	if a.rec.Phase == recOwned {
		return nil // lost after it was bound: reported by observation, never recreated
	}
	op := newOpID()
	if err := a.rec.issue(r.now(), op); err != nil {
		return err
	}
	if err := r.saveRec(ctx, ns, a); err != nil {
		return err
	}
	// The create is not sent until the fence has been rechecked after the intent
	// was recorded: a Stop committed from here on finds the announced create and
	// waits for it to be unable to land.
	cur, err := readRec(ctx, r.cl, ns, identityOf(x), ownerLabels(x.spec.Scope, x.spec.RunID, x.spec.Kind), x.spec.Name)
	switch {
	case err != nil:
		return err
	case cur == nil || cur.rec.Fenced:
		return fenced("the resource was stopped before its create")
	}
	cerr := ops.create(ctx)
	switch {
	case cerr == nil, errors.Is(cerr, kubernetes.ErrAlreadyExists):
	case errors.Is(cerr, kubernetes.ErrUnknownOutcome):
		// The announced entry stays: the create may still land.
	case kubernetes.Definite(cerr):
		_ = a.rec.refused(op)
		if serr := r.saveRec(ctx, ns, a); serr != nil {
			return serr
		}
		kind := ErrUnavailable
		if errors.Is(cerr, kubernetes.ErrInvalid) || errors.Is(cerr, kubernetes.ErrForbidden) {
			kind = ErrPreflight
		}
		return wrapCause(kind, cerr, "the create was refused")
	default:
		return wrapCause(ErrCreateUncertain, cerr, "the create outcome is unknown")
	}
	found, uid, err = ops.read(ctx)
	if err != nil {
		return err
	}
	if !found {
		return wrap(ErrCreateUncertain, "the object is not visible after its create")
	}
	// Read back: this operation's own announcement is settled (and only its own).
	a.rec.endOp(op)
	return r.bind(ctx, ns, a, uid)
}

// newOpID is the unique id of one announced create.
func newOpID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// identityOf are the identity annotations (with the fingerprint) of a spec.
func identityOf(x runRender) map[string]string { return identOf(x.spec.Handle("")) }

// bind fixes the verified object's UID in the record. It never changes.
func (r *RunResources) bind(ctx context.Context, ns string, a *resRec, uid string) error {
	if err := a.rec.adopt(uid); err != nil {
		return asCollision(err)
	}
	return r.saveRec(ctx, ns, a)
}

// ensureCompanion creates a companion object if it is absent and verifies it
// either way; its verified UID is recorded. An object that exists but differs is a
// collision. Every create is announced first: the intent (a create that may
// still land) is persisted, the fence is rechecked after it and only then is the
// create sent, so a Stop that begins from here on cannot report the resource
// stopped, or Remove delete its record, until the create can no longer land.
func (r *RunResources) ensureCompanion(ctx context.Context, x runRender, a *resRec, name string, read func() (bool, string, error), create func() error) error {
	ns := x.tgt.Namespace
	note := func(uid string) error {
		if a.rec.noteCompanion(name, uid) {
			return r.saveRec(ctx, ns, a)
		}
		return nil
	}
	found, uid, err := read()
	if err != nil {
		return err
	}
	if found {
		return note(uid)
	}
	// The object is absent: whatever instance was recorded for it is gone, and the
	// one created now is recorded when it is read back.
	delete(a.rec.Companions, name)
	op := newOpID()
	a.rec.beginOp(op, r.now())
	if err := r.saveRec(ctx, ns, a); err != nil {
		return err
	}
	cur, err := readRec(ctx, r.cl, ns, identityOf(x), ownerLabels(x.spec.Scope, x.spec.RunID, x.spec.Kind), x.spec.Name)
	switch {
	case err != nil:
		return err
	case cur == nil || cur.rec.Fenced:
		return fenced("the resource was stopped before its create")
	}
	cerr := create()
	switch {
	case cerr == nil, errors.Is(cerr, kubernetes.ErrAlreadyExists):
	case errors.Is(cerr, kubernetes.ErrUnknownOutcome):
		// The announced intent stays: the create may still land.
	case kubernetes.Definite(cerr):
		kind := ErrUnavailable
		if errors.Is(cerr, kubernetes.ErrInvalid) || errors.Is(cerr, kubernetes.ErrForbidden) {
			kind = ErrPreflight
		}
		return wrapCause(kind, cerr, "the create was refused")
	default:
		return wrapCause(ErrCreateUncertain, cerr, "the create outcome is unknown")
	}
	found, uid, err = read()
	if err != nil {
		return err
	}
	if !found {
		return wrap(ErrCreateUncertain, "the object is not visible after its create")
	}
	// Read back: only this operation's own announcement is settled.
	a.rec.endOp(op)
	a.rec.noteCompanion(name, uid)
	return r.saveRec(ctx, ns, a)
}

// ---- provisioning ----

func (r *RunResources) provision(ctx context.Context, x runRender, a *resRec) error {
	ns := x.tgt.Namespace
	switch x.spec.Kind {
	case execution.KindWorkspace:
		claim, err := x.claim()
		if err != nil {
			return err
		}
		if a.rec.Phase != recOwned {
			// Nothing is allocated against a class that does not exist or that would
			// keep the volume after the claim is gone.
			sc, found, err := r.getClass(ctx, x.body.Workspace.StorageClass)
			switch {
			case err != nil:
				return err
			case !found:
				return wrap(ErrPolicy, "the workspace storage class does not exist")
			case !deletesVolumes(sc):
				return wrap(ErrPolicy, "the workspace storage class does not delete its volumes")
			}
		}
		err = r.ensurePrimary(ctx, x, a, primaryOps{
			read: func(ctx context.Context) (bool, string, error) {
				c, ok, err := r.getClaim(ctx, ns, x.spec.Name)
				if err != nil || !ok {
					return false, "", err
				}
				if err := r.verifyClaimObject(x, a, c, claim); err != nil {
					return false, "", err
				}
				return true, c.Metadata.UID, nil
			},
			create: func(ctx context.Context) error { _, err := r.cl.CreatePVC(ctx, ns, claim); return err },
		})
		if err != nil {
			return err
		}
	case execution.KindPreparation:
		job, err := x.prepJob()
		if err != nil {
			return err
		}
		if err := r.requireWorkspace(ctx, x); err != nil {
			return err
		}
		err = r.ensurePrimary(ctx, x, a, primaryOps{
			read: func(ctx context.Context) (bool, string, error) {
				j, ok, err := r.getJob(ctx, ns, x.spec.Name)
				if err != nil || !ok {
					return false, "", err
				}
				if err := r.verifyJobObject(x, a, j, job); err != nil {
					return false, "", err
				}
				return true, j.Metadata.UID, nil
			},
			create: func(ctx context.Context) error { _, err := r.cl.CreateJob(ctx, ns, job); return err },
		})
		if err != nil {
			return err
		}
	case execution.KindService:
		return r.provisionService(ctx, x, a)
	case execution.KindNetwork:
		return r.provisionNetwork(ctx, x, a)
	}
	return nil
}

// requireWorkspace refuses to start the preparation against a workspace that does
// not exist, is not the run's, is terminating or was stopped.
func (r *RunResources) requireWorkspace(ctx context.Context, x runRender) error {
	h := execution.ResourceHandle{
		Scope: x.spec.Scope, RunID: x.spec.RunID, Kind: execution.KindWorkspace, Key: execution.KeyWorkspace,
		Name: execution.ResourceName(x.spec.Scope, x.spec.RunID, execution.KindWorkspace, execution.KeyWorkspace),
	}
	ns := x.tgt.Namespace
	c, ok, err := r.getClaim(ctx, ns, h.Name)
	switch {
	case err != nil:
		return err
	case !ok:
		return wrap(ErrNotDispatchable, "the run workspace claim does not exist")
	}
	if err := checkOwned(c.Metadata, ns, h.Name, ownedOf(h, "")); err != nil {
		return asCollision(err)
	}
	if c.Metadata.DeletionTimestamp != "" {
		return wrap(ErrNotDispatchable, "the run workspace claim is being deleted")
	}
	a, err := readRec(ctx, r.cl, ns, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil:
		return asCollision(err)
	case a == nil || a.rec.Fenced:
		return fenced("the run workspace was stopped")
	case a.rec.UID != c.Metadata.UID:
		return collision("the run workspace record binds another claim")
	}
	return nil
}

func (r *RunResources) verifyClaimObject(x runRender, a *resRec, c kubernetes.PersistentVolumeClaim, want kubernetes.PersistentVolumeClaim) error {
	if err := checkOwned(c.Metadata, x.tgt.Namespace, x.spec.Name, x.want(a.rec.UID)); err != nil {
		return asCollision(err)
	}
	return asCollision(verifyDoc("claim", want, c.Raw, benignClaim))
}

func (r *RunResources) verifyJobObject(x runRender, a *resRec, j kubernetes.Job, want kubernetes.Job) error {
	if err := checkOwned(j.Metadata, x.tgt.Namespace, x.spec.Name, x.want(a.rec.UID)); err != nil {
		return asCollision(err)
	}
	return asCollision(verifyDoc("Job", want, j.Raw, benignJob(j.Metadata.UID, j.Metadata.Name)))
}

func (r *RunResources) verifyPodObject(x runRender, a *resRec, p kubernetes.Pod, want kubernetes.Pod) error {
	if err := checkOwned(p.Metadata, x.tgt.Namespace, x.spec.Name, x.want(a.rec.UID)); err != nil {
		return asCollision(err)
	}
	return asCollision(verifyDoc("Pod", want, p.Raw, benignPod))
}

func (r *RunResources) provisionService(ctx context.Context, x runRender, a *resRec) error {
	ns := x.tgt.Namespace
	pod, err := x.servicePod()
	if err != nil {
		return err
	}
	svc, err := x.serviceObject()
	if err != nil {
		return err
	}
	pol := x.ingressPolicy(x.body.Network.Isolation)
	err = r.ensurePrimary(ctx, x, a, primaryOps{
		read: func(ctx context.Context) (bool, string, error) {
			p, ok, err := r.getPod(ctx, ns, x.spec.Name)
			if err != nil || !ok {
				return false, "", err
			}
			if err := r.verifyPodObject(x, a, p, pod); err != nil {
				return false, "", err
			}
			return true, p.Metadata.UID, nil
		},
		create: func(ctx context.Context) error { _, err := r.cl.CreatePod(ctx, ns, pod); return err },
	})
	if err != nil || a.rec.Phase != recOwned {
		return err
	}
	// Companions carry the Pod's UID, so a record that is later lost can still tell
	// them from lookalikes.
	svc.Metadata.Annotations[AnnResourceUID] = a.rec.UID
	pol.Metadata.Annotations[AnnResourceUID] = a.rec.UID
	if err := r.ensureCompanion(ctx, x, a, svc.Metadata.Name,
		func() (bool, string, error) { return r.readServiceObject(ctx, x, a, svc) },
		func() error { _, err := r.cl.CreateService(ctx, ns, svc); return err },
	); err != nil {
		return err
	}
	if err := r.ensureCompanion(ctx, x, a, pol.Metadata.Name,
		func() (bool, string, error) { return r.readPolicy(ctx, x, a, pol, a.rec.UID) },
		func() error { _, err := r.cl.CreateNetworkPolicy(ctx, ns, pol); return err },
	); err != nil {
		return err
	}
	return nil
}

// readServiceObject reads and verifies the service's endpoint object.
func (r *RunResources) readServiceObject(ctx context.Context, x runRender, a *resRec, want kubernetes.Service) (bool, string, error) {
	ns := x.tgt.Namespace
	s, ok, err := r.getService(ctx, ns, want.Metadata.Name)
	if err != nil || !ok {
		return false, "", err
	}
	if err := checkOwned(s.Metadata, ns, want.Metadata.Name, x.want("")); err != nil {
		return false, "", asCollision(err)
	}
	if s.Metadata.Annotations[AnnResourceUID] != a.rec.UID {
		return false, "", collision("the Service belongs to another instance of the resource")
	}
	if rec := a.rec.Companions[want.Metadata.Name]; rec != "" && rec != s.Metadata.UID {
		return false, "", collision("the Service is not the instance this resource created")
	}
	return true, s.Metadata.UID, asCollision(verifyDoc("Service", want, s.Raw, benignService))
}

// readPolicy reads and verifies one owned network policy against its rendering:
// ownership marks, the instance anchor (the record's UID for a network, the Pod's
// for a service), the UID recorded when it was created and the controlled spec.
func (r *RunResources) readPolicy(ctx context.Context, x runRender, a *resRec, want kubernetes.NetworkPolicy, anchor string) (bool, string, error) {
	ns := x.tgt.Namespace
	p, ok, err := r.getPolicy(ctx, ns, want.Metadata.Name)
	if err != nil || !ok {
		return false, "", err
	}
	if err := checkOwned(p.Metadata, ns, want.Metadata.Name, x.want("")); err != nil {
		return false, "", asCollision(err)
	}
	if p.Metadata.Annotations[AnnResourceUID] != anchor {
		return false, "", collision("the policy belongs to another instance of the resource")
	}
	if rec := a.rec.Companions[want.Metadata.Name]; rec != "" && rec != p.Metadata.UID {
		return false, "", collision("the policy is not the instance this resource created")
	}
	return true, p.Metadata.UID, asCollision(verifyDoc("network policy", want, p.Raw, benignNone))
}

// noExtraPolicies refuses a run-owned policy that names this resource but is not
// one of the policies its spec renders. NetworkPolicies are additive, so an extra
// owned-looking policy (a wider peer, another port, a destination the run never
// pinned) would widen the boundary silently: the resource is not ready, it is a
// collision, while such a policy exists.
func (r *RunResources) noExtraPolicies(ctx context.Context, x runRender, h execution.ResourceHandle, expected []string) error {
	sel := LabelResourceOwner + "=" + ValueResourceOwner + "," + LabelRunKey + "=" + RunKey(h.Scope, h.RunID)
	all, err := r.cl.SelectNetworkPolicies(ctx, x.tgt.Namespace, sel)
	if err != nil {
		return unavailable(err, "listing network policies")
	}
	for _, p := range all {
		if p.Metadata.Annotations[AnnResourceName] != h.Name || slices.Contains(expected, p.Metadata.Name) {
			continue
		}
		return collision("a run policy the resource does not render exists")
	}
	return nil
}

func (r *RunResources) networkPolicies(ctx context.Context, x runRender, a *resRec) ([]kubernetes.NetworkPolicy, error) {
	return x.networkPolicies(networkAnchor(a), storedResolver(a.secret.Data))
}

func (r *RunResources) provisionNetwork(ctx context.Context, x runRender, a *resRec) error {
	ns := x.tgt.Namespace
	pols, err := r.networkPolicies(ctx, x, a)
	if err != nil {
		return err
	}
	if a.rec.Phase != recOwned {
		// Phase only: every policy create is announced by its own operation.
		op := newOpID()
		if err := a.rec.issue(r.now(), op); err != nil {
			return err
		}
		a.rec.endOp(op)
		if err := r.saveRec(ctx, ns, a); err != nil {
			return err
		}
		cur, err := readRec(ctx, r.cl, ns, identityOf(x), ownerLabels(x.spec.Scope, x.spec.RunID, x.spec.Kind), x.spec.Name)
		switch {
		case err != nil:
			return err
		case cur == nil || cur.rec.Fenced:
			return fenced("the resource was stopped before its create")
		}
	}
	anchor := networkAnchor(a)
	for _, p := range pols {
		p := p
		if err := r.ensureCompanion(ctx, x, a, p.Metadata.Name,
			func() (bool, string, error) { return r.readPolicy(ctx, x, a, p, anchor) },
			func() error { _, err := r.cl.CreateNetworkPolicy(ctx, ns, p); return err },
		); err != nil {
			return err
		}
	}
	if err := r.bind(ctx, ns, a, anchor); err != nil {
		return err
	}
	return nil
}

// ---- observation ----

// observeVerified reads the resource's objects, verifies them against the
// rendering of the committed spec and reports the phase. The observation carries
// the spec fingerprint only for an object that verified.
func (r *RunResources) observeVerified(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	obs, err := r.observeKind(ctx, x, a, h)
	switch {
	case err == nil:
		return obs, nil
	case errors.Is(err, execution.ErrResourceCollision), errors.Is(err, execution.ErrResourceFenced):
		return execution.ResourceObservation{}, err
	case errors.Is(err, ErrForeignWorkload):
		return execution.ResourceObservation{}, asCollision(err)
	}
	// An outage or any other failure concludes nothing: never absent.
	return r.unknown(h, reasonUnobservable), nil
}

func (r *RunResources) verified(x runRender, a *resRec, phase execution.ResourcePhase) execution.ResourceObservation {
	now := r.now().UTC()
	o := execution.ResourceObservation{
		Handle: x.spec.Handle(a.rec.UID), Fingerprint: x.fp, Phase: phase, ObservedAt: now,
	}
	if phase == execution.PhaseReady {
		o.ReadyAt = &now
	}
	return o
}

// absent reports the definite absence of the primary object: a bound resource's
// object that is gone is absent; one that was never bound is absent only when no
// create can still land.
func (r *RunResources) absent(x runRender, a *resRec, h execution.ResourceHandle) execution.ResourceObservation {
	if a.rec.UID == "" && !a.rec.neverIssued() && !a.rec.quiescent(r.now(), r.opts.CreateQuiescence) {
		return r.unknown(h, reasonUnobservable)
	}
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: r.now().UTC()}
}

func (r *RunResources) observeKind(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	switch x.spec.Kind {
	case execution.KindWorkspace:
		return r.observeWorkspace(ctx, x, a, h)
	case execution.KindPreparation:
		return r.observePreparation(ctx, x, a, h)
	case execution.KindService:
		return r.observeService(ctx, x, a, h)
	}
	return r.observeNetwork(ctx, x, a, h)
}

func (r *RunResources) observeWorkspace(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	claim, err := x.claim()
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	c, ok, err := r.getClaim(ctx, x.tgt.Namespace, x.spec.Name)
	switch {
	case err != nil:
		return execution.ResourceObservation{}, err
	case !ok:
		return r.absent(x, a, h), nil
	}
	if err := r.verifyClaimObject(x, a, c, claim); err != nil {
		return execution.ResourceObservation{}, err
	}
	released, err := r.released(ctx, x.tgt.Namespace, h, a, &c)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	sc, haveClass, err := r.getClass(ctx, x.body.Workspace.StorageClass)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	phase := execution.PhasePending
	reason := execution.ResourceReason("")
	switch {
	case c.Metadata.DeletionTimestamp != "" && !a.rec.Fenced, c.Status.Phase == "Lost":
		phase, reason = execution.PhaseFailed, execution.ReasonWorkspaceLost
	case haveClass && !deletesVolumes(sc):
		// A class that keeps volumes is never accepted, bound or not.
		phase, reason = execution.PhaseFailed, execution.ReasonCapabilityUnsupported
	case c.Status.Phase == "Bound":
		if !haveClass {
			return execution.ResourceObservation{}, wrap(ErrUnavailable, "the storage class of a bound claim is not readable")
		}
		phase = execution.PhaseReady
	case haveClass && c.Spec.VolumeName == "" && sc.VolumeBindingMode == bindWaitForFirstConsumer &&
		c.Spec.StorageClassName != nil && *c.Spec.StorageClassName == x.body.Workspace.StorageClass:
		// A claim of a class that binds on first use is usable as it is: the first
		// consumer (the preparation, or the first attempt of a source-free run) is
		// what provisions its volume, and nothing can consume it before it is ready.
		phase = execution.PhaseReady
	}
	o := r.verified(x, a, phase)
	o.Reason, o.Released = reason, released
	return o, nil
}

// released reports that no process or mount on any node can still use the
// claim's volume. It is decided by the claim's durable identity (its name, its UID
// as committed and every volume bound to that UID), not by the claim object being
// present: no Pod of the namespace references the claim (whatever its phase) and
// no VolumeAttachment of any of its volumes remains. c is the claim as the caller
// verified it (nil when it is absent or not ours). Anything that cannot be read,
// and a claim that vanished or was replaced between the reads, is "not
// confirmed", never released.
func (r *RunResources) released(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, c *kubernetes.PersistentVolumeClaim) (bool, error) {
	pods, err := r.cl.ListPods(ctx, ns, "")
	if err != nil {
		return false, unavailable(err, "listing Pods")
	}
	for _, p := range pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == h.Name {
				return false, nil
			}
		}
	}
	uids := uidsOf(h, a)
	if c != nil {
		uids = append(uids, c.Metadata.UID)
	}
	// A claim that had not bound when it was first read may have bound (and been
	// attached for a Pod that is already gone) since: read it again after the Pods
	// were listed.
	fresh, ok, err := r.getClaim(ctx, ns, h.Name)
	if err != nil {
		return false, err
	}
	switch {
	case c != nil && (!ok || fresh.Metadata.UID != c.Metadata.UID):
		return false, nil
	case c == nil && ok && !slices.Contains(uids, fresh.Metadata.UID):
		ok = false // somebody else's claim of the name: its volume is not ours to consider
	}
	vols, err := r.volumesOf(ctx, ns, h, a, uids, fresh, ok)
	if err != nil {
		return false, err
	}
	if len(vols) == 0 {
		return true, nil
	}
	vas, err := r.cl.ListVolumeAttachments(ctx)
	if err != nil {
		return false, unavailable(err, "listing volume attachments")
	}
	for _, va := range vas {
		if va.Spec.Source.PersistentVolumeName != nil && slices.Contains(vols, *va.Spec.Source.PersistentVolumeName) {
			return false, nil
		}
	}
	return true, nil
}

// volumesOf lists the volume names of the claim's allocation: the one the claim
// reports, the recorded one, the provisioner's deterministic name for each
// committed claim UID and every volume whose claimRef is exactly this claim
// (namespace, name and a committed UID), whatever its own name.
func (r *RunResources) volumesOf(ctx context.Context, ns string, h execution.ResourceHandle, a *resRec, uids []string, c kubernetes.PersistentVolumeClaim, haveClaim bool) ([]string, error) {
	var vols []string
	add := func(v string) {
		if v != "" && !slices.Contains(vols, v) {
			vols = append(vols, v)
		}
	}
	if haveClaim {
		add(c.Spec.VolumeName)
	}
	add(a.rec.Volume)
	for _, u := range uids {
		add("pvc-" + u)
	}
	if len(uids) > 0 {
		pvs, err := r.cl.ListPVs(ctx)
		if err != nil {
			return nil, unavailable(err, "listing volumes")
		}
		for _, pv := range pvs {
			if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == ns && ref.Name == h.Name && slices.Contains(uids, ref.UID) {
				add(pv.Metadata.Name)
			}
		}
	}
	return vols, nil
}

// podsOfJob lists the Pods controlled by the Job of the given UIDs. Labels select
// candidates; a Pod counts only with the controlling owner reference.
func (r *RunResources) podsOfJob(ctx context.Context, ns string, h execution.ResourceHandle, uids ...string) ([]kubernetes.Pod, error) {
	pods, err := r.cl.ListPods(ctx, ns, LabelResourceOwner+"="+ValueResourceOwner+","+LabelRunKey+"="+RunKey(h.Scope, h.RunID)+","+LabelResourceKind+"="+string(h.Kind))
	if err != nil {
		return nil, unavailable(err, "listing Pods")
	}
	var out []kubernetes.Pod
	for _, p := range pods {
		for _, u := range uids {
			if controlledBy(p, h.Name, u) {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

// ownedPods are the Pods of a preparation or service for log reading.
func (r *RunResources) ownedPods(ctx context.Context, ns string, h execution.ResourceHandle, uids ...string) ([]kubernetes.Pod, error) {
	if h.Kind == execution.KindService {
		p, ok, err := r.getPod(ctx, ns, h.Name)
		if err != nil || !ok {
			return nil, err
		}
		if err := checkOwned(p.Metadata, ns, h.Name, ownedOf(h, "")); err != nil || !slices.Contains(nonEmpty(uids), p.Metadata.UID) {
			return nil, collision("the Pod is not the resource's")
		}
		return []kubernetes.Pod{p}, nil
	}
	var ids []string
	j, ok, err := r.getJob(ctx, ns, h.Name)
	if err != nil {
		return nil, err
	}
	if ok && slices.Contains(nonEmpty(uids), j.Metadata.UID) {
		ids = append(ids, j.Metadata.UID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return r.podsOfJob(ctx, ns, h, ids...)
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *RunResources) observePreparation(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	ns := x.tgt.Namespace
	want, err := x.prepJob()
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	j, ok, err := r.getJob(ctx, ns, x.spec.Name)
	switch {
	case err != nil:
		return execution.ResourceObservation{}, err
	case !ok:
		return r.absent(x, a, h), nil
	}
	if err := r.verifyJobObject(x, a, j, want); err != nil {
		return execution.ResourceObservation{}, err
	}
	pods, err := r.podsOfJob(ctx, ns, h, j.Metadata.UID)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	o := r.verified(x, a, execution.PhasePending)
	o.Handle.UID = j.Metadata.UID
	switch {
	case jobFailed(j):
		o.Phase, o.Reason = execution.PhaseFailed, execution.ReasonPreparationFailed
	case isComplete(j):
		o.Phase = execution.PhaseReady
		now := r.now().UTC()
		o.ReadyAt = &now
	default:
		for _, p := range pods {
			if p.Status.Phase == "Failed" {
				o.Phase, o.Reason = execution.PhaseFailed, execution.ReasonPreparationFailed
			}
		}
	}
	if o.Phase == execution.PhaseFailed {
		o.ExitCode, o.LogRef = exitOf(pods), h.Name
	}
	return o, nil
}

func isComplete(j kubernetes.Job) bool {
	_, ok := j.Status.Condition("Complete")
	return ok
}

// exitOf returns the exit code of the primary container of the last terminated Pod.
func exitOf(pods []kubernetes.Pod) *int64 {
	for i := len(pods) - 1; i >= 0; i-- {
		if s, ok := containerStatus(pods[i].Status.ContainerStatuses, PrimaryContainer); ok && s.State.Terminated != nil {
			c := int64(s.State.Terminated.ExitCode)
			return &c
		}
	}
	return nil
}

func (r *RunResources) observeService(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	ns := x.tgt.Namespace
	want, err := x.servicePod()
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	p, ok, err := r.getPod(ctx, ns, x.spec.Name)
	switch {
	case err != nil:
		return execution.ResourceObservation{}, err
	case !ok:
		return r.absent(x, a, h), nil
	}
	if err := r.verifyPodObject(x, a, p, want); err != nil {
		return execution.ResourceObservation{}, err
	}
	o := r.verified(x, a, execution.PhasePending)
	o.Handle.UID = p.Metadata.UID
	if a.rec.Fenced && p.Metadata.DeletionTimestamp != "" {
		return o, nil
	}
	switch p.Status.Phase {
	case "Failed", "Succeeded":
		// A service that exits for any reason before the run settles is lost.
		o.Phase, o.Reason, o.LogRef = execution.PhaseFailed, execution.ReasonServiceLost, h.Name
		o.ExitCode = exitOf([]kubernetes.Pod{p})
		return o, nil
	case "Running":
		if podReady(p) {
			o.Phase = execution.PhaseReady
			now := r.now().UTC()
			o.ReadyAt, o.ProbeKind = &now, x.spec.Service.Readiness.Probe
		}
	}
	// The endpoint exists only with its Service and its ingress policy: readiness
	// without them is not reachability.
	if o.Phase == execution.PhaseReady {
		svc, err := x.serviceObject()
		if err != nil {
			return execution.ResourceObservation{}, err
		}
		pol := x.ingressPolicy(x.body.Network.Isolation)
		for _, check := range []func() (bool, string, error){
			func() (bool, string, error) { return r.readServiceObject(ctx, x, a, svc) },
			func() (bool, string, error) { return r.readPolicy(ctx, x, a, pol, a.rec.UID) },
		} {
			found, _, err := check()
			if err != nil {
				return execution.ResourceObservation{}, err
			}
			if !found {
				o.Phase, o.ReadyAt, o.ProbeKind = execution.PhasePending, nil, ""
				break
			}
		}
	}
	if err := r.noExtraPolicies(ctx, x, h, []string{x.spec.Name + suffixIngress}); err != nil {
		return execution.ResourceObservation{}, err
	}
	return o, nil
}

// podReady reports the Pod's Ready condition together with its primary
// container's readiness.
func podReady(p kubernetes.Pod) bool {
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
		}
	}
	s, ok := containerStatus(p.Status.ContainerStatuses, PrimaryContainer)
	return ready && ok && s.Ready
}

func (r *RunResources) observeNetwork(ctx context.Context, x runRender, a *resRec, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	pols, err := r.networkPolicies(ctx, x, a)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	missing := 0
	names := make([]string, 0, len(pols))
	for _, p := range pols {
		names = append(names, p.Metadata.Name)
		found, _, err := r.readPolicy(ctx, x, a, p, networkAnchor(a))
		if err != nil {
			return execution.ResourceObservation{}, err
		}
		if !found {
			missing++
		}
	}
	if err := r.noExtraPolicies(ctx, x, h, names); err != nil {
		return execution.ResourceObservation{}, err
	}
	o := r.verified(x, a, execution.PhaseReady)
	o.Handle.UID = a.rec.UID
	switch {
	case missing > 0 && a.rec.Phase == recOwned:
		// A policy of a bound network was deleted: the boundary is lost.
		o.Phase, o.ReadyAt = execution.PhaseFailed, nil
	case missing > 0, a.rec.Phase != recOwned:
		o.Phase, o.ReadyAt = execution.PhasePending, nil
	}
	return o, nil
}
