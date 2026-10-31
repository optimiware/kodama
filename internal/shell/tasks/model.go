package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Status is the projected status of a task's current run. It is derived from
// run and step records and is never set by a caller.
type Status string

// Task statuses (work management, "Model").
const (
	// StatusPending: the task has no run yet.
	StatusPending   Status = "pending"
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusWaiting   Status = "waiting"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusQueued, StatusRunning, StatusWaiting, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Terminal reports whether the run behind the status has ended, cleanup
// included. A rerun is admitted only from a terminal status.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Result is the bounded summary of a finished run: its outcome, the bounded
// reason code and the first failed step. The zero Result means none.
type Result struct {
	Outcome runtime.Outcome `json:"outcome,omitempty"`
	Reason  runtime.Reason  `json:"reason,omitempty"`
	Step    domain.StepID   `json:"step,omitempty"`
}

// Progress is the execution projection of a task: a read model of its current
// run. The run, step and wait records own execution truth; every field here is a
// function of them (Project) and can be rebuilt from them at any time. Nothing
// in Progress is a state a caller or a use case moves: there is no transition.
type Progress struct {
	// RunID is the task's one current execution; empty before the first run.
	RunID domain.RunID
	// Status is pending without a run, otherwise the projected run status.
	Status Status
	// Step is the step that is active or waiting.
	Step domain.StepID
	// WaitingFor is the assignment of the current open wait, when it has one.
	// Correlation values belong to the wait and are never projected.
	WaitingFor string
	// Result summarises a finished run.
	Result Result
	// Version counts the changes of the projected fields. It is monotonic.
	Version domain.Version
	// RunVersion is the observed version of the current run's records (see
	// ViewVersion). It is what makes an older view unable to overwrite a newer
	// one, and it may restart when a rerun selects a different run.
	RunVersion domain.Version
}

// same reports whether the projected fields (not the versions) are equal.
func (p Progress) same(o Progress) bool {
	return p.RunID == o.RunID && p.Status == o.Status && p.Step == o.Step && p.WaitingFor == o.WaitingFor && p.Result == o.Result
}

// Content is the editable part of a task. It has its own optimistic version
// (Task.Version) and never advances execution.
type Content struct {
	ParentID     domain.TaskID       `json:"parentId,omitempty"`
	Title        string              `json:"title"`
	Body         string              `json:"body,omitempty"`
	AssignedRole string              `json:"assignedRole,omitempty"`
	RepositoryID domain.RepositoryID `json:"repositoryId,omitempty"`
	Branch       string              `json:"branch,omitempty"`
	// Context is a JSON object of bounded size.
	Context json.RawMessage `json:"context,omitempty"`
}

// Bounds of task content.
const (
	MaxTitleChars   = 512
	MaxBodyBytes    = 64 << 10
	MaxBranchBytes  = 255
	MaxContextBytes = 16 << 10
)

// Roles a project member can hold. A task's assigned role and a wait's
// assignment are compared against them.
const (
	RoleAdmin    = "admin"
	RoleEngineer = "engineer"
	RoleReviewer = "reviewer"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

func knownRole(r string) bool {
	switch r {
	case RoleAdmin, RoleEngineer, RoleReviewer, RoleOperator, RoleViewer:
		return true
	}
	return false
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidValue, fmt.Sprintf(format, args...))
}

// Normalized validates the content and returns it with its context in canonical
// form (an object with sorted keys, `{}` when empty).
func (c Content) Normalized() (Content, error) {
	if n := utf8.RuneCountInString(c.Title); strings.TrimSpace(c.Title) == "" || n > MaxTitleChars || !utf8.ValidString(c.Title) {
		return Content{}, invalidf("a task title is 1 to %d characters", MaxTitleChars)
	}
	if len(c.Body) > MaxBodyBytes || !utf8.ValidString(c.Body) {
		return Content{}, invalidf("a task body is at most %d bytes of UTF-8", MaxBodyBytes)
	}
	if c.AssignedRole != "" && !knownRole(c.AssignedRole) {
		return Content{}, invalidf("assigned role %q", c.AssignedRole)
	}
	if c.ParentID != "" {
		if err := c.ParentID.Validate(); err != nil {
			return Content{}, err
		}
	}
	if c.RepositoryID != "" {
		if err := c.RepositoryID.Validate(); err != nil {
			return Content{}, err
		}
	}
	if len(c.Branch) > MaxBranchBytes || strings.ContainsFunc(c.Branch, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return Content{}, invalidf("a branch is at most %d printable bytes", MaxBranchBytes)
	}
	ctx, err := canonicalObject(c.Context)
	if err != nil {
		return Content{}, err
	}
	c.Context = ctx
	return c, nil
}

func canonicalObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > MaxContextBytes {
		return nil, invalidf("a task context is at most %d bytes", MaxContextBytes)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalidf("a task context is a JSON object")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, invalidf("a task context is a JSON object")
	}
	return out, nil
}

// sameContent compares content by value, so a replayed create is recognised
// whichever way the store orders the keys of the context.
func sameContent(a, b Content) bool {
	var ca, cb any
	if json.Unmarshal(a.Context, &ca) != nil || json.Unmarshal(b.Context, &cb) != nil {
		return false
	}
	return a.ParentID == b.ParentID && a.Title == b.Title && a.Body == b.Body && a.AssignedRole == b.AssignedRole &&
		a.RepositoryID == b.RepositoryID && a.Branch == b.Branch && reflect.DeepEqual(ca, cb)
}

// Task is a unit of work that selects one pipeline and exposes the progress of
// that pipeline's current run. Content and execution are independent: content
// edits use Version, execution is read from Progress.
type Task struct {
	Scope domain.Scope
	ID    domain.TaskID
	Content
	// PipelineID is the immutable project pipeline the task selected at creation.
	PipelineID domain.PipelineID
	// Version is the optimistic content version.
	Version  domain.Version
	Progress Progress
	Created  time.Time
	Updated  time.Time
}

func (t Task) clone() Task {
	t.Context = append(json.RawMessage(nil), t.Context...)
	return t
}

// Event is bounded outbox intent committed with the state change it describes
// (event catalog: task.created, task.progress_changed). Data carries references
// and codes only.
type Event struct {
	Type     string
	DedupKey string
	// Actor references the user behind the change; empty for the system.
	Actor string
	Data  json.RawMessage
}

// Event types of the task tracker.
const (
	EventTaskCreated         = "task.created"
	EventTaskProgressChanged = "task.progress_changed"
)

func createdEvent(t Task, actor string) Event {
	data, _ := json.Marshal(map[string]any{"taskId": t.ID, "pipelineId": t.PipelineID, "status": StatusPending})
	return Event{Type: EventTaskCreated, DedupKey: string(t.ID), Actor: actor, Data: data}
}

func progressEvent(t Task) Event {
	p := t.Progress
	data, _ := json.Marshal(map[string]any{"taskId": t.ID, "runId": p.RunID, "status": p.Status, "step": p.Step,
		"waitingFor": p.WaitingFor, "progressVersion": p.Version})
	return Event{Type: EventTaskProgressChanged, DedupKey: fmt.Sprintf("%s:%d", t.ID, p.Version), Data: data}
}
