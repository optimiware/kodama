package event

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ingressOperation is the registry operation every custom observation commits
// under. It is the same operation name the receipt identity and audit rows use.
const ingressOperation = "event.publish"

// Clock supplies the current time. An event, receipt and audit row carry the
// injected time of the attempt, never a clock read hidden inside an adapter.
type Clock interface {
	Now() time.Time
}

// IngressUnit is the complete atomic custom-observation commit unit.
//
// It is the event-side unit executed by the neutral structured store executor:
// the observation envelope, its durable receipt, the audit rows and the
// publication intent commit together in one transaction. It deliberately
// carries no action or execution state write, so a custom observation can never
// settle or advance a request. It also carries the current-authority reads the
// adapter must re-evaluate inside that same transaction.
type IngressUnit struct {
	Owner       tenancy.Scope             `json:"owner"`
	ExecutionID id.ExecutionID            `json:"execution_id"`
	Event       Envelope                  `json:"event"`
	Receipt     receipt.Record            `json:"receipt"`
	Audit       []Audit                   `json:"audit"`
	Authority   []authorize.AuthorityRead `json:"authority"`
	PostCommit  []PostCommitEffect        `json:"post_commit"`
}

// Validate checks that the unit is one well-formed custom observation and
// nothing else. It refuses a lifecycle or verified-report envelope, a report
// type, an unknown source kind, a scope/owner disagreement, a receipt that is
// not an event.publish receipt for the same caller, authority reads other than
// the applicable event.publish checks for the source kind, an empty or foreign
// audit row, and a post-commit effect other than publishing the committed
// observation.
//
// The source kind is derived from the authenticated principal: an execution
// carries its execution id and the execution_running check; a human (shell) or
// service source carries no execution id and only the membership check.
func (unit IngressUnit) Validate() error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if err := unit.Event.Validate(); err != nil {
		return err
	}
	if unit.Event.Type != TypeCustomObservation {
		return fault.New(fault.InvalidInput, fmt.Sprintf("ingress event type %q is not a custom observation", unit.Event.Type))
	}
	if unit.Event.ReportType != "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("custom observation carries report type %q", unit.Event.ReportType))
	}
	switch unit.Event.Source.Kind {
	case SourceExecution:
		if unit.ExecutionID == "" {
			return fault.New(fault.InvalidInput, "execution source carries no execution id")
		}
		if _, err := id.ParseExecutionID(string(unit.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "ingress execution id is invalid", err)
		}
	case SourceShell, SourceService:
		if unit.ExecutionID != "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("non-execution source %q carries an execution id", unit.Event.Source.Kind))
		}
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event source kind %q", unit.Event.Source.Kind))
	}
	if unit.Event.Scope != unit.Owner {
		return fault.New(fault.InvalidInput, "event scope does not match the unit owner")
	}

	if err := unit.Receipt.Key.Validate(); err != nil {
		return err
	}
	if unit.Receipt.Key.Operation != ingressOperation {
		return fault.New(fault.InvalidInput, fmt.Sprintf("receipt operation %q is not %q", unit.Receipt.Key.Operation, ingressOperation))
	}
	if unit.Receipt.Key.TenantID != unit.Owner.TenantID || unit.Receipt.Key.ProjectID != unit.Owner.ProjectID {
		return fault.New(fault.InvalidInput, "receipt scope does not match the unit owner")
	}
	if unit.Receipt.Key.PrincipalID != unit.Event.Source.PrincipalID {
		return fault.New(fault.InvalidInput, "receipt principal does not match the event source")
	}
	if unit.Receipt.ContentHash == "" {
		return fault.New(fault.InvalidInput, "receipt has no content fingerprint")
	}
	if _, err := id.ParseReceiptID(string(unit.Receipt.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "receipt id is invalid", err)
	}
	if unit.Receipt.At.IsZero() {
		return fault.New(fault.InvalidInput, "receipt time is zero")
	}
	if string(unit.Receipt.Result) != string(unit.Event.ID) {
		return fault.New(fault.InvalidInput, "receipt result does not name the committed event")
	}

	if err := validateIngressAuthority(unit); err != nil {
		return err
	}

	if len(unit.Audit) == 0 {
		return fault.New(fault.InvalidInput, "custom observation carries no audit row")
	}
	for _, audit := range unit.Audit {
		if _, err := id.ParseEventID(string(audit.ID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "audit id is invalid", err)
		}
		if audit.Scope != unit.Owner {
			return fault.New(fault.InvalidInput, "audit scope does not match the unit owner")
		}
		if audit.Actor != unit.Event.Source.PrincipalID {
			return fault.New(fault.InvalidInput, "audit actor does not match the event source")
		}
		if audit.Operation != ingressOperation {
			return fault.New(fault.InvalidInput, fmt.Sprintf("audit operation %q is not %q", audit.Operation, ingressOperation))
		}
		if audit.At.IsZero() {
			return fault.New(fault.InvalidInput, "audit time is zero")
		}
	}

	if len(unit.PostCommit) != 1 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("custom observation has %d post-commit effects; want exactly the publication intent", len(unit.PostCommit)))
	}
	if unit.PostCommit[0].Kind != PostPublishEvent {
		return fault.New(fault.InvalidInput, fmt.Sprintf("custom observation post-commit kind %q is not a publication", unit.PostCommit[0].Kind))
	}
	if unit.PostCommit[0].Target != string(unit.Event.ID) {
		return fault.New(fault.InvalidInput, "custom observation publication target does not match the committed event")
	}
	return nil
}

