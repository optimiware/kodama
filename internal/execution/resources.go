package execution

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// This file freezes the execution-owned run-resource companion seam of the
// run_shared model (docs/contracts/run-resources.md, ADR 0010): provider-neutral
// DTOs, handles, spec fingerprints, UID binding, evidence and the named
// capability set. Provider types (Kubernetes objects, Podman containers,
// processes) never appear here; adapters (P3-P5) translate.

// RunResources is the companion port of a backend that runs run-shared
// resources. It has six methods ([SD-15]) and carries no provider vocabulary.
// Every method is idempotent and acts only on a verified instance of the
// pinned backend: scope, name and (once bound) UID must match, and a foreign
// object at the same name is a collision, never adopted, stopped or removed.
// Execution decides desired state, retries and ordering; it calls this port
// only outside SQL transactions and only for committed intent.
type RunResources interface {
	// Capabilities identifies the backend and the named capability set it
	// enforces (and has tested) for every run.
	Capabilities(ctx context.Context) ResourceCapabilities
	// Ensure creates the resource if absent or adopts the one whose scope, name
	// and fingerprint (and UID, when bound) match; otherwise it reports
	// ErrResourceCollision. It refuses to start a stopped resource
	// (ErrResourceFenced). The observation carries the opaque provider UID.
	Ensure(ctx context.Context, spec ResourceSpec) (ResourceObservation, error)
	// Observe never creates. A missing object is PhaseAbsent; an
	// undeterminable one PhaseUnknown (never absent).
	Observe(ctx context.Context, h ResourceHandle) (ResourceObservation, error)
	// Logs returns a bounded, redacted tail; denied for workspace and network.
	Logs(ctx context.Context, h ResourceHandle, bound LogBound) (ResourceLogs, error)
	// Stop revokes admission, then terminates. Stopped is true only when no
	// process of the resource can still run and no pending create can arrive;
	// for a workspace Released is true only when no process/mount on any node
	// can still use the volume.
	Stop(ctx context.Context, h ResourceHandle) (ResourceStopEvidence, error)
	// Remove requires a prior verified stop, deletes the object (and a
	// workspace's storage allocation) and confirms absence of the verified UID.
	Remove(ctx context.Context, h ResourceHandle) (ResourceRemoveEvidence, error)
}

// Companion-port failures. An adapter may return any other error for an outage;
// execution then records the outcome as unknown and retries.
var (
	// ErrResourceCollision: a same-name object of another scope, fingerprint or
	// UID exists; it is reported, never adopted or removed.
	ErrResourceCollision = fmt.Errorf("%w: run resource collision", ErrConflict)
	// ErrResourceFenced: the resource exists stopped or removing and is never
	// started again.
	ErrResourceFenced = fmt.Errorf("%w: run resource is fenced", ErrConflict)
)

// ResourceKind is the kind of a run resource.
type ResourceKind string

const (
	KindNetwork     ResourceKind = "network"
	KindWorkspace   ResourceKind = "workspace"
	KindPreparation ResourceKind = "preparation"
	KindService     ResourceKind = "service"
)

// Resource keys. A service key is "service." + NAME.
const (
	KeyNetwork     = "network"
	KeyWorkspace   = "workspace"
	KeyPreparation = "preparation"
	servicePrefix  = "service."
)

// ServiceKey returns the resource key of a service.
func ServiceKey(name string) string { return servicePrefix + name }

// ServiceNameOfKey returns the service name of a service key.
func ServiceNameOfKey(key string) (string, bool) {
	if !strings.HasPrefix(key, servicePrefix) || len(key) == len(servicePrefix) {
		return "", false
	}
	return key[len(servicePrefix):], true
}

// HasIdentity reports whether the kind runs a workload with a runtime identity.
func (k ResourceKind) HasIdentity() bool { return k == KindPreparation || k == KindService }

var keyPattern = regexp.MustCompile(`^(network|workspace|preparation|service\.[a-z][a-z0-9-]{0,29})$`)

