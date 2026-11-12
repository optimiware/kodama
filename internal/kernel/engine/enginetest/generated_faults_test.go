package enginetest

// Engine-level generated fault schedules. The pure reducer has
// TestGeneratedSequencesPreserveAtMostOneActiveAction; this is its engine
// counterpart. For a fixed set of seeds a small mixed container/request
// execution is driven by randomly chosen engine calls, clock advances,
// controller restarts, runtime events and one-shot testkit fault injections.
// The Kernel invariants of S02 are checked after every applied
// commit, at every start/remove/release effect and after every step. A
// fault-free phase then asserts liveness.
//
// Every failure message carries the seed and the step, and a schedule is a
// pure function of its seed, so a failure is reproduced by running the
// "seed-N" subtest.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// The injected failures. In the fault phase the engine may report only these.
var (
	errScheduleCommit  = errors.New("generated schedule: injected commit failure")
	errSchedulePrepare = errors.New("generated schedule: injected preparation failure")
	errScheduleStart   = errors.New("generated schedule: injected start failure")
	errScheduleRemove  = errors.New("generated schedule: injected removal failure")
)

const (
	// scheduleMinSteps and scheduleMaxSteps bound the length of the fault
	// phase. The seed picks the length, so short schedules end with a live
	// execution and long ones with a settled one.
	scheduleMinSteps = 12
	scheduleMaxSteps = 80
	// scheduleSeeds is the number of consecutive fixed seeds: 1 through
	// scheduleSeeds. scheduleRegressionSeeds are run as well.
	scheduleSeeds = 64
	// scheduleLivenessBound is the number of fault-free Reconcile calls within
	// which every schedule must reach a terminal state with complete cleanup.
	scheduleLivenessBound = 8
)

// scheduleRegressionSeeds are the seeds in 1..60000 that reach a stuck
// cleanup: an identity removed on a never-started proof that only an
// observation supplied, followed by a failed commit or release in the same
// pass. They fail the liveness assertion unless the proof is committed ahead
// of the removal.
var scheduleRegressionSeeds = []int64{
	5891, 7778, 10194, 11741, 12167, 12695, 19249, 19478, 19646,
	22455, 22957, 30209, 30317, 30374, 30921, 31224, 31292, 31418,
	38087, 41329, 46526, 48740, 50916, 51323, 55198, 57431, 57608,
}

// scheduleShapes are the mixed action sequences the seeds rotate through.
func scheduleShapes() [][]definition.ActionKind {
	c := definition.ActionKindContainer
	r := definition.ActionKindRequest
	return [][]definition.ActionKind{
		{c, r, c},
		{r, c},
		{c, c},
		{c, r},
		{r, c, r},
		{c, c, r},
	}
}

// ---------------------------------------------------------------------------
// Runtime and workspace seams
// ---------------------------------------------------------------------------

// exitedRuntime makes a workload exit stick. Once the schedule lets a workload
// exit, every later observation reports that exit, as a real runtime would,
// even when the engine lost the commit that recorded the first report.
type exitedRuntime struct {
	engine.Runtime
	exits map[id.WorkloadID]kernel.Observation
}

func (r *exitedRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if exit, ok := r.exits[request.Identity.WorkloadID]; ok {
		return exit, nil
	}
	return r.Runtime.Observe(ctx, request)
}

// releaseProbeWorkspace runs a hook before every release attempt, so the
// release precondition is checked at the effect boundary and independently of
// the fixture's own release guard.
type releaseProbeWorkspace struct {
	*testkit.Workspace
	attempts      int
	beforeRelease func()
}

