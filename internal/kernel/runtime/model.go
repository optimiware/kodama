package runtime

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Sentinel errors. Every validation failure additionally wraps
// domain.ErrInvalidValue.
var (
	// ErrIllegalTransition reports a state change the model does not allow.
	ErrIllegalTransition = errors.New("runtime: illegal transition")
	// ErrExecutionClosed reports that a run no longer admits new execution
	// because it was cancelled or has settled.
	ErrExecutionClosed = errors.New("runtime: execution closed")
	// ErrStaleFence reports a writer whose fence was superseded by a takeover.
	ErrStaleFence = errors.New("runtime: stale fence")
	// ErrClaimHeld reports a claim that another owner still holds.
	ErrClaimHeld = errors.New("runtime: claim held")
	// ErrClaimExpired reports a claim whose lease has run out.
	ErrClaimExpired = errors.New("runtime: claim expired")
	// ErrWaitClosed reports a request for a wait that is already decided.
	ErrWaitClosed = errors.New("runtime: wait closed")
	// ErrWaitExpired reports a signal that arrived at or after the deadline.
	ErrWaitExpired = errors.New("runtime: wait deadline passed")
	// ErrWaitNotOpen reports a signal for a wait that was not yet opened.
	ErrWaitNotOpen = errors.New("runtime: wait not open")
	// ErrWaitStale reports a signal issued against an older wait version.
	ErrWaitStale = errors.New("runtime: stale wait version")
	// ErrWaitForeign reports a request whose tenant, project, wait identity or
	// correlation does not belong to the wait it was applied to. Nothing is changed.
	ErrWaitForeign = errors.New("runtime: foreign wait")
	// ErrInvalidOutcome reports an outcome the wait did not declare.
	ErrInvalidOutcome = errors.New("runtime: outcome not declared by the wait")
	// ErrKeyReuse reports a caller key already recorded with a different request.
	// It is a response to the caller only: it creates no record, emits no event
	// and never changes the recorded result.
	ErrKeyReuse = errors.New("runtime: key_reuse")
	// ErrRequiresUnmet reports a protected step whose required waits are not all
	// decided with their approving outcome.
	ErrRequiresUnmet = errors.New("runtime: requires unmet")
	// ErrWaitNotDue reports an expiry request before the deadline.
	ErrWaitNotDue = errors.New("runtime: wait not due")
	// ErrNotReached marks an external effect as affirmatively known not to
	// have reached its backend. Only such an effect may be retried.
	ErrNotReached = errors.New("runtime: effect not reached")
	// ErrUncertain marks an external effect whose outcome is unknown.
	ErrUncertain = errors.New("runtime: effect outcome uncertain")
	// ErrSpecMismatch reports a stable identity that already exists with a
	// different immutable specification.
	ErrSpecMismatch = errors.New("runtime: specification mismatch")
	// ErrMaterializationRevoked reports a Resolve for an identity that was
	// revoked. The resolver publishes nothing for it.
	ErrMaterializationRevoked = errors.New("runtime: materialization revoked")
	// ErrCapability reports a backend that cannot satisfy required capabilities.
	ErrCapability = errors.New("runtime: capability unsatisfied")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{domain.ErrInvalidValue}, args...)...)
}

var codeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func validCode(kind, v string) error {
	if !codeRE.MatchString(v) {
		return invalid("%s %q must be lower_snake_case", kind, v)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Codes
// ---------------------------------------------------------------------------

// Reason is a stable lower_snake_case code explaining a failure, skip,
// cancellation or uncertainty. It is bounded and carries no free text.
type Reason string

// Reason codes recorded on steps, runs, commands, observations and events.
const (
	ReasonNone                  Reason = ""
	ReasonCommandFailed         Reason = "command_failed"
	ReasonStepTimeout           Reason = "step_timeout"
	ReasonWaitExpired           Reason = "wait_expired"
	ReasonCancelled             Reason = "cancelled"
	ReasonDependencyFailed      Reason = "dependency_failed"
	ReasonConditionFalse        Reason = "condition_false"
	ReasonPrerequisiteSkipped   Reason = "prerequisite_skipped"
	ReasonRequiresUnmet         Reason = "requires_unmet"
	ReasonRunFailed             Reason = "run_failed"
	ReasonRunCancelled          Reason = "run_cancelled"
	ReasonCorrelationMissing    Reason = "correlation_missing"
	ReasonLaunchNotReached      Reason = "launch_not_reached"
	ReasonLaunchUncertain       Reason = "launch_uncertain"
	ReasonStopUncertain         Reason = "stop_uncertain"
	ReasonWorkloadLost          Reason = "workload_lost"
	ReasonServiceLost           Reason = "service_lost"
	ReasonWorkspaceLost         Reason = "workspace_lost"
	ReasonSecretUnavailable     Reason = "secret_unavailable"
	ReasonCapabilityUnsatisfied Reason = "capability_unsatisfied"
	ReasonPreparationFailed     Reason = "preparation_failed"
	ReasonCleanupFailed         Reason = "cleanup_failed"
	ReasonCleanupExhausted      Reason = "cleanup_exhausted"
)

// Validate reports whether the reason is a well-formed code. The empty reason
// is valid and means none.
func (r Reason) Validate() error {
	if r == ReasonNone {
		return nil
	}
	return validCode("reason", string(r))
}

// ResultCode is how a settled workload ended, as observed from its backend.
type ResultCode string

// Result codes.
const (
	ResultSucceeded ResultCode = "succeeded"
	ResultFailed    ResultCode = "failed"
	ResultCancelled ResultCode = "cancelled"
	ResultTimedOut  ResultCode = "timed_out"
	ResultLost      ResultCode = "lost"
)

// Valid reports whether c is a known result code.
func (c ResultCode) Valid() bool {
	switch c {
	case ResultSucceeded, ResultFailed, ResultCancelled, ResultTimedOut, ResultLost:
		return true
	}
	return false
}

// Result is the settled outcome of one workload attempt.
type Result struct {
	Code     ResultCode `json:"code"`
	ExitCode int        `json:"exitCode,omitempty"`
	Reason   Reason     `json:"reason,omitempty"`
}

// Validate checks the result.
func (r Result) Validate() error {
	if !r.Code.Valid() {
		return invalid("result code %q", r.Code)
	}
	if r.Code == ResultSucceeded && (r.ExitCode != 0 || r.Reason != ReasonNone) {
		return invalid("a succeeded result has exit code 0 and no reason")
	}
	return r.Reason.Validate()
}

// ---------------------------------------------------------------------------
// Fence and claim
// ---------------------------------------------------------------------------

// Fence is a monotonic per-run counter incremented on every claim takeover.
// The zero value is invalid.
type Fence uint64

// Validate rejects the zero fence.
func (f Fence) Validate() error {
	if f == 0 {
		return invalid("fence must be positive")
	}
	return nil
}

// CheckFence compares a writer's fence with the run's current fence. An older
// fence is a stale writer superseded by a takeover; a newer fence is impossible
// and rejected as invalid.
func CheckFence(held, current Fence) error {
	if err := held.Validate(); err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		return err
	}
	switch {
	case held < current:
		return fmt.Errorf("%w: held %d, current %d", ErrStaleFence, held, current)
	case held > current:
		return invalid("fence %d is ahead of current %d", held, current)
	}
	return nil
}

// ClaimPurpose names why a run is claimed. Cancellation blocks claims for
// execution but not for stopping, removal and reconciliation.
type ClaimPurpose string

// Claim purposes.
const (
	// ClaimExecute progresses steps: launching, observing and choosing the next step.
	ClaimExecute ClaimPurpose = "execute"
	// ClaimReconcile adopts, observes or stops workloads and resources after
	// a restart, lost response or expired lease.
	ClaimReconcile ClaimPurpose = "reconcile"
	// ClaimCleanup stops and removes owned resources after execution ended.
	ClaimCleanup ClaimPurpose = "cleanup"
)

// Valid reports whether p is a known purpose.
func (p ClaimPurpose) Valid() bool {
	return p == ClaimExecute || p == ClaimReconcile || p == ClaimCleanup
}

// Claim is a lease on one run: owner identity, expiry and a monotonic fence.
// The zero value is an inactive claim that holds nothing.
type Claim struct {
	Scope   domain.Scope `json:"scope"`
	RunID   domain.RunID `json:"runId"`
	Owner   string       `json:"owner"`
	Purpose ClaimPurpose `json:"purpose"`
	Fence   Fence        `json:"fence"`
	Expires time.Time    `json:"expires"`
}

// ClaimRequest asks a StateStore to claim a run.
type ClaimRequest struct {
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	Owner   string        `json:"owner"`
	Purpose ClaimPurpose  `json:"purpose"`
	TTL     time.Duration `json:"ttl"`
}

// Validate checks the request.
func (r ClaimRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Owner) == "" {
		return invalid("claim owner is required")
	}
	if !r.Purpose.Valid() {
		return invalid("claim purpose %q", r.Purpose)
	}
	if r.TTL <= 0 {
		return invalid("claim ttl must be positive")
	}
	return nil
}

// Validate checks a held claim.
func (c Claim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.RunID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Owner) == "" {
		return invalid("claim owner is required")
	}
	if !c.Purpose.Valid() {
		return invalid("claim purpose %q", c.Purpose)
	}
	if err := c.Fence.Validate(); err != nil {
		return err
	}
	if c.Expires.IsZero() {
		return invalid("claim expiry is required")
	}
	return nil
}

// Active reports whether the claim is held at now. The zero claim is never active.
func (c Claim) Active(now time.Time) bool {
	return c.Fence != 0 && now.Before(c.Expires)
}

// AcquireClaim grants req against the previous claim (the zero claim when the
// run was never claimed). A still-active previous claim is refused with
// ErrClaimHeld. Otherwise the new claim carries the previous fence plus one, so
// every takeover strictly increases the fence.
func AcquireClaim(prev Claim, req ClaimRequest, now time.Time) (Claim, error) {
	if err := req.Validate(); err != nil {
		return Claim{}, err
	}
	if prev != (Claim{}) && (prev.Scope != req.Scope || prev.RunID != req.RunID) {
		return Claim{}, invalid("previous claim belongs to %s/%s", prev.Scope, prev.RunID)
	}
	if prev.Active(now) {
		return Claim{}, fmt.Errorf("%w: owner %s until %s", ErrClaimHeld, prev.Owner, prev.Expires.UTC().Format(time.RFC3339))
	}
	return Claim{Scope: req.Scope, RunID: req.RunID, Owner: req.Owner, Purpose: req.Purpose,
		Fence: prev.Fence + 1, Expires: now.Add(req.TTL)}, nil
}

// Renew extends an active claim without changing its fence. The lease only
// moves later: a renewal never shortens it. An expired claim cannot be renewed;
// it must be re-acquired, which increments the fence.
func (c Claim) Renew(now time.Time, ttl time.Duration) (Claim, error) {
	if ttl <= 0 {
		return Claim{}, invalid("claim ttl must be positive")
	}
	if !c.Active(now) {
		return Claim{}, ErrClaimExpired
	}
	if e := now.Add(ttl); e.After(c.Expires) {
		c.Expires = e
	}
	return c, nil
}

// RenewClaim is the atomic same-owner renewal a StateStore applies to the
// stored (current) claim. The claim a writer holds must authorize against the
// current one (same tenant, project and run, same owner, equal fence, unexpired:
// see Claim.Authorizes), so a writer superseded by a takeover gets
// ErrStaleFence and an expired lease ErrClaimExpired. The result keeps the
// owner, purpose and fence and carries the later expiry, which the store
// persists in the same atomic step. AcquireClaim never renews: it refuses an
// active lease, including the caller's own.
func RenewClaim(current, held Claim, now time.Time, ttl time.Duration) (Claim, error) {
	if ttl <= 0 {
		return Claim{}, invalid("claim ttl must be positive")
	}
	if err := held.Authorizes(current, now); err != nil {
		return Claim{}, err
	}
	return current.Renew(now, ttl)
}

