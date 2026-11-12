// Package resource declares registered resource bindings and their atomic
// change commit unit.
//
// A binding records a stable resource identity, its ownership scope, the
// enforcing service, trusted endpoint origins, the operations the service may
// verify and the event types it may report, plus bounded metadata. Kodama
// stores metadata, not resource bytes, access rules or provisioning jobs. Each
// change increments the binding version and commits audit and events with the
// metadata change.
package resource

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Binding metadata bounds. Metadata is bounded display/selector data, never
// resource bytes or access rules; the same limits apply to every read, write
// and query so an oversized registration is refused before any effect.
const (
	// MaxMetadataEntries is the largest accepted metadata entry count.
	MaxMetadataEntries = 32
	// MaxMetadataKeyBytes is the largest accepted metadata key length.
	MaxMetadataKeyBytes = 128
	// MaxMetadataValueBytes is the largest accepted metadata value length.
	MaxMetadataValueBytes = 1024
)

// Binding page bounds. A page is stable and cursor based; a non-positive or
// oversized limit is refused rather than silently clamped.
const (
	// DefaultBindingPageSize is the default binding page size.
	DefaultBindingPageSize = 50
	// MaxBindingPageSize is the largest accepted binding page size.
	MaxBindingPageSize = 200
)

// ValidateMetadata checks the declared metadata bounds: at most
// MaxMetadataEntries entries, a non-empty bounded key and a bounded value.
func ValidateMetadata(metadata map[string]string) error {
	if len(metadata) > MaxMetadataEntries {
		return fault.New(fault.InvalidInput, fmt.Sprintf("metadata has %d entries; at most %d are allowed", len(metadata), MaxMetadataEntries))
	}
	for key, value := range metadata {
		if key == "" {
			return fault.New(fault.InvalidInput, "metadata key is empty")
		}
		if len(key) > MaxMetadataKeyBytes {
			return fault.New(fault.InvalidInput, fmt.Sprintf("metadata key is %d bytes; at most %d are allowed", len(key), MaxMetadataKeyBytes))
		}
		if len(value) > MaxMetadataValueBytes {
			return fault.New(fault.InvalidInput, fmt.Sprintf("metadata value for %q is %d bytes; at most %d are allowed", key, len(value), MaxMetadataValueBytes))
		}
	}
	return nil
}

// ResourceID identifies one registered external resource.
type ResourceID string

// BindingID identifies one resource binding.
type BindingID string

// ParseResourceID validates and converts a resource identity.
func ParseResourceID(value string) (ResourceID, error) {
	if !validResourceID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid resource id %q", value))
	}
	return ResourceID(value), nil
}

// ParseBindingID validates and converts a binding identity.
func ParseBindingID(value string) (BindingID, error) {
	if !validResourceID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid binding id %q", value))
	}
	return BindingID(value), nil
}

