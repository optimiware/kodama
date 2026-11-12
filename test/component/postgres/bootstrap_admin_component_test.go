//go:build component

package postgrescomponent

// Real PostgreSQL proof of the guarded first-admin bootstrap's atomic unit.
//
// The bootstrap is composed exactly as in the unit tests, but against the real
// scoped PostgreSQL store. The cryptography is explicitly injected TEST
// cryptography (an ephemeral key provider and envelope cipher); this test never
// constructs or claims a production KMS. The OIDC verifier is a deterministic
// fake because the verifier adapter has its own conformance suite; the
// composition consumes the same identity.OIDCVerifier seam.
//
// The rollback case forces the final event insert to collide with a committed
// event id in another tenant. The tenant, membership and audit inserts have
// already executed inside the same transaction, so a rollback proves the whole
// unit is atomic. The success case proves every row and the encrypted
// derivation key land together, and a second claim for the same tenant is
// refused with no new rows.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

type bootstrapComponentIDs struct {
	mu     sync.Mutex
	row    string
	events []id.EventID
	next   int
}

func (ids *bootstrapComponentIDs) NewRowID() (string, error) {
	if ids.row == "" {
		return "", errors.New("bootstrap component id source has no row id")
	}
	return ids.row, nil
}

func (ids *bootstrapComponentIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.next >= len(ids.events) {
		return "", errors.New("bootstrap component id source has no event id")
	}
	value := ids.events[ids.next]
	ids.next++
	return value, nil
}

type bootstrapComponentVerifier struct {
	principal identity.Principal
	err       error
}

func (verifier bootstrapComponentVerifier) Verify(ctx context.Context, _ identity.OIDCRequest) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, err
	}
	return verifier.principal, verifier.err
}

type bootstrapComponentClock struct {
	now time.Time
}

func (clock bootstrapComponentClock) Now() time.Time { return clock.now }

func (clock bootstrapComponentClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func bootstrapComponentPrincipal(t *testing.T, tenant string, subject string) identity.Principal {
	t.Helper()
	principalID, err := oidc.PrincipalIDFor("https://issuer.example", subject)
	if err != nil {
		t.Fatalf("oidc.PrincipalIDFor: %v", err)
	}
	return identity.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID(tenant)},
		Enabled: true,
	}
}

func writeBootstrapComponentToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access-token")
	if err := os.WriteFile(path, []byte("component-bootstrap-token\n"), 0o600); err != nil {
		t.Fatalf("write component token file: %v", err)
	}
	return path
}

func newBootstrapComponent(
	t *testing.T,
	executor store.Executor,
	ids app.BootstrapAdminIDs,
	verifier identity.OIDCVerifier,
	cipher secret.EnvelopeCipher,
	clock app.Clock,
	config app.BootstrapAdminConfig,
) *app.BootstrapAdmin {
	t.Helper()
	bootstrap, err := app.NewBootstrapAdmin(config, app.BootstrapAdminDependencies{
		Executor: executor,
		IDs:      ids,
		Cipher:   cipher,
		Verifier: verifier,
		Clock:    clock,
	})
	if err != nil {
		t.Fatalf("app.NewBootstrapAdmin: %v", err)
	}
	if bootstrap == nil {
		t.Fatal("app.NewBootstrapAdmin returned nil")
	}
	return bootstrap
}

func bootstrapComponentCount(t *testing.T, ctx context.Context, shard *pgtest.Shard, query string, args ...any) int64 {
	t.Helper()
	value, err := pgtest.ScalarInt(ctx, shard.Admin(), query, args...)
	if err != nil {
		t.Fatalf("scalar query %q: %v", query, err)
	}
	return value
}

func bootstrapComponentString(t *testing.T, ctx context.Context, shard *pgtest.Shard, query string, args ...any) string {
	t.Helper()
	value, err := pgtest.ScalarString(ctx, shard.Admin(), query, args...)
	if err != nil {
		t.Fatalf("scalar query %q: %v", query, err)
	}
	return value
}

