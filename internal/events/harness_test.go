package events_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

const tenantA, projectA = "t_acme", "p_inventory"

// bus is one real PostgreSQL database plus the real NATS broker with a fresh
// stream, wired through the production adapters.
type bus struct {
	t      testing.TB
	env    *eventstest.Env
	broker *eventstest.Broker
	cfg    jetstream.Config
	store  *postgres.Store
	clock  *eventstest.Clock
}

type busOptions struct {
	mutate          func(*postgres.Options)
	duplicateWindow time.Duration
	bindings        []postgres.Binding
}

func newBus(t testing.TB, o busOptions) *bus {
	t.Helper()
	env := eventstest.NewDB(t, 20)
	broker := eventstest.NewBroker(t)
	env.SeedTenant(t, tenantA, projectA)
	window := o.duplicateWindow
	if window == 0 {
		window = time.Minute
	}
	b := &bus{t: t, env: env, broker: broker, cfg: broker.StreamConfig(t, window), clock: &eventstest.Clock{}}
	opts := eventstest.DefaultOptions()
	opts.Clock = b.clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	if o.mutate != nil {
		o.mutate(&opts)
	}
	b.store = env.NewStore(t, opts, o.bindings...)
	b.ownerExec(`CREATE TABLE t005_effects (
		tenant_id kodama_id NOT NULL, consumer text NOT NULL, event_id text NOT NULL, resource_id text, version bigint, at timestamptz NOT NULL DEFAULT now())`)
	b.tenantTable("t005_effects")
	return b
}

// ownerExec runs DDL/DML as the migration (owner) role.
func (b *bus) ownerExec(sql string, args ...any) {
	b.t.Helper()
	if _, err := b.env.Migration.Exec(context.Background(), sql, args...); err != nil {
		b.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// tenantTable applies the module conventions (forced RLS on kodama_tenant_id,
// minimal grants) to a business table created by a test.
func (b *bus) tenantTable(name string) {
	b.t.Helper()
	b.ownerExec(fmt.Sprintf(`ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY; ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_all ON %[1]s USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
		GRANT SELECT, INSERT, UPDATE ON %[1]s TO %[2]s`, name, pgxIdent(b.env.DB.RuntimeRole)))
}

func pgxIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// append commits one event in its own tenant transaction.
func (b *bus) append(env eventsv1.Envelope) postgres.AppendResult {
	b.t.Helper()
	var res postgres.AppendResult
	err := storage.WithTenant(context.Background(), b.env.Runtime, env.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		res, err = b.store.Append(ctx, tx, env)
		return err
	})
	if err != nil {
		b.t.Fatalf("append %s: %v", env.ID, err)
	}
	return res
}

// count runs a scalar query as the runtime role inside tenant A's transaction,
// so row level security (which also binds the table owner) applies.
func (b *bus) count(sql string, args ...any) int { return b.countFor(tenantA, sql, args...) }

func (b *bus) countFor(tenant string, sql string, args ...any) int {
	b.t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), b.env.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		b.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

func (b *bus) effects(consumer, eventID string) int {
	return b.count(`SELECT count(*) FROM t005_effects WHERE consumer = $1 AND event_id = $2`, consumer, eventID)
}

// ledger is a historical local-effect consumer: it writes one effect row per
// processed event in the consumer's transaction.
func ledger(name string) postgres.Binding {
	return postgres.Binding{
		Spec: events.ConsumerSpec{Name: name, Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			_, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id, resource_id, version) VALUES ($1, $2, $3, $4, $5)`,
				string(env.TenantID), name, env.ID, env.Resource.ID, env.Resource.Version)
			return err
		},
	}
}

func (b *bus) conn(addr string) *nats.Conn { return b.broker.Connect(b.t, addr) }

func (b *bus) jsPublisher(addr string) *jetstream.Publisher {
	b.t.Helper()
	p, err := jetstream.NewPublisher(b.conn(addr), b.cfg)
	if err != nil {
		b.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.EnsureStream(ctx); err != nil {
		b.t.Fatalf("EnsureStream: %v", err)
	}
	return p
}

type publisherOptions struct {
	clock   func() time.Time
	store   events.PublicationStore
	timeout time.Duration
}

func (b *bus) publisher(broker events.BrokerPublisher, o publisherOptions) *events.Publisher {
	b.t.Helper()
	cfg := events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: 2 * time.Second, BatchSize: 50, Interval: 50 * time.Millisecond,
		Retry: events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3},
		Clock: b.clock.Now,
	}
	if o.clock != nil {
		cfg.Clock = o.clock
	}
	if o.timeout > 0 {
		cfg.PublishTimeout = o.timeout
		cfg.Lease = o.timeout + time.Minute
	}
	var store events.PublicationStore = b.store
	if o.store != nil {
		store = o.store
	}
	p, err := events.NewPublisher(b.store, store, broker, cfg)
	if err != nil {
		b.t.Fatal(err)
	}
	return p
}

func (b *bus) consumer(addr string, proc events.Processor, consumer string) *jetstream.Consumer {
	b.t.Helper()
	if proc == nil {
		proc = b.store
	}
	c, err := jetstream.NewConsumer(b.conn(addr), b.cfg, jetstream.ConsumerConfig{
		Consumer: consumer, Durable: "t005_" + consumer + "_" + eventstest.RandomHex(b.t, 3),
		AckWait: 5 * time.Second, MaxAckPending: 50, FetchBatch: 10, FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond,
	}, b.store, proc, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Provision(ctx); err != nil {
		b.t.Fatalf("Provision: %v", err)
	}
	return c
}

// pump runs the consumer until cond holds or the timeout elapses.
func pump(t testing.TB, c *jetstream.Consumer, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.RunOnce(ctx)
		cancel()
	}
}

// drainPublisher runs the publisher until no tenant has unpublished events.
func (b *bus) drainPublisher(p *events.Publisher, timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, _ = p.RunOnce(context.Background())
		if b.count(`SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0 {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("outbox not drained within %s", timeout)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// rebuildWithConsumer returns a store over the same database with the given
// consumer bindings, keeping the bus's options.
func rebuildWithConsumer(t testing.TB, b *bus, bindings ...postgres.Binding) *postgres.Store {
	t.Helper()
	opts := eventstest.DefaultOptions()
	opts.Clock = b.clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	return b.env.NewStore(t, opts, bindings...)
}

func workItemEvent(id, workItem string, version int64) eventsv1.Envelope {
	return eventstest.WorkItemTransition(tenantA, projectA, id, workItem, version)
}

func jsConn(t testing.TB, b *bus) (natsjs.JetStream, error) {
	t.Helper()
	return natsjs.New(b.conn(b.broker.Addr()))
}

// append2 appends and returns the envelope, for fluent test setup.
func (b *bus) append2(env eventsv1.Envelope) eventsv1.Envelope {
	b.t.Helper()
	b.append(env)
	return env
}

func storageWithTenant(b *bus, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), b.env.Runtime, tenantA, fn)
}
