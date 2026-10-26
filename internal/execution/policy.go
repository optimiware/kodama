package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// ExecutionPolicy is the installation-owned, versioned, operator-installed
// execution policy of the run_shared model (docs/contracts/run-resources.md).
// It never appears in a pipeline document. Its identity is {ID, digest} where the
// digest is the SHA-256 of its canonical JSON. A run pins {ID, digest} and the
// body at admission; a later policy never mutates a pin.
type ExecutionPolicy struct {
	ID        string          `json:"id"`
	Admission PolicyAdmission `json:"admission"`
	Limits    PolicyLimits    `json:"limits"`
	// Requires adds resource capabilities every run_shared run needs.
	Requires  ResourceCapabilitySet `json:"requires"`
	Workspace PolicyWorkspace       `json:"workspace"`
	// Roles are the per-workload security shapes, the verified source of truth
	// for adapters. The keys are step, service, builderService and preparation.
	Roles          map[string]RoleShape     `json:"roles"`
	PodSecurity    PolicyPodSecurity        `json:"podSecurity"`
	Builders       PolicyBuilders           `json:"builders"`
	EgressProfiles map[string]EgressProfile `json:"egressProfiles"`
}

// PolicyAdmission selects which formats new admissions accept.
type PolicyAdmission struct {
	// LegacyFormat is open, drain or closed (v1alpha1 admission).
	LegacyFormat string `json:"legacyFormat"`
	// RunShared is closed until an installation enables it explicitly (T-116).
	RunShared string `json:"runShared"`
}

// PolicyLimits are the ceilings and defaults of the policy.
type PolicyLimits struct {
	MaxRunSeconds             int64 `json:"maxRunSeconds"`
	MaxServices               int64 `json:"maxServices"`
	MaxRunWorkspaceMiB        int64 `json:"maxRunWorkspaceMiB"`
	DefaultRunWorkspaceMiB    int64 `json:"defaultRunWorkspaceMiB"`
	MaxServiceCPUMillis       int64 `json:"maxServiceCpuMillis"`
	MaxServiceMemoryMiB       int64 `json:"maxServiceMemoryMiB"`
	MaxServiceEphemeralMiB    int64 `json:"maxServiceEphemeralMiB"`
	MaxServiceStorageMiB      int64 `json:"maxServiceStorageMiB"`
	PreparationTimeoutSeconds int64 `json:"preparationTimeoutSeconds"`
	PreparationAttempts       int64 `json:"preparationAttempts"`
	PreparationMaxBytes       int64 `json:"preparationMaxBytes"`
	StopGraceSeconds          int64 `json:"stopGraceSeconds"`
	// Capacity allowances charged at admission beyond services and scratch: the
	// preparation workload and the largest step attempt the policy permits.
	PreparationCPUMillis int64 `json:"preparationCpuMillis"`
	PreparationMemoryMiB int64 `json:"preparationMemoryMiB"`
	StepCPUMillis        int64 `json:"stepCpuMillis"`
	StepMemoryMiB        int64 `json:"stepMemoryMiB"`
	StepEphemeralMiB     int64 `json:"stepEphemeralMiB"`
}

// PolicyWorkspace is the scratch storage shape.
type PolicyWorkspace struct {
	Access       string `json:"access"`
	Reclaim      string `json:"reclaim"`
	Idmapped     bool   `json:"idmapped"`
	StorageClass string `json:"storageClass"`
}

// RoleShape is the security shape of one workload role. A role never carries
// privileged, host namespaces, hostPath, an engine socket or a token mount:
// those have no field, and CapabilitiesAdd/Devices are validated below.
type RoleShape struct {
	User            string   `json:"user"`
	UserNamespace   string   `json:"userNamespace"`
	CapabilitiesAdd []string `json:"capabilitiesAdd"`
	Seccomp         string   `json:"seccomp"`
	LSM             string   `json:"lsm"`
	Devices         []string `json:"devices"`
	Image           string   `json:"image"`
}

// PolicyPodSecurity pins the Pod Security level.
type PolicyPodSecurity struct {
	Level   string `json:"level"`
	Version string `json:"version"`
}

// PolicyBuilders selects the builder modes the installation offers.
type PolicyBuilders struct {
	Rootless []string       `json:"rootless"`
	Service  PolicyBuilderS `json:"service"`
}

// PolicyBuilderS is the pinned builder-service mode.
type PolicyBuilderS struct {
	// Mode is none, rootless, no_process_sandbox or privileged_isolated_worker.
	Mode string `json:"mode"`
}

// EgressProfile is a named destination set authorized by the installation.
type EgressProfile struct {
	Destinations []EgressDestination `json:"destinations"`
}

// EgressDestination is one host/CIDR and its ports.
type EgressDestination struct {
	Host  string  `json:"host"`
	Ports []int64 `json:"ports"`
}

// Policy roles.
const (
	RoleStep           = "step"
	RoleService        = "service"
	RoleBuilderService = "builderService"
	RolePreparation    = "preparation"
)

