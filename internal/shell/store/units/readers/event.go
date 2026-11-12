package readers

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// CursorReader is the store-backed event.CursorPort.
//
// The resume point is the highest committed publication sequence visible in the
// caller's scope. It is read from the durable event table, never inferred from a
// broker position, so a subscription cannot resume past an uncommitted or
// foreign event.
type CursorReader struct {
	scanner store.Scanner
}

// NewCursorReader builds the cursor reader.
func NewCursorReader(scanner store.Scanner) (*CursorReader, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "event cursor reader requires a scanner")
	}
	return &CursorReader{scanner: scanner}, nil
}

var _ event.CursorPort = (*CursorReader)(nil)

// Cursor returns the latest committed event-table sequence for one scope. An
// empty stream is sequence zero.
func (reader *CursorReader) Cursor(ctx context.Context, scope tenancy.Scope) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	page, err := reader.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "events",
		OrderBy: []store.Order{{Column: "publish_sequence", Desc: true}},
		Limit:   1,
	})
	if err != nil {
		return 0, err
	}
	if len(page.Rows) == 0 {
		return 0, nil
	}
	return rowUint(page.Rows[0], "publish_sequence")
}

// CredentialValidity is the store-backed event.CredentialValidity.
//
// It reads the principal's authoritative current credential expiry from the same
// row the authentication boundary reads: an execution's token_expires_at on its
// executions row and a service's token_expires_at on its services row. A
// principal without a current pair has no remaining validity and is not_found.
//
// A human principal's access-token expiry is verified by the OIDC boundary and
// is not stored by the factory, so this store-backed reader cannot answer it;
// that composition is left to the caller rather than guessed from a membership
// row.
type CredentialValidity struct {
	executor store.Executor
}

// NewCredentialValidity builds the credential validity reader.
func NewCredentialValidity(executor store.Executor) (*CredentialValidity, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "credential validity reader requires an executor")
	}
	return &CredentialValidity{executor: executor}, nil
}

var _ event.CredentialValidity = (*CredentialValidity)(nil)

// ValidUntil returns the authoritative expiry of the principal's current
// credential.
func (reader *CredentialValidity) ValidUntil(ctx context.Context, principal identity.Principal) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if err := principal.Validate(); err != nil {
		return time.Time{}, err
	}
	var row store.Row
	var err error
	switch principal.Kind {
	case identity.KindExecution:
		row, err = reader.executor.Read(ctx, principal.Scope, "executions", string(principal.ExecutionID))
		if err != nil {
			return time.Time{}, err
		}
		stored, err := rowString(row, "principal_id")
		if err != nil {
			return time.Time{}, err
		}
		if stored != string(principal.ID) {
			return time.Time{}, fault.New(fault.NotFound, "execution credential is for another principal")
		}
	case identity.KindService:
		row, err = reader.readService(ctx, principal.Scope, string(principal.ID))
		if err != nil {
			return time.Time{}, err
		}
	case identity.KindHuman:
		return time.Time{}, fault.New(fault.NotFound, "human credential validity is not store-backed")
	default:
		return time.Time{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown principal kind %q", principal.Kind))
	}
	expiry, err := rowNullableTime(row, "token_expires_at")
	if err != nil {
		return time.Time{}, err
	}
	if expiry == nil {
		return time.Time{}, fault.New(fault.NotFound, "principal has no current credential expiry")
	}
	return *expiry, nil
}

// readService reads one service row. A project scope falls back to the
// tenant-wide row the identity adapter also resolves, so a tenant-wide service
// keeps its current credential.
func (reader *CredentialValidity) readService(ctx context.Context, scope tenancy.Scope, serviceID string) (store.Row, error) {
	row, err := reader.executor.Read(ctx, scope, "services", serviceID)
	if err == nil {
		return row, nil
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound || !scope.ProjectScoped() {
		return store.Row{}, err
	}
	return reader.executor.Read(ctx, tenancy.Scope{TenantID: scope.TenantID}, "services", serviceID)
}
