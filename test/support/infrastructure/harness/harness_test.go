package harness

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeRunner records commands and answers them with fn.
type fakeRunner struct {
	mu    sync.Mutex
	calls []Command
	fn    func(ctx context.Context, cmd Command) (Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, cmd Command) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return Result{}, nil
	}
	return fn(ctx, cmd)
}

func (f *fakeRunner) snapshot() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Command, len(f.calls))
	copy(out, f.calls)
	return out
}

func hasArgSequence(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// fakeClock advances on Sleep instead of waiting.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	return nil
}

func (c *fakeClock) sleepCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sleeps)
}

func TestRunArgsBuildsExplicitPodmanCommand(t *testing.T) {
	spec := RunSpec{
		Image:   "docker.io/library/postgres:18-alpine",
		Name:    "container-one",
		Network: "network-one",
		Ports:   []Port{{Container: 5432}},
		Volumes: []Volume{{Target: "/var/lib/postgresql/data"}, {Name: "named", Target: "/named"}},
		Env:     map[string]string{"POSTGRES_PASSWORD": "secret"},
		Labels:  map[string]string{"extra": "1", LabelShard: "caller-cannot-win"},
		Cmd:     []string{"postgres", "-c", "shared_buffers=128MB"},
	}
	args, err := runArgs(spec, "shard-1", 4242)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	if args[0] != "run" {
		t.Fatalf("first argument = %q, want run (no shell wrapper)", args[0])
	}
	for _, want := range [][]string{
		{"--detach"},
		{"--name", "container-one"},
		{"--network", "network-one"},
		{"--label", "kodama.test=1"},
		{"--label", "kodama.shard=shard-1"},
		{"--label", "kodama.owner-pid=4242"},
		{"--label", "extra=1"},
		{"--cpus", "1"},
		{"--memory", "1g"},
		{"-p", "127.0.0.1::5432"},
		{"-v", "/var/lib/postgresql/data"},
		{"-v", "named:/named"},
		{"--env", "POSTGRES_PASSWORD=secret"},
		{"docker.io/library/postgres:18-alpine", "postgres", "-c", "shared_buffers=128MB"},
	} {
		if !hasArgSequence(args, want...) {
			t.Errorf("args missing sequence %v:\n%v", want, args)
		}
	}
	// Reserved labels win over caller labels.
	if hasArgSequence(args, "--label", "kodama.shard=caller-cannot-win") {
		t.Error("caller label overrode reserved shard label")
	}
}

func TestRunArgsPassesMetacharactersAsSingleArguments(t *testing.T) {
	spec := RunSpec{
		Image: "img;touch /tmp/harness-pwned",
		Name:  "c",
		Cmd:   []string{"sh", "-c", "echo $(id)"},
	}
	args, err := runArgs(spec, "s", 1)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	image := args[len(args)-4]
	if image != "img;touch /tmp/harness-pwned" {
		t.Fatalf("image argument = %q, want verbatim value", image)
	}
	if _, err := os.Stat("/tmp/harness-pwned"); err == nil {
		t.Fatal("shell metacharacters were interpreted")
	}
}

