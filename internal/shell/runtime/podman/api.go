// Pinned Podman (libpod) runtime adapter API.
//
// This file declares the minimal libpod seam the adapter consumes and the
// adapter configuration surface. It is deliberately much smaller than the
// containers/podman Go client: the seam is an interface implemented by the
// production client and by the deterministic in-memory fake in tests. Nothing
// here opens a socket, starts a container or contacts a daemon.
package podman

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
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
	// MaxLogBytes bounds one log read per stream. A request above this bound is
	// refused before any libpod call.
	MaxLogBytes = 1 << 20
	// Redacted replaces configured credential and secret values in log output.
	Redacted = "[REDACTED]"
	// WorkloadCAPath is the read-only mounted CA bundle path inside the
	// container when Config.WorkloadCAFile is configured.
	WorkloadCAPath = "/kodama/ca.crt"
	// EnvWorkloadCAFile exposes WorkloadCAPath in the container environment
	// when Config.WorkloadCAFile is configured.
	EnvWorkloadCAFile = "KODAMA_CA_FILE"
)

// Container statuses the adapter interprets. Any other status stays unknown.
const (
	// ContainerStatusCreated is a container that exists and was never started.
	ContainerStatusCreated = "created"
	// ContainerStatusRunning is a container whose process may still be alive.
	ContainerStatusRunning = "running"
	// ContainerStatusExited is a container whose main process exited.
	ContainerStatusExited = "exited"
)

// Pinned identity labels shared by the generated container and the stable
// names the adapter derives, so a retry finds exactly the same object.
const (
	// LabelExecution names the execution.
	LabelExecution = "kodama.dev/execution"
	// LabelAction names the action.
	LabelAction = "kodama.dev/action"
	// LabelWorkload names the stable runtime workload identity.
	LabelWorkload = "kodama.dev/workload"
	// LabelWorkspace names the retained execution workspace.
	LabelWorkspace = "kodama.dev/workspace"
	// LabelIdempotencyKey carries the persisted idempotency key so a retry can
	// be attributed to the original effect.
	LabelIdempotencyKey = "kodama.dev/idempotency-key"
)

// Sentinel errors the Libpod seam wraps. The adapter interprets them; an absent
// object is never converted into proof that a command never ran or that a
// process stopped.
var (
	// ErrNotFound reports that an object does not exist.
	ErrNotFound = errors.New("podman: object not found")
	// ErrAlreadyExists reports that an object with the same name exists.
	ErrAlreadyExists = errors.New("podman: object already exists")
)

// Clock is the injected time source. Production uses the real clock; tests use
// a deterministic clock that never sleeps in wall-clock time.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// CleanupPolicy bounds one Remove call.
type CleanupPolicy struct {
	// Attempts is the total attempts per resource per Remove call; at least 1.
	Attempts int
	// Backoff is the delay between attempts, applied through Clock.Sleep.
	Backoff time.Duration
}

// Config binds one adapter instance to one adapter root.
type Config struct {
	// Libpod is the only container/volume effect surface.
	Libpod Libpod
	// Files is the host file/directory boundary.
	Files Files
	// Root is the daemon-visible adapter root; it must be absolute.
	Root string
	// KodamaURL is the reserved KODAMA_URL value injected into workloads.
	KodamaURL string
	// WorkloadCAFile optionally names an absolute host PEM CA bundle mounted
	// read-only at /kodama/ca.crt with KODAMA_CA_FILE=/kodama/ca.crt. Empty
	// mounts no CA file and sets no KODAMA_CA_FILE variable.
	WorkloadCAFile string
	// Cleanup bounds cleanup attempts per Remove call.
	Cleanup CleanupPolicy
	// Clock is optional; nil means a real clock.
	Clock Clock
}

// Mount is one container mount.
type Mount struct {
	// Type is "bind" or "volume".
	Type string
	// Source is a host path or a named volume.
	Source string
	// Destination is the in-container mount path.
	Destination string
	// ReadOnly reports whether the mount is read-only.
	ReadOnly bool
}

