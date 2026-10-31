package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ---------------------------------------------------------------------------
// Stable identities
// ---------------------------------------------------------------------------

// hashID derives a bounded, stable backend identity from length-prefixed parts,
// so a restarted engine, or another one, finds the same workload or resource.
func hashID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p + ";"))
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:24]
}

func workloadID(scope domain.Scope, run domain.RunID, step domain.StepID, attempt int) string {
	return hashID("wl-", string(scope.TenantID), string(scope.ProjectID), string(run), string(step), strconv.Itoa(attempt))
}

// workloadDigest is the immutable specification identity of a workload: its
// image, commands, resolved environment, declared secret references, timeout,
// required capabilities and the workspace it runs in.
func workloadDigest(spec runtime.WorkloadSpec, workspace string) (string, error) {
	raw, err := json.Marshal(struct {
		Spec      runtime.WorkloadSpec
		Workspace string
	}{spec, workspace})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (d *drive) binding(step domain.StepID, attempt int, digest string) runtime.RuntimeBinding {
	r := d.rec.Run
	return runtime.RuntimeBinding{Scope: r.Scope, RunID: r.ID, StepKey: step, Attempt: attempt,
		WorkloadID: workloadID(r.Scope, r.ID, step, attempt), SpecDigest: digest, Backend: d.e.cfg.Backend, Fence: d.claim.Fence}
}

// resourceHandle derives the handle of a workspace or service. A run-scoped
// resource belongs to the run's project, a project service to the project and a
// tenant service to the tenant alone, as runtime.OwnerFor requires.
func (d *drive) resourceHandle(kind runtime.ResourceKind, name string, life runtime.Lifetime) runtime.ResourceHandle {
	r := d.rec.Run
	h := runtime.ResourceHandle{Scope: r.Scope, Owner: runtime.OwnerFor(life, r.Scope), Lifetime: life, Kind: kind, Name: name, Fence: d.claim.Fence}
	t, p := string(r.Scope.TenantID), string(r.Scope.ProjectID)
	switch life {
	case runtime.LifetimePipeline:
		h.RunID = r.ID
		h.Identity = hashID("res-", t, p, string(r.ID), string(kind), name)
	case runtime.LifetimeProject:
		h.Identity = hashID("res-", t, p, string(kind), name)
	default:
		h.Identity = hashID("res-", t, string(kind), name)
	}
	return h
}

func (d *drive) workspaceHandle() runtime.ResourceHandle {
	return d.resourceHandle(runtime.ResourceWorkspace, "workspace", runtime.LifetimePipeline)
}

func (d *drive) serviceHandle(s runtime.ServiceSpec) runtime.ResourceHandle {
	return d.resourceHandle(runtime.ResourceService, s.Name, s.Lifetime)
}

// sameResource reports whether a retained observation is the resource a handle names.
func sameResource(a, b runtime.ResourceHandle) bool {
	a.Fence, b.Fence = 0, 0
	return a == b
}

// resourceObservation returns the latest retained observation of a resource.
func (d *drive) resourceObservation(h runtime.ResourceHandle) (runtime.ResourceObservation, bool) {
	for _, o := range d.rec.Resources {
		if sameResource(o.Handle, h) {
			return o, true
		}
	}
	return runtime.ResourceObservation{}, false
}

// ---------------------------------------------------------------------------
// Workload observations
// ---------------------------------------------------------------------------

var errForeignObservation = errors.New("runner: the backend reported another workload")

// accept checks that a backend's observation is of the workload b names and
// returns it bound to b, so the retained observation keeps the engine's identity.
func accept(obs runtime.WorkloadObservation, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if err := obs.Validate(); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	o := obs.Binding
	if o.Scope != b.Scope || o.RunID != b.RunID || o.StepKey != b.StepKey || o.Attempt != b.Attempt ||
		o.WorkloadID != b.WorkloadID || o.Backend != b.Backend {
		return runtime.WorkloadObservation{}, errForeignObservation
	}
	if o.SpecDigest != b.SpecDigest {
		return runtime.WorkloadObservation{}, fmt.Errorf("%w: workload %s", runtime.ErrSpecMismatch, b.WorkloadID)
	}
	obs.Binding = b
	return obs, nil
}

// launchCommand returns the launch command of a step attempt.
func (d *drive) launchCommand(step domain.StepID, attempt int) (runtime.Command, bool) {
	for _, c := range d.rec.Commands {
		if c.Kind == runtime.CommandLaunch && c.StepKey == step && c.Binding.Attempt == attempt {
			return c, true
		}
	}
	return runtime.Command{}, false
}

// workloadObservation returns the retained observation of a step attempt.
func (d *drive) workloadObservation(step domain.StepID, attempt int) (runtime.WorkloadObservation, bool) {
	for _, o := range d.rec.Workloads {
		if o.Binding.StepKey == step && o.Binding.Attempt == attempt {
			return o, true
		}
	}
	return runtime.WorkloadObservation{}, false
}

// rebind returns the binding under the current fence, the only fence under
// which a backend accepts a stop or removal after a takeover.
func (d *drive) rebind(b runtime.RuntimeBinding) runtime.RuntimeBinding {
	b.Fence = d.claim.Fence
	return b
}

// effectID returns the identity of the next command of kind for a workload: the
// first one that is not a rejected (never reached) command, so a rejected effect
// may be issued again as a new command while an uncertain one is reconciled.
func (d *drive) effectID(kind runtime.CommandKind, b runtime.RuntimeBinding) string {
	base := fmt.Sprintf("%s:%s:%s:%d", kind, b.RunID, b.StepKey, b.Attempt)
	id := base
	for n := 2; ; n++ {
		c, ok := d.command(id)
		if !ok || c.State != runtime.CommandRejected {
			return id
		}
		id = base + "." + strconv.Itoa(n)
	}
}

// satisfied reports whether an observation shows the effect of kind took hold.
func satisfied(kind runtime.CommandKind, o runtime.WorkloadObservation) bool {
	if kind == runtime.CommandRemove {
		return o.Phase == runtime.PhaseAbsent
	}
	return o.Phase == runtime.PhaseExited || o.Phase == runtime.PhaseAbsent
}

// workloadEffect stops or removes the workload orig names, under the current
// fence and with the intent committed first. An intent or uncertain command left
// by an earlier claim is reconciled before anything is issued: the workload is
// observed by its stable identity, and the effect is issued only when the
// observation shows it has not taken hold. The effect is idempotent on the
// workload identity. It returns the last observation and whether the effect is
// confirmed.
func (d *drive) workloadEffect(kind runtime.CommandKind, orig runtime.RuntimeBinding) (runtime.WorkloadObservation, bool, error) {
	b := d.rebind(orig)
	id := d.effectID(kind, b)
	cmd, exists := d.command(id)
	if exists && cmd.State == runtime.CommandConfirmed {
		o, _ := d.workloadObservation(b.StepKey, b.Attempt)
		return o, true, nil
	}
	if !exists {
		nc, err := runtime.NewWorkloadCommand(id, b, kind, d.now())
		if err != nil {
			return runtime.WorkloadObservation{}, false, err
		}
		bt := d.batch()
		bt.commands = []runtime.Command{nc}
		if err := d.commit(bt); err != nil {
			return runtime.WorkloadObservation{}, false, err
		}
		cmd = nc
	} else if obs, err := d.e.cfg.Runtime.Observe(d.ctx, b); err == nil {
		if o, aerr := accept(obs, b); aerr == nil && satisfied(kind, o) {
			return o, true, d.confirmWorkload(cmd, o)
		}
	}

	var obs runtime.WorkloadObservation
	var err error
	if kind == runtime.CommandRemove {
		obs, err = d.e.cfg.Runtime.Remove(d.ctx, b)
	} else {
		obs, err = d.e.cfg.Runtime.Stop(d.ctx, b)
	}
	state := runtime.ClassifyEffect(err)
	if state == runtime.CommandConfirmed {
		o, aerr := accept(obs, b)
		if aerr != nil {
			state = runtime.CommandUncertain
		} else {
			obs = o
		}
	}
	switch state {
	case runtime.CommandConfirmed:
		if satisfied(kind, obs) {
			return obs, true, d.confirmWorkload(cmd, obs)
		}
		// The backend acknowledged the call but the workload is still there.
		return obs, false, d.markUncertain(cmd, runtime.ReasonStopUncertain, &obs)
	case runtime.CommandRejected:
		bt := d.batch()
		c := cmd
		if err := c.Settle(runtime.CommandRejected, runtime.ReasonCleanupFailed); err != nil {
			return obs, false, err
		}
		bt.commands = []runtime.Command{c}
		return obs, false, d.commit(bt)
	}
	// Uncertain: record it, then observe the workload to find out.
	if err := d.markUncertain(cmd, runtime.ReasonStopUncertain, nil); err != nil {
		return obs, false, err
	}
	cmd, _ = d.command(id)
	if o2, oerr := d.e.cfg.Runtime.Observe(d.ctx, b); oerr == nil {
		if o, aerr := accept(o2, b); aerr == nil && satisfied(kind, o) {
			return o, true, d.confirmWorkload(cmd, o)
		}
	}
	return runtime.WorkloadObservation{}, false, nil
}

// confirmWorkload settles a stop or removal as confirmed and retains what was
// observed, with the result a stop saw.
func (d *drive) confirmWorkload(cmd runtime.Command, obs runtime.WorkloadObservation) error {
	c := cmd
	if c.State != runtime.CommandConfirmed {
		if err := c.Settle(runtime.CommandConfirmed, runtime.ReasonNone); err != nil {
			return err
		}
	}
	if obs.Phase == runtime.PhaseExited && c.Result == (runtime.Result{}) {
		if err := c.RecordResult(obs.Result); err != nil {
			return err
		}
	}
	bt := d.batch()
	bt.commands = []runtime.Command{c}
	bt.workloads = []runtime.WorkloadObservation{obs}
	return d.commit(bt)
}

// markUncertain records an effect whose outcome is unknown. A command that is
// already uncertain stays so.
func (d *drive) markUncertain(cmd runtime.Command, reason runtime.Reason, obs *runtime.WorkloadObservation) error {
	bt := d.batch()
	if cmd.State == runtime.CommandIntent {
		c := cmd
		if err := c.Settle(runtime.CommandUncertain, reason); err != nil {
			return err
		}
		bt.commands = []runtime.Command{c}
	}
	if obs != nil {
		bt.workloads = []runtime.WorkloadObservation{*obs}
	}
	if len(bt.commands) == 0 && len(bt.workloads) == 0 {
		return nil
	}
	return d.commit(bt)
}
