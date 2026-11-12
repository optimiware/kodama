// Pinned Process runtime adapter API.
//
// This file declares the minimal host-process seam the adapter consumes and the
// adapter configuration surface. It is intentionally small: the Executor,
// Observer and Files interfaces are implemented by the production host
// boundary and by the deterministic in-memory fakes in tests. Nothing here
// starts a process, invokes a shell or opens a host path.
package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

const (
	// MaxLogBytes bounds one log read. A request above this bound is refused
	// before any external call.
	MaxLogBytes = 1 << 20
	// Redacted replaces configured credential and secret values in log output.
	Redacted = "[REDACTED]"
)

// GroupID is the stable identity of one host process group.
type GroupID string

// SpawnOutcome classifies one spawn attempt. Only SpawnRefused is affirmative
// proof that no process was created.
type SpawnOutcome string

const (
	// SpawnStarted means the executor created the process group.
	SpawnStarted SpawnOutcome = "started"
	// SpawnRefused means the executor affirmatively did not create a process.
	SpawnRefused SpawnOutcome = "refused"
	// SpawnUncertain means the spawn outcome cannot be classified.
	SpawnUncertain SpawnOutcome = "uncertain"
)

// SpawnRequest is one complete, confined process-group launch request. It
// carries no secret material: secrets are only mounted as files.
type SpawnRequest struct {
	Identity   engine.EffectIdentity
	Group      GroupID
	Command    []string
	Env        []string
	Dir        string
	Root       string
	StdoutPath string
	StderrPath string
}

// SpawnResult is the executor's structured spawn outcome.
type SpawnResult struct {
	Outcome SpawnOutcome
	Group   GroupID
	Detail  string
}

// Executor is the only process-effect boundary. Implementations may use
// os/exec; unit tests use a deterministic fake.
type Executor interface {
	Spawn(context.Context, SpawnRequest) (SpawnResult, error)
	SignalGroup(context.Context, GroupID) error
	RemoveGroup(context.Context, GroupID) error
}

// GroupStatus is the observed state of one process group. NeverCreated and
// GroupGone are affirmative evidence; zero values prove nothing.
type GroupStatus struct {
	Present      bool
	NeverCreated bool
	LeaderExited bool
	ExitCode     int
	MembersAlive bool
	GroupGone    bool
}

// GroupLogsRequest selects one bounded stream read.
type GroupLogsRequest struct {
	Group  GroupID
	Stream engine.LogStream
	Offset int64
	Limit  int
}

// GroupLogPage is one bounded stream read result.
type GroupLogPage struct {
	Data      []byte
	Truncated bool
}

// Observer is the process-group evidence and log boundary.
type Observer interface {
	Status(context.Context, GroupID) (GroupStatus, error)
	Logs(context.Context, GroupLogsRequest) (GroupLogPage, error)
}

// Files is the host file/directory boundary. Stat is lstat: it never follows a
// symlink. ReadDir reports entry type bits without following a symlinked entry,
// so the workspace reference scan can refuse to traverse an escape.
type Files interface {
	MkdirAll(context.Context, string, os.FileMode) error
	WriteFile(context.Context, string, []byte, os.FileMode) error
	ReadFile(context.Context, string) ([]byte, error)
	Stat(context.Context, string) (os.FileInfo, error)
	ReadDir(context.Context, string) ([]os.DirEntry, error)
	Rename(context.Context, string, string) error
	RemoveAll(context.Context, string) error
}

// Clock is the injected time source. Production uses the real clock; tests use
// a deterministic clock that never sleeps in wall-clock time.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// CleanupPolicy bounds one Remove call.
type CleanupPolicy struct {
	// Attempts is the total RemoveAll attempts per Remove call; at least 1.
	Attempts int
	// Backoff is the delay between attempts, applied through Clock.Sleep.
	Backoff time.Duration
}

// Config binds one adapter instance to disjoint resource and workspace roots.
type Config struct {
	// Root is the adapter-owned resource root; absolute.
	Root string
	// WorkspaceRoot is the parent of existing workspace directories; absolute.
	WorkspaceRoot string
	// KodamaURL is the reserved KODAMA_URL value injected into workloads.
	KodamaURL string
	// Executor is the process effect boundary.
	Executor Executor
	// Observer is the process-group evidence boundary.
	Observer Observer
	// Files is the host file/directory boundary.
	Files Files
	// Clock is optional; nil means a real clock.
	Clock Clock
	// Cleanup bounds cleanup attempts per Remove call.
	Cleanup CleanupPolicy
}

