// Package request declares request assignments, response schemas and the two
// atomic request commit units.
//
// A request is a running action with a fixed immutable subject, an assignment
// and a deadline. It has no workload and no execution credential. Activation
// and resolution each commit as one unit: current authority/version checks,
// state settlement, credential clearing or issuance, receipt, audit, event
// publication intent and post-commit effects.
package request

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// AssignmentKind names the two request/task-question assignee forms.
type AssignmentKind string

const (
	// AssignmentRole assigns to every human currently holding the role.
	AssignmentRole AssignmentKind = "role"
	// AssignmentService assigns to exactly one service account.
	AssignmentService AssignmentKind = "service"
)

// Assignment is the immutable assignee of one request or task question.
type Assignment struct {
	Kind      AssignmentKind    `json:"kind"`
	Role      tenancy.Role      `json:"role,omitempty"`
	ServiceID service.ServiceID `json:"service_id,omitempty"`
}

// ParseAssignment accepts exactly "role/<role>" or "service/<id>". It performs
// no trimming and accepts no whitespace, empty segment or extra segment.
func ParseAssignment(value string) (Assignment, error) {
	kind, rest, found := strings.Cut(value, "/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return Assignment{}, fault.New(fault.InvalidInput, fmt.Sprintf("invalid assignment %q", value))
	}
	switch AssignmentKind(kind) {
	case AssignmentRole:
		role := tenancy.Role(rest)
		if !role.Valid() {
			return Assignment{}, fault.New(fault.InvalidInput, fmt.Sprintf("invalid assignment role %q", rest))
		}
		return Assignment{Kind: AssignmentRole, Role: role}, nil
	case AssignmentService:
		serviceID, err := service.ParseServiceID(rest)
		if err != nil {
			return Assignment{}, fault.Wrap(fault.InvalidInput, "invalid assignment service id", err)
		}
		return Assignment{Kind: AssignmentService, ServiceID: serviceID}, nil
	default:
		return Assignment{}, fault.New(fault.InvalidInput, fmt.Sprintf("invalid assignment kind %q", kind))
	}
}

// String renders the canonical assignment grammar.
func (assignment Assignment) String() string {
	switch assignment.Kind {
	case AssignmentRole:
		return "role/" + string(assignment.Role)
	case AssignmentService:
		return "service/" + string(assignment.ServiceID)
	default:
		return ""
	}
}

// Valid reports whether the assignment names exactly one assignee of one kind.
func (assignment Assignment) Valid() bool {
	switch assignment.Kind {
	case AssignmentRole:
		return assignment.Role.Valid() && assignment.ServiceID == ""
	case AssignmentService:
		return assignment.Role == "" && assignment.ServiceID != ""
	default:
		return false
	}
}

// Decision is the authorized answer to a request action.
type Decision string

const (
	// DecisionPass succeeds the request action.
	DecisionPass Decision = "pass"
	// DecisionFail fails the request action while retaining the answer.
	DecisionFail Decision = "fail"
)

// Valid reports whether the decision is one of the declared decisions.
func (decision Decision) Valid() bool {
	return decision == DecisionPass || decision == DecisionFail
}

// KernelDecision maps the Shell decision to the Kernel decision carried by the
// flow.AnswerFact and the resulting answer reduction. It reports false for an
// undeclared decision, so a caller can refuse it instead of guessing.
func (decision Decision) KernelDecision() (kernel.Decision, bool) {
	switch decision {
	case DecisionPass:
		return kernel.DecisionPass, true
	case DecisionFail:
		return kernel.DecisionFail, true
	default:
		return "", false
	}
}

// ResponseSchema is the declared response shape of one request action. Keys is
// the complete allowed field set; Required is a subset that must be present.
type ResponseSchema struct {
	Keys     []id.OutputKey `json:"keys,omitempty"`
	Required []id.OutputKey `json:"required,omitempty"`
}

// Validate enforces the declared response keys and required response fields.
// Undeclared fields, missing required fields and a required key outside the
// declared set use invalid_input.
func (schema ResponseSchema) Validate(response kernel.Outputs) error {
	declared := make(map[id.OutputKey]bool, len(schema.Keys))
	for _, key := range schema.Keys {
		if _, err := id.ParseOutputKey(string(key)); err != nil {
			return fault.Wrap(fault.InvalidInput, "declared response key is invalid", err)
		}
		if declared[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("declared response key %q is duplicated", key))
		}
		declared[key] = true
	}
	for _, key := range schema.Required {
		if !declared[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("required response key %q is not declared", key))
		}
	}
	for key := range response {
		if !declared[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("response key %q is not declared", key))
		}
	}
	for _, key := range schema.Required {
		if _, present := response[key]; !present {
			return fault.New(fault.InvalidInput, fmt.Sprintf("required response key %q is missing", key))
		}
	}
	return nil
}

