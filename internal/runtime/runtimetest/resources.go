package runtimetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// ResourcesFixture binds one backend's state to the run-resource contract. A
// factory returns a fresh, isolated state for every subtest. Everything
// backend-specific is a hook; nothing here assumes Podman, processes or
// Kubernetes, so the Kubernetes adapter reuses the suite with its own hooks.
// A hook may be nil only for a capability the backend does not advertise: a
// backend that declares run_services supplies Service, workspace_preparation
// Preparation, workspace_fence HoldWorkspace and run_workspace the Foreign,
// ForeignIntact, Exists and Fault hooks, so no advertised capability escapes its
// checks (the contract fails, it does not skip).
type ResourcesFixture struct {
	// Open returns an adapter over the fixture's backend state. Every call
	// returns a FRESH adapter instance over the SAME state: a second call is a
	// controller restart. Required.
	Open func() execution.RunResources
	// Want, when set, is the exact identity and capability set the backend must
	// declare; leaving it zero checks only that the declaration is stable.
	Want execution.ResourceCapabilities
	// Service returns the body of a service whose workload really runs on this
	// backend and becomes ready (the suite sets its Name and Host). nil skips the
	// service subtests.
	Service func(name string) execution.ServiceSpec
	// Preparation returns a preparation body that really runs on this backend
	// against the run's workspace and completes. nil skips its subtest.
	Preparation func() execution.PreparationSpec
	// Foreign plants an object the adapter does not own at the handle's name
	// (a same-name object without the adapter's ownership). ForeignIntact then
	// reports that the planted object was left untouched. nil skips the
	// foreign-object subtests.
	Foreign       func(h execution.ResourceHandle)
	ForeignIntact func(h execution.ResourceHandle) bool
	// Exists reports whether the backend holds any object, the adapter's or
	// not, for the handle. It proves Observe never creates. nil skips that
	// check.
	Exists func(h execution.ResourceHandle) bool
	// Fault makes the backend undeterminable (an outage) until restore is
	// called. nil skips the unknown-never-absent subtests.
	Fault func() (restore func())
	// HoldWorkspace makes a process or mount of the backend use the workspace
	// volume until release is called. nil skips the release-fence subtest.
	HoldWorkspace func(h execution.ResourceHandle) (release func())
	// Timeout bounds each wait for readiness (default 60s).
	Timeout time.Duration
}

