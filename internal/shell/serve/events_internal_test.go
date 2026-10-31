package serve

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/outbox"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestConnectBusWithoutNATSDeliversInProcessAndSaysSo(t *testing.T) {
	var logs bytes.Buffer
	b, closeBus, err := connectBus(t.Context(), Config{}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBus()
	if _, ok := b.(*bus.Memory); !ok {
		t.Fatalf("bus = %T, want the in-process bus", b)
	}
	if n := strings.Count(logs.String(), "delivered in process"); n != 1 {
		t.Fatalf("the in-process delivery was logged %d times:\n%s", n, logs.String())
	}
}

func TestConnectBusFailsOnAnUnusableNATSURL(t *testing.T) {
	for name, url := range map[string]string{
		"nothing listens": "nats://127.0.0.1:1",
		"wrong scheme":    "http://127.0.0.1:4222",
		"no host":         "nats://",
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		b, closeBus, err := connectBus(ctx, Config{NATSURL: url}, slog.New(slog.DiscardHandler))
		cancel()
		if err == nil {
			closeBus()
			t.Errorf("%s: connected, got %T", name, b)
		}
	}
	if err := ValidateNATSURL(""); err != nil {
		t.Fatal(err)
	}
}

// noJetStreamServer is a reachable NATS server without JetStream: it accepts the
// connection and answers every request with "no responders", as a server that
// does not run JetStream does.
func noJetStreamServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = fmt.Fprint(c, "INFO {\"server_id\":\"x\",\"version\":\"2.10.0\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
				r := bufio.NewReader(c)
				var sid string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(line)
					switch {
					case len(f) >= 3 && f[0] == "SUB":
						sid = f[len(f)-1]
					case len(f) >= 1 && f[0] == "PING":
						_, _ = fmt.Fprint(c, "PONG\r\n")
					case len(f) >= 4 && f[0] == "PUB":
						var n int
						_, _ = fmt.Sscan(f[len(f)-1], &n)
						_, _ = r.Discard(n + 2)
						if len(f) == 4 { // PUB subject reply size
							_, _ = fmt.Fprintf(c, "HMSG %s %s 16 16\r\nNATS/1.0 503\r\n\r\n\r\n", f[2], sid)
						}
					case len(f) >= 3 && f[0] == "HPUB":
						var n int
						_, _ = fmt.Sscan(f[len(f)-1], &n)
						_, _ = r.Discard(n + 2)
						if len(f) == 5 { // HPUB subject reply hdr total
							_, _ = fmt.Fprintf(c, "HMSG %s %s 16 16\r\nNATS/1.0 503\r\n\r\n\r\n", f[2], sid)
						}
					}
				}
			}()
		}
	}()
	return "nats://" + ln.Addr().String()
}