func (w *releaseProbeWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	w.attempts++
	w.beforeRelease()
	return w.Workspace.Release(ctx, workspaceID)
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

// faultSchedule is one seeded run: the subject under test, the fault seams and
// the oracle state the invariants are checked against.
type faultSchedule struct {
	t    *testing.T
	seed int64
	rng  *rand.Rand
	// steps is the length of the fault phase; step and what name the step in
	// progress for failure messages.
	steps int
	step  int
	what  string

	h         *harness
	log       *commitLog
	probe     *probeRuntime
	exited    *exitedRuntime
	workspace *releaseProbeWorkspace
	policy    engine.CleanupPolicy

	version        uint64
	actionVersions map[id.ActionID]uint64
	terminal       *kernel.Execution
	settled        map[id.ActionID]kernel.Action
	deadlines      map[id.ActionID]time.Time
	subjects       map[id.ActionID]map[string]definition.SubjectValue
	staged         map[id.ActionID]kernel.Outputs
	identities     map[id.ActionID]id.WorkloadID
	owners         map[id.WorkloadID]id.ActionID
	retiring       map[id.WorkloadID]bool
	neverStarted   map[id.WorkloadID]int
	cleanup        kernel.Cleanup

	// reconciles counts the fault-free Reconcile calls; ending is how a live
	// execution was ended, exhausted whether cleanup hit its retry bound and
	// lateAnswer whether an answer at the deadline was refused.
	reconciles int
	ending     string
	exhausted  bool
	lateAnswer bool
}

func newFaultSchedule(t *testing.T, seed int64, kinds []definition.ActionKind) *faultSchedule {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	actions := make([]kernel.Action, len(kinds))
	for index, kind := range kinds {
		actionID := fmt.Sprintf("act-%d", index)
		name := fmt.Sprintf("a%d", index)
		if kind == definition.ActionKindContainer {
			actions[index] = containerAction(actionID, name, index, time.Hour, "out")
			continue
		}
		// Some requests need an output of the preceding container, so an
		// unstaged output fails the activation closed.
		fields := map[string]definition.SubjectValue{"repository": inputReference("repository")}
		if index > 0 && kinds[index-1] == definition.ActionKindContainer && rng.Intn(2) == 0 {
			fields["candidate"] = actionReference(fmt.Sprintf("a%d", index-1), "out")
		}
		actions[index] = requestAction(actionID, name, index, time.Hour, definition.NewSubject(fields), "decision")
	}

	s := &faultSchedule{
		t:              t,
		seed:           seed,
		rng:            rng,
		steps:          scheduleMinSteps + rng.Intn(scheduleMaxSteps-scheduleMinSteps+1),
		what:           "setup",
		h:              newHarness(t, newExecution(fmt.Sprintf("exec-generated-%d", seed), fmt.Sprintf("ws-generated-%d", seed), actions...)),
		policy:         engine.CleanupPolicy{MaxAttempts: 1 + rng.Intn(engine.DefaultCleanupMaxAttempts)},
		actionVersions: make(map[id.ActionID]uint64),
		settled:        make(map[id.ActionID]kernel.Action),
		deadlines:      make(map[id.ActionID]time.Time),
		subjects:       make(map[id.ActionID]map[string]definition.SubjectValue),
		staged:         make(map[id.ActionID]kernel.Outputs),
		identities:     make(map[id.ActionID]id.WorkloadID),
		owners:         make(map[id.WorkloadID]id.ActionID),
		retiring:       make(map[id.WorkloadID]bool),
		neverStarted:   make(map[id.WorkloadID]int),
	}
	s.log = newCommitLog(s.h.fixture.State)
	s.log.afterCommit = func(_ flow.Commit, updated kernel.Execution) {
		if updated.Version != s.version+1 {
			s.failf("commit produced execution version %d after %d; versions must increase by one", updated.Version, s.version)
		}
		s.checkState(updated)
	}
	s.exited = &exitedRuntime{Runtime: s.h.runtime, exits: make(map[id.WorkloadID]kernel.Observation)}
	s.probe = newProbeRuntime(s.exited)
	s.probe.beforeStart = s.checkStart
	s.probe.beforeRemove = s.checkRemove
	s.workspace = &releaseProbeWorkspace{Workspace: s.h.fixture.Workspace, beforeRelease: s.checkRelease}
	s.newEngine()
	return s
}

// newEngine builds a controller over the current State handle. The cleanup
// policy is installation configuration, so every controller re-applies it.
func (s *faultSchedule) newEngine() {
	s.h.engine = engine.New(s.log, s.probe, s.workspace, s.h.fixture.Clock)
	s.h.engine.SetCleanupPolicy(s.policy)
}

func (s *faultSchedule) failf(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf("seed %d step %d (%s): %s", s.seed, s.step, s.what, fmt.Sprintf(format, args...))
}

func (s *faultSchedule) load() kernel.Execution {
	s.t.Helper()
	execution, err := s.h.fixture.State.Load(s.t.Context(), s.h.executionID)
	if err != nil {
		s.failf("Load() error = %v", err)
	}
	return execution
}

func (s *faultSchedule) now() time.Time {
	return s.h.fixture.Clock.Now()
}

// current returns the running action, if any.
func (s *faultSchedule) current(execution kernel.Execution) (kernel.Action, bool) {
	if execution.CurrentAction < 0 || execution.CurrentAction >= len(execution.Actions) {
		return kernel.Action{}, false
	}
	return execution.Actions[execution.CurrentAction], true
}

// materialized returns the runtime record of every identity the engine
// prepared successfully, keyed by identity.
func (s *faultSchedule) materialized() map[id.WorkloadID]testkit.Workload {
	records := make(map[id.WorkloadID]testkit.Workload)
	for _, call := range s.probe.snapshot() {
		if record, ok := s.h.fixture.Runtime.Workload(call.workload); ok && call.kind == "prepare" {
			records[call.workload] = record
		}
	}
	return records
}

// ---------------------------------------------------------------------------
// Invariants
// ---------------------------------------------------------------------------

// checkState checks one durable revision against the S02 §2/§3
// invariants: at most one active action, ordered settlement, immutable
// terminal outcomes and outputs, stable identities and deadlines, and a
// cleanup record that never claims more than the evidence.
func (s *faultSchedule) checkState(execution kernel.Execution) {
	s.t.Helper()
	if execution.Version < s.version {
		s.failf("execution version went back from %d to %d", s.version, execution.Version)
	}
	s.version = execution.Version

	running := -1
	blocked := kernel.ActionState("")
	for index, action := range execution.Actions {
		if action.Version < s.actionVersions[action.ID] {
			s.failf("action %q version went back from %d to %d", action.Name, s.actionVersions[action.ID], action.Version)
		}
		s.actionVersions[action.ID] = action.Version

		// Only success advances; a failure or cancellation cancels every
		// pending successor in the same commit.
		switch blocked {
		case "":
		case kernel.ActionPending, kernel.ActionRunning:
			if action.State != kernel.ActionPending {
				s.failf("action %q is %q behind an unsettled predecessor: %s", action.Name, action.State, actionSummary(execution))
			}
		default:
			if action.State != kernel.ActionCancelled {
				s.failf("action %q is %q behind a %s predecessor, want cancelled: %s", action.Name, action.State, blocked, actionSummary(execution))
			}
		}
		if blocked == "" && action.State != kernel.ActionSucceeded {
			blocked = action.State
		}
		if action.State == kernel.ActionRunning {
			if running >= 0 {
				s.failf("more than one action is active: %s", actionSummary(execution))
			}
			running = index
		}
		s.checkAction(action)
	}
	if execution.CurrentAction != running {
		s.failf("CurrentAction = %d but the running action index is %d: %s", execution.CurrentAction, running, actionSummary(execution))
	}

	switch execution.State {
	case kernel.ExecutionQueued:
		if len(execution.Actions) > 0 && blocked != kernel.ActionPending {
			s.failf("queued execution has activated work: %s", actionSummary(execution))
		}
	case kernel.ExecutionRunning:
		if blocked != kernel.ActionPending && blocked != kernel.ActionRunning {
			s.failf("running execution has no pending or active action: %s", actionSummary(execution))
		}
	case kernel.ExecutionSucceeded:
		if blocked != "" {
			s.failf("succeeded execution has an action that did not succeed: %s", actionSummary(execution))
		}
	default:
		if blocked != kernel.ActionFailed && blocked != kernel.ActionCancelled {
			s.failf("%s execution has no failed or cancelled action: %s", execution.State, actionSummary(execution))
		}
	}
	if (execution.Failure != nil) != (execution.State == kernel.ExecutionFailed) {
		s.failf("state %q carries failure %+v", execution.State, execution.Failure)
	}
	if s.terminal != nil {
		if execution.State != s.terminal.State || !reflect.DeepEqual(execution.Failure, s.terminal.Failure) {
			s.failf("terminal outcome %q/%+v changed to %q/%+v", s.terminal.State, s.terminal.Failure, execution.State, execution.Failure)
		}
	} else if execution.State.Terminal() {
		settled := execution
		s.terminal = &settled
	}
	if execution.State.Terminal() && !s.h.fixture.Store.CredentialInvalidated(execution.ID) {
		s.failf("terminal execution %q kept its execution authority", execution.State)
	}

	s.checkCleanup(execution)
}

func (s *faultSchedule) checkAction(action kernel.Action) {
	s.t.Helper()
	// Terminal action outcomes and promoted outputs never change.
	if settled, ok := s.settled[action.ID]; ok {
		if action.State != settled.State {
			s.failf("settled action %q changed from %q to %q", action.Name, settled.State, action.State)
		}
		if !reflect.DeepEqual(action.Outputs, settled.Outputs) {
			s.failf("settled outputs of %q changed from %+v to %+v", action.Name, settled.Outputs, action.Outputs)
		}
		if action.Request != nil && !reflect.DeepEqual(action.Request.Answer, settled.Request.Answer) {
			s.failf("recorded answer of %q changed from %+v to %+v", action.Name, settled.Request.Answer, action.Request.Answer)
		}
	} else if action.State.Terminal() {
		s.checkSettlement(action)
		s.settled[action.ID] = action
	}
	if action.State != kernel.ActionSucceeded && action.Outputs != nil {
		s.failf("%s action %q exposes outputs %+v", action.State, action.Name, action.Outputs)
	}

	// Staging belongs to the running container and is immutable once accepted.
	if action.StagedOutputsSet || action.StagedOutputs != nil {
		if action.State != kernel.ActionRunning || action.Container == nil {
			s.failf("%s action %q holds staged outputs %+v", action.State, action.Name, action.StagedOutputs)
		}
		if staged, ok := s.staged[action.ID]; ok && !reflect.DeepEqual(staged, action.StagedOutputs) {
			s.failf("staged outputs of %q were replaced: %+v became %+v", action.Name, staged, action.StagedOutputs)
		}
		s.staged[action.ID] = action.StagedOutputs
	}

	// The deadline is persisted at activation and never recalculated.
	if recorded, ok := s.deadlines[action.ID]; ok {
		if !action.Deadline.Equal(recorded) {
			s.failf("deadline of %q changed from %s to %s", action.Name, recorded, action.Deadline)
		}
	} else if !action.Deadline.IsZero() {
		if action.State == kernel.ActionPending || !action.Deadline.Equal(action.ActivatedAt.Add(action.Timeout)) {
			s.failf("%s action %q has deadline %s for activation %s", action.State, action.Name, action.Deadline, action.ActivatedAt)
		}
		s.deadlines[action.ID] = action.Deadline
	}
	if action.State == kernel.ActionRunning && action.Deadline.IsZero() {
		s.failf("running action %q has no persisted deadline", action.Name)
	}

	if action.Request != nil {
		s.checkRequest(action)
	}
	if action.Container != nil {
		s.checkIdentity(action)
	}
}

// checkSettlement checks the commit that made one action terminal.
func (s *faultSchedule) checkSettlement(action kernel.Action) {
	s.t.Helper()
	if action.State != kernel.ActionSucceeded {
		return
	}
	if action.Request != nil {
		answer := action.Request.Answer
		if answer == nil || answer.Decision != kernel.DecisionPass || !reflect.DeepEqual(action.Outputs, answer.Outputs) {
			s.failf("request %q succeeded with outputs %+v and answer %+v", action.Name, action.Outputs, answer)
		}
		return
	}
	// A container succeeds only on a confirmed zero exit and promotes exactly
	// the accepted staging.
	observation := action.Container.Observation
	if code, ok := observation.ExitStatus(); !ok || code != 0 || !observation.StopConfirmed() || !action.Stop.IsConfirmed() {
		s.failf("container %q succeeded on observation %+v and stop %+v", action.Name, observation, action.Stop)
	}
	if staged, ok := s.staged[action.ID]; ok {
		if !reflect.DeepEqual(action.Outputs, staged) {
			s.failf("container %q promoted %+v, want the staged %+v", action.Name, action.Outputs, staged)
		}
	} else if action.Outputs != nil {
		s.failf("container %q promoted %+v without a submission", action.Name, action.Outputs)
	}
}

func (s *faultSchedule) checkRequest(action kernel.Action) {
	s.t.Helper()
	request := action.Request
	if (action.State == kernel.ActionPending && request.Subject != nil) || (action.State == kernel.ActionRunning && request.Subject == nil) {
		s.failf("%s request %q has subject %v", action.State, action.Name, request.Subject)
	}
	if request.Subject != nil {
		fields := request.Subject.Fields()
		if recorded, ok := s.subjects[action.ID]; ok && !reflect.DeepEqual(recorded, fields) {
			s.failf("resolved subject of %q changed from %+v to %+v", action.Name, recorded, fields)
		}
		s.subjects[action.ID] = fields
	}
	if request.Answer != nil && action.State != kernel.ActionSucceeded && action.State != kernel.ActionFailed {
		s.failf("%s request %q carries answer %+v", action.State, action.Name, request.Answer)
	}
}

// checkIdentity checks the workload identity of one container action: it is
// persisted not_issued before any effect, never reused, replaced only after
// retirement, and a never-started proof is never contradicted by the runtime.
func (s *faultSchedule) checkIdentity(action kernel.Action) {
	s.t.Helper()
	container := action.Container
	workload := container.WorkloadID
	if workload == "" {
		if action.State == kernel.ActionRunning || container.Launch != (kernel.LaunchIntent{}) {
			s.failf("%s container %q has launch %+v without an identity", action.State, action.Name, container.Launch)
		}
		return
	}
	if container.Launch.WorkloadID != workload || container.Launch.IdempotencyKey == "" {
		s.failf("container %q identity %q disagrees with launch %+v", action.Name, workload, container.Launch)
	}
	if previous, known := s.identities[action.ID]; !known || previous != workload {
		if known && !s.retiring[previous] {
			s.failf("container %q replaced identity %q with %q without retiring it", action.Name, previous, workload)
		}
		if owner, reused := s.owners[workload]; reused {
			s.failf("container %q reuses identity %q of action %q", action.Name, workload, owner)
		}
		if container.Launch.State != kernel.LaunchNotIssued {
			s.failf("container %q first persisted identity %q as %q, want not_issued", action.Name, workload, container.Launch.State)
		}
		s.identities[action.ID] = workload
		s.owners[workload] = action.ID
	}

	proven := container.Observation.ProofOfNonexecution()
	if container.Launch.State == kernel.LaunchRetiring {
		if !proven {
			s.failf("container %q retires %q without a persisted proof: %+v", action.Name, workload, container.Observation)
		}
		s.retiring[workload] = true
	} else if s.retiring[workload] {
		s.failf("retiring identity %q of %q became %q", workload, action.Name, container.Launch.State)
	}
	if _, marked := s.neverStarted[workload]; proven && !marked {
		s.neverStarted[workload] = len(s.probe.snapshot())
		if record, ok := s.h.fixture.Runtime.Workload(workload); ok && record.Started {
			s.failf("identity %q of %q is persisted as never started but the runtime started it", workload, action.Name)
		}
	}
}

// checkCleanup checks the separate cleanup record: it exists only for a
// terminal execution, never regresses, and is complete only when every owned
// identity is stopped or never started, removed, and the workspace released.
func (s *faultSchedule) checkCleanup(execution kernel.Execution) {
	s.t.Helper()
	cleanup := execution.Cleanup
	recorded := cleanup.State == kernel.CleanupPending || cleanup.State == kernel.CleanupComplete
	if recorded && !execution.State.Terminal() {
		s.failf("%s execution carries cleanup %+v", execution.State, cleanup)
	}
	regressed := (s.cleanup.State == kernel.CleanupComplete && !reflect.DeepEqual(cleanup, s.cleanup)) ||
		(s.cleanup.State == kernel.CleanupPending && !recorded) ||
		cleanup.Attempts < s.cleanup.Attempts
	if regressed {
		s.failf("cleanup regressed from %+v to %+v", s.cleanup, cleanup)
	}
	s.cleanup = cleanup
	if s.h.fixture.Workspace.IsReleased(s.h.workspaceID) && !execution.State.Terminal() {
		s.failf("workspace was released while the execution is %q", execution.State)
	}
	if cleanup.State != kernel.CleanupComplete {
		return
	}

	for _, action := range execution.Actions {
		if action.Container == nil || action.Container.WorkloadID == "" || action.Container.Launch.State == kernel.LaunchNotIssued {
			continue
		}
		record, prepared := s.h.fixture.Runtime.Workload(action.Container.WorkloadID)
		if prepared && record.Started && !action.Stop.IsConfirmed() {
			s.failf("cleanup is complete but started identity %q of %q has stop evidence %+v", action.Container.WorkloadID, action.Name, action.Stop)
		}
	}
	for workload, record := range s.materialized() {
		if (record.Started && !record.StopConfirmed) || !record.Removed {
			s.failf("cleanup is complete but workload %q is %+v", workload, record)
		}
	}
	if s.h.fixture.Workspace.IsRetained(s.h.workspaceID) {
		s.failf("cleanup is complete but the workspace is still retained")
	}
}

// checkStart runs before every Start: only the durably issued identity of the
// running action may start, and never a second time.
func (s *faultSchedule) checkStart(request engine.StartRequest) {
	execution := s.load()
	action, ok := s.current(execution)
	if !ok || action.Container == nil ||
		action.Container.WorkloadID != request.Identity.WorkloadID ||
		action.Container.Launch.State != kernel.LaunchIssued ||
		action.Container.Launch.IdempotencyKey != request.Identity.IdempotencyKey {
		s.failf("Start(%q) without a durable issued launch of the running action: %s", request.Identity.WorkloadID, actionSummary(execution))
	}
	if record, ok := s.h.fixture.Runtime.Workload(request.Identity.WorkloadID); !ok || record.Started || record.Removed {
		s.failf("Start(%q) on runtime workload %+v (prepared=%v)", request.Identity.WorkloadID, record, ok)
	}
}

// checkRemove runs before every Remove: a started workload needs a confirmed
// stop, and a live execution removes only its retiring identity.
func (s *faultSchedule) checkRemove(request engine.RemoveRequest) {
	workload := request.Identity.WorkloadID
	if record, ok := s.h.fixture.Runtime.Workload(workload); ok && record.Started && !record.StopConfirmed {
		s.failf("Remove(%q) before its stop is confirmed: %+v", workload, record)
	}
	execution := s.load()
	if execution.State.Terminal() {
		return
	}
	action, ok := s.current(execution)
	if !ok || action.Container == nil || action.Container.WorkloadID != workload ||
		action.Container.Launch.State != kernel.LaunchRetiring || !action.Container.Observation.ProofOfNonexecution() {
		s.failf("Remove(%q) in a live execution without a durable retirement: %s", workload, actionSummary(execution))
	}
}

// checkRelease runs before every workspace release attempt: the execution is
// terminal and no materialized workload is possibly running or still present.
func (s *faultSchedule) checkRelease() {
	if execution := s.load(); !execution.State.Terminal() {
		s.failf("workspace release attempted while the execution is %q", execution.State)
	}
	for workload, record := range s.materialized() {
		if (record.Started && !record.StopConfirmed) || !record.Removed {
			s.failf("workspace release attempted while workload %q is %+v", workload, record)
		}
	}
}

// checkEffects checks the ordered runtime calls: no identity is started twice,
// every effect names a persisted identity, and an identity with a durable
// never-started proof is never prepared, started, observed or stopped again.
func (s *faultSchedule) checkEffects() {
	s.t.Helper()
	starts := make(map[id.WorkloadID]int)
	for index, call := range s.probe.snapshot() {
		if _, persisted := s.owners[call.workload]; !persisted {
			s.failf("%s effect on identity %q, which was never persisted", call.kind, call.workload)
		}
		if call.kind == "start" {
			if starts[call.workload]++; starts[call.workload] > 1 {
				s.failf("identity %q was started more than once", call.workload)
			}
		}
		if mark, ok := s.neverStarted[call.workload]; ok && index >= mark && call.kind != "remove" {
			s.failf("%s effect on never-started identity %q after its proof was persisted", call.kind, call.workload)
		}
	}
	for workload := range s.owners {
		record, ok := s.h.fixture.Runtime.Workload(workload)
		if !ok {
			continue
		}
		if record.StartCount > 1 {
			s.failf("runtime applied %d starts to identity %q", record.StartCount, workload)
		}
		if _, proven := s.neverStarted[workload]; proven && record.Started {
			s.failf("never-started identity %q was started", workload)
		}
	}
}

// afterStep re-reads the durable state and checks every invariant.
func (s *faultSchedule) afterStep() {
	s.t.Helper()
	s.checkState(s.load())
	s.checkEffects()
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

type scheduleStep struct {
	name   string
	weight int
	run    func(*faultSchedule)
}

func scheduleStepTable() []scheduleStep {
	return []scheduleStep{
		{"reconcile", 92, (*faultSchedule).stepReconcile},
		{"scan", 24, (*faultSchedule).stepScan},
		{"workload exits", 44, (*faultSchedule).stepExit},
		{"answer", 32, (*faultSchedule).stepAnswer},
		{"answer at the deadline", 2, (*faultSchedule).stepLateAnswer},
		{"submit outputs", 24, (*faultSchedule).stepSubmit},
		{"cancel", 2, (*faultSchedule).stepCancel},
		{"advance clock", 14, func(s *faultSchedule) {
			s.h.fixture.Clock.Advance(time.Duration(1+s.rng.Intn(10)) * time.Minute)
		}},
		{"advance clock past the deadline", 1, func(s *faultSchedule) {
			s.h.fixture.Clock.Advance(time.Hour + time.Minute)
		}},
		{"restart", 20, func(s *faultSchedule) {
			s.log.rebind(s.h.fixture.Restart())
			s.newEngine()
		}},
		{"fail next commit", 24, func(s *faultSchedule) {
			s.h.fixture.Store.FailNextCommit(errScheduleCommit)
		}},
		{"lose next start response", 10, func(s *faultSchedule) {
			s.h.fixture.Runtime.LoseNextStartResponse("start response lost after the effect")
		}},
		{"prove next start not started", 12, func(s *faultSchedule) {
			s.h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")
		}},
		{"fail next start before the effect", 8, func(s *faultSchedule) {
			s.h.fixture.Runtime.FailNextStartBeforeEffect(errScheduleStart)
		}},
		{"fail next prepare", 4, func(s *faultSchedule) {
			s.h.fixture.Runtime.FailNextPrepare(errSchedulePrepare)
		}},
		{"lose next stop response", 10, func(s *faultSchedule) {
			s.h.fixture.Runtime.LoseNextStopResponse("stop response lost after the request")
		}},
		{"confirm requested stop", 16, (*faultSchedule).confirmRequestedStop},
		{"fail next remove", 16, func(s *faultSchedule) {
			s.h.fixture.Runtime.FailNextRemove(errScheduleRemove)
		}},
		{"expire lease", 6, (*faultSchedule).stepExpireLease},
		{"ambiguous observation", 12, (*faultSchedule).stepAmbiguousObservation},
		{"lose workspace", 1, func(s *faultSchedule) {
			s.h.fixture.Workspace.Lose(s.h.workspaceID)
		}},
	}
}

// expectInjected fails unless err is nil or an injected failure the engine
// reports to its caller: a failed commit or a failed removal of a retiring
// identity. Preparation and start failures become durable outcomes instead.
func (s *faultSchedule) expectInjected(call string, err error) {
	s.t.Helper()
	if err == nil || errors.Is(err, errScheduleCommit) || errors.Is(err, errScheduleRemove) {
		return
	}
	s.failf("%s error = %v, want nil or an injected failure", call, err)
}

// expectRefusal fails unless a command was refused with one of the allowed
// fault classes, or by the injected commit failure, and left no trace.
func (s *faultSchedule) expectRefusal(call string, err error, before kernel.Execution, allowed ...fault.Class) {
	s.t.Helper()
	if after := s.load(); after.Version != before.Version {
		s.failf("%s error = %v but it committed version %d over %d", call, err, after.Version, before.Version)
	}
	if errors.Is(err, errScheduleCommit) {
		return
	}
	class, ok := fault.ClassOf(err)
	for _, candidate := range allowed {
		if ok && class == candidate {
			return
		}
	}
	s.failf("%s error = %v (class %q, %v), want one of %v", call, err, class, ok, allowed)
}

func (s *faultSchedule) stepReconcile() {
	_, err := s.h.engine.Reconcile(s.t.Context(), s.h.executionID)
	s.expectInjected("Reconcile", err)
}

func (s *faultSchedule) stepScan() {
	s.expectInjected("Scan", s.h.engine.Scan(s.t.Context()))
}

func (s *faultSchedule) stepCancel() {
	before := s.load()
	err := s.h.engine.Cancel(s.t.Context(), s.h.executionID, flow.CancelRequested)
	if err != nil {
		s.expectRefusal("Cancel", err, before, fault.ChangedIdempotencyContent)
		return
	}
	if after := s.load(); after.State != kernel.ExecutionCancelled {
		s.failf("Cancel succeeded but the execution is %q", after.State)
	}
}

// stepAnswer settles a random action as the authorized actor. Only the running
// request may accept an answer, and only strictly before its deadline.
func (s *faultSchedule) stepAnswer() {
	before := s.load()
	index := s.rng.Intn(len(before.Actions))
	action := before.Actions[index]
	answer := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"decision": "approved"},
		Actor:    id.PrincipalID("principal-1"),
		At:       s.now(),
	}
	if s.rng.Intn(4) == 0 {
		answer.Decision = kernel.DecisionFail
		answer.Outputs = kernel.Outputs{"decision": "rejected"}
	}
	err := s.h.engine.SettleRequest(s.t.Context(), s.h.executionID, action.ID, answer)
	if err != nil {
		s.expectRefusal("SettleRequest", err, before, fault.InvalidInput, fault.ChangedIdempotencyContent, fault.DeadlineExpired)
		return
	}
	if action.Request == nil {
		s.failf("SettleRequest accepted an answer for container %q", action.Name)
	}
	recorded := s.load().Actions[index].Request.Answer
	if recorded == nil || recorded.Decision != answer.Decision || !reflect.DeepEqual(recorded.Outputs, answer.Outputs) || recorded.Actor != answer.Actor {
		s.failf("SettleRequest succeeded but the recorded answer is %+v, want %+v", recorded, answer)
	}
	if action.Request.Answer == nil && (action.State != kernel.ActionRunning || !s.now().Before(action.Deadline)) {
		s.failf("SettleRequest accepted an answer for %s action %q at %s with deadline %s", action.State, action.Name, s.now(), action.Deadline)
	}
}

