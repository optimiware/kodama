// Package recoverysupport starts the real PostgreSQL, NATS/JetStream and Podman
// boundaries used by the recovery component suite.
//
// It is test support code under test/, never product code. One shard owns one
// isolated PostgreSQL server, one JetStream server whose file store is bind
// mounted on the host, and the host directories a backup/restore set needs. The
// harness supplies labelled cleanup, the 1 CPU / 1 GiB bound, 90-second
// readiness barriers and the 15-minute shard timeout; this package never sleeps
// to wait for readiness.
package recoverysupport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	recovery "github.com/optimiware/kodama/internal/shell/app/drivers/recovery"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

const (
	// NATSClientPort and NATSMonitorPort are the published broker ports.
	NATSClientPort  = 4222
	NATSMonitorPort = 8222
	// StreamName and StreamSubjects are the declared event stream shape.
	StreamName     = "KODAMA_EVENTS"
	StreamSubjects = "kodama.v1.*.*.events"
	// DurableConsumer is the explicit consumer whose position the round trip
	// compares.
	DurableConsumer = "I02F_DURABLE"
	// ServerName is pinned so the restored file store belongs to the same
	// logical JetStream server.
	ServerName = "kodama-i02f"
	// WorkspaceID and WorkspaceSentinel identify the disposable workspace that
	// must never be captured.
	WorkspaceID       = "workspace-i02f"
	WorkspaceSentinel = "workspace-only.txt"
)

// Shard is one isolated recovery environment.
type Shard struct {
	Postgres *pgtest.Shard
	Harness  *harness.Shard
	NATS     *NATS

	Root            string
	RetainedLogsDir string
	KeysDir         string
	JetStreamDir    string
	WorkspaceRoot   string
	BackupRoot      string

	Executor recovery.Executor

	PostgresImageTag    string
	PostgresImageDigest string
	NATSImageTag        string
	NATSImageDigest     string

	service *runtimesupport.Service
	libpod  *podmandriver.Libpod
}

// Start starts a source shard with PostgreSQL, JetStream and the host
// directories. The JetStream server is left running; callers that back up the
// store must stop it first.
func Start(t testing.TB) *Shard {
	t.Helper()
	shard := StartTarget(t)
	shard.WriteWorkspaceSentinel(t)
	shard.StartNATS(t)
	return shard
}

// StartTarget starts PostgreSQL and the host directories without JetStream. It
// is the restore destination: the driver copies the backed-up store into
// JetStreamDir before the broker starts.
func StartTarget(t testing.TB) *Shard {
	t.Helper()
	postgresShard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(postgresShard).Apply(t.Context(), 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	root := t.TempDir()
	shard := &Shard{
		Postgres:            postgresShard,
		Harness:             postgresShard.Harness,
		Root:                root,
		RetainedLogsDir:     filepath.Join(root, "retained-logs"),
		KeysDir:             filepath.Join(root, "keys"),
		JetStreamDir:        filepath.Join(root, "jetstream"),
		WorkspaceRoot:       filepath.Join(root, "workspaces", WorkspaceID),
		BackupRoot:          filepath.Join(root, "backups"),
		Executor:            ContainerExecutor{Container: postgresShard.Container.Name, User: "postgres"},
		PostgresImageTag:    postgresShard.ImageTag,
		PostgresImageDigest: postgresShard.ImageDigest,
	}
	for _, dir := range []string{
		shard.RetainedLogsDir,
		shard.KeysDir,
		shard.JetStreamDir,
		shard.WorkspaceRoot,
		shard.BackupRoot,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	shard.RecordEvidence(t)
	return shard
}

// WriteWorkspaceSentinel creates the disposable workspace bytes that must never
// be captured by a backup or reappear after a restore.
func (s *Shard) WriteWorkspaceSentinel(t testing.TB) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.WorkspaceRoot, WorkspaceSentinel), []byte("disposable workspace bytes"), 0o644); err != nil {
		t.Fatalf("write workspace sentinel: %v", err)
	}
}

// StartNATS starts JetStream over the shard's store directory.
func (s *Shard) StartNATS(t testing.TB) {
	t.Helper()
	if s.NATS != nil {
		t.Fatal("recoverysupport: NATS is already started")
	}
	s.NATS = startNATS(t, s.Harness, s.JetStreamDir)
	s.NATSImageTag = harness.ImageNATS
	s.NATSImageDigest = s.NATS.Digest
	s.RecordEvidence(t)
}

// StopNATS stops the broker so the file store is quiescent for capture.
func (s *Shard) StopNATS(t testing.TB) {
	t.Helper()
	if s.NATS == nil {
		t.Fatal("recoverysupport: NATS is not started")
	}
	s.NATS.Stop(t)
}

