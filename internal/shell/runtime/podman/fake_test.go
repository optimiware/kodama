// Deterministic libpod/file/clock fakes and shared fixtures.
//
// These fakes are the only "container runtime" and "host filesystem" these
// tests touch. They start no container, open no socket, contact no daemon and
// never sleep in wall-clock time. Files are backed by t.TempDir so exact bytes
// and modes can be asserted, while every fault and barrier is explicit and
// deterministic.
package podman

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

const (
	testKodamaURL = "https://kodama.example.com"
	// testImage is a digest-pinned image reference.
	testImage = "registry.example.com/tools/build@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testNow is the fixed authoritative time; tests never read the wall clock.
var testNow = time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)

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
			"input-a": "line1\nline2\n",
		},
		Outputs: map[id.ActionName]kernel.Outputs{
			"build": {"out-a": "out"},
		},
		Task:       []byte(`{"id":"task-1"}`),
		Credential: "tok-abc",
	}
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class, want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s, want %s (error %v)", class, want, err)
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

// ---------------------------------------------------------------------------
// Host file boundary fake
// ---------------------------------------------------------------------------

type fileCall struct {
	op   string
	path string
}

type fileFaultKey struct {
	op     string
	suffix string
}

type fileFailure struct {
	call int
	err  error
}

// fakeFiles is a temp-dir-backed host file boundary with explicit fault
// injection. Lstat never follows a symlink, so a symlink escape is observable.
type fakeFiles struct {
	mu   sync.Mutex
	io   sync.Mutex
	call []fileCall

	before map[fileFaultKey]error
	after  map[fileFaultKey]error
	counts map[string]int
	failOn map[string]fileFailure
	gates  map[string]*fakeGate

	// afterData maps a read operation to an error returned together with the
	// bytes read before the failure. It models an I/O error after a partial read
	// so a caller cannot treat partial bytes as a complete record.
	afterData map[fileFaultKey]error

	// removeAllHooks are one-shot hooks keyed by a path suffix. A hook runs
	// inside RemoveAll before the fake's own effect, so a test can observe the
	// ordering of a destructive step or simulate a partial removal that deletes
	// some entries and then fails.
	removeAllHooks map[string]func(path string) error
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		before:         map[fileFaultKey]error{},
		after:          map[fileFaultKey]error{},
		counts:         map[string]int{},
		failOn:         map[string]fileFailure{},
		gates:          map[string]*fakeGate{},
		afterData:      map[fileFaultKey]error{},
		removeAllHooks: map[string]func(path string) error{},
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

// injectAfterData returns the bytes read before a failure together with the
// error, modelling a partial read that a caller must not treat as complete.
func (f *fakeFiles) injectAfterData(op, suffix string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterData[fileFaultKey{op: op, suffix: suffix}] = err
}

func (f *fakeFiles) afterDataErr(op, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return matchFileFault(f.afterData, op, path)
}

// failOnCall fails exactly the given 1-based call of one operation before its
// effect. It is used to force a deterministic partial preparation.
func (f *fakeFiles) failOnCall(op string, call int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[op] = fileFailure{call: call, err: err}
}

func (f *fakeFiles) clear(op, suffix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.before, fileFaultKey{op: op, suffix: suffix})
	delete(f.after, fileFaultKey{op: op, suffix: suffix})
}

func (f *fakeFiles) clearFailOn(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failOn, op)
}

func (f *fakeFiles) newGate(op string) *fakeGate {
	gate := newGate()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[op] = gate
	return gate
}

