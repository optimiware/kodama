// Deterministic process/observer/file/clock fakes and shared fixtures.
//
// These fakes are the only "operating system" these tests touch. They launch no
// process, invoke no shell, open no socket and never sleep in wall-clock time.
// Files are backed by t.TempDir so exact bytes and modes can be asserted, while
// every fault and barrier is explicit and deterministic.
package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

const (
	testKodamaURL = "https://kodama.example.com"
	// testImage is a digest-pinned image reference. The development Process
	// adapter does not use it, but the admitted declaration still carries one.
	testImage = "registry.example.com/tools/build@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	workspaceSentinel     = "sentinel.txt"
	workspaceSentinelData = "workspace-bytes"
)

// testDeadline is a fixed authoritative time; tests never read the wall clock.
var testDeadline = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

func testIdentity() engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    "exec-1",
		ActionID:       "action-1",
		WorkloadID:     "workload-1",
		IdempotencyKey: "idem-1",
	}
}

func testWorkspaceID() id.WorkspaceID {
	return "workspace-1"
}

func testContainer() definition.Container {
	return definition.Container{
		Image:    testImage,
		Commands: []string{"set -u", `echo "a b"`, "make build"},
		Env: []definition.EnvVar{
			{Name: "BUILD_MODE", Value: "release"},
		},
		Secrets: []id.SecretName{"API_KEY"},
		Outputs: []id.OutputKey{"out-a"},
	}
}

func testPrepareRequest() engine.PrepareRequest {
	return engine.PrepareRequest{
		Identity:    testIdentity(),
		WorkspaceID: testWorkspaceID(),
		ActionName:  "build",
		TaskID:      "task-1",
		Container:   testContainer(),
		Secrets: []engine.SecretValue{
			{Name: "API_KEY", Value: "s3cr3t"},
		},
		Inputs: map[id.InputKey]string{
			"input-a": "line1\nline2",
		},
		Outputs: map[id.ActionName]kernel.Outputs{
			"build": {"out-a": "out"},
		},
		Task:       []byte(`{"id":"task-1"}`),
		Credential: "tok-abc",
	}
}

// fakeGate holds one fake operation at an explicit barrier.
type fakeGate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *fakeGate {
	return &fakeGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// waitGate blocks at the gate until the test releases it or the context ends.
func waitGate(ctx context.Context, gate *fakeGate) error {
	if gate == nil {
		return nil
	}
	select {
	case gate.entered <- struct{}{}:
	default:
	}
	select {
	case <-gate.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitFor fails the test if the channel is not signalled promptly. The timeout
// only detects a deadlocked test; it is not a behavioral sleep.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// fileFaultKey matches one injected fault by operation and path suffix.
type fileFaultKey struct {
	op     string
	suffix string
}

type fileCall struct {
	op   string
	path string
}

// fakeFiles is a temp-dir-backed host file boundary with explicit fault
// injection. Stat is lstat, so a symlink is reported as a symlink and never
// followed.
type fakeFiles struct {
	mu     sync.Mutex
	io     sync.Mutex
	calls  []fileCall
	before map[fileFaultKey]error
	after  map[fileFaultKey]error
	gates  map[string]*fakeGate
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		before: map[fileFaultKey]error{},
		after:  map[fileFaultKey]error{},
		gates:  map[string]*fakeGate{},
	}
}

func (f *fakeFiles) injectBefore(op, suffix string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before[fileFaultKey{op: op, suffix: suffix}] = err
}

func (f *fakeFiles) injectAfter(op, suffix string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.after[fileFaultKey{op: op, suffix: suffix}] = err
}

func (f *fakeFiles) clear(op, suffix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.before, fileFaultKey{op: op, suffix: suffix})
	delete(f.after, fileFaultKey{op: op, suffix: suffix})
}

func (f *fakeFiles) newGate(op string) *fakeGate {
	gate := newGate()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[op] = gate
	return gate
}

func matchFileFault(faults map[fileFaultKey]error, op, path string) error {
	for key, err := range faults {
		if key.op != op {
			continue
		}
		if key.suffix == "" || strings.HasSuffix(path, key.suffix) {
			return err
		}
	}
	return nil
}

func (f *fakeFiles) begin(op, path string) (*fakeGate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fileCall{op: op, path: path})
	return f.gates[op], matchFileFault(f.before, op, path)
}

