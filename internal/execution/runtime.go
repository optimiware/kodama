package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Runtime is the backend that runs attempt workloads: the only seam between
// orchestration and the place work executes. Execution commits intent first,
// calls the runtime outside any transaction and records only what the runtime
// affirmatively reports. Adapters keep their backend's objects (processes,
// containers, Jobs) to themselves.
//
// Every method is idempotent and may be repeated after a lost response or a
// controller restart. An implementation acts only on objects it can prove are
// the attempt's workload, by name, instance UID and spec, and never on a
// foreign object holding the same name.
type Runtime interface {
	// Start creates the attempt's workload, or adopts the one an earlier call
	// created. It returns ErrConflict for another spec or a foreign object at
	// the workload name, and never launches a workload that was stopped first.
	// Any other error leaves the outcome unknown: Start is repeated with the
	// same spec.
	Start(ctx context.Context, spec AttemptSpec) (Workload, error)
	// Observe reports verified evidence about a running workload: dispatched,
	// started or failed. ErrNotFound means the workload is affirmatively
	// absent; ErrConflict that the object found is not the attempt's.
	Observe(ctx context.Context, w Workload) (Observation, error)
	// Logs copies the workload's captured output to dst. It does not follow.
	Logs(ctx context.Context, w Workload, dst io.Writer) error
	// Stop terminates the workload. Stopped is true only once it no longer
	// runs and no launch can still arrive; callers repeat Stop until then. The
	// evidence is LaunchNotStarted only with affirmative proof; a backend
	// without Capabilities.ProvesNeverStarted reports LaunchUnknown when missing
	// records cannot distinguish a never-issued create from a lost one.
	Stop(ctx context.Context, w Workload) (StopEvidence, error)
	// Remove releases what remains of a settled workload. It returns nil once
	// nothing remains (including when nothing ever existed) and ErrBusy while
	// the backend still retains it.
	Remove(ctx context.Context, w Workload) error
	// Capabilities identifies the backend and the guarantees it enforces.
	Capabilities() Capabilities
}

// AttemptSpec is the committed attempt a claim authorizes a runtime to start.
// Every field comes from durable attempt data, so redriving it after a crash
// carries the same identity and pinned context.
type AttemptSpec struct {
	Scope     domain.Scope
	RunID     domain.RunID
	StepID    string
	AttemptID domain.AttemptID
	Ordinal   int
	ActionKey string
	// WorkloadName and SpecFingerprint are the expected workload identity.
	WorkloadName    string
	SpecFingerprint string
	// Context is the canonical pinned runtime context and ContextDigest its
	// SHA-256.
	ContextDigest string
	Context       []byte
	WorkspaceID   string
	Access        Access
	Purpose       Purpose
	// Redrive is true when the spec was reissued after a lapsed claim.
	Redrive bool
	// Shared is set exactly for a run_shared attempt, immediately before each
	// Start (first launch and redrive), from the run's verified pins and the
	// workspace's committed identity. It is not part of the fingerprint: it is
	// derived from data the fingerprint and the run's pins already bind.
	Shared *SharedLaunch
}

// SharedLaunch is what a runtime needs to launch a run_shared attempt: the
// pinned step role shape (the only security shape it may apply), the run's
// workspace with its bound instance UID (the only volume it may mount at
// /workspace), the pinned network model and the step's egress profile names.
// It carries no secret and no provider vocabulary.
type SharedLaunch struct {
	Policy      PolicyPin         `json:"policy"`
	Role        RoleShape         `json:"role"`
	RoleDigest  string            `json:"roleDigest"`
	PodSecurity PolicyPodSecurity `json:"podSecurity"`
	Workspace   ResourceHandle    `json:"workspace"`
	Network     PolicyNetwork     `json:"network"`
	Egress      []string          `json:"egress"`
	// EgressDigests are the resolved destination digests of Egress.
	EgressDigests map[string]string `json:"egressDigests"`
}

