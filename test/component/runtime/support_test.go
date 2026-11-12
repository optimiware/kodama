//go:build component

// Package runtimeconformance is the real Process/Podman runtime
// conformance suite. Every test in this package drives a real host process
// group or a real Podman API service on a private Unix socket; none of them
// uses the deterministic unit fakes.
package runtimeconformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	processdriver "github.com/optimiware/kodama/internal/shell/app/drivers/process"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/runtime/process"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

const testKodamaURL = "https://kodama.example.com"

// TestMain installs the harness signal handler and dead-owner sweep, then
// removes only the runtime-labelled containers and volumes this run owns. The
// sweep is scoped by kodama.test=1 and this run's owner pid; it never selects
// by the product workspace label, so a real Kodama's retained workspaces and a
// concurrent shard's live resources survive.
func TestMain(m *testing.M) {
	code := harness.Main(m)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runtimesupport.RemoveRunContainers(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "runtime conformance: post-run container sweep: %v\n", err)
	}
	if err := runtimesupport.RemoveRunVolumes(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "runtime conformance: post-run volume sweep: %v\n", err)
	}
	os.Exit(code)
}

func testIdentity(execution, action, workload, key string) engine.EffectIdentity {
	return testIdentityInScope(runtimesupport.RunScope(), execution, action, workload, key)
}

// testIdentityInScope maps complete literal identities into one run scope. It
// is called once per fixture so retries and reconstructed controllers inside
// the run derive exactly the same execution, action, workload and idempotency
// values.
func testIdentityInScope(scope runtimesupport.Scope, execution, action, workload, key string) engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    id.ExecutionID(scope.ID(execution)),
		ActionID:       id.ActionID(scope.ID(action)),
		WorkloadID:     id.WorkloadID(scope.ID(workload)),
		IdempotencyKey: scope.ID(key),
	}
}

func testContainer(image string, commands []string, env []definition.EnvVar, secrets []id.SecretName) definition.Container {
	return definition.Container{
		Image:    image,
		Commands: commands,
		Env:      env,
		Secrets:  secrets,
	}
}

func testPrepareRequest(
	identity engine.EffectIdentity,
	workspaceID id.WorkspaceID,
	actionName id.ActionName,
	taskID id.TaskID,
	container definition.Container,
	secrets []engine.SecretValue,
	credential string,
) engine.PrepareRequest {
	return engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		ActionName:  actionName,
		TaskID:      taskID,
		Container:   container,
		Secrets:     secrets,
		Inputs: map[id.InputKey]string{
			"input-a": "line1\nline2\n",
		},
		Outputs: map[id.ActionName]kernel.Outputs{
			"earlier": {"out-a": "earlier-output"},
		},
		Task:       []byte(`{"id":"task-1"}`),
		Credential: credential,
	}
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class; want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s; want %s (error %v)", class, want, err)
	}
}

// waitForFile waits for a file to exist. It is a barrier with a bounded
// attempt, not a fixed sleep.
func waitForFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	probe := func(context.Context) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errors.New("path is a directory")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "file "+path, probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for %s: %v", path, err)
	}
}

