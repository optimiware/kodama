package app

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/event"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fakeNATSServer models the stream state and connection lifecycle behind the
// composition-owned driver seam. It records every administrative call in one
// global sequence so a test can prove provisioning happens before publication.
type fakeNATSServer struct {
	mu sync.Mutex

	dials   int
	clients []*fakeNATSClient
	configs []natsdriver.Config

	ensureCalls []natsdriver.StreamConfig
	publishes   []id.EventID

	// liveSubjects and livePayloads record Core NATS live-log publications in
	// call order, so a composition test can prove the real subject and payload
	// contract without a broker process.
	liveSubjects []natsbroker.Subject
	livePayloads [][]byte

	sequence   int
	ensureSeq  int
	publishSeq int

	publishBeforeEnsure bool
	provisioned         bool

	ensureErr     error
	ensureErrOnce error
	dialErr       error
}

func (server *fakeNATSServer) dial(ctx context.Context, config natsdriver.Config) (natsClient, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.dials++
	server.configs = append(server.configs, config)
	if server.dialErr != nil {
		return nil, server.dialErr
	}
	client := &fakeNATSClient{server: server}
	server.clients = append(server.clients, client)
	return client, nil
}

func (server *fakeNATSServer) ensureCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.ensureCalls)
}

func (server *fakeNATSServer) dialCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.dials
}

func (server *fakeNATSServer) dialConfigs() []natsdriver.Config {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]natsdriver.Config(nil), server.configs...)
}

func (server *fakeNATSServer) publishedIDs() []id.EventID {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]id.EventID(nil), server.publishes...)
}

func (server *fakeNATSServer) firstEnsureConfig(t *testing.T) natsdriver.StreamConfig {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.ensureCalls) == 0 {
		t.Fatal("EnsureStreams was never called")
	}
	return server.ensureCalls[0]
}

func (server *fakeNATSServer) allClientsClosed() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, client := range server.clients {
		if !client.isClosed() {
			return false
		}
	}
	return len(server.clients) > 0
}

// fakeNATSClient implements the composition's natsClient seam. Every dial
// returns a fresh client so the provisioning connection and the broker
// connection are distinguishable.
type fakeNATSClient struct {
	server *fakeNATSServer

	mu     sync.Mutex
	closed bool
}

func (client *fakeNATSClient) EnsureStreams(_ context.Context, config natsdriver.StreamConfig) error {
	server := client.server
	server.mu.Lock()
	defer server.mu.Unlock()
	server.ensureCalls = append(server.ensureCalls, config)
	if server.ensureErr != nil {
		return server.ensureErr
	}
	if server.ensureErrOnce != nil {
		err := server.ensureErrOnce
		server.ensureErrOnce = nil
		return err
	}
	server.sequence++
	server.ensureSeq = server.sequence
	server.provisioned = true
	return nil
}

func (client *fakeNATSClient) PublishJetStream(_ context.Context, _ natsbroker.Subject, eventID id.EventID, _ []byte) error {
	server := client.server
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.ensureCalls) == 0 {
		server.publishBeforeEnsure = true
	}
	server.sequence++
	server.publishSeq = server.sequence
	server.publishes = append(server.publishes, eventID)
	return nil
}

func (client *fakeNATSClient) PublishCore(_ context.Context, subject natsbroker.Subject, payload []byte) error {
	server := client.server
	server.mu.Lock()
	defer server.mu.Unlock()
	server.liveSubjects = append(server.liveSubjects, subject)
	server.livePayloads = append(server.livePayloads, append([]byte(nil), payload...))
	return nil
}

// livePublications returns the recorded live subjects and payloads in call
// order.
func (server *fakeNATSServer) livePublications() ([]natsbroker.Subject, [][]byte) {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]natsbroker.Subject(nil), server.liveSubjects...),
		append([][]byte(nil), server.livePayloads...)
}

func (client *fakeNATSClient) Subscribe(context.Context, natsbroker.Subject, func(context.Context, []byte) error) (natsbroker.Subscription, error) {
	return nil, fault.New(fault.UnsupportedCapability, "fake client has no subscriptions")
}

