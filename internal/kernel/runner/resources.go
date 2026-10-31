package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ensureResult is the outcome of ensuring one resource.
type ensureResult int

const (
	// ensured: the resource exists and was recorded (it may still be starting).
	ensured ensureResult = iota
	// ensureFailed: the resource provably does not exist and will not be retried.
	ensureFailed
	// ensureInconclusive: the backend could not be asked; reconcile later.
	ensureInconclusive
)

func ensureCommandID(runID, kind, name string) string {
	return fmt.Sprintf("%s:%s:%s:%s", runtime.CommandEnsureResource, runID, kind, name)
}

// ensureID is the identity of the next ensure command of a resource: a rejected
// (never reached) one may be issued again as a new command.
func (d *drive) ensureID(h runtime.ResourceHandle) string {
	base := ensureCommandID(string(d.rec.Run.ID), string(h.Kind), h.Name)
	id := base
	for n := 2; ; n++ {
		c, ok := d.command(id)
		if !ok || c.State != runtime.CommandRejected {
			return id
		}
		id = fmt.Sprintf("%s.%d", base, n)
	}
}

func digestOf(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// workspaceSpec asks the preparer for the pinned inputs and builds the one
// ResourceSpec that creates or adopts the run's workspace. A nil preparer means
// a source-free run.
func (d *drive) workspaceSpec() (runtime.ResourceSpec, error) {
	r, plan := d.rec.Run, d.rec.Plan
	h := d.workspaceHandle()
	inputs := runtime.PreparationInputs{Scope: r.Scope, RunID: r.ID, Digest: "source-free:" + plan.Digest}
	if p := d.e.cfg.Workspace; p != nil {
		got, err := p.Inputs(d.ctx, runtime.PrepareRequest{Scope: r.Scope, RunID: r.ID, PlanDigest: plan.Digest, Inputs: plan.Inputs})
		if err != nil {
			return runtime.ResourceSpec{}, err
		}
		if err := got.Validate(); err != nil {
			return runtime.ResourceSpec{}, err
		}
		if got.Scope != r.Scope || got.RunID != r.ID {
			return runtime.ResourceSpec{}, fmt.Errorf("runner: the preparer answered for another run")
		}
		inputs = got
	}
	digest, err := digestOf([]string{plan.Digest, inputs.Digest})
	if err != nil {
		return runtime.ResourceSpec{}, err
	}
	return runtime.ResourceSpec{Scope: r.Scope, Owner: h.Owner, RunID: r.ID, Kind: runtime.ResourceWorkspace, Name: h.Name,
		Lifetime: h.Lifetime, Identity: h.Identity, SpecDigest: digest, Fence: d.claim.Fence, Preparation: &inputs}, nil
}

func (d *drive) serviceSpec(s runtime.ServiceSpec) (runtime.ResourceSpec, error) {
	h := d.serviceHandle(s)
	digest, err := digestOf(s)
	if err != nil {
		return runtime.ResourceSpec{}, err
	}
	caps := runtime.CapabilitySet{runtime.CapRunServices}
	if s.Lifetime != runtime.LifetimePipeline {
		caps = append(caps, runtime.CapPersistentService)
	}
	spec := runtime.ResourceSpec{Scope: h.Scope, Owner: h.Owner, RunID: h.RunID, Kind: runtime.ResourceService, Name: s.Name,
		Lifetime: s.Lifetime, Identity: h.Identity, SpecDigest: digest, Fence: d.claim.Fence, Service: &s, Requires: caps}
	return spec, nil
}

// admitCapabilities refuses a run its backends cannot satisfy before anything is
// created: every step's required capabilities against the runtime, and the
// services against the resource manager.
func (d *drive) admitCapabilities() (bool, error) {
	rc, err := d.e.cfg.Runtime.Capabilities(d.ctx)
	if err != nil {
		return false, err
	}
	for _, p := range d.rec.Plan.Steps {
		if p.Workload != nil && rc.Satisfies(p.Workload.Requires) != nil {
			return false, nil
		}
	}
	mc, err := d.e.cfg.Resources.Capabilities(d.ctx)
	if err != nil {
		return false, err
	}
	for _, s := range d.rec.Plan.Services {
		spec, err := d.serviceSpec(s)
		if err != nil {
			return false, err
		}
		if mc.Satisfies(spec.Requires) != nil {
			return false, nil
		}
	}
	return true, nil
}

// stagePrepare creates the run's workspace and services once, with intent
// committed first, and waits for run-scoped services to become ready before any
// step starts. A resource is never created again: a run that has started and
// finds one missing fails with workspace_lost or service_lost instead.
func (d *drive) stagePrepare() (flow, error) {
	r, plan := d.rec.Run, d.rec.Plan
	started := false
	for _, s := range r.Steps {
		if s.State != runtime.StepPending {
			started = true
		}
	}
	type item struct {
		h    runtime.ResourceHandle
		svc  *runtime.ServiceSpec
		lost runtime.Reason
	}
	items := []item{{h: d.workspaceHandle(), lost: runtime.ReasonWorkspaceLost}}
	for i := range plan.Services {
		items = append(items, item{h: d.serviceHandle(plan.Services[i]), svc: &plan.Services[i], lost: runtime.ReasonServiceLost})
	}
	for _, it := range items {
		if d.ensured(it.h) {
			continue
		}
		if started {
			return d.windDown(modeFail, it.lost)
		}
		if first := it.h.Kind == runtime.ResourceWorkspace; first {
			ok, err := d.admitCapabilities()
			if err != nil {
				return flowPass, err
			}
			if !ok {
				return d.windDown(modeFail, runtime.ReasonCapabilityUnsatisfied)
			}
		}
		var spec runtime.ResourceSpec
		var err error
		if it.svc == nil {
			spec, err = d.workspaceSpec()
		} else {
			spec, err = d.serviceSpec(*it.svc)
		}
		if err != nil {
			return d.windDown(modeFail, runtime.ReasonPreparationFailed)
		}
		res, err := d.ensureResource(spec)
		switch {
		case err != nil:
			return flowPass, err
		case res == ensureFailed:
			return d.windDown(modeFail, runtime.ReasonPreparationFailed)
		case res == ensureInconclusive:
			return d.stop(DriveRetry)
		}
		return flowAgain, nil
	}
	return d.awaitServices()
}

// ensured reports whether the resource has a confirmed ensure command.
func (d *drive) ensured(h runtime.ResourceHandle) bool {
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandEnsureResource && c.State == runtime.CommandConfirmed && c.Target == h.Identity {
			return true
		}
	}
	return false
}