// ActivationUnit is the complete atomic request-activation commit unit.
//
// It carries the confirmed stop evidence, the resolved immutable subject and
// assignment, the persisted deadline, the credential clearing, the Kernel
// ActivateFact reduction (Commit), the receipt, audit, event publication
// intent, current-authority reads and post-commit effects. Activation must
// never run before all prior workloads are confirmed stopped. The adapter
// persists exactly the reduction and appends
// event.ExcludeOwned(event.ProjectLifecycle(pre, Commit), action.requested)
// to the owner's action.requested event, so the pending->running
// action.transition is not dropped on this path.
type ActivationUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	ExecutionID              id.ExecutionID            `json:"execution_id"`
	ActionID                 id.ActionID               `json:"action_id"`
	ExpectedExecutionVersion uint64                    `json:"expected_execution_version"`
	ExpectedActionVersion    uint64                    `json:"expected_action_version"`
	Commit                   flow.Commit               `json:"commit"`
	Subject                  definition.Subject        `json:"subject"`
	Assignment               Assignment                `json:"assignment"`
	Timeout                  time.Duration             `json:"timeout"`
	Deadline                 time.Time                 `json:"deadline"`
	At                       time.Time                 `json:"at"`
	StopEvidence             kernel.StopEvidence       `json:"stop_evidence"`
	CredentialUpdate         *token.Credential         `json:"credential_update,omitempty"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// ActivationResult is the committed activation result.
type ActivationResult struct {
	ExecutionVersion uint64    `json:"execution_version"`
	ActionVersion    uint64    `json:"action_version"`
	Deadline         time.Time `json:"deadline"`
	Replayed         bool      `json:"replayed"`
}

// ResolveUnit is the complete atomic request-resolution commit unit.
//
// Decision and Response are the authorized answer; Actor is the verified
// respondent; IdempotencyKey is the immutable retry identity. Commit is the
// Kernel reduction produced by flow.Reduce with the flow.AnswerFact for this
// action: the Shell never re-derives action/execution state, the adapter
// persists the reduction atomically with the receipt, audit, event publication
// intent and post-commit effects. The adapter rechecks the expected versions,
// the current assignment authority and the authoritative deadline before
// settling, and its receipt-first replay check runs before the reduction is
// required or applied, so an identical retry returns the original result
// instead of a second settlement.
type ResolveUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	ExecutionID              id.ExecutionID            `json:"execution_id"`
	ActionID                 id.ActionID               `json:"action_id"`
	ExpectedExecutionVersion uint64                    `json:"expected_execution_version"`
	ExpectedActionVersion    uint64                    `json:"expected_action_version"`
	Decision                 Decision                  `json:"decision"`
	Response                 kernel.Outputs            `json:"response,omitempty"`
	Actor                    id.PrincipalID            `json:"actor"`
	Assignment               Assignment                `json:"assignment"`
	At                       time.Time                 `json:"at"`
	IdempotencyKey           string                    `json:"idempotency_key"`
	Commit                   flow.Commit               `json:"commit"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// Validate is the first-settlement validation of the Kernel answer reduction
