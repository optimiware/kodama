// Package eventstest holds the test fixtures of the event bus: an isolated,
// migrated PostgreSQL database with a runtime-role pool, a NATS JetStream
// broker with per-test streams and subjects, a fault-injecting TCP proxy,
// event builders and a controllable clock.
//
// PostgreSQL follows test/support/postgres (KODAMA_TEST_POSTGRES_ADMIN_URL,
// KODAMA_REQUIRE_POSTGRES=1 fails instead of skipping). The broker uses the
// existing endpoint and token contracts: KODAMA_NATS_URL, with the token either
// embedded as the URL's user-info or read from the `nats-token` secret file
// (KODAMA_NATS_URL's directory contract: /secrets/nats-token). The same
// KODAMA_REQUIRE_POSTGRES=1 switch makes a missing broker a failure. Streams,
// subjects and durable names carry the task resource prefix t005 and a random
// suffix, and are deleted on cleanup. No new environment names are introduced.
package eventstest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/jetstream"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Prefix is the resource prefix of this task's test resources.
const Prefix = "t005"

// RandomHex returns n random bytes in hex.
func RandomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Env is one migrated database with owner and runtime pools.
type Env struct {
	DB        *pgtest.Database
	Migration *pgxpool.Pool
	Runtime   *pgxpool.Pool
}

// NewDB creates an isolated database, applies the base and event migrations as
// the owner role and returns a runtime-role pool. It skips (or fails with
// KODAMA_REQUIRE_POSTGRES=1) when PostgreSQL is not configured.
func NewDB(t testing.TB, runtimeConns int32) *Env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &Env{DB: db, Migration: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, runtimeConns)}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.Migration, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return e
}

// SeedTenant inserts a tenant and projects as the owner role (the runtime role
// cannot provision tenants).
func (e *Env) SeedTenant(t testing.TB, tenant string, projects ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Migration.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, tenant); err != nil {
		t.Fatalf("seed tenant %s: %v", tenant, err)
	}
	for _, p := range projects {
		if _, err := e.Migration.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, tenant, p); err != nil {
			t.Fatalf("seed project %s: %v", p, err)
		}
	}
}

// DefaultOptions are short, explicit test policies (not production values).
func DefaultOptions() postgres.Options {
	return postgres.Options{
		Backlog: events.BacklogLimits{MaxEvents: 1000, MaxBytes: 1 << 20},
		Retry:   events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3},
		Retention: events.RetentionPolicy{
			PublishedOutbox: time.Hour, Observation: 48 * time.Hour, Inbox: 48 * time.Hour,
			ResolvedQuarantine: time.Hour, CompletedReplay: time.Hour,
			BrokerRedelivery: 24 * time.Hour, ReplayWindow: 24 * time.Hour, MaxClockSkew: time.Minute, BatchSize: 100,
		},
		ReplayLease: time.Minute,
		ReplayBatch: 3,
	}
}

// NewStore builds a Store over the runtime pool.
func (e *Env) NewStore(t testing.TB, opts postgres.Options, bindings ...postgres.Binding) *postgres.Store {
	t.Helper()
	s, err := postgres.NewStore(e.Runtime, opts, bindings...)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// Operators authorizes the listed operator IDs and refuses everyone else.
type Operators []string

// AuthorizeReplay implements events.ReplayAuthorizer.
func (o Operators) AuthorizeReplay(_ context.Context, op events.Operator, _ events.Scope, _ string) error {
	for _, id := range o {
		if id == op.ID {
			return nil
		}
	}
	return errors.New("operator is not authorized")
}

// Clock is a controllable clock: real time plus an adjustable offset, so rows
// stamped with the database clock and Go-side decisions stay comparable.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

// Now returns the adjusted time in UTC.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset)
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// fixedTime makes built events deterministic, so repeating a build yields the
// identical event a producer would re-append.
var fixedTime = time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC)

// WorkItemTransition builds a valid work_item.transitioned event.
func WorkItemTransition(tenant, project, eventID, workItem string, version int64) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_" + eventID})
	return eventsv1.Envelope{
		ID: eventID, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeWorkItemTransitioned,
		OccurredAt: fixedTime,
		TenantID:   domain.TenantID(tenant), ProjectID: domain.ProjectID(project),
		Resource:      eventsv1.Resource{Type: "work_item", ID: workItem, Version: version},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_engineer"},
		CorrelationID: "req_" + eventID, Data: data,
	}
}