func (client *fakeNATSClient) Close(context.Context) error {
	client.mu.Lock()
	client.closed = true
	client.mu.Unlock()
	return nil
}

func (client *fakeNATSClient) isClosed() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.closed
}

func productionConfigWithServer(t *testing.T, server *fakeNATSServer) Config {
	t.Helper()
	config := validProductionConfig(t)
	config.Production.dialNATS = server.dial
	return config
}

func streamTestEnvelope(t *testing.T) event.Envelope {
	t.Helper()
	envelope, err := event.NewEnvelope("event-1",
		event.Source{Kind: event.SourceService, PrincipalID: "service-1"},
		tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"},
		time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		event.EnvelopeInput{
			Type:      event.TypeCustomObservation,
			SubjectID: "subject-1",
			Data:      []byte(`{}`),
		})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return envelope
}

// TestProductionStartupProvisionsStreamsExactlyOnce proves the production
// startup dials the broker, provisions the two JetStream streams through the
// driver seam exactly once, closes the provisioning connection, and performs no
// publication before the background loops exist.
func TestProductionStartupProvisionsStreamsExactlyOnce(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server)

	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	if got := server.ensureCount(); got != 1 {
		t.Fatalf("EnsureStreams calls = %d; want exactly 1", got)
	}
	if got := server.dialCount(); got != 1 {
		t.Fatalf("driver dials = %d; want exactly 1 (the provisioning connection)", got)
	}
	if published := server.publishedIDs(); len(published) != 0 {
		t.Fatalf("startup published %v; want no publication before the loops start", published)
	}
	if !server.allClientsClosed() {
		t.Fatal("startup left the provisioning connection open")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestProductionStartupAppliesConfiguredStreamLimits proves the app Config is
// the single source of the provisioning limits: the driver receives the
// converted explicit values, and a zero field selects the driver default.
func TestProductionStartupAppliesConfiguredStreamLimits(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server)
	config.Production.Streams = &StreamConfig{
		Events: StreamRetention{
			MaxAge:          48 * time.Hour,
			MaxBytes:        1 << 30,
			DuplicateWindow: 30 * time.Minute,
		},
		Invalidations: StreamRetention{
			MaxAge:          2 * time.Hour,
			MaxBytes:        64 << 20,
			DuplicateWindow: 10 * time.Minute,
		},
		Replicas: 3,
	}

	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	got := server.firstEnsureConfig(t)
	if got.Events.MaxAge != 48*time.Hour || got.Events.MaxBytes != 1<<30 || got.Events.DuplicateWindow != 30*time.Minute {
		t.Fatalf("events retention = %+v; want the configured values", got.Events)
	}
	if got.Invalidations.MaxAge != 2*time.Hour || got.Invalidations.MaxBytes != 64<<20 || got.Invalidations.DuplicateWindow != 10*time.Minute {
		t.Fatalf("invalidations retention = %+v; want the configured values", got.Invalidations)
	}
	if got.Replicas != 3 {
		t.Fatalf("replicas = %d; want 3", got.Replicas)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestProductionPublicationDialsOnlyAfterStreamProvisioning proves the
// publication path cannot reach the broker before startup provisioning has
// completed: the first publish is sequenced strictly after EnsureStreams.
func TestProductionPublicationDialsOnlyAfterStreamProvisioning(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() {
		builder.cleanup(context.Background())
	})
	if got := server.ensureCount(); got != 1 {
		t.Fatalf("EnsureStreams calls = %d; want 1", got)
	}
	if err := environment.publisher.Publish(context.Background(), streamTestEnvelope(t)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.publishBeforeEnsure {
		t.Fatal("the broker published before the streams were provisioned")
	}
	if server.publishSeq <= server.ensureSeq {
		t.Fatalf("publish sequence %d is not after ensure sequence %d", server.publishSeq, server.ensureSeq)
	}
	if len(server.publishes) != 1 || server.publishes[0] != "event-1" {
		t.Fatalf("published = %v; want [event-1]", server.publishes)
	}
}

// TestProductionStartupRefusesStreamConflictAndCleansUp proves a stream
// conflict is a startup failure: no App is returned, every registered
// dependency is stopped in reverse order, the provisioning connection is
// closed, and no goroutine is leaked.
func TestProductionStartupRefusesStreamConflictAndCleansUp(t *testing.T) {
	server := &fakeNATSServer{
		ensureErr: fault.New(fault.VersionConflict,
			`existing stream "KODAMA_EVENTS" conflicts with the required configuration (subjects)`),
	}
	config := productionConfigWithServer(t, server)
	var (
		mu    sync.Mutex
		stops []string
	)
	config.Production.RuntimeCloser = func(context.Context) error {
		mu.Lock()
		stops = append(stops, "runtime-driver")
		mu.Unlock()
		return nil
	}
	config.hooks = &startupHook{}

	baseline := runtime.NumGoroutine()
	app, err := New(context.Background(), config)
	if err == nil {
		t.Fatal("New succeeded despite the stream conflict")
	}
	if app != nil {
		t.Fatal("New returned an App after a startup failure")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("startup error class = %q (found=%v); want version_conflict", class, ok)
	}
	if !strings.Contains(err.Error(), "stream provisioning") {
		t.Fatalf("startup error = %q; want a stream provisioning failure", err.Error())
	}
	if !server.allClientsClosed() {
		t.Fatal("the failed startup left the provisioning connection open")
	}
	mu.Lock()
	gotStops := append([]string(nil), stops...)
	mu.Unlock()
	if len(gotStops) != 1 || gotStops[0] != "runtime-driver" {
		t.Fatalf("runtime-driver stops = %v; want exactly one", gotStops)
	}
	if got := config.hooks.stopped; len(got) != 3 || got[0] != "runtime-driver" || got[1] != "live-log-publisher" || got[2] != "broker" {
		t.Fatalf("cleanup order = %v; want [runtime-driver live-log-publisher broker]", got)
	}
	eventually(t, time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+1
	})
}

// TestProductionStartupRefusesDialFailureSecretFree proves a dial failure is a
// startup failure whose public error carries the fault class and never the
// broker URL or credentials.
func TestProductionStartupRefusesDialFailureSecretFree(t *testing.T) {
	server := &fakeNATSServer{
		dialErr: fault.Wrap(fault.DependencyUnavailable, "broker connection failed",
			errors.New("dial tcp nats://kodama:supersecret@broker.internal:4222: connection refused")),
	}
	config := productionConfigWithServer(t, server)

	app, err := New(context.Background(), config)
	if err == nil {
		t.Fatal("New succeeded despite the broker dial failure")
	}
	if app != nil {
		t.Fatal("New returned an App after a startup failure")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("startup error class = %q (found=%v); want dependency_unavailable", class, ok)
	}
	rendered := err.Error()
	for _, secret := range []string{"nats://", "supersecret", "kodama:", "broker.internal", "4222"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("startup error leaked %q: %s", secret, rendered)
		}
	}
}

