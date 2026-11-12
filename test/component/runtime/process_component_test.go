//go:build component

package runtimeconformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	processdriver "github.com/optimiware/kodama/internal/shell/app/drivers/process"
	"github.com/optimiware/kodama/internal/shell/runtime/process"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestProcessRuntimeManifestEnvironmentPermissionsAndExit proves the Process
// adapter's real materialization: exact input/task bytes, read-only modes,
// reserved environment, host-environment isolation, fail-fast exit and log
// redaction.
func TestProcessRuntimeManifestEnvironmentPermissionsAndExit(t *testing.T) {
	ctx := t.Context()
	fixture := newProcessFixture(t, "workspace-process-1")
	identity := testIdentity("exec-p1", "action-p1", "workload-p1", "idem-p1")
	resourceRoot := fixture.resourceRoot(t, identity.WorkloadID)

	commands := []string{
		`env | sort > "$PWD/env.txt"`,
		`stat -c '%a' "$RESOURCE_ROOT/kodama/inputs/input-a" > "$PWD/modes.txt"`,
		`stat -c '%a' "$RESOURCE_ROOT/kodama/secrets/API_KEY" >> "$PWD/modes.txt"`,
		`stat -c '%a' "$RESOURCE_ROOT/script" >> "$PWD/modes.txt"`,
		`cat "$RESOURCE_ROOT/kodama/inputs/input-a" > "$PWD/input-copy.txt"`,
		`cat "$RESOURCE_ROOT/kodama/task.json" > "$PWD/task-copy.json"`,
		`cat "$RESOURCE_ROOT/kodama/secrets/API_KEY"`,
		`cat "$RESOURCE_ROOT/kodama/secrets/KODAMA_TOKEN"`,
		`echo done > "$PWD/done.txt"`,
	}
	request := testPrepareRequest(
		identity,
		fixture.workspaceID,
		"build",
		"task-1",
		testContainer("registry.example.com/tools/build@sha256:"+strings.Repeat("a", 64), commands,
			[]definition.EnvVar{
				{Name: "BUILD_MODE", Value: "release"},
				{Name: "RESOURCE_ROOT", Value: resourceRoot},
			},
			[]id.SecretName{"API_KEY"}),
		[]engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t-value"}},
		"tok-abc",
	)
	t.Setenv("KODAMA_HOST_CANARY", "must-not-leak")

	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, logical := range []string{
		"/kodama/inputs/input-a",
		"/kodama/secrets/API_KEY",
		"/kodama/secrets/KODAMA_TOKEN",
		"/kodama/task.json",
	} {
		host := filepath.Join(resourceRoot, logical)
		info, err := os.Lstat(host)
		if err != nil {
			t.Fatalf("prepared file %s: %v", logical, err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Fatalf("prepared file %s mode = %o; want 444", logical, info.Mode().Perm())
		}
	}
	if info, err := os.Lstat(filepath.Join(resourceRoot, "script")); err != nil {
		t.Fatalf("generated script: %v", err)
	} else if info.Mode().Perm() != 0o444 {
		t.Fatalf("generated script mode = %o; want 444", info.Mode().Perm())
	}

	result, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want accepted", result.Outcome)
	}
	waitForFile(t, ctx, filepath.Join(fixture.workspaceDir, "done.txt"))

	observation := waitForProcessObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	code, ok := observation.ExitStatus()
	if !ok || code != 0 {
		t.Fatalf("exit status = %d/%v; want 0/true (%+v)", code, ok, observation)
	}
	if !observation.StopConfirmed() {
		t.Fatalf("exited observation has no confirmed stop: %+v", observation)
	}

	env := readFileString(t, filepath.Join(fixture.workspaceDir, "env.txt"))
	for _, want := range []string{
		"BUILD_MODE=release",
		"RESOURCE_ROOT=" + resourceRoot,
		"KODAMA_URL=" + testKodamaURL,
		"KODAMA_EXECUTION=" + string(identity.ExecutionID),
		"KODAMA_ACTION=build",
		"KODAMA_TASK=task-1",
	} {
		if !strings.Contains(env, want+"\n") && !strings.Contains(env, want) {
			t.Fatalf("environment is missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "KODAMA_HOST_CANARY") {
		t.Fatalf("host environment leaked into the workload:\n%s", env)
	}
	if strings.Contains(env, "s3cr3t-value") || strings.Contains(env, "tok-abc") {
		t.Fatalf("secret material appeared in the workload environment:\n%s", env)
	}

	modes := readFileString(t, filepath.Join(fixture.workspaceDir, "modes.txt"))
	if strings.TrimSpace(modes) != "444\n444\n444" {
		t.Fatalf("materialized modes = %q; want 444/444/444", modes)
	}
	if got := readFileString(t, filepath.Join(fixture.workspaceDir, "input-copy.txt")); got != "line1\nline2\n" {
		t.Fatalf("input bytes = %q", got)
	}
	if got := readFileString(t, filepath.Join(fixture.workspaceDir, "task-copy.json")); got != `{"id":"task-1"}` {
		t.Fatalf("task snapshot = %q", got)
	}

	batch, err := fixture.adapter.Logs(ctx, engine.LogsRequest{Identity: identity, Limit: 1 << 20})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	logText := ""
	for _, chunk := range batch.Chunks {
		logText += string(chunk.Data)
	}
	if strings.Contains(logText, "s3cr3t-value") || strings.Contains(logText, "tok-abc") {
		t.Fatalf("secret or credential leaked into retained output: %q", logText)
	}
	if !strings.Contains(logText, "[REDACTED]") {
		t.Fatalf("redaction marker is absent from output: %q", logText)
	}

	evidence := waitForProcessStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want removed", removal)
	}
	if _, err := os.Stat(fixture.workspaceDir); err != nil {
		t.Fatalf("workspace was touched by Remove: %v", err)
	}
	if err := fixture.adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(fixture.workspaceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "process", Scenario: "manifest-environment-permissions-exit",
	})
}

