// Podman runtime adapter API pinned by these tests.
//
// This file is the test-side declaration of the minimal Podman (libpod) API
// seam that the production adapter under internal/shell/runtime/podman
// implements. It imports no libpod/containers module, opens no socket and
// starts no container: every test drives the deterministic in-memory libpod,
// file and clock fakes in fake_test.go.
//
// The adapter implements the declared runtime.Client seam exactly:
//
//	Capabilities(context.Context) (engine.Capabilities, error)
//	Prepare(context.Context, engine.PrepareRequest) (engine.Preparation, error)
//	Start(context.Context, engine.StartRequest) (engine.StartResult, error)
//	Observe(context.Context, engine.ObserveRequest) (kernel.Observation, error)
//	Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error)
//	Stop(context.Context, engine.StopRequest) (kernel.StopEvidence, error)
//	Remove(context.Context, engine.RemoveRequest) (engine.Removal, error)
//	RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error)
//	Version(context.Context) (runtime.APIVersion, error)
//
// It also implements the declared Kernel engine.Workspace port:
//
//	Status(context.Context, id.WorkspaceID) (engine.WorkspaceObservation, error)
//	Retain(context.Context, id.WorkspaceID) error
//	Release(context.Context, id.WorkspaceID) error
//
// Pinned package declarations (in api.go, adapter.go, evidence.go, manifest.go
// and doc.go):
//
//	func New(Config) (*Adapter, error)
//	type Adapter struct{ ... }                      // implements runtime.Client
//	func CheckRequirements([]string) error          // fault.UnsupportedCapability
//	func ContainerName(id.WorkloadID) (string, error)
//	func WorkspaceVolumeName(id.WorkspaceID) (string, error)
//
//	const MaxLogBytes = 1 << 20                     // bounded log read per stream
//	const Redacted = "[REDACTED]"
//	const (
//	    ContainerStatusCreated = "created"          // created, never started
//	    ContainerStatusRunning = "running"
//	    ContainerStatusExited  = "exited"
//	)
//	const (
//	    LabelExecution      = "kodama.dev/execution"
//	    LabelAction         = "kodama.dev/action"
//	    LabelWorkload       = "kodama.dev/workload"
//	    LabelWorkspace      = "kodama.dev/workspace"
//	    LabelIdempotencyKey = "kodama.dev/idempotency-key"
//	)
//
//	var (
//	    ErrNotFound      = errors.New("podman: object not found")
//	    ErrAlreadyExists = errors.New("podman: object already exists")
//	)
//
//	type Config struct {
//	    Libpod    Libpod       // the only container/volume effect surface
//	    Files     Files        // host file/directory boundary
//	    Root      string       // daemon-visible adapter root; absolute
//	    KodamaURL string       // reserved KODAMA_URL value
//	    Cleanup   CleanupPolicy
//	    Clock     Clock        // optional; nil means a real clock
//	}
//	type CleanupPolicy struct {
//	    Attempts int           // total attempts per resource per Remove call, >= 1
//	    Backoff  time.Duration // delay between attempts via Clock.Sleep
//	}
//	type Clock interface {
//	    Now() time.Time
//	    Sleep(context.Context, time.Duration) error
//	}
//
//	type Libpod interface {
//	    CreateContainer(context.Context, ContainerSpec) (ContainerState, error)
//	    InspectImage(context.Context, string) error
//	    InspectContainer(context.Context, string) (ContainerState, error)
//	    ListContainers(context.Context, map[string]string) ([]ContainerState, error)
//	    StartContainer(context.Context, string) error
//	    StopContainer(context.Context, string) error
//	    RemoveContainer(context.Context, string) error
//	    ReadLogs(context.Context, LogRequest) (LogPage, error)
//	    InspectVolume(context.Context, string) (Volume, error)
//	    RemoveVolume(context.Context, string) error // absent is ErrNotFound
//	}
//	type ContainerSpec struct {
//	    Name          string
//	    Image         string
//	    Entrypoint    []string          // exactly {script.ShellPath, "/kodama/script"}
//	    Env           []string          // "NAME=value" in declaration order, reserved last
//	    Labels        map[string]string // stable identity labels
//	    Mounts        []Mount
//	    RestartPolicy string            // "no": never replay automatically
//	}
//	type Mount struct {
//	    Type        string // "bind" or "volume"
//	    Source      string
//	    Destination string
//	    ReadOnly    bool
//	}
//	type ContainerState struct {
//	    ID           string
//	    Name         string
//	    Image        string
//	    Status       string // ContainerStatusCreated/Running/Exited
//	    Started      bool   // a start was accepted at least once
//	    ExitCode     int
//	    StartPending bool   // accepted start not settled: a delayed start may still run
//	    WholeStopped bool   // affirmative evidence every container process exited
//	    Labels       map[string]string
//	}
//	type Volume struct {
//	    Name   string
//	    Labels map[string]string
//	}
//	type LogRequest struct {
//	    Name   string
//	    Stream engine.LogStream
//	    Offset int64
//	    Limit  int
//	}
//	type LogPage struct {
//	    Data       []byte
//	    NextOffset int64
//	    Truncated  bool
//	}
//	type Files interface {
//	    MkdirAll(context.Context, string, os.FileMode) error
//	    WriteFile(context.Context, string, []byte, os.FileMode) error
//	    ReadFile(context.Context, string) ([]byte, error)
//	    Lstat(context.Context, string) (os.FileInfo, error) // never follows a symlink
//	    ReadDir(context.Context, string) ([]os.DirEntry, error) // never follows a symlink
//	    Rename(context.Context, string, string) error
//	    RemoveAll(context.Context, string) error
//	}
//
// Host layout for one stable identity, rooted at
// <Config.Root>/<Identity.WorkloadID>:
//
//	<root>/<workload>/kodama/inputs/<key>              exact admitted input bytes
//	<root>/<workload>/kodama/outputs/<action>/<key>    earlier succeeded outputs only
//	<root>/<workload>/kodama/task.json                 only when the request has a task
//	<root>/<workload>/kodama/secrets/<NAME>            declared activation-time values
//	<root>/<workload>/kodama/secrets/KODAMA_TOKEN      the sole mutable mounted file
//	<root>/<workload>/kodama/script                    script.Render output
//
// Every materialized logical path P under /kodama is written at
// filepath.Join(<root>/<workload>, P). Prepared files are regular read-only
// (0o444) files. The generated script is <root>/<workload>/kodama/script. The
// execution workspace is the pre-existing named Podman volume returned by
// WorkspaceVolumeName(WorkspaceID); Prepare and Start attach it at /workspace
// and never create it. Only the separate engine.Workspace Release operation may
// remove it, and only after every reference and retention guard is clear.
//
// The container mounts exactly two things: the directory
// <root>/<workload>/kodama as a read-only bind at /kodama (a DIRECTORY bind so
// an atomic token-file replacement is visible without a restart) and the
// retained workspace volume read-write at /workspace.
//
// Behavior rules these tests pin (S02 §5–6 and S07 §1–3):
//   - Version/Capabilities/CheckRequirements are declaration-only and make no
//     libpod call. The family pins libpod/v4 and exactly the container,
//     retained-workspace, credential-refresh and process-group-stop
//     capabilities; unsupported admitted requirements are refused before
//     dispatch with unsupported_capability.
//   - Prepare reuses workspace.Materialization.Validate and script.Render. It
//     requires a digest-pinned image, a non-empty valid command list, a valid
//     action slug/task id, a non-empty credential, an existing workspace volume
//     owned by the workspace label and confined, non-symlinked read-only files.
//     It writes the activation snapshot once, is idempotent for unchanged
//     content, refuses changed content for the same identity with
//     changed_idempotency_content, and never starts a container. A partial or
//     unsafe preparation never authorizes a start.
//   - The container spec carries the pinned image, the rendered fail-fast
//     entrypoint, the admitted literal environment in declaration order
//     followed by the unshadowable reserved context KODAMA_URL,
//     KODAMA_EXECUTION, KODAMA_ACTION (the action slug, never the action id)
//     and KODAMA_TASK (omitted when task-free), the stable labels and
//     RestartPolicy "no". Secrets and the credential never appear in argv,
//     environment, labels or diagnostics.
//   - Start consumes the persisted engine.EffectIdentity and the completed
//     on-disk preparation. A lost create or start response is StartUnknown and
//     is never replayed as a fresh command: a retry inspects the same stable
//     container and reuses it. Start reports StartProvenNotStarted only for an
//     affirmative never-started observation with no delayed start pending, and
//     StartUnknown for any ambiguous outcome or cancellation.
//   - Observe reports the declared Kernel vocabulary. never_started requires
//     affirmative proof: either a created container with no accepted start and
//     no pending start, or an absent container whose per-workload durable facts
//     show StartContainer could never have been issued (no preparation record,
//     or a preparation record with no start-attempt marker). An absent
//     container with a recorded start attempt stays absent: absence alone is
//     never proof. running, exited(status) and unknown follow the daemon
//     evidence. A confirmed stop requires affirmative whole-container
//     termination (exited AND WholeStopped); a stop acknowledgment alone never
//     confirms.
//   - Remove is idempotent, identity-scoped and bounded by Cleanup.Attempts
//     with Clock.Sleep(Backoff) between attempts. It removes only that
//     identity's container and materialization, writes a durable removal fence
//     outside the removed root, and NEVER removes or writes the retained
//     workspace volume. A live or unconfirmed container retains every owned
//     resource and names the container. Exhaustion returns Removed=false with a
//     detail naming the retained resource, never a false success. A removed
//     identity is refused by Prepare/Start even after adapter reconstruction,
//     and Observe reports absent.
//   - Workspace is separate from Runtime. Status reports availability only for
//     an existing volume labelled for that workspace; a missing, foreign or
//     unlabelled volume is unavailable evidence and is never recreated. Retain
//     is idempotent and persists a durable retained marker, so a stale Release
//     cannot drop a retained workspace across adapter reconstruction. Release
//     removes the volume only when it is owned, unretained and unreferenced by
//     any container or start-attempt marker; an absent volume is idempotent
//     success, and an uncertain removal returns an error without fabricating
//     success. Status/Retain/Release never touch another identity's volume.
//   - RefreshCredential atomically replaces only
//     <root>/<workload>/kodama/secrets/KODAMA_TOKEN (write-temp-then-rename)
//     and never creates, starts, stops or removes the container. A failed or
//     lost replacement leaves the previous token intact and a retry recovers.
//   - Logs are ordered stdout/stderr chunks with a single continuation token
//     packing the stdout position in the high 32 bits and the stderr position
//     in the low 32 bits, so a later read never skips the quieter stream. Each
//     read is bounded by Limit, redacts configured secret/credential values and
//     fails closed when the redaction source is unavailable. Logs carry no
//     lifecycle authority.
package podman

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