// Authorizes reports whether c, presented by a writer, is the run's current
// claim at now: same run and scope, same owner, equal fence and unexpired. A
// superseded fence yields ErrStaleFence.
func (c Claim) Authorizes(current Claim, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Scope != current.Scope || c.RunID != current.RunID {
		return invalid("claim belongs to another run")
	}
	if err := CheckFence(c.Fence, current.Fence); err != nil {
		return err
	}
	if c.Owner != current.Owner {
		return fmt.Errorf("%w: fence %d is owned by another writer", ErrStaleFence, c.Fence)
	}
	if !current.Active(now) {
		return ErrClaimExpired
	}
	return nil
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

// Capability is a guarantee a backend advertises. A backend must not advertise
// a guarantee it cannot enforce; admission refuses a run it cannot satisfy.
type Capability string

// Capabilities a runtime or resource backend may advertise.
const (
	CapNonRoot           Capability = "non_root"
	CapNetworkIsolation  Capability = "network_isolation"
	CapResourceLimits    Capability = "resource_limits"
	CapImagePinning      Capability = "image_pinning"
	CapAdoptWorkload     Capability = "adopt_workload"
	CapFencedStop        Capability = "fenced_stop"
	CapRunServices       Capability = "run_services"
	CapPersistentService Capability = "persistent_services"
)

// CapabilitySet is a set of capabilities. The zero value is empty.
type CapabilitySet []Capability

// Has reports whether c is in the set.
func (s CapabilitySet) Has(c Capability) bool { return slices.Contains(s, c) }

// Missing returns the required capabilities the set does not provide, in the
// order required.
func (s CapabilitySet) Missing(required CapabilitySet) CapabilitySet {
	var out CapabilitySet
	for _, c := range required {
		if !s.Has(c) && !out.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

// Satisfies returns ErrCapability naming the missing capabilities, or nil.
func (s CapabilitySet) Satisfies(required CapabilitySet) error {
	if m := s.Missing(required); len(m) > 0 {
		return fmt.Errorf("%w: %v", ErrCapability, []Capability(m))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

// SecretRef names one declared secret. It carries no value; a SecretResolver
// materializes the value privately, outside /workspace.
type SecretRef struct {
	Owner domain.Owner `json:"owner"`
	Name  string       `json:"name"`
	// Version pins the secret version when known; empty resolves the current one.
	Version string `json:"version,omitempty"`
}

var secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate checks the reference.
func (s SecretRef) Validate() error {
	if err := s.Owner.Validate(); err != nil {
		return err
	}
	if !secretNameRE.MatchString(s.Name) {
		return invalid("secret name %q", s.Name)
	}
	return nil
}

// SelectSecrets returns the declared references named by names. A name that is
// not declared is refused: a step may only receive secrets its plan declared.
func SelectSecrets(declared []SecretRef, names []string) ([]SecretRef, error) {
	out := make([]SecretRef, 0, len(names))
	for _, n := range names {
		i := slices.IndexFunc(declared, func(d SecretRef) bool { return d.Name == n })
		if i < 0 {
			return nil, invalid("secret %q is not declared", n)
		}
		out = append(out, declared[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

// StepKind is the kind of one pipeline step. There is no agent-specific kind.
type StepKind string

// Step kinds.
const (
	// StepContainer runs an image and commands in the shared workspace.
	StepContainer StepKind = "container"
	// StepWait pauses the run, with no workload, until its single decision.
	StepWait StepKind = "wait"
)

// Valid reports whether k is a known kind. The zero kind is invalid. There is no
// condition kind: conditional eligibility is a when predicate on a step, which
// the engine evaluates once over persisted values (see PlanStep.When).
func (k StepKind) Valid() bool {
	return k == StepContainer || k == StepWait
}

// StepState is the lifecycle state of one step.
type StepState string

// Step states.
const (
	StepPending   StepState = "pending"
	StepRunning   StepState = "running"
	StepWaiting   StepState = "waiting"
	StepSucceeded StepState = "succeeded"
	StepFailed    StepState = "failed"
	StepSkipped   StepState = "skipped"
	StepCancelled StepState = "cancelled"
)

// Terminal reports whether no further transition leaves s.
func (s StepState) Terminal() bool {
	return s == StepSucceeded || s == StepFailed || s == StepSkipped || s == StepCancelled
}

// stepTransitions lists the legal moves per kind. Container steps run; wait
// steps wait and never run. Either may be skipped before it starts (join, when
// or requires), and a wait step may fail before it opens (correlation_missing).
var stepTransitions = map[StepKind]map[StepState][]StepState{
	StepContainer: {
		StepPending: {StepRunning, StepSkipped, StepFailed, StepCancelled},
		StepRunning: {StepSucceeded, StepFailed, StepCancelled},
	},
	StepWait: {
		StepPending: {StepWaiting, StepSkipped, StepFailed, StepCancelled},
		StepWaiting: {StepSucceeded, StepFailed, StepCancelled},
	},
}

// CanStep reports whether a step of kind may move from one state to another.
func CanStep(kind StepKind, from, to StepState) bool {
	return slices.Contains(stepTransitions[kind][from], to)
}

// Step is one position in a run's execution order. It is created pending when
// the run is admitted and is never reset; a rerun is a new run.
type Step struct {
	Key     domain.StepID  `json:"key"`
	Kind    StepKind       `json:"kind"`
	State   StepState      `json:"state"`
	Reason  Reason         `json:"reason,omitempty"`
	Version domain.Version `json:"version"`
	// Attempt is the current workload attempt of a container step: 0 before
	// launch, 1 when it starts, and incremented only by Run.RetryStep after a
	// launch that provably did not reach the backend.
	Attempt int `json:"attempt,omitempty"`
	// Result is the settled workload result of a container step. It is the
	// evidence conditions evaluate over prior step results and it survives
	// restart. A succeeded container step always records it.
	Result Result `json:"result,omitempty"`
	// Predicate is the retained evaluation of the step's when predicate. It is
	// recorded once and never re-evaluated: recovery reuses it.
	Predicate PredicateEvaluation `json:"predicate,omitempty"`
}

// PredicateEvaluation is the immutable record of one evaluation of a step's when
// predicate: its result and a digest of the values it read. The zero value means
// the predicate has not been evaluated, which differs from evaluated and false.
type PredicateEvaluation struct {
	Evaluated bool `json:"evaluated,omitempty"`
	// Result is the predicate's value; meaningful only when Evaluated.
	Result bool `json:"result,omitempty"`
	// InputDigest is the digest of the values the evaluation read (see
	// DigestPredicateReads), so an audit can show what the decision rested on.
	InputDigest string    `json:"inputDigest,omitempty"`
	At          time.Time `json:"at,omitempty"`
}

// Validate checks the record: an unevaluated record is entirely zero, an
// evaluated one has its digest and time.
func (p PredicateEvaluation) Validate() error {
	if !p.Evaluated {
		if p != (PredicateEvaluation{}) {
			return invalid("an unevaluated predicate records nothing")
		}
		return nil
	}
	if p.InputDigest == "" || len(p.InputDigest) > 128 || p.At.IsZero() {
		return invalid("an evaluated predicate records its input digest and time")
	}
	return nil
}

// PredicateRead is one value an evaluation read: a reference, its type, and
// either its value or the fact that it was missing.
type PredicateRead struct {
	Ref  string    `json:"ref"`
	Type InputType `json:"type"`
	// Present is false for a missing value, which is distinct from the empty string.
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

// DigestPredicateReads returns the canonical digest of the values an evaluation
// read, independent of the order they were read in. A missing value carries no
// Value, and the type is part of the digest, so the string "1" and the integer 1
// digest differently.
func DigestPredicateReads(reads []PredicateRead) (string, error) {
	sorted := slices.Clone(reads)
	slices.SortFunc(sorted, func(a, b PredicateRead) int { return strings.Compare(a.Ref, b.Ref) })
	fields := []string{"predicate-reads", strconv.Itoa(len(sorted))}
	for i, r := range sorted {
		if strings.TrimSpace(r.Ref) == "" || !r.Type.Valid() || (!r.Present && r.Value != "") || (i > 0 && sorted[i-1].Ref == r.Ref) {
			return "", invalid("predicate read %q", r.Ref)
		}
		fields = append(fields, r.Ref, string(r.Type), strconv.FormatBool(r.Present), r.Value)
	}
	return "sha256:" + digestFields(fields...), nil
}

// NewStep returns a pending step at version 1.
func NewStep(key domain.StepID, kind StepKind) (Step, error) {
	s := Step{Key: key, Kind: kind, State: StepPending, Version: 1}
	return s, s.Validate()
}

// Validate checks the step's shape and that its reason matches its state.
func (s Step) Validate() error {
	if err := s.Key.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return invalid("step kind %q", s.Kind)
	}
	if _, ok := stepTransitions[s.Kind][s.State]; !ok && !s.State.Terminal() {
		return invalid("state %q is not valid for a %s step", s.State, s.Kind)
	}
	if err := s.validateEvidence(); err != nil {
		return err
	}
	if err := s.validatePredicate(); err != nil {
		return err
	}
	if err := s.Version.Validate(); err != nil {
		return err
	}
	if err := s.Reason.Validate(); err != nil {
		return err
	}
	if s.reasonRequired() && s.Reason == ReasonNone {
		return invalid("a %s step requires a reason", s.State)
	}
	if !s.reasonRequired() && s.Reason != ReasonNone {
		return invalid("a %s step has no reason", s.State)
	}
	return nil
}

// validatePredicate checks the retained when evaluation against the step state:
// a step skipped for condition_false carries a false evaluation, a false
// evaluation ends in skipped, and a true one never does.
func (s Step) validatePredicate() error {
	if err := s.Predicate.Validate(); err != nil {
		return err
	}
	falseSkip := s.State == StepSkipped && s.Reason == ReasonConditionFalse
	p := s.Predicate
	switch {
	case falseSkip && !(p.Evaluated && !p.Result):
		return invalid("step %s skipped for condition_false records its false evaluation", s.Key)
	case p.Evaluated && !p.Result && !(s.State == StepPending || falseSkip || s.State == StepCancelled):
		return invalid("step %s evaluated false but is %s", s.Key, s.State)
	}
	return nil
}

// validateEvidence checks the attempt and result a container step records.
func (s Step) validateEvidence() error {
	if s.Attempt < 0 || (s.Attempt > 0 && s.Kind != StepContainer) {
		return invalid("step %s: attempt %d is only for container steps", s.Key, s.Attempt)
	}
	if s.Kind == StepContainer {
		started := s.State == StepRunning || s.State == StepSucceeded
		if started && s.Attempt < 1 {
			return invalid("container step %s is %s without an attempt", s.Key, s.State)
		}
		if s.State == StepPending && s.Attempt != 0 {
			return invalid("a pending container step has no attempt")
		}
	}
	if s.Result == (Result{}) {
		if s.Kind == StepContainer && s.State == StepSucceeded {
			return invalid("a succeeded container step records its result")
		}
		return nil
	}
	if s.Kind != StepContainer || !s.State.Terminal() || s.Attempt < 1 {
		return invalid("step %s: a result belongs to a settled container attempt", s.Key)
	}
	if err := s.Result.Validate(); err != nil {
		return err
	}
	ok := false
	switch s.State {
	case StepSucceeded:
		ok = s.Result.Code == ResultSucceeded
	case StepFailed:
		ok = s.Result.Code == ResultFailed || s.Result.Code == ResultTimedOut || s.Result.Code == ResultLost
	case StepCancelled:
		ok = s.Result.Code == ResultCancelled
	}
	if !ok {
		return invalid("step %s is %s but its result is %s", s.Key, s.State, s.Result.Code)
	}
	return nil
}

func (s Step) reasonRequired() bool {
	return s.State == StepFailed || s.State == StepSkipped || s.State == StepCancelled
}

// Advance moves the step to a legal next state, bumping its version. Failed,
// skipped and cancelled steps require a reason; others reject one. A rejected
// move leaves the step unchanged.
//
// A container step entering running starts attempt 1. Leaving running as
// succeeded needs its result, so that move goes through Finish.
func (s *Step) Advance(to StepState, reason Reason) error {
	return s.advance(to, reason, Result{})
}

// Finish settles a running container step with the workload's result. The
// result must agree with the state: succeeded with ResultSucceeded, failed with
// failed, timed_out or lost, and cancelled with cancelled.
func (s *Step) Finish(to StepState, reason Reason, res Result) error {
	if err := res.Validate(); err != nil {
		return err
	}
	return s.advance(to, reason, res)
}

func (s *Step) advance(to StepState, reason Reason, res Result) error {
	if !CanStep(s.Kind, s.State, to) {
		return fmt.Errorf("%w: %s step %s: %s -> %s", ErrIllegalTransition, s.Kind, s.Key, s.State, to)
	}
	next := *s
	next.State, next.Reason, next.Version, next.Result = to, reason, s.Version+1, res
	if s.Kind == StepContainer && to == StepRunning {
		next.Attempt = 1
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*s = next
	return nil
}

// ---------------------------------------------------------------------------
// Waits
// ---------------------------------------------------------------------------

// Reserved wait outcomes. A wait cannot declare them: the kernel records them
// itself when a deadline or a cancellation decides the wait.
const (
	WaitOutcomeExpired   = "expired"
	WaitOutcomeCancelled = "cancelled"
)

// Bounds fixed by the pipeline format and pipeline execution.
const (
	MinWaitTimeout     = time.Minute
	MaxWaitTimeout     = 30 * 24 * time.Hour
	MaxWaitOutcomes    = 8
	MaxCorrelationKeys = 4
	MaxAnswerBytes     = 4096
	MaxRequestBytes    = 4096
	MaxSignalKeyLength = 128
)

var (
	correlationKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	inputKindRE      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	outcomeRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// ExpiryAction says what a wait's deadline does to its step.
type ExpiryAction string

// Expiry actions.
const (
	// ExpiryFail fails the step with reason wait_expired. It is the default.
	ExpiryFail ExpiryAction = "fail"
	// ExpiryContinue records the reserved outcome expired and settles the step
	// as succeeded, so a when predicate can branch on it.
	ExpiryContinue ExpiryAction = "continue"
)

// Effective returns the action with the default applied.
func (a ExpiryAction) Effective() ExpiryAction {
	if a == "" {
		return ExpiryFail
	}
	return a
}

// WaitSpec is the declared shape of a wait step: the pipeline format's
// steps[].wait. Assignment (Assign) and correlation (Correlate) are separate
// attributes that answer different questions.
type WaitSpec struct {
	// Assign is an optional opaque role label naming who may decide the wait
	// through the inbox. The shell validates it; the kernel never interprets it.
	Assign string `json:"assign,omitempty"`
	// Correlate maps up to four keys to the references read when the wait opens.
	// An external signal must present every key and value.
	Correlate map[string]string `json:"correlate,omitempty"`
	// Request optionally references a declared output recorded with the wait.
	Request string `json:"request,omitempty"`
	// Input is the kind label of the input the resolver supplies.
	Input string `json:"input"`
	// Outcomes is the closed set of 1 to 8 outcomes a resolver may choose.
	Outcomes []string `json:"outcomes"`
	// Approving is the single outcome that counts as approval for a protected
	// step. It is optional, but a protected step may only require a wait that
	// declares it. No other outcome, however named, ever authorizes a protected step.
	Approving string `json:"approving,omitempty"`
	// Timeout is the required deadline measured from the commit that opens the wait.
	Timeout  time.Duration `json:"timeout"`
	OnExpiry ExpiryAction  `json:"onExpiry,omitempty"`
}

// Declares reports whether outcome is one of the declared outcomes.
func (w WaitSpec) Declares(outcome string) bool { return slices.Contains(w.Outcomes, outcome) }

// ApprovingOutcome returns the single approving outcome, if one is declared.
func (w WaitSpec) ApprovingOutcome() (string, bool) { return w.Approving, w.Approving != "" }

// Equal reports whether two specs are identical.
func (w WaitSpec) Equal(o WaitSpec) bool {
	return w.Assign == o.Assign && w.Request == o.Request && w.Input == o.Input && w.Approving == o.Approving &&
		w.Timeout == o.Timeout && w.OnExpiry == o.OnExpiry && slices.Equal(w.Outcomes, o.Outcomes) &&
		maps.Equal(w.Correlate, o.Correlate)
}

// Validate checks the spec against the pipeline format's wait rules.
func (w WaitSpec) Validate() error {
	if w.Assign == "" && len(w.Correlate) == 0 {
		return invalid("a wait requires assign or correlate")
	}
	if len(w.Assign) > 64 || (w.Assign != "" && strings.TrimSpace(w.Assign) == "") {
		return invalid("wait assign %q", w.Assign)
	}
	if len(w.Correlate) > MaxCorrelationKeys {
		return invalid("a wait correlates on at most %d keys", MaxCorrelationKeys)
	}
	for k, ref := range w.Correlate {
		if !correlationKeyRE.MatchString(k) || strings.TrimSpace(ref) == "" || len(ref) > 128 {
			return invalid("wait correlation %q", k)
		}
	}
	if len(w.Request) > 128 {
		return invalid("wait request reference is too long")
	}
	if !inputKindRE.MatchString(w.Input) {
		return invalid("wait input kind %q", w.Input)
	}
	if n := len(w.Outcomes); n < 1 || n > MaxWaitOutcomes {
		return invalid("a wait declares 1 to %d outcomes, not %d", MaxWaitOutcomes, n)
	}
	for i, o := range w.Outcomes {
		if !outcomeRE.MatchString(o) {
			return invalid("wait outcome %q", o)
		}
		if o == WaitOutcomeExpired || o == WaitOutcomeCancelled {
			return invalid("outcome %q is reserved", o)
		}
		if slices.Contains(w.Outcomes[:i], o) {
			return invalid("duplicate outcome %q", o)
		}
	}
	if w.Approving != "" && !w.Declares(w.Approving) {
		return invalid("approving outcome %q is not a declared outcome", w.Approving)
	}
	if w.Timeout < MinWaitTimeout || w.Timeout > MaxWaitTimeout {
		return invalid("wait timeout %s is outside %s to %s", w.Timeout, MinWaitTimeout, MaxWaitTimeout)
	}
	if e := w.OnExpiry; e != "" && e != ExpiryFail && e != ExpiryContinue {
		return invalid("wait on_expiry %q", e)
	}
	return nil
}

// WaitID is the unique, never reused identity of one wait of one run.
type WaitID string

// Validate checks the identity syntax.
func (id WaitID) Validate() error {
	if !idRE.MatchString(string(id)) {
		return invalid("wait id %q", id)
	}
	return nil
}

var idRE = regexp.MustCompile(domain.IDPattern)

// WaitState is the lifecycle state of a wait record.
type WaitState string

// Wait states. A wait has exactly one decision, taken from open (or, for
// cancellation, from pending); resolved, expired and cancelled are the three
// decisions.
const (
	WaitPending   WaitState = "pending"
	WaitOpen      WaitState = "open"
	WaitResolved  WaitState = "resolved"
	WaitExpired   WaitState = "expired"
	WaitCancelled WaitState = "cancelled"
)

// Closed reports whether the wait is decided and can no longer change.
func (s WaitState) Closed() bool {
	return s == WaitResolved || s == WaitExpired || s == WaitCancelled
}

// DecisionKind is how a wait was decided.
type DecisionKind string

// Decision kinds.
const (
	DecisionResolved  DecisionKind = "resolved"
	DecisionExpired   DecisionKind = "expired"
	DecisionCancelled DecisionKind = "cancelled"
)

// Decision is the complete, replayable decision of a wait. It is self-contained:
// a signal record retains a copy so a replay never reads the wait.
type Decision struct {
	Kind DecisionKind `json:"kind"`
	// SignalID is the deciding signal of a resolved decision; empty otherwise.
	SignalID SignalID `json:"signalId,omitempty"`
	// Outcome is the declared outcome of a resolved decision, the reserved
	// outcome expired for an expiry that continues, and empty otherwise.
	Outcome string `json:"outcome,omitempty"`
	// Answer is the optional answer text, at most MaxAnswerBytes.
	Answer string `json:"answer,omitempty"`
	// Actor references the deciding actor of a resolved decision.
	Actor string `json:"actor,omitempty"`
	// Reason is run_cancelled or run_failed for a cancelled decision.
	Reason Reason `json:"reason,omitempty"`
	// Version is the wait version this decision committed.
	Version domain.Version `json:"version"`
	At      time.Time      `json:"at"`
}

// Validate checks the decision's shape for its kind.
func (d Decision) Validate() error {
	if err := d.Version.Validate(); err != nil {
		return err
	}
	if d.At.IsZero() {
		return invalid("a decision records its time")
	}
	if len(d.Answer) > MaxAnswerBytes || !utf8.ValidString(d.Answer) {
		return invalid("a decision answer is at most %d bytes of UTF-8", MaxAnswerBytes)
	}
	switch d.Kind {
	case DecisionResolved:
		if err := d.SignalID.Validate(); err != nil {
			return err
		}
		if d.Actor == "" || d.Reason != ReasonNone {
			return invalid("a resolved decision records its actor and no reason")
		}
		if !outcomeRE.MatchString(d.Outcome) || d.Outcome == WaitOutcomeExpired || d.Outcome == WaitOutcomeCancelled {
			return invalid("resolved outcome %q", d.Outcome)
		}
	case DecisionExpired:
		if d.SignalID != "" || d.Actor != "" || d.Answer != "" || d.Reason != ReasonNone ||
			(d.Outcome != "" && d.Outcome != WaitOutcomeExpired) {
			return invalid("an expired decision records only the reserved outcome")
		}
	case DecisionCancelled:
		if d.SignalID != "" || d.Actor != "" || d.Answer != "" || d.Outcome != "" ||
			(d.Reason != ReasonRunCancelled && d.Reason != ReasonRunFailed) {
			return invalid("a cancelled decision records run_cancelled or run_failed")
		}
	default:
		return invalid("decision kind %q", d.Kind)
	}
	return nil
}

// Wait is the durable record of one wait step. Identity, correlation and
// request are immutable once the wait opens; the decision is taken once.
type Wait struct {
	Scope   domain.Scope  `json:"scope"`
	ID      WaitID        `json:"id"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey"`
	Spec    WaitSpec      `json:"spec"`
	// Assignment is who may decide the wait through the inbox. It starts as
	// Spec.Assign and changes only by an authorized, version-checked reassignment.
	Assignment string `json:"assignment,omitempty"`
	// Correlation holds the values read when the wait opened, one per declared key.
	Correlation map[string]string `json:"correlation,omitempty"`
	// Request is the bounded request text recorded when the wait opened.
	Request string         `json:"request,omitempty"`
	State   WaitState      `json:"state"`
	Version domain.Version `json:"version"`
	// OpenedAt and Deadline are zero until the wait opens.
	OpenedAt time.Time `json:"openedAt,omitempty"`
	Deadline time.Time `json:"deadline,omitempty"`
	// Decision is zero until the wait is decided.
	Decision Decision `json:"decision,omitempty"`
}

// NewWait returns a pending wait at version 1. It is committed at admission and
// opens, with its correlation values, when its step becomes eligible.
func NewWait(scope domain.Scope, id WaitID, run domain.RunID, step domain.StepID, spec WaitSpec) (Wait, error) {
	w := Wait{Scope: scope, ID: id, RunID: run, StepKey: step, Spec: spec, Assignment: spec.Assign, State: WaitPending, Version: 1}
	return w, w.Validate()
}

// Target returns the wait's address: run, step and wait identities.
func (w Wait) Target() SignalTarget {
	return SignalTarget{RunID: w.RunID, StepKey: w.StepKey, WaitID: w.ID}
}

func (w Wait) validCorrelation() error {
	if len(w.Correlation) != len(w.Spec.Correlate) {
		return invalid("wait %s records %d correlation values, declares %d", w.ID, len(w.Correlation), len(w.Spec.Correlate))
	}
	for k := range w.Spec.Correlate {
		if v, ok := w.Correlation[k]; !ok || v == "" || len(v) > MaxRequestBytes {
			return invalid("wait %s is missing correlation %q", w.ID, k)
		}
	}
	return nil
}

// Validate checks the wait's shape and that its state, fields and decision agree.
func (w Wait) Validate() error {
	if err := w.Scope.Validate(); err != nil {
		return err
	}
	if err := w.ID.Validate(); err != nil {
		return err
	}
	if err := w.RunID.Validate(); err != nil {
		return err
	}
	if err := w.StepKey.Validate(); err != nil {
		return err
	}
	if err := w.Spec.Validate(); err != nil {
		return err
	}
	if err := w.Version.Validate(); err != nil {
		return err
	}
	if len(w.Request) > MaxRequestBytes || !utf8.ValidString(w.Request) {
		return invalid("wait request is at most %d bytes of UTF-8", MaxRequestBytes)
	}
	opened := !w.OpenedAt.IsZero()
	switch w.State {
	case WaitPending:
		if opened || !w.Deadline.IsZero() || len(w.Correlation) > 0 || w.Request != "" || w.Decision != (Decision{}) {
			return invalid("a pending wait has no open time, values or decision")
		}
		return nil
	case WaitOpen, WaitResolved, WaitExpired, WaitCancelled:
	default:
		return invalid("wait state %q", w.State)
	}
	if w.State == WaitCancelled && !opened {
		// Cancelled before it ever opened: no values were read.
		if len(w.Correlation) > 0 || w.Request != "" || !w.Deadline.IsZero() {
			return invalid("a wait cancelled before opening has no values")
		}
	} else {
		if !opened {
			return invalid("a %s wait records when it opened", w.State)
		}
		if err := w.validCorrelation(); err != nil {
			return err
		}
		if !w.Deadline.Equal(w.OpenedAt.Add(w.Spec.Timeout)) {
			return invalid("wait %s deadline is not its open time plus its timeout", w.ID)
		}
	}
	if w.State == WaitOpen {
		if w.Decision != (Decision{}) {
			return invalid("an open wait has no decision")
		}
		return nil
	}
	d := w.Decision
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Version != w.Version {
		return invalid("wait %s decision version %d is not its version %d", w.ID, d.Version, w.Version)
	}
	want := map[WaitState]DecisionKind{WaitResolved: DecisionResolved, WaitExpired: DecisionExpired, WaitCancelled: DecisionCancelled}[w.State]
	if d.Kind != want {
		return invalid("a %s wait carries a %s decision", w.State, d.Kind)
	}
	switch d.Kind {
	case DecisionResolved:
		if !w.Spec.Declares(d.Outcome) {
			return invalid("outcome %q is not declared by wait %s", d.Outcome, w.ID)
		}
	case DecisionExpired:
		if (w.Spec.OnExpiry.Effective() == ExpiryContinue) != (d.Outcome == WaitOutcomeExpired) {
			return invalid("an expiry records the outcome expired exactly when on_expiry is continue")
		}
	}
	return nil
}

// Open moves a pending wait to open, recording the correlation values and
// request read at that moment and fixing the deadline. Callers commit the open
// wait before announcing it. The values must be exactly the declared keys.
func (w *Wait) Open(now time.Time, correlation map[string]string, request string) error {
	if w.State != WaitPending {
		return fmt.Errorf("%w: wait %s is %s", ErrIllegalTransition, w.ID, w.State)
	}
	next := *w
	next.State, next.OpenedAt, next.Version = WaitOpen, now, w.Version+1
	next.Deadline = now.Add(w.Spec.Timeout)
	next.Correlation, next.Request = maps.Clone(correlation), request
	if err := next.Validate(); err != nil {
		return err
	}
	*w = next
	return nil
}

func (w *Wait) decide(state WaitState, d Decision) {
	w.State, w.Version, w.Decision = state, w.Version+1, d
	w.Decision.Version = w.Version
}

// Resolve decides an open wait with the outcome and answer of req. The request
// must belong to the wait: the same tenant and project, the wait's identity when
// it names one, and the wait's complete correlation when it presents any;
// otherwise it is ErrWaitForeign and nothing changes. Then the checks run in this
// order: the wait must be open, a supplied expected version must match,
// the deadline must not have been reached (expiry wins), and the outcome must be
// declared. A refused request changes nothing. Replays and routing are handled
// by RouteSignal through the durable signal record, not here.
func (w *Wait) Resolve(req SignalRequest, now time.Time) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	// A request decides only the wait it addresses, in its own tenant and project:
	// the mutator enforces this itself rather than trusting the router's filter.
	if req.Identity.Scope != w.Scope || (req.WaitID != "" && req.WaitID != w.ID) ||
		(len(req.Correlation) > 0 && !maps.Equal(req.Correlation, w.Correlation)) {
		return Decision{}, ErrWaitForeign
	}
	switch {
	case w.State.Closed():
		return Decision{}, ErrWaitClosed
	case w.State != WaitOpen:
		return Decision{}, ErrWaitNotOpen
	case req.ExpectedVersion != 0 && req.ExpectedVersion != w.Version:
		return Decision{}, fmt.Errorf("%w: signal %d, wait %d", ErrWaitStale, req.ExpectedVersion, w.Version)
	case !now.Before(w.Deadline):
		return Decision{}, ErrWaitExpired
	case !w.Spec.Declares(req.Outcome):
		return Decision{}, ErrInvalidOutcome
	}
	next := *w
	next.decide(WaitResolved, Decision{Kind: DecisionResolved, SignalID: req.Identity.ID(), Outcome: req.Outcome,
		Answer: req.Answer, Actor: req.Actor, At: now})
	if err := next.Validate(); err != nil {
		return Decision{}, err
	}
	*w = next
	return w.Decision, nil
}

// Expire decides an open wait whose deadline has been reached. With on_expiry
// continue the decision records the reserved outcome expired; with fail it
// records none and the step fails with reason wait_expired.
func (w *Wait) Expire(now time.Time) (Decision, error) {
	if w.State != WaitOpen {
		return Decision{}, fmt.Errorf("%w: wait %s is %s", ErrWaitClosed, w.ID, w.State)
	}
	if now.Before(w.Deadline) {
		return Decision{}, ErrWaitNotDue
	}
	d := Decision{Kind: DecisionExpired, At: now}
	if w.Spec.OnExpiry.Effective() == ExpiryContinue {
		d.Outcome = WaitOutcomeExpired
	}
	next := *w
	next.decide(WaitExpired, d)
	if err := next.Validate(); err != nil {
		return Decision{}, err
	}
	*w = next
	return w.Decision, nil
}

// Cancel decides a pending or open wait because its run was cancelled or failed
// (reason ReasonRunCancelled or ReasonRunFailed). A decided wait is refused and
// unchanged.
func (w *Wait) Cancel(now time.Time, reason Reason) (Decision, error) {
	if w.State != WaitPending && w.State != WaitOpen {
		return Decision{}, fmt.Errorf("%w: wait %s is %s", ErrWaitClosed, w.ID, w.State)
	}
	next := *w
	next.decide(WaitCancelled, Decision{Kind: DecisionCancelled, Reason: reason, At: now})
	if err := next.Validate(); err != nil {
		return Decision{}, err
	}
	*w = next
	return w.Decision, nil
}

// CheckStepWait verifies that a wait step and its wait record agree. A pending
// step has a pending wait; a waiting step an open one. A step that succeeded
// has a wait that was resolved or expired with continue; one that failed has an
// expired or cancelled wait, or a wait that never opened; a skipped step has a
// wait that never opened or was cancelled; a cancelled step a cancelled wait.
func CheckStepWait(s Step, w Wait) error {
	if s.Kind != StepWait {
		return invalid("step %s is not a wait step", s.Key)
	}
	if s.Key != w.StepKey {
		return invalid("wait %s belongs to step %s", w.ID, w.StepKey)
	}
	var ok bool
	switch s.State {
	case StepPending:
		ok = w.State == WaitPending
	case StepWaiting:
		ok = w.State == WaitOpen
	case StepSucceeded:
		ok = w.State == WaitResolved || (w.State == WaitExpired && w.Spec.OnExpiry.Effective() == ExpiryContinue)
	case StepFailed:
		ok = w.State == WaitExpired || w.State == WaitCancelled || w.State == WaitPending
	case StepSkipped:
		ok = w.State == WaitPending || w.State == WaitCancelled
	case StepCancelled:
		ok = w.State == WaitCancelled
	}
	if !ok {
		return invalid("step %s is %s but wait %s is %s", s.Key, s.State, w.ID, w.State)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

// WorkloadSpec is the container a step runs. It mirrors the container fields of
// a pipeline step (internal/kernel/pipeline.Step) as plain values.
type WorkloadSpec struct {
	Image    string            `json:"image"`
	Commands []string          `json:"commands"`
	Env      map[string]string `json:"env,omitempty"`
	Secrets  []SecretRef       `json:"secrets,omitempty"`
	Timeout  time.Duration     `json:"timeout,omitempty"`
	Requires CapabilitySet     `json:"requires,omitempty"`
}

// Validate checks the spec.
func (w WorkloadSpec) Validate() error {
	if strings.TrimSpace(w.Image) == "" || len(w.Commands) == 0 {
		return invalid("a container step requires an image and commands")
	}
	if w.Timeout < 0 {
		return invalid("timeout must not be negative")
	}
	for _, s := range w.Secrets {
		if err := s.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Requirement names one wait a protected step requires and the outcome it must
// have been decided with, which must be that wait's approving outcome.
type Requirement struct {
	Wait    domain.StepID `json:"wait"`
	Outcome string        `json:"outcome"`
}

// AuthorityIntegrate is the only authority value a protected step may carry. It
// has no other meaning to the kernel: the shell issues the authority.
const AuthorityIntegrate = "integrate"

// MaxRequirements bounds a protected step's requires list.
const MaxRequirements = 4

// Bounds fixed by the pipeline format.
const (
	MaxStepOutputs      = 8
	MaxStepBindings     = 8
	MaxInputs           = 16
	MaxInputStringBytes = 256
)

var (
	inputNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	outputNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// JoinMode says how a step's skipped prerequisites combine.
type JoinMode string

// Join modes. The empty mode means all.
const (
	// JoinAll runs the step only when every prerequisite succeeded. It is the default.
	JoinAll JoinMode = "all"
	// JoinAny runs the step when at least one prerequisite succeeded. It needs at
	// least two prerequisites.
	JoinAny JoinMode = "any"
)

// Effective returns the mode with the default applied.
func (j JoinMode) Effective() JoinMode {
	if j == "" {
		return JoinAll
	}
	return j
}

// InputType is the declared type of a run input. The condition grammar compares
// without coercion, so the admitted plan keeps the type of every value.
type InputType string

// Input types.
const (
	InputString  InputType = "string"
	InputInteger InputType = "integer"
	InputBoolean InputType = "boolean"
)

// Valid reports whether t is a known input type.
func (t InputType) Valid() bool { return t == InputString || t == InputInteger || t == InputBoolean }

// InputValue is one admitted run input: a value together with its declared type.
// Exactly the field matching Type is meaningful; the others must be zero, so the
// string "true", the boolean true, the string "1" and the integer 1 are four
// distinct values.
type InputValue struct {
	Type    InputType `json:"type"`
	String  string    `json:"string,omitempty"`
	Integer int64     `json:"integer,omitempty"`
	Boolean bool      `json:"boolean,omitempty"`
}

// StringInput returns a string input value.
func StringInput(v string) InputValue { return InputValue{Type: InputString, String: v} }

// IntegerInput returns an integer input value.
func IntegerInput(v int64) InputValue { return InputValue{Type: InputInteger, Integer: v} }

// BooleanInput returns a boolean input value.
func BooleanInput(v bool) InputValue { return InputValue{Type: InputBoolean, Boolean: v} }

// Validate checks the value against its type and the format's bounds.
func (v InputValue) Validate() error {
	switch v.Type {
	case InputString:
		if v.Integer != 0 || v.Boolean {
			return invalid("a string input carries only a string")
		}
		if len(v.String) > MaxInputStringBytes || !utf8.ValidString(v.String) {
			return invalid("a string input is at most %d bytes of UTF-8", MaxInputStringBytes)
		}
	case InputInteger:
		if v.String != "" || v.Boolean {
			return invalid("an integer input carries only an integer")
		}
	case InputBoolean:
		if v.String != "" || v.Integer != 0 {
			return invalid("a boolean input carries only a boolean")
		}
	default:
		return invalid("input type %q", v.Type)
	}
	return nil
}

// Text renders the value as the canonical text recorded in digests. The type is
// not part of the text; callers that need it keep InputValue.Type alongside.
func (v InputValue) Text() string {
	switch v.Type {
	case InputInteger:
		return strconv.FormatInt(v.Integer, 10)
	case InputBoolean:
		return strconv.FormatBool(v.Boolean)
	}
	return v.String
}

// PlanStep is one step of an admitted plan. Kind selects exactly one of
// Workload (container) or Wait (wait). There is no condition step: eligibility
// beyond prerequisites is the optional When predicate.
type PlanStep struct {
	Key       domain.StepID   `json:"key"`
	Kind      StepKind        `json:"kind"`
	DependsOn []domain.StepID `json:"dependsOn,omitempty"`
	// When is an optional bounded predicate over declared inputs, prerequisite
	// step results and wait outcomes. The engine evaluates it once, when every
	// prerequisite is terminal, and records the result; its grammar is frozen by
	// the pipeline format, not here.
	When     string        `json:"when,omitempty"`
	Workload *WorkloadSpec `json:"workload,omitempty"`
	Wait     *WaitSpec     `json:"wait,omitempty"`
	// Authority marks a protected container step. It is eligible only when every
	// Requires wait was decided with its approving outcome.
	Authority string        `json:"authority,omitempty"`
	Requires  []Requirement `json:"requires,omitempty"`
	// Join is how skipped prerequisites combine; empty means all.
	Join JoinMode `json:"join,omitempty"`
	// Outputs are the names a container step may record (at most 8). A
	// reference such as a wait's request or correlation, or another step's
	// when, reads them as <step>.outputs.<name>; resolving references is the
	// engine's work, not the plan's.
	Outputs []string `json:"outputs,omitempty"`
	// Bindings inject the current value of a visible reference into a container
	// step's environment (at most 8): variable name to reference. A binding may
	// not collide with the step's env, its secrets or the built-in KODAMA_ names.
	Bindings map[string]string `json:"bindings,omitempty"`
}

// Protected reports whether the step carries authority.
func (p PlanStep) Protected() bool { return p.Authority != "" }

// MaxWhenBytes bounds a when expression's text.
const MaxWhenBytes = 256

// Validate checks the step against its kind. Whether Requires names real
// approving waits is a plan-level check (Plan.Validate).
func (p PlanStep) Validate() error {
	if err := p.Key.Validate(); err != nil {
		return err
	}
	if len(p.When) > MaxWhenBytes {
		return invalid("step %s when is longer than %d bytes", p.Key, MaxWhenBytes)
	}
	var err error
	switch p.Kind {
	case StepContainer:
		if p.Workload == nil || p.Wait != nil {
			return invalid("container step %s requires a workload and no wait", p.Key)
		}
		err = p.Workload.Validate()
	case StepWait:
		if p.Wait == nil || p.Workload != nil {
			return invalid("wait step %s requires a wait and no workload", p.Key)
		}
		err = p.Wait.Validate()
	default:
		return invalid("step %s kind %q", p.Key, p.Kind)
	}
	if err != nil {
		return err
	}
	if err := p.validateShape(); err != nil {
		return err
	}
	switch {
	case p.Authority == "" && len(p.Requires) > 0:
		return invalid("step %s has requires but no authority", p.Key)
	case p.Authority == "":
		return nil
	case p.Authority != AuthorityIntegrate:
		return invalid("step %s authority %q", p.Key, p.Authority)
	case p.Kind != StepContainer:
		return invalid("a wait step cannot be protected: %s", p.Key)
	case len(p.Requires) == 0 || len(p.Requires) > MaxRequirements:
		return invalid("protected step %s requires 1 to %d waits", p.Key, MaxRequirements)
	}
	return nil
}

// validateShape checks join, outputs and bindings.
func (p PlanStep) validateShape() error {
	if j := p.Join; j != "" && j != JoinAll && j != JoinAny {
		return invalid("step %s join %q", p.Key, j)
	}
	if p.Join == JoinAny && len(p.DependsOn) < 2 {
		return invalid("step %s: join any needs at least two prerequisites", p.Key)
	}
	if p.Kind != StepContainer && (len(p.Outputs) > 0 || len(p.Bindings) > 0) {
		return invalid("step %s: outputs and bindings belong to container steps", p.Key)
	}
	if len(p.Outputs) > MaxStepOutputs || len(p.Bindings) > MaxStepBindings {
		return invalid("step %s declares more than %d outputs or %d bindings", p.Key, MaxStepOutputs, MaxStepBindings)
	}
	for i, o := range p.Outputs {
		if !outputNameRE.MatchString(o) || slices.Contains(p.Outputs[:i], o) {
			return invalid("step %s output %q", p.Key, o)
		}
	}
	for name, ref := range p.Bindings {
		if !envNameRE.MatchString(name) || strings.HasPrefix(name, "KODAMA_") || strings.TrimSpace(ref) == "" || len(ref) > 128 {
			return invalid("step %s binding %q", p.Key, name)
		}
		if _, clash := p.Workload.Env[name]; clash || slices.ContainsFunc(p.Workload.Secrets, func(s SecretRef) bool { return s.Name == name }) {
			return invalid("step %s binding %q collides with its environment", p.Key, name)
		}
	}
	return nil
}

// Lifetime is how long a service outlives its run. Values match the pipeline
// format's service scope.
type Lifetime string

// Service lifetimes.
const (
	LifetimePipeline Lifetime = "pipeline"
	LifetimeProject  Lifetime = "project"
	LifetimeTenant   Lifetime = "tenant"
)

// Valid reports whether l is a known lifetime.
func (l Lifetime) Valid() bool {
	return l == LifetimePipeline || l == LifetimeProject || l == LifetimeTenant
}

var serviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ServiceSpec is one support container declared by a plan.
type ServiceSpec struct {
	Name     string            `json:"name"`
	Lifetime Lifetime          `json:"lifetime"`
	Image    string            `json:"image"`
	Commands []string          `json:"commands,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Secrets  []SecretRef       `json:"secrets,omitempty"`
	Ports    []int             `json:"ports,omitempty"`
}

// Validate checks the spec.
func (s ServiceSpec) Validate() error {
	if !serviceNameRE.MatchString(s.Name) {
		return invalid("service name %q", s.Name)
	}
	if !s.Lifetime.Valid() {
		return invalid("service lifetime %q", s.Lifetime)
	}
	if strings.TrimSpace(s.Image) == "" {
		return invalid("service %s requires an image", s.Name)
	}
	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return invalid("service %s port %d", s.Name, p)
		}
	}
	for _, r := range s.Secrets {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Plan is the immutable admission snapshot a run executes: the definition in
// execution order, its services and the declared non-secret inputs. A later
// edit of the pipeline cannot change an admitted plan.
type Plan struct {
	// Digest identifies the canonical definition snapshot.
	Digest string `json:"digest"`
	// Steps are in deterministic execution order; a step depends only on earlier steps.
	Steps    []PlanStep    `json:"steps"`
	Services []ServiceSpec `json:"services,omitempty"`
	// Inputs are the admitted run inputs, each with its declared type.
	Inputs map[string]InputValue `json:"inputs,omitempty"`
}

// Validate checks that the plan is well formed: unique step keys, each step
// valid for its kind, dependencies naming only earlier steps (which makes the
// order acyclic), and unique service names.
func (p Plan) Validate() error {
	if strings.TrimSpace(p.Digest) == "" {
		return invalid("a plan requires a digest")
	}
	if len(p.Steps) == 0 {
		return invalid("a plan requires at least one step")
	}
	if len(p.Inputs) > MaxInputs {
		return invalid("a plan admits at most %d inputs", MaxInputs)
	}
	for name, v := range p.Inputs {
		if !inputNameRE.MatchString(name) {
			return invalid("input name %q", name)
		}
		if err := v.Validate(); err != nil {
			return fmt.Errorf("input %s: %w", name, err)
		}
	}
	// ancestors maps each earlier step to its transitive prerequisites.
	ancestors := map[domain.StepID]map[domain.StepID]bool{}
	byKey := map[domain.StepID]PlanStep{}
	for _, s := range p.Steps {
		if err := s.Validate(); err != nil {
			return err
		}
		anc := map[domain.StepID]bool{}
		for _, d := range s.DependsOn {
			if _, ok := ancestors[d]; !ok {
				return invalid("step %s depends on %q, which is not an earlier step", s.Key, d)
			}
			anc[d] = true
			maps.Copy(anc, ancestors[d])
		}
		if _, dup := ancestors[s.Key]; dup {
			return invalid("duplicate step %s", s.Key)
		}
		ancestors[s.Key], byKey[s.Key] = anc, s
		for _, req := range s.Requires {
			w, ok := byKey[req.Wait]
			switch {
			case !ok || w.Kind != StepWait || !anc[req.Wait]:
				return invalid("step %s requires %q, which is not a wait that precedes it", s.Key, req.Wait)
			case w.Wait.Approving == "":
				return invalid("step %s requires wait %s, which declares no approving outcome", s.Key, req.Wait)
			case req.Outcome != w.Wait.Approving:
				return invalid("step %s requires wait %s outcome %q; only its approving outcome %q may authorize a protected step",
					s.Key, req.Wait, req.Outcome, w.Wait.Approving)
			}
			if _, ok := w.Wait.Correlate["candidate"]; s.Authority == AuthorityIntegrate && !ok {
				return invalid("integrate step %s requires wait %s, which must correlate on candidate", s.Key, req.Wait)
			}
		}
	}
	names := map[string]bool{}
	for _, s := range p.Services {
		if err := s.Validate(); err != nil {
			return err
		}
		if names[s.Name] {
			return invalid("duplicate service %s", s.Name)
		}
		names[s.Name] = true
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resources
// ---------------------------------------------------------------------------

// ResourceKind distinguishes the run workspace from support services.
type ResourceKind string

// Resource kinds.
const (
	ResourceWorkspace ResourceKind = "workspace"
	ResourceService   ResourceKind = "service"
)

// Valid reports whether k is a known kind.
func (k ResourceKind) Valid() bool { return k == ResourceWorkspace || k == ResourceService }

// ResourceState is the observed state of a workspace or service.
type ResourceState string

// Resource states. Unknown means the backend could not confirm anything; it is
// never treated as removed.
const (
	ResourceStarting ResourceState = "starting"
	ResourceReady    ResourceState = "ready"
	ResourceFailed   ResourceState = "failed"
	ResourceStopped  ResourceState = "stopped"
	ResourceRemoved  ResourceState = "removed"
	ResourceUnknown  ResourceState = "unknown"
)

// Valid reports whether s is a known state.
func (s ResourceState) Valid() bool {
	switch s {
	case ResourceStarting, ResourceReady, ResourceFailed, ResourceStopped, ResourceRemoved, ResourceUnknown:
		return true
	}
	return false
}

// SourceInput is one pinned source placed in the workspace. The shell defines
// the locator; the kernel never interprets it as Git.
type SourceInput struct {
	Name string `json:"name"`
	// Locator is opaque to the kernel.
	Locator string `json:"locator"`
	// Revision pins the exact immutable content; a floating ref is refused.
	Revision string `json:"revision"`
	// Path is relative to /workspace.
	Path string `json:"path"`
}

// PreparationInputs are the pinned inputs a WorkspacePreparer supplies and a
// ResourceManager consumes to prepare or adopt a run's workspace. Source-free
// runs have no sources. Credentials are references, resolved outside /workspace.
type PreparationInputs struct {
	Scope       domain.Scope  `json:"scope"`
	RunID       domain.RunID  `json:"runId"`
	Sources     []SourceInput `json:"sources,omitempty"`
	Credentials []SecretRef   `json:"credentials,omitempty"`
	// Digest identifies the exact input set so adoption can compare it.
	Digest string `json:"digest"`
}

// Validate checks that every source is pinned and placed inside /workspace.
func (p PreparationInputs) Validate() error {
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if err := p.RunID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(p.Digest) == "" {
		return invalid("preparation inputs require a digest")
	}
	paths := map[string]bool{}
	for _, s := range p.Sources {
		if s.Name == "" || s.Locator == "" || s.Revision == "" {
			return invalid("source %q requires a name, a locator and a pinned revision", s.Name)
		}
		clean := path.Clean(s.Path)
		if s.Path == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || paths[clean] {
			return invalid("source %s path %q must be unique and inside the workspace", s.Name, s.Path)
		}
		paths[clean] = true
	}
	for _, c := range p.Credentials {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ErrForeignResource reports a resource whose owner is not available to the
// requesting scope: another tenant's resource, or another project's
// project-owned resource.
var ErrForeignResource = errors.New("runtime: foreign resource")

// OwnerFor returns who owns a resource of the given lifetime requested by a run
// in scope: a tenant-lifetime resource belongs to the tenant alone, with no
// project, and is shared by every project of that tenant; project- and
// pipeline-lifetime resources belong to the requesting project.
func OwnerFor(l Lifetime, scope domain.Scope) domain.Owner {
	if l == LifetimeTenant {
		return domain.Owner{TenantID: scope.TenantID}
	}
	return domain.Owner{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
}

// ResourceSpec asks a ResourceManager to ensure one workspace or service.
// Scope is the requesting run's tenant and project; Owner is who owns the
// resource, which differs from Scope for a tenant-lifetime service (owned by the
// tenant, shared by its projects). Ensure is idempotent on (Owner, Identity): an
// existing resource with the same SpecDigest is adopted, and one with a
// different digest is ErrSpecMismatch.
type ResourceSpec struct {
	Scope domain.Scope `json:"scope"`
	// Owner must equal OwnerFor(Lifetime, Scope).
	Owner domain.Owner `json:"owner"`
	// RunID is empty for project- and tenant-lifetime services.
	RunID    domain.RunID `json:"runId,omitempty"`
	Kind     ResourceKind `json:"kind"`
	Name     string       `json:"name"`
	Lifetime Lifetime     `json:"lifetime"`
	// Identity is the stable backend identity derived by the engine.
	Identity string `json:"identity"`
	// SpecDigest identifies this immutable specification.
	SpecDigest string `json:"specDigest"`
	// Fence is the writer's fence; a backend rejects a delayed create from an
	// older fence.
	Fence       Fence              `json:"fence"`
	Service     *ServiceSpec       `json:"service,omitempty"`
	Preparation *PreparationInputs `json:"preparation,omitempty"`
	Requires    CapabilitySet      `json:"requires,omitempty"`
}

// Validate checks the spec against its kind and lifetime.
func (r ResourceSpec) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !r.Kind.Valid() || r.Name == "" || r.Identity == "" || r.SpecDigest == "" {
		return invalid("a resource requires a kind, a name, an identity and a spec digest")
	}
	if !r.Lifetime.Valid() {
		return invalid("resource lifetime %q", r.Lifetime)
	}
	if err := r.Fence.Validate(); err != nil {
		return err
	}
	if r.Owner != OwnerFor(r.Lifetime, r.Scope) {
		return invalid("owner %s/%s does not match the %s lifetime of scope %s", r.Owner.TenantID, r.Owner.ProjectID, r.Lifetime, r.Scope)
	}
	if r.Lifetime == LifetimePipeline {
		if err := r.RunID.Validate(); err != nil {
			return err
		}
	}
	switch r.Kind {
	case ResourceWorkspace:
		if r.Lifetime != LifetimePipeline || r.Service != nil || r.Preparation == nil {
			return invalid("a workspace is run-scoped, has preparation inputs and no service")
		}
		if r.Preparation.Scope != r.Scope || r.Preparation.RunID != r.RunID {
			return invalid("preparation inputs belong to another run")
		}
		return r.Preparation.Validate()
	default:
		if r.Service == nil || r.Preparation != nil {
			return invalid("a service requires a service spec and no preparation")
		}
		if r.Service.Lifetime != r.Lifetime {
			return invalid("service lifetime %q does not match resource lifetime %q", r.Service.Lifetime, r.Lifetime)
		}
		return r.Service.Validate()
	}
}

// Handle returns the handle for the resource the spec describes.
func (r ResourceSpec) Handle() ResourceHandle {
	return ResourceHandle{Scope: r.Scope, Owner: r.Owner, Lifetime: r.Lifetime, RunID: r.RunID,
		Kind: r.Kind, Name: r.Name, Identity: r.Identity, Fence: r.Fence}
}

// ResourceHandle identifies one resource for observe, stop and remove. Scope is
// the requesting run's tenant and project; Owner and Lifetime say who owns the
// resource and how long it lives. An adapter never infers any of them from
// context, and refuses a handle whose Owner is not available to the caller's
// scope (see Reachable). Fences order writers of one run: for a shared
// (project- or tenant-lifetime) resource they compare only among handles with
// the same RunID.
type ResourceHandle struct {
	Scope    domain.Scope `json:"scope"`
	Owner    domain.Owner `json:"owner"`
	Lifetime Lifetime     `json:"lifetime"`
	RunID    domain.RunID `json:"runId,omitempty"`
	Kind     ResourceKind `json:"kind"`
	Name     string       `json:"name"`
	Identity string       `json:"identity"`
	Fence    Fence        `json:"fence"`
}

// Validate checks the handle, including that its owner follows from its
// lifetime and requesting scope.
func (h ResourceHandle) Validate() error {
	if err := h.Scope.Validate(); err != nil {
		return err
	}
	if !h.Kind.Valid() || h.Name == "" || h.Identity == "" {
		return invalid("a resource handle requires a kind, a name and an identity")
	}
	if !h.Lifetime.Valid() {
		return invalid("resource lifetime %q", h.Lifetime)
	}
	if h.Owner != OwnerFor(h.Lifetime, h.Scope) {
		return invalid("owner %s/%s does not match the %s lifetime of scope %s", h.Owner.TenantID, h.Owner.ProjectID, h.Lifetime, h.Scope)
	}
	if h.Lifetime == LifetimePipeline {
		if err := h.RunID.Validate(); err != nil {
			return err
		}
	}
	return h.Fence.Validate()
}

// Reachable reports whether a caller in scope by may use the resource: it must
// belong to the caller's tenant, and to the caller's project unless it is
// tenant-owned. Anything else is ErrForeignResource.
func (h ResourceHandle) Reachable(by domain.Scope) error {
	if !h.Owner.AvailableTo(by) {
		return ErrForeignResource
	}
	return nil
}

// ResourceObservation is a backend's report about one resource.
type ResourceObservation struct {
	Handle     ResourceHandle `json:"handle"`
	State      ResourceState  `json:"state"`
	Reason     Reason         `json:"reason,omitempty"`
	ObservedAt time.Time      `json:"observedAt"`
}

// Validate checks the observation.
func (o ResourceObservation) Validate() error {
	if err := o.Handle.Validate(); err != nil {
		return err
	}
	if !o.State.Valid() {
		return invalid("resource state %q", o.State)
	}
	if o.ObservedAt.IsZero() {
		return invalid("an observation records when it was observed")
	}
	return o.Reason.Validate()
}

// ConfirmedRemoved reports whether the backend positively confirmed removal.
// Unknown is never confirmation.
func (o ResourceObservation) ConfirmedRemoved() bool { return o.State == ResourceRemoved }

// ---------------------------------------------------------------------------
// Workloads
// ---------------------------------------------------------------------------

// RuntimeBinding ties one step attempt to one workload by stable identity. The
// engine derives WorkloadID from run, step and attempt so a restarted engine
// finds the same workload; SpecDigest is the immutable specification identity;
// Fence is the launching writer's fence so a delayed create can be rejected.
type RuntimeBinding struct {
	Scope      domain.Scope  `json:"scope"`
	RunID      domain.RunID  `json:"runId"`
	StepKey    domain.StepID `json:"stepKey"`
	Attempt    int           `json:"attempt"`
	WorkloadID string        `json:"workloadId"`
	SpecDigest string        `json:"specDigest"`
	// Backend names the adapter that holds the workload.
	Backend string `json:"backend"`
	Fence   Fence  `json:"fence"`
}

// Validate checks the binding.
func (b RuntimeBinding) Validate() error {
	if err := b.Scope.Validate(); err != nil {
		return err
	}
	if err := b.RunID.Validate(); err != nil {
		return err
	}
	if err := b.StepKey.Validate(); err != nil {
		return err
	}
	if b.Attempt < 1 || b.WorkloadID == "" || b.SpecDigest == "" || b.Backend == "" {
		return invalid("a runtime binding requires an attempt, a workload id, a spec digest and a backend")
	}
	return b.Fence.Validate()
}

// WorkloadPhase is what a backend reports about one workload.
type WorkloadPhase string

// Workload phases. Absent is a positive confirmation that nothing exists;
// Unknown means the backend could not tell.
const (
	PhaseRunning WorkloadPhase = "running"
	PhaseExited  WorkloadPhase = "exited"
	PhaseAbsent  WorkloadPhase = "absent"
	PhaseUnknown WorkloadPhase = "unknown"
)

// WorkloadObservation is a backend's report about one workload.
type WorkloadObservation struct {
	Binding    RuntimeBinding `json:"binding"`
	Phase      WorkloadPhase  `json:"phase"`
	Result     Result         `json:"result,omitempty"`
	ObservedAt time.Time      `json:"observedAt"`
}

// Validate checks the observation: an exited workload carries a result and no
// other phase does.
func (o WorkloadObservation) Validate() error {
	if err := o.Binding.Validate(); err != nil {
		return err
	}
	switch o.Phase {
	case PhaseExited:
		if err := o.Result.Validate(); err != nil {
			return err
		}
	case PhaseRunning, PhaseAbsent, PhaseUnknown:
		if o.Result != (Result{}) {
			return invalid("only an exited workload has a result")
		}
	default:
		return invalid("workload phase %q", o.Phase)
	}
	if o.ObservedAt.IsZero() {
		return invalid("an observation records when it was observed")
	}
	return nil
}

// CheckObservationUpdate reports whether next may replace prev as the retained
// observation of the same workload attempt. The identity (scope, run, step,
// attempt, workload, specification digest, backend) must match, the fence and
// observation time may not go backwards, and an exited workload keeps its
// result: it may only be repeated or confirmed absent.
func CheckObservationUpdate(prev, next WorkloadObservation) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if prev.Binding.Scope != next.Binding.Scope || prev.Binding.RunID != next.Binding.RunID ||
		prev.Binding.StepKey != next.Binding.StepKey || prev.Binding.Attempt != next.Binding.Attempt ||
		prev.Binding.WorkloadID != next.Binding.WorkloadID || prev.Binding.Backend != next.Binding.Backend {
		return invalid("observation names another workload attempt")
	}
	if prev.Binding.SpecDigest != next.Binding.SpecDigest {
		return fmt.Errorf("%w: workload %s", ErrSpecMismatch, next.Binding.WorkloadID)
	}
	if next.Binding.Fence < prev.Binding.Fence {
		return fmt.Errorf("%w: observation fence %d, retained %d", ErrStaleFence, next.Binding.Fence, prev.Binding.Fence)
	}
	if next.ObservedAt.Before(prev.ObservedAt) {
		return invalid("observation is older than the retained one")
	}
	if prev.Phase == PhaseExited && !(next.Phase == PhaseAbsent || (next.Phase == PhaseExited && next.Result == prev.Result)) {
		return fmt.Errorf("%w: an exited workload keeps its result", ErrIllegalTransition)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Command intent
// ---------------------------------------------------------------------------

// CommandKind is an external effect the engine intends to perform.
type CommandKind string

// Command kinds.
const (
	CommandLaunch         CommandKind = "launch_workload"
	CommandStop           CommandKind = "stop_workload"
	CommandRemove         CommandKind = "remove_workload"
	CommandEnsureResource CommandKind = "ensure_resource"
	CommandStopResource   CommandKind = "stop_resource"
	CommandRemoveResource CommandKind = "remove_resource"
)

// Valid reports whether k is a known command kind.
func (k CommandKind) Valid() bool {
	switch k {
	case CommandLaunch, CommandStop, CommandRemove, CommandEnsureResource, CommandStopResource, CommandRemoveResource:
		return true
	}
	return false
}

// CommandState is what is known about an intended effect.
type CommandState string

// Command states.
const (
	// CommandIntent is recorded before the effect; its outcome is not yet known.
	CommandIntent CommandState = "intent"
	// CommandConfirmed means the backend acknowledged the effect.
	CommandConfirmed CommandState = "confirmed"
	// CommandRejected means the effect affirmatively did not reach the backend.
	CommandRejected CommandState = "rejected"
	// CommandUncertain means the outcome is unknown and must be reconciled.
	CommandUncertain CommandState = "uncertain"
)

// Command is the durable intent to perform one external effect, committed
// before the effect runs. It names the target by stable identity and carries the
// writer's fence.
type Command struct {
	// ID is stable for the intended effect so a replayed commit is idempotent.
	ID      string        `json:"id"`
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey,omitempty"`
	Kind    CommandKind   `json:"kind"`
	// Target is the stable workload or resource identity.
	Target     string `json:"target"`
	SpecDigest string `json:"specDigest,omitempty"`
	Fence      Fence  `json:"fence"`
	// Binding is the full workload binding (backend, attempt, identity,
	// specification digest, fence) of a workload command, so a restarted engine
	// can observe, stop or remove the workload without transient backend
	// configuration. It is zero for resource commands.
	Binding RuntimeBinding `json:"binding,omitempty"`
	State   CommandState   `json:"state"`
	Reason  Reason         `json:"reason,omitempty"`
	// Result is the workload result the backend reported when a confirmed
	// workload command ended it; it is zero otherwise.
	Result     Result    `json:"result,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

// Workload reports whether the kind acts on a workload rather than a resource.
func (k CommandKind) Workload() bool {
	return k == CommandLaunch || k == CommandStop || k == CommandRemove
}

// NewCommand records an intent at now for a workspace or service. Workload
// commands carry a binding and use NewWorkloadCommand.
func NewCommand(id string, scope domain.Scope, run domain.RunID, step domain.StepID, kind CommandKind, target, specDigest string, fence Fence, now time.Time) (Command, error) {
	c := Command{ID: id, Scope: scope, RunID: run, StepKey: step, Kind: kind, Target: target,
		SpecDigest: specDigest, Fence: fence, State: CommandIntent, RecordedAt: now}
	return c, c.Validate()
}

// NewWorkloadCommand records an intent at now for the workload named by b. Scope,
// run, step, target, specification digest and fence all come from the binding.
func NewWorkloadCommand(id string, b RuntimeBinding, kind CommandKind, now time.Time) (Command, error) {
	c := Command{ID: id, Scope: b.Scope, RunID: b.RunID, StepKey: b.StepKey, Kind: kind, Target: b.WorkloadID,
		SpecDigest: b.SpecDigest, Fence: b.Fence, Binding: b, State: CommandIntent, RecordedAt: now}
	return c, c.Validate()
}

// Validate checks the command.
func (c Command) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.RunID.Validate(); err != nil {
		return err
	}
	if c.StepKey != "" {
		if err := c.StepKey.Validate(); err != nil {
			return err
		}
	}
	if c.ID == "" || c.Target == "" || !c.Kind.Valid() || c.RecordedAt.IsZero() {
		return invalid("a command requires an id, a kind, a target and a record time")
	}
	if err := c.Fence.Validate(); err != nil {
		return err
	}
	if err := c.Reason.Validate(); err != nil {
		return err
	}
	if err := c.validateBinding(); err != nil {
		return err
	}
	if c.Result != (Result{}) {
		if c.State != CommandConfirmed || !c.Kind.Workload() {
			return invalid("only a confirmed workload command records a result")
		}
		if err := c.Result.Validate(); err != nil {
			return err
		}
	}
	switch c.State {
	case CommandIntent, CommandConfirmed:
		if c.Reason != ReasonNone {
			return invalid("a %s command has no reason", c.State)
		}
	case CommandRejected, CommandUncertain:
		if c.Reason == ReasonNone {
			return invalid("a %s command requires a reason", c.State)
		}
	default:
		return invalid("command state %q", c.State)
	}
	return nil
}

// validateBinding requires a workload command to carry a binding that agrees
// with the command, and a resource command to carry none.
func (c Command) validateBinding() error {
	if !c.Kind.Workload() {
		if c.Binding != (RuntimeBinding{}) {
			return invalid("a %s command has no workload binding", c.Kind)
		}
		return nil
	}
	if err := c.Binding.Validate(); err != nil {
		return err
	}
	b := c.Binding
	if b.Scope != c.Scope || b.RunID != c.RunID || b.StepKey != c.StepKey || b.WorkloadID != c.Target ||
		b.SpecDigest != c.SpecDigest || b.Fence != c.Fence {
		return invalid("command %s does not match its workload binding", c.ID)
	}
	return nil
}

// RecordResult retains the workload result a confirmed workload command
// reported, for example the exit a stop observed. It changes nothing else.
func (c *Command) RecordResult(res Result) error {
	if c.State != CommandConfirmed || !c.Kind.Workload() {
		return fmt.Errorf("%w: command %s is %s", ErrIllegalTransition, c.ID, c.State)
	}
	next := *c
	next.Result = res
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

// Settle records what became known about the effect. From intent any outcome
// is possible; an uncertain command may later be confirmed or rejected by
// reconciliation; confirmed and rejected are final.
func (c *Command) Settle(to CommandState, reason Reason) error {
	legal := (c.State == CommandIntent && (to == CommandConfirmed || to == CommandRejected || to == CommandUncertain)) ||
		(c.State == CommandUncertain && (to == CommandConfirmed || to == CommandRejected))
	if !legal {
		return fmt.Errorf("%w: command %s: %s -> %s", ErrIllegalTransition, c.ID, c.State, to)
	}
	next := *c
	next.State, next.Reason = to, reason
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

// MayRetry reports whether the effect may be issued again as a new attempt.
// Only a rejected command qualifies: it is affirmatively known not to have
// reached the backend. An intent with no recorded outcome and an uncertain
// command are reconciled, never replayed.
func (c Command) MayRetry() bool { return c.State == CommandRejected }

// ClassifyEffect maps the error of an external call to the command state to
// record. Conservatively, only nil confirms and only an error wrapping
// ErrNotReached rejects; every other error, including context cancellation and
// timeouts, is uncertain.
func ClassifyEffect(err error) CommandState {
	switch {
	case err == nil:
		return CommandConfirmed
	case errors.Is(err, ErrNotReached) && !errors.Is(err, ErrUncertain):
		return CommandRejected
	default:
		return CommandUncertain
	}
}

// ---------------------------------------------------------------------------
// Run: execution outcome and cleanup are separate
// ---------------------------------------------------------------------------

// Outcome is how a run's execution ended. The empty outcome means execution has
// not ended. It says nothing about whether owned resources were cleaned up.
type Outcome string

// Run outcomes.
const (
	OutcomeNone      Outcome = ""
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
)

// Decided reports whether execution has ended with a known outcome.
func (o Outcome) Decided() bool {
	return o == OutcomeSucceeded || o == OutcomeFailed || o == OutcomeCancelled
}

// CleanupState is the state of confirming that a run's owned resources are
// stopped and removed. It is independent of the execution Outcome.
type CleanupState string

// Cleanup states.
const (
	// CleanupNotStarted is the state while execution is still in progress.
	CleanupNotStarted CleanupState = "not_started"
	// CleanupPending means resources are not yet confirmed stopped and removed.
	CleanupPending CleanupState = "pending"
	// CleanupComplete means every owned resource is confirmed stopped and removed.
	CleanupComplete CleanupState = "complete"
	// CleanupExhausted means bounded attempts ended with resources retained and
	// listed; the run is terminal but cleanup stays visibly unfinished.
	CleanupExhausted CleanupState = "exhausted"
)

// Final reports whether cleanup no longer blocks the run from being terminal.
func (s CleanupState) Final() bool { return s == CleanupComplete || s == CleanupExhausted }

// Cleanup records cleanup progress, bounded by MaxAttempts.
type Cleanup struct {
	State       CleanupState `json:"state"`
	Attempts    int          `json:"attempts"`
	MaxAttempts int          `json:"maxAttempts"`
	// Retained lists the run-owned workspaces and services not confirmed removed.
	Retained []ResourceHandle `json:"retained,omitempty"`
	// RetainedWorkloads lists the workloads whose stop or removal is not
	// confirmed, by their scoped bindings, so a later claim can reconcile them
	// without the original backend configuration.
	RetainedWorkloads []RuntimeBinding `json:"retainedWorkloads,omitempty"`
	Reason            Reason           `json:"reason,omitempty"`
}

// Remaining is what an unfinished cleanup attempt could not confirm stopped and
// removed.
type Remaining struct {
	Resources []ResourceHandle `json:"resources,omitempty"`
	Workloads []RuntimeBinding `json:"workloads,omitempty"`
}

// Empty reports whether nothing remains.
func (m Remaining) Empty() bool { return len(m.Resources) == 0 && len(m.Workloads) == 0 }

func (c Cleanup) retained() int { return len(c.Retained) + len(c.RetainedWorkloads) }

// RunStatus is the derived, read-only status of a run.
type RunStatus string

// Run statuses.
const (
	StatusQueued    RunStatus = "queued"
	StatusRunning   RunStatus = "running"
	StatusCleaning  RunStatus = "cleaning"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
	StatusCancelled RunStatus = "cancelled"
)

// Run is one execution of a pipeline plan for a tenant and project. Its steps
// are created at admission. Outcome and Cleanup are separate: a run settles
// when execution ends and is terminal only once cleanup is final. The zero Run
// is invalid and never terminal.
type Run struct {
	Scope           domain.Scope      `json:"scope"`
	ID              domain.RunID      `json:"id"`
	PipelineID      domain.PipelineID `json:"pipelineId"`
	Version         domain.Version    `json:"version"`
	Outcome         Outcome           `json:"outcome,omitempty"`
	Reason          Reason            `json:"reason,omitempty"`
	CancelRequested bool              `json:"cancelRequested,omitempty"`
	Cleanup         Cleanup           `json:"cleanup"`
	Steps           []Step            `json:"steps"`
}

// NewRun admits a run for plan: one pending step per plan step, version 1,
// cleanup not started. maxCleanupAttempts bounds cleanup retries.
func NewRun(scope domain.Scope, id domain.RunID, pipeline domain.PipelineID, plan Plan, maxCleanupAttempts int) (Run, error) {
	if err := plan.Validate(); err != nil {
		return Run{}, err
	}
	steps := make([]Step, 0, len(plan.Steps))
	for _, p := range plan.Steps {
		s, err := NewStep(p.Key, p.Kind)
		if err != nil {
			return Run{}, err
		}
		steps = append(steps, s)
	}
	r := Run{Scope: scope, ID: id, PipelineID: pipeline, Version: 1, Steps: steps,
		Cleanup: Cleanup{State: CleanupNotStarted, MaxAttempts: maxCleanupAttempts}}
	return r, r.Validate()
}

// Validate checks the run's invariants, including that outcome and cleanup agree.
func (r Run) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if err := r.PipelineID.Validate(); err != nil {
		return err
	}
	if err := r.Version.Validate(); err != nil {
		return err
	}
	if len(r.Steps) == 0 {
		return invalid("a run has at least one step")
	}
	seen := map[domain.StepID]bool{}
	for _, s := range r.Steps {
		if err := s.Validate(); err != nil {
			return err
		}
		if seen[s.Key] {
			return invalid("duplicate step %s", s.Key)
		}
		seen[s.Key] = true
	}
	if r.Outcome != OutcomeNone && !r.Outcome.Decided() {
		return invalid("run outcome %q", r.Outcome)
	}
	if err := r.Reason.Validate(); err != nil {
		return err
	}
	if r.Cleanup.MaxAttempts < 1 || r.Cleanup.Attempts < 0 || r.Cleanup.Attempts > r.Cleanup.MaxAttempts {
		return invalid("cleanup attempts %d of %d", r.Cleanup.Attempts, r.Cleanup.MaxAttempts)
	}
	if err := r.Cleanup.Reason.Validate(); err != nil {
		return err
	}
	switch r.Cleanup.State {
	case CleanupNotStarted:
		if r.Outcome.Decided() {
			return invalid("a settled run has started cleanup")
		}
	case CleanupPending, CleanupComplete, CleanupExhausted:
		if !r.Outcome.Decided() {
			return invalid("cleanup cannot start before execution ends")
		}
	default:
		return invalid("cleanup state %q", r.Cleanup.State)
	}
	if r.Cleanup.State == CleanupComplete && r.Cleanup.retained() > 0 {
		return invalid("completed cleanup retains nothing")
	}
	if r.Cleanup.State == CleanupExhausted && (r.Cleanup.retained() == 0 || r.Cleanup.Reason == ReasonNone) {
		return invalid("exhausted cleanup records what it retained and a reason")
	}
	for _, h := range r.Cleanup.Retained {
		if err := h.Validate(); err != nil {
			return err
		}
		if h.Scope != r.Scope || h.Lifetime != LifetimePipeline || h.RunID != r.ID {
			return invalid("retained resource %s is not owned by run %s", h.Name, r.ID)
		}
	}
	for _, b := range r.Cleanup.RetainedWorkloads {
		if err := b.Validate(); err != nil {
			return err
		}
		if b.Scope != r.Scope || b.RunID != r.ID || !slices.ContainsFunc(r.Steps, func(s Step) bool { return s.Key == b.StepKey }) {
			return invalid("retained workload %s is not owned by run %s", b.WorkloadID, r.ID)
		}
	}
	if r.Outcome.Decided() {
		for _, s := range r.Steps {
			if !s.State.Terminal() {
				return invalid("settled run has live step %s", s.Key)
			}
		}
	}
	if r.Outcome == OutcomeFailed && r.Reason == ReasonNone {
		return invalid("a failed run requires a reason")
	}
	if r.Outcome == OutcomeSucceeded && r.Reason != ReasonNone {
		return invalid("a succeeded run has no reason")
	}
	return nil
}

// Settled reports whether execution has ended with an outcome. A settled run
// may still be cleaning up.
func (r Run) Settled() bool { return r.Outcome.Decided() }

// Terminal reports whether the run is done: execution ended and cleanup is final.
func (r Run) Terminal() bool { return r.Settled() && r.Cleanup.State.Final() }

// Admits reports whether a claim for purpose may be granted. Cancellation and
// settlement block execution claims but still permit reconciliation and
// cleanup. A terminal run admits nothing, with one narrow exception: when
// cleanup is exhausted with resources or workloads retained, a cleanup claim is
// still admitted so late reconciliation can confirm them (ReconcileExhausted,
// CompleteCleanup). Execution and general reconciliation stay closed.
func (r Run) Admits(purpose ClaimPurpose) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !purpose.Valid() {
		return invalid("claim purpose %q", purpose)
	}
	if r.Terminal() && !(r.Cleanup.State == CleanupExhausted && purpose == ClaimCleanup) {
		return fmt.Errorf("%w: run %s is terminal", ErrExecutionClosed, r.ID)
	}
	switch purpose {
	case ClaimExecute:
		if r.CancelRequested || r.Settled() {
			return fmt.Errorf("%w: run %s", ErrExecutionClosed, r.ID)
		}
	case ClaimCleanup:
		if !r.CancelRequested && !r.Settled() {
			return fmt.Errorf("%w: run %s is still executing", ErrIllegalTransition, r.ID)
		}
	}
	return nil
}

// Status derives the run's status from its outcome, cleanup and steps.
func (r Run) Status() RunStatus {
	if r.Settled() {
		if !r.Cleanup.State.Final() {
			return StatusCleaning
		}
		switch r.Outcome {
		case OutcomeSucceeded:
			return StatusSucceeded
		case OutcomeCancelled:
			return StatusCancelled
		}
		return StatusFailed
	}
	// A run with an open wait stays running; only the shell's task projection
	// reports waiting.
	for _, s := range r.Steps {
		if s.State != StepPending {
			return StatusRunning
		}
	}
	return StatusQueued
}

// RequestCancel records that cancellation was requested. It blocks new
// execution claims and is idempotent; it does not settle the run. Cancelling a
// settled run does nothing.
func (r *Run) RequestCancel() {
	if r.Settled() || r.CancelRequested {
		return
	}
	r.CancelRequested = true
	r.Version++
}

// AdvanceStep moves one step. A settled run accepts no change. Once
// cancellation is requested no step may start running or waiting, and at most
// one container step runs at a time, so one executable step holds the workspace.
func (r *Run) AdvanceStep(key domain.StepID, to StepState, reason Reason) error {
	return r.moveStep(key, to, reason, nil, nil)
}

// EvaluateStep is the one atomic operation for a step with a when predicate: it
// retains the evaluation and moves the pending step in a single run-version
// increment, so the result commits as one Transition (ExpectedVersion + 1). A
// true evaluation leads to running or waiting (or a later skip for requires or
// cancellation); a false one to skipped with reason condition_false. The
// evaluation is recorded once; on any error nothing changes.
func (r *Run) EvaluateStep(key domain.StepID, ev PredicateEvaluation, to StepState, reason Reason) error {
	if !ev.Evaluated {
		return invalid("record an evaluated predicate")
	}
	return r.moveStep(key, to, reason, nil, &ev)
}

// RecordPredicate retains the evaluation of a pending step's when predicate on
// its own, as a separate commit: the run version advances by one and the step
// stays pending. When the evaluation and the step's move belong to the same
// transition, use EvaluateStep, which advances the version once; two operations
// combined into one Transition would advance it twice, unless collapsed with
// Run.Sealed. The evaluation is recorded once and never re-evaluated: a second
// record is refused whatever it says.
func (r *Run) RecordPredicate(key domain.StepID, ev PredicateEvaluation) error {
	if r.Settled() {
		return fmt.Errorf("%w: run %s is settled", ErrExecutionClosed, r.ID)
	}
	i := slices.IndexFunc(r.Steps, func(s Step) bool { return s.Key == key })
	if i < 0 {
		return invalid("run %s has no step %s", r.ID, key)
	}
	s := r.Steps[i]
	if s.State != StepPending || s.Predicate.Evaluated {
		return fmt.Errorf("%w: step %s is %s and its predicate is already settled", ErrIllegalTransition, key, s.State)
	}
	if !ev.Evaluated {
		return invalid("record an evaluated predicate")
	}
	s.Predicate = ev
	s.Version++
	if err := s.Validate(); err != nil {
		return err
	}
	r.setStep(i, s)
	r.Version++
	return nil
}

// setStep replaces step i without writing through a slice shared with a copy of
// the run, so a Run value copied before the change keeps its own steps.
func (r *Run) setStep(i int, s Step) {
	steps := slices.Clone(r.Steps)
	steps[i] = s
	r.Steps = steps
}

// Clone returns a copy that shares no slice with r.
func (r Run) Clone() Run {
	r.Steps = slices.Clone(r.Steps)
	r.Cleanup.Retained = slices.Clone(r.Cleanup.Retained)
	r.Cleanup.RetainedWorkloads = slices.Clone(r.Cleanup.RetainedWorkloads)
	return r
}

// Sealed batches several in-memory mutations into one Transition. Each mutating
// method counts its own version step, so a run that took several moves is ahead
// of the stored run by more than one. Sealed returns the run at exactly
// prev.Version+1, the only version a Transition may commit, after checking that
// it is a legal update of prev (see CheckRunUpdate). Something must have changed.
// Prefer EvaluateStep for the evaluation-with-move pair, which needs no batching.
func (r Run) Sealed(prev Run) (Run, error) {
	if r.Scope != prev.Scope || r.ID != prev.ID || r.PipelineID != prev.PipelineID {
		return Run{}, invalid("the run belongs to another run")
	}
	if r.Version <= prev.Version {
		return Run{}, invalid("nothing changed since version %d", prev.Version)
	}
	out := r.Clone()
	out.Version = prev.Version + 1
	if err := out.Validate(); err != nil {
		return Run{}, err
	}
	if err := CheckRunUpdate(prev, out); err != nil {
		return Run{}, err
	}
	return out, nil
}

// CheckRunUpdate reports whether next may replace prev in one Transition. It
// enforces immutability: the run version advances by one; step identities and
// kinds are unchanged; a recorded predicate evaluation is never changed or
// removed; a terminal step, and a settled outcome, never change; attempts never
// go backwards; a requested cancellation is never withdrawn. A StateStore applies
// it to the stored run inside Commit.
func CheckRunUpdate(prev, next Run) error {
	if prev.Scope != next.Scope || prev.ID != next.ID || prev.PipelineID != next.PipelineID {
		return invalid("the update belongs to another run")
	}
	if next.Version != prev.Version+1 {
		return invalid("run version %d does not follow %d", next.Version, prev.Version)
	}
	if len(prev.Steps) != len(next.Steps) {
		return invalid("an update cannot add or remove steps")
	}
	for i, p := range prev.Steps {
		n := next.Steps[i]
		if p.Key != n.Key || p.Kind != n.Kind {
			return invalid("an update cannot change step %s", p.Key)
		}
		if p.Predicate.Evaluated && p.Predicate != n.Predicate {
			return fmt.Errorf("%w: step %s predicate was evaluated once and is never re-evaluated", ErrIllegalTransition, p.Key)
		}
		if p.State.Terminal() && (n.State != p.State || n.Reason != p.Reason || n.Result != p.Result || n.Attempt != p.Attempt) {
			return fmt.Errorf("%w: step %s is %s and final", ErrIllegalTransition, p.Key, p.State)
		}
		if n.Attempt < p.Attempt {
			return fmt.Errorf("%w: step %s attempt went backwards", ErrIllegalTransition, p.Key)
		}
	}
	if prev.Settled() && (next.Outcome != prev.Outcome || next.Reason != prev.Reason) {
		return fmt.Errorf("%w: run %s already settled as %s", ErrIllegalTransition, prev.ID, prev.Outcome)
	}
	if prev.CancelRequested && !next.CancelRequested {
		return fmt.Errorf("%w: a cancellation is never withdrawn", ErrIllegalTransition)
	}
	return nil
}

// FinishStep settles a running container step with the workload's result, which
// is retained on the step for conditions and recovery. See Step.Finish.
func (r *Run) FinishStep(key domain.StepID, to StepState, reason Reason, res Result) error {
	return r.moveStep(key, to, reason, &res, nil)
}

// RetryStep starts the next workload attempt of a running container step. It is
// allowed only for a launch command of the current attempt that is rejected,
// that is, affirmatively known not to have reached the backend. An intent with
// no outcome and an uncertain launch are reconciled, never retried. A cancelling
// or settled run retries nothing.
func (r *Run) RetryStep(key domain.StepID, launch Command) error {
	if r.Settled() || r.CancelRequested {
		return fmt.Errorf("%w: run %s", ErrExecutionClosed, r.ID)
	}
	i := slices.IndexFunc(r.Steps, func(s Step) bool { return s.Key == key })
	if i < 0 {
		return invalid("run %s has no step %s", r.ID, key)
	}
	s := r.Steps[i]
	if s.Kind != StepContainer || s.State != StepRunning {
		return fmt.Errorf("%w: step %s is not a running container step", ErrIllegalTransition, key)
	}
	if err := launch.Validate(); err != nil {
		return err
	}
	if launch.Kind != CommandLaunch || launch.Scope != r.Scope || launch.RunID != r.ID || launch.StepKey != key ||
		launch.Binding.Attempt != s.Attempt {
		return invalid("command %s is not the launch of step %s attempt %d", launch.ID, key, s.Attempt)
	}
	if !launch.MayRetry() {
		return fmt.Errorf("%w: launch %s is %s, which must be reconciled, not retried", ErrIllegalTransition, launch.ID, launch.State)
	}
	s.Attempt++
	s.Version++
	r.setStep(i, s)
	r.Version++
	return nil
}

func (r *Run) moveStep(key domain.StepID, to StepState, reason Reason, res *Result, ev *PredicateEvaluation) error {
	if r.Settled() {
		return fmt.Errorf("%w: run %s is settled", ErrExecutionClosed, r.ID)
	}
	i := slices.IndexFunc(r.Steps, func(s Step) bool { return s.Key == key })
	if i < 0 {
		return invalid("run %s has no step %s", r.ID, key)
	}
	if to == StepRunning || to == StepWaiting {
		if r.CancelRequested {
			return fmt.Errorf("%w: run %s is cancelling", ErrExecutionClosed, r.ID)
		}
		if to == StepRunning {
			for _, s := range r.Steps {
				if s.Kind == StepContainer && s.State == StepRunning {
					return fmt.Errorf("%w: step %s still holds the workspace", ErrIllegalTransition, s.Key)
				}
			}
		}
	}
	s := r.Steps[i]
	if ev != nil {
		if s.State != StepPending || s.Predicate.Evaluated {
			return fmt.Errorf("%w: step %s is %s and its predicate is already settled", ErrIllegalTransition, key, s.State)
		}
		s.Predicate = *ev
	}
	var err error
	if res != nil {
		err = s.Finish(to, reason, *res)
	} else {
		err = s.Advance(to, reason)
	}
	if err != nil {
		return err
	}
	r.setStep(i, s)
	r.Version++
	return nil
}

// Settle records the execution outcome once, after every step is terminal, and
// starts cleanup. Succeeded requires no failed or cancelled step; cancelled
// requires a requested cancellation; failed requires a reason. A second Settle
// is refused: the first outcome stands.
func (r *Run) Settle(outcome Outcome, reason Reason) error {
	if r.Settled() {
		return fmt.Errorf("%w: run %s already settled as %s", ErrIllegalTransition, r.ID, r.Outcome)
	}
	if !outcome.Decided() {
		return invalid("run outcome %q", outcome)
	}
	if outcome == OutcomeCancelled && !r.CancelRequested {
		return invalid("a run is cancelled only after cancellation was requested")
	}
	if outcome == OutcomeSucceeded {
		for _, s := range r.Steps {
			if s.State == StepFailed || s.State == StepCancelled {
				return invalid("run cannot succeed with %s step %s", s.State, s.Key)
			}
		}
	}
	next := *r
	next.Outcome, next.Reason = outcome, reason
	next.Cleanup.State = CleanupPending
	next.Version++
	if err := next.Validate(); err != nil {
		return err
	}
	*r = next
	return nil
}

// RecordCleanupAttempt records an unfinished cleanup attempt that left retained
// resources. When the bound is reached the cleanup becomes exhausted: the run
// turns terminal with the retained identities and reason recorded.
func (r *Run) RecordCleanupAttempt(retained []ResourceHandle, reason Reason) error {
	return r.RecordCleanupRemaining(Remaining{Resources: retained}, reason)
}

// RecordCleanupRemaining is RecordCleanupAttempt for any mix of retained
// resources and workloads, so a workload whose stop or removal is uncertain is
// recorded by its binding rather than lost.
func (r *Run) RecordCleanupRemaining(rem Remaining, reason Reason) error {
	if r.Cleanup.State != CleanupPending {
		return fmt.Errorf("%w: cleanup is %s", ErrIllegalTransition, r.Cleanup.State)
	}
	if rem.Empty() || reason == ReasonNone {
		return invalid("an unfinished cleanup attempt retains something and gives a reason")
	}
	next := *r
	next.Cleanup.Attempts++
	next.Cleanup.Retained = slices.Clone(rem.Resources)
	next.Cleanup.RetainedWorkloads = slices.Clone(rem.Workloads)
	next.Cleanup.Reason = reason
	if next.Cleanup.Attempts >= next.Cleanup.MaxAttempts {
		next.Cleanup.State = CleanupExhausted
	}
	next.Version++
	if err := next.Validate(); err != nil {
		return err
	}
	*r = next
	return nil
}

// CompleteCleanup records that every owned resource is confirmed stopped and
// removed. It is allowed from pending, and from exhausted when a late
// reconciliation finishes the job.
func (r *Run) CompleteCleanup() error {
	if r.Cleanup.State != CleanupPending && r.Cleanup.State != CleanupExhausted {
		return fmt.Errorf("%w: cleanup is %s", ErrIllegalTransition, r.Cleanup.State)
	}
	r.Cleanup.State, r.Cleanup.Retained, r.Cleanup.RetainedWorkloads, r.Cleanup.Reason = CleanupComplete, nil, nil, ReasonNone
	r.Version++
	return nil
}

// ReconcileExhausted records progress made by a late cleanup claim after the
// attempt bound was exhausted. remaining must be a subset of what is retained;
// nothing new can appear. An empty remaining completes cleanup; otherwise the
// run stays exhausted with the smaller retained set. It never reopens
// execution.
func (r *Run) ReconcileExhausted(remaining Remaining) error {
	if r.Cleanup.State != CleanupExhausted {
		return fmt.Errorf("%w: cleanup is %s", ErrIllegalTransition, r.Cleanup.State)
	}
	for _, h := range remaining.Resources {
		if !slices.Contains(r.Cleanup.Retained, h) {
			return invalid("resource %s was not retained", h.Name)
		}
	}
	for _, b := range remaining.Workloads {
		if !slices.Contains(r.Cleanup.RetainedWorkloads, b) {
			return invalid("workload %s was not retained", b.WorkloadID)
		}
	}
	if remaining.Empty() {
		return r.CompleteCleanup()
	}
	r.Cleanup.Retained = slices.Clone(remaining.Resources)
	r.Cleanup.RetainedWorkloads = slices.Clone(remaining.Workloads)
	r.Version++
	return nil
}

// ---------------------------------------------------------------------------
// Events and transitions
// ---------------------------------------------------------------------------

// EventKind names an execution event. Values follow the event catalog.
type EventKind string

// Event kinds the kernel emits as outbox intent.
const (
	EventRunCreated          EventKind = "run.created"
	EventRunStateChanged     EventKind = "run.state_changed"
	EventStepStateChanged    EventKind = "run_step.state_changed"
	EventWaitOpened          EventKind = "wait.opened"
	EventWaitResolved        EventKind = "wait.resolved"
	EventWaitExpired         EventKind = "wait.expired"
	EventWaitCancelled       EventKind = "wait.cancelled"
	EventSignalRefused       EventKind = "signal.refused"
	EventServiceStateChanged EventKind = "service.state_changed"
)

// Event is outbox intent committed with the state change it describes. The
// shell publishes it; consumers deduplicate on ID. It carries scope explicitly.
type Event struct {
	// ID is the deduplication key, stable for the committed change.
	ID      string        `json:"id"`
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey,omitempty"`
	Kind    EventKind     `json:"kind"`
	// Version is the run, step or wait version the event describes.
	Version domain.Version `json:"version"`
	Reason  Reason         `json:"reason,omitempty"`
	At      time.Time      `json:"at"`
}

// Validate checks the event.
func (e Event) Validate() error {
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if err := e.RunID.Validate(); err != nil {
		return err
	}
	if e.StepKey != "" {
		if err := e.StepKey.Validate(); err != nil {
			return err
		}
	}
	if e.ID == "" || e.Kind == "" || e.At.IsZero() {
		return invalid("an event requires an id, a kind and a time")
	}
	if err := e.Version.Validate(); err != nil {
		return err
	}
	return e.Reason.Validate()
}

// Transition is one atomic, claim-fenced, version-checked state change with its
// outbox intent. A StateStore commits all of it or none of it.
type Transition struct {
	Scope domain.Scope `json:"scope"`
	RunID domain.RunID `json:"runId"`
	// Claim is the writer's claim; the store refuses a stale fence or expired lease.
	Claim Claim `json:"claim"`
	// Now is the commit time supplied by the engine's Clock.
	Now time.Time `json:"now"`
	// ExpectedVersion is the run version the writer read; Run.Version must be one more.
	ExpectedVersion domain.Version `json:"expectedVersion"`
	Run             Run            `json:"run"`
	// Waits are upserted; each is version-checked against the stored wait.
	Waits []Wait `json:"waits,omitempty"`
	// Commands are upserted by ID.
	Commands []Command `json:"commands,omitempty"`
	// Workloads are the retained observations of workload attempts, upserted by
	// (step, attempt) under CheckObservationUpdate. They carry the bindings and
	// results recovery and conditions read back.
	Workloads []WorkloadObservation `json:"workloads,omitempty"`
	Resources []ResourceObservation `json:"resources,omitempty"`
	// Outputs are the declared outputs of a step this transition settles as
	// succeeded (CheckOutputs); they commit with the step and never change.
	Outputs []StepOutputs `json:"outputs,omitempty"`
	// Materializations are issued before the engine asks a SecretResolver for
	// material and revoked after it confirmed removal (CheckMaterializations),
	// upserted by identity.
	Materializations []MaterializationRecord `json:"materializations,omitempty"`
	// Events are the outbox intent committed with the change.
	Events []Event `json:"events,omitempty"`
	// RenewTTL, when positive, extends the writer's lease to Now+RenewTTL (never
	// shortening it) in the same atomic commit, keeping owner and fence.
	RenewTTL time.Duration `json:"renewTtl,omitempty"`
}

// Validate checks the transition is internally consistent: one run and scope
// throughout, a valid claim for that run, and a run version advanced by exactly
// one. It does not compare with stored state.
func (t Transition) Validate() error {
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if err := t.RunID.Validate(); err != nil {
		return err
	}
	if err := t.Claim.Validate(); err != nil {
		return err
	}
	if t.Claim.Scope != t.Scope || t.Claim.RunID != t.RunID {
		return invalid("claim belongs to another run")
	}
	if t.Now.IsZero() {
		return invalid("a transition records its time")
	}
	if err := t.Run.Validate(); err != nil {
		return err
	}
	if t.Run.Scope != t.Scope || t.Run.ID != t.RunID {
		return invalid("run belongs to another scope")
	}
	if err := t.ExpectedVersion.Validate(); err != nil {
		return err
	}
	if t.Run.Version != t.ExpectedVersion+1 {
		return invalid("run version %d does not follow expected %d", t.Run.Version, t.ExpectedVersion)
	}
	for _, w := range t.Waits {
		if err := w.Validate(); err != nil {
			return err
		}
		if w.Scope != t.Scope || w.RunID != t.RunID {
			return invalid("wait %s belongs to another run", w.ID)
		}
	}
	for _, c := range t.Commands {
		if err := c.Validate(); err != nil {
			return err
		}
		if c.Scope != t.Scope || c.RunID != t.RunID {
			return invalid("command %s belongs to another run", c.ID)
		}
	}
	if t.RenewTTL < 0 {
		return invalid("renewal ttl must not be negative")
	}
	for _, w := range t.Workloads {
		if err := w.Validate(); err != nil {
			return err
		}
		b := w.Binding
		if b.Scope != t.Scope || b.RunID != t.RunID {
			return invalid("workload %s belongs to another run", b.WorkloadID)
		}
		i := slices.IndexFunc(t.Run.Steps, func(s Step) bool { return s.Key == b.StepKey })
		if i < 0 || t.Run.Steps[i].Kind != StepContainer || b.Attempt > t.Run.Steps[i].Attempt {
			return invalid("workload %s does not match a launched container step", b.WorkloadID)
		}
	}
	for _, o := range t.Resources {
		if err := o.Validate(); err != nil {
			return err
		}
		h := o.Handle
		if h.Scope != t.Scope || (h.Lifetime == LifetimePipeline && h.RunID != t.RunID) {
			return invalid("resource %s belongs to another run", h.Name)
		}
		if err := h.Reachable(t.Scope); err != nil {
			return fmt.Errorf("%w: resource %s: %w", domain.ErrInvalidValue, h.Name, err)
		}
	}
	for _, o := range t.Outputs {
		if err := o.Validate(); err != nil {
			return err
		}
	}
	if err := checkMaterializationsOf(t.Run, t.Materializations); err != nil {
		return err
	}
	for _, e := range t.Events {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Scope != t.Scope || e.RunID != t.RunID {
			return invalid("event %s belongs to another run", e.ID)
		}
	}
	return nil
}

// MaxLogChunkBytes bounds one LogChunk so a sink can enforce retention limits.
const MaxLogChunkBytes = 64 * 1024

// LogChunk is one bounded piece of workload or resource output. Sequence is
// strictly increasing per (run, step, source) so a sink deduplicates a replay.
type LogChunk struct {
	Scope domain.Scope `json:"scope"`
	RunID domain.RunID `json:"runId"`
	// StepKey is empty for resource logs.
	StepKey domain.StepID `json:"stepKey,omitempty"`
	// Source names the workload or resource that produced the output.
	Source   string    `json:"source"`
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	Data     []byte    `json:"data"`
}

// Validate checks the chunk is scoped, attributed and bounded.
func (c LogChunk) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.RunID.Validate(); err != nil {
		return err
	}
	if c.StepKey != "" {
		if err := c.StepKey.Validate(); err != nil {
			return err
		}
	}
	if c.Source == "" || c.At.IsZero() {
		return invalid("a log chunk requires a source and a time")
	}
	if len(c.Data) > MaxLogChunkBytes {
		return invalid("log chunk exceeds %d bytes", MaxLogChunkBytes)
	}
	return nil
}