// TestProcessRuntimeNoReplayAcrossControllerRestart proves a lost spawn
// response is never reported as never-started and is never replayed by a
// reconstructed controller.
func TestProcessRuntimeNoReplayAcrossControllerRestart(t *testing.T) {
	ctx := t.Context()
	fixture := newProcessFixture(t, "workspace-process-2")
	identity := testIdentity("exec-p2", "action-p2", "workload-p2", "idem-p2")
	resourceRoot := fixture.resourceRoot(t, identity.WorkloadID)

	executor := &faultExecutor{inner: fixture.driver, loseSpawn: true, spawnEffect: true}
	adapter := newProcessAdapterWithExecutor(t, fixture, executor)
	request := testPrepareRequest(
		identity,
		fixture.workspaceID,
		"build",
		"task-1",
		testContainer("registry.example.com/tools/build@sha256:"+strings.Repeat("a", 64), []string{
			`echo launched >> "$PWD/launch-count"`,
			`sleep 30`,
		}, []definition.EnvVar{{Name: "RESOURCE_ROOT", Value: resourceRoot}}, nil),
		nil,
		"tok-abc",
	)
	if _, err := adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	start, err := adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err == nil {
		t.Fatal("lost spawn response returned no error")
	}
	if start.Outcome != engine.StartUnknown {
		t.Fatalf("lost spawn outcome = %q; want unknown", start.Outcome)
	}
	waitForFile(t, ctx, filepath.Join(fixture.workspaceDir, "launch-count"))

	// A reconstructed controller uses a fresh driver over the same durable
	// state and must reuse the running group without spawning again.
	recoveredDriver, err := processdriver.New(processdriver.Config{StateDir: filepath.Join(fixture.root, "driver-state")})
	if err != nil {
		t.Fatalf("recovered driver: %v", err)
	}
	t.Cleanup(func() { _ = recoveredDriver.Close(context.Background()) })
	recovered := newProcessAdapter(t, fixture, recoveredDriver)

	observation := waitForProcessObservation(t, ctx, recovered,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)
	if observation.Kind != kernel.ObservationRunning {
		t.Fatalf("recovered observation = %+v; want running", observation)
	}
	recoveredStart, err := recovered.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("recovered Start: %v", err)
	}
	if recoveredStart.Outcome != engine.StartAccepted {
		t.Fatalf("recovered start outcome = %q; want accepted", recoveredStart.Outcome)
	}
	if executor.spawnCount() != 1 {
		t.Fatalf("spawn calls = %d; want exactly 1", executor.spawnCount())
	}
	if got := strings.Count(readFileString(t, filepath.Join(fixture.workspaceDir, "launch-count")), "launched"); got != 1 {
		t.Fatalf("workload launched %d times; want 1", got)
	}

	evidence := waitForProcessStop(t, ctx, recovered, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	// The first controller's waiter reaps the leader and persists its exit
	// status; the reconstructed controller can only prove the group is gone,
	// which stays unknown (never running and never never-started).
	waitForProcessObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	// A retry after exit must still reuse the same group and never relaunch.
	recoveredStart, err = recovered.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Start after exit: %v", err)
	}
	if recoveredStart.Outcome != engine.StartAccepted {
		t.Fatalf("start after exit outcome = %q; want accepted", recoveredStart.Outcome)
	}
	if got := strings.Count(readFileString(t, filepath.Join(fixture.workspaceDir, "launch-count")), "launched"); got != 1 {
		t.Fatalf("workload launched %d times after retry; want 1", got)
	}
	if _, err := recovered.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// A lost spawn before any effect stays unknown and is never retried.
	secondIdentity := testIdentity("exec-p2b", "action-p2b", "workload-p2b", "idem-p2b")
	secondRoot := fixture.resourceRoot(t, secondIdentity.WorkloadID)
	beforeEffect := &faultExecutor{inner: fixture.driver, loseSpawn: true, spawnEffect: false}
	secondAdapter := newProcessAdapterWithExecutor(t, fixture, beforeEffect)
	secondRequest := testPrepareRequest(
		secondIdentity,
		fixture.workspaceID,
		"build",
		"task-1",
		testContainer("registry.example.com/tools/build@sha256:"+strings.Repeat("a", 64), []string{
			`echo launched >> "$PWD/launch-count"`,
			`sleep 30`,
		}, []definition.EnvVar{{Name: "RESOURCE_ROOT", Value: secondRoot}}, nil),
		nil,
		"tok-abc",
	)
	if _, err := secondAdapter.Prepare(ctx, secondRequest); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	secondStart, err := secondAdapter.Start(ctx, engine.StartRequest{Identity: secondIdentity, WorkspaceID: fixture.workspaceID})
	if err == nil || secondStart.Outcome != engine.StartUnknown {
		t.Fatalf("pre-effect lost spawn = %q/%v; want unknown", secondStart.Outcome, err)
	}
	if beforeEffect.spawnCount() != 1 {
		t.Fatalf("pre-effect spawn calls = %d; want 1", beforeEffect.spawnCount())
	}
	// The uncertain intent is not replayed: a second start makes no new spawn.
	secondStart, _ = secondAdapter.Start(ctx, engine.StartRequest{Identity: secondIdentity, WorkspaceID: fixture.workspaceID})
	if secondStart.Outcome != engine.StartUnknown {
		t.Fatalf("retry of pre-effect lost spawn = %q; want unknown", secondStart.Outcome)
	}
	if beforeEffect.spawnCount() != 1 {
		t.Fatalf("retry spawned again: %d calls", beforeEffect.spawnCount())
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "process", Scenario: "no-replay-across-controller-restart",
	})
}

