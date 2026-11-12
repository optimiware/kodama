package natsdriver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fakeStreamAdmin is the consumer-owned JetStream stream-administration seam.
// It records every administrative call so the unit suite can assert that
// provisioning touches exactly the two contract streams.
type fakeStreamAdmin struct {
	mu        sync.Mutex
	streams   map[string]jetstream.StreamConfig
	created   []jetstream.StreamConfig
	updated   []jetstream.StreamConfig
	lookups   []string
	streamErr error
	createErr error
	updateErr error
	// onStream runs before a lookup result is returned. It models a
	// concurrent writer or a cancellation barrier.
	onStream func(name string) error
	// onCreate runs before a creation result is returned.
	onCreate func(config jetstream.StreamConfig) error
}

func newFakeStreamAdmin() *fakeStreamAdmin {
	return &fakeStreamAdmin{streams: make(map[string]jetstream.StreamConfig)}
}

func (admin *fakeStreamAdmin) StreamConfig(ctx context.Context, name string) (jetstream.StreamConfig, error) {
	if err := ctx.Err(); err != nil {
		return jetstream.StreamConfig{}, err
	}
	admin.mu.Lock()
	hook := admin.onStream
	admin.lookups = append(admin.lookups, name)
	admin.mu.Unlock()
	if hook != nil {
		if err := hook(name); err != nil {
			return jetstream.StreamConfig{}, err
		}
	}
	admin.mu.Lock()
	err := admin.streamErr
	config, found := admin.streams[name]
	admin.mu.Unlock()
	if err != nil {
		return jetstream.StreamConfig{}, err
	}
	if !found {
		return jetstream.StreamConfig{}, errStreamNotFound
	}
	return config, nil
}

func (admin *fakeStreamAdmin) CreateStream(ctx context.Context, config jetstream.StreamConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	admin.mu.Lock()
	hook := admin.onCreate
	err := admin.createErr
	admin.mu.Unlock()
	if hook != nil {
		if err := hook(config); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if _, found := admin.streams[config.Name]; found {
		return errStreamExists
	}
	admin.streams[config.Name] = config
	admin.created = append(admin.created, config)
	return nil
}

func (admin *fakeStreamAdmin) UpdateStream(ctx context.Context, config jetstream.StreamConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if admin.updateErr != nil {
		return admin.updateErr
	}
	admin.streams[config.Name] = config
	admin.updated = append(admin.updated, config)
	return nil
}

func (admin *fakeStreamAdmin) seed(config jetstream.StreamConfig) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.streams[config.Name] = config
}

func (admin *fakeStreamAdmin) createdConfigs() []jetstream.StreamConfig {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return append([]jetstream.StreamConfig(nil), admin.created...)
}

func (admin *fakeStreamAdmin) updatedConfigs() []jetstream.StreamConfig {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return append([]jetstream.StreamConfig(nil), admin.updated...)
}

func (admin *fakeStreamAdmin) lookupNames() []string {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return append([]string(nil), admin.lookups...)
}

func (admin *fakeStreamAdmin) stored(name string) (jetstream.StreamConfig, bool) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	config, found := admin.streams[name]
	return config, found
}

func newStreamsTestClient(admin streamAdmin) *Client {
	return &Client{streams: admin}
}

type streamExpectation struct {
	name            string
	subjects        []string
	maxAge          time.Duration
	maxBytes        int64
	duplicateWindow time.Duration
	replicas        int
	denyDelete      bool
	denyPurge       bool
	allowRollup     bool
}

