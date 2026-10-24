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
	cfg, present, err := agents.DecodeArgs(c.Command.Args)
	if err != nil {
		return execution.WorkloadPlan{}, execution.Invalidf("the frozen agent payload is invalid")
	}
	if present {
		if err := cfg.Verify(c.Command.Image, c.Command.Command); err != nil {
			return execution.WorkloadPlan{}, execution.Invalidf("the frozen agent payload does not match its command")
		}
		profile, baseline := cfg.Pins()
		c.Profile = &profile
		c.DesignBaseline = &baseline
	}
	plan, err := p.inner.Plan(tenant, c)
	if err != nil {
		return execution.WorkloadPlan{}, fmt.Errorf("agents planner: %w", err)
	}
	return plan, nil
}