// TestProcessRuntimeWorkspaceExclusivityAndCrash proves a second controller
// cannot release a workspace a live workload owns, and that ownership survives
// a controller crash until confirmed stop and removal.
func TestProcessRuntimeWorkspaceExclusivityAndCrash(t *testing.T) {
	ctx := t.Context()
	fixture := newProcessFixture(t, "workspace-process-3")
	identity := testIdentity("exec-p3", "action-p3", "workload-p3", "idem-p3")
	resourceRoot := fixture.resourceRoot(t, identity.WorkloadID)

	request := testPrepareRequest(
		identity,
		fixture.workspaceID,
		"build",
		"task-1",
		testContainer("registry.example.com/tools/build@sha256:"+strings.Repeat("a", 64), []string{
			`echo owned > "$PWD/owned.txt"`,
			`sleep 30`,
		}, []definition.EnvVar{{Name: "RESOURCE_ROOT", Value: resourceRoot}}, nil),
		nil,
		"tok-abc",
	)
	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, ctx, filepath.Join(fixture.workspaceDir, "owned.txt"))

	// A second controller sees the workspace as available but cannot release
	// it while the first workload's manifest is live.
	secondDriver, err := processdriver.New(processdriver.Config{StateDir: filepath.Join(fixture.root, "driver-state")})
	if err != nil {
		t.Fatalf("second driver: %v", err)
	}
	t.Cleanup(func() { _ = secondDriver.Close(context.Background()) })
	second := newProcessAdapter(t, fixture, secondDriver)
	if err := second.Release(ctx, fixture.workspaceID); err == nil {
		t.Fatal("second controller released a workspace owned by a live workload")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	status, err := second.Status(ctx, fixture.workspaceID)
	if err != nil || !status.Available {
		t.Fatalf("workspace status = %+v, %v; want available", status, err)
	}

	// Simulate a controller crash: a third controller reconstructs from disk
	// and still sees the running group.
	crashDriver, err := processdriver.New(processdriver.Config{StateDir: filepath.Join(fixture.root, "driver-state")})
	if err != nil {
		t.Fatalf("crash driver: %v", err)
	}
	t.Cleanup(func() { _ = crashDriver.Close(context.Background()) })
	crashed := newProcessAdapter(t, fixture, crashDriver)
	waitForProcessObservation(t, ctx, crashed,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)
	if err := crashed.Release(ctx, fixture.workspaceID); err == nil {
		t.Fatal("reconstructed controller released a workspace owned by a running workload")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	evidence := waitForProcessStop(t, ctx, crashed, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	if removal, err := crashed.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if err := crashed.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release after confirmed cleanup: %v", err)
	}
	if _, err := os.Stat(fixture.workspaceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "process", Scenario: "workspace-exclusivity-and-crash",
	})
}

