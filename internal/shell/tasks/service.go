package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Caller is an authenticated principal acting in one project. The transport has
// authenticated it; the service decides what it may do from its current roles.
type Caller struct {
	Scope     domain.Scope
	Principal string
}

func (c Caller) validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Principal) == "" {
		return invalidf("a caller names its principal")
	}
	return nil
}

// Config wires the use cases. Every field but NewID and MaxCleanupAttempts is required.
type Config struct {
	Store     Store
	Authority Authority
	Pipelines Pipelines
	Planner   Planner
	Runs      Runs
	Clock     runtime.Clock
	// NewID returns a fresh task ID; the default is random.
	NewID func() (domain.TaskID, error)
	// MaxCleanupAttempts bounds cleanup retries of an admitted run (default 3).
	MaxCleanupAttempts int
}

// Service is the task tracker's set of shell use cases: create, get, list, edit,
// start, rerun, inbox and resolve, plus the projection (Apply, Rebuild).
//
// There is no operation that moves a task between states, parks it, blocks it on
// another task or marks it resolved. A task's execution is the run it selected,
// its progress is a projection of that run, and the only way to advance a run is
// through the kernel: Admit for a new run, DecideSignal for a wait.
type Service struct{ cfg Config }

// New validates the wiring and returns the service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil, cfg.Authority == nil, cfg.Pipelines == nil, cfg.Planner == nil, cfg.Runs == nil, cfg.Clock == nil:
		return nil, errors.New("tasks: a store, authority, pipelines, planner, runs and clock are required")
	}
	if cfg.NewID == nil {
		cfg.NewID = randomTaskID
	}
	if cfg.MaxCleanupAttempts <= 0 {
		cfg.MaxCleanupAttempts = 3
	}
	return &Service{cfg: cfg}, nil
}

func randomTaskID() (domain.TaskID, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return domain.TaskID("tsk_" + hex.EncodeToString(b[:])), nil
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// member returns the caller's current roles. A principal with none is not a
// member of the project, which is reported as not found so a foreign project is
// indistinguishable from an empty one.
func (s *Service) member(ctx context.Context, c Caller) ([]string, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	roles, err := s.cfg.Authority.Roles(ctx, c.Scope, c.Principal)
	if err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		return nil, ErrNotFound
	}
	return roles, nil
}

func holdsAny(roles []string, want ...string) bool {
	return slices.ContainsFunc(roles, func(r string) bool { return slices.Contains(want, r) })
}

// require checks that a member holds one of the roles an action needs.
// Task content and pipeline selection are decided by admins and engineers;
// starting and rerunning executions also by operators; reading by any member.
func (s *Service) require(ctx context.Context, c Caller, want ...string) error {
	roles, err := s.member(ctx, c)
	if err != nil {
		return err
	}
	if len(want) > 0 && !holdsAny(roles, want...) {
		return ErrForbidden
	}
	return nil
}

var (
	writeRoles   = []string{RoleAdmin, RoleEngineer}
	executeRoles = []string{RoleAdmin, RoleEngineer, RoleOperator}
)

// ---------------------------------------------------------------------------
// Content
// ---------------------------------------------------------------------------

// CreateRequest creates a task that selects a pipeline by name.
type CreateRequest struct {
	Caller Caller
	// ID is optional. A caller that supplies it can repeat the request after a
	// lost response: an identical repeat returns the recorded task.
	ID domain.TaskID
	// Pipeline is the project-unique pipeline name the task will run.
	Pipeline string
	Content
}

