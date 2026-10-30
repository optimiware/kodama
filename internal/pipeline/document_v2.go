package pipeline

import (
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
)

// This file holds the kodama/v1alpha2 (run_shared) additions to the typed
// document: its types, normalization and canonical tree. The kodama/v1alpha1
// branches in document.go are untouched; normalize and tree dispatch on
// APIVersion and nothing here is reachable for a v1alpha1 document.

// ServiceRole is the role of a run service.
type ServiceRole string

const (
	// RoleGeneral is the default role: an ordinary support workload.
	RoleGeneral ServiceRole = "general"
	// RoleBuilder is the run-exclusive builder daemon of the installation's
	// pinned builder-service mode. At most one per document.
	RoleBuilder ServiceRole = "builder"
)

// ProtocolTCP is the only service port protocol.
const ProtocolTCP = "TCP"

// DefaultShmMiB is the contract-constant default of resources.shmMiB.
const DefaultShmMiB int64 = 64

// Contract-constant readiness defaults.
const (
	DefaultProbePeriodSeconds  int64 = 2
	DefaultProbeTimeoutSeconds int64 = 5
	DefaultLossSeconds         int64 = 30
)

// RunWorkspace bounds the run scratch filesystem. Its absence means the pinned
// execution policy's default and is never filled into the canonical form.
type RunWorkspace struct {
	SizeMiB int64 `json:"sizeMiB"`
}

// Network declares the allowed service-to-service edges.
type Network struct {
	Edges []Edge `json:"edges,omitempty"`
}

// Edge is a directed service-to-service access declaration.
type Edge struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Ports []string `json:"ports"`
}