func assertStreamConfig(t *testing.T, got jetstream.StreamConfig, want streamExpectation) {
	t.Helper()
	if got.Name != want.name {
		t.Fatalf("stream name = %q; want %q", got.Name, want.name)
	}
	if !reflect.DeepEqual(got.Subjects, want.subjects) {
		t.Fatalf("stream %q subjects = %v; want %v", want.name, got.Subjects, want.subjects)
	}
	if got.Storage != jetstream.FileStorage {
		t.Fatalf("stream %q storage = %v; want file", want.name, got.Storage)
	}
	if got.Retention != jetstream.LimitsPolicy {
		t.Fatalf("stream %q retention = %v; want limits", want.name, got.Retention)
	}
	if got.Discard != jetstream.DiscardOld {
		t.Fatalf("stream %q discard = %v; want old", want.name, got.Discard)
	}
	if got.NoAck || got.Sealed || got.Mirror != nil || len(got.Sources) > 0 {
		t.Fatalf("stream %q has an unexpected acknowledgement, seal, mirror or source", want.name)
	}
	if got.SubjectTransform != nil || got.RePublish != nil {
		t.Fatalf("stream %q has an unexpected subject transform or republish", want.name)
	}
	if got.MaxMsgs != 0 || got.MaxMsgsPerSubject != 0 || got.MaxMsgSize != 0 {
		t.Fatalf("stream %q has an unexpected finite message limit", want.name)
	}
	if got.MaxAge != want.maxAge {
		t.Fatalf("stream %q max age = %v; want %v", want.name, got.MaxAge, want.maxAge)
	}
	if got.MaxBytes != want.maxBytes {
		t.Fatalf("stream %q max bytes = %d; want %d", want.name, got.MaxBytes, want.maxBytes)
	}
	if got.Duplicates != want.duplicateWindow {
		t.Fatalf("stream %q duplicate window = %v; want %v", want.name, got.Duplicates, want.duplicateWindow)
	}
	if got.Replicas != want.replicas {
		t.Fatalf("stream %q replicas = %d; want %d", want.name, got.Replicas, want.replicas)
	}
	if got.DenyDelete != want.denyDelete {
		t.Fatalf("stream %q deny delete = %v; want %v", want.name, got.DenyDelete, want.denyDelete)
	}
	if got.DenyPurge != want.denyPurge {
		t.Fatalf("stream %q deny purge = %v; want %v", want.name, got.DenyPurge, want.denyPurge)
	}
	if got.AllowRollup != want.allowRollup {
		t.Fatalf("stream %q allow rollup = %v; want %v", want.name, got.AllowRollup, want.allowRollup)
	}
}

func defaultEventsExpectation() streamExpectation {
	return streamExpectation{
		name:            EventsStreamName,
		subjects:        []string{EventsStreamSubjects},
		maxAge:          DefaultEventsMaxAge,
		maxBytes:        DefaultEventsMaxBytes,
		duplicateWindow: DefaultDuplicateWindow,
		replicas:        DefaultReplicas,
		denyDelete:      true,
		denyPurge:       true,
		allowRollup:     false,
	}
}

func defaultInvalidationsExpectation() streamExpectation {
	return streamExpectation{
		name:            InvalidationsStreamName,
		subjects:        []string{InvalidationsStreamSubjects},
		maxAge:          DefaultInvalidationsMaxAge,
		maxBytes:        DefaultInvalidationsMaxBytes,
		duplicateWindow: DefaultDuplicateWindow,
		replicas:        DefaultReplicas,
	}
}

// TestEnsureStreamsCreatesBothStreamsWithExactConfig pins the whole
// provisioning contract: exactly the two named streams, the scope-qualified
// wildcard filters, file storage, limits retention with bounded age/bytes, the
// dedupe window, replicas, old-discard, and append-only events.
func TestEnsureStreamsCreatesBothStreamsWithExactConfig(t *testing.T) {
	admin := newFakeStreamAdmin()
	client := newStreamsTestClient(admin)
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	created := admin.createdConfigs()
	if len(created) != 2 {
		t.Fatalf("created streams = %d; want exactly 2", len(created))
	}
	if created[0].Name != EventsStreamName || created[1].Name != InvalidationsStreamName {
		t.Fatalf("created streams = %q, %q; want %q, %q",
			created[0].Name, created[1].Name, EventsStreamName, InvalidationsStreamName)
	}
	assertStreamConfig(t, created[0], defaultEventsExpectation())
	assertStreamConfig(t, created[1], defaultInvalidationsExpectation())
	if updated := admin.updatedConfigs(); len(updated) != 0 {
		t.Fatalf("fresh provisioning updated %d streams; want 0", len(updated))
	}
	// Only the two contract stream names were looked up or created. The seam
	// has no consumer surface, so provisioning cannot create a consumer.
	for _, name := range admin.lookupNames() {
		if name != EventsStreamName && name != InvalidationsStreamName {
			t.Fatalf("provisioning touched unrelated stream %q", name)
		}
	}
}