// RunResourcesContract runs the behavior every execution.RunResources promises
// (docs/contracts/run-resources.md, the companion port): idempotent ensure and
// adoption, scope/fingerprint/UID binding, collision refusal, fencing after
// stop, honest absent/unknown, bounded logs, stop and removal evidence and
// adoption by a restarted adapter. Capability-gated subtests run only for what
// the backend declares; a backend that declares a member is held to it.
func RunResourcesContract(t *testing.T, factory func(t *testing.T) ResourcesFixture) {
	run := func(name string, body func(c *rrCase)) {
		t.Run(name, func(t *testing.T) {
			fx := factory(t)
			if fx.Open == nil {
				t.Fatal("the fixture has no Open")
			}
			if fx.Timeout == 0 {
				fx.Timeout = 60 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*fx.Timeout)
			defer cancel()
			c := &rrCase{t: t, ctx: ctx, fx: fx, res: fx.Open(), run: newRunID(t)}
			c.caps = c.res.Capabilities(ctx)
			// A backend is held to every capability it advertises: the hooks that
			// exercise one are not optional for it.
			if missing := missingHooks(fx, c.caps.Set); len(missing) > 0 {
				t.Fatalf("the fixture advertises capabilities it does not let the contract check; missing hooks: %s", strings.Join(missing, ", "))
			}
			t.Cleanup(func() { c.cleanup() })
			body(c)
		})
	}
	needs := func(c *rrCase, caps ...execution.ResourceCapability) {
		for _, m := range caps {
			if !c.caps.Set.Has(m) {
				c.t.Skipf("the backend does not declare %s", execution.Set(m))
			}
		}
	}

	run("capabilities are a stable identity", func(c *rrCase) {
		if c.caps.Backend == "" {
			c.t.Fatal("a backend declares no identity")
		}
		for range 2 {
			if again := c.fx.Open().Capabilities(c.ctx); again != c.caps {
				c.t.Fatalf("the declaration changed between instances: %+v, then %+v", c.caps, again)
			}
		}
		if c.fx.Want != (execution.ResourceCapabilities{}) && c.caps != c.fx.Want {
			c.t.Fatalf("declared %s %s, want %s %s", c.caps.Backend, c.caps.Set, c.fx.Want.Backend, c.fx.Want.Set)
		}
	})

	for _, kind := range []execution.ResourceKind{execution.KindWorkspace, execution.KindNetwork} {
		run(string(kind)+" is created then adopted idempotently", func(c *rrCase) {
			needs(c, execution.CapRunWorkspace)
			spec := c.spec(kind)
			first := c.ensure(spec)
			if again := c.ensure(spec); again.Handle != first.Handle {
				c.t.Fatalf("a repeated ensure did not adopt: %+v, then %+v", first.Handle, again.Handle)
			}
			if restarted, err := c.fx.Open().Ensure(c.ctx, spec); err != nil || restarted.Handle != first.Handle {
				c.t.Fatalf("a restarted adapter did not adopt: %+v %v", restarted.Handle, err)
			}
			c.concurrentEnsure(spec, first.Handle.UID)
			obs := c.awaitPhase(first.Handle, execution.PhaseReady)
			c.checkObservation(spec, obs)
			if kind == execution.KindWorkspace && c.caps.Set.Has(execution.CapWorkspaceFence) && !obs.Released {
				c.t.Fatalf("an unused workspace is not released: %+v", obs)
			}
		})
	}

	run("competing initial creates converge on one instance", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		specs := []execution.ResourceSpec{c.spec(execution.KindNetwork), c.spec(execution.KindWorkspace)}
		for _, spec := range specs {
			h := c.competingEnsure(spec)
			if again := c.ensure(spec); again.Handle != h {
				c.t.Fatalf("%s: a later ensure holds another instance: %+v, %+v", spec.Key, again.Handle, h)
			}
			c.awaitPhase(h, execution.PhaseReady)
		}
		if c.caps.Set.Has(execution.CapRunServices) && c.fx.Service != nil {
			spec := c.service("db")
			h := c.competingEnsure(spec)
			if obs := c.awaitPhase(h, execution.PhaseReady); obs.Handle != h {
				c.t.Fatalf("service: %+v", obs)
			}
			// One workload runs: stopping the one instance stops everything.
			c.stop(h)
		}
	})

	run("a different spec or a foreign object at the name is a collision", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		spec := c.spec(execution.KindWorkspace)
		h := c.ensure(spec).Handle
		other := spec
		other.Workspace = &execution.WorkspaceSpec{SizeMiB: spec.Workspace.SizeMiB + 1, Initial: spec.Workspace.Initial}
		if _, err := c.res.Ensure(c.ctx, other); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("another fingerprint at the name: %v", err)
		}
		other = spec
		other.Policy.ID = "run-other"
		if _, err := c.res.Ensure(c.ctx, other); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("another policy pin at the name: %v", err)
		}
		// A handle bound to another instance is never adopted, stopped or removed.
		wrong := h
		wrong.UID = "99999999-9999-4999-8999-999999999999"
		if _, err := c.res.Observe(c.ctx, wrong); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("observe another instance: %v", err)
		}
		if _, err := c.res.Stop(c.ctx, wrong); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("stop another instance: %v", err)
		}
		if _, err := c.res.Remove(c.ctx, wrong); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("remove another instance: %v", err)
		}
		if obs := c.awaitPhase(h, execution.PhaseReady); obs.Handle != h {
			c.t.Fatalf("the resource was disturbed: %+v", obs)
		}
		if again := c.ensure(spec); again.Handle != h {
			c.t.Fatalf("the original is no longer adopted: %+v", again.Handle)
		}
	})

	run("a foreign object is never adopted, stopped or removed", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		spec := c.spec(execution.KindWorkspace)
		h := spec.Handle("")
		c.fx.Foreign(h)
		if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("ensure over a foreign object: %v", err)
		}
		bound := h
		bound.UID = "88888888-8888-4888-8888-888888888888"
		if _, err := c.res.Stop(c.ctx, bound); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("a bound handle stopped a foreign object: %v", err)
		}
		if _, err := c.res.Remove(c.ctx, bound); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("a bound handle removed a foreign object: %v", err)
		}
		// Nothing of ours exists there: an unbound stop and remove succeed
		// without touching what is not ours.
		if ev, err := c.res.Stop(c.ctx, h); err != nil || !ev.Stopped {
			c.t.Fatalf("unbound stop: %+v %v", ev, err)
		}
		if ev, err := c.res.Remove(c.ctx, h); err != nil || !ev.Removed {
			c.t.Fatalf("unbound remove: %+v %v", ev, err)
		}
		if !c.fx.ForeignIntact(h) {
			c.t.Fatal("the foreign object was touched")
		}
	})

	run("a stopped or removed resource is fenced", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		for _, kind := range []execution.ResourceKind{execution.KindWorkspace, execution.KindNetwork} {
			spec := c.spec(kind)
			h := c.ensure(spec).Handle
			if _, err := c.res.Remove(c.ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
				c.t.Fatalf("%s: remove before a verified stop: %v", kind, err)
			}
			if obs := c.observe(h); obs.Phase == execution.PhaseAbsent {
				c.t.Fatalf("%s: a refused remove deleted the resource", kind)
			}
			ev := c.stop(h)
			if kind == execution.KindWorkspace && c.caps.Set.Has(execution.CapWorkspaceFence) && !ev.Released {
				c.t.Fatalf("%s: an unused workspace was not released: %+v", kind, ev)
			}
			if again := c.stop(h); again != ev {
				c.t.Fatalf("%s: a repeated stop changed the evidence: %+v, then %+v", kind, ev, again)
			}
			if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				c.t.Fatalf("%s: ensure after stop: %v", kind, err)
			}
			if _, err := c.fx.Open().Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				c.t.Fatalf("%s: ensure after stop by a restarted adapter: %v", kind, err)
			}
			if obs := c.observe(h); obs.Phase != execution.PhaseStopped {
				c.t.Fatalf("%s: observe after stop: %+v", kind, obs)
			}
			for range 2 {
				if rm, err := c.res.Remove(c.ctx, h); err != nil || !rm.Removed {
					c.t.Fatalf("%s: remove: %+v %v", kind, rm, err)
				}
			}
			if obs := c.observe(h); obs.Phase != execution.PhaseAbsent {
				c.t.Fatalf("%s: observe after removal: %+v", kind, obs)
			}
			if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				c.t.Fatalf("%s: ensure after removal: %v", kind, err)
			}
			if ev := c.stop(h); !ev.Stopped {
				c.t.Fatalf("%s: stop after removal: %+v", kind, ev)
			}
		}
	})

	run("a stop before the create fences it", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		spec := c.spec(execution.KindWorkspace)
		ev, err := c.res.Stop(c.ctx, spec.Handle(""))
		if err != nil || !ev.Stopped {
			c.t.Fatalf("stop of a resource that never existed: %+v %v", ev, err)
		}
		if ev.Launch == execution.LaunchStarted {
			c.t.Fatalf("a resource that never existed reported a launch: %+v", ev)
		}
		if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("a create after a confirmed stop: %v", err)
		}
		if rm, err := c.res.Remove(c.ctx, spec.Handle("")); err != nil || !rm.Removed {
			c.t.Fatalf("remove: %+v %v", rm, err)
		}
	})

	run("observe never creates and reports absent only when verified", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		for _, kind := range []execution.ResourceKind{execution.KindWorkspace, execution.KindNetwork} {
			spec := c.spec(kind)
			obs, err := c.res.Observe(c.ctx, spec.Handle(""))
			if err != nil || obs.Phase != execution.PhaseAbsent {
				c.t.Fatalf("%s: observe of a never-created resource: %+v %v", kind, obs, err)
			}
			if c.fx.Exists != nil && c.fx.Exists(spec.Handle("")) {
				c.t.Fatalf("%s: observe created the resource", kind)
			}
		}
		spec := c.spec(execution.KindWorkspace)
		h := c.ensure(spec).Handle
		restore := c.fx.Fault()
		defer func() {
			if restore != nil {
				restore()
			}
		}()
		obs, err := c.fx.Open().Observe(c.ctx, h)
		if errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("an outage is not a collision: %v", err)
		}
		if err == nil && obs.Phase != execution.PhaseUnknown {
			c.t.Fatalf("an undeterminable resource reported %q (never absent, never ready)", obs.Phase)
		}
		restore()
		restore = nil
		if obs := c.awaitPhase(h, execution.PhaseReady); obs.Handle != h {
			c.t.Fatalf("the resource did not survive the outage: %+v", obs)
		}
		// A stop that cannot be verified is not confirmed; the fence it recorded
		// stays, and the stop completes once the backend answers.
		restore = c.fx.Fault()
		if ev, err := c.fx.Open().Stop(c.ctx, h); err == nil && ev.Stopped && ev.Released {
			c.t.Fatalf("a stop was confirmed during an outage: %+v", ev)
		}
		restore()
		restore = nil
		if _, err := c.res.Ensure(c.ctx, spec); err != nil && !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("ensure after an interrupted stop: %v", err)
		}
		c.stop(h)
		restore = c.fx.Fault()
		if rm, err := c.fx.Open().Remove(c.ctx, h); err == nil && rm.Removed {
			c.t.Fatalf("a removal was confirmed during an outage: %+v", rm)
		}
		restore()
		restore = nil
		if rm, err := c.res.Remove(c.ctx, h); err != nil || !rm.Removed {
			c.t.Fatalf("remove after the outage: %+v %v", rm, err)
		}
	})

	run("logs are denied for workspace and network", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace)
		for _, kind := range []execution.ResourceKind{execution.KindWorkspace, execution.KindNetwork} {
			h := c.ensure(c.spec(kind)).Handle
			if _, err := c.res.Logs(c.ctx, h, execution.LogBound{MaxBytes: 1024}); !errors.Is(err, execution.ErrDenied) {
				c.t.Fatalf("%s logs: %v", kind, err)
			}
		}
	})

	run("the workspace is released only when nothing uses it", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace, execution.CapWorkspaceFence)
		c.ensure(c.spec(execution.KindNetwork)) // the holder's attempt joins the run network
		spec := c.spec(execution.KindWorkspace)
		h := c.ensure(spec).Handle
		release := c.fx.HoldWorkspace(h)
		released := false
		defer func() {
			if !released {
				release()
			}
		}()
		if obs := c.observe(h); obs.Released {
			c.t.Fatalf("a held workspace was reported released: %+v", obs)
		}
		if ev, err := c.res.Stop(c.ctx, h); err != nil || ev.Released {
			c.t.Fatalf("a held workspace's stop reported it released: %+v %v", ev, err)
		}
		// The stop fenced the name even though the volume is still held.
		if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("ensure of a stopping workspace: %v", err)
		}
		if rm, err := c.res.Remove(c.ctx, h); err == nil && rm.Removed {
			c.t.Fatalf("a held workspace was removed: %+v", rm)
		}
		release()
		released = true
		ev := c.stop(h)
		if !ev.Released {
			c.t.Fatalf("the workspace was not released after the holder ended: %+v", ev)
		}
		if rm, err := c.res.Remove(c.ctx, h); err != nil || !rm.Removed {
			c.t.Fatalf("remove: %+v %v", rm, err)
		}
	})

	run("a service becomes ready, is stopped, fenced and removed", func(c *rrCase) {
		needs(c, execution.CapRunServices)
		c.ensure(c.spec(execution.KindNetwork)) // execution provisions the run network first
		spec := c.service("db")
		first := c.ensure(spec)
		if again := c.ensure(spec); again.Handle != first.Handle {
			c.t.Fatalf("a repeated ensure did not adopt: %+v, then %+v", first.Handle, again.Handle)
		}
		c.concurrentEnsure(spec, first.Handle.UID)
		obs := c.awaitPhase(first.Handle, execution.PhaseReady)
		c.checkObservation(spec, obs)
		if obs.ReadyAt == nil || obs.ProbeKind != spec.Service.Readiness.Probe {
			c.t.Fatalf("a ready service carries no probe evidence: %+v", obs)
		}
		if logs, err := c.res.Logs(c.ctx, first.Handle, execution.LogBound{MaxBytes: 16}); err != nil || len(logs.Content) > 16 {
			c.t.Fatalf("logs beyond their bound: %d bytes, %v", len(logs.Content), err)
		}
		if _, err := c.res.Remove(c.ctx, first.Handle); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("remove of a running service: %v", err)
		}
		ev := c.stop(first.Handle)
		if ev.Launch != execution.LaunchStarted {
			c.t.Fatalf("a service that ran reported %q", ev.Launch)
		}
		if again := c.stop(first.Handle); again != ev {
			c.t.Fatalf("a repeated stop changed the evidence: %+v, then %+v", ev, again)
		}
		if obs := c.observe(first.Handle); obs.Phase != execution.PhaseStopped {
			c.t.Fatalf("observe after stop: %+v", obs)
		}
		if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("a stopped service was started again: %v", err)
		}
		for range 2 {
			if rm, err := c.res.Remove(c.ctx, first.Handle); err != nil || !rm.Removed {
				c.t.Fatalf("remove: %+v %v", rm, err)
			}
		}
		if obs := c.observe(first.Handle); obs.Phase != execution.PhaseAbsent {
			c.t.Fatalf("observe after removal: %+v", obs)
		}
	})

	run("a stop before a service starts means it never starts", func(c *rrCase) {
		needs(c, execution.CapRunServices)
		spec := c.service("never")
		ev, err := c.res.Stop(c.ctx, spec.Handle(""))
		if err != nil || !ev.Stopped || ev.Launch == execution.LaunchStarted {
			c.t.Fatalf("stop: %+v %v", ev, err)
		}
		if _, err := c.res.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("a service stopped first was started: %v", err)
		}
	})

	run("a service from another run or spec is a collision", func(c *rrCase) {
		needs(c, execution.CapRunServices)
		c.ensure(c.spec(execution.KindNetwork))
		spec := c.service("db")
		h := c.ensure(spec).Handle
		other := spec
		svc := *spec.Service
		svc.Env = map[string]string{"OTHER": "1"}
		other.Service = &svc
		if _, err := c.res.Ensure(c.ctx, other); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("another service spec at the name: %v", err)
		}
		wrong := h
		wrong.UID = "77777777-7777-4777-8777-777777777777"
		if _, err := c.res.Stop(c.ctx, wrong); !errors.Is(err, execution.ErrResourceCollision) {
			c.t.Fatalf("stop of another instance: %v", err)
		}
		if obs := c.awaitPhase(h, execution.PhaseReady); obs.Handle != h {
			c.t.Fatalf("the service was disturbed: %+v", obs)
		}
	})

	run("a preparation completes against the workspace and is stopped", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace, execution.CapWorkspacePreparation)
		c.ensure(c.spec(execution.KindNetwork))
		ws := c.ensure(c.spec(execution.KindWorkspace)).Handle
		prep := c.spec(execution.KindPreparation)
		first := c.ensure(prep)
		if again := c.ensure(prep); again.Handle != first.Handle {
			c.t.Fatalf("a repeated ensure created a second preparation: %+v, then %+v", first.Handle, again.Handle)
		}
		obs := c.awaitPhase(first.Handle, execution.PhaseReady)
		c.checkObservation(prep, obs)
		if _, err := c.res.Logs(c.ctx, first.Handle, execution.LogBound{MaxBytes: 4096}); err != nil {
			c.t.Fatalf("preparation logs: %v", err)
		}
		if ev := c.stop(first.Handle); ev.Launch != execution.LaunchStarted {
			c.t.Fatalf("a preparation that ran reported %q", ev.Launch)
		}
		if _, err := c.res.Ensure(c.ctx, prep); !errors.Is(err, execution.ErrResourceFenced) {
			c.t.Fatalf("a stopped preparation was run again: %v", err)
		}
		if rm, err := c.res.Remove(c.ctx, first.Handle); err != nil || !rm.Removed {
			c.t.Fatalf("remove: %+v %v", rm, err)
		}
		if ev := c.stop(ws); !ev.Stopped {
			c.t.Fatalf("workspace stop: %+v", ev)
		}
	})

	run("a restarted adapter adopts every resource and fences the stopped ones", func(c *rrCase) {
		needs(c, execution.CapRunWorkspace, execution.CapResourceRecovery)
		specs := []execution.ResourceSpec{c.spec(execution.KindNetwork), c.spec(execution.KindWorkspace)}
		if c.caps.Set.Has(execution.CapRunServices) && c.fx.Service != nil {
			specs = append(specs, c.service("db"))
		}
		handles := make([]execution.ResourceHandle, len(specs))
		for i, spec := range specs {
			handles[i] = c.ensure(spec).Handle
			c.awaitPhase(handles[i], execution.PhaseReady)
		}
		restarted := c.fx.Open()
		for i, spec := range specs {
			obs, err := restarted.Ensure(c.ctx, spec)
			if err != nil || obs.Handle != handles[i] {
				c.t.Fatalf("%s: restart ensure: %+v %v", spec.Key, obs.Handle, err)
			}
			got, err := restarted.Observe(c.ctx, handles[i])
			if err != nil || got.Phase != execution.PhaseReady || got.Handle != handles[i] {
				c.t.Fatalf("%s: restart observe: %+v %v", spec.Key, got, err)
			}
		}
		// The restarted adapter stops what the first one created, in reverse order.
		for i := len(specs) - 1; i >= 0; i-- {
			ev, err := restarted.Stop(c.ctx, handles[i])
			if err != nil || !ev.Stopped {
				c.t.Fatalf("%s: stop by the restarted adapter: %+v %v", specs[i].Key, ev, err)
			}
		}
		third := c.fx.Open()
		for i, spec := range specs {
			if _, err := third.Ensure(c.ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				c.t.Fatalf("%s: ensure after a stop by a previous instance: %v", spec.Key, err)
			}
			if obs, err := third.Observe(c.ctx, handles[i]); err != nil || obs.Phase != execution.PhaseStopped {
				c.t.Fatalf("%s: observe by a third instance: %+v %v", spec.Key, obs, err)
			}
		}
		for i := len(specs) - 1; i >= 0; i-- {
			if rm, err := third.Remove(c.ctx, handles[i]); err != nil || !rm.Removed {
				c.t.Fatalf("%s: remove by a third instance: %+v %v", specs[i].Key, rm, err)
			}
		}
	})
}