// ContainerSpec is the complete requested container.
type ContainerSpec struct {
	Name  string
	Image string
	// Entrypoint replaces the image entrypoint with the fail-fast script.
	Entrypoint []string
	// Env is "NAME=value" in declaration order, reserved context last.
	Env []string
	// Labels is the stable identity label set.
	Labels map[string]string
	// Mounts is the activation bind and the retained workspace volume.
	Mounts []Mount
	// RestartPolicy is "no": the command is never replayed automatically.
	RestartPolicy string
}

// ContainerState is the observed container. It separates "a start was accepted"
// from "a delayed start may still run" from affirmative whole-container stop
// evidence so uncertainty never collapses into success.
type ContainerState struct {
	ID           string
	Name         string
	Image        string
	Status       string
	Started      bool
	ExitCode     int
	StartPending bool
	WholeStopped bool
	Labels       map[string]string
}

// Volume is the retained named workspace volume.
type Volume struct {
	Name   string
	Labels map[string]string
}

// LogRequest selects one bounded stream read.
type LogRequest struct {
	Name   string
	Stream engine.LogStream
	Offset int64
	Limit  int
}

// LogPage is one bounded stream read result.
type LogPage struct {
	Data       []byte
	NextOffset int64
	Truncated  bool
}

// Files is the host file/directory boundary. Lstat never follows a symlink, so
// a symlink escape is observable instead of silently accepted. ReadDir reports
// entry type bits without following a symlinked entry, so a workspace reference
// scan can refuse to traverse an escape.
type Files interface {
	MkdirAll(context.Context, string, os.FileMode) error
	WriteFile(context.Context, string, []byte, os.FileMode) error
	ReadFile(context.Context, string) ([]byte, error)
	Lstat(context.Context, string) (os.FileInfo, error)
	ReadDir(context.Context, string) ([]os.DirEntry, error)
	Rename(context.Context, string, string) error
	RemoveAll(context.Context, string) error
}

// Libpod is the only external effect surface of the adapter.
type Libpod interface {
	CreateContainer(context.Context, ContainerSpec) (ContainerState, error)
	// InspectImage checks that a digest-pinned image reference is present in
	// the local container store. An absent image is ErrNotFound.
	InspectImage(context.Context, string) error
	InspectContainer(context.Context, string) (ContainerState, error)
	// ListContainers returns every container matching all supplied labels. It
	// is how workspace ownership is checked before a volume is released.
	ListContainers(context.Context, map[string]string) ([]ContainerState, error)
	StartContainer(context.Context, string) error
	StopContainer(context.Context, string) error
	RemoveContainer(context.Context, string) error
	ReadLogs(context.Context, LogRequest) (LogPage, error)
	InspectVolume(context.Context, string) (Volume, error)
	// RemoveVolume removes one named volume. An absent volume is ErrNotFound,
	// never a fabricated success.
	RemoveVolume(context.Context, string) error
}

// Adapter implements runtime.Client for the Podman container family.
type Adapter struct {
	libpod        Libpod
	files         Files
	root          string
	kodamaURL     string
	workloadCAPEM []byte
	cleanup       CleanupPolicy
	clock         Clock

	mu    sync.Mutex
	locks map[id.WorkloadID]*sync.Mutex

	workspaceMu    sync.Mutex
	workspaceLocks map[id.WorkspaceID]*sync.Mutex
}