// waitForContent waits for a file to contain want. It is a barrier, not a
// sleep.
func waitForContent(t *testing.T, ctx context.Context, path, want string) {
	t.Helper()
	probe := func(context.Context) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(want) {
			return errors.New("content does not match yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "content of "+path, probe, harness.DefaultWaitOptions()); err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("wait for %s to equal %q: %v (last %q)", path, want, err, data)
	}
}

// waitForProcessStop retries the stop barrier until the adapter affirms
// whole-group termination. A signal acknowledgment alone is not confirmation,
// so the barrier is observed, not assumed.
func waitForProcessStop(t *testing.T, ctx context.Context, adapter *process.Adapter, request engine.StopRequest) kernel.StopEvidence {
	t.Helper()
	var last kernel.StopEvidence
	probe := func(ctx context.Context) error {
		evidence, err := adapter.Stop(ctx, request)
		last = evidence
		if err != nil {
			return err
		}
		if !evidence.IsConfirmed() {
			return errors.New("stop is not confirmed yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "process stop barrier", probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for process stop: %v (last %+v)", err, last)
	}
	return last
}

// waitForProcessObservation waits for the adapter to report the wanted kind.
func waitForProcessObservation(t *testing.T, ctx context.Context, adapter *process.Adapter, request engine.ObserveRequest, want kernel.ObservationKind) kernel.Observation {
	t.Helper()
	var last kernel.Observation
	probe := func(ctx context.Context) error {
		observation, err := adapter.Observe(ctx, request)
		if err != nil {
			return err
		}
		last = observation
		if observation.Kind != want {
			return errors.New("observation kind does not match yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "process observation", probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for process observation %s: %v (last %+v)", want, err, last)
	}
	return last
}

// waitForPodmanStop retries the stop barrier until the adapter affirms
// whole-container termination. A stop acknowledgment alone is not
// confirmation.
func waitForPodmanStop(t *testing.T, ctx context.Context, adapter *podman.Adapter, request engine.StopRequest) kernel.StopEvidence {
	t.Helper()
	var last kernel.StopEvidence
	probe := func(ctx context.Context) error {
		evidence, err := adapter.Stop(ctx, request)
		last = evidence
		if err != nil {
			return err
		}
		if !evidence.IsConfirmed() {
			return errors.New("stop is not confirmed yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "podman stop barrier", probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for podman stop: %v (last %+v)", err, last)
	}
	return last
}

// podmanContainerNames returns the containers bound to one workspace.
func podmanContainerNames(t *testing.T, ctx context.Context, workspaceID id.WorkspaceID) []string {
	t.Helper()
	names, err := runtimesupport.ContainerNamesByLabel(ctx, podman.LabelWorkspace, string(workspaceID))
	if err != nil {
		t.Fatalf("list workspace containers: %v", err)
	}
	return names
}

// waitForPodmanObservation waits for the Podman adapter to report the wanted
// kind.
func waitForPodmanObservation(t *testing.T, ctx context.Context, adapter *podman.Adapter, request engine.ObserveRequest, want kernel.ObservationKind) kernel.Observation {
	t.Helper()
	var last kernel.Observation
	probe := func(ctx context.Context) error {
		observation, err := adapter.Observe(ctx, request)
		if err != nil {
			return err
		}
		last = observation
		if observation.Kind != want {
			return errors.New("observation kind does not match yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "podman observation", probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for podman observation %s: %v (last %+v)", want, err, last)
	}
	return last
}

// ---------------------------------------------------------------------------
// Process fixture
// ---------------------------------------------------------------------------

type processFixture struct {
	adapter       *process.Adapter
	driver        *processdriver.Driver
	root          string
	workspaceRoot string
	workspaceDir  string
	workspaceID   id.WorkspaceID
}

func newProcessFixture(t *testing.T, base string) *processFixture {
	t.Helper()
	workspaceID := id.WorkspaceID(runtimesupport.RunScoped(base))
	root := t.TempDir()
	workspaceRoot := t.TempDir()
	workspaceDir := filepath.Join(workspaceRoot, string(workspaceID))
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace dir: %v", err)
	}
	driver, err := processdriver.New(processdriver.Config{StateDir: filepath.Join(root, "driver-state")})
	if err != nil {
		t.Fatalf("process driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	adapter, err := process.New(process.Config{
		Root:          root,
		WorkspaceRoot: workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      driver,
		Observer:      driver,
		Files:         driver,
		Cleanup:       process.CleanupPolicy{Attempts: 3, Backoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("process adapter: %v", err)
	}
	return &processFixture{
		adapter:       adapter,
		driver:        driver,
		root:          root,
		workspaceRoot: workspaceRoot,
		workspaceDir:  workspaceDir,
		workspaceID:   workspaceID,
	}
}

func (f *processFixture) resourceRoot(t *testing.T, workload id.WorkloadID) string {
	t.Helper()
	root, err := process.ResourceRoot(f.root, workload)
	if err != nil {
		t.Fatalf("ResourceRoot: %v", err)
	}
	return root
}

// newProcessAdapter builds a second controller over the same durable roots.
func newProcessAdapter(t *testing.T, f *processFixture, driver *processdriver.Driver) *process.Adapter {
	t.Helper()
	adapter, err := process.New(process.Config{
		Root:          f.root,
		WorkspaceRoot: f.workspaceRoot,
		KodamaURL:     testKodamaURL,
		Executor:      driver,
		Observer:      driver,
		Files:         driver,
		Cleanup:       process.CleanupPolicy{Attempts: 3, Backoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("reconstructed process adapter: %v", err)
	}
	return adapter
}

// ---------------------------------------------------------------------------
// Podman fixture
// ---------------------------------------------------------------------------

type podmanFixture struct {
	service     *runtimesupport.Service
	adapter     *podman.Adapter
	libpod      *podmandriver.Libpod
	files       *podmandriver.Files
	root        string
	workspaceID id.WorkspaceID
	volumeName  string
	image       runtimesupport.PinnedImage
	shardID     string
}

// newPodmanFixture builds a fixture in this process's run scope.
func newPodmanFixture(t *testing.T, base string) *podmanFixture {
	t.Helper()
	return newPodmanFixtureInScope(t, runtimesupport.RunScope(), base)
}

// newPodmanFixtureInScope derives the complete workspace identity from one run
// scope exactly once. Concurrent runs (and the twin-isolation regression test)
// pass distinct scopes so the adapter's deterministic workspace/volume names
// cannot collide.
func newPodmanFixtureInScope(t *testing.T, scope runtimesupport.Scope, base string) *podmanFixture {
	t.Helper()
	workspaceID := id.WorkspaceID(scope.ID(base))
	service := runtimesupport.StartService(t)
	image := runtimesupport.ResolveImage(t.Context(), t, service.Podman(), runtimesupport.ImageAlpineTag)
	root := t.TempDir()
	fixture := &podmanFixture{
		service:     service,
		root:        root,
		workspaceID: workspaceID,
		image:       image,
		shardID:     strings.ReplaceAll(t.Name(), "/", "-"),
	}
	volumeName, err := podman.WorkspaceVolumeName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceVolumeName: %v", err)
	}
	fixture.volumeName = volumeName
	if err := runtimesupport.CreateWorkspaceVolume(t.Context(), volumeName, string(workspaceID), fixture.shardID); err != nil {
		t.Fatalf("create workspace volume: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = runtimesupport.RemoveRunContainersInWorkspace(ctx, string(workspaceID))
		_ = runtimesupport.RemoveWorkspaceVolume(ctx, volumeName)
	})
	fixture.libpod, fixture.files, fixture.adapter = newPodmanAdapter(t, fixture, nil)
	return fixture
}

// newPodmanAdapter builds an adapter over the same root and (optionally) a
// wrapper Libpod. A nil libpod uses the real client.
func newPodmanAdapter(t *testing.T, f *podmanFixture, wrapper podman.Libpod) (*podmandriver.Libpod, *podmandriver.Files, *podman.Adapter) {
	t.Helper()
	libpod, err := podmandriver.NewLibpod(podmandriver.LibpodConfig{SocketPath: f.service.Socket(), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewLibpod: %v", err)
	}
	files, err := podmandriver.NewFiles(f.root)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	client := podman.Libpod(libpod)
	if wrapper != nil {
		client = wrapper
	}
	// Every container the adapter creates carries this run's cleanup labels, so
	// a post-run sweep can never select a foreign or live-owner container.
	client = runtimesupport.ScopedLibpod(client)
	adapter, err := podman.New(podman.Config{
		Libpod:    client,
		Files:     files,
		Root:      f.root,
		KodamaURL: testKodamaURL,
		Cleanup:   podman.CleanupPolicy{Attempts: 3, Backoff: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("podman adapter: %v", err)
	}
	return libpod, files, adapter
}

func (f *podmanFixture) request(identity engine.EffectIdentity, commands []string, env []definition.EnvVar, secrets []engine.SecretValue, credential string) engine.PrepareRequest {
	return testPrepareRequest(identity, f.workspaceID, "build", "task-1",
		testContainer(f.image.Reference, commands, env, secretNames(secrets)), secrets, credential)
}

func secretNames(secrets []engine.SecretValue) []id.SecretName {
	names := make([]id.SecretName, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secret.Name)
	}
	return names
}