// Create records a task. The pipeline name is resolved to the project pipeline's
// immutable ID now and never again; an unknown, ambiguous or other-project name
// fails before anything is written. Authorization: admin or engineer.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Task, error) {
	if err := s.require(ctx, req.Caller, writeRoles...); err != nil {
		return Task{}, err
	}
	content, err := req.Content.Normalized()
	if err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(req.Pipeline) == "" {
		return Task{}, invalidf("a task selects a pipeline")
	}
	scope := req.Caller.Scope
	pid, err := s.cfg.Pipelines.Resolve(ctx, scope, req.Pipeline)
	if err != nil {
		return Task{}, err
	}
	pipeline := domain.PipelineID(pid)
	if err := pipeline.Validate(); err != nil {
		return Task{}, err
	}
	id := req.ID
	if id == "" {
		if id, err = s.cfg.NewID(); err != nil {
			return Task{}, fmt.Errorf("tasks: new id: %w", err)
		}
	}
	if err := id.Validate(); err != nil {
		return Task{}, err
	}
	if content.ParentID == id {
		return Task{}, invalidf("a task is not its own parent")
	}
	now := s.cfg.Clock.Now().UTC().Truncate(time.Microsecond) // PostgreSQL keeps microseconds
	t := Task{Scope: scope, ID: id, Content: content, PipelineID: pipeline, Version: 1,
		Progress: Progress{Status: StatusPending}, Created: now, Updated: now}
	err = s.cfg.Store.Create(ctx, t, req.Caller.Principal)
	if errors.Is(err, domain.ErrConflict) {
		// A repeat after a lost response is the task already recorded; anything
		// else under the same ID is a real conflict.
		existing, gerr := s.cfg.Store.Get(ctx, scope, id)
		if gerr == nil && existing.PipelineID == pipeline && sameContent(existing.Content, content) && existing.Version == 1 {
			return existing, nil
		}
		return Task{}, domain.ErrConflict
	}
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

// Get returns one task. Any project member may read it.
func (s *Service) Get(ctx context.Context, c Caller, id domain.TaskID) (Task, error) {
	if err := s.require(ctx, c); err != nil {
		return Task{}, err
	}
	if err := id.Validate(); err != nil {
		return Task{}, ErrNotFound
	}
	return s.cfg.Store.Get(ctx, c.Scope, id)
}

// ListRequest lists tasks of the caller's project.
type ListRequest struct {
	Caller   Caller
	Status   Status
	ParentID domain.TaskID
	After    domain.TaskID
	Limit    int
}

// Bounds of a page.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

func pageLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}

// List returns one page of tasks. Any project member may list them.
func (s *Service) List(ctx context.Context, req ListRequest) ([]Task, error) {
	if err := s.require(ctx, req.Caller); err != nil {
		return nil, err
	}
	if req.Status != "" && !req.Status.Valid() {
		return nil, invalidf("status %q", req.Status)
	}
	return s.cfg.Store.List(ctx, ListQuery{Scope: req.Caller.Scope, Status: req.Status, ParentID: req.ParentID,
		After: req.After, Limit: pageLimit(req.Limit)})
}

// EditRequest replaces the editable content of a task. The parent and the
// pipeline are fixed at creation.
type EditRequest struct {
	Caller          Caller
	TaskID          domain.TaskID
	ExpectedVersion domain.Version
	Content
}

// Edit replaces the task's content under its own optimistic version. It does not
// advance, restart or otherwise touch execution. Authorization: admin or engineer.
func (s *Service) Edit(ctx context.Context, req EditRequest) (Task, error) {
	if err := s.require(ctx, req.Caller, writeRoles...); err != nil {
		return Task{}, err
	}
	if err := req.TaskID.Validate(); err != nil {
		return Task{}, ErrNotFound
	}
	if err := req.ExpectedVersion.Validate(); err != nil {
		return Task{}, domain.ErrInvalidValue
	}
	cur, err := s.cfg.Store.Get(ctx, req.Caller.Scope, req.TaskID)
	if err != nil {
		return Task{}, err
	}
	req.Content.ParentID = cur.ParentID // the parent is fixed at creation
	content, err := req.Content.Normalized()
	if err != nil {
		return Task{}, err
	}
	return s.cfg.Store.UpdateContent(ctx, req.Caller.Scope, req.TaskID, req.ExpectedVersion, content, s.cfg.Clock.Now().UTC().Truncate(time.Microsecond))
}

// ---------------------------------------------------------------------------
// Execution: start and rerun
// ---------------------------------------------------------------------------

// StartRequest runs a task's pipeline for the first time.
type StartRequest struct {
	Caller Caller
	TaskID domain.TaskID
	Inputs map[string]runtime.InputValue
}

