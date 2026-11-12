package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock is the injected authoritative time source. Mutations use the time the
// commit unit already carries; the authority reader needs one clock to evaluate
// an action deadline at read time.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable row identity a membership insert needs. The
// capability units identify tenants, projects, bindings and services
// themselves; only memberships have no identity field, so the adapter allocates
// one. A nil source uses crypto/rand.
type IDSource interface {
	NewRowID() (string, error)
}

// Config binds the neutral persistence seams and the injected boundaries. Both
// Executor and Scanner are required: a read-only adapter without a scanner
// could not answer the list/query ports, and a scanner without an executor
// could not commit.
type Config struct {
	Executor store.Executor
	Scanner  store.Scanner
	Clock    Clock
	IDs      IDSource
}

// Adapter implements every identity, tenancy, binding, service and credential
// store-backed port over one neutral commit-unit executor and one scoped
// scanner.
type Adapter struct {
	executor store.Executor
	scanner  store.Scanner
	clock    Clock
	ids      IDSource
}

// New validates the injected seams and returns one adapter.
func New(config Config) (*Adapter, error) {
	switch {
	case config.Executor == nil:
		return nil, fault.New(fault.InternalFailure, "identity store adapter requires an executor")
	case config.Scanner == nil:
		return nil, fault.New(fault.InternalFailure, "identity store adapter requires a scanner")
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "identity store adapter requires a clock")
	}
	return &Adapter{
		executor: config.Executor,
		scanner:  config.Scanner,
		clock:    config.Clock,
		ids:      config.IDs,
	}, nil
}

// newRowID allocates one membership row identity.
func (a *Adapter) newRowID() (string, error) {
	if a.ids != nil {
		return a.ids.NewRowID()
	}
	var material [16]byte
	if _, err := rand.Read(material[:]); err != nil {
		return "", fault.Wrap(fault.InternalFailure, "membership identity generation failed", err)
	}
	return "membership-" + hex.EncodeToString(material[:]), nil
}

// encodeJSON renders one structured column value. Every jsonb column in this
// package stores the exact JSON encoding of one typed domain value.
func encodeJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "a structured column value could not be encoded", err)
	}
	return encoded, nil
}

// decodeJSON decodes one stored jsonb value into its typed domain value. A nil
// or empty value is refused rather than silently decoding to a zero value.
func decodeJSON(data []byte, target any) error {
	if len(data) == 0 {
		return fault.New(fault.InternalFailure, "a required structured column value is empty")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, "a stored structured column value is not valid", err)
	}
	return nil
}

// encodeRoles renders a membership or service role list. A nil list encodes as
// JSON null so "no roles" stays distinct from an empty grant.
func encodeRoles(roles []tenancy.Role) ([]byte, error) {
	return encodeJSON(roles)
}

// decodeRoles decodes a stored role list.
func decodeRoles(data []byte) ([]tenancy.Role, error) {
	var roles []tenancy.Role
	if err := decodeJSON(data, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

// rowString reads one text column. A NULL or missing text column is refused
// because every column this package reads as a string is non-nullable.
func rowString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

// rowUint reads one bigint/integer column as an unsigned version.
func rowUint(row store.Row, name string) (uint64, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	switch typed := value.(type) {
	case int64:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	case int:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	default:
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not an integer", row.Table, name))
	}
}

// rowBytes reads one bytea/jsonb column as exact bytes. A NULL value reads as
// nil only when the caller asked for an optional column.
func rowBytes(row store.Row, name string) ([]byte, error) {
	value, ok := row.Values[name]
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return nil, nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not bytes", row.Table, name))
	}
	return bytes, nil
}

// rowNullableTime reads one nullable timestamptz column.
func rowNullableTime(row store.Row, name string) (*time.Time, error) {
	value, ok := row.Values[name]
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return nil, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not a timestamp", row.Table, name))
	}
	return &moment, nil
}

// rowNullableString reads one nullable text column.
func rowNullableString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

// decodeScope rebuilds one scope from a stored row's tenant and project
// columns. The scope columns are adapter-supplied and always present.
func decodeScope(row store.Row) (tenancy.Scope, error) {
	tenant, err := rowString(row, "tenant_id")
	if err != nil {
		return tenancy.Scope{}, err
	}
	project, err := rowNullableString(row, "project_id")
	if err != nil {
		return tenancy.Scope{}, err
	}
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}, nil
}

// decodeReceiptResult decodes the durable result body a replayed receipt retained. A
// replayed unit returns exactly the result the first attempt committed; no
// business write, audit row or event row is applied again.
func decodeReceiptResult[T any](result store.UnitResult) (T, bool, error) {
	var decoded T
	if !result.Replayed {
		return decoded, false, nil
	}
	if len(result.Receipt.Result) == 0 {
		return decoded, true, fault.New(fault.InternalFailure, "a replayed receipt retained no result body")
	}
	if err := json.Unmarshal(result.Receipt.Result, &decoded); err != nil {
		return decoded, true, fault.Wrap(fault.InternalFailure, "a replayed receipt result body is not decodable", err)
	}
	return decoded, true, nil
}

// requireCommitTime refuses a unit whose commit time is absent, so a stored
// created_at can never be a zero value.
func requireCommitTime(at time.Time) error {
	if at.IsZero() {
		return fault.New(fault.InvalidInput, "commit unit time is zero")
	}
	return nil
}

// requireUnitVersion checks the next-value version a capability already decided
// advances the expected version by exactly one. It is a mapping-integrity check,
// not a policy decision: the adapter must not silently write a version the unit
// did not name.
func requireUnitVersion(expected, next uint64) error {
	if next != expected+1 {
		return fault.New(fault.InvalidInput, "commit unit version does not advance the expected version")
	}
	return nil
}
