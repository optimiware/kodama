package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// Test_source_contract runs the source-set contract (T-148: run origins and
// source sets, the runtime context that carries them, the attempt Git grant,
// one attempt per run, cleanup and the earlier run models unchanged) on the
// PostgreSQL store, explicitly in the `test` environment. The in-memory store
// runs the same scenarios on every unit run (memory/source_test.go, the
// Test_unit_* acceptance names).
func Test_source_contract(t *testing.T) {
	executiontest.SourceContract(t, func(tb testing.TB) executiontest.SourceSubject {
		_, s := executiontest.NewSourcePG(tb)
		return s
	})
}