func TestEnsureStreamsIsIdempotent(t *testing.T) {
	admin := newFakeStreamAdmin()
	client := newStreamsTestClient(admin)
	for attempt := range 3 {
		if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
			t.Fatalf("EnsureStreams attempt %d: %v", attempt, err)
		}
	}
	if created := admin.createdConfigs(); len(created) != 2 {
		t.Fatalf("created streams = %d; want 2", len(created))
	}
	if updated := admin.updatedConfigs(); len(updated) != 0 {
		t.Fatalf("repeat provisioning updated %d streams; want 0", len(updated))
	}
}

func TestEnsureStreamsUpdatesOnlyChangedLimits(t *testing.T) {
	admin := newFakeStreamAdmin()
	client := newStreamsTestClient(admin)
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
		t.Fatalf("initial EnsureStreams: %v", err)
	}

	err := client.EnsureStreams(context.Background(), StreamConfig{
		Events: StreamRetention{
			MaxAge:          48 * time.Hour,
			MaxBytes:        2 << 30,
			DuplicateWindow: 2 * time.Hour,
		},
		Invalidations: StreamRetention{MaxBytes: 64 << 20},
	})
	if err != nil {
		t.Fatalf("EnsureStreams with changed limits: %v", err)
	}
	updated := admin.updatedConfigs()
	if len(updated) != 2 {
		t.Fatalf("updated streams = %d; want 2", len(updated))
	}
	assertStreamConfig(t, updated[0], streamExpectation{
		name:            EventsStreamName,
		subjects:        []string{EventsStreamSubjects},
		maxAge:          48 * time.Hour,
		maxBytes:        2 << 30,
		duplicateWindow: 2 * time.Hour,
		replicas:        DefaultReplicas,
		denyDelete:      true,
		denyPurge:       true,
	})
	assertStreamConfig(t, updated[1], streamExpectation{
		name:            InvalidationsStreamName,
		subjects:        []string{InvalidationsStreamSubjects},
		maxAge:          DefaultInvalidationsMaxAge,
		maxBytes:        64 << 20,
		duplicateWindow: DefaultDuplicateWindow,
		replicas:        DefaultReplicas,
	})
	// Limits changes never recreate or delete a stream.
	if created := admin.createdConfigs(); len(created) != 2 {
		t.Fatalf("created streams after a limits update = %d; want 2", len(created))
	}
}

func TestEnsureStreamsLeavesUnchangedStreamAlone(t *testing.T) {
	admin := newFakeStreamAdmin()
	client := newStreamsTestClient(admin)
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
		t.Fatalf("initial EnsureStreams: %v", err)
	}
	if err := client.EnsureStreams(context.Background(), StreamConfig{
		Events: StreamRetention{MaxAge: 48 * time.Hour},
	}); err != nil {
		t.Fatalf("EnsureStreams with one changed limit: %v", err)
	}
	updated := admin.updatedConfigs()
	if len(updated) != 1 || updated[0].Name != EventsStreamName {
		t.Fatalf("updated streams = %v; want only %q", updated, EventsStreamName)
	}
	if updated[0].MaxAge != 48*time.Hour {
		t.Fatalf("events max age = %v; want 48h", updated[0].MaxAge)
	}
}