// stepLateAnswer moves the clock onto the running request's persisted deadline
// and answers it. At equality the answer is late (S02 §3): it is refused as
// deadline_expired and leaves no trace, and the next pass expires the request.
func (s *faultSchedule) stepLateAnswer() {
	before := s.load()
	action, ok := s.current(before)
	if !ok || action.Request == nil {
		return
	}
	if s.now().Before(action.Deadline) {
		s.h.fixture.Clock.Set(action.Deadline)
	}
	// The sender timestamp claims a timely answer; only the authoritative
	// clock counts.
	answer := kernel.Answer{Decision: kernel.DecisionPass, Outputs: kernel.Outputs{"decision": "approved"}, Actor: "principal-1", At: action.ActivatedAt}
	err := s.h.engine.SettleRequest(s.t.Context(), s.h.executionID, action.ID, answer)
	if err == nil {
		s.failf("SettleRequest accepted an answer for %q at %s with deadline %s", action.Name, s.now(), action.Deadline)
	}
	s.expectRefusal("SettleRequest at the deadline", err, before, fault.DeadlineExpired)
	s.lateAnswer = true
}

// stepSubmit submits one of two output values for a random action, so both
// identical retries and conflicting replacements occur.
func (s *faultSchedule) stepSubmit() {
	before := s.load()
	index := s.rng.Intn(len(before.Actions))
	action := before.Actions[index]
	value := []string{"v1", "v2"}[s.rng.Intn(2)]
	err := s.h.engine.SubmitOutputs(s.t.Context(), s.h.executionID, action.ID, kernel.Outputs{"out": value})
	if err != nil {
		s.expectRefusal("SubmitOutputs", err, before, fault.InvalidInput, fault.ChangedIdempotencyContent)
		return
	}
	after := s.load().Actions[index]
	staged := after.StagedOutputsSet && after.StagedOutputs["out"] == value
	settled := after.State == kernel.ActionSucceeded && after.Outputs["out"] == value
	if after.Container == nil || (!staged && !settled) {
		s.failf("SubmitOutputs(%q) succeeded but %q holds staged %+v and settled %+v", value, action.Name, after.StagedOutputs, after.Outputs)
	}
}

