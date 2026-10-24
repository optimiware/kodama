package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// SecretReader is the secrets module's read surface. *secrets/postgres.Store
// implements it.
type SecretReader interface {
	ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error)
	GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error)
}

// Credentials implements delivery.Credentials over the secrets owner's
// metadata. Only metadata is read: kind, current version and revocation. No
// value is opened, so delivery never holds a credential's plaintext.
type Credentials struct{ r SecretReader }

var _ delivery.Credentials = (*Credentials)(nil)

// NewCredentials requires the reader.
func NewCredentials(r SecretReader) (*Credentials, error) {
	if r == nil {
		return nil, errors.New("delivery/adapters: a secret reader is required")
	}
	return &Credentials{r: r}, nil
}

const listPage = 100

// Current implements delivery.Credentials. The reference must be a
// project-owned secret of the scope; missing, deleted, foreign and revoked
// references are all ErrCredentialUnavailable.
func (c *Credentials) Current(ctx context.Context, scope domain.Scope, ref string) (delivery.CredentialInfo, error) {
	unavailable := func(cause error) (delivery.CredentialInfo, error) {
		return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrCredentialUnavailable, cause, "a credential is not available in this project")
	}
	name, err := secrets.ParseSecretRef(ref)
	if err != nil {
		return unavailable(err)
	}
	// Listing is ordered by name and tenant/project scoped; the name is unique
	// within the project.
	after := ""
	for {
		page, err := c.r.ListSecrets(ctx, scope, after, listPage)
		if err != nil {
			return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrUnavailable, err, "the secrets owner could not answer")
		}
		for _, s := range page {
			if s.Name() < name {
				continue
			}
			if s.Name() > name {
				return unavailable(nil)
			}
			if s.Deleted() || s.Scope() != scope || s.CurrentVersion() < 1 {
				return unavailable(nil)
			}
			v, err := c.r.GetSecretVersion(ctx, scope, s.ID(), s.CurrentVersion())
			if err != nil {
				if errors.Is(err, secrets.ErrNotFound) {
					return unavailable(err)
				}
				return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrUnavailable, err, "the secrets owner could not answer")
			}
			if v.Revoked() {
				return unavailable(nil)
			}
			return delivery.CredentialInfo{Ref: s.Ref(), Kind: string(s.Kind()), Version: s.CurrentVersion()}, nil
		}
		if len(page) < listPage {
			return unavailable(nil)
		}
		after = page[len(page)-1].Name()
	}
}
