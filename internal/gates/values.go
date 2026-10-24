package gates

import (
	"regexp"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	idPattern     = regexp.MustCompile(domain.IDPattern)
	commitPattern = regexp.MustCompile(runtimev1.Patterns["GitCommit"])
	digestPattern = regexp.MustCompile(runtimev1.Patterns["Digest"])
	codePattern   = regexp.MustCompile(runtimev1.Patterns["ReasonCode"])
)

// ID identifies a gate. It is module-local: the shared domain package holds
// only the shared identifiers.
type ID string

// Validate checks the opaque identifier syntax.
func (id ID) Validate() error { return validID("gate id", string(id)) }

func validID(kind, v string) error {
	if !idPattern.MatchString(v) {
		return invalidf("%s must match %s", kind, domain.IDPattern)
	}
	return nil
}

// Status is the stored gate state. There are exactly three. Expiry and
// supersession are derived reasons, never statuses.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Decision is what a reviewer asks for.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// Valid reports whether d is a known decision.
func (d Decision) Valid() bool { return d == DecisionApprove || d == DecisionReject }

// Resulting returns the status the decision produces.
func (d Decision) Resulting() Status {
	if d == DecisionApprove {
		return StatusApproved
	}
	return StatusRejected
}

// ActorType identifies who caused a change. It comes from the identity
// authority or the module itself, never from request content.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is an attributable principal.
type Actor struct {
	Type ActorType `json:"type"`
	ID   string    `json:"id"`
}

// Validate checks the actor is usable for attribution.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return invalidf("unknown actor type")
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return invalidf("actor id must be 1..256 bytes")
	}
	return nil
}

// System is the module's own actor for reconciliation.
var System = Actor{Type: ActorService, ID: "gates"}

// Permission names an authorization question. The values equal the identity
// matrix's permission names.
type Permission string

const (
	// PermDecide is the human reviewer's gate decision.
	PermDecide Permission = "gate:decide"
	// PermRead reads gates and their evidence. It maps to evidence:read.
	PermRead Permission = "evidence:read"
)

// Caller is the verified principal and request of a public use case.
type Caller struct {
	PrincipalID string
	RequestID   string
}

// ReviewerScope is the reviewer scope carried by gate.requested: one project
// reviewer decides. There are no quorums and no configurable policy.
const ReviewerScope = "project_reviewer"

// Reason codes of derived applicability.
const (
	ReasonExpired           = "expired"
	ReasonInvalidated       = "invalidated"
	ReasonNotApproved       = "not_approved"
	ReasonRejected          = "rejected"
	ReasonContentChanged    = "content_changed"
	ReasonCandidateChanged  = "candidate_changed"
	ReasonDefinitionChanged = "definition_changed"
	ReasonEvidenceChanged   = "evidence_changed"
	ReasonDeploymentChanged = "deployment_changed"
	ReasonStepNotWaiting    = "step_not_waiting"
	ReasonUncovered         = "uncovered_candidate"
)

func validCode(kind, v string) error {
	if !codePattern.MatchString(v) {
		return invalidf("%s must be a reason code", kind)
	}
	return nil
}

func validDigest(kind, v string) error {
	if !digestPattern.MatchString(v) {
		return invalidf("%s must be sha256:<64 lowercase hex digits>", kind)
	}
	return nil
}

func validCommit(kind, v string) error {
	if !commitPattern.MatchString(v) {
		return invalidf("%s must be a Git commit", kind)
	}
	return nil
}