func TestRunArgsRejectsInvalidSpecs(t *testing.T) {
	cases := map[string]RunSpec{
		"no image":         {Name: "c"},
		"no name":          {Image: "img"},
		"invalid port":     {Image: "img", Name: "c", Ports: []Port{{Container: 70000}}},
		"invalid protocol": {Image: "img", Name: "c", Ports: []Port{{Container: 80, Protocol: "icmp"}}},
		"volume no target": {Image: "img", Name: "c", Volumes: []Volume{{Name: "v"}}},
	}
	for name, spec := range cases {
		if _, err := runArgs(spec, "s", 1); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCommandsAlwaysCarryExplicitEnvironment(t *testing.T) {
	runner := &fakeRunner{}
	podman := NewPodman(WithRunner(runner))
	if err := podman.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	env := calls[0].Env
	if env == nil {
		t.Fatal("command environment is nil; it would inherit the host environment")
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "HOME=") {
		t.Fatalf("environment missing PATH/HOME: %v", env)
	}
	if calls[0].Name != "podman" {
		t.Fatalf("binary = %q, want podman", calls[0].Name)
	}
}

func TestExecRunnerRefusesNilEnvironment(t *testing.T) {
	_, err := (ExecRunner{}).Run(context.Background(), Command{Name: "true"})
	if err == nil || !strings.Contains(err.Error(), "nil environment") {
		t.Fatalf("error = %v, want nil environment refusal", err)
	}
}

func TestEnsureImagePullsWhenAbsentAndRecordsDigest(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeRunner{}
	pulled := false
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		switch {
		case len(cmd.Args) >= 2 && cmd.Args[0] == "image" && cmd.Args[1] == "inspect" && strings.Contains(cmd.Args[3], ".Digest"):
			if !pulled {
				return Result{Stderr: []byte("no such image"), ExitCode: 125}, errors.New("exit status 125")
			}
			return Result{Stdout: []byte("sha256:deadbeef\n")}, nil
		case len(cmd.Args) >= 2 && cmd.Args[0] == "image" && cmd.Args[1] == "inspect" && strings.Contains(cmd.Args[3], ".Id"):
			return Result{Stdout: []byte("image-id\n")}, nil
		case len(cmd.Args) >= 1 && cmd.Args[0] == "pull":
			pulled = true
			return Result{}, nil
		default:
			return Result{}, nil
		}
	}
	var logs []string
	podman := NewPodman(WithRunner(runner), WithEvidenceDir(dir), WithLogger(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	digest, err := podman.EnsureImage(context.Background(), ImagePostgres)
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if digest != "sha256:deadbeef" {
		t.Fatalf("digest = %q", digest)
	}
	data, err := os.ReadFile(filepath.Join(dir, "images.jsonl"))
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	var record ImageRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &record); err != nil {
		t.Fatalf("decode evidence %q: %v", data, err)
	}
	if record.Tag != ImagePostgres || record.Digest != "sha256:deadbeef" || record.ImageID != "image-id" {
		t.Fatalf("evidence record = %+v", record)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "sha256:deadbeef") {
		t.Fatalf("digest was not logged: %v", logs)
	}

	// A second EnsureImage must not pull again.
	before := len(runner.snapshot())
	if _, err := podman.EnsureImage(context.Background(), ImagePostgres); err != nil {
		t.Fatalf("second EnsureImage: %v", err)
	}
	for _, call := range runner.snapshot()[before:] {
		if len(call.Args) > 0 && call.Args[0] == "pull" {
			t.Fatal("EnsureImage pulled an image that was already present")
		}
	}
}

func TestPullFailureMapsToErrImageUnavailable(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		if len(cmd.Args) >= 1 && cmd.Args[0] == "pull" {
			return Result{Stderr: []byte("manifest unknown"), ExitCode: 125}, errors.New("exit status 125")
		}
		return Result{Stderr: []byte("no such image"), ExitCode: 125}, errors.New("exit status 125")
	}
	podman := NewPodman(WithRunner(runner))
	_, err := podman.EnsureImage(context.Background(), ImageNATS)
	if !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("error = %v, want ErrImageUnavailable", err)
	}
}

func TestInspectFailureIsNotImageUnavailable(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		if len(cmd.Args) >= 1 && cmd.Args[0] == "pull" {
			return Result{}, nil
		}
		return Result{Stdout: []byte("garbage")}, nil
	}
	podman := NewPodman(WithRunner(runner))
	_, err := podman.EnsureImage(context.Background(), ImageRegistry)
	if err == nil {
		t.Fatal("expected an error for an invalid digest")
	}
	if errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("inspect failure was misclassified as ErrImageUnavailable: %v", err)
	}
}

// imageInspectCount counts image inspections whose format matches fragment.
func imageInspectCount(calls []Command, fragment string) int {
	count := 0
	for _, call := range calls {
		args := call.Args
		if len(args) >= 4 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], fragment) {
			count++
		}
	}
	return count
}

// findRunCommand returns the single podman run invocation.
func findRunCommand(t *testing.T, calls []Command) Command {
	t.Helper()
	for _, call := range calls {
		if len(call.Args) > 0 && call.Args[0] == "run" {
			return call
		}
	}
	t.Fatalf("no container run command in %v", calls)
	return Command{}
}

// launchedImage returns the image argument of a run command built without a
// trailing container command.
func launchedImage(t *testing.T, call Command) string {
	t.Helper()
	if len(call.Args) < 2 {
		t.Fatalf("run command has no image argument: %v", call.Args)
	}
	return call.Args[len(call.Args)-1]
}

