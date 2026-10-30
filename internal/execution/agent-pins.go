package execution

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// AgentPin is the agent profile and design baseline an attempt of an agent step
// is pinned to: exactly what the step's frozen wrapper payload implies.
type AgentPin struct {
	Profile        runtimev1.Profile
	DesignBaseline runtimev1.DesignBaseline
}

// AgentPins derives the pins of an attempt from the step's frozen command. It is
// consumer-owned: the execution module defines it and never imports the agent
// implementation. Shared (run_shared) and source-set attempts consult it while
// their runtime context is built, before the context is canonicalized and
// digested, so the pins are part of the frozen context and its digest.
//
// Implementations perform no I/O and read no profile: the pins come only from
// the payload admission froze into the command, so restart, retry and redrive
// can never see a newer profile and rebuilding a context yields the same bytes.
// The method is called under the dispatch transaction and may be repeated.
//
// PinAgent reports ok=false for a command that carries no agent payload (the
// context is then left exactly as it would be without a port). A payload that
// is present but invalid, or does not match the command, is an Invalidf error.
type AgentPins interface {
	PinAgent(tenant domain.TenantID, command runtimev1.Command) (pin AgentPin, ok bool, err error)
}

// pinAgent resolves the pins of an attempt's command. Without a port a step that
// is not an agent step is unchanged, while an agent step is refused: it would
// otherwise run a wrapper whose profile and design baseline the control plane
// never pinned, and the agent policy and the wrapper both fail closed on such a
// context. An agent step reaches dispatch only through an agent resolver, so this
// is a composition that expands agents without pinning them.
func pinAgent(p AgentPins, tenant domain.TenantID, step *StepRun, command runtimev1.Command) (*AgentPin, error) {
	agent := step.Spec.Kind == pipeline.KindAgent
	if p == nil {
		if agent {
			return nil, Refusedf("agent step %s cannot run: no agent pin port is configured", step.ID)
		}
		return nil, nil
	}
	pin, ok, err := p.PinAgent(tenant, command)
	if err != nil {
		return nil, err
	}
	if !ok {
		if agent {
			return nil, Refusedf("agent step %s has no agent payload", step.ID)
		}
		return nil, nil
	}
	return &pin, nil
}