// Validate checks that the launch names a bound workspace of the attempt's run
// and a pinned role.
func (l SharedLaunch) Validate(spec AttemptSpec) error {
	if err := l.Policy.Validate(); err != nil {
		return err
	}
	if !digestPattern.MatchString(l.RoleDigest) || l.Role.User == "" {
		return invalidf("the shared launch has no pinned step role")
	}
	if d, err := l.Role.Digest(); err != nil || d != l.RoleDigest {
		return invalidf("the shared launch role does not match its pinned digest")
	}
	if err := l.Workspace.Validate(); err != nil {
		return err
	}
	if l.Workspace.Kind != KindWorkspace || l.Workspace.UID == "" || l.Workspace.Scope != spec.Scope || l.Workspace.RunID != spec.RunID {
		return invalidf("the shared launch names no bound workspace of the attempt's run")
	}
	switch l.Network.Isolation {
	case IsolationTenant, IsolationRun:
	default:
		return invalidf("the shared launch has no network model")
	}
	return nil
}

// Workload returns the handle of the spec's workload with the given instance
// UID ("" while none is known).
func (s AttemptSpec) Workload(uid string) Workload {
	return Workload{Scope: s.Scope, AttemptID: s.AttemptID, Name: s.WorkloadName, UID: uid}
}

// Validate checks that the spec names a well-formed attempt whose workload
// name is its deterministic name and whose context matches its digest.
func (s AttemptSpec) Validate() error {
	if err := s.Workload("").Validate(); err != nil {
		return err
	}
	if err := s.RunID.Validate(); err != nil || s.StepID == "" {
		return invalidf("the attempt spec has no run or step")
	}
	if !digestPattern.MatchString(s.SpecFingerprint) {
		return invalidf("the attempt spec has no fingerprint")
	}
	sum := sha256.Sum256(s.Context)
	if s.ContextDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return invalidf("the attempt context does not match its digest")
	}
	return nil
}

// DecodeContext validates the spec and returns its pinned runtime context,
// which must belong to the same attempt.
func (s AttemptSpec) DecodeContext() (runtimev1.Context, error) {
	if err := s.Validate(); err != nil {
		return runtimev1.Context{}, err
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](s.Context)
	if err != nil {
		return runtimev1.Context{}, invalidf("the attempt context is unreadable")
	}
	if c.Scope() != s.Scope || c.RunID != s.RunID || c.StepID != s.StepID || c.AttemptID != s.AttemptID {
		return runtimev1.Context{}, invalidf("the attempt context belongs to another attempt")
	}
	return c, nil
}

// Workload identifies an attempt's workload to its runtime. Name is the
// attempt's deterministic workload name; UID is the backend instance the
// runtime reported, "" until it did.
type Workload struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
	Name      string
	UID       string
}

// Validate checks that the handle names an attempt's deterministic workload.
func (w Workload) Validate() error {
	if w.Scope.Validate() != nil || w.AttemptID.Validate() != nil {
		return invalidf("the workload names no attempt")
	}
	if w.Name != WorkloadName(w.Scope.TenantID, w.AttemptID) {
		return invalidf("the workload name is not the attempt's")
	}
	if w.UID != "" && !uidPattern.MatchString(w.UID) {
		return invalidf("the workload UID is malformed")
	}
	return nil
}

// StopEvidence is a runtime's statement after Stop.
type StopEvidence struct {
	// Stopped is true only when the workload verifiably no longer runs and no
	// pending launch can still start it.
	Stopped bool
	// Launch says whether the command could have started.
	Launch LaunchEvidence
}

// Capabilities describes a runtime.
type Capabilities struct {
	// Backend is the stable identity of the runtime. A run pins it at
	// admission and no other backend ever starts, stops or removes its
	// workloads.
	Backend string
	// Guarantees are enforced for every workload the backend runs.
	Guarantees Guarantees
	// ProvesNeverStarted declares that stopping a fresh workload (one the
	// runtime never started) affirmatively proves it never started: Stop then
	// reports LaunchNotStarted. A backend that cannot tell an absent record from
	// a create whose outcome was lost leaves it false and reports LaunchUnknown
	// without concrete evidence (owner decision O-25, ADR 0008). It is launch
	// history proof only: it is not a Guarantee, is never part of the admission
	// policy or a run's pinned binding, and does not relax the fencing every
	// backend owes (a stopped workload is never started, Stopped means no
	// launch can still arrive).
	ProvesNeverStarted bool
}

// Guarantees is a set of execution guarantees a runtime enforces or a step
// requires.
type Guarantees uint8