// TestShardRunUsesResolvedImageIdentity proves the shard launches the exact
// identity it inspected and recorded. The fake runner retargets the mutable
// tag on any tag-based launch, so forwarding the tag would start a different
// image than the recorded digest claims.
func TestShardRunUsesResolvedImageIdentity(t *testing.T) {
	runner := &fakeRunner{}
	var mu sync.Mutex
	current := "sha256:first"
	started := []string{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		args := cmd.Args
		switch {
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Digest"):
			digest := current
			// Another pull retargets the shared mutable tag immediately after
			// the shard's inspection.
			current = "sha256:retargeted"
			return Result{Stdout: []byte(digest + "\n")}, nil
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Id"):
			return Result{Stdout: []byte("sha256:first-id\n")}, nil
		case len(args) > 0 && args[0] == "run":
			started = append(started, args[len(args)-1])
			return Result{Stdout: []byte("container-id\n")}, nil
		default:
			return Result{}, nil
		}
	}
	dir := t.TempDir()
	podman := NewPodman(WithRunner(runner), WithEvidenceDir(dir))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))

	digest := shard.RequireImage(shard.Context(), ImagePostgres)
	if digest != "sha256:first" {
		t.Fatalf("RequireImage digest = %q, want sha256:first", digest)
	}
	if _, err := shard.Run(shard.Context(), RunSpec{Image: ImagePostgres}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	image := launchedImage(t, findRunCommand(t, runner.snapshot()))
	if want := "docker.io/library/postgres@sha256:first"; image != want {
		t.Fatalf("launched image = %q, want the resolved immutable identity %q", image, want)
	}
	mu.Lock()
	retargeted := current
	starts := append([]string(nil), started...)
	mu.Unlock()
	if retargeted != "sha256:retargeted" {
		t.Fatalf("fixture did not retarget the tag after inspection: %q", retargeted)
	}
	if len(starts) != 1 || starts[0] != image {
		t.Fatalf("started images = %v, want exactly %q", starts, image)
	}
	if inspections := imageInspectCount(runner.snapshot(), ".Digest"); inspections != 1 {
		t.Fatalf("digest inspections = %d, want one resolved selection", inspections)
	}

	// The recorded evidence and the launched identity are the same image.
	data, err := os.ReadFile(filepath.Join(dir, "images.jsonl"))
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("evidence records = %d, want 1:\n%s", len(lines), data)
	}
	var record ImageRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode evidence %q: %v", lines[0], err)
	}
	if record.Digest != "sha256:first" {
		t.Fatalf("recorded digest = %q, want the launched digest", record.Digest)
	}
	if !strings.HasSuffix(image, "@"+record.Digest) {
		t.Fatalf("launched %q does not pin the recorded digest %q", image, record.Digest)
	}
}

// TestRequireImageSelectsIdentityOnce proves repeated selections of one tag
// reuse the first inspection; a second inspection could observe a retargeted
// tag and make the evidence ambiguous.
func TestRequireImageSelectsIdentityOnce(t *testing.T) {
	runner := &fakeRunner{}
	inspections := 0
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		args := cmd.Args
		switch {
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Digest"):
			inspections++
			return Result{Stdout: []byte("sha256:selected-" + strconv.Itoa(inspections) + "\n")}, nil
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Id"):
			return Result{Stdout: []byte("image-id\n")}, nil
		default:
			return Result{}, nil
		}
	}
	podman := NewPodman(WithRunner(runner))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))

	first := shard.RequireImage(shard.Context(), ImageNATS)
	second := shard.RequireImage(shard.Context(), ImageNATS)
	if first != "sha256:selected-1" {
		t.Fatalf("first digest = %q, want sha256:selected-1", first)
	}
	if second != first {
		t.Fatalf("second RequireImage = %q, want retained selection %q", second, first)
	}
	if inspections != 1 {
		t.Fatalf("digest inspections = %d, want 1", inspections)
	}
	if _, err := shard.Run(shard.Context(), RunSpec{Image: ImageNATS}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	image := launchedImage(t, findRunCommand(t, runner.snapshot()))
	if want := "docker.io/library/nats@sha256:selected-1"; image != want {
		t.Fatalf("launched image = %q, want %q", image, want)
	}
}

