package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// RunModelShared and SharedContextVersion are the discriminators a run_shared
// run pins (runtime v2). A controller that does not implement them must never
// start, adopt, stop or remove such a run.
const (
	RunModelShared       = "run_shared"
	SharedContextVersion = 2
)

// CapacityUnits are the quantities a run_shared run reserves atomically at
// start: every service at startup/steady state, preparation until done, the
// largest step attempt the policy permits and the scratch volume (charged
// separately from container ephemeral storage).
type CapacityUnits struct {
	Services     int64 `json:"services"`
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryMiB    int64 `json:"memoryMiB"`
	EphemeralMiB int64 `json:"ephemeralMiB"`
	StorageMiB   int64 `json:"storageMiB"`
}

// Add returns the sum of u and o.
func (u CapacityUnits) Add(o CapacityUnits) CapacityUnits {
	return CapacityUnits{u.Services + o.Services, u.CPUMillis + o.CPUMillis, u.MemoryMiB + o.MemoryMiB, u.EphemeralMiB + o.EphemeralMiB, u.StorageMiB + o.StorageMiB}
}

// Fits reports whether held plus u stays within limit on every quantity.
func (u CapacityUnits) Fits(held, limit CapacityUnits) bool {
	t := held.Add(u)
	return t.Services <= limit.Services && t.CPUMillis <= limit.CPUMillis && t.MemoryMiB <= limit.MemoryMiB &&
		t.EphemeralMiB <= limit.EphemeralMiB && t.StorageMiB <= limit.StorageMiB
}

// IsZero reports whether no capacity is stated.
func (u CapacityUnits) IsZero() bool { return u == CapacityUnits{} }

// EndpointPin is the pinned, typed endpoint of a service.
type EndpointPin struct {
	Host  string           `json:"host"`
	Ports map[string]int64 `json:"ports"`
}

// RunShared is the immutable pin set of a run_shared run, recorded at
// admission and never rewritten: the run model and context version, the pinned
// policy ({id, digest} and the retained body), the runtime binding facts, the
// resolved limits, endpoints, effective access, execution order and resolved
// service specs. It holds no secret value and no scratch content.
type RunShared struct {
	RunModel       string `json:"runModel"`
	ContextVersion int64  `json:"contextVersion"`

	Policy     PolicyPin       `json:"policy"`
	PolicyBody ExecutionPolicy `json:"policyBody"`

	// Backend is the attempt runtime's identity; the companion port must report
	// the same one. Requires includes the run_resources marker.
	Backend          string                `json:"backend"`
	Requires         Guarantees            `json:"requires"`
	ResourceRequires ResourceCapabilitySet `json:"resourceRequires"`

	SourceBacked  bool   `json:"sourceBacked"`
	RepositoryURL string `json:"repositoryUrl"`

	RunSeconds   int64 `json:"runSeconds"`
	WorkspaceMiB int64 `json:"workspaceMiB"`

	// Order is the executable order, gates included; Access the effective
	// workspace access of every executable step (never recomputed afterwards);
	// Egress the resolved profile names of every executable step.
	Order  []string            `json:"order"`
	Access map[string]Access   `json:"access"`
	Egress map[string][]string `json:"egress"`

	EgressDigests map[string]string      `json:"egressDigests"`
	Endpoints     map[string]EndpointPin `json:"endpoints"`
	Services      map[string]ServiceSpec `json:"services"`
	Edges         []EdgeSpec             `json:"edges"`
	Capacity      CapacityUnits          `json:"capacity"`
}

