package runner

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// maybeLaunched reports whether a launch command may have left a workload
// behind: any that was not affirmatively rejected before the backend, and one
// that was rejected because the workload was never found after an uncertain
// launch, which a late create could still produce.
func maybeLaunched(c runtime.Command) bool {
	return c.State != runtime.CommandRejected || c.Reason == runtime.ReasonLaunchUncertain
}

// cleanup stops and removes everything the settled run owns. Execution outcome
// and cleanup are separate: the outcome was recorded once when execution ended
// and nothing here changes it. The run becomes terminal only when every owned
// workload and resource is confirmed removed, or when the bounded attempts are
// exhausted with what remains recorded by identity.
func (d *drive) cleanup() (flow, error) {
	switch d.rec.Run.Cleanup.State {
	case runtime.CleanupPending:
		return d.cleanupAttempt()
	case runtime.CleanupExhausted:
		return d.lateCleanup()
	}
	return d.terminal()
}

func (d *drive) terminal() (flow, error) {
	d.report = Report{Status: DriveTerminal}
	return flowStop, nil
}

// ownedWorkloads lists, by the binding each was launched with, every workload the
// run may have created: one per launch attempt that may have reached a backend,
// plus any retained by an earlier cleanup attempt.
func (d *drive) ownedWorkloads() []runtime.RuntimeBinding {
	var out []runtime.RuntimeBinding
	add := func(b runtime.RuntimeBinding) {
		if !slices.ContainsFunc(out, func(x runtime.RuntimeBinding) bool { return x.WorkloadID == b.WorkloadID }) {
			out = append(out, b)
		}
	}
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandLaunch && maybeLaunched(c) {
			add(c.Binding)
		}
	}
	for _, b := range d.rec.Run.Cleanup.RetainedWorkloads {
		add(b)
	}
	return out
}

// ownedResources lists the run-scoped resources: those the record observed and
// those an ensure command may have created without an observation. Services of
// project or tenant lifetime outlive the run and are not removed.
func (d *drive) ownedResources() []runtime.ResourceHandle {
	r := d.rec.Run
	var out []runtime.ResourceHandle
	add := func(h runtime.ResourceHandle) {
		if h.Lifetime == runtime.LifetimePipeline && h.RunID == r.ID &&
			!slices.ContainsFunc(out, func(x runtime.ResourceHandle) bool { return sameResource(x, h) }) {
			out = append(out, h)
		}
	}
	for _, o := range d.rec.Resources {
		add(o.Handle)
	}
	ensureMayExist := func(h runtime.ResourceHandle) bool {
		for _, c := range d.rec.Commands {
			if c.Kind == runtime.CommandEnsureResource && c.Target == h.Identity && (c.State != runtime.CommandRejected || !provenNotDelivered(c)) {
				return true
			}
		}
		return false
	}
	if h := d.workspaceHandle(); ensureMayExist(h) {
		add(h)
	}
	for _, s := range d.rec.Plan.Services {
		if h := d.serviceHandle(s); ensureMayExist(h) {
			add(h)
		}
	}
	return out
}

// cleanWorkload stops the workload if it may still run and removes it, each with
// intent first and an uncertain outcome reconciled by observation. It reports
// whether removal is confirmed.
func (d *drive) cleanWorkload(b runtime.RuntimeBinding) (bool, error) {
	if d.removed(b.StepKey, b.Attempt) {
		return true, nil
	}
	if o, ok := d.workloadObservation(b.StepKey, b.Attempt); !ok || (o.Phase != runtime.PhaseExited && o.Phase != runtime.PhaseAbsent) {
		if _, _, err := d.workloadEffect(runtime.CommandStop, b); err != nil {
			return false, err
		}
	}
	_, ok, err := d.workloadEffect(runtime.CommandRemove, b)
	return ok, err
}

// cleanResource stops a service and removes the resource. Removal is confirmed
// only by ResourceRemoved; an unknown state never counts.
func (d *drive) cleanResource(h runtime.ResourceHandle) (bool, error) {
	if o, ok := d.resourceObservation(h); ok && o.State == runtime.ResourceRemoved {
		return true, nil
	}
	if h.Kind == runtime.ResourceService {
		if _, err := d.resourceEffect(runtime.CommandStopResource, h); err != nil {
			return false, err
		}
	}
	return d.resourceEffect(runtime.CommandRemoveResource, h)
}