// TestRequireImageResolvesIdentityOnceUnderConcurrency proves concurrent
// selections share one inspection instead of racing to retarget the tag.
func TestRequireImageResolvesIdentityOnceUnderConcurrency(t *testing.T) {
	runner := &fakeRunner{}
	var inspections atomic.Int64
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		args := cmd.Args
		switch {
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Digest"):
			count := inspections.Add(1)
			return Result{Stdout: []byte("sha256:concurrent-" + strconv.FormatInt(count, 10) + "\n")}, nil
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Id"):
			return Result{Stdout: []byte("image-id\n")}, nil
		default:
			return Result{}, nil
		}
	}
	podman := NewPodman(WithRunner(runner))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))

	digests := make([]string, 8)
	var wg sync.WaitGroup
	for i := range digests {
		wg.Go(func() {
			digests[i] = shard.RequireImage(shard.Context(), ImageRegistry)
		})
	}
	wg.Wait()
	for i, digest := range digests {
		if digest != "sha256:concurrent-1" {
			t.Fatalf("goroutine %d digest = %q, want the single retained selection", i, digest)
		}
	}
	if count := inspections.Load(); count != 1 {
		t.Fatalf("digest inspections = %d, want 1", count)
	}
}

// TestShardRunFailsWhenImageIsAbsent proves an absent pinned image is a loud
// error, never a fallback launch by the mutable tag.
func TestShardRunFailsWhenImageIsAbsent(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		if len(cmd.Args) > 0 && cmd.Args[0] == "version" {
			return Result{Stdout: []byte("5.0.0\n")}, nil
		}
		if len(cmd.Args) > 0 && cmd.Args[0] == "pull" {
			return Result{Stderr: []byte("manifest unknown"), ExitCode: 125}, errors.New("exit status 125")
		}
		return Result{Stderr: []byte("no such image"), ExitCode: 125}, errors.New("exit status 125")
	}
	podman := NewPodman(WithRunner(runner))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))

	_, err := shard.Run(shard.Context(), RunSpec{Image: ImagePostgres})
	if !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("Run error = %v, want ErrImageUnavailable", err)
	}
	for _, call := range runner.snapshot() {
		if len(call.Args) > 0 && call.Args[0] == "run" {
			t.Fatalf("Run launched a container without a resolved identity: %v", call.Args)
		}
	}
}

// TestShardRunPassesThroughImmutableReference proves an already digest- or
// ID-pinned reference is launched verbatim without a fresh selection.
func TestShardRunPassesThroughImmutableReference(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		return Result{Stdout: []byte("container-id\n")}, nil
	}
	podman := NewPodman(WithRunner(runner))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))

	for _, pinned := range []string{
		"docker.io/library/alpine@sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
	} {
		if _, err := shard.Run(shard.Context(), RunSpec{Image: pinned}); err != nil {
			t.Fatalf("Run %q: %v", pinned, err)
		}
	}
	calls := runner.snapshot()
	if inspections := imageInspectCount(calls, ".Digest"); inspections != 0 {
		t.Fatalf("pinned references were re-inspected %d times", inspections)
	}
	var launched []string
	for _, call := range calls {
		if len(call.Args) > 0 && call.Args[0] == "run" {
			launched = append(launched, launchedImage(t, call))
		}
	}
	want := []string{
		"docker.io/library/alpine@sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
	}
	if !reflect.DeepEqual(launched, want) {
		t.Fatalf("launched = %v, want %v", launched, want)
	}
}

func TestWaitReadySucceedsImmediately(t *testing.T) {
	clock := newFakeClock()
	calls := 0
	err := WaitReady(context.Background(), clock, "probe", func(context.Context) error {
		calls++
		return nil
	}, DefaultWaitOptions())
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if calls != 1 || clock.sleepCount() != 0 {
		t.Fatalf("calls=%d sleeps=%d", calls, clock.sleepCount())
	}
}

