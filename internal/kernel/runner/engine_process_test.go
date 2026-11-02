package runner_test

// Process-backend evidence. The other engine tests wire fakeRuntime and
// fakeResources, which are fault injectors: they prove how the engine behaves when
// a backend loses a response, forgets an object or refuses a stale fence, and say
// nothing about what any real backend guarantees. The tests in this file wire the
// real host backend (internal/runtime/process) behind the same ports with the
// memory state store and no container or service. They show real shared-file
// progression, fail-fast, reconstruction from retained state, a wait across which
// the workspace survives, and a real process group stopped on cancellation.
//
// Memory state plus Process proves reconstruction over retained state: a second
// engine continues what a first one stopped driving. It is not process-crash
// durability, which needs PostgreSQL and a recovery-capable backend.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/shell/runstate"
)

// startCounter counts the launches that reached the real backend.
type startCounter struct {
	runtime.Runtime
	mu     sync.Mutex
	starts []domain.StepID
}

func (c *startCounter) Start(ctx context.Context, req runtime.StartRequest) (runtime.WorkloadObservation, error) {
	c.mu.Lock()
	c.starts = append(c.starts, req.Binding.StepKey)
	c.mu.Unlock()
	return c.Runtime.Start(ctx, req)
}

func (c *startCounter) launched() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprint(c.starts)
}

// procEnv is the memory state store and the Process backend, with one
// controllable clock for leases and deadlines (the backend keeps real time).
type procEnv struct {
	*env
	k      *process.Kernel
	rt     *startCounter
	onPoll func() error
}

func newProcEnv(t *testing.T) *procEnv {
	t.Helper()
	k, err := process.NewKernel(process.KernelConfig{Dir: t.TempDir(), KillGrace: time.Second})
	if err != nil {
		t.Fatalf("open the process backend: %v", err)
	}
	t.Cleanup(func() {
		// Nothing a test started may outlive it.
		if _, err := k.Reconcile(context.Background(), func(domain.Scope, domain.RunID) bool { return false }); err != nil {
			t.Errorf("reconcile: %v", err)
		}
		k.Idle(10 * time.Second)
	})
	c := &testClock{now: t0}
	return &procEnv{env: &env{t: t, clock: c, state: runstate.NewMemory(c), outputs: newFakeOutputs(), secrets: &fakeSecrets{}},
		k: k, rt: &startCounter{Runtime: k.Runtime}}
}

func (p *procEnv) engine(owner string) *runner.Engine {
	p.t.Helper()
	eng, err := runner.NewEngine(runner.EngineConfig{State: p.state, Runtime: p.rt, Resources: p.k.Resources, Outputs: p.outputs,
		Clock: p.clock, Owner: owner, Backend: process.Backend, LeaseTTL: leaseTTL, PollInterval: 20 * time.Millisecond,
		StepTimeout: time.Minute, ReadyTimeout: time.Minute, CleanupBackoff: time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			p.clock.Advance(d)
			time.Sleep(5 * time.Millisecond)
			if p.onPoll != nil {
				return p.onPoll()
			}
			return ctx.Err()
		}})
	if err != nil {
		p.t.Fatalf("build the engine: %v", err)
	}
	return eng
}

func sh(key domain.StepID, script string, deps ...domain.StepID) runtime.PlanStep {
	s := ctr(key, deps...)
	s.Workload.Commands = []string{script}
	return s
}

// gone asserts every resource the run retained no longer exists in the backend.
func (p *procEnv) gone(rec runtime.RunRecord) {
	p.t.Helper()
	n := 0
	for _, o := range rec.Resources {
		got, err := p.k.Resources.Observe(context.Background(), o.Handle)
		if err != nil || got.State != runtime.ResourceRemoved {
			p.t.Fatalf("resource %s is not removed in the backend: %+v %v", o.Handle.Name, got, err)
		}
		n++
	}
	if n == 0 {
		p.t.Fatal("the record retained no resource observation")
	}
}