// ValidKey reports whether key is a well-formed resource key of the kind.
func (k ResourceKind) ValidKey(key string) bool {
	if !keyPattern.MatchString(key) {
		return false
	}
	switch k {
	case KindNetwork:
		return key == KeyNetwork
	case KindWorkspace:
		return key == KeyWorkspace
	case KindPreparation:
		return key == KeyPreparation
	case KindService:
		return strings.HasPrefix(key, servicePrefix)
	}
	return false
}

// framedDigest is the lowercase hex SHA-256 of the parts, each framed by its
// byte length as a 4-byte big-endian unsigned integer (the same framing the
// idempotency keys use), so no two tuples share an encoding. The run-resources
// contract says "length-prefixed" without fixing the width; T-112 freezes this
// framing and its golden vectors.
func framedDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ResourceName is the deterministic object name of a resource: "kr-" plus the
// first 24 lowercase hex characters of SHA-256 over the length-prefixed tuple
// (tenantId, projectId, runId, kind, key). A name never authorizes anything.
func ResourceName(scope domain.Scope, run domain.RunID, kind ResourceKind, key string) string {
	return "kr-" + framedDigest(string(scope.TenantID), string(scope.ProjectID), string(run), string(kind), key)[:24]
}

// EndpointHost is the run-unique endpoint host of a service: "s-" plus the
// first 16 hex characters of SHA-256 over (tenantId, projectId, runId), then
// "-NAME" (at most 49 characters).
func EndpointHost(scope domain.Scope, run domain.RunID, service string) string {
	return "s-" + framedDigest(string(scope.TenantID), string(scope.ProjectID), string(run))[:16] + "-" + service
}

// ResourceCapability is one named run-resource capability.
type ResourceCapability uint32

const (
	CapRunWorkspace ResourceCapability = 1 << iota
	CapWorkspaceFence
	CapWorkspacePreparation
	CapResourceRecovery
	CapRunServices
	CapServiceIsolation
	CapRunNetworkIsolation
	CapDeclaredEdges
	CapNamedEgress
	CapEgressDefaultDeny
	CapBuilderRootless
	CapBuilderService
	CapBuilderIsolatedWorker
)

var capabilityNames = [...]string{
	"run_workspace", "workspace_fence", "workspace_preparation", "resource_recovery", "run_services", "service_isolation",
	"run_network_isolation", "declared_edges", "named_egress", "egress_default_deny", "builder_rootless", "builder_service",
	"builder_isolated_worker",
}

// ResourceCapabilitySet is the named set of resource capabilities, a 32-bit set
// (capacity for the contract's at least 16 members; 13 are defined). It is
// separate from Guarantees, which carries only the run_resources marker.
type ResourceCapabilitySet uint32

// Has reports whether every capability in c is in the set.
func (s ResourceCapabilitySet) Has(c ResourceCapability) bool {
	return s&ResourceCapabilitySet(c) == ResourceCapabilitySet(c)
}

// Missing returns the members of required the set lacks.
func (s ResourceCapabilitySet) Missing(required ResourceCapabilitySet) ResourceCapabilitySet {
	return required &^ s
}

// Names lists the members in a stable order.
func (s ResourceCapabilitySet) Names() []string {
	names := []string{}
	for i, n := range capabilityNames {
		if s&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	return names
}

func (s ResourceCapabilitySet) String() string {
	if s == 0 {
		return "none"
	}
	return strings.Join(s.Names(), ",")
}

// MarshalJSON encodes the set as a list of names.
func (s ResourceCapabilitySet) MarshalJSON() ([]byte, error) { return json.Marshal(s.Names()) }

// UnmarshalJSON decodes a list of names and refuses an unknown one, so a
// persisted pin naming a capability this controller does not know is refused.
func (s *ResourceCapabilitySet) UnmarshalJSON(b []byte) error {
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return err
	}
	var out ResourceCapabilitySet
next:
	for _, name := range names {
		for i, known := range capabilityNames {
			if name == known {
				out |= 1 << i
				continue next
			}
		}
		return fmt.Errorf("unknown run resource capability %q", name)
	}
	*s = out
	return nil
}

// Set builds a set from capabilities.
func Set(caps ...ResourceCapability) ResourceCapabilitySet {
	var s ResourceCapabilitySet
	for _, c := range caps {
		s |= ResourceCapabilitySet(c)
	}
	return s
}