// onRemoveAll installs a one-shot hook that runs inside RemoveAll for a path
// ending in the given suffix, before the fake's effect. The hook may inspect the
// temp-dir files and return an error to simulate a partial removal.
func (f *fakeFiles) onRemoveAll(suffix string, hook func(path string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeAllHooks[suffix] = hook
}

// takeRemoveAllHook consumes the longest matching one-shot hook.
func (f *fakeFiles) takeRemoveAllHook(path string) func(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	matched := ""
	for suffix := range f.removeAllHooks {
		if suffix == "" || strings.HasSuffix(path, suffix) {
			if matched == "" || len(suffix) > len(matched) {
				matched = suffix
			}
		}
	}
	if matched == "" {
		return nil
	}
	hook := f.removeAllHooks[matched]
	delete(f.removeAllHooks, matched)
	return hook
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
	f.call = append(f.call, fileCall{op: op, path: path})
	f.counts[op]++
	if failure, ok := f.failOn[op]; ok && f.counts[op] == failure.call {
		delete(f.failOn, op)
		return f.gates[op], failure.err
	}
	return f.gates[op], matchFileFault(f.before, op, path)
}

func (f *fakeFiles) afterErr(op, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return matchFileFault(f.after, op, path)
}

func (f *fakeFiles) callsFor(op string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0)
	for _, call := range f.call {
		if call.op == op {
			paths = append(paths, call.path)
		}
	}
	return paths
}

func (f *fakeFiles) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

func (f *fakeFiles) countPath(op, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.call {
		if call.op == op && call.path == path {
			count++
		}
	}
	return count
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
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("fake files: %q is a symlink", path)
		}
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
	if after := f.afterDataErr("ReadFile", path); after != nil {
		return data, after
	}
	if after := f.afterErr("ReadFile", path); after != nil {
		return nil, after
	}
	return data, nil
}

func (f *fakeFiles) Lstat(ctx context.Context, path string) (os.FileInfo, error) {
	gate, before := f.begin("Lstat", path)
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
	if after := f.afterErr("Lstat", path); after != nil {
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
	if hook := f.takeRemoveAllHook(path); hook != nil {
		if err := hook(path); err != nil {
			return err
		}
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

// ---------------------------------------------------------------------------
// Libpod fake
// ---------------------------------------------------------------------------

type libpodFailure struct {
	err    error
	effect bool
}

// fakeLibpod is a deterministic in-memory libpod API. All state is guarded by
// one mutex; tests inspect it through the helpers below.
type fakeLibpod struct {
	mu sync.Mutex

	containers map[string]ContainerState
	volumes    map[string]Volume
	logs       map[string]map[engine.LogStream][]byte

	calls               []string
	createRequests      []ContainerSpec
	stopRequests        []string
	removeRequests      []string
	volumeRequests      []string
	removeVolumeRequest []string
	logRequests         []LogRequest

	nextFailure    map[string]libpodFailure
	removeFailures int
	removeFailErr  error
	inspectErr     error
	volumeErr      error
	stopEffect     bool

	// createHook runs once after a successful create effect and before
	// CreateContainer returns, so a test can cancel or observe the exact window
	// between container creation and the caller's next durable step.
	createHook func()

	gates map[string]*fakeGate
}

func newFakeLibpod() *fakeLibpod {
	return &fakeLibpod{
		containers:  map[string]ContainerState{},
		volumes:     map[string]Volume{},
		logs:        map[string]map[engine.LogStream][]byte{},
		nextFailure: map[string]libpodFailure{},
		stopEffect:  true,
		gates:       map[string]*fakeGate{},
	}
}

func (f *fakeLibpod) addVolume(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[name] = Volume{Name: name, Labels: copyLabels(labels)}
}

func (f *fakeLibpod) deleteVolume(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
}

func (f *fakeLibpod) volume(name string) (Volume, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	volume, ok := f.volumes[name]
	if !ok {
		return Volume{}, false
	}
	return Volume{Name: volume.Name, Labels: copyLabels(volume.Labels)}, true
}

func (f *fakeLibpod) setContainer(state ContainerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state.Labels = copyLabels(state.Labels)
	f.containers[state.Name] = state
}

func (f *fakeLibpod) container(name string) (ContainerState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.containers[name]
	if !ok {
		return ContainerState{}, false
	}
	state.Labels = copyLabels(state.Labels)
	return state, true
}

func (f *fakeLibpod) containerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

func (f *fakeLibpod) setLogs(name string, stream engine.LogStream, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs[name] == nil {
		f.logs[name] = map[engine.LogStream][]byte{}
	}
	f.logs[name][stream] = append([]byte(nil), data...)
}

func (f *fakeLibpod) setInspectError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectErr = err
}

func (f *fakeLibpod) setVolumeError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeErr = err
}

func (f *fakeLibpod) setStopEffect(effect bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopEffect = effect
}

// onCreate installs a one-shot hook that runs after the create effect and
// before CreateContainer returns.
func (f *fakeLibpod) onCreate(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createHook = hook
}

// failNext fails the next call of one operation. effect reports whether the
// fake applies the effect before returning the error (a lost response after the
// effect) or not (an error before the effect).
func (f *fakeLibpod) failNext(op string, err error, effect bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFailure[op] = libpodFailure{err: err, effect: effect}
}

func (f *fakeLibpod) failRemoveTimes(attempts int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeFailures = attempts
	f.removeFailErr = err
}

func (f *fakeLibpod) newGate(op string) *fakeGate {
	gate := newGate()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[op] = gate
	return gate
}

func (f *fakeLibpod) clearGate(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.gates, op)
}