// startedWorkload returns the running container's workload when the runtime
// applied its start and still holds it.
func (s *faultSchedule) startedWorkload() (engine.EffectIdentity, bool) {
	action, ok := s.current(s.load())
	if !ok || action.Container == nil {
		return engine.EffectIdentity{}, false
	}
	record, prepared := s.h.fixture.Runtime.Workload(action.Container.WorkloadID)
	return identityFor(s.h.executionID, action), prepared && record.Started && !record.Removed
}

// exit lets a started workload exit. A confirmed exit carries real termination
// evidence in the runtime; an unconfirmed one is only an acknowledgment.
func (s *faultSchedule) exit(identity engine.EffectIdentity, code int, confirmed bool) {
	s.t.Helper()
	if !confirmed {
		s.exited.exits[identity.WorkloadID] = kernel.ExitedObservation(code, kernel.PendingStop("stop acknowledged; termination is not confirmed"))
		return
	}
	s.h.fixture.Runtime.ConfirmNextStop("workload exited")
	evidence, err := s.h.fixture.Runtime.Stop(s.t.Context(), engine.StopRequest{Identity: identity, WorkspaceID: s.h.workspaceID})
	if err != nil || !evidence.IsConfirmed() {
		s.failf("runtime stop of %q = %+v, %v, want confirmed", identity.WorkloadID, evidence, err)
	}
	s.exited.exits[identity.WorkloadID] = kernel.ExitedObservation(code, kernel.ConfirmedStop("workload exited"))
}