// TestProcessRuntimeCredentialRefreshWithoutRestart proves the token file is
// replaced atomically, visible to the running workload without a restart and
// recoverable after a lost replacement response.
func TestProcessRuntimeCredentialRefreshWithoutRestart(t *testing.T) {
	ctx := t.Context()
	fixture := newProcessFixture(t, "workspace-process-4")
	identity := testIdentity("exec-p4", "action-p4", "workload-p4", "idem-p4")
	resourceRoot := fixture.resourceRoot(t, identity.WorkloadID)

	request := testPrepareRequest(
		identity,
		fixture.workspaceID,
		"build",
		"task-1",
		testContainer("registry.example.com/tools/build@sha256:"+strings.Repeat("a", 64), []string{
			`echo started >> "$PWD/starts"`,
			`i=0`,
			`while [ ! -f "$PWD/stop" ]; do`,
			`  cat "$RESOURCE_ROOT/kodama/secrets/KODAMA_TOKEN" > "$PWD/token-seen"`,
			`  i=$((i+1))`,
			`  if [ "$i" -ge 20 ]; then break; fi`,
			`  sleep 1`,
			`done`,
		}, []definition.EnvVar{{Name: "RESOURCE_ROOT", Value: resourceRoot}}, nil),
		nil,
		"tok-abc",
	)
	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForContent(t, ctx, filepath.Join(fixture.workspaceDir, "token-seen"), "tok-abc")
	waitForProcessObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)

	if _, err := fixture.adapter.RefreshCredential(ctx, engine.CredentialRequest{Identity: identity, Credential: "tok-new"}); err != nil {
		t.Fatalf("RefreshCredential: %v", err)
	}
	waitForContent(t, ctx, filepath.Join(fixture.workspaceDir, "token-seen"), "tok-new")
	waitForProcessObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)
	if got := strings.Count(readFileString(t, filepath.Join(fixture.workspaceDir, "starts")), "started"); got != 1 {
		t.Fatalf("workload restarted %d times during credential refresh; want 1", got)
	}

	// A lost replacement response leaves the previous token intact; a retry
	// delivers the new value.
	faulty := &faultFiles{Files: fixture.driver, failSuffix: "KODAMA_TOKEN"}
	faultyAdapter, err := process.New(process.Config{
		Root:          fixture.root,
		WorkspaceRoot: fixture.workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      fixture.driver,
		Observer:      fixture.driver,
		Files:         faulty,
		Cleanup:       process.CleanupPolicy{Attempts: 3, Backoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("faulty adapter: %v", err)
	}
	if _, err := faultyAdapter.RefreshCredential(ctx, engine.CredentialRequest{Identity: identity, Credential: "tok-lost"}); err == nil {
		t.Fatal("lost replacement response returned no error")
	} else {
		t.Logf("failed credential write error (expected): %v", err)
	}
	credentialPath := filepath.Join(resourceRoot, "kodama", "secrets", "KODAMA_TOKEN")
	if got := readFileString(t, credentialPath); got != "tok-new" {
		t.Fatalf("credential after failed write = %q; want the previous tok-new", got)
	}
	if _, err := faultyAdapter.RefreshCredential(ctx, engine.CredentialRequest{Identity: identity, Credential: "tok-retry"}); err != nil {
		t.Fatalf("RefreshCredential retry: %v", err)
	}
	waitForContent(t, ctx, filepath.Join(fixture.workspaceDir, "token-seen"), "tok-retry")
	if err := os.WriteFile(filepath.Join(fixture.workspaceDir, "stop"), []byte("stop"), 0o644); err != nil {
		t.Fatalf("write stop marker: %v", err)
	}
	waitForProcessObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	if evidence := waitForProcessStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("Stop = %+v; want confirmed", evidence)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "process", Scenario: "credential-refresh-without-restart",
	})
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// faultExecutor wraps the real process executor and loses exactly one spawn
// response, optionally after the effect.
type faultExecutor struct {
	inner       process.Executor
	mu          sync.Mutex
	calls       int
	loseSpawn   bool
	spawnEffect bool
}

