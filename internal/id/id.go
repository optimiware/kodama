// Package id defines the typed identities shared by the Kernel, Shell and
// Userland contracts.
//
// Every identity is a distinct named string type so that a value from one
// scope cannot be silently accepted in another (for example an action id used
// where an execution id is required). Parsing is the only admission path:
// identities are bounded, ASCII, and free of path separators or control
// characters so they can be used in URLs, file names and log context without
// reinterpretation.
package id

import (
	"errors"
	"fmt"
)

// MaxIDLength bounds every identity payload in bytes.
const MaxIDLength = 128

// ErrInvalidID reports an identity that cannot be used as a stable key.
var ErrInvalidID = errors.New("invalid id")

// ExecutionID identifies one admitted execution.
type ExecutionID string

// ActionID identifies one action inside an execution.
type ActionID string

// TaskID identifies a task conversation.
type TaskID string

// TenantID identifies an isolated tenant organization.
type TenantID string

// ProjectID identifies a project inside a tenant.
type ProjectID string

// PrincipalID identifies a human principal or service account.
type PrincipalID string

// EventID identifies a durable event row.
type EventID string

// ReceiptID identifies an admission or verification receipt.
type ReceiptID string

// OperationID identifies one submitted public operation.
type OperationID string

// WorkloadID identifies one external runtime workload. It is stable for the
// lifetime of the effect and is never reused for a fresh command attempt.
type WorkloadID string

// WorkspaceID identifies one retained execution workspace.
type WorkspaceID string

// validID reports whether value is a bounded, ASCII identity payload.
func validID(value string) bool {
	if value == "" || len(value) > MaxIDLength {
		return false
	}
	// A bare dot or dot-dot is a path component, not an identity.
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

func invalidID(kind, value string) error {
	return fmt.Errorf("%w: %s %q", ErrInvalidID, kind, value)
}

// ParseExecutionID validates and converts an execution identity.
func ParseExecutionID(value string) (ExecutionID, error) {
	if !validID(value) {
		return "", invalidID("execution id", value)
	}
	return ExecutionID(value), nil
}

// ParseActionID validates and converts an action identity.
func ParseActionID(value string) (ActionID, error) {
	if !validID(value) {
		return "", invalidID("action id", value)
	}
	return ActionID(value), nil
}

// ParseTaskID validates and converts a task identity.
func ParseTaskID(value string) (TaskID, error) {
	if !validID(value) {
		return "", invalidID("task id", value)
	}
	return TaskID(value), nil
}

// ParseTenantID validates and converts a tenant identity.
func ParseTenantID(value string) (TenantID, error) {
	if !validID(value) {
		return "", invalidID("tenant id", value)
	}
	return TenantID(value), nil
}

// ParseProjectID validates and converts a project identity.
func ParseProjectID(value string) (ProjectID, error) {
	if !validID(value) {
		return "", invalidID("project id", value)
	}
	return ProjectID(value), nil
}

// ParsePrincipalID validates and converts a principal identity.
func ParsePrincipalID(value string) (PrincipalID, error) {
	if !validID(value) {
		return "", invalidID("principal id", value)
	}
	return PrincipalID(value), nil
}

// ParseEventID validates and converts an event identity.
func ParseEventID(value string) (EventID, error) {
	if !validID(value) {
		return "", invalidID("event id", value)
	}
	return EventID(value), nil
}

// ParseReceiptID validates and converts a receipt identity.
func ParseReceiptID(value string) (ReceiptID, error) {
	if !validID(value) {
		return "", invalidID("receipt id", value)
	}
	return ReceiptID(value), nil
}

// ParseOperationID validates and converts an operation identity.
func ParseOperationID(value string) (OperationID, error) {
	if !validID(value) {
		return "", invalidID("operation id", value)
	}
	return OperationID(value), nil
}

// ParseWorkloadID validates and converts a workload identity.
func ParseWorkloadID(value string) (WorkloadID, error) {
	if !validID(value) {
		return "", invalidID("workload id", value)
	}
	return WorkloadID(value), nil
}

// ParseWorkspaceID validates and converts a workspace identity.
func ParseWorkspaceID(value string) (WorkspaceID, error) {
	if !validID(value) {
		return "", invalidID("workspace id", value)
	}
	return WorkspaceID(value), nil
}