// ensureResource creates or adopts one resource: the intent commits before the
// call, an outcome that cannot be told is reconciled by observing the resource
// by its identity, and a call that provably did not reach the backend is the
// only one issued again.
func (d *drive) ensureResource(spec runtime.ResourceSpec) (ensureResult, error) {
	if err := spec.Validate(); err != nil {
		return ensureFailed, nil
	}
	h := spec.Handle()
	for try := 0; ; try++ {
		id := d.ensureID(h)
		cmd, exists := d.command(id)
		if !exists {
			nc, err := runtime.NewCommand(id, spec.Scope, d.rec.Run.ID, "", runtime.CommandEnsureResource, spec.Identity, spec.SpecDigest, d.claim.Fence, d.now())
			if err != nil {
				return ensureFailed, err
			}
			bt := d.batch()
			bt.commands = []runtime.Command{nc}
			if err := d.commit(bt); err != nil {
				return ensureFailed, err
			}
			cmd = nc
		} else if cmd.State != runtime.CommandIntent && cmd.State != runtime.CommandUncertain {
			return ensured, nil
		} else {
			// Left by an earlier claim: observe, never issue it again.
			return d.reconcileEnsure(cmd, h)
		}

		obs, err := d.e.cfg.Resources.Ensure(d.ctx, spec)
		state := runtime.ClassifyEffect(err)
		if state == runtime.CommandConfirmed {
			o, aerr := d.acceptResource(obs, h)
			if aerr != nil {
				state = runtime.CommandUncertain
			} else {
				obs = o
			}
		}
		switch state {
		case runtime.CommandConfirmed:
			return ensured, d.confirmEnsure(cmd, obs)
		case runtime.CommandRejected:
			c := cmd
			if err := c.Settle(runtime.CommandRejected, runtime.ReasonLaunchNotReached); err != nil {
				return ensureFailed, err
			}
			bt := d.batch()
			bt.commands = []runtime.Command{c}
			if err := d.commit(bt); err != nil {
				return ensureFailed, err
			}
			if r := d.e.cfg.LaunchRetries; r < 0 || try >= r {
				return ensureFailed, nil
			}
			continue
		}
		c := cmd
		if err := c.Settle(runtime.CommandUncertain, runtime.ReasonLaunchUncertain); err != nil {
			return ensureFailed, err
		}
		bt := d.batch()
		bt.commands = []runtime.Command{c}
		if err := d.commit(bt); err != nil {
			return ensureFailed, err
		}
		return d.reconcileEnsure(c, h)
	}
}