func processProgression(t *testing.T) {
	t.Run("steps share files in the real workspace and the run ends cleaned up", func(t *testing.T) {
		p := newProcEnv(t)
		p.admit("run1", plan("proc-seq",
			sh("a", `echo hello-from-a > greeting.txt`),
			sh("b", `test "$(cat greeting.txt)" = hello-from-a && echo done > b.txt`, "a"),
			sh("c", `test -f b.txt && test "$KODAMA_STEP_KEY" = c && test "$KODAMA_RUN_ID" = run1`, "b")), 3)
		rep := p.drive(p.engine("w1"), "run1")

		got := p.load("run1")
		p.expect(rep.Status == runner.DriveTerminal && got.Run.Status() == runtime.StatusSucceeded && got.Run.Cleanup.State == runtime.CleanupComplete, "run: %+v", got.Run)
		for _, k := range []domain.StepID{"a", "b", "c"} {
			s := stepOf(got, k)
			p.expect(s.State == runtime.StepSucceeded && s.Result.Code == runtime.ResultSucceeded && s.Attempt == 1, "step %s: %+v", k, s)
		}
		p.expect(p.rt.launched() == "[a b c]", "one launch per step, in order: %s", p.rt.launched())
		p.gone(got)
	})

	t.Run("a failing command fails fast with its real exit code", func(t *testing.T) {
		p := newProcEnv(t)
		p.admit("run1", plan("proc-fail", sh("a", `echo ok`), sh("b", `exit 3`, "a"), sh("c", `echo never > c.txt`, "b")), 3)
		rep := p.drive(p.engine("w1"), "run1")

		got := p.load("run1")
		p.expect(rep.Status == runner.DriveTerminal && got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonCommandFailed, "run: %+v", got.Run)
		b := stepOf(got, "b")
		p.expect(b.State == runtime.StepFailed && b.Result.Code == runtime.ResultFailed && b.Result.ExitCode == 3, "step b: %+v", b)
		p.expect(stepOf(got, "c").State == runtime.StepSkipped && stepOf(got, "c").Reason == runtime.ReasonRunFailed && p.rt.launched() == "[a b]", "c never ran: %s", p.rt.launched())
		p.gone(got)
	})

	t.Run("a second engine reconstructs the run from retained state and adopts the live workload", func(t *testing.T) {
		p := newProcEnv(t)
		p.admit("run1", plan("proc-recon",
			sh("a", `sleep 0.4; echo from-a > a.txt`),
			sh("b", `test "$(cat a.txt)" = from-a`, "a")), 3)
		polls := 0
		p.onPoll = func() error {
			if polls++; polls == 3 {
				return errCrash
			}
			return nil
		}
		_, err := p.engine("w1").Drive(context.Background(), p.ref("run1", runtime.ClaimExecute))
		p.expect(err != nil && err.Error() == errCrash.Error(), "the first engine stopped mid-step: %v", err)
		mid := p.load("run1")
		p.expect(stepOf(mid, "a").State == runtime.StepRunning && p.rt.launched() == "[a]", "step a is running in the backend: %v", states(mid))

		p.onPoll = nil
		p.expire()
		p.drive(p.engine("w2"), "run1")
		got := p.load("run1")
		p.expect(got.Run.Status() == runtime.StatusSucceeded && got.Claim.Fence == 2, "run: %+v claim %+v", got.Run, got.Claim)
		p.expect(p.rt.launched() == "[a b]", "step a was launched once, by the first engine: %s", p.rt.launched())
		p.gone(got)
	})
}

func processWaits(t *testing.T) {
	t.Run("a wait holds no process and the real workspace survives it", func(t *testing.T) {
		p := newProcEnv(t)
		build := sh("build", `echo candidate-1 > cand.txt`)
		build.Outputs = []string{"commit"}
		review := runtime.PlanStep{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &runtime.WaitSpec{
			Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"}, Input: "approval",
			Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour}}
		ship := sh("ship", `test "$(cat cand.txt)" = candidate-1`, "review")
		ship.When = `review.outcome == "approved"`
		p.admit("run1", plan("proc-wait", build, review, ship), 3)
		p.outputs.produce("build", map[string]string{"commit": "c1"})

		rep := p.drive(p.engine("w1"), "run1")
		p.expect(rep.Status == runner.DriveWaiting && p.rt.launched() == "[build]", "parked on the wait: %s", rep.Status)
		mid := p.load("run1")
		for _, o := range mid.Resources {
			got, err := p.k.Resources.Observe(context.Background(), o.Handle)
			p.expect(err == nil && got.State == runtime.ResourceReady, "the workspace is retained while the wait is open: %+v %v", got, err)
		}

		res := p.decide(signal("run1", "k1", "approved"))
		p.expect(res.Applied, "the signal decided the wait: %+v", res)
		p.expire()
		p.drive(p.engine("w2"), "run1")
		got := p.load("run1")
		p.expect(got.Run.Status() == runtime.StatusSucceeded && p.rt.launched() == "[build ship]", "the same run continued once: %s %+v", p.rt.launched(), got.Run)
		p.gone(got)
	})
}

func processCleanup(t *testing.T) {
	t.Run("a cancellation stops the real process group and the run ends cleaned up", func(t *testing.T) {
		p := newProcEnv(t)
		p.admit("run1", plan("proc-cancel", sh("a", `sleep 60`), sh("b", `echo never`, "a")), 3)
		polls := 0
		p.onPoll = func() error {
			if polls++; polls == 3 {
				_, err := p.state.RequestCancel(context.Background(), scope, "run1")
				return err
			}
			return nil
		}
		start := time.Now()
		rep := p.drive(p.engine("w1"), "run1")
		got := p.load("run1")
		p.expect(rep.Status == runner.DriveTerminal && got.Run.Status() == runtime.StatusCancelled && got.Run.Cleanup.State == runtime.CleanupComplete, "run: %+v", got.Run)
		p.expect(time.Since(start) < 30*time.Second && stepOf(got, "a").State == runtime.StepCancelled && p.rt.launched() == "[a]", "the sleeping process was stopped: %v", time.Since(start))
		p.expect(p.k.Idle(10*time.Second), "no supervised process outlived the run")
		p.gone(got)
	})
}