// Digest is a stable fingerprint of the pins, folded into the run's snapshot
// digest so the replayed admission carries them.
func (r RunShared) Digest() (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	c, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", invalidf("the run_shared pins are not canonicalizable")
	}
	sum := sha256.Sum256(append([]byte("kodama/run-shared/v1|"), c...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Supported reports that this controller implements the pinned run model and
// context version. A controller that does not must never start, adopt, stop or
// remove the run.
func (r RunShared) Supported() error {
	if r.RunModel != RunModelShared || r.ContextVersion != SharedContextVersion {
		return invalidf("the run model is not supported by this controller")
	}
	return nil
}

// Verify checks the pins' internal consistency, in particular that the
// retained policy body still matches the pinned digest: a corrupt or swapped
// body is policy_unavailable, never a silent fallback to the current policy.
func (r RunShared) Verify() error {
	if err := r.Supported(); err != nil {
		return err
	}
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	d, err := r.PolicyBody.Digest()
	if err != nil || d != r.Policy.Digest || r.PolicyBody.ID != r.Policy.ID {
		return fmt.Errorf("%w: %s", ErrNotConfigured, ReasonPolicyUnavailable)
	}
	if r.Backend == "" || r.Requires&RunResourcesMarker == 0 {
		return invalidf("the run_shared pins have no backend binding")
	}
	return nil
}

// ---- requirements ----

// RequiredResourceCapabilities derives the resource capabilities a run needs
// from its definition and policy. Every run_shared run, even one with no
// services, no edges and no egress, needs the normal set plus the network
// members its pinned policy's network model selects (O-30); another backend
// never substitutes for a missing member.
func RequiredResourceCapabilities(doc pipeline.Document, sourceBacked bool, policy ExecutionPolicy) ResourceCapabilitySet {
	req := NormalResourceRequirements | policy.Requires
	if policy.Network.Isolation != IsolationTenant {
		req |= ResourceCapabilitySet(CapRunNetworkIsolation)
	}
	if !policy.Network.PublicEgress {
		req |= ResourceCapabilitySet(CapEgressDefaultDeny)
	}
	if sourceBacked {
		req |= ResourceCapabilitySet(CapWorkspacePreparation)
	}
	if len(doc.Spec.Services) > 0 {
		req |= ResourceCapabilitySet(CapRunServices | CapServiceIsolation)
	}
	// Under the tenant model every workload of the tenant may reach the run's
	// services, so edges are a document rule (endpoint expressions), not an
	// enforced boundary; only the run model enforces them.
	if doc.Spec.Network != nil && len(doc.Spec.Network.Edges) > 0 && policy.Network.Isolation != IsolationTenant {
		req |= ResourceCapabilitySet(CapDeclaredEdges)
	}
	named := false
	for _, st := range doc.Spec.Steps {
		if len(st.Egress) > 0 {
			named = true
		}
	}
	for _, sv := range doc.Spec.Services {
		if len(sv.Egress) > 0 {
			named = true
		}
		if sv.Role == pipeline.RoleBuilder {
			req |= ResourceCapabilitySet(CapBuilderService)
			if policy.Builders.Service.Mode == "privileged_isolated_worker" {
				req |= ResourceCapabilitySet(CapBuilderIsolatedWorker)
			}
		}
	}
	if named {
		req |= ResourceCapabilitySet(CapNamedEgress)
	}
	return req
}

// RunGuarantees is the run-level counterpart of step Requirements: the union of
// the steps' guarantees plus the companion-port marker and the tenant network
// boundary (network_policy), which no run_shared run may be admitted without.
func RunGuarantees(doc pipeline.Document, requirements Requirements) Guarantees {
	g := RunResourcesMarker | NetworkPolicy
	for _, st := range doc.Spec.Steps {
		g |= requirements(st)
	}
	return g
}

// ---- pure planning ----

var endpointExprPattern = regexp.MustCompile(`^\$\{\{[ \t]*services\.([a-z][a-z0-9-]{0,29})\.(host|ports\.([a-z][a-z0-9-]{0,14}))[ \t]*\}\}$`)

// resolveEndpointRef resolves a whole-scalar endpoint expression against the
// pinned endpoints. ok is false when raw is not an endpoint expression.
func resolveEndpointRef(raw string, endpoints map[string]EndpointPin) (value string, ok bool, err error) {
	m := endpointExprPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", false, nil
	}
	ep, found := endpoints[m[1]]
	if !found {
		return "", true, fmt.Errorf("%w: unknown service", ErrInvalid)
	}
	if m[3] == "" {
		return ep.Host, true, nil
	}
	p, found := ep.Ports[m[3]]
	if !found {
		return "", true, fmt.Errorf("%w: unknown port", ErrInvalid)
	}
	return strconv.FormatInt(p, 10), true, nil
}

// SourceSplit is whether a run has a repository: work runs are source-backed.
type sharedInput struct {
	Scope         domain.Scope
	RunID         domain.RunID
	Doc           pipeline.Document
	Policy        ExecutionPolicy
	PolicyPin     PolicyPin
	Backend       string
	Requires      Guarantees
	ResourceReq   ResourceCapabilitySet
	SourceBacked  bool
	RepositoryURL string
	Snapshot      Snapshot
}

// planSharedPins derives and validates every pin of a run_shared run. Any
// ceiling violation is refused (limit_exceeds_policy), never clamped.
func planSharedPins(in sharedInput) (*RunShared, error) {
	doc, pol := in.Doc, in.Policy
	if err := pol.Validate(); err != nil {
		return nil, Refusedf("%s", ReasonPolicyUnavailable)
	}
	if in.SourceBacked {
		// The checkout identity is persisted in the preparation spec: it must be a
		// plain HTTPS repository URL that carries no credential.
		u, err := url.Parse(in.RepositoryURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, Refusedf("the repository URL is not admissible for a run_shared checkout")
		}
	}
	if doc.Spec.TimeoutSeconds < 60 || doc.Spec.TimeoutSeconds > pol.Limits.MaxRunSeconds {
		return nil, Refusedf("limit_exceeds_policy: the run deadline exceeds the pinned policy")
	}
	if int64(len(doc.Spec.Services)) > pol.Limits.MaxServices {
		return nil, Refusedf("limit_exceeds_policy: too many services")
	}
	ws := pol.Limits.DefaultRunWorkspaceMiB
	if doc.Spec.RunWorkspace != nil {
		ws = doc.Spec.RunWorkspace.SizeMiB
	}
	if ws < 1 || ws > pol.Limits.MaxRunWorkspaceMiB {
		return nil, Refusedf("limit_exceeds_policy: the scratch size exceeds the pinned policy")
	}
	order, err := doc.ExecutionOrder()
	if err != nil {
		return nil, Refusedf("the definition has no executable order")
	}
	shared := &RunShared{
		RunModel: RunModelShared, ContextVersion: SharedContextVersion, Policy: in.PolicyPin, PolicyBody: pol,
		Backend: in.Backend, Requires: in.Requires | RunResourcesMarker, ResourceRequires: in.ResourceReq,
		SourceBacked: in.SourceBacked, RepositoryURL: in.RepositoryURL, RunSeconds: doc.Spec.TimeoutSeconds, WorkspaceMiB: ws,
		Order: order, Access: map[string]Access{}, Egress: map[string][]string{}, EgressDigests: map[string]string{},
		Endpoints: map[string]EndpointPin{}, Services: map[string]ServiceSpec{}, Edges: []EdgeSpec{},
	}
	useEgress := func(names []string) ([]string, error) {
		out := []string{}
		for _, n := range names {
			d, ok := pol.EgressDigest(n)
			if !ok {
				return nil, Refusedf("egress_profile_unknown: a named egress profile is not defined")
			}
			shared.EgressDigests[n] = d
			out = append(out, n)
		}
		return out, nil
	}
	for _, st := range doc.Spec.Steps {
		if !st.Executable() {
			continue
		}
		access := AccessRead
		if in.SourceBacked {
			if st.Workspace != nil && st.Workspace.Access == pipeline.AccessNone {
				return nil, Refusedf("workspace_none_source_backed: a source-backed step has read or write access")
			}
			if st.Workspace != nil && st.Workspace.Access != "" {
				access = Access(st.Workspace.Access)
			}
		} else {
			access = AccessNone
			if st.Workspace != nil && st.Workspace.Access != "" && st.Workspace.Access != pipeline.AccessNone {
				return nil, Refusedf("workspace_access_source_free: a source-free run has no repository")
			}
			if st.Kind == pipeline.KindAgent {
				return nil, Refusedf("agent_source_free: an agent step needs a repository")
			}
		}
		shared.Access[st.ID] = access
		names, err := useEgress(st.Egress)
		if err != nil {
			return nil, err
		}
		shared.Egress[st.ID] = names
	}
	names := make([]string, 0, len(doc.Spec.Services))
	for n := range doc.Spec.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sv := doc.Spec.Services[n]
		ports := map[string]int64{}
		for pn, p := range sv.Ports {
			ports[pn] = p.Port
		}
		shared.Endpoints[n] = EndpointPin{Host: EndpointHost(in.Scope, in.RunID, n), Ports: ports}
	}
	var storage int64
	for _, n := range names {
		sv := doc.Spec.Services[n]
		r := sv.Resources
		if r.CPUMillis > pol.Limits.MaxServiceCPUMillis || r.MemoryMiB > pol.Limits.MaxServiceMemoryMiB || r.EphemeralMiB > pol.Limits.MaxServiceEphemeralMiB {
			return nil, Refusedf("limit_exceeds_policy: a service's resources exceed the pinned policy")
		}
		var svcStorage int64
		for _, s := range sv.Storage {
			svcStorage += s.SizeMiB
		}
		if svcStorage > pol.Limits.MaxServiceStorageMiB {
			return nil, Refusedf("limit_exceeds_policy: a service's storage exceeds the pinned policy")
		}
		spec, err := resolveServiceSpec(in, shared, n, sv)
		if err != nil {
			return nil, err
		}
		eg, err := useEgress(sv.Egress)
		if err != nil {
			return nil, err
		}
		spec.Egress = eg
		shared.Services[n] = spec
		shim := r.ShmMiB
		var shm int64 = pipeline.DefaultShmMiB
		if shim != nil {
			shm = *shim
		}
		shared.Capacity.Services++
		shared.Capacity.CPUMillis += r.CPUMillis
		shared.Capacity.MemoryMiB += r.MemoryMiB + shm
		shared.Capacity.EphemeralMiB += r.EphemeralMiB
		storage += svcStorage
	}
	if doc.Spec.Network != nil {
		for _, e := range doc.Spec.Network.Edges {
			shared.Edges = append(shared.Edges, EdgeSpec{From: e.From, To: e.To, Ports: append([]string{}, e.Ports...)})
		}
	}
	// Preparation (source-backed), the largest permitted step attempt and the
	// scratch volume, charged separately from container ephemeral storage.
	if in.SourceBacked {
		shared.Capacity.CPUMillis += pol.Limits.PreparationCPUMillis
		shared.Capacity.MemoryMiB += pol.Limits.PreparationMemoryMiB
	}
	shared.Capacity.CPUMillis += pol.Limits.StepCPUMillis
	shared.Capacity.MemoryMiB += pol.Limits.StepMemoryMiB
	shared.Capacity.EphemeralMiB += pol.Limits.StepEphemeralMiB
	shared.Capacity.StorageMiB = ws + storage
	return shared, nil
}