func (f *fakeFiles) afterErr(op, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return matchFileFault(f.after, op, path)
}

func (f *fakeFiles) callTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeFiles) callsFor(op string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0)
	for _, call := range f.calls {
		if call.op == op {
			paths = append(paths, call.path)
		}
	}
	return paths
}

func (f *fakeFiles) MkdirAll(ctx context.Context, path string, mode os.FileMode) error {
	gate, before := f.begin("MkdirAll", path)
	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if before != nil {
		return before
	}
	f.io.Lock()
	defer f.io.Unlock()
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return f.afterErr("MkdirAll", path)
}

func (f *fakeFiles) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	gate, before := f.begin("WriteFile", path)
	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if before != nil {
		return before
	}
	f.io.Lock()
	defer f.io.Unlock()
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return fmt.Errorf("fake files: %q is a directory", path)
		}
		// Replace semantics: an existing read-only prepared file is replaced.
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return f.afterErr("WriteFile", path)
}

func (f *fakeFiles) ReadFile(ctx context.Context, path string) ([]byte, error) {
	gate, before := f.begin("ReadFile", path)
	if err := waitGate(ctx, gate); err != nil {
		return nil, err
	}
	if before != nil {
		return nil, before
	}
	f.io.Lock()
	defer f.io.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if after := f.afterErr("ReadFile", path); after != nil {
		return nil, after
	}
	return data, nil
}

func (f *fakeFiles) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	gate, before := f.begin("Stat", path)
	if err := waitGate(ctx, gate); err != nil {
		return nil, err
	}
	if before != nil {
		return nil, before
	}
	f.io.Lock()
	defer f.io.Unlock()
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if after := f.afterErr("Stat", path); after != nil {
		return nil, after
	}
	return info, nil
}

func (f *fakeFiles) ReadDir(ctx context.Context, path string) ([]os.DirEntry, error) {
	gate, before := f.begin("ReadDir", path)
	if err := waitGate(ctx, gate); err != nil {
		return nil, err
	}
	if before != nil {
		return nil, before
	}
	f.io.Lock()
	defer f.io.Unlock()
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if after := f.afterErr("ReadDir", path); after != nil {
		return nil, after
	}
	return entries, nil
}

func (f *fakeFiles) Rename(ctx context.Context, source, destination string) error {
	gate, before := f.begin("Rename", destination)
	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if before != nil {
		return before
	}
	f.io.Lock()
	defer f.io.Unlock()
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return f.afterErr("Rename", destination)
}

func (f *fakeFiles) RemoveAll(ctx context.Context, path string) error {
	gate, before := f.begin("RemoveAll", path)
	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if before != nil {
		return before
	}
	f.io.Lock()
	defer f.io.Unlock()
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return f.afterErr("RemoveAll", path)
}

// fakeExecutor is the injected process-effect boundary. It records every
// request, can hold Spawn or SignalGroup at a barrier and can report an
// affirmative refusal or an uncertain outcome. It never runs a real process.
type fakeExecutor struct {
	mu sync.Mutex

	observer *fakeObserver
	calls    []string

	requests     []SpawnRequest
	spawns       int
	spawnOutcome SpawnOutcome
	spawnDetail  string
	spawnErr     error
	spawnEffect  bool

	signals   int
	signaled  []GroupID
	signalErr error

	removals int

	gates map[string]*fakeGate
}

