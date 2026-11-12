package recovery

import (
	"context"
	"path/filepath"
	"strings"
)

// ActorKind classifies one inventoried fence actor.
type ActorKind string

const (
	// ActorController is an old controller process.
	ActorController ActorKind = "controller"
	// ActorService is an enforcing service.
	ActorService ActorKind = "service"
	// ActorConsumer is a broker consumer.
	ActorConsumer ActorKind = "consumer"
)

// Actor is one explicitly inventoried container. Fence never discovers actors
// outside this list.
type Actor struct {
	Kind ActorKind
	Name string
}

// Executor runs one command inside one container. It is the only effect
// surface for pg_dump and pg_restore.
type Executor interface {
	Exec(context.Context, ExecRequest) (ExecResult, error)
}

// ExecRequest is one container command.
type ExecRequest struct {
	Container string
	Argv      []string
	Stdin     []byte
}

// ExecResult is one container command result.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// FileKind is the observed kind of one host path.
type FileKind string

const (
	// FileDirectory is a directory.
	FileDirectory FileKind = "directory"
	// FileRegular is a regular file.
	FileRegular FileKind = "regular"
)

// FileInfo is the observed host path metadata.
type FileInfo struct {
	Kind FileKind
	Size int64
}

// Files is the host file/directory boundary used by backup and restore.
type Files interface {
	// EnsureEmpty creates the destination when absent and refuses a non-empty
	// one.
	EnsureEmpty(context.Context, string) error
	// Snapshot copies one source tree into a fresh destination and reports the
	// number of regular files captured.
	Snapshot(context.Context, string, string) (int, error)
	// Restore copies one captured tree into an empty destination.
	Restore(context.Context, string, string) error
	// Stat observes one path without following a symlink.
	Stat(context.Context, string) (FileInfo, error)
	// ReadFile reads one regular file.
	ReadFile(context.Context, string) ([]byte, error)
	// WriteFile creates parent directories and replaces the target atomically.
	WriteFile(context.Context, string, []byte) error
	// RemoveAll removes one tree.
	RemoveAll(context.Context, string) error
}

// BackupSource is one durable family captured by a backup.
type BackupSource struct {
	Name  string
	Root  string
	Ready func(context.Context) error
}

// Workspace is one disposable workspace. Its root is never captured; only the
// identity is recorded so a restore can report it as unrecoverable.
type Workspace struct {
	ID   string
	Root string
}

// RestoreTarget is one durable family destination.
type RestoreTarget struct {
	Name string
	Root string
}

// WorkloadIdentity is one explicit existing workload observed during
// reconciliation.
type WorkloadIdentity struct {
	ID string
}

// ObservationState is the observed state of one workload identity.
type ObservationState string

const (
	// ObservationAbsent is an identity with no observable workload.
	ObservationAbsent ObservationState = "absent"
	// ObservationRunning is an identity whose workload may still effect
	// changes.
	ObservationRunning ObservationState = "running"
	// ObservationStopped is an identity with affirmative whole-container stop
	// evidence.
	ObservationStopped ObservationState = "stopped"
	// ObservationUnknown is an identity whose state cannot be observed.
	ObservationUnknown ObservationState = "unknown"
)

// WorkloadObservation is the observed state of one workload identity.
type WorkloadObservation struct {
	Identity         WorkloadIdentity
	State            ObservationState
	WorkspaceID      string
	WorkspacePresent bool
	Detail           string
}

// Observer observes only explicit workload identities.
type Observer interface {
	Observe(context.Context, WorkloadIdentity) (WorkloadObservation, error)
}

// validName reports whether name is a single, non-special path component. It
// keeps a caller-supplied identity from escaping the configured root.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return filepath.Base(name) == name && !filepath.IsAbs(name)
}

// copyStrings returns an independent copy of values, preserving nil.
func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}