func scalarOf(s *pipeline.Scalar) string {
	switch s.Type {
	case pipeline.TypeBoolean:
		return strconv.FormatBool(s.Bool)
	case pipeline.TypeInteger:
		return strconv.FormatInt(s.Int, 10)
	}
	return s.Str
}

// resolveServiceSpec resolves a service's nonsecret environment (literals,
// whole-scalar inputs and endpoint references) and its secret pins. A
// reference that cannot resolve is refused: nothing resolves later.
func resolveServiceSpec(in sharedInput, shared *RunShared, name string, sv pipeline.RunService) (ServiceSpec, error) {
	values := in.Snapshot.Values()
	pol := in.Policy
	spec := ServiceSpec{
		Name: name, Role: string(sv.Role), Image: sv.Image, Command: append([]string(nil), sv.Command...), Args: append([]string{}, sv.Args...),
		Env: map[string]string{}, Secrets: []SecretPin{}, Ports: map[string]int64{}, Storage: []StorageSpec{}, Egress: []string{},
		Host: shared.Endpoints[name].Host,
	}
	if sv.Command == nil {
		spec.Command = nil
	}
	if spec.Role == "" {
		spec.Role = string(pipeline.RoleGeneral)
	}
	for pn, p := range sv.Ports {
		spec.Ports[pn] = p.Port
	}
	var shm int64 = pipeline.DefaultShmMiB
	if sv.Resources.ShmMiB != nil {
		shm = *sv.Resources.ShmMiB
	}
	spec.Resources = ServiceResourcesSpec{CPUMillis: sv.Resources.CPUMillis, MemoryMiB: sv.Resources.MemoryMiB, EphemeralMiB: sv.Resources.EphemeralMiB, ShmMiB: shm}
	for _, s := range sv.Storage {
		spec.Storage = append(spec.Storage, StorageSpec{Path: s.Path, SizeMiB: s.SizeMiB})
	}
	rd := sv.Readiness
	spec.Readiness = ReadinessSpec{TimeoutSecs: rd.TimeoutSeconds, PeriodSecs: rd.PeriodSeconds, ProbeTimeout: rd.ProbeTimeoutSeconds, LossSecs: rd.LossSeconds, Exec: []string{}}
	switch {
	case rd.TCP != "":
		spec.Readiness.Probe, spec.Readiness.Port = "tcp", rd.TCP
	case rd.HTTP != nil:
		spec.Readiness.Probe, spec.Readiness.Port, spec.Readiness.Path = "http", rd.HTTP.Port, rd.HTTP.Path
	default:
		spec.Readiness.Probe, spec.Readiness.Exec = "exec", append([]string{}, rd.Exec...)
	}
	envNames := make([]string, 0, len(sv.Env))
	for n := range sv.Env {
		envNames = append(envNames, n)
	}
	sort.Strings(envNames)
	for _, en := range envNames {
		e := sv.Env[en]
		if e.Scalar == nil {
			continue // secret binding, pinned below
		}
		raw := scalarOf(e.Scalar)
		if v, ok, err := resolveEndpointRef(raw, shared.Endpoints); ok {
			if err != nil {
				return ServiceSpec{}, Refusedf("a service endpoint reference does not resolve")
			}
			spec.Env[en] = v
			continue
		}
		if strings.Contains(raw, "${{") {
			ref := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "${{"), "}}"))
			parts := strings.Split(ref, ".")
			if len(parts) != 2 || parts[0] != "inputs" {
				return ServiceSpec{}, Refusedf("a service reference is not supported")
			}
			v, ok := values[parts[1]]
			if !ok {
				return ServiceSpec{}, Refusedf("a service input has no pinned value")
			}
			spec.Env[en] = scalarString(v)
			continue
		}
		spec.Env[en] = raw
	}
	for _, p := range in.Snapshot.SecretsOf(serviceStepKey(name)) {
		spec.Secrets = append(spec.Secrets, SecretPin{Ref: p.Ref, Version: p.Version, Name: p.Name, Delivery: p.Delivery})
	}
	if sv.HasSecrets() && len(spec.Command) == 0 {
		return ServiceSpec{}, Refusedf("a service with secret bindings declares its command")
	}
	_ = pol
	return spec, nil
}

