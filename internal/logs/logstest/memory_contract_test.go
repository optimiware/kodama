package logstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/logs/logstest"
)

// Test_unit_results_logs_contract runs the shared Contract of the log ports on
// the memory adapters, with no service (SD-13, SD-A11).
func Test_unit_results_logs_contract(t *testing.T) {
	logstest.Contract(t, func(testing.TB) logstest.Subject { return logstest.NewMemory().Subject() })
}