// TestProductionRestartReprovisionsIdempotently proves every production
// startup provisions again through the idempotent driver operation, so a
// restart against already-provisioned streams succeeds.
func TestProductionRestartReprovisionsIdempotently(t *testing.T) {
	server := &fakeNATSServer{provisioned: true}
	first := productionConfigWithServer(t, server)
	app, err := New(context.Background(), first)
	if err != nil {
		t.Fatalf("first startup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second := productionConfigWithServer(t, server)
	app, err = New(context.Background(), second)
	if err != nil {
		t.Fatalf("restart against provisioned streams: %v", err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := server.ensureCount(); got != 2 {
		t.Fatalf("EnsureStreams calls = %d; want one per startup", got)
	}
	if !server.provisioned {
		t.Fatal("the fake broker was never provisioned")
	}
}

// TestProductionRestartRecoversAfterPartialProvisioningFailure proves a
// transient provisioning failure aborts the first startup, and a restart
// against the partially provisioned server succeeds because EnsureStreams is
// idempotent.
func TestProductionRestartRecoversAfterPartialProvisioningFailure(t *testing.T) {
	server := &fakeNATSServer{
		ensureErrOnce: fault.New(fault.DependencyUnavailable, "stream creation failed"),
	}
	if app, err := New(context.Background(), productionConfigWithServer(t, server)); err == nil {
		t.Fatal("first startup succeeded despite the provisioning failure")
	} else if app != nil {
		t.Fatal("first startup returned an App after the provisioning failure")
	}
	if got := server.ensureCount(); got != 1 {
		t.Fatalf("first startup EnsureStreams calls = %d; want 1", got)
	}
	if !server.allClientsClosed() {
		t.Fatal("the failed startup left the provisioning connection open")
	}
	// The events stream was created before the failure; the restart must adopt
	// it instead of refusing or replaying the creation.
	server.mu.Lock()
	server.provisioned = true
	server.mu.Unlock()

	app, err := New(context.Background(), productionConfigWithServer(t, server))
	if err != nil {
		t.Fatalf("restart after a partial provisioning failure: %v", err)
	}
	if got := server.ensureCount(); got != 2 {
		t.Fatalf("restart EnsureStreams calls = %d; want 2 total", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDevelopmentProfileNeverProvisionsStreams proves the development profile
// builds the in-process broker and never constructs the real NATS driver, so it
// cannot provision JetStream streams.
func TestDevelopmentProfileNeverProvisionsStreams(t *testing.T) {
	environment := newTestEnvironment(t)
	config := environment.config.resolved()
	builder := &builder{cfg: config, logger: config.logger()}
	built, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() {
		builder.cleanup(context.Background())
	})
	if _, ok := built.publisher.(*inProcessBroker); !ok {
		t.Fatalf("development publisher = %T; want the in-process broker", built.publisher)
	}
	if config.Production != nil {
		t.Fatal("development composition accepted a production driver configuration")
	}
	if config.Profile != ProfileDevelopment {
		t.Fatalf("profile = %q; want development", config.Profile)
	}
}

// TestStreamConfigValidation covers the explicit limits, the driver defaults
// selected by zero values and the replica bounds before any adapter is built.
func TestStreamConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		streams *StreamConfig
		want    string
	}{
		{"nil uses defaults", nil, ""},
		{"zero uses defaults", &StreamConfig{}, ""},
		{"custom limits accepted", &StreamConfig{
			Events:        StreamRetention{MaxAge: 48 * time.Hour, MaxBytes: 1 << 30, DuplicateWindow: 30 * time.Minute},
			Invalidations: StreamRetention{MaxAge: 2 * time.Hour, MaxBytes: 64 << 20, DuplicateWindow: 10 * time.Minute},
			Replicas:      3,
		}, ""},
		{"negative events max age", &StreamConfig{Events: StreamRetention{MaxAge: -time.Hour}}, "max age"},
		{"negative events max bytes", &StreamConfig{Events: StreamRetention{MaxBytes: -1}}, "max bytes"},
		{"negative events duplicate window", &StreamConfig{Events: StreamRetention{DuplicateWindow: -time.Second}}, "duplicate window"},
		{"events duplicate window below minimum", &StreamConfig{Events: StreamRetention{DuplicateWindow: 50 * time.Millisecond}}, "minimum"},
		{"events duplicate window exceeds age", &StreamConfig{Events: StreamRetention{MaxAge: time.Hour, DuplicateWindow: 2 * time.Hour}}, "exceeds"},
		{"negative invalidations max age", &StreamConfig{Invalidations: StreamRetention{MaxAge: -time.Hour}}, "max age"},
		{"negative invalidations max bytes", &StreamConfig{Invalidations: StreamRetention{MaxBytes: -1}}, "max bytes"},
		{"negative invalidations duplicate window", &StreamConfig{Invalidations: StreamRetention{DuplicateWindow: -time.Second}}, "duplicate window"},
		{"invalidations duplicate window exceeds age", &StreamConfig{Invalidations: StreamRetention{MaxAge: time.Hour, DuplicateWindow: 2 * time.Hour}}, "exceeds"},
		{"negative replicas", &StreamConfig{Replicas: -1}, "replicas"},
		{"too many replicas", &StreamConfig{Replicas: 6}, "replicas"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := validProductionConfig(t)
			config.Production.Streams = testCase.streams
			err := config.resolved().validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("validate refused a valid stream configuration: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate accepted an invalid stream configuration")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}