// ResourceCapabilities is what the companion port declares: the backend
// identity (which must equal the attempt runtime's) and the named set.
type ResourceCapabilities struct {
	Backend string                `json:"backend"`
	Set     ResourceCapabilitySet `json:"set"`
}

// NormalResourceRequirements are the capabilities every run_shared run needs,
// including one with no services, no edges and no egress. The network members
// follow the pinned policy's network model (O-30): run isolation adds
// run_network_isolation and a policy without public egress adds
// egress_default_deny. The tenant boundary itself is the attempt runtime's
// network_policy guarantee, which every run_shared run requires.
var NormalResourceRequirements = Set(CapRunWorkspace, CapWorkspaceFence, CapResourceRecovery)

// ResourcePhase is the observed phase of one resource. It is not a business
// run, step or attempt status.
type ResourcePhase string

const (
	PhasePending ResourcePhase = "pending"
	PhaseReady   ResourcePhase = "ready"
	PhaseFailed  ResourcePhase = "failed"
	PhaseStopped ResourcePhase = "stopped"
	PhaseAbsent  ResourcePhase = "absent"
	PhaseUnknown ResourcePhase = "unknown"
)

// ResourceReason is a safe, stable reason code of a resource or of the run
// failure it caused. None of them is a run, step or attempt status.
type ResourceReason string

const (
	ReasonPreparationFailed     ResourceReason = "preparation_failed"
	ReasonServiceNotReady       ResourceReason = "service_not_ready"
	ReasonServiceLost           ResourceReason = "service_lost"
	ReasonWorkspaceLost         ResourceReason = "workspace_lost"
	ReasonWorkspaceUnverified   ResourceReason = "workspace_unverified"
	ReasonRunDeadlineExceeded   ResourceReason = "run_deadline_exceeded"
	ReasonPolicyUnavailable     ResourceReason = "policy_unavailable"
	ReasonCapabilityUnsupported ResourceReason = "capability_unsupported"
	ReasonResourceCollision     ResourceReason = "resource_collision"
	ReasonStopUnconfirmed       ResourceReason = "resource_stop_unconfirmed"
	ReasonCleanupPending        ResourceReason = "cleanup_pending"
	ReasonResourceFenced        ResourceReason = "resource_fenced"
	reasonResourceUnknown       ResourceReason = "resource_unknown"
	reasonResourceUnobservable  ResourceReason = "resource_unobservable"
	reasonResourceUnverified    ResourceReason = "resource_unverified"
)

// Valid reports whether r is a well-formed reason code.
func (r ResourceReason) Valid() bool { return Reason(r).Valid() }

// ResourceHandle addresses one resource: {scope, runId, kind, key, name, uid}.
// UID is empty before the first successful Ensure.
type ResourceHandle struct {
	Scope domain.Scope
	RunID domain.RunID
	Kind  ResourceKind
	Key   string
	Name  string
	UID   string
}

// Validate checks that the handle names a run's deterministic resource.
func (h ResourceHandle) Validate() error {
	if h.Scope.Validate() != nil || h.RunID.Validate() != nil {
		return invalidf("the resource handle names no run")
	}
	if !h.Kind.ValidKey(h.Key) {
		return invalidf("the resource handle has a malformed kind or key")
	}
	if h.Name != ResourceName(h.Scope, h.RunID, h.Kind, h.Key) {
		return invalidf("the resource name is not the run's deterministic name")
	}
	if h.UID != "" && !resourceUIDPattern.MatchString(h.UID) {
		return invalidf("the resource UID is malformed")
	}
	return nil
}

// resourceUIDPattern admits the provider's opaque instance identity: neither
// the shape of a Kubernetes UID nor a Podman ID is assumed beyond a bounded,
// printable token.
var resourceUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ResourceObservation is what the backend reports about one resource. It
// carries no secrets, scratch contents or provider objects.
type ResourceObservation struct {
	Handle ResourceHandle
	// Fingerprint is the spec fingerprint the provider object was verified
	// against; the empty string means the backend could not verify it.
	Fingerprint string
	Phase       ResourcePhase
	ObservedAt  time.Time
	// ReadyAt is when the resource was verified usable (ready).
	ReadyAt *time.Time
	// ProbeKind names a service's passing probe (tcp, http or exec).
	ProbeKind string
	// Released is a workspace's verified "no process or mount on any node can
	// still use the volume" (the holder fence).
	Released bool
	// Reason and ExitCode describe failed, unknown or collision outcomes.
	Reason   ResourceReason
	ExitCode *int64
	// LogRef is a bounded log reference, never log content.
	LogRef string
}

