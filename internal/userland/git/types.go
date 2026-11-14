// Package git declares the independent Git service contract: access rules, the
// durable receive journal, recovery and reporting seams.
//
// The service owns Git binaries, smart HTTP, hooks, storage and the journal. It
// authenticates as a service and uses public verification/report contracts; it
// never imports Kernel or Shell internals and never accesses the factory
// database or tenant keys. These declarations perform no Git command and open
// no repository.
package git

import (
	"fmt"
	"strings"
)

// Permission is one access-rule permission.
type Permission string

const (
	// PermissionRead allows read access.
	PermissionRead Permission = "R"
	// PermissionWrite allows create or fast-forward updates.
	PermissionWrite Permission = "RW"
	// PermissionForce also allows rewind and delete.
	PermissionForce Permission = "RW+"
	// PermissionDeny refuses the matched attempt.
	PermissionDeny Permission = "-"
)

// permissions is the declaration order.
var permissions = []Permission{PermissionRead, PermissionWrite, PermissionForce, PermissionDeny}

// Valid reports whether the permission is one of the declared permissions.
func (permission Permission) Valid() bool {
	for _, candidate := range permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}

// covers reports whether the permission allows the attempted update.
func (permission Permission) covers(update UpdateKind) bool {
	switch permission {
	case PermissionRead:
		return update == UpdateRead
	case PermissionWrite:
		return update == UpdateRead || update == UpdateCreate || update == UpdateFastForward
	case PermissionForce:
		return update == UpdateRead || update == UpdateCreate || update == UpdateFastForward ||
			update == UpdateRewind || update == UpdateDelete
	default:
		return false
	}
}

// UpdateKind is one attempted ref update.
type UpdateKind string

const (
	// UpdateRead reads a ref.
	UpdateRead UpdateKind = "read"
	// UpdateCreate creates a ref.
	UpdateCreate UpdateKind = "create"
	// UpdateFastForward fast-forwards a ref.
	UpdateFastForward UpdateKind = "fast_forward"
	// UpdateRewind rewinds or force-updates a ref.
	UpdateRewind UpdateKind = "rewind"
	// UpdateDelete deletes a ref.
	UpdateDelete UpdateKind = "delete"
)

// updateKinds is the declaration order.
var updateKinds = []UpdateKind{UpdateRead, UpdateCreate, UpdateFastForward, UpdateRewind, UpdateDelete}

// Valid reports whether the kind is one of the declared kinds.
func (kind UpdateKind) Valid() bool {
	for _, candidate := range updateKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// PrincipalRef is one rule principal: `@all`, `@execution`, a current human role
// (`@admin`, `@engineer`, `@reviewer`, `@operator`, `@viewer`) or an explicit
// service ID.
type PrincipalRef string

// roleNames is the role vocabulary behind `@<role>` principals.
var roleNames = []string{"admin", "engineer", "reviewer", "operator", "viewer"}

// validHumanRoleName reports whether value is a declared human role name.
func validHumanRoleName(value string) bool {
	for _, candidate := range roleNames {
		if candidate == value {
			return true
		}
	}
	return false
}

// Valid reports whether the principal reference is usable. A bare role word is
// not a principal: roles need the `@` prefix and services are explicit IDs.
func (principal PrincipalRef) Valid() bool {
	value := string(principal)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "@") {
		role := strings.TrimPrefix(value, "@")
		return role == "all" || role == "execution" || validHumanRoleName(role)
	}
	if validHumanRoleName(value) {
		return false
	}
	return validServiceRef(value)
}