// missingHooks lists the fixture hooks the contract needs to exercise the
// capabilities a backend declares: a backend advertising run_services supplies
// the Service hook, one advertising workspace_preparation the Preparation hook,
// workspace_fence the HoldWorkspace hook, and any backend with a run workspace the
// hooks that prove foreign objects are never touched, observation never creates
// and an outage is never read as absence.
func missingHooks(fx ResourcesFixture, set execution.ResourceCapabilitySet) []string {
	var missing []string
	need := func(present bool, capability execution.ResourceCapability, hook string) {
		if !present && set.Has(capability) {
			missing = append(missing, hook+" (for "+execution.Set(capability).String()+")")
		}
	}
	need(fx.Service != nil, execution.CapRunServices, "Service")
	need(fx.Preparation != nil, execution.CapWorkspacePreparation, "Preparation")
	need(fx.HoldWorkspace != nil, execution.CapWorkspaceFence, "HoldWorkspace")
	need(fx.Foreign != nil, execution.CapRunWorkspace, "Foreign")
	need(fx.ForeignIntact != nil, execution.CapRunWorkspace, "ForeignIntact")
	need(fx.Exists != nil, execution.CapRunWorkspace, "Exists")
	need(fx.Fault != nil, execution.CapRunWorkspace, "Fault")
	return missing
}