func (f *faultExecutor) Spawn(ctx context.Context, request process.SpawnRequest) (process.SpawnResult, error) {
	f.mu.Lock()
	f.calls++
	lose := f.loseSpawn
	effect := f.spawnEffect
	f.loseSpawn = false
	f.mu.Unlock()
	if !lose {
		return f.inner.Spawn(ctx, request)
	}
	if !effect {
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "lost before effect"},
			errors.New("injected lost spawn response before effect")
	}
	if _, err := f.inner.Spawn(ctx, request); err != nil {
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "lost after effect"},
			errors.New("injected lost spawn response after effect")
	}
	return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "lost after effect"},
		errors.New("injected lost spawn response after effect")
}

func (f *faultExecutor) SignalGroup(ctx context.Context, group process.GroupID) error {
	return f.inner.SignalGroup(ctx, group)
}

func (f *faultExecutor) RemoveGroup(ctx context.Context, group process.GroupID) error {
	return f.inner.RemoveGroup(ctx, group)
}

func (f *faultExecutor) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// faultFiles fails exactly one write whose target matches a suffix. The failure
// happens before the effect, so the previous credential remains intact.
type faultFiles struct {
	process.Files
	mu         sync.Mutex
	failSuffix string
	failed     bool
}

func (f *faultFiles) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	fail := !f.failed && strings.HasSuffix(path, f.failSuffix)
	if fail {
		f.failed = true
	}
	f.mu.Unlock()
	if fail {
		return errors.New("injected lost credential write")
	}
	return f.Files.WriteFile(ctx, path, data, mode)
}

func newProcessAdapterWithExecutor(t *testing.T, f *processFixture, executor process.Executor) *process.Adapter {
	t.Helper()
	adapter, err := process.New(process.Config{
		Root:          f.root,
		WorkspaceRoot: f.workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      executor,
		Observer:      f.driver,
		Files:         f.driver,
		Cleanup:       process.CleanupPolicy{Attempts: 3, Backoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("process adapter with executor: %v", err)
	}
	return adapter
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