func TestBootstrapAdminAtomicityOnPostgres(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	runtime := shard.Runtime(4)
	t.Cleanup(runtime.Close)
	scoped := storepg.New(runtime)

	// Explicitly injected TEST cryptography: an ephemeral provider and envelope
	// cipher. This is not a production KMS and the test does not claim one.
	provider, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewEphemeral: %v", err)
	}
	cipher, err := keys.NewCipher(provider, rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewCipher: %v", err)
	}

	clock := bootstrapComponentClock{now: pgtest.FixtureTime}
	const installationID = "installation-component"

	// --- rollback: the last insert collides, so the earlier inserts must roll back ---
	const rollbackTenant = "tenant-bootstrap-rollback"
	const collisionEventID = "event-bootstrap-collision"
	shard.SeedTenant(ctx, "tenant-other", 1)
	shard.SeedEvent(ctx, pgtest.TenantScope("tenant-other"), collisionEventID, "config.changed", "tenant-other", 1)

	rollbackPrincipal := bootstrapComponentPrincipal(t, rollbackTenant, "component-bootstrap-rollback")
	rollbackIDs := &bootstrapComponentIDs{
		row:    "row-bootstrap-rollback",
		events: []id.EventID{"event-bootstrap-audit-rollback", id.EventID(collisionEventID)},
	}
	rollback := newBootstrapComponent(t, scoped, rollbackIDs,
		bootstrapComponentVerifier{principal: rollbackPrincipal}, cipher, clock,
		app.BootstrapAdminConfig{
			InstallationID: installationID,
			TenantID:       id.TenantID(rollbackTenant),
			TenantName:     "Bootstrap Rollback",
			TokenFile:      writeBootstrapComponentToken(t),
			KeyVersion:     secret.MinKeyVersion,
		})
	if _, err := rollback.Run(ctx); err == nil {
		t.Fatal("bootstrap succeeded despite the colliding event identity")
	}
	for _, check := range []struct {
		label string
		query string
	}{
		{"tenant", `SELECT count(*) FROM tenants WHERE id = $1`},
		{"membership", `SELECT count(*) FROM memberships WHERE tenant_id = $1`},
		{"audit", `SELECT count(*) FROM audit WHERE tenant_id = $1`},
		{"event", `SELECT count(*) FROM events WHERE tenant_id = $1`},
	} {
		if got := bootstrapComponentCount(t, ctx, shard, check.query, rollbackTenant); got != 0 {
			t.Fatalf("rollback left %d %s rows; want none", got, check.label)
		}
	}
	if owner := bootstrapComponentString(t, ctx, shard,
		`SELECT tenant_id FROM events WHERE id = $1`, collisionEventID); owner != "tenant-other" {
		t.Fatalf("pre-existing colliding event tenant = %q; want tenant-other", owner)
	}

	// --- success: every row and the encrypted key land together ---
	const successTenant = "tenant-bootstrap-ok"
	successPrincipal := bootstrapComponentPrincipal(t, successTenant, "component-bootstrap-ok")
	successIDs := &bootstrapComponentIDs{
		row:    "row-bootstrap-ok",
		events: []id.EventID{"event-bootstrap-audit-ok", "event-bootstrap-config-ok"},
	}
	success := newBootstrapComponent(t, scoped, successIDs,
		bootstrapComponentVerifier{principal: successPrincipal}, cipher, clock,
		app.BootstrapAdminConfig{
			InstallationID: installationID,
			TenantID:       id.TenantID(successTenant),
			TenantName:     "Bootstrap Success",
			TokenFile:      writeBootstrapComponentToken(t),
			KeyVersion:     secret.MinKeyVersion,
		})
	result, err := success.Run(ctx)
	if err != nil {
		t.Fatalf("successful bootstrap: %v", err)
	}
	if result.TenantID != id.TenantID(successTenant) || result.PrincipalID != successPrincipal.ID ||
		result.MembershipID != "row-bootstrap-ok" ||
		result.AuditID != "event-bootstrap-audit-ok" || result.EventID != "event-bootstrap-config-ok" {
		t.Fatalf("bootstrap result = %+v; want the allocated identities", result)
	}
	for _, check := range []struct {
		label string
		query string
	}{
		{"tenant", `SELECT count(*) FROM tenants WHERE id = $1`},
		{"membership", `SELECT count(*) FROM memberships WHERE tenant_id = $1`},
		{"audit", `SELECT count(*) FROM audit WHERE tenant_id = $1`},
		{"event", `SELECT count(*) FROM events WHERE tenant_id = $1`},
	} {
		if got := bootstrapComponentCount(t, ctx, shard, check.query, successTenant); got != 1 {
			t.Fatalf("successful bootstrap wrote %d %s rows; want exactly 1", got, check.label)
		}
	}

	if name := bootstrapComponentString(t, ctx, shard, `SELECT name FROM tenants WHERE id = $1`, successTenant); name != "Bootstrap Success" {
		t.Fatalf("tenant name = %q; want Bootstrap Success", name)
	}
	if version := bootstrapComponentCount(t, ctx, shard, `SELECT version FROM tenants WHERE id = $1`, successTenant); version != 1 {
		t.Fatalf("tenant version = %d; want 1", version)
	}
	rawKey := bootstrapComponentString(t, ctx, shard, `SELECT derivation_key::text FROM tenants WHERE id = $1`, successTenant)
	var key token.TenantKey
	if err := json.Unmarshal([]byte(rawKey), &key); err != nil {
		t.Fatalf("decode stored derivation key: %v", err)
	}
	if key.TenantID != id.TenantID(successTenant) || key.Version != 1 || key.KeyRef == "" ||
		key.KeyVersion != uint64(secret.MinKeyVersion) {
		t.Fatalf("stored derivation key = %+v; want a version-1 encrypted key", key)
	}
	if len(key.Ciphertext) <= secret.ContentNonceBytes {
		t.Fatalf("stored derivation key ciphertext = %d bytes; want nonce plus ciphertext", len(key.Ciphertext))
	}
	if strings.Contains(rawKey, "component-bootstrap-token") {
		t.Fatalf("stored derivation key leaked the access token: %s", rawKey)
	}
	plaintext, err := cipher.Open(ctx, secret.AssociatedData{
		InstallationID: installationID,
		TenantID:       key.TenantID,
		SecretID:       secret.SecretID("tenant-derivation-key"),
		Name:           id.SecretName("TENANT_DERIVATION_KEY"),
		KeyVersion:     secret.KeyVersion(key.KeyVersion),
	}, secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef(key.KeyRef),
		KeyVersion: secret.KeyVersion(key.KeyVersion),
		Nonce:      append([]byte(nil), key.Ciphertext[:secret.ContentNonceBytes]...),
		Ciphertext: append([]byte(nil), key.Ciphertext[secret.ContentNonceBytes:]...),
		WrappedKey: append([]byte(nil), key.WrappedKey...),
		WrapNonce:  append([]byte(nil), key.WrapNonce...),
	})
	if err != nil {
		t.Fatalf("open stored derivation key: %v", err)
	}
	if len(plaintext) != 32 {
		t.Fatalf("derivation key plaintext = %d bytes; want 32", len(plaintext))
	}

	if principal := bootstrapComponentString(t, ctx, shard,
		`SELECT principal_id FROM memberships WHERE tenant_id = $1`, successTenant); principal != string(successPrincipal.ID) {
		t.Fatalf("membership principal = %q; want %q", principal, successPrincipal.ID)
	}
	rawRoles := bootstrapComponentString(t, ctx, shard,
		`SELECT roles::text FROM memberships WHERE tenant_id = $1`, successTenant)
	var roles []tenancy.Role
	if err := json.Unmarshal([]byte(rawRoles), &roles); err != nil {
		t.Fatalf("decode membership roles: %v", err)
	}
	if len(roles) != 1 || roles[0] != tenancy.RoleAdmin {
		t.Fatalf("membership roles = %v; want exactly [admin]", roles)
	}
	if project := bootstrapComponentString(t, ctx, shard,
		`SELECT coalesce(project_id, '') FROM memberships WHERE tenant_id = $1`, successTenant); project != "" {
		t.Fatalf("membership project_id = %q; want tenant-wide NULL", project)
	}
	if version := bootstrapComponentCount(t, ctx, shard,
		`SELECT version FROM memberships WHERE tenant_id = $1`, successTenant); version != 1 {
		t.Fatalf("membership version = %d; want 1", version)
	}

	if actor := bootstrapComponentString(t, ctx, shard,
		`SELECT actor FROM audit WHERE id = $1`, "event-bootstrap-audit-ok"); actor != string(successPrincipal.ID) {
		t.Fatalf("audit actor = %q; want %q", actor, successPrincipal.ID)
	}
	if operation := bootstrapComponentString(t, ctx, shard,
		`SELECT operation FROM audit WHERE id = $1`, "event-bootstrap-audit-ok"); operation != app.BootstrapAdminOperation {
		t.Fatalf("audit operation = %q; want %q", operation, app.BootstrapAdminOperation)
	}

	if eventType := bootstrapComponentString(t, ctx, shard,
		`SELECT type FROM events WHERE id = $1`, "event-bootstrap-config-ok"); eventType != "config.changed" {
		t.Fatalf("event type = %q; want config.changed", eventType)
	}
	if sourceKind := bootstrapComponentString(t, ctx, shard,
		`SELECT source_kind FROM events WHERE id = $1`, "event-bootstrap-config-ok"); sourceKind != "shell" {
		t.Fatalf("event source kind = %q; want shell", sourceKind)
	}
	if sourcePrincipal := bootstrapComponentString(t, ctx, shard,
		`SELECT source_principal FROM events WHERE id = $1`, "event-bootstrap-config-ok"); sourcePrincipal != string(successPrincipal.ID) {
		t.Fatalf("event source principal = %q; want %q", sourcePrincipal, successPrincipal.ID)
	}
	if subject := bootstrapComponentString(t, ctx, shard,
		`SELECT subject_id FROM events WHERE id = $1`, "event-bootstrap-config-ok"); subject != successTenant {
		t.Fatalf("event subject = %q; want %q", subject, successTenant)
	}
	if version := bootstrapComponentCount(t, ctx, shard,
		`SELECT subject_version FROM events WHERE id = $1`, "event-bootstrap-config-ok"); version != 1 {
		t.Fatalf("event subject version = %d; want 1", version)
	}
	if publishState := bootstrapComponentString(t, ctx, shard,
		`SELECT publish_state FROM events WHERE id = $1`, "event-bootstrap-config-ok"); publishState != "pending" {
		t.Fatalf("event publish state = %q; want pending", publishState)
	}

	// --- one-shot guard: a second claim for the same tenant is refused and
	// leaves every committed row untouched ---
	secondPrincipal := bootstrapComponentPrincipal(t, successTenant, "component-bootstrap-second")
	secondIDs := &bootstrapComponentIDs{
		row:    "row-bootstrap-second",
		events: []id.EventID{"event-bootstrap-audit-second", "event-bootstrap-config-second"},
	}
	second := newBootstrapComponent(t, scoped, secondIDs,
		bootstrapComponentVerifier{principal: secondPrincipal}, cipher, clock,
		app.BootstrapAdminConfig{
			InstallationID: installationID,
			TenantID:       id.TenantID(successTenant),
			TenantName:     "Bootstrap Second",
			TokenFile:      writeBootstrapComponentToken(t),
			KeyVersion:     secret.MinKeyVersion,
		})
	if _, err := second.Run(ctx); err == nil {
		t.Fatal("a second bootstrap succeeded for the same tenant")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("second claim error class = %v (ok=%v); want version_conflict", class, ok)
	}
	if got := bootstrapComponentCount(t, ctx, shard,
		`SELECT count(*) FROM memberships WHERE principal_id = $1`, secondPrincipal.ID); got != 0 {
		t.Fatalf("second claim wrote %d memberships; want none", got)
	}
	for _, check := range []struct {
		label string
		query string
	}{
		{"tenant", `SELECT count(*) FROM tenants WHERE id = $1`},
		{"membership", `SELECT count(*) FROM memberships WHERE tenant_id = $1`},
		{"audit", `SELECT count(*) FROM audit WHERE tenant_id = $1`},
		{"event", `SELECT count(*) FROM events WHERE tenant_id = $1`},
	} {
		if got := bootstrapComponentCount(t, ctx, shard, check.query, successTenant); got != 1 {
			t.Fatalf("second claim changed %s rows to %d; want exactly 1", check.label, got)
		}
	}
}