// stepExit lets the running workload exit: usually zero with a confirmed
// stop, sometimes nonzero, sometimes with a stop that is only acknowledged. A
// later exit step confirms an acknowledged stop; the exit status never changes.
func (s *faultSchedule) stepExit() {
	variant := s.rng.Intn(10)
	identity, started := s.startedWorkload()
	if !started {
		return
	}
	if exit, exited := s.exited.exits[identity.WorkloadID]; exited {
		if !exit.StopConfirmed() {
			s.exit(identity, *exit.ExitCode, true)
		}
		return
	}
	switch {
	case variant < 7:
		s.exit(identity, 0, true)
	case variant < 8:
		s.exit(identity, 1, true)
	default:
		s.exit(identity, 0, false)
	}
}

// confirmRequestedStop lets the runtime confirm termination on the next stop
// request, but only for a started workload whose stop the engine has already
// requested. The first request is therefore always a mere acknowledgment.
func (s *faultSchedule) confirmRequestedStop() {
	for _, record := range s.materialized() {
		if record.Started && !record.StopConfirmed && record.StopRequests > 0 {
			s.h.fixture.Runtime.ConfirmNextStop("termination observed")
			return
		}
	}
}

func (s *faultSchedule) stepExpireLease() {
	if identity, started := s.startedWorkload(); started {
		s.h.fixture.Runtime.ExpireLease(identity.WorkloadID)
	}
}

