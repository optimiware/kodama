// Package inbox declares the scoped role-inbox query and its stable page.
//
// The inbox is a query over active request actions, waiting tasks and failed
// open tasks, filtered by the caller's current roles and scope. It is not an
// independently mutable request entity: authoritative state lives on the
// execution/action and task rows, so a stale event can never strand or
// resurrect an item.
package inbox

import (
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Kind names one inbox item category.
type Kind string

const (
	// KindRequest is an active request action assigned to the caller.
	KindRequest Kind = "request"
	// KindWaitingTask is a waiting task assigned to the caller.
	KindWaitingTask Kind = "waiting_task"
	// KindFailedTask is a failed open task shown to authorized operators.
	KindFailedTask Kind = "failed_task"
)

var kinds = [...]Kind{KindRequest, KindWaitingTask, KindFailedTask}

// Kinds returns every inbox kind in declaration order.
func Kinds() []Kind {
	return append([]Kind(nil), kinds[:]...)
}

// Valid reports whether the kind is one of the declared kinds.
func (kind Kind) Valid() bool {
	for _, candidate := range kinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Query is one scoped inbox request. Limit is the requested page size; Cursor
// is the opaque stable continuation returned by the previous page.
type Query struct {
	Scope     tenancy.Scope      `json:"scope"`
	Principal identity.Principal `json:"principal"`
	Kinds     []Kind             `json:"kinds,omitempty"`
	Cursor    string             `json:"cursor,omitempty"`
	Limit     int                `json:"limit"`
}

// Item is one active inbox entry with its authoritative version.
//
// A request item is self-sufficient like a waiting_task item: it carries the
// immutable resolved subject, the declared response output names and the
// action deadline (At). A waiting task carries everything the exact
// outstanding assignee needs to answer without a second read: the question
// identity, body and durable references, and the task lifecycle version.
// Version stays the QUESTION version (the value a reply names as its expected
// question version); LifecycleVersion is the task lifecycle version the reply
// names as its expected task version. Waiting-task and failed-task items leave
// the request fields empty; a failed-task item's Version is already its
// lifecycle version.
type Item struct {
	Kind             Kind                `json:"kind"`
	TaskID           id.TaskID           `json:"task_id,omitempty"`
	ExecutionID      id.ExecutionID      `json:"execution_id,omitempty"`
	ActionID         id.ActionID         `json:"action_id,omitempty"`
	Assignment       request.Assignment  `json:"assignment"`
	Subject          *definition.Subject `json:"subject,omitempty"`
	Outputs          []id.OutputKey      `json:"outputs,omitempty"`
	QuestionID       task.QuestionID     `json:"question_id,omitempty"`
	Question         string              `json:"question,omitempty"`
	References       []string            `json:"references,omitempty"`
	Version          uint64              `json:"version"`
	LifecycleVersion uint64              `json:"lifecycle_version,omitempty"`
	At               time.Time           `json:"at"`
}

// Page is one stable inbox page.
type Page struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