// rrCase is one subtest's backend, run and cleanup list.
type rrCase struct {
	t    *testing.T
	ctx  context.Context
	fx   ResourcesFixture
	res  execution.RunResources
	caps execution.ResourceCapabilities
	run  domain.RunID

	mu      sync.Mutex
	created []execution.ResourceHandle
}

func newRunID(t *testing.T) domain.RunID {
	t.Helper()
	return domain.RunID("run_" + string(NewAttemptID(t)))
}

// SharedSpec returns a planned attempt (a fresh attempt ID) of the run that owns
// workspace, whose workload runs script in image ("" selects HostImage) and
// which carries the SharedLaunch of a run_shared attempt mounting that
// workspace. Several calls give successive attempts of one run.
func SharedSpec(t testing.TB, workspace execution.ResourceHandle, image, script string) execution.AttemptSpec {
	t.Helper()
	if workspace.Kind != execution.KindWorkspace || workspace.Scope != Scope {
		t.Fatalf("%s is not a workspace of the contract scope", workspace.Name)
	}
	s := specOf(t, workspace.RunID, NewAttemptID(t), image, script)
	s.Shared = SharedLaunch(t, workspace)
	return s
}

// SharedLaunch is the launch of a run_shared attempt mounting workspace under
// the plain non-root step role and the tenant network model.
func SharedLaunch(t testing.TB, workspace execution.ResourceHandle) *execution.SharedLaunch {
	t.Helper()
	role := execution.RoleShape{User: "nonroot", UserNamespace: "pod", CapabilitiesAdd: []string{}, Seccomp: "RuntimeDefault", Devices: []string{}}
	digest, err := role.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return &execution.SharedLaunch{
		Policy:      execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		Role:        role,
		RoleDigest:  digest,
		PodSecurity: execution.PolicyPodSecurity{Level: "baseline", Version: "test"},
		Workspace:   workspace,
		Network:     execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true},
		Egress:      []string{}, EgressDigests: map[string]string{},
	}
}