// Libpod returns the real libpod API client over a private service socket. The
// service is started lazily because only the fence/reconcile scenarios need it.
func (s *Shard) Libpod(t testing.TB) *podmandriver.Libpod {
	t.Helper()
	if s.libpod != nil {
		return s.libpod
	}
	s.service = runtimesupport.StartService(t)
	client, err := podmandriver.NewLibpod(podmandriver.LibpodConfig{
		SocketPath: s.service.Socket(),
		Timeout:    30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewLibpod: %v", err)
	}
	s.libpod = client
	return client
}

// AlpineImage resolves the pinned workload image used by the Podman fence and
// reconcile scenarios.
func (s *Shard) AlpineImage(t testing.TB) runtimesupport.PinnedImage {
	t.Helper()
	return runtimesupport.ResolveImage(t.Context(), t, s.Harness.Podman, runtimesupport.ImageAlpineTag)
}

// RunWorkload starts one labelled workload container in the shard.
func (s *Shard) RunWorkload(t testing.TB, image runtimesupport.PinnedImage, name string, command ...string) *harness.Container {
	t.Helper()
	container, err := s.Harness.Run(t.Context(), harness.RunSpec{
		Image: image.Reference,
		Name:  name,
		Cmd:   command,
	})
	if err != nil {
		t.Fatalf("run workload %s: %v", name, err)
	}
	return container
}

// WaitContainerStopped is a bounded barrier on whole-container stop evidence.
func WaitContainerStopped(t testing.TB, libpod *podmandriver.Libpod, name string) {
	t.Helper()
	probe := func(ctx context.Context) error {
		state, err := libpod.InspectContainer(ctx, name)
		if err != nil {
			return err
		}
		if !state.WholeStopped || state.StartPending {
			return fmt.Errorf("container %s is not wholly stopped", name)
		}
		return nil
	}
	if err := harness.WaitReady(t.Context(), harness.RealClock{}, "container stop "+name, probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for %s to stop: %v", name, err)
	}
}

// NATS is one real JetStream broker whose file store is a host directory.
type NATS struct {
	Shard      *harness.Shard
	Container  *harness.Container
	URL        string
	MonitorURL string
	StoreDir   string
	Digest     string
	Version    string
}

func startNATS(t testing.TB, shard *harness.Shard, storeDir string) *NATS {
	t.Helper()
	ctx := shard.Context()
	digest := shard.RequireImage(ctx, harness.ImageNATS)
	container, err := shard.Run(ctx, harness.RunSpec{
		Image:   harness.ImageNATS,
		Ports:   []harness.Port{{Container: NATSClientPort}, {Container: NATSMonitorPort}},
		Volumes: []harness.Volume{{Target: storeDir + ":/data:Z"}},
		Cmd:     []string{"-js", "-sd", "/data", "-m", fmt.Sprintf("%d", NATSMonitorPort), "-n", ServerName},
	})
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	clientAddr, err := container.Addr(ctx, NATSClientPort)
	if err != nil {
		t.Fatalf("nats client address: %v", err)
	}
	monitorAddr, err := container.Addr(ctx, NATSMonitorPort)
	if err != nil {
		t.Fatalf("nats monitor address: %v", err)
	}
	broker := &NATS{
		Shard:      shard,
		Container:  container,
		URL:        "nats://" + clientAddr,
		MonitorURL: "http://" + monitorAddr,
		StoreDir:   storeDir,
		Digest:     digest,
	}
	broker.WaitReady(t)
	broker.Version = broker.ServerVersion(t)
	return broker
}

// WaitReady waits for the monitoring health endpoint.
func (n *NATS) WaitReady(t testing.TB) {
	t.Helper()
	if err := n.Shard.WaitReady(n.Shard.Context(), "nats", harness.NATSProbe(n.MonitorURL)); err != nil {
		if logs, logErr := n.Container.Logs(n.Shard.Context()); logErr == nil {
			t.Logf("nats logs:\n%s", logs)
		}
		t.Fatalf("nats readiness: %v", err)
	}
}

// ServerVersion reads the running server version from /varz.
func (n *NATS) ServerVersion(t testing.TB) string {
	t.Helper()
	var document struct {
		Version string `json:"version"`
	}
	if err := getJSON(n.Shard.Context(), n.MonitorURL+"/varz", &document); err != nil {
		t.Fatalf("read nats version: %v", err)
	}
	return document.Version
}

// Stop stops the broker container.
func (n *NATS) Stop(t testing.TB) {
	t.Helper()
	if err := n.Container.Stop(n.Shard.Context()); err != nil {
		t.Fatalf("stop nats: %v", err)
	}
}

// Raw opens one plain nats.go connection used only to inspect or seed the
// broker.
func (n *NATS) Raw(t testing.TB) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(n.URL, natsgo.Name("i02f-raw-"+t.Name()))
	if err != nil {
		t.Fatalf("raw nats connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// JetStream returns a JetStream context over a raw connection.
func (n *NATS) JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(n.Raw(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// EnsureStreams creates the declared event stream.
func (n *NATS) EnsureStreams(t testing.TB) {
	t.Helper()
	if _, err := n.JetStream(t).CreateOrUpdateStream(n.Shard.Context(), jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{StreamSubjects},
		Storage:  jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create stream %s: %v", StreamName, err)
	}
}

// PublishEvents publishes count distinct events with stable message IDs.
func (n *NATS) PublishEvents(t testing.TB, count int) {
	t.Helper()
	js := n.JetStream(t)
	ctx := n.Shard.Context()
	for index := 1; index <= count; index++ {
		subject := "kodama.v1.tenant-1.project-1.events"
		payload := []byte(fmt.Sprintf(`{"id":"event-%d","type":"test.event"}`, index))
		if _, err := js.Publish(ctx, subject, payload, jetstream.WithMsgID(fmt.Sprintf("event-%d", index))); err != nil {
			t.Fatalf("publish event %d: %v", index, err)
		}
	}
	n.WaitForStreamMessages(t, uint64(count))
}

// AckFirst creates the durable consumer, delivers and acknowledges the first
// count events, then waits until the durable ack floor reflects them.
func (n *NATS) AckFirst(t testing.TB, count int) {
	t.Helper()
	js := n.JetStream(t)
	ctx := n.Shard.Context()
	consumer, err := js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:   DurableConsumer,
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create durable consumer: %v", err)
	}
	batch, err := consumer.Fetch(count, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch events: %v", err)
	}
	acked := 0
	for message := range batch.Messages() {
		if err := message.Ack(); err != nil {
			t.Fatalf("ack event: %v", err)
		}
		acked++
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("fetch events: %v", err)
	}
	if acked != count {
		t.Fatalf("acked %d events; want %d", acked, count)
	}
	n.WaitForAckFloor(t, uint64(count))
}

// StreamMessages reads the current stream message count.
func (n *NATS) StreamMessages(t testing.TB) uint64 {
	t.Helper()
	count, err := n.streamMessages()
	if err != nil {
		t.Fatalf("stream %s: %v", StreamName, err)
	}
	return count
}

func (n *NATS) streamMessages() (uint64, error) {
	js, closeConn, err := n.probeJetStream()
	if err != nil {
		return 0, err
	}
	defer closeConn()
	stream, err := js.Stream(n.Shard.Context(), StreamName)
	if err != nil {
		return 0, err
	}
	info, err := stream.Info(n.Shard.Context())
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

// AckFloor reads the durable consumer's acknowledged stream sequence.
func (n *NATS) AckFloor(t testing.TB) uint64 {
	t.Helper()
	floor, err := n.ackFloor()
	if err != nil {
		t.Fatalf("consumer %s: %v", DurableConsumer, err)
	}
	return floor
}

func (n *NATS) ackFloor() (uint64, error) {
	js, closeConn, err := n.probeJetStream()
	if err != nil {
		return 0, err
	}
	defer closeConn()
	stream, err := js.Stream(n.Shard.Context(), StreamName)
	if err != nil {
		return 0, err
	}
	consumer, err := stream.Consumer(n.Shard.Context(), DurableConsumer)
	if err != nil {
		return 0, err
	}
	info, err := consumer.Info(n.Shard.Context())
	if err != nil {
		return 0, err
	}
	return info.AckFloor.Stream, nil
}

// probeJetStream opens a short-lived connection for a bounded barrier probe so
// a transient broker error is returned to the barrier instead of failing the
// test through t.Fatalf.
func (n *NATS) probeJetStream() (jetstream.JetStream, func(), error) {
	conn, err := natsgo.Connect(n.URL, natsgo.Name("i02f-probe"))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return js, conn.Close, nil
}

// WaitForStreamMessages is a bounded barrier on the stored message count.
func (n *NATS) WaitForStreamMessages(t testing.TB, want uint64) {
	t.Helper()
	probe := func(context.Context) error {
		got, err := n.streamMessages()
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("stream has %d messages; want %d", got, want)
		}
		return nil
	}
	if err := harness.WaitReady(n.Shard.Context(), harness.RealClock{}, "stream messages", probe, fastWaitOptions()); err != nil {
		t.Fatalf("stream message barrier: %v", err)
	}
}

// WaitForAckFloor is a bounded barrier on the durable consumer position.
func (n *NATS) WaitForAckFloor(t testing.TB, want uint64) {
	t.Helper()
	probe := func(context.Context) error {
		got, err := n.ackFloor()
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("consumer ack floor = %d; want %d", got, want)
		}
		return nil
	}
	if err := harness.WaitReady(n.Shard.Context(), harness.RealClock{}, "consumer ack floor", probe, fastWaitOptions()); err != nil {
		t.Fatalf("consumer ack floor barrier: %v", err)
	}
}

// ContainerExecutor runs one command inside the PostgreSQL container with
// `podman exec -i`, so pg_dump/pg_restore use the server image's own binaries
// and never a host-installed client. User names the container OS user the
// command runs as: the official image starts the server as `postgres`, and
// libpq defaults to the OS user, so exec'ing as root would connect as a
// nonexistent `root` role.
type ContainerExecutor struct {
	Container string
	User      string
}

// Exec implements recovery.Executor.
func (e ContainerExecutor) Exec(ctx context.Context, request recovery.ExecRequest) (recovery.ExecResult, error) {
	container := request.Container
	if container == "" {
		container = e.Container
	}
	args := []string{"exec"}
	if e.User != "" {
		args = append(args, "--user", e.User)
	}
	args = append(args, "-i", container)
	args = append(args, request.Argv...)
	command := exec.CommandContext(ctx, "podman", args...)
	command.Env = harness.BaseEnv()
	command.Stdin = bytes.NewReader(request.Stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := recovery.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			result.ExitCode = exit.ExitCode()
		} else {
			result.ExitCode = -1
		}
		return result, fmt.Errorf("recoverysupport: %s %s failed: %w: %s",
			"podman", strings.Join(args[:min(3, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return result, nil
}

// ContainerObserver observes explicit workload container identities through
// the real libpod seam. It never starts, stops or lists anything.
type ContainerObserver struct {
	Libpod *podmandriver.Libpod
	Names  map[string]string
}

// Observe implements recovery.Observer.
func (o *ContainerObserver) Observe(ctx context.Context, identity recovery.WorkloadIdentity) (recovery.WorkloadObservation, error) {
	name, ok := o.Names[identity.ID]
	if !ok {
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationAbsent}, nil
	}
	state, err := o.Libpod.InspectContainer(ctx, name)
	if errors.Is(err, runtimepodman.ErrNotFound) {
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationAbsent}, nil
	}
	if err != nil {
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationUnknown, Detail: "inspect failed"}, nil
	}
	switch {
	case state.WholeStopped && !state.StartPending:
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationStopped}, nil
	case state.StartPending:
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationUnknown, Detail: "start pending"}, nil
	case state.Status == runtimepodman.ContainerStatusCreated:
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationStopped}, nil
	case state.Status == runtimepodman.ContainerStatusRunning || (state.Started && !state.WholeStopped):
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationRunning}, nil
	default:
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationUnknown, Detail: "unclassified state"}, nil
	}
}

