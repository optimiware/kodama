//go:build component

package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain installs the harness signal handler and orphan sweep for this
// component test binary.
func TestMain(m *testing.M) {
	os.Exit(Main(m))
}

func requirePodman(t *testing.T, podman *Podman) {
	t.Helper()
	if err := podman.Version(context.Background()); err != nil {
		SkipOrFail(t, requireInfra(), err)
	}
}

func requireImage(t *testing.T, shard *Shard, tag string) string {
	t.Helper()
	return shard.RequireImage(shard.Context(), tag)
}

func runContainer(t *testing.T, shard *Shard, spec RunSpec) *Container {
	t.Helper()
	container, err := shard.Run(shard.Context(), spec)
	if err != nil {
		t.Fatalf("run %s: %v", spec.Image, err)
	}
	return container
}

func waitReady(t *testing.T, shard *Shard, container *Container, name string, probe Probe) {
	t.Helper()
	ctx := shard.Context()
	if err := shard.WaitReady(ctx, name, probe); err != nil {
		if logs, logErr := container.Logs(ctx); logErr == nil {
			t.Logf("container logs for %s:\n%s", container.Name, logs)
		}
		t.Fatalf("wait ready %s: %v", name, err)
	}
}

// postgresCompleteStartup opens a real PostgreSQL connection and reads the
// protocol exchange through ReadyForQuery, proving the server actually serves
// sessions after the readiness barrier rather than answering from a starting,
// recovering or shutting-down state. It needs no driver.
func postgresCompleteStartup(ctx context.Context, addr, user, database string) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	payload := []byte{0, 3, 0, 0} // protocol 3.0
	for _, pair := range [][2]string{{"user", user}, {"database", database}} {
		payload = append(payload, pair[0]...)
		payload = append(payload, 0)
		payload = append(payload, pair[1]...)
		payload = append(payload, 0)
	}
	payload = append(payload, 0) // terminator
	message := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(message, uint32(len(message)))
	copy(message[4:], payload)
	if _, err := conn.Write(message); err != nil {
		return err
	}
	for {
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 1<<20 {
			return fmt.Errorf("harness: postgres message has invalid length %d", length)
		}
		body := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		switch header[0] {
		case 'E':
			return fmt.Errorf("harness: postgres rejected the connection: %s", body)
		case 'Z':
			return nil
		}
	}
}