// MembershipChange builds a valid tenant-wide membership.changed event.
func MembershipChange(tenant, eventID string, version int64) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.MembershipChanged{UserID: "usr_1", BindingIDs: []string{"mem_1"}, EffectiveVersion: version})
	return eventsv1.Envelope{
		ID: eventID, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeMembershipChanged,
		OccurredAt: fixedTime, TenantID: domain.TenantID(tenant),
		Resource: eventsv1.Resource{Type: "membership", ID: "mem_1", Version: version},
		Actor:    eventsv1.Actor{Type: eventsv1.ActorOperator, ID: "ops_1"}, CorrelationID: "req_" + eventID, Data: data,
	}
}

// Eventually polls cond until it holds or the timeout elapses.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Broker is the shared NATS server under test.
type Broker struct {
	addr  string // host:port
	url   *url.URL
	token string
}

// NewBroker locates the broker from KODAMA_NATS_URL, skipping the test when it
// is unset unless KODAMA_REQUIRE_POSTGRES=1 makes the dependency mandatory.
func NewBroker(t testing.TB) *Broker {
	t.Helper()
	layer.SkipService(t, "NATS")
	raw := os.Getenv("KODAMA_NATS_URL")
	if raw == "" {
		if os.Getenv(pgtest.RequireEnv) == "1" {
			t.Fatalf("KODAMA_NATS_URL is unset but %s=1: NATS is required", pgtest.RequireEnv)
		}
		t.Skip("NATS integration test skipped: set KODAMA_NATS_URL (token as URL user-info or in /secrets/nats-token)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("KODAMA_NATS_URL is not a URL: %v", err)
	}
	b := &Broker{addr: u.Host, url: u}
	if u.User != nil {
		b.token = u.User.Username()
	} else if data, err := os.ReadFile("/secrets/nats-token"); err == nil {
		b.token = strings.TrimSpace(string(data))
	}
	return b
}

// Addr is the broker's host:port.
func (b *Broker) Addr() string { return b.addr }

// Connect opens a connection to addr (the broker or a proxy of it). Reconnects
// are enabled so outage tests exercise recovery. Closed on cleanup.
func (b *Broker) Connect(t testing.TB, addr string, opts ...nats.Option) *nats.Conn {
	t.Helper()
	all := []nats.Option{nats.Timeout(5 * time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(100 * time.Millisecond)}
	if b.token != "" {
		all = append(all, nats.Token(b.token))
	}
	all = append(all, opts...)
	nc, err := nats.Connect("nats://"+addr, all...)
	if err != nil {
		t.Fatalf("connect to NATS %s: %v", addr, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// StreamConfig returns a unique, bounded stream configuration (namespace,
// stream and subjects carry the t005 prefix and a random suffix) and deletes the
// stream on cleanup.
func (b *Broker) StreamConfig(t testing.TB, duplicateWindow time.Duration) jetstream.Config {
	t.Helper()
	suffix := RandomHex(t, 4)
	cfg := jetstream.Config{
		Namespace: Prefix + suffix, Stream: Prefix + "_" + suffix,
		MaxAge: time.Hour, MaxBytes: 64 << 20, MaxMsgs: 100000, DuplicateWindow: duplicateWindow, Replicas: 1,
	}
	t.Cleanup(func() {
		nc, err := nats.Connect("nats://"+b.addr, nats.Timeout(5*time.Second), nats.Token(b.token))
		if err != nil {
			return
		}
		defer nc.Close()
		js, err := natsjs.New(nc)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = js.DeleteStream(ctx, cfg.Stream)
	})
	return cfg
}

// Proxy is a TCP proxy to the broker that can sever and restore connectivity
// without stopping the shared NATS service.
type Proxy struct {
	t      testing.TB
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
}

// NewProxy starts a proxy in front of target and stops it on cleanup.
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	p := &Proxy{t: t, target: target, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.Stop)
	return p
}

// Addr is the proxy's host:port.
func (p *Proxy) Addr() string { return p.addr }

func (p *Proxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.DialTimeout("tcp", p.target, 2*time.Second)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns[c], p.conns[up] = struct{}{}, struct{}{}
			p.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 {
						if _, werr := dst.Write(buf[:n]); werr != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
				_ = dst.Close()
				_ = src.Close()
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
}

// Stop severs every connection and refuses new ones: a broker outage.
func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// Start accepts connections again on the original address.
func (p *Proxy) Start() {
	p.t.Helper()
	p.mu.Lock()
	running := p.ln != nil
	p.mu.Unlock()
	if running {
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			p.serve(ln)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.t.Fatalf("restart proxy: %v", err)
}
