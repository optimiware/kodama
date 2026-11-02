package execution

import (
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

type pinFn func(domain.TenantID, runtimev1.Command) (AgentPin, bool, error)

func (f pinFn) PinAgent(t domain.TenantID, c runtimev1.Command) (AgentPin, bool, error) {
	return f(t, c)
}

// An agent step is never run without pins: with no port, or with a port that finds
// no payload, it is refused with a classified admission refusal. A step of
// another kind is left exactly as before in both cases (T-163).
func Test_unit_pin_agent_policy(t *testing.T) {
	agent := &StepRun{ID: "gen", Spec: pipeline.Step{ID: "gen", Kind: pipeline.KindAgent}}
	plain := &StepRun{ID: "build", Spec: pipeline.Step{ID: "build", Kind: pipeline.KindContainer}}
	cmd := runtimev1.Command{Image: "img", Command: []string{"/bin/true"}}
	none := pinFn(func(domain.TenantID, runtimev1.Command) (AgentPin, bool, error) { return AgentPin{}, false, nil })
	boom := pinFn(func(domain.TenantID, runtimev1.Command) (AgentPin, bool, error) {
		return AgentPin{}, false, Invalidf("the frozen agent payload is invalid")
	})
	some := pinFn(func(tenant domain.TenantID, _ runtimev1.Command) (AgentPin, bool, error) {
		return AgentPin{Profile: runtimev1.Profile{Name: string(tenant), Version: 1}}, true, nil
	})

	for name, tc := range map[string]struct {
		pins    AgentPins
		step    *StepRun
		refused bool
		invalid bool
		pinned  bool
	}{
		"no port, agent step":        {pins: nil, step: agent, refused: true},
		"no port, container step":    {pins: nil, step: plain},
		"no payload, agent step":     {pins: none, step: agent, refused: true},
		"no payload, container step": {pins: none, step: plain},
		"invalid payload":            {pins: boom, step: agent, invalid: true},
		"invalid payload, container": {pins: boom, step: plain, invalid: true},
		"payload, agent step":        {pins: some, step: agent, pinned: true},
	} {
		t.Run(name, func(t *testing.T) {
			pin, err := pinAgent(tc.pins, "t_acme", tc.step, cmd)
			switch {
			case tc.refused:
				if !errors.Is(err, ErrAdmissionRefused) || pin != nil {
					t.Fatalf("%v %v", pin, err)
				}
			case tc.invalid:
				if !errors.Is(err, ErrInvalid) || pin != nil {
					t.Fatalf("%v %v", pin, err)
				}
			case err != nil:
				t.Fatal(err)
			case tc.pinned:
				if pin == nil || pin.Profile.Name != "t_acme" {
					t.Fatalf("the pin carries the run's tenant: %+v", pin)
				}
			case pin != nil:
				t.Fatalf("no pin expected: %+v", pin)
			}
		})
	}
}
