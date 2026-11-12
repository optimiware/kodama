// Development-only Process runtime adapter API pinned by these tests.
//
// This file is the test-side declaration of the minimal host process seam that
// the production adapter under internal/shell/runtime/process implements. It
// starts no process, invokes no shell, opens no socket and writes no host path
// outside t.TempDir(): every test drives the deterministic in-memory executor,
// observer, file and clock fakes in fake_test.go.
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
// It also implements the declared Kernel engine.Workspace port over the same
// confined roots and Files seam:
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
//	func GroupName(id.WorkloadID) (GroupID, error)
//	func ResourceRoot(root string, workload id.WorkloadID) (string, error)
//	func WorkspaceDir(workspaceRoot string, workspace id.WorkspaceID) (string, error)
//
//	const MaxLogBytes = 1 << 20                      // bounded log read
//	const Redacted = "[REDACTED]"
//
//	type Config struct {
//	    Root          string        // adapter-owned resource root; absolute
//	    WorkspaceRoot string        // parent of existing workspace dirs; absolute
//	    KodamaURL     string        // reserved KODAMA_URL value
//	    Executor      Executor      // process effect boundary
//	    Observer      Observer      // process-group evidence boundary
//	    Files         Files         // host file/directory boundary
//	    Clock         Clock         // optional; nil means a real clock
//	    Cleanup       CleanupPolicy // attempts per Remove call, >= 1
//	}
//	type CleanupPolicy struct {
//	    Attempts int
//	    Backoff  time.Duration
//	}
//	type Clock interface {
//	    Now() time.Time
//	    Sleep(context.Context, time.Duration) error
//	}
//
//	type Executor interface {
//	    Spawn(context.Context, SpawnRequest) (SpawnResult, error)
//	    SignalGroup(context.Context, GroupID) error
//	    RemoveGroup(context.Context, GroupID) error
//	}
//	type GroupID string
//	type SpawnOutcome string
//	const (
//	    SpawnStarted   SpawnOutcome = "started"
//	    SpawnRefused   SpawnOutcome = "refused"
//	    SpawnUncertain SpawnOutcome = "uncertain"
//	)
//	type SpawnRequest struct {
//	    Identity   engine.EffectIdentity
//	    Group      GroupID
//	    Command    []string // generated fail-fast argv
//	    Env        []string // "NAME=value" in declaration order
//	    Dir        string   // the execution's existing workspace directory
//	    Root       string   // this identity's adapter-owned resource root
//	    StdoutPath string
//	    StderrPath string
//	}
//	type SpawnResult struct {
//	    Outcome SpawnOutcome
//	    Group   GroupID
//	    Detail  string
//	}
//
//	type Observer interface {
//	    Status(context.Context, GroupID) (GroupStatus, error)
//	    Logs(context.Context, GroupLogsRequest) (GroupLogPage, error)
//	}
//	type GroupStatus struct {
//	    Present      bool // the group object still exists
//	    NeverCreated bool // affirmative evidence no process was ever created
//	    LeaderExited bool // the group leader terminated
//	    ExitCode     int  // the leader's status when LeaderExited
//	    MembersAlive bool // at least one group member is still alive
//	    GroupGone    bool // affirmative evidence the WHOLE group terminated
//	}
//	type GroupLogsRequest struct {
//	    Group  GroupID
//	    Stream engine.LogStream
//	    Offset int64
//	    Limit  int
//	}
//	type GroupLogPage struct {
//	    Data      []byte
//	    Truncated bool
//	}
//
//	type Files interface {
//	    MkdirAll(context.Context, string, os.FileMode) error
//	    WriteFile(context.Context, string, []byte, os.FileMode) error
//	    ReadFile(context.Context, string) ([]byte, error)
//	    Stat(context.Context, string) (os.FileInfo, error) // lstat: never follows a symlink
//	    ReadDir(context.Context, string) ([]os.DirEntry, error) // no symlink traversal
//	    Rename(context.Context, string, string) error
//	    RemoveAll(context.Context, string) error
//	}
//
// Resource layout for one stable identity, rooted at
// ResourceRoot(Config.Root, Identity.WorkloadID):
//
//	<root>/kodama/inputs/<key>              exact admitted input bytes
//	<root>/kodama/outputs/<action>/<key>    earlier succeeded outputs only
//	<root>/kodama/task.json                 only when the request has a task
//	<root>/kodama/secrets/<NAME>            declared activation-time values
//	<root>/kodama/secrets/KODAMA_TOKEN      the sole mutable mounted file
//	<root>/script                           internal/kernel/script.Render output
//	<root>/logs/stdout, <root>/logs/stderr  the process group's log streams
//	<root>/group                            persisted spawn intent/bookkeeping
//
// Every materialized logical path P under /kodama is written at
// filepath.Join(ResourceRoot(...), P). The generated script is
// <root>/script, logs are <root>/logs/{stdout,stderr} and group bookkeeping is
// <root>/group. Prepared files are regular files with mode 0o444; the mutable
// credential is replaced in place without changing its read-only mode. The
// execution workspace is WorkspaceDir(Config.WorkspaceRoot, WorkspaceID) and is
// attached, never created, never written and never removed by this adapter.
//
// Behavior rules these tests pin (all from S02 §5–6 and S07 §1–3):
//   - Prepare materializes one confined, all-or-nothing activation snapshot
//     outside the workspace, attaches the existing workspace directory, and
//     refuses traversal, symlink escape, reserved-name collision, a missing
//     workspace and partial preparation. Re-preparing the same identity with
//     unchanged content is idempotent; changed content reports
//     changed_idempotency_content. Preparation never starts a process.
//   - The generated fail-fast script is exactly internal/kernel/script.Render of
//     the declared commands (byte/order/quoting preserving, no tracing) and the
//     argv is exactly {script.ShellPath, <root>/script}. The environment is the
//     admitted literal environment in declaration order followed by the
//     unshadowable reserved context KODAMA_URL, KODAMA_EXECUTION,
//     KODAMA_ACTION (the action slug, never the action id) and KODAMA_TASK
//     (omitted when task-free); a declaration that tries to set a reserved name
//     is refused. Secrets and the credential never appear in argv, environment,
//     workspace files, log output or diagnostics.
//   - Start reuses the persisted identity and idempotency key. A retry, a lost
//     response or concurrent duplicate starts never spawn a second process; a
//     fresh adapter recovers the existing group by identity. Start reports
//     StartProvenNotStarted only when the executor affirmatively reports the
//     spawn never happened, StartUnknown for any ambiguous outcome or
//     cancellation at the barrier, and StartAccepted when the group exists.
//   - Observe reports the declared Kernel vocabulary. never_started requires an
//     affirmative never-created proof for a prepared identity. absent and
//     unknown never prove nonexecution. exited(status) and a confirmed stop
//     require affirmative whole-group termination; a leader exit or a signal
//     acknowledgment alone never confirms it.
//   - Stop requests whole-process-group termination. Acknowledgment is not
//     confirmation; a lost response stays pending until affirmative evidence
//     arrives.
//   - Remove is idempotent, removes only the identity's own resource root with
//     one bounded, retried RemoveAll (never the workspace) and refuses/retains a
//     started identity without confirmed whole-group termination, naming the
//     retained group. A prepared never-started identity is removable without
//     stop evidence. Once removal completes, Prepare and Start for that identity
//     are refused and Observe reports absent, never never_started, including
//     after reconstruction.
//   - Logs are ordered, identified, bounded and redacted with the packed
//     per-stream cursor; truncation flags are preserved, they carry no lifecycle
//     authority and they fail closed when the configured redaction source is
//     unavailable.
//   - RefreshCredential replaces only the mounted credential file, never
//     restarts or re-signals the command, is recoverable after a lost update
//     response and never revives a removed identity.
//   - Capabilities are exactly process, process-group-stop and
//     credential-refresh at process/v1. The adapter makes no isolation claim.
package process

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

