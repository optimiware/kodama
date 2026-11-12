//go:build component

package messaging_test

import (
	"os"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestMain creates this process's evidence run directory before the tests so
// shard image records and the results summary share it, runs the harness, and
// publishes the required summary. A failed summary publication fails the run;
// it is never reduced to a logged warning over a passing run.
//
// The harness exit code stays authoritative: this is the suite's production
// path, so a masking defect inside RunMain must not hide a red messaging run
// from CI. On a passing run and on a failing run whose RunMain result is not
// masked, behaviour is identical to returning RunMain's code directly.
func TestMain(m *testing.M) {
	ran := 0
	code := support.RunMain(func() int {
		ran = harness.Main(m)
		return ran
	})
	if ran != 0 && code == 0 {
		code = ran
	}
	os.Exit(code)
}