func TestAdapterPinsLibpodAPIVersion(t *testing.T) {
	libpod := newFakeLibpod()
	adapter, err := New(Config{
		Libpod:    libpod,
		Files:     newFakeFiles(),
		Root:      t.TempDir(),
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 1},
		Clock:     newFakeClock(),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	version, err := adapter.Version(context.Background())
	if err != nil {
		t.Fatalf("Version returned error: %v", err)
	}
	if version.Adapter != runtime.AdapterPodman {
		t.Fatalf("version adapter = %q, want %q", version.Adapter, runtime.AdapterPodman)
	}
	if version.Version != "libpod/v4" {
		t.Fatalf("version = %q, want libpod/v4", version.Version)
	}
	if calls := libpod.callTotal(); calls != 0 {
		t.Fatalf("Version made %d libpod calls, want 0", calls)
	}

	// The runtime contract pins the same version for the Podman family, so the
	// adapter and the contract cannot drift apart.
	var pinned bool
	for _, candidate := range runtime.PinnedAPIVersions() {
		if candidate.Adapter != runtime.AdapterPodman {
			continue
		}
		pinned = true
		if candidate.Version != version.Version {
			t.Fatalf("pinned podman version = %q, want %q", candidate.Version, version.Version)
		}
	}
	if !pinned {
		t.Fatal("runtime contract has no pinned podman API version")
	}
}

func TestAdapterCapabilitiesAreExplicit(t *testing.T) {
	libpod := newFakeLibpod()
	adapter, err := New(Config{
		Libpod:    libpod,
		Files:     newFakeFiles(),
		Root:      t.TempDir(),
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 1},
		Clock:     newFakeClock(),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities returned error: %v", err)
	}
	want := []string{
		string(runtime.CapabilityContainer),
		string(runtime.CapabilityRetainedWorkspace),
		string(runtime.CapabilityCredentialRefresh),
		string(runtime.CapabilityProcessGroupStop),
	}
	if len(capabilities.Names) != len(want) {
		t.Fatalf("capabilities = %v, want exactly %v", capabilities.Names, want)
	}
	for _, name := range want {
		if !capabilities.Supports(name) {
			t.Fatalf("capabilities %v missing %q", capabilities.Names, name)
		}
	}
	for _, forbidden := range []string{
		string(runtime.CapabilityProcess),
		string(runtime.CapabilityNetworkIsolation),
		string(runtime.CapabilityJobDeletionNotStopProof),
	} {
		if capabilities.Supports(forbidden) {
			t.Fatalf("podman adapter claims %q", forbidden)
		}
	}
	if calls := libpod.callTotal(); calls != 0 {
		t.Fatalf("Capabilities made %d libpod calls, want 0", calls)
	}
}

func TestCheckRequirementsRefusesUnsupportedWork(t *testing.T) {
	libpod := newFakeLibpod()
	// CheckRequirements is a pure declaration check; no adapter or libpod call
	// is needed to exercise it.
	for _, name := range []string{
		string(runtime.CapabilityContainer),
		string(runtime.CapabilityRetainedWorkspace),
		string(runtime.CapabilityCredentialRefresh),
		string(runtime.CapabilityProcessGroupStop),
	} {
		if err := CheckRequirements([]string{name}); err != nil {
			t.Fatalf("CheckRequirements(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		string(runtime.CapabilityProcess),
		string(runtime.CapabilityNetworkIsolation),
		string(runtime.CapabilityJobDeletionNotStopProof),
		"not-a-capability",
	} {
		err := CheckRequirements([]string{name})
		assertFaultClass(t, err, fault.UnsupportedCapability)
	}
	if calls := libpod.callTotal(); calls != 0 {
		t.Fatalf("CheckRequirements made %d libpod calls, want 0", calls)
	}
}

func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	valid := func() Config {
		return Config{
			Libpod:    newFakeLibpod(),
			Files:     newFakeFiles(),
			Root:      t.TempDir(),
			KodamaURL: testKodamaURL,
			Cleanup:   CleanupPolicy{Attempts: 2, Backoff: time.Second},
			Clock:     newFakeClock(),
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing libpod", func(config *Config) { config.Libpod = nil }},
		{"missing files", func(config *Config) { config.Files = nil }},
		{"relative root", func(config *Config) { config.Root = "relative/path" }},
		{"empty root", func(config *Config) { config.Root = "" }},
		{"empty kodama url", func(config *Config) { config.KodamaURL = "  " }},
		{"zero attempts", func(config *Config) { config.Cleanup.Attempts = 0 }},
		{"negative backoff", func(config *Config) { config.Cleanup.Backoff = -time.Second }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := valid()
			testCase.mutate(&config)
			if adapter, err := New(config); err == nil || adapter != nil {
				t.Fatalf("New(%s) = (%v, %v), want refusal", testCase.name, adapter, err)
			} else {
				assertFaultClass(t, err, fault.InvalidInput)
			}
		})
	}
	config := valid()
	if adapter, err := New(config); err != nil || adapter == nil {
		t.Fatalf("New(valid) = (%v, %v), want an adapter", adapter, err)
	}
}

func TestStableIdentityNames(t *testing.T) {
	first, err := ContainerName("workload-1")
	if err != nil {
		t.Fatalf("ContainerName returned error: %v", err)
	}
	again, err := ContainerName("workload-1")
	if err != nil {
		t.Fatalf("ContainerName returned error: %v", err)
	}
	other, err := ContainerName("workload-2")
	if err != nil {
		t.Fatalf("ContainerName returned error: %v", err)
	}
	if first != again {
		t.Fatalf("ContainerName is not stable: %q != %q", first, again)
	}
	if first == other {
		t.Fatalf("distinct workloads share container name %q", first)
	}
	if first == "" || strings.ToLower(first) != first || strings.ContainsAny(first, " /:@") {
		t.Fatalf("container name %q is not a safe stable name", first)
	}
	if _, err := ContainerName(""); err == nil {
		t.Fatal("ContainerName accepted an empty workload id")
	}

	volume, err := WorkspaceVolumeName("workspace-1")
	if err != nil {
		t.Fatalf("WorkspaceVolumeName returned error: %v", err)
	}
	volumeAgain, err := WorkspaceVolumeName("workspace-1")
	if err != nil {
		t.Fatalf("WorkspaceVolumeName returned error: %v", err)
	}
	otherVolume, err := WorkspaceVolumeName("workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceVolumeName returned error: %v", err)
	}
	if volume != volumeAgain {
		t.Fatalf("WorkspaceVolumeName is not stable: %q != %q", volume, volumeAgain)
	}
	if volume == otherVolume {
		t.Fatalf("distinct workspaces share volume name %q", volume)
	}
	if volume == "" || strings.ToLower(volume) != volume || strings.ContainsAny(volume, " /:@") {
		t.Fatalf("volume name %q is not a safe stable name", volume)
	}
	if _, err := WorkspaceVolumeName(""); err == nil {
		t.Fatal("WorkspaceVolumeName accepted an empty workspace id")
	}
}

func TestPinnedLabelVocabulary(t *testing.T) {
	want := map[string]string{
		"LabelExecution":      "kodama.dev/execution",
		"LabelAction":         "kodama.dev/action",
		"LabelWorkload":       "kodama.dev/workload",
		"LabelWorkspace":      "kodama.dev/workspace",
		"LabelIdempotencyKey": "kodama.dev/idempotency-key",
	}
	got := map[string]string{
		"LabelExecution":      LabelExecution,
		"LabelAction":         LabelAction,
		"LabelWorkload":       LabelWorkload,
		"LabelWorkspace":      LabelWorkspace,
		"LabelIdempotencyKey": LabelIdempotencyKey,
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q", name, got[name], value)
		}
	}
}

var _ runtime.Client = (*Adapter)(nil)
var _ engine.Workspace = (*Adapter)(nil)