func TestWaitReadyUsesBackoffUntilReady(t *testing.T) {
	clock := newFakeClock()
	calls := 0
	err := WaitReady(context.Background(), clock, "probe", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	}, WaitOptions{Timeout: 10 * time.Second, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	clock.mu.Lock()
	sleeps := append([]time.Duration(nil), clock.sleeps...)
	clock.mu.Unlock()
	if !reflect.DeepEqual(sleeps, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}) {
		t.Fatalf("sleeps = %v, want exponential backoff", sleeps)
	}
}

func TestWaitReadyTimesOutWithoutSleepingPastDeadline(t *testing.T) {
	clock := newFakeClock()
	err := WaitReady(context.Background(), clock, "never", func(context.Context) error {
		return errors.New("still not ready")
	}, WaitOptions{Timeout: time.Second, InitialBackoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond})
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("error = %v, want ErrProbeTimeout", err)
	}
	if clock.sleepCount() > 10 {
		t.Fatalf("slept %d times; barrier must be bounded", clock.sleepCount())
	}
	clock.mu.Lock()
	elapsed := clock.now.Sub(time.Unix(1_700_000_000, 0))
	clock.mu.Unlock()
	if elapsed > 2*time.Second {
		t.Fatalf("barrier advanced %s past its deadline", elapsed)
	}
}

func TestWaitReadyHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probed := false
	err := WaitReady(ctx, newFakeClock(), "cancelled", func(context.Context) error {
		probed = true
		return nil
	}, DefaultWaitOptions())
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("error = %v, want ErrProbeTimeout", err)
	}
	if probed {
		t.Fatal("probe ran after context cancellation")
	}
}

func TestPostgresProbeHandshakeOverLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 512)
		_, _ = conn.Read(buffer)
		// AuthenticationOk: type R, length 8, status 0.
		_, _ = conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0})
	}()
	probe := PostgresProbe(listener.Addr().String(), "postgres")
	if err := probe(context.Background()); err != nil {
		t.Fatalf("PostgresProbe: %v", err)
	}
	<-done
}

func TestPostgresProbeRefusesWhenNoServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	probe := PostgresProbe(addr, "postgres")
	if err := probe(context.Background()); err == nil {
		t.Fatal("expected connection refusal")
	}
}

// postgresBackendMessage frames one PostgreSQL backend protocol message.
func postgresBackendMessage(kind byte, body []byte) []byte {
	message := make([]byte, 5+len(body))
	message[0] = kind
	binary.BigEndian.PutUint32(message[1:], uint32(4+len(body)))
	copy(message[5:], body)
	return message
}

// postgresErrorResponse builds an ErrorResponse whose fields are the given
// type/value pairs, NUL-terminated and closed by the final terminator byte.
func postgresErrorResponse(fields ...[2]string) []byte {
	var body []byte
	for _, field := range fields {
		body = append(body, field[0][0])
		body = append(body, field[1]...)
		body = append(body, 0)
	}
	body = append(body, 0)
	return postgresBackendMessage('E', body)
}

func postgresAuthenticationOK() []byte {
	return postgresBackendMessage('R', []byte{0, 0, 0, 0})
}

// startFakePostgres accepts loopback connections and answers the startup
// message of each with reply(conn, attempt), counting attempts from one.
func startFakePostgres(t *testing.T, reply func(conn net.Conn, attempt int)) (string, func() int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	attempts := 0
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			attempts++
			attempt := attempts
			mu.Unlock()
			go func(conn net.Conn, attempt int) {
				defer conn.Close()
				buffer := make([]byte, 512)
				_, _ = conn.Read(buffer)
				reply(conn, attempt)
			}(conn, attempt)
		}
	}()
	return listener.Addr().String(), func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}
}