// realJetStream returns a genuinely initialized adapter over the broker named by
// KODAMA_NATS_URL, or skips.
func realJetStream(t *testing.T) (*natsbus.JetStream, *nats.Conn, string) {
	t.Helper()
	url := os.Getenv("KODAMA_NATS_URL")
	if url == "" {
		t.Skip("set KODAMA_NATS_URL to a NATS server with JetStream to run the initialized-adapter test")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	stream := "KODAMA_SERVE_TEST"
	js, err := natsbus.NewJetStream(t.Context(), nc, natsbus.JetStreamConfig{Stream: stream, Subjects: "kodama.v1.servetest.>"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if raw, err := natsjs.New(nc); err == nil {
			_ = raw.DeleteStream(context.Background(), stream)
		}
	})
	return js, nc, stream
}

func TestConnectBusValidatesTheProfileBeforeAnythingElse(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	dev, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development})
	if err != nil {
		t.Fatal(err)
	}
	var typedNilJS *natsbus.JetStream
	var typedNilBroker *natsbus.Broker

	// An unknown environment stops startup whatever is injected or configured.
	for _, env := range []string{"prod", "Production", "staging", " test"} {
		for name, cfg := range map[string]Config{
			"nothing": {Env: env},
			"memory":  {Env: env, Bus: bus.NewMemory()},
			"url":     {Env: env, NATSURL: "nats://127.0.0.1:1"},
			"zero":    {Env: env, Bus: &natsbus.JetStream{}},
		} {
			if b, _, err := connectBus(t.Context(), cfg, quiet); !errors.Is(err, natsbus.ErrProfileUnknown) {
				t.Errorf("env %q with %s = %T, %v; want ErrProfileUnknown", env, name, b, err)
			}
		}
	}

	noJS := noJetStreamServer(t)
	for _, env := range []string{"test", "production"} {
		// Deployed profiles refuse every injected bus that is not a verified,
		// initialized JetStream adapter: the in-process bus, the development
		// broker, a development flagged adapter, an unknown implementation, a nil
		// or typed-nil value and a zero-value adapter or broker.
		for name, injected := range map[string]bus.Bus{
			"memory":             bus.NewMemory(),
			"development broker": dev,
			"flagged adapter":    flaggedBus{bus.NewMemory()},
			"unknown":            plainBus{bus.NewMemory()},
			"empty broker":       &natsbus.Broker{Bus: bus.NewMemory()},
			"zero broker":        &natsbus.Broker{},
			"zero jetstream":     &natsbus.JetStream{},
			"broker over zero":   &natsbus.Broker{Bus: &natsbus.JetStream{}},
			"typed nil jet":      typedNilJS,
			"typed nil broker":   typedNilBroker,
		} {
			b, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet)
			if err == nil || (!errors.Is(err, natsbus.ErrBrokerRequired) && !errors.Is(err, natsbus.ErrBrokerUnavailable)) {
				t.Errorf("%s injected in %s = %T, %v; want a startup failure", name, env, b, err)
			}
		}
		// The normal broker startup failures.
		if b, _, err := connectBus(t.Context(), Config{Env: env}, quiet); !errors.Is(err, natsbus.ErrBrokerRequired) {
			t.Errorf("%s without NATS = %T, %v; want ErrBrokerRequired", env, b, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		b, _, err := connectBus(ctx, Config{Env: env, NATSURL: "nats://127.0.0.1:1"}, quiet)
		cancel()
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
			t.Errorf("%s with an unreachable NATS = %T, %v; want ErrBrokerUnavailable", env, b, err)
		}
		// A reachable server that does not run JetStream is a startup failure.
		ctx, cancel = context.WithTimeout(t.Context(), 20*time.Second)
		b, closeBus, err := connectBus(ctx, Config{Env: env, NATSURL: noJS}, quiet)
		cancel()
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
			if closeBus != nil {
				closeBus()
			}
			t.Errorf("%s with a server without JetStream = %T, %v; want ErrBrokerUnavailable", env, b, err)
		}
		if _, _, err := connectBus(t.Context(), Config{Env: env, NATSURL: "http://127.0.0.1:4222"}, quiet); err == nil {
			t.Errorf("%s accepted a malformed NATS URL", env)
		}
	}

	// Development accepts an injected bus, including the in-process ones; but a
	// configured server without JetStream is still an error there.
	for name, injected := range map[string]bus.Bus{"memory": bus.NewMemory(), "development broker": dev} {
		for _, env := range []string{"", "development"} {
			if b, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet); err != nil || b != injected {
				t.Errorf("%s injected in development %q = %T, %v", name, env, b, err)
			}
		}
	}
	if _, _, err := connectBus(t.Context(), Config{NATSURL: noJS}, quiet); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Errorf("development with a server without JetStream = %v", err)
	}
}

