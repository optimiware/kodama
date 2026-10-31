package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// StepRef names one step attempt of one run.
type StepRef struct {
	Scope   domain.Scope
	RunID   domain.RunID
	StepKey domain.StepID
	Attempt int
}

// Outputs is the engine's consumer-owned port for the declared outputs of a
// container step. The run record has no field for them, so the shell (or a test)
// retains them: Collect reads and durably records the declared outputs of a
// step that settled as succeeded, and is idempotent on the step reference; Read
// returns what was recorded, or nothing. The engine calls Collect only for a
// succeeded step and Read only for one that succeeded, so the outputs of a
// failed or cancelled step are never recorded or visible. An engine without an
// Outputs port treats every output as missing.
type Outputs interface {
	Collect(ctx context.Context, ref StepRef, declared []string) (map[string]string, error)
	Read(ctx context.Context, ref StepRef) (map[string]string, error)
}

// EngineConfig wires the engine to its ports. State, Runtime, Resources and
// Clock are required; the rest are optional. The engine reads no environment
// and no ambient time.
type EngineConfig struct {
	State     runtime.StateStore
	Runtime   runtime.Runtime
	Resources runtime.ResourceManager
	// Secrets resolves declared references; a step that declares secrets fails
	// with secret_unavailable when it is nil.
	Secrets runtime.SecretResolver
	// Workspace supplies the pinned preparation inputs; a nil preparer means a
	// source-free run.
	Workspace runtime.WorkspacePreparer
	// Outputs records and reads declared step outputs; see Outputs.
	Outputs Outputs
	Clock   runtime.Clock

	// Owner identifies this engine in claims; it must be unique per worker.
	Owner string
	// Backend names the runtime adapter recorded in every workload binding.
	Backend string

	// LeaseTTL is the claim lease, renewed by every commit and every poll.
	LeaseTTL time.Duration
	// PollInterval is how long the engine waits between observations of a
	// running workload or a starting service.
	PollInterval time.Duration
	// StepTimeout bounds a step whose plan sets no timeout.
	StepTimeout time.Duration
	// ReadyTimeout bounds how long a run-scoped service may take to become ready.
	ReadyTimeout time.Duration
	// CleanupBackoff is the pause between unfinished cleanup attempts.
	CleanupBackoff time.Duration
	// LaunchRetries is how many times a launch that provably did not reach the
	// backend is issued again as a new attempt; zero means the default of 2 and a
	// negative value disables retries.
	LaunchRetries int
	// ObserveFailures is how many consecutive failed or inconclusive observations
	// the engine tolerates before it stops and leaves the run for a later claim.
	ObserveFailures int
	// Sleep pauses between polls; tests replace it to drive time. It must return
	// early with the context's error.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Engine progresses runs through the kernel ports. It holds no run state: every
// decision is made from the durable record it loads under a claim, so any engine
// instance can take over a run another one left.
type Engine struct{ cfg EngineConfig }

// NewEngine validates the wiring and applies defaults.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	switch {
	case cfg.State == nil:
		return nil, errors.New("runner: the engine needs a state store")
	case cfg.Runtime == nil:
		return nil, errors.New("runner: the engine needs a runtime")
	case cfg.Resources == nil:
		return nil, errors.New("runner: the engine needs a resource manager")
	case cfg.Clock == nil:
		return nil, errors.New("runner: the engine needs a clock")
	case cfg.Owner == "":
		return nil, errors.New("runner: the engine needs an owner identity")
	case cfg.Backend == "":
		return nil, errors.New("runner: the engine needs a backend name")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 15 * time.Minute
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.CleanupBackoff <= 0 {
		cfg.CleanupBackoff = time.Second
	}
	if cfg.LaunchRetries == 0 {
		cfg.LaunchRetries = 2
	}
	if cfg.ObserveFailures <= 0 {
		cfg.ObserveFailures = 5
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	return &Engine{cfg: cfg}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// DriveStatus says why a Drive stopped.
type DriveStatus string

// Drive statuses.
const (
	// DriveTerminal: the run is terminal, its cleanup confirmed or exhausted.
	DriveTerminal DriveStatus = "terminal"
	// DriveWaiting: the run is parked on an open wait; Next is the earliest
	// deadline. The lease is left to expire so a later claim resumes the run.
	DriveWaiting DriveStatus = "waiting"
	// DriveCleaning: execution ended and cleanup is unfinished but cannot make
	// progress now (a retained resource or workload, or a transient failure).
	DriveCleaning DriveStatus = "cleaning"
	// DriveRetry: an observation or effect was inconclusive; a later claim
	// reconciles it from the durable record.
	DriveRetry DriveStatus = "retry"
	// DriveSkipped: the claim was not granted (held, or the purpose closed).
	DriveSkipped DriveStatus = "skipped"
	// DriveLost: a takeover superseded this engine's claim; it wrote nothing more.
	DriveLost DriveStatus = "lost"
)

// Report is the outcome of one Drive.
type Report struct {
	Ref    runtime.RunRef
	Status DriveStatus
	// Next is when a waiting run next needs attention (the earliest wait deadline).
	Next time.Time
	// Run is the last record the engine wrote or read; zero when the claim was refused.
	Run runtime.Run
}

// Poll claims and drives up to limit runnable runs, in the order the store lists
// them. Reports are returned for every run it attempted, including skipped ones.
func (e *Engine) Poll(ctx context.Context, limit int) ([]Report, error) {
	refs, err := e.cfg.State.Runnable(ctx, runtime.RunnableQuery{Now: e.cfg.Clock.Now(), Limit: limit})
	if err != nil {
		return nil, err
	}
	var out []Report
	for _, ref := range refs {
		rep, err := e.Drive(ctx, ref)
		if err != nil {
			return out, fmt.Errorf("run %s: %w", ref.RunID, err)
		}
		out = append(out, rep)
	}
	return out, nil
}

// Serve polls until the context ends, pausing interval between sweeps.
func (e *Engine) Serve(ctx context.Context, interval time.Duration, limit int) error {
	for {
		if _, err := e.Poll(ctx, limit); err != nil && ctx.Err() == nil {
			return err
		}
		if err := e.cfg.Sleep(ctx, interval); err != nil {
			return nil
		}
	}
}

// Drive claims one run for ref.Purpose and progresses it until it is terminal,
// parked on a wait, or cannot safely continue. A claim that is held or whose
// purpose the run no longer admits is reported as skipped, not as an error. A
// takeover during the drive is reported as lost: the stale engine stops writing.
func (e *Engine) Drive(ctx context.Context, ref runtime.RunRef) (Report, error) {
	rec, err := e.cfg.State.Claim(ctx, runtime.ClaimRequest{Scope: ref.Scope, RunID: ref.RunID, Owner: e.cfg.Owner,
		Purpose: ref.Purpose, TTL: e.cfg.LeaseTTL}, e.cfg.Clock.Now())
	if errors.Is(err, runtime.ErrClaimHeld) || errors.Is(err, runtime.ErrExecutionClosed) {
		return Report{Ref: ref, Status: DriveSkipped}, nil
	}
	if err != nil {
		return Report{}, err
	}
	d := &drive{e: e, ctx: ctx, rec: rec, claim: rec.Claim, fresh: map[string]pendingStart{}}
	rep, err := d.run()
	rep.Ref, rep.Run = ref, d.rec.Run
	if errors.Is(err, runtime.ErrStaleFence) || errors.Is(err, runtime.ErrClaimExpired) {
		rep.Status = DriveLost
		return rep, nil
	}
	return rep, err
}

// flow is what a stage of the drive loop decided.
type flow int

const (
	// flowPass: the stage had nothing to do; try the next one.
	flowPass flow = iota
	// flowAgain: the stage changed state; start over from the record.
	flowAgain
	// flowStop: the drive ends with d.report.
	flowStop
)

// maxConflicts bounds how often one drive reloads after a lost version race
// before it gives up and leaves the run to a later claim.
const maxConflicts = 16

type drive struct {
	e     *Engine
	ctx   context.Context
	rec   runtime.RunRecord
	claim runtime.Claim

	report    Report
	conflicts int
	obsFails  int
	// fresh holds the launch intents this drive committed and has not yet
	// executed. Every other unresolved command is found after a restart,
	// takeover or lost response, and is reconciled, never executed again.
	fresh map[string]pendingStart
	// nextWake is the earliest open-wait deadline seen in this pass.
	nextWake time.Time
	// mats are the secret materializations this drive created, by step.
	mats map[domain.StepID]runtime.Materialization
}

func (d *drive) run() (Report, error) {
	for {
		if err := d.ctx.Err(); err != nil {
			return Report{Status: DriveRetry}, err
		}
		f, err := d.next()
		if err != nil {
			if isConflict(err) && d.conflicts < maxConflicts {
				d.conflicts++
				if rerr := d.reload(); rerr != nil {
					return Report{}, rerr
				}
				continue
			}
			return Report{}, err
		}
		if f == flowStop {
			return d.report, nil
		}
	}
}

func isConflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeVersionConflict
}

func (d *drive) reload() error {
	rec, err := d.e.cfg.State.Load(d.ctx, d.rec.Run.Scope, d.rec.Run.ID)
	if err != nil {
		return err
	}
	// Keep the claim the store holds: a commit extends the lease.
	d.rec = rec
	if rec.Claim.Fence == d.claim.Fence && rec.Claim.Owner == d.claim.Owner {
		d.claim = rec.Claim
	}
	d.nextWake = time.Time{}
	return nil
}

// next runs one unit of work chosen from the durable record.
func (d *drive) next() (flow, error) {
	d.nextWake = time.Time{}
	r := d.rec.Run
	switch {
	case r.Terminal() && !(r.Cleanup.State == runtime.CleanupExhausted):
		d.report = Report{Status: DriveTerminal}
		return flowStop, nil
	case r.Settled():
		return d.cleanup()
	case r.CancelRequested:
		return d.cancel()
	}
	return d.execute()
}

// execute progresses one unit of an unsettled, uncancelled run.
func (d *drive) execute() (flow, error) {
	for _, stage := range []func() (flow, error){
		d.stageFailFast, d.stageReconcile, d.stagePrepare, d.stageRelease,
		d.stageRunning, d.stageWaits, d.stageNext, d.stageSettle,
	} {
		f, err := stage()
		if err != nil || f != flowPass {
			return f, err
		}
	}
	return flowStop, fmt.Errorf("runner: run %s has no executable step and is not complete", d.rec.Run.ID)
}

// ---------------------------------------------------------------------------
// Atomic commits
// ---------------------------------------------------------------------------

type pendingEvent struct {
	kind    runtime.EventKind
	step    domain.StepID
	suffix  string
	version domain.Version // zero: the sealed run version
	reason  runtime.Reason
}

// batch collects every change of one transition: the run (mutated through the
// model's own transition methods), its waits, commands, observations and the
// outbox intent. It commits all of it or none of it.
type batch struct {
	run       runtime.Run
	waits     []runtime.Wait
	commands  []runtime.Command
	workloads []runtime.WorkloadObservation
	resources []runtime.ResourceObservation
	events    []pendingEvent
}

func (d *drive) batch() *batch { return &batch{run: d.rec.Run.Clone()} }

// moved records the step's current state as an outbox event.
func (b *batch) moved(key domain.StepID) {
	for _, s := range b.run.Steps {
		if s.Key == key {
			b.events = append(b.events, pendingEvent{kind: runtime.EventStepStateChanged, step: key, version: s.Version, reason: s.Reason})
			return
		}
	}
}

func (b *batch) runChanged(reason runtime.Reason) {
	b.events = append(b.events, pendingEvent{kind: runtime.EventRunStateChanged, reason: reason})
}

func (d *drive) commit(b *batch) error {
	prev := d.rec.Run
	run := b.run
	if run.Version == prev.Version {
		run.Version++
	}
	sealed, err := run.Sealed(prev)
	if err != nil {
		return err
	}
	now := d.e.cfg.Clock.Now()
	t := runtime.Transition{Scope: prev.Scope, RunID: prev.ID, Claim: d.claim, Now: now, ExpectedVersion: prev.Version,
		Run: sealed, Waits: b.waits, Commands: b.commands, Workloads: b.workloads, Resources: b.resources,
		RenewTTL: d.e.cfg.LeaseTTL}
	for _, pe := range b.events {
		v := pe.version
		if v == 0 {
			v = sealed.Version
		}
		id := fmt.Sprintf("%s:%s:%s:%d", pe.kind, prev.ID, pe.step, sealed.Version)
		if pe.suffix != "" {
			id += ":" + pe.suffix
		}
		t.Events = append(t.Events, runtime.Event{ID: id, Scope: prev.Scope, RunID: prev.ID, StepKey: pe.step,
			Kind: pe.kind, Version: v, Reason: pe.reason, At: now})
	}
	rec, err := d.e.cfg.State.Commit(d.ctx, t)
	if err != nil {
		return err
	}
	d.rec, d.claim = rec, rec.Claim
	d.conflicts = 0
	return nil
}

// renew extends the lease without a state change, so a long poll keeps it.
func (d *drive) renew() error {
	c, err := d.e.cfg.State.Renew(d.ctx, d.claim, d.e.cfg.LeaseTTL, d.e.cfg.Clock.Now())
	if err != nil {
		return err
	}
	d.claim = c
	d.rec.Claim = c
	return nil
}

// pause renews the lease and waits one poll interval.
func (d *drive) pause() error {
	if err := d.renew(); err != nil {
		return err
	}
	return d.e.cfg.Sleep(d.ctx, d.e.cfg.PollInterval)
}

// stop ends the drive with a retry status: something inconclusive was left for a
// later claim to reconcile from the durable record.
func (d *drive) stop(status DriveStatus) (flow, error) {
	d.report = Report{Status: status, Next: d.nextWake}
	return flowStop, nil
}

// ---------------------------------------------------------------------------
// Record access
// ---------------------------------------------------------------------------

func (d *drive) planStep(key domain.StepID) runtime.PlanStep {
	for _, p := range d.rec.Plan.Steps {
		if p.Key == key {
			return p
		}
	}
	return runtime.PlanStep{}
}

func (d *drive) step(key domain.StepID) runtime.Step {
	for _, s := range d.rec.Run.Steps {
		if s.Key == key {
			return s
		}
	}
	return runtime.Step{}
}

func (d *drive) command(id string) (runtime.Command, bool) {
	for _, c := range d.rec.Commands {
		if c.ID == id {
			return c, true
		}
	}
	return runtime.Command{}, false
}

func (d *drive) wait(key domain.StepID) (runtime.Wait, bool) {
	for _, w := range d.rec.Waits {
		if w.StepKey == key {
			return w, true
		}
	}
	return runtime.Wait{}, false
}

func (d *drive) now() time.Time { return d.e.cfg.Clock.Now() }