// TestAdapterSatisfiesRuntimeClient is a compile-time proof that the adapter
// exposes exactly the declared runtime seam and no parallel vocabulary.
var _ runtime.Client = (*Adapter)(nil)

// TestAdapterPinsProcessAPIVersion pins process/v1 and the Process family.
func TestAdapterPinsProcessAPIVersion(t *testing.T) {
	fixture := newFixture(t)

	version, err := fixture.adapter.Version(context.Background())
	if err != nil {
		t.Fatalf("Version returned error: %v", err)
	}
	want := runtime.APIVersion{Adapter: runtime.AdapterProcess, Version: "process/v1"}
	if version != want {
		t.Fatalf("Version = %+v; want %+v", version, want)
	}
	for _, candidate := range runtime.PinnedAPIVersions() {
		if candidate.Adapter == runtime.AdapterProcess && candidate.Version == version.Version {
			return
		}
	}
	t.Fatalf("adapter version %q is not the declared Process API version", version.Version)
}

// TestAdapterCapabilitiesAreExplicit pins the Process capability set and proves
// it makes no container, retained-workspace, network-isolation or
// Job-deletion claim.
func TestAdapterCapabilitiesAreExplicit(t *testing.T) {
	fixture := newFixture(t)

	capabilities, err := fixture.adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities returned error: %v", err)
	}
	want := map[string]bool{}
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterProcess) {
		want[string(capability)] = true
	}
	if len(capabilities.Names) != len(want) {
		t.Fatalf("capabilities = %v; want the %d declared Process capabilities", capabilities.Names, len(want))
	}
	for _, name := range capabilities.Names {
		if !want[name] {
			t.Errorf("capabilities declare %q, which the declared Process family does not have", name)
		}
	}
	for _, required := range []string{
		string(runtime.CapabilityProcess),
		string(runtime.CapabilityProcessGroupStop),
		string(runtime.CapabilityCredentialRefresh),
	} {
		if !capabilities.Supports(required) {
			t.Errorf("process adapter does not declare %q", required)
		}
	}
	for _, forbidden := range []string{
		string(runtime.CapabilityContainer),
		string(runtime.CapabilityRetainedWorkspace),
		string(runtime.CapabilityNetworkIsolation),
		string(runtime.CapabilityJobDeletionNotStopProof),
	} {
		if capabilities.Supports(forbidden) {
			t.Errorf("development process adapter claims %q", forbidden)
		}
	}
	if calls := fixture.executor.callTotal() + fixture.observer.callTotal() + fixture.files.callTotal(); calls != 0 {
		t.Fatalf("Capabilities made %d boundary calls; it must be declaration-only", calls)
	}
}

