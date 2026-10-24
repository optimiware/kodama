// Package storage is Kodama's shared PostgreSQL infrastructure: a pgx
// connection pool, the tenant transaction boundary, narrow non-tenant read
// paths, and an ordered embedded-migration runner with the base schema.
//
// It is an adapter-side package. Domain entities stay independent of pgx, and
// entity-specific persistence ports and queries belong to the modules that
// consume them; nothing here is a generic repository.
//
// # Exported API
//
// Pool:
//
//	type Config struct{ URL string; MaxConns, MinConns int32; ... }
//	func Open(ctx context.Context, cfg Config) (*Pool, error)
//	type Pool struct{ *pgxpool.Pool }
//
// Tenant transactions:
//
//	func WithTenant(ctx, db TxBeginner, tenantID domain.TenantID,
//	    fn func(ctx context.Context, tx pgx.Tx) error) error
//	func WithTenantOptions(ctx, db, tenantID, pgx.TxOptions, fn) error
//
// WithTenant validates the identifier, begins a transaction and sets the
// transaction-local setting kodama.tenant_id with set_config(..., true).
// Row level security policies compare tenant_id with kodama_tenant_id(),
// which is NULL when the setting is absent, so statements outside WithTenant
// see and write nothing. Commit, rollback, panic, error and context
// cancellation all end the transaction, and PostgreSQL discards the setting
// with it: nothing tenant specific survives on the pooled connection. fn must
// not commit or roll back, and must not make network calls.
//
// Non-tenant paths (both backed by SECURITY DEFINER functions):
//
//	func DiscoverMemberships(ctx, q Querier, issuer, subject string) ([]Membership, error)
//	func ListTenantIDs(ctx, q Querier) ([]domain.TenantID, error)
//
// Migrations:
//
//	type Migration struct{ ID, SQL string }
//	type Contribution struct{ Module string; Migrations []Migration }
//	func LoadFS(fsys fs.FS, dir string) ([]Migration, error)
//	func NewManifest(contributions ...Contribution) (Manifest, error)
//	func BaseContribution() Contribution
//	func Migrate(ctx, db Acquirer, m Manifest, opts MigrateOptions) (MigrateResult, error)
//
// See README.md for how modules contribute migrations and for the checks.
package storage