func (f *fakeLibpod) takeFailure(op string) (libpodFailure, bool) {
	failure, ok := f.nextFailure[op]
	if ok {
		delete(f.nextFailure, op)
	}
	return failure, ok
}

func (f *fakeLibpod) begin(op string) *fakeGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
	return f.gates[op]
}

func (f *fakeLibpod) callTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLibpod) callsFor(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == op {
			count++
		}
	}
	return count
}

func (f *fakeLibpod) createSpecs() []ContainerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	specs := make([]ContainerSpec, len(f.createRequests))
	for index, spec := range f.createRequests {
		specs[index] = copySpec(spec)
	}
	return specs
}

func (f *fakeLibpod) stoppedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopRequests...)
}

func (f *fakeLibpod) removedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removeRequests...)
}

func (f *fakeLibpod) inspectedVolumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.volumeRequests...)
}

func (f *fakeLibpod) removedVolumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removeVolumeRequest...)
}

func (f *fakeLibpod) seenLogRequests() []LogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LogRequest(nil), f.logRequests...)
}

func (f *fakeLibpod) CreateContainer(ctx context.Context, spec ContainerSpec) (ContainerState, error) {
	gate := f.begin("CreateContainer")
	f.mu.Lock()
	f.createRequests = append(f.createRequests, copySpec(spec))
	failure, hasFailure := f.takeFailure("CreateContainer")
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return ContainerState{}, err
	}
	if err := ctx.Err(); err != nil {
		return ContainerState{}, err
	}

	f.mu.Lock()
	if _, exists := f.containers[spec.Name]; exists {
		f.mu.Unlock()
		return ContainerState{}, ErrAlreadyExists
	}
	if hasFailure && !failure.effect {
		f.mu.Unlock()
		return ContainerState{}, failure.err
	}
	state := ContainerState{
		ID:     "container-" + spec.Name,
		Name:   spec.Name,
		Image:  spec.Image,
		Status: ContainerStatusCreated,
		Labels: copyLabels(spec.Labels),
	}
	f.containers[spec.Name] = state
	hook := f.createHook
	f.createHook = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if hasFailure {
		return state, failure.err
	}
	return state, nil
}

func (f *fakeLibpod) InspectContainer(ctx context.Context, name string) (ContainerState, error) {
	gate := f.begin("InspectContainer")
	f.mu.Lock()
	inspectErr := f.inspectErr
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return ContainerState{}, err
	}
	if inspectErr != nil {
		return ContainerState{}, inspectErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.containers[name]
	if !ok {
		return ContainerState{}, ErrNotFound
	}
	state.Labels = copyLabels(state.Labels)
	return state, nil
}

