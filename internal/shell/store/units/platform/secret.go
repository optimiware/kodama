package platform

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// SecretLookup is the store-backed secret.NameLookup.
//
// Lookup reads one current stored secret by its exact scoped name and returns
// readable metadata plus the encrypted envelope. It never decrypts, never
// widens the scope and never returns plaintext. A project-scoped lookup reads
// only the project row; the activation resolver owns the tenant-wide fallback.
type SecretLookup struct {
	scanner store.Scanner
}

// NewSecretLookup builds the secret name lookup adapter.
func NewSecretLookup(scanner store.Scanner) (*SecretLookup, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "secret lookup requires a scanner")
	}
	return &SecretLookup{scanner: scanner}, nil
}

var _ secret.NameLookup = (*SecretLookup)(nil)

// Lookup returns the current stored value for one exact scoped name.
func (lookup *SecretLookup) Lookup(ctx context.Context, scope tenancy.Scope, name id.SecretName) (secret.Stored, error) {
	if err := scope.Validate(); err != nil {
		return secret.Stored{}, err
	}
	if _, err := id.ParseSecretName(string(name)); err != nil {
		return secret.Stored{}, fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	page, err := lookup.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   secretsTable,
		Equals:  map[string]any{nameColumn: string(name)},
		OrderBy: []store.Order{{Column: idColumn}},
		Limit:   1,
	})
	if err != nil {
		return secret.Stored{}, err
	}
	if len(page.Rows) == 0 {
		return secret.Stored{}, fault.New(fault.NotFound, "secret is not stored in scope")
	}
	return storedFromRow(page.Rows[0])
}

// storedFromRow maps one secrets row to the management/resolution value.
// Every retained byte field is copied, so a caller cannot alias stored state.
func storedFromRow(row store.Row) (secret.Stored, error) {
	identity, err := textValue(row, idColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	if _, err := secret.ParseSecretID(identity); err != nil {
		return secret.Stored{}, fault.Wrap(fault.InternalFailure, "stored secret id is invalid", err)
	}
	tenant, err := textValue(row, tenantIDColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	project, err := nullableTextValue(row, projectIDColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	storedName, err := textValue(row, nameColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	if _, err := id.ParseSecretName(storedName); err != nil {
		return secret.Stored{}, fault.Wrap(fault.InternalFailure, "stored secret name is invalid", err)
	}
	version, err := nullableUintValue(row, versionColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	createdAt, err := timeValue(row, createdAtColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	updatedAt, err := timeValue(row, updatedAtColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	keyRef, err := textValue(row, keyRefColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	keyVersion, err := intValue(row, keyVersionColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	nonce, err := bytesValue(row, nonceColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	ciphertext, err := bytesValue(row, ciphertextColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	wrappedKey, err := bytesValue(row, wrappedKeyColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	wrapNonce, err := bytesValue(row, wrapNonceColumn)
	if err != nil {
		return secret.Stored{}, err
	}
	envelope := secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef(keyRef),
		KeyVersion: secret.KeyVersion(uint32(keyVersion)),
		Nonce:      nonce,
		Ciphertext: ciphertext,
		WrappedKey: wrappedKey,
		WrapNonce:  wrapNonce,
	}
	if err := envelope.Validate(); err != nil {
		return secret.Stored{}, fault.Wrap(fault.InternalFailure, "stored secret envelope is invalid", err)
	}
	return secret.Stored{
		Metadata: secret.Metadata{
			ID:        secret.SecretID(identity),
			Scope:     tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
			Name:      id.SecretName(storedName),
			Version:   version,
			UpdatedAt: updatedAt,
		},
		CreatedAt: createdAt,
		Envelope:  envelope,
	}, nil
}