// TestEnsureStreamsRefusesConflictingStream proves a pre-existing stream is
// never silently reconfigured into the contract shape or a weaker one. Every
// non-limit difference is a refusal, and the stored stream is left untouched.
func TestEnsureStreamsRefusesConflictingStream(t *testing.T) {
	cases := []struct {
		reason string
		mutate func(*jetstream.StreamConfig)
	}{
		{"subjects", func(config *jetstream.StreamConfig) {
			config.Subjects = []string{"kodama.v1.*.*.ingress"}
		}},
		{"storage", func(config *jetstream.StreamConfig) {
			config.Storage = jetstream.MemoryStorage
		}},
		{"retention", func(config *jetstream.StreamConfig) {
			config.Retention = jetstream.WorkQueuePolicy
		}},
		{"discard policy", func(config *jetstream.StreamConfig) {
			config.Discard = jetstream.DiscardNew
		}},
		{"replicas", func(config *jetstream.StreamConfig) {
			config.Replicas = 3
		}},
		{"deny-delete", func(config *jetstream.StreamConfig) {
			config.DenyDelete = false
		}},
		{"deny-purge", func(config *jetstream.StreamConfig) {
			config.DenyPurge = false
		}},
		{"rollup", func(config *jetstream.StreamConfig) {
			config.AllowRollup = true
		}},
		{"no-ack", func(config *jetstream.StreamConfig) {
			config.NoAck = true
		}},
		{"sealed", func(config *jetstream.StreamConfig) {
			config.Sealed = true
		}},
		{"mirror/sources", func(config *jetstream.StreamConfig) {
			config.Sources = []*jetstream.StreamSource{{Name: "OTHER"}}
		}},
		{"subject transform", func(config *jetstream.StreamConfig) {
			config.SubjectTransform = &jetstream.SubjectTransformConfig{
				Source:      "kodama.v1.*.*.events",
				Destination: "other.>",
			}
		}},
		{"republish", func(config *jetstream.StreamConfig) {
			config.RePublish = &jetstream.RePublish{Destination: "other.>"}
		}},
		{"message count limit", func(config *jetstream.StreamConfig) {
			config.MaxMsgs = 10
		}},
		{"per-subject message limit", func(config *jetstream.StreamConfig) {
			config.MaxMsgsPerSubject = 10
		}},
		{"message size limit", func(config *jetstream.StreamConfig) {
			config.MaxMsgSize = 1024
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.reason, func(t *testing.T) {
			admin := newFakeStreamAdmin()
			existing := eventsStreamConfig(StreamConfig{})
			testCase.mutate(&existing)
			admin.seed(existing)
			client := newStreamsTestClient(admin)

			err := client.EnsureStreams(context.Background(), StreamConfig{})
			if err == nil {
				t.Fatalf("EnsureStreams adopted a stream with conflicting %s", testCase.reason)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.VersionConflict {
				t.Fatalf("conflict fault class = %q (ok=%v); want %q", class, ok, fault.VersionConflict)
			}
			if !strings.Contains(err.Error(), EventsStreamName) {
				t.Fatalf("conflict error does not name the stream: %v", err)
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Fatalf("conflict error does not name the difference %q: %v", testCase.reason, err)
			}
			if created := admin.createdConfigs(); len(created) != 0 {
				t.Fatalf("conflict created %d streams; want 0", len(created))
			}
			if updated := admin.updatedConfigs(); len(updated) != 0 {
				t.Fatalf("conflict updated %d streams; want 0", len(updated))
			}
			stored, found := admin.stored(EventsStreamName)
			if !found || !reflect.DeepEqual(stored, existing) {
				t.Fatalf("conflicting stream was reconfigured: %#v", stored)
			}
		})
	}
}

func TestEnsureStreamsMapsCancellation(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		admin := newFakeStreamAdmin()
		client := newStreamsTestClient(admin)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := client.EnsureStreams(ctx, StreamConfig{})
		if err == nil {
			t.Fatal("EnsureStreams accepted a canceled context")
		}
		if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
			t.Fatalf("fault class = %q; want deadline_expired", class)
		}
		if created := admin.createdConfigs(); len(created) != 0 {
			t.Fatalf("canceled provisioning created %d streams", len(created))
		}
	})

	t.Run("between streams", func(t *testing.T) {
		admin := newFakeStreamAdmin()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		admin.onStream = func(name string) error {
			if name == InvalidationsStreamName {
				cancel()
			}
			return nil
		}
		client := newStreamsTestClient(admin)
		err := client.EnsureStreams(ctx, StreamConfig{})
		if err == nil {
			t.Fatal("EnsureStreams continued after cancellation")
		}
		if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
			t.Fatalf("fault class = %q; want deadline_expired", class)
		}
		created := admin.createdConfigs()
		if len(created) != 1 || created[0].Name != EventsStreamName {
			t.Fatalf("created streams after cancellation = %v; want only %q", created, EventsStreamName)
		}
	})

	t.Run("driver returns context error", func(t *testing.T) {
		admin := newFakeStreamAdmin()
		admin.streamErr = context.Canceled
		client := newStreamsTestClient(admin)
		err := client.EnsureStreams(context.Background(), StreamConfig{})
		if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
			t.Fatalf("fault class = %q; want deadline_expired", class)
		}
	})
}