func newFakeExecutor(observer *fakeObserver) *fakeExecutor {
	return &fakeExecutor{
		observer:     observer,
		spawnOutcome: SpawnStarted,
		spawnDetail:  "spawned process group",
		spawnEffect:  true,
		gates:        map[string]*fakeGate{},
	}
}

func (e *fakeExecutor) setSpawn(outcome SpawnOutcome, detail string, effect bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spawnOutcome = outcome
	e.spawnDetail = detail
	e.spawnEffect = effect
	e.spawnErr = err
}

func (e *fakeExecutor) setSignalError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.signalErr = err
}

func (e *fakeExecutor) newGate(op string) *fakeGate {
	gate := newGate()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gates[op] = gate
	return gate
}

func (e *fakeExecutor) Spawn(ctx context.Context, request SpawnRequest) (SpawnResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, "Spawn")
	e.requests = append(e.requests, request)
	gate := e.gates["Spawn"]
	outcome := e.spawnOutcome
	detail := e.spawnDetail
	spawnErr := e.spawnErr
	effect := e.spawnEffect
	e.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return SpawnResult{
			Outcome: SpawnUncertain,
			Group:   request.Group,
			Detail:  "spawn interrupted at the barrier",
		}, err
	}
	if err := ctx.Err(); err != nil {
		return SpawnResult{
			Outcome: SpawnUncertain,
			Group:   request.Group,
			Detail:  "spawn cancelled before the effect",
		}, err
	}
	if effect {
		if e.observer != nil {
			e.observer.markRunning(request.Group)
		}
		e.mu.Lock()
		e.spawns++
		e.mu.Unlock()
	} else if outcome == SpawnRefused && e.observer != nil {
		// An affirmative refusal means the executor knows no process was
		// created, so the observer reports the affirmative never-created
		// evidence the Kernel's retire-then-replace path requires.
		e.observer.markNeverCreated(request.Group)
	}
	return SpawnResult{Outcome: outcome, Group: request.Group, Detail: detail}, spawnErr
}

func (e *fakeExecutor) SignalGroup(ctx context.Context, group GroupID) error {
	e.mu.Lock()
	e.calls = append(e.calls, "SignalGroup")
	e.signals++
	e.signaled = append(e.signaled, group)
	gate := e.gates["SignalGroup"]
	signalErr := e.signalErr
	e.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return signalErr
}

func (e *fakeExecutor) RemoveGroup(ctx context.Context, group GroupID) error {
	e.mu.Lock()
	e.calls = append(e.calls, "RemoveGroup")
	e.removals++
	gate := e.gates["RemoveGroup"]
	e.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	return ctx.Err()
}

func (e *fakeExecutor) spawnRequestCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func (e *fakeExecutor) spawnRequests() []SpawnRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]SpawnRequest(nil), e.requests...)
}

func (e *fakeExecutor) spawnCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spawns
}

func (e *fakeExecutor) signalCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.signals
}

func (e *fakeExecutor) signaledGroups() []GroupID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]GroupID(nil), e.signaled...)
}

func (e *fakeExecutor) removalCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.removals
}

func (e *fakeExecutor) callTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// fakeObserver is the injected process-group evidence and log boundary. It
// reports only what the test configures and never changes lifecycle state.
type fakeObserver struct {
	mu sync.Mutex

	status    map[GroupID]GroupStatus
	statusErr map[GroupID]error
	logs      map[GroupID]map[engine.LogStream][]byte
	calls     []string
	gates     map[string]*fakeGate
}

func newFakeObserver() *fakeObserver {
	return &fakeObserver{
		status:    map[GroupID]GroupStatus{},
		statusErr: map[GroupID]error{},
		logs:      map[GroupID]map[engine.LogStream][]byte{},
		gates:     map[string]*fakeGate{},
	}
}

func (o *fakeObserver) newGate(op string) *fakeGate {
	gate := newGate()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gates[op] = gate
	return gate
}