// New validates one adapter configuration. An incomplete configuration is
// refused: the adapter never guesses a root, URL or cleanup bound.
func New(config Config) (*Adapter, error) {
	if config.Libpod == nil {
		return nil, fault.New(fault.InvalidInput, "podman adapter requires a libpod client")
	}
	if config.Files == nil {
		return nil, fault.New(fault.InvalidInput, "podman adapter requires a host file boundary")
	}
	if strings.TrimSpace(config.Root) == "" || !filepath.IsAbs(config.Root) {
		return nil, fault.New(fault.InvalidInput, "podman adapter requires an absolute adapter root")
	}
	if strings.TrimSpace(config.KodamaURL) == "" {
		return nil, fault.New(fault.InvalidInput, "podman adapter requires the Kodama URL")
	}
	if config.Cleanup.Attempts < 1 {
		return nil, fault.New(fault.InvalidInput, "podman adapter requires at least one cleanup attempt")
	}
	if config.Cleanup.Backoff < 0 {
		return nil, fault.New(fault.InvalidInput, "podman adapter cleanup backoff is negative")
	}
	var workloadCAPEM []byte
	if caFile := strings.TrimSpace(config.WorkloadCAFile); caFile != "" {
		loaded, err := loadWorkloadCAPEM(caFile)
		if err != nil {
			return nil, err
		}
		workloadCAPEM = loaded
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Adapter{
		libpod:        config.Libpod,
		files:         config.Files,
		root:          filepath.Clean(config.Root),
		kodamaURL:     config.KodamaURL,
		workloadCAPEM: workloadCAPEM,
		cleanup:       config.Cleanup,
		clock:         clock,
		locks:         map[id.WorkloadID]*sync.Mutex{},

		workspaceLocks: map[id.WorkspaceID]*sync.Mutex{},
	}, nil
}

func loadWorkloadCAPEM(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fault.New(fault.InvalidInput, "podman adapter workload CA file must be an absolute path")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput, "podman adapter workload CA file is unreadable")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "podman adapter workload CA file is unreadable")
	}
	// Workloads receive only the certificates: any other block in the file,
	// such as a private key, is dropped.
	var certs []byte
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			continue
		}
		certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
	}
	if len(certs) == 0 {
		return nil, fault.New(fault.InvalidInput, "podman adapter workload CA file is not a usable PEM certificate")
	}
	return certs, nil
}

// lockFor returns the per-workload mutex that serializes lifecycle effects on
// one stable identity inside this process. It never substitutes for the durable
// removal fence.
func (a *Adapter) lockFor(workload id.WorkloadID) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	lock := a.locks[workload]
	if lock == nil {
		lock = &sync.Mutex{}
		a.locks[workload] = lock
	}
	return lock
}

// lockForWorkspace returns the per-workspace mutex shared by every operation
// that attaches, detaches or observes one retained workspace volume. Lifecycle
// operations acquire the workload lock first and this lock second, so the
// ordering is consistent and a release can never interleave with a start.
func (a *Adapter) lockForWorkspace(workspace id.WorkspaceID) *sync.Mutex {
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	lock := a.workspaceLocks[workspace]
	if lock == nil {
		lock = &sync.Mutex{}
		a.workspaceLocks[workspace] = lock
	}
	return lock
}

// Version reports the pinned libpod API surface without contacting the daemon.
func (a *Adapter) Version(context.Context) (runtime.APIVersion, error) {
	return runtime.APIVersion{Adapter: runtime.AdapterPodman, Version: "libpod/v4"}, nil
}

// Capabilities reports the explicit Podman capability set. It is
// declaration-only and makes no libpod call.
func (a *Adapter) Capabilities(context.Context) (engine.Capabilities, error) {
	family := runtime.CapabilitiesFor(runtime.AdapterPodman)
	names := make([]string, len(family))
	for index, capability := range family {
		names[index] = string(capability)
	}
	return engine.Capabilities{Names: names}, nil
}

// CheckRequirements refuses admitted work the Podman family cannot run.
func CheckRequirements(required []string) error {
	supported := make(map[string]bool)
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterPodman) {
		supported[string(capability)] = true
	}
	for _, name := range required {
		if !supported[name] {
			return fault.New(fault.UnsupportedCapability, "podman adapter does not support required capability "+name)
		}
	}
	return nil
}

// ContainerName returns the deterministic container name for one stable
// workload. A retry must find exactly this container, never a generated one.
func ContainerName(workload id.WorkloadID) (string, error) {
	if _, err := id.ParseWorkloadID(string(workload)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workload id is invalid", err)
	}
	return "kodama-workload-" + stableDigest(string(workload)), nil
}

// WorkspaceVolumeName returns the deterministic name of one retained workspace
// volume. The Shell provisions the volume before Prepare; the adapter attaches
// exactly this volume and never creates or removes it.
func WorkspaceVolumeName(workspace id.WorkspaceID) (string, error) {
	if _, err := id.ParseWorkspaceID(string(workspace)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workspace id is invalid", err)
	}
	return "kodama-workspace-" + stableDigest(string(workspace)), nil
}

// stableDigest returns a short deterministic, DNS-safe digest of a stable
// identity. Names are derived only from persisted identities, never from
// randomness, so a retry finds the same object.
func stableDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:24]
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