func TestEnsureStreamsMapsDependencyFailuresWithoutLeakingSecrets(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeStreamAdmin)
	}{
		{"lookup", func(admin *fakeStreamAdmin) {
			admin.streamErr = errors.New("lookup failed with token=supersecret")
		}},
		{"create", func(admin *fakeStreamAdmin) {
			admin.createErr = errors.New("create failed with token=supersecret")
		}},
		{"update", func(admin *fakeStreamAdmin) {
			admin.seed(eventsStreamConfig(StreamConfig{}))
			admin.seed(invalidationsStreamConfig(StreamConfig{}))
			admin.updateErr = errors.New("update failed with token=supersecret")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			admin := newFakeStreamAdmin()
			testCase.set(admin)
			client := newStreamsTestClient(admin)
			config := StreamConfig{}
			if testCase.name == "update" {
				config.Events.MaxAge = 48 * time.Hour
			}
			err := client.EnsureStreams(context.Background(), config)
			if err == nil {
				t.Fatal("EnsureStreams ignored a dependency failure")
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.DependencyUnavailable {
				t.Fatalf("fault class = %q (ok=%v); want %q", class, ok, fault.DependencyUnavailable)
			}
			if strings.Contains(err.Error(), "supersecret") {
				t.Fatalf("provisioning error leaked a secret: %v", err)
			}
		})
	}
}

func TestEnsureStreamsWithoutStreamAdministrationRefuses(t *testing.T) {
	client := &Client{}
	err := client.EnsureStreams(context.Background(), StreamConfig{})
	if err == nil {
		t.Fatal("EnsureStreams succeeded without a stream-administration seam")
	}
	if class, _ := fault.ClassOf(err); class != fault.DependencyUnavailable {
		t.Fatalf("fault class = %q; want dependency_unavailable", class)
	}
}