func (o *fakeObserver) setStatus(group GroupID, status GroupStatus) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status[group] = status
}

func (o *fakeObserver) statusOf(group GroupID) GroupStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status[group]
}

func (o *fakeObserver) markRunning(group GroupID) {
	o.setStatus(group, GroupStatus{Present: true, MembersAlive: true})
}

func (o *fakeObserver) markLeaderExited(group GroupID, exitCode int, groupGone bool) {
	o.setStatus(group, GroupStatus{
		Present:      true,
		LeaderExited: true,
		ExitCode:     exitCode,
		MembersAlive: !groupGone,
		GroupGone:    groupGone,
	})
}

func (o *fakeObserver) markNeverCreated(group GroupID) {
	o.setStatus(group, GroupStatus{NeverCreated: true})
}

func (o *fakeObserver) markAbsent(group GroupID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.status, group)
}

func (o *fakeObserver) setStatusError(group GroupID, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.statusErr[group] = err
}

func (o *fakeObserver) setLogs(group GroupID, stream engine.LogStream, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.logs[group] == nil {
		o.logs[group] = map[engine.LogStream][]byte{}
	}
	o.logs[group][stream] = append([]byte(nil), data...)
}

func (o *fakeObserver) Status(ctx context.Context, group GroupID) (GroupStatus, error) {
	o.mu.Lock()
	o.calls = append(o.calls, "Status")
	gate := o.gates["Status"]
	status := o.status[group]
	statusErr := o.statusErr[group]
	o.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return GroupStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return GroupStatus{}, err
	}
	if statusErr != nil {
		return GroupStatus{}, statusErr
	}
	return status, nil
}

func (o *fakeObserver) Logs(ctx context.Context, request GroupLogsRequest) (GroupLogPage, error) {
	o.mu.Lock()
	o.calls = append(o.calls, "Logs")
	gate := o.gates["Logs"]
	data := append([]byte(nil), o.logs[request.Group][request.Stream]...)
	o.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return GroupLogPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return GroupLogPage{}, err
	}
	start := min(max(request.Offset, 0), int64(len(data)))
	end := max(min(start+int64(request.Limit), int64(len(data))), start)
	return GroupLogPage{
		Data:      append([]byte(nil), data[start:end]...),
		Truncated: end < int64(len(data)),
	}, nil
}

func (o *fakeObserver) callTotal() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.calls)
}

func (o *fakeObserver) callsFor(op string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, call := range o.calls {
		if call == op {
			count++
		}
	}
	return count
}

// fakeClock is an injected deterministic clock. It never sleeps in real time.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	sleeps   []time.Duration
	sleepErr error
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testDeadline}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, duration)
	c.now = c.now.Add(duration)
	sleepErr := c.sleepErr
	c.mu.Unlock()
	if sleepErr != nil {
		return sleepErr
	}
	return ctx.Err()
}

func (c *fakeClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// fixture bundles one isolated adapter, its fakes and its workspace.
type fixture struct {
	root          string
	workspaceRoot string
	workspaceDir  string
	files         *fakeFiles
	observer      *fakeObserver
	executor      *fakeExecutor
	clock         *fakeClock
	adapter       *Adapter
	request       engine.PrepareRequest
}

// testRoots returns two disjoint existing absolute roots under t.TempDir.
func testRoots(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "resources")
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating resource root: %v", err)
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatalf("creating workspace root: %v", err)
	}
	return root, workspaceRoot
}

// baseConfig is a complete configuration without a constructed adapter.
func baseConfig(t *testing.T) Config {
	t.Helper()
	root, workspaceRoot := testRoots(t)
	observer := newFakeObserver()
	return Config{
		Root:          root,
		WorkspaceRoot: workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      newFakeExecutor(observer),
		Observer:      observer,
		Files:         newFakeFiles(),
		Clock:         newFakeClock(),
		Cleanup:       CleanupPolicy{Attempts: 3, Backoff: 2 * time.Second},
	}
}

