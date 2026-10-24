package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TemplatePrefix prefixes every template database and its two roles.
const TemplatePrefix = NamePrefix + "tpl_"

// Migrator applies a schema to the empty database d as d's migration role
// (d.MigrationPool) and grants d.RuntimeRole whatever its migrations grant.
// The roles already exist and public-schema access is set up as in New.
type Migrator func(ctx context.Context, d *Database) error

// ManifestDigest identifies a schema by its ordered migration identities, for
// example each migration's ID and checksum. The digest changes when a
// migration is added, removed, reordered or edited, which retires the old
// template and builds a new one.
func ManifestDigest(entries ...string) string {
	h := sha256.New()
	for _, e := range entries {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(e)))
		h.Write(n[:])
		h.Write([]byte(e))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// template names one migrated database and the roles its objects belong to.
// Clones keep those owners and ACLs, so they share the template's roles; the
// roles carry no privilege outside databases cloned from the template.
type template struct {
	adminURL, name, migRole, rtRole, migPassword, rtPassword string
}

var digestPattern = regexp.MustCompile(`^(sha256:)?[0-9a-f]{16,64}$`)

func newTemplate(adminURL, digest string) (template, error) {
	if !digestPattern.MatchString(digest) {
		return template{}, fmt.Errorf("template digest %q must be 16 to 64 lowercase hex digits (see ManifestDigest)", digest)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		return template{}, fmt.Errorf("parse %s: %w", AdminURLEnv, err)
	}
	digest = strings.TrimPrefix(digest, "sha256:")
	name := TemplatePrefix + digest[:min(len(digest), 32)]
	adminPassword, _ := u.User.Password()
	return template{
		adminURL: adminURL, name: name, migRole: name + "_mig", rtRole: name + "_rt",
		// Derived, so any process that clones the template can connect without
		// sharing state, and unguessable without the administrator credential.
		migPassword: derive(adminPassword, name+"_mig"), rtPassword: derive(adminPassword, name+"_rt"),
	}, nil
}

func derive(secret, label string) string {
	sum := sha256.Sum256([]byte(secret + "\x00" + label))
	return hex.EncodeToString(sum[:16])
}

// NewFromTemplate returns a fresh database cloned from the template migrated
// for digest, so a test pays for a file-level copy instead of a migration. The
// first caller for a digest builds the template with migrate under a
// cluster-wide lock; later callers, in any process, wait for it and clone. The
// caller's contract is that the same digest always means the same schema.
//
// The clone's roles are the template's: MigrationRole owns the database and
// its objects, RuntimeRole is NOSUPERUSER NOBYPASSRLS. t.Cleanup drops the
// clone; the template and roles stay for the next test and are removed with
// DropTemplate or with the scope (make dev-down).
func NewFromTemplate(t testing.TB, digest string, migrate Migrator) *Database {
	t.Helper()
	adminURL := requireAdminURL(t)
	tpl, err := newTemplate(adminURL, digest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// One administrator connection serves the readiness check and the clone.
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as PostgreSQL administrator (%s): %v", AdminURLEnv, err)
	}
	defer admin.Close(context.Background())
	if err := tpl.ensureWith(ctx, t, admin, migrate); err != nil {
		t.Fatalf("template %s: %v", tpl.name, err)
	}
	d := &Database{
		Name:          NamePrefix + randomHex(t, 6),
		MigrationRole: tpl.migRole,
		RuntimeRole:   tpl.rtRole,
		adminURL:      adminURL,
		shared:        true,
		t:             t,
	}
	if d.MigrationURL, err = withIdentity(adminURL, tpl.migRole, tpl.migPassword, d.Name); err != nil {
		t.Fatal(err)
	}
	if d.RuntimeURL, err = withIdentity(adminURL, tpl.rtRole, tpl.rtPassword, d.Name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.drop) // registered first so a partial clone is still removed
	if err := tpl.clone(ctx, admin, d.Name); err != nil {
		t.Fatalf("clone %s: %v", tpl.name, err)
	}
	return d
}

// DropTemplate removes the template for digest and its roles. It fails while
// a clone still exists, because the clone's objects belong to the roles.
func DropTemplate(t testing.TB, digest string) {
	t.Helper()
	adminURL := requireAdminURL(t)
	tpl, err := newTemplate(adminURL, digest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as PostgreSQL administrator (%s): %v", AdminURLEnv, err)
	}
	defer admin.Close(ctx)
	if err := lock(ctx, admin, tpl.name); err != nil {
		t.Fatal(err)
	}
	if err := tpl.drop(ctx, admin); err != nil {
		t.Fatalf("drop template %s: %v", tpl.name, err)
	}
	for _, role := range []string{tpl.rtRole, tpl.migRole} {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+ident(role)); err != nil {
			t.Fatalf("drop role %s (a clone still exists?): %v", role, err)
		}
	}
}