// sweep cleans every given target and returns what could not be confirmed.
func (d *drive) sweep(workloads []runtime.RuntimeBinding, resources []runtime.ResourceHandle) (runtime.Remaining, error) {
	var rem runtime.Remaining
	for _, b := range workloads {
		ok, err := d.cleanWorkload(b)
		if err != nil {
			return rem, err
		}
		if !ok {
			rem.Workloads = append(rem.Workloads, b)
		}
	}
	// Services first, the workspace last: nothing that uses it is left running.
	ordered := slices.Clone(resources)
	slices.SortStableFunc(ordered, func(a, b runtime.ResourceHandle) int {
		if a.Kind == b.Kind {
			return 0
		}
		if a.Kind == runtime.ResourceService {
			return -1
		}
		return 1
	})
	for _, h := range ordered {
		ok, err := d.cleanResource(h)
		if err != nil {
			return rem, err
		}
		if !ok {
			rem.Resources = append(rem.Resources, h)
		}
	}
	return rem, nil
}

// cleanupAttempt makes one bounded attempt: it records either completed cleanup
// or the identities still retained and why. After the last permitted attempt the
// run is terminal with cleanup exhausted; before it, the engine backs off and
// tries again.
func (d *drive) cleanupAttempt() (flow, error) {
	// Revocation first: a run is terminal only once no settled step's secret
	// access remains. A release that fails leaves cleanup visibly pending.
	if err := d.revokeSettled(); err != nil {
		return d.stop(DriveCleaning)
	}
	rem, err := d.sweep(d.ownedWorkloads(), d.ownedResources())
	if err != nil {
		return flowPass, err
	}
	bt := d.batch()
	if rem.Empty() {
		if err := bt.run.CompleteCleanup(); err != nil {
			return flowPass, err
		}
		bt.runChanged(runtime.ReasonNone)
		if err := d.commit(bt); err != nil {
			return flowPass, err
		}
		return d.terminal()
	}
	if err := bt.run.RecordCleanupRemaining(rem, runtime.ReasonCleanupFailed); err != nil {
		return flowPass, err
	}
	if bt.run.Cleanup.State == runtime.CleanupExhausted {
		bt.runChanged(runtime.ReasonCleanupExhausted)
		if err := d.commit(bt); err != nil {
			return flowPass, err
		}
		return d.terminal()
	}
	bt.runChanged(runtime.ReasonCleanupFailed)
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	if err := d.backoff(d.e.cfg.CleanupBackoff * time.Duration(d.rec.Run.Cleanup.Attempts)); err != nil {
		return flowPass, err
	}
	return flowAgain, nil
}

// lateCleanup is a cleanup claim on a run whose attempts were exhausted: it
// retries only what is retained, by the identities recorded, and narrows or
// clears the record. It never reopens execution.
func (d *drive) lateCleanup() (flow, error) {
	if err := d.revokeSettled(); err != nil {
		return d.stop(DriveCleaning)
	}
	c := d.rec.Run.Cleanup
	rem, err := d.sweep(c.RetainedWorkloads, c.Retained)
	if err != nil {
		return flowPass, err
	}
	if len(rem.Workloads) == len(c.RetainedWorkloads) && len(rem.Resources) == len(c.Retained) {
		return d.terminal() // nothing more is confirmed; the record already says what remains
	}
	bt := d.batch()
	if err := bt.run.ReconcileExhausted(rem); err != nil {
		return flowPass, err
	}
	bt.runChanged(runtime.ReasonNone)
	if err := d.commit(bt); err != nil {
		return flowPass, err
	}
	return d.terminal()
}

// backoff renews the lease, waits and reloads, so a long retry interval neither
// loses the claim nor hides a takeover.
func (d *drive) backoff(wait time.Duration) error {
	if err := d.renew(); err != nil {
		return err
	}
	if err := d.e.cfg.Sleep(d.ctx, wait); err != nil {
		return err
	}
	return d.reload()
}

// ---------------------------------------------------------------------------
// Resource stop and removal
// ---------------------------------------------------------------------------

func (d *drive) resourceEffectID(kind runtime.CommandKind, h runtime.ResourceHandle) string {
	base := fmt.Sprintf("%s:%s:%s:%s", kind, d.rec.Run.ID, h.Kind, h.Name)
	id := base
	for n := 2; ; n++ {
		c, ok := d.command(id)
		if !ok || c.State != runtime.CommandRejected {
			return id
		}
		id = base + "." + strconv.Itoa(n)
	}
}