// ServicePort is one declared container port, which is also the endpoint port.
type ServicePort struct {
	Port     int64  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

// ServiceResources bounds a service workload. ShmMiB is a pointer because an
// explicit 0 is a valid size distinct from the default.
type ServiceResources struct {
	CPUMillis    int64  `json:"cpuMillis"`
	MemoryMiB    int64  `json:"memoryMiB"`
	EphemeralMiB int64  `json:"ephemeralMiB"`
	ShmMiB       *int64 `json:"shmMiB,omitempty"`
}

// ServiceStorage is one run-owned ephemeral volume of a service.
type ServiceStorage struct {
	Path    string `json:"path"`
	SizeMiB int64  `json:"sizeMiB"`
}

// HTTPProbe is an HTTP readiness probe over plain HTTP.
type HTTPProbe struct {
	Port string `json:"port"`
	Path string `json:"path"`
}

// Readiness is a service's readiness contract: exactly one of TCP, HTTP or
// Exec, plus the startup budget. Zero values of the optional numeric fields
// mean "absent" (their minimum is above zero) and normalize to the contract
// defaults.
type Readiness struct {
	TCP  string     `json:"tcp,omitempty"`
	HTTP *HTTPProbe `json:"http,omitempty"`
	Exec []string   `json:"exec,omitempty"`

	TimeoutSeconds      int64 `json:"timeoutSeconds"`
	PeriodSeconds       int64 `json:"periodSeconds,omitempty"`
	ProbeTimeoutSeconds int64 `json:"probeTimeoutSeconds,omitempty"`
	LossSeconds         int64 `json:"lossSeconds,omitempty"`
}

// RunService is a run-lifetime support workload. Command is nil when absent, which
// is meaningful (the image defaults run) and preserved by normalization; Args
// absent and empty are one value and normalize to an empty list.
type RunService struct {
	Image       string                 `json:"image"`
	Role        ServiceRole            `json:"role,omitempty"`
	Command     []string               `json:"command,omitempty"`
	Args        []string               `json:"args,omitempty"`
	Env         map[string]EnvValue    `json:"env,omitempty"`
	SecretFiles map[string]string      `json:"secretFiles,omitempty"`
	Ports       map[string]ServicePort `json:"ports"`
	Resources   ServiceResources       `json:"resources"`
	Storage     []ServiceStorage       `json:"storage,omitempty"`
	Readiness   Readiness              `json:"readiness"`
	Egress      []string               `json:"egress,omitempty"`
}

// HasSecrets reports whether the service has any secret binding.
func (s RunService) HasSecrets() bool {
	if len(s.SecretFiles) > 0 {
		return true
	}
	for _, v := range s.Env {
		if v.SecretRef != "" {
			return true
		}
	}
	return false
}

// IsV1Alpha2 reports whether the document is a kodama/v1alpha2 document.
func (d Document) IsV1Alpha2() bool { return d.APIVersion == pipelinev1alpha2.APIVersion }

// IsV1Alpha3 reports whether the document is a kodama/v1alpha3 document: the
// run_shared format plus a declared source set.
func (d Document) IsV1Alpha3() bool { return d.APIVersion == pipelinev1alpha3.APIVersion }

// IsRunShared reports whether the document describes the run_shared model, in
// either of its versions. The kodama/v1alpha1 document is attempt_private.
func (d Document) IsRunShared() bool { return d.IsV1Alpha2() || d.IsV1Alpha3() }

// SourceAccess is the access a declared source gives its run: read, or write
// as a ceiling for the step that holds the writer reservation.
type SourceAccess string

// Source accesses.
const (
	SourceRead  SourceAccess = "read"
	SourceWrite SourceAccess = "write"
)

// Source declares one repository of a kodama/v1alpha3 run's source set.
// Repository is the repository's logical name within the project, resolved and
// pinned to one commit at admission; Path is relative to /workspace. A document
// never selects a commit.
type Source struct {
	Repository string       `json:"repository"`
	Path       string       `json:"path"`
	Access     SourceAccess `json:"access"`
}

// RootSourcePath is the only path that checks a repository out at /workspace
// itself. It is the implicit path of a document without sources, and when
// declared it is the only source.
const RootSourcePath = "."

// hasV2Fields reports whether a document carries a field only kodama/v1alpha2
// defines. A v1alpha1 document that does is refused instead of silently losing
// the field to the v1alpha1 normalization.
func (d Document) hasV2Fields() (path string, ok bool) {
	switch {
	case d.Spec.TimeoutSeconds != 0:
		return "$.spec.timeoutSeconds", true
	case d.Spec.RunWorkspace != nil:
		return "$.spec.runWorkspace", true
	case d.Spec.Services != nil:
		return "$.spec.services", true
	case d.Spec.Network != nil:
		return "$.spec.network", true
	case d.Spec.Sources != nil:
		return "$.spec.sources", true
	}
	for i, s := range d.Spec.Steps {
		if s.Egress != nil {
			return stepPath(i) + ".egress", true
		}
	}
	return "", false
}

func copyStrings(in []string) []string { return append([]string{}, in...) }

func copyEnv(in map[string]EnvValue) map[string]EnvValue {
	out := make(map[string]EnvValue, len(in))
	for k, v := range in {
		e := EnvValue{SecretRef: v.SecretRef}
		if v.Scalar != nil {
			sc := *v.Scalar
			e.Scalar = &sc
		}
		out[k] = e
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeRetryV2(in *Retry) *Retry {
	r := Retry{MaxAttempts: 1, On: []RetryClass{}}
	if in != nil {
		r.BackoffSeconds = in.BackoffSeconds
		r.On = append(r.On, in.On...)
		if in.MaxAttempts != 0 {
			r.MaxAttempts = in.MaxAttempts
		}
	}
	return &r
}

// normalizeV2 returns a deep copy of a kodama/v1alpha2 document with the
// contract-constant defaults applied and nothing else: step workspace,
// spec.runWorkspace, service command, environment and when are never filled, so
// the digest is independent of run source and installation policy. The copy
// shares no map, slice or pointer with the receiver and retains every v1alpha2
// field.
func normalizeV2(d Document) Document {
	out := Document{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: d.Metadata}
	out.Spec.TimeoutSeconds = d.Spec.TimeoutSeconds
	if d.Spec.RunWorkspace != nil {
		w := *d.Spec.RunWorkspace
		out.Spec.RunWorkspace = &w
	}
	if d.Spec.Sources != nil {
		out.Spec.Sources = append([]Source{}, d.Spec.Sources...)
	}
	out.Spec.Inputs = make(map[string]Input, len(d.Spec.Inputs))
	for name, in := range d.Spec.Inputs {
		c := Input{Type: in.Type, Required: in.Required}
		if in.Default != nil {
			v := *in.Default
			c.Default = &v
		}
		out.Spec.Inputs[name] = c
	}
	if d.Spec.Services != nil {
		out.Spec.Services = make(map[string]RunService, len(d.Spec.Services))
		for name, s := range d.Spec.Services {
			out.Spec.Services[name] = normalizeService(s)
		}
	}
	net := Network{Edges: []Edge{}}
	if d.Spec.Network != nil {
		for _, e := range d.Spec.Network.Edges {
			net.Edges = append(net.Edges, Edge{From: e.From, To: e.To, Ports: copyStrings(e.Ports)})
		}
	}
	out.Spec.Network = &net

	out.Spec.Steps = make([]Step, len(d.Spec.Steps))
	for i, s := range d.Spec.Steps {
		c := Step{ID: s.ID, Kind: s.Kind, Needs: copyStrings(s.Needs)}
		if s.When != nil {
			w := *s.When
			c.When = &w
		}
		switch s.Kind {
		case KindGate:
			g := GateSpec{Evidence: []string{}}
			if s.Gate != nil {
				g.Evidence = append(g.Evidence, s.Gate.Evidence...)
				g.DeploymentID = s.Gate.DeploymentID
			}
			c.Gate = &g
		default:
			if s.Workspace != nil {
				w := *s.Workspace
				c.Workspace = &w
			}
			c.Retry = normalizeRetryV2(s.Retry)
			c.TimeoutSeconds = s.TimeoutSeconds
			c.Outputs = make(map[string]Output, len(s.Outputs))
			for k, v := range s.Outputs {
				c.Outputs[k] = v
			}
			if s.Kind == KindContainer {
				c.Environment = s.Environment
				c.Image = s.Image
				c.Command = copyStrings(s.Command)
				c.Args = copyStrings(s.Args)
				c.Env = copyEnv(s.Env)
				c.SecretFiles = copyStringMap(s.SecretFiles)
				c.Egress = copyStrings(s.Egress)
			}
			if s.Agent != nil {
				a := *s.Agent
				c.Agent = &a
			}
		}
		out.Spec.Steps[i] = c
	}
	return out
}

func normalizeService(s RunService) RunService {
	c := RunService{Image: s.Image, Role: s.Role}
	if c.Role == "" {
		c.Role = RoleGeneral
	}
	if s.Command != nil {
		c.Command = copyStrings(s.Command)
	}
	c.Args = copyStrings(s.Args)
	c.Env = copyEnv(s.Env)
	c.SecretFiles = copyStringMap(s.SecretFiles)
	c.Ports = make(map[string]ServicePort, len(s.Ports))
	for k, p := range s.Ports {
		if p.Protocol == "" {
			p.Protocol = ProtocolTCP
		}
		c.Ports[k] = p
	}
	c.Resources = s.Resources
	shm := DefaultShmMiB
	if s.Resources.ShmMiB != nil {
		shm = *s.Resources.ShmMiB
	}
	c.Resources.ShmMiB = &shm
	c.Storage = append([]ServiceStorage{}, s.Storage...)
	c.Readiness = Readiness{
		TCP: s.Readiness.TCP, Exec: nil,
		TimeoutSeconds: s.Readiness.TimeoutSeconds, PeriodSeconds: s.Readiness.PeriodSeconds,
		ProbeTimeoutSeconds: s.Readiness.ProbeTimeoutSeconds, LossSeconds: s.Readiness.LossSeconds,
	}
	if s.Readiness.HTTP != nil {
		h := *s.Readiness.HTTP
		c.Readiness.HTTP = &h
	}
	if s.Readiness.Exec != nil {
		c.Readiness.Exec = copyStrings(s.Readiness.Exec)
	}
	if c.Readiness.PeriodSeconds == 0 {
		c.Readiness.PeriodSeconds = DefaultProbePeriodSeconds
	}
	if c.Readiness.ProbeTimeoutSeconds == 0 {
		c.Readiness.ProbeTimeoutSeconds = DefaultProbeTimeoutSeconds
	}
	if c.Readiness.LossSeconds == 0 {
		c.Readiness.LossSeconds = DefaultLossSeconds
	}
	c.Egress = copyStrings(s.Egress)
	return c
}

// treeV2 renders a normalized kodama/v1alpha2 document as the generic JSON tree
// of canonical serialization.
func (d Document) treeV2() map[string]any {
	inputs := map[string]any{}
	for name, in := range d.Spec.Inputs {
		m := map[string]any{"type": string(in.Type), "required": in.Required}
		if in.Default != nil {
			m["default"] = in.Default.value()
		}
		inputs[name] = m
	}
	spec := map[string]any{"timeoutSeconds": d.Spec.TimeoutSeconds, "inputs": inputs}
	if d.Spec.RunWorkspace != nil {
		spec["runWorkspace"] = map[string]any{"sizeMiB": d.Spec.RunWorkspace.SizeMiB}
	}
	if d.Spec.Sources != nil {
		sources := make([]any, len(d.Spec.Sources))
		for i, src := range d.Spec.Sources {
			sources[i] = map[string]any{"repository": src.Repository, "path": src.Path, "access": string(src.Access)}
		}
		spec["sources"] = sources
	}
	if d.Spec.Services != nil {
		services := map[string]any{}
		for name, s := range d.Spec.Services {
			services[name] = serviceTree(s)
		}
		spec["services"] = services
	}
	edges := []any{}
	if d.Spec.Network != nil {
		for _, e := range d.Spec.Network.Edges {
			edges = append(edges, map[string]any{"from": e.From, "to": e.To, "ports": strs(e.Ports)})
		}
	}
	spec["network"] = map[string]any{"edges": edges}

	steps := make([]any, 0, len(d.Spec.Steps))
	for _, s := range d.Spec.Steps {
		m := map[string]any{"id": s.ID, "kind": string(s.Kind), "needs": strs(s.Needs)}
		if s.When != nil {
			m["when"] = map[string]any{"input": s.When.Input, "equals": s.When.Equals.value()}
		}
		switch s.Kind {
		case KindGate:
			g := map[string]any{"evidence": strs(s.Gate.Evidence)}
			if s.Gate.DeploymentID != "" {
				g["deploymentId"] = s.Gate.DeploymentID
			}
			m["gate"] = g
		default:
			if s.Workspace != nil {
				m["workspace"] = map[string]any{"access": string(s.Workspace.Access)}
			}
			classes := make([]any, len(s.Retry.On))
			for i, c := range s.Retry.On {
				classes[i] = string(c)
			}
			m["retry"] = map[string]any{"maxAttempts": s.Retry.MaxAttempts, "backoffSeconds": s.Retry.BackoffSeconds, "on": classes}
			m["timeoutSeconds"] = s.TimeoutSeconds
			outs := map[string]any{}
			for k, o := range s.Outputs {
				om := map[string]any{"type": string(o.Type)}
				if o.Path != "" {
					om["path"] = o.Path
				}
				outs[k] = om
			}
			m["outputs"] = outs
			if s.Kind == KindContainer {
				if s.Environment != "" {
					m["environment"] = s.Environment
				}
				m["image"] = s.Image
				m["command"] = strs(s.Command)
				m["args"] = strs(s.Args)
				m["env"] = envTree(s.Env)
				m["secretFiles"] = stringMapTree(s.SecretFiles)
				m["egress"] = strs(s.Egress)
			}
			if s.Agent != nil {
				m["agent"] = map[string]any{"profile": s.Agent.Profile}
			}
		}
		steps = append(steps, m)
	}
	spec["steps"] = steps
	return map[string]any{
		"apiVersion": d.APIVersion,
		"kind":       d.Kind,
		"metadata":   map[string]any{"name": d.Metadata.Name},
		"spec":       spec,
	}
}

func envTree(in map[string]EnvValue) map[string]any {
	env := map[string]any{}
	for k, v := range in {
		env[k] = v.value()
	}
	return env
}

func stringMapTree(in map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func serviceTree(s RunService) map[string]any {
	ports := map[string]any{}
	for k, p := range s.Ports {
		ports[k] = map[string]any{"port": p.Port, "protocol": p.Protocol}
	}
	storage := make([]any, 0, len(s.Storage))
	for _, st := range s.Storage {
		storage = append(storage, map[string]any{"path": st.Path, "sizeMiB": st.SizeMiB})
	}
	res := map[string]any{"cpuMillis": s.Resources.CPUMillis, "memoryMiB": s.Resources.MemoryMiB, "ephemeralMiB": s.Resources.EphemeralMiB}
	if s.Resources.ShmMiB != nil {
		res["shmMiB"] = *s.Resources.ShmMiB
	}
	ready := map[string]any{
		"timeoutSeconds": s.Readiness.TimeoutSeconds, "periodSeconds": s.Readiness.PeriodSeconds,
		"probeTimeoutSeconds": s.Readiness.ProbeTimeoutSeconds, "lossSeconds": s.Readiness.LossSeconds,
	}
	switch {
	case s.Readiness.TCP != "":
		ready["tcp"] = s.Readiness.TCP
	case s.Readiness.HTTP != nil:
		ready["http"] = map[string]any{"port": s.Readiness.HTTP.Port, "path": s.Readiness.HTTP.Path}
	case s.Readiness.Exec != nil:
		ready["exec"] = strs(s.Readiness.Exec)
	}
	m := map[string]any{
		"image": s.Image, "role": string(s.Role), "args": strs(s.Args), "env": envTree(s.Env),
		"secretFiles": stringMapTree(s.SecretFiles), "ports": ports, "resources": res, "storage": storage,
		"readiness": ready, "egress": strs(s.Egress),
	}
	if s.Command != nil {
		m["command"] = strs(s.Command)
	}
	return m
}
