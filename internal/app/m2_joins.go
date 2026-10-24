package app

// The M2 production joins (T-101): the bindings of the prerequisites T-096 to
// T-100 that earlier slices recorded as "not wired".
//
// Everything here is pure composition over injected adapters and operator
// configuration, so each binding is unit-tested without a service. Every
// binding fails closed: an absent statement or input leaves exactly the
// capability that needs it disabled, with a reason recorded in Routes.Reasons,
// and never widens authority. NewCore supplies the PostgreSQL, S3 and runtime
// backed adapters (core.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// ConformanceReportOutput is the declared artifact output name under which the
// OpenCode wrapper (T-031, images/harness-opencode/wrapper/spec.ConformanceOutput)
// returns the candidate's design conformance report. The gate module reads the
// report of an agent-produced candidate from this output of the producing step.
const ConformanceReportOutput = "conformance"

// Capability names the T-101 bindings record in Routes.Disabled when their
// configuration or input is absent. Routes.Reasons names why.
const (
	capActionCeiling    = "action-ceiling"
	capRegistry         = "oci-registry"
	capDeploymentGates  = "deployment-gates"
	capCandidateGates   = "agent-candidate-gates"
	capStepEnvironments = "step-environments"
	capSecretResolution = "runtime-secret-resolution"
	capGitWebhooks      = "git-webhooks"
)

// ---- action ceiling (T-096) ----------------------------------------------------

// ActionCeilingConfig is the operator's statement of which conditional runtime
// actions an attempt of a step kind may hold beyond the defaults
// (action-ceiling.json). Empty by default: every attempt then holds exactly the
// default actions, so an agent attempt cannot reach the model gateway or the
// MCP proxy and a step cannot call the deployment interface.
//
//	{"kinds": {"agent": {"grants": ["model.invoke", "tool.invoke"]},
//	           "container": {"grants": ["deployment.call"]}}}
//
// A grant is an upper bound ("may hold"): runtimeauth.CeilingPolicy still
// requires the attempt's own pinned facts (the profile names a model, a tool,
// the step declares an environment) before it holds the action.
type ActionCeilingConfig struct {
	Kinds map[string]KindCeilingConfig `json:"kinds"`
}

// KindCeilingConfig lists the actions one step kind may hold.
type KindCeilingConfig struct {
	Grants []string `json:"grants"`
}

// Configured reports whether the operator granted any action.
func (c ActionCeilingConfig) Configured() bool {
	for _, k := range c.Kinds {
		if len(k.Grants) > 0 {
			return true
		}
	}
	return false
}

// runtime maps the statement to the runtime authority's typed configuration. It
// validates names and combinations (runtimeauth.CeilingConfig.Validate).
func (c ActionCeilingConfig) runtime() (runtimeauth.CeilingConfig, error) {
	out := runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{}}
	for kind, k := range c.Kinds {
		grants := make([]runtimeauth.Action, 0, len(k.Grants))
		for _, name := range k.Grants {
			a, err := runtimeauth.ParseAction(name)
			if err != nil {
				return runtimeauth.CeilingConfig{}, fmt.Errorf("action ceiling: %w", err)
			}
			grants = append(grants, a)
		}
		out.Kinds[runtimeauth.StepKind(kind)] = runtimeauth.KindCeiling{Grants: grants}
	}
	if err := out.Validate(); err != nil {
		return runtimeauth.CeilingConfig{}, fmt.Errorf("action ceiling: %w", err)
	}
	return out, nil
}

// Validate checks the statement against the environment. The Kubernetes runtime
// registers its attempts under the execution policy's own reader and writer
// action lists and has no per-kind ceiling yet (T-089), so production refuses
// a statement it could not honour rather than ignoring it.
func (c ActionCeilingConfig) Validate(env Env) error {
	if _, err := c.runtime(); err != nil {
		return err
	}
	if c.Configured() && env == Production {
		return errors.New("action ceiling: the Kubernetes runtime takes its action lists from the execution policy; a per-kind ceiling is not wired there yet (T-089)")
	}
	return nil
}

// LoadActionCeiling reads the strict JSON document.
func LoadActionCeiling(path string) (ActionCeilingConfig, error) {
	var c ActionCeilingConfig
	if err := readStrictJSON(path, "action ceiling file", &c); err != nil {
		return ActionCeilingConfig{}, err
	}
	return c, nil
}

