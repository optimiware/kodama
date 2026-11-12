package app

// Guarded first-admin bootstrap.
//
// BootstrapAdmin is the one-shot composition helper that creates the very first
// tenant and its first ADMIN from a verified OIDC access token. It reads one
// token from a file, verifies it through the identity.OIDCVerifier seam,
// requires an enabled tenant-wide human of the configured tenant, mints the
// initial encrypted tenant derivation key and commits the tenant, key,
// ADMIN-only tenant-wide membership, audit row and config.changed publication
// intent as ONE store.Unit.
//
// The tenant's unique insert is the one-shot guard: every existing tenant is
// refused, including one whose last Admin was revoked. Bootstrap is not
// recovery, and a second claim loses with version_conflict and leaves no row,
// audit or event of its own.
//
// It starts no server, dispatcher, runtime or broker.

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// BootstrapAdminOperation is the immutable audit operation of the first-admin
// bootstrap.
const BootstrapAdminOperation = "bootstrap.admin"

// bootstrapAdminMaxTokenBytes bounds the token file read. It is the same bound
// the OIDC verifier applies to a presented access token (the production
// configuration sets oidc.Config.MaxTokenBytes to 1 MiB), so an oversized file
// is refused before, not after, the whole file is in memory.
const bootstrapAdminMaxTokenBytes = 1 << 20

// BootstrapAdminIDs is the identity source the bootstrap allocates from. The
// membership row id is allocated first, then the audit event id and the
// config.changed event id, in that pinned order.
type BootstrapAdminIDs interface {
	NewRowID() (string, error)
	NewEventID() (id.EventID, error)
}

// BootstrapAdminConfig is the explicit bootstrap input. Every field is
// required; the installation and tenant are never inferred from the token.
type BootstrapAdminConfig struct {
	// InstallationID binds the encrypted derivation key to this installation.
	InstallationID string
	// TenantID is the one tenant this bootstrap may create.
	TenantID id.TenantID
	// TenantName is the new tenant's display name.
	TenantName string
	// TokenFile is the path of the single OIDC access token to verify.
	TokenFile string
	// KeyVersion is the envelope key version the derivation key is sealed with.
	KeyVersion secret.KeyVersion
}

// BootstrapAdminDependencies is the explicit adapter set the bootstrap consumes.
type BootstrapAdminDependencies struct {
	Executor store.Executor
	IDs      BootstrapAdminIDs
	Cipher   secret.EnvelopeCipher
	Verifier identity.OIDCVerifier
	Clock    Clock
}

// BootstrapAdminResult is the committed bootstrap outcome. It carries only
// identities, never token, key or credential material.
type BootstrapAdminResult struct {
	TenantID     id.TenantID
	PrincipalID  id.PrincipalID
	MembershipID string
	AuditID      id.EventID
	EventID      id.EventID
}

// BootstrapAdmin is the one-shot guarded first-admin bootstrap.
type BootstrapAdmin struct {
	config   BootstrapAdminConfig
	executor store.Executor
	ids      BootstrapAdminIDs
	cipher   secret.EnvelopeCipher
	verifier identity.OIDCVerifier
	clock    Clock
}

// NewBootstrapAdmin validates the explicit configuration and dependency set.
// An incomplete input is invalid_input before anything is read or written.
func NewBootstrapAdmin(config BootstrapAdminConfig, dependencies BootstrapAdminDependencies) (*BootstrapAdmin, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if err := dependencies.validate(); err != nil {
		return nil, err
	}
	return &BootstrapAdmin{
		config:   config,
		executor: dependencies.Executor,
		ids:      dependencies.IDs,
		cipher:   dependencies.Cipher,
		verifier: dependencies.Verifier,
		clock:    dependencies.Clock,
	}, nil
}

// Run performs the bootstrap once. Every refusal happens before the single
// commit unit, so a failed attempt leaves no tenant, membership, audit or event
// behind. A public error preserves its fault class and never renders the token,
// a KMS cause or key material.
func (bootstrap *BootstrapAdmin) Run(ctx context.Context) (BootstrapAdminResult, error) {
	if bootstrap == nil {
		return BootstrapAdminResult{}, fault.New(fault.InternalFailure, "bootstrap admin is not configured")
	}
	if err := ctx.Err(); err != nil {
		return BootstrapAdminResult{}, err
	}
	tokenValue, err := readBootstrapAdminToken(ctx, bootstrap.config.TokenFile)
	if err != nil {
		return BootstrapAdminResult{}, err
	}
	now := bootstrap.clock.Now()
	principal, err := bootstrap.verifier.Verify(ctx, identity.OIDCRequest{AccessToken: tokenValue, Now: now})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return BootstrapAdminResult{}, contextErr
		}
		// The verifier's private cause stays reachable through errors.Is but is
		// never rendered, so provider diagnostics cannot leak the token.
		return BootstrapAdminResult{}, fault.Wrap(fault.Unauthenticated, "bootstrap access token was not verified", err)
	}
	if err := ctx.Err(); err != nil {
		return BootstrapAdminResult{}, err
	}
	if err := bootstrap.admit(principal); err != nil {
		return BootstrapAdminResult{}, err
	}
	key, err := bootstrap.newTenantKey(ctx)
	if err != nil {
		return BootstrapAdminResult{}, err
	}
	unit, result, err := buildBootstrapAdminUnit(bootstrap.config, principal.ID, key, now, bootstrap.ids)
	if err != nil {
		return BootstrapAdminResult{}, err
	}
	scope := tenancy.Scope{TenantID: bootstrap.config.TenantID}
	if _, err := bootstrap.executor.Execute(ctx, scope, unit); err != nil {
		// The tenant's unique insert is the guard: an existing tenant is a
		// version_conflict from the store and its class is preserved unchanged.
		return BootstrapAdminResult{}, err
	}
	return result, nil
}

