package tasks

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Stable errors. Callers classify with errors.Is; messages are fixed and carry
// no task content.
var (
	// ErrNotFound reports a task, run, step or wait that does not exist for the
	// caller. A missing one, a foreign one and one the caller may not see are
	// indistinguishable.
	ErrNotFound error = &domain.Error{Code: domain.CodeNotFound, Message: "task not found"}
	// ErrForbidden reports a project member who lacks the role an action needs.
	ErrForbidden error = &domain.Error{Code: domain.CodeForbidden, Message: "the caller's role does not allow this"}
	// ErrContentConflict reports a content edit made against an older version.
	ErrContentConflict error = &domain.Error{Code: domain.CodeVersionConflict, Message: "the task content changed"}
	// ErrVersionRequired reports a resolution that did not name the wait version it decides.
	ErrVersionRequired error = &domain.Error{Code: domain.CodeVersionRequired, Message: "the wait version is required"}
	// ErrStaleRun reports a request made against a run that is not the task's current one.
	ErrStaleRun error = &domain.Error{Code: CodeStaleRun, Message: "the run is not the task's current run"}
	// ErrAlreadyStarted reports a Start on a task that has a run; reruns are explicit.
	ErrAlreadyStarted error = &domain.Error{Code: CodeAlreadyStarted, Message: "the task already has a run; rerun it explicitly"}
	// ErrRunActive reports a rerun while the current run has not finished.
	ErrRunActive error = &domain.Error{Code: CodeRunActive, Message: "the current run has not finished"}
	// ErrNoRun reports a rerun of a task that never ran.
	ErrNoRun error = &domain.Error{Code: CodeNoRun, Message: "the task has no run to rerun"}
)

// Stable task error codes.
const (
	CodeStaleRun       domain.ErrorCode = "task_stale_run"
	CodeAlreadyStarted domain.ErrorCode = "task_already_started"
	CodeRunActive      domain.ErrorCode = "task_run_active"
	CodeNoRun          domain.ErrorCode = "task_no_run"
)

// ListQuery selects tasks of one project, ordered by task ID.
type ListQuery struct {
	Scope    domain.Scope
	Status   Status        // empty: any
	ParentID domain.TaskID // empty: any
	After    domain.TaskID // keyset: tasks after this ID
	Limit    int
}

// RunSelection makes a run the task's one current execution.
type RunSelection struct {
	Scope  domain.Scope
	TaskID domain.TaskID
	// Expected is the current run the caller observed, empty for the first run.
	// The selection commits only when it is still the current run.
	Expected domain.RunID
	// Run is the run to select; Progress is its initial projection.
	Run      domain.RunID
	Progress Progress
	Actor    string
}

// Store is the task persistence port. Each method is one atomic, scope-checked
// transaction that commits its outbox events with the state change; a task,
// run or project outside the scope is ErrNotFound. Memory and PostgreSQL
// implement it and share one contract suite.
type Store interface {
	// Create records a task and its task.created event. A task with the ID
	// already recorded is domain.ErrConflict; nothing is written.
	Create(ctx context.Context, t Task, actor string) error
	Get(ctx context.Context, scope domain.Scope, id domain.TaskID) (Task, error)
	List(ctx context.Context, q ListQuery) ([]Task, error)
	// UpdateContent replaces the content of the task at expected: the version
	// advances by one, and a task at another version is ErrContentConflict. It
	// never touches the pipeline, the current run or the progress. The editing
	// actor and a bounded task.content_changed event commit in the same
	// transaction as the version change: if the event cannot be recorded, the
	// content and version are not changed either.
	UpdateContent(ctx context.Context, scope domain.Scope, id domain.TaskID, expected domain.Version, c Content, now time.Time, actor string) (Task, error)
	// Select makes sel.Run the current run when the current run is still
	// sel.Expected, projecting sel.Progress. Selecting the run that is already
	// current is a replay and returns the task unchanged; any other current run is
	// ErrStaleRun.
	Select(ctx context.Context, sel RunSelection) (Task, error)
	// Advance applies the guarded projection of one observed run (see advance):
	// stale and replayed observations change nothing and are reported.
	Advance(ctx context.Context, scope domain.Scope, id domain.TaskID, obs Progress, force bool) (Task, Verdict, error)
	// TaskOfRun names the task a run belongs to, current or earlier, or
	// ErrNotFound for a run that belongs to none.
	TaskOfRun(ctx context.Context, scope domain.Scope, run domain.RunID) (domain.TaskID, error)
	// Waiting lists tasks whose projection says they wait for one of roles,
	// ordered by task ID after the cursor. It is a candidate query: the use case
	// checks each against the run's own records.
	Waiting(ctx context.Context, scope domain.Scope, roles []string, after domain.TaskID, limit int) ([]Task, error)
}

// Pipelines resolves a project-unique pipeline name to its immutable ID. The
// pipeline registry satisfies it (pipelines.Registry); a missing, ambiguous or
// foreign name is its ErrNotFound.
type Pipelines interface {
	Resolve(ctx context.Context, scope domain.Scope, name string) (string, error)
}

// Authority reports the roles a principal currently holds in a project. It is
// asked on every call: a role revoked a moment ago grants nothing now. A
// principal that is not a member has no roles.
type Authority interface {
	Roles(ctx context.Context, scope domain.Scope, principal string) ([]string, error)
}

// PlanRequest asks for the immutable plan of one run of a task.
type PlanRequest struct {
	Scope      domain.Scope
	TaskID     domain.TaskID
	PipelineID domain.PipelineID
	Inputs     map[string]runtime.InputValue
}

// Planner compiles the selected pipeline into the plan a run is admitted with.
type Planner interface {
	Plan(ctx context.Context, req PlanRequest) (runtime.Plan, error)
}

// Runs is the part of the kernel state port the task tracker uses. It never
// writes run state directly: a run is admitted through Admit, and a wait is
// decided only through DecideSignal. runtime.StateStore satisfies it.
type Runs interface {
	Admit(ctx context.Context, a runtime.Admission) (runtime.RunRecord, error)
	Load(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunRecord, error)
	View(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunView, error)
	LookupSignal(ctx context.Context, id runtime.SignalIdentity) (runtime.SignalLookup, error)
	DecideSignal(ctx context.Context, req runtime.SignalRequest, now time.Time) (runtime.SignalResult, error)
}

var _ Runs = runtime.StateStore(nil)