// serviceStepKey is the per-step key under which a service's secret pins are
// recorded in the run snapshot: service secrets are never a step's bindings,
// even for the same reference.
func serviceStepKey(name string) string { return servicePrefix + name }

// ---- resource specs ----

// BuildResourceSpecs derives the canonical specs of a run's resources, in
// provisioning order (network, workspace, preparation, services by name).
// deadline is the run's pinned deadline.
func BuildResourceSpecs(run *Run, deadline time.Time) ([]ResourceSpec, error) {
	sh := run.Shared
	if sh == nil {
		return nil, invalidf("not a run_shared run")
	}
	base := func(kind ResourceKind, key, role string) (ResourceSpec, error) {
		shape, err := sh.PolicyBody.RoleDigest(role)
		if err != nil {
			return ResourceSpec{}, err
		}
		return ResourceSpec{
			Scope: run.Scope, RunID: run.ID, Backend: sh.Backend, Kind: kind, Key: key, Name: ResourceName(run.Scope, run.ID, kind, key),
			Policy: sh.Policy, RoleShape: shape, DeadlineAt: deadline.UTC().Format(time.RFC3339),
		}, nil
	}
	var out []ResourceSpec
	net, err := base(KindNetwork, KeyNetwork, RoleStep)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sh.Services))
	for n := range sh.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	workloads := map[string][]string{}
	for step, eg := range sh.Egress {
		workloads[step] = append([]string{}, eg...)
	}
	for _, n := range names {
		workloads[ServiceKey(n)] = append([]string{}, sh.Services[n].Egress...)
	}
	egress := map[string]string{}
	for k, v := range sh.EgressDigests {
		egress[k] = v
	}
	net.Network = &NetworkSpec{
		Services: names, Edges: append([]EdgeSpec{}, sh.Edges...), Egress: egress, Workloads: workloads,
		Isolation: sh.PolicyBody.Network.Isolation, PublicEgress: sh.PolicyBody.Network.PublicEgress,
	}
	out = append(out, net)

	wsSpec, err := base(KindWorkspace, KeyWorkspace, RoleStep)
	if err != nil {
		return nil, err
	}
	initial := "empty"
	if sh.SourceBacked {
		initial = "git"
	}
	wsSpec.Workspace = &WorkspaceSpec{SizeMiB: sh.WorkspaceMiB, Initial: initial}
	out = append(out, wsSpec)

	if sh.SourceBacked {
		p, err := base(KindPreparation, KeyPreparation, RolePreparation)
		if err != nil {
			return nil, err
		}
		pl := sh.PolicyBody.Limits
		p.Preparation = &PreparationSpec{
			Image: sh.PolicyBody.Roles[RolePreparation].Image, RepositoryURL: sh.RepositoryURL, Commit: run.SourceCommit,
			TimeoutSecs: pl.PreparationTimeoutSeconds, MaxBytes: pl.PreparationMaxBytes, MaxAttempts: pl.PreparationAttempts,
		}
		out = append(out, p)
	}
	for _, n := range names {
		role := RoleService
		if sh.Services[n].Role == string(pipeline.RoleBuilder) {
			role = RoleBuilderService
		}
		sv, err := base(KindService, ServiceKey(n), role)
		if err != nil {
			return nil, err
		}
		svc := sh.Services[n]
		sv.Service = &svc
		out = append(out, sv)
	}
	for i := range out {
		if err := out[i].Validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
