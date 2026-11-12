package factory

// Read-only approval evidence for the protected factory golden path.
//
// The protected-ref verifier persists the request assignment it actually used
// on the verification row (`requested_approval`). The evidence reader selects
// that exact durable value by verification ID through the scoped runtime role
// and decodes it with the same assignment grammar the product stores. It never
// infers the approval from the committed Git policy or from a caller-supplied
// string.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// ApprovalAssignment is one decoded stored request assignment: either
// `role/<role>` or `service/<id>`. String renders exactly that grammar, so a
// test failure names the approval without echoing stored JSON.
type ApprovalAssignment struct {
	Kind      string
	Role      string
	ServiceID string
}

// String renders the canonical assignment grammar.
func (assignment ApprovalAssignment) String() string {
	switch request.AssignmentKind(assignment.Kind) {
	case request.AssignmentRole:
		return "role/" + assignment.Role
	case request.AssignmentService:
		return "service/" + assignment.ServiceID
	default:
		return ""
	}
}

// Validate checks that the assignment names exactly one assignee of one kind.
func (assignment ApprovalAssignment) Validate() error {
	if assignment.Kind == "" {
		return errors.New("stored requested approval is missing")
	}
	converted := request.Assignment{
		Kind:      request.AssignmentKind(assignment.Kind),
		Role:      tenancy.Role(assignment.Role),
		ServiceID: service.ServiceID(assignment.ServiceID),
	}
	if !converted.Valid() {
		return fmt.Errorf("stored requested approval %q is not a valid assignment", assignment.String())
	}
	return nil
}

// RequestedApproval reads the stored assignment of one exact verification row.
// The read is scoped to this fixture's project and selected by verification ID;
// a missing row is reported instead of synthesized.
func (fixture *GitFixture) RequestedApproval(ctx context.Context, verificationID string) (ApprovalAssignment, error) {
	if fixture == nil || fixture.h == nil {
		return ApprovalAssignment{}, errors.New("git fixture has no harness")
	}
	if strings.TrimSpace(verificationID) == "" {
		return ApprovalAssignment{}, errors.New("requested approval needs a verification identity")
	}
	rows, err := pgtest.ScopedQuery(ctx, fixture.h.runtimeDB, fixture.h.Scope, 1,
		`SELECT coalesce(requested_approval::text, '') FROM verifications WHERE id = $1`, verificationID)
	if err != nil {
		return ApprovalAssignment{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return ApprovalAssignment{}, fmt.Errorf("verification %s returned %d approval rows; want 1", verificationID, len(rows))
	}
	return decodeRequestedApproval([]byte(rows[0][0]))
}

// decodeRequestedApproval decodes one stored `requested_approval` column. The
// stored form is the product assignment object; a missing column, an unknown
// member, a malformed object or a trailing document is refused.
func decodeRequestedApproval(raw []byte) (ApprovalAssignment, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ApprovalAssignment{}, errors.New("stored requested approval is missing")
	}
	var wire struct {
		Kind      string `json:"kind"`
		Role      string `json:"role"`
		ServiceID string `json:"service_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return ApprovalAssignment{}, fmt.Errorf("decode stored requested approval: %w", err)
	}
	if err := rejectTrailingDocument(decoder); err != nil {
		return ApprovalAssignment{}, err
	}
	assignment := ApprovalAssignment{Kind: wire.Kind, Role: wire.Role, ServiceID: wire.ServiceID}
	if err := assignment.Validate(); err != nil {
		return ApprovalAssignment{}, err
	}
	return assignment, nil
}

// rejectTrailingDocument requires the stored column to be exactly one JSON
// document.
func rejectTrailingDocument(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode stored requested approval: %w", err)
	}
	return errors.New("stored requested approval carries trailing data")
}
