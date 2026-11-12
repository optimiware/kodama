package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// Published container ports and the stream names this suite creates. The
// stream filters use the broker's own wildcard capture; the adapter itself
// never publishes or subscribes to a wildcard subject.
const (
	NATSClientPort  = 4222
	NATSMonitorPort = 8222

	EventsStream        = "KODAMA_EVENTS"
	InvalidationsStream = "KODAMA_INVALIDATIONS"

	EventsStreamSubjects        = "kodama.v1.*.*.events"
	InvalidationsStreamSubjects = "kodama.v1.*.*.invalidations"
)

// NATS is one real JetStream-enabled broker started through the infrastructure
// harness.
type NATS struct {
	Shard      *harness.Shard
	Container  *harness.Container
	URL        string
	MonitorURL string
	Digest     string
	Version    string
}

// StartNATS starts the pinned NATS image with JetStream and the monitoring
// endpoint, waits for the readiness barrier and records image evidence.
func StartNATS(t *testing.T) *NATS {
	t.Helper()
	shard := harness.NewShard(t)
	ctx := shard.Context()
	digest := shard.RequireImage(ctx, harness.ImageNATS)
	container, err := shard.Run(ctx, harness.RunSpec{
		Image: harness.ImageNATS,
		Ports: []harness.Port{{Container: NATSClientPort}, {Container: NATSMonitorPort}},
		Cmd:   []string{"-js", "-m", fmt.Sprintf("%d", NATSMonitorPort)},
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
		Digest:     digest,
	}
	broker.WaitReady(t)
	broker.Version = broker.ServerVersion(t)
	RecordImage(harness.ImageNATS, digest, "nats-server "+broker.Version)
	return broker
}

// WaitReady waits for the monitoring health endpoint.
func (broker *NATS) WaitReady(t *testing.T) {
	t.Helper()
	if err := broker.Shard.WaitReady(broker.Shard.Context(), "nats", harness.NATSProbe(broker.MonitorURL)); err != nil {
		if logs, logErr := broker.Container.Logs(broker.Shard.Context()); logErr == nil {
			t.Logf("nats logs:\n%s", logs)
		}
		t.Fatalf("nats readiness: %v", err)
	}
}

// WaitDown waits until the broker stops accepting client connections.
func (broker *NATS) WaitDown(t *testing.T) {
	t.Helper()
	addr := strings.TrimPrefix(broker.URL, "nats://")
	probe := func(ctx context.Context) error {
		dialer := net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		return errors.New("nats is still accepting connections")
	}
	if err := harness.WaitReady(broker.Shard.Context(), harness.RealClock{}, "nats down", probe, fastWaitOptions()); err != nil {
		t.Fatalf("nats did not stop: %v", err)
	}
}

// Stop stops the broker container.
func (broker *NATS) Stop(t *testing.T) {
	t.Helper()
	if err := broker.Container.Stop(broker.Shard.Context()); err != nil {
		t.Fatalf("stop nats: %v", err)
	}
	broker.WaitDown(t)
}

// Start starts the stopped broker container through the harness runner (the
// harness owns no start helper) and waits for readiness again.
func (broker *NATS) Start(t *testing.T) {
	t.Helper()
	_, err := harness.ExecRunner{}.Run(broker.Shard.Context(), harness.Command{
		Name: "podman",
		Args: []string{"start", broker.Container.Name},
		Env:  harness.BaseEnv(),
	})
	if err != nil {
		t.Fatalf("start nats container: %v", err)
	}
	broker.WaitReady(t)
}

// Restart stops and starts the same container, preserving the JetStream store
// directory inside the container filesystem.
func (broker *NATS) Restart(t *testing.T) {
	t.Helper()
	broker.Stop(t)
	broker.Start(t)
}

// ServerVersion reads the running server version from the monitoring /varz.
func (broker *NATS) ServerVersion(t *testing.T) string {
	t.Helper()
	var document struct {
		Version string `json:"version"`
	}
	if err := getJSON(broker.Shard.Context(), broker.MonitorURL+"/varz", &document); err != nil {
		t.Fatalf("read nats version: %v", err)
	}
	if document.Version == "" {
		t.Fatal("nats monitor reported no version")
	}
	return document.Version
}

// Raw opens one plain nats.go connection used only to inspect or seed the
// broker. It is closed at test cleanup.
func (broker *NATS) Raw(t *testing.T) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(broker.URL, natsgo.Name("i02c-raw-"+t.Name()))
	if err != nil {
		t.Fatalf("raw nats connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// EnsureStreams creates the events and invalidations streams the broker
// adapter publishes into. The subjects are the contract's scope-qualified
// layout captured with broker-side wildcards.
func (broker *NATS) EnsureStreams(t *testing.T) {
	t.Helper()
	ctx := broker.Shard.Context()
	conn := broker.Raw(t)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	streams := []jetstream.StreamConfig{
		{Name: EventsStream, Subjects: []string{EventsStreamSubjects}, Storage: jetstream.FileStorage},
		{Name: InvalidationsStream, Subjects: []string{InvalidationsStreamSubjects}, Storage: jetstream.FileStorage},
	}
	for _, config := range streams {
		if _, err := js.CreateOrUpdateStream(ctx, config); err != nil {
			t.Fatalf("create stream %s: %v", config.Name, err)
		}
	}
}

// JetStream returns a JetStream context over a raw connection.
func (broker *NATS) JetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(broker.Raw(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// StreamInfo reads one stream's current state.
func (broker *NATS) StreamInfo(t *testing.T, stream string) *jetstream.StreamInfo {
	t.Helper()
	ctx := broker.Shard.Context()
	js := broker.JetStream(t)
	info, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("stream %s: %v", stream, err)
	}
	state, err := info.Info(ctx)
	if err != nil {
		t.Fatalf("stream %s info: %v", stream, err)
	}
	return state
}

// WaitForStreamMessages is a bounded barrier on the stored message count.
func (broker *NATS) WaitForStreamMessages(t *testing.T, stream string, want uint64) *jetstream.StreamInfo {
	t.Helper()
	var last *jetstream.StreamInfo
	probe := func(context.Context) error {
		last = broker.StreamInfo(t, stream)
		if last.State.Msgs != want {
			return fmt.Errorf("stream %s has %d messages; want %d", stream, last.State.Msgs, want)
		}
		return nil
	}
	if err := harness.WaitReady(broker.Shard.Context(), harness.RealClock{}, "stream "+stream, probe, fastWaitOptions()); err != nil {
		t.Fatalf("stream message barrier: %v", err)
	}
	return last
}

// DriverConfig returns the NATS driver configuration for this broker.
func (broker *NATS) DriverConfig(name string) natsdriver.Config {
	return natsdriver.Config{
		URL:            broker.URL,
		Name:           name,
		ConnectTimeout: 3 * time.Second,
		ReconnectWait:  50 * time.Millisecond,
		MaxReconnects:  -1,
		DrainTimeout:   2 * time.Second,
	}
}

// DialDriver connects one real NATS driver client. It is closed at cleanup.
func (broker *NATS) DialDriver(t *testing.T, config natsdriver.Config) *natsdriver.Client {
	t.Helper()
	client, err := natsdriver.Dial(broker.Shard.Context(), config)
	if err != nil {
		t.Fatalf("dial driver: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

// ConnectionSubjects returns the live server-side subscription subjects of one
// named client connection, or nil when the connection is absent.
func (broker *NATS) ConnectionSubjects(t *testing.T, name string) []string {
	t.Helper()
	var document struct {
		Connections []struct {
			Name               string   `json:"name"`
			SubscriptionCount  int      `json:"subscriptions"`
			SubscriptionTopics []string `json:"subscriptions_list"`
		} `json:"connections"`
	}
	if err := getJSON(broker.Shard.Context(), broker.MonitorURL+"/connz?subs=1", &document); err != nil {
		t.Fatalf("read nats connections: %v", err)
	}
	for _, connection := range document.Connections {
		if connection.Name == name {
			return append([]string(nil), connection.SubscriptionTopics...)
		}
	}
	return nil
}

// WaitForNoSubscription is a bounded barrier proving the server no longer has
// the named subscription on the named connection.
func (broker *NATS) WaitForNoSubscription(t *testing.T, name, subject string) {
	t.Helper()
	probe := func(context.Context) error {
		if slices.Contains(broker.ConnectionSubjects(t, name), subject) {
			return fmt.Errorf("connection %q still subscribes to %s", name, subject)
		}
		return nil
	}
	if err := harness.WaitReady(broker.Shard.Context(), harness.RealClock{}, "subscription closed", probe, waitOptions(15*time.Second)); err != nil {
		t.Fatalf("subscription close barrier: %v", err)
	}
}

// WaitForSubscription is a bounded barrier proving the server has the named
// subscription on the named connection.
func (broker *NATS) WaitForSubscription(t *testing.T, name, subject string) {
	t.Helper()
	probe := func(context.Context) error {
		if slices.Contains(broker.ConnectionSubjects(t, name), subject) {
			return nil
		}
		return fmt.Errorf("connection %q does not subscribe to %s", name, subject)
	}
	if err := harness.WaitReady(broker.Shard.Context(), harness.RealClock{}, "subscription open", probe, waitOptions(15*time.Second)); err != nil {
		t.Fatalf("subscription open barrier: %v", err)
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
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("GET %s returned %d", rawURL, response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}

func fastWaitOptions() harness.WaitOptions {
	return waitOptions(10 * time.Second)
}

func waitOptions(timeout time.Duration) harness.WaitOptions {
	return harness.WaitOptions{
		Timeout:        timeout,
		AttemptTimeout: 2 * time.Second,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     250 * time.Millisecond,
	}
}

// RealClock is the broker adapter clock over the real time source.
type RealClock struct{}

// Now implements nats.Clock.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep implements nats.Clock and honors context cancellation.
func (RealClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// NoopSink accepts observations without an effect; these scenarios never use
// the ingress.
type NoopSink struct{}

// Observe implements nats.ObservationSink.
func (NoopSink) Observe(context.Context, broker.Observation) error { return nil }

// Host returns the broker host:port.
func (broker *NATS) Host() string { return strings.TrimPrefix(broker.URL, "nats://") }

// FastWaitOptions returns the suite's bounded barrier options.
func FastWaitOptions() harness.WaitOptions { return fastWaitOptions() }
