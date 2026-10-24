// Package postgres is the PostgreSQL adapter of the durable event bus: the
// transaction-local append, observation history, outbox claims, inbox
// processing, quarantine, replay and retention, over the tenant transaction
// boundary of internal/storage.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries an explicit tenant predicate, and is further constrained by
// forced row level security. The package depends on pgx; the entities and ports
// it implements live in internal/events and know nothing of pgx.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
	storage.Querier
}

// TxHandler is the adapter-level local effect of a consumer. It runs inside the
// consumer's tenant transaction, in a savepoint: an error rolls back every
// effect it wrote while the inbox outcome (retry, quarantine) is still recorded.
// It must not make network calls and must not commit or roll back tx; external
// effects are represented by durable intents owned by the effect's module. Keep
// pgx confined to this callback: it should delegate to a domain-oriented port.
//
// Errors are classified with events.Transient / events.Permanent (unclassified
// errors are retried as transient until the attempt limit). Error text is
// stored, bounded, in the inbox history: never put payload data or secrets in it.
type TxHandler func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error

// Binding attaches a local effect to a consumer identity.
type Binding struct {
	Spec   events.ConsumerSpec
	Handle TxHandler
}

// Options configures a Store. Every policy is explicit; none has a default.
type Options struct {
	Backlog   events.BacklogLimits
	Retry     events.RetryPolicy
	Retention events.RetentionPolicy
	// ReplayLease is how long a replay runner owns a request before another
	// runner may resume it.
	ReplayLease time.Duration
	// ReplayBatch bounds the events handled per replay progress step.
	ReplayBatch int
	// Authorizer decides replay and fence operations; nil refuses them all.
	Authorizer events.ReplayAuthorizer
	Clock      func() time.Time
	Logger     *slog.Logger
}

// Validate checks every policy.
func (o Options) Validate() error {
	if err := o.Backlog.Validate(); err != nil {
		return err
	}
	if err := o.Retry.Validate(); err != nil {
		return err
	}
	if err := o.Retention.Validate(); err != nil {
		return err
	}
	if o.ReplayLease <= 0 || o.ReplayBatch < 1 {
		return fmt.Errorf("%w: replay lease and batch must be positive", events.ErrInvalidConfiguration)
	}
	return nil
}

// Store implements events.PublicationStore, events.StatsReader,
// events.Processor, events.TenantSource and events.SubjectResolver over
// PostgreSQL, plus the append, observation, replay and retention operations.
type Store struct {
	db        DB
	opts      Options
	consumers map[string]Binding
}

// NewStore validates the options and consumer bindings.
func NewStore(db DB, opts Options, bindings ...Binding) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: a database is required", events.ErrInvalidConfiguration)
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Store{db: db, opts: opts, consumers: map[string]Binding{}}
	for _, b := range bindings {
		if err := b.Spec.Validate(); err != nil {
			return nil, err
		}
		if b.Handle == nil {
			return nil, fmt.Errorf("%w: consumer %s has no handler", events.ErrInvalidConfiguration, b.Spec.Name)
		}
		if _, dup := s.consumers[b.Spec.Name]; dup {
			return nil, fmt.Errorf("%w: consumer %s registered twice", events.ErrInvalidConfiguration, b.Spec.Name)
		}
		s.consumers[b.Spec.Name] = b
	}
	return s, nil
}

func (s *Store) now() time.Time { return s.opts.Clock().UTC() }

// TenantIDs enumerates tenants through the merged storage discovery function.
func (s *Store) TenantIDs(ctx context.Context) ([]domain.TenantID, error) {
	return storage.ListTenantIDs(ctx, s.db)
}

func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(ctx, s.db, tenant, fn)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