// Policy admission values.
const (
	PolicyOpen   = "open"
	PolicyDrain  = "drain"
	PolicyClosed = "closed"
)

// Canonical returns the policy's canonical JSON.
func (p ExecutionPolicy) Canonical() ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return runtimev1.Canonicalize(raw)
}

// Digest is the SHA-256 of the canonical JSON of the policy.
func (p ExecutionPolicy) Digest() (string, error) {
	b, err := p.Canonical()
	if err != nil {
		return "", invalidf("the execution policy is not canonicalizable")
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Pin returns {id, digest}.
func (p ExecutionPolicy) Pin() (PolicyPin, error) {
	d, err := p.Digest()
	if err != nil {
		return PolicyPin{}, err
	}
	return PolicyPin{ID: p.ID, Digest: d}, nil
}

// RoleDigest is the digest of one role's shape; a resource spec carries it so
// adapters verify the observed object against the pinned role, not the current
// installation default.
func (p ExecutionPolicy) RoleDigest(role string) (string, error) {
	shape, ok := p.Roles[role]
	if !ok {
		return "", invalidf("the policy has no %s role", role)
	}
	raw, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	c, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", invalidf("the role shape is not canonicalizable")
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EgressDigest is the digest of one named profile's resolved destinations.
func (p ExecutionPolicy) EgressDigest(name string) (string, bool) {
	prof, ok := p.EgressProfiles[name]
	if !ok {
		return "", false
	}
	dests := append([]EgressDestination(nil), prof.Destinations...)
	sort.Slice(dests, func(i, j int) bool { return dests[i].Host < dests[j].Host })
	raw, err := json.Marshal(dests)
	if err != nil {
		return "", false
	}
	c, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

// RunSharedOpen reports whether the policy admits new run_shared runs.
func (p ExecutionPolicy) RunSharedOpen() bool { return p.Admission.RunShared == PolicyOpen }

// Validate checks the policy's contract invariants, independently of the
// qualified values (T-111): positive ceilings, the four roles, capabilitiesAdd
// limited to the user-namespace mapping helpers and devices to /dev/fuse.
func (p ExecutionPolicy) Validate() error {
	if !policyIDPattern.MatchString(p.ID) {
		return invalidf("the policy id is malformed")
	}
	switch p.Admission.LegacyFormat {
	case PolicyOpen, PolicyDrain, PolicyClosed:
	default:
		return invalidf("the policy's legacy format admission is unknown")
	}
	switch p.Admission.RunShared {
	case PolicyOpen, PolicyClosed:
	default:
		return invalidf("the policy's run_shared admission is unknown")
	}
	l := p.Limits
	for _, v := range []int64{l.MaxRunSeconds, l.MaxServices, l.MaxRunWorkspaceMiB, l.DefaultRunWorkspaceMiB, l.MaxServiceCPUMillis,
		l.MaxServiceMemoryMiB, l.MaxServiceEphemeralMiB, l.MaxServiceStorageMiB, l.PreparationTimeoutSeconds, l.PreparationAttempts,
		l.PreparationMaxBytes, l.StopGraceSeconds, l.StepCPUMillis, l.StepMemoryMiB, l.StepEphemeralMiB} {
		if v < 1 {
			return invalidf("the policy limits must be positive")
		}
	}
	if l.DefaultRunWorkspaceMiB > l.MaxRunWorkspaceMiB || l.MaxServices > 8 || l.MaxRunSeconds > 604800 {
		return invalidf("the policy limits exceed the contract bounds")
	}
	for _, role := range []string{RoleStep, RoleService, RoleBuilderService, RolePreparation} {
		shape, ok := p.Roles[role]
		if !ok {
			return invalidf("the policy has no %s role", role)
		}
		for _, c := range shape.CapabilitiesAdd {
			if c != "SETUID" && c != "SETGID" {
				return invalidf("a role may add only the user-namespace mapping capabilities")
			}
		}
		for _, d := range shape.Devices {
			if d != "/dev/fuse" {
				return invalidf("a role may list only /dev/fuse as a device")
			}
		}
		if shape.User == "" || shape.UserNamespace == "" {
			return invalidf("a role names its user and user namespace")
		}
	}
	if !digestInImage(p.Roles[RolePreparation].Image) {
		return invalidf("the preparation image must be pinned by digest")
	}
	if p.Workspace.Access != "ReadWriteOncePod" || p.Workspace.Reclaim != "Delete" {
		// ReadWriteOnce or Pod deletion is never accepted as the workspace fence.
		return invalidf("the workspace must be ReadWriteOncePod with delete reclaim")
	}
	return nil
}

// Policies is the installation's policy source. Admission asks for the current
// policy; the pinned body of a run is retained by execution with the run, so
// dispatch and adoption verify against the pin and never against Current.
type Policies interface {
	Current(ctx context.Context) (ExecutionPolicy, error)
}