// With a real broker: a genuinely initialized adapter is accepted in the
// deployed profiles, directly and inside a broker, and stops being accepted once
// its broker or stream is gone.
func TestConnectBusAdmitsOnlyAnInitializedReachableAdapter(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	js, nc, stream := realJetStream(t)
	wrapped := &natsbus.Broker{Bus: js}
	for _, env := range []string{"test", "production"} {
		for name, injected := range map[string]bus.Bus{"jetstream": js, "broker over jetstream": wrapped} {
			if b, _, err := connectBus(t.Context(), Config{Env: env, Bus: injected}, quiet); err != nil || b != injected {
				t.Errorf("%s injected in %s = %T, %v", name, env, b, err)
			}
		}
	}
	raw, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.DeleteStream(t.Context(), stream); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connectBus(t.Context(), Config{Env: "production", Bus: js}, quiet); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("an adapter whose stream is gone was accepted: %v", err)
	}
	nc.Close()
	if _, _, err := connectBus(t.Context(), Config{Env: "test", Bus: wrapped}, quiet); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("an adapter whose connection is closed was accepted: %v", err)
	}
}

type flaggedBus struct{ bus.Bus }

func (flaggedBus) DevelopmentOnly() bool { return true }

type plainBus struct{ bus.Bus }

// recordingStore wraps the memory backend and records the size of every
// reconciliation read.
type recordingStore struct {
	*store.MemoryGit
	mu     sync.Mutex
	limits []int
	sizes  []int
}

func (r *recordingStore) UnpublishedEvents(ctx context.Context, tenant domain.TenantID, cutoff time.Time, after store.OutboxCursor, limit int) ([]store.OutboxEvent, error) {
	out, err := r.MemoryGit.UnpublishedEvents(ctx, tenant, cutoff, after, limit)
	r.mu.Lock()
	r.limits, r.sizes = append(r.limits, limit), append(r.sizes, len(out))
	r.mu.Unlock()
	return out, err
}

type sinkRecorder struct {
	mu      sync.Mutex
	failFor map[string]bool
	ids     []string
}

func (s *sinkRecorder) Publish(_ context.Context, _ string, m bus.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFor[m.ID] {
		return errors.New("broker down")
	}
	s.ids = append(s.ids, m.ID)
	return nil
}