// validateIngressAuthority checks the unit carries exactly the event.publish
// current-authority reads applicable to its source kind, in registry order.
func validateIngressAuthority(unit IngressUnit) error {
	operation, ok := authorize.LookupOperation(authorize.OperationName(ingressOperation))
	if !ok {
		return fault.New(fault.InternalFailure, "event.publish is not a registered operation")
	}
	checks := ingressAuthorityChecks(operation, unit.Event.Source.Kind)
	if len(unit.Authority) != len(checks) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("custom observation carries %d authority reads; want %d", len(unit.Authority), len(checks)))
	}
	for index, check := range checks {
		read := unit.Authority[index]
		if read.Check != check {
			return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d is %q; want %q", index, read.Check, check))
		}
		if read.Operation != operation.Name {
			return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d names operation %q; want %q", index, read.Operation, operation.Name))
		}
		if !sameRequires(read.Required, operation.Requires) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d does not carry the event.publish grant context", index))
		}
		if read.Scope != unit.Owner {
			return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d scope does not match the unit owner", index))
		}
		if read.PrincipalID != unit.Event.Source.PrincipalID {
			return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d principal does not match the event source", index))
		}
		switch check {
		case authorize.CheckExecutionRunning:
			if read.TargetID != string(unit.ExecutionID) {
				return fault.New(fault.InvalidInput, "execution_running authority target does not match the unit execution")
			}
		default:
			if read.TargetID != "" {
				return fault.New(fault.InvalidInput, fmt.Sprintf("authority read %d carries an unexpected target", index))
			}
		}
	}
	return nil
}

// ingressAuthorityChecks returns the event.publish current-authority checks that
// apply to one source kind. Membership applies to every actor; execution_running
// is an execution-only fact, so a human or service source never carries it.
func ingressAuthorityChecks(operation authorize.Operation, kind SourceKind) []authorize.AuthorityCheck {
	if kind == SourceExecution {
		return append([]authorize.AuthorityCheck(nil), operation.Authority...)
	}
	checks := make([]authorize.AuthorityCheck, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		if check == authorize.CheckExecutionRunning {
			continue
		}
		checks = append(checks, check)
	}
	return checks
}

// sameRequires compares two operation grant sets.
func sameRequires(left, right authorize.Requires) bool {
	if left.Execution != right.Execution {
		return false
	}
	if len(left.HumanRoles) != len(right.HumanRoles) || len(left.ServiceRoles) != len(right.ServiceRoles) {
		return false
	}
	for index, role := range left.HumanRoles {
		if right.HumanRoles[index] != role {
			return false
		}
	}
	for index, role := range left.ServiceRoles {
		if right.ServiceRoles[index] != role {
			return false
		}
	}
	return true
}

// IngressResult reports only the committed identities and the replay fact. It
// never reports a decision or a state transition.
type IngressResult struct {
	EventID   id.EventID   `json:"event_id"`
	ReceiptID id.ReceiptID `json:"receipt_id"`
	Replayed  bool         `json:"replayed"`
}

