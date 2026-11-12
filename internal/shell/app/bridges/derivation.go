// Tenant derivation-key resolver bridge.
//
// token.TenantKey stores the encrypted derivation key as the
// AES-256-GCM envelope members: the content nonce is prefixed to the
// ciphertext, and the wrapped data key, wrap nonce, key reference and provider
// key version are carried separately. The bridge reconstructs that envelope and
// opens it through the keys seam. The installation, secret identity and name
// used as authenticated associated data are trusted composition configuration;
// the tenant and provider key version come from the stored key itself. The
// returned plaintext belongs to the caller and is never logged, rendered or
// retained by this bridge.
package bridges

import (
	"context"
	"math"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/token"
)

// DerivationKeyConfig binds the envelope-cipher seam and the trusted
// associated identity of a tenant derivation key. TenantID, ProjectID and
// KeyVersion in Associated are ignored and replaced from the stored key.
type DerivationKeyConfig struct {
	Cipher     secret.EnvelopeCipher
	Associated secret.AssociatedData
}

// DerivationKeyResolver opens one tenant derivation key.
type DerivationKeyResolver struct {
	cipher     secret.EnvelopeCipher
	associated secret.AssociatedData
}

var _ token.DerivationKeyResolver = (*DerivationKeyResolver)(nil)

// NewDerivationKeyResolver builds the resolver. A nil cipher or an unusable
// associated identity is refused at construction.
func NewDerivationKeyResolver(config DerivationKeyConfig) (*DerivationKeyResolver, error) {
	if config.Cipher == nil {
		return nil, fault.New(fault.InvalidInput, "derivation key resolver requires an envelope cipher")
	}
	associated := config.Associated
	if associated.InstallationID == "" {
		return nil, fault.New(fault.InvalidInput, "derivation key associated identity has no installation id")
	}
	if associated.ProjectID != "" {
		return nil, fault.New(fault.InvalidInput, "derivation key associated identity must be tenant scoped")
	}
	if _, err := secret.ParseSecretID(string(associated.SecretID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "derivation key associated secret id is invalid", err)
	}
	if _, err := id.ParseSecretName(string(associated.Name)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "derivation key associated secret name is invalid", err)
	}
	return &DerivationKeyResolver{cipher: config.Cipher, associated: associated}, nil
}

// ResolveDerivationKey opens and authenticates one stored tenant key. Every
// failure returns no plaintext and a dependency fault; the key's ciphertext,
// wrapped key and key reference never appear in the error.
func (resolver *DerivationKeyResolver) ResolveDerivationKey(ctx context.Context, key token.TenantKey) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DeadlineExpired, "tenant derivation key resolution was canceled", err)
	}
	if resolver == nil || resolver.cipher == nil {
		return nil, fault.New(fault.InternalFailure, "derivation key resolver is not configured")
	}
	if err := key.Validate(); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "stored tenant derivation key is invalid", err)
	}
	if key.KeyVersion > math.MaxUint32 {
		return nil, fault.New(fault.DependencyUnavailable, "stored tenant derivation key provider version is out of range")
	}
	if len(key.Ciphertext) <= secret.ContentNonceBytes {
		return nil, fault.New(fault.DependencyUnavailable, "stored tenant derivation key ciphertext is incomplete")
	}
	associated := resolver.associated
	associated.TenantID = key.TenantID
	associated.ProjectID = ""
	associated.KeyVersion = secret.KeyVersion(key.KeyVersion)
	envelope := secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef(key.KeyRef),
		KeyVersion: secret.KeyVersion(key.KeyVersion),
		Nonce:      key.Ciphertext[:secret.ContentNonceBytes],
		Ciphertext: key.Ciphertext[secret.ContentNonceBytes:],
		WrappedKey: key.WrappedKey,
		WrapNonce:  key.WrapNonce,
	}
	plaintext, err := resolver.cipher.Open(ctx, associated, envelope)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fault.Wrap(fault.DeadlineExpired, "tenant derivation key resolution was canceled", ctxErr)
		}
		return nil, fault.Wrap(fault.DependencyUnavailable, "tenant derivation key cannot be opened", err)
	}
	return plaintext, nil
}