func newRecordingStore(t *testing.T) (*recordingStore, func(name string, at time.Time) string) {
	t.Helper()
	mem := store.NewMemoryGit()
	mem.AddTenant("t_a", "a")
	mem.AddProject("t_a", "p_1", "erp")
	rs := &recordingStore{MemoryGit: mem}
	return rs, func(name string, at time.Time) string {
		mem.SetClock(func() time.Time { return at })
		if _, _, err := mem.CreateRepository(t.Context(), store.Repository{TenantID: "t_a", ProjectID: "p_1", ID: name, Name: name, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		if _, err := mem.ConfirmInitialized(t.Context(), "t_a", name, "main", true); err != nil {
			t.Fatal(err)
		}
		evs := mem.Events()
		return evs[len(evs)-1].ID
	}
}

func reconcilePublisher(t *testing.T, rs *recordingStore, sink *sinkRecorder, now time.Time, batch int) *outbox.Publisher {
	t.Helper()
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: storeOutbox{rs}, Bus: sink, Batch: batch, Grace: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Reconciliation reaches an old deferred event behind a full page of young
// ones, which the by-next-attempt order lists first, in bounded reads, and
// terminates.
func TestReconcileReachesAStaleDeferredEventBehindYoungOnes(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rs, seed := newRecordingStore(t)
	old := seed("old", now.Add(-2*time.Hour))
	if err := rs.PublishFailed(t.Context(), "t_a", old, now.Add(24*time.Hour)); err != nil { // deferred a day out
		t.Fatal(err)
	}
	for i := range 10 {
		seed(fmt.Sprintf("young-%02d", i), now.Add(-time.Second)) // due now, younger than the grace
	}
	// The store's by-next-attempt order lists every young event before the old one.
	if first, _ := rs.PendingEvents(t.Context(), "t_a", now.Add(48*time.Hour), 3); len(first) != 3 || first[0].ID == old {
		t.Fatalf("setup: the young events must precede the deferred one: %+v", first)
	}
	sink := &sinkRecorder{}
	p := reconcilePublisher(t, rs, sink, now, 3)
	done := make(chan struct{})
	var res outbox.Result
	var err error
	go func() { defer close(done); res, err = p.Reconcile(t.Context()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reconciliation did not terminate")
	}
	if err != nil || res.Published != 1 || res.Failed != 0 || len(sink.ids) != 1 || sink.ids[0] != old {
		t.Fatalf("reconcile = %+v, %v, published %v", res, err, sink.ids)
	}
	for _, l := range rs.limits {
		if l != 3 {
			t.Fatalf("a reconciliation read asked for %d rows; pages are bounded by the batch", l)
		}
	}
	for _, n := range rs.sizes {
		if n > 3 {
			t.Fatalf("a reconciliation read returned %d rows", n)
		}
	}
}

// Many stale events across several pages, failed publications and equal
// creation times are each attempted exactly once per pass, in bounded reads; the
// failed ones are reached again by the next pass.
func TestReconcilePagesStaleEventsOnceEachAndRetriesFailures(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rs, seed := newRecordingStore(t)
	var stale []string
	for i := range 8 {
		stale = append(stale, seed(fmt.Sprintf("stale-%d", i), now.Add(-time.Duration(10-i)*time.Hour)))
	}
	same := now.Add(-3 * time.Hour) // five events with one creation time: the id breaks the tie
	for i := range 5 {
		stale = append(stale, seed(fmt.Sprintf("tie-%d", i), same))
	}
	for i := range 6 {
		seed(fmt.Sprintf("fresh-%d", i), now)
	}
	sink := &sinkRecorder{failFor: map[string]bool{stale[1]: true, stale[5]: true, stale[10]: true}}
	p := reconcilePublisher(t, rs, sink, now, 3)
	res, err := p.Reconcile(t.Context())
	if err != nil || res.Published != len(stale)-3 || res.Failed != 3 {
		t.Fatalf("first pass = %+v, %v; published %v", res, err, sink.ids)
	}
	seen := map[string]int{}
	for _, id := range sink.ids {
		seen[id]++
	}
	for _, id := range stale {
		if want := map[bool]int{true: 0, false: 1}[sink.failFor[id]]; seen[id] != want {
			t.Fatalf("%s published %d times in the first pass, want %d", id, seen[id], want)
		}
	}
	for _, l := range rs.limits {
		if l != 3 {
			t.Fatalf("a read asked for %d rows", l)
		}
	}
	// The failed ones are still unpublished and the next pass reaches them.
	sink.mu.Lock()
	sink.failFor = nil
	sink.mu.Unlock()
	res, err = p.Reconcile(t.Context())
	if err != nil || res.Published != 3 || res.Failed != 0 {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
	if res, _ := p.Reconcile(t.Context()); res.Published != 0 {
		t.Fatalf("a third pass republished %+v", res)
	}
}

func TestOutboxCursorRoundTrips(t *testing.T) {
	e := store.OutboxEvent{ID: "evt/with/slashes", CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC)}
	c, err := parseCursor(cursorOf(e))
	if err != nil || !c.CreatedAt.Equal(e.CreatedAt) || c.ID != e.ID || c != e.CursorOf() && !c.CreatedAt.Equal(e.CursorOf().CreatedAt) {
		t.Fatalf("cursor = %+v, %v", c, err)
	}
	if z, err := parseCursor(""); err != nil || z != (store.OutboxCursor{}) {
		t.Fatalf("empty cursor = %+v, %v", z, err)
	}
	for _, bad := range []string{"x", "abc/def", "/id"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	older := store.OutboxEvent{ID: "z", CreatedAt: e.CreatedAt.Add(-time.Nanosecond)}
	if !(cursorOf(older) < cursorOf(e)) {
		t.Fatal("cursors must sort in (created_at, id) order")
	}
}