func (f *fakeLibpod) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerState, error) {
	gate := f.begin("ListContainers")
	f.mu.Lock()
	failure, hasFailure := f.takeFailure("ListContainers")
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if hasFailure && !failure.effect {
		return nil, failure.err
	}
	matched := make([]ContainerState, 0, len(f.containers))
	for _, state := range f.containers {
		if !labelsMatch(state.Labels, labels) {
			continue
		}
		state.Labels = copyLabels(state.Labels)
		matched = append(matched, state)
	}
	sort.Slice(matched, func(left, right int) bool { return matched[left].Name < matched[right].Name })
	if hasFailure {
		return matched, failure.err
	}
	return matched, nil
}

func (f *fakeLibpod) StartContainer(ctx context.Context, name string) error {
	gate := f.begin("StartContainer")
	f.mu.Lock()
	failure, hasFailure := f.takeFailure("StartContainer")
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.containers[name]
	if !ok {
		return ErrNotFound
	}
	if hasFailure && !failure.effect {
		return failure.err
	}
	state.Started = true
	state.Status = ContainerStatusRunning
	state.StartPending = false
	f.containers[name] = state
	if hasFailure {
		return failure.err
	}
	return nil
}

func (f *fakeLibpod) StopContainer(ctx context.Context, name string) error {
	gate := f.begin("StopContainer")
	f.mu.Lock()
	f.stopRequests = append(f.stopRequests, name)
	failure, hasFailure := f.takeFailure("StopContainer")
	effect := f.stopEffect
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.containers[name]
	if !ok {
		return ErrNotFound
	}
	apply := effect
	if hasFailure {
		apply = failure.effect
	}
	if apply {
		state.Status = ContainerStatusExited
		state.ExitCode = 0
		state.WholeStopped = true
		state.Started = true
		f.containers[name] = state
	}
	if hasFailure {
		return failure.err
	}
	return nil
}

func (f *fakeLibpod) RemoveContainer(ctx context.Context, name string) error {
	gate := f.begin("RemoveContainer")
	f.mu.Lock()
	f.removeRequests = append(f.removeRequests, name)
	if f.removeFailures > 0 {
		f.removeFailures--
		err := f.removeFailErr
		f.mu.Unlock()
		if gateErr := waitGate(ctx, gate); gateErr != nil {
			return gateErr
		}
		return err
	}
	failure, hasFailure := f.takeFailure("RemoveContainer")
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if hasFailure && !failure.effect {
		return failure.err
	}
	delete(f.containers, name)
	if hasFailure {
		return failure.err
	}
	return nil
}

func (f *fakeLibpod) ReadLogs(ctx context.Context, request LogRequest) (LogPage, error) {
	gate := f.begin("ReadLogs")
	f.mu.Lock()
	f.logRequests = append(f.logRequests, request)
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return LogPage{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	data := f.logs[request.Name][request.Stream]
	offset := min(max(request.Offset, 0), int64(len(data)))
	limit := max(request.Limit, 0)
	end := min(offset+int64(limit), int64(len(data)))
	page := LogPage{
		Data:       append([]byte(nil), data[offset:end]...),
		NextOffset: end,
		Truncated:  end < int64(len(data)),
	}
	return page, nil
}

func (f *fakeLibpod) InspectVolume(ctx context.Context, name string) (Volume, error) {
	gate := f.begin("InspectVolume")
	f.mu.Lock()
	f.volumeRequests = append(f.volumeRequests, name)
	volumeErr := f.volumeErr
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return Volume{}, err
	}
	if volumeErr != nil {
		return Volume{}, volumeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	volume, ok := f.volumes[name]
	if !ok {
		return Volume{}, ErrNotFound
	}
	volume.Labels = copyLabels(volume.Labels)
	return volume, nil
}

func (f *fakeLibpod) RemoveVolume(ctx context.Context, name string) error {
	gate := f.begin("RemoveVolume")
	f.mu.Lock()
	f.removeVolumeRequest = append(f.removeVolumeRequest, name)
	failure, hasFailure := f.takeFailure("RemoveVolume")
	f.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if hasFailure && !failure.effect {
		return failure.err
	}
	if _, ok := f.volumes[name]; !ok {
		return ErrNotFound
	}
	delete(f.volumes, name)
	if hasFailure {
		return failure.err
	}
	return nil
}

func labelsMatch(labels, filter map[string]string) bool {
	for key, want := range filter {
		if labels[key] != want {
			return false
		}
	}
	return true
}

func copyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	copied := make(map[string]string, len(labels))
	maps.Copy(copied, labels)
	return copied
}

