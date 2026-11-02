package adapters

import (
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Planner decorates the configured execution.Planner. It reads only the frozen
// command payload of the attempt's context, validates it, populates the runtime
// context's profile and design baseline pins, and delegates fingerprinting and
// context generation, so the pins are part of the attempt's canonical context
// and digest. It performs no I/O and never queries a profile: the pins come from
// what admission froze, so restart, retry and redrive cannot see a newer
// profile. An ordinary container command (no payload marker) is delegated
// unchanged.
type Planner struct{ inner execution.Planner }

var _ execution.Planner = Planner{}

// Plan implements execution.Planner.
func (p Planner) Plan(tenant domain.TenantID, c runtimev1.Context) (execution.WorkloadPlan, error) {
	pin, present, err := pinCommand(c.Command)
	if err != nil {
		return execution.WorkloadPlan{}, err
	}
	if present {
		c.Profile, c.DesignBaseline = &pin.Profile, &pin.DesignBaseline
	}
	plan, err := p.inner.Plan(tenant, c)
	if err != nil {
		return execution.WorkloadPlan{}, fmt.Errorf("agents planner: %w", err)
	}
	return plan, nil
}

// Pinner implements execution.AgentPins: it derives the profile and design
// baseline pins of a shared (v2) or source-set (v3) attempt from the frozen
// payload of the step's command, with exactly the checks the Planner applies to a
// v1 attempt (it shares their code). It performs no I/O and never queries a
// profile, so rebuilding a context for the same step yields the same pins. A
// command without the payload marker reports no pin.
type Pinner struct{}

var _ execution.AgentPins = Pinner{}

// PinAgent implements execution.AgentPins.
func (Pinner) PinAgent(_ domain.TenantID, c runtimev1.Command) (execution.AgentPin, bool, error) {
	return pinCommand(c)
}

// pinCommand is the one place a frozen command becomes pins, for every context
// version.
func pinCommand(c runtimev1.Command) (execution.AgentPin, bool, error) {
	cfg, present, err := agents.DecodeArgs(c.Args)
	if err != nil {
		return execution.AgentPin{}, false, execution.Invalidf("the frozen agent payload is invalid")
	}
	if !present {
		return execution.AgentPin{}, false, nil
	}
	if err := cfg.Verify(c.Image, c.Command); err != nil {
		return execution.AgentPin{}, false, execution.Invalidf("the frozen agent payload does not match its command")
	}
	profile, baseline := cfg.Pins()
	return execution.AgentPin{Profile: profile, DesignBaseline: baseline}, true, nil
}