func inspectStatus(t *testing.T, shard *Shard, name string) string {
	t.Helper()
	result, err := shard.Podman.run(shard.Context(), 0, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return strings.TrimSpace(string(result.Stdout))
}

func containerExists(t *testing.T, podman *Podman, name string) bool {
	t.Helper()
	exists, err := resourceExists(context.Background(), podman, "container", name)
	if err != nil {
		t.Fatalf("probe container %s: %v", name, err)
	}
	return exists
}

// resourceExists reports whether Podman explicitly reports one named resource
// as present. Podman's `exists` commands exit 1 only for an explicit absent
// answer; every other failure (a lost runtime, a timeout or an exec error)
// means existence could not be proven, so it is returned as an error instead of
// being collapsed into absence.
func resourceExists(ctx context.Context, podman *Podman, kind, name string) (bool, error) {
	res, err := podman.run(ctx, 0, kind, "exists", name)
	if err == nil {
		return true, nil
	}
	if res.ExitCode == 1 {
		return false, nil
	}
	return false, fmt.Errorf("harness: probe %s %s: %w", kind, name, err)
}

// ownCleanupVerdict decides whether a post-kill orphan sweep removed this
// process's own resources. The aggregate CleanupOrphans error can belong to a
// foreign lane whose resource another process removed between the sweep's list
// and its inspect (test/integration/lifecycle follows the same precedent: a
// sweep error is logged, an own leak is fatal). Absence must be an explicit
// Podman answer: a probe that cannot prove absence fails the verdict.
func ownCleanupVerdict(ctx context.Context, podman *Podman, containerName, networkName string, sweepErr error, logf func(string, ...any)) error {
	if sweepErr != nil && logf != nil {
		logf("harness: orphan sweep error (logged, not fatal; a foreign lane can race the same sweep): %v", sweepErr)
	}
	var errs []error
	for _, resource := range []struct{ kind, name string }{
		{"container", containerName},
		{"network", networkName},
	} {
		exists, err := resourceExists(ctx, podman, resource.kind, resource.name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if exists {
			errs = append(errs, fmt.Errorf("harness: orphan %s %s survived cleanup", resource.kind, resource.name))
		}
	}
	return errors.Join(errs...)
}

// foreignOrphanRunner wraps the real Podman runner for the foreign-sweep race
// fixture. It reports one extra container during the orphan listing and fails
// that container's inspect, exactly as another lane removing its own resource
// between the sweep's list and inspect would. Every other command, including
// this process's own container, volume and network operations, is forwarded
// unchanged so the sweep really removes this test's orphaned resources.
type foreignOrphanRunner struct {
	real Runner
	name string
}

// Run implements Runner.
func (r foreignOrphanRunner) Run(ctx context.Context, cmd Command) (Result, error) {
	args := cmd.Args
	switch {
	case len(args) > 0 && args[0] == "ps":
		res, err := r.real.Run(ctx, cmd)
		if err != nil {
			return res, err
		}
		stdout := append([]byte(nil), res.Stdout...)
		if len(stdout) > 0 && stdout[len(stdout)-1] != '\n' {
			stdout = append(stdout, '\n')
		}
		res.Stdout = append(stdout, r.name+"\n"...)
		return res, nil
	case len(args) > 1 && args[0] == "inspect" && args[len(args)-1] == r.name:
		return Result{ExitCode: 125, Stderr: []byte("no such container: " + r.name)},
			fmt.Errorf("harness: inspect container %s: foreign resource already removed", r.name)
	default:
		return r.real.Run(ctx, cmd)
	}
}

// TestSmokePostgres pulls, starts, probes with a real startup exchange, stops
// and removes the pinned PostgreSQL image. Trust authentication lets the smoke
// test complete a real connection without a driver.
func TestSmokePostgres(t *testing.T) {
	shard := NewShard(t)
	requireImage(t, shard, ImagePostgres)
	container := runContainer(t, shard, RunSpec{
		Image: ImagePostgres,
		Ports: []Port{{Container: 5432}},
		Env: map[string]string{
			"POSTGRES_HOST_AUTH_METHOD": "trust",
			"POSTGRES_USER":             "postgres",
			"POSTGRES_DB":               "postgres",
		},
	})
	ctx := shard.Context()
	addr, err := container.Addr(ctx, 5432)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	waitReady(t, shard, container, "postgres", PostgresProbe(addr, "postgres"))
	// A readiness reply alone is not proof: the first real session must reach
	// ReadyForQuery, so a starting/recovering server cannot fake the barrier.
	if err := postgresCompleteStartup(ctx, addr, "postgres", "postgres"); err != nil {
		t.Fatalf("postgres session after readiness: %v", err)
	}
	if logs, err := container.Logs(ctx); err != nil || len(logs) == 0 {
		t.Fatalf("logs after readiness: %d bytes, %v", len(logs), err)
	}
	if err := container.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if status := inspectStatus(t, shard, container.Name); status != "exited" {
		t.Fatalf("status after stop = %q, want exited", status)
	}
	if err := container.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if containerExists(t, shard.Podman, container.Name) {
		t.Fatalf("container %s still exists after removal", container.Name)
	}
}

// TestSmokeNATS pulls, starts, probes, stops and removes the pinned NATS image
// with JetStream enabled.
func TestSmokeNATS(t *testing.T) {
	shard := NewShard(t)
	requireImage(t, shard, ImageNATS)
	container := runContainer(t, shard, RunSpec{
		Image: ImageNATS,
		Ports: []Port{{Container: 4222}, {Container: 8222}},
		Cmd:   []string{"-js", "-m", "8222"},
	})
	ctx := shard.Context()
	monitor, err := container.Addr(ctx, 8222)
	if err != nil {
		t.Fatalf("monitor address: %v", err)
	}
	waitReady(t, shard, container, "nats", NATSProbe("http://"+monitor))
	if err := container.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if status := inspectStatus(t, shard, container.Name); status != "exited" {
		t.Fatalf("status after stop = %q, want exited", status)
	}
	if err := container.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if containerExists(t, shard.Podman, container.Name) {
		t.Fatalf("container %s still exists after removal", container.Name)
	}
}

// TestSmokeRegistry pulls, starts, probes, stops and removes the pinned
// registry image.
func TestSmokeRegistry(t *testing.T) {
	shard := NewShard(t)
	requireImage(t, shard, ImageRegistry)
	container := runContainer(t, shard, RunSpec{
		Image: ImageRegistry,
		Ports: []Port{{Container: 5000}},
	})
	ctx := shard.Context()
	baseURL, err := container.Addr(ctx, 5000)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	waitReady(t, shard, container, "registry", RegistryProbe("http://"+baseURL))
	if err := container.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if status := inspectStatus(t, shard, container.Name); status != "exited" {
		t.Fatalf("status after stop = %q, want exited", status)
	}
	if err := container.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if containerExists(t, shard.Podman, container.Name) {
		t.Fatalf("container %s still exists after removal", container.Name)
	}
}

// TestShardIsolation starts two shards concurrently and proves they have
// distinct networks and Podman-allocated host ports.
func TestShardIsolation(t *testing.T) {
	first := NewShard(t)
	second := NewShard(t)
	requireImage(t, first, ImageNATS)
	requireImage(t, second, ImageNATS)

	firstNetwork, err := first.Network(first.Context())
	if err != nil {
		t.Fatalf("first network: %v", err)
	}
	secondNetwork, err := second.Network(second.Context())
	if err != nil {
		t.Fatalf("second network: %v", err)
	}
	if firstNetwork == secondNetwork {
		t.Fatalf("shards share network %q", firstNetwork)
	}

	type started struct {
		container *Container
		err       error
	}
	results := make(chan started, 2)
	for _, shard := range []*Shard{first, second} {
		go func() {
			container, err := shard.Run(shard.Context(), RunSpec{
				Image: ImageNATS,
				Ports: []Port{{Container: 4222}, {Container: 8222}},
				Cmd:   []string{"-js", "-m", "8222"},
			})
			results <- started{container: container, err: err}
		}()
	}
	firstResult := <-results
	secondResult := <-results
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("concurrent run: %v / %v", firstResult.err, secondResult.err)
	}

	firstPort, err := firstResult.container.HostPort(first.Context(), 4222)
	if err != nil {
		t.Fatalf("first port: %v", err)
	}
	secondPort, err := secondResult.container.HostPort(second.Context(), 4222)
	if err != nil {
		t.Fatalf("second port: %v", err)
	}
	if firstPort == secondPort {
		t.Fatalf("shards share host port %d", firstPort)
	}
	firstMonitor, _ := firstResult.container.Addr(first.Context(), 8222)
	secondMonitor, _ := secondResult.container.Addr(second.Context(), 8222)
	waitReady(t, first, firstResult.container, "first nats", NATSProbe("http://"+firstMonitor))
	waitReady(t, second, secondResult.container, "second nats", NATSProbe("http://"+secondMonitor))
}

// TestKillRecoveryHelperProcess is the child process for TestKillRecovery. It
// starts a container, publishes its name, and waits to be SIGKILLed.
func TestKillRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("KODAMA_HARNESS_HELPER") != "1" {
		t.Skip("helper process only")
	}
	shard := NewShard(t)
	requireImage(t, shard, ImageRegistry)
	container := runContainer(t, shard, RunSpec{
		Image: ImageRegistry,
		Ports: []Port{{Container: 5000}},
	})
	nameFile := os.Getenv("KODAMA_HELPER_NAME_FILE")
	if nameFile == "" {
		t.Fatal("KODAMA_HELPER_NAME_FILE is not set")
	}
	if err := os.WriteFile(nameFile, []byte(container.Name), 0o644); err != nil {
		t.Fatalf("write name file: %v", err)
	}
	t.Logf("helper started %s; waiting for SIGKILL", container.Name)
	select {}
}