func copySpec(spec ContainerSpec) ContainerSpec {
	copied := spec
	copied.Entrypoint = append([]string(nil), spec.Entrypoint...)
	copied.Env = append([]string(nil), spec.Env...)
	copied.Labels = copyLabels(spec.Labels)
	copied.Mounts = append([]Mount(nil), spec.Mounts...)
	return copied
}

// ---------------------------------------------------------------------------
// Clock fake
// ---------------------------------------------------------------------------

// fakeClock records injected backoff and never sleeps in wall-clock time.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	sleeps   []time.Duration
	sleepErr error
	gate     *fakeGate
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testNow}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, duration)
	gate := c.gate
	err := c.sleepErr
	c.mu.Unlock()

	if gate != nil {
		if waitErr := waitGate(ctx, gate); waitErr != nil {
			return waitErr
		}
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (c *fakeClock) newGate() *fakeGate {
	gate := newGate()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate = gate
	return gate
}

func (c *fakeClock) sleepDurations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type fixture struct {
	adapter       *Adapter
	libpod        *fakeLibpod
	files         *fakeFiles
	clock         *fakeClock
	root          string
	request       engine.PrepareRequest
	volumeName    string
	containerName string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	files := newFakeFiles()
	libpod := newFakeLibpod()
	clock := newFakeClock()

	volumeName, err := WorkspaceVolumeName(testWorkspaceID())
	if err != nil {
		t.Fatalf("WorkspaceVolumeName returned error: %v", err)
	}
	libpod.addVolume(volumeName, map[string]string{LabelWorkspace: string(testWorkspaceID())})

	adapter, err := New(Config{
		Libpod:    libpod,
		Files:     files,
		Root:      root,
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 3, Backoff: time.Second},
		Clock:     clock,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	containerName, err := ContainerName(testIdentity().WorkloadID)
	if err != nil {
		t.Fatalf("ContainerName returned error: %v", err)
	}
	return &fixture{
		adapter:       adapter,
		libpod:        libpod,
		files:         files,
		clock:         clock,
		root:          root,
		request:       testPrepareRequest(),
		volumeName:    volumeName,
		containerName: containerName,
	}
}

func (f *fixture) workloadRoot() string {
	return filepath.Join(f.root, string(f.request.Identity.WorkloadID))
}

func (f *fixture) kodamaRoot() string {
	return filepath.Join(f.workloadRoot(), "kodama")
}

func (f *fixture) hostPath(logical string) string {
	return filepath.Join(f.workloadRoot(), logical)
}

func (f *fixture) scriptPath() string {
	return filepath.Join(f.kodamaRoot(), "script")
}

func (f *fixture) credentialPath() string {
	return filepath.Join(f.kodamaRoot(), "secrets", "KODAMA_TOKEN")
}

func (f *fixture) prepare(t *testing.T) engine.Preparation {
	t.Helper()
	preparation, err := f.adapter.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	if preparation.WorkspaceID != f.request.WorkspaceID {
		t.Fatalf("preparation workspace = %q, want %q", preparation.WorkspaceID, f.request.WorkspaceID)
	}
	if preparation.Detail == "" {
		t.Fatal("preparation has no detail")
	}
	return preparation
}

func (f *fixture) start(t *testing.T) engine.StartResult {
	t.Helper()
	result, err := f.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    f.request.Identity,
		WorkspaceID: f.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	return result
}

func (f *fixture) startResult(t *testing.T) (engine.StartResult, error) {
	t.Helper()
	return f.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    f.request.Identity,
		WorkspaceID: f.request.WorkspaceID,
	})
}