func resourceSatisfied(kind runtime.CommandKind, o runtime.ResourceObservation) bool {
	if kind == runtime.CommandRemoveResource {
		return o.State == runtime.ResourceRemoved
	}
	return o.State == runtime.ResourceStopped || o.State == runtime.ResourceRemoved
}

// resourceEffect stops or removes a resource under the current fence with the
// intent committed first. Like a workload effect, an intent or uncertain command
// is observation-only: the resource is observed by its identity and the command
// confirmed when the observation shows it took hold; nothing is issued again on
// a failed, inconclusive or unchanged observation. Only a rejected command (the
// effect provably never reached the backend) is issued again, as a new command.
// It reports whether the effect is confirmed.
func (d *drive) resourceEffect(kind runtime.CommandKind, orig runtime.ResourceHandle) (bool, error) {
	h := orig
	h.Fence = d.claim.Fence
	id := d.resourceEffectID(kind, h)
	cmd, exists := d.command(id)
	if exists && cmd.State == runtime.CommandConfirmed {
		return true, nil
	}
	if exists {
		obs, err := d.e.cfg.Resources.Observe(d.ctx, h)
		if err != nil {
			return false, nil
		}
		if o, aerr := d.acceptResource(obs, h); aerr == nil && resourceSatisfied(kind, o) {
			return true, d.confirmResource(cmd, o)
		}
		return false, nil
	}
	nc, err := runtime.NewCommand(id, h.Scope, d.rec.Run.ID, "", kind, h.Identity, "", d.claim.Fence, d.now())
	if err != nil {
		return false, err
	}
	bt := d.batch()
	bt.commands = []runtime.Command{nc}
	if err := d.commit(bt); err != nil {
		return false, err
	}
	cmd = nc

	var obs runtime.ResourceObservation
	if kind == runtime.CommandRemoveResource {
		obs, err = d.e.cfg.Resources.Remove(d.ctx, h)
	} else {
		obs, err = d.e.cfg.Resources.Stop(d.ctx, h)
	}
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
		if resourceSatisfied(kind, obs) {
			return true, d.confirmResource(cmd, obs)
		}
		return false, d.markResourceUncertain(cmd, &obs)
	case runtime.CommandRejected:
		c := cmd
		if err := c.Settle(runtime.CommandRejected, runtime.ReasonCleanupFailed); err != nil {
			return false, err
		}
		bt := d.batch()
		bt.commands = []runtime.Command{c}
		return false, d.commit(bt)
	}
	if err := d.markResourceUncertain(cmd, nil); err != nil {
		return false, err
	}
	cmd, _ = d.command(id)
	if obs2, oerr := d.e.cfg.Resources.Observe(d.ctx, h); oerr == nil {
		if o, aerr := d.acceptResource(obs2, h); aerr == nil && resourceSatisfied(kind, o) {
			return true, d.confirmResource(cmd, o)
		}
	}
	return false, nil
}

func (d *drive) confirmResource(cmd runtime.Command, obs runtime.ResourceObservation) error {
	c := cmd
	if c.State != runtime.CommandConfirmed {
		if err := c.Settle(runtime.CommandConfirmed, runtime.ReasonNone); err != nil {
			return err
		}
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	bt.resources = []runtime.ResourceObservation{obs}
	if obs.Handle.Kind == runtime.ResourceService {
		bt.events = append(bt.events, pendingEvent{kind: runtime.EventServiceStateChanged, suffix: obs.Handle.Name + "." + string(obs.State), reason: obs.Reason})
	}
	return d.commit(bt)
}

func (d *drive) markResourceUncertain(cmd runtime.Command, obs *runtime.ResourceObservation) error {
	bt := d.batch()
	if cmd.State == runtime.CommandIntent {
		c := cmd
		if err := c.Settle(runtime.CommandUncertain, runtime.ReasonStopUncertain); err != nil {
			return err
		}
		bt.commands = []runtime.Command{c}
	}
	if obs != nil {
		bt.resources = []runtime.ResourceObservation{*obs}
	}
	if len(bt.commands) == 0 && len(bt.resources) == 0 {
		return nil
	}
	return d.commit(bt)
}