// TestCheckRequirementsRefusesUnsupportedWork pins pre-dispatch capability
// refusal: a development Process adapter must refuse admitted work that needs
// container isolation, a retained workspace or network isolation instead of
// degrading silently.
func TestCheckRequirementsRefusesUnsupportedWork(t *testing.T) {
	if err := CheckRequirements(nil); err != nil {
		t.Fatalf("CheckRequirements(nil) = %v; want nil", err)
	}

	supported := make([]string, 0, 3)
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterProcess) {
		supported = append(supported, string(capability))
	}
	if err := CheckRequirements(supported); err != nil {
		t.Fatalf("CheckRequirements(%v) = %v; want nil", supported, err)
	}

	for _, required := range []string{
		string(runtime.CapabilityContainer),
		string(runtime.CapabilityRetainedWorkspace),
		string(runtime.CapabilityNetworkIsolation),
		"gpu",
	} {
		err := CheckRequirements([]string{string(runtime.CapabilityProcess), required})
		if err == nil {
			t.Fatalf("CheckRequirements accepted unsupported capability %q", required)
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.UnsupportedCapability {
			t.Fatalf("CheckRequirements(%q) error class = %v (present=%v); want %q",
				required, class, ok, fault.UnsupportedCapability)
		}
	}
}

// TestNewRefusesIncompleteConfiguration proves the adapter cannot be composed
// without an executor, observer, file boundary, disjoint absolute roots, a
// Kodama URL or a positive cleanup bound.
func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "nil executor", mutate: func(config *Config) { config.Executor = nil }},
		{name: "nil observer", mutate: func(config *Config) { config.Observer = nil }},
		{name: "nil files", mutate: func(config *Config) { config.Files = nil }},
		{name: "empty root", mutate: func(config *Config) { config.Root = "" }},
		{name: "relative root", mutate: func(config *Config) { config.Root = "resources" }},
		{name: "empty workspace root", mutate: func(config *Config) { config.WorkspaceRoot = "" }},
		{name: "relative workspace root", mutate: func(config *Config) { config.WorkspaceRoot = "workspaces" }},
		{name: "identical roots", mutate: func(config *Config) { config.WorkspaceRoot = config.Root }},
		{name: "root inside workspace root", mutate: func(config *Config) {
			config.Root = filepath.Join(config.WorkspaceRoot, "resources")
		}},
		{name: "workspace root inside root", mutate: func(config *Config) {
			config.WorkspaceRoot = filepath.Join(config.Root, "workspaces")
		}},
		{name: "empty url", mutate: func(config *Config) { config.KodamaURL = "" }},
		{name: "zero cleanup attempts", mutate: func(config *Config) { config.Cleanup.Attempts = 0 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := baseConfig(t)
			testCase.mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatalf("New accepted configuration with %s", testCase.name)
			}
		})
	}

	if _, err := New(baseConfig(t)); err != nil {
		t.Fatalf("New refused a complete configuration: %v", err)
	}
}