// ResourceStopEvidence is the backend's statement after Stop. Launch follows
// the attempt runtime's rule: a missing record never upgrades unknown to
// not_started.
type ResourceStopEvidence struct {
	Stopped  bool
	Launch   LaunchEvidence
	Released bool
}

// ResourceRemoveEvidence is the backend's statement after Remove: Removed is
// true only once absence of the verified UID is confirmed.
type ResourceRemoveEvidence struct {
	Removed bool
}

// LogBound bounds a log request.
type LogBound struct {
	MaxBytes int64
}

// ResourceLogs is a bounded, redacted tail of a preparation or service.
type ResourceLogs struct {
	Content   string
	Truncated bool
}

// ---- specification ----

// PolicyPin is the identity of an execution policy: {id, digest}.
type PolicyPin struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// Validate checks the pin's syntax.
func (p PolicyPin) Validate() error {
	if !policyIDPattern.MatchString(p.ID) || !digestPattern.MatchString(p.Digest) {
		return invalidf("the policy pin is malformed")
	}
	return nil
}

var policyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// WorkspaceSpec is the desired scratch filesystem.
type WorkspaceSpec struct {
	SizeMiB int64 `json:"sizeMiB"`
	// Initial is "git" (prepared from the pinned commit) or "empty".
	Initial string `json:"initial"`
}

// PreparationSpec is the controller-selected one-shot checkout workload.
type PreparationSpec struct {
	Image         string `json:"image"`
	RepositoryURL string `json:"repositoryUrl"`
	Commit        string `json:"commit"`
	TimeoutSecs   int64  `json:"timeoutSeconds"`
	MaxBytes      int64  `json:"maxBytes"`
	MaxAttempts   int64  `json:"maxAttempts"`
	// Sources lists every checkout of a source-set run (runtime v3: one
	// preparation workload checks out the whole set, in path order). It is
	// omitted for a single-repository run, whose spec, fingerprint and stored
	// shape are unchanged; then RepositoryURL and Commit name the one checkout.
	// For a source-set run both are empty and Sources is the whole assignment.
	Sources []PreparationSourceSpec `json:"sources,omitempty"`
}

// PreparationSourceSpec is one checkout of a source-set preparation.
type PreparationSourceSpec struct {
	RepositoryID  string `json:"repositoryId"`
	RepositoryURL string `json:"repositoryUrl"`
	Commit        string `json:"commit"`
	Path          string `json:"path"`
}

// SecretPin is a service's pinned secret binding (no value).
type SecretPin struct {
	Ref      string                   `json:"ref"`
	Version  int64                    `json:"version"`
	Name     string                   `json:"name"`
	Delivery runtimev1.SecretDelivery `json:"delivery"`
}

// ServiceResourcesSpec bounds a service workload.
type ServiceResourcesSpec struct {
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryMiB    int64 `json:"memoryMiB"`
	EphemeralMiB int64 `json:"ephemeralMiB"`
	ShmMiB       int64 `json:"shmMiB"`
}

// StorageSpec is a service's run-owned ephemeral volume.
type StorageSpec struct {
	Path    string `json:"path"`
	SizeMiB int64  `json:"sizeMiB"`
}

// ReadinessSpec is a service's readiness contract.
type ReadinessSpec struct {
	Probe        string   `json:"probe"` // tcp, http or exec
	Port         string   `json:"port"`  // tcp/http port name
	Path         string   `json:"path"`
	Exec         []string `json:"exec"`
	TimeoutSecs  int64    `json:"timeoutSeconds"`
	PeriodSecs   int64    `json:"periodSeconds"`
	ProbeTimeout int64    `json:"probeTimeoutSeconds"`
	LossSecs     int64    `json:"lossSeconds"`
}