// ScriptedObserver overrides selected identities as unknown so a scenario can
// prove that uncertainty blocks dispatch while the other identities are still
// observed through the real Podman seam.
type ScriptedObserver struct {
	base recovery.Observer

	mu      sync.Mutex
	unknown map[string]bool
}

// NewScriptedObserver wraps a base observer.
func NewScriptedObserver(base recovery.Observer) *ScriptedObserver {
	return &ScriptedObserver{base: base, unknown: map[string]bool{}}
}

// SetUnknown marks one workload identity as unobservable.
func (o *ScriptedObserver) SetUnknown(id string, unknown bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unknown[id] = unknown
}

// Observe implements recovery.Observer.
func (o *ScriptedObserver) Observe(ctx context.Context, identity recovery.WorkloadIdentity) (recovery.WorkloadObservation, error) {
	o.mu.Lock()
	unknown := o.unknown[identity.ID]
	o.mu.Unlock()
	if unknown {
		return recovery.WorkloadObservation{Identity: identity, State: recovery.ObservationUnknown, Detail: "scripted uncertainty"}, nil
	}
	return o.base.Observe(ctx, identity)
}

// RecordEvidence logs the pinned image tags and digests that served the shard.
func (s *Shard) RecordEvidence(t testing.TB) {
	t.Helper()
	t.Logf("recovery evidence: postgres %s %s", s.PostgresImageTag, s.PostgresImageDigest)
	if s.NATSImageTag != "" {
		t.Logf("recovery evidence: nats %s %s", s.NATSImageTag, s.NATSImageDigest)
	}
}

func getJSON(ctx context.Context, rawURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d", rawURL, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func fastWaitOptions() harness.WaitOptions {
	return harness.WaitOptions{
		Timeout:        15 * time.Second,
		AttemptTimeout: 2 * time.Second,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     250 * time.Millisecond,
	}
}
