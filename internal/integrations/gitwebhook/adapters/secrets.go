package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

// SecretReader is the read surface of the secrets module the hook source needs.
// *secrets/postgres.Store (and secretstest.MemStore) satisfy it.
type SecretReader interface {
	ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error)
	GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error)
	GetScopeKey(ctx context.Context, tenant domain.TenantID, id string) (secrets.ScopeKey, error)
}

// HookSecretsConfig wires HookSecrets.
type HookSecretsConfig struct {
	Reader SecretReader
	// Wrapper unwraps the tenant's scope key; Installation is the identity the
	// values were sealed under. Both must equal the secrets service's.
	Wrapper      secrets.KeyWrapper
	Installation string
}

// HookSecrets implements gitwebhook.Secrets over the secrets module: the hook
// secret is a project secret of kind generic, resolved by its secret://
// reference in the trigger's own scope. Resolution is just in time: the
// current non-revoked version is read, its scope key unwrapped and the value
// opened under its bound ownership context, and the plaintext is returned to
// the caller alone (who clears it). The adapter keeps no copy, never logs, and
// every error it returns is free of the value, the reference's content and
// provider detail.
//
// The hook secret is not a run's secret: nothing pins it, so unlike a runtime
// resolution this read writes no pin or resolution audit; the trigger's own
// audit (git_hook.*) records each use. A hosted custody adapter (O-19) replaces
// this type without changing the port.
type HookSecrets struct{ cfg HookSecretsConfig }

var _ gw.Secrets = (*HookSecrets)(nil)

// NewHookSecrets requires every collaborator.
func NewHookSecrets(cfg HookSecretsConfig) (*HookSecrets, error) {
	if cfg.Reader == nil || cfg.Wrapper == nil || cfg.Installation == "" {
		return nil, errors.New("gitwebhook/adapters: a secret reader, key wrapper and installation ID are required")
	}
	return &HookSecrets{cfg: cfg}, nil
}

const listPage = 100

// maxListPages bounds the name walk; a project holds few secrets of one name.
const maxListPages = 1000

// errSecretUnavailable is the only outage text: no cause is attached, because
// provider, envelope and database errors can carry identifiers of the value's
// custody. The caller retries; the operator reads the secrets module's own
// failure surfaces.
var errSecretUnavailable = fmt.Errorf("%w: the hook secret could not be read", gw.ErrUnavailable)

// Resolve implements gitwebhook.Secrets. A missing, deleted, revoked, wrong-kind
// or foreign reference is ErrSecretNotFound, indistinguishable from one another.
func (h *HookSecrets) Resolve(ctx context.Context, scope domain.Scope, ref string) ([]byte, error) {
	if err := scope.Validate(); err != nil {
		return nil, gw.ErrSecretNotFound
	}
	name, err := secrets.ParseSecretRef(ref)
	if err != nil {
		return nil, gw.ErrSecretNotFound
	}
	sec, err := h.find(ctx, scope, name)
	if err != nil {
		return nil, err
	}
	if sec.Deleted() || sec.Scope() != scope || sec.Kind() != secrets.KindGeneric || sec.CurrentVersion() < 1 {
		return nil, gw.ErrSecretNotFound
	}
	ver, err := h.cfg.Reader.GetSecretVersion(ctx, scope, sec.ID(), sec.CurrentVersion())
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return nil, gw.ErrSecretNotFound
	case err != nil:
		return nil, errSecretUnavailable
	case ver.Revoked():
		return nil, gw.ErrSecretNotFound
	}
	sk, err := h.cfg.Reader.GetScopeKey(ctx, scope.TenantID, ver.Record().ScopeKeyID)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return nil, errSecretUnavailable // a value without its key is an inconsistency, not a missing reference
	case err != nil:
		return nil, errSecretUnavailable
	}
	material, err := secrets.UnwrapScopeKey(ctx, h.cfg.Wrapper, h.cfg.Installation, sk)
	if err != nil {
		return nil, errSecretUnavailable
	}
	defer clear(material)
	plain, err := envelope.Open(material, ver.EnvelopeContext(h.cfg.Installation, sec.Name()), ver.Envelope())
	if err != nil {
		return nil, errSecretUnavailable
	}
	return plain, nil
}

// find pages through the project's secrets (ordered by name, scoped by tenant
// and project) for the exact name.
func (h *HookSecrets) find(ctx context.Context, scope domain.Scope, name string) (secrets.Secret, error) {
	after := ""
	for range maxListPages {
		page, err := h.cfg.Reader.ListSecrets(ctx, scope, after, listPage)
		if err != nil {
			return secrets.Secret{}, errSecretUnavailable
		}
		for _, s := range page {
			switch {
			case s.Name() < name:
				continue
			case s.Name() == name:
				return s, nil
			default:
				return secrets.Secret{}, gw.ErrSecretNotFound
			}
		}
		if len(page) < listPage {
			return secrets.Secret{}, gw.ErrSecretNotFound
		}
		after = page[len(page)-1].Name()
	}
	return secrets.Secret{}, gw.ErrSecretNotFound
}
