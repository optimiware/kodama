package work

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// Caller identifies who is asking. PrincipalID is the verified identity of the
// authenticated request; the actor type is resolved by the authority port.
// RequestID, when present, is recorded in history and used as the event
// correlation identifier.
type Caller struct {
	PrincipalID string
	RequestID   string
}

func (c Caller) validate() error {
	if c.PrincipalID == "" {
		return fmt.Errorf("%w: a verified caller is required", ErrDenied)
	}
	return nil
}

// CreateCommand creates a work item in backlog.
type CreateCommand struct {
	Scope         domain.Scope
	Caller        Caller
	Kind          Kind
	Title         string
	Spec          string
	Criteria      []Criterion
	EvidenceLinks []EvidenceRef
	AssigneeID    string
	ParentID      domain.WorkItemID
}

// EditCommand applies a Patch under the expected resource version.
type EditCommand struct {
	Scope    domain.Scope
	Caller   Caller
	ID       domain.WorkItemID
	Expected domain.Version
	Patch    Patch
}

// TransitionCommand requests an explicit business transition under the expected
// resource version.
type TransitionCommand struct {
	Scope    domain.Scope
	Caller   Caller
	ID       domain.WorkItemID
	Expected domain.Version
	Request  TransitionRequest
}

// RandomID returns prefix + 128 random bits in hex, a valid opaque identifier.
func RandomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
