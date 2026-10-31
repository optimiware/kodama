package runner

import (
	"fmt"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func (d *drive) stageWaits() (flow, error) { return flowPass, nil }

func (d *drive) openWait(ps runtime.PlanStep, ev *runtime.PredicateEvaluation) (flow, error) {
	return d.failBeforeStart(ps, ev, runtime.ReasonPreparationFailed)
}

func consumeDecision(bt *batch, s runtime.Step, w runtime.Wait, stepReason runtime.Reason) error {
	return fmt.Errorf("runner: waits are not supported yet")
}