func TestEnsureStreamsValidatesConfig(t *testing.T) {
	cases := []struct {
		name   string
		config StreamConfig
	}{
		{"negative events age", StreamConfig{Events: StreamRetention{MaxAge: -time.Second}}},
		{"negative events bytes", StreamConfig{Events: StreamRetention{MaxBytes: -1}}},
		{"negative events dedupe", StreamConfig{Events: StreamRetention{DuplicateWindow: -time.Second}}},
		{"negative invalidation age", StreamConfig{Invalidations: StreamRetention{MaxAge: -time.Second}}},
		{"dedupe above default age", StreamConfig{Events: StreamRetention{MaxAge: 30 * time.Minute}}},
		{"dedupe below server minimum", StreamConfig{Events: StreamRetention{DuplicateWindow: 50 * time.Millisecond}}},
		{"negative replicas", StreamConfig{Replicas: -1}},
		{"replicas above server maximum", StreamConfig{Replicas: 6}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			admin := newFakeStreamAdmin()
			client := newStreamsTestClient(admin)
			err := client.EnsureStreams(context.Background(), testCase.config)
			if err == nil {
				t.Fatal("EnsureStreams accepted an invalid configuration")
			}
			if class, _ := fault.ClassOf(err); class != fault.InvalidInput {
				t.Fatalf("fault class = %q; want invalid_input", class)
			}
			if len(admin.lookupNames()) != 0 {
				t.Fatalf("invalid configuration touched the broker: %v", admin.lookupNames())
			}
		})
	}

	var nilContext context.Context
	if err := newStreamsTestClient(newFakeStreamAdmin()).EnsureStreams(nilContext, StreamConfig{}); err == nil {
		t.Fatal("EnsureStreams accepted a nil context")
	}
}

// TestEnsureStreamsAdoptsAStreamCreatedByAnotherProcess covers the create race
// between two composition roots: the loser sees "already in use", re-reads the
// stream and converges on it instead of failing or reconfiguring it.
func TestEnsureStreamsAdoptsAStreamCreatedByAnotherProcess(t *testing.T) {
	admin := newFakeStreamAdmin()
	admin.seed(eventsStreamConfig(StreamConfig{}))
	lookups := 0
	admin.onStream = func(name string) error {
		if name == EventsStreamName {
			lookups++
			if lookups == 1 {
				return errStreamNotFound
			}
		}
		return nil
	}
	admin.onCreate = func(config jetstream.StreamConfig) error {
		if config.Name == EventsStreamName {
			return errStreamExists
		}
		return nil
	}
	client := newStreamsTestClient(admin)
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
		t.Fatalf("EnsureStreams after a create race: %v", err)
	}
	created := admin.createdConfigs()
	if len(created) != 1 || created[0].Name != InvalidationsStreamName {
		t.Fatalf("created streams = %v; want only %q", created, InvalidationsStreamName)
	}
	if updated := admin.updatedConfigs(); len(updated) != 0 {
		t.Fatalf("adopting an equal stream updated %d streams; want 0", len(updated))
	}
}

// TestEnsureStreamsResumesAfterPartialCreation proves a failed second stream
// does not roll back the first, and a later call completes idempotently.
func TestEnsureStreamsResumesAfterPartialCreation(t *testing.T) {
	admin := newFakeStreamAdmin()
	failOnce := true
	admin.onCreate = func(config jetstream.StreamConfig) error {
		if config.Name == InvalidationsStreamName && failOnce {
			failOnce = false
			return errors.New("transient create failure")
		}
		return nil
	}
	client := newStreamsTestClient(admin)
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err == nil {
		t.Fatal("EnsureStreams ignored the failed second stream")
	}
	created := admin.createdConfigs()
	if len(created) != 1 || created[0].Name != EventsStreamName {
		t.Fatalf("created streams after partial failure = %v; want only %q", created, EventsStreamName)
	}
	if err := client.EnsureStreams(context.Background(), StreamConfig{}); err != nil {
		t.Fatalf("EnsureStreams retry: %v", err)
	}
	if created := admin.createdConfigs(); len(created) != 2 {
		t.Fatalf("created streams after retry = %d; want 2", len(created))
	}
	if updated := admin.updatedConfigs(); len(updated) != 0 {
		t.Fatalf("retry updated %d streams; want 0", len(updated))
	}
}

func TestEnsureStreamsConcurrentCallsConverge(t *testing.T) {
	admin := newFakeStreamAdmin()
	client := newStreamsTestClient(admin)
	const workers = 8
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wait.Go(func() {
			errs <- client.EnsureStreams(context.Background(), StreamConfig{})
		})
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureStreams: %v", err)
		}
	}
	if created := admin.createdConfigs(); len(created) != 2 {
		t.Fatalf("concurrent provisioning created %d streams; want 2", len(created))
	}
	if updated := admin.updatedConfigs(); len(updated) != 0 {
		t.Fatalf("concurrent provisioning updated %d streams; want 0", len(updated))
	}
}