// ServiceSpec is one support service. Env holds only nonsecret resolved
// values; secret bindings are pins.
type ServiceSpec struct {
	Name      string               `json:"name"`
	Role      string               `json:"role"`
	Image     string               `json:"image"`
	Command   []string             `json:"command"`
	Args      []string             `json:"args"`
	Env       map[string]string    `json:"env"`
	Secrets   []SecretPin          `json:"secrets"`
	Ports     map[string]int64     `json:"ports"`
	Resources ServiceResourcesSpec `json:"resources"`
	Storage   []StorageSpec        `json:"storage"`
	Readiness ReadinessSpec        `json:"readiness"`
	Egress    []string             `json:"egress"`
	Host      string               `json:"host"`
}

// EdgeSpec is a declared service-to-service edge.
type EdgeSpec struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Ports []string `json:"ports"`
}

// NetworkSpec is the run's access boundary: its services, declared edges and
// the resolved egress destination digest of every named profile.
type NetworkSpec struct {
	Services []string   `json:"services"`
	Edges    []EdgeSpec `json:"edges"`
	// Egress maps the profile name to the digest of its resolved destinations.
	Egress map[string]string `json:"egress"`
	// Workloads maps a workload (a step id, "preparation" or "service.NAME") to
	// the profile names it may use.
	Workloads map[string][]string `json:"workloads"`
	// Isolation and PublicEgress are the pinned policy's network model (O-30):
	// "tenant" or "run" reachability and whether public HTTP/HTTPS egress is
	// open beside the named profiles.
	Isolation    string `json:"isolation"`
	PublicEgress bool   `json:"publicEgress"`
}

// ResourceSpec is the canonical, provider-neutral desired state of one
// resource. It contains no secret values, tokens, scratch contents, provider
// objects or raw Pod fields. The fingerprint excludes the provider UID.
type ResourceSpec struct {
	Scope      domain.Scope `json:"scope"`
	RunID      domain.RunID `json:"runId"`
	Backend    string       `json:"backend"`
	Kind       ResourceKind `json:"kind"`
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Policy     PolicyPin    `json:"policy"`
	RoleShape  string       `json:"roleShape"`
	DeadlineAt string       `json:"deadlineAt"`

	Workspace   *WorkspaceSpec   `json:"workspace"`
	Preparation *PreparationSpec `json:"preparation"`
	Service     *ServiceSpec     `json:"service"`
	Network     *NetworkSpec     `json:"network"`
}

// Handle returns the spec's handle with the given bound UID.
func (s ResourceSpec) Handle(uid string) ResourceHandle {
	return ResourceHandle{Scope: s.Scope, RunID: s.RunID, Kind: s.Kind, Key: s.Key, Name: s.Name, UID: uid}
}

// Validate checks the spec's identity and that exactly the kind's body is set.
func (s ResourceSpec) Validate() error {
	if err := s.Handle("").Validate(); err != nil {
		return err
	}
	if s.Backend == "" || s.Policy.Validate() != nil || !digestPattern.MatchString(s.RoleShape) {
		return invalidf("the resource spec has no backend, policy pin or role shape")
	}
	if _, err := time.Parse(time.RFC3339, s.DeadlineAt); err != nil {
		return invalidf("the resource spec has no deadline")
	}
	bodies := 0
	for _, set := range []bool{s.Workspace != nil, s.Preparation != nil, s.Service != nil, s.Network != nil} {
		if set {
			bodies++
		}
	}
	ok := bodies == 1
	switch s.Kind {
	case KindWorkspace:
		ok = ok && s.Workspace != nil && s.Workspace.SizeMiB > 0 && (s.Workspace.Initial == "git" || s.Workspace.Initial == "empty")
	case KindPreparation:
		ok = ok && s.Preparation != nil && digestInImage(s.Preparation.Image) && s.Preparation.validCheckouts()
	case KindService:
		ok = ok && s.Service != nil && s.Key == ServiceKey(s.Service.Name) && digestInImage(s.Service.Image)
	case KindNetwork:
		ok = ok && s.Network != nil && (s.Network.Isolation == IsolationTenant || s.Network.Isolation == IsolationRun)
	default:
		ok = false
	}
	if !ok {
		return invalidf("the resource spec body does not match its kind")
	}
	return nil
}