func readStrictJSON(path, what string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if dec.More() {
		return fmt.Errorf("%s: trailing content", what)
	}
	return nil
}

// CeilingBinding is the one action-ceiling policy of an installation, in the
// two halves that must agree (runtimeauth README, binding obligations): Policy
// chooses the ceiling a runtime registers for an attempt, and Allowed is the
// attempt authority's own action list (AttemptState.Actions), which a
// registration must be a subset of. Allowed is the defaults plus every action
// the operator granted to any kind; the effective authority of an attempt
// stays its registered ceiling intersected with it, so it never exceeds Policy.
type CeilingBinding struct {
	Policy  runtimeauth.CeilingPolicy
	Allowed func(access execution.Access) (runtimeauth.ActionSet, error)
}

// NewCeilingBinding builds the binding. A statement with no grant yields nil:
// the runtime keeps the default actions. profiles resolves a pinned agent
// profile version to what it names; without it an agent attempt keeps the
// defaults.
func NewCeilingBinding(cfg ActionCeilingConfig, profiles runtimeauth.ProfileResolver) (*CeilingBinding, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	rc, err := cfg.runtime()
	if err != nil {
		return nil, err
	}
	policy, err := runtimeauth.NewCeilingPolicy(runtimeauth.StaticCeiling(rc), profiles)
	if err != nil {
		return nil, err
	}
	var granted []runtimeauth.Action
	for _, k := range rc.Kinds {
		for _, a := range k.Grants {
			if !slices.Contains(granted, a) {
				granted = append(granted, a)
			}
		}
	}
	allowed := func(access execution.Access) (runtimeauth.ActionSet, error) {
		all := runtimeauth.DefaultActions(access == execution.AccessWrite)
		for _, a := range granted {
			if !slices.Contains(all, a) {
				all = append(all, a)
			}
		}
		return runtimeauth.NewActionSet(all...)
	}
	return &CeilingBinding{Policy: policy, Allowed: allowed}, nil
}

// StoredProfiles is the agent profile service's exact-version read.
type StoredProfiles interface {
	Stored(ctx context.Context, scope domain.Scope, ref string) (agents.ProfileVersion, error)
}

// profileCapabilities resolves a pinned profile version to whether it names a
// model and tools, from the stored immutable version in the attempt's own
// scope. It carries no endpoint, tool name or credential, and any failure
// (including an unknown or foreign version) grants nothing beyond the defaults.
type profileCapabilities struct{ profiles StoredProfiles }

var _ runtimeauth.ProfileResolver = profileCapabilities{}

// NewProfileCapabilities requires the profile reader.
func NewProfileCapabilities(p StoredProfiles) (runtimeauth.ProfileResolver, error) {
	if p == nil {
		return nil, errors.New("app: a profile reader is required for the action ceiling")
	}
	return profileCapabilities{profiles: p}, nil
}

func (p profileCapabilities) ProfileCapabilities(ctx context.Context, scope domain.Scope, pin runtimev1.Profile) (runtimeauth.ProfileCapabilities, error) {
	v, err := p.profiles.Stored(ctx, scope, agents.Ref(pin.Name, domain.Version(pin.Version)))
	if err != nil {
		return runtimeauth.ProfileCapabilities{}, err
	}
	d := v.Content.Draft()
	return runtimeauth.ProfileCapabilities{Model: d.Model.Name != "", Tools: len(d.Tools) > 0}, nil
}

// ---- delivery joins (T-097) ------------------------------------------------------

// DeliveryJoins are the three transaction-bound joins T-097 supplies, plus the
// conformance report name of the agent-candidate gate. A zero member is an
// absent join: the capability that needs it stays disabled and the owning
// module fails closed on its own (execution refuses an environment-declaring
// admission, the gate store cannot decide a deployment gate, the runtime route
// answers not found).
type DeliveryJoins struct {
	// Environments is execution's Peers.Environments: delivery's revalidation
	// bound to each admission transaction.
	Environments execpg.EnvironmentBinder
	// Deployments is the gate store's DeploymentBinder.
	Deployments gatespg.DeploymentBinder
	// DeploymentReader is the committed deployment read of a gate's
	// preparation (gates.Dependencies.Deployments).
	DeploymentReader gates.Deployments
	// Secrets serves resolveSecret on the runtime API.
	Secrets SecretResolver
	// ConformanceOutput is the report output name (ConformanceReportOutput).
	ConformanceOutput string
}

