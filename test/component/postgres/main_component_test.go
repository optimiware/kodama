//go:build component

package postgrescomponent

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestMain installs the harness signal handler and orphan sweep for this
// component test binary.
func TestMain(m *testing.M) {
	code := harness.Main(m)
	filtered := false
	if run := flag.Lookup("test.run"); run != nil && run.Value.String() != "" {
		filtered = true
	}
	if code == 0 && !filtered {
		if err := pgtest.GoldenCoverageError(nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		if err := pgtest.UnitStateCoverageError(filepath.Join("..", "..", "..")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		matrixPath := filepath.Join("..", "..", "support", "infrastructure", "postgres", "README.md")
		if os.Getenv("KODAMA_UPDATE_UNIT_MATRIX") == "1" {
			if err := pgtest.WriteUnitStateMatrix(matrixPath); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
		if err := pgtest.UnitStateMatrixError(matrixPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}