func validResourceID(value string) bool {
	if value == "" || len(value) > id.MaxIDLength {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// Operation is one resource operation a binding may verify and a verified
// report may cite. Resource operations name effects an enforcing external
// service performs against its own resource, for example a Git ref update.
//
// Resource operations are deliberately a separate vocabulary from the public
// authorize.OperationName registry: the public registry covers the operations
// the Shell router serves, while a binding declares only the effects its
// enforcing service verifies. A public operation name is not a resource
// operation and a resource operation is not a public operation.
type Operation string

const (
	// OperationGitRefUpdate is the Git service's ref-update effect, verified
	// against a repository binding and guarded by Git access rules.
	OperationGitRefUpdate Operation = "git.ref.update"
	// OperationGitRead is the Git service's repository-level read check. It is
	// verified with an empty subject and no approval: verification supplies the
	// principal and scoped roles, then the repository's ordered rules decide
	// the read. It is never a per-ref operation.
	OperationGitRead Operation = "git.read"
)

// resourceOperations is the declaration order of resource operations. Only
// operations the product specs name as verifiable effects belong here.
var resourceOperations = []Operation{
	OperationGitRefUpdate,
	OperationGitRead,
}

// Operations returns every known resource operation in declaration order. Each
// call returns a copy so a consumer cannot mutate the declared vocabulary.
func Operations() []Operation {
	return append([]Operation(nil), resourceOperations...)
}

// ParseOperation validates and converts a resource operation name.
func ParseOperation(value string) (Operation, error) {
	operation := Operation(value)
	if !operation.Valid() {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid resource operation %q", value))
	}
	return operation, nil
}

// Valid reports whether the operation is a known resource operation. Empty and
// unknown names are refused.
func (operation Operation) Valid() bool {
	for _, candidate := range resourceOperations {
		if operation == candidate {
			return true
		}
	}
	return false
}

// Binding is one registered resource at its current version.
//
// ResourceID is the stable resource identity a verified report and the
// resources row cite; ID is the binding identity. They are distinct typed
// names even when a deployment chooses the same stable value, so a caller
// cannot silently pass a binding id where a resource id is required.
type Binding struct {
	ID               BindingID         `json:"id"`
	ResourceID       ResourceID        `json:"resource_id"`
	Scope            tenancy.Scope     `json:"scope"`
	EnforcingService service.ServiceID `json:"enforcing_service"`
	TrustedOrigins   []string          `json:"trusted_origins,omitempty"`
	Operations       []Operation       `json:"operations,omitempty"`
	ReportableEvents []string          `json:"reportable_events,omitempty"`
	Version          uint64            `json:"version"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks the binding identity, linked resource identity, scope,
// enforcing service, declared resource operation/reportable event names and
// metadata bounds. Origin syntax is an adapter and public-API concern.
func (binding Binding) Validate() error {
	if _, err := ParseBindingID(string(binding.ID)); err != nil {
		return err
	}
	if _, err := ParseResourceID(string(binding.ResourceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "binding resource id is invalid", err)
	}
	if err := binding.Scope.Validate(); err != nil {
		return err
	}
	if _, err := service.ParseServiceID(string(binding.EnforcingService)); err != nil {
		return err
	}
	for _, operation := range binding.Operations {
		if !operation.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("binding declares unknown operation %q", operation))
		}
	}
	for _, eventType := range binding.ReportableEvents {
		if eventType == "" {
			return fault.New(fault.InvalidInput, "binding declares an empty reportable event type")
		}
	}
	return ValidateMetadata(binding.Metadata)
}

// DeclaresReport reports whether the binding lists the reportable event type.
// The verified-report unit checks this before accepting a declared type.
func (binding Binding) DeclaresReport(eventType string) bool {
	for _, declared := range binding.ReportableEvents {
		if declared == eventType {
			return true
		}
	}
	return false
}

// BindingUnit is the complete atomic binding-change commit unit. It carries
// the owner scope, expected version, the next binding value, the receipt,
// audit, publication intent, current-authority reads and post-commit effects.
type BindingUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	BindingID       BindingID                 `json:"binding_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	Binding         Binding                   `json:"binding"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// BindingResult is the committed binding result.
type BindingResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// BindingReadRequest asks for one binding by its stable identity inside the
// caller's scope. The adapter applies the current scope at read time.
type BindingReadRequest struct {
	Scope     tenancy.Scope `json:"scope"`
	BindingID BindingID     `json:"binding_id"`
}

// Validate checks the scope and binding identity.
func (request BindingReadRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	_, err := ParseBindingID(string(request.BindingID))
	return err
}

// BindingDeleteUnit is the complete atomic binding-deletion commit unit. The
// adapter compares ExpectedVersion and re-reads the supplied current authority
// inside the same transaction as the binding removal, receipt, audit and event
// publication intent.
type BindingDeleteUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	BindingID       BindingID                 `json:"binding_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// BindingDeleteResult is the committed binding-deletion result.
type BindingDeleteResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// BindingQuery is one scoped binding listing. EnforcingService and ResourceID
// optionally narrow the listing; Cursor is the opaque stable continuation
// returned by the previous page.
type BindingQuery struct {
	Scope            tenancy.Scope     `json:"scope"`
	EnforcingService service.ServiceID `json:"enforcing_service,omitempty"`
	ResourceID       ResourceID        `json:"resource_id,omitempty"`
	Cursor           string            `json:"cursor,omitempty"`
	Limit            int               `json:"limit"`
}

// Validate checks the scope, optional filters, cursor and bounded page size.
func (query BindingQuery) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if query.EnforcingService != "" {
		if _, err := service.ParseServiceID(string(query.EnforcingService)); err != nil {
			return fault.Wrap(fault.InvalidInput, "binding query enforcing service id is invalid", err)
		}
	}
	if query.ResourceID != "" {
		if _, err := ParseResourceID(string(query.ResourceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "binding query resource id is invalid", err)
		}
	}
	if query.Limit <= 0 || query.Limit > MaxBindingPageSize {
		return fault.New(fault.InvalidInput, "binding query limit is outside the allowed page bounds")
	}
	return nil
}

// BindingPage is one stable binding page.
type BindingPage struct {
	Bindings   []Binding `json:"bindings"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
