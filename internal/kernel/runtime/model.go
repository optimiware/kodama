package runtime

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

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
	// ErrWaitClosed reports a signal for a wait already consumed by another
	// signal, expired or cancelled.
	ErrWaitClosed = errors.New("runtime: wait closed")
	// ErrWaitExpired reports a signal that arrived at or after the deadline.
	ErrWaitExpired = errors.New("runtime: wait deadline passed")
	// ErrWaitNotOpen reports a signal for a wait that was not yet opened.
	ErrWaitNotOpen = errors.New("runtime: wait not open")
	// ErrWaitStale reports a signal issued against an older wait version.
	ErrWaitStale = errors.New("runtime: stale wait version")
	// ErrWaitForeign reports a signal naming another tenant, project, run,
	// step, wait or correlation.
	ErrWaitForeign = errors.New("runtime: foreign wait")
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

// Renew extends an active claim without changing its fence. An expired claim
// cannot be renewed; it must be re-acquired, which increments the fence.
func (c Claim) Renew(now time.Time, ttl time.Duration) (Claim, error) {
	if ttl <= 0 {
		return Claim{}, invalid("claim ttl must be positive")
	}
	if !c.Active(now) {
		return Claim{}, ErrClaimExpired
	}
	c.Expires = now.Add(ttl)
	return c, nil
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
	// StepWait pauses the run, with no workload, until a correlated signal or deadline.
	StepWait StepKind = "wait"
	// StepCondition evaluates a bounded predicate over declared values and selects what runs.
	StepCondition StepKind = "condition"
)