// reconcileEnsure settles an ensure whose outcome is unknown by observing the
// resource: one that exists is adopted; one that is not there is not created
// again, and the run fails; a backend that cannot answer leaves it for later.
func (d *drive) reconcileEnsure(cmd runtime.Command, h runtime.ResourceHandle) (ensureResult, error) {
	obs, err := d.e.cfg.Resources.Observe(d.ctx, h)
	if err != nil {
		return ensureInconclusive, nil
	}
	o, err := d.acceptResource(obs, h)
	if err != nil {
		return ensureFailed, nil
	}
	switch o.State {
	case runtime.ResourceStarting, runtime.ResourceReady, runtime.ResourceFailed:
		return ensured, d.confirmEnsure(cmd, o)
	case runtime.ResourceUnknown:
		return ensureInconclusive, nil
	}
	c := cmd
	if err := c.Settle(runtime.CommandRejected, runtime.ReasonLaunchUncertain); err != nil {
		return ensureFailed, err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	return ensureFailed, d.commit(bt)
}

func (d *drive) acceptResource(obs runtime.ResourceObservation, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	if err := obs.Validate(); err != nil {
		return runtime.ResourceObservation{}, err
	}
	if !sameResource(obs.Handle, h) {
		return runtime.ResourceObservation{}, errForeignObservation
	}
	obs.Handle = h
	return obs, nil
}

func (d *drive) confirmEnsure(cmd runtime.Command, obs runtime.ResourceObservation) error {
	c := cmd
	if err := c.Settle(runtime.CommandConfirmed, runtime.ReasonNone); err != nil {
		return err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	bt.resources = []runtime.ResourceObservation{obs}
	if obs.Handle.Kind == runtime.ResourceService {
		bt.events = append(bt.events, pendingEvent{kind: runtime.EventServiceStateChanged, suffix: obs.Handle.Name, reason: obs.Reason})
	}
	return d.commit(bt)
}

// awaitServices waits for every service of the plan to be ready before a step
// starts. A service that failed to start fails the run; one still starting past
// the ready timeout does too.
func (d *drive) awaitServices() (flow, error) {
	starting := false
	for _, s := range d.rec.Plan.Services {
		h := d.serviceHandle(s)
		o, ok := d.resourceObservation(h)
		if !ok {
			return d.windDown(modeFail, runtime.ReasonServiceLost)
		}
		switch o.State {
		case runtime.ResourceReady:
			continue
		case runtime.ResourceStarting:
			starting = true
		default:
			if d.started() {
				return d.windDown(modeFail, runtime.ReasonServiceLost)
			}
			return d.windDown(modeFail, runtime.ReasonPreparationFailed)
		}
		obs, err := d.e.cfg.Resources.Observe(d.ctx, h)
		if err != nil {
			continue
		}
		if n, aerr := d.acceptResource(obs, h); aerr == nil && n.State != o.State {
			bt := d.batch()
			bt.resources = []runtime.ResourceObservation{n}
			bt.events = append(bt.events, pendingEvent{kind: runtime.EventServiceStateChanged, suffix: n.Handle.Name, reason: n.Reason})
			if err := d.commit(bt); err != nil {
				return flowPass, err
			}
			return flowAgain, nil
		}
	}
	if !starting {
		return flowPass, nil
	}
	if c, ok := d.command(d.lastEnsure()); ok && d.now().Sub(c.RecordedAt) >= d.e.cfg.ReadyTimeout {
		return d.windDown(modeFail, runtime.ReasonPreparationFailed)
	}
	if err := d.pause(); err != nil {
		return flowPass, err
	}
	return flowAgain, nil
}

func (d *drive) lastEnsure() string {
	var last string
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandEnsureResource {
			last = c.ID
		}
	}
	return last
}

func (d *drive) started() bool {
	for _, s := range d.rec.Run.Steps {
		if s.State != runtime.StepPending {
			return true
		}
	}
	return false
}

// verify checks that the workspace and every service the run uses still exist
// and are ready. It reports a lost resource's reason, or ok. An observation
// that cannot be made, or that is inconclusive, is transient: the caller leaves
// the run for a later claim rather than guessing.
func (d *drive) verify() (lost runtime.Reason, transient bool) {
	handles := []struct {
		h runtime.ResourceHandle
		r runtime.Reason
	}{{d.workspaceHandle(), runtime.ReasonWorkspaceLost}}
	for _, s := range d.rec.Plan.Services {
		handles = append(handles, struct {
			h runtime.ResourceHandle
			r runtime.Reason
		}{d.serviceHandle(s), runtime.ReasonServiceLost})
	}
	for _, it := range handles {
		obs, err := d.e.cfg.Resources.Observe(d.ctx, it.h)
		if err != nil {
			return runtime.ReasonNone, true
		}
		o, err := d.acceptResource(obs, it.h)
		if err != nil {
			return it.r, false
		}
		switch o.State {
		case runtime.ResourceReady, runtime.ResourceStarting:
		case runtime.ResourceUnknown:
			return runtime.ReasonNone, true
		default:
			return it.r, false
		}
	}
	return runtime.ReasonNone, false
}