const (
	// OCIImage runs the step's digest-pinned image, never a host program.
	OCIImage Guarantees = 1 << iota
	// Isolation separates the workload's filesystem and processes from the
	// host and from other workloads.
	Isolation
	// ResourceLimits bounds the workload's CPU, memory and processes.
	ResourceLimits
	// NetworkPolicy enforces the tenant's network egress policy.
	NetworkPolicy
	// ResourceQuotas enforces the tenant's aggregate resource quota.
	ResourceQuotas
	// CrashRecovery verifies and adopts workloads after a controller restart.
	CrashRecovery
	// RunResourcesMarker marks a backend that also implements the companion
	// RunResources port. It is the one spare bit of the uint8 the run_shared
	// contract uses; the last bit stays reserved so the set cannot overflow.
	// Finer resource capabilities live in the separate ResourceCapabilitySet,
	// never here. A persisted binding that lists "run_resources" is refused by
	// an older decoder (unknown name), a deliberate protection beside the
	// explicit run-model check.
	RunResourcesMarker

	// AllGuarantees is every attempt-runtime guarantee of the original model. It
	// deliberately excludes RunResources: only a backend that implements the
	// companion port declares it (see WithRunResources).
	AllGuarantees = OCIImage | Isolation | ResourceLimits | NetworkPolicy | ResourceQuotas | CrashRecovery

	// reservedGuarantee is the last bit of the uint8, never assigned.
	reservedGuarantee Guarantees = 1 << 7
)

var guaranteeNames = [...]string{"oci_image", "isolation", "resource_limits", "network_policy", "resource_quotas", "crash_recovery", "run_resources"}

// WithRunResources adds the companion-port marker to g.
func (g Guarantees) WithRunResources() Guarantees { return g | RunResourcesMarker }

// Missing returns the guarantees of required that g lacks.
func (g Guarantees) Missing(required Guarantees) Guarantees { return required &^ g }

// Names lists the guarantees in a stable order.
func (g Guarantees) Names() []string {
	names := []string{}
	for i, name := range guaranteeNames {
		if g&(1<<i) != 0 {
			names = append(names, name)
		}
	}
	return names
}

func (g Guarantees) String() string {
	if g == 0 {
		return "none"
	}
	return strings.Join(g.Names(), ",")
}

// MarshalJSON encodes the set as a list of names.
func (g Guarantees) MarshalJSON() ([]byte, error) { return json.Marshal(g.Names()) }

// UnmarshalJSON decodes a list of names and refuses an unknown one.
func (g *Guarantees) UnmarshalJSON(b []byte) error {
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return err
	}
	var out Guarantees
next:
	for _, name := range names {
		for i, known := range guaranteeNames {
			if name == known {
				out |= 1 << i
				continue next
			}
		}
		return fmt.Errorf("unknown runtime guarantee %q", name)
	}
	*g = out
	return nil
}

// Requirements derives the guarantees a step needs from its definition. The
// installation chooses the policy; it is applied at admission and again
// before every launch.
type Requirements func(pipeline.Step) Guarantees

// DefaultRequirements requires the container semantics an executable step
// declares: its pinned image, run in isolation within resource limits.
func DefaultRequirements(step pipeline.Step) Guarantees {
	if !step.Executable() {
		return 0
	}
	return OCIImage | Isolation | ResourceLimits
}

// RuntimeBinding is the runtime a run was admitted for and what its steps
// require of it. It is pinned with the run's snapshot.
type RuntimeBinding struct {
	Backend  string     `json:"backend"`
	Requires Guarantees `json:"requires"`
	// ResourceRequires is the named resource-capability set a run_shared run
	// pinned at admission (beside Requires, which then lists run_resources). It is
	// omitted for every attempt_private run, so historical bindings and their
	// digests are unchanged.
	ResourceRequires ResourceCapabilitySet `json:"resourceRequires,omitempty"`
}

// Workload returns the handle of the attempt's workload.
func (a *Attempt) Workload() Workload {
	return Workload{Scope: a.Scope, AttemptID: a.ID, Name: a.WorkloadName, UID: a.WorkloadUID}
}

// spec returns the attempt's committed start specification.
func (a *Attempt) spec(purpose Purpose, redrive bool) AttemptSpec {
	return AttemptSpec{
		Scope: a.Scope, RunID: a.RunID, StepID: a.StepID, AttemptID: a.ID, Ordinal: a.Ordinal, ActionKey: a.ActionKey,
		WorkloadName: a.WorkloadName, SpecFingerprint: a.SpecFingerprint, ContextDigest: a.ContextDigest,
		Context: append([]byte(nil), a.Context...), WorkspaceID: a.WorkspaceID, Access: a.Access, Purpose: purpose, Redrive: redrive,
	}
}

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)