// TestPinnedConstants pins the redaction marker and a positive log bound.
func TestPinnedConstants(t *testing.T) {
	if Redacted != "[REDACTED]" {
		t.Errorf("Redacted = %q; want %q", Redacted, "[REDACTED]")
	}
	if MaxLogBytes <= 0 {
		t.Errorf("MaxLogBytes = %d; want a positive bound", MaxLogBytes)
	}
}

// TestStableNamingHelpers pins deterministic identity-only naming so the Shell
// can locate resources before Prepare and after a restart, and so two identities
// never share resources.
func TestStableNamingHelpers(t *testing.T) {
	first, err := GroupName("workload-1")
	if err != nil {
		t.Fatalf("GroupName returned error: %v", err)
	}
	second, err := GroupName("workload-1")
	if err != nil {
		t.Fatalf("GroupName returned error: %v", err)
	}
	if first != second || first == "" {
		t.Fatalf("GroupName is not stable: %q then %q", first, second)
	}
	other, err := GroupName("workload-2")
	if err != nil {
		t.Fatalf("GroupName returned error: %v", err)
	}
	if other == first {
		t.Fatalf("distinct workloads share group %q", first)
	}
	if _, err := GroupName("bad workload"); err == nil {
		t.Fatal("GroupName accepted an invalid workload id")
	}

	root := t.TempDir()
	resourceFirst, err := ResourceRoot(root, "workload-1")
	if err != nil {
		t.Fatalf("ResourceRoot returned error: %v", err)
	}
	resourceSecond, err := ResourceRoot(root, "workload-1")
	if err != nil {
		t.Fatalf("ResourceRoot returned error: %v", err)
	}
	if resourceFirst != resourceSecond {
		t.Fatalf("ResourceRoot is not stable: %q then %q", resourceFirst, resourceSecond)
	}
	if !strings.HasPrefix(resourceFirst, root+string(os.PathSeparator)) {
		t.Fatalf("ResourceRoot %q is outside the adapter root %q", resourceFirst, root)
	}
	otherRoot, err := ResourceRoot(t.TempDir(), "workload-1")
	if err != nil {
		t.Fatalf("ResourceRoot returned error: %v", err)
	}
	if otherRoot == resourceFirst {
		t.Fatalf("distinct roots share resource root %q", resourceFirst)
	}
	if _, err := ResourceRoot(root, "bad workload"); err == nil {
		t.Fatal("ResourceRoot accepted an invalid workload id")
	}

	workspaceRoot := t.TempDir()
	workspace, err := WorkspaceDir(workspaceRoot, "workspace-1")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	again, err := WorkspaceDir(workspaceRoot, "workspace-1")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if workspace != again {
		t.Fatalf("WorkspaceDir is not stable: %q then %q", workspace, again)
	}
	distinct, err := WorkspaceDir(workspaceRoot, "workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if distinct == workspace {
		t.Fatalf("distinct workspaces share directory %q", workspace)
	}
	if _, err := WorkspaceDir(workspaceRoot, "bad workspace"); err == nil {
		t.Fatal("WorkspaceDir accepted an invalid workspace id")
	}
}

// assertRefusedClass proves an operation was refused with one of the expected
// fault classes.
func assertRefusedClass(t *testing.T, err error, want ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatal("operation was accepted; want a refusal")
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("refusal %v has no fault class; want one of %v", err, want)
	}
	if slices.Contains(want, class) {
		return
	}
	t.Fatalf("refusal class = %q; want one of %v", class, want)
}

// assertNoSecrets proves a diagnostic never carries secret or credential
// material.
func assertNoSecrets(t *testing.T, label, text string) {
	t.Helper()
	for _, value := range []string{"s3cr3t", "tok-abc", "tok-new", "tok-lost"} {
		if strings.Contains(text, value) {
			t.Fatalf("%s leaked %q: %q", label, value, text)
		}
	}
}
