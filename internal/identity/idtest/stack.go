// Package idtest is the shared fixture of the identity tests: one isolated,
// migrated PostgreSQL database with the owner and runtime-role pools, the real
// event store, the identity store and service, and the other modules' stores the
// integration tests exercise (work, HTTP idempotency). It is imported only by
// tests. Fixture identifiers carry the task resource prefix t010.
package idtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Prefix is this task's resource prefix.
const Prefix = "t010"

// Stack is one migrated database with identity wired over it.
type Stack struct {
	T       testing.TB
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	Events  *eventspg.Store
	Store   *identitypg.Store
	Service *identity.Service
	Clock   *eventstest.Clock
}

// Options tune the stack.
type Options struct {
	// Conns is the runtime pool size (default 24).
	Conns int32
	// Events mutates the event store options (for example the backlog bound).
	Events func(*eventspg.Options)
}

// Manifest is the squashed baseline: identity, work and idempotency share one
// contribution, so there is nothing to select.
func Manifest() (storage.Manifest, error) {
	return storage.NewManifest(storage.BaseContribution())
}

// New provisions the database, migrations and services.
func New(t testing.TB, o Options) *Stack {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conns := o.Conns
	if conns == 0 {
		conns = 24
	}
	s := &Stack{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, conns), Clock: &eventstest.Clock{}}
	manifest, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, s.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = s.Clock.Now
	if o.Events != nil {
		o.Events(&opts)
	}
	if s.Events, err = eventspg.NewStore(s.Runtime, opts); err != nil {
		t.Fatal(err)
	}
	if s.Store, err = identitypg.NewStore(s.Runtime, s.Events); err != nil {
		t.Fatal(err)
	}
	if s.Service, err = identity.NewService(identity.Config{Repository: s.Store, Directory: s.Store, Clock: s.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	return s
}

// OwnerExec runs SQL as the migration (owner) role; it is how tests seed rows
// the runtime role cannot create.
func (s *Stack) OwnerExec(sql string, args ...any) {
	s.T.Helper()
	if _, err := s.Owner.Exec(context.Background(), sql, args...); err != nil {
		s.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// SeedTenant creates a tenant.
func (s *Stack) SeedTenant(tenant domain.TenantID) {
	s.T.Helper()
	s.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(tenant))
}

// SeedProject creates a project (and its tenant if needed).
func (s *Stack) SeedProject(tenant domain.TenantID, project domain.ProjectID, wip int) {
	s.T.Helper()
	s.SeedTenant(tenant)
	s.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, $3)`, string(tenant), string(project), wip)
}

// User maps a verified principal exactly as a first login does.
func (s *Stack) User(issuer, subject string) identity.UserID {
	s.T.Helper()
	id, err := s.Store.MapPrincipal(context.Background(), identity.Principal{Issuer: issuer, Subject: subject})
	if err != nil {
		s.T.Fatalf("map principal: %v", err)
	}
	return id
}

// Bind seeds a binding directly as owner, including its authorization revision,
// the way an earlier committed grant would have left them. It returns the binding ID.
func (s *Stack) Bind(tenant domain.TenantID, user identity.UserID, project domain.ProjectID, role identity.Role) identity.BindingID {
	s.T.Helper()
	id := identity.BindingID("mb_" + RandomHex(s.T, 8))
	var p *string
	if project != "" {
		v := string(project)
		p = &v
	}
	s.OwnerExec(`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ($1, $2, $3, $4, $5)`, string(tenant), string(id), string(user), p, string(role))
	s.OwnerExec(`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ($1, $2, 1)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET revision = identity_authz_revisions.revision + 1`, string(tenant), string(user))
	return id
}

// Caller builds a service caller.
func Caller(user identity.UserID) identity.Caller {
	return identity.Caller{UserID: user, RequestID: "req_" + hex.EncodeToString([]byte(string(user)))[:16]}
}

// Count runs a scalar count query as owner.
func (s *Stack) Count(sql string, args ...any) int {
	s.T.Helper()
	var n int
	if err := s.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		s.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Revision returns the user's authorization revision (0 when the row is absent).
func (s *Stack) Revision(tenant domain.TenantID, user identity.UserID) int64 {
	s.T.Helper()
	var rev int64
	err := s.Owner.QueryRow(context.Background(), `SELECT revision FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2`, string(tenant), string(user)).Scan(&rev)
	if err != nil && err != pgx.ErrNoRows {
		s.T.Fatal(err)
	}
	return rev
}

// RandomHex returns n random bytes in hex.
func RandomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// CountIn runs a scalar count query as the owner inside the tenant's context, so
// tables with tenant-only policies (outbox, audit) are visible.
func (s *Stack) CountIn(tenant domain.TenantID, sql string, args ...any) int {
	s.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), s.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		s.T.Fatalf("count in %s: %v\n%s", tenant, err, sql)
	}
	return n
}

// Outbox returns the payloads of the tenant's events of the type, oldest first.
func (s *Stack) Outbox(tenant domain.TenantID, eventType string) []string {
	s.T.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), s.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT convert_from(payload, 'UTF8') FROM outbox WHERE tenant_id = $1 AND event_type = $2 ORDER BY created_at, event_id`, string(tenant), eventType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		s.T.Fatal(err)
	}
	return out
}