// TestPostgresProbeKeepsRetryingWhileServerIsStarting proves that a server
// answering 57P03 is not a false ready barrier: the probe errors and WaitReady
// retries until the server completes startup.
func TestPostgresProbeKeepsRetryingWhileServerIsStarting(t *testing.T) {
	addr, attempts := startFakePostgres(t, func(conn net.Conn, attempt int) {
		if attempt < 3 {
			_, _ = conn.Write(postgresErrorResponse(
				[2]string{"S", "FATAL"},
				[2]string{"C", "57P03"},
				[2]string{"M", "the database system is starting up"},
			))
			return
		}
		_, _ = conn.Write(postgresAuthenticationOK())
	})
	probe := PostgresProbe(addr, "postgres")
	if err := probe(context.Background()); err == nil {
		t.Fatal("PostgresProbe accepted SQLSTATE 57P03 as ready")
	}
	if err := WaitReady(context.Background(), newFakeClock(), "postgres", probe, WaitOptions{
		Timeout:        5 * time.Second,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got := attempts(); got < 3 {
		t.Fatalf("probe attempts = %d, want at least 3", got)
	}
}

// TestPostgresProbeAcceptsCompletedStartup proves that Authentication messages
// and startup-stage rejections prove the server finished starting.
func TestPostgresProbeAcceptsCompletedStartup(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"authentication ok", postgresAuthenticationOK()},
		{"invalid password", postgresErrorResponse([2]string{"C", "28P01"})},
		{"invalid authorization", postgresErrorResponse([2]string{"C", "28000"})},
		{"unknown database", postgresErrorResponse([2]string{"C", "3D000"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := startFakePostgres(t, func(conn net.Conn, _ int) {
				_, _ = conn.Write(tc.body)
			})
			if err := PostgresProbe(addr, "postgres")(context.Background()); err != nil {
				t.Fatalf("PostgresProbe: %v", err)
			}
		})
	}
}

// TestPostgresProbeRejectsNotReadyReplies proves the not-ready SQLSTATE family
// and unknown or missing SQLSTATE never pass the barrier.
func TestPostgresProbeRejectsNotReadyReplies(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"starting up", postgresErrorResponse([2]string{"C", "57P03"})},
		{"admin shutdown", postgresErrorResponse([2]string{"C", "57P01"})},
		{"crash shutdown", postgresErrorResponse([2]string{"C", "57P02"})},
		{"too many connections", postgresErrorResponse([2]string{"C", "53300"})},
		{"unknown sqlstate", postgresErrorResponse([2]string{"C", "XX000"})},
		{"missing sqlstate", postgresErrorResponse([2]string{"S", "FATAL"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := startFakePostgres(t, func(conn net.Conn, _ int) {
				_, _ = conn.Write(tc.body)
			})
			if err := PostgresProbe(addr, "postgres")(context.Background()); err == nil {
				t.Fatal("PostgresProbe accepted a not-ready reply")
			}
		})
	}
}

// TestPostgresProbeRejectsUnexpectedReplyType proves a garbage first byte is
// not mistaken for readiness.
func TestPostgresProbeRejectsUnexpectedReplyType(t *testing.T) {
	addr, _ := startFakePostgres(t, func(conn net.Conn, _ int) {
		_, _ = conn.Write(postgresBackendMessage('Q', []byte{0, 0, 0, 0}))
	})
	if err := PostgresProbe(addr, "postgres")(context.Background()); err == nil {
		t.Fatal("PostgresProbe accepted an unexpected message type")
	}
}