// TestKillRecovery proves that CleanupOrphans removes resources whose owner
// test process was SIGKILLed.
func TestKillRecovery(t *testing.T) {
	podman := NewPodman(WithLogger(t.Logf), WithEvidenceDir(os.Getenv("KODAMA_EVIDENCE_DIR")))
	requirePodman(t, podman)
	ctx := context.Background()
	if _, err := podman.EnsureImage(ctx, ImageRegistry); err != nil {
		SkipOrFail(t, requireInfra(), err)
	}

	dir := t.TempDir()
	nameFile := filepath.Join(dir, "container-name")
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	helper := exec.Command(executable, "-test.run=^TestKillRecoveryHelperProcess$", "-test.v")
	helper.Env = append(os.Environ(),
		"KODAMA_HARNESS_HELPER=1",
		"KODAMA_HELPER_NAME_FILE="+nameFile,
	)
	var output bytes.Buffer
	helper.Stdout = &output
	helper.Stderr = &output
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	var containerName string
	probe := func(context.Context) error {
		data, err := os.ReadFile(nameFile)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return os.ErrNotExist
		}
		containerName = string(data)
		return nil
	}
	if err := WaitReady(context.Background(), RealClock{}, "helper container", probe, DefaultWaitOptions()); err != nil {
		t.Fatalf("helper did not start a container: %v\n%s", err, output.String())
	}
	// Positive control before the kill: the container must exist now, so a
	// clean sweep afterwards proves removal instead of a probe that can never
	// see it.
	if exists, err := resourceExists(ctx, podman, "container", containerName); err != nil {
		t.Fatalf("probe helper container before kill: %v", err)
	} else if !exists {
		t.Fatalf("helper container %s was not present before the kill", containerName)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()
	if ProcessAlive(helper.Process.Pid) {
		t.Fatalf("helper pid %d is still alive", helper.Process.Pid)
	}

	shardID := strings.TrimSuffix(containerName, "-docker-io-library-registry-2-8-3")
	networkName := "kodama-" + shardID

	// A foreign lane's sweep can race this one: the orphan listing sees a
	// resource that another process removes before its inspect, so
	// CleanupOrphans returns an aggregate error even when every one of this
	// test's own resources is gone. The foreign error is logged; the exact
	// absence of this test's own container and network decides the test. The
	// runner injects that race deterministically while forwarding every real
	// own-resource operation to Podman.
	t.Run("foreign sweep race", func(t *testing.T) {
		const foreignName = "kodama-foreign-lane-orphan"
		racing := NewPodman(
			WithRunner(foreignOrphanRunner{real: ExecRunner{}, name: foreignName}),
			WithLogger(t.Logf),
			WithEvidenceDir(os.Getenv("KODAMA_EVIDENCE_DIR")),
		)
		sweepErr := racing.CleanupOrphans(context.Background())
		if sweepErr == nil {
			t.Fatal("injected foreign sweep failure did not surface")
		}
		if !strings.Contains(sweepErr.Error(), foreignName) {
			t.Fatalf("sweep error %v does not name the foreign resource %s", sweepErr, foreignName)
		}
		var logged []string
		if err := ownCleanupVerdict(context.Background(), racing, containerName, networkName, sweepErr,
			func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }); err != nil {
			t.Fatalf("own resources after a foreign sweep race: %v", err)
		}
		if !strings.Contains(strings.Join(logged, "\n"), foreignName) {
			t.Fatalf("foreign sweep error was not logged: %v", logged)
		}
	})
}