func (f *fixture) observe(t *testing.T) kernel.Observation {
	t.Helper()
	observation, err := f.adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    f.request.Identity,
		WorkspaceID: f.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	return observation
}

func (f *fixture) stop(t *testing.T) kernel.StopEvidence {
	t.Helper()
	evidence, err := f.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    f.request.Identity,
		WorkspaceID: f.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	return evidence
}

func (f *fixture) remove(t *testing.T) engine.Removal {
	t.Helper()
	removal, err := f.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    f.request.Identity,
		WorkspaceID: f.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	return removal
}

func (f *fixture) refresh(t *testing.T, credential string) engine.CredentialRefresh {
	t.Helper()
	refresh, err := f.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   f.request.Identity,
		Credential: credential,
	})
	if err != nil {
		t.Fatalf("RefreshCredential returned error: %v", err)
	}
	return refresh
}

func (f *fixture) containerLabels() map[string]string {
	return map[string]string{
		LabelExecution:      string(f.request.Identity.ExecutionID),
		LabelAction:         string(f.request.Identity.ActionID),
		LabelWorkload:       string(f.request.Identity.WorkloadID),
		LabelWorkspace:      string(f.request.WorkspaceID),
		LabelIdempotencyKey: f.request.Identity.IdempotencyKey,
	}
}

func (f *fixture) seedCreated(t *testing.T, startPending bool) ContainerState {
	t.Helper()
	state := ContainerState{
		ID:           "container-" + f.containerName,
		Name:         f.containerName,
		Image:        f.request.Container.Image,
		Status:       ContainerStatusCreated,
		StartPending: startPending,
		Labels:       f.containerLabels(),
	}
	f.libpod.setContainer(state)
	return state
}

func (f *fixture) seedRunning(t *testing.T) ContainerState {
	t.Helper()
	state := ContainerState{
		ID:      "container-" + f.containerName,
		Name:    f.containerName,
		Image:   f.request.Container.Image,
		Status:  ContainerStatusRunning,
		Started: true,
		Labels:  f.containerLabels(),
	}
	f.libpod.setContainer(state)
	return state
}

func (f *fixture) seedExited(t *testing.T, exitCode int, wholeStopped bool) ContainerState {
	t.Helper()
	state := ContainerState{
		ID:           "container-" + f.containerName,
		Name:         f.containerName,
		Image:        f.request.Container.Image,
		Status:       ContainerStatusExited,
		Started:      true,
		ExitCode:     exitCode,
		WholeStopped: wholeStopped,
		Labels:       f.containerLabels(),
	}
	f.libpod.setContainer(state)
	return state
}

// newAdapterWithRoot reconstructs an adapter over the same durable root and
// fakes, exactly as a controller restart would.
func newAdapterWithRoot(t *testing.T, fx *fixture) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		Libpod:    fx.libpod,
		Files:     fx.files,
		Root:      fx.root,
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 3, Backoff: time.Second},
		Clock:     fx.clock,
	})
	if err != nil {
		t.Fatalf("New (reconstructed) returned error: %v", err)
	}
	return adapter
}

func readHostFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	return data
}

func hostFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("lstat %q: %v", path, err)
	return false
}

func hostFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	return info.Mode().Perm()
}

func joinEnv(name, value string) string {
	return name + "=" + value
}

var (
	errFakeUnavailable = errors.New("libpod: daemon unavailable")
	errFakeLost        = errors.New("libpod: response lost")
)
