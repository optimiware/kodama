// Package secret_test pins the secret-custody management and resolution API.
//
// Package kodama/internal/shell/secret provides:
//
//	// Stored is one current stored secret: readable metadata, its creation
//	// time and the encrypted envelope. It never carries plaintext.
//	type Stored struct {
//		Metadata  Metadata
//		CreatedAt time.Time
//		Envelope  Envelope
//	}
//
//	// EnvelopeCipher is the consumer-owned envelope seam mirrored from the
//	// key adapter. It seals and opens one value under scoped associated
//	// data and never logs plaintext.
//	type EnvelopeCipher interface {
//		Seal(context.Context, AssociatedData, []byte) (Envelope, error)
//		Open(context.Context, AssociatedData, Envelope) ([]byte, error)
//	}
//
//	// NameLookup retrieves the current stored value for one exact scoped
//	// name. An empty ProjectID reads the tenant-wide row. A missing row is
//	// fault.NotFound; an unreadable row is a dependency failure. Lookup never
//	// widens or rewrites the supplied scope.
//	type NameLookup interface {
//		Lookup(context.Context, tenancy.Scope, id.SecretName) (Stored, error)
//	}
//
//	// NamePolicy authorizes one declared secret name for one scope at
//	// activation. Check returns nil only for an explicit allow.
//	type NamePolicy interface {
//		Check(context.Context, tenancy.Scope, id.SecretName) error
//	}
//
//	// Clock is the injected authoritative time source for management commits.
//	type Clock interface {
//		Now() time.Time
//	}
//
//	// IDSource allocates the durable identities one management commit needs.
//	type IDSource interface {
//		NewSecretID() (SecretID, error)
//		NewReceiptID() (id.ReceiptID, error)
//		NewEventID() (id.EventID, error)
//	}
//
//	// ManagerConfig is the complete management boundary set. InstallationID
//	// and a positive KeyVersion are required; every port is required.
//	type ManagerConfig struct {
//		InstallationID string
//		KeyVersion     KeyVersion
//		Clock          Clock
//		IDs            IDSource
//		Executor       store.Executor
//		// Scanner is the scoped multi-row read seam the commit-boundary
//		// membership guard scans by principal_id; a membership row id is never
//		// the principal id.
//		Scanner        store.Scanner
//		Cipher         EnvelopeCipher
//		Lookup         NameLookup
//	}
//
//	func NewManager(ManagerConfig) (*Manager, error)
//
//	// Manager writes, replaces and deletes one current secret value through
//	// one guarded store commit unit. Reads return Metadata only.
//	type Manager struct{ /* ... */ }
//
//	type PutCommand struct {
//		Scope           tenancy.Scope
//		Name            id.SecretName
//		Value           string
//		ExpectedVersion uint64 // 0 inserts; a positive value replaces
//		Actor           id.PrincipalID
//		CallerKey       string
//		Content         receipt.Content
//	}
//
//	type DeleteCommand struct {
//		Scope           tenancy.Scope
//		Name            id.SecretName
//		ExpectedVersion uint64
//		Actor           id.PrincipalID
//		CallerKey       string
//		Content         receipt.Content
//	}
//
//	func (*Manager) Put(context.Context, PutCommand) (Metadata, error)
//	func (*Manager) Delete(context.Context, DeleteCommand) error
//	func (*Manager) Get(context.Context, tenancy.Scope, SecretID) (Metadata, error)
//
// A metadata read goes through store.Executor.Read and returns Metadata only;
// the envelope never leaves the management seam.
//
//	// ResolverConfig is the complete activation-resolution boundary set.
//	type ResolverConfig struct {
//		InstallationID string
//		Policy         NamePolicy
//		Lookup         NameLookup
//		Cipher         EnvelopeCipher
//	}
//
//	func NewResolver(ResolverConfig) (*SecretResolver, error)
//
//	// SecretResolver authorizes and resolves declared names at activation.
//	type SecretResolver struct{ /* ... */ }
//
//	func (*SecretResolver) Resolve(context.Context, ResolveRequest) ([]Resolved, error)
//
// Behavior these tests rely on:
//
//   - Management writes seal the value with AssociatedData carrying the
//     configured installation id, the row scope (empty project for tenant-wide
//     rows), the stable secret id, the declared name and the configured key
//     version. The row stores tenant_id, project_id (nil for tenant-wide),
//     name, version, created_at, updated_at, ciphertext, wrapped_key, key_ref,
//     key_version and nonce.
//   - Insert commits Insert with ExpectVersion 0; replacement commits Update
//     with the current id and ExpectVersion, bumps version by one and keeps
//     created_at. Delete commits a guarded Delete with the current id and
//     ExpectVersion. Each commit carries one receipt, one audit row and one
//     config.changed event publication intent in the same store.Unit.
//   - No receipt result, audit detail, event data or error message contains
//     the plaintext value.
//   - Resolution checks policy for each declared name before that name is
//     looked up or decrypted. A project-scoped request reads the project row
//     first and reads the tenant-wide row only when the project row is absent.
//     Denial, an unreadable row, a tampered envelope, a cross-scope row and a
//     missing tenant row all fail the whole request with no partial result.
//   - Resolved values keep the request name order and carry the scope where
//     the value was found (project or tenant-wide).
//   - A denial, missing, unreadable, tampered or cross-scope value never
//     returns a plaintext value or a partial set of values.
package secret_test

import (
	"github.com/optimiware/kodama/internal/shell/secret"
)

// Compile-time pins for the declarations the behavior tests consume.
var (
	_ secret.Resolver = (*secret.SecretResolver)(nil)
	_                 = secret.NewManager
	_                 = secret.NewResolver
)