// stepAmbiguousObservation stages evidence that proves neither nonexecution
// nor stop for the running container's identity.
func (s *faultSchedule) stepAmbiguousObservation() {
	observation := kernel.AbsentObservation()
	if s.rng.Intn(2) == 0 {
		observation = kernel.UnknownObservation()
	}
	if action, ok := s.current(s.load()); ok && action.Container != nil {
		s.h.fixture.Runtime.QueueObservation(action.Container.WorkloadID, observation)
	}
}

// ---------------------------------------------------------------------------
// Fault-free phase
// ---------------------------------------------------------------------------

// disarm consumes every one-shot fault that is still armed, so the fault-free
// phase starts with nothing pending. It commits against an unknown execution
// and drives two scratch workloads in a scratch workspace through the runtime
// fake; neither touches the execution under test.
func (s *faultSchedule) disarm() {
	ctx := s.t.Context()
	runtime := s.h.fixture.Runtime
	_, _ = s.h.fixture.State.Commit(ctx, flow.Commit{ExecutionID: "exec-disarm"})

	idle := engine.EffectIdentity{ExecutionID: "exec-disarm", ActionID: "act-disarm", WorkloadID: "wl-disarm-idle", IdempotencyKey: "disarm-idle"}
	live := engine.EffectIdentity{ExecutionID: "exec-disarm", ActionID: "act-disarm", WorkloadID: "wl-disarm-live", IdempotencyKey: "disarm-live"}
	// The first prepare consumes a failed preparation.
	for _, identity := range []engine.EffectIdentity{idle, idle, live} {
		_, _ = runtime.Prepare(ctx, engine.PrepareRequest{Identity: identity, WorkspaceID: "ws-disarm"})
	}
	// A failed start, a refused start and a lost start response are consumed
	// by at most three start requests.
	for range 3 {
		_, _ = runtime.Start(ctx, engine.StartRequest{Identity: live, WorkspaceID: "ws-disarm"})
	}
	// A staged confirmation is consumed by the first stop request and a lost
	// stop response by the first one that is not confirmed.
	for _, identity := range []engine.EffectIdentity{idle, live} {
		_, _ = runtime.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: "ws-disarm"})
	}
	// The first removal consumes a failed removal.
	for range 2 {
		_, _ = runtime.Remove(ctx, engine.RemoveRequest{Identity: idle, WorkspaceID: "ws-disarm"})
	}
	removed, _ := runtime.Workload(idle.WorkloadID)
	started, _ := runtime.Workload(live.WorkloadID)
	if !removed.Removed || !started.Started || started.StopRequests == 0 {
		s.failf("scratch workloads after disarming = %+v and %+v, want one removed and one started and stopped", removed, started)
	}
}