func lock(ctx context.Context, admin *pgx.Conn, name string) error {
	// Session-level: released when the connection closes, even on a crash.
	_, err := admin.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "kodama-test-template:"+name)
	return err
}

// ensure builds the template unless a complete one exists. A template is
// complete exactly when it is marked IS_TEMPLATE, which happens last; an
// interrupted build is dropped and redone.
func (tpl template) ensure(ctx context.Context, t testing.TB, migrate Migrator) error {
	admin, err := pgx.Connect(ctx, tpl.adminURL)
	if err != nil {
		return fmt.Errorf("connect as PostgreSQL administrator (%s): %w", AdminURLEnv, err)
	}
	defer admin.Close(ctx)
	return tpl.ensureWith(ctx, t, admin, migrate)
}

// ready reports whether a complete template exists.
func (tpl template) ready(ctx context.Context, admin *pgx.Conn) (exists, ready bool, err error) {
	err = admin.QueryRow(ctx, "SELECT datistemplate FROM pg_database WHERE datname = $1", tpl.name).Scan(&ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, ready, err
}

func (tpl template) ensureWith(ctx context.Context, t testing.TB, admin *pgx.Conn, migrate Migrator) error {
	// The template never changes once marked, so the common case needs no lock.
	if _, ready, err := tpl.ready(ctx, admin); err != nil || ready {
		return err
	}
	if err := lock(ctx, admin, tpl.name); err != nil {
		return err
	}
	// Another process may have finished while this one waited for the lock.
	switch exists, ready, err := tpl.ready(ctx, admin); {
	case err != nil:
		return err
	case exists && ready:
		return nil
	case exists:
		if err := tpl.drop(ctx, admin); err != nil {
			return fmt.Errorf("remove interrupted build: %w", err)
		}
	}
	if err := tpl.build(ctx, t, admin, migrate); err != nil {
		// Leave nothing half-built for the next caller to mistake for a template.
		if dropErr := tpl.drop(context.WithoutCancel(ctx), admin); dropErr != nil {
			err = errors.Join(err, dropErr)
		}
		return err
	}
	return nil
}

func (tpl template) build(ctx context.Context, t testing.TB, admin *pgx.Conn, migrate Migrator) error {
	for _, role := range []struct{ name, password string }{{tpl.migRole, tpl.migPassword}, {tpl.rtRole, tpl.rtPassword}} {
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role.name).Scan(&exists); err != nil {
			return err
		}
		verb := "CREATE"
		if exists {
			verb = "ALTER"
		}
		stmt := fmt.Sprintf("%s ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %s", verb, ident(role.name), literal(role.password))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s role %s: %w", strings.ToLower(verb), role.name, err)
		}
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", ident(tpl.name), ident(tpl.migRole))); err != nil {
		return err
	}
	inDB, err := pgx.Connect(ctx, mustReplaceDatabase(tpl.adminURL, tpl.name))
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", ident(tpl.rtRole)),
	} {
		if _, err := inDB.Exec(ctx, stmt); err != nil {
			inDB.Close(ctx)
			return fmt.Errorf("setup %q: %w", stmt, err)
		}
	}
	inDB.Close(ctx)

	d := &Database{Name: tpl.name, MigrationRole: tpl.migRole, RuntimeRole: tpl.rtRole, adminURL: tpl.adminURL, shared: true, t: t}
	if d.MigrationURL, err = withIdentity(tpl.adminURL, tpl.migRole, tpl.migPassword, tpl.name); err != nil {
		return err
	}
	if d.RuntimeURL, err = withIdentity(tpl.adminURL, tpl.rtRole, tpl.rtPassword, tpl.name); err != nil {
		return err
	}
	err = migrate(ctx, d)
	d.closePools()
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// CREATE DATABASE ... TEMPLATE needs the source to have no sessions.
	if err := tpl.awaitIdle(ctx, admin); err != nil {
		return err
	}
	_, err = admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS false IS_TEMPLATE true", ident(tpl.name)))
	return err
}

func (tpl template) awaitIdle(ctx context.Context, admin *pgx.Conn) error {
	for {
		var n int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = $1", tpl.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// drop removes the template database whether or not it was completed.
func (tpl template) drop(ctx context.Context, admin *pgx.Conn) error {
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", tpl.name).Scan(&exists); err != nil || !exists {
		return err
	}
	for _, stmt := range []string{
		fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE false ALLOW_CONNECTIONS true", ident(tpl.name)),
		fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", ident(tpl.name)),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// clone creates name from the template. PostgreSQL refuses a clone while
// another session is still attached to the source; that only happens during
// a rebuild, so wait briefly.
func (tpl template) clone(ctx context.Context, admin *pgx.Conn, name string) error {
	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s OWNER %s", ident(name), ident(tpl.name), ident(tpl.migRole))
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := admin.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55006" /* object_in_use */ || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