// existsProbe decodes a `container exists`/`network exists` command.
func existsProbe(args []string) (kind, name string, ok bool) {
	if len(args) == 3 && args[1] == "exists" && (args[0] == "container" || args[0] == "network") {
		return args[0], args[2], true
	}
	return "", "", false
}

// TestKillRecoveryCleanupVerdict pins the post-kill cleanup decision used by
// TestKillRecovery. A sweep error caused by a foreign resource is logged and
// never fatal; a surviving own container or network is fatal; and a probe that
// cannot prove absence is fatal instead of being read as absence.
func TestKillRecoveryCleanupVerdict(t *testing.T) {
	const (
		containerName = "orphan-container"
		networkName   = "kodama-orphan"
	)
	type probe struct {
		exists bool
		err    error
	}
	cases := []struct {
		name      string
		sweepErr  error
		container probe
		network   probe
		wantErr   string
		wantLog   bool
	}{
		{
			name:      "foreign sweep error with own resources absent",
			sweepErr:  errors.New("inspect container foreign-orphan: no such container"),
			container: probe{exists: false},
			network:   probe{exists: false},
			wantLog:   true,
		},
		{
			name:      "surviving own container",
			container: probe{exists: true},
			network:   probe{exists: false},
			wantErr:   "container " + containerName + " survived",
		},
		{
			name:      "surviving own network",
			container: probe{exists: false},
			network:   probe{exists: true},
			wantErr:   "network " + networkName + " survived",
		},
		{
			name:      "failed container absence probe",
			container: probe{err: errors.New("podman runtime unavailable")},
			network:   probe{exists: false},
			wantErr:   "probe container " + containerName,
		},
		{
			name:      "failed network absence probe",
			container: probe{exists: false},
			network:   probe{err: errors.New("podman runtime unavailable")},
			wantErr:   "probe network " + networkName,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			runner.fn = func(_ context.Context, cmd Command) (Result, error) {
				kind, name, ok := existsProbe(cmd.Args)
				if !ok {
					return Result{}, fmt.Errorf("unexpected command %v", cmd.Args)
				}
				var outcome probe
				switch {
				case kind == "container" && name == containerName:
					outcome = tc.container
				case kind == "network" && name == networkName:
					outcome = tc.network
				default:
					return Result{}, fmt.Errorf("unexpected probe %s %s", kind, name)
				}
				switch {
				case outcome.err != nil:
					return Result{ExitCode: 125, Stderr: []byte(outcome.err.Error())}, outcome.err
				case outcome.exists:
					return Result{}, nil
				default:
					return Result{ExitCode: 1, Stderr: []byte("no such object")}, errors.New("exit status 1")
				}
			}
			podman := NewPodman(WithRunner(runner))
			var logged []string
			err := ownCleanupVerdict(context.Background(), podman, containerName, networkName, tc.sweepErr,
				func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verdict = %v; want success", err)
				}
			} else {
				if err == nil {
					t.Fatalf("verdict = nil; want a failure containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("verdict = %v; want a failure containing %q", err, tc.wantErr)
				}
			}
			loggedText := strings.Join(logged, "\n")
			if tc.wantLog {
				if tc.sweepErr == nil || !strings.Contains(loggedText, tc.sweepErr.Error()) {
					t.Fatalf("sweep error was not logged: %v", logged)
				}
			} else if len(logged) != 0 {
				t.Fatalf("unexpected log output: %v", logged)
			}
		})
	}
}
