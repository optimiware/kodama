// Activation-time secret resolution.
//
// Resolution authorizes each declared name before it is looked up or decrypted.
// A project-scoped request reads the project row first and reads the tenant-wide
// row only when the project row is absent. Denial, an unreadable row, a
// tampered envelope, a cross-scope row and a missing tenant row all fail the
// whole request: no plaintext and no partial result is ever returned.
package secret

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// SecretResolver authorizes and resolves declared names at activation.
type SecretResolver struct {
	installationID string
	policy         NamePolicy
	lookup         NameLookup
	cipher         EnvelopeCipher
}

// NewResolver validates the activation-resolution boundaries. A missing port or
// an empty installation id is a construction failure.
func NewResolver(config ResolverConfig) (*SecretResolver, error) {
	switch {
	case config.InstallationID == "":
		return nil, fault.New(fault.InvalidInput, "secret resolver requires an installation id")
	case config.Policy == nil:
		return nil, fault.New(fault.InvalidInput, "secret resolver requires a name policy")
	case config.Lookup == nil:
		return nil, fault.New(fault.InvalidInput, "secret resolver requires a name lookup")
	case config.Cipher == nil:
		return nil, fault.New(fault.InvalidInput, "secret resolver requires an envelope cipher")
	}
	return &SecretResolver{
		installationID: config.InstallationID,
		policy:         config.Policy,
		lookup:         config.Lookup,
		cipher:         config.Cipher,
	}, nil
}

// Resolve authorizes and resolves every declared name in request order. It
// returns either the complete ordered set or no values at all.
func (resolver *SecretResolver) Resolve(ctx context.Context, request ResolveRequest) ([]Resolved, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	resolved := make([]Resolved, 0, len(request.Names))
	for _, name := range request.Names {
		if err := resolver.policy.Check(ctx, request.Scope, name); err != nil {
			return nil, err
		}
		stored, foundScope, err := resolver.lookupName(ctx, request.Scope, name)
		if err != nil {
			return nil, err
		}
		if err := validateStoredIdentity(stored, foundScope, name); err != nil {
			return nil, err
		}
		associated := AssociatedData{
			InstallationID: resolver.installationID,
			TenantID:       foundScope.TenantID,
			ProjectID:      foundScope.ProjectID,
			SecretID:       stored.Metadata.ID,
			Name:           name,
			KeyVersion:     stored.Envelope.KeyVersion,
		}
		if err := associated.Validate(); err != nil {
			return nil, err
		}
		if err := stored.Envelope.ValidateFor(associated); err != nil {
			return nil, err
		}
		plaintext, err := resolver.cipher.Open(ctx, associated, stored.Envelope)
		if err != nil {
			return nil, err
		}
		if err := DefaultBounds.CheckValue(len(plaintext)); err != nil {
			return nil, err
		}
		value := Resolved{Name: name, Value: string(plaintext), Scope: foundScope}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}

// lookupName reads exactly one snapshot per name. A project-scoped request
// falls back to the tenant-wide row only when the project row is absent; any
// other failure is returned without a fallback read.
func (resolver *SecretResolver) lookupName(ctx context.Context, scope tenancy.Scope, name id.SecretName) (Stored, tenancy.Scope, error) {
	if scope.ProjectScoped() {
		stored, err := resolver.lookup.Lookup(ctx, scope, name)
		if err == nil {
			return stored, scope, nil
		}
		if !isNotFound(err) {
			return Stored{}, tenancy.Scope{}, err
		}
		tenant := tenancy.Scope{TenantID: scope.TenantID}
		stored, err = resolver.lookup.Lookup(ctx, tenant, name)
		if err != nil {
			return Stored{}, tenancy.Scope{}, err
		}
		return stored, tenant, nil
	}
	stored, err := resolver.lookup.Lookup(ctx, scope, name)
	if err != nil {
		return Stored{}, tenancy.Scope{}, err
	}
	return stored, scope, nil
}

// isNotFound reports whether err is the explicit absent-row class. Only that
// class permits tenant fallback; an unreadable row never does.
func isNotFound(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.NotFound
}

// String renders a redacted resolution request for diagnostics. It never
// includes a resolved value.
func (request ResolveRequest) String() string {
	return fmt.Sprintf("resolve secrets scope=%+v names=%v", request.Scope, request.Names)
}