// Adapter implements runtime.Client for the development Process family.
type Adapter struct {
	root          string
	workspaceRoot string
	kodamaURL     string
	executor      Executor
	observer      Observer
	files         Files
	clock         Clock
	cleanup       CleanupPolicy

	mu sync.Mutex
}

// New validates one adapter configuration. An incomplete or ambiguous
// configuration is refused: the adapter never guesses a root, URL or cleanup
// bound.
func New(config Config) (*Adapter, error) {
	if config.Executor == nil {
		return nil, fault.New(fault.InvalidInput, "process adapter requires an executor")
	}
	if config.Observer == nil {
		return nil, fault.New(fault.InvalidInput, "process adapter requires an observer")
	}
	if config.Files == nil {
		return nil, fault.New(fault.InvalidInput, "process adapter requires a file boundary")
	}
	if !filepath.IsAbs(config.Root) {
		return nil, fault.New(fault.InvalidInput, "process adapter requires an absolute resource root")
	}
	if !filepath.IsAbs(config.WorkspaceRoot) {
		return nil, fault.New(fault.InvalidInput, "process adapter requires an absolute workspace root")
	}
	root := filepath.Clean(config.Root)
	workspaceRoot := filepath.Clean(config.WorkspaceRoot)
	if root == workspaceRoot || pathWithin(root, workspaceRoot) || pathWithin(workspaceRoot, root) {
		return nil, fault.New(fault.InvalidInput, "process adapter resource and workspace roots must be disjoint")
	}
	if strings.TrimSpace(config.KodamaURL) == "" {
		return nil, fault.New(fault.InvalidInput, "process adapter requires the Kodama URL")
	}
	if config.Cleanup.Attempts < 1 {
		return nil, fault.New(fault.InvalidInput, "process adapter requires at least one cleanup attempt")
	}
	if config.Cleanup.Backoff < 0 {
		return nil, fault.New(fault.InvalidInput, "process adapter cleanup backoff is negative")
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Adapter{
		root:          root,
		workspaceRoot: workspaceRoot,
		kodamaURL:     config.KodamaURL,
		executor:      config.Executor,
		observer:      config.Observer,
		files:         config.Files,
		clock:         clock,
		cleanup:       config.Cleanup,
	}, nil
}

// Version reports the pinned Process API surface without any external call.
func (a *Adapter) Version(context.Context) (runtime.APIVersion, error) {
	return runtime.APIVersion{Adapter: runtime.AdapterProcess, Version: "process/v1"}, nil
}

// Capabilities reports the explicit Process capability set. It is
// declaration-only and makes no boundary call.
func (a *Adapter) Capabilities(context.Context) (engine.Capabilities, error) {
	family := runtime.CapabilitiesFor(runtime.AdapterProcess)
	names := make([]string, len(family))
	for index, capability := range family {
		names[index] = string(capability)
	}
	return engine.Capabilities{Names: names}, nil
}

// CheckRequirements refuses admitted work the Process family cannot run.
func CheckRequirements(required []string) error {
	supported := make(map[string]bool)
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterProcess) {
		supported[string(capability)] = true
	}
	for _, name := range required {
		if !supported[name] {
			return fault.New(fault.UnsupportedCapability, "process adapter does not support required capability "+name)
		}
	}
	return nil
}

// GroupName returns the stable process-group name for one workload identity.
func GroupName(workload id.WorkloadID) (GroupID, error) {
	if _, err := id.ParseWorkloadID(string(workload)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workload id is invalid", err)
	}
	return GroupID("kodama-process-" + stableDigest(string(workload))), nil
}

// ResourceRoot returns the adapter-owned resource root for one workload
// identity. It is derived only from the configured root and the persisted
// identity so a restart finds the same resources.
func ResourceRoot(root string, workload id.WorkloadID) (string, error) {
	if _, err := id.ParseWorkloadID(string(workload)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workload id is invalid", err)
	}
	return filepath.Join(root, "workloads", string(workload)), nil
}

// WorkspaceDir returns the existing execution workspace directory. The adapter
// attaches it and never creates, writes or removes it.
func WorkspaceDir(workspaceRoot string, workspace id.WorkspaceID) (string, error) {
	if _, err := id.ParseWorkspaceID(string(workspace)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workspace id is invalid", err)
	}
	return filepath.Join(workspaceRoot, string(workspace)), nil
}

// stableDigest returns a short deterministic, path-safe digest of a stable
// identity.
func stableDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:24]
}

// pathWithin reports whether child is parent or below it.
func pathWithin(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(os.PathSeparator))
}

// realClock is the production clock. It never appears in unit tests.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// Sleep waits for the duration or until the context ends.
func (realClock) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ runtime.Client = (*Adapter)(nil)
