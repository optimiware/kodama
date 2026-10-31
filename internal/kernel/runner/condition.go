package runner

import (
	"errors"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func (d *drive) evaluateWhen(ps runtime.PlanStep) (runtime.PredicateEvaluation, error) {
	return runtime.PredicateEvaluation{}, errors.New("runner: conditions are not supported yet")
}

func (d *drive) reference(ref string) (runtime.InputValue, bool, error) {
	return runtime.InputValue{}, false, nil
}