// settle stops injecting faults, lets stops and removals succeed and asserts
// liveness: within scheduleLivenessBound Reconcile calls the execution is
// terminal and its cleanup complete. A live execution is ended by the seed's
// choice of completion, cancellation or deadline expiry.
func (s *faultSchedule) settle() {
	s.step++
	s.what = "disarm"
	s.disarm()
	for workload, exit := range s.exited.exits {
		if !exit.StopConfirmed() {
			s.exit(engine.EffectIdentity{ExecutionID: s.h.executionID, WorkloadID: workload}, *exit.ExitCode, true)
		}
	}
	ending := []string{"finish", "cancel", "expire"}[s.rng.Intn(3)]
	if !s.load().State.Terminal() {
		s.ending = ending
	}

	for {
		s.what = fmt.Sprintf("fault-free Reconcile %d, ending by %s", s.reconciles+1, ending)
		execution := s.load()
		if execution.State.Terminal() && execution.Cleanup.State == kernel.CleanupComplete {
			break
		}
		if s.reconciles >= scheduleLivenessBound {
			s.failf("no terminal state with complete cleanup within %d fault-free Reconcile calls: %s",
				scheduleLivenessBound, actionSummary(execution))
		}
		switch {
		case !execution.State.Terminal():
			s.end(ending, execution)
		case execution.Cleanup.Attempts >= s.policy.MaxAttempts:
			s.assertCleanupExhausted(execution)
		}
		s.confirmRequestedStop()
		s.reconcile()
		s.afterStep()
	}
	s.assertQuiescent()
}

// reconcile is one fault-free Reconcile call, which must not fail.
func (s *faultSchedule) reconcile() {
	s.t.Helper()
	s.reconciles++
	if _, err := s.h.engine.Reconcile(s.t.Context(), s.h.executionID); err != nil {
		s.failf("fault-free Reconcile error = %v", err)
	}
}

// end moves a live execution toward a terminal state.
func (s *faultSchedule) end(ending string, execution kernel.Execution) {
	s.t.Helper()
	switch ending {
	case "cancel":
		if err := s.h.engine.Cancel(s.t.Context(), s.h.executionID, flow.CancelRequested); err != nil {
			s.failf("fault-free Cancel error = %v", err)
		}
		return
	case "expire":
		s.h.fixture.Clock.Advance(2 * time.Hour)
		return
	}

	// Completion: answer the waiting request, or let the started workload
	// exit. Anything else (a pending successor, an unissued or retiring
	// launch) is advanced by the Reconcile call itself.
	action, ok := s.current(execution)
	if !ok {
		return
	}
	if action.Request != nil {
		// A request whose deadline already passed is expired instead.
		if s.now().Before(action.Deadline) {
			answer := kernel.Answer{Decision: kernel.DecisionPass, Outputs: kernel.Outputs{"decision": "approved"}, Actor: "principal-1", At: s.now()}
			if err := s.h.engine.SettleRequest(s.t.Context(), s.h.executionID, action.ID, answer); err != nil {
				s.failf("fault-free SettleRequest error = %v", err)
			}
		}
		return
	}
	if identity, started := s.startedWorkload(); started {
		if _, exited := s.exited.exits[identity.WorkloadID]; !exited {
			s.exit(identity, 0, true)
		}
	}
}