// IngressPort applies one complete custom-observation commit unit.
//
// The adapter must validate the unit, deduplicate by the durable receipt,
// re-read the supplied current-authority facts, and commit the event row,
// receipt, audit rows and publication intent in one transaction. An equal retry
// is answered with the original committed identities; changed content is
// refused. The unit contains no action or execution state write.
type IngressPort interface {
	Ingest(context.Context, IngressUnit) (IngressResult, error)
}

// PublishAttempt is one custom observation submission. The presented credential
// is authenticated by the Shell boundary and never stored. Scope and Source are
// optional body claims: when present they must exactly equal the
// credential-derived values, and they never override them.
type PublishAttempt struct {
	Credential     string          `json:"-"`
	CallerKey      string          `json:"caller_key"`
	Content        receipt.Content `json:"content"`
	Type           string          `json:"type"`
	Scope          tenancy.Scope   `json:"scope,omitempty"`
	Source         Source          `json:"source,omitempty"`
	SubjectID      string          `json:"subject_id"`
	SubjectVersion uint64          `json:"subject_version,omitempty"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	CausationID    string          `json:"causation_id,omitempty"`
	Data           []byte          `json:"data,omitempty"`
}

// IngressConfig is the complete ingress composition. Every dependency is
// required; NewIngress refuses a partial composition instead of guessing a
// substitute.
type IngressConfig struct {
	Credentials  authorize.CurrentPrincipal
	Authority    authorize.AuthorityReader
	Authorizer   authorize.Authorizer
	Port         IngressPort
	Clock        Clock
	NewEventID   func() (id.EventID, error)
	NewReceiptID func() (id.ReceiptID, error)
}

// Ingress orchestrates one authenticated custom-observation publication. It
// holds only its immutable dependencies, so concurrent attempts share no
// per-attempt state and every retry authenticates and re-reads current
// authority afresh.
type Ingress struct {
	credentials  authorize.CurrentPrincipal
	authority    authorize.AuthorityReader
	authorizer   authorize.Authorizer
	port         IngressPort
	clock        Clock
	newEventID   func() (id.EventID, error)
	newReceiptID func() (id.ReceiptID, error)
}

// NewIngress validates the composition and returns an ingress.
func NewIngress(config IngressConfig) (*Ingress, error) {
	switch {
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires a current-principal resolver")
	case config.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires an authority reader")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires an authorizer")
	case config.Port == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires an ingress port")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires a clock")
	case config.NewEventID == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires an event identity generator")
	case config.NewReceiptID == nil:
		return nil, fault.New(fault.InvalidInput, "ingress requires a receipt identity generator")
	}
	return &Ingress{
		credentials:  config.Credentials,
		authority:    config.Authority,
		authorizer:   config.Authorizer,
		port:         config.Port,
		clock:        config.Clock,
		newEventID:   config.NewEventID,
		newReceiptID: config.NewReceiptID,
	}, nil
}

// Publish authenticates the presenting principal credential, derives the event
// source and scope from that authenticated principal, refuses body
// scope/source spoofing and lifecycle-type impersonation, re-reads the
// event.publish authority facts, and commits one custom-observation unit. An
// execution credential is admitted by its execution permission; a human or
// service credential is admitted only by the registry's engineer grant through
// the shared matrix.
//
// A custom observation can never settle or advance a request: the unit carries
// only the event, receipt, audit rows, authority reads and publication intent.
// An equal retry is answered with the original committed identities; changed
// content is refused with changed_idempotency_content. An ambiguous adapter
// outcome is an error, never a successful publication.
func (ingress *Ingress) Publish(ctx context.Context, attempt PublishAttempt) (IngressResult, error) {
	if err := validateAttempt(attempt); err != nil {
		return IngressResult{}, err
	}

	principal, err := ingress.credentials.Resolve(ctx, attempt.Credential)
	if err != nil {
		return IngressResult{}, err
	}
	source, err := ingress.authorizeActor(ctx, principal)
	if err != nil {
		return IngressResult{}, err
	}
	if err := checkBodyClaims(attempt, principal, source); err != nil {
		return IngressResult{}, err
	}
	if err := checkObservationType(attempt.Type); err != nil {
		return IngressResult{}, err
	}

	now := ingress.clock.Now()

	eventID, err := ingress.newEventID()
	if err != nil {
		return IngressResult{}, fault.Wrap(fault.InternalFailure, "event identity generator failed", err)
	}
	if _, err := id.ParseEventID(string(eventID)); err != nil {
		return IngressResult{}, fault.Wrap(fault.InternalFailure, "event identity generator returned an invalid id", err)
	}

	envelope, err := NewEnvelope(eventID, source, principal.Scope, now, EnvelopeInput{
		Type:           TypeCustomObservation,
		SubjectID:      attempt.SubjectID,
		SubjectVersion: attempt.SubjectVersion,
		CorrelationID:  attempt.CorrelationID,
		CausationID:    attempt.CausationID,
		Data:           attempt.Data,
	})
	if err != nil {
		return IngressResult{}, err
	}

	receiptID, err := ingress.newReceiptID()
	if err != nil {
		return IngressResult{}, fault.Wrap(fault.InternalFailure, "receipt identity generator failed", err)
	}
	if _, err := id.ParseReceiptID(string(receiptID)); err != nil {
		return IngressResult{}, fault.Wrap(fault.InternalFailure, "receipt identity generator returned an invalid id", err)
	}

	contentHash, err := receipt.Fingerprint(attempt.Content)
	if err != nil {
		return IngressResult{}, err
	}
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    principal.Scope.TenantID,
			ProjectID:   principal.Scope.ProjectID,
			PrincipalID: principal.ID,
			Operation:   ingressOperation,
			CallerKey:   attempt.CallerKey,
		},
		ContentHash: contentHash,
		Result:      []byte(eventID),
		At:          now,
	}

	audit := Audit{
		ID:        eventID,
		Scope:     principal.Scope,
		Actor:     principal.ID,
		Operation: ingressOperation,
		At:        now,
	}

	reads := ingressAuthorityReads(principal, source.Kind)
	facts, err := ingress.authority.Read(ctx, reads)
	if err != nil {
		return IngressResult{}, err
	}
	if err := requireAllowed(facts, reads); err != nil {
		return IngressResult{}, err
	}

	unit := IngressUnit{
		Owner:       principal.Scope,
		ExecutionID: executionIDFor(principal),
		Event:       envelope,
		Receipt:     record,
		Audit:       []Audit{audit},
		Authority:   reads,
		PostCommit:  []PostCommitEffect{{Kind: PostPublishEvent, Target: string(eventID)}},
	}
	if err := unit.Validate(); err != nil {
		return IngressResult{}, err
	}

	return ingress.port.Ingest(ctx, unit)
}

// validateAttempt checks the structural request shape before any dependency is
// touched. The credential itself is left to the authentication boundary: an
// empty or unverifiable token is that resolver's refusal, not a shape error.
func validateAttempt(attempt PublishAttempt) error {
	if attempt.CallerKey == "" {
		return fault.New(fault.InvalidInput, "publish caller key is empty")
	}
	if _, err := receipt.Fingerprint(attempt.Content); err != nil {
		return err
	}
	if len(attempt.Data) > DefaultBounds.MaxDataBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("event data is %d bytes; at most %d are allowed", len(attempt.Data), DefaultBounds.MaxDataBytes))
	}
	return nil
}

// authorizeActor derives the authenticated event source and applies the
// event.publish grant. An execution principal is admitted by its execution
// permission (the registry row declares own_events) and must carry its
// execution id. A human or service principal is admitted only through the
// shared matrix's engineer grant in the authenticated scope. The source and
// scope always come from the authenticated principal, never from the body.
func (ingress *Ingress) authorizeActor(ctx context.Context, principal identity.Principal) (Source, error) {
	if err := principal.Validate(); err != nil {
		return Source{}, fault.Wrap(fault.Unauthenticated, "resolved principal is invalid", err)
	}
	if !principal.Enabled {
		return Source{}, fault.New(fault.Forbidden, "principal is currently disabled")
	}
	operation, err := registryOperation(authorize.OperationName(ingressOperation))
	if err != nil {
		return Source{}, err
	}
	switch principal.Kind {
	case identity.KindExecution:
		if operation.Requires.Execution == authorize.ExecNone {
			return Source{}, fault.New(fault.Forbidden, "execution principal has no permission for event.publish")
		}
		if principal.ExecutionID == "" {
			return Source{}, fault.New(fault.Forbidden, "execution principal carries no execution id")
		}
		return Source{Kind: SourceExecution, PrincipalID: principal.ID}, nil
	case identity.KindHuman:
		if err := ingress.authorizeGrant(ctx, principal, operation); err != nil {
			return Source{}, err
		}
		return Source{Kind: SourceShell, PrincipalID: principal.ID}, nil
	case identity.KindService:
		if err := ingress.authorizeGrant(ctx, principal, operation); err != nil {
			return Source{}, err
		}
		return Source{Kind: SourceService, PrincipalID: principal.ID}, nil
	default:
		return Source{}, fault.New(fault.Forbidden, fmt.Sprintf("principal kind %q cannot publish custom observations", principal.Kind))
	}
}

// authorizeGrant applies the shared matrix to a human or service publisher in
// the authenticated scope.
func (ingress *Ingress) authorizeGrant(ctx context.Context, principal identity.Principal, operation authorize.Operation) error {
	decision, err := ingress.authorizer.Authorize(ctx, principal, operation.Name, principal.Scope)
	if err != nil {
		return err
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", operation.Name))
	}
	return nil
}

// executionIDFor retains the execution identity only for an execution source.
func executionIDFor(principal identity.Principal) id.ExecutionID {
	if principal.Kind == identity.KindExecution {
		return principal.ExecutionID
	}
	return ""
}

// checkBodyClaims refuses a body that tries to choose the event's ownership
// scope or producer identity. Absent claims are filled from the authenticated
// caller; present claims must match it exactly.
func checkBodyClaims(attempt PublishAttempt, principal identity.Principal, source Source) error {
	if attempt.Scope != (tenancy.Scope{}) && attempt.Scope != principal.Scope {
		return fault.New(fault.Forbidden, "request body scope does not match the authenticated scope")
	}
	if attempt.Source != (Source{}) && attempt.Source != source {
		return fault.New(fault.Forbidden, "request body source does not match the authenticated principal")
	}
	return nil
}

// checkObservationType refuses an unknown name as invalid input and any known
// lifecycle or verified-report name as forbidden. A custom observation can
// never impersonate a lifecycle or decision fact, including the internal
// request.activated alias.
func checkObservationType(value string) error {
	canonical, ok := api.CanonicalEventType(value)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event type %q", value))
	}
	if canonical != string(TypeCustomObservation) {
		return fault.New(fault.Forbidden, fmt.Sprintf("event type %q is not a custom observation", value))
	}
	return nil
}

// ingressAuthorityReads builds the applicable event.publish current-authority
// reads in registry order. The adapter re-evaluates them inside the commit, so
// an allow observed here is never durable permission. Membership applies to
// every source kind; execution_running is execution-only.
func ingressAuthorityReads(principal identity.Principal, kind SourceKind) []authorize.AuthorityRead {
	operation, err := registryOperation(authorize.OperationName(ingressOperation))
	if err != nil {
		return nil
	}
	checks := ingressAuthorityChecks(operation, kind)
	reads := make([]authorize.AuthorityRead, 0, len(checks))
	for _, check := range checks {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       principal.Scope,
			PrincipalID: principal.ID,
		}
		if check == authorize.CheckExecutionRunning {
			read.TargetID = string(principal.ExecutionID)
		}
		reads = append(reads, read)
	}
	return reads
}

// requireAllowed refuses the attempt when any current-authority fact does not
// hold. A missing or mismatched fact is a dependency-contract failure rather
// than an implicit allow.
func requireAllowed(facts []authorize.AuthorityFact, reads []authorize.AuthorityRead) error {
	if len(facts) != len(reads) {
		return fault.New(fault.InternalFailure, fmt.Sprintf("authority reader returned %d facts for %d reads", len(facts), len(reads)))
	}
	for index, fact := range facts {
		read := reads[index]
		if fact.Check != read.Check || fact.Scope != read.Scope ||
			fact.PrincipalID != read.PrincipalID || fact.TargetID != read.TargetID {
			return fault.New(fault.InternalFailure, fmt.Sprintf("authority fact %d does not match the requested read", index))
		}
		if !fact.Allowed {
			return fault.New(fault.Forbidden, fmt.Sprintf("current authority %q does not hold", read.Check))
		}
	}
	return nil
}