// executionPeers returns peers with the environment binding of the joins.
func (j DeliveryJoins) executionPeers(p execpg.Peers) execpg.Peers {
	p.Environments = j.Environments
	return p
}

// gateStore returns the gate store configuration with the deployment binder.
func (j DeliveryJoins) gateStore(c gatespg.Config) gatespg.Config {
	c.Deployments = j.Deployments
	return c
}

// gateDependencies adds the committed deployment reader and the conformance
// mapping to the gate service's dependencies. The reader is joined only
// together with the binder: a gate is preparable and decidable, or neither.
func (j DeliveryJoins) gateDependencies(d gates.Dependencies, conformance gates.Conformance) gates.Dependencies {
	if j.Deployments != nil && j.DeploymentReader != nil {
		d.Deployments = j.DeploymentReader
	}
	if j.ConformanceOutput != "" {
		d.Conformance = conformance
	}
	return d
}

// Disabled lists the capabilities the absent joins leave unavailable, with the
// reason of each. admitsEnvironments is the execution store's own statement
// (execution.EnvironmentStore), so the report follows what the store really
// binds.
func (j DeliveryJoins) Disabled(admitsEnvironments bool) (names []string, reasons map[string]string) {
	reasons = map[string]string{}
	add := func(name, why string) {
		names = append(names, name)
		reasons[name] = why
	}
	if !admitsEnvironments {
		add(capStepEnvironments, "the execution store has no transaction-bound delivery reader: an environment-declaring step is refused at admission")
	}
	if j.Deployments == nil || j.DeploymentReader == nil {
		add(capDeploymentGates, "the gate store has no transaction-bound deployment binder: a deployment-bearing gate cannot be decided or consumed")
	}
	if j.ConformanceOutput == "" {
		add(capCandidateGates, "no conformance report output name is bound: a gate that needs conformance evidence is not created")
	}
	if j.Secrets == nil {
		add(capSecretResolution, "the runtime secret route is not mounted: resolveSecret answers not found")
	}
	return names, reasons
}

// deliveryUnavailableM4 is deliveryUnavailableM2 after the T-101 joins: each
// capability a join enables is no longer listed.
func deliveryUnavailableM4(j DeliveryJoins, admitsEnvironments, modelTools, qualified bool) []string {
	out := deliveryUnavailableM2(admitsEnvironments, modelTools, qualified)
	if j.Deployments != nil && j.DeploymentReader != nil {
		out = slices.DeleteFunc(out, func(n string) bool { return n == capDeploymentGates })
	}
	if j.ConformanceOutput != "" {
		out = slices.DeleteFunc(out, func(n string) bool { return n == capCandidateGates })
	}
	return out
}

// JoinState is what the composition bound, for the capability report.
type JoinState struct {
	Joins              DeliveryJoins
	AdmitsEnvironments bool
	// Ceiling, Registry and GitTriggers say the operator statement was present
	// and bound.
	Ceiling     bool
	Registry    bool
	GitTriggers bool
}

// m2JoinReport completes the disabled-capability list with the capabilities
// the T-101 bindings leave off and says why. A capability another slice already
// lists keeps its place; the reasons are added.
func m2JoinReport(disabled []string, st JoinState) ([]string, map[string]string) {
	_, reasons := st.Joins.Disabled(st.AdmitsEnvironments)
	if !st.Ceiling {
		reasons[capActionCeiling] = "no action ceiling is configured (action-ceiling.json): attempts hold only the default runtime actions, so an agent attempt cannot call the model gateway or the MCP proxy and a step cannot call the deployment interface"
	}
	if !st.Registry {
		reasons[capRegistry] = "no registry is configured (registry.json): every OCI registration of an `ociArtifact` output is refused"
	}
	if !st.GitTriggers {
		reasons[capGitWebhooks] = "the Git push trigger is not selected (git-triggers.json): the hook route is not registered"
	}
	out := slices.Clone(disabled)
	for name := range reasons {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out[len(disabled):])
	return out, reasons
}