// TestStreamSubjectsMatchAdapterSubjectBuilders pins the wildcard filters to
// the broker adapter's concrete scope-qualified builders. A filter that drifts
// from EventSubject/InvalidationSubject would silently stop capturing events.
func TestStreamSubjectsMatchAdapterSubjectBuilders(t *testing.T) {
	if EventsStreamSubjects != "kodama.v1.*.*.events" {
		t.Fatalf("events stream subjects = %q", EventsStreamSubjects)
	}
	if InvalidationsStreamSubjects != "kodama.v1.*.*.invalidations" {
		t.Fatalf("invalidations stream subjects = %q", InvalidationsStreamSubjects)
	}
	for _, filter := range []string{EventsStreamSubjects, InvalidationsStreamSubjects} {
		if err := validateStreamSubjectFilter(filter); err != nil {
			t.Fatalf("stream filter %q is not a valid NATS stream subject: %v", filter, err)
		}
	}

	scopes := []tenancy.Scope{
		{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")},
		{TenantID: id.TenantID("tenant-b")},
	}
	for _, scope := range scopes {
		events, err := broker.EventSubject(scope)
		if err != nil {
			t.Fatalf("EventSubject(%+v): %v", scope, err)
		}
		if !subjectMatchesFilter(EventsStreamSubjects, string(events)) {
			t.Fatalf("events filter %q does not capture adapter subject %q", EventsStreamSubjects, events)
		}
		invalidations, err := broker.InvalidationSubject(scope)
		if err != nil {
			t.Fatalf("InvalidationSubject(%+v): %v", scope, err)
		}
		if !subjectMatchesFilter(InvalidationsStreamSubjects, string(invalidations)) {
			t.Fatalf("invalidations filter %q does not capture adapter subject %q",
				InvalidationsStreamSubjects, invalidations)
		}
	}

	if subjectMatchesFilter(EventsStreamSubjects, "kodama.v1.tenant-a.project-a.ingress") {
		t.Fatal("the events filter captured an ingress subject")
	}
	if subjectMatchesFilter(InvalidationsStreamSubjects, "kodama.v1.tenant-a.project-a.logs.execution-1") {
		t.Fatal("the invalidations filter captured a log subject")
	}
}

// validateStreamSubjectFilter checks the exact NATS stream-filter grammar used
// by the two contract streams: a five-token scope-qualified subject with one
// single-token wildcard for the tenant and one for the project.
func validateStreamSubjectFilter(filter string) error {
	tokens := strings.Split(filter, ".")
	if len(tokens) != 5 {
		return fmt.Errorf("has %d tokens; want 5", len(tokens))
	}
	for _, token := range tokens {
		if token == "" || strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("has an empty or whitespace token")
		}
	}
	if tokens[0] != "kodama" || tokens[1] != "v1" {
		return fmt.Errorf("does not use the kodama.v1 prefix")
	}
	if tokens[2] != "*" || tokens[3] != "*" {
		return fmt.Errorf("does not wildcard exactly the tenant and project tokens")
	}
	switch tokens[4] {
	case "events", "invalidations":
		return nil
	default:
		return fmt.Errorf("has unknown kind %q", tokens[4])
	}
}

func subjectMatchesFilter(filter, subject string) bool {
	filterTokens := strings.Split(filter, ".")
	subjectTokens := strings.Split(subject, ".")
	if len(filterTokens) != len(subjectTokens) {
		return false
	}
	for index, token := range filterTokens {
		switch token {
		case "*":
			continue
		case ">":
			return true
		default:
			if token != subjectTokens[index] {
				return false
			}
		}
	}
	return true
}