// TestPostgresProbeRejectsOversizedReplyWithoutHanging proves a claimed body
// length above the bound is rejected before any body is read.
func TestPostgresProbeRejectsOversizedReplyWithoutHanging(t *testing.T) {
	addr, _ := startFakePostgres(t, func(conn net.Conn, _ int) {
		header := []byte{'E', 0, 0, 0, 0}
		// 1 MiB is far beyond any sane startup message and must be rejected
		// from the declared length alone.
		binary.BigEndian.PutUint32(header[1:], 1<<20)
		_, _ = conn.Write(header)
		// Hold the connection open: a probe that reads the declared body would
		// block until its deadline instead of rejecting the length.
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := PostgresProbe(addr, "postgres")(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("PostgresProbe accepted an oversized reply")
	}
	if elapsed > time.Second {
		t.Fatalf("oversized reply took %s; the length must be rejected without reading the body", elapsed)
	}
}

// TestPostgresProbeRejectsTruncatedReply proves a short body fails cleanly.
func TestPostgresProbeRejectsTruncatedReply(t *testing.T) {
	addr, _ := startFakePostgres(t, func(conn net.Conn, _ int) {
		header := []byte{'E', 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[1:], 40)
		_, _ = conn.Write(header)
		_, _ = conn.Write([]byte{'C', '2'})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := PostgresProbe(addr, "postgres")(ctx); err == nil {
		t.Fatal("PostgresProbe accepted a truncated reply")
	}
}

func TestHTTPProbes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v2/":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	if err := NATSProbe(server.URL)(context.Background()); err != nil {
		t.Fatalf("NATSProbe: %v", err)
	}
	if err := RegistryProbe(server.URL)(context.Background()); err != nil {
		t.Fatalf("RegistryProbe: %v", err)
	}
	if err := HTTPProbe(server.URL+"/bad", 200)(context.Background()); err == nil {
		t.Fatal("expected status mismatch")
	}
}

func TestContainerHostPortLookupAndCache(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		return Result{Stdout: []byte("127.0.0.1:49153\n")}, nil
	}
	podman := NewPodman(WithRunner(runner))
	container := &Container{Name: "c", podman: podman, ports: map[int]int{}}
	port, err := container.HostPort(context.Background(), 5432)
	if err != nil {
		t.Fatalf("HostPort: %v", err)
	}
	if port != 49153 {
		t.Fatalf("port = %d", port)
	}
	if _, err := container.HostPort(context.Background(), 5432); err != nil {
		t.Fatalf("cached HostPort: %v", err)
	}
	if calls := len(runner.snapshot()); calls != 1 {
		t.Fatalf("port lookups = %d, want 1 cached lookup", calls)
	}
	addr, err := container.Addr(context.Background(), 5432)
	if err != nil || addr != "127.0.0.1:49153" {
		t.Fatalf("Addr = %q, %v", addr, err)
	}
}

func TestStopIsBounded(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	podman := NewPodman(WithRunner(runner), WithStopTimeout(50*time.Millisecond))
	container := &Container{Name: "c", podman: podman, ports: map[int]int{}}
	start := time.Now()
	err := container.Stop(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s, want bounded", elapsed)
	}
}

func TestShardCleanupOrderAndLabels(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		args := cmd.Args
		switch {
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Digest"):
			return Result{Stdout: []byte("sha256:cleanup\n")}, nil
		case len(args) >= 5 && args[0] == "image" && args[1] == "inspect" && strings.Contains(args[3], ".Id"):
			return Result{Stdout: []byte("cleanup-id\n")}, nil
		case len(args) > 0 && args[0] == "run":
			return Result{Stdout: []byte("container-id\n")}, nil
		default:
			return Result{}, nil
		}
	}
	podman := NewPodman(WithRunner(runner))
	shard := NewShard(t, WithPodman(podman), WithShardTimeout(time.Minute))
	ctx := shard.Context()
	network, err := shard.Network(ctx)
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	volume, err := shard.CreateVolume(ctx, shard.Name("data"))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	container, err := shard.Run(ctx, RunSpec{Image: ImagePostgres})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	shard.Close()

	calls := runner.snapshot()
	networkCreate := indexOfCall(calls, "network", "create")
	containerRun := indexOfCall(calls, "run")
	containerStop := indexOfCall(calls, "stop")
	containerRemove := indexOfCall(calls, "rm")
	volumeRemove := indexOfCall(calls, "volume", "rm")
	networkRemove := indexOfCall(calls, "network", "rm")
	for name, index := range map[string]int{
		"network create": networkCreate, "container run": containerRun,
		"container stop": containerStop, "container remove": containerRemove,
		"volume remove": volumeRemove, "network remove": networkRemove,
	} {
		if index < 0 {
			t.Fatalf("%s was not invoked: %v", name, calls)
		}
	}
	if !(networkCreate < containerRun && containerRun < containerStop && containerStop < containerRemove &&
		containerRemove < volumeRemove && volumeRemove < networkRemove) {
		t.Fatalf("cleanup order wrong: create=%d run=%d stop=%d rm=%d volume=%d network=%d",
			networkCreate, containerRun, containerStop, containerRemove, volumeRemove, networkRemove)
	}
	createArgs := calls[networkCreate].Args
	if !hasArgSequence(createArgs, "--label", "kodama.test=1") ||
		!hasArgSequence(createArgs, "--label", "kodama.shard="+shard.ID) ||
		!hasArgSequence(createArgs, "--label", "kodama.owner-pid="+strconv.Itoa(os.Getpid())) {
		t.Fatalf("network create missing labels: %v", createArgs)
	}
	if !strings.Contains(network, shard.ID) || !strings.Contains(volume, shard.ID) || !strings.Contains(container.Name, shard.ID) {
		t.Fatalf("resources are not shard scoped: %q %q %q", network, volume, container.Name)
	}
}