// validServiceRef reports whether value is a usable explicit service ID.
func validServiceRef(value string) bool {
	if value == "" || len(value) > 128 {
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

// Identity is one current principal attempting a ref operation.
type Identity struct {
	Roles     []string `json:"roles,omitempty"`
	Execution bool     `json:"execution,omitempty"`
	ServiceID string   `json:"service_id,omitempty"`
}

// Validate checks that exactly one principal kind is present.
func (identity Identity) Validate() error {
	if identity.Execution {
		if identity.ServiceID != "" || len(identity.Roles) > 0 {
			return fmt.Errorf("execution identity must not carry roles or a service id")
		}
		return nil
	}
	if identity.ServiceID != "" {
		if len(identity.Roles) > 0 {
			return fmt.Errorf("service identity must not carry human roles")
		}
		if strings.HasPrefix(identity.ServiceID, "@") || !validServiceRef(identity.ServiceID) {
			return fmt.Errorf("service id %q is invalid", identity.ServiceID)
		}
		return nil
	}
	if len(identity.Roles) == 0 {
		return fmt.Errorf("identity has no roles, service id or execution flag")
	}
	for _, role := range identity.Roles {
		if !validHumanRoleName(role) {
			return fmt.Errorf("unknown human role %q", role)
		}
	}
	return nil
}

// AccessRequest is one attempted access evaluation.
type AccessRequest struct {
	Identity Identity   `json:"identity"`
	Ref      string     `json:"ref"`
	Update   UpdateKind `json:"update"`
}

// ReadRequest is one local Git read-authorization attempt. Reads are decided at
// repository granularity by the repository's ordered rules; they never use a
// per-ref read rule and there is no `git.ref.read` resource operation.
type ReadRequest struct {
	Identity Identity `json:"identity"`
}

// Validate checks the identity.
func (request ReadRequest) Validate() error {
	return request.Identity.Validate()
}

// Validate checks the identity, anchored ref and update kind.
func (request AccessRequest) Validate() error {
	if err := request.Identity.Validate(); err != nil {
		return err
	}
	if !validRef(request.Ref) {
		return fmt.Errorf("ref %q is not an anchored ref", request.Ref)
	}
	if strings.HasSuffix(request.Ref, "/") {
		return fmt.Errorf("ref %q is a rule prefix, not an attempt ref", request.Ref)
	}
	if !request.Update.Valid() {
		return fmt.Errorf("unknown update kind %q", request.Update)
	}
	return nil
}

// validRef reports whether value is an anchored, non-glob ref. A trailing slash
// marks a prefix rule; the attempt refs themselves never end in a slash.
func validRef(value string) bool {
	if value == "" || !strings.HasPrefix(value, "refs/") {
		return false
	}
	if strings.ContainsAny(value, "*?[]\\ ") {
		return false
	}
	if strings.Contains(value, "..") {
		return false
	}
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "refs" {
		return true
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" {
			return false
		}
	}
	return true
}

// Decision is the outcome of one access evaluation.
type Decision string

const (
	// DecisionAllow permits the attempt.
	DecisionAllow Decision = "allow"
	// DecisionDeny refuses the attempt.
	DecisionDeny Decision = "deny"
)

// Rule is one ordered access rule.
type Rule struct {
	Line       int            `json:"line"`
	Permission Permission     `json:"permission"`
	Ref        string         `json:"ref,omitempty"`
	Principals []PrincipalRef `json:"principals"`
}

// ApprovalRule is one `option approve.<ref> = <role>` requirement.
type ApprovalRule struct {
	Line int    `json:"line"`
	Ref  string `json:"ref"`
	Role string `json:"role"`
}

// RepositoryPolicy is one repository's ordered rules, optional resource
// binding and approval options.
type RepositoryPolicy struct {
	Name       string         `json:"name"`
	ResourceID string         `json:"resource_id,omitempty"`
	Rules      []Rule         `json:"rules"`
	Approvals  []ApprovalRule `json:"approvals,omitempty"`
}

// Config is one parsed tenant git.conf.
type Config struct {
	policies map[string]RepositoryPolicy
	order    []string
}

// Policy returns a repository policy by tenant-relative name.
func (config Config) Policy(name string) (RepositoryPolicy, bool) {
	policy, ok := config.policies[name]
	if !ok {
		return RepositoryPolicy{}, false
	}
	return clonePolicy(policy), true
}

// PolicyNames returns every configured repository in declaration order.
func (config Config) PolicyNames() []string {
	return append([]string(nil), config.order...)
}

// clonePolicy returns a deep copy so a consumer cannot mutate the parsed rules.
func clonePolicy(policy RepositoryPolicy) RepositoryPolicy {
	policy.Rules = append([]Rule(nil), policy.Rules...)
	for index := range policy.Rules {
		policy.Rules[index].Principals = append([]PrincipalRef(nil), policy.Rules[index].Principals...)
	}
	policy.Approvals = append([]ApprovalRule(nil), policy.Approvals...)
	return policy
}