// that settles the action. It requires the reduction, checks that it targets
// exactly this execution and action with a declared next state and an
// authoritative time, and binds the reduction to the unit: the answer decision
// is the Kernel decision of unit.Decision, the answer actor is the unit's
// verified respondent, the commit's expected versions are the unit's expected
// versions, and the next states are the pair flow.Reduce produces for that
// decision (pass commits a succeeded action with a running or succeeded
// execution; fail commits a failed action and execution). A unit that carries
// no reduction, or a reduction that does not belong to this unit, would let the
// adapter apply state the service never authorized, so it is refused with
// invalid_input. A retry of an already-answered action carries no reduction and
// uses ValidateReplay instead.
func (unit ResolveUnit) Validate() error {
	if unit.Commit.ExecutionID == "" || unit.Commit.ActionID == "" {
		return fault.New(fault.InvalidInput, "resolve unit carries no kernel answer commit")
	}
	if unit.Commit.ExecutionID != unit.ExecutionID {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer commit targets execution %q, not %q", unit.Commit.ExecutionID, unit.ExecutionID))
	}
	if unit.Commit.ActionID != unit.ActionID {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer commit targets action %q, not %q", unit.Commit.ActionID, unit.ActionID))
	}
	if unit.Commit.Answer == nil {
		return fault.New(fault.InvalidInput, "kernel answer commit carries no answer")
	}
	if !unit.Commit.NextExecutionState.Valid() || !unit.Commit.NextActionState.Valid() {
		return fault.New(fault.InvalidInput, "kernel answer commit carries an unknown next state")
	}
	if unit.Commit.At.IsZero() {
		return fault.New(fault.InvalidInput, "kernel answer commit has no authoritative time")
	}
	kernelDecision, ok := unit.Decision.KernelDecision()
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("resolve unit carries unknown decision %q", unit.Decision))
	}
	if unit.Commit.Answer.Decision != kernelDecision {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer decision %q does not match resolve decision %q", unit.Commit.Answer.Decision, unit.Decision))
	}
	if unit.Commit.Answer.Actor != unit.Actor {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer actor %q does not match resolve actor %q", unit.Commit.Answer.Actor, unit.Actor))
	}
	if unit.Commit.ExpectedExecutionVersion != unit.ExpectedExecutionVersion {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer commit expects execution version %d, not %d", unit.Commit.ExpectedExecutionVersion, unit.ExpectedExecutionVersion))
	}
	if unit.Commit.ExpectedActionVersion != unit.ExpectedActionVersion {
		return fault.New(fault.InvalidInput, fmt.Sprintf("kernel answer commit expects action version %d, not %d", unit.Commit.ExpectedActionVersion, unit.ExpectedActionVersion))
	}
	switch kernelDecision {
	case kernel.DecisionPass:
		if unit.Commit.NextActionState != kernel.ActionSucceeded {
			return fault.New(fault.InvalidInput, fmt.Sprintf("passing kernel answer commits next action state %q, not %q", unit.Commit.NextActionState, kernel.ActionSucceeded))
		}
		if unit.Commit.NextExecutionState != kernel.ExecutionRunning && unit.Commit.NextExecutionState != kernel.ExecutionSucceeded {
			return fault.New(fault.InvalidInput, fmt.Sprintf("passing kernel answer commits next execution state %q, not running or succeeded", unit.Commit.NextExecutionState))
		}
	case kernel.DecisionFail:
		if unit.Commit.NextActionState != kernel.ActionFailed {
			return fault.New(fault.InvalidInput, fmt.Sprintf("failing kernel answer commits next action state %q, not %q", unit.Commit.NextActionState, kernel.ActionFailed))
		}
		if unit.Commit.NextExecutionState != kernel.ExecutionFailed {
			return fault.New(fault.InvalidInput, fmt.Sprintf("failing kernel answer commits next execution state %q, not %q", unit.Commit.NextExecutionState, kernel.ExecutionFailed))
		}
	}
	return nil
}

// ValidateReplay is the receipt-first replay validation. An equal-content
// retry of an action flow.Reduce already answered yields no reduction, so the
// replay unit legitimately carries a zero Commit. It still must carry the
// identity the adapter needs for the receipt lookup: the owning scope, the
// execution and action identities, non-zero expected versions, the idempotency
// key and a well-formed receipt key in that scope for this actor and caller
// key. A unit that does carry a reduction is validated as a first settlement.
//
// The adapter looks up the receipt first. An equal-content hit returns the
// original result and applies nothing; changed content is refused with
// changed_idempotency_content; only a miss reaches first settlement and applies
// Validate. A miss with a zero Commit is refused because there is no settled
// answer to return and no reduction to apply.
func (unit ResolveUnit) ValidateReplay() error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if unit.ExecutionID == "" || unit.ActionID == "" {
		return fault.New(fault.InvalidInput, "resolve replay unit carries no execution/action identity")
	}
	if unit.IdempotencyKey == "" {
		return fault.New(fault.InvalidInput, "resolve replay unit carries no idempotency key")
	}
	if unit.ExpectedExecutionVersion == 0 || unit.ExpectedActionVersion == 0 {
		return fault.New(fault.InvalidInput, "resolve replay unit carries no expected versions")
	}
	if err := unit.Receipt.Key.Validate(); err != nil {
		return err
	}
	if unit.Receipt.Key.TenantID != unit.Owner.TenantID || unit.Receipt.Key.ProjectID != unit.Owner.ProjectID {
		return fault.New(fault.InvalidInput, "resolve replay receipt key is outside the unit scope")
	}
	if unit.Receipt.Key.PrincipalID != unit.Actor {
		return fault.New(fault.InvalidInput, "resolve replay receipt key names another principal")
	}
	if unit.Receipt.Key.CallerKey != unit.IdempotencyKey {
		return fault.New(fault.InvalidInput, "resolve replay receipt key does not match the idempotency key")
	}
	if unit.Commit.ExecutionID != "" || unit.Commit.ActionID != "" || unit.Commit.Answer != nil {
		return unit.Validate()
	}
	return nil
}

// ResolveResult is the committed resolution result.
type ResolveResult struct {
	ExecutionVersion uint64   `json:"execution_version"`
	ActionVersion    uint64   `json:"action_version"`
	Decision         Decision `json:"decision"`
	Result           []byte   `json:"result,omitempty"`
	Replayed         bool     `json:"replayed"`
}