func TestCleanupOrphansRemovesOnlyDeadOwners(t *testing.T) {
	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, cmd Command) (Result, error) {
		args := cmd.Args
		switch {
		case len(args) >= 1 && args[0] == "ps":
			return Result{Stdout: []byte("dead-container\nlive-container\n")}, nil
		case len(args) >= 2 && args[0] == "volume" && args[1] == "ls":
			return Result{Stdout: []byte("dead-volume\nlive-volume\n")}, nil
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return Result{Stdout: []byte("dead-network\nlive-network\n")}, nil
		case len(args) >= 1 && args[0] == "inspect":
			return labelsResult(strings.HasPrefix(args[len(args)-1], "dead-"))
		case len(args) >= 2 && args[0] == "volume" && args[1] == "inspect":
			return labelsResult(strings.HasPrefix(args[len(args)-1], "dead-"))
		case len(args) >= 2 && args[0] == "network" && args[1] == "inspect":
			return labelsResult(strings.HasPrefix(args[len(args)-1], "dead-"))
		default:
			return Result{}, nil
		}
	}
	podman := NewPodman(WithRunner(runner), WithProcessAlive(func(pid int) bool { return pid == 1 }))
	if err := podman.CleanupOrphans(context.Background()); err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	calls := runner.snapshot()
	var removed []string
	for _, call := range calls {
		if len(call.Args) > 0 && (call.Args[0] == "rm" || call.Args[0] == "volume" || call.Args[0] == "network") {
			if hasArgSequence(call.Args, "--force") {
				removed = append(removed, call.Args[len(call.Args)-1])
			}
		}
	}
	want := []string{"dead-container", "dead-volume", "dead-network"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
}

func labelsResult(dead bool) (Result, error) {
	pid := "1"
	if dead {
		pid = "999999"
	}
	labels := map[string]string{LabelTest: "1", LabelOwnerPID: pid, LabelShard: "s"}
	data, _ := json.Marshal(labels)
	return Result{Stdout: append(data, '\n')}, nil
}

func indexOfCall(calls []Command, prefix ...string) int {
	for i, call := range calls {
		if len(call.Args) >= len(prefix) && reflect.DeepEqual(call.Args[:len(prefix)], prefix) {
			return i
		}
	}
	return -1
}

func TestHandleSignalRunsCleanup(t *testing.T) {
	called := false
	code := HandleSignal(syscall.SIGTERM, func(context.Context) error {
		called = true
		return nil
	})
	if !called || code != 1 {
		t.Fatalf("called=%v code=%d", called, code)
	}
	if code := HandleSignal(nil, func(context.Context) error {
		t.Fatal("cleanup ran for nil signal")
		return nil
	}); code != 0 {
		t.Fatalf("nil signal code = %d", code)
	}
}

func TestInfraUnavailablePolicy(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	run := func(require bool) (string, int) {
		requireValue := "0"
		if require {
			requireValue = "1"
		}
		cmd := exec.Command(exe, "-test.run=^TestSkipPolicyHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "HARNESS_SKIP_HELPER=1", "HARNESS_SKIP_REQUIRE="+requireValue)
		output, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exit.ExitCode()
		}
		return string(output), code
	}
	output, code := run(false)
	if code != 0 {
		t.Fatalf("skip helper exit = %d\n%s", code, output)
	}
	if !strings.Contains(output, "unqualified: infrastructure unavailable") {
		t.Fatalf("skip reason missing:\n%s", output)
	}
	output, code = run(true)
	if code == 0 {
		t.Fatalf("require helper passed unexpectedly:\n%s", output)
	}
	if !strings.Contains(output, "KODAMA_REQUIRE_INFRA=1") {
		t.Fatalf("failure reason missing:\n%s", output)
	}
}

// TestSkipPolicyHelper is only meaningful when invoked by
// TestInfraUnavailablePolicy in a child process.
func TestSkipPolicyHelper(t *testing.T) {
	if os.Getenv("HARNESS_SKIP_HELPER") != "1" {
		t.Skip("helper process only")
	}
	SkipOrFail(t, os.Getenv("HARNESS_SKIP_REQUIRE") == "1", errors.New("podman binary not found"))
}