// assertCleanupExhausted pins the documented exhausted state (S02 §6): the
// record stays pending with its diagnostic and no further effect is issued.
// The installation then raises the bound so the pending cleanup resumes.
func (s *faultSchedule) assertCleanupExhausted(execution kernel.Execution) {
	s.t.Helper()
	s.exhausted = true
	s.reconcile()
	calls := len(s.probe.snapshot())
	releases := s.workspace.attempts
	commits := s.h.fixture.Store.CommitCount()
	s.reconcile()
	exhausted := s.load()
	if exhausted.Cleanup.State != kernel.CleanupPending || exhausted.Cleanup.Attempts != execution.Cleanup.Attempts ||
		!strings.HasPrefix(exhausted.Cleanup.LastError, "cleanup retry exhausted") {
		s.failf("exhausted cleanup = %+v, want pending with %d attempts and the exhaustion diagnostic", exhausted.Cleanup, execution.Cleanup.Attempts)
	}
	if len(s.probe.snapshot()) != calls || s.workspace.attempts != releases || s.h.fixture.Store.CommitCount() != commits {
		s.failf("exhausted cleanup kept working: runtime calls %d->%d, releases %d->%d, commits %d->%d",
			calls, len(s.probe.snapshot()), releases, s.workspace.attempts, commits, s.h.fixture.Store.CommitCount())
	}
	s.policy = engine.CleanupPolicy{MaxAttempts: exhausted.Cleanup.Attempts + engine.DefaultCleanupMaxAttempts}
	s.h.engine.SetCleanupPolicy(s.policy)
}

// assertQuiescent checks that a completed execution owes nothing and that
// further passes neither commit nor perform an effect.
func (s *faultSchedule) assertQuiescent() {
	s.t.Helper()
	s.what = "quiescence"
	final := s.load()
	if stops, cleanups := s.h.fixture.Store.PendingStops(), s.h.fixture.Store.PendingCleanups(); len(stops) != 0 || len(cleanups) != 0 {
		s.failf("completed execution still owes stops %+v and cleanups %+v", stops, cleanups)
	}
	for _, kind := range engine.ScanKinds() {
		owed, err := s.h.fixture.State.Scan(s.t.Context(), engine.ScanRequest{Kind: kind, Now: s.now()})
		if err != nil || len(owed) != 0 {
			s.failf("%s scan of a completed execution = %v, %v, want nothing", kind, owed, err)
		}
	}
	calls := len(s.probe.snapshot())
	commits := s.h.fixture.Store.CommitCount()
	for range 2 {
		if _, err := s.h.engine.Reconcile(s.t.Context(), s.h.executionID); err != nil {
			s.failf("Reconcile of a completed execution error = %v", err)
		}
	}
	if err := s.h.engine.Scan(s.t.Context()); err != nil {
		s.failf("Scan of a completed execution error = %v", err)
	}
	if len(s.probe.snapshot()) != calls || s.h.fixture.Store.CommitCount() != commits || !reflect.DeepEqual(s.load(), final) {
		s.failf("completed execution was not quiescent: runtime calls %d->%d, commits %d->%d",
			calls, len(s.probe.snapshot()), commits, s.h.fixture.Store.CommitCount())
	}
	s.afterStep()
}

// ---------------------------------------------------------------------------
// Test
// ---------------------------------------------------------------------------

// TestGeneratedEngineFaultSchedulesPreserveInvariants pins the Kernel
// invariants under generated engine-level fault schedules: at most one action
// is active; a terminal outcome, its failure and promoted outputs never
// change; only succeeded actions expose outputs; no identity is started twice;
// a retiring or never-started identity is never started or stopped; the
// workspace is never released, and cleanup never complete, while a
// possibly-started workload lacks a confirmed stop; versions only increase.
// Once faults stop, every schedule reaches a terminal state with complete
// cleanup within a bounded number of Reconcile calls, and an exhausted cleanup
// stays visibly pending until the installation raises its bound. The fixed
// seeds must together reach every terminal outcome and every recovery path
// named below, so the schedules cannot silently become trivial.
func TestGeneratedEngineFaultSchedulesPreserveInvariants(t *testing.T) {
	shapes := scheduleShapes()
	steps := scheduleStepTable()
	total := 0
	for _, step := range steps {
		total += step.weight
	}
	reached := make(map[string]int)
	seeds := make([]int64, 0, scheduleSeeds+len(scheduleRegressionSeeds))
	for seed := int64(1); seed <= scheduleSeeds; seed++ {
		seeds = append(seeds, seed)
	}
	seeds = append(seeds, scheduleRegressionSeeds...)
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			s := newFaultSchedule(t, seed, shapes[int(seed)%len(shapes)])
			s.afterStep()
			for s.step = 1; s.step <= s.steps; s.step++ {
				choice := s.rng.Intn(total)
				for _, step := range steps {
					if choice < step.weight {
						s.what = step.name
						step.run(s)
						break
					}
					choice -= step.weight
				}
				s.afterStep()
			}
			s.settle()

			outcome := string(s.terminal.State)
			if s.terminal.Failure != nil {
				outcome += "/" + string(s.terminal.Failure.Kind)
			}
			reached[outcome]++
			if s.ending != "" {
				reached["live execution ended by "+s.ending]++
			}
			if len(s.retiring) > 0 {
				reached["retired identity"]++
			}
			if s.exhausted {
				reached["exhausted cleanup"]++
			}
			if s.lateAnswer {
				reached["late answer refused"]++
			}
		})
	}
	if t.Failed() {
		return
	}
	for _, want := range []string{
		"succeeded", "cancelled",
		"failed/timeout", "failed/runtime", "failed/preparation", "failed/workspace_lost",
		"live execution ended by finish", "live execution ended by cancel", "live execution ended by expire",
		"retired identity", "exhausted cleanup", "late answer refused",
	} {
		if reached[want] == 0 {
			t.Errorf("no seed reached %q: %v", want, reached)
		}
	}
}