// Start admits the task's first run and makes it the current run. It is
// idempotent: the run's identity is derived from the task, so a repeat after a
// lost response finds the run it already admitted and selects nothing twice.
// Authorization: admin, engineer or operator.
func (s *Service) Start(ctx context.Context, req StartRequest) (Task, error) {
	t, err := s.executable(ctx, req.Caller, req.TaskID)
	if err != nil {
		return Task{}, err
	}
	if t.Progress.RunID != "" {
		if t.Progress.RunID == runIDFor(t, "") {
			return t, nil // the repeat of a Start whose response was lost
		}
		return Task{}, ErrAlreadyStarted
	}
	return s.launch(ctx, t, "", req.Caller.Principal, req.Inputs)
}

// RerunRequest runs the task's pipeline again after its current run finished.
type RerunRequest struct {
	Caller Caller
	TaskID domain.TaskID
	// ExpectedRunID is the current run the caller is rerunning. A task whose
	// current run is another one is refused as stale.
	ExpectedRunID domain.RunID
	Inputs        map[string]runtime.InputValue
}

// Rerun admits a new run and makes it current. The earlier run keeps every step,
// wait and result row exactly as it ended: a rerun resets nothing, and the
// earlier run stays queryable as history. It is refused while the current run
// has not finished and, like Start, is idempotent on the run it replaces.
// Authorization: admin, engineer or operator.
func (s *Service) Rerun(ctx context.Context, req RerunRequest) (Task, error) {
	if err := req.ExpectedRunID.Validate(); err != nil {
		return Task{}, ErrNoRun
	}
	t, err := s.executable(ctx, req.Caller, req.TaskID)
	if err != nil {
		return Task{}, err
	}
	switch {
	case t.Progress.RunID == "":
		return Task{}, ErrNoRun
	case t.Progress.RunID == runIDFor(t, req.ExpectedRunID):
		return t, nil // the repeat of a Rerun whose response was lost
	case t.Progress.RunID != req.ExpectedRunID:
		return Task{}, ErrStaleRun
	}
	// The stored projection may lag the run: judge from the run's own records.
	t, err = s.refresh(ctx, t, false)
	if err != nil {
		return Task{}, err
	}
	if !t.Progress.Status.Terminal() {
		return Task{}, ErrRunActive
	}
	return s.launch(ctx, t, req.ExpectedRunID, req.Caller.Principal, req.Inputs)
}

func (s *Service) executable(ctx context.Context, c Caller, id domain.TaskID) (Task, error) {
	if err := s.require(ctx, c, executeRoles...); err != nil {
		return Task{}, err
	}
	if err := id.Validate(); err != nil {
		return Task{}, ErrNotFound
	}
	return s.cfg.Store.Get(ctx, c.Scope, id)
}

