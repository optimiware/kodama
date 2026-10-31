package runner_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ---------------------------------------------------------------------------
// Uncertain stop and removal are observation-only
// ---------------------------------------------------------------------------

// uncertainEffectsObservationOnly proves an intent or uncertain stop or removal is
// never issued again by any later attempt or claim, whatever an observation
// says: it fails, is inconclusive, or shows the object still there. None of
// those proves the first call never arrived.
func uncertainEffectsObservationOnly(t *testing.T) {
	modes := []string{"the observation fails", "the observation is inconclusive", "the object is still there"}
	for _, kind := range []string{"workload removal", "workload stop", "resource removal"} {
		for _, mode := range modes {
			t.Run(kind+" across takeover: "+mode, func(t *testing.T) {
				e := newEnv(t)
				silent := func(int, runtime.RuntimeBinding) (bool, error) { return false, errors.New("timeout") }
				switch kind {
				case "workload removal":
					e.admit("run1", plan("u-remove", ctr("a"), ctr("b", "a")), 2)
					e.rt.removeHook = silent
				case "workload stop":
					slow := ctr("a")
					slow.Workload.Timeout = 2 * poll
					e.admit("run1", plan("u-stop", slow), 2)
					e.rt.scripts["a"] = script{polls: 1000}
					e.rt.stopHook, e.rt.removeHook = silent, silent
				default:
					e.admit("run1", plan("u-res", ctr("a")), 2)
					e.res.removeHook = func(int, runtime.ResourceHandle) (bool, error) { return false, errors.New("timeout") }
				}
				e.drive(e.engine("w1"), "run1")

				c := e.load("run1").Run.Cleanup
				e.expect(c.State == runtime.CleanupExhausted && len(c.RetainedWorkloads)+len(c.Retained) > 0, "retained after bounded attempts: %+v", c)
				issued := func() [3]int {
					return [3]int{e.rt.count(e.rt.stops), e.rt.count(e.rt.removes), len(e.res.removes)}
				}
				first := issued()
				e.expect(first[0] <= 1 && first[1] <= 1 && first[2] <= 1, "each effect was issued once across the release stage and every cleanup attempt: %v", first)

				switch {
				case mode == "the observation fails":
					e.rt.observeHook = func(int, runtime.RuntimeBinding) error { return errors.New("backend unavailable") }
					e.res.observeHook = func(runtime.ResourceHandle) error { return errors.New("backend unavailable") }
				case mode == "the observation is inconclusive":
					e.rt.unknown, e.res.unknown = true, true
				case kind == "workload removal":
					for _, w := range e.rt.workloads {
						w.phase = runtime.PhaseRunning
					}
				}
				// Every hook that applied nothing stays: the object is still there.
				for i := 2; i <= 3; i++ {
					e.expire()
					rep := e.cleanupDrive(e.engine(fmt.Sprintf("w%d", i)), "run1")
					got := e.load("run1")
					e.expect(rep.Status == runner.DriveTerminal && got.Run.Cleanup.State == runtime.CleanupExhausted, "still retained: %+v", got.Run.Cleanup)
					e.expect(issued() == first, "claim %d issued nothing again: %v vs %v", i, issued(), first)
				}

				// The backend's own reconciliation ends the object; observation confirms it.
				e.rt.observeHook, e.res.observeHook, e.rt.unknown, e.res.unknown = nil, nil, false, false
				e.rt.workloads = map[string]*fakeWorkload{}
				e.res.state = map[string]runtime.ResourceState{}
				e.expire()
				e.cleanupDrive(e.engine("w4"), "run1")
				got := e.load("run1")
				e.expect(got.Run.Cleanup.State == runtime.CleanupComplete && issued() == first, "confirmed by observation without an effect: %+v", got.Run.Cleanup)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// An ensure that was never found
// ---------------------------------------------------------------------------

// ensureAbsentFailsAtomically proves the non-retryable failure of an uncertain
// ensure commits with the run's failure: a crash at the boundary leaves the
// command unsettled and the run unfailed, and the next claim reconciles it
// without issuing the ensure again.
func ensureAbsentFailsAtomically(t *testing.T) {
	e := newEnv(t)
	e.admit("run1", plan("ensure-absent", ctr("a")), 3)
	e.res.ensureHook = func(int, runtime.ResourceSpec) (bool, error) { return false, context.DeadlineExceeded }
	crashed := false
	store := &raceStore{StateStore: e.state, before: func(tr runtime.Transition) {
		if !crashed && tr.Run.Outcome == runtime.OutcomeFailed {
			crashed = true
			panic(errCrash) // the process dies right before the run's failure commits
		}
	}}
	e.crash(e.engine("w1", func(c *runner.EngineConfig) { c.State = store }), "run1")

	mid := e.load("run1")
	ensure := func(rec runtime.RunRecord) runtime.Command {
		for _, c := range rec.Commands {
			if c.Kind == runtime.CommandEnsureResource {
				return c
			}
		}
		t.Fatal("no ensure command")
		return runtime.Command{}
	}
	e.expect(ensure(mid).State == runtime.CommandUncertain && !mid.Run.Settled() && stepOf(mid, "a").State == runtime.StepPending,
		"the failure and the command's settlement commit together, so neither happened: %+v %v", ensure(mid), states(mid))

	e.expire()
	e.drive(e.engine("w2"), "run1")
	got := e.load("run1")
	c := ensure(got)
	e.expect(len(e.res.ensures) == 1, "the ensure was never issued again: %v", e.res.ensures)
	e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonPreparationFailed && stepOf(got, "a").State == runtime.StepSkipped, "run: %+v", got.Run)
	e.expect(c.State == runtime.CommandRejected && c.Reason == runtime.ReasonLaunchUncertain, "final, and not a proof of non-delivery: %+v", c)
	e.expect(slices.Contains(e.res.removes, "workspace") && got.Run.Cleanup.State == runtime.CleanupComplete, "cleanup still removed the identity: %v", e.res.removes)

	// A claim that finds the rejected command after the failure committed still
	// never issues the ensure again.
	e2 := newEnv(t)
	e2.admit("run1", plan("ensure-absent-2", ctr("a")), 3)
	e2.res.ensureHook = func(int, runtime.ResourceSpec) (bool, error) { return false, context.DeadlineExceeded }
	e2.drive(e2.engine("w1"), "run1")
	e2.expect(len(e2.res.ensures) == 1, "one ensure: %v", e2.res.ensures)
}

// delayedEnsureIsCleaned proves a create that lands after the engine decided the
// ensure failed is still fenced and removed by cleanup, from the identity the
// run retained, and that the identity stays retained until removal is confirmed.
func delayedEnsureIsCleaned(t *testing.T) {
	e := newEnv(t)
	e.admit("run1", plan("ensure-late", ctr("a")), 2)
	var identity string
	e.res.ensureHook = func(_ int, spec runtime.ResourceSpec) (bool, error) {
		identity = spec.Identity
		return false, context.DeadlineExceeded
	}
	var removed []runtime.ResourceHandle
	e.res.onRemove = func(h runtime.ResourceHandle) {
		removed = append(removed, h)
		e.res.state[identity] = runtime.ResourceReady // the delayed create lands before the removal
	}
	e.drive(e.engine("w1"), "run1")
	got := e.load("run1")
	e.expect(got.Run.Reason == runtime.ReasonPreparationFailed && len(removed) == 1 && removed[0].Identity == identity && removed[0].Fence == 1,
		"cleanup removed the identity it had retained, under its fence: %+v", removed)
	e.expect(len(e.res.state) == 0 && got.Run.Cleanup.State == runtime.CleanupComplete, "the late resource did not leak: %v", e.res.state)

	// And while removal is unconfirmed the identity stays retained.
	e2 := newEnv(t)
	e2.admit("run1", plan("ensure-late-2", ctr("a")), 2)
	e2.res.ensureHook = func(_ int, spec runtime.ResourceSpec) (bool, error) {
		identity = spec.Identity
		return false, context.DeadlineExceeded
	}
	e2.res.removeHook = func(int, runtime.ResourceHandle) (bool, error) { return false, errors.New("storage unavailable") }
	e2.res.onRemove = func(runtime.ResourceHandle) { e2.res.state[identity] = runtime.ResourceReady } // the delayed create landed
	e2.drive(e2.engine("w1"), "run1")
	c := e2.load("run1").Run.Cleanup
	e2.expect(c.State == runtime.CleanupExhausted && len(c.Retained) == 1 && c.Retained[0].Identity == identity && c.Retained[0].Name == "workspace",
		"the identity of a possibly created resource is retained: %+v", c)
}

// ---------------------------------------------------------------------------
// Secret revocation
// ---------------------------------------------------------------------------

func secretStep(key domain.StepID, deps ...domain.StepID) runtime.PlanStep {
	s := ctr(key, deps...)
	s.Workload.Secrets = []runtime.SecretRef{{Owner: domain.Owner{TenantID: "t1", ProjectID: "p1"}, Name: "TOKEN"}}
	return s
}

// diskSecrets is a SecretResolver that really creates private material on disk and
// really removes it, so a fake cannot conceal what a recovering engine is able to
// revoke. Release removes exactly the directory in the materialization it is given
// and refuses one without a directory: it does not guess from the step's identity.
// The fakeSecrets above stay for fault injection only.
type diskSecrets struct {
	root string
	mu   sync.Mutex
	n    int
	// failReleases fails that many Release calls before any succeeds.
	failReleases int
	// beforeRelease runs at the start of every Release (a panic stands for a crash).
	beforeRelease func()
}

func newDiskSecrets(t *testing.T) *diskSecrets { return &diskSecrets{root: t.TempDir()} }

func (d *diskSecrets) Resolve(_ context.Context, req runtime.ResolveRequest) (runtime.Materialization, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	dir := filepath.Join(d.root, fmt.Sprintf("%s-%d", req.StepKey, d.n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return runtime.Materialization{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "TOKEN"), []byte("s3cret-"+string(req.StepKey)), 0o400); err != nil {
		return runtime.Materialization{}, err
	}
	return runtime.Materialization{Scope: req.Scope, RunID: req.RunID, StepKey: req.StepKey, Directory: dir, Refs: req.Refs,
		Revision: fmt.Sprintf("rev-%d", d.n)}, nil
}

func (d *diskSecrets) Release(_ context.Context, m runtime.Materialization) error {
	if d.beforeRelease != nil {
		d.beforeRelease()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failReleases > 0 {
		d.failReleases--
		return errors.New("vault unavailable")
	}
	if m.Directory == "" || m.Revision == "" {
		return errors.New("cannot revoke a materialization without its directory and revision")
	}
	return os.RemoveAll(m.Directory)
}

// material lists the private material still on disk.
func (d *diskSecrets) material() []string {
	entries, _ := os.ReadDir(d.root)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (e *env) withDisk(d *diskSecrets) func(*runner.EngineConfig) {
	return func(c *runner.EngineConfig) { c.Secrets = d }
}

func (e *env) driveStatus(eng *runner.Engine, id domain.RunID, purpose runtime.ClaimPurpose) runner.Report {
	e.t.Helper()
	rep, err := eng.Drive(context.Background(), e.ref(id, purpose))
	if err != nil {
		e.t.Fatalf("drive: %v", err)
	}
	return rep
}

// revocationIsFaithful proves the engine that resolved a step's secrets removes
// the material before any later step starts.
func revocationIsFaithful(t *testing.T) {
	e := newEnv(t)
	disk := newDiskSecrets(t)
	e.admit("run1", plan("revoke-disk", secretStep("a"), ctr("b", "a")), 3)
	e.rt.onStart = func(req runtime.StartRequest) {
		if req.Binding.StepKey == "a" {
			_, err := os.Stat(filepath.Join(req.Secrets.Directory, "TOKEN"))
			e.expect(err == nil, "the workload's private material exists while it runs: %v", err)
			return
		}
		e.expect(len(disk.material()) == 0, "b started with a's material still on disk: %v", disk.material())
	}
	e.drive(e.engine("w1", e.withDisk(disk)), "run1")
	got := e.load("run1")
	e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && len(disk.material()) == 0, "run %+v, material %v", got.Run, disk.material())
}

// revocationHoldsOnTakeover proves a replacement engine never advances, completes
// or skips over secret material it cannot revoke faithfully: it holds with a
// stable reason, with no resolver or with one that is not given the
// materialization (which the durable record does not hold).
func revocationHoldsOnTakeover(t *testing.T) {
	crashes := map[string]func(*env, *diskSecrets) *runner.Engine{
		"mid-step": func(e *env, disk *diskSecrets) *runner.Engine {
			e.rt.scripts["a"] = script{polls: 6}
			sleeps := 0
			e.onSleep = func() error {
				if sleeps++; sleeps == 2 {
					return errCrash
				}
				return nil
			}
			return e.engine("w1", e.withDisk(disk))
		},
		"right after settlement": func(e *env, disk *diskSecrets) *runner.Engine {
			disk.beforeRelease = func() { panic(errCrash) }
			return e.engine("w1", e.withDisk(disk))
		},
	}
	for name, first := range crashes {
		t.Run("the first engine dies "+name, func(t *testing.T) {
			e := newEnv(t)
			disk := newDiskSecrets(t)
			e.admit("run1", plan("revoke-takeover", secretStep("a"), ctr("b", "a")), 3)
			eng := first(e, disk)
			func() {
				defer func() {
					if r := recover(); r != nil && r != errCrash {
						panic(r)
					}
				}()
				_, _ = eng.Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
			}()
			e.onSleep = nil
			disk.beforeRelease = nil
			e.expect(len(disk.material()) == 1, "the first engine left private material: %v", disk.material())

			// A replacement with no resolver cannot revoke at all.
			e.expire()
			rep := e.driveStatus(e.engine("w2", func(c *runner.EngineConfig) { c.Secrets = nil }), "run1", runtime.ClaimExecute)
			e.expect(rep.Status == runner.DriveHeld && rep.Reason == runner.ReasonRevocationNoResolver, "no resolver: %s %q", rep.Status, rep.Reason)
			e.expect(!stepStarted(e, "b"), "b did not start: %v", e.rt.startOrder)

			// One with a resolver is not handed the materialization either.
			e.expire()
			rep = e.driveStatus(e.engine("w3", e.withDisk(disk)), "run1", runtime.ClaimExecute)
			e.expect(rep.Status == runner.DriveHeld && rep.Reason == runner.ReasonRevocationUnrecoverable, "no durable materialization: %s %q", rep.Status, rep.Reason)
			got := e.load("run1")
			e.expect(!stepStarted(e, "b") && !got.Run.Settled() && len(disk.material()) == 1,
				"nothing advanced, nothing was silently skipped, the material is still there and still reported: %v %v", got.Run.Status(), disk.material())
		})
	}
}

func stepStarted(e *env, key domain.StepID) bool {
	for _, k := range e.rt.startOrder {
		if k == key {
			return true
		}
	}
	return false
}

// failedReleaseHoldsTheRun proves a release that fails is retried in place by the
// engine that holds the materialization, never discarded, and that when it keeps
// failing the run does not advance and a later claim holds rather than guesses.
func failedReleaseHoldsTheRun(t *testing.T) {
	t.Run("a transient failure is retried in place", func(t *testing.T) {
		e := newEnv(t)
		disk := newDiskSecrets(t)
		disk.failReleases = 2
		e.admit("run1", plan("revoke-retry", secretStep("a"), ctr("b", "a")), 3)
		e.rt.onStart = func(req runtime.StartRequest) {
			if req.Binding.StepKey == "b" {
				e.expect(len(disk.material()) == 0, "b started before the release succeeded: %v", disk.material())
			}
		}
		e.drive(e.engine("w1", e.withDisk(disk)), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && len(disk.material()) == 0, "run %+v material %v", got.Run, disk.material())
	})
	t.Run("a persistent failure stops the run at the settled step", func(t *testing.T) {
		e := newEnv(t)
		disk := newDiskSecrets(t)
		disk.failReleases = 1000
		e.admit("run1", plan("revoke-stuck", secretStep("a"), ctr("b", "a")), 3)
		rep := e.drive(e.engine("w1", e.withDisk(disk)), "run1")
		got := e.load("run1")
		e.expect(rep.Status == runner.DriveRetry && stepOf(got, "a").State == runtime.StepSucceeded && !stepStarted(e, "b") && len(disk.material()) == 1,
			"%s %v %v", rep.Status, states(got), disk.material())
		disk.failReleases = 0
		e.expire()
		rep = e.drive(e.engine("w2", e.withDisk(disk)), "run1")
		e.expect(rep.Status == runner.DriveHeld && rep.Reason == runner.ReasonRevocationUnrecoverable && !stepStarted(e, "b") && len(disk.material()) == 1,
			"a later claim cannot revoke what it did not materialize: %s %q", rep.Status, rep.Reason)
	})
}

// cleanupHoldsForRevocation proves a run whose settled step's secrets cannot be
// revoked is never terminal: cleanup stays visibly pending, and a replacement
// cleanup claim holds with a stable reason instead of completing.
func cleanupHoldsForRevocation(t *testing.T) {
	e := newEnv(t)
	disk := newDiskSecrets(t)
	disk.failReleases = 1000
	e.admit("run1", plan("revoke-clean", secretStep("a"), ctr("b", "a")), 3)
	e.rt.scripts["a"] = script{result: runtime.Result{Code: runtime.ResultFailed, ExitCode: 2}}
	rep := e.drive(e.engine("w1", e.withDisk(disk)), "run1")
	got := e.load("run1")
	e.expect(rep.Status != runner.DriveTerminal && got.Run.Outcome == runtime.OutcomeFailed && !got.Run.Terminal() && got.Run.Status() == runtime.StatusCleaning && len(disk.material()) == 1,
		"the failed run is cleaning, not terminal: %s %v %v", rep.Status, got.Run.Status(), disk.material())

	disk.failReleases = 0
	for i, tc := range []struct {
		secrets runtime.SecretResolver
		reason  runtime.Reason
	}{{nil, runner.ReasonRevocationNoResolver}, {disk, runner.ReasonRevocationUnrecoverable}} {
		e.expire()
		eng := e.engine(fmt.Sprintf("w%d", i+2), func(c *runner.EngineConfig) { c.Secrets = tc.secrets })
		rep = e.driveStatus(eng, "run1", runtime.ClaimCleanup)
		got = e.load("run1")
		e.expect(rep.Status == runner.DriveHeld && rep.Reason == tc.reason && !got.Run.Terminal() && got.Run.Cleanup.State == runtime.CleanupPending && len(disk.material()) == 1,
			"a replacement cleanup claim holds: %s %q %v", rep.Status, rep.Reason, got.Run.Cleanup.State)
	}
}

// ---------------------------------------------------------------------------
// Loss during a wait; transient reads
// ---------------------------------------------------------------------------

func lastWaitPlan(onExpiry runtime.ExpiryAction, withService bool) runtime.Plan {
	build := ctr("build")
	build.Outputs = []string{"commit"}
	review := runtime.PlanStep{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &runtime.WaitSpec{
		Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"}, Input: "approval",
		Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour, OnExpiry: onExpiry}}
	p := plan("last-wait-"+string(onExpiry), build, review)
	if withService {
		p.Services = []runtime.ServiceSpec{{Name: "db", Lifetime: runtime.LifetimePipeline, Image: "postgres:16"}}
	}
	return p
}

// lostStateBlocksResume proves a decided wait does not let the run report success
// over a workspace or service lost while it was parked, even with no executable
// step left to notice.
func lostStateBlocksResume(t *testing.T) {
	park := func(t *testing.T, onExpiry runtime.ExpiryAction, svc bool) *env {
		e := newEnv(t)
		e.admit("run1", lastWaitPlan(onExpiry, svc), 3)
		e.outputs.produce("build", map[string]string{"commit": "c1"})
		e.expect(e.drive(e.engine("w1"), "run1").Status == runner.DriveWaiting, "the run parks on its last step")
		return e
	}
	t.Run("a resolution after the workspace was lost", func(t *testing.T) {
		e := park(t, "", false)
		e.res.lose("workspace")
		e.decide(signal("run1", "k1", "approved"))
		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonWorkspaceLost, "run: %+v", got.Run)
		e.expect(reviewWaitOf(got).State == runtime.WaitResolved && len(e.res.ensures) == 1, "the decision stands; nothing was replaced")
	})
	t.Run("an expiry that continues after a service was lost", func(t *testing.T) {
		e := park(t, runtime.ExpiryContinue, true)
		e.res.lose("db")
		e.clock.Advance(2 * time.Hour)
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonServiceLost, "run: %+v", got.Run)
	})
	t.Run("an expiry the store already recorded, after the workspace was lost", func(t *testing.T) {
		e := park(t, runtime.ExpiryContinue, false)
		e.res.lose("workspace")
		e.clock.Set(reviewWaitOf(e.load("run1")).Deadline)
		res := e.decide(signal("run1", "k1", "approved"))
		e.expect(!res.Applied && res.Decision.Kind == runtime.DecisionExpired, "the store expired the wait: %+v", res)
		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonWorkspaceLost, "run: %+v", got.Run)
	})
	t.Run("an intact workspace lets the same resolution succeed", func(t *testing.T) {
		e := park(t, "", true)
		e.decide(signal("run1", "k1", "approved"))
		e.expire()
		e.drive(e.engine("w2"), "run1")
		e.expect(e.load("run1").Run.Outcome == runtime.OutcomeSucceeded, "the check does not fail a healthy run")
	})
}

// transientReadsLeaveStepsPending proves a failing outputs port is not a verdict
// on a condition or a binding: the step stays pending for a later claim, while a
// condition the grammar refuses still fails it.
func transientReadsLeaveStepsPending(t *testing.T) {
	mk := func(mutate func(*runtime.PlanStep)) runtime.Plan {
		build := ctr("build")
		build.Outputs = []string{"mode"}
		deploy := ctr("deploy", "build")
		mutate(&deploy)
		return plan("transient", build, deploy)
	}
	for name, mutate := range map[string]func(*runtime.PlanStep){
		"a when predicate": func(s *runtime.PlanStep) { s.When = `build.outputs.mode == "fast"` },
		"a binding":        func(s *runtime.PlanStep) { s.Bindings = map[string]string{"MODE": "build.outputs.mode"} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.admit("run1", mk(mutate), 3)
			e.outputs.produce("build", map[string]string{"mode": "fast"})
			bad := &flakyOutputs{fakeOutputs: e.outputs, failReads: 1}
			eng := e.engine("w1", func(c *runner.EngineConfig) { c.Outputs = bad })
			rep := e.drive(eng, "run1")
			got := e.load("run1")
			e.expect(rep.Status == runner.DriveRetry && stepOf(got, "deploy").State == runtime.StepPending && !stepOf(got, "deploy").Predicate.Evaluated,
				"a failing port leaves the step pending: %s %+v", rep.Status, stepOf(got, "deploy"))
			e.expire()
			e.drive(eng, "run1")
			got = e.load("run1")
			e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && stepOf(got, "deploy").State == runtime.StepSucceeded, "the next claim ran it: %v", states(got))
		})
	}
	t.Run("a condition the grammar refuses still fails the step", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", mk(func(s *runtime.PlanStep) { s.When = `build.outputs.mode == 1` }), 3)
		e.drive(e.engine("w1"), "run1")
		got := e.load("run1")
		d := stepOf(got, "deploy")
		e.expect(d.State == runtime.StepFailed && d.Reason == runtime.ReasonPreparationFailed, "step: %+v", d)
	})
}
