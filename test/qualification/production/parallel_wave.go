//go:build qualification

package production

import (
	"testing"

	"github.com/optimiware/kodama/test/support/parallel"
)

// stageWave1Gate bounds how many wave 1 probes run at once against the stage.
// The package is sequential unless KODAMA_STAGE_CONCURRENCY asks for more: Go
// runs the tests that are not parallel first, so a parallel run of the whole
// package would start with the chaos probes. The unified suite selects the two
// waves separately and sets the bound for wave 1.
var stageWave1Gate = &parallel.Gate{Env: "KODAMA_STAGE_CONCURRENCY", Default: 1, Max: 24}

// stageWave1Parallel must be the first statement of every probe listed in
// suite.StageWave1Probes. The chaos probes of suite.StageWave2ChaosProbes never
// call it, so no other probe runs while one of them disrupts a shared service.
func stageWave1Parallel(t *testing.T) {
	t.Helper()
	stageWave1Gate.Enter(t)
}