func newTestAdapter(t *testing.T, config Config) *Adapter {
	t.Helper()
	adapter, err := New(config)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return adapter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, workspaceRoot := testRoots(t)
	workspaceDir, err := WorkspaceDir(workspaceRoot, testWorkspaceID())
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, workspaceSentinel), []byte(workspaceSentinelData), 0o644); err != nil {
		t.Fatalf("seeding workspace: %v", err)
	}

	files := newFakeFiles()
	observer := newFakeObserver()
	executor := newFakeExecutor(observer)
	clock := newFakeClock()
	config := Config{
		Root:          root,
		WorkspaceRoot: workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      executor,
		Observer:      observer,
		Files:         files,
		Clock:         clock,
		Cleanup:       CleanupPolicy{Attempts: 3, Backoff: 2 * time.Second},
	}
	return &fixture{
		root:          root,
		workspaceRoot: workspaceRoot,
		workspaceDir:  workspaceDir,
		files:         files,
		observer:      observer,
		executor:      executor,
		clock:         clock,
		adapter:       newTestAdapter(t, config),
		request:       testPrepareRequest(),
	}
}

// config rebuilds the same configuration for a reconstructed adapter.
func (f *fixture) config() Config {
	return Config{
		Root:          f.root,
		WorkspaceRoot: f.workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      f.executor,
		Observer:      f.observer,
		Files:         f.files,
		Clock:         f.clock,
		Cleanup:       CleanupPolicy{Attempts: 3, Backoff: 2 * time.Second},
	}
}

func (f *fixture) resourceRoot(t *testing.T) string {
	t.Helper()
	root, err := ResourceRoot(f.root, f.request.Identity.WorkloadID)
	if err != nil {
		t.Fatalf("ResourceRoot returned error: %v", err)
	}
	return root
}

// hostPath maps one logical /kodama path onto the adapter-owned host root.
func (f *fixture) hostPath(t *testing.T, logical string) string {
	t.Helper()
	return filepath.Join(f.resourceRoot(t), logical)
}

func (f *fixture) groupID(t *testing.T) GroupID {
	t.Helper()
	group, err := GroupName(f.request.Identity.WorkloadID)
	if err != nil {
		t.Fatalf("GroupName returned error: %v", err)
	}
	return group
}

func (f *fixture) credentialPath(t *testing.T) string {
	t.Helper()
	return f.hostPath(t, workspace.ReservedCredentialPath)
}

func prepareTestWorkload(t *testing.T, adapter *Adapter, request engine.PrepareRequest) engine.Preparation {
	t.Helper()
	preparation, err := adapter.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	return preparation
}

func startTestWorkload(t *testing.T, adapter *Adapter, identity engine.EffectIdentity, workspaceID id.WorkspaceID) engine.StartResult {
	t.Helper()
	result, err := adapter.Start(context.Background(), engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	return result
}

func readHostFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	return string(data)
}

// assertHostFile proves exact bytes and the read-only regular-file mode.
func assertHostFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readHostFile(t, path); got != want {
		t.Fatalf("file %q = %q; want %q", path, got, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("file %q mode %v is not a regular file", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Fatalf("file %q mode = %04o; want 0444", path, perm)
	}
}

func assertNoPath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("path %q exists; want it absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %q: %v", path, err)
	}
}

func assertUnder(t *testing.T, path, root string) {
	t.Helper()
	prefix := root + string(os.PathSeparator)
	if path != root && !strings.HasPrefix(path, prefix) {
		t.Fatalf("path %q is outside %q", path, root)
	}
}

// workspaceEntries lists the workspace recursively so a test can prove the
// adapter never wrote into it.
func workspaceEntries(t *testing.T, dir string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking workspace %q: %v", dir, err)
	}
	sort.Strings(entries)
	return entries
}
