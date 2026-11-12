// Package identity declares verified principals and their kinds.
//
// A principal is a verified human, an enabled service account, or the current
// eligible execution. URL ids, environment variables, body roles, network
// location and correlation strings are never proof of identity; a principal
// value only exists after the Shell's authentication boundary has produced it.
// Current roles and enablement are re-read at each authorized commit rather
// than trusted from a cached copy.
package identity

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// PrincipalKind names the three authenticated principal kinds.
type PrincipalKind string

const (
	// KindHuman is a verified human membership holder.
	KindHuman PrincipalKind = "human"
	// KindService is an enabled service account.
	KindService PrincipalKind = "service"
	// KindExecution is the current running container of one execution.
	KindExecution PrincipalKind = "execution"
)

var principalKinds = [...]PrincipalKind{
	KindHuman,
	KindService,
	KindExecution,
}

// PrincipalKinds returns every principal kind in declaration order.
func PrincipalKinds() []PrincipalKind {
	return append([]PrincipalKind(nil), principalKinds[:]...)
}

// Valid reports whether the kind is one of the declared kinds.
func (kind PrincipalKind) Valid() bool {
	for _, candidate := range principalKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Principal is one authenticated identity in one ownership scope.
//
// Roles are the current scoped grants; for an execution principal they are
// always empty because execution authority is the separate
// authorize.ExecutionPermission value admitted with the execution. ExecutionID
// is set only for KindExecution.
type Principal struct {
	ID          id.PrincipalID `json:"id"`
	Kind        PrincipalKind  `json:"kind"`
	Scope       tenancy.Scope  `json:"scope"`
	Roles       []tenancy.Role `json:"roles,omitempty"`
	Enabled     bool           `json:"enabled"`
	Version     uint64         `json:"version"`
	ExecutionID id.ExecutionID `json:"execution_id,omitempty"`
}

// Validate checks that the principal is structurally eligible for its kind.
//
// Structural validity is not current authority: a disabled or revoked
// principal may still be well formed, and authorization rechecks enablement
// and roles at the commit boundary.
func (principal Principal) Validate() error {
	if _, err := id.ParsePrincipalID(string(principal.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "principal id is invalid", err)
	}
	if !principal.Kind.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown principal kind %q", principal.Kind))
	}
	if err := principal.Scope.Validate(); err != nil {
		return err
	}
	for _, role := range principal.Roles {
		if !role.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown principal role %q", role))
		}
	}
	switch principal.Kind {
	case KindHuman:
		if principal.ExecutionID != "" {
			return fault.New(fault.InvalidInput, "human principal carries an execution id")
		}
	case KindService:
		if principal.ExecutionID != "" {
			return fault.New(fault.InvalidInput, "service principal carries an execution id")
		}
		for _, role := range principal.Roles {
			if role == tenancy.RoleAdmin {
				return fault.New(fault.InvalidInput, "service principal holds the Admin role")
			}
		}
	case KindExecution:
		if len(principal.Roles) != 0 {
			return fault.New(fault.InvalidInput, "execution principal carries membership roles")
		}
		if _, err := id.ParseExecutionID(string(principal.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "execution principal execution id is invalid", err)
		}
	}
	return nil
}

// HasRole reports whether the principal currently holds the role.
func (principal Principal) HasRole(role tenancy.Role) bool {
	for _, candidate := range principal.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}