// cleanup stops and removes whatever the case created, best effort, so a
// failing case leaves nothing in a shared backend. Everything is reversed:
// the last created is the first stopped.
func (c *rrCase) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), c.fx.Timeout)
	defer cancel()
	res := c.fx.Open()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.created) - 1; i >= 0; i-- {
		h := c.created[i]
		for range 3 {
			if ev, err := res.Stop(ctx, h); err == nil && ev.Stopped {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = res.Remove(ctx, h)
	}
}

func (c *rrCase) track(h execution.ResourceHandle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, h)
}

// spec builds the canonical spec of a workspace, network or preparation
// resource of the case's run.
func (c *rrCase) spec(kind execution.ResourceKind) execution.ResourceSpec {
	key := string(kind)
	s := c.base(kind, key)
	switch kind {
	case execution.KindWorkspace:
		s.Workspace = &execution.WorkspaceSpec{SizeMiB: 64, Initial: "empty"}
	case execution.KindNetwork:
		s.Network = &execution.NetworkSpec{
			Services: []string{}, Edges: []execution.EdgeSpec{}, Egress: map[string]string{}, Workloads: map[string][]string{},
			Isolation: execution.IsolationTenant, PublicEgress: true,
		}
	case execution.KindPreparation:
		p := c.fx.Preparation()
		s.Preparation = &p
	default:
		c.t.Fatalf("no generic spec for %s", kind)
	}
	if err := s.Validate(); err != nil {
		c.t.Fatalf("the contract built an invalid %s spec: %v", kind, err)
	}
	return s
}