// Valid reports whether k is a known kind. The zero kind is invalid.
func (k StepKind) Valid() bool {
	return k == StepContainer || k == StepWait || k == StepCondition
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
// steps wait and never run or skip; condition steps settle at once.
var stepTransitions = map[StepKind]map[StepState][]StepState{
	StepContainer: {
		StepPending: {StepRunning, StepSkipped, StepFailed, StepCancelled},
		StepRunning: {StepSucceeded, StepFailed, StepCancelled},
	},
	StepWait: {
		StepPending: {StepWaiting, StepCancelled},
		StepWaiting: {StepSucceeded, StepFailed, StepCancelled},
	},
	StepCondition: {
		StepPending: {StepSucceeded, StepFailed, StepSkipped, StepCancelled},
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
	if s.State == StepSkipped && s.Kind == StepWait {
		return invalid("a wait step cannot be skipped")
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

func (s Step) reasonRequired() bool {
	return s.State == StepFailed || s.State == StepSkipped || s.State == StepCancelled
}

// Advance moves the step to a legal next state, bumping its version. Failed,
// skipped and cancelled steps require a reason; others reject one. A rejected
// move leaves the step unchanged.
func (s *Step) Advance(to StepState, reason Reason) error {
	if !CanStep(s.Kind, s.State, to) {
		return fmt.Errorf("%w: %s step %s: %s -> %s", ErrIllegalTransition, s.Kind, s.Key, s.State, to)
	}
	next := *s
	next.State, next.Reason, next.Version = to, reason, s.Version+1
	if err := next.Validate(); err != nil {
		return err
	}
	*s = next
	return nil
}

// ---------------------------------------------------------------------------
// Waits
// ---------------------------------------------------------------------------

// WaitTargetKind says what a wait is for.
type WaitTargetKind string

// Wait target kinds.
const (
	// WaitForRole is assigned to a role that resolves it.
	WaitForRole WaitTargetKind = "role"
	// WaitForExternal is resolved by an external callback with a correlation.
	WaitForExternal WaitTargetKind = "external"
)

// WaitSpec is the declared shape of a wait step. See the pipeline format's
// steps[].wait (for, input, timeout).
type WaitSpec struct {
	ForKind WaitTargetKind `json:"forKind"`
	// For is the role name or external correlation family.
	For string `json:"for"`
	// Input names the expected input the resolution must supply.
	Input string `json:"input"`
	// Timeout is the deadline measured from opening; zero means none.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Validate checks the spec.
func (w WaitSpec) Validate() error {
	if w.ForKind != WaitForRole && w.ForKind != WaitForExternal {
		return invalid("wait target kind %q", w.ForKind)
	}
	if strings.TrimSpace(w.For) == "" || strings.TrimSpace(w.Input) == "" {
		return invalid("a wait requires a target and an expected input")
	}
	if w.Timeout < 0 {
		return invalid("wait timeout must not be negative")
	}
	return nil
}

// WaitID is the unique identity of one wait of one run.
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

// Wait states.
const (
	WaitPending   WaitState = "pending"
	WaitOpen      WaitState = "open"
	WaitConsumed  WaitState = "consumed"
	WaitExpired   WaitState = "expired"
	WaitCancelled WaitState = "cancelled"
)

// Closed reports whether the wait can no longer change.
func (s WaitState) Closed() bool {
	return s == WaitConsumed || s == WaitExpired || s == WaitCancelled
}

// WaitResult is the immutable record of the one signal that consumed a wait.
type WaitResult struct {
	SignalID string `json:"signalId"`
	Actor    string `json:"actor"`
	// Outcome is the bounded lower_snake_case answer (for example approved or
	// rejected). A rejection is a recorded outcome, not an infrastructure failure.
	Outcome string `json:"outcome"`
	// InputRef references the immutable input the shell recorded.
	InputRef string    `json:"inputRef,omitempty"`
	At       time.Time `json:"at"`
}

// Wait is the durable record of one wait step. Assignment (Spec) and
// correlation are separate; every changed candidate needs a fresh wait ID.
type Wait struct {
	Scope       domain.Scope   `json:"scope"`
	ID          WaitID         `json:"id"`
	RunID       domain.RunID   `json:"runId"`
	StepKey     domain.StepID  `json:"stepKey"`
	Spec        WaitSpec       `json:"spec"`
	Correlation string         `json:"correlation"`
	State       WaitState      `json:"state"`
	Version     domain.Version `json:"version"`
	OpenedAt    time.Time      `json:"openedAt,omitempty"`
	// Deadline is zero when the wait has no timeout.
	Deadline time.Time  `json:"deadline,omitempty"`
	Result   WaitResult `json:"result,omitempty"`
}

// NewWait returns a pending wait at version 1. It is committed before it is
// announced; Open moves it to open.
func NewWait(scope domain.Scope, id WaitID, run domain.RunID, step domain.StepID, spec WaitSpec, correlation string) (Wait, error) {
	w := Wait{Scope: scope, ID: id, RunID: run, StepKey: step, Spec: spec, Correlation: correlation, State: WaitPending, Version: 1}
	return w, w.Validate()
}

// Validate checks the wait's shape and state-dependent fields.
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
	if strings.TrimSpace(w.Correlation) == "" {
		return invalid("a wait requires a correlation")
	}
	if err := w.Version.Validate(); err != nil {
		return err
	}
	switch w.State {
	case WaitPending:
		if !w.OpenedAt.IsZero() {
			return invalid("a pending wait has no open time")
		}
	case WaitOpen, WaitConsumed, WaitExpired, WaitCancelled:
		if w.OpenedAt.IsZero() && w.State != WaitCancelled {
			return invalid("a %s wait records when it opened", w.State)
		}
	default:
		return invalid("wait state %q", w.State)
	}
	if w.State == WaitConsumed {
		if w.Result.SignalID == "" || w.Result.Actor == "" {
			return invalid("a consumed wait records its signal and actor")
		}
		if err := validCode("wait outcome", w.Result.Outcome); err != nil {
			return err
		}
	} else if w.Result != (WaitResult{}) {
		return invalid("only a consumed wait has a result")
	}
	return nil
}

// Open moves a pending wait to open and fixes its deadline. Callers commit the
// open wait before announcing it.
func (w *Wait) Open(now time.Time) error {
	if w.State != WaitPending {
		return fmt.Errorf("%w: wait %s is %s", ErrIllegalTransition, w.ID, w.State)
	}
	w.State, w.OpenedAt, w.Version = WaitOpen, now, w.Version+1
	if w.Spec.Timeout > 0 {
		w.Deadline = now.Add(w.Spec.Timeout)
	}
	return nil
}

// Signal is an authorized, correlated resolution of one wait. The shell
// authorizes the actor; the kernel only checks identity, version and state.
type Signal struct {
	Scope       domain.Scope  `json:"scope"`
	ID          string        `json:"id"`
	RunID       domain.RunID  `json:"runId"`
	StepKey     domain.StepID `json:"stepKey"`
	WaitID      WaitID        `json:"waitId"`
	Correlation string        `json:"correlation"`
	// WaitVersion is the open wait version the signal was issued against.
	WaitVersion domain.Version `json:"waitVersion"`
	Actor       string         `json:"actor"`
	Outcome     string         `json:"outcome"`
	InputRef    string         `json:"inputRef,omitempty"`
}

// Validate checks the signal.
func (s Signal) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	if err := s.RunID.Validate(); err != nil {
		return err
	}
	if err := s.StepKey.Validate(); err != nil {
		return err
	}
	if err := s.WaitID.Validate(); err != nil {
		return err
	}
	if err := s.WaitVersion.Validate(); err != nil {
		return err
	}
	if s.ID == "" || s.Actor == "" || s.Correlation == "" {
		return invalid("a signal requires an id, an actor and a correlation")
	}
	return validCode("wait outcome", s.Outcome)
}

// Consumption is the committed result of consuming a wait. Duplicate is true
// when the same signal was already consumed: the recorded result is returned
// and no second continuation is created.
type Consumption struct {
	Result    WaitResult `json:"result"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

// Consume applies sig at now. Exactly one signal consumes an open wait. A
// repeat of the consuming signal returns the recorded result as a duplicate; a
// different signal, or one for an expired, cancelled, stale, unopened or
// foreign wait, is refused and changes nothing.
func (w *Wait) Consume(sig Signal, now time.Time) (Consumption, error) {
	if err := sig.Validate(); err != nil {
		return Consumption{}, err
	}
	if err := w.Validate(); err != nil {
		return Consumption{}, err
	}
	if sig.Scope != w.Scope || sig.RunID != w.RunID || sig.StepKey != w.StepKey ||
		sig.WaitID != w.ID || sig.Correlation != w.Correlation {
		return Consumption{}, ErrWaitForeign
	}
	switch w.State {
	case WaitConsumed:
		if w.Result.SignalID == sig.ID {
			return Consumption{Result: w.Result, Duplicate: true}, nil
		}
		return Consumption{}, ErrWaitClosed
	case WaitExpired, WaitCancelled:
		return Consumption{}, ErrWaitClosed
	case WaitPending:
		return Consumption{}, ErrWaitNotOpen
	}
	if sig.WaitVersion != w.Version {
		return Consumption{}, fmt.Errorf("%w: signal %d, wait %d", ErrWaitStale, sig.WaitVersion, w.Version)
	}
	if !w.Deadline.IsZero() && !now.Before(w.Deadline) {
		return Consumption{}, ErrWaitExpired
	}
	w.State, w.Version = WaitConsumed, w.Version+1
	w.Result = WaitResult{SignalID: sig.ID, Actor: sig.Actor, Outcome: sig.Outcome, InputRef: sig.InputRef, At: now}
	return Consumption{Result: w.Result}, nil
}

// Expire closes an open wait whose deadline has passed. The engine then takes
// the declared timeout branch, or fails the run when there is none.
func (w *Wait) Expire(now time.Time) error {
	if w.State != WaitOpen {
		return fmt.Errorf("%w: wait %s is %s", ErrWaitClosed, w.ID, w.State)
	}
	if w.Deadline.IsZero() || now.Before(w.Deadline) {
		return ErrWaitNotDue
	}
	w.State, w.Version = WaitExpired, w.Version+1
	return nil
}

// Cancel closes a pending or open wait. A closed wait is unchanged and refused.
func (w *Wait) Cancel() error {
	if w.State != WaitPending && w.State != WaitOpen {
		return fmt.Errorf("%w: wait %s is %s", ErrWaitClosed, w.ID, w.State)
	}
	w.State, w.Version = WaitCancelled, w.Version+1
	return nil
}

// CheckStepWait verifies that a wait step and its wait record agree: a waiting
// step has an open wait, a pending step a pending one, and a terminal step a
// closed one.
func CheckStepWait(s Step, w Wait) error {
	if s.Kind != StepWait {
		return invalid("step %s is not a wait step", s.Key)
	}
	if s.Key != w.StepKey {
		return invalid("wait %s belongs to step %s", w.ID, w.StepKey)
	}
	ok := false
	switch {
	case s.State == StepPending:
		ok = w.State == WaitPending
	case s.State == StepWaiting:
		ok = w.State == WaitOpen
	case s.State.Terminal():
		ok = w.State.Closed()
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

// PlanStep is one step of an admitted plan. Kind selects exactly one of
// Workload (container), Wait (wait) or When (condition).
type PlanStep struct {
	Key       domain.StepID   `json:"key"`
	Kind      StepKind        `json:"kind"`
	DependsOn []domain.StepID `json:"dependsOn,omitempty"`
	// When is an optional bounded predicate over declared inputs, prior step
	// results and wait outcomes. A condition step requires it. The grammar is
	// frozen by the pipeline format, not here.
	When     string        `json:"when,omitempty"`
	Workload *WorkloadSpec `json:"workload,omitempty"`
	Wait     *WaitSpec     `json:"wait,omitempty"`
}

// Validate checks the step against its kind.
func (p PlanStep) Validate() error {
	if err := p.Key.Validate(); err != nil {
		return err
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
	case StepCondition:
		if p.Wait != nil || p.Workload != nil || strings.TrimSpace(p.When) == "" {
			return invalid("condition step %s requires only a condition", p.Key)
		}
	default:
		return invalid("step %s kind %q", p.Key, p.Kind)
	}
	return err
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
	Steps    []PlanStep        `json:"steps"`
	Services []ServiceSpec     `json:"services,omitempty"`
	Inputs   map[string]string `json:"inputs,omitempty"`
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
	seen := map[domain.StepID]bool{}
	for _, s := range p.Steps {
		if err := s.Validate(); err != nil {
			return err
		}
		for _, d := range s.DependsOn {
			if !seen[d] {
				return invalid("step %s depends on %q, which is not an earlier step", s.Key, d)
			}
		}
		if seen[s.Key] {
			return invalid("duplicate step %s", s.Key)
		}
		seen[s.Key] = true
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

// ResourceSpec asks a ResourceManager to ensure one workspace or service.
// Ensure is idempotent on Identity: an existing resource with the same
// SpecDigest is adopted, and one with a different digest is ErrSpecMismatch.
type ResourceSpec struct {
	Scope domain.Scope `json:"scope"`
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

// ResourceHandle identifies one resource for observe, stop and remove. It
// carries scope and fence; an adapter never infers either from context.
type ResourceHandle struct {
	Scope    domain.Scope `json:"scope"`
	RunID    domain.RunID `json:"runId,omitempty"`
	Kind     ResourceKind `json:"kind"`
	Name     string       `json:"name"`
	Identity string       `json:"identity"`
	Fence    Fence        `json:"fence"`
}

// Validate checks the handle.
func (h ResourceHandle) Validate() error {
	if err := h.Scope.Validate(); err != nil {
		return err
	}
	if !h.Kind.Valid() || h.Name == "" || h.Identity == "" {
		return invalid("a resource handle requires a kind, a name and an identity")
	}
	return h.Fence.Validate()
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
	Target     string       `json:"target"`
	SpecDigest string       `json:"specDigest,omitempty"`
	Fence      Fence        `json:"fence"`
	State      CommandState `json:"state"`
	Reason     Reason       `json:"reason,omitempty"`
	RecordedAt time.Time    `json:"recordedAt"`
}

// NewCommand records an intent at now.
func NewCommand(id string, scope domain.Scope, run domain.RunID, step domain.StepID, kind CommandKind, target, specDigest string, fence Fence, now time.Time) (Command, error) {
	c := Command{ID: id, Scope: scope, RunID: run, StepKey: step, Kind: kind, Target: target,
		SpecDigest: specDigest, Fence: fence, State: CommandIntent, RecordedAt: now}
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
	// Retained lists the resources not confirmed removed.
	Retained []ResourceHandle `json:"retained,omitempty"`
	Reason   Reason           `json:"reason,omitempty"`
}

// RunStatus is the derived, read-only status of a run.
type RunStatus string

// Run statuses.
const (
	StatusQueued    RunStatus = "queued"
	StatusRunning   RunStatus = "running"
	StatusWaiting   RunStatus = "waiting"
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
	if r.Cleanup.State == CleanupComplete && len(r.Cleanup.Retained) > 0 {
		return invalid("completed cleanup retains no resources")
	}
	if r.Cleanup.State == CleanupExhausted && (len(r.Cleanup.Retained) == 0 || r.Cleanup.Reason == ReasonNone) {
		return invalid("exhausted cleanup records the retained resources and a reason")
	}
	for _, h := range r.Cleanup.Retained {
		if err := h.Validate(); err != nil {
			return err
		}
		if h.Scope != r.Scope {
			return invalid("retained resource belongs to %s", h.Scope)
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
// cleanup; a terminal run admits nothing.
func (r Run) Admits(purpose ClaimPurpose) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !purpose.Valid() {
		return invalid("claim purpose %q", purpose)
	}
	if r.Terminal() {
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
	started, waiting := false, false
	for _, s := range r.Steps {
		switch {
		case s.State == StepRunning:
			return StatusRunning
		case s.State == StepWaiting:
			waiting = true
		case s.State != StepPending:
			started = true
		}
	}
	if waiting {
		return StatusWaiting
	}
	if started {
		return StatusRunning
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
	if err := r.Steps[i].Advance(to, reason); err != nil {
		return err
	}
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
	if r.Cleanup.State != CleanupPending {
		return fmt.Errorf("%w: cleanup is %s", ErrIllegalTransition, r.Cleanup.State)
	}
	if len(retained) == 0 || reason == ReasonNone {
		return invalid("an unfinished cleanup attempt retains resources and gives a reason")
	}
	next := *r
	next.Cleanup.Attempts++
	next.Cleanup.Retained = slices.Clone(retained)
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
	r.Cleanup.State, r.Cleanup.Retained, r.Cleanup.Reason = CleanupComplete, nil, ReasonNone
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
	Commands  []Command             `json:"commands,omitempty"`
	Resources []ResourceObservation `json:"resources,omitempty"`
	// Events are the outbox intent committed with the change.
	Events []Event `json:"events,omitempty"`
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
	for _, o := range t.Resources {
		if err := o.Validate(); err != nil {
			return err
		}
		if o.Handle.Scope != t.Scope {
			return invalid("resource %s belongs to another scope", o.Handle.Name)
		}
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