// validCheckouts: a single-repository preparation names one commit; a
// source-set preparation names none of its own and a well formed source each.
func (p *PreparationSpec) validCheckouts() bool {
	if len(p.Sources) == 0 {
		return commitPattern.MatchString(p.Commit)
	}
	if p.Commit != "" || p.RepositoryURL != "" || len(p.Sources) > gitv1.MaxSources {
		return false
	}
	for _, s := range p.Sources {
		if !commitPattern.MatchString(s.Commit) || s.RepositoryID == "" || s.RepositoryURL == "" || gitv1.ValidateSourcePath(s.Path) != nil {
			return false
		}
	}
	return true
}

var imageDigestPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func digestInImage(image string) bool { return imageDigestPattern.MatchString(image) }

// Canonical returns the spec's canonical JSON (the same canonicalization as
// definitions: sorted keys, no insignificant whitespace, integers only).
func (s ResourceSpec) Canonical() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return runtimev1.Canonicalize(raw)
}

// Fingerprint is "sha256:" + SHA-256 of the canonical spec; the UID is not part
// of it.
func (s ResourceSpec) Fingerprint() (string, error) {
	b, err := s.Canonical()
	if err != nil {
		return "", invalidf("the resource spec is not canonicalizable")
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CheckUIDBinding is the NEGATIVE identity check: a different instance or a
// different non-empty fingerprint is a collision. It is not positive evidence:
// an empty fingerprint means "could not verify" and never matches (see
// RunResource.observe, which treats such an observation as unknown and binds
// nothing). Callers needing affirmative identity use ObservationVerifies.
// CheckUIDBinding verifies that an observation or evidence concerns the bound
// instance: a different UID or fingerprint is a collision that is never
// adopted, modified or removed.
func CheckUIDBinding(boundUID, expectedFingerprint string, o ResourceObservation) error {
	if boundUID != "" && o.Handle.UID != "" && o.Handle.UID != boundUID {
		return ErrResourceCollision
	}
	if o.Fingerprint != "" && expectedFingerprint != "" && o.Fingerprint != expectedFingerprint {
		return ErrResourceCollision
	}
	return nil
}

// ---- preparation receipt ----

// PreparationReceiptObject is the exact object whose canonical JSON the
// receipt digest covers: the entrypoint's request plus the run, the resource key
// and the bound volume UID the server observed (runtime v2, resource subjects).
// A marker file in the volume is never evidence: tenant code can write it.
type PreparationReceiptObject struct {
	Request      runtimev2.PreparationReceiptRequest `json:"request"`
	RunID        domain.RunID                        `json:"runId"`
	ResourceKey  string                              `json:"resourceKey"`
	WorkspaceUID string                              `json:"workspaceUid"`
}

// Digest is "sha256:" + SHA-256 of the canonical JSON of the object.
func (o PreparationReceiptObject) Digest() (string, error) {
	return receiptDigest(o, o.WorkspaceUID, o.RunID, o.ResourceKey)
}

// PreparationReceiptObjectV3 is the same frozen object for the receipt of a
// source-set preparation (runtime v3): the request carries one receipt per
// source.
type PreparationReceiptObjectV3 struct {
	Request      runtimev3.PreparationReceiptRequestV3 `json:"request"`
	RunID        domain.RunID                          `json:"runId"`
	ResourceKey  string                                `json:"resourceKey"`
	WorkspaceUID string                                `json:"workspaceUid"`
}

// Digest is "sha256:" + SHA-256 of the canonical JSON of the object.
func (o PreparationReceiptObjectV3) Digest() (string, error) {
	return receiptDigest(o, o.WorkspaceUID, o.RunID, o.ResourceKey)
}

func receiptDigest(o any, workspaceUID string, run domain.RunID, key string) (string, error) {
	if workspaceUID == "" || run.Validate() != nil || key != KeyPreparation {
		return "", invalidf("the receipt is not bound to a run, the preparation resource and a workspace UID")
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", invalidf("the receipt is not canonicalizable")
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ObservationVerifies reports affirmative identity evidence: the observation
// carries the expected, non-empty spec fingerprint.
func ObservationVerifies(expectedFingerprint string, o ResourceObservation) bool {
	return expectedFingerprint != "" && o.Fingerprint == expectedFingerprint
}