// service builds the spec of a service from the fixture's body.
func (c *rrCase) service(name string) execution.ResourceSpec {
	body := c.fx.Service(name)
	body.Name = name
	body.Role = "general"
	body.Host = execution.EndpointHost(Scope, c.run, name)
	s := c.base(execution.KindService, execution.ServiceKey(name))
	s.Service = &body
	if err := s.Validate(); err != nil {
		c.t.Fatalf("the contract built an invalid service spec: %v", err)
	}
	return s
}

func (c *rrCase) base(kind execution.ResourceKind, key string) execution.ResourceSpec {
	sum := sha256.Sum256([]byte("contract-role-shape"))
	return execution.ResourceSpec{
		Scope: Scope, RunID: c.run, Backend: c.caps.Backend, Kind: kind, Key: key,
		Name:       execution.ResourceName(Scope, c.run, kind, key),
		Policy:     execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape:  "sha256:" + hex.EncodeToString(sum[:]),
		DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

func (c *rrCase) ensure(spec execution.ResourceSpec) execution.ResourceObservation {
	c.t.Helper()
	obs, err := c.res.Ensure(c.ctx, spec)
	if err != nil {
		c.t.Fatalf("ensure %s: %v", spec.Key, err)
	}
	c.track(obs.Handle)
	if obs.Phase != execution.PhasePending && obs.Phase != execution.PhaseReady {
		c.t.Fatalf("ensure %s reported %q", spec.Key, obs.Phase)
	}
	c.checkObservation(spec, obs)
	return obs
}

// checkObservation holds an observation of a live object to the contract: the
// handle is the spec's with a bound UID and the fingerprint is affirmative.
func (c *rrCase) checkObservation(spec execution.ResourceSpec, obs execution.ResourceObservation) {
	c.t.Helper()
	fp, err := spec.Fingerprint()
	if err != nil {
		c.t.Fatal(err)
	}
	if obs.Handle.UID == "" || obs.Handle != spec.Handle(obs.Handle.UID) || obs.Handle.Validate() != nil {
		c.t.Fatalf("%s: the observation names another handle: %+v", spec.Key, obs.Handle)
	}
	if !execution.ObservationVerifies(fp, obs) {
		c.t.Fatalf("%s: the observation carries no affirmative fingerprint: %q", spec.Key, obs.Fingerprint)
	}
}

func (c *rrCase) observe(h execution.ResourceHandle) execution.ResourceObservation {
	c.t.Helper()
	obs, err := c.res.Observe(c.ctx, h)
	if err != nil {
		c.t.Fatalf("observe %s: %v", h.Key, err)
	}
	return obs
}

// stop repeats Stop until the resource is verified stopped.
func (c *rrCase) stop(h execution.ResourceHandle) execution.ResourceStopEvidence {
	c.t.Helper()
	var ev execution.ResourceStopEvidence
	c.await("the stop of "+h.Key, func() bool {
		var err error
		if ev, err = c.res.Stop(c.ctx, h); err != nil {
			c.t.Fatalf("stop %s: %v", h.Key, err)
		}
		return ev.Stopped && (h.Kind != execution.KindWorkspace || ev.Released || !c.caps.Set.Has(execution.CapWorkspaceFence))
	})
	return ev
}

// awaitPhase polls Observe, which is how execution itself learns progress.
func (c *rrCase) awaitPhase(h execution.ResourceHandle, want execution.ResourcePhase) execution.ResourceObservation {
	c.t.Helper()
	var obs execution.ResourceObservation
	c.await(h.Key+" to be "+string(want), func() bool {
		obs = c.observe(h)
		if obs.Phase == execution.PhaseFailed {
			c.t.Fatalf("%s failed: %+v", h.Key, obs)
		}
		return obs.Phase == want
	})
	return obs
}

func (c *rrCase) await(what string, done func() bool) {
	c.t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(c.fx.Timeout)
	for !done() {
		select {
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", what)
		case <-c.ctx.Done():
			c.t.Fatalf("waiting for %s: %v", what, c.ctx.Err())
		case <-tick.C:
		}
	}
}

// competingEnsure races fresh adapters creating the same resource for the first
// time: every one succeeds and they all name one instance. It returns its handle.
func (c *rrCase) competingEnsure(spec execution.ResourceSpec) execution.ResourceHandle {
	c.t.Helper()
	const n = 6
	start := make(chan struct{})
	type result struct {
		obs execution.ResourceObservation
		err error
	}
	results := make(chan result, n)
	adapters := make([]execution.RunResources, n)
	for i := range adapters {
		adapters[i] = c.fx.Open()
	}
	var wg sync.WaitGroup
	for _, res := range adapters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			obs, err := res.Ensure(c.ctx, spec)
			results <- result{obs, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first execution.ResourceHandle
	for r := range results {
		if r.err != nil {
			c.t.Fatalf("%s: a competing initial create failed: %v", spec.Key, r.err)
		}
		if first == (execution.ResourceHandle{}) {
			first = r.obs.Handle
			c.track(first)
		}
		if r.obs.Handle != first {
			c.t.Fatalf("%s: competing initial creates made two instances: %+v, %+v", spec.Key, first, r.obs.Handle)
		}
		c.checkObservation(spec, r.obs)
	}
	return first
}

// concurrentEnsure proves simultaneous creates converge on one instance.
func (c *rrCase) concurrentEnsure(spec execution.ResourceSpec, uid string) {
	c.t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	uids := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs, err := c.fx.Open().Ensure(c.ctx, spec)
			errs <- err
			uids <- obs.Handle.UID
		}()
	}
	wg.Wait()
	close(errs)
	close(uids)
	for err := range errs {
		if err != nil {
			c.t.Fatalf("concurrent ensure: %v", err)
		}
	}
	for got := range uids {
		if got != uid {
			c.t.Fatalf("concurrent ensure produced instance %q, want %q", got, uid)
		}
	}
}