// admit requires the verified principal to be an enabled tenant-wide human of
// the configured tenant. A disabled identity is unauthenticated; a service,
// project-scoped or foreign-tenant identity is forbidden.
func (bootstrap *BootstrapAdmin) admit(principal identity.Principal) error {
	if !principal.Enabled {
		return fault.New(fault.Unauthenticated, "bootstrap principal is not enabled")
	}
	if principal.Kind != identity.KindHuman {
		return fault.New(fault.Forbidden, "bootstrap requires a human principal")
	}
	if principal.Scope.TenantID != bootstrap.config.TenantID {
		return fault.New(fault.Forbidden, "bootstrap principal belongs to another tenant")
	}
	if principal.Scope.ProjectID != "" {
		return fault.New(fault.Forbidden, "bootstrap requires a tenant-wide principal")
	}
	if _, err := id.ParsePrincipalID(string(principal.ID)); err != nil {
		return fault.Wrap(fault.Unauthenticated, "bootstrap principal id is invalid", err)
	}
	return nil
}

// newTenantKey mints the initial encrypted derivation key through the envelope
// cipher, bound to the installation and tenant. It never returns plaintext.
func (bootstrap *BootstrapAdmin) newTenantKey(ctx context.Context) (token.TenantKey, error) {
	source := tenantKeySource{
		cipher:     bootstrap.cipher,
		keyVersion: bootstrap.config.KeyVersion,
		associated: tenantDerivationAssociated(bootstrap.config.InstallationID, bootstrap.config.KeyVersion),
	}
	return source.NewTenantKey(ctx, bootstrap.config.TenantID, 1)
}

// readBootstrapAdminToken reads the one access token, requiring a regular file
// no larger than the verifier's token bound, and trims the surrounding file
// whitespace. A FIFO, directory or device is invalid_input even when it would
// never yield a token, so a mounted path cannot block the helper or exhaust its
// memory. Symlinks are followed: a Kubernetes Secret volume mounts the token as
// a relative symlink chain. An unreadable, oversized or empty file is
// invalid_input; the token value and the file content never appear in an error.
func readBootstrapAdminToken(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Stat before open: os.Stat follows symlinks and refuses a FIFO, directory
	// or device without the blocking open a FIFO would perform.
	info, err := os.Stat(path)
	if err != nil {
		return "", fault.Wrap(fault.InvalidInput, "bootstrap access token file could not be read", err)
	}
	if !info.Mode().IsRegular() {
		return "", fault.New(fault.InvalidInput, "bootstrap access token path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fault.Wrap(fault.InvalidInput, "bootstrap access token file could not be read", err)
	}
	defer file.Close()
	// Re-check the opened descriptor so the regular-file rule holds for the file
	// actually read, not only for the path inspected a moment earlier.
	opened, err := file.Stat()
	if err != nil {
		return "", fault.Wrap(fault.InvalidInput, "bootstrap access token file could not be inspected", err)
	}
	if !opened.Mode().IsRegular() {
		return "", fault.New(fault.InvalidInput, "bootstrap access token path is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, bootstrapAdminMaxTokenBytes+1))
	if err != nil {
		return "", fault.Wrap(fault.InvalidInput, "bootstrap access token file could not be read", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(raw) > bootstrapAdminMaxTokenBytes {
		return "", fault.New(fault.InvalidInput, "bootstrap access token file exceeds the token bound")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fault.New(fault.InvalidInput, "bootstrap access token file is empty")
	}
	return value, nil
}

// validate checks the explicit bootstrap configuration.
func (config BootstrapAdminConfig) validate() error {
	if config.InstallationID == "" {
		return fault.New(fault.InvalidInput, "bootstrap installation id is empty")
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "bootstrap tenant id is invalid", err)
	}
	if config.TenantName == "" {
		return fault.New(fault.InvalidInput, "bootstrap tenant name is empty")
	}
	if config.TokenFile == "" {
		return fault.New(fault.InvalidInput, "bootstrap token file is empty")
	}
	if config.KeyVersion == 0 {
		return fault.New(fault.InvalidInput, "bootstrap key version is zero")
	}
	return nil
}

// validate checks the explicit bootstrap dependency set.
func (dependencies BootstrapAdminDependencies) validate() error {
	if dependencies.Executor == nil {
		return fault.New(fault.InvalidInput, "bootstrap executor is missing")
	}
	if dependencies.IDs == nil {
		return fault.New(fault.InvalidInput, "bootstrap id source is missing")
	}
	if dependencies.Cipher == nil {
		return fault.New(fault.InvalidInput, "bootstrap envelope cipher is missing")
	}
	if dependencies.Verifier == nil {
		return fault.New(fault.InvalidInput, "bootstrap OIDC verifier is missing")
	}
	if dependencies.Clock == nil {
		return fault.New(fault.InvalidInput, "bootstrap clock is missing")
	}
	return nil
}