// runIDFor derives the identity of the run that follows `after` (empty for the
// first). It is a pure function of the task, so a retried launch addresses the
// run its lost predecessor admitted.
func runIDFor(t Task, after domain.RunID) domain.RunID {
	h := sha256.New()
	for _, p := range []string{string(t.Scope.TenantID), string(t.Scope.ProjectID), string(t.ID), string(after)} {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return domain.RunID("run_" + hex.EncodeToString(h.Sum(nil)[:12]))
}

func (s *Service) launch(ctx context.Context, t Task, after domain.RunID, actor string, inputs map[string]runtime.InputValue) (Task, error) {
	plan, err := s.cfg.Planner.Plan(ctx, PlanRequest{Scope: t.Scope, TaskID: t.ID, PipelineID: t.PipelineID, Inputs: inputs})
	if err != nil {
		return Task{}, err
	}
	adm, err := s.admission(t, runIDFor(t, after), plan)
	if err != nil {
		return Task{}, err
	}
	if _, err := s.cfg.Runs.Admit(ctx, adm); err != nil {
		// The run's identity is derived from the task, so a run already admitted
		// under it is the one a lost response left behind, even when the
		// pipeline's plan has changed since.
		if !errors.Is(err, domain.ErrConflict) {
			return Task{}, err
		}
		if _, lerr := s.cfg.Runs.Load(ctx, t.Scope, adm.Run.ID); lerr != nil {
			return Task{}, err
		}
	}
	view, err := s.cfg.Runs.View(ctx, t.Scope, adm.Run.ID)
	if err != nil {
		return Task{}, err
	}
	return s.cfg.Store.Select(ctx, RunSelection{Scope: t.Scope, TaskID: t.ID, Expected: after, Run: adm.Run.ID,
		Progress: Project(view), Actor: actor})
}

func (s *Service) admission(t Task, id domain.RunID, plan runtime.Plan) (runtime.Admission, error) {
	run, err := runtime.NewRun(t.Scope, id, t.PipelineID, plan, s.cfg.MaxCleanupAttempts)
	if err != nil {
		return runtime.Admission{}, err
	}
	adm := runtime.Admission{Run: run, Plan: plan}
	for _, p := range plan.Steps {
		if p.Kind != runtime.StepWait {
			continue
		}
		w, err := runtime.NewWait(t.Scope, runtime.WaitID(fmt.Sprintf("%s-%s", id, p.Key)), id, p.Key, *p.Wait)
		if err != nil {
			return runtime.Admission{}, err
		}
		adm.Waits = append(adm.Waits, w)
	}
	adm.Events = []runtime.Event{{ID: "run.created:" + string(id), Scope: t.Scope, RunID: id, Kind: runtime.EventRunCreated,
		Version: 1, At: s.cfg.Clock.Now().UTC()}}
	return adm, adm.Validate()
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

// Applied reports what the projection did with an event.
type Applied struct {
	// Task is the task after the event; zero when the run belongs to no task.
	Task    Task
	Verdict Verdict
	// NoTask is true for a run that no task owns (a triggered run): there is
	// nothing to project and the event is settled.
	NoTask bool
}

// Apply is the projection consumer. The event is only a notification: it names
// a run, and the projection is rebuilt from that run's own records. An event of
// a run that is not the task's current run, or whose records are not newer than
// the projection, changes nothing, so a redelivered, reordered or late event of
// an older run cannot overwrite a newer view. It returns an error, to be
// redelivered, only when a store could not be read or written.
func (s *Service) Apply(ctx context.Context, ev runtime.Event) (Applied, error) {
	if err := ev.Validate(); err != nil {
		return Applied{}, err
	}
	id, err := s.cfg.Store.TaskOfRun(ctx, ev.Scope, ev.RunID)
	if errors.Is(err, ErrNotFound) {
		return Applied{NoTask: true}, nil
	}
	if err != nil {
		return Applied{}, err
	}
	t, err := s.cfg.Store.Get(ctx, ev.Scope, id)
	if err != nil {
		return Applied{}, err
	}
	if t.Progress.RunID != ev.RunID {
		return Applied{Task: t, Verdict: VerdictStale}, nil
	}
	view, err := s.cfg.Runs.View(ctx, ev.Scope, ev.RunID)
	if err != nil {
		return Applied{}, err
	}
	next, v, err := s.cfg.Store.Advance(ctx, ev.Scope, id, Project(view), false)
	return Applied{Task: next, Verdict: v}, err
}

// refresh advances the stored projection of the task's current run from the
// run's records and returns the task as stored afterwards.
func (s *Service) refresh(ctx context.Context, t Task, force bool) (Task, error) {
	view, err := s.cfg.Runs.View(ctx, t.Scope, t.Progress.RunID)
	if err != nil {
		return Task{}, err
	}
	next, _, err := s.cfg.Store.Advance(ctx, t.Scope, t.ID, Project(view), force)
	return next, err
}

// Rebuild recomputes one task's projection from its current run's records. It
// repairs a projection that missed events or was damaged, and never lowers it. A
// task without a run has nothing to rebuild. Authorization: admin or engineer.
func (s *Service) Rebuild(ctx context.Context, c Caller, id domain.TaskID) (Task, error) {
	if err := s.require(ctx, c, writeRoles...); err != nil {
		return Task{}, err
	}
	t, err := s.cfg.Store.Get(ctx, c.Scope, id)
	if err != nil {
		return Task{}, err
	}
	if t.Progress.RunID == "" {
		return t, nil
	}
	return s.refresh(ctx, t, true)
}

// RebuildProject rebuilds the projection of every task of the caller's project
// that has a run, and returns how many it examined. Authorization: admin or engineer.
func (s *Service) RebuildProject(ctx context.Context, c Caller) (int, error) {
	if err := s.require(ctx, c, writeRoles...); err != nil {
		return 0, err
	}
	n, after := 0, domain.TaskID("")
	for {
		page, err := s.cfg.Store.List(ctx, ListQuery{Scope: c.Scope, After: after, Limit: MaxLimit})
		if err != nil {
			return n, err
		}
		for _, t := range page {
			after = t.ID
			if t.Progress.RunID == "" {
				continue
			}
			if _, err := s.refresh(ctx, t, true); err != nil {
				return n, err
			}
			n++
		}
		if len(page) < MaxLimit {
			return n, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Inbox and resolution
// ---------------------------------------------------------------------------

// InboxEntry is one wait the caller may decide, bound to the exact run, step,
// wait identity and wait version. Correlation is shown for context (for example
// the candidate commit); it never confers access.
type InboxEntry struct {
	Task        Task
	RunID       domain.RunID
	StepKey     domain.StepID
	WaitID      runtime.WaitID
	WaitVersion domain.Version
	Assignment  string
	Request     string
	Correlation map[string]string
	Input       string
	Outcomes    []string
	Deadline    time.Time
}

// InboxRequest pages the caller's role inbox.
type InboxRequest struct {
	Caller Caller
	After  domain.TaskID
	Limit  int
}

// Inbox lists the tasks whose current wait is open and assigned to a role the
// caller holds now. Only assignment grants an entry: a wait without assignment
// has none, and knowing the correlation grants nothing. A candidate is kept only
// when the wait's own record, not just the projection, is open, assigned to one
// of the caller's roles and the task's current wait, so a reassignment or a
// decision removes the entry at once however far the projection lags. It returns
// the cursor for the next page, empty when the listing is exhausted.
func (s *Service) Inbox(ctx context.Context, req InboxRequest) ([]InboxEntry, domain.TaskID, error) {
	roles, err := s.member(ctx, req.Caller)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(req.Limit)
	var out []InboxEntry
	after := req.After
	for pages := 0; pages < 10; pages++ {
		page, err := s.cfg.Store.Waiting(ctx, req.Caller.Scope, roles, after, limit)
		if err != nil {
			return nil, "", err
		}
		for _, t := range page {
			after = t.ID
			e, ok, err := s.entry(ctx, t, roles)
			if err != nil {
				return nil, "", err
			}
			if ok {
				out = append(out, e)
			}
			if len(out) == limit {
				return out, after, nil
			}
		}
		if len(page) < limit {
			return out, "", nil
		}
	}
	return out, after, nil
}

func (s *Service) entry(ctx context.Context, t Task, roles []string) (InboxEntry, bool, error) {
	if t.Progress.RunID == "" {
		return InboxEntry{}, false, nil
	}
	rec, err := s.cfg.Runs.Load(ctx, t.Scope, t.Progress.RunID)
	if isNotFound(err) {
		return InboxEntry{}, false, nil
	}
	if err != nil {
		return InboxEntry{}, false, err
	}
	for _, w := range rec.Waits {
		if w.State != runtime.WaitOpen || w.Assignment == "" || !slices.Contains(roles, w.Assignment) {
			continue
		}
		return InboxEntry{Task: t, RunID: w.RunID, StepKey: w.StepKey, WaitID: w.ID, WaitVersion: w.Version,
			Assignment: w.Assignment, Request: w.Request, Correlation: cloneMap(w.Correlation), Input: w.Spec.Input,
			Outcomes: slices.Clone(w.Spec.Outcomes), Deadline: w.Deadline}, true, nil
	}
	return InboxEntry{}, false, nil
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ResolveRequest decides one wait, addressed by its exact run, step and wait
// identity and the wait version the caller saw.
type ResolveRequest struct {
	Caller  Caller
	TaskID  domain.TaskID
	RunID   domain.RunID
	StepKey domain.StepID
	WaitID  runtime.WaitID
	// ExpectedVersion is the wait version of the inbox entry; it is required.
	ExpectedVersion domain.Version
	// Key is the caller's idempotency key. With the tenant, the project and the
	// caller's principal it forms the signal identity.
	Key     string
	Outcome string
	Answer  string
}

// Resolve submits the caller's decision of one wait as a kernel signal. The task
// state is not mutated: the actor, outcome and answer are recorded immutably by
// the kernel against that exact run, step and wait, and the run's own engine
// continues from the recorded outcome.
//
// A replay of a recorded key returns its recorded result whatever the wait is
// now. A new decision needs the caller's current membership and the wait's
// assigned role, and an exact run, step and wait of the task; anything the
// caller may not decide is ErrNotFound, indistinguishable from a wait that does
// not exist, and correlation values authorize nothing. A wait that already closed,
// a stale version, an undeclared or reserved outcome are refusals the kernel
// records and returns unapplied.
func (s *Service) Resolve(ctx context.Context, req ResolveRequest) (runtime.SignalResult, error) {
	roles, err := s.member(ctx, req.Caller)
	if err != nil {
		return runtime.SignalResult{}, err
	}
	if err := req.TaskID.Validate(); err != nil {
		return runtime.SignalResult{}, ErrNotFound
	}
	if err := req.ExpectedVersion.Validate(); err != nil {
		return runtime.SignalResult{}, ErrVersionRequired
	}
	sig := runtime.SignalRequest{
		Identity:        runtime.SignalIdentity{Scope: req.Caller.Scope, Caller: req.Caller.Principal, Key: req.Key},
		WaitID:          req.WaitID,
		ExpectedVersion: req.ExpectedVersion,
		Outcome:         req.Outcome,
		Answer:          req.Answer,
		Actor:           req.Caller.Principal,
	}
	if err := sig.Validate(); err != nil {
		return runtime.SignalResult{}, err
	}
	if err := req.RunID.Validate(); err != nil {
		return runtime.SignalResult{}, ErrNotFound
	}
	if err := req.StepKey.Validate(); err != nil {
		return runtime.SignalResult{}, ErrNotFound
	}
	// A recorded key is answered from its record. The record is the caller's own
	// earlier result, so it reveals nothing the caller was not already given.
	prior, err := s.cfg.Runs.LookupSignal(ctx, sig.Identity)
	if err != nil {
		return runtime.SignalResult{}, err
	}
	if !prior.Found() {
		if err := s.authorizeWait(ctx, req, roles); err != nil {
			return runtime.SignalResult{}, err
		}
	}
	return s.cfg.Runs.DecideSignal(ctx, sig, s.cfg.Clock.Now().UTC())
}

// authorizeWait checks that the wait is the exact run, step and wait of the
// task, that it was opened, and that the caller holds its assigned role.
func (s *Service) authorizeWait(ctx context.Context, req ResolveRequest, roles []string) error {
	scope := req.Caller.Scope
	owner, err := s.cfg.Store.TaskOfRun(ctx, scope, req.RunID)
	if err != nil || owner != req.TaskID {
		return ErrNotFound
	}
	rec, err := s.cfg.Runs.Load(ctx, scope, req.RunID)
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	i := slices.IndexFunc(rec.Waits, func(w runtime.Wait) bool { return w.ID == req.WaitID && w.StepKey == req.StepKey })
	if i < 0 {
		return ErrNotFound
	}
	w := rec.Waits[i]
	// A wait that never opened has no inbox entry, and a wait without an
	// assignment is decided only by correlation, never through the inbox.
	if w.State == runtime.WaitPending || w.Assignment == "" || !slices.Contains(roles, w.Assignment) {
		return ErrNotFound
	}
	t, err := s.cfg.Store.Get(ctx, scope, req.TaskID)
	if err != nil {
		return err
	}
	// An earlier run's wait is never decided on behalf of the current one. A
	// closed one still answers with its recorded decision through the kernel.
	if t.Progress.RunID != req.RunID && w.State == runtime.WaitOpen {
		return ErrStaleRun
	}
	return nil
}
